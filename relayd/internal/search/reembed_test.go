package search_test

import (
	"context"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/search"
	"github.com/luthor007/relay/relayd/internal/store"
)

// The providers built in internal/llm must satisfy this package's Embedder
// without either package importing the other. If that ever stops being true it
// should stop being true here, at compile time, and not at the point somebody
// tries to wire an embedder into a Searcher.
var (
	_ search.Embedder = (llm.Embedder)(nil)
	_ llm.Embedder    = (llm.Embedder)(nil)
)

// namedEmbedder is a HashEmbedder under a different model name, which is all
// these tests need: the point is the name written to the index, not the
// vectors.
type namedEmbedder struct {
	search.Embedder
	name string
}

func (e namedEmbedder) Model() string { return e.name }

func embedderNamed(name string) search.Embedder {
	return namedEmbedder{Embedder: search.NewHashEmbedder(store.EmbeddingDims), name: name}
}

func seedEmbedded(t *testing.T, db *store.DB, emb search.Embedder, texts ...string) {
	t.Helper()
	ctx := context.Background()
	if err := search.SetEmbeddingModel(ctx, db, emb.Model()); err != nil {
		t.Fatalf("set embedding model: %v", err)
	}
	for i, text := range texts {
		vecs, err := emb.Embed(ctx, []string{text})
		if err != nil {
			t.Fatalf("embed: %v", err)
		}
		if _, err := db.PutSummary(ctx, store.Summary{
			Kind:      store.SummarySession,
			Runtime:   "claude-code",
			SessionID: "sess-" + string(rune('a'+i)),
			Path:      "/tmp/t.jsonl",
			Text:      text,
		}, vecs[0]); err != nil {
			t.Fatalf("put summary: %v", err)
		}
	}
}

func TestInspectEmbeddingReportsBothModelsAndTheWork(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	old := embedderNamed("nomic-embed-text")
	seedEmbedded(t, db, old, "the payments branch", "the glasses firmware")

	// A summary with no vector at all — the "backfill has not caught up" state,
	// which is different from a mismatch and must not be confused with one.
	if _, err := db.PutSummary(ctx, store.Summary{
		Kind: store.SummarySession, Runtime: "codex", SessionID: "sess-z",
		Path: "/tmp/t.jsonl", Text: "not embedded yet",
	}, nil); err != nil {
		t.Fatal(err)
	}

	st, err := search.InspectEmbedding(ctx, db, old)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if st.Indexed != "nomic-embed-text" || st.Current != "nomic-embed-text" {
		t.Fatalf("models: %+v", st)
	}
	if st.Mismatch() {
		t.Fatal("the same model on both sides is not a mismatch")
	}
	if st.Summaries != 3 || st.Vectors != 2 || st.Unembedded() != 1 {
		t.Fatalf("counts: summaries=%d vectors=%d unembedded=%d", st.Summaries, st.Vectors, st.Unembedded())
	}
	if st.Dims != store.EmbeddingDims {
		t.Fatalf("dims %d", st.Dims)
	}
	if st.Reason() != "" {
		t.Fatalf("a healthy index should have nothing to report: %q", st.Reason())
	}

	// A nil embedder is the supported no-embedder state, not an error.
	st, err = search.InspectEmbedding(ctx, db, nil)
	if err != nil {
		t.Fatalf("inspect with no embedder: %v", err)
	}
	if st.Mismatch() {
		t.Fatal("no embedder cannot mismatch anything")
	}
	if st.Reason() != search.NoEmbedderReason {
		t.Fatalf("reason %q", st.Reason())
	}
}

// The mismatch is actionable, not fatal: lexical results still come back, and
// the reason names both models so a person can decide which one to keep.
func TestModelChangeDegradesAndNamesBothModels(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	seedEmbedded(t, db, embedderNamed("nomic-embed-text"),
		"stripe webhook signature verification on the payments branch")

	next := embedderNamed("text-embedding-3-small")
	s, err := search.New(search.Options{DB: db, Embedder: next})
	if err != nil {
		t.Fatalf("a model change must not stop the searcher from being built: %v", err)
	}

	res, err := s.Search(ctx, search.Query{Text: "stripe webhook payments"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("a model change must not take search down; the lexical half still works")
	}
	if res.Hybrid() {
		t.Fatal("the dense half must not run against another model's vectors")
	}
	if res.VectorCandidates != 0 {
		t.Fatalf("the dense half ran anyway: %d candidates", res.VectorCandidates)
	}
	joined := strings.Join(res.Degraded, " | ")
	for _, want := range []string{"nomic-embed-text", "text-embedding-3-small", "Re-embed"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the degraded reason must mention %q: %q", want, joined)
		}
	}

	// And the index still refuses to be quietly re-pointed at another model.
	if err := search.SetEmbeddingModel(ctx, db, next.Model()); err == nil {
		t.Fatal("SetEmbeddingModel must refuse to relabel an index that already holds vectors")
	}
}

