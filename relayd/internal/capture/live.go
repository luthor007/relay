package capture

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// The live path — APPS-SCOPE.md §3's Path A.
//
// "Path A is the interactive loop, opened only when the user taps or says the
// wake word, and closed immediately after." It is a turn, not a state: both
// radios and both batteries are busy while it is open, which is exactly why the
// all-day capture rides Path B instead.
//
// Everything here is about the gap between the phone and the box. The link
// (`glasses/bridge/src/relayd.ts`) is at-least-once by construction — anything
// sent on a connection that then closed abnormally goes back to the *head* of
// the outbox and is sent again — so this side must be idempotent on the
// envelope `id`, tolerant of reordering, and honest about what never arrived.

// Chunk is one `audio.chunk` frame, unwrapped from SYSTEM.md §6.1's envelope.
//
// The fields come from `relayd/internal/api`'s AudioChunk (`{seq, codec, data}`)
// plus the envelope's own `id` and `at`, which are the two things that make
// dedupe and gap timing possible. This package deliberately does not import the
// api package: the wiring goes api → capture, and a cycle would be the
// consequence of doing it the other way round.
type Chunk struct {
	// EnvelopeID is SYSTEM.md §6.1's `id`. It is the dedupe key, and
	// `relayd.ts` says so in as many words: "which is what the envelope's `id`
	// is for, and why the server side dedupes on it".
	EnvelopeID string
	// Seq is monotonic within one voice turn. The iOS client's own comment:
	// "Monotonic per voice session; gaps mean dropped chunks."
	Seq   int64
	Codec string
	Data  []byte
	// At is the envelope's timestamp.
	At time.Time
}

// ChunkResult is what one chunk did. Every field is observable; nothing here
// reports audio the box did not receive.
type ChunkResult struct {
	// Stored is how many frames this call wrote, which is more than one when it
	// unblocked buffered successors.
	Stored int
	// Duplicate is a chunk the box already has. It is a success: a client
	// retrying after a lost ack is behaving correctly and must not be told it
	// failed.
	Duplicate bool
	// Buffered is a chunk held because its predecessor has not arrived.
	Buffered bool
	// Gaps were declared by this call.
	Gaps []Gap
	// NextSeq is what the box wants next. The phone can use it to resume.
	NextSeq int64
	// Pending is how many chunks are waiting on a missing predecessor.
	Pending int
}

// Errors from the live path.
var (
	// ErrClosed is a chunk on a stream that has already closed.
	ErrClosed = errors.New("capture: this stream is closed")
	// ErrCodecChanged is a stream whose codec changed mid-turn. Refused rather
	// than concatenated: two codecs in one file decode to noise, and a
	// transcript of noise is worse than a refusal somebody can see.
	ErrCodecChanged = errors.New("capture: the codec changed mid-stream")
)

// LiveOptions configures [Ingester]'s live path.
type LiveOptions struct {
	// ReorderWindow is how many out-of-order chunks may be held before the box
	// gives up on the missing one and declares a gap. Default
	// [DefaultReorderWindow].
	ReorderWindow int
	// GapTimeout is how long a hole may stay open before it is declared.
	// Default [DefaultGapTimeout].
	GapTimeout time.Duration
	// RecentIDs is how many envelope ids to remember per stream for dedupe.
	// Default [DefaultRecentIDs].
	RecentIDs int
}

// Defaults for [LiveOptions].
const (
	// DefaultReorderWindow is 64 chunks — a bit over a second of 16 kHz Opus.
	// Large enough to absorb the reordering a reconnect produces, small enough
	// that a genuinely lost chunk is declared while the sentence is still being
	// spoken rather than after it.
	DefaultReorderWindow = 64
	// DefaultGapTimeout is two seconds. Past that the chunk is not late, it is
	// gone, and SYSTEM.md §7b's whole point is that the prompt should be ready
	// the instant they stop talking.
	DefaultGapTimeout = 2 * time.Second
	// DefaultRecentIDs is 512 envelope ids. The outbox replays from its head,
	// so the duplicates a reconnect produces are recent by construction.
	DefaultRecentIDs = 512
)

// LiveStream is one open voice turn.
type LiveStream struct {
	ing    *Ingester
	id     string
	device string

	mu       sync.Mutex
	closed   bool
	codec    string
	nextSeq  int64
	pending  map[int64]pendingChunk
	recent   []string
	recentAt map[string]bool

	frames    int64
	dataBytes int64
	firstAt   time.Time
	lastAt    time.Time
	// intervals accumulates observed inter-chunk arrival gaps, which is the
	// only basis the box has for estimating how long a missing run of audio
	// would have been.
	intervalSum   time.Duration
	intervalCount int64

	// lastGap is the gap most recently declared, so reconcileLocked can report
	// it without re-reading the manifest.
	lastGap Gap
}

type pendingChunk struct {
	seq  int64
	data []byte
	at   time.Time
	// arrived is when the box saw it, which is what the gap timer runs on.
	arrived time.Time
}

// ID is the segment id this stream is filling.
func (s *LiveStream) ID() string { return s.id }

