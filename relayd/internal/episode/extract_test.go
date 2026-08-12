package episode

import (
	"strings"
	"testing"
	"time"
)

func episodeOf(us ...Utterance) Episode {
	eps := Segment(us, Options{})
	if len(eps) != 1 {
		panic("test built more than one episode")
	}
	return eps[0]
}

// ARCHITECTURE.md §4: "You told Marc you'd send the BOM by Friday" is worth more
// than a searchable transcript.
func TestTheCanonicalCommitment(t *testing.T) {
	e := episodeOf(
		said("2026-08-10T09:00:00Z", "me", "I'll send Marc the BOM by Friday."),
		said("2026-08-10T09:00:20Z", "marc", "Great, that works for the quote."),
	)
	ex := Extract(e, Options{})
	if len(ex.Commitments) != 1 {
		t.Fatalf("Commitments = %+v, want one", ex.Commitments)
	}
	c := ex.Commitments[0]
	if c.Speaker != "me" {
		t.Fatalf("Speaker = %q", c.Speaker)
	}
	if c.OwedTo != "marc" {
		t.Fatalf("OwedTo = %q, want marc", c.OwedTo)
	}
	// Said on Monday 10 August 2026; the Friday coming is the 14th.
	if want := at("2026-08-14T17:00:00Z"); !c.DueAt.Equal(want) {
		t.Fatalf("DueAt = %s, want %s", c.DueAt, want)
	}
	if c.Evidence == "" || c.Cue == "" {
		t.Fatalf("a commitment must be traceable to a sentence and a rule: %+v", c)
	}

	line := RenderCommitment(c, "me")
	if !strings.HasPrefix(line, "You told marc:") {
		t.Fatalf("rendered = %q", line)
	}
	if !strings.Contains(line, "due Fri 14 Aug") {
		t.Fatalf("rendered = %q, want the date", line)
	}
}

func TestACommitmentMadeToTheWearer(t *testing.T) {
	e := episodeOf(
		said("2026-08-10T09:00:00Z", "me", "Do we have the enclosure drawings yet?"),
		said("2026-08-10T09:00:20Z", "marc", "I'll send you the drawings tomorrow."),
	)
	ex := Extract(e, Options{})
	if len(ex.Commitments) != 1 {
		t.Fatalf("Commitments = %+v", ex.Commitments)
	}
	c := ex.Commitments[0]
	if c.Speaker != "marc" || c.OwedTo != "me" {
		t.Fatalf("who owes whom is wrong: speaker %q owes %q", c.Speaker, c.OwedTo)
	}
	if line := RenderCommitment(c, "me"); !strings.HasPrefix(line, "marc told you:") {
		t.Fatalf("rendered = %q — 'you owe Marc' and 'Marc owes you' are opposite facts", line)
	}
}

func TestNoDateMeansNoDate(t *testing.T) {
	e := episodeOf(said("2026-08-10T09:00:00Z", "me", "I'll look at the regulator footprint."))
	ex := Extract(e, Options{})
	if len(ex.Commitments) != 1 {
		t.Fatalf("Commitments = %+v", ex.Commitments)
	}
	if !ex.Commitments[0].DueAt.IsZero() {
		t.Fatalf("DueAt = %s, want zero — nobody said a date", ex.Commitments[0].DueAt)
	}
	if line := RenderCommitment(ex.Commitments[0], "me"); strings.Contains(line, "due") {
		t.Fatalf("rendered = %q, want no invented deadline", line)
	}
}

func TestReportedSpeechIsNotAPromise(t *testing.T) {
	e := episodeOf(said("2026-08-10T09:00:00Z", "marc",
		"He said I'll never get the firmware from them, which is probably true."))
	ex := Extract(e, Options{})
	if len(ex.Commitments) != 0 {
		t.Fatalf("Commitments = %+v, want none — that is a report, not a promise", ex.Commitments)
	}
}

func TestDecisionsAreExtractedAndNotDoubleCounted(t *testing.T) {
	e := episodeOf(
		said("2026-08-10T09:00:00Z", "me", "We decided I'll take the Stripe migration."),
		said("2026-08-10T09:00:20Z", "marc", "Let's go with the WCH part for the next run."),
	)
	ex := Extract(e, Options{})
	if len(ex.Decisions) != 2 {
		t.Fatalf("Decisions = %+v, want two", ex.Decisions)
	}
	for _, c := range ex.Commitments {
		if strings.Contains(c.Text, "Stripe migration") {
			t.Fatal("a decision containing a promise was counted twice")
		}
	}
}

