package episode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/capture"
	"github.com/luthor007/relay/relayd/internal/logx"
	"github.com/luthor007/relay/relayd/internal/transcript"
)

// SYSTEM.md §10 step 5, end to end and in one test: **capture → transcript →
// episodes**, with the audio destroyed on the way out.
//
// Everything in it is the real implementation except the recogniser, which is
// the one component that cannot run without an audio stack.
func TestCaptureToTranscriptToEpisodes(t *testing.T) {
	now := at("2026-08-10T09:00:00Z")
	clock := func() time.Time { return now }
	dir := t.TempDir()

	spool, err := capture.OpenSpool(capture.SpoolOptions{
		Dir: dir, Retention: time.Hour, Now: clock, Log: logx.Discard(),
	})
	if err != nil {
		t.Fatal(err)
	}
	consent := capture.NewRegistry(capture.GateOptions{
		Scope: capture.ScopeAlways, IndicatorVisible: true,
		Since: at("2026-08-01T00:00:00Z"), Now: clock,
	})
	ing, err := capture.New(capture.Options{
		Spool: spool, Consent: consent, Now: clock, Log: logx.Discard(),
	})
	if err != nil {
		t.Fatal(err)
	}
	pipe, err := transcript.NewPipeline(transcript.PipelineOptions{
		Audio: spool, Router: transcript.NewRouter(&transcript.Fake{Speaker: "me"}),
		Redact: transcript.Detector(), Diarize: true, Now: clock, Log: logx.Discard(),
	})
	if err != nil {
		t.Fatal(err)
	}
	db := openStore(t)
	writer, err := NewWriter(WriterOptions{Store: db, Redact: Detector(), Now: clock})
	if err != nil {
		t.Fatal(err)
	}

	// --- capture: two voice turns, an hour and a half apart -----------------
	type turn struct {
		at    string
		lines []string
	}
	turns := []turn{
		{"2026-08-10T09:00:00Z", []string{
			"The CRC-16 variant in the appendix is wrong.",
			"I'll send Marc the BOM by Friday.",
		}},
		{"2026-08-10T14:00:00Z", []string{
			"We decided to go with the WCH part.",
			"I'll update the schematic tomorrow.",
		}},
	}

	var utterances []Utterance
	var segIDs []string
	for i, tn := range turns {
		now = at(tn.at)
		s, err := ing.OpenLive(capture.LiveSpec{
			Device: "phone", Codec: "opus", StartedAt: now, UserInitiated: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		for j, line := range tn.lines {
			now = now.Add(4 * time.Second)
			if _, err := s.Chunk(capture.Chunk{
				EnvelopeID: strings.Join([]string{"env", tn.at, line[:4]}, "-"),
				Seq:        int64(j), Codec: "opus", Data: []byte(line), At: now,
			}); err != nil {
				t.Fatal(err)
			}
		}
		seg, err := ing.CloseLive("phone", now)
		if err != nil {
			t.Fatal(err)
		}
		segIDs = append(segIDs, seg.ID)

		// --- transcript ----------------------------------------------------
		tr, err := pipe.Run(context.Background(), transcript.Job{SegmentID: seg.ID})
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		if !tr.Complete() {
			t.Fatalf("turn %d transcript is incomplete: %v", i, tr.Notes)
		}
		utterances = append(utterances, FromTranscript(tr, "lab")...)
	}

	// --- episodes ----------------------------------------------------------
	digest, results, err := writer.WriteDay(context.Background(),
		at("2026-08-10T00:00:00Z"), utterances, Options{}, DigestLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("wrote %d episodes, want one per turn ninety minutes apart", len(results))
	}

	stored, err := db.ListEpisodes(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("stored %d episodes", len(stored))
	}
	for _, e := range stored {
		if e.Kind != string(KindFocus) {
			t.Fatalf("kind = %q, want focus — one speaker, working", e.Kind)
		}
		if !strings.Contains(e.Transcript, "me:") {
			t.Fatalf("transcript lost its speaker: %q", e.Transcript)
		}
	}

	commitments, err := db.ListCommitments(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(commitments) != 2 {
		t.Fatalf("commitments = %+v, want the BOM and the schematic", commitments)
	}
	if !strings.Contains(strings.Join(digest.Commitments, " | "), "BOM") {
		t.Fatalf("digest commitments = %v", digest.Commitments)
	}
	if len(digest.Decisions) != 1 || !strings.Contains(digest.Decisions[0], "WCH") {
		t.Fatalf("digest decisions = %v", digest.Decisions)
	}
	if digest.Coverage.Episodes != 2 || digest.Coverage.Gaps != 0 {
		t.Fatalf("Coverage = %+v", digest.Coverage)
	}

	// --- and the audio stops existing --------------------------------------
	//
	// SYSTEM.md §5: "Audio is kept only long enough to re-transcribe, then
	// discarded." The memory above outlives it; the recording does not.
	for _, id := range segIDs {
		if _, err := os.Stat(filepath.Join(dir, id+".audio")); err != nil {
			t.Fatalf("audio for %s should still exist inside the window: %v", id, err)
		}
	}
	now = now.Add(2 * time.Hour)
	sweep := spool.Sweep()
	if len(sweep.Discarded) != 2 {
		t.Fatalf("Discarded = %v, want both segments", sweep.Discarded)
	}
	for _, id := range segIDs {
		if _, err := os.Stat(filepath.Join(dir, id+".audio")); !os.IsNotExist(err) {
			t.Fatalf("audio for %s is still on disk: %v", id, err)
		}
	}
	if spool.Bytes() != 0 {
		t.Fatalf("the spool still holds %d bytes of audio", spool.Bytes())
	}

	// The episodes are untouched by the sweep. That asymmetry is the product.
	after, err := db.ListEpisodes(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 || after[0].Transcript == "" {
		t.Fatal("the transcript did not outlive the audio")
	}
}
