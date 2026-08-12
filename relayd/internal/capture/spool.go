package capture

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The spool is where audio lives for the short time it is allowed to exist.
//
// SYSTEM.md §5 states the rule as an absence in the data model: "No raw audio
// after transcription. Audio is kept only long enough to re-transcribe, then
// discarded. Storing a recording of someone's life is a liability with no
// matching benefit, and it is the single easiest promise to make and keep."
//
// Easy to make, easy to break, and impossible to undo — so it is a retention
// window and a sweeper that unlinks files, not a comment. The counter-rule is
// APPS-SCOPE.md §3.2's, read from this end: once the box acknowledges a chunk
// the phone may free it, so between the acknowledgement and the transcript the
// box holds the only copy. The sweeper therefore refuses to delete anything
// that has not been transcribed, and reports what it refused.

// Kind distinguishes APPS-SCOPE.md §3's two capture paths, which are different
// products rather than two sizes of the same one.
type Kind string

const (
	// KindLive is Path A: opened on a tap or a wake word, closed immediately
	// after. Small, ordered, latency-sensitive.
	KindLive Kind = "live"
	// KindBulk is Path B: the nightly ritual, 170 MB–1.8 GB, over the LAN.
	KindBulk Kind = "bulk"
)

// State is where a segment is in its short life.
type State string

const (
	// StateReceiving: bytes are still arriving.
	StateReceiving State = "receiving"
	// StateComplete: every byte we expect is here, and no transcript exists.
	// This is the state in which the box holds the only copy.
	StateComplete State = "complete"
	// StateTranscribed: a transcript exists. The retention clock starts here
	// and nowhere else.
	StateTranscribed State = "transcribed"
	// StateDiscarded: the audio is gone. The manifest survives so the transcript
	// can say what it came from and when it was destroyed.
	StateDiscarded State = "discarded"
)

// Gap is audio that was never received.
//
// It exists because the alternative is worse: splicing the frames that did
// arrive produces a transcript that reads as continuous and is not. APPS-SCOPE
// §4.2 settled this on the phone ("every drop records a gap carrying its
// sequence range and byte count, so the box can mark the hole") and the same
// answer applies to a hole the box discovers itself.
type Gap struct {
	// FromSeq and ToSeq are the missing range, inclusive.
	FromSeq int64 `json:"from_seq"`
	ToSeq   int64 `json:"to_seq"`
	// At is when the gap was declared, not when the audio would have been
	// spoken — the box cannot know the latter for audio it never saw.
	At time.Time `json:"at"`
	// EstimatedBytes is missing frames times the mean frame size observed so
	// far. It is named "estimated" because it is: the frames are gone, and a
	// number presented as measured would be a claim about audio nobody has.
	EstimatedBytes int64 `json:"estimated_bytes"`
	// EstimatedDuration is the same estimate expressed as time, which is what a
	// transcript renders.
	EstimatedDuration time.Duration `json:"estimated_duration"`
	Reason            string        `json:"reason"`
}

