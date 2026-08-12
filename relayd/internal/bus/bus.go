// Package bus is relayd's event bus: fan-in from every adapter's normalized
// event stream, fan-out to everything that needs to watch it.
//
// Five runtimes, three protocols, one list — SYSTEM.md §10 step 2. Each adapter
// session hands out a `<-chan event.Event`; [Bus.Attach] pumps one of those into
// the bus and [Bus.Subscribe] hands the merged stream to the registry, the
// index, the API, and later the narrator. Nothing downstream of here knows which
// runtime an event came from except by reading Meta.Runtime.
//
// Two policies live in this package rather than in its callers, because both are
// behaviour and not plumbing:
//
//   - **Backpressure.** A subscriber that stops reading must not stall an agent,
//     so queues are bounded and overflow is dropped — but only for the kinds
//     where a dropped value is chatter (TextDelta, Reasoning, ToolOutput).
//     Turn boundaries and blocked questions are never dropped, because losing a
//     NeedsInput hangs a session and losing a TurnCompleted loses a state
//     transition. See [Droppable].
//   - **Pings.** ADAPTERS.md §7 says when the user hears from us unprompted, and
//     it is not "on every event". [Pinger] implements it: blocking questions
//     never batch and are never suppressed, completions batch and wait for a gap,
//     quiet hours hold the speech and not the notification.
package bus

import (
	"context"
	"log/slog"

	"github.com/luthor007/relay/relayd/internal/event"
)

// Options configures a bus.
type Options struct {
	// Buffer is the per-subscriber queue depth. Default 256 — a turn's text
	// deltas arrive faster than a TTS consumer drains them.
	Buffer int
	Log    *slog.Logger
}

// Bus is the normalized event stream, merged across every runtime.
type Bus struct {
	t   *Topic[event.Event]
	log *slog.Logger
}

// New builds a bus.
func New(o Options) *Bus {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	return &Bus{
		t: NewTopic(TopicOptions[event.Event]{
			Buffer:    o.Buffer,
			Droppable: Droppable,
			Log:       log,
		}),
		log: log,
	}
}

// Droppable reports whether an event may be discarded to keep a slow subscriber
// from blocking a runtime.
//
// The three droppable kinds are the streaming ones: TextDelta feeds TTS and a
// stale delta is worth less than a fresh one, Reasoning is never spoken at all
// (ADAPTERS.md §5), and ToolOutput is a chunk of a stream whose status arrives
// again on the next update. Everything else is a transition — a turn boundary, a
// tool starting, a plan, a blocked question, an error — and dropping one of
// those makes the registry's idea of a session wrong rather than merely thin.
func Droppable(ev event.Event) bool {
	switch ev.Kind() {
	case event.KindTextDelta, event.KindReasoning, event.KindToolOutput:
		return true
	default:
		return false
	}
}

// Publish puts one event on the bus. It never blocks.
func (b *Bus) Publish(ev event.Event) { b.t.Publish(ev) }

// Published is how many events have crossed the bus.
func (b *Bus) Published() uint64 { return b.t.Published() }

// Subscribers is how many subscriptions are open.
func (b *Bus) Subscribers() int { return b.t.Subscribers() }

// Subscription is a filtered view of the merged event stream.
type Subscription = Sub[event.Event]

// Subscribe opens a subscription. The zero Filter receives everything.
func (b *Bus) Subscribe(name string, f Filter) *Subscription {
	if f.empty() {
		return b.t.Subscribe(name)
	}
	return b.t.SubscribeFunc(name, f.Match)
}

// Close closes the bus and every subscription on it.
func (b *Bus) Close() { b.t.Close() }

// Attach pumps one adapter session's event channel into the bus until the
// channel closes or ctx is done, then returns. It is the fan-in half: one call
// per live session, whichever runtime it belongs to.
//
// It deliberately does not close the bus when a session ends — five runtimes
// share one bus and one of them exiting is not the end of the stream.
func (b *Bus) Attach(ctx context.Context, ch <-chan event.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			b.Publish(ev)
		}
	}
}

// Filter narrows a subscription. Fields combine with AND; the values inside one
// field combine with OR. The zero value matches everything.
type Filter struct {
	// Sessions matches Meta.Session — Relay's id, not the runtime's.
	Sessions []string
	// Runtimes matches Meta.Runtime.
	Runtimes []string
	// Kinds matches the event kind.
	Kinds []event.Kind
	// Pings restricts to events that would reach a user unprompted. It is what
	// the Pinger subscribes with, and it is a filter rather than a separate
	// stream so that the ping policy reads the same events everything else does.
	Pings bool
	// ExcludeReplay drops events an adapter is re-reading rather than watching
	// happen. ACP's session/load replays a whole conversation and Claude Code
	// echoes injected turns; a consumer that counts turns wants this on, and one
	// rebuilding a transcript wants it off.
	ExcludeReplay bool
}

func (f Filter) empty() bool {
	return len(f.Sessions) == 0 && len(f.Runtimes) == 0 && len(f.Kinds) == 0 &&
		!f.Pings && !f.ExcludeReplay
}

// Match reports whether an event passes the filter.
func (f Filter) Match(ev event.Event) bool {
	m := ev.Envelope()
	if f.ExcludeReplay && m.Replay {
		return false
	}
	if f.Pings && ev.Ping() == event.PingNone {
		return false
	}
	if len(f.Sessions) > 0 && !contains(f.Sessions, m.Session) {
		return false
	}
	if len(f.Runtimes) > 0 && !contains(f.Runtimes, m.Runtime) {
		return false
	}
	if len(f.Kinds) > 0 {
		found := false
		for _, k := range f.Kinds {
			if k == ev.Kind() {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}
