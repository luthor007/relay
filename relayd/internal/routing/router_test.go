package routing_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/routing"
	"github.com/luthor007/relay/relayd/internal/store"
)

func session(id, subject, workspace string, rt adapter.Runtime, age time.Duration) routing.SessionView {
	return routing.SessionView{
		ID:           id,
		Runtime:      rt,
		Subject:      subject,
		Workspace:    workspace,
		LastActive:   now().Add(-age),
		State:        store.SessionIdle,
		Capabilities: adapter.Baseline(rt),
	}
}

func now() time.Time { return time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC) }

func router(t *testing.T, o routing.Options) *routing.Router {
	t.Helper()
	if o.Now == nil {
		o.Now = now
	}
	r, err := routing.New(o)
	if err != nil {
		t.Fatalf("routing.New: %v", err)
	}
	return r
}

// ORCHESTRATOR.md §4 rule 2: say what it chose, before acting. Every decision
// carries a clause, and a decision with no announcement is a silent router.
func TestEveryDecisionAnnouncesItself(t *testing.T) {
	ctx := context.Background()
	live := routing.StaticSessions{
		session("s1", "the payments refactor", "/repo/payments", adapter.ClaudeCode, time.Minute),
		session("s2", "the api docs", "/repo/api", adapter.Codex, time.Hour),
	}
	r := router(t, routing.Options{Sessions: live})

	for _, text := range []string{
		"run the tests",
		"new session",
		"talk to the payments one",
		"stop",
		"undo that",
		"what is it doing",
	} {
		d, err := r.Route(ctx, routing.Request{Text: text})
		if err != nil {
			t.Fatalf("Route(%q): %v", text, err)
		}
		if strings.TrimSpace(d.Announcement) == "" {
			t.Errorf("Route(%q) → %s with no announcement; a wrong guess has to be catchable", text, d.Kind)
		}
	}
}

