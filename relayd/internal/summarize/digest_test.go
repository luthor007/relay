package summarize_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/summarize"
)

var base = time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)

func meta(seq int) event.Meta {
	return event.Meta{
		Runtime: "claude-code", Session: "s1", Turn: "t1",
		At: base.Add(time.Duration(seq) * time.Second), Seq: uint64(seq),
	}
}

// A successful turn with a tool call and forty kilobytes of test output — the
// ordinary case, and the one where "summarise events, not the transcript"
// earns its keep.
func passingTurn() []event.Event {
	return []event.Event{
		event.TurnStarted{Meta: meta(0)},
		event.Reasoning{Meta: meta(1), Text: "I should run the tests first."},
		event.ToolStarted{Meta: meta(2), ID: "tool_1", Tool: "Bash", Target: "go test ./..."},
		event.ToolOutput{Meta: meta(3), ID: "tool_1", Chunk: strings.Repeat("ok  github.com/luthor007/relay\n", 1500)},
		event.ToolOutput{Meta: meta(4), ID: "tool_1", Status: event.ToolCompleted},
		event.TextDelta{Meta: meta(5), Text: "Tests pass. "},
		event.TextDelta{Meta: meta(6), Text: "Two files changed."},
		event.TurnCompleted{
			Meta: meta(7), OK: true, StopReason: event.StopEndTurn, Duration: 7 * time.Second,
			Usage: &event.Usage{
				InputTokens:   event.I64(33497),
				TotalTokens:   event.I64(34000),
				ContextWindow: event.I64(1000000),
				CostUSD:       event.F64(0.12),
			},
		},
	}
}

func TestDigestHoldsNoToolOutput(t *testing.T) {
	d := summarize.DigestOf(passingTurn())

	if len(d.Tools) != 1 {
		t.Fatalf("want one tool, got %d", len(d.Tools))
	}
	tool := d.Tools[0]
	if tool.Tool != "Bash" || tool.Target != "go test ./..." {
		t.Fatalf("tool: %+v", tool)
	}
	if tool.Status != event.ToolCompleted {
		t.Fatalf("status %q — the update must merge onto the ToolStarted", tool.Status)
	}
	if tool.OutputBytes < 39000 {
		t.Fatalf("output volume not counted: %d", tool.OutputBytes)
	}

	// The forty kilobytes of test output must exist as a number and nowhere
	// else. A type that cannot hold a diff cannot leak one into a prompt or an
	// ear, and that is the whole reason this type is shaped this way.
	brief := summarize.Brief(summarize.MomentCompleted, d)
	if strings.Contains(brief, "github.com/luthor007/relay\n") {
		t.Fatal("tool output reached the brief")
	}
	if len(brief) > 1000 {
		t.Fatalf("brief is %d bytes; it is meant to be a handful of lines", len(brief))
	}
}

func TestDigestNeverCarriesReasoning(t *testing.T) {
	d := summarize.DigestOf(passingTurn())
	if !d.SawReasoning {
		t.Fatal("reasoning not noticed")
	}
	// Noticed, never quoted. Reasoning is not spoken on any runtime.
	if strings.Contains(d.Text, "I should run the tests") {
		t.Fatalf("reasoning leaked into the text: %q", d.Text)
	}
	if strings.Contains(summarize.Brief(summarize.MomentCompleted, d), "I should run the tests") {
		t.Fatal("reasoning leaked into the brief")
	}
}

func TestDigestOutcome(t *testing.T) {
	d := summarize.DigestOf(passingTurn())
	if !d.Completed || !d.OK || d.StopReason != event.StopEndTurn {
		t.Fatalf("outcome: %+v", d)
	}
	if d.Text != "Tests pass. Two files changed." {
		t.Fatalf("text %q", d.Text)
	}
	if got := d.Duration(); got != 7*time.Second {
		t.Fatalf("duration %v", got)
	}
	if p, ok := d.Usage.ContextPressure(); !ok || p < 0.03 || p > 0.04 {
		t.Fatalf("pressure %v %v", p, ok)
	}
}

// A runtime with no plan event must not produce a digest that looks like it has
// an empty plan: "not reported" and "nothing planned" lead to different
// narration, and only one of them is honest for Claude Code.
func TestDigestDistinguishesAbsentPlanFromEmptyPlan(t *testing.T) {
	none := summarize.DigestOf(passingTurn())
	if none.PlanObserved {
		t.Fatal("plan reported for a runtime that emitted none")
	}
	if !strings.Contains(summarize.Brief(summarize.MomentCompleted, none), "not reported") {
		t.Fatal("the brief does not tell the model the plan is absent")
	}

	withPlan := summarize.DigestOf([]event.Event{
		event.TurnStarted{Meta: meta(0)},
		event.PlanUpdated{Meta: meta(1), Steps: []event.PlanStep{
			{Text: "read the schema", Status: event.PlanCompleted},
			{Text: "wire the vec0 insert", Status: event.PlanInProgress},
			{Text: "write the test", Status: event.PlanPending},
		}},
	})
	if !withPlan.PlanObserved {
		t.Fatal("plan not observed")
	}
	done, total := withPlan.PlanProgress()
	if done != 1 || total != 3 {
		t.Fatalf("progress %d/%d", done, total)
	}
	step, ok := withPlan.CurrentStep()
	if !ok || step.Text != "wire the vec0 insert" {
		t.Fatalf("current step %+v %v", step, ok)
	}
}

