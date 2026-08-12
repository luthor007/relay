package transcript

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// The deterministic local recogniser.
//
// **The audio is the text.** Every frame carries UTF-8, so a fixture reads as
// prose, the output is exactly reproducible, and nothing in this package's tests
// depends on an audio stack, a model download or a network. RelayKit's
// `MockRecognizer` makes the same trade for the same stated reason — recognition
// "is one more thing that cannot run in a unit test" — so the seam is where the
// test lives.
//
// It is not a stub. It streams: a partial per word and a final per sentence,
// which is exactly the shape a real streaming provider produces and exactly the
// shape [Builder] and [Pipeline] have to handle correctly. A stub that returned
// one string at the end would have let a batch-shaped bug through.

// Fake is the deterministic recogniser.
type Fake struct {
	// Speaker labels every result. Empty means no diarisation, which is the
	// honest default: one microphone on someone's face does not separate voices
	// by itself.
	Speaker string
	// Confidence is what every result reports. Zero means "not reported",
	// which is what most providers actually do.
	Confidence float64
	// FailAfter makes Write fail once this many frames have been written, so
	// the pipeline's "a failed recognition must not delete the audio" path has
	// something to fail with. Zero never fails.
	FailAfter int
	// Err is the error FailAfter returns.
	Err error

	mu     sync.Mutex
	opened int
}

// Opened is how many streams were opened, for tests that care whether the
// router chose this one.
func (f *Fake) Opened() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opened
}

// Name identifies the fake.
func (f *Fake) Name() string { return "fake" }

// Source is local: it runs here and the audio never leaves.
func (f *Fake) Source() Source { return SourceLocal }

// Capabilities reports what it can do, and does not overclaim. Diarization is
// true only when a speaker was configured, because a label nobody asked for is
// a claim about who was talking.
func (f *Fake) Capabilities() Capabilities {
	return Capabilities{
		Streaming:     true,
		Diarization:   f.Speaker != "",
		Confidence:    f.Confidence > 0,
		Offline:       true,
		LeavesMachine: false,
	}
}

// Open starts a fake stream.
func (f *Fake) Open(_ context.Context, cfg StreamConfig) (Stream, error) {
	f.mu.Lock()
	f.opened++
	f.mu.Unlock()
	return &fakeStream{f: f, cfg: cfg, results: make(chan Result, 256)}, nil
}

type fakeStream struct {
	f       *Fake
	cfg     StreamConfig
	results chan Result

	mu     sync.Mutex
	closed bool

	written int
	// buf accumulates words until a sentence terminator settles them.
	buf       []string
	start     time.Duration
	end       time.Duration
	haveStart bool
}

// nominalFrame is how long one frame of audio is assumed to be when the caller
// gives no timestamps. 20 ms is the usual Opus frame at 16 kHz. It is used only
// for offsets inside a transcript, and never reported as a measurement.
const nominalFrame = 20 * time.Millisecond

func (s *fakeStream) Write(a Audio) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrStreamClosed
	}
	s.written++
	if s.f.FailAfter > 0 && s.written > s.f.FailAfter {
		err := s.f.Err
		if err == nil {
			err = ErrFakeFailed
		}
		s.mu.Unlock()
		return err
	}
	emits := s.consumeLocked(a)
	s.mu.Unlock()

	// Sends happen outside the lock, and they block rather than drop.
	// APPS-SCOPE.md §4.2: never drop silently. A consumer that stops reading
	// should stall the producer visibly, not lose the sentence.
	return s.send(emits)
}

func (s *fakeStream) consumeLocked(a Audio) []Result {
	var emits []Result
	at := s.offsetLocked(a)

	if a.Gap {
		// A hole ends whatever was being accumulated: the words on either side
		// were not adjacent, and running them together produces a sentence
		// nobody said.
		emits = append(emits, s.flushLocked(true)...)
		emits = append(emits, Result{
			Final: true, Gap: true, GapReason: a.GapReason,
			Start: at, End: at + a.GapFor, Source: s.f.Source(),
			Speaker: s.f.Speaker,
		})
		s.end = at + a.GapFor
		return emits
	}

	text := string(a.Data)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if !s.haveStart {
		s.start = at
		s.haveStart = true
	}
	for _, word := range strings.Fields(text) {
		s.buf = append(s.buf, word)
		s.end = at + nominalFrame
		// A partial per word: this is what "streaming, not batch" means at the
		// wire, and it is what lets routing start before the speaker stops.
		emits = append(emits, Result{
			Text: strings.Join(s.buf, " "), Final: false,
			Start: s.start, End: s.end,
			Confidence: s.f.Confidence, Speaker: s.f.Speaker, Source: s.f.Source(),
		})
		if endsSentence(word) {
			emits = append(emits, s.flushLocked(false)...)
		}
	}
	return emits
}

func (s *fakeStream) CloseSend() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	emits := s.flushLocked(false)
	s.closed = true
	s.mu.Unlock()

	if err := s.send(emits); err != nil {
		return err
	}
	close(s.results)
	return nil
}

func (s *fakeStream) Results() <-chan Result { return s.results }

func (s *fakeStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.results)
	}
	return nil
}

func (s *fakeStream) send(rs []Result) error {
	for _, r := range rs {
		s.results <- r
	}
	return nil
}

func (s *fakeStream) flushLocked(dropStart bool) []Result {
	if len(s.buf) == 0 {
		if dropStart {
			s.haveStart = false
		}
		return nil
	}
	r := Result{
		Text: strings.Join(s.buf, " "), Final: true,
		Start: s.start, End: s.end,
		Confidence: s.f.Confidence, Speaker: s.f.Speaker, Source: s.f.Source(),
	}
	s.buf = nil
	s.haveStart = false
	return []Result{r}
}

func (s *fakeStream) offsetLocked(a Audio) time.Duration {
	if !a.At.IsZero() && !s.cfg.StartedAt.IsZero() {
		if d := a.At.Sub(s.cfg.StartedAt); d >= 0 {
			return d
		}
	}
	return time.Duration(s.written-1) * nominalFrame
}

func endsSentence(word string) bool {
	if word == "" {
		return false
	}
	switch word[len(word)-1] {
	case '.', '?', '!':
		return true
	}
	return false
}

// ErrFakeFailed is what [Fake.FailAfter] returns when no other error is set.
var ErrFakeFailed = errors.New("transcript: the fake recogniser was told to fail")
