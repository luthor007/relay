package orchestrator

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/luthor007/relay/relayd/internal/mcp"
)

// Skill is something the orchestrator learned to ask for, written down so the
// next time is one step instead of five.
//
// A skill is *instructions*, never code and never an action. That is the whole
// distinction this package is built on: Relay orchestrates, it does not
// execute. A skill for "check the staging dashboard" does not open a browser —
// it tells whichever agent has a browser how to, and Relay's job was choosing
// that agent and handing it the text.
//
// The shape is deliberately the shape of a good tool description: a name, the
// conditions under which it applies, and what to do. The When field is what
// gets read by a model deciding whether to reach for it, so it says when
// rather than what.
type Skill struct {
	// Name is the wire name — lowercase, underscores, specific.
	Name string
	// Title is one line for a human reading the console.
	Title string
	// When is the trigger condition: "when the user asks about deploy status".
	When string
	// Steps is what the executing agent is told to do. Plain instructions in
	// the imperative, not code.
	Steps string
	// Needs names the capabilities the agent must already have — "browser",
	// "kubectl". Advisory: it is in the instruction text so the agent can say
	// it lacks one rather than improvise.
	Needs []string

	CreatedAt time.Time
	// Origin records where the skill came from, because a skill Relay wrote for
	// itself and a skill a human wrote deserve different scrutiny in review.
	Origin string
}

// Validate rejects the skills that would be worse than not having them.
func (s Skill) Validate() error {
	switch {
	case strings.TrimSpace(s.Name) == "":
		return fmt.Errorf("orchestrator: a skill needs a name")
	case strings.ContainsAny(s.Name, " \t/\\"):
		return fmt.Errorf("orchestrator: %q is not a usable tool name", s.Name)
	case strings.TrimSpace(s.When) == "":
		// A skill with no trigger is a skill no model will ever reach for,
		// which is worse than absent: it occupies context and never fires.
		return fmt.Errorf("orchestrator: skill %q does not say when to use it", s.Name)
	case strings.TrimSpace(s.Steps) == "":
		return fmt.Errorf("orchestrator: skill %q has no instructions", s.Name)
	}
	return nil
}

// SkillConnector is the grant every authored skill belongs to.
//
// One connector rather than one per skill, because the decision a human is
// actually making is "may agents on this machine use the playbooks Relay has
// written", and splitting that into a decision per skill would train people to
// click through it. Revoking it withdraws all of them at once, in all five
// runtimes, which is the property ORCHESTRATOR.md §4b asks for.
const SkillConnector = "relay-skills"

// SkillStore is where skills survive a restart.
//
// It is an interface so this package does not import internal/store, and it is
// not optional in any interesting sense: a book with no store forgets every
// playbook when the daemon stops, and a skill that has to be rediscovered every
// morning is not something the orchestrator learned.
type SkillStore interface {
	Load(ctx context.Context) ([]Skill, error)
	Save(ctx context.Context, s Skill) error
}

// SkillBook holds the authored skills and publishes them to every runtime.
//
// It is an [mcp.Provider], which is the entire distribution story: the gateway
// is already in each runtime's config, so a skill written once is visible to
// Claude Code, Codex and the three ACP runtimes without touching any of their
// config files again. Grant once, works in all five, revoke once.
//
// The in-memory map is a cache in front of [SkillStore], not the record.
// [Provider.Tools] is called on every tools/list and must not touch a database,
// so the read path is the map and the write path is both.
type SkillBook struct {
	mu     sync.RWMutex
	skills map[string]Skill
	store  SkillStore
	now    func() time.Time

	// ExportDir is where SKILL.md files are written, if anywhere. Empty means
	// the bus is the only distribution — which reaches every runtime pointed at
	// our gateway and nothing that is not.
	ExportDir string
}

// NewSkillBook builds an empty one that forgets on restart. Use
// [OpenSkillBook] anywhere the daemon is running.
func NewSkillBook() *SkillBook {
	return &SkillBook{skills: map[string]Skill{}, now: time.Now}
}

// OpenSkillBook builds a book backed by a store and loads what is already
// there.
//
// A load failure is returned rather than swallowed: starting with an empty book
// on a machine that has skills would silently withdraw tools from every runtime
// on the bus, and an agent whose tools disappear without explanation is the
// failure MEMORY.md §7 spends its length avoiding.
func OpenSkillBook(ctx context.Context, s SkillStore) (*SkillBook, error) {
	b := NewSkillBook()
	b.store = s
	if s == nil {
		return b, nil
	}
	existing, err := s.Load(ctx)
	if err != nil {
		return nil, err
	}
	for _, sk := range existing {
		b.skills[sk.Name] = sk
	}
	return b, nil
}