// ACP sends tool_call_update with only a toolCallId and every other field null.
// Such an update must not invent a tool call with a made-up name.
func TestDigestMergesBareToolUpdates(t *testing.T) {
	d := summarize.DigestOf([]event.Event{
		event.ToolStarted{Meta: meta(0), ID: "tc_1", Tool: "Edit", Target: "index.go"},
		event.ToolOutput{Meta: meta(1), ID: "tc_1"},
		event.ToolOutput{Meta: meta(2), ID: "tc_1", Status: event.ToolFailed},
	})
	if len(d.Tools) != 1 {
		t.Fatalf("bare updates created %d tools", len(d.Tools))
	}
	if !d.Tools[0].Failed() {
		t.Fatal("failure status lost")
	}
	if d.Tools[0].Tool != "Edit" {
		t.Fatalf("tool name changed to %q", d.Tools[0].Tool)
	}
}

func TestDigestIgnoresAnotherTurnsEvents(t *testing.T) {
	other := meta(9)
	other.Turn = "t2"
	d := summarize.DigestOf([]event.Event{
		event.ToolStarted{Meta: meta(0), ID: "a", Tool: "Bash"},
		event.ToolStarted{Meta: other, ID: "b", Tool: "Write"},
	})
	if len(d.Tools) != 1 || d.Tools[0].Tool != "Bash" {
		t.Fatalf("mixed two turns: %+v", d.Tools)
	}
}

func TestDigestQuestion(t *testing.T) {
	q := event.NewNeedsInput(meta(1), event.InputSpec{
		Ask:    event.InputPermission,
		Prompt: "Run `rm -rf build/`?",
		Options: []event.Option{
			{ID: "a", Name: "Allow once", Kind: event.OptionAllowOnce},
			{ID: "b", Name: "Always allow", Kind: event.OptionAllowAlways},
		},
		Tool: &event.ToolRef{Name: "Bash", Title: "Remove build directory"},
	}, func(context.Context, event.Reply) error { return nil })

	d := summarize.DigestOf([]event.Event{q})
	if d.Question == nil {
		t.Fatal("no question")
	}
	if len(d.Question.Options) != 2 {
		t.Fatalf("options %v", d.Question.Options)
	}
	if d.Question.Standing[0] || !d.Question.Standing[1] {
		t.Fatalf("standing flags wrong: %v", d.Question.Standing)
	}
	if d.Question.Tool != "Remove build directory" {
		t.Fatalf("tool %q", d.Question.Tool)
	}
}

func TestDigestClipsALongErrorToOneLine(t *testing.T) {
	trace := "panic: nil map\n\ngoroutine 1 [running]:\nmain.main()\n\t/src/main.go:12 +0x1d\n"
	d := summarize.DigestOf([]event.Event{
		event.Error{Meta: meta(0), Code: "E_PANIC", Message: trace},
		event.TurnCompleted{Meta: meta(1), OK: false, StopReason: event.StopError},
	})
	if len(d.Errors) != 1 {
		t.Fatalf("errors %v", d.Errors)
	}
	if strings.Contains(d.Errors[0], "goroutine") {
		t.Fatalf("stack trace kept: %q", d.Errors[0])
	}
	if d.Errors[0] != "panic: nil map" {
		t.Fatalf("error %q", d.Errors[0])
	}
}

func TestDigestMarksReplay(t *testing.T) {
	m := meta(0)
	m.Replay = true
	d := summarize.DigestOf([]event.Event{
		event.TurnCompleted{Meta: m, OK: true, StopReason: event.StopEndTurn},
	})
	if !d.Replay {
		t.Fatal("replay not marked")
	}
}

func TestDigesterReset(t *testing.T) {
	g := summarize.NewDigester()
	for _, ev := range passingTurn() {
		g.Add(ev)
	}
	if g.Digest().Events == 0 {
		t.Fatal("nothing folded")
	}
	g.Reset()
	if d := g.Digest(); d.Events != 0 || len(d.Tools) != 0 || d.Text != "" {
		t.Fatalf("reset left %+v", d)
	}
}
