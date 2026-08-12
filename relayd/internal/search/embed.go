package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/luthor007/relay/relayd/internal/store"
)

// Embedder turns text into vectors. It is an interface with a deterministic
// local implementation ([HashEmbedder]) so nothing in this package's tests — or
// in the summariser's — makes a network call. Real providers are configured
// through internal/llm and wired in by the orchestrator.
//
// Embed takes a batch because every real provider charges and bills per call,
// and because a session summariser has many short texts rather than one long
// one.
type Embedder interface {
	// Model names the embedding model. It is written to the index's
	// embedding_model meta key on first write and checked on every search:
	// vectors from two different models share a space only by coincidence.
	Model() string

	// Dims is the vector width. It must equal store.EmbeddingDims — a vec0
	// column's width is fixed at create time, so a mismatch is a schema error
	// and not something to paper over at query time.
	Dims() int

	// Embed returns one vector per input, in order. Implementations must return
	// L2-normalised vectors: summary_vec is an L2 index, and on unit vectors L2
	// distance is monotonic in cosine similarity, so normalising is what makes
	// "cosine" in MEMORY.md §3 true of the thing we actually query.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// ErrDims is returned when an embedder's width does not match the index.
var ErrDims = errors.New("search: embedding width does not match the index")

// ---------------------------------------------------------- the local fake --

// HashEmbedder is a deterministic, dependency-free embedder: hashed
// bag-of-tokens, signed, L2-normalised. It makes no network call, produces the
// same vector for the same text on every machine and every run, and is the
// default in tests.
//
// It is a fake, not a model. It has no semantics beyond token overlap, which is
// exactly why the tests that care about a dense-vs-lexical disagreement use
// [StaticEmbedder] to state the disagreement outright rather than hoping a hash
// produces one.
type HashEmbedder struct {
	dims int
	name string
}

// NewHashEmbedder builds a deterministic embedder of the given width.
func NewHashEmbedder(dims int) *HashEmbedder {
	if dims <= 0 {
		dims = 768
	}
	return &HashEmbedder{dims: dims, name: "relay-hash-v1"}
}

func (e *HashEmbedder) Model() string { return e.name }
func (e *HashEmbedder) Dims() int     { return e.dims }

func (e *HashEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = e.one(t)
	}
	return out, nil
}

func (e *HashEmbedder) one(text string) []float32 {
	v := make([]float32, e.dims)
	for _, tok := range Tokenize(text) {
		h := fnv.New64a()
		_, _ = h.Write([]byte(tok))
		sum := h.Sum64()
		idx := int(sum % uint64(e.dims))
		if sum&(1<<63) != 0 {
			v[idx] -= 1
		} else {
			v[idx] += 1
		}
	}
	Normalize(v)
	return v
}

// StaticEmbedder returns vectors stated in a table, falling back to Next for
// anything it has never seen.
//
// It exists so a test can construct the case that matters — a paraphrase that
// is nearer in vector space than the document containing the literal
// identifier — instead of hoping a hash function happens to produce it.
type StaticEmbedder struct {
	Vectors map[string][]float32
	Next    Embedder
	Name    string
}

func (e *StaticEmbedder) Model() string {
	if e.Name != "" {
		return e.Name
	}
	return "relay-static"
}

func (e *StaticEmbedder) Dims() int {
	if e.Next != nil {
		return e.Next.Dims()
	}
	for _, v := range e.Vectors {
		return len(v)
	}
	return 0
}

func (e *StaticEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	var missing []int
	for i, t := range texts {
		if v, ok := e.Vectors[t]; ok {
			cp := make([]float32, len(v))
			copy(cp, v)
			Normalize(cp)
			out[i] = cp
			continue
		}
		missing = append(missing, i)
	}
	if len(missing) == 0 {
		return out, nil
	}
	if e.Next == nil {
		return nil, errors.New("search: StaticEmbedder has no vector for " + texts[missing[0]] + " and no fallback")
	}
	sub := make([]string, len(missing))
	for i, j := range missing {
		sub[i] = texts[j]
	}
	got, err := e.Next.Embed(ctx, sub)
	if err != nil {
		return nil, err
	}
	for i, j := range missing {
		out[j] = got[i]
	}
	return out, nil
}

// ------------------------------------------------------- changing the model --

