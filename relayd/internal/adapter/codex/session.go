package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/event"
)

// Session is one Codex thread. Many of them share one [Adapter] and therefore
// one `codex app-server` process and one JSON-RPC connection.
type Session struct {
	a   *Adapter
	log *slog.Logger

	id        string // Relay's id
	workspace string

	src  *metaSource
	q    *queue
	norm *normalizer

	ready     chan struct{} // closed when thread/started has named the thread
	readyOnce sync.Once

	sendMu sync.Mutex // one turn at a time, so a turn/started binds unambiguously

	mu         sync.Mutex
	thread     string
	caps       adapter.Capabilities
	activeTurn string
	turnWaiter chan string
	pending    map[string]*pendingRequest
	blocked    []string
	closed     bool
	needs      needsInputState
}

var (
	_ adapter.Session = (*Session)(nil)
	_ Compactor       = (*Session)(nil)
)

// Compactor is a session that can be compacted on demand. MEMORY.md §9 drives
// idle compaction through this; the interface exists so a caller can ask
// without importing this package's concrete type.
type Compactor interface {
	Compact(ctx context.Context) error
}

// needsInputState is why CapNeedsInput is where it is. All three reasons can be
// true at once and the note has to say which, or "the glasses never ask me
// anything" reads as a feature until something destructive runs unattended.
type needsInputState struct {
	policy     string // approvalPolicy, from thread/settings/updated
	reviewer   string // approvalsReviewer
	autoReview bool   // an item/autoApprovalReview/* was seen
	unverified map[string]bool
}

func newSession(a *Adapter, id, workspace string) *Session {
	src := &metaSource{
		runtime: string(adapter.Codex),
		session: id,
		now:     a.now,
	}
	s := &Session{
		a:         a,
		log:       a.log.With("runtime", "codex", "session", id),
		id:        id,
		workspace: workspace,
		src:       src,
		q:         newQueue(),
		ready:     make(chan struct{}),
		pending:   map[string]*pendingRequest{},
		caps:      a.Capabilities(),
		needs:     needsInputState{unverified: map[string]bool{}},
	}
	s.norm = newNormalizer(s.emit, src, s.log, hooks{
		onThreadStarted:   s.bindThread,
		onTurnStarted:     s.turnStarted,
		onTurnCompleted:   s.turnCompleted,
		onSettings:        s.applySettings,
		onAutoApproval:    s.noteAutoApproval,
		onStatus:          s.applyStatus,
		onRequestResolved: func(id json.RawMessage) { s.withdraw(id, "answered outside Relay") },
		onThreadGone:      func(method string) { s.finish("codex reported "+method, true) },
	})
	return s
}

func (s *Session) ID() string                 { return s.id }
func (s *Session) Runtime() adapter.Runtime   { return adapter.Codex }
func (s *Session) Events() <-chan event.Event { return s.q.events() }
func (s *Session) conn() *conn                { return s.a.c }
func (s *Session) now() time.Time             { return s.a.now() }
func (s *Session) emit(e event.Event)         { s.q.push(e) }
func (s *Session) Workspace() string          { return s.workspace }
func (s *Session) DroppedEvents() int         { return s.q.droppedCount() }
func (s *Session) LastUsage() *event.Usage    { return s.norm.usage() }
func (s *Session) Compactions() int           { return s.norm.compactionCount() }
func (s *Session) Capabilities() adapter.Capabilities {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.caps
}

// Native is Codex's own thread id, which Relay learns from the `thread/started`
// notification and never from a result.
func (s *Session) Native() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.thread
}

// Blocked reports the `thread/status/changed` active flags — waitingOnApproval,
// waitingOnUserInput — currently set on this thread.
//
// This is deliberately *not* turned into a NeedsInput. It is a second,
// independent signal that a session is stuck, and the only one that survives a
// reconnect; but after a reconnect the JSON-RPC request it refers to is gone,
// so there is no reply path and a question nobody can answer is a hung session.
// Surfacing it as state lets the console say "blocked" honestly.
func (s *Session) Blocked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.blocked...)
}

