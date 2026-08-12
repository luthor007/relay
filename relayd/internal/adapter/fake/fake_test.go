package fake_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/adapter/fake"
	"github.com/luthor007/relay/relayd/internal/event"
)

func TestFakeSatisfiesTheInterface(t *testing.T) {
	ctx := context.Background()
	a := fake.New(fake.Options{Runtime: adapter.Codex})

	var _ adapter.Adapter = a
	if a.Runtime() != adapter.Codex {
		t.Fatalf("runtime is %s", a.Runtime())
	}

	s, err := a.Start(ctx, adapter.SessionOptions{ID: "s-1", Workspace: "/repo"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s.ID() != "s-1" || s.Native() != "s-1" || s.Runtime() != adapter.Codex {
		t.Fatalf("session identity: %s %s %s", s.ID(), s.Native(), s.Runtime())
	}
	if err := a.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := s.Send(ctx, adapter.Turn{Text: "hi"}); !errors.Is(err, adapter.ErrSessionClosed) {
		t.Fatalf("send after close: %v", err)
	}
}

// A whole turn, consumed off the event channel the way the registry will.
func TestScriptedTurn(t *testing.T) {
	ctx := context.Background()
	a := fake.New(fake.Options{Runtime: adapter.ClaudeCode})

	sess, err := a.Start(ctx, adapter.SessionOptions{ID: "s-1", Workspace: "/repo"})
	if err != nil {
		t.Fatal(err)
	}
	s := sess.(*fake.Session)
	s.OnSend = func(s *fake.Session, turnID string, _ adapter.Turn) {
		fake.HappyTurn(s, turnID, "tests pass")
	}

	turnID, err := s.Send(ctx, adapter.Turn{Text: "run the tests"})
	if err != nil {
		t.Fatal(err)
	}

	var kinds []event.Kind
	var completed event.TurnCompleted
	deadline := time.After(2 * time.Second)
	for len(kinds) < 5 {
		select {
		case ev := <-s.Events():
			kinds = append(kinds, ev.Kind())
			if c, ok := ev.(event.TurnCompleted); ok {
				completed = c
			}
			if ev.Envelope().Turn != turnID {
				t.Fatalf("%s carried turn %q, want %q", ev.Kind(), ev.Envelope().Turn, turnID)
			}
		case <-deadline:
			t.Fatalf("only got %v", kinds)
		}
	}

	want := []event.Kind{
		event.KindTurnStarted, event.KindTextDelta, event.KindToolStarted,
		event.KindToolOutput, event.KindTurnCompleted,
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("event %d is %s, want %s (got %v)", i, kinds[i], want[i], kinds)
		}
	}
	if !completed.OK || completed.StopReason != event.StopEndTurn {
		t.Fatalf("turn did not complete cleanly: %+v", completed)
	}
	if completed.Envelope().Seq == 0 {
		t.Fatal("events must carry a monotonic sequence")
	}
	if got := s.Sent(); len(got) != 1 || got[0].Text != "run the tests" {
		t.Fatalf("Sent: %+v", got)
	}
}

// The fake refuses what its capability set says the runtime cannot do — which
// is how a test gets an ACP-shaped runtime without an ACP runtime.
func TestFakeHonoursItsCapabilities(t *testing.T) {
	ctx := context.Background()

	acp := fake.New(fake.Options{Runtime: adapter.OpenClaw})
	s, err := acp.Start(ctx, adapter.SessionOptions{ID: "s-1"})
	if err != nil {
		t.Fatal(err)
	}
	turnID, err := s.Send(ctx, adapter.Turn{Text: "start"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Steer(ctx, turnID, adapter.Turn{Text: "also update the changelog"}); !errors.Is(err, adapter.ErrUnsupported) {
		t.Fatalf("ACP steering returned %v, want ErrUnsupported", err)
	}
	// Cancel-and-re-prompt is the fallback, and it works.
	if err := s.Cancel(ctx, turnID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	// Resume on ACP is unprobed, so the registry gets told rather than lied to.
	if _, err := acp.Resume(ctx, adapter.SessionRef{ID: "s-1"}, adapter.SessionOptions{}); !errors.Is(err, adapter.ErrUnsupported) {
		t.Fatalf("ACP resume returned %v, want ErrUnsupported", err)
	}

	// Claude Code can steer, and only the active turn.
	cc := fake.New(fake.Options{Runtime: adapter.ClaudeCode})
	s2, err := cc.Start(ctx, adapter.SessionOptions{ID: "s-2"})
	if err != nil {
		t.Fatal(err)
	}
	tid, err := s2.Send(ctx, adapter.Turn{Text: "start"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.Steer(ctx, tid, adapter.Turn{Text: "and the changelog"}); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if err := s2.Steer(ctx, "some-older-turn", adapter.Turn{Text: "late"}); !errors.Is(err, adapter.ErrTurnNotActive) {
		t.Fatalf("stale steer returned %v, want ErrTurnNotActive", err)
	}
	if got := s2.(*fake.Session).Steers(); len(got) != 1 {
		t.Fatalf("Steers: %+v", got)
	}
}

// Cancelling has to resolve every outstanding permission request, or the
// agent's turn cannot unwind.
func TestCancelResolvesOutstandingQuestions(t *testing.T) {
	ctx := context.Background()
	a := fake.New(fake.Options{Runtime: adapter.OpenCode})
	sess, err := a.Start(ctx, adapter.SessionOptions{ID: "s-1"})
	if err != nil {
		t.Fatal(err)
	}
	s := sess.(*fake.Session)

	turnID, err := s.Send(ctx, adapter.Turn{Text: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	q := s.Ask(turnID, event.InputSpec{
		Ask:     event.InputPermission,
		Prompt:  "Run npm test?",
		Options: []event.Option{{ID: "o1", Name: "Allow", Kind: event.OptionAllowOnce}},
	})

	got := <-s.Events()
	if got != event.Event(q) {
		t.Fatal("the NeedsInput on the stream must be the same object the adapter holds")
	}
	if got.Ping() != event.PingBlocking {
		t.Fatalf("a permission request pings %v, want blocking", got.Ping())
	}

	if err := s.Cancel(ctx, turnID); err != nil {
		t.Fatal(err)
	}
	if !q.Answered() {
		t.Fatal("cancelling left a permission request outstanding")
	}
	if err := q.Reply(ctx, event.Reply{OptionID: "o1"}); !errors.Is(err, event.ErrWithdrawn) {
		t.Fatalf("reply after cancel: %v", err)
	}
}

func TestAskRepliesReachTheSession(t *testing.T) {
	ctx := context.Background()
	a := fake.New(fake.Options{Runtime: adapter.Codex})
	sess, _ := a.Start(ctx, adapter.SessionOptions{ID: "s-1"})
	s := sess.(*fake.Session)

	q := s.Ask("t-1", event.InputSpec{
		Ask: event.InputPermission,
		Options: []event.Option{
			{ID: "allow", Name: "Allow", Kind: event.OptionAllowOnce},
			{ID: "stop", Name: "No, stop", Kind: event.OptionRejectOnce},
		},
	})
	<-s.Events()

	if err := q.Reply(ctx, event.Reply{
		OptionID: "stop", Decision: event.DecisionDeny, Interrupt: true,
	}); err != nil {
		t.Fatal(err)
	}
	got := s.Replies()
	if len(got) != 1 || got[0].OptionID != "stop" || !got[0].Interrupt {
		t.Fatalf("Replies: %+v", got)
	}
}

// Closing a session closes its event channel exactly once and retracts any
// pending question, so a consumer's range loop terminates.
func TestCloseIsIdempotent(t *testing.T) {
	ctx := context.Background()
	a := fake.New(fake.Options{Runtime: adapter.Hermes})
	sess, _ := a.Start(ctx, adapter.SessionOptions{ID: "s-1"})
	s := sess.(*fake.Session)

	q := s.Ask("t-1", event.InputSpec{Ask: event.InputPermission})
	<-s.Events()

	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if !q.Answered() {
		t.Fatal("closing must retract pending questions")
	}
	for range s.Events() {
		t.Fatal("no events should follow a close")
	}
	if s.Emit(event.TurnStarted{}) {
		t.Fatal("Emit must refuse after close rather than panic on a closed channel")
	}
}

func TestStartErrorIsInjectable(t *testing.T) {
	boom := errors.New("no binary on PATH")
	a := fake.New(fake.Options{Runtime: adapter.Codex, StartErr: boom})
	if _, err := a.Start(context.Background(), adapter.SessionOptions{}); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}

func TestCapabilitiesCanBeOverriddenPerSession(t *testing.T) {
	ctx := context.Background()
	caps := adapter.Baseline(adapter.OpenClaw).
		With(adapter.CapResume, adapter.SupportYes, "loadSession advertised")
	a := fake.New(fake.Options{Runtime: adapter.OpenClaw, Caps: &caps})

	s, err := a.Resume(ctx, adapter.SessionRef{ID: "s-1", Native: "agent:main:main"}, adapter.SessionOptions{})
	if err != nil {
		t.Fatalf("Resume with loadSession advertised: %v", err)
	}
	if s.Native() != "agent:main:main" {
		t.Fatalf("native id is %q — OpenClaw session keys look like agent:main:main", s.Native())
	}

	fs := s.(*fake.Session)
	fs.SetCapabilities(fs.Capabilities().With(adapter.CapCancel, adapter.SupportNo, "test"))
	if err := fs.Cancel(ctx, "t-1"); !errors.Is(err, adapter.ErrUnsupported) {
		t.Fatalf("got %v", err)
	}
}

func TestGeneratedIDs(t *testing.T) {
	ctx := context.Background()
	a := fake.New(fake.Options{})
	s1, _ := a.Start(ctx, adapter.SessionOptions{})
	s2, _ := a.Start(ctx, adapter.SessionOptions{})
	if s1.ID() == "" || s1.ID() == s2.ID() {
		t.Fatalf("ids: %q %q", s1.ID(), s2.ID())
	}
	if len(a.Sessions()) != 2 {
		t.Fatalf("%d sessions tracked", len(a.Sessions()))
	}
}
