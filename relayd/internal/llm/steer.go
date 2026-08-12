package llm

import (
	"context"
	"strings"
	"sync"
	"time"
)

// QueueMode is what happens to something you say while a run is already going.
//
// This is the hardest question a voice orchestrator has, and it does not have
// one right answer — "actually, use the staging database" wants to reach the
// run in progress, "and when you're done, deploy it" wants to wait, and "no,
// stop" wants to end it. OpenClaw ships all four as a per-channel setting for
// that reason, and the four below are the same four.
type QueueMode string

const (
	// QueueSteer pushes the words into the running turn at the next model
	// boundary. The default, because it is what people expect from talking to
	// something that is listening.
	QueueSteer QueueMode = "steer"

	// QueueFollowup waits for the run to end and starts a new turn.
	QueueFollowup QueueMode = "followup"

	// QueueCollect waits, and coalesces everything said in the meantime into
	// one turn after a quiet window. For a device on your face this is the
	// mode that survives a burst of thinking-out-loud.
	QueueCollect QueueMode = "collect"

	// QueueInterrupt aborts the run and starts the new words instead. "No,
	// stop" — the one case where waiting for a boundary is the wrong answer,
	// because the boundary might be a minute away.
	QueueInterrupt QueueMode = "interrupt"
)

// Disposition is what the queue did with an utterance, so the router can say
// so out loud.
//
// ORCHESTRATOR.md §4's second rule is that the orchestrator announces its
// choice before acting. Steering is a choice: silently deciding that what you
// just said will be heard in ninety seconds rather than now is exactly the kind
// of thing that reads as broken.
type Disposition string

const (
	// DispositionStarted means nothing was running and this becomes the prompt.
	DispositionStarted Disposition = "started"
	// DispositionSteered means it will reach the running turn at its next
	// model boundary — after the tool batch in flight, before the next call.
	DispositionSteered Disposition = "steered"
	// DispositionQueued means it runs after the current one finishes.
	DispositionQueued Disposition = "queued"
	// DispositionCollected means it is being gathered with others into one
	// later turn.
	DispositionCollected Disposition = "collected"
	// DispositionInterrupted means the running turn was cancelled for it.
	DispositionInterrupted Disposition = "interrupted"
)

// Announce is the one-clause sentence for a disposition, in the register
// ADAPTERS.md §6 budgets for: short enough to speak before the thing it
// describes happens.
func (d Disposition) Announce() string {
	switch d {
	case DispositionSteered:
		return "Adding that to what it's doing."
	case DispositionQueued:
		return "I'll pass that on when it's done."
	case DispositionCollected:
		return "Noted — I'll send that with the rest."
	case DispositionInterrupted:
		return "Stopping."
	default:
		return "On it."
	}
}

// DefaultCollectWindow is how long [QueueCollect] waits for more words before
// deciding a thought is finished. Roughly the length of a breath between
// sentences: long enough not to split "deploy it — actually, to staging", short
// enough not to feel like being ignored.
const DefaultCollectWindow = 2500 * time.Millisecond

// Queue holds what was said while a run was in progress.
//
// It is the [Hooks.Boundary] implementation for [Loop] and the followup source
// for whatever drives the loop. One queue per session: ORCHESTRATOR.md §4 routes
// by session and OpenClaw serialises runs per session lane for the same
// reason — two turns interleaving on one conversation corrupts it.
type Queue struct {
	mu      sync.Mutex
	mode    QueueMode
	pending []string
	cancel  context.CancelFunc
	last    time.Time

	// CollectWindow overrides [DefaultCollectWindow].
	CollectWindow time.Duration
	// Now is injectable so the collect window is testable without sleeping.
	Now func() time.Time
}

// NewQueue builds a queue. An empty mode means [QueueSteer].
func NewQueue(mode QueueMode) *Queue {
	if mode == "" {
		mode = QueueSteer
	}
	return &Queue{mode: mode}
}

// Mode reports the current mode.
func (q *Queue) Mode() QueueMode {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.mode
}

// SetMode changes the mode. Words already queued keep waiting — changing the
// mode is not a reason to lose them.
func (q *Queue) SetMode(mode QueueMode) {
	if mode == "" {
		mode = QueueSteer
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.mode = mode
}

// Attach registers the cancel function of a run that is now in progress. Pass
// nil — or call [Queue.Detach] — when it ends.
func (q *Queue) Attach(cancel context.CancelFunc) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.cancel = cancel
}

// Detach clears the running run.
func (q *Queue) Detach() { q.Attach(nil) }

// Running reports whether a run is attached.
func (q *Queue) Running() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.cancel != nil
}

// Push offers one utterance and reports what happened to it.
func (q *Queue) Push(text string) Disposition {
	text = strings.TrimSpace(text)
	if text == "" {
		return DispositionQueued
	}

	q.mu.Lock()
	q.pending = append(q.pending, text)
	q.last = q.now()
	mode, cancel := q.mode, q.cancel
	q.mu.Unlock()

	if cancel == nil {
		return DispositionStarted
	}
	switch mode {
	case QueueInterrupt:
		// Cancel outside the lock: the run's own goroutine may be in Boundary
		// waiting for it.
		cancel()
		return DispositionInterrupted
	case QueueFollowup:
		return DispositionQueued
	case QueueCollect:
		return DispositionCollected
	default:
		return DispositionSteered
	}
}

// Boundary is the [Hooks.Boundary] implementation.
//
// It drains only in [QueueSteer]. The other three modes are defined by *not*
// reaching the running turn, and a boundary hook that drained them anyway would
// turn every mode into steering with extra steps.
func (q *Queue) Boundary(context.Context) []Message {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.mode != QueueSteer || len(q.pending) == 0 {
		return nil
	}
	return q.take()
}

// Drain returns what is waiting for the next turn, or nil if it is not ready.
//
// In [QueueCollect] "ready" means the quiet window has passed since the last
// thing said, so a burst becomes one turn instead of five.
func (q *Queue) Drain() []Message {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return nil
	}
	if q.mode == QueueCollect {
		window := q.CollectWindow
		if window <= 0 {
			window = DefaultCollectWindow
		}
		if q.now().Sub(q.last) < window {
			return nil
		}
		// One message, in arrival order. Separate turns would make the model
		// answer the first sentence before hearing the correction in the
		// second.
		joined := strings.Join(q.pending, " ")
		q.pending = nil
		return []Message{{Role: RoleUser, Text: joined}}
	}
	return q.take()
}

// Pending reports how many utterances are waiting.
func (q *Queue) Pending() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}

// take drains to one message per utterance, in arrival order. Callers hold the
// lock.
func (q *Queue) take() []Message {
	out := make([]Message, 0, len(q.pending))
	for _, t := range q.pending {
		out = append(out, Message{Role: RoleUser, Text: t})
	}
	q.pending = nil
	return out
}

func (q *Queue) now() time.Time {
	if q.Now != nil {
		return q.Now()
	}
	return time.Now()
}