func TestFrenchCommitmentsAreExtracted(t *testing.T) {
	// Quebec is the home market. A francophone user whose commitments are
	// silently unextracted gets a memory that works for half their day.
	e := episodeOf(said("2026-08-10T09:00:00Z", "me", "Je vais envoyer la facture demain."))
	ex := Extract(e, Options{})
	if len(ex.Commitments) != 1 {
		t.Fatalf("Commitments = %+v", ex.Commitments)
	}
	if want := at("2026-08-11T17:00:00Z"); !ex.Commitments[0].DueAt.Equal(want) {
		t.Fatalf("DueAt = %s, want %s", ex.Commitments[0].DueAt, want)
	}
}

func TestParseDue(t *testing.T) {
	// Monday, 10 August 2026.
	monday := at("2026-08-10T09:00:00Z")
	cases := []struct {
		sentence string
		want     string
	}{
		{"I'll do it tomorrow.", "2026-08-11T17:00:00Z"},
		{"I'll do it today.", "2026-08-10T17:00:00Z"},
		{"I'll have it by end of day.", "2026-08-10T17:00:00Z"},
		{"I'll send it Friday.", "2026-08-14T17:00:00Z"},
		{"I'll send it Monday.", "2026-08-17T17:00:00Z"}, // the Monday coming, not today
		{"I'll get it done by the end of the week.", "2026-08-14T17:00:00Z"},
		{"I'll do it next week.", "2026-08-17T17:00:00Z"},
		{"I'll do it in 3 days.", "2026-08-13T17:00:00Z"},
		{"I'll do it in 2 hours.", "2026-08-10T11:00:00Z"},
		{"Je vais le faire dans 2 jours.", "2026-08-12T17:00:00Z"},
		{"I'll get to it at some point.", ""},
	}
	for _, tc := range cases {
		got := ParseDue(tc.sentence, monday)
		if tc.want == "" {
			if !got.IsZero() {
				t.Fatalf("%q → %s, want no date", tc.sentence, got)
			}
			continue
		}
		if !got.Equal(at(tc.want)) {
			t.Fatalf("%q → %s, want %s", tc.sentence, got, tc.want)
		}
	}
}

func TestNotesKeepSentencesThatNameSomething(t *testing.T) {
	e := episodeOf(
		said("2026-08-10T09:00:00Z", "me", "Yeah. Sure. Okay then."),
		said("2026-08-10T09:00:20Z", "marc", "The CRC-16 variant in the appendix is wrong and the checksum fails."),
		said("2026-08-10T09:00:40Z", "marc", "The regulator drops 200 mV under load which is more than we budgeted."),
	)
	ex := Extract(e, Options{})
	if len(ex.Notes) != 2 {
		t.Fatalf("Notes = %+v, want the two that name something", ex.Notes)
	}
	for _, n := range ex.Notes {
		if strings.HasPrefix(n, "Yeah") {
			t.Fatal("filler reached the notes; a digest that is filler is a digest nobody reads")
		}
	}
}

func TestOwedToIsEmptyRatherThanGuessedInAMeeting(t *testing.T) {
	e := episodeOf(
		said("2026-08-10T09:00:00Z", "me", "I'll write up the summary after this."),
		said("2026-08-10T09:00:20Z", "marc", "Sounds good, the numbers are in the sheet."),
		said("2026-08-10T09:00:40Z", "ana", "Same here, I have the invoice numbers ready."),
	)
	ex := Extract(e, Options{})
	var mine *Commitment
	for i := range ex.Commitments {
		if strings.Contains(ex.Commitments[i].Text, "summary") {
			mine = &ex.Commitments[i]
		}
	}
	if mine == nil {
		t.Fatalf("Commitments = %+v", ex.Commitments)
	}
	if mine.OwedTo != "" {
		t.Fatalf("OwedTo = %q — with three people and no name in the sentence, picking one invents a fact", mine.OwedTo)
	}
}

func TestCommitmentIDIsStable(t *testing.T) {
	c := Commitment{Text: "I'll send the BOM.", At: at("2026-08-10T09:00:00Z")}
	if CommitmentID("ep", c) != CommitmentID("ep", c) {
		t.Fatal("ids differ across calls")
	}
	other := c
	other.At = c.At.Add(time.Second)
	if CommitmentID("ep", c) == CommitmentID("ep", other) {
		t.Fatal("two different moments produced the same id")
	}
}
