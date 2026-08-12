package compaction

import (
	"errors"
	"math"
	"math/rand"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/adapter"
)

// The single configuration in MEMORY.md §9's survey that provably converts a
// graceful pause into a lost thread. No input may produce a limit that reaches
// the wall.
func TestCodexLimitNeverReachesWindow(t *testing.T) {
	windows := []int64{1, 2, 1000, 8192, 100_000, 272_000, 1_000_000, math.MaxInt32}
	requests := []int64{-1, 0, 1, 1000, 258_000, 271_999, 272_000, 272_001, 10_000_000, math.MaxInt64}

	for _, w := range windows {
		for _, r := range requests {
			lim, err := CodexAutoCompactLimit(r, w)
			if err != nil {
				// The only legal refusal is a window with no room at all.
				if w > 2 {
					t.Fatalf("window=%d requested=%d: unexpected error %v", w, r, err)
				}
				continue
			}
			if lim.Value >= w {
				t.Fatalf("window=%d requested=%d: limit %d reaches the wall", w, r, lim.Value)
			}
			if lim.Value <= 0 {
				t.Fatalf("window=%d requested=%d: limit %d is not usable", w, r, lim.Value)
			}
			if err := CheckCodexLimit(lim.Value, w); err != nil {
				t.Fatalf("window=%d requested=%d: own output fails own check: %v", w, r, err)
			}
		}
	}

	// And fuzz it, because "no input" is the claim.
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 5000; i++ {
		w := rng.Int63n(2_000_000) + 3
		r := rng.Int63n(4_000_000) - 1_000_000
		lim, err := CodexAutoCompactLimit(r, w)
		if err != nil {
			t.Fatalf("window=%d requested=%d: %v", w, r, err)
		}
		if lim.Value >= w {
			t.Fatalf("window=%d requested=%d: limit %d reaches the wall", w, r, lim.Value)
		}
	}
}

func TestCodexLimitRefusesAnUnknownWall(t *testing.T) {
	if _, err := CodexAutoCompactLimit(258_000, 0); !errors.Is(err, ErrUnknownWindow) {
		t.Fatalf("err = %v, want ErrUnknownWindow", err)
	}
	if err := CheckCodexLimit(258_000, 0); !errors.Is(err, ErrUnknownWindow) {
		t.Fatalf("err = %v, want ErrUnknownWindow", err)
	}
}

// Codex ships 258k under a 272k wall; the clamp should land in that
// neighbourhood rather than inventing a tighter or looser gap.
func TestCodexLimitReproducesTheShippedHeadroom(t *testing.T) {
	lim, err := CodexAutoCompactLimit(0, 272_000)
	if err != nil {
		t.Fatal(err)
	}
	if lim.Value != 272_000-13_600 {
		t.Fatalf("limit = %d, want 258400", lim.Value)
	}
	if lim.Clamped {
		t.Fatal("nothing was requested, so nothing was clamped")
	}
}

func TestCheckCodexLimitRejectsTheWallItself(t *testing.T) {
	for _, v := range []int64{272_000, 272_001, 1_000_000} {
		if err := CheckCodexLimit(v, 272_000); !errors.Is(err, ErrWouldExceedWindow) {
			t.Fatalf("CheckCodexLimit(%d, 272000) = %v, want ErrWouldExceedWindow", v, err)
		}
	}
	if err := CheckCodexLimit(258_000, 272_000); err != nil {
		t.Fatalf("a safe limit must pass: %v", err)
	}
}

