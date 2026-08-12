package transcript

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Source is where a piece of text came from. It travels all the way to the
// episode, because "the phone heard this" and "a cloud vendor heard this" are
// different facts about the same sentence — one of them left the machine.
type Source string

const (
	// SourcePhone is recognition that already happened on the handset. Free,
	// offline, and the default per SYSTEM.md §8.
	SourcePhone Source = "phone"
	// SourceLocal is a recogniser running on the user's own box. Costs CPU,
	// costs nothing else, and never leaves the machine.
	SourceLocal Source = "local"
	// SourceCloud is a hosted recogniser. Better in a noisy room, and the audio
	// leaves the machine to get there — which is a consent question, not a
	// configuration one.
	SourceCloud Source = "cloud"
)

// Audio is one unit of sound on its way to a recogniser.
type Audio struct {
	Seq        int64
	At         time.Time
	Codec      string
	SampleRate int
	Channels   int
	Data       []byte

	// Gap marks audio that was never received. It carries no data, and a
	// recogniser must not splice across it: the frames on either side were not
	// adjacent, and a transcript that reads as continuous when it is not is the
	// failure `internal/capture` declares gaps to prevent.
	Gap bool
	// GapFor is how long the missing audio is estimated to have been.
	GapFor time.Duration
	// GapReason is why it is missing, in words.
	GapReason string
}

// Result is one thing a recogniser heard.
type Result struct {
	Text string
	// Final distinguishes a streaming partial from a settled utterance. Only
	// finals reach a transcript; partials exist so the orchestrator can start
	// routing before the speaker stops.
	Final bool
	// Confidence is 0..1 where the provider reports one, and 0 where it does
	// not — never a made-up number, because a confidence nobody measured is
	// exactly the kind of claim that decides a consent prompt.
	Confidence float64
	// Start and End are offsets from the stream's start.
	Start, End time.Duration
	// Speaker is a diarisation label where the provider does one, and empty
	// where it does not.
	Speaker string
	Source  Source
	// Gap marks a hole rather than speech. Text is empty.
	Gap bool
	// GapReason explains the hole.
	GapReason string
}

// StreamConfig opens a recognition stream.
type StreamConfig struct {
	// StreamID is the capture segment being transcribed, for logs and errors.
	StreamID   string
	Codec      string
	SampleRate int
	Channels   int
	StartedAt  time.Time
	// Language is a BCP-47 tag, or empty for the provider's default. Quebec is
	// the home market and a French speaker whose recogniser is pinned to
	// en-US produces confident nonsense rather than an error, so this is
	// carried rather than assumed.
	Language string
	// Diarize asks for speaker labels. Providers that cannot do it say so
	// through [Recognizer.Capabilities] rather than silently returning one
	// speaker.
	Diarize bool
}

// Capabilities is what a recogniser can actually do.
//
// It is a descriptor rather than a promise for the same reason `ADAPTERS.md` §8
// gives about the runtimes: a component that claims a capability it does not
// have produces plausible output that is wrong, and that is worse than a gap.
type Capabilities struct {
	// Streaming is whether partials arrive before the audio ends.
	Streaming bool
	// Diarization is whether it labels speakers.
	Diarization bool
	// Confidence is whether it reports one.
	Confidence bool
	// Offline is whether it works with no network.
	Offline bool
	// LeavesMachine is whether the audio goes to someone else's computer.
	LeavesMachine bool
	// Codecs it accepts. Empty means it will take whatever it is given, which
	// is only true of the fake.
	Codecs []string
}

// Recognizer is one configured speech-to-text provider.
type Recognizer interface {
	// Name identifies it in logs and in the console.
	Name() string
	Source() Source
	Capabilities() Capabilities
	// Open starts a stream. The caller writes audio, reads results, and closes.
	Open(ctx context.Context, cfg StreamConfig) (Stream, error)
}

// Stream is one recognition in progress.
//
// The shape is deliberately the streaming one even though a batch provider can
// satisfy it: writing the interface the other way round — audio in, transcript
// out — would make streaming an optimisation somebody adds later, and SYSTEM.md
// §7b names it as one of the two largest available latency wins.
//
// **Concurrency contract.** One goroutine writes and closes; another reads
// [Stream.Results]. That split is required rather than optional — writing and
// reading from the same goroutine deadlocks the moment a provider's result
// buffer fills, and the buffer filling is the normal case for anything longer
// than a sentence. [Stream.Write] and [Stream.Close] are not safe to call
// concurrently with each other.
type Stream interface {
	// Write feeds audio. It must not block on a network round trip; a provider
	// that needs to should buffer and return.
	Write(Audio) error
	// Results yields partials and finals. It closes after CloseSend and the
	// last final.
	Results() <-chan Result
	// CloseSend says no more audio is coming. Finals follow, then Results
	// closes.
	CloseSend() error
	// Close abandons the stream. Safe to call twice, and safe to call after
	// CloseSend.
	Close() error
}

// Errors from this package.
var (
	// ErrNoRecognizer is having nothing to transcribe with. It is an error
	// rather than a silent no-op: a box that stops producing transcripts should
	// say so loudly, because the failure is invisible until someone asks what
	// they said last Tuesday and the answer is nothing.
	ErrNoRecognizer = errors.New("transcript: no recogniser is configured")
	// ErrStreamClosed is a write after CloseSend.
	ErrStreamClosed = errors.New("transcript: this stream is closed")
	// ErrNoRedactor is a builder without a secret detector.
	ErrNoRedactor = errors.New("transcript: no secret detector, and writing transcripts without one is not allowed")
	// ErrCodecUnsupported is a recogniser that cannot take what the glasses
	// produced. Reported rather than transcoded: re-encoding costs battery and
	// quality (APPS-SCOPE.md §4.2) and a silent transcode hides the mismatch.
	ErrCodecUnsupported = errors.New("transcript: this recogniser does not accept that codec")
)

// CheckCodec reports whether a recogniser accepts a codec.
func CheckCodec(r Recognizer, codec string) error {
	caps := r.Capabilities()
	if len(caps.Codecs) == 0 || codec == "" {
		return nil
	}
	for _, c := range caps.Codecs {
		if c == codec {
			return nil
		}
	}
	return fmt.Errorf("%w: %s takes %v, the audio is %s", ErrCodecUnsupported, r.Name(), caps.Codecs, codec)
}
