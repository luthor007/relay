package bus_test

import (
	"context"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/adapter/fake"
	"github.com/luthor007/relay/relayd/internal/bus"
	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/logx"
)

func meta(session string, seq uint64) event.Meta {
	return event.Meta{
		Runtime: "claude-code", Session: session, Turn: "t1",
		At: time.Unix(0, 0), Seq: seq,
	}
}

func recv(t *testing.T, ch <-chan event.Event) event.Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("subscription closed while waiting for an event")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an event")
		return nil
	}
}

func TestFanOutReachesEverySubscriber(t *testing.T) {
	b := bus.New(bus.Options{Log: logx.Discard()})
	defer b.Close()

	a := b.Subscribe("a", bus.Filter{})
	defer a.Close()
	c := b.Subscribe("b", bus.Filter{})
	defer c.Close()

	b.Publish(event.TurnStarted{Meta: meta("s1", 1)})

	for _, s := range []*bus.Subscription{a, c} {
		if got := recv(t, s.C()).Kind(); got != event.KindTurnStarted {
			t.Fatalf("%s got %s", s.Name(), got)
		}
	}
	if b.Published() != 1 {
		t.Fatalf("published = %d, want 1", b.Published())
	}
}

func TestFilterNarrowsBySessionRuntimeAndKind(t *testing.T) {
	b := bus.New(bus.Options{Log: logx.Discard()})
	defer b.Close()

	only := b.Subscribe("s2-only", bus.Filter{Sessions: []string{"s2"}})
	defer only.Close()
	codexOnly := b.Subscribe("codex", bus.Filter{Runtimes: []string{"codex"}})
	defer codexOnly.Close()
	completions := b.Subscribe("completions", bus.Filter{Kinds: []event.Kind{event.KindTurnCompleted}})
	defer completions.Close()

	b.Publish(event.TurnStarted{Meta: meta("s1", 1)})
	b.Publish(event.TurnStarted{Meta: meta("s2", 2)})
	b.Publish(event.TurnCompleted{Meta: meta("s1", 3), OK: true, StopReason: event.StopEndTurn})

	if got := recv(t, only.C()).Envelope().Session; got != "s2" {
		t.Fatalf("session filter let through %q", got)
	}
	if got := recv(t, completions.C()).Kind(); got != event.KindTurnCompleted {
		t.Fatalf("kind filter let through %s", got)
	}
	select {
	case ev := <-codexOnly.C():
		t.Fatalf("runtime filter let through a %s event from %s", ev.Kind(), ev.Envelope().Runtime)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestReplayIsExcludedOnRequest(t *testing.T) {
	b := bus.New(bus.Options{Log: logx.Discard()})
	defer b.Close()

	live := b.Subscribe("live", bus.Filter{ExcludeReplay: true})
	defer live.Close()

	m := meta("s1", 1)
	m.Replay = true
	b.Publish(event.TurnCompleted{Meta: m, OK: true})
	b.Publish(event.TurnCompleted{Meta: meta("s1", 2), OK: true})

	got := recv(t, live.C())
	if got.Envelope().Seq != 2 {
		t.Fatalf("replayed event reached a live-only subscriber (seq %d)", got.Envelope().Seq)
	}
}

// A slow subscriber must lose streaming chatter rather than block a runtime —
// and must never lose a turn boundary or a blocked question, because those are
// state transitions and losing one makes the registry wrong.
func TestSlowSubscriberDropsChatterAndKeepsTransitions(t *testing.T) {
	b := bus.New(bus.Options{Buffer: 4, Log: logx.Discard()})
	defer b.Close()

	slow := b.Subscribe("slow", bus.Filter{})
	defer slow.Close()

	// Nobody reads. Overrun the queue with deltas, then push the transitions
	// that must survive.
	for i := 0; i < 200; i++ {
		b.Publish(event.TextDelta{Meta: meta("s1", uint64(i)), Text: "x"})
	}
	b.Publish(event.TurnCompleted{Meta: meta("s1", 900), OK: true, StopReason: event.StopEndTurn})
	b.Publish(event.ToolStarted{Meta: meta("s1", 901), ID: "tool-1", Tool: "Bash"})

	// Give the drain goroutine a moment to have taken at most a couple.
	deadline := time.After(2 * time.Second)
	kinds := map[event.Kind]int{}
	for {
		select {
		case ev := <-slow.C():
			kinds[ev.Kind()]++
			if kinds[event.KindTurnCompleted] > 0 && kinds[event.KindToolStarted] > 0 {
				if slow.Dropped() == 0 {
					t.Fatal("expected drops with a 4-deep queue and 200 deltas")
				}
				if kinds[event.KindTextDelta] >= 200 {
					t.Fatalf("nothing was dropped: %d deltas", kinds[event.KindTextDelta])
				}
				return
			}
		case <-deadline:
			t.Fatalf("transitions never arrived: %v (dropped %d)", kinds, slow.Dropped())
		}
	}
}

func TestBlockedQuestionIsNeverDropped(t *testing.T) {
	b := bus.New(bus.Options{Buffer: 2, Log: logx.Discard()})
	defer b.Close()

	slow := b.Subscribe("slow", bus.Filter{})
	defer slow.Close()

	reply := func(context.Context, event.Reply) error { return nil }
	var asks []*event.NeedsInput
	for i := 0; i < 20; i++ {
		n := event.NewNeedsInput(meta("s1", uint64(i)), event.InputSpec{
			Ask:    event.InputPermission,
			Prompt: "run rm -rf?",
		}, reply)
		asks = append(asks, n)
		b.Publish(n)
	}
	for i := 0; i < 50; i++ {
		b.Publish(event.TextDelta{Meta: meta("s1", uint64(100+i)), Text: "x"})
	}

	seen := 0
	deadline := time.After(3 * time.Second)
	for seen < len(asks) {
		select {
		case ev := <-slow.C():
			if ev.Kind() == event.KindNeedsInput {
				seen++
			}
		case <-deadline:
			t.Fatalf("only %d of %d blocked questions survived backpressure", seen, len(asks))
		}
	}
}

func TestAttachPumpsAnAdapterSessionAndReturnsWhenItEnds(t *testing.T) {
	b := bus.New(bus.Options{Log: logx.Discard()})
	defer b.Close()

	sub := b.Subscribe("test", bus.Filter{})
	defer sub.Close()

	ad := fake.New(fake.Options{Runtime: adapter.ClaudeCode})
	sess, err := ad.Start(context.Background(), adapter.SessionOptions{ID: "s1", Workspace: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	fs := sess.(*fake.Session)

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Attach(context.Background(), sess.Events())
	}()

	fake.HappyTurn(fs, "turn-1", "working on it")

	kinds := []event.Kind{
		event.KindTurnStarted, event.KindTextDelta, event.KindToolStarted,
		event.KindToolOutput, event.KindTurnCompleted,
	}
	for _, want := range kinds {
		if got := recv(t, sub.C()).Kind(); got != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	}

	_ = sess.Close(context.Background())
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Attach did not return when the session's channel closed")
	}
}

func TestSubscriptionCloseIsIdempotentAndClosesTheChannel(t *testing.T) {
	b := bus.New(bus.Options{Log: logx.Discard()})
	sub := b.Subscribe("x", bus.Filter{})
	sub.Close()
	sub.Close()

	select {
	case _, ok := <-sub.C():
		if ok {
			t.Fatal("received a value after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("channel was not closed")
	}
	b.Close()
	b.Close()
}

func TestCloseClosesEverySubscription(t *testing.T) {
	b := bus.New(bus.Options{Log: logx.Discard()})
	a := b.Subscribe("a", bus.Filter{})
	c := b.Subscribe("b", bus.Filter{})
	if b.Subscribers() != 2 {
		t.Fatalf("subscribers = %d", b.Subscribers())
	}
	b.Close()

	for _, s := range []*bus.Subscription{a, c} {
		select {
		case _, ok := <-s.C():
			if ok {
				t.Fatalf("%s still delivering after bus close", s.Name())
			}
		case <-time.After(time.Second):
			t.Fatalf("%s was not closed", s.Name())
		}
	}
	// Publishing after close is a no-op rather than a panic: shutdown races a
	// runtime that is still emitting, every time.
	b.Publish(event.TurnStarted{Meta: meta("s1", 1)})
}

func TestPingFilterSelectsOnlyTheThreeThatReachAUser(t *testing.T) {
	b := bus.New(bus.Options{Log: logx.Discard()})
	defer b.Close()

	pings := b.Subscribe("pings", bus.Filter{Pings: true})
	defer pings.Close()

	b.Publish(event.TextDelta{Meta: meta("s1", 1), Text: "hi"})
	b.Publish(event.Reasoning{Meta: meta("s1", 2), Text: "thinking"})
	b.Publish(event.Error{Meta: meta("s1", 3), Message: "retrying", Retryable: true})
	b.Publish(event.TurnCompleted{Meta: meta("s1", 4), OK: true})

	got := recv(t, pings.C())
	if got.Kind() != event.KindTurnCompleted {
		t.Fatalf("ping filter let through %s (a retryable error must not ping)", got.Kind())
	}
}
