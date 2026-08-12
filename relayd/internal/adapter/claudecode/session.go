package claudecode

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/event"
)

// Session is one Claude Code conversation: one process, one stdin we push
// turns into, one stdout we normalize.
type Session struct {
	a         *Adapter
	log       *slog.Logger
	id        string
	native    string
	workspace string

	norm *normalizer

	events     chan event.Event
	quit       chan struct{}
	quitOnce   sync.Once
	done       chan struct{}
	stderrDone chan struct{}

	emitMu     sync.RWMutex
	emitClosed bool

	mu        sync.Mutex
	caps      adapter.Capabilities
	proc      Process
	stderr    *ringBuffer
	closing   bool
	mode      string
	modeClass ModeClass
	scan      SettingsScan
	hazards   []Hazard
	pending   map[string]*pendingApproval
	exitErr   error
}

var _ adapter.Session = (*Session)(nil)
var _ approver = (*Session)(nil)

type sessionConfig struct {
	adapter   *Adapter
	id        string
	native    string
	workspace string
	mode      string
	modeClass ModeClass
	scan      SettingsScan
	resuming  bool
}

// pendingApproval is a permission call blocked inside the MCP handler.
type pendingApproval struct {
	turn     string
	ask      *event.NeedsInput
	resolved chan PermissionDecision
	once     sync.Once
}

func (p *pendingApproval) settle(d PermissionDecision) {
	p.once.Do(func() { p.resolved <- d })
}

func newSession(c sessionConfig) *Session {
	a := c.adapter
	s := &Session{
		a:          a,
		log:        a.log.With("session", c.id, "runtime", string(adapter.ClaudeCode)),
		id:         c.id,
		native:     c.native,
		workspace:  c.workspace,
		events:     make(chan event.Event, a.opts.Buffer),
		quit:       make(chan struct{}),
		done:       make(chan struct{}),
		stderrDone: make(chan struct{}),
		stderr:     newRingBuffer(16 << 10),
		mode:       c.mode,
		modeClass:  c.modeClass,
		scan:       c.scan,
		pending:    map[string]*pendingApproval{},
	}
	s.caps = a.caps

	// The mode we asked for, before the runtime has told us anything.
	if h, bad := modeHazard(HazardRunPermissionMode, "--permission-mode", c.mode); bad {
		s.hazards = append(s.hazards, h)
	}
	s.hazards = append(s.hazards, c.scan.Hazards...)
	s.applyMode(c.modeClass, "--permission-mode "+c.mode)

	s.norm = newNormalizer(normOptions{
		Runtime: string(adapter.ClaudeCode),
		Session: c.id,
		Log:     s.log,
		Now:     a.opts.Now,
		Out:     func(ev event.Event) { s.emit(ev) },
		OnInit:  s.onInit,
		// A resumed session may replay its history before it does anything
		// live. Nothing in the vendored trace shows what --resume emits, so
		// this is the conservative reading: mark everything replay until our
		// own first turn is acknowledged, because a replayed TurnCompleted
		// would otherwise ping the user about a turn from two weeks ago.
		Replay: c.resuming,
	})
	return s
}

// applyMode narrows CapNeedsInput for a permission mode. Caller holds s.mu, or
// is the constructor.
func (s *Session) applyMode(class ModeClass, source string) {
	switch class {
	case ModeAsks:
		return
	case ModePartial:
		s.caps = s.caps.With(adapter.CapNeedsInput, adapter.SupportNo,
			"permission mode approves some actions without asking ("+source+"), so Relay sees only the approvals it did not auto-grant and cannot claim to see every one")
	default:
		s.caps = s.caps.With(adapter.CapNeedsInput, adapter.SupportNo,
			"the permission-prompt tool is never called in this permission mode ("+source+"): the tool runs, there is no warning and the process exits 0")
	}
}

