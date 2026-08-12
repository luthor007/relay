package search_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/search"
	"github.com/luthor007/relay/relayd/internal/store"
)

func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// unit builds a 768-wide unit vector from a handful of leading components, so a
// test can state "this one is nearer" instead of hoping a hash makes it so.
func unit(components ...float32) []float32 {
	v := make([]float32, store.EmbeddingDims)
	copy(v, components)
	search.Normalize(v)
	return v
}

// The corpus for the identifier test. The query is an exact identifier; doc
// paraphrase is the nearest thing to it in vector space and does not contain
// it; doc identifier contains it and is far away in vector space. That is the
// disagreement MEMORY.md §3 says hybrid retrieval exists to resolve, stated
// outright rather than hoped for.
const (
	queryText = "STRIPE_SECRET_KEY"

	docParaphrase = "Rotated the payment provider credentials for billing and rolled the signing key."
	docIdentifier = "Set STRIPE_SECRET_KEY in the Vercel environment for the checkout service."
	docNeighbour  = "Reviewed the billing dashboard and the payment provider webhooks."
	docUnrelated  = "Fixed the BLE codec CRC on the glasses firmware."
)

func identifierCorpus(t *testing.T) (*store.DB, *search.Searcher, map[string]int64) {
	t.Helper()
	ctx := context.Background()
	db := openDB(t)

	emb := &search.StaticEmbedder{
		Name: "test-static",
		Vectors: map[string][]float32{
			queryText: unit(1, 0, 0),
			// Nearest to the query, and it never says "stripe".
			docParaphrase: unit(0.99, 0.14, 0),
			docNeighbour:  unit(0.90, 0.44, 0),
			docUnrelated:  unit(0.70, 0.71, 0),
			// The document that actually answers the question, placed last in
			// vector space on purpose.
			docIdentifier: unit(0.20, 0, 0.98),
		},
	}
	if err := search.SetEmbeddingModel(ctx, db, emb.Model()); err != nil {
		t.Fatalf("set embedding model: %v", err)
	}

	ids := map[string]int64{}
	for i, text := range []string{docParaphrase, docIdentifier, docNeighbour, docUnrelated} {
		vecs, err := emb.Embed(ctx, []string{text})
		if err != nil {
			t.Fatalf("embed: %v", err)
		}
		id, err := db.PutSummary(ctx, store.Summary{
			Kind:      store.SummarySession,
			Runtime:   "claude-code",
			SessionID: "sess-" + string(rune('a'+i)),
			Path:      "/tmp/transcript.jsonl",
			Text:      text,
			Model:     "small",
			CreatedAt: time.Date(2026, 8, 1+i, 12, 0, 0, 0, time.UTC),
		}, vecs[0])
		if err != nil {
			t.Fatalf("put summary: %v", err)
		}
		ids[text] = id
	}

	s, err := search.New(search.Options{DB: db, Embedder: emb})
	if err != nil {
		t.Fatalf("new searcher: %v", err)
	}
	return db, s, ids
}

