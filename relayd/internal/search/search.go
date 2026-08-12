package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/logx"
	"github.com/luthor007/relay/relayd/internal/store"
)

// The three retriever names, used as [List.Name] and as the keys in
// [Fused.Ranks]. They are constants because the console displays them and a
// renamed key is a silently empty column.
const (
	// ListLexical is BM25 over the whole query, OR'd. Broad recall.
	ListLexical = "bm25"
	// ListVector is the dense half.
	ListVector = "cosine"
	// ListExact returns only documents containing every identifier in the
	// query. It runs only when the query has an identifier in it, and it is the
	// difference between finding the summary that says STRIPE_SECRET_KEY and
	// finding four summaries about billing.
	//
	// It is a third retriever rather than a weight on the lexical half because
	// a weight is a global thumb on the scale and this is evidence about one
	// document: either the summary contains the identifier or it does not.
	// Measured on the corpus in this package's tests, weighting BM25 up by 3×
	// still left an exact match a hair behind a paraphrase that was rank 2
	// lexically and rank 1 densely; the exact list wins the same case by ~50%.
	ListExact = "exact"
)

// Mode selects which halves run. Hybrid is the default and the only one
// MEMORY.md §3 endorses; the other two exist for diagnosis and for the case
// where a caller genuinely wants one behaviour — the backfill verifier checks
// lexical-only results against the session list it just built.
type Mode string

const (
	ModeHybrid  Mode = ""
	ModeLexical Mode = "lexical"
	ModeVector  Mode = "vector"
)

// Defaults. Depth is per half, before fusion: fusion can only promote a
// document that at least one half retrieved, so a depth equal to the limit
// would make fusion decorative.
const (
	DefaultLimit = 10
	DefaultDepth = 50
	MaxDepth     = 400
	// FilterOverfetch multiplies depth when a query carries metadata filters.
	// The vector half cannot be pre-filtered — summary_vec is a vec0 table with
	// one column and no metadata to filter on — so filtering happens after the
	// kNN and the candidate set has to be deeper to survive it.
	FilterOverfetch = 4
	// SlowQuery is when a lookup gets logged. The measured budget is 45 ms
	// median at 22k vectors on the build we ship (MEMORY.md §3); 300 ms means
	// something is wrong, not that the corpus grew a little.
	SlowQuery = 300 * time.Millisecond
)

// Options configures a Searcher.
type Options struct {
	DB *store.DB
	// Embedder may be nil. A nil embedder is not an error — it is lexical-only
	// retrieval, reported in Results.Degraded on every query.
	Embedder Embedder
	// K is the rank-fusion constant; DefaultRRFK when zero.
	K float64
	// LexicalWeight, VectorWeight and ExactWeight scale each retriever's
	// contribution; 1 when zero.
	LexicalWeight float64
	VectorWeight  float64
	ExactWeight   float64
	Log           *slog.Logger
	Now           func() time.Time
}

// Searcher runs MEMORY.md §3's hybrid retrieval over the index.
type Searcher struct {
	db     *store.DB
	emb    Embedder
	k      float64
	wLex   float64
	wVec   float64
	wExact float64
	log    *slog.Logger
	now    func() time.Time
}