// ActiveTurn is the turn Codex says is running, "" between turns.
func (s *Session) ActiveTurn() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeTurn
}

// ---- lifecycle hooks, all called on the connection's reader goroutine ----

func (s *Session) setThread(id string) {
	s.mu.Lock()
	s.thread = id
	s.mu.Unlock()
}

func (s *Session) bindThread(t thread) {
	s.setThread(t.ID)
	s.log.Info("codex: thread started", "thread", t.ID, "session_tree", t.SessionID, "cli", t.CliVersion)
	s.readyOnce.Do(func() { close(s.ready) })
}

func (s *Session) turnStarted(turnID string) {
	s.mu.Lock()
	s.activeTurn = turnID
	w := s.turnWaiter
	s.turnWaiter = nil
	s.mu.Unlock()
	if w != nil {
		w <- turnID
	}
}

func (s *Session) turnCompleted(turnID string) {
	s.mu.Lock()
	if s.activeTurn == turnID {
		s.activeTurn = ""
	}
	s.mu.Unlock()
}

// applySettings is the live trap detector. `approvalPolicy: "never"` disables
// approvals outright; `approvalsReviewer` anything but "user" routes them to a
// subagent that decides on our behalf. Either one and the five approval
// requests simply never arrive.
func (s *Session) applySettings(set threadSettings) {
	s.mu.Lock()
	s.needs.policy = set.approvalPolicyName()
	s.needs.reviewer = set.ApprovalsReviewer
	s.recomputeNeedsInput()
	caps := s.caps
	s.mu.Unlock()
	s.log.Info("codex: thread settings", "approval_policy", set.approvalPolicyName(),
		"approvals_reviewer", set.ApprovalsReviewer, "model", set.Model,
		"needs_input", caps.Get(adapter.CapNeedsInput).String())
}

func (s *Session) noteAutoApproval(reviewID string) {
	s.mu.Lock()
	first := !s.needs.autoReview
	s.needs.autoReview = true
	s.recomputeNeedsInput()
	s.mu.Unlock()
	if first {
		s.log.Warn("codex: approvals are being answered by the auto-review subagent, not by the user",
			"review", reviewID)
	}
}

func (s *Session) noteUnverified(method string) {
	s.mu.Lock()
	s.needs.unverified[method] = true
	s.recomputeNeedsInput()
	s.mu.Unlock()
}

// recomputeNeedsInput must be called with s.mu held.
func (s *Session) recomputeNeedsInput() {
	var reasons []string
	blocked := false

	switch s.needs.policy {
	case "":
		// Nothing observed yet: keep the baseline.
	case "never":
		blocked = true
		reasons = append(reasons, `approvalPolicy is "never", so no approval request can arrive`)
	}
	switch s.needs.reviewer {
	case "", "user":
	default:
		blocked = true
		reasons = append(reasons, fmt.Sprintf("approvalsReviewer is %q, so approvals are decided by a subagent", s.needs.reviewer))
	}
	if s.needs.autoReview {
		blocked = true
		reasons = append(reasons, "item/autoApprovalReview/* observed: a subagent is deciding")
	}
	if len(s.needs.unverified) > 0 {
		m := make([]string, 0, len(s.needs.unverified))
		for k := range s.needs.unverified {
			m = append(m, k)
		}
		sort.Strings(m)
		reasons = append(reasons, "no verified reply shape for "+strings.Join(m, ", ")+" (ADAPTERS.md §8 item 7)")
	}

	level := adapter.SupportYes
	note := "server-to-client requests that block until answered"
	if blocked {
		level = adapter.SupportNo
	}
	if len(reasons) > 0 {
		note = strings.Join(reasons, "; ")
	}
	s.caps = s.caps.With(adapter.CapNeedsInput, level, note)
}

func (s *Session) applyStatus(st threadStatus) {
	s.mu.Lock()
	s.blocked = nil
	if st.Type == "active" {
		s.blocked = append(s.blocked, st.ActiveFlags...)
	}
	waiting := len(s.blocked) > 0
	answerable := len(s.pending) > 0
	s.mu.Unlock()

	if waiting && !answerable {
		// The flags say blocked and we hold no request to answer. That is what
		// a reconnect looks like: the question outlived our copy of it.
		s.log.Warn("codex: thread is blocked but Relay holds no answerable request",
			"flags", st.ActiveFlags)
	}
}

