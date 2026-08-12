package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/event"
)

// ExecAdapter is the survivability plan.
//
// ADAPTERS.md §1 and §8 item 2 both say the same thing: `codex app-server` is
// marked `[experimental]`, that is UPSTREAM CHURN and permanently so, and the
// mitigation is a `codex exec --json` fallback for the day it changes shape
// under us. This is that fallback, and it is deliberately worse: one shot per
// process, no steering, no approvals, no reattach.
//
// It is also *honest* about being worse. Its capability descriptor says No to
// steering and needs-input, and Unknown to plan, tokens and context window,
// because `codex exec --json`'s event vocabulary is not in any vendored schema
// — `generate-json-schema` describes app-server only. So this adapter maps the
// lines it recognises, which are the ones shaped like app-server notifications,
// and logs the rest by name rather than guessing at them. If Codex's exec
// stream turns out to speak a different vocabulary, those log lines are the
// probe result, and they belong in ADAPTERS.md §8.
type ExecAdapter struct {
	opts ExecOptions
	log  *slog.Logger

	mu       sync.Mutex
	sessions []*ExecSession
	closed   bool
}

// ExecOptions configures the one-shot fallback.
type ExecOptions struct {
	// Binary defaults to "codex".
	Binary string
	// Args are prepended to the prompt. Default ["exec", "--json"].
	Args []string
	Env  []string
	Log  *slog.Logger

	Clock      func() time.Time
	DrainGrace time.Duration

	// runner is the seam tests use: it plays a recorded NDJSON stream instead
	// of spawning a binary that does not exist in the build container.
	runner execRunner
}

type execRunner func(ctx context.Context, dir string, argv []string, env []string, stdout io.Writer) error

func (o *ExecOptions) defaults() {
	if o.Binary == "" {
		o.Binary = "codex"
	}
	if len(o.Args) == 0 {
		o.Args = []string{"exec", "--json"}
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.DrainGrace == 0 {
		o.DrainGrace = 2 * time.Second
	}
	if o.runner == nil {
		o.runner = spawnExec
	}
}

var _ adapter.Adapter = (*ExecAdapter)(nil)

// NewExec builds the fallback adapter. Nothing is spawned until a turn is sent.
func NewExec(opts ExecOptions) *ExecAdapter {
	opts.defaults()
	return &ExecAdapter{opts: opts, log: opts.Log.With("runtime", "codex", "path", "exec")}
}

func (a *ExecAdapter) Runtime() adapter.Runtime { return adapter.Codex }

// Capabilities is the Codex baseline with everything this path cannot do
// knocked out. The point of the fallback is that the orchestrator can read what
// it lost and degrade visibly — no silent "steering did nothing".
func (a *ExecAdapter) Capabilities() adapter.Capabilities {
	return adapter.Baseline(adapter.Codex).
		With(adapter.CapSteer, adapter.SupportNo,
			"codex exec --json is one-shot; there is no channel to inject into a running turn. Cancel and re-prompt").
		With(adapter.CapNeedsInput, adapter.SupportNo,
			"codex exec --json is non-interactive: there is no server-to-client request path, so approvals cannot be answered").
		With(adapter.CapResume, adapter.SupportUnknown,
			"`codex exec resume` exists but its behaviour and its JSON stream have not been probed").
		With(adapter.CapFork, adapter.SupportNo, "no fork on the exec path").
		With(adapter.CapPlan, adapter.SupportUnknown,
			"the exec --json vocabulary is in no vendored schema; a plan is emitted only if a line arrives shaped like turn/plan/updated").
		With(adapter.CapReasoning, adapter.SupportUnknown, "same: unverified vocabulary").
		With(adapter.CapTokens, adapter.SupportUnknown, "same: unverified vocabulary").
		With(adapter.CapContextWindow, adapter.SupportUnknown, "same: unverified vocabulary").
		With(adapter.CapCostUSD, adapter.SupportNo, "no dollar figure anywhere in the Codex contract").
		With(adapter.CapPromptImage, adapter.SupportNo, "the exec path carries a text prompt only").
		With(adapter.CapPromptAudio, adapter.SupportNo, "the exec path carries a text prompt only").
		With(adapter.CapPromptEmbeddedContext, adapter.SupportNo, "the exec path carries a text prompt only")
}

func (a *ExecAdapter) Start(ctx context.Context, opts adapter.SessionOptions) (adapter.Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, adapter.ErrSessionClosed
	}
	id := opts.ID
	if id == "" {
		id = uuid.NewString()
	}
	s := newExecSession(a, id, opts)
	a.sessions = append(a.sessions, s)
	return s, nil
}

