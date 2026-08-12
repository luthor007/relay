package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/event"
)

// Mode is how a new utterance interacts with a turn that is already running.
//
// ACP has no steer, so ADAPTERS.md §4's table is the whole answer and both
// halves of it live here as first-class operations. The small model decides
// which; the adapter makes both cheap and says which one happened.
type Mode string

const (
	// ModeAuto queues when a turn is running. It is the conservative default —
	// §4's "when unsure it queues and says which it chose".
	ModeAuto Mode = ""
	// ModeQueue is an addition: "also update the changelog". It waits for the
	// running turn to finish and then goes out as its own turn.
	ModeQueue Mode = "queue"
	// ModeRedirect is a redirect: "no, stop, do X instead". It cancels the
	// running turn and re-prompts. The cancel is safe because ACP's contract
	// says the agent flushes pending session/update notifications before
	// resolving, so nothing observed is lost.
	ModeRedirect Mode = "redirect"
)

// Disposition is what actually happened to an utterance.
type Disposition string

const (
	// DispositionStarted means nothing was running and the turn went straight out.
	DispositionStarted Disposition = "started"
	// DispositionQueued means it is waiting behind a running turn.
	DispositionQueued Disposition = "queued"
	// DispositionRedirect means the running turn was cancelled for this one.
	DispositionRedirect Disposition = "redirect"
)

// Delivery reports which half of §4's table was taken, so the orchestrator can
// announce it — "I'll add that when this finishes" versus "stopping, switching
// to that" — rather than guessing.
type Delivery struct {
	TurnID      string
	Disposition Disposition
	// QueueDepth is how many utterances are waiting, including this one.
	QueueDepth int
	// CancelledTurn and CancelledText describe what a redirect displaced. The
	// text is there because merging the instruction is the small model's job,
	// not the adapter's, and it needs to know what it interrupted.
	CancelledTurn string
	CancelledText string
}

// turn is one session/prompt in flight or waiting to be.
type turn struct {
	id      string
	spec    adapter.Turn
	blocks  []contentBlock
	started time.Time
	done    chan struct{}

	mu         sync.Mutex
	cancelling bool
	claimed    bool
	stop       event.StopReason
	err        error
}

// claim reserves the right to finish this turn. Exactly one caller gets it —
// the prompt goroutine when the request resolves, or Session.finish when the
// connection dies underneath it — so `done` is closed once and TurnCompleted is
// emitted once.
func (t *turn) claim() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.claimed {
		return false
	}
	t.claimed = true
	return true
}

// finishNow records the outcome and unblocks everyone waiting. It runs after
// TurnCompleted has been queued, so a Cancel that returns implies the event is
// already on its way to the consumer.
func (t *turn) finishNow(stop event.StopReason, err error) {
	t.mu.Lock()
	t.stop = stop
	t.err = err
	t.mu.Unlock()
	close(t.done)
}

func (t *turn) markCancelling() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cancelling {
		return false
	}
	t.cancelling = true
	return true
}

func (t *turn) isCancelling() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cancelling
}

// Session is one ACP conversation.
type Session struct {
	a       *Adapter
	id      string
	native  string
	cwd     string
	runtime adapter.Runtime
	q       *queue
	log     *slog.Logger

	seq       atomic.Uint64
	replaying atomic.Bool

	mu          sync.Mutex
	caps        adapter.Capabilities
	active      *turn
	pending     []*turn
	held        bool
	redirecting bool
	turnN       int
	closed      bool

	tools map[string]*toolState
	perms map[string]*pendingPermission

	modes       *SessionModeState
	models      *SessionModelState
	currentMode string
	commands    []AvailableCommand

	unknownUpdates map[string]int
	droppedContent int

	// boot holds traffic that arrived while the session was still registering.
	boot   []inbound
	booted bool

	wake       chan struct{}
	stopPump   chan struct{}
	pumpDone   chan struct{}
	finishOnce sync.Once
}

var _ adapter.Session = (*Session)(nil)

