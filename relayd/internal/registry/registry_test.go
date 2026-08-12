package registry_test

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/adapter/fake"
	"github.com/luthor007/relay/relayd/internal/bus"
	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/logx"
	"github.com/luthor007/relay/relayd/internal/registry"
	"github.com/luthor007/relay/relayd/internal/store"
)

func openDB(t *testing.T) (*store.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relay.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

type harness struct {
	t   *testing.T
	Reg *registry.Registry
	DB  *store.DB
	Ad  *fake.Adapter
	ids int
	mu  sync.Mutex
}

func newHarness(t *testing.T, o registry.Options) *harness {
	t.Helper()
	h := &harness{t: t}
	if o.DB == nil {
		o.DB, _ = openDB(t)
	}
	if o.Log == nil {
		o.Log = logx.Discard()
	}
	if o.NewID == nil {
		o.NewID = func() string {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.ids++
			return "id-" + strconv.Itoa(h.ids)
		}
	}
	if o.FlushInterval == 0 {
		o.FlushInterval = 20 * time.Millisecond
	}
	reg, err := registry.New(o)
	if err != nil {
		t.Fatal(err)
	}
	ad := fake.New(fake.Options{Runtime: adapter.ClaudeCode})
	reg.AddAdapter(ad)
	h.Reg, h.DB, h.Ad = reg, o.DB, ad
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		reg.Shutdown(ctx)
	})
	return h
}

func (h *harness) start(t *testing.T, subject string) (*registry.Entry, *fake.Session) {
	t.Helper()
	e, err := h.Reg.Start(context.Background(), registry.StartOptions{
		Runtime:   adapter.ClaudeCode,
		Subject:   subject,
		Workspace: "/repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	sessions := h.Ad.Sessions()
	return e, sessions[len(sessions)-1]
}

func waitState(t *testing.T, e *registry.Entry, want store.SessionState) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if e.Row().State == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("session %s stayed %s, wanted %s", e.ID(), e.Row().State, want)
}

func TestOneListWhereThereWereFive(t *testing.T) {
	db, _ := openDB(t)
	h := newHarness(t, registry.Options{DB: db})

	for _, rt := range []adapter.Runtime{adapter.Codex, adapter.Hermes} {
		h.Reg.AddAdapter(fake.New(fake.Options{Runtime: rt}))
	}
	for _, rt := range []adapter.Runtime{adapter.ClaudeCode, adapter.Codex, adapter.Hermes} {
		if _, err := h.Reg.Start(context.Background(), registry.StartOptions{
			Runtime: rt, Subject: string(rt) + " work", Workspace: "/repo",
		}); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := h.Reg.List(context.Background(), store.SessionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d sessions, want 3 across three runtimes", len(rows))
	}
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Runtime] = true
	}
	if len(seen) != 3 {
		t.Fatalf("runtimes = %v", seen)
	}
}

