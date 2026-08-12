package event_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/event"
)

func meta() event.Meta {
	return event.Meta{
		Runtime: "claude-code",
		Session: "s-1",
		Turn:    "t-1",
		At:      time.Unix(1770000000, 0),
		Seq:     3,
	}
}

// ADAPTERS.md §5 marks exactly three events ⚑PING. Getting this wrong in
// either direction is a product failure: too few and a blocked session waits
// forever, too many and the glasses narrate every token out loud.
func TestOnlyThreeKindsPing(t *testing.T) {
	events := []event.Event{
		event.TurnStarted{Meta: meta()},
		event.TextDelta{Meta: meta(), Text: "hi"},
		event.Reasoning{Meta: meta(), Text: "thinking"},
		event.ToolStarted{Meta: meta(), Tool: "Bash"},
		event.ToolOutput{Meta: meta(), Chunk: "ok"},
		event.PlanUpdated{Meta: meta()},
		event.NewNeedsInput(meta(), event.InputSpec{Ask: event.InputPermission}, noReply),
		event.TurnCompleted{Meta: meta(), OK: true, StopReason: event.StopEndTurn},
		event.Error{Meta: meta(), Message: "boom"},
	}
	if len(events) != len(event.Kinds()) {
		t.Fatalf("this test covers %d events but there are %d kinds", len(events), len(event.Kinds()))
	}

	pinging := map[event.Kind]event.Ping{}
	for _, ev := range events {
		if p := ev.Ping(); p != event.PingNone {
			pinging[ev.Kind()] = p
		}
	}

	want := map[event.Kind]event.Ping{
		event.KindNeedsInput:    event.PingBlocking,
		event.KindTurnCompleted: event.PingInformational,
		event.KindError:         event.PingInformational,
	}
	if len(pinging) != len(want) {
		t.Fatalf("pinging kinds: got %v, want %v", pinging, want)
	}
	for k, w := range want {
		if pinging[k] != w {
			t.Fatalf("%s pings %v, want %v", k, pinging[k], w)
		}
	}

	// PingKinds() must agree with the behaviour above.
	for _, k := range event.PingKinds() {
		if _, ok := want[k]; !ok {
			t.Fatalf("PingKinds lists %s, which does not ping", k)
		}
	}
}

// A replayed event is not news. ACP's session/load replays the whole
// conversation as session/update notifications, and Claude Code echoes injected
// turns — pinging on those wakes someone about a turn from two weeks ago.
func TestReplayedEventsNeverPing(t *testing.T) {
	m := meta()
	m.Replay = true

	cases := []event.Event{
		event.TurnCompleted{Meta: m, OK: true, StopReason: event.StopEndTurn},
		event.Error{Meta: m, Message: "old failure"},
		event.NewNeedsInput(m, event.InputSpec{Ask: event.InputPermission}, noReply),
	}
	for _, ev := range cases {
		if p := ev.Ping(); p != event.PingNone {
			t.Fatalf("replayed %s pings %v, want none", ev.Kind(), p)
		}
	}
}

// Codex's error notification carries willRetry, and a retryable error is not a
// user-facing failure.
func TestRetryableErrorDoesNotPing(t *testing.T) {
	if p := (event.Error{Meta: meta(), Message: "429", Retryable: true}).Ping(); p != event.PingNone {
		t.Fatalf("retryable error pings %v, want none", p)
	}
	if p := (event.Error{Meta: meta(), Message: "auth"}).Ping(); p != event.PingInformational {
		t.Fatalf("non-retryable error pings %v, want informational", p)
	}
}

func TestStopReasons(t *testing.T) {
	if !event.StopEndTurn.OK() {
		t.Fatal("end_turn should be OK")
	}
	for _, s := range []event.StopReason{
		event.StopMaxTokens, event.StopMaxTurnRequests, event.StopRefusal,
		event.StopCancelled, event.StopError,
	} {
		if s.OK() {
			t.Fatalf("%s should not be OK", s)
		}
	}
	// ACP drops the user prompt and everything after it on a refusal, so the
	// instruction has to be carried again rather than retried on top of what is
	// no longer there.
	if event.StopRefusal.Retryable() {
		t.Fatal("a refusal must not be retryable: the context it referred to is gone")
	}
	if !event.StopMaxTokens.Retryable() {
		t.Fatal("max_tokens should be retryable")
	}
}

// Usage is all pointers so that "this runtime does not report it" is
// distinguishable from zero. ACP 0.4.5 has no usage object at all.
func TestUsageAbsenceIsNotZero(t *testing.T) {
	acp := event.TurnCompleted{Meta: meta(), OK: true, StopReason: event.StopEndTurn}
	if acp.Usage != nil {
		t.Fatal("an ACP turn must carry no usage at all")
	}
	if _, ok := acp.Usage.ContextPressure(); ok {
		t.Fatal("context pressure cannot be computed without a usage object")
	}

	// Codex: tokens but no money, and a nullable context window.
	codex := &event.Usage{
		InputTokens:       event.I64(90_000),
		CachedInputTokens: event.I64(10_000),
		TotalTokens:       event.I64(105_000),
	}
	if _, ok := codex.ContextPressure(); ok {
		t.Fatal("a null modelContextWindow must not yield a pressure figure")
	}
	codex.ContextWindow = event.I64(200_000)
	got, ok := codex.ContextPressure()
	if !ok || got < 0.49 || got > 0.51 {
		t.Fatalf("context pressure = %v (%v), want ~0.5", got, ok)
	}
	if codex.CostUSD != nil {
		t.Fatal("Codex reports no dollar figure anywhere in its contract")
	}
}

// Claude Code has no plan event. If a plan is emitted for it at all, it has to
// be marked as inference rather than observation.
func TestSynthesizedPlanIsMarked(t *testing.T) {
	native := event.PlanUpdated{Meta: meta(), Steps: []event.PlanStep{{Text: "run tests", Status: event.PlanInProgress}}}
	if native.Synthesized {
		t.Fatal("a native plan must not claim to be synthesized")
	}
	inferred := event.PlanUpdated{Meta: meta(), Synthesized: true}
	if !inferred.Synthesized {
		t.Fatal("flag did not survive")
	}
}

func TestEnvelopeCarriesThrough(t *testing.T) {
	ev := event.TextDelta{Meta: meta(), Text: "hello"}
	env := ev.Envelope()
	if env.Session != "s-1" || env.Turn != "t-1" || env.Seq != 3 || env.Runtime != "claude-code" {
		t.Fatalf("envelope lost fields: %+v", env)
	}
	if ev.Kind() != event.KindTextDelta {
		t.Fatalf("kind is %s", ev.Kind())
	}
}

func noReply(context.Context, event.Reply) error { return nil }

var _ = errors.Is