// ---- adapter.Session ----

func (s *Session) Send(ctx context.Context, t adapter.Turn) (string, error) {
	if err := adapter.CheckTurn(s.Capabilities(), t); err != nil {
		return "", err
	}
	input, err := toInput(t)
	if err != nil {
		return "", err
	}

	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", adapter.ErrSessionClosed
	}
	thread := s.thread
	waiter := make(chan string, 1)
	s.turnWaiter = waiter
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		if s.turnWaiter == waiter {
			s.turnWaiter = nil
		}
		s.mu.Unlock()
	}()

	p := turnStartParams{
		ThreadID:            thread,
		Input:               input,
		ClientUserMessageID: t.ID,
	}
	if _, err := s.conn().call(ctx, "turn/start", p); err != nil {
		return "", fmt.Errorf("codex: turn/start: %w", err)
	}

	// The turn id is not read out of the result: there is no ServerResponse
	// schema and nothing says a result carries one. `turn/started` does, and it
	// is required there.
	select {
	case id := <-waiter:
		return id, nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-s.conn().done():
		return "", adapter.ErrSessionClosed
	}
}

// Steer injects into a turn that is already running. Codex is one of only two
// runtimes that can do this at all.
func (s *Session) Steer(ctx context.Context, turnID string, t adapter.Turn) error {
	caps := s.Capabilities()
	if err := caps.Require(adapter.CapSteer); err != nil {
		return err
	}
	if err := adapter.CheckTurn(caps, t); err != nil {
		return err
	}
	input, err := toInput(t)
	if err != nil {
		return err
	}
	if turnID == "" {
		// expectedTurnId is a precondition, not a convenience. Guessing the
		// active turn here would defeat the point of the precondition.
		return fmt.Errorf("%w: turn/steer requires an expectedTurnId", adapter.ErrTurnNotActive)
	}

	s.mu.Lock()
	closed, active, thread := s.closed, s.activeTurn, s.thread
	s.mu.Unlock()
	if closed {
		return adapter.ErrSessionClosed
	}
	if active != turnID {
		return fmt.Errorf("%w: %s is not the active turn (%q is)", adapter.ErrTurnNotActive, turnID, active)
	}

	_, err = s.conn().call(ctx, "turn/steer", turnSteerParams{
		ThreadID:            thread,
		ExpectedTurnID:      turnID,
		Input:               input,
		ClientUserMessageID: t.ID,
	})
	if err == nil {
		return nil
	}
	// Classify by what we can observe rather than by matching the error prose:
	// if the turn stopped being active while the call was in flight, the
	// precondition is exactly what failed.
	s.mu.Lock()
	stillActive := s.activeTurn == turnID
	s.mu.Unlock()
	if !stillActive {
		return fmt.Errorf("%w: %s finished before the steer landed: %w", adapter.ErrTurnNotActive, turnID, err)
	}
	return fmt.Errorf("codex: turn/steer: %w", err)
}

func (s *Session) Cancel(ctx context.Context, turnID string) error {
	if err := s.Capabilities().Require(adapter.CapCancel); err != nil {
		return err
	}
	s.mu.Lock()
	closed := s.closed
	if turnID == "" {
		turnID = s.activeTurn
	}
	s.mu.Unlock()
	if closed {
		return adapter.ErrSessionClosed
	}
	if turnID == "" {
		return fmt.Errorf("%w: nothing is running", adapter.ErrTurnNotActive)
	}
	// Answer everything outstanding first: a turn cannot unwind while Codex is
	// still blocked on a question, and turn/interrupt does not answer them.
	s.cancelPending(turnID, "the turn was cancelled")
	return s.interrupt(ctx, turnID)
}