var _ mcp.Provider = (*SkillBook)(nil)

// ProviderName identifies the book in the audit line.
func (b *SkillBook) ProviderName() string { return "relay-skills" }

// List returns every skill, newest name order stable for display.
func (b *SkillBook) List(context.Context) ([]Skill, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Skill, 0, len(b.skills))
	for _, s := range b.skills {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Author records a skill, replacing one of the same name.
//
// Replacing rather than refusing is the point of the feature: Hermes's idea
// worth stealing is that a skill improves *during use*, so the second version
// of a playbook is the normal case rather than a conflict.
func (b *SkillBook) Author(ctx context.Context, s Skill) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = b.now()
	}
	if s.Origin == "" {
		s.Origin = "orchestrator"
	}

	// Durable first. A skill that is on the bus but not in the file would
	// vanish at the next restart, and the model would have been told — and
	// would have told the user — that it is available from now on.
	if b.store != nil {
		if err := b.store.Save(ctx, s); err != nil {
			return err
		}
	}

	b.mu.Lock()
	b.skills[s.Name] = s
	dir := b.ExportDir
	b.mu.Unlock()

	if dir != "" {
		// A failed export is not a failed authoring. The skill is in the file
		// and on the bus, which is the path that matters; the SKILL.md copy is
		// the wider, weaker one and a read-only directory must not cost the
		// user the playbook.
		if _, err := ExportSkillMD(dir, s); err != nil {
			return &ErrExport{Skill: s.Name, Err: err}
		}
	}
	return nil
}

// ErrExport means the skill was saved and published but the portable copy was
// not written. It is a distinct type so a caller can tell "you have the skill"
// from "you do not".
type ErrExport struct {
	Skill string
	Err   error
}

func (e *ErrExport) Error() string {
	return "orchestrator: " + e.Skill + " is saved and on the bus, but the SKILL.md copy failed: " + e.Err.Error()
}

func (e *ErrExport) Unwrap() error { return e.Err }

// Forget removes one.
func (b *SkillBook) Forget(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.skills[name]
	delete(b.skills, name)
	return ok
}

// Tools renders every skill as a tool on the shared bus.
//
// Note what the handler does and does not do. It returns the instructions and
// stops. There is no execution here and there is nowhere for it to go: Relay
// has no browser, no shell and no filesystem tool, and the agent that called
// this one has all three. The tool is a way of handing over knowledge, and the
// asymmetry is the architecture rather than an unfinished part of it.
func (b *SkillBook) Tools(context.Context) []mcp.Tool {
	b.mu.RLock()
	defer b.mu.RUnlock()

	out := make([]mcp.Tool, 0, len(b.skills))
	for _, s := range b.skills {
		skill := s
		out = append(out, mcp.Tool{
			Name:        mcp.ToolName(SkillConnector, skill.Name),
			Title:       skill.Title,
			Description: skillDescription(skill),
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"context": map[string]any{
						"type":        "string",
						"description": "Anything specific about this run — a branch, an environment, a date.",
					},
				},
				"additionalProperties": false,
			},
			Connector: SkillConnector,
			Access:    mcp.AccessRead,
			// No Consequence: reading a playbook changes nothing outside this
			// machine. Whatever the agent then does with it is confirmed by
			// the tools that actually do it, which is where the confirmation
			// belongs — asking twice trains people to stop reading the ask.
			Handler: func(_ context.Context, c mcp.Call) (mcp.Result, error) {
				var b strings.Builder
				fmt.Fprintf(&b, "%s\n\n", skill.Steps)
				if len(skill.Needs) > 0 {
					fmt.Fprintf(&b, "This needs: %s. If you do not have one of these, say so rather than improvising.\n",
						strings.Join(skill.Needs, ", "))
				}
				if extra, _ := c.Arguments["context"].(string); strings.TrimSpace(extra) != "" {
					fmt.Fprintf(&b, "\nFor this run: %s\n", extra)
				}
				return mcp.Result{Text: b.String(), Target: skill.Name}, nil
			},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func skillDescription(s Skill) string {
	var b strings.Builder
	b.WriteString("A Relay playbook. Call this ")
	b.WriteString(strings.TrimSuffix(strings.TrimSpace(s.When), "."))
	b.WriteString(". It returns instructions for you to carry out; it does not do anything itself.")
	if len(s.Needs) > 0 {
		fmt.Fprintf(&b, " Assumes you have: %s.", strings.Join(s.Needs, ", "))
	}
	return b.String()
}
