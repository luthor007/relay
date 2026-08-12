package transcript

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/luthor007/relay/relayd/internal/capture"
)

// Pipeline runs one spooled segment through a recogniser.
//
// It is the join between `internal/capture` and everything downstream, and it
// owns the one ordering that SYSTEM.md §5 turns on:
//
//	audio → transcript → MarkTranscribed → (retention window) → the audio is gone
//
// **MarkTranscribed is called only when a transcript exists.** A recognition
// that fails leaves the segment untranscribed, which is exactly the state the
// sweeper refuses to delete — so a model timeout costs a retry rather than an
// hour of somebody's life. That is the rule APPS-SCOPE.md §3.2 states from the
// device end ("never delete un-uploaded audio"), read from this one.

// AudioSource is the half of `*capture.Spool` this package needs. It is an
// interface so a test can drive the pipeline without a filesystem, and so the
// dependency is visible: the pipeline reads audio, marks it transcribed, and
// touches nothing else.
type AudioSource interface {
	Get(id string) (capture.Segment, error)
	// Frames reads a live segment's Opus packets back as they were appended.
	Frames(id string) ([][]byte, error)
	// Reader opens a flat segment. The bulk path uses it and reads in blocks,
	// because a night is up to 1.84 GB (APPS-SCOPE.md §3.1) and the one thing
	// that must never happen to it is io.ReadAll.
	Reader(id string) (io.ReadSeekCloser, error)
	MarkTranscribed(id string, at time.Time) error
}

// BulkBlockBytes is how much of a flat file is handed to a recogniser at a
// time. 64 KB is four seconds of Opus and half a second of PCM — small enough
// that a day never lands in memory, large enough that a sixteen-hour file is
// tens of thousands of writes rather than millions.
const BulkBlockBytes = 64 * 1024

// PipelineOptions configures a [Pipeline].
type PipelineOptions struct {
	Audio  AudioSource
	Router *Router
	// Redact is required, and is `internal/index`'s detector.
	Redact Redactor
	// Language is a BCP-47 tag passed to the recogniser.
	Language string
	// Diarize asks for speaker labels where a provider can produce them.
	Diarize bool
	Now     func() time.Time
	Log     *slog.Logger
}

// Pipeline transcribes segments.
type Pipeline struct {
	audio  AudioSource
	router *Router
	redact Redactor
	lang   string
	diar   bool
	now    func() time.Time
	log    *slog.Logger
}

