package transcript

import (
	"context"
	"sync"
	"time"
)

// Not every provider streams, and the ones that do not should say so.
//
// SYSTEM.md §7b is specific about why it matters: "Streaming ASR, not batch.
// Recognise while they speak so the prompt is ready the instant they stop,
// rather than starting a 400 ms job at that point." A batch recogniser is a
// 400 ms job at that point, and the difference is felt.
//
// So a batch provider is not refused — it may be the only accurate one for a
// language, and it is perfectly fine for the nightly bulk sync where nobody is
// waiting — but it is wrapped rather than pretended about. [Batch] reports
// `Streaming: false` in its [Capabilities], and that is what [Router] reads.

// BatchFunc recognises a whole recording at once.
type BatchFunc func(ctx context.Context, cfg StreamConfig, audio [][]byte) ([]Result, error)

// Batch adapts a whole-recording recogniser to the streaming [Recognizer]
// interface.
//
// It buffers what it is given, calls the function once on [Stream.CloseSend],
// and emits the results then — which is exactly the latency SYSTEM.md §7b warns
// about, and exactly why [Capabilities.Streaming] is false here. The wrapper
// exists so a batch provider is *usable and labelled*, not so it can pass for a
// streaming one.
type Batch struct {
	// ProviderName is what it is called in logs and in the console.
	ProviderName string
	// From is where it runs. A hosted batch recogniser is still
	// [SourceCloud] and still needs a grant before audio reaches it.
	From Source
	// Recognize is the call.
	Recognize BatchFunc
	// Caps is what it can do. Streaming is forced to false regardless of what
	// is set here, because it is not.
	Caps Capabilities
}

// Name implements [Recognizer].
func (b *Batch) Name() string {
	if b.ProviderName == "" {
		return "batch"
	}
	return b.ProviderName
}

// Source implements [Recognizer].
func (b *Batch) Source() Source {
	if b.From == "" {
		return SourceLocal
	}
	return b.From
}

// Capabilities implements [Recognizer]. Streaming is false, always: a wrapper
// that claimed otherwise would let the router pick it for a live turn on the
// strength of a property it does not have.
func (b *Batch) Capabilities() Capabilities {
	caps := b.Caps
	caps.Streaming = false
	return caps
}

// Open implements [Recognizer].
func (b *Batch) Open(ctx context.Context, cfg StreamConfig) (Stream, error) {
	return &batchStream{b: b, ctx: ctx, cfg: cfg, results: make(chan Result, 64)}, nil
}

type batchStream struct {
	b   *Batch
	ctx context.Context
	cfg StreamConfig

	mu      sync.Mutex
	closed  bool
	audio   [][]byte
	gaps    []Result
	results chan Result
	err     error
}

func (s *batchStream) Write(a Audio) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrStreamClosed
	}
	if a.Gap {
		// Holes are kept out of the buffer and re-emitted in place afterwards.
		// Handing a batch provider a concatenation across a hole would get back
		// a sentence nobody said, which is the failure gaps exist to prevent.
		s.gaps = append(s.gaps, Result{
			Final: true, Gap: true, GapReason: a.GapReason,
			Start: offsetOf(a, s.cfg), End: offsetOf(a, s.cfg) + a.GapFor,
			Source: s.b.Source(),
		})
		return nil
	}
	s.audio = append(s.audio, a.Data)
	return nil
}

func (s *batchStream) CloseSend() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	audio, gaps := s.audio, s.gaps
	s.mu.Unlock()

	out, err := s.b.Recognize(s.ctx, s.cfg, audio)
	if err != nil {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		close(s.results)
		return err
	}
	for _, r := range append(out, gaps...) {
		if r.Source == "" {
			r.Source = s.b.Source()
		}
		s.results <- r
	}
	close(s.results)
	return nil
}

func (s *batchStream) Results() <-chan Result { return s.results }

func (s *batchStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.results)
	}
	return s.err
}

func offsetOf(a Audio, cfg StreamConfig) time.Duration {
	if a.At.IsZero() || cfg.StartedAt.IsZero() {
		return 0
	}
	if d := a.At.Sub(cfg.StartedAt); d > 0 {
		return d
	}
	return 0
}