// onInit runs after every system/init. ADAPTERS.md §2: permissionMode is
// re-reported at the head of every turn, and reading it there is the check —
// there is no need to go back to settings.json, and this answer outranks it
// because it is what the runtime actually did.
//
// It must not call back into the normalizer; it is invoked with the
// normalizer's lock held so a session never observes a half-applied init.
func (s *Session) onInit(i InitInfo) {
	class := ClassifyMode(i.PermissionMode)

	s.mu.Lock()
	defer s.mu.Unlock()
	if class.SafeMode() {
		return
	}
	h, _ := modeHazard(HazardRuntimeReported, "system/init", i.PermissionMode)
	for _, existing := range s.hazards {
		if existing.Kind == HazardRuntimeReported && existing.Value == i.PermissionMode {
			return
		}
	}
	s.hazards = append(s.hazards, h)
	s.applyMode(class, "system/init reported "+i.PermissionMode)
	s.log.Warn("claudecode: the runtime reports approvals are off",
		"permission_mode", i.PermissionMode, "detail", h.Detail, "remedy", h.Remedy)
}

func (s *Session) attach(p Process) {
	s.mu.Lock()
	s.proc = p
	s.mu.Unlock()

	go s.readStderr(p)
	go s.readStdout(p)
}

func (s *Session) ID() string               { return s.id }
func (s *Session) Native() string           { return s.native }
func (s *Session) Runtime() adapter.Runtime { return adapter.ClaudeCode }

func (s *Session) Capabilities() adapter.Capabilities {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.caps
}

func (s *Session) Events() <-chan event.Event { return s.events }

// --- observation accessors ---

// Init is the last system/init, or nil before the first one arrives.
func (s *Session) Init() *InitInfo { return s.norm.initInfo() }

// RateLimit is the last rate_limit_event, quota state and all. Only status
// "allowed" has been observed; anything else also raises an Error event.
func (s *Session) RateLimit() *RateLimitInfo { return s.norm.rateLimitInfo() }

// Context is the live context size and its denominator — the numerator from the
// most recent request's usage, the window from modelUsage. This, not
// TurnCompleted.Usage, is what MEMORY.md §9's compact-at-70% must read.
func (s *Session) Context() ContextState { return s.norm.contextState() }

// LastResult is the last result event kept whole, including the final assistant
// text that ADAPTERS.md §6 summarises for speech and the per-turn cost.
func (s *Session) LastResult() *ResultInfo { return s.norm.lastResult() }

// Hooks is every hook this session has seen, in the order they started.
// Responses are correlated by hook_id because hooks run concurrently and come
// back out of order.
func (s *Session) Hooks() []HookRun { return s.norm.hookRuns() }

// Unseen is the wire shapes this adapter met and did not normalize, with
// counts. It is how a version bump becomes visible instead of silent.
func (s *Session) Unseen() map[string]int { return s.norm.unseen() }

// Hazards is every reason approvals might not reach us: the mode we asked for,
// the user's settings files, and what the runtime reported back. It is what the
// console renders next to a session that will never ask a question.
func (s *Session) Hazards() []Hazard {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Hazard(nil), s.hazards...)
}

// ExitErr is how the runtime process ended, once it has. It is nil while the
// session is alive and after a clean exit.
func (s *Session) ExitErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitErr
}

// SettingsScan is the pre-flight scan of the user's settings files.
func (s *Session) SettingsScan() SettingsScan {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scan
}

// --- driving a turn ---

// Send starts a new turn by writing one NDJSON line to the live process's
// stdin. It returns as soon as the line is written; the turn boundary is
// TurnCompleted on the event stream, and the isReplay echo is the runtime's
// acknowledgement that the turn was accepted.
func (s *Session) Send(ctx context.Context, t adapter.Turn) (string, error) {
	caps := s.Capabilities()
	if err := adapter.CheckTurn(caps, t); err != nil {
		return "", err
	}
	text, err := turnText(t)
	if err != nil {
		return "", err
	}
	id := t.ID
	if id == "" {
		id = uuid.NewString()
	}
	s.norm.queueTurn(id, text)
	if err := s.writeTurn(text); err != nil {
		return "", err
	}
	return id, nil
}

