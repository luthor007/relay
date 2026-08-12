package capture

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/logx"
)

func newIngester(t *testing.T, c *clock, opts ...func(*Options)) *Ingester {
	t.Helper()
	sp, _ := newSpool(t, c)
	reg := NewRegistry(GateOptions{Scope: ScopeAlways, IndicatorVisible: true, Now: c.now})
	o := Options{Spool: sp, Consent: reg, Now: c.now, Log: logx.Discard()}
	for _, f := range opts {
		f(&o)
	}
	ing, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return ing
}

func chunk(id string, seq int64, body string, at time.Time) Chunk {
	return Chunk{EnvelopeID: id, Seq: seq, Codec: "opus", Data: []byte(body), At: at}
}

func TestLiveStreamStoresChunksInOrder(t *testing.T) {
	c := &clock{t: at("2026-08-10T09:00:00Z")}
	ing := newIngester(t, c)

	s, err := ing.OpenLive(LiveSpec{Device: "phone", Codec: "opus", StartedAt: c.t, UserInitiated: true})
	if err != nil {
		t.Fatal(err)
	}
	for i, body := range []string{"a", "b", "c"} {
		c.add(20 * time.Millisecond)
		res, err := s.Chunk(chunk(fmt.Sprintf("env-%d", i), int64(i), body, c.t))
		if err != nil {
			t.Fatal(err)
		}
		if res.Stored != 1 || res.Duplicate || res.Buffered {
			t.Fatalf("chunk %d: %+v", i, res)
		}
	}
	seg, err := s.Close(c.t)
	if err != nil {
		t.Fatal(err)
	}
	if seg.State != StateComplete {
		t.Fatalf("state = %s, want complete — a closed turn has no transcript yet", seg.State)
	}
	frames, err := ing.Spool().Frames(seg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 || string(frames[0]) != "a" || string(frames[2]) != "c" {
		t.Fatalf("frames = %q", frames)
	}
}

// The link is at-least-once by construction: `glasses/bridge/src/relayd.ts` puts
// in-flight envelopes back at the *head* of the outbox on an abnormal close and
// says "the server side dedupes on it". This is that.
func TestDuplicateEnvelopesAreASuccessAndAreStoredOnce(t *testing.T) {
	c := &clock{t: at("2026-08-10T09:00:00Z")}
	ing := newIngester(t, c)
	s, err := ing.OpenLive(LiveSpec{Device: "phone", Codec: "opus", StartedAt: c.t, UserInitiated: true})
	if err != nil {
		t.Fatal(err)
	}

	first := chunk("env-0", 0, "hello", c.t)
	if res, err := s.Chunk(first); err != nil || res.Stored != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	// Same envelope id, replayed.
	res, err := s.Chunk(first)
	if err != nil {
		t.Fatalf("a replayed envelope must not be an error: %v", err)
	}
	if !res.Duplicate || res.Stored != 0 {
		t.Fatalf("replay = %+v, want a duplicate that stored nothing", res)
	}
	// Different envelope id, same sequence the box already has durably — the
	// shape a client takes when it re-sends after a lost ack.
	res, err = s.Chunk(chunk("env-0-again", 0, "hello", c.t))
	if err != nil {
		t.Fatalf("a re-sent sequence must not be an error: %v", err)
	}
	if !res.Duplicate {
		t.Fatalf("re-sent sequence = %+v, want a duplicate", res)
	}

	if _, err := s.Close(c.t); err != nil {
		t.Fatal(err)
	}
	frames, _ := ing.Spool().Frames(s.ID())
	if len(frames) != 1 {
		t.Fatalf("stored %d frames for one chunk sent three times", len(frames))
	}
}

func TestOutOfOrderChunksAreBufferedThenDrained(t *testing.T) {
	c := &clock{t: at("2026-08-10T09:00:00Z")}
	ing := newIngester(t, c)
	s, err := ing.OpenLive(LiveSpec{Device: "phone", Codec: "opus", StartedAt: c.t, UserInitiated: true})
	if err != nil {
		t.Fatal(err)
	}

	c.add(20 * time.Millisecond)
	if res, _ := s.Chunk(chunk("e2", 2, "c", c.t)); !res.Buffered || res.Stored != 0 {
		t.Fatalf("chunk 2 arriving first = %+v, want buffered", res)
	}
	c.add(20 * time.Millisecond)
	if res, _ := s.Chunk(chunk("e1", 1, "b", c.t)); !res.Buffered {
		t.Fatalf("chunk 1 = %+v, want buffered", res)
	}
	if got := s.sortedPending(); len(got) != 2 {
		t.Fatalf("pending = %v", got)
	}
	c.add(20 * time.Millisecond)
	res, err := s.Chunk(chunk("e0", 0, "a", c.t))
	if err != nil {
		t.Fatal(err)
	}
	if res.Stored != 3 {
		t.Fatalf("the missing head should have drained all three, stored %d", res.Stored)
	}
	if _, err := s.Close(c.t); err != nil {
		t.Fatal(err)
	}
	frames, _ := ing.Spool().Frames(s.ID())
	if len(frames) != 3 || string(frames[0]) != "a" || string(frames[1]) != "b" || string(frames[2]) != "c" {
		t.Fatalf("frames = %q, want a b c in order", frames)
	}
}

// The rule the whole pipeline turns on: never emit what you cannot observe.
// Splicing the frames that arrived would produce a transcript that reads as
// continuous and is not.
func TestALostChunkBecomesAVisibleGap(t *testing.T) {
	c := &clock{t: at("2026-08-10T09:00:00Z")}
	ing := newIngester(t, c, func(o *Options) { o.Live.GapTimeout = time.Second })
	s, err := ing.OpenLive(LiveSpec{Device: "phone", Codec: "opus", StartedAt: c.t, UserInitiated: true})
	if err != nil {
		t.Fatal(err)
	}

	c.add(20 * time.Millisecond)
	if _, err := s.Chunk(chunk("e0", 0, "aaaa", c.t)); err != nil {
		t.Fatal(err)
	}
	c.add(20 * time.Millisecond)
	if _, err := s.Chunk(chunk("e1", 1, "bbbb", c.t)); err != nil {
		t.Fatal(err)
	}
	// 2 and 3 never arrive. 4 does.
	c.add(20 * time.Millisecond)
	if res, _ := s.Chunk(chunk("e4", 4, "eeee", c.t)); !res.Buffered {
		t.Fatalf("chunk 4 = %+v, want buffered while 2 and 3 might still be late", res)
	}

	// Nothing else arrives; the timer is what declares the hole.
	c.add(2 * time.Second)
	res, err := s.Tick(c.t)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Gaps) != 1 {
		t.Fatalf("Gaps = %+v, want one", res.Gaps)
	}
	g := res.Gaps[0]
	if g.FromSeq != 2 || g.ToSeq != 3 {
		t.Fatalf("gap covers %d..%d, want 2..3", g.FromSeq, g.ToSeq)
	}
	if g.EstimatedBytes != 8 {
		t.Fatalf("EstimatedBytes = %d, want 2 frames at the observed 4-byte mean", g.EstimatedBytes)
	}
	if g.EstimatedDuration != 40*time.Millisecond {
		t.Fatalf("EstimatedDuration = %s, want 2 frames at the observed 20 ms rate", g.EstimatedDuration)
	}
	if g.Reason == "" {
		t.Fatal("a gap has to say why it was declared")
	}

	seg, err := s.Close(c.t)
	if err != nil {
		t.Fatal(err)
	}
	if len(seg.Gaps) != 1 {
		t.Fatalf("the gap has to travel with the segment: %+v", seg.Gaps)
	}
	frames, _ := ing.Spool().Frames(seg.ID)
	if len(frames) != 3 {
		t.Fatalf("stored %d frames, want the three that actually arrived", len(frames))
	}
}

