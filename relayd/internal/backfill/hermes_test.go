package backfill

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/index"
)

// buildHermesDB materialises a synthetic state.db from the fixture SQL. The
// fixture is SQL rather than a committed binary so the schema it claims to
// match is reviewable in a diff.
func buildHermesDB(t *testing.T, files ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")

	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(testdata(t, "hermes"), f))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func hermesReader(t *testing.T, files ...string) *Hermes {
	t.Helper()
	if len(files) == 0 {
		files = []string{"schema.sql", "seed.sql"}
	}
	h := NewHermes(fixtureEnv(t))
	h.DBPath = buildHermesDB(t, files...)
	return h
}

func TestHermesScanKeysResumeOnTheSessionNotTheFile(t *testing.T) {
	h := hermesReader(t)
	res, err := h.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StoreOK {
		t.Fatalf("status %q: %v", res.Status, res.Notes)
	}
	if len(res.Refs) != 3 {
		t.Fatalf("%d sessions, want 3", len(res.Refs))
	}

	for _, r := range res.Refs {
		if !strings.Contains(r.MTimeFrom, "session") {
			t.Errorf("%s: resume key is %q — state.db's own mtime moves for every session at once and would re-index all of them",
				r.SessionID, r.MTimeFrom)
		}
		if r.Path != h.DBPath {
			t.Errorf("%s: path %q", r.SessionID, r.Path)
		}
	}

	first := refByID(t, res.Refs, "hs-0001")
	if first.MTime.UnixMilli() != 1770556800000 {
		t.Errorf("hs-0001 last activity %v", first.MTime)
	}
	if first.Size != 42 {
		t.Errorf("hs-0001 size %d, want its message count", first.Size)
	}
}

func TestHermesReadTakesTheTitleItAlreadyWrote(t *testing.T) {
	h := hermesReader(t)
	scan, _ := h.Scan(context.Background())
	s, err := h.Read(context.Background(), refByID(t, scan.Refs, "hs-0001"))
	if err != nil {
		t.Fatal(err)
	}

	if s.Title != "refactor the BLE codec around CRC-16/MODBUS" {
		t.Errorf("title %q", s.Title)
	}
	if s.TitleSource != index.TitleGenerated {
		t.Errorf("title source %q — Hermes titles its own sessions (MEMORY.md §4)", s.TitleSource)
	}
	if s.Workspace != "/home/user/src/relay" || s.Model != "claude-opus-4" {
		t.Errorf("metadata %+v", s)
	}
	if s.Messages != 42 || s.ToolCalls != 11 {
		t.Errorf("counts %d/%d", s.Messages, s.ToolCalls)
	}
	if s.CostUSD == nil || *s.CostUSD != 2.11 {
		t.Errorf("cost %v — actual_cost_usd must win over the estimate", s.CostUSD)
	}
	if s.TokensTotal == nil || *s.TokensTotal != 91000+12400+410000 {
		t.Errorf("tokens %v", s.TokensTotal)
	}
	if !strings.Contains(s.Text, "MODBUS with init 0xFFFF") {
		t.Errorf("messages not extracted: %q", s.Text)
	}
	if s.StartedAt.UnixMilli() != 1770553200000 {
		t.Errorf("started %v", s.StartedAt)
	}
}

func TestHermesEstimatedCostIsLabelledAsAnEstimate(t *testing.T) {
	h := hermesReader(t)
	scan, _ := h.Scan(context.Background())
	s, err := h.Read(context.Background(), refByID(t, scan.Refs, "hs-0002"))
	if err != nil {
		t.Fatal(err)
	}
	if s.CostUSD == nil || *s.CostUSD != 0.31 {
		t.Fatalf("cost %v", s.CostUSD)
	}
	if !strings.Contains(strings.Join(s.Notes, "\n"), "estimated_cost_usd") {
		t.Errorf("an estimate was presented as an actual: %v", s.Notes)
	}
	if s.Title != "" || s.TitleSource != index.TitleNone {
		t.Errorf("untitled session got title %q from %q", s.Title, s.TitleSource)
	}
}

func TestHermesLeavesUnrecordedFieldsNil(t *testing.T) {
	h := hermesReader(t)
	scan, _ := h.Scan(context.Background())
	s, err := h.Read(context.Background(), refByID(t, scan.Refs, "hs-0003"))
	if err != nil {
		t.Fatal(err)
	}
	if s.CostUSD != nil {
		t.Errorf("cost %v — nil, never zero, when the store recorded nothing", *s.CostUSD)
	}
	if s.TokensTotal != nil {
		t.Errorf("tokens %v — same rule", *s.TokensTotal)
	}
	if s.Model != "" {
		t.Errorf("model %q", s.Model)
	}
	if s.Messages != 1 {
		t.Errorf("messages %d — message_count was null, so the rows were counted", s.Messages)
	}
	if !strings.Contains(strings.Join(s.Notes, "\n"), "no cost") {
		t.Errorf("missing cost was not disclosed: %v", s.Notes)
	}
}

