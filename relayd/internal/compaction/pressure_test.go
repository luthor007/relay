package compaction

import (
	"errors"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/event"
)

// The whole point of Numerator. ADAPTERS.md §2's fixture: turn 1 took two
// requests and result.usage reports 51,997 cache-read tokens for a context that
// was ~33,600. Against the 1,000,000 window from modelUsage that is 5.2% rather
// than 3.4% — small here, and eightfold on a turn with eight tool calls.
func TestPerTurnUsageIsRefused(t *testing.T) {
	_, err := Observe(Observation{
		Runtime: adapter.ClaudeCode,
		Model:   "claude-opus-5[1m]",
		Kind:    NumeratorTurn,
		Used:    51997 + 18502,
		Have:    true,
		Window:  1_000_000,
	}, Windows{})
	if !errors.Is(err, ErrTurnUsage) {
		t.Fatalf("a per-turn usage must be refused, got %v", err)
	}

	if _, err := FromTurnUsage(event.TurnCompleted{}); !errors.Is(err, ErrTurnUsage) {
		t.Fatalf("FromTurnUsage must always refuse, got %v", err)
	}
}

func TestNumeratorMustBeStated(t *testing.T) {
	_, err := Observe(Observation{Runtime: adapter.Codex, Used: 10, Have: true, Window: 100}, Windows{})
	if !errors.Is(err, ErrNumeratorUnset) {
		t.Fatalf("an unset numerator must be an error, got %v", err)
	}
}

// The fixture's three requests, monotonic and true, against modelUsage's
// contextWindow.
func TestClaudeCodeLiveReading(t *testing.T) {
	for _, used := range []int64{33497, 33609, 33637} {
		r, err := FromLatestRequest(adapter.ClaudeCode, "claude-opus-5[1m]", used, 1_000_000, 3, Windows{})
		if err != nil {
			t.Fatal(err)
		}
		if r.Source != WindowRuntime {
			t.Fatalf("window source = %q, want runtime", r.Source)
		}
		p, ok := r.Pressure()
		if !ok {
			t.Fatal("pressure should be known")
		}
		if p < 0.033 || p > 0.034 {
			t.Fatalf("pressure = %v, want ~0.0336", p)
		}
		if r.Degraded() != "" {
			t.Fatalf("a fully reported reading must not be degraded: %q", r.Degraded())
		}
		if r.Estimated() {
			t.Fatal("a runtime-reported window is not an estimate")
		}
	}
}

// Codex's modelContextWindow is int64|null even when the counts are present.
func TestCodexNullWindowDegradesVisibly(t *testing.T) {
	u := &event.Usage{
		InputTokens:       event.I64(180_000),
		CachedInputTokens: event.I64(20_000),
		TotalTokens:       event.I64(210_000),
		// ContextWindow deliberately nil.
	}

	r, err := FromThreadTotal(adapter.Codex, "gpt-5-codex", u, 12, Windows{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Pressure(); ok {
		t.Fatal("pressure must be unknown without a denominator")
	}
	if r.Degraded() == "" {
		t.Fatal("a missing window must degrade visibly, not silently")
	}
	if r.Turns != 12 {
		t.Fatalf("turns = %d, want 12", r.Turns)
	}

	// With a fallback the same observation becomes usable, and says it is an
	// estimate.
	r, err = FromThreadTotal(adapter.Codex, "gpt-5-codex", u, 12, Windows{ByModel: map[string]int64{"gpt-5-codex": 272_000}})
	if err != nil {
		t.Fatal(err)
	}
	p, ok := r.Pressure()
	if !ok {
		t.Fatal("the fallback window should make pressure computable")
	}
	if p < 0.73 || p > 0.74 {
		t.Fatalf("pressure = %v, want ~0.735", p)
	}
	if !r.Estimated() {
		t.Fatal("a fallback window must report itself as an estimate")
	}
	if r.Degraded() != "" {
		t.Fatalf("a fallback window is an estimate, not a degradation: %q", r.Degraded())
	}
}

// nil means unknown, never zero — event.Usage's rule, applied here.
func TestNilUsageIsUnknownNotZero(t *testing.T) {
	r, err := FromThreadTotal(adapter.Codex, "gpt-5-codex", nil, 0, Windows{Default: 272_000})
	if err != nil {
		t.Fatal(err)
	}
	if r.Known() {
		t.Fatal("a nil usage must not read as a session with zero tokens used")
	}
	if _, ok := r.Pressure(); ok {
		t.Fatal("pressure must be unknown")
	}
	if !strings.Contains(r.Degraded(), "falling back to turns") {
		t.Fatalf("degraded reason = %q", r.Degraded())
	}
}

// ACP: the word "token" appears twice in the whole 87-definition schema, both
// times in a stop reason.
func TestACPHasNoPressureAtAll(t *testing.T) {
	for _, rt := range []adapter.Runtime{adapter.OpenClaw, adapter.Hermes, adapter.OpenCode} {
		r := Unmeasured(rt, "", 31)
		if r.Known() {
			t.Fatalf("%s cannot report pressure", rt)
		}
		if !strings.Contains(r.Degraded(), "no token or usage field") {
			t.Fatalf("%s degraded reason = %q", rt, r.Degraded())
		}
		if r.Turns != 31 {
			t.Fatalf("%s turns = %d", rt, r.Turns)
		}
	}
}

func TestCanonicalModel(t *testing.T) {
	cases := map[string]string{
		"claude-opus-5[1m]": "claude-opus-5",
		"claude-opus-5":     "claude-opus-5",
		" gpt-5-codex ":     "gpt-5-codex",
		"":                  "",
	}
	for in, want := range cases {
		if got := CanonicalModel(in); got != want {
			t.Errorf("CanonicalModel(%q) = %q, want %q", in, got, want)
		}
	}
}

// A table keyed on the decorated id misses every long-context session, which is
// exactly the population this package exists for.
func TestWindowsLookupIgnoresTheDecoration(t *testing.T) {
	w := Windows{ByModel: map[string]int64{"claude-opus-5": 1_000_000}}
	if v, ok := w.Lookup("claude-opus-5[1m]"); !ok || v != 1_000_000 {
		t.Fatalf("lookup of a decorated id = %d, %v", v, ok)
	}
	if _, ok := w.Lookup("something-else"); ok {
		t.Fatal("an unknown model with no default must miss")
	}
	if v, ok := (Windows{Default: 200_000}).Lookup("anything"); !ok || v != 200_000 {
		t.Fatalf("default = %d, %v", v, ok)
	}
	if _, ok := (Windows{}).Lookup("anything"); ok {
		t.Fatal("an empty table must not invent a window")
	}
}

func TestPressureIsNotClamped(t *testing.T) {
	r, err := FromLatestRequest(adapter.ClaudeCode, "m", 110, 100, 1, Windows{})
	if err != nil {
		t.Fatal(err)
	}
	p, ok := r.Pressure()
	if !ok || p <= 1 {
		t.Fatalf("a session over its own window must read over 1.0, got %v", p)
	}
}
