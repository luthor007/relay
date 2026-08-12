package compaction

import (
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
)

// Action is what to do about a session's context.
type Action string

const (
	// ActionNone: leave it alone. The overwhelming majority of decisions.
	ActionNone Action = "none"

	// ActionFlush is the silent memory pass, stolen from OpenClaw's
	// agents.defaults.compaction.memoryFlush: a turn the user never hears,
	// fired at a soft threshold *below* the compaction threshold, whose job is
	// to get what mattered out of the transcript before anything throws the
	// transcript away. It compacts nothing.
	ActionFlush Action = "flush"

	// ActionCompact: the same work is continuing and the history still matters.
	ActionCompact Action = "compact"

	// ActionNew: the topic changed. Compacting here would drag irrelevant
	// context forward and charge for it on every turn thereafter, forever.
	ActionNew Action = "new"

	// ActionHandoff: the work continues but the session is used up. A fresh
	// session seeded with a [Brief] — what the work is, what was decided, which
	// files, which facts — because a runtime compacting summarises its own
	// transcript with no idea what mattered, and we have the index and the
	// facts. This is the outcome no runtime can produce.
	ActionHandoff Action = "handoff"
)

// Acts reports whether an action does anything at all.
func (a Action) Acts() bool { return a != ActionNone && a != "" }

// Pauses reports whether an action makes the session unavailable for a while.
// A compaction is ten to sixty seconds; a handoff is a brief plus a new
// session. Starting a new session is neither, and neither is a flush, which
// runs only when nobody is waiting.
func (a Action) Pauses() bool { return a == ActionCompact || a == ActionHandoff }

// Trigger is why a decision fired, which decides whether anyone hears about it.
type Trigger string

const (
	TriggerNone Trigger = ""
	// TriggerIdle is the good one: nobody is listening, so the ten to sixty
	// seconds cost nothing in wall-clock terms.
	TriggerIdle Trigger = "idle"
	// TriggerBeforeTurn is the one we did not want: the user has spoken to a
	// session that is nearly full, because the idle pass never happened — the
	// machine was asleep, relayd was restarting, they talked for an hour. It is
	// narrated ([Narrate]) rather than suffered in silence.
	TriggerBeforeTurn Trigger = "before_turn"
)

// Policy is the thresholds. Every number here is a starting point rather than a
// measurement, and the two that are not negotiable are the *shape*: idle before
// threshold, and never during a turn.
type Policy struct {
	// IdleAt is the pressure at which an idle session is compacted. MEMORY.md
	// §9's ~70%, and the whole inversion: a session that is nearly full when you
	// next speak to it has already lost.
	IdleAt float64

	// FlushAt is the soft threshold for the silent memory pass, below IdleAt —
	// the same relationship OpenClaw's memoryFlush has to its own compaction
	// threshold.
	FlushAt float64

	// ReactiveAt is the only pressure at which this package will pause a session
	// while someone is waiting, and it sits below every runtime's own ~95%
	// trigger on purpose: if it is going to happen mid-conversation anyway, it
	// should happen where we can narrate it rather than where the runtime does
	// it silently.
	ReactiveAt float64

	// MinIdle is how quiet a session must be to count as idle.
	//
	// Both directions of this are a real cost and there is no free setting. Too
	// short and the idle pass collides with someone coming back to think out
	// loud, which is the silence this package exists to prevent. Too long and we
	// never get a window at all, and the runtime's own trigger fires at 95%
	// instead — which is exactly why MEMORY.md §9 says to leave that trigger
	// switched on.
	MinIdle time.Duration

	// Cooldown is the floor between two compactions of one session.
	Cooldown time.Duration

	// FlushCooldown is the floor between two silent memory passes.
	FlushCooldown time.Duration

	// MaxCompactions is how many times a session is compacted before a handoff
	// is the better answer. Each pass summarises a summary, and the brief we
	// write from the index is better targeted than the third generation of a
	// runtime summarising itself.
	MaxCompactions int

	// ExhaustionWindow is how quickly pressure has to return to the threshold
	// after a compaction for that compaction to count as not having bought any
	// room. Inside it, the answer is a handoff rather than another pass.
	ExhaustionWindow time.Duration

	// TurnBudget is the degraded rule for a session whose pressure cannot be
	// measured at all — three of the five runtimes report no tokens anywhere in
	// their protocol. MEMORY.md §9: degrade to "compact on idle after N turns"
	// rather than silently never compacting. There is no measurement behind this
	// number; there is only the requirement that it not be infinity.
	TurnBudget int

	Thresholds Thresholds
}