func newSession(a *Adapter, id, native, cwd string) *Session {
	s := &Session{
		a:              a,
		id:             id,
		native:         native,
		cwd:            cwd,
		runtime:        a.opts.Runtime,
		q:              newQueue(),
		log:            a.log.With("session", id, "native", native),
		caps:           a.caps,
		tools:          map[string]*toolState{},
		perms:          map[string]*pendingPermission{},
		unknownUpdates: map[string]int{},
		wake:           make(chan struct{}, 1),
		stopPump:       make(chan struct{}),
		pumpDone:       make(chan struct{}),
	}
	go s.pump()
	return s
}

// ID is Relay's session id.
func (s *Session) ID() string { return s.id }

// Native is the runtime's own session id — an ACP `sessionId`, which on
// OpenClaw is a Gateway key like "agent:main:main". It is what the registry
// reconciles against the runtime's own store.
func (s *Session) Native() string { return s.native }

// Runtime is which of the three this session belongs to.
func (s *Session) Runtime() adapter.Runtime { return s.runtime }

// Workspace is the absolute cwd the session was opened with.
func (s *Session) Workspace() string { return s.cwd }

// Capabilities is this session's view, which narrows as things are observed:
// agent_thought_chunk moves CapReasoning from unknown to yes the first time one
// actually arrives.
func (s *Session) Capabilities() adapter.Capabilities {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.caps
}

func (s *Session) narrow(cap adapter.Capability, sup adapter.Support, note string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.caps = s.caps.With(cap, sup, note)
}

// Events is the normalized stream. It closes exactly once, when the session
// ends.
func (s *Session) Events() <-chan event.Event { return s.q.events() }

// Modes and Models are what the session responses advertised. Models is
// UNSTABLE upstream and may be nil on any runtime.
func (s *Session) Modes() *SessionModeState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.modes
}

func (s *Session) Models() *SessionModelState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.models
}

// CurrentMode is the mode the agent last said it was in. It can change without
// us asking — current_mode_update — and a mode change can change permission
// behaviour underneath a session the registry believes it understands.
func (s *Session) CurrentMode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentMode
}

// Commands is the last available_commands_update. This is ACP's answer to
// SYSTEM.md §9's tool-list-refresh problem: the set is pushed when it changes
// rather than polled.
func (s *Session) Commands() []AvailableCommand {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]AvailableCommand(nil), s.commands...)
}

// UnknownUpdates counts session/update variants outside the documented eight,
// by discriminant. A non-zero count is how a ninth variant announces itself.
func (s *Session) UnknownUpdates() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.unknownUpdates))
	for k, v := range s.unknownUpdates {
		out[k] = v
	}
	return out
}

// DroppedContent counts content blocks in agent messages that had no
// normalized event to become — an image in an agent_message_chunk has no
// TextDelta, and inventing one would be a guess.
func (s *Session) DroppedContent() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.droppedContent
}

// DroppedEvents is how many normalized events a consumer that stopped reading
// never saw. Events() closes exactly once whether or not anybody is listening,
// and this is what that cost.
func (s *Session) DroppedEvents() int { return s.q.droppedCount() }

// Cancelling reports whether session/cancel has gone out for the running turn
// and we are still waiting for it to resolve with "cancelled". The console
// shows "stopping" rather than "running" from this.
func (s *Session) Cancelling() bool {
	s.mu.Lock()
	act := s.active
	s.mu.Unlock()
	return act != nil && act.isCancelling()
}

func (s *Session) setModes(modes *SessionModeState, models *SessionModelState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if modes != nil {
		s.modes = modes
		s.currentMode = modes.CurrentModeID
	}
	if models != nil {
		s.models = models
	}
}