// The rule from MEMORY.md §9 and §12.5: Hermes coordinates its own compaction
// through a lease with real contention. Backfill is a reader and must not
// reach for it.
func TestHermesNeverTouchesTheCompressionLease(t *testing.T) {
	h := hermesReader(t)
	ctx := context.Background()

	db, err := h.open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := h.query(ctx, db, `SELECT * FROM compression_locks`); err == nil {
		t.Fatal("the guard let a compression_locks query through")
	}
	if _, err := h.query(ctx, db, `UPDATE compression_locks SET holder = 'relay'`); err == nil {
		t.Fatal("the guard let a compression_locks write through")
	}

	// And the lease is untouched after a full read.
	scan, err := h.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range scan.Refs {
		if _, err := h.Read(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}

	var holder string
	var expires int64
	row := db.QueryRow(`SELECT holder, expires_at FROM compression_locks WHERE session_id = 'hs-0001'`)
	if err := row.Scan(&holder, &expires); err != nil {
		t.Fatal(err)
	}
	if holder != "hermes-cli/17422" || expires != 1770556800000 {
		t.Fatalf("the lease moved: %s / %d", holder, expires)
	}
}

// Backfill reads another program's live 2.5 GB database. It must not write to
// it, and the cheapest proof is that the bytes did not change.
func TestHermesOpensReadOnly(t *testing.T) {
	h := hermesReader(t)
	ctx := context.Background()

	before := hashFile(t, h.DBPath)

	scan, err := h.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range scan.Refs {
		if _, err := h.Read(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	if after := hashFile(t, h.DBPath); after != before {
		t.Fatal("state.db changed under a read-only backfill")
	}

	db, err := h.open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO sessions (id) VALUES ('written-by-relay')`); err == nil {
		t.Fatal("the connection accepted a write")
	}
}

// The schema has never been probed beyond the columns MEMORY.md §4 names, so
// the reader introspects. This is the same store under different names.
func TestHermesToleratesADifferentSchema(t *testing.T) {
	h := hermesReader(t, "schema-variant.sql")
	ctx := context.Background()

	scan, err := h.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if scan.Status != StoreOK || len(scan.Refs) != 1 {
		t.Fatalf("status %q with %d refs: %v", scan.Status, len(scan.Refs), scan.Notes)
	}

	s, err := h.Read(ctx, scan.Refs[0])
	if err != nil {
		t.Fatal(err)
	}
	if s.Title != "ship the installer" {
		t.Errorf("title %q", s.Title)
	}
	if s.Workspace != "/home/user/src/relay" || s.Model != "claude-sonnet-4" {
		t.Errorf("%+v", s)
	}
	if s.StartedAt.IsZero() {
		t.Error("ISO-text timestamps were not parsed")
	}
	if !strings.Contains(s.Text, "installed but never run") {
		t.Errorf("messages under synonym columns were not read: %q", s.Text)
	}
	notes := strings.Join(scan.Notes, "\n")
	if !strings.Contains(notes, "tool_call_count") {
		t.Errorf("the absent column was not disclosed: %v", scan.Notes)
	}
}

func TestHermesAbsentDatabaseIsNotAnError(t *testing.T) {
	h := NewHermes(fixtureEnv(t))
	res, err := h.Scan(context.Background())
	if err != nil {
		t.Fatalf("absent store must not error: %v", err)
	}
	if res.Status != StoreAbsent {
		t.Errorf("status %q", res.Status)
	}
	if len(res.Roots) == 0 {
		t.Error("where we looked must be reported")
	}
}

// "No sessions" and "we could not read it" lead to opposite decisions. A
// corrupt database must never look like a clean install.
func TestHermesUnreadableDatabaseIsNotAnEmptyHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(path, []byte("this is not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewHermes(fixtureEnv(t))
	h.DBPath = path

	res, err := h.Scan(context.Background())
	if err != nil {
		t.Fatalf("should degrade, not fail: %v", err)
	}
	if res.Status != StoreUnreadable {
		t.Fatalf("status %q, want unreadable", res.Status)
	}
	if res.Err == nil || len(res.Notes) == 0 {
		t.Error("an unreadable store must carry its reason")
	}
}

func hashFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