// The index records which model wrote its vectors, and every search checks it,
// because vectors from two models share a space only by coincidence — the
// failure is not an error, it is plausible-looking nonsense at the top of every
// result list.
//
// That check is the easy half. The hard half is what to do about a mismatch,
// and there are exactly three options: refuse to start, mix the two anyway, or
// degrade. Refusing to start makes a changed model brick the box. Mixing is the
// one thing that must never happen, under any circumstances, because it is
// silent. So a mismatch **degrades** — the dense half stops, the lexical half
// keeps working, and Results.Degraded names both models — and the way out is a
// re-embed, which is a first-class flow rather than an error message.
//
// [InspectEmbedding] is what a CLI needs to offer that flow: what the index
// holds, what this build has, and how much work the rebuild is.
// [ResetEmbeddingIndex] is the flow itself.

// EmbeddingState is the index's embedding situation, for a caller that has to
// explain it to a person.
type EmbeddingState struct {
	// Indexed is the model that wrote the vectors, or empty when none have been
	// written yet — in which case any model may claim the index.
	Indexed string
	// Current is the configured embedder's model, or empty when there is none.
	Current string
	// Dims is the index's fixed vector width.
	Dims int
	// Summaries is how many summary rows exist; Vectors how many of them have
	// an embedding. Both are what a re-embed estimate is built from.
	Summaries int64
	Vectors   int64
}

// Mismatch reports whether the index and this build disagree about the model.
// An index with no vectors yet does not disagree with anything.
func (s EmbeddingState) Mismatch() bool {
	return s.Indexed != "" && s.Current != "" && s.Indexed != s.Current
}

// Unembedded is how many summaries have no vector — the work a backfill still
// has in front of it. Every vector shares its summary's rowid, so this is
// arithmetic rather than a join.
func (s EmbeddingState) Unembedded() int64 {
	if s.Summaries < s.Vectors {
		return 0
	}
	return s.Summaries - s.Vectors
}

// Reason is the sentence a caller shows. Empty when nothing is wrong.
func (s EmbeddingState) Reason() string {
	switch {
	case s.Current == "":
		return NoEmbedderReason
	case s.Mismatch():
		return MismatchReason(s.Indexed, s.Current)
	}
	return ""
}

// NoEmbedderReason is the Degraded string for a box with no embedder at all. It
// is a supported state: worse search, not no search.
const NoEmbedderReason = "no embedder configured: lexical only"

// MismatchReason is the Degraded string for an index written by one model and
// queried by another. It names both models, because a mismatch message without
// them tells nobody which one to keep, and it names the way out, because the
// remedy is a re-embed and not a config edit.
func MismatchReason(indexed, current string) string {
	return fmt.Sprintf(
		"the index was embedded with %q and this build has %q: lexical only, "+
			"because vectors from two models are not comparable. Re-embed to switch",
		indexed, current)
}

// InspectEmbedding reads the index's embedding state. emb may be nil, which is
// the no-embedder case rather than an error.
func InspectEmbedding(ctx context.Context, db *store.DB, emb Embedder) (EmbeddingState, error) {
	s := EmbeddingState{Dims: store.EmbeddingDims}
	if emb != nil {
		s.Current = emb.Model()
	}

	indexed, err := db.Meta(ctx, "embedding_model")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return s, fmt.Errorf("search: read embedding_model: %w", err)
	}
	s.Indexed = indexed

	if err := db.SQL().QueryRowContext(ctx, `SELECT count(*) FROM summary`).Scan(&s.Summaries); err != nil {
		return s, fmt.Errorf("search: count summaries: %w", err)
	}
	if err := db.SQL().QueryRowContext(ctx, `SELECT count(*) FROM summary_vec`).Scan(&s.Vectors); err != nil {
		return s, fmt.Errorf("search: count vectors: %w", err)
	}
	return s, nil
}

// ClearEmbeddings drops every vector and the recorded model, returning how many
// vectors went. The summaries and the lexical index are untouched, so search
// keeps working throughout — worse, and visibly so.
//
// This is the re-embed primitive. The summaries are the expensive part
// (MEMORY.md §4 budgets an hour or two for summarisation) and they do not depend
// on the embedder; only the vectors have to be recomputed, which is minutes.
// Saying "reindex" about this is misleading in a way worth correcting out loud.
func ClearEmbeddings(ctx context.Context, db *store.DB) (int64, error) {
	return clearEmbeddings(ctx, db, "")
}

