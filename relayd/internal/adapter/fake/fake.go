// Package fake is an in-memory adapter for testing the registry, the router
// and anything else that consumes normalized events, without a runtime
// installed.
//
// No agent runtimes exist in the build container — no codex, openclaw, hermes
// or opencode binary, no config directories — so every test above the adapter
// layer runs against this. It is deliberately scriptable rather than clever:
// you push events and it emits them, in order, on the session's channel.
//
// It also defends the invariant the real adapters have to keep: a fake session
// built with a capability set that says SupportNo for steering refuses to
// steer, exactly as an ACP session does. Tests that want a runtime which
// cannot do something get one by asking for it, rather than by mocking it.
package fake

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/event"
)

// Options configures a fake adapter.
type Options struct {
	// Runtime defaults to adapter.ClaudeCode.
	Runtime adapter.Runtime
	// Caps defaults to adapter.Baseline(Runtime).
	Caps *adapter.Capabilities
	// Buffer is the per-session event channel depth. Default 64.
	Buffer int
	// Clock defaults to time.Now.
	Clock func() time.Time
	// StartErr, if set, is returned by Start.
	StartErr error
	// ResumeErr, if set, is returned by Resume.
	ResumeErr error
}

// Adapter is an in-memory adapter.Adapter.
type Adapter struct {
	opts Options

	mu       sync.Mutex
	sessions []*Session
	closed   bool
}

var _ adapter.Adapter = (*Adapter)(nil)

// New builds a fake adapter.
func New(opts Options) *Adapter {
	if opts.Runtime == "" {
		opts.Runtime = adapter.ClaudeCode
	}
	if opts.Buffer <= 0 {
		opts.Buffer = 64
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.Caps == nil {
		c := adapter.Baseline(opts.Runtime)
		opts.Caps = &c
	}
	return &Adapter{opts: opts}
}

func (a *Adapter) Runtime() adapter.Runtime           { return a.opts.Runtime }
func (a *Adapter) Capabilities() adapter.Capabilities { return *a.opts.Caps }

// Sessions returns every session this adapter has opened, closed or not.
func (a *Adapter) Sessions() []*Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*Session(nil), a.sessions...)
}

func (a *Adapter) Start(ctx context.Context, opts adapter.SessionOptions) (adapter.Session, error) {
	if a.opts.StartErr != nil {
		return nil, a.opts.StartErr
	}
	return a.open(opts, "")
}

func (a *Adapter) Resume(ctx context.Context, ref adapter.SessionRef, opts adapter.SessionOptions) (adapter.Session, error) {
	if a.opts.ResumeErr != nil {
		return nil, a.opts.ResumeErr
	}
	if err := a.Capabilities().Require(adapter.CapResume); err != nil {
		return nil, err
	}
	if opts.ID == "" {
		opts.ID = ref.ID
	}
	return a.open(opts, ref.Native)
}

func (a *Adapter) open(opts adapter.SessionOptions, native string) (adapter.Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, adapter.ErrSessionClosed
	}
	id := opts.ID
	if id == "" {
		id = uuid.NewString()
	}
	if native == "" {
		native = id
	}
	s := &Session{
		id:      id,
		native:  native,
		runtime: a.opts.Runtime,
		caps:    *a.opts.Caps,
		clock:   a.opts.Clock,
		events:  make(chan event.Event, a.opts.Buffer),
		opts:    opts,
	}
	a.sessions = append(a.sessions, s)
	return s, nil
}

func (a *Adapter) Close(ctx context.Context) error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	ss := append([]*Session(nil), a.sessions...)
	a.mu.Unlock()

	for _, s := range ss {
		_ = s.Close(ctx)
	}
	return nil
}

// Session is an in-memory adapter.Session.
type Session struct {
	id      string
	native  string
	runtime adapter.Runtime
	clock   func() time.Time
	opts    adapter.SessionOptions

	mu      sync.Mutex
	caps    adapter.Capabilities
	events  chan event.Event
	closed  bool
	seq     uint64
	turn    string
	sent    []adapter.Turn
	steered []Steer
	cancels []string
	replies []event.Reply
	asks    []*event.NeedsInput

	// OnSend, when set, runs in its own goroutine after each Send so a test can
	// script a turn against the session it was sent to.
	OnSend func(s *Session, turnID string, t adapter.Turn)
}

var _ adapter.Session = (*Session)(nil)

// Steer records a mid-turn steer.
type Steer struct {
	TurnID string
	Turn   adapter.Turn
}

func (s *Session) ID() string                      { return s.id }
func (s *Session) Native() string                  { return s.native }
func (s *Session) Runtime() adapter.Runtime        { return s.runtime }
func (s *Session) Options() adapter.SessionOptions { return s.opts }
func (s *Session) Events() <-chan event.Event      { return s.events }

func (s *Session) Capabilities() adapter.Capabilities {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.caps
}

