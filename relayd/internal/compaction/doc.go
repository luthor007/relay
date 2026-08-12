// Package compaction is MEMORY.md §9: when a session is compacted, when it is
// abandoned for a fresh one, and when it is handed off with a brief.
//
// The package is named compaction rather than context because every file in it
// takes a context.Context, and a package that shadows the standard library's
// most-imported name in its own signatures is a small cruelty repeated forever.
//
// # Why the orchestrator owns this at all
//
// All five runtimes already solve it, alone and blindly: each one watches its
// own window, summarises its own transcript and knows nothing about the other
// four or about what the user actually came back to. We are the only component
// that can see across all of them, and the only one holding the index
// (MEMORY.md §3) and the facts (§5). That is the whole argument for a third
// outcome — [ActionHandoff] — that no runtime can produce.
//
// # The policy inverts the usual one, and the reason is the product
//
// Compaction is ten to sixty seconds of silence. Fired at the threshold it
// fires mid-conversation, and the user hears nothing after speaking. On a
// screen that is a progress bar; in your ear it is a broken product. So:
//
//  1. Compact on idle at ~70% ([Policy.IdleAt]), never reactively at 95%.
//     Idle compaction is free in wall-clock terms, which is the rare case where
//     the careful thing and the fast thing are the same thing.
//  2. Never while a turn is running. [Decide] returns [ActionNone] for a
//     session with InTurn set, whatever the pressure, because a compaction
//     queued behind a live turn *is* the silence this package exists to
//     prevent.
//  3. If it has to happen while someone is waiting — we missed the idle window
//     because the machine was asleep or relayd was restarting — [Narrate] says
//     so. That is ORCHESTRATOR.md §3b's grounding rule applied to an event the
//     user would otherwise experience as a fault.
//
// # Do not disable any runtime's own auto-compaction
//
// An earlier draft took the decision away from them. Probing showed that is the
// dangerous move: if we compact at 70% on idle their trigger never fires
// anyway, so leaving it on costs nothing and buys a safety net for the pass
// that did not happen. Disabling it is what breaks things, and in two runtimes
// the failure is explicit in the source — OpenCode sets finish: "error" and
// idles the session, Hermes returns compaction_disabled: true and its own
// config comment says to set it false if you "want errors on overflow".
//
// [Refuse] is that rule as a chokepoint rather than a paragraph: a config
// writer calls it before touching any of the five runtimes' files, and there is
// no exported path in this package that produces a setting which disables
// auto-compaction. The single most dangerous write in the whole survey —
// raising Codex's model_auto_compact_token_limit to or above
// model_context_window, which converts a graceful pause into the terminal
// ContextWindowExceeded "start a new thread" — is refused by [Refuse] and
// clamped by [CodexAutoCompactLimit].
//
// # Measuring the pressure, and the trap in it
//
// Context size arrives free in the turn events (ADAPTERS.md §5), but only from
// the right field. Claude Code's result.usage *sums the requests a turn took*,
// so a turn with eight tool calls overstates pressure eightfold and fires
// compaction on a nearly empty session. [Observation.Kind] makes that
// structural: a reading built from a per-turn usage is refused with
// [ErrTurnUsage], and [FromTurnUsage] exists only to say so out loud. The two
// legitimate numerators are the latest request's usage (Claude Code
// message_start) and the thread running total (Codex tokenUsage.total).
//
// Denominators are worse. Codex's modelContextWindow is nullable and ACP has no
// token field anywhere in its protocol, so [Reading] degrades in two visible
// steps: a fallback window from [Windows] if the installer supplied one, and
// otherwise "compact on idle after N turns" with [Reading.Degraded] carrying
// the reason. Never a zero, never a silent never-compact.
//
// # Three outcomes, and how they are chosen
//
// [ActionCompact] when the same work continues and the history still matters.
// [ActionNew] when the topic changed, because compaction drags irrelevant
// context forward and you pay for it on every turn thereafter, forever.
// [ActionHandoff] when the work continues but the session is exhausted.
//
// The choice comes from machinery that already exists rather than a new
// classifier ([Signals]): topic drift as cosine distance over the embeddings
// internal/search already computes, working-directory change, time gap, and the
// user simply saying so.
//
// # Precedent
//
// OpenClaw ships this pattern already — agents.defaults.compaction.memoryFlush
// runs a silent agent turn at a soft threshold below the compaction threshold,
// with a NO_REPLY convention so the user never sees it. [FlushTurn] is the same
// shape: at [Policy.FlushAt], on idle, we ask the session to write down what
// mattered *before* compaction throws the transcript away, and the answer
// becomes brief material rather than speech. The reply is parsed as labelled
// lines and nothing else — a prompt this package wrote, in a format this
// package chose — because SYSTEM.md's standing rule is that if you find
// yourself parsing prose you are on the wrong path.
//
// # Seams
//
// Nothing here launches a process, opens a socket or writes a config file.
// [Sessions], [Actor], [Briefs] and [Lease] are interfaces; [Sweeper] is the
// only thing that puts them together, and it is driven by a clock the caller
// owns.
package compaction
