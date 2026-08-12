package orchestrator_test

import (
	"context"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/orchestrator"
)

func okResult(toolCalls int) llm.Result {
	return llm.Result{Stop: event.StopEndTurn, ToolCalls: toolCalls}
}

func proposal(worth bool) string {
	if !worth {
		return textReply(`{\"worth_writing\":false}`)
	}
	return textReply(`{\"worth_writing\":true,\"name\":\"check_staging_health\",` +
		`\"title\":\"Check staging health\",` +
		`\"when\":\"when the user asks whether staging is up\",` +
		`\"steps\":\"Open the staging dashboard. Read the error rate.\",` +
		`\"needs\":[\"browser\"]}`)
}

// TestOrdinaryWorkWritesNoPlaybook is the bar, and it is the important half.
//
// A skill for something trivial is worse than no skill: it costs context on
// every future turn and will never be the best answer to anything. So a run
// that did one or two things is not a candidate, and no model call is even made.
func TestOrdinaryWorkWritesNoPlaybook(t *testing.T) {
	tr := &countingTransport{}
	book := orchestrator.NewSkillBook()
	r := orchestrator.NewReflection(orchestrator.ReflectOptions{
		Model: testProvider(t, tr), Skills: book,
	})

	for _, calls := range []int{0, 1, 2} {
		trigger, skill, err := r.Consider(context.Background(),
			orchestrator.Utterance{Text: "what is running"}, okResult(calls), nil)
		if err != nil {
			t.Fatal(err)
		}
		if trigger != orchestrator.TriggerNone || skill != nil {
			t.Errorf("%d tool calls triggered %q", calls, trigger)
		}
	}
	if tr.calls.Load() != 0 {
		t.Errorf("the model was asked %d times about ordinary work", tr.calls.Load())
	}
}

// TestAComplexSuccessIsACandidate — Hermes creates a skill autonomously after a
// complex task, and this is that trigger.
func TestAComplexSuccessIsACandidate(t *testing.T) {
	tr := &countingTransport{script: []string{proposal(true)}}
	book := orchestrator.NewSkillBook()
	r := orchestrator.NewReflection(orchestrator.ReflectOptions{
		Model: testProvider(t, tr), Skills: book,
	})

	trigger, skill, err := r.Consider(context.Background(),
		orchestrator.Utterance{Text: "is staging healthy"}, okResult(4), nil)
	if err != nil {
		t.Fatal(err)
	}
	if trigger != orchestrator.TriggerComplexSuccess {
		t.Fatalf("trigger = %q", trigger)
	}
	if skill == nil || skill.Name != "check_staging_health" {
		t.Fatalf("skill = %+v", skill)
	}
	// And it landed, so the next turn of the same shape is one step.
	list, _ := book.List(context.Background())
	if len(list) != 1 {
		t.Fatalf("the playbook was proposed and not written: %+v", list)
	}
	if list[0].Origin != string(orchestrator.TriggerComplexSuccess) {
		t.Errorf("origin = %q; a skill Relay wrote for itself and one a human wrote "+
			"deserve different scrutiny in review", list[0].Origin)
	}
}

// TestTheModelCanDeclineAndUsuallyShould. worth_writing:false has to stay cheap
// to say, or reflection fills the bus with playbooks for nothing.
func TestTheModelCanDecline(t *testing.T) {
	tr := &countingTransport{script: []string{proposal(false)}}
	book := orchestrator.NewSkillBook()
	r := orchestrator.NewReflection(orchestrator.ReflectOptions{
		Model: testProvider(t, tr), Skills: book,
	})

	trigger, skill, err := r.Consider(context.Background(),
		orchestrator.Utterance{Text: "do a thing"}, okResult(5), nil)
	if err != nil {
		t.Fatal(err)
	}
	if trigger != orchestrator.TriggerComplexSuccess {
		t.Errorf("trigger = %q", trigger)
	}
	if skill != nil {
		t.Errorf("wrote a playbook the model declined to write: %+v", skill)
	}
	if list, _ := book.List(context.Background()); len(list) != 0 {
		t.Errorf("book = %+v", list)
	}
}

