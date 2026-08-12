package capture

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/logx"
)

func hashOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// A day, in miniature: a manifest, chunks that are exactly chunkBytes until the
// remainder, and a completion that refuses while anything is missing.
func nightManifest(id string, startedAt time.Time, total, chunkBytes int64) Manifest {
	return Manifest{
		SessionID:   id,
		Kind:        "audio",
		StartedAtMS: startedAt.UnixMilli(),
		DurationS:   3600,
		TotalBytes:  total,
		ChunkBytes:  chunkBytes,
		Encoding:    "opus",
		SourceName:  "REC0001.opus",
	}
}

func TestChunkCountMatchesTheTypeScript(t *testing.T) {
	// `chunkCount()` in connector/src/protocol.ts, including its stated edge
	// case: "Zero-byte sessions are one empty chunk" — the code returns 0, and
	// the code is the contract three clients implement.
	cases := []struct {
		total, chunk int64
		want         int
	}{
		{0, 256, 0},
		{1, 256, 1},
		{256, 256, 1},
		{257, 256, 2},
		{1024, 256, 4},
	}
	for _, tc := range cases {
		m := Manifest{TotalBytes: tc.total, ChunkBytes: tc.chunk}
		if got := m.ChunkCount(); got != tc.want {
			t.Fatalf("ChunkCount(%d/%d) = %d, want %d", tc.total, tc.chunk, got, tc.want)
		}
	}
}

func TestBulkUploadResumesFromWhatTheBoxHas(t *testing.T) {
	// The gate opens in the morning, the audio is recorded in the afternoon, and
	// the sync lands at three the next morning — which is the real sequence and
	// the reason consent is checked against the recording time.
	c := &clock{t: at("2026-08-10T08:00:00Z")}
	ing := newIngester(t, c)
	b := ing.Bulk()

	recorded := at("2026-08-10T14:00:00Z")
	c.t = at("2026-08-11T03:00:00Z")
	body := bytes.Repeat([]byte("abcd"), 250) // 1000 bytes
	m := nightManifest("night-1", recorded, int64(len(body)), 256)

	st, err := b.Declare("phone", m)
	if err != nil {
		t.Fatal(err)
	}
	if st.ExpectedChunks != 4 {
		t.Fatalf("ExpectedChunks = %d, want 4", st.ExpectedChunks)
	}
	if st.NextChunk == nil || *st.NextChunk != 0 {
		t.Fatalf("NextChunk = %v, want 0", st.NextChunk)
	}

	put := func(seq int) (PutResult, error) {
		lo := seq * 256
		hi := lo + 256
		if hi > len(body) {
			hi = len(body)
		}
		part := body[lo:hi]
		return b.PutChunk("night-1", seq, part, hashOf(part))
	}

	// The phone dies after 0 and 1.
	for _, seq := range []int{0, 1} {
		if _, err := put(seq); err != nil {
			t.Fatalf("chunk %d: %v", seq, err)
		}
	}
	if _, err := b.Complete("night-1", c.t); err == nil {
		t.Fatal("Complete must refuse while chunks are missing — the box is the last place the day exists in one piece")
	}

	st, err = b.Status("night-1")
	if err != nil {
		t.Fatal(err)
	}
	if st.NextChunk == nil || *st.NextChunk != 2 {
		t.Fatalf("NextChunk = %v, want 2 so a resuming phone sends only what is missing", st.NextChunk)
	}
	if len(st.ReceivedChunks) != 2 {
		t.Fatalf("ReceivedChunks = %v", st.ReceivedChunks)
	}

	// It comes back, re-declares (idempotent) and finishes — out of order,
	// which is allowed because chunks are addressed rather than sequenced.
	if _, err := b.Declare("phone", m); err != nil {
		t.Fatalf("re-declaring must be idempotent: %v", err)
	}
	for _, seq := range []int{3, 2} {
		if _, err := put(seq); err != nil {
			t.Fatalf("chunk %d: %v", seq, err)
		}
	}
	// And it re-sends one it already delivered, because the ack was lost.
	res, err := put(1)
	if err != nil {
		t.Fatalf("a re-sent chunk must be a success: %v", err)
	}
	if !res.Duplicate {
		t.Fatal("a re-sent chunk should be reported as a duplicate")
	}

	st, err = b.Complete("night-1", c.t)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Complete || st.NextChunk != nil {
		t.Fatalf("status after complete = %+v", st)
	}

	r, err := ing.Spool().Reader("night-1")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("reassembled %d bytes, want the original %d", len(got), len(body))
	}

	// The night is dated by when it was recorded, not by when it was uploaded.
	seg, err := ing.Spool().Get("night-1")
	if err != nil {
		t.Fatal(err)
	}
	if !seg.StartedAt.Equal(recorded.UTC()) {
		t.Fatalf("StartedAt = %s, want the recording time %s", seg.StartedAt, recorded)
	}
	if want := recorded.UTC().Add(time.Hour); !seg.EndedAt.Equal(want) {
		t.Fatalf("EndedAt = %s, want start + the manifest's duration (%s)", seg.EndedAt, want)
	}
	if seg.State != StateComplete {
		t.Fatalf("state = %s — a completed upload still has no transcript", seg.State)
	}
}

