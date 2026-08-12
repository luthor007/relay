package compaction

import (
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/summarize"
)

// An idle compaction is silent. Speaking about work the user is not waiting for
// is not a nicety, it is noise about something that cost them nothing.
func TestIdleDecisionsSayNothing(t *testing.T) {
	for _, a := range []Action{ActionCompact, ActionHandoff, ActionFlush, ActionNew, ActionNone} {
		d := Decision{Action: a, Trigger: TriggerIdle, Silent: true}
		if _, ok := Narrate(d); ok {
			t.Fatalf("%s on idle must be silent", a)
		}
	}
}

func TestMidTurnCompactionIsNarrated(t *testing.T) {
	sp, ok := Narrate(Decision{Action: ActionCompact, Trigger: TriggerBeforeTurn, Announce: true})
	if !ok {
		t.Fatal("a compaction someone is waiting through must be spoken")
	}
	if sp.Text != TidyingLine {
		t.Fatalf("text = %q", sp.Text)
	}
	if !sp.Grounded {
		t.Fatal("this is an event we are about to cause, not a guess about an agent")
	}
	if sp.Source != summarize.SourceTemplate {
		t.Fatalf("source = %q: no model is involved in stating our own decision", sp.Source)
	}
	if !sp.WithinCap() {
		t.Fatalf("%q is %d chars, over the %d-char budget", sp.Text, len([]rune(sp.Text)), sp.Cap)
	}
	if sp.Truncated {
		t.Fatal("the line should fit without clipping")
	}
}

func TestHandoffSaysSomethingDifferent(t *testing.T) {
	sp, ok := Narrate(Decision{Action: ActionHandoff, Trigger: TriggerBeforeTurn, Announce: true})
	if !ok {
		t.Fatal("a handoff pauses the session too")
	}
	if sp.Text != MovingLine {
		t.Fatalf("text = %q, want the handoff line", sp.Text)
	}
	if sp.Text == TidyingLine {
		t.Fatal("a handoff and a compaction are different promises")
	}
	if !strings.Contains(sp.Text, "fresh session") {
		t.Fatalf("text = %q", sp.Text)
	}
}

// A new session is instant, so there is nothing to warn about, and routing
// announces it anyway.
func TestNonPausingActionsAreNotNarrated(t *testing.T) {
	for _, a := range []Action{ActionNew, ActionFlush, ActionNone} {
		if _, ok := Narrate(Decision{Action: a, Trigger: TriggerBeforeTurn, Announce: true}); ok {
			t.Fatalf("%s does not pause the session; nothing to say", a)
		}
	}
}

// The end-to-end shape: the policy decides, and the line follows from the
// decision rather than from a caller choosing to speak.
func TestNarrationFollowsTheDecision(t *testing.T) {
	st := idleState(t, 950, 1000)
	st.Idle = 0
	d := Decide(st, Policy{})
	if !d.Announce {
		t.Fatalf("expected an announced decision, got %+v", d)
	}
	sp, ok := Narrate(d)
	if !ok || sp.Text == "" {
		t.Fatal("an announced decision must produce a line")
	}
}
