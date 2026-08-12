// Package summarize turns sessions and turns into the short text that gets
// indexed, and turns a turn's events into the two sentences someone hears.
//
// Two jobs, one package, because they share the same refusal: neither of them
// reads the transcript when there is something structured to read instead.
//
// # 1. What gets embedded (MEMORY.md §3)
//
// Summaries. Not transcripts. The instinct is to vectorise all 3.6 GB of
// history and it is wrong — ~875,000 raw chunks and ~2.7 GB of vectors become
// ~22,000 chunks and ~68 MB — but cost is the smaller half of the argument. A
// coding transcript is mostly command output and patch hunks, and embedding
// that buries the two sentences that actually said what the session was for.
// Summarising first is both cheaper and more accurate, which is rare enough to
// take.
//
// Two runtimes have already done the first half of this job: Claude Code writes
// aiTitle and Hermes writes title, both per session. [Title] takes those rather
// than paying a model to re-derive them, which across the measured corpus is
// most of the sessions.
//
// # 2. What gets spoken (ADAPTERS.md §6)
//
// Three rules, all enforced here in code rather than only asked for in a prompt:
//
//   - Summarise events, not the transcript. [Digest] is built from the
//     normalized event stream — which tools ran, against what, what the plan
//     was, whether it succeeded. It deliberately holds no tool output at all,
//     only byte counts, so a stack trace cannot reach a prompt or an ear
//     through this path.
//   - Budget by seconds. Speech is ~14 characters a second, so the caps are
//     ~40 characters for an acknowledgement, ~90 mid-task, ~160 for a completed
//     turn and ~120 plus options for a question. [Fit] enforces them; a model
//     that ignores the instruction gets clipped.
//   - Lead with the outcome. A listener walking down a street retains the first
//     clause and little else. The prompt requires outcome-first phrasing and
//     forbids preamble, and [HasPreamble] checks the answer: output that opens
//     with "I've finished working on…" is rejected and the deterministic
//     template speaks instead. Rejecting is better than rewriting — editing
//     model prose to remove a preamble is prose-parsing, which is the thing
//     SYSTEM.md tells us is the wrong path.
//
// # Detection before indexing
//
// [Scanner] runs over every piece of text before it reaches a model or the
// index, never after. That ordering is not a preference: an embedded key cannot
// be unembedded. It is enforced structurally — [New] refuses to build a
// Summarizer without a scanner, so there is no code path that writes a summary
// without having looked first.
package summarize