// Resume is not available: `codex exec resume` exists but neither its behaviour
// nor its JSON stream has been probed, and CapResume says Unknown rather than
// yes. The registry has to start a new session and say so.
func (a *ExecAdapter) Resume(context.Context, adapter.SessionRef, adapter.SessionOptions) (adapter.Session, error) {
	return nil, a.Capabilities().Require(adapter.CapResume)
}

func (a *ExecAdapter) Close(ctx context.Context) error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	sessions := a.sessions
	a.sessions = nil
	a.mu.Unlock()

	var err error
	for _, s := range sessions {
		if e := s.Close(ctx); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// ExecSession is one `codex exec --json` conversation, which is one turn.
type ExecSession struct {
	a    *ExecAdapter
	log  *slog.Logger
	id   string
	opts adapter.SessionOptions

	src  *metaSource
	q    *queue
	norm *normalizer

	mu     sync.Mutex
	closed bool
	turns  int
	cancel context.CancelFunc
	active string
	done   chan struct{}
}

var _ adapter.Session = (*ExecSession)(nil)

func newExecSession(a *ExecAdapter, id string, opts adapter.SessionOptions) *ExecSession {
	src := &metaSource{runtime: string(adapter.Codex), session: id, now: a.opts.Clock}
	s := &ExecSession{
		a:    a,
		log:  a.log.With("session", id),
		id:   id,
		opts: opts,
		src:  src,
		q:    newQueue(),
	}
	s.norm = newNormalizer(func(e event.Event) { s.q.push(e) }, src, s.log, hooks{})
	return s
}

func (s *ExecSession) ID() string                         { return s.id }
func (s *ExecSession) Runtime() adapter.Runtime           { return adapter.Codex }
func (s *ExecSession) Events() <-chan event.Event         { return s.q.events() }
func (s *ExecSession) Capabilities() adapter.Capabilities { return s.a.Capabilities() }

// Native is empty until a turn has run and Codex has named a thread. On this
// path it usually stays empty: nothing in the exec stream is contractually
// required to carry a thread id, and reporting one we did not see would be the
// exact failure this package exists to avoid.
func (s *ExecSession) Native() string { return "" }

func (s *ExecSession) Send(ctx context.Context, t adapter.Turn) (string, error) {
	if err := adapter.CheckTurn(s.Capabilities(), t); err != nil {
		return "", err
	}
	input, err := toInput(t)
	if err != nil {
		return "", err
	}
	prompt := input[0].Text

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", adapter.ErrSessionClosed
	}
	if s.turns > 0 {
		s.mu.Unlock()
		// A second turn would need `codex exec resume`, which is unprobed.
		return "", s.Capabilities().Require(adapter.CapResume)
	}
	s.turns++
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	// The exec path has no observable turn id, so Relay names the turn. That is
	// an identifier, not an event: nothing is claimed about the runtime.
	turnID := "exec-" + uuid.NewString()
	s.active = turnID
	s.done = make(chan struct{})
	done := s.done
	s.mu.Unlock()

	argv := s.a.execArgv(prompt)
	env := append(append([]string{}, s.a.opts.Env...), s.opts.Env...)

	s.q.push(event.TurnStarted{Meta: s.src.meta(turnID)})

	go s.run(runCtx, cancel, done, turnID, argv, env)
	return turnID, nil
}

func (s *ExecSession) run(ctx context.Context, cancel context.CancelFunc, done chan struct{}, turnID string, argv, env []string) {
	defer cancel()
	defer close(done)

	pr, pw := io.Pipe()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.consume(pr, turnID)
	}()

	start := s.a.opts.Clock()
	runErr := s.a.opts.runner(ctx, s.opts.Workspace, argv, env, pw)
	_ = pw.CloseWithError(io.EOF)
	wg.Wait()

	stop := event.StopEndTurn
	switch {
	case runErr == nil:
	case errors.Is(ctx.Err(), context.Canceled):
		stop = event.StopCancelled
	default:
		stop = event.StopError
		s.q.push(event.Error{
			Meta:    s.src.meta(turnID),
			Code:    "exec_failed",
			Message: runErr.Error(),
		})
	}

	s.mu.Lock()
	s.active = ""
	s.mu.Unlock()

	// The process exiting is a real observation, so the turn boundary is real.
	// Usage is the running thread total rather than a per-turn figure: nothing
	// on this path correlates usage to a turn, and one turn per process makes
	// the two the same number anyway. It is nil when the stream said nothing,
	// never a zero.
	s.q.push(event.TurnCompleted{
		Meta:       s.src.meta(turnID),
		OK:         stop.OK(),
		StopReason: stop,
		Duration:   s.a.opts.Clock().Sub(start),
		Usage:      s.norm.usage(),
	})
}