// Steer injects an utterance into a turn that is already running. It is the
// same wire message as Send — ADAPTERS.md §2 probed two turns through one
// long-lived process and the second was answered with the first still in
// context — which is why this runtime has real mid-turn steering where ACP has
// none.
func (s *Session) Steer(ctx context.Context, turnID string, t adapter.Turn) error {
	caps := s.Capabilities()
	if err := caps.Require(adapter.CapSteer); err != nil {
		return err
	}
	if err := adapter.CheckTurn(caps, t); err != nil {
		return err
	}
	if active := s.norm.currentTurn(); active == "" || active != turnID {
		return fmt.Errorf("%w: %s", adapter.ErrTurnNotActive, turnID)
	}
	text, err := turnText(t)
	if err != nil {
		return err
	}
	return s.writeTurn(text)
}

// Cancel stops a turn.
//
// The verified path is the permission prompt: a deny with interrupt: true
// aborts the turn outright, and that is the hard stop for "no, stop" spoken at
// the glasses. Outside that, the client-side interrupt message is not in the
// vendored trace — system/init advertises interrupt_receipt_v1, so the feature
// exists, but guessing its wire form would be inventing protocol. So a turn
// that is not blocked on an approval returns an *UnsupportedError the caller
// can degrade against, unless Options.UnverifiedControlInterrupt is set.
func (s *Session) Cancel(ctx context.Context, turnID string) error {
	if p := s.takePendingForTurn(turnID); p != nil {
		p.ask.Withdraw("turn cancelled")
		p.settle(Deny("cancelled by the user", true))
		s.log.Info("claudecode: turn cancelled through the permission prompt", "turn", turnID)
		return nil
	}
	if s.a.opts.UnverifiedControlInterrupt {
		line := `{"type":"control_request","request_id":"` + uuid.NewString() + `","request":{"subtype":"interrupt"}}`
		if err := s.writeLine([]byte(line)); err != nil {
			return err
		}
		s.log.Warn("claudecode: sent an unverified control_request interrupt", "turn", turnID)
		return nil
	}
	return &adapter.UnsupportedError{
		Runtime:    adapter.ClaudeCode,
		Capability: adapter.CapCancel,
		Support:    s.Capabilities().Get(adapter.CapCancel),
		Note:       s.Capabilities().Note(adapter.CapCancel),
	}
}

// Close ends the session and closes Events. Outstanding approvals are denied
// with interrupt so the runtime unwinds rather than sitting on a call nobody
// will ever answer.
func (s *Session) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		<-s.done
		return nil
	}
	s.closing = true
	proc := s.proc
	pend := make([]*pendingApproval, 0, len(s.pending))
	for _, p := range s.pending {
		pend = append(pend, p)
	}
	s.pending = map[string]*pendingApproval{}
	s.mu.Unlock()

	for _, p := range pend {
		p.ask.Withdraw("session closed")
		p.settle(Deny("the session was closed before anyone answered", true))
	}

	var err error
	if proc != nil {
		err = proc.Kill()
		// Release the reader immediately rather than waiting for it to drain
		// into a consumer that may have stopped reading. Close means stop, and
		// a few buffered deltas are worth less than a wedged daemon.
		s.signalQuit()
	} else {
		// Nothing was ever attached; nobody will close the channel for us.
		s.closeEvents()
	}

	select {
	case <-s.done:
	case <-ctx.Done():
		// The reader is stuck on a consumer that stopped reading. Release it
		// rather than leaving a goroutine and a process behind.
		s.signalQuit()
		<-s.done
		s.a.forget(s.id)
		return ctx.Err()
	}
	s.a.forget(s.id)
	return err
}

// --- the permission path ---

