// Package store is the SQLite layer: one file, no server, vector search
// without Postgres, and backup is a file copy.
//
// Two databases, deliberately. [Open] gives the main one — MEMORY.md §2's
// registry and index tiers, SYSTEM.md §5's seven entities, and the facts tier.
// [OpenVault] gives a second file holding credentials, which is never indexed
// and shares no tables with the first. That separation is the whole reason a
// credential cannot turn up in a search result.
//
// # The stack, and the trap
//
// Pure Go via wazero, no cgo, so one machine cross-compiles darwin/linux ×
// arm64/amd64 — which is what SYSTEM.md §8 requires and what the $0 self-host
// tier depends on. Three pins are load-bearing and are documented in
// MEMORY.md §3 with their failure modes:
//
//   - github.com/asg017/sqlite-vec-go-bindings/ncruces v0.1.6 embeds a SQLite
//     wasm with sqlite-vec compiled in.
//   - github.com/ncruces/go-sqlite3 v0.21.0. v0.35 removed sqlite3.Binary;
//     v0.22–v0.30 fail at module instantiation with a wasm-threads error.
//   - github.com/tetratelabs/wazero ≥ v1.9.0, which nothing imports directly
//     and which go get will not choose for you. On v1.8.2 every vec0 table
//     panics with an out-of-bounds memory access — on ten 4-dimensional
//     vectors, so it is not a scale problem. select vec_version() succeeds on
//     the broken versions and proves nothing, which is why the test in this
//     package inserts into vec0 and reads a row back.
//
// Never import github.com/ncruces/go-sqlite3/embed: its init() overwrites
// sqlite3.Binary with a vanilla SQLite and sqlite-vec silently disappears.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	// Registers the "sqlite3" driver.
	_ "github.com/ncruces/go-sqlite3/driver"
	// Sets sqlite3.Binary to a SQLite wasm build that includes sqlite-vec.
	// Blank-importing this and NOT importing go-sqlite3/embed is the whole
	// difference between having vec0 and not.
	_ "github.com/asg017/sqlite-vec-go-bindings/ncruces"
)

//go:embed migrations
var migrationFS embed.FS

// Kind is which of the two databases this handle is.
type Kind string

const (
	// KindMain holds the registry, index and facts tiers.
	KindMain Kind = "main"
	// KindVault holds credentials and is never indexed.
	KindVault Kind = "vault"
)

// EmbeddingDims is the width of summary_vec. Changing it is a migration,
// because a vec0 column's width is fixed at create time.
const EmbeddingDims = 768

// DB is an open database with its migrations applied.
type DB struct {
	sql  *sql.DB
	path string
	kind Kind
}

// Open opens (creating if needed) the main database and applies every
// outstanding migration.
func Open(path string) (*DB, error) { return open(path, KindMain) }

// OpenVault opens the credential vault. Separate file, separate migration set,
// no FTS5 and no vec0.
func OpenVault(path string) (*DB, error) { return open(path, KindVault) }

func open(path string, kind Kind) (*DB, error) {
	if path == "" {
		return nil, errors.New("store: empty database path")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("store: create data dir: %w", err)
		}
	}

	sdb, err := sql.Open("sqlite3", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// The wasm build is single-threaded per connection and WAL gives us one
	// writer anyway; a small pool keeps reads concurrent without the lock
	// contention a large one buys.
	sdb.SetMaxOpenConns(4)
	sdb.SetMaxIdleConns(4)
	sdb.SetConnMaxIdleTime(5 * time.Minute)

	db := &DB{sql: sdb, path: path, kind: kind}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := sdb.PingContext(ctx); err != nil {
		_ = sdb.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}
	if err := db.migrate(ctx); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	return db, nil
}

func dsn(path string) string {
	if path == ":memory:" {
		path = ":memory:"
	}
	q := url.Values{}
	// Order matters for the ncruces driver: busy timeout first.
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "journal_mode(wal)")
	q.Add("_pragma", "synchronous(normal)")
	q.Add("_pragma", "foreign_keys(on)")
	q.Set("_txlock", "immediate")
	return "file:" + path + "?" + q.Encode()
}

// SQL is the underlying handle. Downstream packages own their own queries;
// this package owns the schema and the invariants that cross tables.
func (d *DB) SQL() *sql.DB { return d.sql }

// Path is the file this database lives in.
func (d *DB) Path() string { return d.path }

// Kind is which of the two databases this is.
func (d *DB) Kind() Kind { return d.kind }

// Close closes the database.
func (d *DB) Close() error { return d.sql.Close() }

// Tx runs fn in a transaction, committing on nil and rolling back otherwise.
func (d *DB) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Meta reads a key from the meta table.
func (d *DB) Meta(ctx context.Context, key string) (string, error) {
	var v string
	err := d.sql.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetMeta writes a key to the meta table.
func (d *DB) SetMeta(ctx context.Context, key, value string) error {
	_, err := d.sql.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// ms converts a time to unix milliseconds; the zero time becomes 0.
func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// at converts unix milliseconds back to a time; 0 becomes the zero time.
func at(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v).UTC()
}

// nullMS is ms for a column that is NULL rather than 0 when unset.
func nullMS(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

func atPtr(v *int64) time.Time {
	if v == nil {
		return time.Time{}
	}
	return at(*v)
}
