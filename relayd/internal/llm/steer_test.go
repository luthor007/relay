package llm_test

import (
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/llm"
)

func TestQueueModesDisposeOfWordsDifferently(t *testing.T) {
	for _, tc := range []struct {
		mode    llm.QueueMode
		want    llm.Disposition
		atBound bool // does Boundary drain it into the running turn?
	}{
		{llm.QueueSteer, llm.DispositionSteered, true},
		{llm.QueueFollowup, llm.DispositionQueued, false},
		{llm.QueueCollect, llm.DispositionCollected, false},
		{llm.QueueInterrupt, llm.DispositionInterrupted, false},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			q := llm.NewQueue(tc.mode)

			var cancelled bool
			q.Attach(func() { cancelled = true })

			if got := q.Push("use staging"); got != tc.want {
				t.Errorf("disposition = %q, want %q", got, tc.want)
			}
			if got := len(q.Boundary(t.Context())); (got > 0) != tc.atBound {
				t.Errorf("Boundary drained %d messages; a mode is defined by whether it reaches the running turn", got)
			}
			if cancelled != (tc.mode == llm.QueueInterrupt) {
				t.Errorf("cancelled = %v in mode %q", cancelled, tc.mode)
			}
			// Every disposition has something to say. ORCHESTRATOR.md §4: the
			// choice is announced before it is acted on, and "queued" is a
			// choice the user cannot otherwise observe.
			if strings.TrimSpace(tc.want.Announce()) == "" {
				t.Errorf("%q announces nothing", tc.want)
			}
		})
	}
}

func TestNothingRunningMeansItStartsRatherThanQueues(t *testing.T) {
	q := llm.NewQueue(llm.QueueSteer)
	if got := q.Push("check the CRC"); got != llm.DispositionStarted {
		t.Errorf("disposition = %q, want started", got)
	}
	msgs := q.Drain()
	if len(msgs) != 1 || msgs[0].Text != "check the CRC" {
		t.Fatalf("drain = %+v", msgs)
	}
}

// TestCollectCoalescesABurstAfterTheQuietWindow is the mode that exists for a
// device on your face: thinking out loud is one thought, not five prompts, and
// answering the first sentence before hearing the correction in the third is
// worse than waiting two seconds.
func TestCollectCoalescesABurstAfterTheQuietWindow(t *testing.T) {
	now := time.Unix(1700000000, 0)
	q := llm.NewQueue(llm.QueueCollect)
	q.CollectWindow = time.Second
	q.Now = func() time.Time { return now }
	q.Attach(func() {})

	q.Push("deploy it")
	q.Push("actually, to staging")

	if got := q.Drain(); got != nil {
		t.Errorf("drained %d messages inside the quiet window", len(got))
	}

	now = now.Add(2 * time.Second)
	msgs := q.Drain()
	if len(msgs) != 1 {
		t.Fatalf("a burst became %d turns, want 1", len(msgs))
	}
	if msgs[0].Text != "deploy it actually, to staging" {
		t.Errorf("coalesced text = %q", msgs[0].Text)
	}
	if q.Pending() != 0 {
		t.Errorf("%d left after draining", q.Pending())
	}
}

// TestChangingModeKeepsWhatWasAlreadySaid — the mode is a routing preference,
// not a reason to drop words someone has already spoken.
func TestChangingModeKeepsWhatWasAlreadySaid(t *testing.T) {
	q := llm.NewQueue(llm.QueueFollowup)
	q.Attach(func() {})
	q.Push("and run the tests")

	q.SetMode(llm.QueueSteer)
	if got := q.Boundary(t.Context()); len(got) != 1 || got[0].Text != "and run the tests" {
		t.Errorf("boundary = %+v; the utterance was lost across a mode change", got)
	}
}

func TestDetachEndsTheRun(t *testing.T) {
	q := llm.NewQueue(llm.QueueSteer)
	q.Attach(func() {})
	if !q.Running() {
		t.Fatal("not running after Attach")
	}
	q.Detach()
	if q.Running() {
		t.Fatal("still running after Detach")
	}
	if got := q.Push("next thing"); got != llm.DispositionStarted {
		t.Errorf("disposition = %q after the run ended, want started", got)
	}
}
