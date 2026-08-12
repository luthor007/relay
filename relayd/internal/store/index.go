package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/ncruces"
)

// SessionIndex is one row per session ever seen, across all five runtimes.
//
// It holds a POINTER into the original transcript — runtime, session id, path,
// byte offset — and never a copy. MEMORY.md §3: keep the raw transcripts on
// disk, in place, unmoved; we are building an index, not a copy.
type SessionIndex struct {
	ID         string
	Runtime    string
	SessionID  string
	Path       string
	ByteOffset int64

	Title       string
	Workspace   string
	GitBranch   string
	Model       string
	StartedAt   time.Time
	EndedAt     time.Time
	Messages    int64
	ToolCalls   int64
	CostUSD     *float64
	TokensTotal *int64

	// SourceMTime and SourceSize are the backfill resume key. Backfill is
	// incremental and keyed on (runtime, session_id, mtime) because 3.6 GB
	// through a small model is an hour or two and must survive interruption.
	SourceMTime time.Time
	SourceSize  int64
	IndexedAt   time.Time
}

// SummaryKind distinguishes a whole-session summary from a turn-cluster one.
type SummaryKind string

const (
	SummarySession SummaryKind = "session"
	SummaryCluster SummaryKind = "cluster"
)

// Summary is what gets embedded. Raw transcripts are not: ~875k chunks of
// diffs, stack traces and command output buries the two sentences that said
// what the session was for.
type Summary struct {
	ID         int64
	Kind       SummaryKind
	Runtime    string
	SessionID  string
	Path       string
	ByteOffset int64
	ByteLength int64
	Text       string
	Model      string
	CreatedAt  time.Time
}

// SecretMarker is what replaces a credential in the index. Detection happens
// before indexing, never after — an embedded key cannot be unembedded — and the
// searchable artefact is this marker: "a Stripe secret key appeared in this
// session".
type SecretMarker struct {
	ID         string
	Runtime    string
	SessionID  string
	Path       string
	ByteOffset int64
	Detector   string
	Service    string
	VaultID    string
	At         time.Time
}

// ------------------------------------------------------------ session index --

func (d *DB) PutSessionIndex(ctx context.Context, v SessionIndex) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO session_index (id, runtime, session_id, path, byte_offset,
			title, workspace, git_branch, model, started_at, ended_at,
			message_count, tool_call_count, cost_usd, tokens_total,
			source_mtime, source_size, indexed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (runtime, session_id) DO UPDATE SET
			path = excluded.path, byte_offset = excluded.byte_offset,
			title = excluded.title, workspace = excluded.workspace,
			git_branch = excluded.git_branch, model = excluded.model,
			started_at = excluded.started_at, ended_at = excluded.ended_at,
			message_count = excluded.message_count,
			tool_call_count = excluded.tool_call_count,
			cost_usd = excluded.cost_usd, tokens_total = excluded.tokens_total,
			source_mtime = excluded.source_mtime, source_size = excluded.source_size,
			indexed_at = excluded.indexed_at`,
		v.ID, v.Runtime, v.SessionID, v.Path, v.ByteOffset,
		v.Title, v.Workspace, v.GitBranch, v.Model, nullMS(v.StartedAt), nullMS(v.EndedAt),
		v.Messages, v.ToolCalls, v.CostUSD, v.TokensTotal,
		ms(v.SourceMTime), v.SourceSize, nullMS(v.IndexedAt))
	return err
}

func (d *DB) GetSessionIndex(ctx context.Context, runtime, sessionID string) (SessionIndex, error) {
	row := d.sql.QueryRowContext(ctx, `
		SELECT id, runtime, session_id, path, byte_offset, title, workspace, git_branch,
		       model, started_at, ended_at, message_count, tool_call_count,
		       cost_usd, tokens_total, source_mtime, source_size, indexed_at
		FROM session_index WHERE runtime = ? AND session_id = ?`, runtime, sessionID)

	var v SessionIndex
	var started, ended, indexed *int64
	var mtime int64
	err := row.Scan(&v.ID, &v.Runtime, &v.SessionID, &v.Path, &v.ByteOffset,
		&v.Title, &v.Workspace, &v.GitBranch, &v.Model, &started, &ended,
		&v.Messages, &v.ToolCalls, &v.CostUSD, &v.TokensTotal, &mtime, &v.SourceSize, &indexed)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionIndex{}, fmt.Errorf("%w: %s/%s", ErrNotFound, runtime, sessionID)
	}
	if err != nil {
		return SessionIndex{}, err
	}
	v.StartedAt, v.EndedAt, v.IndexedAt = atPtr(started), atPtr(ended), atPtr(indexed)
	v.SourceMTime = at(mtime)
	return v, nil
}

// NeedsIndexing reports whether a transcript file has to be read again. It is
// the resume check for MEMORY.md §4's incremental backfill: unseen files and
// files whose mtime or size moved come back true, everything else false.
func (d *DB) NeedsIndexing(ctx context.Context, runtime, sessionID string, mtime time.Time, size int64) (bool, error) {
	var haveMTime, haveSize int64
	var indexed *int64
	err := d.sql.QueryRowContext(ctx,
		`SELECT source_mtime, source_size, indexed_at FROM session_index
		 WHERE runtime = ? AND session_id = ?`, runtime, sessionID).
		Scan(&haveMTime, &haveSize, &indexed)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if indexed == nil {
		return true, nil
	}
	return haveMTime != ms(mtime) || haveSize != size, nil
}

// ------------------------------------------------------------- summaries --

// ErrEmbeddingDims is returned when a vector is not exactly EmbeddingDims wide.
// vec0 fixes the column width at create time, so this is a schema error rather
// than a data error.
var ErrEmbeddingDims = errors.New("store: wrong embedding width")

// PutSummary writes a summary, its FTS5 row (via trigger) and its vector, in
// one transaction. Passing a nil embedding writes the lexical half only, which
// is what MEMORY.md §11's build order wants at the "no embeddings yet" stage.
//
// This is the one function that has to hold the two index invariants together:
// the row carries a pointer into the original transcript, and secrets were
// already replaced with markers before this was called. Detect first, index
// second — an embedded key cannot be unembedded.
func (d *DB) PutSummary(ctx context.Context, s Summary, embedding []float32) (int64, error) {
	if embedding != nil && len(embedding) != EmbeddingDims {
		return 0, fmt.Errorf("%w: got %d, summary_vec is %d", ErrEmbeddingDims, len(embedding), EmbeddingDims)
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now()
	}
	if s.Kind == "" {
		s.Kind = SummarySession
	}

	var id int64
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO summary (kind, runtime, session_id, path, byte_offset, byte_length,
			                     text, model, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			string(s.Kind), s.Runtime, s.SessionID, s.Path, s.ByteOffset, s.ByteLength,
			s.Text, s.Model, ms(s.CreatedAt))
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		if err != nil {
			return err
		}
		if embedding == nil {
			return nil
		}
		blob, err := sqlite_vec.SerializeFloat32(embedding)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO summary_vec (rowid, embedding) VALUES (?, ?)`, id, blob)
		return err
	})
	return id, err
}

