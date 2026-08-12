package summarize_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/index"
	"github.com/luthor007/relay/relayd/internal/search"
	"github.com/luthor007/relay/relayd/internal/store"
	"github.com/luthor007/relay/relayd/internal/summarize"
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

func newSummarizer(t *testing.T, db *store.DB, model *fakeModel, emb search.Embedder) *summarize.Summarizer {
	t.Helper()
	opts := summarize.Options{DB: db, Redactor: summarize.Detector(), Embedder: emb}
	if model != nil {
		opts.Model = model
	}
	s, err := summarize.New(opts)
	if err != nil {
		t.Fatalf("new summarizer: %v", err)
	}
	return s
}

// Indexing without a detector is not a configuration this package offers. The
// ordering in MEMORY.md §6 — detect first, index second — stops being a
// convention when the constructor enforces it.
func TestSummarizerRefusesToRunWithoutAScanner(t *testing.T) {
	db := openDB(t)
	_, err := summarize.New(summarize.Options{DB: db})
	if !errors.Is(err, summarize.ErrNoRedactor) {
		t.Fatalf("built a summarizer with no detector: %v", err)
	}
	_, err = summarize.New(summarize.Options{Redactor: summarize.Detector()})
	if !errors.Is(err, summarize.ErrNoStore) {
		t.Fatalf("built a summarizer with no store: %v", err)
	}
}

func sampleSession() summarize.SessionInput {
	start := time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC)
	return summarize.SessionInput{
		Runtime:    "claude-code",
		SessionID:  "1f0c0d2e-0000-4000-8000-000000000001",
		Path:       "/home/u/.claude/projects/api/1f0c.jsonl",
		ByteOffset: 0,
		ByteLength: 48_000,
		Workspace:  "/home/u/src/api",
		GitBranch:  "payments",
		Model:      "claude-opus-5",
		StartedAt:  start,
		EndedAt:    start.Add(90 * time.Minute),
		Messages:   42,
		ToolCalls:  17,
		Excerpt:    "user: the stripe webhook keeps 400ing\nagent: the signature check was using the wrong secret; fixed and redeployed",
		Clusters: []summarize.ClusterInput{
			{Index: 0, ByteOffset: 0, ByteLength: 20000, StartedAt: start, Turns: 5,
				Tools: []string{"Bash", "Read"}, Excerpt: "user: reproduce the 400\nagent: the payload digest differs"},
			{Index: 1, ByteOffset: 20000, ByteLength: 28000, StartedAt: start.Add(time.Hour), Turns: 4,
				Tools: []string{"Edit"}, Excerpt: "agent: swapped the signing secret and redeployed"},
		},
	}
}

// MEMORY.md §4: Claude Code and Hermes already title their own sessions, so the
// summariser's first job is done for a large share of the corpus. Taking theirs
// is worth one model call per session across 3.6 GB of history, which is the
// difference between a one-hour install step and a two-hour one.
func TestTitleIsStolenFromTheRuntimeWhenItHasOne(t *testing.T) {
	ctx := context.Background()

	withTitle := sampleSession()
	withTitle.Title = "Stripe webhook signature mismatch"
	model := &fakeModel{Reply: "Fixed the Stripe webhook signature check and redeployed."}
	s := newSummarizer(t, openDB(t), model, nil)

	res, err := s.SummarizeSession(ctx, withTitle)
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if !res.TitleStolen {
		t.Fatal("did not take the runtime's title")
	}
	if res.Title != "Stripe webhook signature mismatch" {
		t.Fatalf("title %q", res.Title)
	}
	stolenCalls := model.calls()

	// The same session with no title costs one extra call — the title one.
	noTitle := sampleSession()
	model2 := &fakeModel{Replies: []string{
		"Stripe webhook signatures",
		"Fixed the Stripe webhook signature check and redeployed.",
		"Reproduced the 400.", "Swapped the signing secret.",
	}}
	s2 := newSummarizer(t, openDB(t), model2, nil)
	res2, err := s2.SummarizeSession(ctx, noTitle)
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if res2.TitleStolen {
		t.Fatal("claimed to steal a title that did not exist")
	}
	if model2.calls() != stolenCalls+1 {
		t.Fatalf("titling cost %d calls, not one (%d vs %d)",
			model2.calls()-stolenCalls, model2.calls(), stolenCalls)
	}
	if res2.ModelCalls != model2.calls() {
		t.Fatalf("ModelCalls reported %d, actual %d", res2.ModelCalls, model2.calls())
	}
}

