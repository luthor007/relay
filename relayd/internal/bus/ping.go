package bus

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/luthor007/relay/relayd/internal/event"
)

// ADAPTERS.md §7's defaults. Every one of them is a behaviour, so they are
// named constants rather than literals buried in a struct initialiser.
const (
	// DefaultBatchWindow is how long completions accumulate before one ping goes
	// out. §7: "Three sessions finishing inside a minute is one ping."
	DefaultBatchWindow = time.Minute

	// DefaultRepingAfter is the single retry on an unheard blocking ping. §7:
	// "If unheard, re-ping once at 2 min, then hold."
	DefaultRepingAfter = 2 * time.Minute

	// DefaultGapTimeout bounds how long an informational ping waits for a gap in
	// the conversation. Past it the notification still goes out; only the speech
	// is dropped, because a completion nobody ever hears about is a completion
	// that did not happen as far as the user is concerned.
	DefaultGapTimeout = 5 * time.Minute

	// DefaultUtteranceTimeout bounds the wait for the current utterance to end
	// before a blocking ping interrupts anyway. Someone who has been talking for
	// half a minute is not going to be interrupted mid-word by this.
	DefaultUtteranceTimeout = 30 * time.Second
)

// Class is how loudly a ping arrives, and it is not a severity — it decides
// batching, repetition and whether quiet hours may hold it.
type Class uint8

const (
	// ClassBlocking is a session waiting on a human. Never batches, may
	// interrupt, is never held by quiet hours, repeats exactly once.
	ClassBlocking Class = iota + 1
	// ClassInformational is a turn that ended. Batches, waits for a gap, silent
	// during quiet hours, never repeats.
	ClassInformational
)

func (c Class) String() string {
	switch c {
	case ClassBlocking:
		return "blocking"
	case ClassInformational:
		return "informational"
	default:
		return "none"
	}
}

// Ping is one thing the user hears from us without having asked.
type Ping struct {
	// ID is stable across the re-ping, so a phone can replace a notification
	// rather than stack a second one, and an answer can name what it answers.
	ID    string
	Class Class
	At    time.Time

	// Repeat is 0 for the first delivery and 1 for the two-minute re-ping.
	Repeat int

	// Sessions are the Relay session ids this ping is about, in the order they
	// arrived.
	Sessions []string

	// Events is what produced it: one NeedsInput for a blocking ping, one or
	// more TurnCompleted/Error for an informational one.
	Events []event.Event

	// Ask is the blocked question, set only on ClassBlocking. It carries its own
	// reply path back into whichever runtime asked (event.NeedsInput.Reply), so
	// a voice answer never needs to know which of the five is on the other end.
	Ask *event.NeedsInput

	// Consequential marks an approval for something with effects outside the
	// machine. ORCHESTRATOR.md §4b: these confirm every time and are not
	// suppressible — not by batching, not by quiet hours. An approval the user
	// never heard is an approval they did not give.
	Consequential bool

	// Gap and Quiet are the two facts this policy established while it waited.
	// Neither says what to do about it — PRODUCT.md §6b: nothing in relayd
	// decides *how* an event surfaces, so there is deliberately no Speak here.
	//
	// Gap is whether the moment the policy waited for actually arrived: the end
	// of the current utterance for a blocking ping, a gap in the conversation
	// for an informational one. False means the wait timed out and we are
	// delivering anyway.
	//
	// Quiet is whether quiet hours apply to this ping. It is a policy fact, not
	// a rendering one: ADAPTERS.md §7 exempts blocking pings from quiet hours
	// entirely, so a blocked session at 3 a.m. carries Quiet false — the hours
	// were consulted and found not to apply, rather than not consulted.
	//
	// A voice backend speaks a blocking ping always, and an informational one
	// when Gap && !Quiet. A display backend in v2 shows both either way and
	// uses Quiet only to withhold the chime. Neither rule lives here.
	Gap   bool
	Quiet bool

	// Line is a plain, outcome-first sentence built only from the structured
	// events above, capped at SpeechCap. It is the honest fallback the small
	// model rephrases; nothing here guesses (ORCHESTRATOR.md §3b).
	//
	// It is the *voice* backend's fallback string and is budgeted in seconds of
	// speech. A second backend re-renders from Events and Ask, which travel
	// alongside it for exactly that reason, rather than reusing an ear-sized
	// sentence on a screen.
	Line string
}

// SpeechCap is ADAPTERS.md §6's turn-completed budget: ~160 characters, which
// is about eleven seconds of speech.
const SpeechCap = 160