func TestBulkRefusesCorruptedAndConflictingChunks(t *testing.T) {
	c := &clock{t: at("2026-08-10T08:00:00Z")}
	ing := newIngester(t, c)
	c.t = at("2026-08-11T03:00:00Z")
	b := ing.Bulk()
	body := bytes.Repeat([]byte("z"), 512)
	m := nightManifest("night-2", at("2026-08-10T14:00:00Z"), int64(len(body)), 256)
	if _, err := b.Declare("phone", m); err != nil {
		t.Fatal(err)
	}

	first := body[:256]
	if _, err := b.PutChunk("night-2", 0, first, hashOf([]byte("something else"))); !errors.Is(err, ErrChunkMismatch) {
		t.Fatalf("bad hash = %v, want ErrChunkMismatch", err)
	}
	if _, err := b.PutChunk("night-2", 0, first, hashOf(first)); err != nil {
		t.Fatal(err)
	}

	// Same index, different bytes: two recordings share one session id, or
	// something is corrupting the stream. Refuse rather than silently pick.
	other := bytes.Repeat([]byte("y"), 256)
	if _, err := b.PutChunk("night-2", 0, other, hashOf(other)); !errors.Is(err, ErrChunkMismatch) {
		t.Fatalf("conflicting chunk = %v, want ErrChunkMismatch", err)
	}

	if _, err := b.PutChunk("night-2", 9, first, hashOf(first)); !errors.Is(err, ErrBadRequest) {
		t.Fatal("a sequence outside the manifest must be refused")
	}
	short := body[:100]
	if _, err := b.PutChunk("night-2", 1, short, hashOf(short)); !errors.Is(err, ErrBadRequest) {
		t.Fatal("a chunk that is not the size the manifest implies must be refused; padding shifts every byte after it")
	}
	if _, err := b.PutChunk("no-such-session", 0, first, hashOf(first)); !errors.Is(err, ErrUnknownSession) {
		t.Fatal("an unknown session must be refused by name")
	}
}

func TestOfferDefersOffLANRatherThanRefusing(t *testing.T) {
	c := &clock{t: at("2026-08-11T03:00:00Z")}
	ing := newIngester(t, c)
	b := ing.Bulk()

	plan := b.Offer(Offer{Device: "phone", Files: 64, Bytes: 173 << 20, OnLAN: false, At: c.t, CapturedAt: c.t})
	if plan.Accept {
		t.Fatal("bulk sync must not ride cellular")
	}
	if !plan.Defer {
		t.Fatal("off-LAN is 'not now', not 'no' — a phone told 'no' may delete the day")
	}
	if plan.Reason == "" {
		t.Fatal("the app has to be able to say why it is waiting")
	}

	plan = b.Offer(Offer{Device: "phone", Files: 64, Bytes: 173 << 20, OnLAN: true, At: c.t, CapturedAt: c.t})
	if !plan.Accept || plan.Defer {
		t.Fatalf("on the LAN with room, the offer should be accepted: %+v", plan)
	}
	if plan.ChunkBytes != ChunkBytes {
		t.Fatalf("ChunkBytes = %d, want the protocol's %d", plan.ChunkBytes, ChunkBytes)
	}
}