func TestSessionStateFollowsTheEventStream(t *testing.T) {
	h := newHarness(t, registry.Options{})
	e, fs := h.start(t, "payments")

	if got := e.Row().State; got != store.SessionIdle {
		t.Fatalf("a new session starts %s, want idle", got)
	}

	fs.Emit(event.TurnStarted{Meta: fs.Meta("turn-1")})
	waitState(t, e, store.SessionRunning)

	ask := fs.Ask("turn-1", event.InputSpec{
		Ask:     event.InputPermission,
		Prompt:  "run rm -rf /tmp/x?",
		Options: []event.Option{{ID: "yes", Name: "Allow once", Kind: event.OptionAllowOnce}},
	})
	waitState(t, e, store.SessionAwaiting)

	qs := e.Questions()
	if len(qs) != 1 || qs[0].Prompt != "run rm -rf /tmp/x?" {
		t.Fatalf("questions = %+v", qs)
	}

	if err := h.Reg.Answer(context.Background(), e.ID(), event.Reply{
		OptionID: "yes", Decision: event.DecisionAllow,
	}); err != nil {
		t.Fatal(err)
	}
	if !ask.Answered() {
		t.Fatal("the reply never reached the adapter")
	}
	waitState(t, e, store.SessionRunning)

	fs.Emit(event.TurnCompleted{
		Meta: fs.Meta("turn-1"), OK: true, StopReason: event.StopEndTurn,
		Duration: 900 * time.Millisecond,
		Usage: &event.Usage{
			CostUSD: event.F64(0.42), TotalTokens: event.I64(1200),
			InputTokens: event.I64(1000), ContextWindow: event.I64(200000),
		},
	})
	waitState(t, e, store.SessionIdle)

	row := e.Row()
	if row.CostUSD == nil || *row.CostUSD != 0.42 {
		t.Fatalf("cost = %v", row.CostUSD)
	}
	if row.ContextWindow == nil || *row.ContextWindow != 200000 {
		t.Fatalf("context window = %v", row.ContextWindow)
	}
}

