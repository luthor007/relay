package store_test

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/store"
)

func openMain(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestStackWorks is the load-bearing test for MEMORY.md §3's three pins. It
// creates both virtual tables (they are in the migration), round-trips a vector
// through sqlite_vec.SerializeFloat32 and reads it back by kNN, and runs an
// FTS5 match.
//
// The vec0 insert is the point. select vec_version() succeeds on the broken
// wazero versions and proves nothing; every vec0 table panics on v1.8.2 with an
// out-of-bounds memory access, on ten 4-dimensional vectors. If wazero ever
// drifts below v1.9.0 this test is what goes red.
func TestStackWorks(t *testing.T) {
	ctx := context.Background()
	db := openMain(t)

	var sqliteVersion, vecVersion string
	if err := db.SQL().QueryRowContext(ctx, `SELECT sqlite_version(), vec_version()`).
		Scan(&sqliteVersion, &vecVersion); err != nil {
		t.Fatalf("vec_version: %v — sqlite-vec is not in this build, which usually "+
			"means go-sqlite3/embed got imported somewhere and overwrote sqlite3.Binary: %v", err, err)
	}
	t.Logf("sqlite %s, sqlite-vec %s", sqliteVersion, vecVersion)

	// Both virtual tables exist as virtual tables, not as ordinary ones.
	for _, tbl := range []string{"summary_fts", "summary_vec"} {
		var sqlText string
		if err := db.SQL().QueryRowContext(ctx,
			`SELECT sql FROM sqlite_master WHERE name = ?`, tbl).Scan(&sqlText); err != nil {
			t.Fatalf("%s missing: %v", tbl, err)
		}
		if !strings.Contains(strings.ToUpper(sqlText), "VIRTUAL TABLE") {
			t.Fatalf("%s is not a virtual table: %s", tbl, sqlText)
		}
	}

	// Three summaries, each with a distinct vector, written through the one
	// function that keeps the row, the FTS index and the vector together.
	rng := rand.New(rand.NewSource(7))
	want := unitVector(rng)
	ids := make([]int64, 0, 3)
	for i, text := range []string{
		"refactored the payments module and fixed the Stripe webhook retry",
		"debugged a CRC-16/MODBUS mismatch in the glasses BLE codec",
		"set up sqlite-vec and FTS5 for the session index",
	} {
		vec := want
		if i > 0 {
			vec = unitVector(rng)
		}
		id, err := db.PutSummary(ctx, store.Summary{
			Kind:       store.SummarySession,
			Runtime:    "claude-code",
			SessionID:  "sess-" + string(rune('a'+i)),
			Path:       "/home/u/.claude/projects/x/deadbeef.jsonl",
			ByteOffset: int64(1024 * (i + 1)),
			ByteLength: 512,
			Text:       text,
			Model:      "test",
			CreatedAt:  time.Unix(1770000000, 0),
		}, vec)
		if err != nil {
			t.Fatalf("PutSummary %d: %v", i, err)
		}
		ids = append(ids, id)
	}

	// The vector round trip: nearest neighbour to the vector we stored first
	// must be the row we stored it on, at distance ~0.
	hits, err := db.SearchVector(ctx, want, 3)
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("SearchVector returned %d hits, want 3", len(hits))
	}
	if hits[0].SummaryID != ids[0] {
		t.Fatalf("nearest neighbour is %d, want %d", hits[0].SummaryID, ids[0])
	}
	if hits[0].Score > 1e-4 {
		t.Fatalf("distance to itself is %g, want ~0 — the vector did not survive the round trip", hits[0].Score)
	}
	if hits[0].Rank != 1 || hits[2].Rank != 3 {
		t.Fatalf("ranks are not 1-based and ordered: %+v", hits)
	}

	// The lexical half. BM25 over the same rows, matching an exact identifier —
	// which is the case MEMORY.md §3 keeps FTS5 for.
	lex, err := db.SearchLexical(ctx, "MODBUS", 5)
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}
	if len(lex) != 1 {
		t.Fatalf("FTS5 match returned %d rows, want 1: %+v", len(lex), lex)
	}
	if lex[0].SummaryID != ids[1] {
		t.Fatalf("FTS5 matched summary %d, want %d", lex[0].SummaryID, ids[1])
	}

	// The index holds a pointer into the transcript, not a copy of it.
	got, err := db.GetSummary(ctx, ids[1])
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	if got.Path == "" || got.ByteOffset != 2048 {
		t.Fatalf("summary lost its pointer: path=%q offset=%d", got.Path, got.ByteOffset)
	}
}

func TestPutSummaryRejectsWrongWidth(t *testing.T) {
	db := openMain(t)
	_, err := db.PutSummary(context.Background(), store.Summary{
		Runtime: "codex", SessionID: "s", Path: "/x", Text: "hi",
	}, []float32{1, 2, 3})
	if !errors.Is(err, store.ErrEmbeddingDims) {
		t.Fatalf("want ErrEmbeddingDims, got %v", err)
	}
}

// PutSummary with a nil embedding is the "no embeddings yet" stage of
// MEMORY.md §11's build order: lexical search works, vector search finds
// nothing, and neither is a failure.
func TestPutSummaryWithoutEmbedding(t *testing.T) {
	ctx := context.Background()
	db := openMain(t)

	id, err := db.PutSummary(ctx, store.Summary{
		Runtime: "hermes", SessionID: "s1", Path: "/x", Text: "deployed to vercel",
	}, nil)
	if err != nil {
		t.Fatalf("PutSummary: %v", err)
	}
	lex, err := db.SearchLexical(ctx, "vercel", 5)
	if err != nil || len(lex) != 1 || lex[0].SummaryID != id {
		t.Fatalf("lexical half should still work: %v %+v", err, lex)
	}
	vec, err := db.SearchVector(ctx, unitVector(rand.New(rand.NewSource(1))), 5)
	if err != nil {
		t.Fatalf("SearchVector on an empty vec table: %v", err)
	}
	if len(vec) != 0 {
		t.Fatalf("want no vector hits, got %d", len(vec))
	}
}

func TestMigrationsAreForwardOnlyAndIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.db")

	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	v1, err := db.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mig, err := store.Migrations(store.KindMain)
	if err != nil {
		t.Fatal(err)
	}
	if v1 != len(mig) {
		t.Fatalf("version %d after open, want %d", v1, len(mig))
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening applies nothing and breaks nothing.
	db2, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	v2, err := db2.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v2 != v1 {
		t.Fatalf("reopen moved the version from %d to %d", v1, v2)
	}
}

func TestSchemaAheadIsRefused(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.db")

	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO schema_migration (version, name, applied_at) VALUES (9999, 'from_the_future', 0)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	if _, err := store.Open(path); !errors.Is(err, store.ErrSchemaAhead) {
		t.Fatalf("want ErrSchemaAhead, got %v", err)
	}
}

func unitVector(rng *rand.Rand) []float32 {
	v := make([]float32, store.EmbeddingDims)
	var norm float64
	for i := range v {
		x := rng.NormFloat64()
		v[i] = float32(x)
		norm += x * x
	}
	norm = math.Sqrt(norm)
	for i := range v {
		v[i] = float32(float64(v[i]) / norm)
	}
	return v
}