// Step 2b of MEMORY.md §11 is a working product: a plain indexed list of every
// session with its title, repo and date, no model and no embeddings.
func TestSummarizeWithNoModelStillIndexesSomethingUseful(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	s := newSummarizer(t, db, nil, nil)

	res, err := s.SummarizeSession(ctx, sampleSession())
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if res.ModelCalls != 0 {
		t.Fatalf("called a model that was not configured")
	}
	if res.Embedded {
		t.Fatal("claimed to embed with no embedder")
	}
	for _, want := range []string{"api", "payments"} {
		if !strings.Contains(res.Summary+res.Title, want) {
			t.Fatalf("metadata summary lost %q: title=%q summary=%q", want, res.Title, res.Summary)
		}
	}

	// It is searchable, which is the whole claim of step 2b.
	sr, err := search.New(search.Options{DB: db})
	if err != nil {
		t.Fatalf("searcher: %v", err)
	}
	hits, err := sr.Search(ctx, search.Query{Text: "payments"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits.Hits) == 0 {
		t.Fatal("nothing indexed")
	}
}

func TestSummarizeWritesTheWholeIndexRow(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	emb := search.NewHashEmbedder(store.EmbeddingDims)
	model := &fakeModel{Reply: "Fixed the Stripe webhook signature check on the payments branch."}
	s := newSummarizer(t, db, model, emb)

	in := sampleSession()
	in.Title = "Stripe webhook signature mismatch"
	res, err := s.SummarizeSession(ctx, in)
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if !res.Embedded {
		t.Fatal("not embedded")
	}
	if len(res.ClusterIDs) != 2 {
		t.Fatalf("cluster summaries: %d", len(res.ClusterIDs))
	}

	row, err := db.GetSessionIndex(ctx, in.Runtime, in.SessionID)
	if err != nil {
		t.Fatalf("session index: %v", err)
	}
	// The index stores a pointer, never a copy.
	if row.Path != in.Path || row.Title != in.Title || row.Workspace != in.Workspace {
		t.Fatalf("row: %+v", row)
	}
	if row.Messages != 42 || row.ToolCalls != 17 {
		t.Fatalf("counts: %+v", row)
	}
	if row.IndexedAt.IsZero() {
		t.Fatalf("indexed_at not set: %+v", row)
	}

	// Each cluster summary points at its own byte range, so a hit can be
	// reopened at the right place in a 48 kB transcript.
	sum, err := db.GetSummary(ctx, res.ClusterIDs[1])
	if err != nil {
		t.Fatalf("cluster summary: %v", err)
	}
	if sum.ByteOffset != 20000 || sum.ByteLength != 28000 {
		t.Fatalf("cluster pointer: %+v", sum)
	}
	if sum.Kind != store.SummaryCluster {
		t.Fatalf("kind %q", sum.Kind)
	}

	// And the embedding model was recorded, so a later build cannot query this
	// index with a different one and get plausible nonsense.
	got, err := db.Meta(ctx, "embedding_model")
	if err != nil || got != emb.Model() {
		t.Fatalf("embedding_model %q %v", got, err)
	}
}

// The rule that cannot be got wrong: a credential in a transcript must be gone
// before the text reaches a model and before it reaches the index. An embedded
// key cannot be unembedded, and a key posted to a provider has already left the
// machine.
func TestSecretsAreRedactedBeforeTheModelAndBeforeTheIndex(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	const key = "glpat-TESTONLYneverIssuedToAnybody04"
	model := &fakeModel{Reply: "Rotated the token."}
	s := newSummarizer(t, db, model, search.NewHashEmbedder(store.EmbeddingDims))

	in := sampleSession()
	in.Title = "Rotating " + key
	in.Excerpt = "user: the old key " + key + " leaked, rotate it\nagent: done"
	in.Clusters[0].Excerpt = "agent: replaced " + key

	res, err := s.SummarizeSession(ctx, in)
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}

	// Nothing the model saw carried the key.
	for _, p := range model.allPrompts() {
		if strings.Contains(p, key) {
			t.Fatalf("the key was sent to a model:\n%s", p)
		}
	}
	// Nothing in the index carries it either — title, session summary or any
	// cluster summary.
	if strings.Contains(res.Title+res.Summary, key) {
		t.Fatalf("key in the summary: %q / %q", res.Title, res.Summary)
	}
	rows, err := db.SQL().QueryContext(ctx, `SELECT text FROM summary`)
	if err != nil {
		t.Fatalf("read summaries: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(text, key) {
			t.Fatalf("key in an indexed summary: %q", text)
		}
	}
	row, err := db.GetSessionIndex(ctx, in.Runtime, in.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row.Title, key) {
		t.Fatalf("key in the session title: %q", row.Title)
	}

	// The findings reach the caller, which is what the vault proposal flow in
	// MEMORY.md §6 needs. The secret_marker rows themselves are written by
	// internal/index, which read the same text off disk first: one credential,
	// one marker, one writer.
	if len(res.Findings) == 0 {
		t.Fatal("findings not reported to the caller")
	}
	// The vendor named here follows the fixture, and the fixture's shape is
	// constrained by what may be published — see cmd/relay/backfill_test.go.
	// What is being asserted is that a tier-1 vendor finding reaches the caller
	// proposable, not that it came from any particular vendor.
	var proposable bool
	for _, f := range res.Findings {
		if f.Service == "gitlab" && summarize.Proposable(f) {
			proposable = true
		}
	}
	if !proposable {
		t.Fatalf("no proposable tier-1 finding: %+v", res.Findings)
	}
}