// The manual path is the shipping default: the conversation is in a session and
// utterances go there until the user says otherwise.
func TestManualPathFollowsTheFocus(t *testing.T) {
	ctx := context.Background()
	live := routing.StaticSessions{
		session("s1", "the payments refactor", "/repo/payments", adapter.ClaudeCode, time.Minute),
		session("s2", "the api docs", "/repo/api", adapter.Codex, time.Hour),
	}
	r := router(t, routing.Options{Sessions: live})
	r.SetFocus("s2")

	d, err := r.Route(ctx, routing.Request{Text: "run the tests"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != routing.KindContinue || d.Session != "s2" {
		t.Fatalf("got %s/%s, want continue/s2", d.Kind, d.Session)
	}
	if d.Reason != routing.ReasonFocus {
		t.Errorf("reason = %q, want the focus", d.Reason)
	}
	if d.Automatic {
		t.Error("the manual path must not be marked automatic")
	}
	if !strings.Contains(d.Announcement, "api docs") {
		t.Errorf("announcement %q does not name the session it chose", d.Announcement)
	}
}

// With no focus and more than one session running, asking is the correct
// outcome. ORCHESTRATOR.md §4: a router that is right 80% of the time and
// silent about it is worse than one that asks.
func TestAmbiguousWithoutAFocusAsks(t *testing.T) {
	ctx := context.Background()
	live := routing.StaticSessions{
		session("s1", "the payments refactor", "/repo/payments", adapter.ClaudeCode, time.Minute),
		session("s2", "the api docs", "/repo/api", adapter.Codex, time.Hour),
	}
	r := router(t, routing.Options{Sessions: live})

	d, err := r.Route(ctx, routing.Request{Text: "run the tests"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != routing.KindAsk {
		t.Fatalf("got %s/%s, want an ask", d.Kind, d.Session)
	}
	if len(d.Candidates) != 2 {
		t.Errorf("candidates = %d, want both sessions named", len(d.Candidates))
	}
	if !strings.Contains(d.Question, "payments") || !strings.Contains(d.Question, "api") {
		t.Errorf("question %q should name the candidates; \"which session?\" is answered by silence", d.Question)
	}
}

func TestOneLiveSessionContinuesAndSaysSo(t *testing.T) {
	ctx := context.Background()
	live := routing.StaticSessions{session("s1", "the payments refactor", "/repo/payments", adapter.ClaudeCode, time.Minute)}
	r := router(t, routing.Options{Sessions: live})

	d, _ := r.Route(ctx, routing.Request{Text: "run the tests"})
	if d.Kind != routing.KindContinue || d.Session != "s1" {
		t.Fatalf("got %s/%s", d.Kind, d.Session)
	}
	if d.Reason != routing.ReasonOnlyLive {
		t.Errorf("reason = %q", d.Reason)
	}
	if !strings.Contains(d.Announcement, "only one running") {
		t.Errorf("announcement %q should say why, so a wrong assumption is visible", d.Announcement)
	}
}

func TestNothingRunningStartsSomething(t *testing.T) {
	ctx := context.Background()
	rr := runtimeRouter(t, routing.Entitlements{routing.ClaudeSubscription}, nil,
		used(adapter.ClaudeCode), used(adapter.Codex))
	r := router(t, routing.Options{Sessions: routing.StaticSessions{}, Runtime: rr})

	d, _ := r.Route(ctx, routing.Request{Text: "look at the payments module", Workspace: "/repo/payments"})
	if d.Kind != routing.KindNew {
		t.Fatalf("got %s, want a new session", d.Kind)
	}
	if d.Runtime != adapter.ClaudeCode {
		t.Errorf("runtime = %s; the Claude subscription should decide", d.Runtime)
	}
	if !strings.Contains(d.Announcement, "Claude Code") || !strings.Contains(d.Announcement, "payments") {
		t.Errorf("announcement %q should name the runtime and where", d.Announcement)
	}
}

// The escape hatch, end to end.
func TestExplicitCommandsAlwaysWin(t *testing.T) {
	ctx := context.Background()
	live := routing.StaticSessions{
		session("s1", "the payments refactor", "/repo/payments", adapter.ClaudeCode, time.Minute),
		session("s2", "the api docs", "/repo/api", adapter.Codex, time.Hour),
	}
	rr := runtimeRouter(t, routing.Entitlements{routing.APIKeysOnly}, nil, used(adapter.Codex))
	r := router(t, routing.Options{Sessions: live, Runtime: rr})
	r.SetFocus("s1")

	t.Run("talk to the other one", func(t *testing.T) {
		d, _ := r.Route(ctx, routing.Request{Text: "talk to the api one"})
		if d.Kind != routing.KindContinue || d.Session != "s2" {
			t.Fatalf("got %s/%s, want continue/s2", d.Kind, d.Session)
		}
		if d.Reason != routing.ReasonExplicit {
			t.Errorf("reason = %q", d.Reason)
		}
	})

	t.Run("new session, despite a live focus", func(t *testing.T) {
		d, _ := r.Route(ctx, routing.Request{Text: "new session, run the tests"})
		if d.Kind != routing.KindNew {
			t.Fatalf("got %s, want a new session", d.Kind)
		}
		if d.Text != "run the tests" {
			t.Errorf("text = %q; the instruction after the command is what gets sent", d.Text)
		}
	})

	t.Run("a name that matches nothing asks rather than guessing", func(t *testing.T) {
		d, _ := r.Route(ctx, routing.Request{Text: "talk to the migrations one"})
		if d.Kind != routing.KindAsk {
			t.Fatalf("got %s/%s, want an ask", d.Kind, d.Session)
		}
		if !strings.Contains(d.Question, "migrations") {
			t.Errorf("question %q should repeat the name back — the usual cause is a misrecognition", d.Question)
		}
	})

	t.Run("an ambiguous name asks", func(t *testing.T) {
		two := routing.StaticSessions{
			session("a", "payments refactor", "/repo/one", adapter.Codex, time.Minute),
			session("b", "payments migration", "/repo/two", adapter.Codex, time.Minute),
		}
		r2 := router(t, routing.Options{Sessions: two})
		d, _ := r2.Route(ctx, routing.Request{Text: "talk to the payments one"})
		if d.Kind != routing.KindAsk {
			t.Fatalf("got %s/%s; two equal matches is a question", d.Kind, d.Session)
		}
	})
}

// A misheard session name is a wrong continue with extra steps, so a
// low-confidence reference is confirmed rather than acted on.
func TestLowConfidenceReferenceIsConfirmed(t *testing.T) {
	ctx := context.Background()
	live := routing.StaticSessions{
		session("s1", "the payments refactor", "/repo/payments", adapter.ClaudeCode, time.Minute),
		session("s2", "the api docs", "/repo/api", adapter.Codex, time.Hour),
	}
	r := router(t, routing.Options{Sessions: live})

	d, _ := r.Route(ctx, routing.Request{Text: "talk to the payments one", Confidence: 0.3})
	if d.Kind != routing.KindAsk {
		t.Fatalf("got %s, want a confirmation", d.Kind)
	}
	// And a confident one acts.
	d, _ = r.Route(ctx, routing.Request{Text: "talk to the payments one", Confidence: 0.95})
	if d.Kind != routing.KindContinue || d.Session != "s1" {
		t.Fatalf("got %s/%s, want continue/s1", d.Kind, d.Session)
	}
}

// "Yes" is only an answer when something is actually blocked. Otherwise it is
// the word "yes" in a sentence, and it goes through the normal path.
func TestAnswerGoesToTheBlockedSession(t *testing.T) {
	ctx := context.Background()
	blocked := session("s2", "the api docs", "/repo/api", adapter.Codex, time.Minute)
	blocked.State = store.SessionAwaiting
	live := routing.StaticSessions{
		session("s1", "the payments refactor", "/repo/payments", adapter.ClaudeCode, time.Minute),
		blocked,
	}
	r := router(t, routing.Options{Sessions: live})
	r.SetFocus("s1")

	d, _ := r.Route(ctx, routing.Request{Text: "yes"})
	if d.Kind != routing.KindControl || d.Session != "s2" {
		t.Fatalf("got %s/%s, want a control aimed at the blocked session", d.Kind, d.Session)
	}
	if d.Command == nil || !d.Command.Approve {
		t.Fatal("the answer's polarity is lost")
	}

	// With nothing blocked, the same word is just an utterance.
	r2 := router(t, routing.Options{Sessions: routing.StaticSessions{
		session("s1", "the payments refactor", "/repo/payments", adapter.ClaudeCode, time.Minute),
	}})
	d2, _ := r2.Route(ctx, routing.Request{Text: "yes"})
	if d2.Kind != routing.KindContinue {
		t.Fatalf("got %s; with nothing blocked this is not an answer", d2.Kind)
	}
}

// "Stop" with three sessions running and no focus is a question, not a guess.
func TestBareControlVerbNeedsATarget(t *testing.T) {
	ctx := context.Background()
	live := routing.StaticSessions{
		session("s1", "one", "/repo/a", adapter.ClaudeCode, time.Minute),
		session("s2", "two", "/repo/b", adapter.Codex, time.Minute),
	}
	r := router(t, routing.Options{Sessions: live})
	d, _ := r.Route(ctx, routing.Request{Text: "stop"})
	if d.Kind != routing.KindControl {
		t.Fatalf("got %s", d.Kind)
	}
	if d.Session != "" {
		t.Errorf("session = %q; with two idle sessions and no focus there is no obvious target", d.Session)
	}

	// One busy session is an obvious target.
	busy := session("s2", "two", "/repo/b", adapter.Codex, time.Minute)
	busy.State = store.SessionRunning
	r2 := router(t, routing.Options{Sessions: routing.StaticSessions{
		session("s1", "one", "/repo/a", adapter.ClaudeCode, time.Minute), busy,
	}})
	d2, _ := r2.Route(ctx, routing.Request{Text: "stop"})
	if d2.Session != "s2" {
		t.Errorf("session = %q, want the one that is actually running", d2.Session)
	}
}

// ------------------------------------------------------------------- undo --

type fakeDriver struct {
	sent      []string
	sentTo    []string
	cancelled []string
	started   int
	cancelErr error
	startID   string
}

func (d *fakeDriver) Send(_ context.Context, sessionID, text string) (string, error) {
	d.sentTo = append(d.sentTo, sessionID)
	d.sent = append(d.sent, text)
	return "turn-" + text, nil
}

func (d *fakeDriver) Start(_ context.Context, spec routing.NewSession) (string, error) {
	d.started++
	if d.startID == "" {
		d.startID = "new-session"
	}
	_ = spec
	return d.startID, nil
}

func (d *fakeDriver) Cancel(_ context.Context, sessionID, turnID string) error {
	d.cancelled = append(d.cancelled, sessionID+"/"+turnID)
	return d.cancelErr
}

// ORCHESTRATOR.md §4 rule 3: undo moves the last turn to a different session.
// The announcement is only worth something if disagreeing with it does
// something.
func TestUndoMovesTheLastTurn(t *testing.T) {
	ctx := context.Background()
	live := routing.StaticSessions{
		session("s1", "the payments refactor", "/repo/payments", adapter.ClaudeCode, time.Minute),
		session("s2", "the api docs", "/repo/api", adapter.Codex, time.Hour),
	}
	drv := &fakeDriver{}
	r := router(t, routing.Options{Sessions: live, Driver: drv})
	r.SetFocus("s1")

	d, _ := r.Route(ctx, routing.Request{Text: "add a rate limiter"})
	if d.Session != "s1" {
		t.Fatalf("setup: went to %q", d.Session)
	}
	r.Confirm(d, "s1", "t-1")

	res, err := r.Undo(ctx, routing.UndoTarget{Ref: "api docs"})
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if res.From != "s1" || res.To != "s2" {
		t.Fatalf("moved %s → %s, want s1 → s2", res.From, res.To)
	}
	if len(drv.sent) != 1 || drv.sent[0] != "add a rate limiter" {
		t.Fatalf("re-sent %v; the same utterance has to arrive at the new session", drv.sent)
	}
	if !res.Cancelled || len(drv.cancelled) != 1 {
		t.Errorf("the wrong session's turn should be cancelled first, got %v", drv.cancelled)
	}
	if r.Focus() != "s2" {
		t.Errorf("focus = %q; undo moves the conversation too", r.Focus())
	}
	if !strings.Contains(res.Announcement, "api docs") {
		t.Errorf("announcement %q should say where it went", res.Announcement)
	}
}

// Three of the five runtimes cannot cancel a turn once it is running. That is a
// fact about the runtime and it has to reach the user, not be swallowed into a
// success message that implies a rollback.
func TestUndoReportsARuntimeThatCannotCancel(t *testing.T) {
	ctx := context.Background()
	live := routing.StaticSessions{
		session("s1", "the payments refactor", "/repo/payments", adapter.OpenClaw, time.Minute),
		session("s2", "the api docs", "/repo/api", adapter.Codex, time.Hour),
	}
	drv := &fakeDriver{cancelErr: &adapter.UnsupportedError{
		Runtime: adapter.OpenClaw, Capability: adapter.CapCancel,
	}}
	r := router(t, routing.Options{Sessions: live, Driver: drv})
	r.SetFocus("s1")

	d, _ := r.Route(ctx, routing.Request{Text: "add a rate limiter"})
	r.Confirm(d, "s1", "t-1")

	res, err := r.Undo(ctx, routing.UndoTarget{Session: "s2"})
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if res.Cancelled {
		t.Fatal("nothing was cancelled, so this must not report that it was")
	}
	if res.CancelNote == "" {
		t.Fatal("the user has to be told the old turn is still running")
	}
	if !strings.Contains(res.Announcement, "finish") {
		t.Errorf("announcement %q should say the old turn will finish anyway", res.Announcement)
	}
}

func TestUndoIntoANewSession(t *testing.T) {
	ctx := context.Background()
	live := routing.StaticSessions{session("s1", "the payments refactor", "/repo/payments", adapter.ClaudeCode, time.Minute)}
	drv := &fakeDriver{startID: "s9"}
	r := router(t, routing.Options{Sessions: live, Driver: drv})
	r.SetFocus("s1")

	d, _ := r.Route(ctx, routing.Request{Text: "what is the deploy story"})
	r.Confirm(d, "s1", "t-1")

	res, err := r.Undo(ctx, routing.UndoTarget{New: true, Runtime: adapter.Codex, Subject: "deploys"})
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if !res.NewSession || res.To != "s9" {
		t.Fatalf("res = %+v, want a new session", res)
	}
	if drv.started != 1 {
		t.Errorf("started %d sessions, want 1", drv.started)
	}
}

func TestUndoRefusesWhatItCannotDo(t *testing.T) {
	ctx := context.Background()
	live := routing.StaticSessions{session("s1", "one", "/repo/a", adapter.ClaudeCode, time.Minute)}

	t.Run("no driver", func(t *testing.T) {
		r := router(t, routing.Options{Sessions: live})
		if _, err := r.Undo(ctx, routing.UndoTarget{Session: "s1"}); !errors.Is(err, routing.ErrNoDriver) {
			t.Fatalf("err = %v, want ErrNoDriver", err)
		}
	})

	t.Run("nothing routed yet", func(t *testing.T) {
		r := router(t, routing.Options{Sessions: live, Driver: &fakeDriver{}})
		if _, err := r.Undo(ctx, routing.UndoTarget{Session: "s1"}); !errors.Is(err, routing.ErrNothingToUndo) {
			t.Fatalf("err = %v, want ErrNothingToUndo", err)
		}
	})

	t.Run("a control verb is not an undoable turn", func(t *testing.T) {
		r := router(t, routing.Options{Sessions: live, Driver: &fakeDriver{}})
		d, _ := r.Route(ctx, routing.Request{Text: "stop"})
		r.Confirm(d, "s1", "t-1")
		if _, err := r.Undo(ctx, routing.UndoTarget{Session: "s1"}); !errors.Is(err, routing.ErrNothingToUndo) {
			t.Fatalf("err = %v; \"stop\" did not put a turn anywhere", err)
		}
	})

	t.Run("undoing twice does not move it a third time", func(t *testing.T) {
		two := routing.StaticSessions{
			session("s1", "one", "/repo/a", adapter.ClaudeCode, time.Minute),
			session("s2", "two", "/repo/b", adapter.Codex, time.Minute),
		}
		drv := &fakeDriver{}
		r := router(t, routing.Options{Sessions: two, Driver: drv})
		r.SetFocus("s1")
		d, _ := r.Route(ctx, routing.Request{Text: "do the thing"})
		r.Confirm(d, "s1", "t-1")

		if _, err := r.Undo(ctx, routing.UndoTarget{Session: "s2"}); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Undo(ctx, routing.UndoTarget{Session: "s1"}); !errors.Is(err, routing.ErrNothingToUndo) {
			t.Fatalf("err = %v, want ErrNothingToUndo", err)
		}
	})
}

func TestJournalRecordsWhatWentWhere(t *testing.T) {
	ctx := context.Background()
	live := routing.StaticSessions{session("s1", "one", "/repo/a", adapter.ClaudeCode, time.Minute)}
	r := router(t, routing.Options{Sessions: live})

	d, _ := r.Route(ctx, routing.Request{Text: "first"})
	r.Confirm(d, "s1", "t-1")
	d2, _ := r.Route(ctx, routing.Request{Text: "second"})
	r.Confirm(d2, "s1", "t-2")

	j := r.Journal()
	if len(j) != 2 {
		t.Fatalf("journal has %d entries, want 2", len(j))
	}
	if j[0].Text != "first" || j[1].Text != "second" {
		t.Errorf("journal is out of order: %q, %q", j[0].Text, j[1].Text)
	}
	last, ok := r.Last()
	if !ok || last.Text != "second" || last.Turn != "t-2" {
		t.Errorf("Last = %+v", last)
	}
}

func TestRouterNeedsASessionList(t *testing.T) {
	if _, err := routing.New(routing.Options{}); err == nil {
		t.Fatal("a router with no session list should not build")
	}
}