// DefaultPolicy is MEMORY.md §9 as numbers.
func DefaultPolicy() Policy {
	return Policy{
		IdleAt:           0.70,
		FlushAt:          0.55,
		ReactiveAt:       0.90,
		MinIdle:          3 * time.Minute,
		Cooldown:         10 * time.Minute,
		FlushCooldown:    30 * time.Minute,
		MaxCompactions:   2,
		ExhaustionWindow: time.Hour,
		TurnBudget:       30,
		Thresholds:       DefaultThresholds(),
	}
}

func (p Policy) withDefaults() Policy {
	d := DefaultPolicy()
	if p.IdleAt <= 0 {
		p.IdleAt = d.IdleAt
	}
	if p.FlushAt <= 0 {
		p.FlushAt = d.FlushAt
	}
	if p.ReactiveAt <= 0 {
		p.ReactiveAt = d.ReactiveAt
	}
	if p.MinIdle <= 0 {
		p.MinIdle = d.MinIdle
	}
	if p.Cooldown <= 0 {
		p.Cooldown = d.Cooldown
	}
	if p.FlushCooldown <= 0 {
		p.FlushCooldown = d.FlushCooldown
	}
	if p.MaxCompactions <= 0 {
		p.MaxCompactions = d.MaxCompactions
	}
	if p.ExhaustionWindow <= 0 {
		p.ExhaustionWindow = d.ExhaustionWindow
	}
	if p.TurnBudget <= 0 {
		p.TurnBudget = d.TurnBudget
	}
	p.Thresholds = p.Thresholds.withDefaults()

	// The soft pass must stay below the threshold it is protecting, and the
	// reactive ceiling must stay above it. A policy file that inverted either
	// would turn the flush into a second compaction trigger, or make every
	// idle pass a mid-turn one.
	if p.FlushAt >= p.IdleAt {
		p.FlushAt = d.FlushAt * (p.IdleAt / d.IdleAt)
	}
	if p.ReactiveAt < p.IdleAt {
		p.ReactiveAt = p.IdleAt
	}
	return p
}

// FlushTurns is the degraded flush trigger: the turn count that stands in for
// FlushAt when there is no pressure to measure, scaled the same way FlushAt is
// scaled against IdleAt.
func (p Policy) FlushTurns() int {
	p = p.withDefaults()
	n := int(float64(p.TurnBudget) * (p.FlushAt / p.IdleAt))
	if n < 1 {
		n = 1
	}
	return n
}

// State is everything [Decide] looks at. It carries elapsed durations rather
// than timestamps so the decision is a pure function of its inputs and the
// clock lives in [Sweeper].
type State struct {
	Session string
	Runtime adapter.Runtime

	Reading Reading
	Signals Signals

	// Idle is how long since anything happened in this session.
	Idle time.Duration

	// InTurn is set while a turn is running. Nothing this package does is
	// allowed to happen then.
	InTurn bool

	// Compactions is how many times this session has been compacted, by us or
	// by the runtime itself — both count, because both consume the same
	// fidelity.
	Compactions int
	// SinceCompaction is meaningful only when Compactions > 0.
	SinceCompaction time.Duration
	// SinceFlush is meaningful only when Flushes > 0.
	Flushes    int
	SinceFlush time.Duration

	// CompactUnavailable is why this session cannot be compacted right now —
	// no drivable mechanism on this runtime, a capability the adapter reports
	// as absent, a compression lease somebody else is holding. Empty means it
	// can. A session we cannot compact is not a session with no options: the
	// handoff is ours to do regardless of what the runtime supports.
	CompactUnavailable string
}

// Decision is one answer, with the reason attached. The reason is a sentence
// rather than a code because it is what the console shows and what the small
// model is handed when it has to explain a pause.
type Decision struct {
	Session string
	Runtime adapter.Runtime

	Action  Action
	Trigger Trigger
	Reason  string

	// Pressure and Known are the reading this was decided on. Known false with
	// an action set means the turn-count fallback fired, and Degraded says so.
	Pressure float64
	Known    bool
	// Estimated is true when the denominator came from the fallback table
	// rather than from the runtime.
	Estimated bool
	Degraded  string

	// Silent is set for an idle decision the user must never hear about.
	// Announce is set for one they must — they are exclusive, and exactly one
	// of them is set on any action that pauses the session.
	Silent   bool
	Announce bool

	// NeedsBrief is set on a handoff: the action cannot be carried out without
	// one, and a handoff with nothing to carry degrades to a compaction.
	NeedsBrief bool
}

// Acts is shorthand for "this decision does something".
func (d Decision) Acts() bool { return d.Action.Acts() }