func (s *Session) interrupt(ctx context.Context, turnID string) error {
	s.mu.Lock()
	thread := s.thread
	s.mu.Unlock()
	if _, err := s.conn().call(ctx, "turn/interrupt", turnInterruptParams{ThreadID: thread, TurnID: turnID}); err != nil {
		return fmt.Errorf("codex: turn/interrupt: %w", err)
	}
	return nil
}

// Compact asks Codex to compact this thread now. `thread/compact/start` takes
// `{threadId}` and nothing else — no threshold, no mode — so the policy
// (MEMORY.md §9's compact-on-idle at ~70%) lives entirely on our side.
func (s *Session) Compact(ctx context.Context) error {
	s.mu.Lock()
	closed, thread := s.closed, s.thread
	s.mu.Unlock()
	if closed {
		return adapter.ErrSessionClosed
	}
	if _, err := s.conn().call(ctx, "thread/compact/start", threadIDParams{ThreadID: thread}); err != nil {
		return fmt.Errorf("codex: thread/compact/start: %w", err)
	}
	return nil
}

// Unsubscribe stops this thread's notifications without ending it. There is no
// `thread/subscribe`: subscription is implicit in start/resume/fork, so this is
// the only half of the pair that exists.
func (s *Session) Unsubscribe(ctx context.Context) error {
	s.mu.Lock()
	thread := s.thread
	s.mu.Unlock()
	if thread == "" {
		return nil
	}
	if _, err := s.conn().call(ctx, "thread/unsubscribe", threadIDParams{ThreadID: thread}); err != nil {
		return fmt.Errorf("codex: thread/unsubscribe: %w", err)
	}
	return nil
}

func (s *Session) Close(ctx context.Context) error {
	s.mu.Lock()
	already := s.closed
	s.mu.Unlock()

	var unsubErr error
	if !already {
		// Detach before tearing down, so app-server stops serialising events at
		// us for a conversation nobody is watching. Bounded: a Close that hangs
		// because the runtime stopped answering is worse than a Close that
		// leaves one subscription behind.
		unsubCtx := ctx
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			unsubCtx, cancel = context.WithTimeout(ctx, unsubscribeTimeout)
			defer cancel()
		}
		if err := s.Unsubscribe(unsubCtx); err != nil && !errors.Is(err, ErrConnClosed) {
			unsubErr = err
		}
	}
	s.finish("the session was closed", false)
	s.a.forget(s)
	s.q.closeAndDrain(s.a.opts.DrainGrace)
	return unsubErr
}

// finish marks the session over. It never blocks, because it is also called
// from the reader goroutine when Codex says the thread is gone.
func (s *Session) finish(reason string, fatal bool) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	turn := s.activeTurn
	s.activeTurn = ""
	s.mu.Unlock()

	s.cancelPending("", reason)
	if fatal {
		s.emit(event.Error{
			Meta:    s.src.meta(turn),
			Code:    "session_gone",
			Message: reason,
			Fatal:   true,
		})
	}
	s.readyOnce.Do(func() { close(s.ready) })
	s.q.close()
}

// toInput maps a Turn onto `UserInput[]`. Only the text variant is produced:
// image and localImage are gated behind prompt capabilities nobody has probed
// on Codex, and mapping a resource_link onto the `mention` variant (which wants
// `{name, path}`, not a URI) would be a guess.
func toInput(t adapter.Turn) ([]userInput, error) {
	var parts []string
	if t.Text != "" {
		parts = append(parts, t.Text)
	}
	for _, b := range t.Blocks {
		switch b.Kind {
		case adapter.BlockText:
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		default:
			return nil, fmt.Errorf("%w: codex prompts carry text only; %s blocks have no verified UserInput mapping (ADAPTERS.md §8)",
				adapter.ErrUnsupported, b.Kind)
		}
	}
	if len(parts) == 0 {
		return nil, errors.New("codex: a turn needs some text")
	}
	return textInput(strings.Join(parts, "\n\n")), nil
}

func msDuration(ms int64) time.Duration { return time.Duration(ms) * time.Millisecond }

// unsubscribeTimeout bounds the detach in Close when the caller passed a
// context with no deadline of its own.
const unsubscribeTimeout = 5 * time.Second