// consume reads the NDJSON stream. Lines shaped like app-server notifications
// go through the same mapping table as the real adapter; anything else is
// logged by whatever key it uses to name itself and dropped.
func (s *ExecSession) consume(r io.Reader, turnID string) {
	br := bufio.NewReaderSize(r, 1<<16)
	fr := ndjson{}
	unknown := map[string]int{}
	for {
		payload, err := fr.read(br)
		if err != nil {
			break
		}
		var m message
		if err := json.Unmarshal(payload, &m); err != nil {
			s.log.Debug("codex exec: line is not JSON", "line", truncate(payload, 120))
			continue
		}
		if m.Method != "" && len(m.Params) > 0 {
			s.norm.handle(m.Method, withTurn(m.Params, turnID))
			continue
		}
		unknown[execShape(payload)]++
	}
	for shape, n := range unknown {
		s.log.Warn("codex exec: unrecognised stream shape — this is the probe result ADAPTERS.md §8 wants",
			"shape", shape, "lines", n)
	}
}

// withTurn fills in the turnId the exec stream may not carry, so events land on
// the turn Relay named. It never overwrites one that is already there.
func withTurn(params json.RawMessage, turnID string) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(params, &m); err != nil {
		return params
	}
	if v, ok := m["turnId"]; ok && len(v) > 0 && string(v) != "null" {
		return params
	}
	b, err := json.Marshal(turnID)
	if err != nil {
		return params
	}
	m["turnId"] = b
	out, err := json.Marshal(m)
	if err != nil {
		return params
	}
	return out
}

// execShape names an unrecognised line by its discriminator keys, so the log
// says what arrived rather than dumping a transcript into a file.
func execShape(payload []byte) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return "not-an-object"
	}
	for _, k := range []string{"type", "msg", "method", "event"} {
		if v, ok := m[k]; ok {
			var s string
			if err := json.Unmarshal(v, &s); err == nil {
				return k + "=" + s
			}
			return k + "=<object>"
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return "keys:" + strings.Join(keys, ",")
}

// Steer is the whole reason the app-server path exists. Saying so is the point.
func (s *ExecSession) Steer(context.Context, string, adapter.Turn) error {
	return s.Capabilities().Require(adapter.CapSteer)
}

func (s *ExecSession) Cancel(ctx context.Context, turnID string) error {
	s.mu.Lock()
	cancel, active, done := s.cancel, s.active, s.done
	s.mu.Unlock()
	if cancel == nil || active == "" {
		return fmt.Errorf("%w: nothing is running", adapter.ErrTurnNotActive)
	}
	if turnID != "" && turnID != active {
		return fmt.Errorf("%w: %s is not the active turn (%q is)", adapter.ErrTurnNotActive, turnID, active)
	}
	cancel()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (s *ExecSession) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	cancel, done := s.cancel, s.done
	s.mu.Unlock()

	if cancel != nil {
		cancel()
		if done != nil {
			select {
			case <-done:
			case <-ctx.Done():
			}
		}
	}
	s.q.closeAndDrain(s.a.opts.DrainGrace)
	return nil
}

// spawnExec is the real runner.
func spawnExec(ctx context.Context, dir string, argv, env []string, stdout io.Writer) error {
	if len(argv) == 0 {
		return errors.New("codex exec: no argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = stdout
	cmd.Stderr = io.Discard
	return cmd.Run()
}

// execArgv is exactly what will be run, so "what would you execute?" has one
// answer and the installer's doctor command can print it.
func (a *ExecAdapter) execArgv(prompt string) []string {
	argv := make([]string, 0, len(a.opts.Args)+2)
	argv = append(argv, a.opts.Binary)
	argv = append(argv, a.opts.Args...)
	return append(argv, prompt)
}
