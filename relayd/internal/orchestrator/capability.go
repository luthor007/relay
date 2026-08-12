package orchestrator

import (
	"context"
	"time"
)

// Kind is what a tool does to the world.
//
// There is deliberately no "execute" member, and that absence is the
// architecture. Relay chooses which agent does the work, gives it the access
// and the instructions it needs, and gets out of the way — it does not drive
// the browser, it tells an agent with a browser what to do. Every tool the big
// model can call is one of these three, and [Toolbox] refuses to build a tool
// that is not classified, so adding an executing tool means first inventing a
// word for it here and explaining why.
//
// The practical reason, not just the aesthetic one: the runtimes already have
// shells, browsers, editors and their own approval prompts, all of which the
// user has already configured and audited. A second execution path inside
// relayd would be a second sandbox boundary, a second confirmation surface and
// a second thing to keep safe, on a machine the user already controls.
type Kind string

const (
	// KindRead answers a question and changes nothing.
	KindRead Kind = "read"
	// KindInstruct hands work to an agent session. This is the only way work
	// gets done, and there is exactly one such tool.
	KindInstruct Kind = "instruct"
	// KindProvision changes what is available or what is remembered — a grant,
	// a skill, a fact. It prepares work rather than doing it.
	KindProvision Kind = "provision"
)

// Capability is something an agent on this machine could be given access to:
// a browser, a dashboard, a mailbox, an issue tracker.
//
// The orchestrator's interest in these is narrow and worth stating: it wants to
// know what exists so it can pick a session that has what a task needs, and it
// wants to be able to ask for a grant. It never uses one.
type Capability struct {
	Name string
	// Summary is what having this lets an agent do, in one plain sentence.
	Summary string
	// Opens is what the *user* is told when granting, which is recorded with
	// the grant so the trail says what was agreed to.
	Opens   string
	Granted bool
	// Half is "read" or "write". ORCHESTRATOR.md §4b rule 2 keeps them
	// separate: reading a calendar is not sending invitations.
	Half string
}

// Capabilities is the connector registry, seen from the orchestrator.
type Capabilities interface {
	List(ctx context.Context) ([]Capability, error)
	// Grant records a decision a human has already made. Implementations must
	// refuse a request that does not carry one — internal/connector's Grants
	// does, and that refusal is what makes ORCHESTRATOR.md §4b rule 1 a
	// property of the code rather than of this package's manners.
	Grant(ctx context.Context, name, half, by string) error
}

// sessionKey carries the session a tool call belongs to.
//
// Request-scoped and read by exactly one handler, which is what context values
// are for. The alternative — rebuilding the toolbox per turn so the session can
// be closed over — would work and would mean every future tool silently
// depending on when it was built.
type sessionKey struct{}

// WithSession tags a context with the session a turn belongs to.
func WithSession(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionKey{}, id)
}

// SessionFrom reads it back, or "" when a tool was called outside a turn.
func SessionFrom(ctx context.Context) string {
	id, _ := ctx.Value(sessionKey{}).(string)
	return id
}

// Fact is one durable thing worth remembering across sessions.
type Fact struct {
	// Text is the fact, in a sentence that will still make sense in a month.
	Text string
	// Subject is what it is about — a repository, a service, a person.
	Subject string
	// Session is where it was learned.
	Session string
	At      time.Time
}

// Notebook is the write half of memory.
//
// Separate from [Memory] for the same reason connectors split read from write:
// most of what the orchestrator does with memory is read it, an install can
// reasonably have the read half and not the write half, and a nil Notebook
// simply means the remember tool is not offered rather than that memory is
// broken.
type Notebook interface {
	Remember(ctx context.Context, f Fact) error
}

// Skills is the authored-playbook store. [SkillBook] implements it.
type Skills interface {
	List(ctx context.Context) ([]Skill, error)
	Author(ctx context.Context, s Skill) error
}