// GetSummary reads a summary row back by id.
func (d *DB) GetSummary(ctx context.Context, id int64) (Summary, error) {
	row := d.sql.QueryRowContext(ctx, `
		SELECT id, kind, runtime, session_id, path, byte_offset, byte_length, text, model, created_at
		FROM summary WHERE id = ?`, id)
	var v Summary
	var kind string
	var created int64
	err := row.Scan(&v.ID, &kind, &v.Runtime, &v.SessionID, &v.Path, &v.ByteOffset,
		&v.ByteLength, &v.Text, &v.Model, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Summary{}, fmt.Errorf("%w: summary %d", ErrNotFound, id)
	}
	if err != nil {
		return Summary{}, err
	}
	v.Kind, v.CreatedAt = SummaryKind(kind), at(created)
	return v, nil
}

// Hit is one result from either half of MEMORY.md §3's hybrid retrieval. The
// two halves are kept separate on purpose: reciprocal-rank fusion over BM25 and
// cosine is the search package's job, not the store's.
type Hit struct {
	SummaryID int64
	// Score is BM25 for lexical (lower is better, as SQLite reports it) and L2
	// distance for vector (lower is better). Do not compare across the two.
	Score float64
	// Rank is 1-based position within its own result list, which is all
	// reciprocal-rank fusion needs.
	Rank int
}

// SearchLexical is the BM25 half. Exact identifiers — a repo name, an error
// string, STRIPE_SECRET_KEY — are where vector search is weakest and this is
// strongest, and those are most of what routing actually looks up.
//
// query is FTS5 MATCH syntax; callers that accept user text should quote it.
func (d *DB) SearchLexical(ctx context.Context, query string, limit int) ([]Hit, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := d.sql.QueryContext(ctx, `
		SELECT rowid, bm25(summary_fts) FROM summary_fts
		WHERE summary_fts MATCH ? ORDER BY bm25(summary_fts) LIMIT ?`, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectHits(rows)
}

// SearchVector is the dense half: brute force over vec0, no ANN index.
// Measured at 44 ms for 22k vectors on the pure-Go wasm build we ship, which is
// inside SYSTEM.md §7b's voice budget. Revisit at ~50k; 100k costs 187 ms.
func (d *DB) SearchVector(ctx context.Context, embedding []float32, k int) ([]Hit, error) {
	if len(embedding) != EmbeddingDims {
		return nil, fmt.Errorf("%w: got %d, summary_vec is %d", ErrEmbeddingDims, len(embedding), EmbeddingDims)
	}
	if k <= 0 {
		k = 10
	}
	blob, err := sqlite_vec.SerializeFloat32(embedding)
	if err != nil {
		return nil, err
	}
	rows, err := d.sql.QueryContext(ctx, `
		SELECT rowid, distance FROM summary_vec
		WHERE embedding MATCH ? AND k = ? ORDER BY distance`, blob, k)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectHits(rows)
}

func collectHits(rows *sql.Rows) ([]Hit, error) {
	var out []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.SummaryID, &h.Score); err != nil {
			return nil, err
		}
		h.Rank = len(out) + 1
		out = append(out, h)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------- secret markers --

func (d *DB) PutSecretMarker(ctx context.Context, v SecretMarker) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO secret_marker (id, runtime, session_id, path, byte_offset,
		                           detector, service, vault_id, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET vault_id = excluded.vault_id`,
		v.ID, v.Runtime, v.SessionID, v.Path, v.ByteOffset,
		v.Detector, v.Service, v.VaultID, ms(v.At))
	return err
}

func (d *DB) ListSecretMarkers(ctx context.Context, runtime, sessionID string) ([]SecretMarker, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT id, runtime, session_id, path, byte_offset, detector, service, vault_id, at
		FROM secret_marker WHERE runtime = ? AND session_id = ? ORDER BY byte_offset`,
		runtime, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SecretMarker
	for rows.Next() {
		var v SecretMarker
		var atMS int64
		if err := rows.Scan(&v.ID, &v.Runtime, &v.SessionID, &v.Path, &v.ByteOffset,
			&v.Detector, &v.Service, &v.VaultID, &atMS); err != nil {
			return nil, err
		}
		v.At = at(atMS)
		out = append(out, v)
	}
	return out, rows.Err()
}
