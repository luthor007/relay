// Package routing answers two different questions and refuses to conflate them.
//
// # 1. Which session? (ORCHESTRATOR.md §4)
//
// "Does this utterance belong to an existing session or a new one?" is a
// classification problem with two bad failure modes. A **wrong continue** drops
// a new question into an unrelated session and poisons its context. A **wrong
// new** starts fresh with none of the history and answers confidently without
// it. Neither is recoverable by the model that made the mistake.
//
// Three things make it survivable and all three ship before anything clever:
//
//  1. An explicit escape hatch — [ParseCommand]. "New session." "Talk to the
//     refactor one." Cheap, always correct, and the first thing a user reaches
//     for once routing has surprised them.
//  2. Saying what it chose, before acting — [Decision.Announcement], built by
//     [Announce] from the decision itself and never by a model. SYSTEM.md §7b
//     notes this doubles as the acknowledgement that fills the silence while
//     the big model thinks.
//  3. Undo — [Router.Undo] moves the last turn to a different session, and
//     cancels the turn it landed in where the runtime can be cancelled.
//
// The automatic router — recency, repo and file overlap, entity match, an LLM
// tie-break — exists in [Scorer], and it is **off by default**. Options.Auto
// turns it on. That ordering is the doc's, not a convenience: a router that is
// right 80% of the time and silent about it is worse than one that asks, so the
// shipping default is the manual path plus the announcement, and [KindAsk] is
// a first-class outcome rather than a failure.
//
// # 2. Which runtime? (MEMORY.md §8)
//
// [RuntimeRouter] is the second question, and it is not a small optimisation.
// For a heavy user it is the difference between a subscription they already pay
// for and a metered bill they did not expect, and it is invisible to them
// unless we get it right. The entitlement table is [Table]; the priority order
// is continuity, explicit preference, entitlement, capability, load, applied in
// that order by [RuntimeRouter.Choose].
//
// One rule outranks the scoring: **never route to a runtime with no history and
// no explicit preference.** MEMORY.md §1 found two installed-but-unused
// runtimes on a real machine, and sending someone's first voice command to the
// tool they have never opened is a bad first impression dressed up as load
// balancing. [HistoryUnknown] counts as no history, because a runtime nobody
// has looked at is exactly the one this rule is about.
//
// Same guardrails as question 1: the choice is announced, it can be undone, and
// an explicit "use Codex" always wins.
//
// # 3. What the small model may answer alone (ORCHESTRATOR.md §3b)
//
// [Classify] is the escalation allowlist, as data. It starts almost empty —
// status, control, memory lookups, acknowledgement — and everything else goes
// up to the big model. The bias is deliberate and asymmetric: escalating
// unnecessarily costs a few cents, self-answering wrongly costs trust. So the
// default is [ClassEscalate] and [Veto] overrides a matching allowlist row
// whenever the utterance touches a repo, a tool or a decision.
//
// # 4. Narration that cannot drift (ORCHESTRATOR.md §3b, ADAPTERS.md §5)
//
// Narration is a rephrasing of structured events the big run emits, never a
// guess. That is a plumbing problem rather than a prompt-engineering one, so
// [Narration] is built so drift is impossible by construction: the only way in
// is [Narration.Observe], which takes an [event.Event] and nothing else. There
// is no exported path that accepts a digest, a summary or a string. Given no
// events it says "still working" or says nothing, and it will not claim a turn
// completed until it has seen the event that says so.
//
// # Seams
//
// Everything the router reads is an interface with a static implementation for
// tests: [Sessions] for the live list, [Preferences] for the facts tier,
// [Driver] for moving a turn. [FromRegistry] and [FromDetect] wire the real
// ones. Nothing in this package launches a process or opens a socket.
package routing
