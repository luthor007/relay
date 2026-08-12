package codex

import (
	"sync"
	"time"

	"github.com/luthor007/relay/relayd/internal/event"
)

// queue decouples the single reader goroutine from whoever is consuming
// normalized events.
//
// adapter.Session's contract says the implementation "must not block on it
// forever — a consumer that stops reading is a bug in the consumer, but a
// deadlocked runtime is a bug in the adapter". Writing a bounded channel
// directly from the reader would let a slow consumer stall Codex, including
// stalling the answers to approval requests, which is exactly the deadlock the
// contract forbids. So pushes are unbounded and one pump goroutine does the
// blocking instead.
type queue struct {
	mu      sync.Mutex
	items   []event.Event
	closed  bool
	dropped int

	signal  chan struct{} // buffered 1: "there may be work, or we may be done"
	out     chan event.Event
	drained chan struct{}

	hardOnce sync.Once
	hard     chan struct{}
}

func newQueue() *queue {
	q := &queue{
		signal:  make(chan struct{}, 1),
		out:     make(chan event.Event),
		drained: make(chan struct{}),
		hard:    make(chan struct{}),
	}
	go q.pump()
	return q
}

// push never blocks.
func (q *queue) push(e event.Event) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.items = append(q.items, e)
	q.mu.Unlock()
	q.wake()
}

// close marks the end of the stream. The pump drains what is already queued and
// then closes the channel, so a consumer still sees the final TurnCompleted.
func (q *queue) close() {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	q.mu.Unlock()
	q.wake()
}

// abandon gives up on a consumer that is not reading. Whatever is queued is
// dropped and the channel closes anyway: Events() promises to close exactly
// once, and one leaked goroutine per dead session is worse than a truncated
// tail nobody was reading.
func (q *queue) abandon() {
	q.hardOnce.Do(func() { close(q.hard) })
}

// closeAndDrain closes the queue and gives the consumer up to grace to take
// what is left before abandoning it. It returns once the channel is closed.
func (q *queue) closeAndDrain(grace time.Duration) {
	q.close()
	if grace > 0 {
		t := time.NewTimer(grace)
		defer t.Stop()
		select {
		case <-q.drained:
			return
		case <-t.C:
		}
	}
	q.abandon()
	<-q.drained
}

func (q *queue) events() <-chan event.Event { return q.out }

// droppedCount is how many events a non-reading consumer never saw. Tests and
// the console both want to know rather than guess.
func (q *queue) droppedCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dropped
}

func (q *queue) wake() {
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

func (q *queue) pump() {
	defer close(q.drained)
	defer close(q.out)
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			e := q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()

			select {
			case q.out <- e:
			case <-q.hard:
				q.mu.Lock()
				q.dropped += len(q.items) + 1
				q.items = nil
				q.mu.Unlock()
				return
			}
			continue
		}
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return
		}
		select {
		case <-q.signal:
		case <-q.hard:
			q.mu.Lock()
			q.dropped += len(q.items)
			q.items = nil
			q.mu.Unlock()
			return
		}
	}
}