// ResetEmbeddingIndex clears every vector and hands the index to a named model
// in one step, for the caller that already knows what it is switching to.
func ResetEmbeddingIndex(ctx context.Context, db *store.DB, model string) (int64, error) {
	if model == "" {
		return 0, errors.New("search: empty embedding model")
	}
	return clearEmbeddings(ctx, db, model)
}

// clearEmbeddings does both in one transaction, on purpose. If the vectors were
// cleared and the meta key were not — or the other way round — the index would
// be in the one state this design refuses to allow: some rows from one model,
// some from another, and nothing to detect it by. Either both move or neither
// does.
func clearEmbeddings(ctx context.Context, db *store.DB, model string) (int64, error) {
	var cleared int64
	err := db.Tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM summary_vec`).Scan(&cleared); err != nil {
			return err
		}
		// vec0 has no bulk delete path worth trusting, so the rowids are named.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM summary_vec WHERE rowid IN (SELECT rowid FROM summary_vec)`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO meta (key, value) VALUES ('embedding_model', ?)
			 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, model)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("search: clear the embedding index: %w", err)
	}
	return cleared, nil
}

// EmbeddingIndex is the narrow view of the index a re-embed command needs:
// three counts and one destructive operation, and nothing else.
//
// It exists so the CLI and the installer can offer the re-embed without opening
// the database themselves or taking a dependency on this package's query
// surface — and so that the destructive half is one method on a four-method
// type rather than something reachable from a general-purpose handle.
type EmbeddingIndex struct{ DB *store.DB }

// Summaries is how many summaries exist, embedded or not.
func (i EmbeddingIndex) Summaries(ctx context.Context) (int64, error) {
	return i.count(ctx, `SELECT count(*) FROM summary`)
}

// Embeddings is how many of them carry a vector.
func (i EmbeddingIndex) Embeddings(ctx context.Context) (int64, error) {
	return i.count(ctx, `SELECT count(*) FROM summary_vec`)
}

// EmbeddingModel is the model recorded on the index, or "" when nothing has
// been embedded yet.
func (i EmbeddingIndex) EmbeddingModel(ctx context.Context) (string, error) {
	v, err := i.DB.Meta(ctx, "embedding_model")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("search: read embedding_model: %w", err)
	}
	return v, nil
}

// ClearEmbeddings drops every vector and the recorded model.
func (i EmbeddingIndex) ClearEmbeddings(ctx context.Context) (int64, error) {
	return ClearEmbeddings(ctx, i.DB)
}

func (i EmbeddingIndex) count(ctx context.Context, q string) (int64, error) {
	var n int64
	if err := i.DB.SQL().QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("search: %s: %w", q, err)
	}
	return n, nil
}

// ------------------------------------------------------------ vector maths --

// Normalize scales v to unit length in place. A zero vector is left alone:
// there is no direction to preserve, and dividing by zero would poison the
// whole index with NaNs.
func Normalize(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return
	}
	inv := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
}

// Cosine is the similarity of two vectors in [-1,1]. Vectors of different
// widths, and zero vectors, give 0.
//
// MEMORY.md §9 uses this directly: topic drift is the cosine distance between
// recent turn summaries and the session summary, computed from the embeddings
// §3 already stores rather than from a new classifier.
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// ------------------------------------------------------------- tokenising --

// Tokenize lowercases and splits text into terms, keeping compound identifiers
// both whole and in pieces: STRIPE_SECRET_KEY yields "stripe_secret_key" plus
// "stripe", "secret", "key".
//
// Keeping both is the point. The whole token is what makes an exact identifier
// match exactly; the pieces are what let "the stripe key" find it at all. FTS5's
// porter unicode61 tokenizer splits on the underscore and keeps only the pieces,
// so the lexical half already has one of these two behaviours and this supplies
// the other.
func Tokenize(text string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if len(s) < 2 || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}

	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' && r != '.' && r != '/'
	})
	for _, f := range fields {
		f = strings.Trim(f, "-._/")
		if f == "" {
			continue
		}
		add(f)
		if strings.ContainsAny(f, "_-./") {
			for _, part := range strings.FieldsFunc(f, func(r rune) bool {
				return r == '_' || r == '-' || r == '.' || r == '/'
			}) {
				add(part)
			}
		}
	}
	sort.Strings(out)
	return out
}