// Approve is called from the MCP server when Claude Code asks for permission.
// It raises a NeedsInput on the event stream and blocks until somebody answers,
// which is exactly the point: the call may be held for up to ~27.8 hours, so a
// user can be pinged, walk away, and answer an hour later.
func (s *Session) Approve(ctx context.Context, req PermissionRequest) (PermissionDecision, error) {
	timeout := s.a.opts.PermissionTimeout
	deadline := s.a.opts.Now().Add(timeout)

	p := &pendingApproval{
		turn:     s.norm.currentTurn(),
		resolved: make(chan PermissionDecision, 1),
	}

	meta := s.norm.outOfBandMeta(p.turn)

	spec := event.InputSpec{
		Ask:      event.InputPermission,
		Prompt:   req.prompt(),
		Deadline: deadline,
		Tool: &event.ToolRef{
			ID:       req.ToolUseID,
			Name:     req.ToolName,
			Title:    req.prompt(),
			RawInput: req.Input,
		},
		// Three options and no standing grant. ORCHESTRATOR.md §4b requires
		// consequential actions to be confirmed every time, so allow_always is
		// never offered and updatedPermissions is never returned.
		Options: []event.Option{
			{ID: optAllow, Name: "Allow", Kind: event.OptionAllowOnce},
			{ID: optDeny, Name: "Deny", Kind: event.OptionRejectOnce},
			{ID: optDenyStop, Name: "Deny and stop", Kind: event.OptionRejectOnce},
		},
	}

	p.ask = event.NewNeedsInput(meta, spec, func(_ context.Context, r event.Reply) error {
		d, err := decisionFor(r)
		if err != nil {
			return err
		}
		p.settle(d)
		return nil
	})

	key := req.ToolUseID
	if key == "" {
		key = uuid.NewString()
	}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return Deny("the session is closing", true), nil
	}
	s.pending[key] = p
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.pending, key)
		s.mu.Unlock()
	}()

	if !s.emit(p.ask) {
		return Deny("Relay could not raise the question: the session is closed", true), nil
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case d := <-p.resolved:
		return d, nil
	case <-ctx.Done():
		p.ask.Withdraw("the runtime stopped waiting")
		return PermissionDecision{}, ctx.Err()
	case <-s.quit:
		p.ask.Withdraw("session closed")
		return Deny("the session ended before anyone answered", true), nil
	case <-timer.C:
		p.ask.Withdraw("timed out")
		return PermissionDecision{}, timeoutError(timeout)
	}
}

// The three option ids this adapter offers. They are Relay's vocabulary, not
// the runtime's: Claude Code's permission response is allow/deny plus an
// interrupt flag, and the third option is the flag.
const (
	optAllow    = "allow"
	optDeny     = "deny"
	optDenyStop = "deny_stop"
)

// decisionFor maps a normalized reply onto Claude Code's response vocabulary.
func decisionFor(r event.Reply) (PermissionDecision, error) {
	switch r.OptionID {
	case optAllow:
		return Allow(r.UpdatedInput), nil
	case optDeny:
		return Deny(r.Message, false), nil
	case optDenyStop:
		return Deny(r.Message, true), nil
	}
	switch r.Decision {
	case event.DecisionAllow:
		return Allow(r.UpdatedInput), nil
	case event.DecisionDeny:
		return Deny(r.Message, r.Interrupt), nil
	case event.DecisionCancelled:
		// ACP's cancelled outcome. Claude Code's equivalent is a deny that also
		// aborts the turn.
		msg := r.Message
		if msg == "" {
			msg = "cancelled"
		}
		return Deny(msg, true), nil
	}
	return PermissionDecision{}, fmt.Errorf("claudecode: a reply needs an option id or a decision")
}

func (s *Session) takePendingForTurn(turnID string) *pendingApproval {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, p := range s.pending {
		if turnID == "" || p.turn == turnID || p.turn == "" {
			delete(s.pending, k)
			return p
		}
	}
	return nil
}

// --- process I/O ---

func (s *Session) writeTurn(text string) error {
	line, err := userTurnLine(text)
	if err != nil {
		return err
	}
	return s.writeLine(line)
}

func (s *Session) writeLine(line []byte) error {
	s.mu.Lock()
	proc, closing := s.proc, s.closing
	s.mu.Unlock()
	if closing || proc == nil {
		return adapter.ErrSessionClosed
	}
	if _, err := proc.Stdin().Write(append(line, '\n')); err != nil {
		return fmt.Errorf("claudecode: could not write to the runtime's stdin: %w", err)
	}
	return nil
}

