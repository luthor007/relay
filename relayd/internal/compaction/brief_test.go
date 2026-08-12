package compaction

import (
	"errors"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/adapter"
)

// A synthetic GitHub token: tier 1 in the measured ruleset, and deliberately
// not one of the four shapes scripts/build-public-repo.sh greps for.
const fakeToken = "ghp_" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func builder(t *testing.T, o BriefOptions) *BriefBuilder {
	t.Helper()
	o.Redactor = Detector()
	b, err := NewBriefBuilder(o)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The same structural rule internal/summarize enforces: there is no code path
// that writes text without having looked at it first.
func TestBriefBuilderRefusesWithoutADetector(t *testing.T) {
	if _, err := NewBriefBuilder(BriefOptions{}); !errors.Is(err, ErrNoRedactor) {
		t.Fatalf("err = %v, want ErrNoRedactor", err)
	}
}

func TestBriefCarriesTheWorkNotJustTheArtefacts(t *testing.T) {
	b := builder(t, BriefOptions{})

	_, err := b.Build(BriefInput{Session: "s1", Files: []string{"a.go", "b.go"}, Facts: []string{"uses Go"}})
	if !errors.Is(err, ErrNothingToCarry) {
		t.Fatalf("err = %v: eight paths and no statement of the work is not a brief", err)
	}

	if _, err := b.Build(BriefInput{Session: "s1"}); !errors.Is(err, ErrNothingToCarry) {
		t.Fatalf("err = %v, want ErrNothingToCarry", err)
	}
}

func TestBriefFallsBackToTheNewestTurn(t *testing.T) {
	b := builder(t, BriefOptions{})
	got, err := b.Build(BriefInput{
		Session: "s1",
		Recent:  []string{"looked at the schema", "wrote the migration"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Work != "wrote the migration" {
		t.Fatalf("work = %q, want the newest turn summary", got.Work)
	}
}

// A brief is sent into a fresh agent session. That is an outbound path, and the
// detector runs before the text exists rather than after.
func TestBriefRedactsBeforeItIsText(t *testing.T) {
	b := builder(t, BriefOptions{})
	got, err := b.Build(BriefInput{
		Session:   "s1",
		Summary:   "wiring the deploy",
		Decisions: []string{"the CI token is " + fakeToken},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Redactions == 0 {
		t.Fatal("the detector found nothing in text that plainly contains a token")
	}
	text := got.Text()
	if strings.Contains(text, fakeToken) {
		t.Fatal("a credential reached the brief text")
	}
	if !strings.Contains(text, "[relay:redacted") {
		t.Fatalf("the marker should replace it:\n%s", text)
	}
}

func TestBriefOmitsSectionsItHasNoMaterialFor(t *testing.T) {
	b := builder(t, BriefOptions{})
	got, err := b.Build(BriefInput{Session: "s1", Summary: "porting the parser"})
	if err != nil {
		t.Fatal(err)
	}
	text := got.Text()
	for _, header := range []string{"Decided so far", "Files in play", "Standing facts", "Next:"} {
		if strings.Contains(text, header) {
			t.Fatalf("%q appears with nothing behind it:\n%s", header, text)
		}
	}
	if !strings.Contains(text, "porting the parser") {
		t.Fatalf("the work statement is missing:\n%s", text)
	}
	if !strings.Contains(text, "s1") {
		t.Fatal("the brief should say which session it came from")
	}
}

// The claim in MEMORY.md §9 is that the brief is smaller and better targeted
// than the runtime's own compaction. The budget is that claim, enforced.
func TestBriefFitsItsBudgetAndDropsInPriorityOrder(t *testing.T) {
	long := strings.Repeat("x", 150)
	in := BriefInput{
		Session: "s1",
		Summary: "rewriting the payments retry loop",
		Next:    "make the idempotency key stable across retries",
	}
	for i := 0; i < 20; i++ {
		in.Decisions = append(in.Decisions, "decision "+itoa(i)+" "+long)
		in.Files = append(in.Files, "pkg/"+itoa(i)+"/"+long+".go")
		in.Facts = append(in.Facts, "fact "+itoa(i)+" "+long)
	}

	// Across every budget the priority holds: facts go before files, files
	// before decisions, and the work statement and next step never go at all.
	for _, budget := range []int{600, 900, 1400, 2200, 3000} {
		b := builder(t, BriefOptions{Budget: budget})
		got, err := b.Build(in)
		if err != nil {
			t.Fatalf("budget %d: %v", budget, err)
		}
		if got.Chars() > budget && !got.Truncated {
			t.Fatalf("budget %d: brief is %d chars and does not admit it", budget, got.Chars())
		}
		if got.Work == "" || got.Next == "" {
			t.Fatalf("budget %d: the work and the next step are never what gets dropped", budget)
		}
		if len(got.Facts) > 0 && len(got.Files) != 8 {
			t.Fatalf("budget %d: facts survived (%d) while files were dropped (%d)", budget, len(got.Facts), len(got.Files))
		}
		if len(got.Files) > 0 && len(got.Decisions) != 6 {
			t.Fatalf("budget %d: files survived (%d) while decisions were dropped (%d)", budget, len(got.Files), len(got.Decisions))
		}
		if got.Truncated {
			if got.Dropped == 0 {
				t.Fatalf("budget %d: truncated but nothing recorded as dropped", budget)
			}
			if !strings.Contains(got.Text(), "Shortened to fit") {
				t.Fatalf("budget %d: a shortened brief should say so", budget)
			}
		}
	}

	// And a budget so small that nothing droppable is left still keeps the one
	// sentence that says what the work is.
	b := builder(t, BriefOptions{Budget: 10})
	got, err := b.Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Text(), "rewriting the payments retry loop") {
		t.Fatalf("the work statement was mutilated to fit:\n%s", got.Text())
	}
	if !got.Truncated {
		t.Fatal("a brief over its budget must say so rather than pretend")
	}
}

func TestBriefCapsEachList(t *testing.T) {
	var many []string
	for i := 0; i < 50; i++ {
		many = append(many, "item "+itoa(i))
	}
	b := builder(t, BriefOptions{MaxDecisions: 3, MaxFiles: 2, MaxFacts: 1, Budget: 100_000})
	got, err := b.Build(BriefInput{Session: "s", Summary: "a thing", Decisions: many, Files: many, Facts: many})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Decisions) != 3 || len(got.Files) != 2 || len(got.Facts) != 1 {
		t.Fatalf("caps ignored: %d/%d/%d", len(got.Decisions), len(got.Files), len(got.Facts))
	}
}

func TestBriefClipsARunawayLine(t *testing.T) {
	b := builder(t, BriefOptions{MaxLine: 40, Budget: 100_000})
	got, err := b.Build(BriefInput{Session: "s", Summary: strings.Repeat("y", 500)})
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(got.Work)) > 40 {
		t.Fatalf("work is %d runes, want <= 40", len([]rune(got.Work)))
	}
	if !strings.HasSuffix(got.Work, "…") {
		t.Fatalf("a clipped line should say it was clipped: %q", got.Work)
	}
}

func TestBriefDeduplicatesAndCollapsesWhitespace(t *testing.T) {
	b := builder(t, BriefOptions{Budget: 100_000})
	got, err := b.Build(BriefInput{
		Session:   "s",
		Summary:   "a\n\nthing   with\tspace",
		Decisions: []string{"same", "same", "other"},
		Files:     []string{"a.go", "a.go", "b/c/d.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Work != "a thing with space" {
		t.Fatalf("work = %q", got.Work)
	}
	if len(got.Decisions) != 2 {
		t.Fatalf("decisions = %v", got.Decisions)
	}
	if len(got.Files) != 2 {
		t.Fatalf("files = %v", got.Files)
	}
	if got.Files[0] != "a.go" {
		t.Fatalf("shortest path first: %v", got.Files)
	}
}

func TestBriefTurn(t *testing.T) {
	b := builder(t, BriefOptions{})
	got, err := b.Build(BriefInput{Session: "s1", Runtime: adapter.Codex, Workspace: "/repos/api", Summary: "the retry loop"})
	if err != nil {
		t.Fatal(err)
	}
	turn := got.Turn()
	if turn.Text != got.Text() {
		t.Fatal("the turn is the brief")
	}
	if !strings.Contains(turn.Text, "/repos/api") || !strings.Contains(turn.Text, string(adapter.Codex)) {
		t.Fatalf("the brief should place the work:\n%s", turn.Text)
	}
	if got.Empty() {
		t.Fatal("a brief with a work statement is not empty")
	}
	if (Brief{}).Empty() != true {
		t.Fatal("a zero brief is empty")
	}
}
