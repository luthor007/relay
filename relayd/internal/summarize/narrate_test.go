package summarize_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/summarize"
)

func failingTurn() []event.Event {
	return []event.Event{
		event.TurnStarted{Meta: meta(0)},
		event.ToolStarted{Meta: meta(1), ID: "t1", Tool: "Bash", Target: "go build ./auth/..."},
		event.ToolOutput{Meta: meta(2), ID: "t1", Chunk: "auth/token.go:14: undefined: Signer\n", Status: event.ToolFailed},
		event.Error{Meta: meta(3), Code: "E_BUILD", Message: "build failed: auth/token.go:14: undefined: Signer\n\tstack…"},
		event.TurnCompleted{Meta: meta(4), OK: false, StopReason: event.StopError, Duration: 3 * time.Second},
	}
}

// The task's headline requirement, checked end to end: the cap is enforced in
// code, not only asked for in the prompt.
func TestNarrationIsCappedEvenWhenTheModelIgnoresTheInstruction(t *testing.T) {
	ctx := context.Background()
	model := &fakeModel{Reply: strings.Repeat(
		"The tests all pass and the build is green and I checked every package twice. ", 12)}
	n := summarize.NewNarrator(summarize.NarratorOptions{Model: model})

	sp := n.Completed(ctx, summarize.DigestOf(passingTurn()))
	if !sp.WithinCap() {
		t.Fatalf("%d chars over a %d cap: %q", len([]rune(sp.Text)), sp.Cap, sp.Text)
	}
	if sp.Duration() > 12*time.Second {
		t.Fatalf("%v of speech", sp.Duration())
	}
	if sp.Text == "" {
		t.Fatal("clipped to nothing")
	}
}

// Preamble is rejected, and the deterministic template speaks instead. The
// alternative — editing the model's sentence — is prose-parsing.
func TestPreambleIsRejectedInFavourOfTheTemplate(t *testing.T) {
	ctx := context.Background()
	model := &fakeModel{Reply: "I've finished working on the payments branch and I can report that the tests pass."}
	n := summarize.NewNarrator(summarize.NarratorOptions{Model: model})

	sp := n.Completed(ctx, summarize.DigestOf(passingTurn()))
	if sp.Source != summarize.SourceTemplate {
		t.Fatalf("preamble accepted: %q", sp.Text)
	}
	if summarize.HasPreamble(sp.Text) {
		t.Fatalf("the template itself has preamble: %q", sp.Text)
	}
	if model.calls() != 1 {
		t.Fatalf("model called %d times", model.calls())
	}
}

// ORCHESTRATOR.md §3b names narration drift as one of the two ways the
// two-model split breaks, and calls it a plumbing problem. This is the plumbing:
// a specific the events never mentioned is not spoken.
func TestUngroundedSpecificsAreRejected(t *testing.T) {
	ctx := context.Background()
	d := summarize.DigestOf(passingTurn())

	bad := &fakeModel{Reply: "Tests pass. internal/registry/router.go was rewritten."}
	sp := summarize.NewNarrator(summarize.NarratorOptions{Model: bad}).Completed(ctx, d)
	if sp.Source != summarize.SourceModel {
		// Correct outcome, but check it was rejected for the right reason.
		if strings.Contains(sp.Text, "registry") {
			t.Fatalf("invented file survived: %q", sp.Text)
		}
	} else {
		t.Fatalf("ungrounded specific accepted: %q", sp.Text)
	}

	// The same shape, but grounded: "go test ./..." IS in the events.
	good := &fakeModel{Reply: "Tests pass. Bash ran go test ./... cleanly."}
	sp = summarize.NewNarrator(summarize.NarratorOptions{Model: good}).Completed(ctx, d)
	if sp.Source != summarize.SourceModel {
		t.Fatalf("grounded line rejected: %q", sp.Text)
	}
}

func TestGroundedAllowsOrdinaryEnglish(t *testing.T) {
	brief := "tools: Bash on go test ./... — ok\noutcome: succeeded\n"
	// A line naming nothing passes. A vague true update beats a precise
	// invented one.
	if !summarize.Grounded("Everything passed, nothing else to report.", brief) {
		t.Fatal("plain English rejected")
	}
	if summarize.Grounded("Rebuilt internal/api/server.go.", brief) {
		t.Fatal("invented path accepted")
	}
	if !summarize.Grounded("Ran go test ./... and it passed.", brief) {
		t.Fatal("grounded path rejected")
	}
}

func TestPathsAndURLsAreNeverSpoken(t *testing.T) {
	ctx := context.Background()
	d := summarize.DigestOf(passingTurn())
	for _, reply := range []string{
		"Tests pass. See https://ci.example.com/builds/1234 for details.",
		"Tests pass. " + strings.Repeat("a", 45) + " changed.",
	} {
		sp := summarize.NewNarrator(summarize.NarratorOptions{Model: &fakeModel{Reply: reply}}).Completed(ctx, d)
		if sp.Source == summarize.SourceModel {
			t.Fatalf("unspeakable token accepted: %q", sp.Text)
		}
	}
}

