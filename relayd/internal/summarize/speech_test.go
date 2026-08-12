package summarize_test

import (
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/summarize"
)

// The caps are ADAPTERS.md §6's table. They are asserted here rather than only
// used, because they are a product decision — seconds in someone's ear — and a
// silent change to one of them is a silent change to how the device feels.
func TestCapsMatchTheDocument(t *testing.T) {
	cases := []struct {
		m    summarize.Moment
		want int
	}{
		{summarize.MomentAck, 40},
		{summarize.MomentProgress, 90},
		{summarize.MomentCompleted, 160},
		{summarize.MomentNeedsInput, 120},
	}
	for _, c := range cases {
		if got := c.m.Cap(); got != c.want {
			t.Fatalf("%s cap is %d, ADAPTERS.md §6 says %d", c.m, got, c.want)
		}
	}
	if summarize.CharsPerSecond != 14 {
		t.Fatalf("speech rate is %d, ADAPTERS.md §6 says 14", summarize.CharsPerSecond)
	}
	// A completed turn is about eleven seconds. If that ever reads as a long
	// time, the cap is what moves, not the rate.
	if got := summarize.MomentCompleted.Budget(); got < 11*time.Second || got > 12*time.Second {
		t.Fatalf("completed budget is %v", got)
	}
}

// Enforce the cap in code, not only in the prompt. This is the test the task
// asks for: a model that ignores the instruction still cannot produce a
// thirty-second utterance.
func TestFitEnforcesTheCap(t *testing.T) {
	long := strings.Repeat("The tests are running and everything is fine so far. ", 20)
	for _, m := range []summarize.Moment{
		summarize.MomentAck, summarize.MomentProgress,
		summarize.MomentCompleted, summarize.MomentNeedsInput,
	} {
		got, cut := summarize.Fit(long, m.Cap())
		if n := len([]rune(got)); n > m.Cap() {
			t.Fatalf("%s: %d chars, cap %d: %q", m, n, m.Cap(), got)
		}
		if !cut {
			t.Fatalf("%s: 1040 chars fitted without truncation", m)
		}
		if got == "" {
			t.Fatalf("%s: clipped to nothing", m)
		}
	}
}

func TestFitPrefersSentenceThenWordBoundaries(t *testing.T) {
	got, _ := summarize.Fit("Tests pass. Two files changed. And a third thing that will not fit at all here.", 40)
	if got != "Tests pass. Two files changed." {
		t.Fatalf("did not cut at a sentence: %q", got)
	}

	// No sentence boundary in range: cut on a word, never mid-word. A clipped
	// word is heard as a stutter and makes the line sound broken, not short.
	got, _ = summarize.Fit("Rebuilding the authentication middleware configuration", 30)
	if strings.HasSuffix(got, "middlew") || strings.HasSuffix(got, "configurat") {
		t.Fatalf("cut mid-word: %q", got)
	}
	if len([]rune(got)) > 30 {
		t.Fatalf("over cap: %q", got)
	}
	for _, w := range strings.Fields(got) {
		if !strings.Contains("Rebuilding the authentication middleware configuration", w) {
			t.Fatalf("invented or broke a word: %q in %q", w, got)
		}
	}
}

func TestFitCountsRunesNotBytes(t *testing.T) {
	// The budget is characters per second of speech. Counting bytes would clip
	// an accented or non-Latin line to roughly half its allowance.
	in := strings.Repeat("é", 100)
	got, _ := summarize.Fit(in, 40)
	if n := len([]rune(got)); n > 40 {
		t.Fatalf("%d runes", n)
	}
	if n := len([]rune(got)); n < 20 {
		t.Fatalf("counted bytes, not runes: only %d runes survived", n)
	}
}