// Delivery is where a ping goes: the phone socket, the glasses, a test.
type Delivery interface {
	Deliver(ctx context.Context, p Ping) error

	// Retract withdraws a ping that is no longer true — the blocked question was
	// answered in a terminal, or the turn was cancelled. Codex's
	// serverRequest/resolved is exactly this case, and without it a ping
	// outlives its question and wakes someone to approve what is already
	// approved.
	Retract(ctx context.Context, id, reason string) error
}

// DeliveryFunc adapts a function to [Delivery] with a no-op Retract.
type DeliveryFunc func(ctx context.Context, p Ping) error

func (f DeliveryFunc) Deliver(ctx context.Context, p Ping) error { return f(ctx, p) }
func (f DeliveryFunc) Retract(context.Context, string, string) error {
	return nil
}

// PingOptions configures a [Pinger].
type PingOptions struct {
	Delivery Delivery
	Gate     Gate
	Quiet    QuietHours

	BatchWindow      time.Duration
	RepingAfter      time.Duration
	GapTimeout       time.Duration
	UtteranceTimeout time.Duration

	// Namer turns a session id into something speakable — "payments", "docs".
	// The registry supplies it. Without one the ping says "a session", never a
	// guessed subject.
	Namer func(sessionID string) string

	Now   func() time.Time
	NewID func() string
	Log   *slog.Logger
}

// PingStats is what the health endpoint shows about the ping policy.
type PingStats struct {
	Blocking      uint64
	Informational uint64
	Repings       uint64
	Retracted     uint64
	Withdrawn     uint64
	Batched       uint64 // events folded into a batch beyond the first
	Failed        uint64
}

// Pinger turns the event stream into pings, applying ADAPTERS.md §7.
//
// The whole policy, in one place, because it is the part most likely to be
// re-derived wrongly by someone reading only the code:
//
//	                 | NeedsInput          | TurnCompleted / Error
//	urgency          | blocking            | informational
//	batches          | never               | yes, one window
//	waits for        | end of utterance    | a gap in the conversation
//	quiet hours      | ignored             | speech held, notification silent
//	if unheard       | once more at 2 min  | never repeats
//	suppressible     | no                  | yes
//
// The bottom two rows of the quiet-hours cell are written in one backend's
// verbs because ADAPTERS.md §7 is. What this package emits is the fact — see
// [Ping.Gap] and [Ping.Quiet] — and internal/api.speaks is where the voice
// backend turns "quiet hours apply" into "hold the speech, keep the
// notification". PRODUCT.md §6b: nothing here decides how a ping surfaces.
type Pinger struct {
	opts PingOptions
	log  *slog.Logger

	stats struct {
		blocking, informational, repings, retracted, withdrawn, batched, failed atomic.Uint64
	}

	mu    sync.Mutex
	heard map[string]chan struct{}

	wg sync.WaitGroup
}

// NewPinger builds a pinger. Delivery is required; everything else defaults.
func NewPinger(o PingOptions) *Pinger {
	if o.Gate == nil {
		o.Gate = OpenGate{}
	}
	if o.BatchWindow <= 0 {
		o.BatchWindow = DefaultBatchWindow
	}
	if o.RepingAfter <= 0 {
		o.RepingAfter = DefaultRepingAfter
	}
	if o.GapTimeout <= 0 {
		o.GapTimeout = DefaultGapTimeout
	}
	if o.UtteranceTimeout <= 0 {
		o.UtteranceTimeout = DefaultUtteranceTimeout
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.NewID == nil {
		o.NewID = uuid.NewString
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Pinger{opts: o, log: o.Log, heard: map[string]chan struct{}{}}
}

// Stats is a snapshot for the health endpoint.
func (p *Pinger) Stats() PingStats {
	return PingStats{
		Blocking:      p.stats.blocking.Load(),
		Informational: p.stats.informational.Load(),
		Repings:       p.stats.repings.Load(),
		Retracted:     p.stats.retracted.Load(),
		Withdrawn:     p.stats.withdrawn.Load(),
		Batched:       p.stats.batched.Load(),
		Failed:        p.stats.failed.Load(),
	}
}

// Heard acknowledges a ping so it is not repeated. The phone calls it when the
// notification is opened and the glasses call it when the speech finished
// playing; an answer to the underlying question counts as heard too.
func (p *Pinger) Heard(id string) {
	p.mu.Lock()
	ch, ok := p.heard[id]
	if ok {
		delete(p.heard, id)
	}
	p.mu.Unlock()
	if ok {
		close(ch)
	}
}

func (p *Pinger) track(id string) chan struct{} {
	ch := make(chan struct{})
	p.mu.Lock()
	p.heard[id] = ch
	p.mu.Unlock()
	return ch
}

func (p *Pinger) untrack(id string) {
	p.mu.Lock()
	delete(p.heard, id)
	p.mu.Unlock()
}

// Run consumes events until in closes or ctx is done, then waits for the pings
// already in flight to settle.
//
// Subscribe with Filter{Pings: true}: everything else on the bus is invisible to
// this policy by construction, and a replayed event never pings because
// event.Ping() already returns none for it — reattaching to a session must not
// fire a completion ping for every turn in its history.
func (p *Pinger) Run(ctx context.Context, in <-chan event.Event) error {
	defer p.wg.Wait()

	var pending []event.Event
	var flush <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case ev, ok := <-in:
			if !ok {
				return nil
			}
			switch ev.Ping() {
			case event.PingBlocking:
				ask, isAsk := ev.(*event.NeedsInput)
				if !isAsk {
					// Only NeedsInput is blocking. Anything else claiming to be
					// is a bug in an adapter, and it is worth a line rather
					// than a silent reinterpretation.
					p.log.Warn("bus: non-NeedsInput event claims a blocking ping",
						"kind", string(ev.Kind()), "session", ev.Envelope().Session)
					continue
				}
				p.wg.Add(1)
				go func() {
					defer p.wg.Done()
					p.blocking(ctx, ask)
				}()

			case event.PingInformational:
				if len(pending) > 0 {
					p.stats.batched.Add(1)
				}
				pending = append(pending, ev)
				if flush == nil {
					// A tumbling window from the first event, not a sliding one:
					// a sliding window under a steady stream of completions
					// never fires at all.
					flush = time.After(p.opts.BatchWindow)
				}
			}

		case <-flush:
			batch := pending
			pending, flush = nil, nil
			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				p.informational(ctx, batch)
			}()
		}
	}
}

