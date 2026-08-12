package capture

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// The bulk path — APPS-SCOPE.md §3's Path B, and the reason this package has
// two of everything.
//
// A day is 170 MB as Opus and 1.84 GB as PCM (APPS-SCOPE.md §3.1). Over BLE
// that is sixteen hours to a week, so it cannot ride the control socket; it
// comes over the glasses' access point to the phone and then over the LAN to the
// box, as a *designed ritual* while both devices charge. SYSTEM.md §7: "If they
// are not on the same LAN, bulk sync waits. That is the correct tradeoff and the
// app should say so rather than silently burning someone's data plan."
//
// **The wire contract is `connector/src/protocol.ts` and is not re-invented
// here.** Three implementations have to agree on it — that file, the Kotlin
// client and the Swift one — so [Manifest], [Status], [ChunkBytes],
// [MaxChunkBytes] and the error strings below mirror it field for field.
//
// Where this differs from the TypeScript reference is *where the bytes go*. That
// implementation holds chunks in a `Map<number, Uint8Array>`, which is correct
// for a test double and would be 1.8 GB of heap here. So a chunk is written at
// `sequence * chunkBytes` into one file and the received-set is a bitmap: 7,200
// chunks of a PCM day is 900 bytes of state, and resume after a crash is a file
// read rather than a re-upload.

// ChunkBytes is 256 KB, from `connector/src/protocol.ts`.
//
// Its comment there is the reasoning and it is worth keeping: chosen against the
// failure mode rather than the throughput, because an interrupted chunk is
// re-sent whole, so the chunk size is the amount of work lost per drop.
const ChunkBytes int64 = 256 * 1024

// MaxChunkBytes is 4 MB. Anything larger is a bug or an attack, not a recording.
const MaxChunkBytes int64 = 4 * 1024 * 1024

// Manifest declares a capture session before its bytes arrive, so the box can
// refuse a session it has no room for *before* the phone spends an hour of radio
// on it. Mirrors `SessionManifest`.
type Manifest struct {
	SessionID string `json:"sessionId"`
	// Kind is "audio" or "photo".
	Kind string `json:"kind"`
	// StartedAtMS is the device clock at the first sample. It is what consent
	// is judged against: a night's audio is offered hours after it was recorded
	// and the only honest question is whether consent covered the recording.
	StartedAtMS int64   `json:"startedAtMs"`
	DurationS   float64 `json:"durationS"`
	TotalBytes  int64   `json:"totalBytes"`
	ChunkBytes  int64   `json:"chunkBytes"`
	// Encoding is "opus" for audio, "image/jpeg" for photos.
	Encoding string `json:"encoding"`
	// SourceName is the name on the glasses, so a partial sync can be
	// reconciled with the device.
	SourceName string `json:"sourceName"`
}

// StartedAt is the manifest's start time as a time.Time.
func (m Manifest) StartedAt() time.Time {
	if m.StartedAtMS == 0 {
		return time.Time{}
	}
	return time.UnixMilli(m.StartedAtMS).UTC()
}

// ChunkCount is `chunkCount()` from the TypeScript, including its edge case:
// a zero-byte session is zero chunks.
func (m Manifest) ChunkCount() int {
	if m.TotalBytes <= 0 {
		return 0
	}
	n := m.ChunkBytes
	if n <= 0 {
		n = ChunkBytes
	}
	return int((m.TotalBytes + n - 1) / n)
}

// Status is what the box has, so a phone that died at chunk 37 of 40 can send
// only what is missing. Mirrors `SessionStatus`.
//
// The received-chunk set, not the byte count, is the unit of truth: a byte count
// cannot express "I have 1–20 and 25–37".
type Status struct {
	SessionID      string `json:"sessionId"`
	ReceivedChunks []int  `json:"receivedChunks"`
	ExpectedChunks int    `json:"expectedChunks"`
	Complete       bool   `json:"complete"`
	// NextChunk is the first missing index, so the common case — a client that
	// never had a gap — does not have to reason about the set. Nil when nothing
	// is missing.
	NextChunk *int `json:"nextChunk"`
}

// PutResult is one chunk's outcome. Duplicate is a success.
type PutResult struct {
	Duplicate bool
	Status    Status
}