// deliver applies one addressed message, or holds it until the session has
// finished registering.
func (s *Session) deliver(in inbound) {
	s.mu.Lock()
	if !s.booted {
		s.boot = append(s.boot, in)
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.apply(in)
}

// bootstrap drains what arrived before the session existed, then what arrived
// while it was draining, and only then opens the direct path. The reader is a
// single goroutine, so this converges.
func (s *Session) bootstrap(early []inbound) {
	for {
		s.mu.Lock()
		if len(early) == 0 && len(s.boot) == 0 {
			s.booted = true
			s.mu.Unlock()
			return
		}
		batch := early
		early = nil
		if len(batch) == 0 {
			batch = s.boot
			s.boot = nil
		}
		s.mu.Unlock()
		for _, in := range batch {
			s.apply(in)
		}
	}
}

func (s *Session) apply(in inbound) {
	switch in.method {
	case methodSessionUpdate:
		s.handleUpdate(in.update)
	case methodRequestPermission:
		s.raisePermission(in.id, *in.perm)
	default:
		s.log.Warn("acp: nothing to do with an addressed message", "method", in.method)
	}
}

func (s *Session) setReplaying(v bool) { s.replaying.Store(v) }

// Replaying reports whether the adapter is inside a session/load, which replays
// the whole conversation back as session/update notifications before it
// resolves. Every event produced while it is true carries Meta.Replay and
// therefore never pings.
func (s *Session) Replaying() bool { return s.replaying.Load() }

func (s *Session) meta(turnID string) event.Meta {
	if turnID == "" {
		s.mu.Lock()
		if s.active != nil {
			turnID = s.active.id
		}
		s.mu.Unlock()
	}
	return event.Meta{
		Runtime: string(s.runtime),
		Session: s.id,
		Turn:    turnID,
		At:      s.a.opts.Clock(),
		Seq:     s.seq.Add(1),
		Replay:  s.replaying.Load(),
	}
}

// ---------- sending ----------

// Send starts a turn, or queues it behind one already running. It is
// [Deliver] with [ModeAuto]; use Deliver when the caller needs to know which
// happened, or to ask for a redirect.
func (s *Session) Send(ctx context.Context, t adapter.Turn) (string, error) {
	d, err := s.Deliver(ctx, t, ModeAuto)
	return d.TurnID, err
}

// Deliver is ADAPTERS.md §4's table, both halves.
func (s *Session) Deliver(ctx context.Context, t adapter.Turn, m Mode) (Delivery, error) {
	if err := adapter.CheckTurn(s.Capabilities(), t); err != nil {
		return Delivery{}, err
	}
	blocks, err := encodeBlocks(t)
	if err != nil {
		return Delivery{}, err
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Delivery{}, adapter.ErrSessionClosed
	}
	tr := s.newTurnLocked(t, blocks)
	// Any explicit new instruction lifts a hold left by a cancel or a refusal:
	// the user is talking again, so the queue is live again.
	s.held = false

	if m == ModeRedirect && s.active != nil {
		act := s.active
		s.redirecting = true
		s.pending = append([]*turn{tr}, s.pending...)
		depth := len(s.pending)
		s.mu.Unlock()

		d := Delivery{
			TurnID:        tr.id,
			Disposition:   DispositionRedirect,
			QueueDepth:    depth,
			CancelledTurn: act.id,
			CancelledText: act.spec.Text,
		}
		if err := s.cancelTurn(ctx, act); err != nil {
			// The redirect is still queued at the head, so it goes out when the
			// turn does eventually resolve. Say what happened rather than
			// pretending the switch already took effect.
			return d, err
		}
		return d, nil
	}

	disp := DispositionQueued
	if s.active == nil && len(s.pending) == 0 {
		disp = DispositionStarted
	}
	s.pending = append(s.pending, tr)
	depth := len(s.pending)
	s.mu.Unlock()
	s.signal()

	return Delivery{TurnID: tr.id, Disposition: disp, QueueDepth: depth}, nil
}

// Pending is the utterances waiting to go out, oldest first. A bare cancel
// holds the queue rather than firing it — "stop" should not be followed by the
// addition you queued a minute earlier — and this is how the orchestrator sees
// that something is waiting.
func (s *Session) Pending() []adapter.Turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]adapter.Turn, 0, len(s.pending))
	for _, t := range s.pending {
		spec := t.spec
		spec.ID = t.id
		out = append(out, spec)
	}
	return out
}

// Held reports whether the queue is paused because the last turn ended in a
// cancel or a refusal.
func (s *Session) Held() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held
}

// Flush lifts a hold and lets the queue drain.
func (s *Session) Flush() {
	s.mu.Lock()
	s.held = false
	s.mu.Unlock()
	s.signal()
}

// Drop removes a queued utterance — the undo half of ORCHESTRATOR.md §4's
// announce-and-undo rule.
func (s *Session) Drop(turnID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.pending {
		if t.id == turnID {
			s.pending = append(s.pending[:i:i], s.pending[i+1:]...)
			return true
		}
	}
	return false
}