// Two runtimes fail explicitly in their own source when auto-compaction is off.
// There must be no way through this package to write that.
func TestRefuseWillNotDisableAutoCompaction(t *testing.T) {
	cases := []struct {
		rt  adapter.Runtime
		key string
		val any
	}{
		{adapter.OpenCode, "compaction.auto", false},
		{adapter.OpenCode, "compaction.auto", "false"},
		{adapter.Hermes, "compression.enabled", false},
		{adapter.Hermes, "compression_enabled", false},
		{adapter.ClaudeCode, "autoCompactEnabled", false},
		{adapter.OpenClaw, "agents.defaults.compaction.enabled", false},
	}
	for _, c := range cases {
		if err := Refuse(c.rt, c.key, c.val, 272_000); !errors.Is(err, ErrWouldDisable) {
			t.Errorf("Refuse(%s, %s, %v) = %v, want ErrWouldDisable", c.rt, c.key, c.val, err)
		}
		// Turning them on is always fine.
		if err := Refuse(c.rt, c.key, true, 272_000); err != nil {
			t.Errorf("Refuse(%s, %s, true) = %v, want nil", c.rt, c.key, err)
		}
	}
}

func TestRefuseGuardsTheCodexLimit(t *testing.T) {
	if err := Refuse(adapter.Codex, "model_auto_compact_token_limit", 272_000, 272_000); !errors.Is(err, ErrWouldExceedWindow) {
		t.Fatalf("err = %v, want ErrWouldExceedWindow", err)
	}
	if err := Refuse(adapter.Codex, "model_auto_compact_token_limit", 258_000, 272_000); err != nil {
		t.Fatalf("a safe limit must pass: %v", err)
	}
	if err := Refuse(adapter.Codex, "model_auto_compact_token_limit", 258_000, 0); !errors.Is(err, ErrUnknownWindow) {
		t.Fatalf("err = %v, want ErrUnknownWindow", err)
	}
	if err := Refuse(adapter.Codex, "model_auto_compact_token_limit", "lots", 272_000); err == nil {
		t.Fatal("a non-integer limit must be refused")
	}
}

func TestRefuseGuardsHermesThreshold(t *testing.T) {
	// Its own default, which is the one that fights our pass.
	if err := Refuse(adapter.Hermes, "compression.threshold", 0.50, 0); !errors.Is(err, ErrWouldFightIdlePass) {
		t.Fatalf("err = %v, want ErrWouldFightIdlePass", err)
	}
	if err := Refuse(adapter.Hermes, "compression.threshold", HermesThreshold, 0); err != nil {
		t.Fatalf("0.90 must pass: %v", err)
	}
	for _, bad := range []float64{0, -1, 1.5} {
		if err := Refuse(adapter.Hermes, "compression.threshold", bad, 0); err == nil {
			t.Fatalf("threshold %v must be refused", bad)
		}
	}
}

func TestHermesPlanRaisesTheThresholdAndLeavesItOn(t *testing.T) {
	p := Plan(PlanInput{Runtime: adapter.Hermes, CurrentHermesThreshold: 0.50})

	var sawThreshold, sawEnabled bool
	for _, s := range p.Settings {
		switch s.Key {
		case "compression.threshold":
			sawThreshold = true
			if s.Float == nil || *s.Float != HermesThreshold {
				t.Fatalf("threshold = %v, want %v", s.Float, HermesThreshold)
			}
		case "compression.enabled":
			sawEnabled = true
			if s.Bool == nil || !*s.Bool {
				t.Fatal("compression must stay enabled")
			}
		}
		if err := Refuse(s.Runtime, s.Key, settingValue(s), 0); err != nil {
			t.Fatalf("this package produced a setting its own guard refuses: %v", err)
		}
	}
	if !sawThreshold || !sawEnabled {
		t.Fatalf("settings = %v", p.Settings)
	}
	if len(p.KeepEnabled) == 0 {
		t.Fatal("the plan must say what it is deliberately leaving switched on")
	}

	// Already raised: nothing to write.
	if p := Plan(PlanInput{Runtime: adapter.Hermes, CurrentHermesThreshold: 0.95}); len(p.Settings) != 0 {
		t.Fatalf("settings = %v, want none", p.Settings)
	}
}