// Wire error codes, from `connector/src/protocol.ts`'s ErrorCode. They are
// values rather than free text because the phone branches on them: a
// `storageFull` keeps the day on the glasses, a `chunkMismatch` re-sends one
// chunk, and telling the two apart is the difference between losing a day and
// losing 256 KB.
var (
	ErrUnknownSession = errors.New("unknownSession")
	ErrChunkMismatch  = errors.New("chunkMismatch")
	ErrBadRequest     = errors.New("badRequest")
)

// Offer is `sync.offer` from SYSTEM.md §6.1: the phone saying it has a night to
// hand over.
type Offer struct {
	Device string
	Files  int
	Bytes  int64
	// OnLAN is whether the phone and the box share a network. SYSTEM.md §7:
	// bulk sync waits rather than silently burning a data plan.
	OnLAN bool
	// At is when the offer was made; CapturedAt is when the audio was recorded,
	// and it is the one consent is judged against. A phone that does not send
	// it gets a refusal rather than the benefit of the doubt.
	At         time.Time
	CapturedAt time.Time
}

// Plan is the box's answer to an offer. It is deliberately three-valued:
// "accept", "not now" and "no" lead to different behaviour on the phone, and
// collapsing the middle one into either of the others is how a phone either
// deletes a day it should have kept or retries forever on a refusal.
type Plan struct {
	Accept bool
	// Defer means try again later — the conditions, not the request, are wrong.
	Defer  bool
	Reason string
	// ChunkBytes is the size the box wants, so the phone does not have to
	// hardcode it.
	ChunkBytes int64
	FreeBytes  int64
}

// Bulk is the nightly sync's box-side half.
type Bulk struct{ ing *Ingester }

// Offer answers `sync.offer`.
func (b *Bulk) Offer(o Offer) Plan {
	plan := Plan{ChunkBytes: ChunkBytes, FreeBytes: b.ing.spool.Free()}

	captured := o.CapturedAt
	if captured.IsZero() {
		captured = o.At
	}
	if err := b.ing.consent.Check(o.Device, captured); err != nil {
		var ce *ErrConsent
		if errors.As(err, &ce) {
			plan.Reason = ce.Decision.Why
		} else {
			plan.Reason = err.Error()
		}
		return plan
	}
	if !o.OnLAN {
		plan.Defer = true
		plan.Reason = "the phone and this machine are not on the same network, so bulk sync waits rather than spending a data plan on it (SYSTEM.md §7)"
		return plan
	}
	if o.Bytes > plan.FreeBytes {
		plan.Reason = fmt.Sprintf("%s offered and %s free — keep it on the glasses",
			bytesHuman(o.Bytes), bytesHuman(plan.FreeBytes))
		return plan
	}
	plan.Accept = true
	plan.Reason = fmt.Sprintf("on the LAN with room for %s", bytesHuman(plan.FreeBytes))
	return plan
}

// Declare accepts a manifest and returns what the box already has.
//
// Idempotent, for the reason `connector/src/store.ts` states: a phone that lost
// the response and retried must not create a second session or clobber the
// chunks already uploaded, which is exactly what a naive overwrite would do at
// the worst possible moment.
func (b *Bulk) Declare(device string, m Manifest) (Status, error) {
	if strings.TrimSpace(m.SessionID) == "" {
		return Status{}, fmt.Errorf("%w: manifest has no session id", ErrBadRequest)
	}
	if m.TotalBytes < 0 {
		return Status{}, fmt.Errorf("%w: negative totalBytes", ErrBadRequest)
	}
	if m.ChunkBytes <= 0 {
		m.ChunkBytes = ChunkBytes
	}
	if m.ChunkBytes > MaxChunkBytes {
		return Status{}, fmt.Errorf("%w: chunkBytes %d is above the %d ceiling",
			ErrBadRequest, m.ChunkBytes, MaxChunkBytes)
	}
	captured := m.StartedAt()
	if err := b.ing.consent.Check(device, captured); err != nil {
		return Status{}, err
	}

	seg, err := b.ing.spool.Create(SegmentSpec{
		ID:            m.SessionID,
		Device:        device,
		Kind:          KindBulk,
		Codec:         m.Encoding,
		StartedAt:     captured,
		ExpectedBytes: m.TotalBytes,
		ChunkBytes:    m.ChunkBytes,
		RecordedFor:   time.Duration(m.DurationS * float64(time.Second)),
		Framed:        false,
		Source:        m.SourceName,
	})
	if err != nil {
		return Status{}, err
	}
	return b.status(seg, m.ChunkCount()), nil
}

