package transcript

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/capture"
	"github.com/luthor007/relay/relayd/internal/logx"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// harness wires the real spool, the real ingester and the real pipeline. The
// only fake in it is the recogniser, which is the one thing that cannot run in a
// unit test.
type harness struct {
	dir   string
	clock *clock
	spool *capture.Spool
	ing   *capture.Ingester
	pipe  *Pipeline
	rec   *Fake
}

func newHarness(t *testing.T, retention time.Duration, recs ...Recognizer) *harness {
	t.Helper()
	c := &clock{t: mustTime("2026-08-10T09:00:00Z")}
	dir := t.TempDir()
	sp, err := capture.OpenSpool(capture.SpoolOptions{
		Dir: dir, Retention: retention, Now: c.now, Log: logx.Discard(),
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := capture.NewRegistry(capture.GateOptions{
		Scope: capture.ScopeAlways, IndicatorVisible: true, Now: c.now,
		Since: mustTime("2026-08-01T00:00:00Z"),
	})
	ing, err := capture.New(capture.Options{Spool: sp, Consent: reg, Now: c.now, Log: logx.Discard()})
	if err != nil {
		t.Fatal(err)
	}

	fake := &Fake{Speaker: "me"}
	if len(recs) == 0 {
		recs = []Recognizer{fake}
	}
	pipe, err := NewPipeline(PipelineOptions{
		Audio: sp, Router: NewRouter(recs...), Redact: Detector(),
		Diarize: true, Now: c.now, Log: logx.Discard(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{dir: dir, clock: c, spool: sp, ing: ing, pipe: pipe, rec: fake}
}

// say opens a live turn, speaks the words as chunks, and closes it.
func (h *harness) say(t *testing.T, words ...string) capture.Segment {
	t.Helper()
	s, err := h.ing.OpenLive(capture.LiveSpec{
		Device: "phone", Codec: "opus", StartedAt: h.clock.t, UserInitiated: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, w := range words {
		h.clock.add(500 * time.Millisecond)
		_, err := s.Chunk(capture.Chunk{
			EnvelopeID: "env-" + w, Seq: int64(i), Codec: "opus",
			Data: []byte(w), At: h.clock.t,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	seg, err := h.ing.CloseLive("phone", h.clock.t)
	if err != nil {
		t.Fatal(err)
	}
	return seg
}

// The whole of SYSTEM.md §5's promise, end to end: audio arrives, becomes text,
// and then stops existing.
func TestAudioIsGoneOnceItHasBeenTranscribedAndTheWindowPasses(t *testing.T) {
	h := newHarness(t, time.Hour)
	seg := h.say(t, "I'll send Marc the BOM on Friday.")

	audioPath := filepath.Join(h.dir, seg.ID+".audio")
	if fi, err := os.Stat(audioPath); err != nil || fi.Size() == 0 {
		t.Fatalf("no audio on disk before transcription: %v", err)
	}

	tr, err := h.pipe.Run(context.Background(), Job{SegmentID: seg.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tr.Text(), "BOM") {
		t.Fatalf("Text = %q", tr.Text())
	}

	// Still inside the window: the audio survives so it can be re-transcribed.
	h.clock.add(30 * time.Minute)
	if res := h.spool.Sweep(); len(res.Discarded) != 0 {
		t.Fatalf("swept inside the window: %v", res.Discarded)
	}

	h.clock.add(31 * time.Minute)
	res := h.spool.Sweep()
	if len(res.Discarded) != 1 {
		t.Fatalf("Discarded = %v, want the transcribed segment", res.Discarded)
	}
	if _, err := os.Stat(audioPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the audio file is still there: %v", err)
	}

	// The transcript outlives it, and a re-transcription now says so rather
	// than returning nothing.
	if _, err := h.pipe.Run(context.Background(), Job{SegmentID: seg.ID}); !errors.Is(err, capture.ErrDiscarded) {
		t.Fatalf("re-running after the sweep = %v, want ErrDiscarded", err)
	}
}

// The counter-rule, and the one that is expensive to get wrong: a recogniser
// that fails must not cost the audio.
func TestAFailedRecognitionKeepsTheAudio(t *testing.T) {
	failing := &Fake{FailAfter: 1}
	h := newHarness(t, time.Hour, failing)
	seg := h.say(t, "one", "two", "three")

	if _, err := h.pipe.Run(context.Background(), Job{SegmentID: seg.ID}); err == nil {
		t.Fatal("expected the recogniser's failure to surface")
	}
	got, err := h.spool.Get(seg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == capture.StateTranscribed {
		t.Fatal("a failed recognition marked the segment transcribed, which starts the deletion clock")
	}

	// And no amount of waiting deletes it.
	h.clock.add(30 * 24 * time.Hour)
	res := h.spool.Sweep()
	if len(res.Discarded) != 0 {
		t.Fatalf("untranscribed audio was swept: %v", res.Discarded)
	}
	if len(res.Stuck) != 1 {
		t.Fatalf("audio held past the deadline has to be visible: %+v", res.Stuck)
	}
	if !strings.Contains(res.Stuck[0].Reason, "promised to delete") {
		t.Fatalf("Stuck reason = %q", res.Stuck[0].Reason)
	}

	// A retry with a working recogniser finishes the job.
	working, err := NewPipeline(PipelineOptions{
		Audio: h.spool, Router: NewRouter(&Fake{Speaker: "me"}), Redact: Detector(),
		Now: h.clock.now, Log: logx.Discard(),
	})
	if err != nil {
		t.Fatal(err)
	}
	tr, err := working.Run(context.Background(), Job{SegmentID: seg.ID})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Text() == "" {
		t.Fatal("the retry produced nothing")
	}
}

// A gap declared during capture has to reach the transcript. That chain — one
// chunk lost on BLE, a hole in a stored day — is the whole reason the gap type
// exists in both packages.
func TestAGapSurvivesFromCaptureIntoTheTranscript(t *testing.T) {
	h := newHarness(t, time.Hour)
	s, err := h.ing.OpenLive(capture.LiveSpec{
		Device: "phone", Codec: "opus", StartedAt: h.clock.t, UserInitiated: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.clock.add(500 * time.Millisecond)
	if _, err := s.Chunk(capture.Chunk{EnvelopeID: "a", Seq: 0, Codec: "opus", Data: []byte("send the invoice to"), At: h.clock.t}); err != nil {
		t.Fatal(err)
	}
	// 1 is lost. 2 arrives.
	h.clock.add(500 * time.Millisecond)
	if _, err := s.Chunk(capture.Chunk{EnvelopeID: "c", Seq: 2, Codec: "opus", Data: []byte("by Friday."), At: h.clock.t}); err != nil {
		t.Fatal(err)
	}
	seg, err := h.ing.CloseLive("phone", h.clock.t)
	if err != nil {
		t.Fatal(err)
	}
	if len(seg.Gaps) != 1 {
		t.Fatalf("capture declared %d gaps, want 1", len(seg.Gaps))
	}

	tr, err := h.pipe.Run(context.Background(), Job{SegmentID: seg.ID})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Complete() {
		t.Fatal("a transcript over lost audio must not report itself complete")
	}
	if !strings.Contains(tr.Text(), "[relay:gap") {
		t.Fatalf("the hole is invisible in the transcript: %q", tr.Text())
	}
	if strings.Contains(tr.Text(), "invoice to by Friday") {
		t.Fatal("the transcript spliced across the hole")
	}
	named := false
	for _, n := range tr.Notes {
		if strings.Contains(n, "never reached this machine") {
			named = true
		}
	}
	if !named {
		t.Fatalf("the transcript should say audio was lost: %v", tr.Notes)
	}
}

func TestASegmentStillReceivingIsNotTranscribed(t *testing.T) {
	h := newHarness(t, time.Hour)
	s, err := h.ing.OpenLive(capture.LiveSpec{
		Device: "phone", Codec: "opus", StartedAt: h.clock.t, UserInitiated: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Chunk(capture.Chunk{EnvelopeID: "a", Seq: 0, Codec: "opus", Data: []byte("half a"), At: h.clock.t}); err != nil {
		t.Fatal(err)
	}
	_, err = h.pipe.Run(context.Background(), Job{SegmentID: s.ID()})
	if err == nil || !strings.Contains(err.Error(), "still receiving") {
		t.Fatalf("err = %v, want a refusal to file a prefix as the whole recording", err)
	}
}

// The phone-native path: the handset already did the work, so the box uses the
// text rather than paying for recognition twice — and still redacts it.
func TestPhoneRecognisedTextIsUsedAndStillRedacted(t *testing.T) {
	h := newHarness(t, time.Hour)
	seg := h.say(t, "unused audio")

	tr, err := h.pipe.Run(context.Background(), Job{
		SegmentID: seg.ID,
		Phone: []PhoneResult{
			{Text: "the key is AKIAIOSFODNN7EXAMPLE", Confidence: 0.95, At: seg.StartedAt},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Source != SourcePhone {
		t.Fatalf("Source = %s, want the phone's own result", tr.Source)
	}
	if h.rec.Opened() != 0 {
		t.Fatal("a recogniser ran even though the phone had already done the work")
	}
	if strings.Contains(tr.Text(), "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("the phone's text went in un-redacted: %q", tr.Text())
	}
	if tr.Redactions != 1 {
		t.Fatalf("Redactions = %d", tr.Redactions)
	}
}

// The bulk path is a file, and the pipeline reads it in blocks rather than
// loading a night into memory.
func TestBulkSegmentsAreReadInBlocks(t *testing.T) {
	h := newHarness(t, time.Hour)
	b := h.ing.Bulk()

	// Two blocks' worth of "audio", which for the fake is text. The filler is
	// one unbroken run rather than words, so the test measures block boundaries
	// rather than the fake's tokenizer.
	body := []byte(strings.Repeat("x", 2*BulkBlockBytes) + " the meeting is at three.")
	m := capture.Manifest{
		SessionID: "night", Kind: "audio", StartedAtMS: mustTime("2026-08-09T14:00:00Z").UnixMilli(),
		DurationS: 3600, TotalBytes: int64(len(body)), ChunkBytes: capture.ChunkBytes,
		Encoding: "opus", SourceName: "REC0001.opus",
	}
	if _, err := b.Declare("phone", m); err != nil {
		t.Fatal(err)
	}
	total := m.ChunkCount()
	for i := 0; i < total; i++ {
		lo := int64(i) * m.ChunkBytes
		hi := lo + m.ChunkBytes
		if hi > int64(len(body)) {
			hi = int64(len(body))
		}
		part := body[lo:hi]
		sum := sha256.Sum256(part)
		if _, err := b.PutChunk("night", i, part, hex.EncodeToString(sum[:])); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}
	if _, err := b.Complete("night", h.clock.t); err != nil {
		t.Fatal(err)
	}

	tr, err := h.pipe.Run(context.Background(), Job{SegmentID: "night"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tr.Text(), "the meeting is at three.") {
		t.Fatalf("the tail of the file never reached the recogniser: %q", tr.Text()[max(0, len(tr.Text())-80):])
	}
	if got, _ := h.spool.Get("night"); got.State != capture.StateTranscribed {
		t.Fatalf("state = %s, want transcribed", got.State)
	}
}

func TestPipelineRefusesWithoutARedactor(t *testing.T) {
	c := &clock{t: mustTime("2026-08-10T09:00:00Z")}
	sp, err := capture.OpenSpool(capture.SpoolOptions{Dir: t.TempDir(), Now: c.now, Log: logx.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewPipeline(PipelineOptions{Audio: sp, Router: NewRouter(&Fake{})})
	if !errors.Is(err, ErrNoRedactor) {
		t.Fatalf("err = %v, want ErrNoRedactor", err)
	}
}

// A sanity check that the spool really satisfies the interface the pipeline
// asks for, rather than the test using a convenient double.
var _ AudioSource = (*capture.Spool)(nil)

var _ = bytes.Equal
