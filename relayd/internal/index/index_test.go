package index

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/store"
)

func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func sampleSession() Session {
	return Session{
		Runtime:     adapter.ClaudeCode,
		SessionID:   "3f2b1c9e-0a4d-4c11-9d2e-77c0b5a41f00",
		Path:        "/home/user/.claude/projects/-home-user-src-relay/3f2b.jsonl",
		Title:       "wire up the payments webhook",
		TitleSource: TitleGenerated,
		Workspace:   "/home/user/src/relay",
		GitBranch:   "main",
		Model:       "claude-opus-4",
		StartedAt:   time.UnixMilli(1_770_000_000_000).UTC(),
		EndedAt:     time.UnixMilli(1_770_000_900_000).UTC(),
		Messages:    12,
		ToolCalls:   4,
		SourceMTime: time.UnixMilli(1_770_000_900_000).UTC(),
		SourceSize:  4096,
		MTimeFrom:   "file mtime",
	}
}

func TestIndexWritesOneRowThatPointsAtTheTranscript(t *testing.T) {
	db := openDB(t)
	ix := New(db, nil)
	ix.Now = func() time.Time { return time.UnixMilli(1_770_001_000_000).UTC() }

	s := sampleSession()
	s.Text = "let's finish the webhook handler"

	res, err := ix.Index(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Wrote {
		t.Fatal("nothing written")
	}

	got, err := db.GetSessionIndex(context.Background(), string(s.Runtime), s.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != s.Path {
		t.Errorf("path %q, want the transcript's own path %q", got.Path, s.Path)
	}
	if got.Title != s.Title || got.Workspace != s.Workspace || got.GitBranch != "main" {
		t.Errorf("metadata lost: %+v", got)
	}
	if got.Messages != 12 || got.ToolCalls != 4 {
		t.Errorf("counts wrong: %+v", got)
	}
	if got.CostUSD != nil || got.TokensTotal != nil {
		t.Errorf("cost and tokens must stay nil when the store did not report them: %+v", got)
	}
	if !got.IndexedAt.Equal(time.UnixMilli(1_770_001_000_000).UTC()) {
		t.Errorf("indexed_at %v", got.IndexedAt)
	}
}

// TestIndexNeverStoresTranscriptText is MEMORY.md §3's "index, not a copy"
// asserted against the database rather than against a comment.
func TestIndexNeverStoresTranscriptText(t *testing.T) {
	db := openDB(t)
	ix := New(db, nil)

	s := sampleSession()
	s.Text = "MAGIC_TRANSCRIPT_BODY_9137 — this sentence is only in the transcript"

	if _, err := ix.Index(context.Background(), s); err != nil {
		t.Fatal(err)
	}

	// The transcript body must not be findable in any column of any table. The
	// row is a pointer; re-reading the file is how the text is obtained.
	assertNoTextAnywhere(t, db, "MAGIC_TRANSCRIPT_BODY_9137")
}

// TestIndexRedactsBeforeWriting is the ordering rule from MEMORY.md §6, which
// says plainly that it is not negotiable: an embedded key cannot be unembedded.
func TestIndexRedactsBeforeWriting(t *testing.T) {
	db := openDB(t)
	ix := New(db, nil)
	ctx := context.Background()

	key := "sk_live_" + strings.Repeat("Zq7Y", 6)
	s := sampleSession()
	s.Title = "rotate " + key
	s.Text = "the webhook secret is " + key + "\nand the deploy went out"

	res, err := ix.Index(ctx, s)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(res.Redacted, key) {
		t.Fatal("the text handed to the summariser still carries the key")
	}
	if !strings.Contains(res.Redacted, "[relay:redacted Stripe secret key]") {
		t.Fatalf("no marker in %q", res.Redacted)
	}

	row, err := db.GetSessionIndex(ctx, string(s.Runtime), s.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row.Title, key) {
		t.Fatalf("the indexed title carries the key: %q", row.Title)
	}
	if !strings.Contains(row.Title, "[relay:redacted") {
		t.Fatalf("the indexed title lost its marker: %q", row.Title)
	}

	markers, err := db.ListSecretMarkers(ctx, string(s.Runtime), s.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(markers) != 2 {
		t.Fatalf("want a marker for the text hit and the title hit, got %d", len(markers))
	}
	for _, m := range markers {
		if m.Service != "stripe" {
			t.Errorf("marker service %q", m.Service)
		}
		if !strings.Contains(m.Detector, "stripe_secret") || !strings.Contains(m.Detector, "tier1") {
			t.Errorf("marker detector %q should name the rule and its tier", m.Detector)
		}
		if m.Path != s.Path {
			t.Errorf("marker path %q should point at the transcript", m.Path)
		}
		if m.VaultID != "" {
			t.Error("nothing is captured silently: vault_id is set when a proposal is accepted, not at detection")
		}
	}

	// The whole database, scanned for the credential. Belt and braces, because
	// this is the one invariant that cannot be walked back.
	assertNoTextAnywhere(t, db, key)
}

func TestIndexTierTwoIsRedactedButNotProposed(t *testing.T) {
	db := openDB(t)
	ix := New(db, nil)
	ctx := context.Background()

	// 32 lowercase hex: a Twilio auth token and an MD5 digest are the same
	// shape, which is exactly why this may be redacted and never proposed.
	token := "d41d8cd98f00b204e9800998ecf8427e"
	s := sampleSession()
	s.Text = "TWILIO_AUTH_TOKEN=" + token

	res, err := ix.Index(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Redacted, token) {
		t.Fatal("tier 2 hit was not redacted")
	}
	if len(res.VaultCandidates()) != 0 {
		t.Fatalf("tier 2 must never be a vault candidate, got %+v", res.VaultCandidates())
	}
	assertNoTextAnywhere(t, db, token)
}

func TestIndexIsIdempotentAndResumable(t *testing.T) {
	db := openDB(t)
	ix := New(db, nil)
	ctx := context.Background()
	s := sampleSession()

	need, err := ix.NeedsIndexing(ctx, s.Runtime, s.SessionID, s.SourceMTime, s.SourceSize)
	if err != nil {
		t.Fatal(err)
	}
	if !need {
		t.Fatal("an unseen session must need indexing")
	}

	if _, err := ix.Index(ctx, s); err != nil {
		t.Fatal(err)
	}
	need, err = ix.NeedsIndexing(ctx, s.Runtime, s.SessionID, s.SourceMTime, s.SourceSize)
	if err != nil {
		t.Fatal(err)
	}
	if need {
		t.Fatal("an unchanged session must be skipped on resume")
	}

	// The file grew: the session has to be read again.
	need, err = ix.NeedsIndexing(ctx, s.Runtime, s.SessionID, s.SourceMTime.Add(time.Minute), s.SourceSize+10)
	if err != nil {
		t.Fatal(err)
	}
	if !need {
		t.Fatal("a session whose mtime moved must be re-indexed")
	}

	// Re-indexing updates in place rather than duplicating.
	if _, err := ix.Index(ctx, s); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.SQL().QueryRow(`SELECT count(*) FROM session_index`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("re-indexing produced %d rows", n)
	}
}

func TestIndexDryRunWritesNothing(t *testing.T) {
	db := openDB(t)
	ix := New(db, nil)
	ix.DryRun = true

	s := sampleSession()
	s.Text = "nothing to see"
	res, err := ix.Index(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if res.Wrote {
		t.Fatal("dry run wrote")
	}
	var n int
	if err := db.SQL().QueryRow(`SELECT count(*) FROM session_index`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("dry run left %d rows", n)
	}
}

func TestIndexRejectsAnUnidentifiedSession(t *testing.T) {
	ix := New(openDB(t), nil)
	if _, err := ix.Index(context.Background(), Session{Runtime: adapter.Codex}); err == nil {
		t.Fatal("a session with no id was accepted")
	}
}

func TestMarkerSentencesAreDeduplicated(t *testing.T) {
	d := MustDetector()
	text := "one " + "sk_live_" + strings.Repeat("Ab1c", 6) + " two " + "sk_live_" + strings.Repeat("Dd2e", 6)
	_, findings := d.Redact(text)
	res := Result{Findings: findings}
	got := res.MarkerSentences()
	if len(got) != 1 || got[0] != "a Stripe secret key appeared in this session" {
		t.Fatalf("marker sentences %q", got)
	}
}

// assertNoTextAnywhere greps every text column of every table.
func assertNoTextAnywhere(t *testing.T, db *store.DB, needle string) {
	t.Helper()
	rows, err := db.SQL().Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	rows.Close()

	for _, tbl := range tables {
		cols, err := db.SQL().Query(`SELECT name FROM pragma_table_info(?)`, tbl)
		if err != nil {
			continue
		}
		var names []string
		for cols.Next() {
			var n string
			if err := cols.Scan(&n); err != nil {
				break
			}
			names = append(names, n)
		}
		cols.Close()

		for _, c := range names {
			var n int
			q := `SELECT count(*) FROM "` + tbl + `" WHERE CAST("` + c + `" AS TEXT) LIKE ?`
			if err := db.SQL().QueryRow(q, "%"+needle+"%").Scan(&n); err != nil {
				continue
			}
			if n != 0 {
				t.Fatalf("credential found in %s.%s — an embedded key cannot be unembedded", tbl, c)
			}
		}
	}
}