// TestAFailedPlaybookIsImprovedOnOneFailure is the asymmetry.
//
// Success needs a high bar because a new skill costs context forever. A skill
// that was followed and still went wrong needs no bar at all: the playbook
// already exists, improving it costs nothing new, and one failure is the
// strongest signal there is that it is wrong. This is what Hermes means by
// "skills self-improve during use".
func TestAFailedPlaybookIsImprovedOnOneFailure(t *testing.T) {
	tr := &countingTransport{script: []string{proposal(true)}}
	book := orchestrator.NewSkillBook()
	r := orchestrator.NewReflection(orchestrator.ReflectOptions{
		Model: testProvider(t, tr), Skills: book,
	})

	// One tool call — far under the success bar — and the turn failed.
	failed := llm.Result{Stop: event.StopError, ToolCalls: 1}
	trigger, skill, err := r.Consider(context.Background(),
		orchestrator.Utterance{Text: "is staging healthy"}, failed,
		[]string{"check_staging_health"})
	if err != nil {
		t.Fatal(err)
	}
	if trigger != orchestrator.TriggerSkillFailed {
		t.Fatalf("trigger = %q; a followed playbook on a failed turn is the signal", trigger)
	}
	if skill == nil {
		t.Fatal("nothing was improved")
	}

	// The prompt has to tell the model to keep the name, or the improvement
	// forks the playbook instead of replacing it.
	sent := tr.body(0)
	for _, want := range []string{"keep the name", "check_staging_health"} {
		if !strings.Contains(sent, want) {
			t.Errorf("the improvement prompt is missing %q:\n%s", want, sent)
		}
	}
}

// TestReflectionAsksForAStructuredAnswer. "Did the model decide to write a
// skill" has to be a field, not something inferred from whether it used the
// word "skill".
func TestReflectionAsksForAStructuredAnswer(t *testing.T) {
	tr := &countingTransport{script: []string{proposal(false)}}
	r := orchestrator.NewReflection(orchestrator.ReflectOptions{
		Model: testProvider(t, tr), Skills: orchestrator.NewSkillBook(),
	})
	if _, _, err := r.Consider(context.Background(),
		orchestrator.Utterance{Text: "x"}, okResult(4), nil); err != nil {
		t.Fatal(err)
	}

	sent := tr.body(0)
	if !strings.Contains(sent, `"json_schema"`) || !strings.Contains(sent, "worth_writing") {
		t.Errorf("reflection did not constrain the reply:\n%s", sent)
	}
	// And it holds no tools: reflection decides what to write down, it does not
	// get to start sessions while thinking about the last turn.
	if !strings.Contains(sent, `"tool_choice":{"type":"none"}`) {
		t.Errorf("the reflection turn was not denied tools:\n%s", sent)
	}
}

// TestSkillsUsedReadsEventsNotTheTranscript. A skill is reached through the
// shared bus, so the runtime that used one emitted a ToolStarted naming it —
// that is an observation rather than an inference.
func TestSkillsUsedReadsEvents(t *testing.T) {
	events := []event.Event{
		event.ToolStarted{Tool: "Bash"},
		event.ToolStarted{Tool: orchestrator.SkillConnector + "_check_staging_health"},
		event.ToolStarted{Tool: orchestrator.SkillConnector + "_check_staging_health"},
		event.ToolStarted{Tool: "gmail_send"},
	}
	got := orchestrator.SkillsUsed(events)
	if len(got) != 1 || got[0] != "check_staging_health" {
		t.Fatalf("SkillsUsed = %v", got)
	}
}

// TestNoModelMeansNoLearningRatherThanNoOrchestrator: an install with no key
// still works, it just stops getting better.
func TestNoModelMeansNoLearning(t *testing.T) {
	if r := orchestrator.NewReflection(orchestrator.ReflectOptions{
		Skills: orchestrator.NewSkillBook(),
	}); r != nil {
		t.Error("reflection was built with no model")
	}
	var nilRef *orchestrator.Reflection
	trigger, skill, err := nilRef.Consider(context.Background(),
		orchestrator.Utterance{}, okResult(9), nil)
	if err != nil || skill != nil || trigger != orchestrator.TriggerNone {
		t.Errorf("a nil reflection did something: %q %+v %v", trigger, skill, err)
	}
}