// SetCapabilities narrows this session's capabilities the way a real handshake
// does — an ACP initialize response, or a Claude Code system/init that reports
// an auto permission mode.
func (s *Session) SetCapabilities(c adapter.Capabilities) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.caps = c
}

func (s *Session) Send(ctx context.Context, t adapter.Turn) (string, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", adapter.ErrSessionClosed
	}
	caps := s.caps
	s.mu.Unlock()

	if err := adapter.CheckTurn(caps, t); err != nil {
		return "", err
	}

	turnID := t.ID
	if turnID == "" {
		turnID = uuid.NewString()
	}

	s.mu.Lock()
	s.turn = turnID
	s.sent = append(s.sent, t)
	hook := s.OnSend
	s.mu.Unlock()

	if hook != nil {
		go hook(s, turnID, t)
	}
	return turnID, nil
}

func (s *Session) Steer(ctx context.Context, turnID string, t adapter.Turn) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return adapter.ErrSessionClosed
	}
	caps, active := s.caps, s.turn
	s.mu.Unlock()

	if err := caps.Require(adapter.CapSteer); err != nil {
		return err
	}
	if turnID != active {
		return fmt.Errorf("%w: %s", adapter.ErrTurnNotActive, turnID)
	}

	s.mu.Lock()
	s.steered = append(s.steered, Steer{TurnID: turnID, Turn: t})
	s.mu.Unlock()
	return nil
}

func (s *Session) Cancel(ctx context.Context, turnID string) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return adapter.ErrSessionClosed
	}
	caps := s.caps
	s.cancels = append(s.cancels, turnID)
	asks := append([]*event.NeedsInput(nil), s.asks...)
	s.mu.Unlock()

	if err := caps.Require(adapter.CapCancel); err != nil {
		return err
	}
	// ADAPTERS.md §4: every outstanding request_permission has to be resolved
	// or the agent's turn cannot unwind. A real adapter answers with the
	// cancelled outcome; here the question is simply withdrawn.
	for _, a := range asks {
		a.Withdraw("turn cancelled")
	}
	return nil
}

func (s *Session) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	for _, a := range s.asks {
		a.Withdraw("session closed")
	}
	close(s.events)
	return nil
}

// Sent returns every turn sent to this session.
func (s *Session) Sent() []adapter.Turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]adapter.Turn(nil), s.sent...)
}

// Steers returns every mid-turn steer that was accepted.
func (s *Session) Steers() []Steer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Steer(nil), s.steered...)
}

// Cancels returns the turn ids Cancel was called with.
func (s *Session) Cancels() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cancels...)
}

// Replies returns every answer that came back through a NeedsInput raised by
// Ask, in order.
func (s *Session) Replies() []event.Reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]event.Reply(nil), s.replies...)
}

// Meta builds an envelope for this session's next event.
func (s *Session) Meta(turnID string) event.Meta {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return event.Meta{
		Runtime: string(s.runtime),
		Session: s.id,
		Turn:    turnID,
		At:      s.clock(),
		Seq:     s.seq,
	}
}

// Emit pushes an event onto the stream. It returns false if the session is
// closed or the buffer is full, rather than blocking a test forever.
func (s *Session) Emit(ev event.Event) bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false
	}
	ch := s.events
	s.mu.Unlock()

	select {
	case ch <- ev:
		return true
	default:
		return false
	}
}

// Ask raises a NeedsInput whose replies land in Replies. The returned pointer
// is the same one that goes onto the event stream, so a test can assert on the
// outcome after the consumer answers it.
func (s *Session) Ask(turnID string, spec event.InputSpec) *event.NeedsInput {
	n := event.NewNeedsInput(s.Meta(turnID), spec, func(ctx context.Context, r event.Reply) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return adapter.ErrSessionClosed
		}
		s.replies = append(s.replies, r)
		return nil
	})

	s.mu.Lock()
	s.asks = append(s.asks, n)
	s.mu.Unlock()

	s.Emit(n)
	return n
}

// HappyTurn emits a complete, uneventful turn: started, one text delta, one
// tool call and its output, then completed. It is the default script for tests
// that need a session to do something plausible.
func HappyTurn(s *Session, turnID, text string) {
	s.Emit(event.TurnStarted{Meta: s.Meta(turnID)})
	s.Emit(event.TextDelta{Meta: s.Meta(turnID), Text: text})
	s.Emit(event.ToolStarted{Meta: s.Meta(turnID), ID: "tool-1", Tool: "Bash", Target: "go test ./..."})
	s.Emit(event.ToolOutput{Meta: s.Meta(turnID), ID: "tool-1", Chunk: "ok\n", Status: event.ToolCompleted})
	s.Emit(event.TurnCompleted{
		Meta:       s.Meta(turnID),
		OK:         true,
		StopReason: event.StopEndTurn,
		Duration:   1200 * time.Millisecond,
	})
}
