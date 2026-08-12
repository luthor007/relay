package event

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Errors returned by NeedsInput.Reply.
var (
	// ErrAnswered means this question was already resolved. Replies are
	// single-shot: three sessions asking is three pings, but one session asking
	// is one answer.
	ErrAnswered = errors.New("event: this question was already answered")

	// ErrWithdrawn means the runtime withdrew the question before we answered.
	// Codex's serverRequest/resolved notification is exactly this: an approval
	// answered somewhere else, in a terminal. Without honouring it a Relay ping
	// outlives its question and wakes the user to approve something already
	// approved.
	ErrWithdrawn = errors.New("event: the question was withdrawn before it was answered")

	// ErrUnknownOption means the reply named an option the agent did not offer.
	// ADAPTERS.md §4: options are agent-supplied and open-ended, and we never
	// offer — or send back — one that was not in the array.
	ErrUnknownOption = errors.New("event: no such option on this question")
)

// InputKind is what the runtime is blocked on. Every one of these is a
// server→client request that blocks until answered, which is what makes voice
// answers real rather than aspirational (ADAPTERS.md §7).
type InputKind string

const (
	// InputPermission covers Claude Code's permission-prompt MCP tool, Codex's
	// item/{commandExecution,fileChange,permissions}/requestApproval, and ACP's
	// session/request_permission.
	InputPermission InputKind = "permission"
	// InputToolValue is Codex item/tool/requestUserInput — a tool wants a value.
	InputToolValue InputKind = "tool_value"
	// InputElicitation is Codex mcpServer/elicitation/request — an MCP server
	// is asking.
	InputElicitation InputKind = "elicitation"
)

// OptionKind is the agent's UI hint about an option, not a fixed menu.
// PermissionOptionKind in ACP; the same four shapes appear on the other two.
type OptionKind string

const (
	OptionAllowOnce    OptionKind = "allow_once"
	OptionAllowAlways  OptionKind = "allow_always"
	OptionRejectOnce   OptionKind = "reject_once"
	OptionRejectAlways OptionKind = "reject_always"
	OptionOther        OptionKind = "other"
)

// Standing reports whether choosing this option grants something beyond the
// single action in front of us.
//
// ORCHESTRATOR.md §4b requires consequential actions to be confirmed every
// time, so the orchestrator must never select a standing option on the user's
// behalf. The option is still carried — ADAPTERS.md §4 says we speak the names
// we were given — but a human has to pick it.
func (k OptionKind) Standing() bool {
	return k == OptionAllowAlways || k == OptionRejectAlways
}

// Option is one answer the agent will accept. Name is spoken verbatim.
type Option struct {
	ID   string
	Name string
	Kind OptionKind
}

// ToolRef describes the tool call a permission request is about. Every field
// may be empty: ACP sends a ToolCallUpdate where only toolCallId is required,
// so there is no guarantee of a human-readable description to read aloud. When
// it is missing, say what is known — do not infer one from RawInput.
type ToolRef struct {
	ID       string
	Name     string
	Title    string
	Kind     string
	RawInput map[string]any
}

// Decision is the shape of an answer that survives all three runtimes.
type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionDeny  Decision = "deny"
	// DecisionCancelled is ACP's cancelled outcome, and the answer an adapter
	// must send for every outstanding request when a turn is cancelled — the
	// agent's turn cannot unwind otherwise.
	DecisionCancelled Decision = "cancelled"
)

// Reply is an answer to a NeedsInput, in terms the orchestrator can form
// without knowing which runtime asked.
//
// OptionID is authoritative when the question carried options; Decision is the
// coarse intent the adapter maps onto its own vocabulary when it did not.
//
// Interrupt is the hard stop — "no, stop" spoken at the glasses. Each runtime
// spells it differently and none of them fold it into the option: Claude Code
// sets interrupt:true on a deny, Codex sends cancel rather than decline, and
// ACP has no hard stop inside the permission response at all, so an ACP adapter
// must answer reject and then send session/cancel.
type Reply struct {
	OptionID     string
	Decision     Decision
	Interrupt    bool
	Message      string
	UpdatedInput map[string]any
}

// ReplyFunc resolves a question back through the adapter that raised it. Codex
// resolves a pending JSON-RPC request, Claude Code returns from the
// permission-prompt MCP tool, ACP answers session/request_permission — and
// NeedsInput does not know or care which.
type ReplyFunc func(ctx context.Context, r Reply) error

