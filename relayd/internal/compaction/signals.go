package compaction

import (
	"time"

	"github.com/luthor007/relay/relayd/internal/search"
)

// Statement is the user saying which way it goes. It outranks every computed
// signal in both directions, which is the same escape hatch ORCHESTRATOR.md §4
// gives routing: cheap, always correct, and the first thing someone reaches for
// once a heuristic has surprised them.
type Statement string

const (
	StatementNone     Statement = ""
	StatementNew      Statement = "new"
	StatementContinue Statement = "continue"
)

// Thresholds are the compact-versus-new cut-offs.
//
// None of these numbers is measured. They are exported and defaulted rather
// than baked in for the same reason routing's weights are: the shape of the
// decision is the doc's, the constants are a starting point, and moving them
// must not need a rebuild. What *is* load-bearing is the asymmetry below, and
// that is in the code rather than in the numbers.
type Thresholds struct {
	// DriftHard is the cosine distance between the session summary and the
	// recent turn summaries above which the topic has plainly changed.
	DriftHard float64
	// DriftSoft is drift that only counts alongside another signal.
	DriftSoft float64

	// GapHard is the silence after which a resumed session is usually a new
	// topic wearing an old session's name. MEMORY.md §9 says three days.
	GapHard time.Duration
	// GapSoft is a gap that only counts alongside drift.
	GapSoft time.Duration
}

// DefaultThresholds returns the starting point.
func DefaultThresholds() Thresholds {
	return Thresholds{
		DriftHard: 0.45,
		DriftSoft: 0.30,
		GapHard:   72 * time.Hour,
		GapSoft:   12 * time.Hour,
	}
}

func (t Thresholds) withDefaults() Thresholds {
	d := DefaultThresholds()
	if t.DriftHard <= 0 {
		t.DriftHard = d.DriftHard
	}
	if t.DriftSoft <= 0 {
		t.DriftSoft = d.DriftSoft
	}
	if t.GapHard <= 0 {
		t.GapHard = d.GapHard
	}
	if t.GapSoft <= 0 {
		t.GapSoft = d.GapSoft
	}
	if t.DriftSoft > t.DriftHard {
		t.DriftSoft = t.DriftHard
	}
	if t.GapSoft > t.GapHard {
		t.GapSoft = t.GapHard
	}
	return t
}

// Signals are MEMORY.md §9's four inputs to compact-versus-new. Every one of
// them already exists somewhere else in this codebase; none of them is a new
// classifier.
type Signals struct {
	// Drift is the cosine *distance* in [0,2] between the session's own summary
	// and its recent turns — 0 is the same topic, 1 is unrelated. Built by
	// [DriftBetween] from the embeddings internal/search already stores.
	Drift float64
	// DriftKnown is false when there was no embedding to compare, which happens
	// on every box with no embedder configured and on every session summarised
	// but not yet embedded. Unknown drift is never read as zero drift.
	DriftKnown bool

	// WorkspaceChanged is the cheap, strongly predictive one: the session is
	// being used from a different repo or working directory than it was opened
	// in.
	WorkspaceChanged bool

	// Gap is the silence before the utterance that woke this session.
	Gap time.Duration

	// Stated is the user's own answer, if they gave one.
	Stated Statement
}

// NewTopic reports whether this looks like a new topic rather than a
// continuation, and the sentence explaining why.
//
// The combination rule is deliberately asymmetric, and the asymmetry is the
// costs in MEMORY.md §9's table. Getting *compact* wrong when the topic changed
// drags irrelevant context forward and you pay for it on every turn thereafter,
// forever. Getting *new* wrong costs one session's history, once, on a session
// that was about to be compacted into a summary anyway. So a single strong
// signal is enough to start fresh, while continuing is the answer only when
// nothing at all suggests otherwise.
func (s Signals) NewTopic(t Thresholds) (bool, string) {
	t = t.withDefaults()

	switch s.Stated {
	case StatementNew:
		return true, "you said to start a new one"
	case StatementContinue:
		return false, "you said to keep going"
	}

	if s.WorkspaceChanged {
		return true, "it moved to a different repo"
	}
	if s.DriftKnown && s.Drift >= t.DriftHard {
		return true, "the recent turns are about something else"
	}
	if s.Gap >= t.GapHard {
		return true, "it has been quiet for " + humanDuration(s.Gap)
	}
	if s.DriftKnown && s.Drift >= t.DriftSoft && s.Gap >= t.GapSoft {
		return true, "it drifted, and it has been quiet for " + humanDuration(s.Gap)
	}
	return false, "the same work is still going"
}

// DriftBetween is the cosine distance between a session's summary embedding and
// the mean of its recent turn embeddings.
//
// ok is false — never a zero distance — when the two cannot be compared:
// different widths, an empty vector, a zero vector. search.Cosine returns 0 for
// all three, and 0 through this function would mean "identical topic", which is
// the most confident possible answer to a question nothing answered. That is
// the same rule event.Usage follows with pointers.
func DriftBetween(session, recent []float32) (float64, bool) {
	if len(session) == 0 || len(recent) == 0 || len(session) != len(recent) {
		return 0, false
	}
	if zero(session) || zero(recent) {
		return 0, false
	}
	return 1 - search.Cosine(session, recent), true
}

// MeanVector averages the recent turn embeddings into one vector to compare
// against the session summary. Vectors of the wrong width are skipped rather
// than truncated, and skipping every one of them yields nil, which
// [DriftBetween] reads as unknown.
func MeanVector(vs [][]float32) []float32 {
	var width int
	for _, v := range vs {
		if len(v) > 0 {
			width = len(v)
			break
		}
	}
	if width == 0 {
		return nil
	}

	out := make([]float32, width)
	n := 0
	for _, v := range vs {
		if len(v) != width {
			continue
		}
		for i, x := range v {
			out[i] += x
		}
		n++
	}
	if n == 0 {
		return nil
	}
	inv := float32(1) / float32(n)
	for i := range out {
		out[i] *= inv
	}
	if zero(out) {
		return nil
	}
	search.Normalize(out)
	return out
}

func zero(v []float32) bool {
	for _, x := range v {
		if x != 0 {
			return false
		}
	}
	return true
}

// humanDuration is for a spoken or written reason, so it says "three days" and
// not "72h0m0s".
func humanDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return itoa(int(d/(24*time.Hour))) + " days"
	case d >= 24*time.Hour:
		return "a day"
	case d >= 2*time.Hour:
		return itoa(int(d/time.Hour)) + " hours"
	case d >= time.Hour:
		return "an hour"
	case d >= 2*time.Minute:
		return itoa(int(d/time.Minute)) + " minutes"
	default:
		return "a moment"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
