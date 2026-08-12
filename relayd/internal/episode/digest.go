package episode

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// The daily digest — SYSTEM.md §4's outputs table, exactly:
//
//	Daily digest | {notes[], commitments[], decisions[]} | phone + memory
//
// Three lists of sentences, and the shape matches `internal/api`'s Digest frame
// field for field so the transport is a marshal rather than a translation. It is
// defined here rather than imported from there because the dependency runs
// api → episode; M3 owns that package and a cycle would be the result of doing
// it the other way round.

// Digest is the day, in three lists.
type Digest struct {
	// Day is the date this covers, at midnight in the digest's own location.
	Day         time.Time `json:"day"`
	Notes       []string  `json:"notes"`
	Commitments []string  `json:"commitments"`
	Decisions   []string  `json:"decisions"`
	// Coverage says what the day is made of and what is missing from it. A
	// digest with three bullets because the recogniser fell over reads exactly
	// like a quiet day, and those are opposite facts.
	Coverage Coverage `json:"coverage"`
}

// Coverage is the honest footnote on a digest.
type Coverage struct {
	Episodes int `json:"episodes"`
	// Gaps is how many stretches of audio never arrived.
	Gaps int `json:"gaps"`
	// Ambient is how many episodes could not be attributed to anyone.
	Ambient int `json:"ambient"`
	// Recorded is how much of the day is covered by episodes at all.
	Recorded time.Duration `json:"recorded"`
	// Notes carries the segmenter's and the transcriber's own caveats.
	Notes []string `json:"notes,omitempty"`
}

// DigestLimits caps a digest.
//
// SYSTEM.md §7c's rule is "cap length before synthesis, not after" — an agent
// that returns 2,000 characters should be summarised to something speakable
// first, and it names that as the single largest cost spike. A digest is read
// more often than it is spoken, so the caps here are generous by comparison and
// still finite.
type DigestLimits struct {
	Notes       int
	Commitments int
	Decisions   int
}

// Defaults for [DigestLimits].
const (
	DefaultDigestNotes       = 7
	DefaultDigestCommitments = 12
	DefaultDigestDecisions   = 7
)

func (l DigestLimits) withDefaults() DigestLimits {
	if l.Notes <= 0 {
		l.Notes = DefaultDigestNotes
	}
	if l.Commitments <= 0 {
		l.Commitments = DefaultDigestCommitments
	}
	if l.Decisions <= 0 {
		l.Decisions = DefaultDigestDecisions
	}
	return l
}

// Day builds the digest for one day from its episodes.
//
// Episodes outside the day are skipped rather than silently folded in: a digest
// for Tuesday that quietly contains Monday's commitments is worse than one that
// is short.
func Day(day time.Time, eps []Episode, o Options, limits DigestLimits) Digest {
	o = o.withDefaults()
	limits = limits.withDefaults()

	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location())
	end := start.AddDate(0, 0, 1)

	d := Digest{Day: start, Notes: []string{}, Commitments: []string{}, Decisions: []string{}}
	seenNote := map[string]bool{}
	seenDecision := map[string]bool{}

	var (
		commitments []Commitment
		noteSet     []string
	)
	for _, e := range eps {
		if e.StartedAt.Before(start) || !e.StartedAt.Before(end) {
			continue
		}
		d.Coverage.Episodes++
		d.Coverage.Gaps += e.Gaps
		d.Coverage.Recorded += e.Duration()
		if e.Kind == KindAmbient {
			d.Coverage.Ambient++
		}
		for _, n := range e.Notes {
			if !seenNote[n] {
				seenNote[n] = true
				d.Coverage.Notes = append(d.Coverage.Notes, n)
			}
		}

		ex := Extract(e, o)
		commitments = append(commitments, ex.Commitments...)
		for _, dec := range ex.Decisions {
			line := renderDecision(dec, o.Wearer)
			if seenDecision[line] {
				continue
			}
			seenDecision[line] = true
			d.Decisions = append(d.Decisions, line)
		}
		noteSet = append(noteSet, ex.Notes...)
	}

	// Commitments with a date first, soonest first, then the undated ones in
	// the order they were said. A digest is read top-down and the thing due
	// tomorrow belongs above the thing with no date at all.
	sort.SliceStable(commitments, func(i, j int) bool {
		a, b := commitments[i], commitments[j]
		switch {
		case !a.DueAt.IsZero() && b.DueAt.IsZero():
			return true
		case a.DueAt.IsZero() && !b.DueAt.IsZero():
			return false
		case !a.DueAt.IsZero() && !b.DueAt.IsZero() && !a.DueAt.Equal(b.DueAt):
			return a.DueAt.Before(b.DueAt)
		}
		return a.At.Before(b.At)
	})

	seenC := map[string]bool{}
	for _, c := range commitments {
		line := RenderCommitment(c, o.Wearer)
		if seenC[line] {
			continue
		}
		seenC[line] = true
		d.Commitments = append(d.Commitments, line)
	}

	seenN := map[string]bool{}
	for _, n := range noteSet {
		if seenN[n] {
			continue
		}
		seenN[n] = true
		d.Notes = append(d.Notes, n)
	}

	d.Notes = limitTo(d.Notes, limits.Notes)
	d.Commitments = limitTo(d.Commitments, limits.Commitments)
	d.Decisions = limitTo(d.Decisions, limits.Decisions)
	return d
}

// limitTo is named rather than called cap, because shadowing a builtin in a file
// somebody will read at 2 a.m. is a small unkindness.
func limitTo(in []string, n int) []string {
	if len(in) <= n {
		return in
	}
	return in[:n]
}

// RenderCommitment writes a commitment the way ARCHITECTURE.md §4 words it:
// "You told Marc you'd send the BOM by Friday."
//
// The wearer's own commitments read as "you"; somebody else's read with their
// name, because "you owe Marc a BOM" and "Marc owes you a BOM" are opposite
// facts and a digest that blurs them is worse than no digest.
func RenderCommitment(c Commitment, wearer string) string {
	var b strings.Builder
	switch {
	case c.Speaker == "":
		b.WriteString("Someone said: ")
	case c.Speaker == wearer:
		if c.OwedTo != "" && c.OwedTo != wearer {
			b.WriteString("You told " + c.OwedTo + ": ")
		} else {
			b.WriteString("You said: ")
		}
	default:
		if c.OwedTo == wearer {
			b.WriteString(c.Speaker + " told you: ")
		} else {
			b.WriteString(c.Speaker + " said: ")
		}
	}
	b.WriteString(strings.TrimSpace(c.Text))
	if !c.DueAt.IsZero() {
		b.WriteString(" — due " + c.DueAt.Format("Mon 2 Jan"))
	}
	return b.String()
}

func renderDecision(d Decision, wearer string) string {
	text := strings.TrimSpace(d.Text)
	if d.Speaker == "" || d.Speaker == wearer {
		return text
	}
	return fmt.Sprintf("%s: %s", d.Speaker, text)
}