func TestResetEmbeddingIndexIsAllOrNothing(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	old := embedderNamed("nomic-embed-text")
	seedEmbedded(t, db, old, "the payments branch", "the glasses firmware", "the vault migration")

	cleared, err := search.ResetEmbeddingIndex(ctx, db, "text-embedding-3-small")
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if cleared != 3 {
		t.Fatalf("cleared %d vectors, want 3", cleared)
	}

	st, err := search.InspectEmbedding(ctx, db, embedderNamed("text-embedding-3-small"))
	if err != nil {
		t.Fatal(err)
	}
	// The vectors are gone and the label moved together. Neither half of that
	// may happen without the other: an index carrying two models' vectors is the
	// one state with no way to detect it.
	if st.Vectors != 0 {
		t.Fatalf("%d vectors survived the reset", st.Vectors)
	}
	if st.Indexed != "text-embedding-3-small" {
		t.Fatalf("indexed model is %q", st.Indexed)
	}
	if st.Mismatch() {
		t.Fatal("after a reset the new model owns the index")
	}
	// The summaries themselves are untouched — they are the expensive part.
	if st.Summaries != 3 || st.Unembedded() != 3 {
		t.Fatalf("summaries=%d unembedded=%d; a re-embed must not throw away the summaries",
			st.Summaries, st.Unembedded())
	}

	// The dense half comes back once the vectors are rewritten.
	next := embedderNamed("text-embedding-3-small")
	vecs, err := next.Embed(ctx, []string{"the payments branch"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.PutSummary(ctx, store.Summary{
		Kind: store.SummarySession, Runtime: "claude-code", SessionID: "sess-new",
		Path: "/tmp/t.jsonl", Text: "the payments branch",
	}, vecs[0]); err != nil {
		t.Fatal(err)
	}
	s, err := search.New(search.Options{DB: db, Embedder: next})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Search(ctx, search.Query{Text: "payments branch"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !res.Hybrid() {
		t.Fatalf("the dense half should be back after a re-embed: %v", res.Degraded)
	}
}

func TestResetEmbeddingIndexRefusesAnEmptyModel(t *testing.T) {
	if _, err := search.ResetEmbeddingIndex(context.Background(), openDB(t), ""); err == nil {
		t.Fatal("handing the index to an unnamed model is how two models end up mixed")
	}
}

// ClearEmbeddings is the re-embed primitive: it releases the index rather than
// handing it to a named model, so whichever embedder writes next claims it.
func TestClearEmbeddingsReleasesTheIndex(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	seedEmbedded(t, db, embedderNamed("nomic-embed-text"), "one", "two")

	idx := search.EmbeddingIndex{DB: db}
	was, err := idx.EmbeddingModel(ctx)
	if err != nil || was != "nomic-embed-text" {
		t.Fatalf("%q %v", was, err)
	}
	summaries, err := idx.Summaries(ctx)
	if err != nil || summaries != 2 {
		t.Fatalf("summaries %d %v", summaries, err)
	}
	embedded, err := idx.Embeddings(ctx)
	if err != nil || embedded != 2 {
		t.Fatalf("embedded %d %v", embedded, err)
	}

	cleared, err := idx.ClearEmbeddings(ctx)
	if err != nil || cleared != 2 {
		t.Fatalf("cleared %d %v", cleared, err)
	}

	now, err := idx.EmbeddingModel(ctx)
	if err != nil || now != "" {
		t.Fatalf("the index should be unclaimed, got %q %v", now, err)
	}
	if n, _ := idx.Embeddings(ctx); n != 0 {
		t.Fatalf("%d vectors survived", n)
	}
	if n, _ := idx.Summaries(ctx); n != 2 {
		t.Fatalf("the summaries must survive a re-embed: %d", n)
	}

	// And the next model claims it without a fight, which is the whole point of
	// releasing rather than relabelling.
	if err := search.SetEmbeddingModel(ctx, db, "text-embedding-3-small"); err != nil {
		t.Fatalf("claiming a cleared index: %v", err)
	}
}