// PutChunk stores one chunk after verifying its hash.
//
// The three refusals, and each one exists because the alternative is silent
// corruption of somebody's day:
//
//   - a sequence outside the manifest's range;
//   - a body whose SHA-256 is not the declared `contentHash`;
//   - a chunk index already stored with *different* bytes, which means two
//     recordings were assigned one session id. The stored copy is no more
//     trustworthy than the new one, so it refuses rather than picking.
func (b *Bulk) PutChunk(sessionID string, sequence int, body []byte, contentHash string) (PutResult, error) {
	seg, err := b.ing.spool.Get(sessionID)
	if errors.Is(err, ErrNoSegment) {
		return PutResult{}, fmt.Errorf("%w: %s", ErrUnknownSession, sessionID)
	}
	if err != nil {
		return PutResult{}, err
	}
	if seg.Kind != KindBulk {
		return PutResult{}, fmt.Errorf("%w: %s is a live stream", ErrBadRequest, sessionID)
	}
	if int64(len(body)) > MaxChunkBytes {
		return PutResult{}, fmt.Errorf("%w: %d bytes is above the %d ceiling",
			ErrBadRequest, len(body), MaxChunkBytes)
	}

	total := chunkCountFor(seg)
	if sequence < 0 || sequence >= total {
		return PutResult{}, fmt.Errorf("%w: sequence %d outside 0..%d", ErrBadRequest, sequence, total-1)
	}

	sum := sha256.Sum256(body)
	actual := hex.EncodeToString(sum[:])
	if !strings.EqualFold(actual, contentHash) {
		return PutResult{}, fmt.Errorf("%w: content hash mismatch on chunk %d", ErrChunkMismatch, sequence)
	}

	// Every chunk but the last is exactly chunkBytes; the last is the
	// remainder. A client that pads or truncates would otherwise shift every
	// byte after it.
	want := seg.ChunkBytes
	if sequence == total-1 {
		want = seg.ExpectedBytes - int64(sequence)*seg.ChunkBytes
	}
	if int64(len(body)) != want {
		return PutResult{}, fmt.Errorf("%w: chunk %d is %d bytes, the manifest implies %d",
			ErrBadRequest, sequence, len(body), want)
	}

	bitmap := ensureBitmap(seg.Received, total)
	if bitGet(bitmap, sequence) {
		stored, err := b.readChunk(seg, sequence, int(want))
		if err != nil {
			return PutResult{}, err
		}
		storedSum := sha256.Sum256(stored)
		if hex.EncodeToString(storedSum[:]) != actual {
			return PutResult{}, fmt.Errorf("%w: chunk %d is already stored with different content",
				ErrChunkMismatch, sequence)
		}
		return PutResult{Duplicate: true, Status: b.status(seg, total)}, nil
	}

	off := int64(sequence) * seg.ChunkBytes
	if err := b.ing.spool.WriteAt(sessionID, off, body); err != nil {
		return PutResult{}, err
	}
	// The bit is set under the spool's lock, not here: chunks arrive in
	// parallel and a read-modify-write in this function loses one of two
	// concurrent bits. See [Spool.MarkReceived].
	if _, err := b.ing.spool.MarkReceived(sessionID, sequence, total); err != nil {
		return PutResult{}, err
	}

	seg, err = b.ing.spool.Get(sessionID)
	if err != nil {
		return PutResult{}, err
	}
	return PutResult{Status: b.status(seg, total)}, nil
}

// Status reports what the box has.
func (b *Bulk) Status(sessionID string) (Status, error) {
	seg, err := b.ing.spool.Get(sessionID)
	if errors.Is(err, ErrNoSegment) {
		return Status{}, fmt.Errorf("%w: %s", ErrUnknownSession, sessionID)
	}
	if err != nil {
		return Status{}, err
	}
	return b.status(seg, chunkCountFor(seg)), nil
}