// Device is the phone that opened it.
func (s *LiveStream) Device() string { return s.device }

// Chunk accepts one `audio.chunk`.
//
// The four answers, in the order they are checked, because the order is the
// contract:
//
//  1. **Consent** — refused before the bytes are stored, never filtered after.
//  2. **Duplicate** — by envelope id, or by a sequence the box already has.
//     Reported as a success.
//  3. **In order** — written, and any buffered successors written behind it.
//  4. **Out of order** — buffered, until the window or the timeout says the
//     missing chunk is not late but gone, at which point a [Gap] is declared.
func (s *LiveStream) Chunk(c Chunk) (ChunkResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ChunkResult{}, fmt.Errorf("%w: %s", ErrClosed, s.id)
	}
	at := c.At
	if at.IsZero() {
		at = s.ing.now()
	}
	if err := s.ing.consent.Check(s.device, at); err != nil {
		return ChunkResult{}, err
	}
	if c.Codec != "" {
		if s.codec == "" {
			s.codec = c.Codec
		} else if c.Codec != s.codec {
			return ChunkResult{}, fmt.Errorf("%w: %s became %s on stream %s",
				ErrCodecChanged, s.codec, c.Codec, s.id)
		}
	}

	res := ChunkResult{}

	// Dedupe first: an id we have seen, or a sequence already durable, is a
	// retry and not an error.
	if c.EnvelopeID != "" && s.recentAt[c.EnvelopeID] {
		res.Duplicate = true
		res.NextSeq, res.Pending = s.nextSeq, len(s.pending)
		return res, nil
	}
	if c.Seq < s.nextSeq {
		s.rememberLocked(c.EnvelopeID)
		res.Duplicate = true
		res.NextSeq, res.Pending = s.nextSeq, len(s.pending)
		return res, nil
	}
	if _, held := s.pending[c.Seq]; held {
		s.rememberLocked(c.EnvelopeID)
		res.Duplicate = true
		res.NextSeq, res.Pending = s.nextSeq, len(s.pending)
		return res, nil
	}

	s.rememberLocked(c.EnvelopeID)
	s.observeArrivalLocked(at)

	if c.Seq == s.nextSeq {
		if err := s.writeLocked(c.Data); err != nil {
			return res, err
		}
		res.Stored++
		s.nextSeq++
		n, err := s.drainLocked()
		if err != nil {
			return res, err
		}
		res.Stored += n
	} else {
		s.pending[c.Seq] = pendingChunk{seq: c.Seq, data: c.Data, at: at, arrived: s.ing.now()}
		res.Buffered = true
	}

	gaps, stored, err := s.reconcileLocked(s.ing.now())
	if err != nil {
		return res, err
	}
	res.Gaps = gaps
	res.Stored += stored
	res.NextSeq, res.Pending = s.nextSeq, len(s.pending)

	if len(gaps) > 0 {
		// A gap is the one thing worth an fsync mid-turn: it is the fact that
		// makes the transcript honest.
		if err := s.ing.spool.SetResume(s.id, s.nextSeq, s.recent); err != nil {
			return res, err
		}
	}
	return res, nil
}

// Tick declares gaps whose timeout has expired without a new chunk arriving.
//
// A stream that goes quiet with a hole in it would otherwise sit there: the
// timeout only fires when something else arrives to fire it. Call it from the
// same timer that drives the rest of the daemon.
func (s *LiveStream) Tick(now time.Time) (ChunkResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ChunkResult{}, fmt.Errorf("%w: %s", ErrClosed, s.id)
	}
	gaps, stored, err := s.reconcileLocked(now)
	if err != nil {
		return ChunkResult{}, err
	}
	if len(gaps) > 0 {
		if err := s.ing.spool.SetResume(s.id, s.nextSeq, s.recent); err != nil {
			return ChunkResult{}, err
		}
	}
	return ChunkResult{Stored: stored, Gaps: gaps, NextSeq: s.nextSeq, Pending: len(s.pending)}, nil
}

// Close ends the turn.
//
// Anything still buffered is written — it is real audio that arrived — and the
// hole in front of it becomes a gap, because at close there is nothing left to
// wait for. The segment moves to [StateComplete], which is deliberately not
// [StateTranscribed]: the retention clock does not start until a transcript
// exists.
func (s *LiveStream) Close(at time.Time) (Segment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return s.ing.spool.Get(s.id)
	}
	s.closed = true
	if at.IsZero() {
		at = s.ing.now()
	}

	for len(s.pending) > 0 {
		lowest := s.lowestPendingLocked()
		if lowest > s.nextSeq {
			if err := s.declareGapLocked(s.nextSeq, lowest-1, at, "the stream closed with the chunk still missing"); err != nil {
				return Segment{}, err
			}
			s.nextSeq = lowest
		}
		if _, err := s.drainLocked(); err != nil {
			return Segment{}, err
		}
	}

	if err := s.ing.spool.SetResume(s.id, s.nextSeq, s.recent); err != nil {
		return Segment{}, err
	}
	if err := s.ing.spool.MarkComplete(s.id, at); err != nil {
		return Segment{}, err
	}
	s.ing.forget(s.id)
	return s.ing.spool.Get(s.id)
}

