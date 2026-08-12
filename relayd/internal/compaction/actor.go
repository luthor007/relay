package compaction

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
)

// SessionSource hands out the live session for a Relay session id. The registry
// is the implementation; this package holds no sessions of its own.
type SessionSource interface {
	Session(id string) (adapter.Session, bool)
}

// Compactable is the optional method a runtime adapter implements when
// compaction is a protocol call rather than a slash command. The Codex adapter
// has it — thread/compact/start — and nothing else does, so this is a type
// assertion rather than a change to adapter.Session.
type Compactable interface {
	Compact(ctx context.Context) error
}

var (
	// ErrNoSession means the session is not live any more. A session that ended
	// between the sweep listing it and the sweep acting on it does not need
	// compacting.
	ErrNoSession = errors.New("compaction: no live session")

	// ErrNotWired is what an unconfigured half of [TurnCompactor] returns.
	ErrNotWired = errors.New("compaction: this action is not wired up")
)

// TurnCompactor is an [Actor] over the adapter interface.
//
// It does the two things that are purely mechanical — ask a runtime to compact,
// and send the silent memory pass — and delegates the two that are not.
// Handing work to a fresh session is routing's job (ORCHESTRATOR.md §4): it
// picks the runtime, announces the choice and can be undone, and none of that
// belongs to a package that decides when a context is full.
//
// # The flush reply is deliberately not read here
//
// A flush is an ordinary turn, and its answer arrives on the session's event
// channel, which has exactly one consumer. Competing with that consumer to read
// the reply would be a race with the registry over a channel it owns. So this
// sends the turn and stops; whoever consumes events recognises it with
// [IsFlush], parses it with [ReadFlush], and must not speak it, ping about it,
// or let it count as user activity.
type TurnCompactor struct {
	Sessions SessionSource

	// Handoffs and NewSessions are the delegated halves. Nil returns
	// [ErrNotWired], which surfaces on the outcome rather than being swallowed.
	Handoffs    func(ctx context.Context, v SessionView, b Brief) error
	NewSessions func(ctx context.Context, v SessionView) error

	Now func() time.Time
}

var _ Actor = (*TurnCompactor)(nil)

func (c *TurnCompactor) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

func (c *TurnCompactor) session(id string) (adapter.Session, error) {
	if c.Sessions == nil {
		return nil, ErrNotWired
	}
	s, ok := c.Sessions.Session(id)
	if !ok || s == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoSession, id)
	}
	return s, nil
}

// Compact drives the runtime's documented mechanism.
//
// The choice is made from [MechanismFor] and the session's own type, never from
// a guess: a runtime with no documented trigger gets an error rather than a
// hopeful "/compact" typed at whatever is listening.
func (c *TurnCompactor) Compact(ctx context.Context, v SessionView) error {
	m, ok := MechanismFor(v.Runtime)
	if !ok {
		return fmt.Errorf("compaction: no documented way to compact %s", label(v.Runtime))
	}
	sess, err := c.session(v.ID)
	if err != nil {
		return err
	}

	// A protocol call beats a command wherever the adapter offers one.
	if cc, ok := sess.(Compactable); ok && m.Kind == MechanismCall {
		return cc.Compact(ctx)
	}

	switch m.Kind {
	case MechanismCommand:
		// The slash command goes in as a user message, which is how every one
		// of these runtimes exposes it — verified for Claude Code in
		// --print/stream-json.
		if _, err := sess.Send(ctx, adapter.Turn{Text: m.Method}); err != nil {
			return fmt.Errorf("compaction: %s %s: %w", label(v.Runtime), m.Method, err)
		}
		return nil

	case MechanismHTTP:
		// OpenCode's summarize endpoint lives on `opencode serve`, outside ACP.
		// There is no ACP message for it, and sending "/compact" to an agent
		// that never advertised the command would be inventing a capability.
		return fmt.Errorf("compaction: %s compacts through %s, which is outside the agent protocol and not wired up",
			label(v.Runtime), m.Method)

	case MechanismCall:
		return fmt.Errorf("compaction: %s compacts through %s and this session does not implement it",
			label(v.Runtime), m.Method)
	}
	return fmt.Errorf("compaction: unknown mechanism %q for %s", m.Kind, label(v.Runtime))
}

// Flush sends the silent memory pass. It returns as soon as the turn is
// accepted: the point of doing this on idle is that nothing waits for it.
func (c *TurnCompactor) Flush(ctx context.Context, v SessionView) error {
	sess, err := c.session(v.ID)
	if err != nil {
		return err
	}
	if _, err := sess.Send(ctx, FlushTurn(v.ID, c.now())); err != nil {
		return fmt.Errorf("compaction: memory pass on %s: %w", v.ID, err)
	}
	return nil
}

// Handoff delegates.
func (c *TurnCompactor) Handoff(ctx context.Context, v SessionView, b Brief) error {
	if c.Handoffs == nil {
		return fmt.Errorf("%w: starting a fresh session and moving the work to it is routing's job", ErrNotWired)
	}
	return c.Handoffs(ctx, v, b)
}

// StartNew delegates.
func (c *TurnCompactor) StartNew(ctx context.Context, v SessionView) error {
	if c.NewSessions == nil {
		return fmt.Errorf("%w: deciding where a new session starts is routing's job", ErrNotWired)
	}
	return c.NewSessions(ctx, v)
}