// Complete seals a session.
//
// It refuses while chunks are missing, and the refusal names them. The box is
// the last place the day's audio exists in one piece, so "complete" has to mean
// it rather than "the client said so".
func (b *Bulk) Complete(sessionID string, at time.Time) (Status, error) {
	seg, err := b.ing.spool.Get(sessionID)
	if errors.Is(err, ErrNoSegment) {
		return Status{}, fmt.Errorf("%w: %s", ErrUnknownSession, sessionID)
	}
	if err != nil {
		return Status{}, err
	}

	total := chunkCountFor(seg)
	st := b.status(seg, total)
	if len(st.ReceivedChunks) != total {
		missing := missingChunks(seg.Received, total)
		return st, fmt.Errorf("%w: %d of %d chunks are missing, starting at %d",
			ErrBadRequest, len(missing), total, missing[0])
	}
	if at.IsZero() {
		at = b.ing.now()
	}
	// End the segment at start + the manifest's duration, never at the arrival
	// time. The night's sync lands hours after the recording, and dating a
	// segment by when it was uploaded would put every episode of the day at
	// 3 a.m. — which is exactly the kind of quietly-wrong metadata that makes a
	// memory untrustworthy.
	end := at
	if !seg.StartedAt.IsZero() && seg.RecordedFor > 0 {
		end = seg.StartedAt.Add(seg.RecordedFor)
	}
	if err := b.ing.spool.MarkComplete(sessionID, end); err != nil {
		return st, err
	}
	seg, err = b.ing.spool.Get(sessionID)
	if err != nil {
		return st, err
	}
	return b.status(seg, total), nil
}

func (b *Bulk) readChunk(seg Segment, sequence, size int) ([]byte, error) {
	r, err := b.ing.spool.Reader(seg.ID)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	if _, err := r.Seek(int64(sequence)*seg.ChunkBytes, io.SeekStart); err != nil {
		return nil, fmt.Errorf("capture: seek chunk %d: %w", sequence, err)
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("capture: read chunk %d: %w", sequence, err)
	}
	return buf, nil
}

func (b *Bulk) status(seg Segment, total int) Status {
	bitmap := ensureBitmap(seg.Received, total)
	st := Status{
		SessionID:      seg.ID,
		ExpectedChunks: total,
		Complete:       seg.State != StateReceiving,
		ReceivedChunks: []int{},
	}
	for i := 0; i < total; i++ {
		if bitGet(bitmap, i) {
			st.ReceivedChunks = append(st.ReceivedChunks, i)
			continue
		}
		if st.NextChunk == nil {
			n := i
			st.NextChunk = &n
		}
	}
	sort.Ints(st.ReceivedChunks)
	return st
}

func chunkCountFor(seg Segment) int {
	if seg.ExpectedBytes <= 0 {
		return 0
	}
	n := seg.ChunkBytes
	if n <= 0 {
		n = ChunkBytes
	}
	return int((seg.ExpectedBytes + n - 1) / n)
}

func missingChunks(bitmap []byte, total int) []int {
	var out []int
	for i := 0; i < total; i++ {
		if !bitGet(bitmap, i) {
			out = append(out, i)
		}
	}
	return out
}

// The bitmap is the whole reason this scales: a 1.84 GB PCM day at 256 KB per
// chunk is 7,360 chunks, which is 920 bytes of resume state rather than a JSON
// array of seven thousand integers rewritten on every chunk.
func ensureBitmap(b []byte, total int) []byte {
	need := (total + 7) / 8
	if len(b) >= need {
		return append([]byte(nil), b...)
	}
	out := make([]byte, need)
	copy(out, b)
	return out
}

func bitGet(b []byte, i int) bool {
	idx := i / 8
	if idx >= len(b) {
		return false
	}
	return b[idx]&(1<<(uint(i)%8)) != 0
}

func bitSet(b []byte, i int) {
	idx := i / 8
	if idx >= len(b) {
		return
	}
	b[idx] |= 1 << (uint(i) % 8)
}

func bytesHuman(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}