func TestOfferRefusesWhatWillNotFit(t *testing.T) {
	c := &clock{t: at("2026-08-11T03:00:00Z")}
	sp, err := OpenSpool(SpoolOptions{Dir: t.TempDir(), Capacity: 1 << 20, Now: c.now, Log: logx.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(GateOptions{Scope: ScopeAlways, IndicatorVisible: true, Now: c.now})
	ing, err := New(Options{Spool: sp, Consent: reg, Now: c.now, Log: logx.Discard()})
	if err != nil {
		t.Fatal(err)
	}

	plan := ing.Bulk().Offer(Offer{Device: "phone", Bytes: 1 << 30, OnLAN: true, At: c.t, CapturedAt: c.t})
	if plan.Accept || plan.Defer {
		t.Fatalf("a night that will not fit is a refusal, not a wait: %+v", plan)
	}
	if plan.Reason == "" {
		t.Fatal("the refusal has to say how much room there is")
	}

	// And the refusal is enforced, not advisory.
	m := nightManifest("too-big", c.t, 1<<30, ChunkBytes)
	if _, err := ing.Bulk().Declare("phone", m); !errors.Is(err, ErrStorageFull) {
		t.Fatalf("Declare = %v, want ErrStorageFull", err)
	}
}

// The counterpart of the live-path test, and the one that matters more: a
// night's audio is judged against the consent in force when it was recorded.
func TestBulkIsRefusedWhenConsentDidNotCoverTheRecording(t *testing.T) {
	c := &clock{t: at("2026-08-10T16:00:00Z")}
	sp, _ := newSpool(t, c)
	reg := NewRegistry(GateOptions{Scope: ScopeNone, IndicatorVisible: true, Now: c.now})
	ing, err := New(Options{Spool: sp, Consent: reg, Now: c.now, Log: logx.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Gate("phone").Grant(ScopeAlways); err != nil {
		t.Fatal(err)
	}
	c.add(11 * time.Hour) // 03:00, the sync

	before := nightManifest("morning", at("2026-08-10T09:00:00Z"), 256, 256)
	if _, err := ing.Bulk().Declare("phone", before); !errors.Is(err, ErrNoConsent) {
		t.Fatalf("Declare of audio recorded before consent = %v, want a refusal", err)
	}
	if got := sp.List(); len(got) != 0 {
		t.Fatalf("a refused night must not leave a segment behind: %+v", got)
	}

	after := nightManifest("evening", at("2026-08-10T17:00:00Z"), 256, 256)
	if _, err := ing.Bulk().Declare("phone", after); err != nil {
		t.Fatalf("audio recorded under consent should be accepted: %v", err)
	}

	// An offer that carries no capture time is refused rather than assumed
	// covered by whatever is granted now.
	plan := ing.Bulk().Offer(Offer{Device: "phone", Bytes: 1024, OnLAN: true, At: c.t, CapturedAt: at("2026-08-10T09:00:00Z")})
	if plan.Accept {
		t.Fatal("an offer of audio from before consent must not be accepted")
	}
}

// 1.8 GB of PCM is 7,360 chunks. The resume state has to be a bitmap, not a
// list of seven thousand integers rewritten on every chunk.
func TestResumeStateStaysSmallForAWholeDay(t *testing.T) {
	c := &clock{t: at("2026-08-10T05:00:00Z")}
	ing := newIngester(t, c)
	c.t = at("2026-08-11T03:00:00Z")
	// APPS-SCOPE.md §3.1: a 16-hour day at 16 kHz, 16-bit, mono is ~1.84 GB.
	const day = int64(16 * 3600 * 16000 * 2)
	m := nightManifest("pcm-day", at("2026-08-10T06:00:00Z"), day, ChunkBytes)
	st, err := ing.Bulk().Declare("phone", m)
	if err != nil {
		t.Fatal(err)
	}
	if st.ExpectedChunks < 7000 || st.ExpectedChunks > 8000 {
		t.Fatalf("ExpectedChunks = %d, want ~7,360 for a PCM day at 256 KB", st.ExpectedChunks)
	}
	bitmap := ensureBitmap(nil, st.ExpectedChunks)
	if len(bitmap) > 1024 {
		t.Fatalf("resume bitmap is %d bytes for a whole day", len(bitmap))
	}
	for i := 0; i < st.ExpectedChunks; i++ {
		if bitGet(bitmap, i) {
			t.Fatalf("bit %d set in a fresh bitmap", i)
		}
		bitSet(bitmap, i)
		if !bitGet(bitmap, i) {
			t.Fatalf("bit %d did not set", i)
		}
	}
}

// A resumable uploader sends chunks in parallel. Two goroutines each reading
// the received bitmap, setting a bit and writing it back would lose one — and a
// lost bit is a chunk the box has and does not know it has, so the phone
// re-sends it forever and the night never completes.
func TestParallelChunksDoNotLoseBitsFromTheBitmap(t *testing.T) {
	c := &clock{t: at("2026-08-10T08:00:00Z")}
	ing := newIngester(t, c)
	c.t = at("2026-08-11T03:00:00Z")
	b := ing.Bulk()

	const chunkSize = 256
	const chunks = 32
	body := bytes.Repeat([]byte("q"), chunkSize*chunks)
	m := nightManifest("parallel", at("2026-08-10T14:00:00Z"), int64(len(body)), chunkSize)
	if _, err := b.Declare("phone", m); err != nil {
		t.Fatal(err)
	}

	errs := make(chan error, chunks)
	var wg sync.WaitGroup
	for i := 0; i < chunks; i++ {
		wg.Add(1)
		go func(seq int) {
			defer wg.Done()
			part := body[seq*chunkSize : (seq+1)*chunkSize]
			_, err := b.PutChunk("parallel", seq, part, hashOf(part))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	st, err := b.Status("parallel")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.ReceivedChunks) != chunks {
		t.Fatalf("the box holds %d of %d chunks; a bit was lost to a race", len(st.ReceivedChunks), chunks)
	}
	if _, err := b.Complete("parallel", c.t); err != nil {
		t.Fatalf("Complete = %v", err)
	}
}