// blocking implements the NeedsInput half of §7.
func (p *Pinger) blocking(ctx context.Context, ask *event.NeedsInput) {
	if ask.Answered() {
		// Withdrawn or answered before we ever spoke. Say nothing.
		p.stats.withdrawn.Add(1)
		return
	}

	// It may interrupt the conversation, but not a sentence.
	gap := true
	if p.opts.UtteranceTimeout > 0 {
		wctx, cancel := context.WithTimeout(ctx, p.opts.UtteranceTimeout)
		gap = p.opts.Gate.AwaitUtteranceEnd(wctx) == nil
		cancel()
	}
	if ctx.Err() != nil {
		return
	}
	if ask.Answered() {
		p.stats.withdrawn.Add(1)
		return
	}

	id := p.opts.NewID()
	heard := p.track(id)
	defer p.untrack(id)

	ping := Ping{
		ID:       id,
		Class:    ClassBlocking,
		At:       p.opts.Now(),
		Sessions: []string{ask.Envelope().Session},
		Events:   []event.Event{ask},
		Ask:      ask,
		// ORCHESTRATOR.md §4b: a permission request is a confirmation for
		// something with consequences, and confirmations are not suppressible.
		Consequential: ask.Ask == event.InputPermission,
		// Gap here is the end of the current utterance, not of the
		// conversation — false only if the user talked past the timeout and we
		// are interrupting anyway.
		Gap: gap,
		// Quiet stays false whatever the clock says. §7 again: a session
		// blocked at 3 a.m. is still blocked at 8 a.m., so quiet hours do not
		// apply to this ping and the field records that they do not.
		Quiet: false,
		Line:  askLine(ask, p.opts.Namer),
	}
	p.stats.blocking.Add(1)
	p.deliver(ctx, ping)

	// If unheard, re-ping once at 2 min, then hold.
	select {
	case <-ctx.Done():
		return
	case <-ask.Done():
		p.retract(ctx, id, "answered")
		return
	case <-heard:
		// Someone heard it. The question may still be open — that is fine, it
		// is theirs to answer now — but we do not ask again. We do still wait,
		// because the notification has to be retracted when the question closes
		// or it sits on a lock screen asking for an answer that already exists.
		select {
		case <-ctx.Done():
		case <-ask.Done():
			p.retract(ctx, id, "answered")
		}
		return
	case <-time.After(p.opts.RepingAfter):
	}

	if ask.Answered() {
		p.retract(ctx, id, "answered")
		return
	}
	ping.Repeat = 1
	ping.At = p.opts.Now()
	p.stats.repings.Add(1)
	p.deliver(ctx, ping)

	// Then hold: never a third time.
	select {
	case <-ctx.Done():
	case <-ask.Done():
		p.retract(ctx, id, "answered")
	}
}