// TestExactIdentifierBeatsItsSemanticNeighbour is the reason this package fuses
// instead of just embedding.
//
// MEMORY.md §3: exact identifiers — a repo name, an error string,
// STRIPE_SECRET_KEY — are where vector search is weakest and BM25 is strongest,
// and those are most of what routing actually looks up. The test proves all
// three legs: the dense half really does get this wrong, the lexical half gets
// it right, and fusion keeps the right answer on top.
func TestExactIdentifierBeatsItsSemanticNeighbour(t *testing.T) {
	ctx := context.Background()
	_, s, ids := identifierCorpus(t)

	dense, err := s.Search(ctx, search.Query{Text: queryText, Mode: search.ModeVector})
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if len(dense.Hits) < 2 {
		t.Fatalf("dense half returned %d hits", len(dense.Hits))
	}
	if dense.Hits[0].Summary.ID != ids[docParaphrase] {
		t.Fatalf("premise broken: dense half ranked %q first, so this test proves nothing",
			dense.Hits[0].Summary.Text)
	}
	densePos := rankOf(dense.Hits, ids[docIdentifier])
	if densePos <= 1 {
		t.Fatalf("premise broken: the identifier doc is at dense position %d", densePos)
	}

	lex, err := s.Search(ctx, search.Query{Text: queryText, Mode: search.ModeLexical})
	if err != nil {
		t.Fatalf("lexical search: %v", err)
	}
	if len(lex.Hits) == 0 || lex.Hits[0].Summary.ID != ids[docIdentifier] {
		t.Fatalf("BM25 did not put the exact identifier first: %v", texts(lex.Hits))
	}

	hybrid, err := s.Search(ctx, search.Query{Text: queryText})
	if err != nil {
		t.Fatalf("hybrid search: %v", err)
	}
	if !hybrid.Hybrid() {
		t.Fatalf("hybrid search degraded: %v", hybrid.Degraded)
	}
	if hybrid.Hits[0].Summary.ID != ids[docIdentifier] {
		t.Fatalf("fusion lost the exact identifier: got %q first, hits %v",
			hybrid.Hits[0].Summary.Text, texts(hybrid.Hits))
	}
	// And the semantically-adjacent one is still retrieved: fusion demotes it,
	// it does not delete it.
	if rankOf(hybrid.Hits, ids[docParaphrase]) == 0 {
		t.Fatal("fusion dropped the dense-only hit entirely")
	}
	// The winning hit must show that both halves saw it.
	top := hybrid.Hits[0]
	if top.LexicalRank == 0 || top.VectorRank == 0 {
		t.Fatalf("top hit reports lexical=%d vector=%d; both should be set", top.LexicalRank, top.VectorRank)
	}
}

func rankOf(hits []search.Result, id int64) int {
	for i, h := range hits {
		if h.Summary.ID == id {
			return i + 1
		}
	}
	return 0
}

func texts(hits []search.Result) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Summary.Text
	}
	return out
}

func TestSearchWithoutEmbedderDegradesVisibly(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	if _, err := db.PutSummary(ctx, store.Summary{
		Runtime: "codex", SessionID: "s1", Text: docIdentifier,
	}, nil); err != nil {
		t.Fatalf("put: %v", err)
	}
	s, err := search.New(search.Options{DB: db})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	res, err := s.Search(ctx, search.Query{Text: queryText})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("want the lexical hit, got %d", len(res.Hits))
	}
	if res.Hybrid() {
		t.Fatal("lexical-only results claimed to be hybrid")
	}
	if !strings.Contains(strings.Join(res.Degraded, " "), "no embedder") {
		t.Fatalf("degradation not explained: %v", res.Degraded)
	}
	if res.Hits[0].VectorRank != 0 {
		t.Fatal("a half that never ran reported a rank")
	}
}

func TestSearchRefusesAMismatchedEmbeddingWidth(t *testing.T) {
	db := openDB(t)
	_, err := search.New(search.Options{DB: db, Embedder: search.NewHashEmbedder(64)})
	if !errors.Is(err, search.ErrDims) {
		t.Fatalf("want ErrDims, got %v", err)
	}
}