// Segment is one spooled recording: a live turn, or one file of a night's sync.
type Segment struct {
	ID     string `json:"id"`
	Device string `json:"device"`
	Kind   Kind   `json:"kind"`
	State  State  `json:"state"`

	// Codec is the encoding as the phone declared it. Never transcoded here:
	// APPS-SCOPE.md §4.2 asks for Opus to pass through un-transcoded, because
	// re-encoding costs battery and quality for nothing.
	Codec      string `json:"codec"`
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`

	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	// RecordedFor is the duration the source declared, which for bulk sync is
	// the only way to date the end of a recording that arrives hours later.
	RecordedFor time.Duration `json:"recorded_for,omitempty"`
	// ExpectedBytes is what a bulk manifest declared, and 0 for a live stream
	// whose length is not known until it closes.
	ExpectedBytes int64 `json:"expected_bytes,omitempty"`
	Bytes         int64 `json:"bytes"`
	Frames        int64 `json:"frames"`
	// Framed says the file is length-prefixed frames rather than a flat file.
	// Live audio is framed because Opus packets have no self-delimiting length;
	// bulk audio is a file the glasses already wrote and is copied verbatim.
	Framed bool `json:"framed"`

	Gaps []Gap `json:"gaps,omitempty"`

	TranscribedAt time.Time `json:"transcribed_at,omitempty"`
	DiscardedAt   time.Time `json:"discarded_at,omitempty"`
	// DiscardReason is why the bytes went, so a transcript that outlives its
	// audio can say what happened rather than merely lacking a file.
	DiscardReason string `json:"discard_reason,omitempty"`

	// Source names the file on the glasses, so a partial sync can be reconciled
	// with the device. Bulk only.
	Source string `json:"source,omitempty"`

	// resume state for the live path, persisted so a restart does not
	// re-accept chunks it already durably has.
	NextSeq   int64    `json:"next_seq,omitempty"`
	RecentIDs []string `json:"recent_ids,omitempty"`

	// bulk resume state: which chunk indices have landed.
	ChunkBytes int64  `json:"chunk_bytes,omitempty"`
	Received   []byte `json:"received,omitempty"` // bitmap, one bit per chunk

	// sinceSave counts frames appended since the manifest was last written. A
	// live turn appends a frame every ~20 ms and an fsync per frame would cost
	// more than the audio does; [Spool.load] recovers the truth from the file
	// itself, so the manifest is allowed to lag.
	sinceSave int
}

// HasAudio reports whether the bytes are still on disk.
func (s Segment) HasAudio() bool { return s.State != StateDiscarded }

// Duration is the wall-clock span the segment covers, gaps included. It is zero
// while a live stream is open.
func (s Segment) Duration() time.Duration {
	if s.EndedAt.IsZero() || s.StartedAt.IsZero() {
		return 0
	}
	return s.EndedAt.Sub(s.StartedAt)
}

// Errors from the spool.
var (
	// ErrDiscarded is the audio being gone on purpose. It is not a failure:
	// SYSTEM.md §5 says this is what is supposed to happen.
	ErrDiscarded = errors.New("capture: the audio for this segment was discarded after transcription")
	// ErrNoSegment is an unknown id.
	ErrNoSegment = errors.New("capture: no such segment")
	// ErrStorageFull is the box refusing to accept audio it has no room for.
	// connector/src/protocol.ts calls this `storageFull` on the wire.
	ErrStorageFull = errors.New("capture: no room for this capture")
	// ErrNotFramed is a frame read against a flat bulk file.
	ErrNotFramed = errors.New("capture: this segment is not framed")
)

// SpoolOptions configures a [Spool].
type SpoolOptions struct {
	// Dir is where segments live. Created 0o700 — this is audio of someone's
	// day, and a mode that lets another account read it is a bug.
	Dir string
	// Retention is how long transcribed audio may survive. It is the window in
	// which a better model, or a failed pass, can re-transcribe. Default
	// [DefaultRetention].
	Retention time.Duration
	// StuckAfter is how long an untranscribed segment may sit before the
	// sweeper starts naming it. It never causes a deletion. Default
	// [DefaultStuckAfter].
	StuckAfter time.Duration
	// Capacity is the byte ceiling. A capture product that fills the disk takes
	// down the machine it runs on, and the agents share it. Default
	// [DefaultCapacity].
	Capacity int64
	Now      func() time.Time
	Log      *slog.Logger
}

// Defaults for [SpoolOptions].
const (
	// DefaultRetention is one day. Long enough to re-run a failed transcription
	// overnight or to re-transcribe with a better model the morning after;
	// short enough that "we do not keep your audio" is true in any ordinary
	// sense of the word.
	DefaultRetention = 24 * time.Hour
	// DefaultStuckAfter is three days. Past this an untranscribed segment is a
	// bug being reported, not a queue being slow.
	DefaultStuckAfter = 72 * time.Hour
	// DefaultCapacity is 64 GB, matching connector/src/store.ts. A day is
	// 170 MB–1.8 GB (APPS-SCOPE.md §3.1), so this is days of headroom without
	// being a promise to store a year.
	DefaultCapacity = int64(64) << 30
)

// Spool is the audio store.
type Spool struct {
	dir        string
	retention  time.Duration
	stuckAfter time.Duration
	capacity   int64
	now        func() time.Time
	log        *slog.Logger

	mu   sync.Mutex
	segs map[string]*Segment
}

// OpenSpool opens (creating if needed) a spool directory and loads whatever
// manifests are already there.
//
// Loading matters: a restart in the middle of a night's sync must resume rather
// than start over, and a segment that was complete-but-untranscribed when the
// daemon died is the case where the box holds the only copy.
func OpenSpool(o SpoolOptions) (*Spool, error) {
	if strings.TrimSpace(o.Dir) == "" {
		return nil, errors.New("capture: spool needs a directory")
	}
	if o.Retention <= 0 {
		o.Retention = DefaultRetention
	}
	if o.StuckAfter <= 0 {
		o.StuckAfter = DefaultStuckAfter
	}
	if o.Capacity <= 0 {
		o.Capacity = DefaultCapacity
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("capture: create spool dir: %w", err)
	}

	s := &Spool{
		dir: o.Dir, retention: o.Retention, stuckAfter: o.StuckAfter,
		capacity: o.Capacity, now: o.Now, log: o.Log,
		segs: map[string]*Segment{},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Retention is the configured window, for whoever has to explain it.
func (s *Spool) Retention() time.Duration { return s.retention }

func (s *Spool) load() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("capture: read spool dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			return fmt.Errorf("capture: read manifest %s: %w", e.Name(), err)
		}
		var seg Segment
		if err := json.Unmarshal(b, &seg); err != nil {
			return fmt.Errorf("capture: parse manifest %s: %w", e.Name(), err)
		}
		// Trust the file on disk over the manifest: the manifest is allowed to
		// lag by design (see Segment.sinceSave), and a crash mid-append can
		// leave a torn frame. The bytes are the thing that is actually there.
		if seg.State != StateDiscarded {
			if err := s.repair(&seg); err != nil {
				return err
			}
		}
		s.segs[seg.ID] = &seg
	}
	return nil
}

func (s *Spool) dataPath(id string) string { return filepath.Join(s.dir, id+".audio") }
func (s *Spool) manifestPath(id string) string {
	return filepath.Join(s.dir, id+".json")
}

// repair reconciles a manifest with the file it describes after a restart.
//
// For a flat bulk file that is a stat. For a framed live file it is a walk of
// the frame headers, which also finds a **torn tail** — a crash between the
// length prefix and the payload — and truncates it. A torn frame left in place
// would be read back as a length that runs past the end of the file, and the
// whole turn would fail to decode over a few bytes that were never audio.
func (s *Spool) repair(seg *Segment) error {
	fi, err := os.Stat(s.dataPath(seg.ID))
	if errors.Is(err, os.ErrNotExist) {
		seg.Bytes, seg.Frames = 0, 0
		return nil
	}
	if err != nil {
		return fmt.Errorf("capture: stat segment %s: %w", seg.ID, err)
	}
	if !seg.Framed {
		seg.Bytes = fi.Size()
		return nil
	}

	f, err := os.Open(s.dataPath(seg.ID))
	if err != nil {
		return fmt.Errorf("capture: open segment %s: %w", seg.ID, err)
	}
	defer f.Close()

	var (
		hdr    [4]byte
		off    int64
		frames int64
	)
	for {
		if _, err := io.ReadFull(f, hdr[:]); err != nil {
			break
		}
		n := int64(binary.BigEndian.Uint32(hdr[:]))
		if off+4+n > fi.Size() {
			break // torn tail
		}
		if _, err := f.Seek(n, io.SeekCurrent); err != nil {
			break
		}
		off += 4 + n
		frames++
	}
	seg.Bytes, seg.Frames = off, frames
	if off < fi.Size() {
		s.log.Warn("capture: truncating a torn frame left by an unclean shutdown",
			"segment", seg.ID, "kept", off, "dropped", fi.Size()-off)
		if err := os.Truncate(s.dataPath(seg.ID), off); err != nil {
			return fmt.Errorf("capture: truncate torn segment %s: %w", seg.ID, err)
		}
	}
	return nil
}

// saveEvery is how many appended frames may pass before the manifest is
// rewritten. 64 frames of 16 kHz Opus is a bit over a second — cheap to lose,
// and [Spool.repair] recovers it anyway.
const saveEvery = 64

// SegmentSpec declares a segment before its bytes arrive.
type SegmentSpec struct {
	ID            string
	Device        string
	Kind          Kind
	Codec         string
	SampleRate    int
	Channels      int
	StartedAt     time.Time
	RecordedFor   time.Duration
	ExpectedBytes int64
	ChunkBytes    int64
	Framed        bool
	Source        string
}

// Create declares a segment. It is idempotent on the id: a phone that lost the
// response and retried must not create a second segment or truncate the one it
// already filled, which is what a naive overwrite would do at the worst
// possible moment. Same rule as `connector/src/store.ts`'s `declare`.
func (s *Spool) Create(spec SegmentSpec) (Segment, error) {
	if strings.TrimSpace(spec.ID) == "" {
		return Segment{}, errors.New("capture: segment needs an id")
	}
	if strings.ContainsAny(spec.ID, `/\.`) {
		// The id becomes a filename. A traversal here writes audio wherever the
		// sender likes.
		return Segment{}, fmt.Errorf("capture: segment id %q may not contain a path separator or a dot", spec.ID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.segs[spec.ID]; ok {
		return *existing, nil
	}
	if spec.ExpectedBytes > 0 && spec.ExpectedBytes > s.freeLocked() {
		return Segment{}, fmt.Errorf("%w: %d bytes offered, %d free", ErrStorageFull, spec.ExpectedBytes, s.freeLocked())
	}

	seg := &Segment{
		ID: spec.ID, Device: spec.Device, Kind: spec.Kind, State: StateReceiving,
		Codec: spec.Codec, SampleRate: spec.SampleRate, Channels: spec.Channels,
		StartedAt: spec.StartedAt, RecordedFor: spec.RecordedFor,
		ExpectedBytes: spec.ExpectedBytes,
		Framed:        spec.Framed, Source: spec.Source, ChunkBytes: spec.ChunkBytes,
	}
	f, err := os.OpenFile(s.dataPath(seg.ID), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return Segment{}, fmt.Errorf("capture: create segment file: %w", err)
	}
	_ = f.Close()

	s.segs[seg.ID] = seg
	if err := s.saveLocked(seg); err != nil {
		return Segment{}, err
	}
	return *seg, nil
}

// Get returns a segment by id.
func (s *Spool) Get(id string) (Segment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seg, ok := s.segs[id]
	if !ok {
		return Segment{}, fmt.Errorf("%w: %s", ErrNoSegment, id)
	}
	return *seg, nil
}

// List returns every segment in a state, or every segment when states is empty.
// Ordered by start time, then id, so output is stable.
func (s *Spool) List(states ...State) []Segment {
	s.mu.Lock()
	defer s.mu.Unlock()

	want := map[State]bool{}
	for _, st := range states {
		want[st] = true
	}
	var out []Segment
	for _, seg := range s.segs {
		if len(want) > 0 && !want[seg.State] {
			continue
		}
		out = append(out, *seg)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.Before(out[j].StartedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Bytes is how much audio is on disk right now.
func (s *Spool) Bytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytesLocked()
}

func (s *Spool) bytesLocked() int64 {
	var n int64
	for _, seg := range s.segs {
		n += seg.Bytes
	}
	return n
}

// Free is how much room is left before the spool refuses new captures.
func (s *Spool) Free() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.freeLocked()
}

func (s *Spool) freeLocked() int64 {
	free := s.capacity - s.bytesLocked()
	if free < 0 {
		return 0
	}
	return free
}

// Append writes one frame to a framed (live) segment and returns its offset.
func (s *Spool) Append(id string, data []byte) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	seg, err := s.mutableLocked(id)
	if err != nil {
		return 0, err
	}
	if !seg.Framed {
		return 0, fmt.Errorf("%w: %s", ErrNotFramed, id)
	}
	need := int64(len(data)) + 4
	if need > s.freeLocked() {
		return 0, fmt.Errorf("%w: %d bytes, %d free", ErrStorageFull, need, s.freeLocked())
	}

	f, err := os.OpenFile(s.dataPath(id), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("capture: open segment for append: %w", err)
	}
	defer f.Close()

	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(data)))
	if _, err := f.Write(hdr[:]); err != nil {
		return 0, fmt.Errorf("capture: write frame header: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		return 0, fmt.Errorf("capture: write frame: %w", err)
	}
	off := seg.Bytes
	seg.Bytes += need
	seg.Frames++
	seg.sinceSave++
	if seg.sinceSave < saveEvery {
		return off, nil
	}
	return off, s.saveLocked(seg)
}

// Flush writes a segment's manifest now. Called when a stream closes and on
// shutdown; [Spool.Append] otherwise batches (see saveEvery).
func (s *Spool) Flush(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	seg, ok := s.segs[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoSegment, id)
	}
	return s.saveLocked(seg)
}

// WriteAt writes bytes at an offset in a flat (bulk) segment.
func (s *Spool) WriteAt(id string, off int64, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	seg, err := s.mutableLocked(id)
	if err != nil {
		return err
	}
	if seg.Framed {
		return fmt.Errorf("capture: segment %s is framed; use Append", id)
	}
	if int64(len(data)) > s.freeLocked() {
		return fmt.Errorf("%w: %d bytes, %d free", ErrStorageFull, len(data), s.freeLocked())
	}

	f, err := os.OpenFile(s.dataPath(id), os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("capture: open segment for write: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteAt(data, off); err != nil {
		return fmt.Errorf("capture: write at %d: %w", off, err)
	}
	if end := off + int64(len(data)); end > seg.Bytes {
		seg.Bytes = end
	}
	return s.saveLocked(seg)
}

// Reader opens the raw bytes. The caller closes it.
func (s *Spool) Reader(id string) (io.ReadSeekCloser, error) {
	s.mu.Lock()
	seg, ok := s.segs[id]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrNoSegment, id)
	}
	state := seg.State
	s.mu.Unlock()

	if state == StateDiscarded {
		return nil, fmt.Errorf("%w: %s", ErrDiscarded, id)
	}
	f, err := os.Open(s.dataPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrDiscarded, id)
	}
	if err != nil {
		return nil, fmt.Errorf("capture: open segment: %w", err)
	}
	return f, nil
}

// Frames reads a framed segment back as the frames that were appended.
//
// It reads the whole segment into memory, which is correct for a live turn — a
// few hundred kilobytes — and is why the bulk path is flat rather than framed.
func (s *Spool) Frames(id string) ([][]byte, error) {
	seg, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if !seg.Framed {
		return nil, fmt.Errorf("%w: %s", ErrNotFramed, id)
	}
	r, err := s.Reader(id)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	var out [][]byte
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, fmt.Errorf("capture: read frame header: %w", err)
		}
		n := binary.BigEndian.Uint32(hdr[:])
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, fmt.Errorf("capture: short frame: %w", err)
		}
		out = append(out, buf)
	}
}

// SetResume persists the live path's dedupe state: the next sequence the box
// wants, and the envelope ids it has recently accepted.
//
// It is written on close and on every gap rather than on every chunk. A restart
// mid-turn therefore may re-accept a handful of already-stored chunks, which is
// a duplicate frame in one turn's audio — cheap. Losing the whole turn's
// ordering, which is what forgetting NextSeq would cost, is not.
func (s *Spool) SetResume(id string, nextSeq int64, recent []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	seg, err := s.mutableLocked(id)
	if err != nil {
		return err
	}
	seg.NextSeq = nextSeq
	seg.RecentIDs = append([]string(nil), recent...)
	return s.saveLocked(seg)
}

// MarkReceived records that a bulk chunk landed, and returns the resulting
// bitmap.
//
// The read-modify-write happens under the spool's own lock rather than in the
// caller, and that is not fussiness: a resumable uploader sends chunks in
// parallel, and two goroutines each reading the bitmap, setting their bit and
// writing it back would lose one of them. The lost bit is a chunk the box has
// but does not know it has — so the phone re-sends it forever and the session
// never completes.
func (s *Spool) MarkReceived(id string, index, total int) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seg, err := s.mutableLocked(id)
	if err != nil {
		return nil, err
	}
	bitmap := ensureBitmap(seg.Received, total)
	bitSet(bitmap, index)
	seg.Received = bitmap
	if err := s.saveLocked(seg); err != nil {
		return nil, err
	}
	return append([]byte(nil), bitmap...), nil
}

// AddGap records audio that was never received.
func (s *Spool) AddGap(id string, g Gap) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	seg, err := s.mutableLocked(id)
	if err != nil {
		return err
	}
	seg.Gaps = append(seg.Gaps, g)
	return s.saveLocked(seg)
}

// MarkComplete says every byte we expect has arrived. It does **not** start the
// retention clock: a complete segment with no transcript is the state in which
// the box holds the only copy of something that already happened.
func (s *Spool) MarkComplete(id string, endedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	seg, err := s.mutableLocked(id)
	if err != nil {
		return err
	}
	if seg.State == StateReceiving {
		seg.State = StateComplete
	}
	if seg.EndedAt.IsZero() {
		seg.EndedAt = endedAt
	}
	return s.saveLocked(seg)
}

// MarkTranscribed says a transcript now exists. This is the only call that
// starts the retention clock, and it is the only door to [Spool.Sweep] deleting
// anything.
func (s *Spool) MarkTranscribed(id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	seg, err := s.mutableLocked(id)
	if err != nil {
		return err
	}
	if seg.State == StateDiscarded {
		return fmt.Errorf("%w: %s", ErrDiscarded, id)
	}
	if seg.EndedAt.IsZero() {
		seg.EndedAt = at
	}
	seg.State = StateTranscribed
	seg.TranscribedAt = at
	return s.saveLocked(seg)
}

// Discard deletes a segment's audio now, without waiting for the window.
//
// Used by an explicit "forget this" and by [Spool.Sweep]. It refuses on a
// segment that has never been transcribed unless force is set, because that
// audio is the only copy — and force exists for the one case where a human
// asked for it in words.
func (s *Spool) Discard(id, reason string, force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	seg, ok := s.segs[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoSegment, id)
	}
	if seg.State != StateTranscribed && !force {
		return fmt.Errorf("capture: segment %s has no transcript yet, and it is the only copy: %w",
			id, errKeepUntranscribed)
	}
	return s.discardLocked(seg, reason)
}

var errKeepUntranscribed = errors.New("un-transcribed audio is never deleted (APPS-SCOPE.md §3.2, from the other end)")

// ErrKeepUntranscribed is the refusal to delete audio that has no transcript.
var ErrKeepUntranscribed = errKeepUntranscribed

func (s *Spool) discardLocked(seg *Segment, reason string) error {
	if err := os.Remove(s.dataPath(seg.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("capture: discard %s: %w", seg.ID, err)
	}
	seg.State = StateDiscarded
	seg.Bytes = 0
	seg.DiscardedAt = s.now()
	seg.DiscardReason = reason
	return s.saveLocked(seg)
}

// SweepResult is what one pass did, and — as importantly — what it refused to
// do. A sweeper that only reports deletions cannot tell you that your audio is
// piling up because transcription has been failing since Tuesday.
type SweepResult struct {
	At time.Time
	// Discarded is the segments whose audio was deleted.
	Discarded []string
	// FreedBytes is how much disk that returned.
	FreedBytes int64
	// Kept is segments the sweeper deliberately left alone, with the reason.
	Kept []Kept
	// Stuck is the subset of Kept that has been waiting longer than
	// StuckAfter. This is the number that should reach a human: every entry is
	// audio of somebody's life that we promised to delete and have not.
	Stuck []Kept
}

// Kept is one segment the sweeper did not delete, and why.
type Kept struct {
	ID     string
	State  State
	Age    time.Duration
	Bytes  int64
	Reason string
}

// Sweep enforces SYSTEM.md §5's retention rule.
//
// One pass: transcribed audio past the window is deleted, everything else is
// kept and named. Call it on a timer and on shutdown.
func (s *Spool) Sweep() SweepResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	res := SweepResult{At: now}

	ids := make([]string, 0, len(s.segs))
	for id := range s.segs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		seg := s.segs[id]
		switch seg.State {
		case StateDiscarded:
			continue

		case StateTranscribed:
			age := now.Sub(seg.TranscribedAt)
			if age < s.retention {
				res.Kept = append(res.Kept, Kept{
					ID: id, State: seg.State, Age: age, Bytes: seg.Bytes,
					Reason: "inside the re-transcription window",
				})
				continue
			}
			freed := seg.Bytes
			if err := s.discardLocked(seg, "retention window elapsed after transcription"); err != nil {
				s.log.Error("capture: sweep could not discard audio",
					"segment", id, "err", err)
				res.Kept = append(res.Kept, Kept{
					ID: id, State: seg.State, Age: age, Bytes: seg.Bytes,
					Reason: "deletion failed: " + err.Error(),
				})
				continue
			}
			res.Discarded = append(res.Discarded, id)
			res.FreedBytes += freed

		default:
			// Receiving or complete: no transcript, so this may be the only
			// copy. Never deleted, always named.
			age := now.Sub(seg.StartedAt)
			k := Kept{
				ID: id, State: seg.State, Age: age, Bytes: seg.Bytes,
				Reason: "no transcript yet, and the box may hold the only copy",
			}
			res.Kept = append(res.Kept, k)
			if age >= s.stuckAfter {
				k.Reason = "no transcript after " + age.Round(time.Minute).String() +
					" — audio we promised to delete is still here"
				res.Stuck = append(res.Stuck, k)
			}
		}
	}

	if len(res.Stuck) > 0 {
		s.log.Warn("capture: audio is waiting on a transcript past the deadline",
			"segments", len(res.Stuck), "retention", s.retention, "stuck_after", s.stuckAfter)
	}
	return res
}

// Forget removes a segment's manifest as well as its audio. Used when the
// transcript is also being deleted — the user's data is theirs, exportable and
// deletable (ARCHITECTURE.md §6).
func (s *Spool) Forget(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	seg, ok := s.segs[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoSegment, id)
	}
	if seg.State != StateDiscarded {
		if err := s.discardLocked(seg, "forgotten on request"); err != nil {
			return err
		}
	}
	if err := os.Remove(s.manifestPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("capture: forget %s: %w", id, err)
	}
	delete(s.segs, id)
	return nil
}

func (s *Spool) mutableLocked(id string) (*Segment, error) {
	seg, ok := s.segs[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoSegment, id)
	}
	if seg.State == StateDiscarded {
		return nil, fmt.Errorf("%w: %s", ErrDiscarded, id)
	}
	return seg, nil
}

// saveLocked writes the manifest through a temp file and a rename, so a crash
// mid-write leaves the old manifest rather than half of the new one.
func (s *Spool) saveLocked(seg *Segment) error {
	b, err := json.Marshal(seg)
	if err != nil {
		return fmt.Errorf("capture: encode manifest: %w", err)
	}
	seg.sinceSave = 0
	tmp := s.manifestPath(seg.ID) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("capture: write manifest: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return fmt.Errorf("capture: write manifest: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("capture: sync manifest: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("capture: close manifest: %w", err)
	}
	if err := os.Rename(tmp, s.manifestPath(seg.ID)); err != nil {
		return fmt.Errorf("capture: commit manifest: %w", err)
	}
	return nil
}
