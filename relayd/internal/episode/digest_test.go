package episode

import (
	"strings"
	"testing"
	"time"
)

func aDay() []Utterance {
	return []Utterance{
		// Morning, alone at the bench.
		said("2026-08-10T09:00:00Z", "me", "The CRC-16 variant in the appendix is wrong."),
		said("2026-08-10T09:00:30Z", "me", "I'll fix the checksum table today."),

		// Mid-morning, with Marc.
		said("2026-08-10T10:30:00Z", "me", "I'll send Marc the BOM by Friday."),
		said("2026-08-10T10:30:30Z", "marc", "We decided to go with the WCH part for this run."),
		said("2026-08-10T10:31:00Z", "marc", "I'll send you the quote tomorrow."),

		// Afternoon, three people.
		said("2026-08-10T14:00:00Z", "me", "The regulator drops 200 mV under load."),
		said("2026-08-10T14:00:30Z", "ana", "Let's go with the larger inductor then."),
		said("2026-08-10T14:01:00Z", "marc", "Agreed, and I'll update the schematic next week."),
	}
}

// SYSTEM.md §4's outputs table, exactly: {notes[], commitments[], decisions[]}.
func TestTheDailyDigestHasTheThreeLists(t *testing.T) {
	eps := Segment(aDay(), Options{})
	d := Day(at("2026-08-10T00:00:00Z"), eps, Options{}, DigestLimits{})

	if len(d.Commitments) == 0 || len(d.Decisions) == 0 || len(d.Notes) == 0 {
		t.Fatalf("digest = %+v, want all three lists populated", d)
	}
	if d.Coverage.Episodes != len(eps) {
		t.Fatalf("Coverage.Episodes = %d, want %d", d.Coverage.Episodes, len(eps))
	}

	// The soonest deadline is at the top: a digest is read down the page.
	first := d.Commitments[0]
	if !strings.Contains(first, "today") && !strings.Contains(first, "checksum") {
		t.Fatalf("first commitment = %q, want the one due today", first)
	}
	dated := 0
	for _, c := range d.Commitments {
		if strings.Contains(c, "due ") {
			dated++
		}
	}
	if dated < 3 {
		t.Fatalf("only %d of %d commitments carry a date: %v", dated, len(d.Commitments), d.Commitments)
	}

	joined := strings.Join(d.Decisions, " | ")
	if !strings.Contains(joined, "WCH") {
		t.Fatalf("Decisions = %v", d.Decisions)
	}
}

func TestTheDigestKeepsToItsOwnDay(t *testing.T) {
	us := append(aDay(),
		said("2026-08-11T09:00:00Z", "me", "I'll order the connectors on Thursday."))
	eps := Segment(us, Options{})

	monday := Day(at("2026-08-10T00:00:00Z"), eps, Options{}, DigestLimits{})
	for _, c := range monday.Commitments {
		if strings.Contains(c, "connectors") {
			t.Fatalf("Tuesday's commitment leaked into Monday's digest: %q", c)
		}
	}
	tuesday := Day(at("2026-08-11T00:00:00Z"), eps, Options{}, DigestLimits{})
	if len(tuesday.Commitments) != 1 {
		t.Fatalf("Tuesday = %+v, want just the one", tuesday.Commitments)
	}
}

// A short digest because the recogniser fell over reads exactly like a quiet
// day. Those are opposite facts and the coverage block is what tells them apart.
func TestCoverageSaysWhatIsMissing(t *testing.T) {
	base := at("2026-08-10T09:00:00Z")
	us := []Utterance{
		said("2026-08-10T09:00:00Z", "", "Something was said here but nobody knows by whom."),
		{At: base.Add(30 * time.Second), End: base.Add(90 * time.Second), Gap: true, GapReason: "the link dropped"},
	}
	eps := Segment(us, Options{})
	d := Day(at("2026-08-10T00:00:00Z"), eps, Options{}, DigestLimits{})

	if d.Coverage.Gaps != 1 {
		t.Fatalf("Coverage.Gaps = %d, want 1", d.Coverage.Gaps)
	}
	if d.Coverage.Ambient != 1 {
		t.Fatalf("Coverage.Ambient = %d, want the unattributable episode counted", d.Coverage.Ambient)
	}
	if len(d.Coverage.Notes) == 0 {
		t.Fatal("the coverage block should carry the episode's own caveats")
	}
}

func TestDigestLimitsAreEnforced(t *testing.T) {
	var us []Utterance
	base := at("2026-08-10T09:00:00Z")
	for i := 0; i < 40; i++ {
		t := base.Add(time.Duration(i) * 30 * time.Second)
		us = append(us, Utterance{
			At: t, End: t.Add(4 * time.Second), Speaker: "me",
			Text: "I'll check revision R" + string(rune('A'+i%26)) + "7 of the layout tomorrow.",
		})
	}
	eps := Segment(us, Options{})
	d := Day(at("2026-08-10T00:00:00Z"), eps, Options{}, DigestLimits{Commitments: 5, Notes: 2, Decisions: 1})
	if len(d.Commitments) != 5 {
		t.Fatalf("Commitments = %d, want the cap", len(d.Commitments))
	}
	if len(d.Notes) > 2 {
		t.Fatalf("Notes = %d, want at most the cap", len(d.Notes))
	}
}

func TestAnEmptyDayIsAnEmptyDigestNotANilOne(t *testing.T) {
	d := Day(at("2026-08-10T00:00:00Z"), nil, Options{}, DigestLimits{})
	if d.Notes == nil || d.Commitments == nil || d.Decisions == nil {
		t.Fatalf("digest = %+v, want empty slices so the JSON frame carries [] rather than null", d)
	}
	if d.Coverage.Episodes != 0 {
		t.Fatalf("Coverage.Episodes = %d", d.Coverage.Episodes)
	}
}