// ADAPTERS.md §5: every cost field is a pointer and nil means the runtime does
// not report it. An ACP turn carries no usage object at all, so the console must
// show a gap rather than a free turn.
func TestUnreportedCostStaysNilRatherThanZero(t *testing.T) {
	db, _ := openDB(t)
	h := newHarness(t, registry.Options{DB: db})
	acp := fake.New(fake.Options{Runtime: adapter.Hermes})
	h.Reg.AddAdapter(acp)

	e, err := h.Reg.Start(context.Background(), registry.StartOptions{
		Runtime: adapter.Hermes, Subject: "docs", Workspace: "/repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	fs := acp.Sessions()[0]
	fs.Emit(event.TurnCompleted{Meta: fs.Meta("t1"), OK: true, StopReason: event.StopEndTurn})
	waitState(t, e, store.SessionIdle)

	row, err := h.Reg.Session(context.Background(), e.ID())
	if err != nil {
		t.Fatal(err)
	}
	if row.CostUSD != nil || row.TokensTotal != nil {
		t.Fatalf("ACP reports no usage; got cost=%v tokens=%v", row.CostUSD, row.TokensTotal)
	}
}

func TestReplayedEventsDoNotMoveStateOrCost(t *testing.T) {
	h := newHarness(t, registry.Options{})
	e, fs := h.start(t, "payments")

	m := fs.Meta("old-turn")
	m.Replay = true
	fs.Emit(event.TurnCompleted{
		Meta: m, OK: true, StopReason: event.StopEndTurn,
		Usage: &event.Usage{CostUSD: event.F64(9.99)},
	})

	// Give the pump a moment; a replayed completion must change nothing.
	time.Sleep(100 * time.Millisecond)
	row := e.Row()
	if row.CostUSD != nil {
		t.Fatalf("a replayed turn was billed: %v", *row.CostUSD)
	}
	if row.State != store.SessionIdle {
		t.Fatalf("state = %s", row.State)
	}
}

func TestTurnsAndToolCallsArePersistedWithoutArguments(t *testing.T) {
	h := newHarness(t, registry.Options{})
	e, fs := h.start(t, "payments")

	if _, err := e.Send(context.Background(), adapter.Turn{ID: "turn-1", Text: "run the tests"}); err != nil {
		t.Fatal(err)
	}
	fs.Emit(event.ToolStarted{
		Meta: fs.Meta("turn-1"), ID: "tool-1", Tool: "Bash", Target: "go test ./...",
		RawInput: map[string]any{"command": "go test ./...", "token": "sk-live-abc123"},
	})
	fs.Emit(event.TextDelta{Meta: fs.Meta("turn-1"), Text: "tests pass"})
	fs.Emit(event.TurnCompleted{Meta: fs.Meta("turn-1"), OK: true, StopReason: event.StopEndTurn})
	waitState(t, e, store.SessionIdle)

	d, err := h.Reg.Detail(context.Background(), e.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Turns) != 2 {
		t.Fatalf("turns = %d, want a user turn and an agent turn", len(d.Turns))
	}
	if len(d.Tools) != 1 {
		t.Fatalf("tool calls = %d", len(d.Tools))
	}
	tc := d.Tools[0]
	if tc.Tool != "Bash" || tc.Target != "go test ./..." {
		t.Fatalf("tool call = %+v", tc)
	}
	// SYSTEM.md §5: args_digest is a digest and never the arguments. Tool
	// arguments routinely carry paths, tokens and payloads.
	if tc.ArgsDigest == "" {
		t.Fatal("no digest recorded")
	}
	for _, leak := range []string{"sk-live-abc123", "go test"} {
		if contains(tc.ArgsDigest, leak) {
			t.Fatalf("the digest contains the arguments: %q", tc.ArgsDigest)
		}
	}
	if !d.Live {
		t.Fatal("detail should say this session is live")
	}
}

// ACP's tool_call_update may carry a toolCallId and nothing else. Merging it
// onto what we already have is the rule (ADAPTERS.md §5); overwriting is how the
// tool's name and target vanish halfway through a session.
func TestToolUpdatesMergeRatherThanOverwrite(t *testing.T) {
	h := newHarness(t, registry.Options{})
	e, fs := h.start(t, "payments")

	fs.Emit(event.ToolStarted{
		Meta: fs.Meta("turn-1"), ID: "tool-1", Tool: "Bash", Target: "go test ./...",
	})
	fs.Emit(event.ToolOutput{Meta: fs.Meta("turn-1"), ID: "tool-1", Chunk: "ok\n"})
	fs.Emit(event.ToolOutput{Meta: fs.Meta("turn-1"), ID: "tool-1", Status: event.ToolCompleted})
	fs.Emit(event.TurnCompleted{Meta: fs.Meta("turn-1"), OK: true, StopReason: event.StopEndTurn})
	waitState(t, e, store.SessionIdle)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		d, err := h.Reg.Detail(context.Background(), e.ID())
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Tools) == 1 && d.Tools[0].ResultStatus == string(event.ToolCompleted) {
			if d.Tools[0].Tool != "Bash" || d.Tools[0].Target != "go test ./..." {
				t.Fatalf("a status-only update erased the tool call: %+v", d.Tools[0])
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the tool call never reached completed")
}

func contains(s, sub string) bool {
	return len(sub) <= len(s) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// The registry has to survive relayd restarting, and it must not claim a
// session is still running when the process driving it is gone.
func TestSurvivesRestartAndDetachesOrphans(t *testing.T) {
	db, path := openDB(t)
	h := newHarness(t, registry.Options{DB: db})
	e, fs := h.start(t, "payments")
	fs.Emit(event.TurnStarted{Meta: fs.Meta("turn-1")})
	waitState(t, e, store.SessionRunning)
	id := e.ID()

	// relayd dies here: the process goes away without closing anything.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	h.Reg.Shutdown(ctx)
	cancel()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	// Simulate the row surviving as "running", which is what a kill -9 leaves.
	row, err := db2.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	row.State = store.SessionRunning
	if err := db2.PutSession(context.Background(), row); err != nil {
		t.Fatal(err)
	}

	h2 := newHarness(t, registry.Options{DB: db2})
	res, err := h2.Reg.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Detached) != 1 || res.Detached[0] != id {
		t.Fatalf("detached = %v, want [%s]", res.Detached, id)
	}
	back, err := h2.Reg.Session(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if back.State != store.SessionIdle {
		t.Fatalf("state after recovery = %s, want idle", back.State)
	}
	if back.Subject != "payments" {
		t.Fatalf("the session did not survive the restart: %+v", back)
	}

	var found bool
	for _, i := range h2.Reg.Incidents() {
		if i.Kind == registry.IncidentOrphanDetached && i.Session == id {
			found = true
		}
	}
	if !found {
		t.Fatal("a detached session must produce an incident, not a silent state change")
	}
}

func TestUnexpectedExitIsAnIncidentAndRestartsThroughResume(t *testing.T) {
	h := newHarness(t, registry.Options{
		Restart: registry.RestartPolicy{
			Mode: registry.RestartOnFailure, MaxAttempts: 2, Backoff: 10 * time.Millisecond,
		},
	})
	e, fs := h.start(t, "payments")
	id := e.ID()

	// The runtime dies: the events channel closes without anyone asking.
	_ = fs.Close(context.Background())

	deadline := time.Now().Add(5 * time.Second)
	var exited, restarted bool
	for time.Now().Before(deadline) && !(exited && restarted) {
		for _, i := range h.Reg.Incidents() {
			if i.Kind == registry.IncidentSessionExited && i.Session == id {
				exited = true
			}
			if i.Kind == registry.IncidentRestarted {
				restarted = true
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !exited {
		t.Fatal("a session dying under us produced no incident")
	}
	if !restarted {
		t.Fatalf("Claude Code can resume, so the session should have come back: %+v", h.Reg.Incidents())
	}
}

// The default is deliberately not "start a fresh session": ACP's loadSession is
// unprobed, and a new session wearing the old name is a user talking to
// something that has forgotten the conversation and cannot tell.
func TestARuntimeThatCannotReattachIsNotSilentlyReplaced(t *testing.T) {
	db, _ := openDB(t)
	h := newHarness(t, registry.Options{
		DB: db,
		Restart: registry.RestartPolicy{
			Mode: registry.RestartOnFailure, MaxAttempts: 1, Backoff: 5 * time.Millisecond,
		},
	})
	caps := adapter.Baseline(adapter.Hermes) // CapResume is SupportUnknown
	acp := fake.New(fake.Options{Runtime: adapter.Hermes, Caps: &caps})
	h.Reg.AddAdapter(acp)

	e, err := h.Reg.Start(context.Background(), registry.StartOptions{
		Runtime: adapter.Hermes, Subject: "docs", Workspace: "/repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = acp.Sessions()[0].Close(context.Background())

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, i := range h.Reg.Incidents() {
			if i.Kind == registry.IncidentRestartedFresh {
				t.Fatal("a runtime that cannot reattach was silently replaced with a fresh session")
			}
			if i.Kind == registry.IncidentRestartFailed && i.Session == e.ID() {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected a restart_failed incident, got %+v", h.Reg.Incidents())
}

func TestDeliberateCloseIsNotRestarted(t *testing.T) {
	h := newHarness(t, registry.Options{
		Restart: registry.RestartPolicy{Mode: registry.RestartOnFailure, Backoff: 5 * time.Millisecond},
	})
	e, _ := h.start(t, "payments")
	if err := e.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitState(t, e, store.SessionClosed)

	time.Sleep(200 * time.Millisecond)
	for _, i := range h.Reg.Incidents() {
		if i.Kind == registry.IncidentSessionExited || i.Kind == registry.IncidentRestarted {
			t.Fatalf("a deliberate close was treated as a crash: %+v", i)
		}
	}
}

func TestDegradedCapabilityIsRecordedRatherThanHidden(t *testing.T) {
	db, _ := openDB(t)
	h := newHarness(t, registry.Options{DB: db})

	// Claude Code under permissions.defaultMode "auto": the permission-prompt
	// tool is never called, so the session has no observable needs-input path.
	caps := adapter.Baseline(adapter.ClaudeCode).
		With(adapter.CapNeedsInput, adapter.SupportNo, "permissionMode is auto")
	silent := fake.New(fake.Options{Runtime: adapter.Codex, Caps: &caps})
	h.Reg.AddAdapter(silent)

	if _, err := h.Reg.Start(context.Background(), registry.StartOptions{
		Runtime: adapter.Codex, Workspace: "/repo",
	}); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, i := range h.Reg.Incidents() {
		if i.Kind == registry.IncidentDegraded {
			found = true
		}
	}
	if !found {
		t.Fatal("a session that cannot report a blocked question must say so")
	}
}

func TestQuestionsAreWithdrawnWhenTheSessionDies(t *testing.T) {
	h := newHarness(t, registry.Options{
		Restart: registry.RestartPolicy{Mode: registry.RestartNever},
	})
	e, fs := h.start(t, "payments")
	ask := fs.Ask("turn-1", event.InputSpec{Ask: event.InputPermission, Prompt: "may I?"})
	waitState(t, e, store.SessionAwaiting)

	_ = fs.Close(context.Background())

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ask.Answered() {
			if err := ask.Reply(context.Background(), event.Reply{Decision: event.DecisionAllow}); !errors.Is(err, event.ErrWithdrawn) {
				t.Fatalf("reply after death = %v, want ErrWithdrawn", err)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("a question outlived the session that asked it")
}

func TestChangesAreWatchable(t *testing.T) {
	h := newHarness(t, registry.Options{})
	w := h.Reg.Watch("test")
	defer w.Close()

	e, fs := h.start(t, "payments")
	fs.Emit(event.TurnStarted{Meta: fs.Meta("turn-1")})

	var kinds []registry.ChangeKind
	deadline := time.After(3 * time.Second)
	for {
		select {
		case c := <-w.C():
			kinds = append(kinds, c.Kind)
			if c.Kind == registry.ChangeUpdated && c.Session.State == store.SessionRunning {
				if c.Session.ID != e.ID() {
					t.Fatalf("change for the wrong session: %s", c.Session.ID)
				}
				return
			}
		case <-deadline:
			t.Fatalf("never saw the running change: %v", kinds)
		}
	}
}

func TestEventsReachTheBusAfterTheRowHasMoved(t *testing.T) {
	h := newHarness(t, registry.Options{})
	sub := h.Reg.Bus().Subscribe("test", bus.Filter{Kinds: []event.Kind{event.KindTurnCompleted}})
	defer sub.Close()

	e, fs := h.start(t, "payments")
	fs.Emit(event.TurnStarted{Meta: fs.Meta("turn-1")})
	waitState(t, e, store.SessionRunning)
	fs.Emit(event.TurnCompleted{Meta: fs.Meta("turn-1"), OK: true, StopReason: event.StopEndTurn})

	select {
	case <-sub.C():
		// The row must already be idle: a subscriber that reacts by re-reading
		// the list has to get the new state, not the old one.
		if got := e.Row().State; got != store.SessionIdle {
			t.Fatalf("the bus saw the completion before the row moved: state %s", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the completion never reached the bus")
	}
}

func TestStartWithoutAnAdapterFails(t *testing.T) {
	h := newHarness(t, registry.Options{})
	_, err := h.Reg.Start(context.Background(), registry.StartOptions{Runtime: adapter.OpenCode})
	if !errors.Is(err, registry.ErrNoAdapter) {
		t.Fatalf("err = %v, want ErrNoAdapter", err)
	}
}

func TestShutdownClosesEverySession(t *testing.T) {
	h := newHarness(t, registry.Options{})
	e1, _ := h.start(t, "payments")
	e2, _ := h.start(t, "docs")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := h.Reg.Shutdown(ctx)
	if len(res.Closed) != 2 {
		t.Fatalf("closed = %v", res.Closed)
	}
	if res.TimedOut {
		t.Fatal("shutdown timed out")
	}
	for _, e := range []*registry.Entry{e1, e2} {
		if got := e.Row().State; got != store.SessionClosed {
			t.Fatalf("%s is %s after shutdown", e.ID(), got)
		}
	}
}