// informational implements the TurnCompleted/Error half of §7.
func (p *Pinger) informational(ctx context.Context, batch []event.Event) {
	if len(batch) == 0 {
		return
	}

	// A completion ping waits for a gap. Whether the gap arrived is recorded and
	// not acted on: "if the gap never comes the speech is dropped and the
	// notification is not" is a rule about one backend, so it lives in the
	// backend (PRODUCT.md §6b).
	gctx, cancel := context.WithTimeout(ctx, p.opts.GapTimeout)
	gap := p.opts.Gate.AwaitGap(gctx) == nil
	cancel()
	if ctx.Err() != nil {
		return
	}

	quiet := p.opts.Quiet.Active(p.opts.Now())

	sessions := make([]string, 0, len(batch))
	seen := map[string]bool{}
	for _, ev := range batch {
		s := ev.Envelope().Session
		if s != "" && !seen[s] {
			seen[s] = true
			sessions = append(sessions, s)
		}
	}

	p.stats.informational.Add(1)
	p.deliver(ctx, Ping{
		ID:       p.opts.NewID(),
		Class:    ClassInformational,
		At:       p.opts.Now(),
		Sessions: sessions,
		Events:   batch,
		Gap:      gap,
		Quiet:    quiet,
		Line:     completionLine(batch, p.opts.Namer),
	})
	// Never repeats. There is no second half to this function on purpose.
}

func (p *Pinger) deliver(ctx context.Context, ping Ping) {
	if p.opts.Delivery == nil {
		return
	}
	if err := p.opts.Delivery.Deliver(ctx, ping); err != nil {
		p.stats.failed.Add(1)
		p.log.Warn("bus: ping delivery failed",
			"ping", ping.ID, "class", ping.Class.String(), "error", err)
	}
}

func (p *Pinger) retract(ctx context.Context, id, reason string) {
	if p.opts.Delivery == nil {
		return
	}
	p.stats.retracted.Add(1)
	// The context that carried us here may already be cancelled; a retraction is
	// exactly the message that must still get out.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := p.opts.Delivery.Retract(rctx, id, reason); err != nil {
		p.log.Warn("bus: ping retraction failed", "ping", id, "error", err)
	}
}

// ---------------------------------------------------------------- phrasing --

// askLine renders a blocked question, outcome first: what is being asked, then
// where. ADAPTERS.md §6 caps needs-input at ~120 characters plus the options,
// and the options travel structured rather than in this string.
func askLine(ask *event.NeedsInput, namer func(string) string) string {
	name := sessionName(ask.Envelope().Session, namer)
	prompt := strings.TrimSpace(ask.Prompt)
	if prompt == "" && ask.Tool != nil {
		if ask.Tool.Title != "" {
			prompt = ask.Tool.Title
		} else if ask.Tool.Name != "" {
			prompt = ask.Tool.Name
		}
	}
	if prompt == "" {
		// Say what is known. ADAPTERS.md §4: ACP may send a permission request
		// with nothing but a toolCallId, and inventing a description from raw
		// input is the thing the grounding rule forbids.
		prompt = "needs an answer"
	}
	return capText(fmt.Sprintf("%s: %s", name, prompt), 120)
}

// completionLine renders a batch, outcome first, under SpeechCap.
//
// "payments and docs are done, the migration failed" — the example in
// ADAPTERS.md §7, and this function produces exactly that shape from three
// TurnCompleted events. Nothing here reads a transcript or infers a reason; a
// batch it cannot phrase says how many finished rather than guessing why.
func completionLine(batch []event.Event, namer func(string) string) string {
	var done, failed []string
	seen := map[string]bool{}
	for _, ev := range batch {
		s := ev.Envelope().Session
		if seen[s] {
			continue
		}
		ok := false
		switch v := ev.(type) {
		case event.TurnCompleted:
			ok = v.OK
		case event.Error:
			ok = false
		default:
			continue
		}
		seen[s] = true
		name := sessionName(s, namer)
		if ok {
			done = append(done, name)
		} else {
			failed = append(failed, name)
		}
	}

	var parts []string
	if len(done) > 0 {
		verb := "is done"
		if len(done) > 1 {
			verb = "are done"
		}
		parts = append(parts, joinNames(done)+" "+verb)
	}
	if len(failed) > 0 {
		parts = append(parts, joinNames(failed)+" failed")
	}
	if len(parts) == 0 {
		return ""
	}
	return capText(strings.Join(parts, ", "), SpeechCap)
}

func sessionName(id string, namer func(string) string) string {
	if namer != nil {
		if n := strings.TrimSpace(namer(id)); n != "" {
			return n
		}
	}
	if id == "" {
		return "a session"
	}
	if len(id) > 8 {
		return "session " + id[:8]
	}
	return "session " + id
}

func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}

// cap truncates at a word boundary. Speech that runs long is worse than speech
// that stops early: ADAPTERS.md §6 budgets by seconds in someone's ear.
func capText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	if i := strings.LastIndexByte(cut, ' '); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:") + "…"
}
