package search_test

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"

	"github.com/luthor007/relay/relayd/internal/search"
	"github.com/luthor007/relay/relayd/internal/store"
)

// BenchmarkHybridSearch times the whole retrieval path — two FTS5 queries, one
// kNN, one hydration — rather than the kNN alone.
//
// MEMORY.md §3 measured the kNN in isolation at ~45 ms median for 22,000
// vectors on the pure-Go wasm build we ship. This exists because that is not
// the number SYSTEM.md §7b's budget cares about: the budget is for a memory
// lookup end to end, and the lexical halves and the hydration are on the same
// clock. Run it with -benchtime=1x and a large -bench.N corpus when the design
// target changes:
//
//	go test ./internal/search -run xxx -bench HybridSearch -benchtime 20x
//
// It is a benchmark and not a test on purpose: building the corpus costs
// seconds and `go test ./...` should not pay for it.
func BenchmarkHybridSearch(b *testing.B) {
	const corpus = 5000

	ctx := context.Background()
	db, err := store.Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	defer db.Close()

	emb := search.NewHashEmbedder(store.EmbeddingDims)
	if err := search.SetEmbeddingModel(ctx, db, emb.Model()); err != nil {
		b.Fatal(err)
	}

	rng := rand.New(rand.NewSource(1))
	words := []string{
		"stripe", "webhook", "signature", "payments", "vercel", "supabase",
		"migration", "auth", "token", "codex", "hermes", "adapter", "sqlite",
		"vector", "index", "compaction", "glasses", "firmware", "ble", "crc",
	}
	for i := 0; i < corpus; i++ {
		text := fmt.Sprintf("Session %d: ", i)
		for j := 0; j < 12; j++ {
			text += words[rng.Intn(len(words))] + " "
		}
		if i%97 == 0 {
			text += "STRIPE_SECRET_KEY was rotated"
		}
		vecs, err := emb.Embed(ctx, []string{text})
		if err != nil {
			b.Fatal(err)
		}
		if _, err := db.PutSummary(ctx, store.Summary{
			Kind:      store.SummaryCluster,
			Runtime:   "claude-code",
			SessionID: fmt.Sprintf("s%d", i),
			Path:      "/t/x.jsonl",
			Text:      text,
		}, vecs[0]); err != nil {
			b.Fatal(err)
		}
	}

	s, err := search.New(search.Options{DB: db, Embedder: emb})
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := s.Search(ctx, search.Query{Text: "where did I set STRIPE_SECRET_KEY"})
		if err != nil {
			b.Fatal(err)
		}
		if len(res.Hits) == 0 {
			b.Fatal("no hits")
		}
	}
}