// A failed turn names what failed and stops. It never reads the error; it
// offers it.
func TestFailedTurnOffersTheErrorInsteadOfReadingIt(t *testing.T) {
	ctx := context.Background()
	d := summarize.DigestOf(failingTurn())

	sp := summarize.NewNarrator(summarize.NarratorOptions{}).Completed(ctx, d)
	if !strings.HasSuffix(sp.Text, summarize.OfferError) {
		t.Fatalf("no offer: %q", sp.Text)
	}
	if strings.Contains(sp.Text, "stack") || strings.Contains(sp.Text, "token.go:14") {
		t.Fatalf("read the error aloud: %q", sp.Text)
	}
	if !strings.Contains(strings.ToLower(sp.Text), "bash") {
		t.Fatalf("did not say what failed: %q", sp.Text)
	}
	if !sp.WithinCap() {
		t.Fatalf("over cap: %q", sp.Text)
	}
	if sp.Offer != summarize.OfferError {
		t.Fatalf("offer not reported: %q", sp.Offer)
	}

	// With a model, the offer still survives the model's phrasing.
	model := &fakeModel{Reply: "Build broke on the auth module"}
	sp = summarize.NewNarrator(summarize.NarratorOptions{Model: model}).Completed(ctx, d)
	if !strings.HasSuffix(sp.Text, summarize.OfferError) {
		t.Fatalf("model phrasing dropped the offer: %q", sp.Text)
	}
}

// Given no event, say "still working" — never invent a specific.
func TestNoEventsMeansNoSpecifics(t *testing.T) {
	ctx := context.Background()
	empty := summarize.Digest{Runtime: "codex", Session: "s1", Turn: "t1"}

	// A model that would happily make something up is never even asked, because
	// there is nothing to ask about.
	model := &fakeModel{Reply: "Refactored the payment handler and updated six tests."}
	n := summarize.NewNarrator(summarize.NarratorOptions{Model: model})

	sp := n.Progress(ctx, empty)
	if sp.Grounded {
		t.Fatal("claimed to be grounded with no events")
	}
	if sp.Text != "Still working." {
		t.Fatalf("invented a specific: %q", sp.Text)
	}
	if strings.Contains(sp.Text, "payment") {
		t.Fatalf("model output used: %q", sp.Text)
	}
}

func TestProgressUsesThePlanWhenThereIsOne(t *testing.T) {
	ctx := context.Background()
	d := summarize.DigestOf([]event.Event{
		event.PlanUpdated{Meta: meta(0), Steps: []event.PlanStep{
			{Text: "read the schema", Status: event.PlanCompleted},
			{Text: "wire the vec0 insert", Status: event.PlanInProgress},
		}},
	})
	sp := summarize.NewNarrator(summarize.NarratorOptions{}).Progress(ctx, d)
	if !strings.Contains(sp.Text, "vec0") {
		t.Fatalf("plan ignored: %q", sp.Text)
	}
	if len([]rune(sp.Text)) > summarize.CapProgress {
		t.Fatalf("over the progress cap: %q", sp.Text)
	}
}

func TestAckIsShortAndImmediate(t *testing.T) {
	sp := summarize.NewNarrator(summarize.NarratorOptions{}).
		Ack(context.Background(), summarize.AckHint{Subject: "payments branch"})
	if len([]rune(sp.Text)) > summarize.CapAck {
		t.Fatalf("%d chars: %q", len([]rune(sp.Text)), sp.Text)
	}
	if sp.Duration() > 3*time.Second {
		t.Fatalf("%v to say", sp.Duration())
	}
	if !strings.Contains(sp.Text, "payments branch") {
		t.Fatalf("subject lost: %q", sp.Text)
	}
}

func TestNeedsInputSpeaksTheQuestionAndTheOptions(t *testing.T) {
	ctx := context.Background()
	q := event.NewNeedsInput(meta(0), event.InputSpec{
		Ask:    event.InputPermission,
		Prompt: "Codex wants to run a command that deletes the build directory. Allow it?",
		Options: []event.Option{
			{ID: "once", Name: "Allow once", Kind: event.OptionAllowOnce},
			{ID: "always", Name: "Always allow commands like this one in this workspace", Kind: event.OptionAllowAlways},
			{ID: "no", Name: "Reject", Kind: event.OptionRejectOnce},
		},
	}, func(context.Context, event.Reply) error { return nil })

	sp := summarize.NewNarrator(summarize.NarratorOptions{}).
		NeedsInput(ctx, summarize.DigestOf([]event.Event{q}))

	if len([]rune(sp.Text)) > summarize.CapNeedsInput {
		t.Fatalf("question over cap: %q", sp.Text)
	}
	if len(sp.Options) != 3 {
		t.Fatalf("options %v", sp.Options)
	}
	for _, o := range sp.Options {
		if len([]rune(o)) > summarize.CapOption {
			t.Fatalf("option too long to say: %q", o)
		}
	}
	// The standing option is flagged, not filtered: we speak the names the
	// agent gave us, and a human picks.
	if sp.Standing[0] || !sp.Standing[1] || sp.Standing[2] {
		t.Fatalf("standing flags: %v", sp.Standing)
	}
}