func TestGapIsDeclaredWhenTheReorderWindowFills(t *testing.T) {
	c := &clock{t: at("2026-08-10T09:00:00Z")}
	ing := newIngester(t, c, func(o *Options) {
		o.Live.ReorderWindow = 3
		o.Live.GapTimeout = time.Hour // out of the way; the window is under test
	})
	s, err := ing.OpenLive(LiveSpec{Device: "phone", Codec: "opus", StartedAt: c.t, UserInitiated: true})
	if err != nil {
		t.Fatal(err)
	}

	// Sequence 0 never arrives. 1, 2, 3 do.
	var gaps []Gap
	for seq := int64(1); seq <= 3; seq++ {
		c.add(20 * time.Millisecond)
		res, err := s.Chunk(chunk(fmt.Sprintf("e%d", seq), seq, "xxxx", c.t))
		if err != nil {
			t.Fatal(err)
		}
		gaps = append(gaps, res.Gaps...)
	}
	if len(gaps) != 1 || gaps[0].FromSeq != 0 || gaps[0].ToSeq != 0 {
		t.Fatalf("gaps = %+v, want one covering sequence 0 once the window filled", gaps)
	}
	if s.NextSeq() != 4 {
		t.Fatalf("NextSeq = %d, want 4 after the gap drained the buffer", s.NextSeq())
	}
}