// Backfill is incremental and resumable, so a session that grew gets
// re-summarised. That must replace its summaries rather than accumulate a
// second set — including in the vector table, which has no delete trigger.
func TestReindexingReplacesRatherThanAccumulates(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	s := newSummarizer(t, db, nil, search.NewHashEmbedder(store.EmbeddingDims))

	in := sampleSession()
	if _, err := s.SummarizeSession(ctx, in); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	in.Messages = 60
	in.Excerpt += "\nagent: and added a regression test"
	if _, err := s.SummarizeSession(ctx, in); err != nil {
		t.Fatalf("second pass: %v", err)
	}

	var summaries, vectors int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT count(*) FROM summary WHERE runtime = ? AND session_id = ?`,
		in.Runtime, in.SessionID).Scan(&summaries); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRowContext(ctx, `SELECT count(*) FROM summary_vec`).Scan(&vectors); err != nil {
		t.Fatal(err)
	}
	if summaries != 3 {
		t.Fatalf("%d summaries after re-indexing, want 3 (one session, two clusters)", summaries)
	}
	if vectors != summaries {
		t.Fatalf("%d vectors for %d summaries — orphaned rowids in summary_vec", vectors, summaries)
	}

	row, err := db.GetSessionIndex(ctx, in.Runtime, in.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Messages != 60 {
		t.Fatalf("row not updated: %+v", row)
	}
}

func TestEmbeddingFailureStillIndexesTheLexicalHalf(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	// An embedder with no fallback and no vectors: every Embed call fails.
	broken := &search.StaticEmbedder{Name: "broken", Vectors: map[string][]float32{
		"never used": make([]float32, store.EmbeddingDims),
	}}
	s := newSummarizer(t, db, nil, broken)

	res, err := s.SummarizeSession(ctx, sampleSession())
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if res.Embedded {
		t.Fatal("claimed to embed after a provider failure")
	}
	if res.SummaryID == 0 {
		t.Fatal("nothing written at all — a dense-half failure took the whole index down")
	}
}

func TestTitleAndDerivedTitle(t *testing.T) {
	in := sampleSession()
	if _, ok := summarize.Title(in); ok {
		t.Fatal("stole a title from a session with none")
	}
	in.Title = "  A very long runtime-supplied title that goes well past the character budget for a spoken or displayed session name  "
	got, ok := summarize.Title(in)
	if !ok {
		t.Fatal("did not take the title")
	}
	if len([]rune(got)) > summarize.MaxTitleChars {
		t.Fatalf("%d chars: %q", len([]rune(got)), got)
	}

	derived := summarize.DerivedTitle(sampleSession())
	if !strings.Contains(derived, "api") || !strings.Contains(derived, "payments") {
		t.Fatalf("derived title lost the repo or branch: %q", derived)
	}
	if !strings.Contains(derived, "2026") {
		t.Fatalf("derived title lost the date: %q", derived)
	}
}

// internal/index owns the session row during backfill — MEMORY.md §11 splits
// step 2b (readers and the session index) from step 2c (summaries and hybrid
// search) — so this package must fill gaps and never erase. The two steps run
// in either order without one undoing the other.
func TestSessionRowMergesRatherThanOverwrites(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)

	// A reader got there first and knew things this package never sees.
	in := sampleSession()
	if err := db.PutSessionIndex(ctx, store.SessionIndex{
		ID:          in.Runtime + "/" + in.SessionID,
		Runtime:     in.Runtime,
		SessionID:   in.SessionID,
		Path:        in.Path,
		Title:       "Stripe webhook signature mismatch",
		Workspace:   in.Workspace,
		GitBranch:   in.GitBranch,
		StartedAt:   in.StartedAt,
		Messages:    42,
		ToolCalls:   17,
		SourceMTime: time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC),
		SourceSize:  48_000,
	}); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	// The summariser runs with none of that metadata in hand.
	bare := summarize.SessionInput{
		Runtime: in.Runtime, SessionID: in.SessionID,
		Excerpt: in.Excerpt, Clusters: in.Clusters,
	}
	if _, err := newSummarizer(t, db, nil, nil).SummarizeSession(ctx, bare); err != nil {
		t.Fatalf("summarize: %v", err)
	}

	row, err := db.GetSessionIndex(ctx, in.Runtime, in.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Title != "Stripe webhook signature mismatch" {
		t.Fatalf("the reader's title was overwritten: %q", row.Title)
	}
	if row.Path != in.Path || row.Workspace != in.Workspace || row.GitBranch != in.GitBranch {
		t.Fatalf("the reader's pointer or metadata was blanked: %+v", row)
	}
	if row.Messages != 42 || row.ToolCalls != 17 {
		t.Fatalf("counts blanked: %+v", row)
	}
	if row.SourceSize != 48_000 || row.SourceMTime.IsZero() {
		t.Fatalf("resume key blanked, so backfill would redo this session: %+v", row)
	}
}

// The handoff between step 2b and step 2c, in one call: the reader's row and
// its already-redacted text become the summariser's input, and nothing the
// reader knew is lost on the way.
func TestFromIndexed(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)

	sess := index.Session{
		Runtime: "hermes", SessionID: "h-1",
		Path: "/home/u/.hermes/state.db", ByteOffset: 0,
		Title: "Webhook signatures", TitleSource: index.TitleGenerated,
		Workspace: "/home/u/src/api", GitBranch: "payments", Model: "sonnet",
		StartedAt: time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC),
		Messages:  12, ToolCalls: 3,
		SourceMTime: time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC),
		SourceSize:  9000,
		Text:        "user: the webhook keeps 400ing with glpat-TESTONLYneverIssuedToAnybody05",
	}
	res, err := index.New(db, nil).Index(ctx, sess)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if strings.Contains(res.Redacted, "glpat-") {
		t.Fatalf("the reader handed over an unredacted transcript: %q", res.Redacted)
	}

	in := summarize.FromIndexed(sess, res.Redacted, nil)
	if in.Runtime != "hermes" || in.SessionID != "h-1" || in.Title != "Webhook signatures" {
		t.Fatalf("lost the reader's metadata: %+v", in)
	}
	if in.Messages != 12 || in.ToolCalls != 3 || in.SourceSize != 9000 {
		t.Fatalf("lost the counts or the resume key: %+v", in)
	}

	out, err := newSummarizer(t, db, nil, nil).SummarizeSession(ctx, in)
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if !out.TitleStolen {
		t.Fatal("the runtime's title did not survive the handoff")
	}
	if len(out.Findings) != 0 {
		t.Fatalf("the reader already redacted; the summariser found %d more: %+v", len(out.Findings), out.Findings)
	}

	row, err := db.GetSessionIndex(ctx, "hermes", "h-1")
	if err != nil {
		t.Fatal(err)
	}
	if row.Title != "Webhook signatures" || row.Workspace != "/home/u/src/api" {
		t.Fatalf("the summariser damaged the reader's row: %+v", row)
	}
}