func TestNarrationFallsBackWhenTheModelFails(t *testing.T) {
	ctx := context.Background()
	model := &fakeModel{Err: errors.New("provider is down")}
	sp := summarize.NewNarrator(summarize.NarratorOptions{Model: model}).
		Completed(ctx, summarize.DigestOf(passingTurn()))
	if sp.Source != summarize.SourceTemplate {
		t.Fatalf("source %q", sp.Source)
	}
	if sp.Text == "" {
		t.Fatal("silence after a failed narration")
	}
	if !sp.WithinCap() {
		t.Fatalf("over cap: %q", sp.Text)
	}
}

// The template is the honest floor: every word of it comes from an event, and
// it has to be good enough to ship without a model at all.
func TestTemplateAlone(t *testing.T) {
	pass := summarize.Template(summarize.MomentCompleted, summarize.DigestOf(passingTurn()))
	if summarize.HasPreamble(pass.Text) {
		t.Fatalf("template preamble: %q", pass.Text)
	}
	if !strings.HasPrefix(pass.Text, "Tests pass.") {
		t.Fatalf("template did not lead with the outcome: %q", pass.Text)
	}
	if !pass.WithinCap() {
		t.Fatalf("over cap: %q", pass.Text)
	}

	stopped := summarize.Template(summarize.MomentCompleted, summarize.DigestOf([]event.Event{
		event.TurnCompleted{Meta: meta(0), OK: false, StopReason: event.StopCancelled},
	}))
	if stopped.Text != "Stopped." {
		t.Fatalf("cancelled turn said %q", stopped.Text)
	}
	// Nothing to offer means no offer is promised.
	if stopped.Offer != "" {
		t.Fatalf("offered an error that does not exist: %q", stopped.Offer)
	}
}

func TestSpeechPromptStatesTheRules(t *testing.T) {
	p := summarize.SpeechPrompt(summarize.MomentCompleted)
	for _, want := range []string{"160 characters", "Lead with the outcome", "No preamble", OfferPhrase} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt is missing %q:\n%s", want, p)
		}
	}
}

// OfferPhrase is spelled out so the prompt test fails if the constant and the
// instruction drift apart.
const OfferPhrase = "Want the error?"

// A credential in a tool target must not reach the model, and must not reach
// the speaker either.
//
// This is a regression test with a specific history. The narrator never
// redacted anything: it took the digest [Summarizer.SummarizeTurn] had already
// cleaned a *copy* of, and put the raw tool target into both the prompt it
// posts to the small model and the line it speaks. The gap survived because the
// one test that would have caught it used a fixture long enough to be cut by
// the 60-character clip on the target — the key was truncated, the assertion
// looked for the whole key, and it passed for a reason that had nothing to do
// with redaction.
//
// So this checks both consumers, and it checks the template path with no model
// at all, because that path leaks to the speaker without any provider being
// involved. The key is kept short enough to survive every clip in narrate.go;
// if a future edit shortens one below this, that is the test's business.
func TestACredentialInAToolTargetReachesNeitherTheModelNorTheSpeaker(t *testing.T) {
	const key = "glpat-TESTONLYneverIssued07"
	turn := []event.Event{
		event.TurnStarted{Meta: meta(0)},
		event.ToolStarted{Meta: meta(1), ID: "t1", Tool: "Bash", Target: key},
		event.ToolOutput{Meta: meta(2), ID: "t1", Status: event.ToolCompleted},
		event.TurnCompleted{Meta: meta(3), OK: true, StopReason: event.StopEndTurn, Duration: time.Second},
	}
	d := summarize.DigestOf(turn)

	// The digest itself still holds it: redaction is the narrator's job, and a
	// digest that quietly cleaned itself would hide where the guarantee lives.
	var raw bool
	for _, tool := range d.Tools {
		if strings.Contains(tool.Target, key) {
			raw = true
		}
	}
	if !raw {
		t.Fatal("the fixture never carried the key, so this test proves nothing")
	}

	// The template path, which speaks without ever calling a provider. Asked
	// for through the narrator rather than by calling Template directly,
	// because the narrator is what production holds and the guarantee has to
	// live on the path that actually runs.
	quiet := summarize.NewNarrator(summarize.NarratorOptions{})
	if got := quiet.Completed(context.Background(), d).Text; strings.Contains(got, key) {
		t.Errorf("the spoken template carries the key: %q", got)
	}

	// The model path.
	model := &fakeModel{Reply: "Echoed a value."}
	n := summarize.NewNarrator(summarize.NarratorOptions{Model: model})
	sp := n.Completed(context.Background(), d)
	for _, p := range model.allPrompts() {
		if strings.Contains(p, key) {
			t.Errorf("the key was sent to a model:\n%s", p)
		}
	}
	if strings.Contains(sp.Text, key) {
		t.Errorf("the narrated line carries the key: %q", sp.Text)
	}
}
