package capture

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/logx"
)

func newSpool(t *testing.T, c *clock, opts ...func(*SpoolOptions)) (*Spool, string) {
	t.Helper()
	dir := t.TempDir()
	o := SpoolOptions{Dir: dir, Now: c.now, Log: logx.Discard()}
	for _, f := range opts {
		f(&o)
	}
	s, err := OpenSpool(o)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

// The promise SYSTEM.md §5 calls "the single easiest promise to make and keep".
// It is only kept if the bytes are actually gone from the filesystem, so this
// test looks at the filesystem.
func TestSweepActuallyDeletesTheAudio(t *testing.T) {
	c := &clock{t: at("2026-08-10T09:00:00Z")}
	s, dir := newSpool(t, c, func(o *SpoolOptions) { o.Retention = time.Hour })

	seg, err := s.Create(SegmentSpec{ID: "turn-1", Device: "phone", Kind: KindLive, Framed: true, StartedAt: c.t})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(seg.ID, []byte("opus frame bytes")); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkComplete(seg.ID, c.t); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "turn-1.audio")
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		t.Fatalf("expected audio on disk before transcription: %v", err)
	}

	// Untranscribed: the sweeper must not touch it, whatever the clock says.
	c.add(72 * time.Hour)
	res := s.Sweep()
	if len(res.Discarded) != 0 {
		t.Fatalf("swept untranscribed audio: %v", res.Discarded)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("untranscribed audio must survive: %v", err)
	}
	if len(res.Stuck) != 1 {
		t.Fatalf("audio held past the deadline has to be visible, got %+v", res.Stuck)
	}

	// Transcribed, but inside the window: still there.
	if err := s.MarkTranscribed(seg.ID, c.t); err != nil {
		t.Fatal(err)
	}
	c.add(30 * time.Minute)
	if res := s.Sweep(); len(res.Discarded) != 0 {
		t.Fatalf("swept inside the re-transcription window: %v", res.Discarded)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("audio inside the window must survive: %v", err)
	}

	// Past the window: gone from the disk, not merely marked.
	c.add(31 * time.Minute)
	res = s.Sweep()
	if len(res.Discarded) != 1 || res.Discarded[0] != "turn-1" {
		t.Fatalf("Discarded = %v, want [turn-1]", res.Discarded)
	}
	if res.FreedBytes == 0 {
		t.Fatal("a sweep that deleted audio should report the bytes it freed")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the audio file is still on disk: %v", err)
	}
	if got, _ := s.Get("turn-1"); got.State != StateDiscarded || got.Bytes != 0 {
		t.Fatalf("segment after sweep = %+v", got)
	}
	if _, err := s.Reader("turn-1"); !errors.Is(err, ErrDiscarded) {
		t.Fatalf("reading discarded audio = %v, want ErrDiscarded", err)
	}
	if s.Bytes() != 0 {
		t.Fatalf("spool still reports %d bytes", s.Bytes())
	}

	// And a fresh open of the same directory agrees — the promise survives a
	// restart, which is the only version of it that matters.
	again, err := OpenSpool(SpoolOptions{Dir: dir, Now: c.now, Log: logx.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := again.Get("turn-1"); got.State != StateDiscarded {
		t.Fatalf("after reopen, state = %s", got.State)
	}
	if again.Bytes() != 0 {
		t.Fatalf("after reopen, spool reports %d bytes", again.Bytes())
	}
}

func TestDiscardRefusesUntranscribedAudio(t *testing.T) {
	c := &clock{t: at("2026-08-10T09:00:00Z")}
	s, _ := newSpool(t, c)
	if _, err := s.Create(SegmentSpec{ID: "night", Device: "phone", Kind: KindBulk, StartedAt: c.t}); err != nil {
		t.Fatal(err)
	}
	err := s.Discard("night", "cleanup", false)
	if !errors.Is(err, ErrKeepUntranscribed) {
		t.Fatalf("Discard = %v, want the refusal to delete the only copy", err)
	}
	// A human asking in words is the one exception, and it has to be explicit.
	if err := s.Discard("night", "the user asked us to forget it", true); err != nil {
		t.Fatal(err)
	}
}

func TestFramesRoundTripAndSurviveARestart(t *testing.T) {
	c := &clock{t: at("2026-08-10T09:00:00Z")}
	s, dir := newSpool(t, c)
	if _, err := s.Create(SegmentSpec{ID: "turn-2", Device: "phone", Kind: KindLive, Framed: true, StartedAt: c.t}); err != nil {
		t.Fatal(err)
	}
	want := [][]byte{[]byte("one"), []byte("two"), []byte(""), []byte("four")}
	for _, f := range want {
		if _, err := s.Append("turn-2", f); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Flush("turn-2"); err != nil {
		t.Fatal(err)
	}

	again, err := OpenSpool(SpoolOptions{Dir: dir, Now: c.now, Log: logx.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := again.Frames("turn-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("read %d frames, wrote %d", len(got), len(want))
	}
	for i := range want {
		if string(got[i]) != string(want[i]) {
			t.Fatalf("frame %d = %q, want %q", i, got[i], want[i])
		}
	}
	if seg, _ := again.Get("turn-2"); seg.Frames != int64(len(want)) {
		t.Fatalf("frame count after reopen = %d, want %d", seg.Frames, len(want))
	}
}

// A crash between a frame's length prefix and its payload leaves a length that
// runs past the end of the file. Left alone, the whole turn fails to decode over
// a few bytes that were never audio.
func TestATornFrameIsTruncatedRatherThanFatal(t *testing.T) {
	c := &clock{t: at("2026-08-10T09:00:00Z")}
	s, dir := newSpool(t, c)
	if _, err := s.Create(SegmentSpec{ID: "torn", Device: "phone", Kind: KindLive, Framed: true, StartedAt: c.t}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("torn", []byte("good frame")); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush("torn"); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "torn.audio")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// A four-byte header claiming 99 bytes, and two bytes of payload.
	if _, err := f.Write([]byte{0, 0, 0, 99, 'x', 'y'}); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	again, err := OpenSpool(SpoolOptions{Dir: dir, Now: c.now, Log: logx.Discard()})
	if err != nil {
		t.Fatalf("a torn tail must not stop the spool from opening: %v", err)
	}
	got, err := again.Frames("torn")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || string(got[0]) != "good frame" {
		t.Fatalf("frames = %q, want the one complete frame", got)
	}
}

func TestCreateIsIdempotentAndRejectsAPathTraversal(t *testing.T) {
	c := &clock{t: at("2026-08-10T09:00:00Z")}
	s, _ := newSpool(t, c)

	first, err := s.Create(SegmentSpec{ID: "same", Device: "phone", Kind: KindLive, Framed: true, StartedAt: c.t})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("same", []byte("frame")); err != nil {
		t.Fatal(err)
	}
	second, err := s.Create(SegmentSpec{ID: "same", Device: "phone", Kind: KindLive, Framed: true, StartedAt: c.t})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatal("re-declaring must return the same segment")
	}
	frames, err := s.Frames("same")
	if err != nil || len(frames) != 1 {
		t.Fatalf("re-declaring truncated the segment: %v %d", err, len(frames))
	}

	for _, bad := range []string{"../escape", "a/b", "dot.ted"} {
		if _, err := s.Create(SegmentSpec{ID: bad, Kind: KindLive}); err == nil {
			t.Fatalf("id %q was accepted; it becomes a filename", bad)
		}
	}
}

func TestCapacityRefusesRatherThanEvicting(t *testing.T) {
	// APPS-SCOPE.md §4.2's eviction rule, one layer down: for a memory product
	// the store holds the only copy of something that already happened, so a
	// refusal is a state the UI can report and an eviction is last Tuesday
	// disappearing.
	c := &clock{t: at("2026-08-10T09:00:00Z")}
	s, _ := newSpool(t, c, func(o *SpoolOptions) { o.Capacity = 32 })
	if _, err := s.Create(SegmentSpec{ID: "small", Kind: KindLive, Framed: true, StartedAt: c.t}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("small", make([]byte, 20)); err != nil {
		t.Fatal(err)
	}
	_, err := s.Append("small", make([]byte, 20))
	if !errors.Is(err, ErrStorageFull) {
		t.Fatalf("Append past capacity = %v, want ErrStorageFull", err)
	}
	if frames, _ := s.Frames("small"); len(frames) != 1 {
		t.Fatalf("the refusal must not have cost the frame already stored, got %d", len(frames))
	}
}