// InputSpec is everything an adapter knows about a question at the moment it
// raises one.
type InputSpec struct {
	Ask     InputKind
	Prompt  string
	Options []Option
	Tool    *ToolRef

	// Deadline is when the runtime will stop waiting, zero if it will wait
	// indefinitely. Claude Code's default MCP tool-call timeout is 1e8 ms —
	// about 27.8 hours — which is what makes "ping, walk away, answer an hour
	// later" fit inside the budget.
	Deadline time.Time
}

// NeedsInput is a session blocked on a human. ⚑PING — blocking.
//
// It is the only event that carries a way back into the runtime. Use
// [NewNeedsInput]; the zero value has no reply path and is not usable.
type NeedsInput struct {
	Meta
	Ask      InputKind
	Prompt   string
	Options  []Option
	Tool     *ToolRef
	Deadline time.Time

	mu       sync.Mutex
	done     chan struct{}
	reply    ReplyFunc
	outcome  *Reply
	answered bool
	err      error
}

// NewNeedsInput builds a question with its reply path attached. reply must be
// non-nil: a question nobody can answer is a hung session, and the constructor
// is the only place that invariant can be enforced.
func NewNeedsInput(m Meta, spec InputSpec, reply ReplyFunc) *NeedsInput {
	if reply == nil {
		panic("event: NewNeedsInput requires a reply function")
	}
	return &NeedsInput{
		Meta:     m,
		Ask:      spec.Ask,
		Prompt:   spec.Prompt,
		Options:  spec.Options,
		Tool:     spec.Tool,
		Deadline: spec.Deadline,
		done:     make(chan struct{}),
		reply:    reply,
	}
}

func (*NeedsInput) Kind() Kind { return KindNeedsInput }

func (n *NeedsInput) Ping() Ping {
	if n.Replay {
		return PingNone
	}
	return PingBlocking
}

func (*NeedsInput) relayEvent() {}

// Option looks up an offered option by id.
func (n *NeedsInput) Option(id string) (Option, bool) {
	for _, o := range n.Options {
		if o.ID == id {
			return o, true
		}
	}
	return Option{}, false
}

// Reply answers the question and unblocks the runtime. It is single-shot:
// the second call returns ErrAnswered, and a call after Withdraw returns
// ErrWithdrawn.
func (n *NeedsInput) Reply(ctx context.Context, r Reply) error {
	n.mu.Lock()
	if n.answered {
		err := n.err
		n.mu.Unlock()
		if err != nil {
			return err
		}
		return ErrAnswered
	}
	if r.OptionID != "" {
		if _, ok := n.Option(r.OptionID); !ok {
			n.mu.Unlock()
			return fmt.Errorf("%w: %q", ErrUnknownOption, r.OptionID)
		}
	}
	fn := n.reply
	n.mu.Unlock()

	if err := fn(ctx, r); err != nil {
		// The runtime refused the answer; the question is still open so the
		// caller can try again with a different option.
		return err
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	if n.answered {
		// Withdrawn while the reply was in flight. The runtime already moved
		// on, so report that rather than pretending the answer landed.
		if n.err != nil {
			return n.err
		}
		return ErrAnswered
	}
	n.answered = true
	n.outcome = &r
	close(n.done)
	return nil
}

// Withdraw resolves the question from the runtime's side without an answer:
// the approval was granted in a terminal, the turn was cancelled, the session
// died. Subsequent Reply calls fail with ErrWithdrawn and Done is closed, so a
// pending ping can be retracted.
func (n *NeedsInput) Withdraw(reason string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.answered {
		return
	}
	n.answered = true
	if reason == "" {
		n.err = ErrWithdrawn
	} else {
		n.err = fmt.Errorf("%w: %s", ErrWithdrawn, reason)
	}
	close(n.done)
}

// Done closes when the question is resolved, either way.
func (n *NeedsInput) Done() <-chan struct{} { return n.done }

// Outcome returns the reply that resolved this question. ok is false while it
// is still open and when it was withdrawn.
func (n *NeedsInput) Outcome() (Reply, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.outcome == nil {
		return Reply{}, false
	}
	return *n.outcome, true
}

// Answered reports whether the question is resolved, by reply or withdrawal.
func (n *NeedsInput) Answered() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.answered
}
