package episode

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/store"
)

func openStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestWriterRefusesWithoutADetector(t *testing.T) {
	db := openStore(t)
	if _, err := NewWriter(WriterOptions{Store: db}); !errors.Is(err, ErrNoRedactor) {
		t.Fatalf("err = %v, want ErrNoRedactor", err)
	}
}

func TestEpisodesAndCommitmentsReachTheStore(t *testing.T) {
	db := openStore(t)
	w, err := NewWriter(WriterOptions{Store: db, Redact: Detector()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	eps := Segment(aDay(), Options{})
	var total int
	for _, e := range eps {
		res, err := w.Write(ctx, e, Extract(e, Options{}))
		if err != nil {
			t.Fatal(err)
		}
		total += res.Commitments
	}

	stored, err := db.ListEpisodes(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != len(eps) {
		t.Fatalf("stored %d episodes, segmented %d", len(stored), len(eps))
	}
	for _, e := range stored {
		switch store.Episode(e).Kind {
		case string(KindMeeting), string(KindFocus), string(KindConversation), string(KindAmbient):
		default:
			t.Fatalf("kind %q is not one of SYSTEM.md §5's four", e.Kind)
		}
	}

	commitments, err := db.ListCommitments(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(commitments) != total {
		t.Fatalf("stored %d commitments, wrote %d", len(commitments), total)
	}
	found := false
	for _, c := range commitments {
		if strings.Contains(c.Text, "BOM") {
			found = true
			if c.OwedTo != "marc" {
				t.Fatalf("OwedTo = %q", c.OwedTo)
			}
			if c.DueAt.IsZero() {
				t.Fatal("the Friday deadline did not survive the write")
			}
		}
	}
	if !found {
		t.Fatal("the BOM commitment is not in the store")
	}
}

// Re-running the nightly job over the same day must update rows, not double
// them. store.PutEpisode upserts on the id, and that only helps if the ids are
// stable — which is what the deterministic id functions are for.
func TestRewritingADayIsIdempotent(t *testing.T) {
	db := openStore(t)
	w, err := NewWriter(WriterOptions{Store: db, Redact: Detector()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	for run := 0; run < 2; run++ {
		if _, _, err := w.WriteDay(ctx, at("2026-08-10T00:00:00Z"), aDay(), Options{}, DigestLimits{}); err != nil {
			t.Fatal(err)
		}
	}
	eps, err := db.ListEpisodes(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	segmented := Segment(aDay(), Options{})
	if len(eps) != len(segmented) {
		t.Fatalf("two runs produced %d episodes for %d moments", len(eps), len(segmented))
	}
	cs, err := db.ListCommitments(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range cs {
		if seen[c.Text+c.EpisodeID] {
			t.Fatalf("commitment stored twice: %q", c.Text)
		}
		seen[c.Text+c.EpisodeID] = true
	}
}

// Somebody reading a key aloud is not hypothetical, and an episode transcript is
// exactly the text a search index is later built over. Detection happens before
// the write, never after.
func TestCredentialsNeverReachTheStore(t *testing.T) {
	db := openStore(t)
	w, err := NewWriter(WriterOptions{Store: db, Redact: Detector()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// AWS's own documentation example key id — synthetic, and deliberately not
	// a shape scripts/build-public-repo.sh greps for.
	const key = "AKIAIOSFODNN7EXAMPLE"
	us := []Utterance{
		said("2026-08-10T09:00:00Z", "me", "The access key is "+key+" and I'll put it in the env file today."),
	}
	digest, results, err := w.WriteDay(ctx, at("2026-08-10T00:00:00Z"), us, Options{}, DigestLimits{})
	if err != nil {
		t.Fatal(err)
	}

	eps, err := db.ListEpisodes(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 {
		t.Fatalf("got %d episodes", len(eps))
	}
	if strings.Contains(eps[0].Transcript, key) {
		t.Fatalf("the credential is in the stored transcript: %q", eps[0].Transcript)
	}
	if !strings.Contains(eps[0].Transcript, "[relay:redacted") {
		t.Fatalf("no marker replaced it: %q", eps[0].Transcript)
	}

	cs, err := db.ListCommitments(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if strings.Contains(c.Text, key) {
			t.Fatalf("the credential is in a stored commitment: %q", c.Text)
		}
	}
	for _, n := range append(append([]string{}, digest.Notes...), digest.Commitments...) {
		if strings.Contains(n, key) {
			t.Fatalf("the credential is in the digest: %q", n)
		}
	}

	markers, err := db.ListSecretMarkers(ctx, MarkerRuntime, eps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(markers) != 1 {
		t.Fatalf("SecretMarkers = %+v, want one", markers)
	}
	if markers[0].Service != "aws" {
		t.Fatalf("Service = %q", markers[0].Service)
	}

	// The finding itself is in memory only, so MEMORY.md §6's vault proposal
	// has something to offer — and it is tier 1, so it may be proposed.
	if len(results) != 1 || len(results[0].Findings) == 0 {
		t.Fatalf("results = %+v, want the finding carried back to the caller", results)
	}
	if !Proposable(results[0].Findings[0]) {
		t.Fatal("a vendor-shaped key should be proposable to the vault")
	}
}

// A whole day, from utterances to a digest, in the call the nightly job makes.
func TestWriteDayProducesTheDigestAndPersistsIt(t *testing.T) {
	db := openStore(t)
	w, err := NewWriter(WriterOptions{Store: db, Redact: Detector()})
	if err != nil {
		t.Fatal(err)
	}
	digest, results, err := w.WriteDay(context.Background(), at("2026-08-10T00:00:00Z"), aDay(), Options{}, DigestLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("no episodes were written")
	}
	if len(digest.Commitments) == 0 {
		t.Fatalf("digest = %+v", digest)
	}
	if digest.Coverage.Episodes != len(results) {
		t.Fatalf("Coverage.Episodes = %d, wrote %d", digest.Coverage.Episodes, len(results))
	}
	if !digest.Day.Equal(at("2026-08-10T00:00:00Z")) {
		t.Fatalf("Day = %s", digest.Day)
	}
}
