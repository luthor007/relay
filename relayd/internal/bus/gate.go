package bus

import (
	"context"
	"sync"
)

// Gate is turn-taking, which ADAPTERS.md §7 makes a ping policy rather than a
// speech-synthesis detail: "a completion ping waits for a gap; a needs-input
// ping may interrupt, because the alternative is a session blocked
// indefinitely — but it waits for the current *utterance* to end, not the
// current conversation."
//
// Two waits, because those are two different moments.
type Gate interface {
	// AwaitUtteranceEnd returns once the user is not mid-sentence. A blocking
	// ping waits on this and then speaks over whatever else is happening.
	AwaitUtteranceEnd(ctx context.Context) error

	// AwaitGap returns once the conversation is idle — nobody speaking, in
	// either direction. A completion ping waits on this.
	AwaitGap(ctx context.Context) error
}

// OpenGate is a gate that is always open: no turn-taking, everything speaks
// immediately. It is the default until something drives a real one, and it is
// what tests use when turn-taking is not what they are testing.
type OpenGate struct{}

var _ Gate = OpenGate{}

func (OpenGate) AwaitUtteranceEnd(ctx context.Context) error { return ctx.Err() }
func (OpenGate) AwaitGap(ctx context.Context) error          { return ctx.Err() }

// SpeechGate tracks who is talking. The phone drives the user half from ASR
// endpointing and the device half from the TTS player; relayd only reads it.
//
// The zero value is usable and open.
type SpeechGate struct {
	mu        sync.Mutex
	utterance int // nested StartUtterance calls
	speaking  int
	changed   chan struct{}
}

var _ Gate = (*SpeechGate)(nil)

// NewSpeechGate builds an open gate.
func NewSpeechGate() *SpeechGate { return &SpeechGate{} }

// StartUtterance marks the user as speaking. Calls nest, so two overlapping
// sources of speech do not open the gate early.
func (g *SpeechGate) StartUtterance() { g.bump(&g.utterance, 1) }

// EndUtterance marks the user as finished speaking.
func (g *SpeechGate) EndUtterance() { g.bump(&g.utterance, -1) }

// StartSpeaking marks Relay as speaking.
func (g *SpeechGate) StartSpeaking() { g.bump(&g.speaking, 1) }

// StopSpeaking marks Relay as finished speaking.
func (g *SpeechGate) StopSpeaking() { g.bump(&g.speaking, -1) }

// Busy reports whether anyone is speaking right now.
func (g *SpeechGate) Busy() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.utterance > 0 || g.speaking > 0
}

func (g *SpeechGate) bump(p *int, d int) {
	g.mu.Lock()
	*p += d
	if *p < 0 {
		*p = 0
	}
	if g.changed != nil {
		close(g.changed)
		g.changed = nil
	}
	g.mu.Unlock()
}

func (g *SpeechGate) wait(ctx context.Context, open func() bool) error {
	for {
		g.mu.Lock()
		if open() {
			g.mu.Unlock()
			return ctx.Err()
		}
		if g.changed == nil {
			g.changed = make(chan struct{})
		}
		ch := g.changed
		g.mu.Unlock()

		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// AwaitUtteranceEnd waits only for the user to stop talking. Relay speaking is
// not a reason to hold a blocking ping — interrupting our own narration is
// exactly what a blocked session should do.
func (g *SpeechGate) AwaitUtteranceEnd(ctx context.Context) error {
	return g.wait(ctx, func() bool { return g.utterance == 0 })
}

// AwaitGap waits for silence in both directions.
func (g *SpeechGate) AwaitGap(ctx context.Context) error {
	return g.wait(ctx, func() bool { return g.utterance == 0 && g.speaking == 0 })
}