// reconcileLocked declares any gap the window or the timeout has decided is not
// arriving, then drains whatever that unblocked.
func (s *LiveStream) reconcileLocked(now time.Time) ([]Gap, int, error) {
	var (
		gaps   []Gap
		stored int
	)
	for len(s.pending) > 0 {
		lowest := s.lowestPendingLocked()
		if lowest <= s.nextSeq {
			// Nothing is missing; drain and stop.
			n, err := s.drainLocked()
			if err != nil {
				return gaps, stored, err
			}
			stored += n
			break
		}

		reason := ""
		switch {
		case len(s.pending) >= s.ing.live.ReorderWindow:
			reason = fmt.Sprintf("%d chunks were waiting behind it, which is the whole reorder window",
				len(s.pending))
		case now.Sub(s.oldestArrivalLocked()) >= s.ing.live.GapTimeout:
			reason = fmt.Sprintf("nothing filled the hole for %s", s.ing.live.GapTimeout)
		}
		if reason == "" {
			break
		}

		if err := s.declareGapLocked(s.nextSeq, lowest-1, now, reason); err != nil {
			return gaps, stored, err
		}
		gaps = append(gaps, s.lastGapLocked())
		s.nextSeq = lowest

		n, err := s.drainLocked()
		if err != nil {
			return gaps, stored, err
		}
		stored += n
	}
	return gaps, stored, nil
}

func (s *LiveStream) lastGapLocked() Gap { return s.lastGap }

func (s *LiveStream) declareGapLocked(from, to int64, at time.Time, reason string) error {
	missing := to - from + 1
	if missing < 1 {
		return nil
	}
	g := Gap{
		FromSeq:        from,
		ToSeq:          to,
		At:             at,
		EstimatedBytes: missing * s.meanFrameBytesLocked(),
		Reason:         reason,
	}
	if iv := s.meanIntervalLocked(); iv > 0 {
		g.EstimatedDuration = time.Duration(missing) * iv
	} else {
		g.Reason += " (no arrival rate had been observed yet, so its duration is unknown rather than estimated)"
	}
	s.lastGap = g
	return s.ing.spool.AddGap(s.id, g)
}

func (s *LiveStream) meanFrameBytesLocked() int64 {
	if s.frames == 0 {
		return 0
	}
	return s.dataBytes / s.frames
}

func (s *LiveStream) meanIntervalLocked() time.Duration {
	if s.intervalCount == 0 {
		return 0
	}
	return s.intervalSum / time.Duration(s.intervalCount)
}

func (s *LiveStream) observeArrivalLocked(at time.Time) {
	if s.firstAt.IsZero() {
		s.firstAt = at
		s.lastAt = at
		return
	}
	if d := at.Sub(s.lastAt); d > 0 {
		s.intervalSum += d
		s.intervalCount++
	}
	if at.After(s.lastAt) {
		s.lastAt = at
	}
}

func (s *LiveStream) drainLocked() (int, error) {
	n := 0
	for {
		c, ok := s.pending[s.nextSeq]
		if !ok {
			return n, nil
		}
		delete(s.pending, s.nextSeq)
		if err := s.writeLocked(c.data); err != nil {
			return n, err
		}
		s.nextSeq++
		n++
	}
}

func (s *LiveStream) writeLocked(data []byte) error {
	if _, err := s.ing.spool.Append(s.id, data); err != nil {
		return err
	}
	s.frames++
	s.dataBytes += int64(len(data))
	return nil
}

func (s *LiveStream) lowestPendingLocked() int64 {
	lowest := int64(-1)
	for seq := range s.pending {
		if lowest < 0 || seq < lowest {
			lowest = seq
		}
	}
	return lowest
}

func (s *LiveStream) oldestArrivalLocked() time.Time {
	var oldest time.Time
	for _, c := range s.pending {
		if oldest.IsZero() || c.arrived.Before(oldest) {
			oldest = c.arrived
		}
	}
	return oldest
}

func (s *LiveStream) rememberLocked(id string) {
	if id == "" {
		return
	}
	if s.recentAt[id] {
		return
	}
	s.recentAt[id] = true
	s.recent = append(s.recent, id)
	if len(s.recent) > s.ing.live.RecentIDs {
		drop := s.recent[0]
		s.recent = s.recent[1:]
		delete(s.recentAt, drop)
	}
}

// Pending is how many chunks are waiting on a missing predecessor. Exported for
// the health surface: a number that stays above zero is a link that is losing
// chunks.
func (s *LiveStream) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

// NextSeq is the sequence the box wants next, which is what a resuming phone
// asks for.
func (s *LiveStream) NextSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextSeq
}

// sortedPending is a test and debug helper: the buffered sequences, in order.
func (s *LiveStream) sortedPending() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int64, 0, len(s.pending))
	for seq := range s.pending {
		out = append(out, seq)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
