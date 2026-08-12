package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Migration is one numbered, forward-only schema change.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// ErrSchemaAhead means the file was written by a newer relayd. There is no
// down migration and there never will be: rolling a schema backwards on a
// user's only copy of their memory is not a recoverable operation.
var ErrSchemaAhead = errors.New("store: database schema is newer than this build")

// Migrations returns the embedded migrations for a database kind, in order.
func Migrations(kind Kind) ([]Migration, error) {
	dir := path.Join("migrations", string(kind))
	entries, err := fs.ReadDir(migrationFS, dir)
	if err != nil {
		return nil, fmt.Errorf("store: read migrations for %s: %w", kind, err)
	}

	var out []Migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		num, name, ok := strings.Cut(strings.TrimSuffix(e.Name(), ".sql"), "_")
		if !ok {
			return nil, fmt.Errorf("store: migration %q is not NNNN_name.sql", e.Name())
		}
		v, err := strconv.Atoi(num)
		if err != nil {
			return nil, fmt.Errorf("store: migration %q has a non-numeric version: %w", e.Name(), err)
		}
		body, err := fs.ReadFile(migrationFS, path.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, Migration{Version: v, Name: name, SQL: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })

	for i, m := range out {
		if m.Version != i+1 {
			return nil, fmt.Errorf("store: migration versions must be contiguous from 1; found %d at position %d", m.Version, i+1)
		}
	}
	return out, nil
}

// Version reports the highest migration applied to this database.
func (d *DB) Version(ctx context.Context) (int, error) {
	var v sql.NullInt64
	err := d.sql.QueryRowContext(ctx, `SELECT max(version) FROM schema_migration`).Scan(&v)
	if err != nil {
		return 0, err
	}
	return int(v.Int64), nil
}

func (d *DB) migrate(ctx context.Context) error {
	if _, err := d.sql.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migration (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at INTEGER NOT NULL
		) STRICT`); err != nil {
		return fmt.Errorf("store: create schema_migration: %w", err)
	}

	migrations, err := Migrations(d.kind)
	if err != nil {
		return err
	}

	current, err := d.Version(ctx)
	if err != nil {
		return err
	}
	if current > len(migrations) {
		return fmt.Errorf("%w: file is at %d, this build knows %d", ErrSchemaAhead, current, len(migrations))
	}

	for _, m := range migrations {
		if m.Version <= current {
			continue
		}
		if err := d.apply(ctx, m); err != nil {
			return fmt.Errorf("store: migration %04d_%s: %w", m.Version, m.Name, err)
		}
	}
	return nil
}

func (d *DB) apply(ctx context.Context, m Migration) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migration (version, name, applied_at) VALUES (?, ?, ?)`,
		m.Version, m.Name, time.Now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}