// A model swap under a populated index is silent corruption: the vectors still
// have the right width and still return neighbours, and every one of them is
// meaningless. It has to be caught at query time and reported.
func TestSearchDegradesWhenTheIndexWasEmbeddedByAnotherModel(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	if err := search.SetEmbeddingModel(ctx, db, "some-other-model"); err != nil {
		t.Fatalf("set model: %v", err)
	}
	if _, err := db.PutSummary(ctx, store.Summary{
		Runtime: "codex", SessionID: "s1", Text: docIdentifier,
	}, nil); err != nil {
		t.Fatalf("put: %v", err)
	}
	s, err := search.New(search.Options{DB: db, Embedder: search.NewHashEmbedder(store.EmbeddingDims)})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	res, err := s.Search(ctx, search.Query{Text: queryText})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if res.Hybrid() {
		t.Fatal("searched a foreign vector space and called it hybrid")
	}
	if !strings.Contains(strings.Join(res.Degraded, " "), "some-other-model") {
		t.Fatalf("degradation did not name the model: %v", res.Degraded)
	}
	if err := search.SetEmbeddingModel(ctx, db, "relay-hash-v1"); !errors.Is(err, search.ErrEmbeddingModelChanged) {
		t.Fatalf("silently overwrote the index's embedding model: %v", err)
	}
}

func TestSearchFilters(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	mk := func(runtime, sess, text string, day int, kind store.SummaryKind) {
		t.Helper()
		if _, err := db.PutSummary(ctx, store.Summary{
			Kind: kind, Runtime: runtime, SessionID: sess, Text: text,
			CreatedAt: time.Date(2026, 8, day, 9, 0, 0, 0, time.UTC),
		}, nil); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	mk("claude-code", "a", "stripe checkout keys rotated", 1, store.SummarySession)
	mk("codex", "b", "stripe checkout keys audited", 2, store.SummaryCluster)
	mk("hermes", "c", "stripe checkout keys documented", 9, store.SummarySession)

	if err := db.PutSessionIndex(ctx, store.SessionIndex{
		ID: "x1", Runtime: "claude-code", SessionID: "a",
		Path: "/t/a.jsonl", Title: "Payments", Workspace: "/repo/api",
		StartedAt: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("put index: %v", err)
	}

	s, err := search.New(search.Options{DB: db})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	cases := []struct {
		name  string
		q     search.Query
		wantN int
	}{
		{"runtime", search.Query{Text: "stripe checkout", Runtime: "codex"}, 1},
		{"kind", search.Query{Text: "stripe checkout", Kind: store.SummaryCluster}, 1},
		{"workspace", search.Query{Text: "stripe checkout", Workspace: "/repo/api"}, 1},
		{"since", search.Query{Text: "stripe checkout", Since: time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)}, 1},
		{"until", search.Query{Text: "stripe checkout", Until: time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)}, 2},
		{"none", search.Query{Text: "stripe checkout"}, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := s.Search(ctx, c.q)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if len(res.Hits) != c.wantN {
				t.Fatalf("want %d hits, got %d: %v", c.wantN, len(res.Hits), texts(res.Hits))
			}
		})
	}
}

