package compaction

import (
	"math"
	"strings"
	"testing"
	"time"
)

func vec(v ...float32) []float32 { return v }

// search.Cosine returns 0 for mismatched widths and for zero vectors, and 0
// through DriftBetween would mean "identical topic" — the most confident
// possible answer to a question nothing answered.
func TestUnknownDriftIsNeverZeroDrift(t *testing.T) {
	cases := map[string][2][]float32{
		"different widths": {vec(1, 0, 0), vec(1, 0)},
		"empty session":    {nil, vec(1, 0, 0)},
		"empty recent":     {vec(1, 0, 0), nil},
		"zero session":     {vec(0, 0, 0), vec(1, 0, 0)},
		"zero recent":      {vec(1, 0, 0), vec(0, 0, 0)},
	}
	for name, c := range cases {
		if _, ok := DriftBetween(c[0], c[1]); ok {
			t.Errorf("%s: drift must be unknown, not zero", name)
		}
	}

	// And an unknown drift never decides anything.
	s := Signals{Drift: 0, DriftKnown: false}
	if newTopic, _ := s.NewTopic(Thresholds{}); newTopic {
		t.Fatal("unknown drift must not start a new session")
	}
}

func TestDriftBetween(t *testing.T) {
	same, ok := DriftBetween(vec(1, 0, 0), vec(1, 0, 0))
	if !ok || math.Abs(same) > 1e-6 {
		t.Fatalf("identical vectors: drift = %v, ok = %v", same, ok)
	}
	orth, ok := DriftBetween(vec(1, 0, 0), vec(0, 1, 0))
	if !ok || math.Abs(orth-1) > 1e-6 {
		t.Fatalf("orthogonal vectors: drift = %v, want 1", orth)
	}
	opp, ok := DriftBetween(vec(1, 0, 0), vec(-1, 0, 0))
	if !ok || math.Abs(opp-2) > 1e-6 {
		t.Fatalf("opposed vectors: drift = %v, want 2", opp)
	}
}

func TestMeanVector(t *testing.T) {
	got := MeanVector([][]float32{vec(1, 0, 0), vec(0, 1, 0)})
	if len(got) != 3 {
		t.Fatalf("width = %d, want 3", len(got))
	}
	// Normalised, so both components are 1/sqrt(2).
	want := float32(1 / math.Sqrt2)
	if math.Abs(float64(got[0]-want)) > 1e-6 || math.Abs(float64(got[1]-want)) > 1e-6 {
		t.Fatalf("mean = %v, want ~[%v %v 0]", got, want, want)
	}

	if MeanVector(nil) != nil {
		t.Fatal("no vectors is unknown, not a zero vector")
	}
	if MeanVector([][]float32{vec(0, 0), vec(0, 0)}) != nil {
		t.Fatal("a zero mean is unknown, not a direction")
	}
	// Wrong widths are skipped rather than truncated.
	if got := MeanVector([][]float32{vec(1, 0, 0), vec(1, 1)}); len(got) != 3 {
		t.Fatalf("mixed widths: got width %d, want the majority width kept", len(got))
	}
}

func TestNewTopicSignals(t *testing.T) {
	th := DefaultThresholds()

	cases := []struct {
		name string
		s    Signals
		want bool
		says string
	}{
		{"nothing", Signals{}, false, "same work"},
		{"the user said new", Signals{Stated: StatementNew}, true, "you said"},
		{"the user said continue, everything else screaming", Signals{
			Stated: StatementContinue, WorkspaceChanged: true, Drift: 1.5, DriftKnown: true,
			Gap: 30 * 24 * time.Hour,
		}, false, "you said"},
		{"repo changed", Signals{WorkspaceChanged: true}, true, "different repo"},
		{"hard drift", Signals{Drift: 0.6, DriftKnown: true}, true, "something else"},
		{"soft drift alone", Signals{Drift: 0.35, DriftKnown: true}, false, "same work"},
		{"three days quiet", Signals{Gap: 73 * time.Hour}, true, "quiet for"},
		{"soft drift plus a night", Signals{Drift: 0.35, DriftKnown: true, Gap: 14 * time.Hour}, true, "drifted"},
		{"soft drift plus an hour", Signals{Drift: 0.35, DriftKnown: true, Gap: time.Hour}, false, "same work"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, why := c.s.NewTopic(th)
			if got != c.want {
				t.Fatalf("NewTopic = %v (%s), want %v", got, why, c.want)
			}
			if !strings.Contains(why, c.says) {
				t.Fatalf("reason = %q, want it to mention %q", why, c.says)
			}
		})
	}
}

func TestThresholdDefaultsCannotInvert(t *testing.T) {
	th := Thresholds{DriftHard: 0.2, DriftSoft: 0.9, GapHard: time.Hour, GapSoft: 48 * time.Hour}.withDefaults()
	if th.DriftSoft > th.DriftHard {
		t.Fatalf("soft drift %v must not exceed hard %v", th.DriftSoft, th.DriftHard)
	}
	if th.GapSoft > th.GapHard {
		t.Fatalf("soft gap %v must not exceed hard %v", th.GapSoft, th.GapHard)
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		30 * time.Second:    "a moment",
		5 * time.Minute:     "5 minutes",
		90 * time.Minute:    "an hour",
		5 * time.Hour:       "5 hours",
		25 * time.Hour:      "a day",
		73 * time.Hour:      "3 days",
		30 * 24 * time.Hour: "30 days",
	}
	for in, want := range cases {
		if got := humanDuration(in); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", in, got, want)
		}
	}
}
