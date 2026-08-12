package routing_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/routing"
	"github.com/luthor007/relay/relayd/internal/summarize"
)

func meta(session, turn string, seq uint64) event.Meta {
	return event.Meta{Runtime: "claude-code", Session: session, Turn: turn, At: now(), Seq: seq}
}

// ORCHESTRATOR.md §3b: given no event, the small model says "still working" or
// says nothing — it never invents a specific. This is the plumbing that makes
// that true rather than requested.
func TestNarrationWithNoEventsSaysNothingSpecific(t *testing.T) {
	ctx := context.Background()
	n := routing.NewNarration(routing.NarrationOptions{Session: "s1", Model: alwaysSpecific{}})

	for _, sp := range []summarize.Speech{
		n.Progress(ctx),
		n.Completed(ctx),
		n.NeedsInput(ctx),
	} {
		if sp.Text != routing.StillWorking {
			t.Errorf("with nothing observed the line was %q, want %q", sp.Text, routing.StillWorking)
		}
		if sp.Grounded {
			t.Error("a line with no events behind it must not claim to be grounded")
		}
	}
	if n.Observed() != 0 {
		t.Errorf("observed = %d", n.Observed())
	}
}

// alwaysSpecific is a model that would happily invent. It is here to prove the
// invention never gets the chance: with no events, the model is not called at
// all.
type alwaysSpecific struct{}

func (alwaysSpecific) Vendor() string { return "test" }
func (alwaysSpecific) Model() string  { return "invents" }
func (alwaysSpecific) API() llm.API   { return llm.APIOpenAI }
func (alwaysSpecific) Complete(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{Text: "Running the tests in payments/charge_test.go, 40 in so far."}, nil
}
func (alwaysSpecific) Stream(context.Context, llm.Request) (llm.Stream, error) { return nil, nil }
func (alwaysSpecific) Probe(context.Context) llm.ProbeResult                   { return llm.ProbeResult{} }

// The type has exactly one way in. This is a compile-time-ish assertion of the
// design rule: if someone adds a method that takes a digest or a string, this
// test is where the argument has to happen.
func TestNarrationOnlyAcceptsEvents(t *testing.T) {
	n := routing.NewNarration(routing.NarrationOptions{Session: "s1"})
	if ok := n.Observe(nil); ok {
		t.Error("a nil event must not count as an observation")
	}
	if ok := n.Observe(event.TextDelta{Meta: meta("s1", "t1", 1), Text: "hello"}); !ok {
		t.Error("a real event should be accepted")
	}
	if n.Observed() != 1 {
		t.Errorf("observed = %d, want 1", n.Observed())
	}
}

// A replayed event is history being re-read, not something happening.
// Narrating it would announce a turn from two weeks ago as if it were now.
func TestNarrationRefusesReplayedEvents(t *testing.T) {
	n := routing.NewNarration(routing.NarrationOptions{Session: "s1"})
	m := meta("s1", "t1", 1)
	m.Replay = true
	if n.Observe(event.ToolStarted{Meta: m, ID: "x", Tool: "Bash", Target: "go test ./..."}) {
		t.Fatal("a replayed event was accepted")
	}
	if n.Observed() != 0 || n.Dropped() != 1 {
		t.Fatalf("observed = %d dropped = %d", n.Observed(), n.Dropped())
	}
	if sp := n.Progress(context.Background()); sp.Text != routing.StillWorking {
		t.Errorf("line = %q; a replay must not become a narration", sp.Text)
	}
}

// A digest that mixes two turns narrates neither, so events from another
// session are dropped rather than merged.
func TestNarrationRefusesAnotherSessionsEvents(t *testing.T) {
	n := routing.NewNarration(routing.NarrationOptions{Session: "s1"})
	if n.Observe(event.TextDelta{Meta: meta("s2", "t1", 1), Text: "not mine"}) {
		t.Fatal("an event from another session was accepted")
	}
	if n.Dropped() != 1 {
		t.Fatalf("dropped = %d", n.Dropped())
	}
}