func TestCloseFlushesWhatArrivedAndMarksTheHole(t *testing.T) {
	c := &clock{t: at("2026-08-10T09:00:00Z")}
	ing := newIngester(t, c, func(o *Options) { o.Live.GapTimeout = time.Hour })
	s, err := ing.OpenLive(LiveSpec{Device: "phone", Codec: "opus", StartedAt: c.t, UserInitiated: true})
	if err != nil {
		t.Fatal(err)
	}
	c.add(20 * time.Millisecond)
	if _, err := s.Chunk(chunk("e0", 0, "aaaa", c.t)); err != nil {
		t.Fatal(err)
	}
	c.add(20 * time.Millisecond)
	if _, err := s.Chunk(chunk("e2", 2, "cccc", c.t)); err != nil {
		t.Fatal(err)
	}

	seg, err := s.Close(c.t)
	if err != nil {
		t.Fatal(err)
	}
	if len(seg.Gaps) != 1 || seg.Gaps[0].FromSeq != 1 {
		t.Fatalf("Gaps = %+v, want the hole at 1 declared on close", seg.Gaps)
	}
	frames, _ := ing.Spool().Frames(seg.ID)
	if len(frames) != 2 {
		t.Fatalf("the buffered chunk is real audio and must be written: %q", frames)
	}
}