func (s *Session) newTurnLocked(t adapter.Turn, blocks []contentBlock) *turn {
	s.turnN++
	id := t.ID
	if id == "" {
		id = fmt.Sprintf("%s#t%d", s.id, s.turnN)
	}
	return &turn{id: id, spec: t, blocks: blocks, done: make(chan struct{})}
}

func (s *Session) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// pump is the only thing that starts a turn. Routing every start through one
// goroutine is what keeps "cancel, then re-prompt" from racing a queue drain.
func (s *Session) pump() {
	defer close(s.pumpDone)
	for {
		select {
		case <-s.wake:
		case <-s.stopPump:
			return
		}
		for {
			s.mu.Lock()
			if s.closed || s.active != nil || s.held || len(s.pending) == 0 {
				s.mu.Unlock()
				break
			}
			tr := s.pending[0]
			s.pending = s.pending[1:]
			s.active = tr
			s.mu.Unlock()

			tr.started = s.a.opts.Clock()
			s.q.push(event.TurnStarted{Meta: s.meta(tr.id)})
			go s.runPrompt(tr)
		}
	}
}

// runPrompt owns one session/prompt. The request does not resolve until the
// whole turn is over — every model call, every tool, every permission round
// trip happens inside it — so it runs on its own goroutine and the single
// stopReason it returns is the turn boundary.
func (s *Session) runPrompt(tr *turn) {
	raw, err := s.a.c.call(context.Background(), methodPrompt, promptParams{
		SessionID: s.native,
		Prompt:    tr.blocks,
	})
	if err != nil {
		s.completeTurn(tr, event.StopError, err)
		return
	}
	var res promptResult
	if err := json.Unmarshal(raw, &res); err != nil {
		s.completeTurn(tr, event.StopError, fmt.Errorf("acp: session/prompt result: %w", err))
		return
	}
	s.completeTurn(tr, mapStopReason(s.log, res.StopReason), nil)
}

// mapStopReason passes an unrecognised value through rather than flattening it.
// A sixth stop reason is something we observed, and hiding it behind "error"
// would lose the only evidence that the contract moved.
func mapStopReason(log *slog.Logger, s string) event.StopReason {
	switch event.StopReason(s) {
	case event.StopEndTurn, event.StopMaxTokens, event.StopMaxTurnRequests,
		event.StopRefusal, event.StopCancelled:
		return event.StopReason(s)
	}
	log.Warn("acp: session/prompt returned a stopReason outside the documented five", "stopReason", s)
	return event.StopReason(s)
}

func (s *Session) completeTurn(tr *turn, stop event.StopReason, cause error) {
	if !tr.claim() {
		// The session was torn down while the prompt was in flight and has
		// already reported this turn.
		return
	}
	duration := time.Duration(0)
	if !tr.started.IsZero() {
		duration = s.a.opts.Clock().Sub(tr.started)
	}

	// A turn that ends with anything outstanding leaves the agent waiting on us
	// forever. Withdraw rather than answer: the question died with the turn.
	s.withdrawPermissions(tr.id, "the turn ended before this question was answered")

	if cause != nil {
		s.q.push(event.Error{
			Meta:    s.meta(tr.id),
			Code:    "prompt_failed",
			Message: cause.Error(),
			Fatal:   errors.Is(cause, ErrConnClosed),
		})
	}

	usage := s.turnCost(tr, stop)
	s.q.push(event.TurnCompleted{
		Meta:       s.meta(tr.id),
		OK:         stop.OK(),
		StopReason: stop,
		Duration:   duration,
		Usage:      usage,
	})

	s.mu.Lock()
	if s.active == tr {
		s.active = nil
	}
	// A cancel or a refusal holds the queue. A cancel because "no, stop" must
	// not be followed by the addition queued a minute ago; a refusal because
	// ACP drops the refused prompt and everything after it from the next one,
	// so a queued follow-up would land on top of context that is gone.
	if stop == event.StopCancelled || stop == event.StopRefusal {
		if s.redirecting {
			// A redirect is the explicit instruction that lifts the hold, and
			// it is already at the head of the queue.
			s.redirecting = false
		} else {
			s.held = len(s.pending) > 0
			if s.held {
				s.log.Info("acp: holding queued utterances after a turn ended without finishing",
					"stopReason", string(stop), "queued", len(s.pending))
			}
		}
	}
	s.mu.Unlock()

	tr.finishNow(stop, cause)
	s.signal()
}

