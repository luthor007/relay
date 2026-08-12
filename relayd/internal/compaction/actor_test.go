package compaction

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/adapter/fake"
)

type oneSession struct {
	id string
	s  adapter.Session
}

func (o oneSession) Session(id string) (adapter.Session, bool) {
	if id != o.id || o.s == nil {
		return nil, false
	}
	return o.s, true
}

func fakeSession(t *testing.T, rt adapter.Runtime, id string) *fake.Session {
	t.Helper()
	a := fake.New(fake.Options{Runtime: rt})
	s, err := a.Start(context.Background(), adapter.SessionOptions{ID: id, Workspace: "/repos/api"})
	if err != nil {
		t.Fatal(err)
	}
	return s.(*fake.Session)
}

// Every command runtime gets the command MEMORY.md §9 recorded for it, sent as
// an ordinary user message.
func TestCompactSendsTheDocumentedCommand(t *testing.T) {
	want := map[adapter.Runtime]string{
		adapter.ClaudeCode: "/compact",
		adapter.OpenClaw:   "/compact",
		adapter.Hermes:     "/compress",
	}
	for rt, cmd := range want {
		sess := fakeSession(t, rt, "s1")
		act := &TurnCompactor{Sessions: oneSession{id: "s1", s: sess}}

		if err := act.Compact(context.Background(), SessionView{ID: "s1", Runtime: rt}); err != nil {
			t.Fatalf("%s: %v", rt, err)
		}
		sent := sess.Sent()
		if len(sent) != 1 || sent[0].Text != cmd {
			t.Fatalf("%s sent %+v, want %q", rt, sent, cmd)
		}
	}
}

// Codex compacts through thread/compact/start, and a session that implements it
// must be used in preference to typing a command at it.
type compactableSession struct {
	*fake.Session
	calls int
	err   error
}

func (c *compactableSession) Compact(context.Context) error {
	c.calls++
	return c.err
}

func TestCompactPrefersTheProtocolCall(t *testing.T) {
	inner := fakeSession(t, adapter.Codex, "s1")
	sess := &compactableSession{Session: inner}
	act := &TurnCompactor{Sessions: oneSession{id: "s1", s: sess}}

	if err := act.Compact(context.Background(), SessionView{ID: "s1", Runtime: adapter.Codex}); err != nil {
		t.Fatal(err)
	}
	if sess.calls != 1 {
		t.Fatalf("calls = %d, want the protocol method", sess.calls)
	}
	if len(inner.Sent()) != 0 {
		t.Fatalf("sent %+v: a command was typed at a runtime with a real method", inner.Sent())
	}
}

// OpenCode's endpoint is outside ACP. Sending "/compact" to an agent that never
// advertised the command would be inventing a capability.
func TestOpenCodeCompactionIsRefusedRatherThanGuessed(t *testing.T) {
	sess := fakeSession(t, adapter.OpenCode, "s1")
	act := &TurnCompactor{Sessions: oneSession{id: "s1", s: sess}}

	err := act.Compact(context.Background(), SessionView{ID: "s1", Runtime: adapter.OpenCode})
	if err == nil {
		t.Fatal("want an error rather than a hopeful command")
	}
	if !strings.Contains(err.Error(), "outside the agent protocol") {
		t.Fatalf("err = %v", err)
	}
	if len(sess.Sent()) != 0 {
		t.Fatalf("sent %+v", sess.Sent())
	}
}

func TestCompactRefusesAnUnknownRuntime(t *testing.T) {
	sess := fakeSession(t, adapter.ClaudeCode, "s1")
	act := &TurnCompactor{Sessions: oneSession{id: "s1", s: sess}}
	if err := act.Compact(context.Background(), SessionView{ID: "s1", Runtime: "gemini-cli"}); err == nil {
		t.Fatal("want an error for a runtime with no documented mechanism")
	}
	if len(sess.Sent()) != 0 {
		t.Fatal("nothing should have been sent")
	}
}

func TestCompactOnADeadSession(t *testing.T) {
	act := &TurnCompactor{Sessions: oneSession{id: "other"}}
	err := act.Compact(context.Background(), SessionView{ID: "s1", Runtime: adapter.ClaudeCode})
	if !errors.Is(err, ErrNoSession) {
		t.Fatalf("err = %v, want ErrNoSession", err)
	}
	if err := (&TurnCompactor{}).Flush(context.Background(), SessionView{ID: "s1"}); !errors.Is(err, ErrNotWired) {
		t.Fatalf("err = %v, want ErrNotWired", err)
	}
}

func TestFlushSendsAMarkedSilentTurn(t *testing.T) {
	sess := fakeSession(t, adapter.ClaudeCode, "s1")
	at := time.Unix(1_700_000_000, 0)
	act := &TurnCompactor{Sessions: oneSession{id: "s1", s: sess}, Now: func() time.Time { return at }}

	if err := act.Flush(context.Background(), SessionView{ID: "s1", Runtime: adapter.ClaudeCode}); err != nil {
		t.Fatal(err)
	}
	sent := sess.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent = %+v", sent)
	}
	if !IsFlush(sent[0].ID) {
		t.Fatalf("turn id %q is not marked as a flush, so something downstream will speak it", sent[0].ID)
	}
	if !strings.Contains(sent[0].Text, NoReply) {
		t.Fatal("the flush prompt must carry the NO_REPLY convention")
	}
}

func TestDelegatedHalvesSayWhoOwnsThem(t *testing.T) {
	act := &TurnCompactor{Sessions: oneSession{}}
	for _, err := range []error{
		act.Handoff(context.Background(), SessionView{}, Brief{}),
		act.StartNew(context.Background(), SessionView{}),
	} {
		if !errors.Is(err, ErrNotWired) {
			t.Fatalf("err = %v, want ErrNotWired", err)
		}
		if !strings.Contains(err.Error(), "routing") {
			t.Fatalf("err = %v, want it to name the owner", err)
		}
	}

	var handed, started bool
	act.Handoffs = func(context.Context, SessionView, Brief) error { handed = true; return nil }
	act.NewSessions = func(context.Context, SessionView) error { started = true; return nil }
	if err := act.Handoff(context.Background(), SessionView{}, Brief{}); err != nil {
		t.Fatal(err)
	}
	if err := act.StartNew(context.Background(), SessionView{}); err != nil {
		t.Fatal(err)
	}
	if !handed || !started {
		t.Fatal("the delegates were not called")
	}
}

// End to end through the sweeper: a full idle Codex session is compacted by the
// protocol call, with no command typed anywhere.
func TestSweeperDrivesTheRealActor(t *testing.T) {
	inner := fakeSession(t, adapter.Codex, "s1")
	sess := &compactableSession{Session: inner}

	v := fullView(t, "s1", adapter.Codex, 800)
	s, err := NewSweeper(SweeperOptions{
		Sessions: &fakeSessions{views: []SessionView{v}},
		Actor:    &TurnCompactor{Sessions: oneSession{id: "s1", s: sess}},
		Now:      func() time.Time { return sweepNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Acted != 1 || sess.calls != 1 {
		t.Fatalf("acted = %d, compact calls = %d (%+v)", res.Acted, sess.calls, res.Outcomes[0])
	}
}
