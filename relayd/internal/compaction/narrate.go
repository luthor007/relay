package compaction

import "github.com/luthor007/relay/relayd/internal/summarize"

// The two lines this package can say. They are constants rather than model
// output for the same reason routing's announcement is: the fact being reported
// is a decision this process just made about a session that exists, and there is
// nothing for a model to add except the opportunity to get it wrong.
const (
	// TidyingLine covers a compaction that has to happen while someone is
	// waiting. MEMORY.md §9: "give me a second, I'm tidying its memory".
	TidyingLine = "One second — tidying this session's memory."

	// MovingLine covers a handoff, which is a different wait and a different
	// promise: the work carries over, the session does not.
	MovingLine = "One second — moving this to a fresh session."
)

// Narrate returns what to say about a decision, and false when there is nothing
// to say.
//
// The rule is the one MEMORY.md §9 states and ORCHESTRATOR.md §3b generalises:
// an idle compaction is silent, because the whole point of doing it on idle is
// that nobody experiences it; a compaction someone is waiting through is
// narrated, because otherwise they experience ten to sixty seconds of nothing
// after speaking, and silence after speech reads as a fault rather than as work.
//
// So false here is not "we had nothing to say". It is the positive assertion
// that this decision costs the user nothing, and speaking about it would be
// worse than not.
func Narrate(d Decision) (summarize.Speech, bool) {
	if !d.Announce || !d.Action.Pauses() {
		return summarize.Speech{}, false
	}

	line := TidyingLine
	if d.Action == ActionHandoff {
		line = MovingLine
	}

	sp := summarize.Speech{
		Moment: summarize.MomentProgress,
		Cap:    summarize.MomentProgress.Cap(),
		Source: summarize.SourceTemplate,
		// Grounded: this is an event we are about to cause, not an inference
		// about what an agent might be doing.
		Grounded: true,
	}
	sp.Text, sp.Truncated = summarize.Fit(line, sp.Cap)
	return sp, true
}