// turnCost asks the out-of-band metering hook, if there is one. ACP itself has
// no token, cost or usage field anywhere in its schema, so with no hook wired
// Usage stays nil — never a zero, which would read as a free turn.
func (s *Session) turnCost(tr *turn, stop event.StopReason) *event.Usage {
	src := s.a.opts.Cost
	if src == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.a.opts.CostTimeout)
	defer cancel()
	u, err := src.TurnCost(ctx, TurnInfo{
		SessionID:  s.id,
		Native:     s.native,
		TurnID:     tr.id,
		StopReason: stop,
	})
	if err != nil {
		s.log.Warn("acp: out-of-band cost lookup failed; reporting no cost rather than a wrong one",
			"source", src.Describe(), "err", err)
		return nil
	}
	return u
}

// ---------- steering, cancelling ----------

// Steer is not available on ACP and never has been. The complete client→agent
// surface is eight methods and none of them steers; the strings "steer",
// "inject" and "interrupt" do not occur anywhere in the published package. The
// caller cancels and re-prompts — [Session.Deliver] with [ModeRedirect].
func (s *Session) Steer(ctx context.Context, turnID string, t adapter.Turn) error {
	return s.Capabilities().Require(adapter.CapSteer)
}

// Cancel stops a turn.
//
// The sequence is fixed by ACP's contract and all three parts are mandatory:
// send the notification, answer every outstanding session/request_permission
// with the cancelled outcome, and keep reading session/update until the
// original session/prompt resolves with stopReason "cancelled". The agent
// flushes what it has already produced before resolving, so nothing observed is
// lost — which is what makes cancel-and-re-prompt an acceptable substitute for
// the steering ACP does not have.
func (s *Session) Cancel(ctx context.Context, turnID string) error {
	s.mu.Lock()
	act := s.active
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return adapter.ErrSessionClosed
	}
	if act == nil {
		return fmt.Errorf("%w: no turn is running on %s", adapter.ErrTurnNotActive, s.id)
	}
	if turnID != "" && turnID != act.id {
		return fmt.Errorf("%w: %s is not the running turn (%s is)", adapter.ErrTurnNotActive, turnID, act.id)
	}
	return s.cancelTurn(ctx, act)
}

func (s *Session) cancelTurn(ctx context.Context, act *turn) error {
	// The turn may have resolved between the caller reading s.active and
	// getting here. Sending session/cancel then would aim at whatever the agent
	// does next rather than at what the user meant to stop.
	select {
	case <-act.done:
		return nil
	default:
	}

	first := act.markCancelling()
	if first {
		if err := s.a.c.notify(methodCancel, cancelParams{SessionID: s.native}); err != nil {
			return fmt.Errorf("acp: session/cancel: %w", err)
		}
		// Mandatory, and in this order: the agent cannot unwind its turn while
		// a permission request of ours is still outstanding.
		s.cancelPermissions()
	}

	grace := time.NewTimer(s.a.opts.CancelGrace)
	defer grace.Stop()
	select {
	case <-act.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-grace.C:
		return fmt.Errorf("acp: %s did not resolve session/prompt within %s of session/cancel; its contract says it must resolve with stopReason cancelled",
			s.runtime, s.a.opts.CancelGrace)
	}
}

// ---------- modes and models ----------

// SetMode changes the session mode. modeId must be one the agent advertised —
// the same rule as permission options: we never send back something that was
// not offered.
func (s *Session) SetMode(ctx context.Context, modeID string) error {
	s.mu.Lock()
	modes := s.modes
	s.mu.Unlock()
	if modes == nil {
		return fmt.Errorf("acp: %s advertised no session modes, so there is nothing to set", s.runtime)
	}
	if !hasMode(modes.AvailableModes, modeID) {
		return fmt.Errorf("acp: %q is not one of the modes this session offers %v", modeID, modeIDs(modes.AvailableModes))
	}
	cctx, cancel := context.WithTimeout(ctx, s.a.opts.CallTimeout)
	defer cancel()
	if _, err := s.a.c.call(cctx, methodSetMode, setModeParams{SessionID: s.native, ModeID: modeID}); err != nil {
		return fmt.Errorf("acp: session/set_mode: %w", err)
	}
	s.mu.Lock()
	s.currentMode = modeID
	s.mu.Unlock()
	return nil
}