// Decide is the whole policy. It reads nothing and writes nothing.
func Decide(st State, p Policy) Decision {
	p = p.withDefaults()

	d := Decision{
		Session: st.Session,
		Runtime: st.Runtime,
		Action:  ActionNone,
	}
	if pr, ok := st.Reading.Pressure(); ok {
		d.Pressure, d.Known = pr, true
	}
	d.Estimated = st.Reading.Estimated()
	d.Degraded = st.Reading.Degraded()

	// 1. Never during a turn. This outranks pressure, exhaustion and anything
	// the user said, because a compaction queued behind a live turn is the ten
	// to sixty seconds of silence this whole section exists to prevent — it just
	// arrives after the answer instead of before it.
	if st.InTurn {
		d.Reason = "a turn is running; this waits for silence"
		return d
	}

	idle := st.Idle >= p.MinIdle
	atThreshold := false

	switch {
	case d.Known:
		atThreshold = d.Pressure >= p.IdleAt
	case st.Reading.Turns >= p.TurnBudget:
		// The degraded path. There is no denominator, so "70%" has nothing to be
		// 70% of, and the rule becomes turns.
		atThreshold = true
		if d.Degraded == "" {
			d.Degraded = "no context window; falling back to turns"
		}
	default:
		if idle && st.Reading.Turns >= p.FlushTurns() && flushDue(st, p) {
			d.Action, d.Trigger, d.Silent = ActionFlush, TriggerIdle, true
			d.Reason = "quiet, and " + itoa(st.Reading.Turns) + " turns in with no window to measure"
			return d
		}
		d.Reason = "nothing to do"
		return d
	}

	// 2. Below the threshold: the only thing on offer is the silent pass, and
	// only when nobody is listening.
	if !atThreshold {
		if idle && d.Known && d.Pressure >= p.FlushAt && flushDue(st, p) {
			d.Action, d.Trigger, d.Silent = ActionFlush, TriggerIdle, true
			d.Reason = "quiet, and " + percent(d.Pressure) + " full — writing down what matters before anything is thrown away"
			return d
		}
		d.Reason = "nothing to do"
		return d
	}

	// 3. At the threshold. Idle is the case this package is built for; the
	// alternative is that we already missed the window.
	switch {
	case idle:
		d.Trigger, d.Silent = TriggerIdle, true
	case !d.Known:
		// Never pause a session someone is waiting on because of a turn count.
		// The runtime's own auto-compaction — which MEMORY.md §9 says to leave
		// switched on precisely for this — is a better answer than a guess.
		d.Reason = "someone is waiting and there is no measurement to justify a pause"
		return d
	case d.Pressure < p.ReactiveAt:
		d.Reason = "someone is waiting and it is only " + percent(d.Pressure) + " full; it can wait for a quiet moment"
		return d
	default:
		d.Trigger, d.Announce = TriggerBeforeTurn, true
	}

	// 4. Which of the three outcomes.
	newTopic, why := st.Signals.NewTopic(p.Thresholds)
	switch {
	case newTopic:
		d.Action = ActionNew
		d.Reason = why
		// Starting a new session is instant, so nobody needs to be warned about
		// a pause that is not going to happen. Routing announces the new session
		// itself (ORCHESTRATOR.md §4).
		d.Announce = false
		return d

	case st.CompactUnavailable != "":
		d.Action, d.NeedsBrief = ActionHandoff, true
		d.Reason = st.CompactUnavailable + "; handing off instead"
		return d

	case st.Compactions >= p.MaxCompactions:
		d.Action, d.NeedsBrief = ActionHandoff, true
		d.Reason = "compacted " + itoa(st.Compactions) + " times already; a fresh session with a brief is better than a summary of a summary"
		return d

	case st.Compactions > 0 && st.SinceCompaction < p.ExhaustionWindow:
		d.Action, d.NeedsBrief = ActionHandoff, true
		d.Reason = "back to " + percent(d.Pressure) + " " + humanDuration(st.SinceCompaction) + " after the last compaction; it is not buying room any more"
		return d

	case st.Compactions > 0 && st.SinceCompaction < p.Cooldown:
		d.Action = ActionNone
		d.Trigger, d.Silent, d.Announce = TriggerNone, false, false
		d.Reason = "compacted " + humanDuration(st.SinceCompaction) + " ago"
		return d
	}

	d.Action = ActionCompact
	d.Reason = percent(d.Pressure) + " full and " + why
	if !d.Known {
		d.Reason = itoa(st.Reading.Turns) + " turns in and " + why
	}
	return d
}

func flushDue(st State, p Policy) bool {
	return st.Flushes == 0 || st.SinceFlush >= p.FlushCooldown
}

// percent renders a fraction the way a person says it.
func percent(f float64) string {
	n := int(f*100 + 0.5)
	if n < 0 {
		n = 0
	}
	return itoa(n) + "%"
}