// New builds a Searcher.
//
// It fails when the embedder's width does not match the index. That is
// deliberate: vec0 fixes a column's width at create time, so a mismatched
// embedder cannot write and cannot query, and finding that out at the first
// voice lookup instead of at startup is the worst possible time.
func New(o Options) (*Searcher, error) {
	if o.DB == nil {
		return nil, errors.New("search: no database")
	}
	if o.Embedder != nil && o.Embedder.Dims() != store.EmbeddingDims {
		return nil, fmt.Errorf("%w: embedder %s is %d, summary_vec is %d",
			ErrDims, o.Embedder.Model(), o.Embedder.Dims(), store.EmbeddingDims)
	}
	s := &Searcher{
		db:     o.DB,
		emb:    o.Embedder,
		k:      o.K,
		wLex:   o.LexicalWeight,
		wVec:   o.VectorWeight,
		wExact: o.ExactWeight,
		log:    o.Log,
		now:    o.Now,
	}
	if s.k == 0 {
		s.k = DefaultRRFK
	}
	if s.wLex == 0 {
		s.wLex = 1
	}
	if s.wVec == 0 {
		s.wVec = 1
	}
	if s.wExact == 0 {
		s.wExact = 1
	}
	if s.log == nil {
		s.log = logx.Discard()
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// Embedder is the configured embedder, or nil.
func (s *Searcher) Embedder() Embedder { return s.emb }

// Query is one lookup.
type Query struct {
	Text string
	// Limit is how many results to return; DefaultLimit when zero.
	Limit int
	// Depth is how many candidates each half retrieves before fusion;
	// DefaultDepth when zero, capped at MaxDepth.
	Depth int
	Mode  Mode

	// Filters. Every one of these is applied AFTER retrieval, because the
	// vector half cannot be pre-filtered; see FilterOverfetch.
	Runtime   string
	Workspace string
	Kind      store.SummaryKind
	Since     time.Time
	Until     time.Time
}

// Result is one fused, hydrated hit.
type Result struct {
	Summary store.Summary
	// Score is the fused RRF score. Comparable within one Results, nowhere else.
	Score float64
	// LexicalRank, VectorRank and ExactRank are 1-based positions, or 0 when
	// that retriever did not return this document at all. Zero means "not
	// retrieved", never "ranked zeroth".
	LexicalRank int
	VectorRank  int
	ExactRank   int
	// BM25 and Distance are each half's own raw score, for display and
	// debugging. They are on unrelated scales and must not be added.
	BM25     float64
	Distance float64

	// Title and Workspace come from the session_index row this summary points
	// at, so a hit can be spoken ("the payments branch, on Tuesday") without a
	// second query.
	Title     string
	Workspace string
	StartedAt time.Time
}

// Results is one lookup's answer.
type Results struct {
	Hits []Result
	// Degraded names every half that did not run, and why. A caller that gets
	// lexical-only results is told so rather than being handed a quietly worse
	// answer — the same rule the adapters apply at the runtime boundary.
	Degraded []string
	Elapsed  time.Duration
	// Candidates is how many each retriever returned before fusion.
	LexicalCandidates int
	VectorCandidates  int
	ExactCandidates   int
	Match             MatchQuery
}

// Hybrid reports whether both halves actually ran.
func (r Results) Hybrid() bool { return len(r.Degraded) == 0 }

// Search runs the retrievers and fuses them by rank.
//
// The shape is one embedding call, one kNN, one or two FTS5 queries and one
// hydration query — at most five round trips regardless of how many results
// come back, and none of them per-candidate. On the pure-Go wasm sqlite-vec
// build we ship the kNN alone is ~45 ms median at the 22k design target and
// ~190 ms at 100k (MEMORY.md §3), so a per-candidate query would blow
// SYSTEM.md §7b's budget on its own.
func (s *Searcher) Search(ctx context.Context, q Query) (Results, error) {
	start := s.now()
	res := Results{}

	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	depth := q.Depth
	if depth <= 0 {
		depth = DefaultDepth
	}
	if q.filtered() {
		depth *= FilterOverfetch
	}
	if depth > MaxDepth {
		depth = MaxDepth
	}
	if depth < limit {
		depth = limit
	}

	m, err := BuildMatch(q.Text)
	if err != nil {
		return res, err
	}
	res.Match = m

	var lists []List

	if q.Mode != ModeVector {
		hits, err := s.db.SearchLexical(ctx, m.Expr, depth)
		if err != nil {
			return res, fmt.Errorf("search: lexical half: %w", err)
		}
		res.LexicalCandidates = len(hits)
		lists = append(lists, List{Name: ListLexical, Hits: hits, Weight: s.wLex})

		if m.ExactExpr != "" {
			exact, err := s.db.SearchLexical(ctx, m.ExactExpr, depth)
			if err != nil {
				return res, fmt.Errorf("search: exact half: %w", err)
			}
			res.ExactCandidates = len(exact)
			lists = append(lists, List{Name: ListExact, Hits: exact, Weight: s.wExact})
		}
	} else {
		res.Degraded = append(res.Degraded, "lexical half disabled by mode=vector")
	}

	if q.Mode != ModeLexical {
		hits, reason, err := s.vectorHalf(ctx, q.Text, depth)
		if err != nil {
			return res, err
		}
		if reason != "" {
			res.Degraded = append(res.Degraded, reason)
		} else {
			res.VectorCandidates = len(hits)
			lists = append(lists, List{Name: ListVector, Hits: hits, Weight: s.wVec})
		}
	} else {
		res.Degraded = append(res.Degraded, "dense half disabled by mode=lexical")
	}

	fused := Fuse(s.k, lists...)
	if len(fused) == 0 {
		res.Elapsed = s.now().Sub(start)
		return res, nil
	}

	ids := make([]int64, 0, len(fused))
	for _, f := range fused {
		ids = append(ids, f.SummaryID)
	}
	rows, err := s.hydrate(ctx, ids)
	if err != nil {
		return res, err
	}

	for _, f := range fused {
		r, ok := rows[f.SummaryID]
		if !ok {
			// The vector table outlived its summary row. Skip it rather than
			// returning a hit with no text; a dangling rowid is a bug to fix in
			// deletion, not something to surface to a person.
			s.log.Warn("search: fused hit has no summary row", "summary_id", f.SummaryID)
			continue
		}
		if !q.keep(r) {
			continue
		}
		r.Score = f.Score
		r.LexicalRank = f.Ranks[ListLexical]
		r.VectorRank = f.Ranks[ListVector]
		r.ExactRank = f.Ranks[ListExact]
		r.BM25 = f.Scores[ListLexical]
		r.Distance = f.Scores[ListVector]
		res.Hits = append(res.Hits, r)
		if len(res.Hits) >= limit {
			break
		}
	}

	res.Elapsed = s.now().Sub(start)
	if res.Elapsed >= SlowQuery {
		s.log.Warn("search: slow lookup",
			"elapsed_ms", res.Elapsed.Milliseconds(),
			"lexical_candidates", res.LexicalCandidates,
			"exact_candidates", res.ExactCandidates,
			"vector_candidates", res.VectorCandidates)
	}
	return res, nil
}

func (q Query) filtered() bool {
	return q.Runtime != "" || q.Workspace != "" || q.Kind != "" ||
		!q.Since.IsZero() || !q.Until.IsZero()
}

func (q Query) keep(r Result) bool {
	if q.Runtime != "" && r.Summary.Runtime != q.Runtime {
		return false
	}
	if q.Kind != "" && r.Summary.Kind != q.Kind {
		return false
	}
	if q.Workspace != "" && r.Workspace != q.Workspace {
		return false
	}
	if !q.Since.IsZero() && r.Summary.CreatedAt.Before(q.Since) {
		return false
	}
	if !q.Until.IsZero() && r.Summary.CreatedAt.After(q.Until) {
		return false
	}
	return true
}

// vectorHalf runs the dense retriever, or explains why it did not.
//
// The model check is the important part. An index written by one embedding
// model and queried by another shares a vector space only by coincidence, and
// the failure is silent — plausible-looking nonsense at the top of every result
// list. meta.embedding_model is written on the first embedded summary, so a
// mismatch is detectable and is reported as degradation rather than guessed at.
func (s *Searcher) vectorHalf(ctx context.Context, text string, depth int) ([]store.Hit, string, error) {
	if s.emb == nil {
		return nil, NoEmbedderReason, nil
	}
	indexed, err := s.db.Meta(ctx, "embedding_model")
	if err != nil {
		return nil, "", fmt.Errorf("search: read embedding_model: %w", err)
	}
	if indexed != "" && indexed != s.emb.Model() {
		// Degradation, not an error: the lexical half still answers, and the
		// reason names both models and the way out. See MismatchReason.
		return nil, MismatchReason(indexed, s.emb.Model()), nil
	}

	vecs, err := s.emb.Embed(ctx, []string{text})
	if err != nil {
		// A provider outage must not take search down. Say so and fall back.
		s.log.Warn("search: embedding the query failed, falling back to lexical", "err", err)
		return nil, "embedding the query failed: lexical only", nil
	}
	if len(vecs) != 1 || len(vecs[0]) != store.EmbeddingDims {
		return nil, "embedder returned the wrong shape: lexical only", nil
	}
	hits, err := s.db.SearchVector(ctx, vecs[0], depth)
	if err != nil {
		return nil, "", fmt.Errorf("search: dense half: %w", err)
	}
	return hits, "", nil
}

// hydrate reads every fused id in one query, joined to its session_index row.
func (s *Searcher) hydrate(ctx context.Context, ids []int64) (map[int64]Result, error) {
	if len(ids) == 0 {
		return map[int64]Result{}, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.SQL().QueryContext(ctx, `
		SELECT s.id, s.kind, s.runtime, s.session_id, s.path, s.byte_offset,
		       s.byte_length, s.text, s.model, s.created_at,
		       COALESCE(x.title, ''), COALESCE(x.workspace, ''), x.started_at
		FROM summary s
		LEFT JOIN session_index x
		       ON x.runtime = s.runtime AND x.session_id = s.session_id
		WHERE s.id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("search: hydrate: %w", err)
	}
	defer rows.Close()

	out := make(map[int64]Result, len(ids))
	for rows.Next() {
		var r Result
		var kind string
		var created int64
		var started *int64
		if err := rows.Scan(&r.Summary.ID, &kind, &r.Summary.Runtime, &r.Summary.SessionID,
			&r.Summary.Path, &r.Summary.ByteOffset, &r.Summary.ByteLength, &r.Summary.Text,
			&r.Summary.Model, &created, &r.Title, &r.Workspace, &started); err != nil {
			return nil, err
		}
		r.Summary.Kind = store.SummaryKind(kind)
		r.Summary.CreatedAt = time.UnixMilli(created).UTC()
		if started != nil && *started != 0 {
			r.StartedAt = time.UnixMilli(*started).UTC()
		}
		out[r.Summary.ID] = r
	}
	return out, rows.Err()
}

// SetEmbeddingModel records which model wrote the index's vectors, refusing to
// change it once set.
//
// It is here rather than in store because it is a retrieval invariant: the
// vectors already written cannot be compared with vectors from another model,
// so switching models is a re-embed, not a config change. The caller that wants
// to switch deletes and rebuilds.
func (s *Searcher) SetEmbeddingModel(ctx context.Context, model string) error {
	return SetEmbeddingModel(ctx, s.db, model)
}

// ErrEmbeddingModelChanged means the index already holds vectors from a
// different model.
var ErrEmbeddingModelChanged = errors.New("search: the index was embedded with a different model")

// SetEmbeddingModel records the embedding model on a database, once.
func SetEmbeddingModel(ctx context.Context, db *store.DB, model string) error {
	if model == "" {
		return errors.New("search: empty embedding model")
	}
	have, err := db.Meta(ctx, "embedding_model")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if have == model {
		return nil
	}
	if have != "" {
		return fmt.Errorf("%w: index holds %q, this build has %q; re-embed rather than switching",
			ErrEmbeddingModelChanged, have, model)
	}
	return db.SetMeta(ctx, "embedding_model", model)
}