// SetModel changes the session model.
//
// It and the `models` field on the session responses are the only UNSTABLE
// members of the ACP surface — "not part of the spec yet, and may be removed or
// changed at any point" — so it refuses unless Options.AllowUnstableSetModel
// says otherwise. That is a deliberate speed bump, not an oversight: model
// choice matters, and calling an unstable method by default is how a runtime
// upgrade breaks a session mid-conversation.
func (s *Session) SetModel(ctx context.Context, modelID string) error {
	if !s.a.opts.AllowUnstableSetModel {
		return fmt.Errorf("acp: session/set_model is UNSTABLE upstream and is off by default; set Options.AllowUnstableSetModel to call it")
	}
	s.mu.Lock()
	models := s.models
	s.mu.Unlock()
	if models == nil {
		return fmt.Errorf("acp: %s advertised no models, so there is nothing to set", s.runtime)
	}
	if !hasModel(models.AvailableModels, modelID) {
		return fmt.Errorf("acp: %q is not one of the models this session offers %v", modelID, modelIDs(models.AvailableModels))
	}
	cctx, cancel := context.WithTimeout(ctx, s.a.opts.CallTimeout)
	defer cancel()
	if _, err := s.a.c.call(cctx, methodSetModel, setModelParams{SessionID: s.native, ModelID: modelID}); err != nil {
		return fmt.Errorf("acp: session/set_model: %w", err)
	}
	s.mu.Lock()
	if s.models != nil {
		s.models.CurrentModelID = modelID
	}
	s.mu.Unlock()
	return nil
}

func hasMode(list []SessionMode, id string) bool {
	for _, m := range list {
		if m.ID == id {
			return true
		}
	}
	return false
}

func modeIDs(list []SessionMode) []string {
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.ID)
	}
	return out
}

func hasModel(list []ModelInfo, id string) bool {
	for _, m := range list {
		if m.ModelID == id {
			return true
		}
	}
	return false
}

func modelIDs(list []ModelInfo) []string {
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.ModelID)
	}
	return out
}

// ---------- shutdown ----------

// Close ends the session and closes Events.
func (s *Session) Close(ctx context.Context) error {
	s.a.forget(s.native)
	s.finish(nil)
	return nil
}

// finish is idempotent: the adapter calls it on Close and again when the
// connection dies.
func (s *Session) finish(cause error) {
	s.finishOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		act := s.active
		s.active = nil
		s.pending = nil
		boot := s.boot
		s.boot = nil
		s.mu.Unlock()

		close(s.stopPump)
		<-s.pumpDone

		// A permission request that arrived while the session was still
		// registering is still a request the agent is blocked on.
		for _, in := range boot {
			if in.method == methodRequestPermission && in.id != nil {
				_ = s.a.c.respond(in.id, requestPermissionResult{Outcome: permissionOutcome{Outcome: outcomeCancelled}})
			}
		}
		s.withdrawPermissions("", "the session ended")

		if cause != nil {
			s.emitFatal(cause)
		}
		if act != nil {
			if act.claim() {
				s.q.push(event.TurnCompleted{
					Meta:       s.meta(act.id),
					OK:         false,
					StopReason: event.StopError,
					Duration:   s.a.opts.Clock().Sub(act.started),
				})
				act.finishNow(event.StopError, cause)
			} else {
				// The prompt goroutine already owns finishing this turn and is
				// on its way to pushing TurnCompleted. Closing the queue out
				// from under it would drop the turn boundary, which is the one
				// event the orchestrator cannot do without.
				t := time.NewTimer(s.a.opts.DrainGrace)
				select {
				case <-act.done:
				case <-t.C:
				}
				t.Stop()
			}
		}
		s.q.closeAndDrain(s.a.opts.DrainGrace)
	})
}
