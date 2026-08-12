package compaction

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Lease is a compression lock. Only Hermes has one; [NoLease] is the answer for
// the other four.
type Lease interface {
	// Acquire takes or renews the lease. It returns false — not an error —
	// when someone else holds it, because that is an ordinary outcome and not a
	// fault: Hermes's compression_locks table has dedicated upstream
	// concurrency tests, which is a good sign the contention is real. The right
	// response to false is to leave the session alone until the next sweep,
	// never to compress anyway.
	Acquire(ctx context.Context, session string, ttl time.Duration) (bool, error)
	// Release drops a lease this holder owns. Releasing one it does not own is
	// a no-op rather than an error: the lease may have expired and been taken.
	Release(ctx context.Context, session string) error
}

// NoLease is the lease for a runtime that has none. It always succeeds.
type NoLease struct{}

func (NoLease) Acquire(context.Context, string, time.Duration) (bool, error) { return true, nil }
func (NoLease) Release(context.Context, string) error                        { return nil }

// MemoryLease is a process-local lease, for tests and for a deployment driving
// a runtime whose store we cannot reach. It coordinates this relayd with
// itself and with nothing else, which is exactly what it claims.
type MemoryLease struct {
	Now func() time.Time

	mu    sync.Mutex
	held  map[string]memLease
	owner string
}

type memLease struct {
	holder  string
	expires time.Time
}

// NewMemoryLease builds one for a named holder.
func NewMemoryLease(holder string) *MemoryLease {
	return &MemoryLease{owner: holder, held: map[string]memLease{}, Now: time.Now}
}

func (l *MemoryLease) now() time.Time {
	if l.Now == nil {
		return time.Now()
	}
	return l.Now()
}

func (l *MemoryLease) Acquire(_ context.Context, session string, ttl time.Duration) (bool, error) {
	if session == "" {
		return false, errors.New("compaction: a lease needs a session id")
	}
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cur, ok := l.held[session]
	if ok && cur.holder != l.owner && cur.expires.After(now) {
		return false, nil
	}
	l.held[session] = memLease{holder: l.owner, expires: now.Add(ttl)}
	return true, nil
}

func (l *MemoryLease) Release(_ context.Context, session string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur, ok := l.held[session]; ok && cur.holder == l.owner {
		delete(l.held, session)
	}
	return nil
}

// DefaultLeaseTTL bounds how long we may hold a compression lease. Compaction
// is ten to sixty seconds, so a couple of minutes covers a slow one while still
// expiring quickly enough that a relayd killed mid-compaction does not lock a
// session out of its own runtime's compression until someone reboots.
const DefaultLeaseTTL = 2 * time.Minute

// SQLLease is Hermes's compression_locks lease.
//
// # What is known, and what is not
//
// MEMORY.md §12.5 is explicit that this is unresolved: the table is
// (session_id, holder, expires_at) and it has dedicated upstream concurrency
// tests, and that is the whole of the evidence. The acquire/renew/expire
// semantics have not been read from those tests, Hermes is not installed here,
// and its 2.5 GB store is on the author's machine. So:
//
//   - The table and column names are fields, not constants, because a schema
//     read from a doc is a guess until something opens the file.
//   - Encode is a field for the same reason: expires_at's unit is unverified.
//     Comparisons happen entirely in encoded space, so a wrong unit produces
//     wrong *expiry* behaviour — never a wrong holder — which is the failure
//     mode to prefer if one must be chosen.
//   - The standing instruction until the probe runs is take the lease and back
//     off if it is held, never compress optimistically. [SQLLease.Acquire] returning
//     false is that back-off, and [Sweeper] treats it as a skip with a reason
//     rather than as an error.
type SQLLease struct {
	// DB is Hermes's own database. This package never opens it: whoever owns
	// runtime integration decides whether writing into another product's store
	// is acceptable, and hands the handle in.
	DB *sql.DB
	// Holder is the string written into the holder column. It should identify
	// this relayd, because the upstream tests exist precisely because two
	// writers turn up.
	Holder string

	// Table, SessionColumn, HolderColumn and ExpiresColumn default to the
	// documented shape.
	Table         string
	SessionColumn string
	HolderColumn  string
	ExpiresColumn string

	Now    func() time.Time
	Encode func(time.Time) int64
}

func (l *SQLLease) table() string {
	if l.Table == "" {
		return "compression_locks"
	}
	return l.Table
}

func (l *SQLLease) col(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func (l *SQLLease) now() time.Time {
	if l.Now == nil {
		return time.Now()
	}
	return l.Now()
}

func (l *SQLLease) encode(t time.Time) int64 {
	if l.Encode == nil {
		return t.UnixMilli()
	}
	return l.Encode(t)
}

func (l *SQLLease) check() error {
	if l.DB == nil {
		return errors.New("compaction: the compression lease needs Hermes's database handle")
	}
	if l.Holder == "" {
		return errors.New("compaction: the compression lease needs a holder that identifies this relayd")
	}
	return nil
}

// Acquire takes the lease, renews one this holder already has, or reports that
// someone else holds a live one.
func (l *SQLLease) Acquire(ctx context.Context, session string, ttl time.Duration) (bool, error) {
	if err := l.check(); err != nil {
		return false, err
	}
	if session == "" {
		return false, errors.New("compaction: a lease needs a session id")
	}
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}

	sess := l.col(l.SessionColumn, "session_id")
	holder := l.col(l.HolderColumn, "holder")
	expires := l.col(l.ExpiresColumn, "expires_at")
	tbl := l.table()

	now := l.now()
	nowEnc := l.encode(now)
	untilEnc := l.encode(now.Add(ttl))

	tx, err := l.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("compaction: begin lease tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var curHolder string
	var curExpires int64
	row := tx.QueryRowContext(ctx,
		fmt.Sprintf("SELECT %s, %s FROM %s WHERE %s = ?", holder, expires, tbl, sess), session)
	switch err := row.Scan(&curHolder, &curExpires); {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO %s (%s, %s, %s) VALUES (?, ?, ?)", tbl, sess, holder, expires),
			session, l.Holder, untilEnc); err != nil {
			return false, fmt.Errorf("compaction: take lease: %w", err)
		}
	case err != nil:
		return false, fmt.Errorf("compaction: read lease: %w", err)
	default:
		if curHolder != l.Holder && curExpires > nowEnc {
			// Live, and someone else's. Back off.
			return false, nil
		}
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf("UPDATE %s SET %s = ?, %s = ? WHERE %s = ?", tbl, holder, expires, sess),
			l.Holder, untilEnc, session); err != nil {
			return false, fmt.Errorf("compaction: renew lease: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("compaction: commit lease: %w", err)
	}
	return true, nil
}

// Release drops the lease if this holder still owns it.
func (l *SQLLease) Release(ctx context.Context, session string) error {
	if err := l.check(); err != nil {
		return err
	}
	sess := l.col(l.SessionColumn, "session_id")
	holder := l.col(l.HolderColumn, "holder")
	_, err := l.DB.ExecContext(ctx,
		fmt.Sprintf("DELETE FROM %s WHERE %s = ? AND %s = ?", l.table(), sess, holder),
		session, l.Holder)
	if err != nil {
		return fmt.Errorf("compaction: release lease: %w", err)
	}
	return nil
}

// LeaseFor picks the lease a runtime needs. Only Hermes requires one; the other
// four get [NoLease], which is a statement about their protocols rather than an
// optimisation.
func LeaseFor(m Mechanism, hermes Lease) Lease {
	if m.RequiresLease {
		if hermes == nil {
			return nil
		}
		return hermes
	}
	return NoLease{}
}