func TestCleanRemovesWhatAVoiceWouldReadAloud(t *testing.T) {
	got := summarize.Clean("- **Done**.\n  Ran `go test ./...`\n\n  Two files changed.")
	if strings.ContainsAny(got, "*`\n") {
		t.Fatalf("markup survived: %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("double space survived: %q", got)
	}
}

func TestTrimOpener(t *testing.T) {
	for in, want := range map[string]string{
		"Okay, tests pass.":         "Tests pass.",
		"Sure — two files changed.": "Two files changed.",
		"Right. Build is green.":    "Build is green.",
		"Tests pass.":               "Tests pass.",
		"So, alright, tests pass.":  "Tests pass.",
		"Okay":                      "Okay", // nothing underneath; leave it
	} {
		if got := summarize.TrimOpener(in); got != want {
			t.Fatalf("%q → %q, want %q", in, got, want)
		}
	}
}

// "Lead with the outcome" is the third rule, and it is enforced by rejection
// rather than by rewriting: editing model prose to remove a preamble means
// parsing prose, which SYSTEM.md says is the wrong path.
func TestHasPreamble(t *testing.T) {
	bad := []string{
		"I've finished working on the payments branch and I can report that the tests pass.",
		"I have completed the refactor.",
		"I'm happy to report that everything builds.",
		"Here's a summary of what I did.",
		"Let me walk you through the changes.",
		"Just to let you know, the migration ran.",
		"As requested, the tests were updated.",
		"It looks like the build succeeded.",
		"In summary: two files changed.",
		"The good news is the tests pass.",
	}
	for _, s := range bad {
		if !summarize.HasPreamble(s) {
			t.Fatalf("preamble not caught: %q", s)
		}
	}
	good := []string{
		"Tests pass. Two files changed.",
		"Build broke on the auth module. Want the error?",
		"Migration ran, 12 rows moved.",
		"Stopped.",
		"Okay, tests pass.", // a bare opener is trimmed, not preamble
		"Two files changed and the tests pass.",
	}
	for _, s := range good {
		if summarize.HasPreamble(s) {
			t.Fatalf("false positive on %q", s)
		}
	}
}

// If the turn failed, say what failed and stop — offer the error rather than
// reading it. When the whole line will not fit, the offer is the half that
// survives, because it is the half the listener can act on.
func TestFitWithOfferKeepsTheOffer(t *testing.T) {
	got, _ := summarize.FitWithOffer(
		"The build failed while compiling the authentication middleware in the payments service after the dependency upgrade",
		summarize.OfferError, summarize.CapCompleted)
	if !strings.HasSuffix(got, summarize.OfferError) {
		t.Fatalf("offer lost: %q", got)
	}
	if n := len([]rune(got)); n > summarize.CapCompleted {
		t.Fatalf("%d chars: %q", n, got)
	}

	// Even with a cap too small for both, the offer wins.
	got, _ = summarize.FitWithOffer("Something went wrong somewhere in the build", summarize.OfferError, 20)
	if !strings.Contains(got, "error") {
		t.Fatalf("offer dropped under a tight cap: %q", got)
	}

	// The example from ADAPTERS.md §6 must come out intact.
	got, _ = summarize.FitWithOffer("Build broke on the auth module", summarize.OfferError, summarize.CapCompleted)
	if got != "Build broke on the auth module. Want the error?" {
		t.Fatalf("got %q", got)
	}
}

func TestSpeechDuration(t *testing.T) {
	s := summarize.Speech{
		Moment: summarize.MomentCompleted,
		Cap:    summarize.CapCompleted,
		Text:   strings.Repeat("x", 140),
	}
	if !s.WithinCap() {
		t.Fatal("140 chars should fit a 160 cap")
	}
	if got := s.Duration(); got < 9*time.Second || got > 11*time.Second {
		t.Fatalf("duration %v for 140 characters", got)
	}
	// Options are spoken too and count towards the time, even though they do
	// not count against the question's cap.
	s.Options = []string{"allow once", "reject"}
	if s.Duration() <= 10*time.Second {
		t.Fatalf("options did not lengthen the utterance: %v", s.Duration())
	}
}