func (s *Session) readStdout(p Process) {
	defer s.closeEvents()

	br := bufio.NewReaderSize(p.Stdout(), 1<<16)
	for {
		line, err := readLine(br)
		if len(line) > 0 {
			s.norm.push(line)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.log.Warn("claudecode: stdout ended badly", "err", err)
			}
			break
		}
	}

	waitErr := p.Wait()

	// The exit message quotes the tail of stderr, so wait for that copy to
	// finish — bounded, because a process whose stderr never closes must not
	// hold the session open.
	select {
	case <-s.stderrDone:
	case <-time.After(2 * time.Second):
	}

	s.mu.Lock()
	closing := s.closing
	s.exitErr = waitErr
	s.mu.Unlock()

	if waitErr != nil && !closing {
		msg := "the Claude Code process exited: " + waitErr.Error()
		if tail := strings.TrimSpace(s.stderr.String()); tail != "" {
			msg += "\n" + lastLines(tail, 5)
		}
		s.emit(event.Error{
			Meta:    s.norm.outOfBandMeta(""),
			Code:    "process_exit",
			Message: msg,
			Fatal:   true,
		})
	}

	// Anything still blocked has to be released, or the caller sits on a
	// question the runtime is no longer listening for.
	s.mu.Lock()
	pend := make([]*pendingApproval, 0, len(s.pending))
	for _, pa := range s.pending {
		pend = append(pend, pa)
	}
	s.pending = map[string]*pendingApproval{}
	s.mu.Unlock()
	for _, pa := range pend {
		pa.ask.Withdraw("the runtime exited")
		pa.settle(Deny("the runtime exited before anyone answered", true))
	}
}

func (s *Session) readStderr(p Process) {
	defer close(s.stderrDone)
	if p.Stderr() == nil {
		return
	}
	// Copied to a ring buffer for diagnostics only. Nothing parses it.
	_, _ = io.Copy(s.stderr, p.Stderr())
}

// emit puts an event on the stream. It returns false once the stream is closed.
//
// Two goroutines emit — the stdout reader and any blocked permission handler —
// so the close has to be a handshake rather than a plain close(): quit unblocks
// an in-flight send, the write lock waits for every sender to leave, and only
// then is the channel closed.
func (s *Session) emit(ev event.Event) bool {
	s.emitMu.RLock()
	defer s.emitMu.RUnlock()
	if s.emitClosed {
		return false
	}
	select {
	case s.events <- ev:
		return true
	case <-s.quit:
		return false
	}
}

// signalQuit releases every blocked emit. Close uses it to break a consumer
// that stopped reading — a stuck consumer is a bug in the consumer, but a
// deadlocked adapter is a bug here.
func (s *Session) signalQuit() { s.quitOnce.Do(func() { close(s.quit) }) }

func (s *Session) closeEvents() {
	s.emitMu.Lock()
	already := s.emitClosed
	s.emitClosed = true
	s.emitMu.Unlock()
	if already {
		return
	}
	s.signalQuit()

	// Take the lock again so any sender that got past the flag check before it
	// was set has left the select before the channel is closed.
	s.emitMu.Lock()
	//nolint:staticcheck // the point is the barrier, not the critical section
	s.emitMu.Unlock()

	close(s.events)
	close(s.done)
}

func turnText(t adapter.Turn) (string, error) {
	var b strings.Builder
	b.WriteString(t.Text)
	for _, blk := range t.Blocks {
		switch blk.Kind {
		case adapter.BlockText:
			b.WriteString(blk.Text)
		case adapter.BlockResourceLink:
			// stream-json has no observed resource-link block, so the URI goes
			// in verbatim as text. Referring to a path in prose is how you
			// point Claude Code at a file; nothing is invented.
			if blk.URI != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(blk.URI)
			}
		default:
			return "", adapter.Unsupported(adapter.ClaudeCode, adapter.CapPromptImage,
				"stream-json's user message carries text blocks; "+string(blk.Kind)+" has no observed wire form here")
		}
	}
	if strings.TrimSpace(b.String()) == "" {
		return "", errors.New("claudecode: a turn needs some text")
	}
	return b.String(), nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