// "It's done" is a specific. Without the event that says so, it does not get
// said — even if the caller asks for the completion line.
func TestCompletedDoesNotClaimCompletionWithoutTheEvent(t *testing.T) {
	ctx := context.Background()
	n := routing.NewNarration(routing.NarrationOptions{Session: "s1"})
	n.Observe(event.TurnStarted{Meta: meta("s1", "t1", 1)})
	n.Observe(event.ToolStarted{Meta: meta("s1", "t1", 2), ID: "x", Tool: "Bash", Target: "go test ./..."})

	if n.Complete() {
		t.Fatal("no TurnCompleted has been seen")
	}
	sp := n.Completed(ctx)
	if sp.Moment != summarize.MomentProgress {
		t.Errorf("moment = %q; before the completion event this is progress, not a completion", sp.Moment)
	}
	if strings.Contains(strings.ToLower(sp.Text), "done") ||
		strings.Contains(strings.ToLower(sp.Text), "finish") {
		t.Errorf("line = %q; it has not finished and nothing said it had", sp.Text)
	}

	n.Observe(event.TurnCompleted{Meta: meta("s1", "t1", 3), OK: true, StopReason: event.StopEndTurn,
		Duration: 2 * time.Second})
	if !n.Complete() {
		t.Fatal("the completion event was not recorded")
	}
	if sp := n.Completed(ctx); sp.Moment != summarize.MomentCompleted {
		t.Errorf("moment = %q, want a completion now that the event exists", sp.Moment)
	}
}

// What it *does* say comes from the events. This is the positive half: the
// narration is a rephrasing of what the adapter observed.
func TestNarrationSpeaksFromTheEventsItGot(t *testing.T) {
	ctx := context.Background()
	n := routing.NewNarration(routing.NarrationOptions{Session: "s1"})
	n.Observe(event.TurnStarted{Meta: meta("s1", "t1", 1)})
	n.Observe(event.ToolStarted{Meta: meta("s1", "t1", 2), ID: "x", Tool: "Bash", Target: "go test ./..."})

	sp := n.Progress(ctx)
	if !sp.Grounded {
		t.Fatalf("line %q is not marked grounded despite an observed tool call", sp.Text)
	}
	if !strings.Contains(sp.Text, "Bash") && !strings.Contains(sp.Text, "go test") {
		t.Errorf("line = %q; it should name what it actually saw", sp.Text)
	}
}

func TestResetClearsTheTurn(t *testing.T) {
	ctx := context.Background()
	n := routing.NewNarration(routing.NarrationOptions{Session: "s1"})
	n.Observe(event.ToolStarted{Meta: meta("s1", "t1", 1), ID: "x", Tool: "Bash", Target: "go test"})
	n.Reset()
	if n.Observed() != 0 {
		t.Fatalf("observed = %d after reset", n.Observed())
	}
	if sp := n.Progress(ctx); sp.Text != routing.StillWorking {
		t.Errorf("line = %q; a new turn starts vague again rather than repeating the last one", sp.Text)
	}
}

// The announcement is the one line that is not grounded in agent events, and it
// is honest for a different reason: it is a decision this process just made,
// phrased deterministically. It is also the acknowledgement (SYSTEM.md §7b).
func TestAnnounceSpeaksTheRoutingDecision(t *testing.T) {
	n := routing.NewNarration(routing.NarrationOptions{Session: "s1", Model: alwaysSpecific{}})
	d := routing.Decision{
		Kind: routing.KindContinue, Session: "s1", Subject: "the payments refactor",
		Reason: routing.ReasonFocus,
	}
	d.Announcement = routing.Announce(d)

	sp := n.Announce(d)
	if sp.Source != summarize.SourceTemplate {
		t.Error("the announcement is never phrased by a model — it is the audit trail")
	}
	if !strings.Contains(sp.Text, "payments refactor") {
		t.Errorf("line = %q", sp.Text)
	}
	if sp.Moment != summarize.MomentAck {
		t.Errorf("moment = %q, want the acknowledgement", sp.Moment)
	}
}

// MEMORY.md §8's own worked example has to fit in the budget. It does not fit
// ADAPTERS.md §6's 40-character acknowledgement, which is why AnnounceCap is
// the progress budget instead.
func TestAnnouncementBudgetHoldsTheDocsOwnExample(t *testing.T) {
	d := routing.Decision{
		Kind: routing.KindNew, Runtime: "codex", Subject: "the api repo",
		Reason: routing.ReasonNothingLive,
	}
	line := routing.Announce(d)
	if strings.HasSuffix(line, "…") {
		t.Fatalf("the announcement was clipped: %q", line)
	}
	if !strings.Contains(line, "Codex") || !strings.Contains(line, "api repo") {
		t.Fatalf("line = %q; MEMORY.md §8 says \"starting a new Codex session on the API repo\"", line)
	}
	if len([]rune(line)) > routing.AnnounceCap {
		t.Fatalf("line is %d chars, over the %d budget", len([]rune(line)), routing.AnnounceCap)
	}
	if len([]rune(line)) <= summarize.CapAck {
		t.Logf("note: %q fits the ack budget after all (%d ≤ %d)", line, len([]rune(line)), summarize.CapAck)
	}
}
