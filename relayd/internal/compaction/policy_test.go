package compaction

import (
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
)

func reading(t *testing.T, used, window int64, turns int) Reading {
	t.Helper()
	r, err := FromLatestRequest(adapter.ClaudeCode, "claude-opus-5", used, window, turns, Windows{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func idleState(t *testing.T, used, window int64) State {
	t.Helper()
	return State{
		Session: "s1",
		Runtime: adapter.ClaudeCode,
		Reading: reading(t, used, window, 5),
		Idle:    10 * time.Minute,
	}
}

// The inversion, stated as a test: 70% on idle acts, 95% mid-turn does not.
func TestCompactsOnIdleAtSeventyPercent(t *testing.T) {
	d := Decide(idleState(t, 700, 1000), Policy{})
	if d.Action != ActionCompact {
		t.Fatalf("action = %q (%s), want compact", d.Action, d.Reason)
	}
	if d.Trigger != TriggerIdle {
		t.Fatalf("trigger = %q, want idle", d.Trigger)
	}
	if !d.Silent || d.Announce {
		t.Fatal("an idle compaction is silent — the user must never experience it")
	}
}

func TestNeverDuringATurn(t *testing.T) {
	st := idleState(t, 990, 1000)
	st.InTurn = true
	d := Decide(st, Policy{})
	if d.Action != ActionNone {
		t.Fatalf("action = %q, want none: a compaction queued behind a live turn is the silence itself", d.Action)
	}
	if !strings.Contains(d.Reason, "turn is running") {
		t.Fatalf("reason = %q", d.Reason)
	}
}

// Someone is waiting. 70% is not enough to make them wait ten to sixty seconds.
func TestBeforeTurnWaitsUntilTheReactiveCeiling(t *testing.T) {
	st := idleState(t, 750, 1000)
	st.Idle = 5 * time.Second

	d := Decide(st, Policy{})
	if d.Action != ActionNone {
		t.Fatalf("action = %q (%s), want none at 75%% with a user waiting", d.Action, d.Reason)
	}

	st.Reading = reading(t, 930, 1000, 5)
	d = Decide(st, Policy{})
	if d.Action != ActionCompact {
		t.Fatalf("action = %q (%s), want compact at 93%%", d.Action, d.Reason)
	}
	if d.Trigger != TriggerBeforeTurn {
		t.Fatalf("trigger = %q, want before_turn", d.Trigger)
	}
	if !d.Announce || d.Silent {
		t.Fatal("a compaction someone is waiting through must be narrated")
	}
	if d.Pressure < 0.9 {
		t.Fatalf("the reactive ceiling must sit below every runtime's own ~95%%, got %v", d.Pressure)
	}
	if p := DefaultPolicy(); p.ReactiveAt >= 0.95 {
		t.Fatalf("ReactiveAt = %v: we must get there before the runtime does, or there is nothing to narrate", p.ReactiveAt)
	}
}

func TestSilentFlushBelowTheThreshold(t *testing.T) {
	d := Decide(idleState(t, 600, 1000), Policy{})
	if d.Action != ActionFlush {
		t.Fatalf("action = %q (%s), want flush at 60%%", d.Action, d.Reason)
	}
	if !d.Silent {
		t.Fatal("the memory pass is the one the user never sees")
	}

	// Below the soft threshold there is nothing to do at all.
	if d := Decide(idleState(t, 400, 1000), Policy{}); d.Action != ActionNone {
		t.Fatalf("action = %q at 40%%, want none", d.Action)
	}
}

func TestFlushRespectsItsCooldown(t *testing.T) {
	st := idleState(t, 600, 1000)
	st.Flushes = 1
	st.SinceFlush = time.Minute
	if d := Decide(st, Policy{}); d.Action != ActionNone {
		t.Fatalf("action = %q, want none inside the flush cooldown", d.Action)
	}
	st.SinceFlush = 2 * time.Hour
	if d := Decide(st, Policy{}); d.Action != ActionFlush {
		t.Fatalf("action = %q, want flush once the cooldown has passed", d.Action)
	}
}

// The topic changed: compacting would drag irrelevant context forward and
// charge for it on every turn thereafter, forever.
func TestNewSessionWhenTheTopicChanged(t *testing.T) {
	st := idleState(t, 800, 1000)
	st.Signals = Signals{WorkspaceChanged: true}

	d := Decide(st, Policy{})
	if d.Action != ActionNew {
		t.Fatalf("action = %q (%s), want new", d.Action, d.Reason)
	}
	if d.Announce {
		t.Fatal("starting a new session pauses nothing, so there is nothing to warn about")
	}
	if d.NeedsBrief {
		t.Fatal("a new topic does not carry a brief")
	}
}

func TestUserStatementOutranksEverything(t *testing.T) {
	st := idleState(t, 800, 1000)
	st.Signals = Signals{Stated: StatementNew}
	if d := Decide(st, Policy{}); d.Action != ActionNew {
		t.Fatalf("action = %q, want new when the user said so", d.Action)
	}

	// And the other way: drifted, quiet for a week, but the user said keep going.
	st.Signals = Signals{
		Stated: StatementContinue, Drift: 0.9, DriftKnown: true,
		Gap: 7 * 24 * time.Hour, WorkspaceChanged: true,
	}
	if d := Decide(st, Policy{}); d.Action != ActionCompact {
		t.Fatalf("action = %q, want compact when the user said keep going", d.Action)
	}
}

func TestHandoffWhenTheSessionIsExhausted(t *testing.T) {
	t.Run("compacted too many times", func(t *testing.T) {
		st := idleState(t, 800, 1000)
		st.Compactions = 2
		st.SinceCompaction = 8 * time.Hour
		d := Decide(st, Policy{})
		if d.Action != ActionHandoff {
			t.Fatalf("action = %q (%s), want handoff", d.Action, d.Reason)
		}
		if !d.NeedsBrief {
			t.Fatal("a handoff without a brief is just a lost session")
		}
	})

	t.Run("compaction bought no room", func(t *testing.T) {
		st := idleState(t, 800, 1000)
		st.Compactions = 1
		st.SinceCompaction = 4 * time.Minute
		d := Decide(st, Policy{})
		if d.Action != ActionHandoff {
			t.Fatalf("action = %q (%s), want handoff", d.Action, d.Reason)
		}
		if !strings.Contains(d.Reason, "not buying room") {
			t.Fatalf("reason = %q", d.Reason)
		}
	})

	t.Run("nothing we can drive", func(t *testing.T) {
		st := idleState(t, 800, 1000)
		st.CompactUnavailable = "the compression lease is held"
		d := Decide(st, Policy{})
		if d.Action != ActionHandoff {
			t.Fatalf("action = %q (%s), want handoff — the handoff is ours regardless of the runtime", d.Action, d.Reason)
		}
	})
}

func TestCompactionCooldown(t *testing.T) {
	st := idleState(t, 800, 1000)
	st.Compactions = 1
	// Outside the exhaustion window (so not a handoff) but inside the cooldown.
	st.SinceCompaction = 90 * time.Minute
	d := Decide(st, Policy{Cooldown: 2 * time.Hour, ExhaustionWindow: time.Hour})
	if d.Action != ActionNone {
		t.Fatalf("action = %q (%s), want none inside the cooldown", d.Action, d.Reason)
	}
	if d.Silent || d.Announce {
		t.Fatal("a decision that does nothing announces nothing")
	}
}

// Three of the five runtimes report no tokens at all. "Never compact" is not an
// acceptable answer to that.
func TestDegradesToTurnsWithoutADenominator(t *testing.T) {
	st := State{
		Session: "acp-1",
		Runtime: adapter.OpenCode,
		Reading: Unmeasured(adapter.OpenCode, "", 40),
		Idle:    time.Hour,
	}
	d := Decide(st, Policy{})
	if d.Action != ActionCompact {
		t.Fatalf("action = %q (%s), want compact on the turn budget", d.Action, d.Reason)
	}
	if d.Known {
		t.Fatal("the decision must not claim a pressure it does not have")
	}
	if d.Degraded == "" {
		t.Fatal("acting without a measurement has to be visible")
	}
	if !strings.Contains(d.Reason, "40 turns in") {
		t.Fatalf("reason = %q, want it to say what it acted on", d.Reason)
	}
}

func TestDegradedFlushOnTurns(t *testing.T) {
	p := DefaultPolicy()
	st := State{
		Runtime: adapter.Hermes,
		Reading: Unmeasured(adapter.Hermes, "", p.FlushTurns()),
		Idle:    time.Hour,
	}
	if d := Decide(st, p); d.Action != ActionFlush {
		t.Fatalf("action = %q, want a flush at the scaled turn budget (%d)", d.Action, p.FlushTurns())
	}
	if p.FlushTurns() >= p.TurnBudget {
		t.Fatalf("the flush must fire before the compaction: %d vs %d", p.FlushTurns(), p.TurnBudget)
	}
}

// Never pause a session someone is waiting on because of a turn count. The
// runtime's own auto-compaction — left switched on for exactly this — is a
// better answer than a guess.
func TestNoDegradedActionWhileSomeoneWaits(t *testing.T) {
	st := State{
		Runtime: adapter.OpenClaw,
		Reading: Unmeasured(adapter.OpenClaw, "", 500),
		Idle:    2 * time.Second,
	}
	d := Decide(st, Policy{})
	if d.Action != ActionNone {
		t.Fatalf("action = %q (%s), want none", d.Action, d.Reason)
	}
	if !strings.Contains(d.Reason, "no measurement") {
		t.Fatalf("reason = %q", d.Reason)
	}
}

func TestPolicyDefaultsKeepTheirOrdering(t *testing.T) {
	p := Policy{IdleAt: 0.5, FlushAt: 0.8, ReactiveAt: 0.2}.withDefaults()
	if p.FlushAt >= p.IdleAt {
		t.Fatalf("FlushAt %v must stay below IdleAt %v", p.FlushAt, p.IdleAt)
	}
	if p.ReactiveAt < p.IdleAt {
		t.Fatalf("ReactiveAt %v must stay at or above IdleAt %v", p.ReactiveAt, p.IdleAt)
	}

	d := DefaultPolicy()
	if d.IdleAt != 0.70 {
		t.Fatalf("IdleAt = %v, want MEMORY.md §9's 0.70", d.IdleAt)
	}
	if d.FlushAt >= d.IdleAt {
		t.Fatal("the memory pass must sit below the compaction threshold, as OpenClaw's does")
	}
}

// Whatever else changes, an action that pauses the session is either silent
// (idle) or announced (someone waiting), and never both or neither.
func TestEveryPausingActionIsEitherSilentOrAnnounced(t *testing.T) {
	idles := []time.Duration{time.Second, 30 * time.Minute}
	pressures := []int64{300, 560, 700, 800, 930, 990}
	comps := []int{0, 1, 2}

	for _, idle := range idles {
		for _, used := range pressures {
			for _, c := range comps {
				st := idleState(t, used, 1000)
				st.Idle = idle
				st.Compactions = c
				st.SinceCompaction = 8 * time.Hour
				d := Decide(st, Policy{})
				if !d.Action.Pauses() {
					continue
				}
				if d.Silent == d.Announce {
					t.Fatalf("idle=%v used=%d comp=%d action=%s: silent=%v announce=%v",
						idle, used, c, d.Action, d.Silent, d.Announce)
				}
			}
		}
	}
}