func TestCodecChangeIsRefusedRatherThanConcatenated(t *testing.T) {
	c := &clock{t: at("2026-08-10T09:00:00Z")}
	ing := newIngester(t, c)
	s, err := ing.OpenLive(LiveSpec{Device: "phone", Codec: "opus", StartedAt: c.t, UserInitiated: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Chunk(chunk("e0", 0, "a", c.t)); err != nil {
		t.Fatal(err)
	}
	bad := Chunk{EnvelopeID: "e1", Seq: 1, Codec: "pcm16", Data: []byte("b"), At: c.t}
	if _, err := s.Chunk(bad); !errors.Is(err, ErrCodecChanged) {
		t.Fatalf("err = %v, want ErrCodecChanged", err)
	}
}

// The rule from the task, stated as a test: refuse, do not accept and filter.
func TestIngestIsRefusedWhenConsentDoesNotCoverIt(t *testing.T) {
	c := &clock{t: at("2026-08-10T09:00:00Z")}
	sp, _ := newSpool(t, c)
	reg := NewRegistry(GateOptions{Scope: ScopeNone, IndicatorVisible: true, Now: c.now})
	ing, err := New(Options{Spool: sp, Consent: reg, Now: c.now, Log: logx.Discard()})
	if err != nil {
		t.Fatal(err)
	}

	_, err = ing.OpenLive(LiveSpec{Device: "phone", Codec: "opus", StartedAt: c.t})
	if !errors.Is(err, ErrNoConsent) {
		t.Fatalf("OpenLive = %v, want a consent refusal", err)
	}
	if got := sp.List(); len(got) != 0 {
		t.Fatalf("a refused stream must not leave a segment behind: %+v", got)
	}

	// And a stream that was legitimately opened stops accepting the moment
	// consent is withdrawn — mid-turn, without waiting for a close.
	if err := reg.Gate("phone").Grant(ScopeAlways); err != nil {
		t.Fatal(err)
	}
	s, err := ing.OpenLive(LiveSpec{Device: "phone", Codec: "opus", StartedAt: c.t})
	if err != nil {
		t.Fatal(err)
	}
	c.add(time.Second)
	if _, err := s.Chunk(chunk("e0", 0, "a", c.t)); err != nil {
		t.Fatal(err)
	}
	c.add(time.Second)
	reg.Gate("phone").Revoke()
	if _, err := s.Chunk(chunk("e1", 1, "b", c.t)); !errors.Is(err, ErrNoConsent) {
		t.Fatalf("mid-turn revocation = %v, want a refusal", err)
	}
	frames, _ := sp.Frames(s.ID())
	if len(frames) != 1 {
		t.Fatalf("the refused chunk was stored anyway: %q", frames)
	}
}

func TestNewRefusesWithoutAConsentRegistry(t *testing.T) {
	c := &clock{t: at("2026-08-10T09:00:00Z")}
	sp, _ := newSpool(t, c)
	if _, err := New(Options{Spool: sp, Now: c.now}); err == nil {
		t.Fatal("an ingester without a consent registry must not be constructible")
	}
}

// A reconnect mid-turn is the normal path, not the exception.
func TestReopeningADeviceReturnsTheOpenTurn(t *testing.T) {
	c := &clock{t: at("2026-08-10T09:00:00Z")}
	ing := newIngester(t, c)
	first, err := ing.OpenLive(LiveSpec{Device: "phone", Codec: "opus", StartedAt: c.t, UserInitiated: true})
	if err != nil {
		t.Fatal(err)
	}
	c.add(time.Second)
	second, err := ing.OpenLive(LiveSpec{Device: "phone", Codec: "opus", StartedAt: c.t, UserInitiated: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID() != second.ID() {
		t.Fatalf("a reconnect opened a second turn: %s vs %s", first.ID(), second.ID())
	}
	if ing.OpenStreams() != 1 {
		t.Fatalf("OpenStreams = %d", ing.OpenStreams())
	}
	if _, err := ing.CloseLive("phone", c.t); err != nil {
		t.Fatal(err)
	}
	if ing.OpenStreams() != 0 {
		t.Fatalf("OpenStreams after close = %d", ing.OpenStreams())
	}
}

// A restart mid-turn must not re-accept chunks it already holds.
func TestResumeAfterARestartKeepsTheSequence(t *testing.T) {
	c := &clock{t: at("2026-08-10T09:00:00Z")}
	dir := t.TempDir()
	open := func() *Ingester {
		sp, err := OpenSpool(SpoolOptions{Dir: dir, Now: c.now, Log: logx.Discard()})
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry(GateOptions{Scope: ScopeAlways, IndicatorVisible: true, Now: c.now})
		ing, err := New(Options{Spool: sp, Consent: reg, Now: c.now, Log: logx.Discard()})
		if err != nil {
			t.Fatal(err)
		}
		return ing
	}

	ing := open()
	s, err := ing.OpenLive(LiveSpec{ID: "turn", Device: "phone", Codec: "opus", StartedAt: c.t, UserInitiated: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		c.add(20 * time.Millisecond)
		if _, err := s.Chunk(chunk(fmt.Sprintf("e%d", i), int64(i), "xxxx", c.t)); err != nil {
			t.Fatal(err)
		}
	}
	// A gap forces the resume state to disk, which is the point of doing it
	// there: the sequence position is what a restart cannot afford to lose.
	c.add(20 * time.Millisecond)
	if _, err := s.Chunk(chunk("e5", 5, "xxxx", c.t)); err != nil {
		t.Fatal(err)
	}
	c.add(5 * time.Second)
	if _, err := s.Tick(c.t); err != nil {
		t.Fatal(err)
	}

	// Restart.
	restarted := open()
	s2, err := restarted.OpenLive(LiveSpec{ID: "turn", Device: "phone", Codec: "opus", StartedAt: c.t, UserInitiated: true})
	if err != nil {
		t.Fatal(err)
	}
	if s2.NextSeq() != 6 {
		t.Fatalf("NextSeq after restart = %d, want 6", s2.NextSeq())
	}
	res, err := s2.Chunk(chunk("e1", 1, "xxxx", c.t))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Duplicate {
		t.Fatal("a chunk the box already durably holds must be a duplicate after a restart too")
	}
}

// Two audio.chunk frames can be dispatched concurrently on a reconnect. A
// check-then-act in OpenLive would open two segments for one turn, splitting a
// sentence across two files that each look complete.
func TestConcurrentOpensProduceOneTurn(t *testing.T) {
	c := &clock{t: at("2026-08-10T09:00:00Z")}
	ing := newIngester(t, c)

	const n = 16
	ids := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := ing.OpenLive(LiveSpec{Device: "phone", Codec: "opus", StartedAt: c.t, UserInitiated: true})
			if err != nil {
				ids <- "error: " + err.Error()
				return
			}
			ids <- s.ID()
		}()
	}
	wg.Wait()
	close(ids)

	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	if len(seen) != 1 {
		t.Fatalf("concurrent opens produced %d turns: %v", len(seen), seen)
	}
	if got := ing.Spool().List(); len(got) != 1 {
		t.Fatalf("the spool holds %d segments for one turn", len(got))
	}
}
