package routing

import (
	"context"
	"sync"

	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/summarize"
)

// StillWorking is what the small model says when it has nothing observed to say.
//
// ORCHESTRATOR.md §3b: given no event, the small model says "still working" or
// says nothing — it never invents a specific. A vague true update beats a
// precise invented one.
const StillWorking = "Still working."

// NarrationOptions configures a [Narration].
type NarrationOptions struct {
	// Session is the session this narration speaks for. Events from any other
	// session are dropped, because a digest that mixes two turns narrates
	// neither.
	Session string

	// Model is the small model. Nil is fully supported and produces the
	// deterministic template — a blunter product, not a broken one.
	Model llm.Provider
}

// Narration is the narration surface, built so drift is impossible by
// construction rather than discouraged by a prompt.
//
// ORCHESTRATOR.md §3b names narration drift as one of the two ways the
// two-model split breaks: the small model says "running the tests" while the
// big one is doing something else, and you have built something that lies
// confidently. The rule that prevents it is that narration is a rephrasing of
// structured events the big run emits, never a guess — and the doc is explicit
// that this is a plumbing problem, not a prompt-engineering one.
//
// So this type has exactly one way in: [Narration.Observe], which takes an
// [event.Event]. There is no exported method that accepts a digest, a summary,
// a plan or a string, and the digest it maintains is unexported. A caller who
// wants the narrator to say a specific thing has to make an adapter emit the
// event that says it, which is the point.
//
// Three further refusals, each of which is a lie the type cannot tell:
//
//   - A replayed event is not news. ACP's session/load replays a whole
//     conversation and Claude Code echoes injected turns; narrating either
//     would announce a turn from two weeks ago as if it were happening.
//   - An event from another session is dropped rather than merged.
//   - [Narration.Completed] before a TurnCompleted has been observed does not
//     claim completion. It falls back to the progress line, which is true.
type Narration struct {
	session string
	n       *summarize.Narrator

	mu        sync.Mutex
	g         *summarize.Digester
	observed  int
	dropped   int
	completed bool
}

// NewNarration builds one.
func NewNarration(o NarrationOptions) *Narration {
	return &Narration{
		session: o.Session,
		n:       summarize.NewNarrator(summarize.NarratorOptions{Model: o.Model}),
		g:       summarize.NewDigester(),
	}
}

// Observe folds one event in. It is the only way anything enters this type.
//
// It reports whether the event was accepted, so a caller can log the drop
// rather than wonder why the narration is thin.
func (n *Narration) Observe(ev event.Event) bool {
	if ev == nil {
		return false
	}
	m := ev.Envelope()
	if m.Replay {
		n.drop()
		return false
	}
	if n.session != "" && m.Session != "" && m.Session != n.session {
		n.drop()
		return false
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	n.g.Add(ev)
	n.observed++
	if _, ok := ev.(event.TurnCompleted); ok {
		n.completed = true
	}
	return true
}

func (n *Narration) drop() {
	n.mu.Lock()
	n.dropped++
	n.mu.Unlock()
}

// Observed is how many events this narration has been given. Zero means there
// is nothing honest to say beyond "still working", and every method here acts
// on that rather than filling the gap.
func (n *Narration) Observed() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.observed
}

// Dropped is how many events were refused — replays, and events belonging to
// another session.
func (n *Narration) Dropped() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.dropped
}

// Complete reports whether a TurnCompleted has actually been seen.
func (n *Narration) Complete() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.completed
}

// Announce speaks the routing decision.
//
// This is the one line in the two-model split that is *not* grounded in agent
// events, and it is honest for a different reason: it is grounded in a decision
// this process just made, about a session that exists, phrased by [Announce]
// rather than by a model. It is also the acknowledgement — SYSTEM.md §7b — so
// it is what fills the silence while the big model thinks.
func (n *Narration) Announce(d Decision) summarize.Speech {
	line := d.Announcement
	if line == "" {
		line = Announce(d)
	}
	sp := summarize.Speech{
		Moment:   summarize.MomentAck,
		Cap:      AnnounceCap,
		Source:   summarize.SourceTemplate,
		Grounded: line != "",
	}
	sp.Text, sp.Truncated = summarize.Fit(line, AnnounceCap)
	return sp
}

// Progress is a mid-task update, and it is the method the grounding rule is
// really about: called with nothing observed, it says "still working" and does
// not call the model at all.
func (n *Narration) Progress(ctx context.Context) summarize.Speech {
	d, count := n.snapshot()
	if count == 0 {
		return ungrounded(summarize.MomentProgress)
	}
	return n.n.Progress(ctx, d)
}

// Completed is the turn boundary. Called before a TurnCompleted has been
// observed it narrates progress instead, because "it's done" is a specific and
// there is no event that says so.
func (n *Narration) Completed(ctx context.Context) summarize.Speech {
	d, count := n.snapshot()
	if count == 0 {
		return ungrounded(summarize.MomentProgress)
	}
	if !d.Completed {
		return n.n.Progress(ctx, d)
	}
	return n.n.Completed(ctx, d)
}

// NeedsInput speaks a blocked session's question. With no question observed
// there is nothing to ask, so it degrades to progress rather than inventing a
// prompt.
func (n *Narration) NeedsInput(ctx context.Context) summarize.Speech {
	d, count := n.snapshot()
	if count == 0 || d.Question == nil {
		return ungrounded(summarize.MomentProgress)
	}
	return n.n.NeedsInput(ctx, d)
}

// Reset starts a new turn. The event count goes back to zero, which means the
// first Progress of a new turn is vague again rather than repeating the last
// turn's specifics.
func (n *Narration) Reset() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.g.Reset()
	n.observed = 0
	n.completed = false
}

func (n *Narration) snapshot() (summarize.Digest, int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.g.Digest(), n.observed
}

// ungrounded is the line for "no events". It is a constant rather than a
// template call so there is no path by which it can acquire a specific.
func ungrounded(m summarize.Moment) summarize.Speech {
	return summarize.Speech{
		Moment:   m,
		Cap:      m.Cap(),
		Source:   summarize.SourceTemplate,
		Grounded: false,
		Text:     StillWorking,
	}
}
