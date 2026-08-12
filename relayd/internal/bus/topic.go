package bus

import (
	"log/slog"
	"sync"
	"sync/atomic"
)

// Topic is a fan-out broadcaster with a bounded, per-subscriber queue.
//
// It exists because every fan-out in relayd has the same three requirements and
// getting any of them wrong is a production bug rather than a style choice:
//
//   - A slow subscriber must never block the publisher. The publisher here is a
//     runtime's event stream, and blocking it stalls an agent.
//   - Losing data must be visible. Every drop is counted and the first one per
//     subscriber is logged, so "the console missed an event" is a number
//     somebody can read rather than a mystery.
//   - Some values must not be dropped at all. [TopicOptions.Droppable] decides,
//     and a subscriber whose queue is full of undroppable values grows instead
//     of losing them.
//
// Delivery is per-subscriber FIFO. Order across subscribers is not coordinated
// and nothing here promises it.
type Topic[T any] struct {
	opts TopicOptions[T]

	mu     sync.RWMutex
	subs   map[uint64]*sub[T]
	nextID uint64
	closed bool

	published atomic.Uint64
}

// TopicOptions configures a topic.
type TopicOptions[T any] struct {
	// Buffer is the per-subscriber queue depth. Default 256.
	Buffer int

	// Droppable reports whether a value may be discarded to keep a slow
	// subscriber from blocking the publisher. Nil means everything may be
	// dropped, which is only correct for topics where every value is a
	// snapshot rather than a transition.
	Droppable func(T) bool

	Log *slog.Logger
}

// NewTopic builds a topic.
func NewTopic[T any](o TopicOptions[T]) *Topic[T] {
	if o.Buffer <= 0 {
		o.Buffer = 256
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Topic[T]{opts: o, subs: map[uint64]*sub[T]{}}
}

// Publish delivers a value to every matching subscriber. It never blocks.
func (t *Topic[T]) Publish(v T) {
	t.mu.RLock()
	if t.closed {
		t.mu.RUnlock()
		return
	}
	// Copy under the read lock so a subscriber closing mid-publish cannot race
	// the iteration.
	subs := make([]*sub[T], 0, len(t.subs))
	for _, s := range t.subs {
		subs = append(subs, s)
	}
	t.mu.RUnlock()

	t.published.Add(1)
	for _, s := range subs {
		s.push(v)
	}
}

// Published is how many values have been published since the topic opened.
func (t *Topic[T]) Published() uint64 { return t.published.Load() }

// Subscribe returns a subscription that receives everything.
func (t *Topic[T]) Subscribe(name string) *Sub[T] { return t.SubscribeFunc(name, nil) }

// SubscribeFunc returns a subscription filtered by match. A nil match receives
// everything. Filtering happens on the publishing goroutine, which is what
// keeps a narrowly-scoped subscriber from paying for traffic it does not want.
func (t *Topic[T]) SubscribeFunc(name string, match func(T) bool) *Sub[T] {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.nextID++
	s := &sub[T]{
		id:      t.nextID,
		name:    name,
		topic:   t,
		match:   match,
		cap:     t.opts.Buffer,
		ch:      make(chan T),
		notify:  make(chan struct{}, 1),
		done:    make(chan struct{}),
		log:     t.opts.Log,
		droppab: t.opts.Droppable,
	}
	if t.closed {
		// A subscription taken after Close is immediately closed, so a caller
		// racing shutdown reads a closed channel rather than hanging.
		close(s.done)
		go func() { close(s.ch) }()
		return &Sub[T]{s: s}
	}
	t.subs[s.id] = s
	go s.run()
	return &Sub[T]{s: s}
}

// Subscribers is how many subscriptions are open.
func (t *Topic[T]) Subscribers() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.subs)
}

// Close closes the topic and every subscription on it. Subscribers see their
// channel close after draining what was already queued.
func (t *Topic[T]) Close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	subs := make([]*sub[T], 0, len(t.subs))
	for _, s := range t.subs {
		subs = append(subs, s)
	}
	t.subs = map[uint64]*sub[T]{}
	t.mu.Unlock()

	for _, s := range subs {
		s.close()
	}
}

func (t *Topic[T]) remove(id uint64) {
	t.mu.Lock()
	delete(t.subs, id)
	t.mu.Unlock()
}

// sub is one subscriber's queue plus the goroutine that drains it.
type sub[T any] struct {
	id      uint64
	name    string
	topic   *Topic[T]
	match   func(T) bool
	droppab func(T) bool
	cap     int
	log     *slog.Logger

	ch     chan T
	notify chan struct{}
	done   chan struct{}

	mu      sync.Mutex
	q       []T
	closed  bool
	dropped uint64
	high    int
	warned  bool
}

func (s *sub[T]) push(v T) {
	if s.match != nil && !s.match(v) {
		return
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.q = append(s.q, v)
	if len(s.q) > s.cap {
		// Drop the OLDEST droppable value, not the newest. On this bus the
		// droppable kinds are streaming chatter — text deltas feeding TTS,
		// reasoning, tool output — and a stale delta is worth less than a fresh
		// one. If nothing in the queue may be dropped the queue grows; losing a
		// NeedsInput to a buffer limit would hang an agent session.
		idx := -1
		for i := range s.q {
			if s.droppab == nil || s.droppab(s.q[i]) {
				idx = i
				break
			}
		}
		if idx >= 0 {
			s.q = append(s.q[:idx], s.q[idx+1:]...)
			s.dropped++
			if !s.warned {
				s.warned = true
				s.log.Warn("bus: subscriber is behind, dropping events",
					"subscriber", s.name, "queue", s.cap)
			}
		}
	}
	if len(s.q) > s.high {
		s.high = len(s.q)
	}
	s.mu.Unlock()

	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *sub[T]) run() {
	defer close(s.ch)
	for {
		select {
		case <-s.done:
			return
		case <-s.notify:
		}
		for {
			s.mu.Lock()
			if len(s.q) == 0 {
				s.mu.Unlock()
				break
			}
			v := s.q[0]
			s.q = s.q[1:]
			s.mu.Unlock()

			select {
			case s.ch <- v:
			case <-s.done:
				return
			}
		}
	}
}

func (s *sub[T]) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	close(s.done)
}

// Sub is a handle on one subscription.
type Sub[T any] struct{ s *sub[T] }

// C is the value stream. It is closed when the subscription or the topic is.
func (h *Sub[T]) C() <-chan T { return h.s.ch }

// Name identifies the subscriber in logs and in the health endpoint.
func (h *Sub[T]) Name() string { return h.s.name }

// Dropped is how many values this subscription lost to backpressure. Anything
// above zero is a real gap and the console shows it.
func (h *Sub[T]) Dropped() uint64 {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	return h.s.dropped
}

// Backlog is the current queue depth, and HighWater the deepest it has been.
func (h *Sub[T]) Backlog() int {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	return len(h.s.q)
}

// HighWater is the deepest this subscription's queue has been.
func (h *Sub[T]) HighWater() int {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	return h.s.high
}

// Close unsubscribes. It is idempotent and safe to call from any goroutine.
func (h *Sub[T]) Close() {
	h.s.topic.remove(h.s.id)
	h.s.close()
}