// A hit has to be speakable without a second query: "the payments branch" comes
// from the session_index row, and hydration joins it in the one query that
// already runs.
func TestSearchHydratesTheSessionRow(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	if _, err := db.PutSummary(ctx, store.Summary{
		Runtime: "hermes", SessionID: "s9", Path: "/t/s9.jsonl",
		Text: "Fixed the webhook signature check.", ByteOffset: 4096,
	}, nil); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := db.PutSessionIndex(ctx, store.SessionIndex{
		ID: "i9", Runtime: "hermes", SessionID: "s9", Path: "/t/s9.jsonl",
		Title: "Webhook signatures", Workspace: "/repo/payments",
		StartedAt: time.Date(2026, 7, 4, 8, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("index: %v", err)
	}
	s, _ := search.New(search.Options{DB: db})
	res, err := s.Search(ctx, search.Query{Text: "webhook signature"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("want 1 hit, got %d", len(res.Hits))
	}
	h := res.Hits[0]
	if h.Title != "Webhook signatures" || h.Workspace != "/repo/payments" {
		t.Fatalf("session row not joined: %+v", h)
	}
	if h.StartedAt.IsZero() {
		t.Fatal("started_at not hydrated")
	}
	// The index holds a pointer, never a copy: the hit must be able to reopen
	// the original transcript.
	if h.Summary.Path == "" || h.Summary.ByteOffset != 4096 {
		t.Fatalf("lost the pointer into the transcript: %+v", h.Summary)
	}
}

func TestSearchEmptyQuery(t *testing.T) {
	db := openDB(t)
	s, _ := search.New(search.Options{DB: db})
	if _, err := s.Search(context.Background(), search.Query{Text: "  ,,, "}); !errors.Is(err, search.ErrEmptyQuery) {
		t.Fatalf("want ErrEmptyQuery, got %v", err)
	}
}

// Fusion can only promote what a half retrieved, so the candidate depth has to
// exceed the result limit. A depth equal to the limit makes fusion decorative.
func TestSearchDepthExceedsLimit(t *testing.T) {
	if search.DefaultDepth <= search.DefaultLimit {
		t.Fatalf("DefaultDepth %d must exceed DefaultLimit %d", search.DefaultDepth, search.DefaultLimit)
	}
}

// The exact retriever is what actually decides the identifier case, so its
// contribution has to be visible rather than folded into the lexical score.
func TestExactRetrieverRunsOnlyForIdentifiers(t *testing.T) {
	ctx := context.Background()
	_, s, ids := identifierCorpus(t)

	withID, err := s.Search(ctx, search.Query{Text: queryText})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !withID.Match.HasIdentifier() {
		t.Fatalf("%q was not recognised as an identifier query", queryText)
	}
	if withID.ExactCandidates == 0 {
		t.Fatal("the exact retriever returned nothing for an exact identifier")
	}
	if withID.Hits[0].ExactRank != 1 {
		t.Fatalf("the winning hit was not the exact match: %+v", withID.Hits[0])
	}
	// The paraphrase must not be in the exact list at all — it never says
	// STRIPE_SECRET_KEY.
	for _, h := range withID.Hits {
		if h.Summary.ID == ids[docParaphrase] && h.ExactRank != 0 {
			t.Fatalf("paraphrase appeared in the exact list at rank %d", h.ExactRank)
		}
	}

	// A descriptive query names nothing, so the exact retriever must not run:
	// AND-ing ordinary words together is a precision trap, not a signal.
	plain, err := s.Search(ctx, search.Query{Text: "the billing dashboard"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if plain.Match.HasIdentifier() {
		t.Fatalf("ordinary words treated as an identifier: %v", plain.Match.Identifiers)
	}
	if plain.ExactCandidates != 0 {
		t.Fatal("exact retriever ran on a descriptive query")
	}
}

func TestErrorStringsCountAsIdentifiers(t *testing.T) {
	// MEMORY.md §3 names error strings alongside repo names and
	// STRIPE_SECRET_KEY. A shouted acronym is the shape they arrive in.
	m, err := search.BuildMatch("that ECONNREFUSED on the deploy")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !m.HasIdentifier() {
		t.Fatalf("ECONNREFUSED not treated as an identifier: %+v", m)
	}
	// A caps-lock query is not fourteen identifiers.
	shout, err := search.BuildMatch("WHERE IS THE PAYMENTS WORK")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if shout.HasIdentifier() {
		t.Fatalf("caps lock read as identifiers: %v", shout.Identifiers)
	}
}

// internal/llm's Embedder is deliberately a superset of this package's, so a
// real provider satisfies search.Embedder without either package importing the
// other. This assertion is what notices if the two ever drift apart — the
// failure otherwise appears as "the configured embedder cannot be used", at
// wiring time, in a different package.
func TestLLMEmbedderSatisfiesSearchEmbedder(t *testing.T) {
	var _ search.Embedder = (llm.Embedder)(nil)
	if llm.EmbeddingDims != store.EmbeddingDims {
		t.Fatalf("llm.EmbeddingDims %d, store.EmbeddingDims %d — one of them is wrong and vec0's width is fixed at create time",
			llm.EmbeddingDims, store.EmbeddingDims)
	}
}