func TestCodexPlanRefusesWithoutAWindow(t *testing.T) {
	p := Plan(PlanInput{Runtime: adapter.Codex, CurrentCodexLimit: 900_000})
	if len(p.Settings) != 0 {
		t.Fatalf("settings = %v, want none without a known window", p.Settings)
	}
	if len(p.Refusals) == 0 || !strings.Contains(p.Refusals[0], "model_auto_compact_token_limit") {
		t.Fatalf("refusals = %v", p.Refusals)
	}

	// With a window, a dangerous existing value is clamped.
	p = Plan(PlanInput{Runtime: adapter.Codex, ContextWindow: 272_000, CurrentCodexLimit: 272_000})
	if len(p.Settings) != 1 {
		t.Fatalf("settings = %v, want one", p.Settings)
	}
	s := p.Settings[0]
	if !s.Clamped || s.Int == nil || *s.Int >= 272_000 {
		t.Fatalf("setting = %s (clamped=%v)", s, s.Clamped)
	}
	if err := Refuse(adapter.Codex, s.Key, *s.Int, 272_000); err != nil {
		t.Fatalf("this package produced a setting its own guard refuses: %v", err)
	}
}

// Four of the five need nothing. Writing an unmeasured "improvement" into
// someone's config is not a neutral act.
func TestUnmeasuredKnobsAreLeftAlone(t *testing.T) {
	for _, rt := range []adapter.Runtime{adapter.OpenCode, adapter.OpenClaw, adapter.ClaudeCode} {
		p := Plan(PlanInput{Runtime: rt})
		if len(p.Settings) != 0 {
			t.Fatalf("%s: settings = %v, want none", rt, p.Settings)
		}
		if len(p.Refusals) == 0 {
			t.Fatalf("%s: leaving something alone should say so", rt)
		}
	}
}

func TestPlanAllIsStable(t *testing.T) {
	plans := PlanAll(map[adapter.Runtime]PlanInput{
		adapter.Hermes:     {CurrentHermesThreshold: 0.5},
		adapter.Codex:      {ContextWindow: 272_000},
		adapter.ClaudeCode: {},
	})
	if len(plans) != 3 {
		t.Fatalf("plans = %d, want 3", len(plans))
	}
	for i := 1; i < len(plans); i++ {
		if plans[i-1].Runtime > plans[i].Runtime {
			t.Fatalf("plans are not sorted: %v", plans)
		}
	}
}

// Every runtime has a documented on-demand trigger, and only one of them tells
// us it happened.
func TestMechanismCoverage(t *testing.T) {
	for _, rt := range adapter.Runtimes() {
		m, ok := MechanismFor(rt)
		if !ok {
			t.Fatalf("%s: no mechanism", rt)
		}
		if m.Method == "" || m.Note == "" {
			t.Fatalf("%s: mechanism = %+v", rt, m)
		}
		if m.Observable && rt != adapter.Codex {
			t.Fatalf("%s claims an observable compaction; only Codex has one (item/completed contextCompaction)", rt)
		}
		if m.RequiresLease != (rt == adapter.Hermes) {
			t.Fatalf("%s: RequiresLease = %v; only Hermes has compression_locks", rt, m.RequiresLease)
		}
	}
	if _, ok := MechanismFor("gemini-cli"); ok {
		t.Fatal("an unknown runtime must not get a fabricated mechanism")
	}
}

func TestSettingString(t *testing.T) {
	n := int64(258_400)
	f := 0.9
	b := true
	if got := (Setting{Key: "k", Int: &n}).String(); got != "k = 258400" {
		t.Fatal(got)
	}
	if got := (Setting{Key: "k", Float: &f}).String(); got != "k = 0.9" {
		t.Fatal(got)
	}
	if got := (Setting{Key: "k", Bool: &b}).String(); got != "k = true" {
		t.Fatal(got)
	}
	if got := (Setting{Key: "k"}).String(); !strings.Contains(got, "unset") {
		t.Fatal(got)
	}
}

func settingValue(s Setting) any {
	switch {
	case s.Int != nil:
		return *s.Int
	case s.Float != nil:
		return *s.Float
	case s.Bool != nil:
		return *s.Bool
	}
	return nil
}