// NewPipeline builds a pipeline. It refuses without a redactor for the reason
// in [ErrNoRedactor].
func NewPipeline(o PipelineOptions) (*Pipeline, error) {
	if o.Audio == nil {
		return nil, errors.New("transcript: no audio source")
	}
	if o.Router == nil {
		return nil, errors.New("transcript: no router")
	}
	if o.Redact == nil {
		return nil, ErrNoRedactor
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Pipeline{
		audio: o.Audio, router: o.Router, redact: o.Redact,
		lang: o.Language, diar: o.Diarize, now: o.Now, log: o.Log,
	}, nil
}

// PhoneResult is text the handset already recognised, handed to the pipeline so
// it can be used instead of paying for recognition twice.
type PhoneResult struct {
	Text       string
	Confidence float64
	// At is when it was spoken, used for the segment offset.
	At time.Time
	// NoisyRoom is the handset saying its recogniser is struggling.
	NoisyRoom bool
}

// Job is one transcription.
type Job struct {
	// SegmentID is the capture segment to read.
	SegmentID string
	// Phone is what the handset already produced, if anything.
	Phone []PhoneResult
	// Offline and CloudAllowed reach [Router.Choose] unchanged.
	Offline      bool
	CloudAllowed bool
}

// Run transcribes a segment.
//
// The three refusals worth naming, because each one is a case where producing
// *something* would be worse than producing nothing:
//
//   - A segment whose audio has already been discarded cannot be
//     re-transcribed, and says so rather than returning an empty transcript.
//   - A segment that is still receiving is not transcribed at all: the tail is
//     still arriving, and a transcript of a prefix would be filed as the whole
//     thing.
//   - A recogniser that fails leaves the audio in place and returns the error.
func (p *Pipeline) Run(ctx context.Context, job Job) (Transcript, error) {
	seg, err := p.audio.Get(job.SegmentID)
	if err != nil {
		return Transcript{}, err
	}
	switch seg.State {
	case capture.StateDiscarded:
		return Transcript{}, fmt.Errorf("%w: %s", capture.ErrDiscarded, seg.ID)
	case capture.StateReceiving:
		return Transcript{}, fmt.Errorf("transcript: segment %s is still receiving; transcribing a prefix would file it as the whole recording", seg.ID)
	}

	cond := Conditions{
		Bulk:         seg.Kind == capture.KindBulk,
		Offline:      job.Offline,
		CloudAllowed: job.CloudAllowed,
		Codec:        seg.Codec,
		Diarize:      p.diar,
	}
	if len(job.Phone) > 0 {
		cond.PhoneText = job.Phone[0].Text
		cond.PhoneConfidence = job.Phone[0].Confidence
		cond.NoisyRoom = job.Phone[0].NoisyRoom
	}

	choice, err := p.router.Choose(cond)
	if err != nil {
		return Transcript{}, err
	}

	b, err := NewBuilder(BuilderOptions{
		StreamID:   seg.ID,
		StartedAt:  seg.StartedAt,
		Source:     choice.Source,
		Recognizer: recognizerName(choice),
		Redact:     p.redact,
	})
	if err != nil {
		return Transcript{}, err
	}
	b.Note(choice.Why)
	if choice.Degraded {
		b.Note("this transcript is degraded: " + choice.Why)
	}

	if choice.Source == SourcePhone {
		p.addPhone(b, seg, job.Phone)
	} else if err := p.recognize(ctx, b, seg, choice); err != nil {
		// The audio stays. It is the only copy, and a model that timed out is a
		// retry rather than a reason to destroy an hour of someone's day.
		return Transcript{}, fmt.Errorf("transcript: %s failed on segment %s (the audio is kept for a retry): %w",
			recognizerName(choice), seg.ID, err)
	}

	if choice.Recognizer != nil && p.diar && !choice.Recognizer.Capabilities().Diarization {
		b.Note("this recogniser does not separate speakers, so nobody is named in this transcript")
	}
	if len(seg.Gaps) > 0 {
		b.Note(fmt.Sprintf("%d stretch(es) of audio never reached this machine and are marked in the text", len(seg.Gaps)))
	}

	t := b.Build()
	if err := p.audio.MarkTranscribed(seg.ID, p.now()); err != nil {
		return t, fmt.Errorf("transcript: segment %s was transcribed but could not be marked, so its audio will not be swept: %w", seg.ID, err)
	}
	return t, nil
}

// addPhone folds handset-recognised text into a transcript.
//
// It is redacted on the way in exactly like machine output: a credential read
// aloud is a credential either way, and the phone's recogniser has no detector.
func (p *Pipeline) addPhone(b *Builder, seg capture.Segment, rs []PhoneResult) {
	for _, r := range rs {
		var off time.Duration
		if !r.At.IsZero() && !seg.StartedAt.IsZero() {
			if d := r.At.Sub(seg.StartedAt); d > 0 {
				off = d
			}
		}
		b.Add(Result{
			Text: r.Text, Final: true, Confidence: r.Confidence,
			Start: off, End: off, Source: SourcePhone,
		})
	}
	// Gaps still belong in the text: the phone recognised what it heard, and
	// what it did not hear is still missing.
	for _, g := range seg.Gaps {
		b.Add(gapResult(g))
	}
	b.Note("nothing was recognised on this machine; the handset's own text was used")
}

// recognize streams a segment's frames through a recogniser.
func (p *Pipeline) recognize(ctx context.Context, b *Builder, seg capture.Segment, choice Choice) error {
	rec := choice.Recognizer
	if err := CheckCodec(rec, seg.Codec); err != nil {
		return err
	}

	stream, err := rec.Open(ctx, StreamConfig{
		StreamID: seg.ID, Codec: seg.Codec, SampleRate: seg.SampleRate,
		Channels: seg.Channels, StartedAt: seg.StartedAt,
		Language: p.lang, Diarize: p.diar,
	})
	if err != nil {
		return err
	}
	defer stream.Close()

	// Results are drained concurrently with the writes. That is not a detail:
	// it is what makes this streaming rather than batch, and SYSTEM.md §7b
	// names streaming ASR as one of the perceived-latency fixes that matter.
	done := make(chan struct{})
	var results []Result
	go func() {
		defer close(done)
		for r := range stream.Results() {
			results = append(results, r)
		}
	}()

	writeErr := p.feed(stream, seg)
	closeErr := stream.CloseSend()
	<-done

	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	for _, r := range results {
		b.Add(r)
	}
	return nil
}

// feed writes a segment's audio, interleaving its gaps in sequence order so a
// recogniser sees each hole where it happened rather than all of them at the
// end.
func (p *Pipeline) feed(stream Stream, seg capture.Segment) error {
	gaps := append([]capture.Gap(nil), seg.Gaps...)
	next := 0
	emitGapsUpTo := func(seq int64) error {
		for next < len(gaps) && gaps[next].FromSeq <= seq {
			if err := stream.Write(gapAudio(gaps[next])); err != nil {
				return err
			}
			next++
		}
		return nil
	}

	if seg.Framed {
		frames, err := p.audio.Frames(seg.ID)
		if err != nil {
			return err
		}
		for i, f := range frames {
			seq := int64(i)
			if err := emitGapsUpTo(seq); err != nil {
				return err
			}
			if err := stream.Write(Audio{
				Seq: seq, Codec: seg.Codec, SampleRate: seg.SampleRate,
				Channels: seg.Channels, Data: f,
			}); err != nil {
				return err
			}
		}
	} else if err := p.feedFlat(stream, seg, emitGapsUpTo); err != nil {
		return err
	}

	for ; next < len(gaps); next++ {
		if err := stream.Write(gapAudio(gaps[next])); err != nil {
			return err
		}
	}
	return nil
}

// feedFlat streams a bulk file in blocks.
//
// This is the shape difference APPS-SCOPE.md §3 keeps insisting on. A live turn
// is a sequence of small packets; a night is a file, and the only thing that
// matters about it here is that it is read in pieces and never held whole.
func (p *Pipeline) feedFlat(stream Stream, seg capture.Segment, emitGapsUpTo func(int64) error) error {
	r, err := p.audio.Reader(seg.ID)
	if err != nil {
		return err
	}
	defer r.Close()

	buf := make([]byte, BulkBlockBytes)
	var seq int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if gerr := emitGapsUpTo(seq); gerr != nil {
				return gerr
			}
			block := make([]byte, n)
			copy(block, buf[:n])
			if werr := stream.Write(Audio{
				Seq: seq, Codec: seg.Codec, SampleRate: seg.SampleRate,
				Channels: seg.Channels, Data: block,
			}); werr != nil {
				return werr
			}
			seq++
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("transcript: read segment %s: %w", seg.ID, err)
		}
	}
}

func gapAudio(g capture.Gap) Audio {
	return Audio{
		Seq: g.FromSeq, Gap: true, GapFor: g.EstimatedDuration,
		GapReason: g.Reason,
	}
}

func gapResult(g capture.Gap) Result {
	return Result{
		Final: true, Gap: true, GapReason: g.Reason,
		End: g.EstimatedDuration,
	}
}

func recognizerName(c Choice) string {
	if c.Recognizer == nil {
		return string(c.Source)
	}
	return c.Recognizer.Name()
}
