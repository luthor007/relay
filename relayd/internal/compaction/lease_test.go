package compaction

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	// The same two blank imports internal/store uses, and for the same reason:
	// the driver plus a SQLite wasm build that has sqlite-vec in it. Importing
	// github.com/ncruces/go-sqlite3/embed instead would silently swap in a
	// vanilla SQLite — never do that here or anywhere else.
	_ "github.com/asg017/sqlite-vec-go-bindings/ncruces"
	_ "github.com/ncruces/go-sqlite3/driver"

	"github.com/luthor007/relay/relayd/internal/adapter"
)

// hermesDB stands in for Hermes's own store: the three columns MEMORY.md §12.5
// records and nothing else, because nothing else has been read from upstream.
func hermesDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hermes.db")
	db, err := sql.Open("sqlite3", "file:"+path+"?_pragma=busy_timeout(10000)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(`CREATE TABLE compression_locks (
		session_id TEXT PRIMARY KEY,
		holder     TEXT NOT NULL,
		expires_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSQLLeaseTakesRenewsAndReleases(t *testing.T) {
	ctx := context.Background()
	db := hermesDB(t)
	now := time.Unix(1_700_000_000, 0)

	l := &SQLLease{DB: db, Holder: "relayd", Now: func() time.Time { return now }}

	got, err := l.Acquire(ctx, "s1", time.Minute)
	if err != nil || !got {
		t.Fatalf("acquire = %v, %v", got, err)
	}

	// Renewing our own lease succeeds and moves the expiry.
	var first int64
	if err := db.QueryRow(`SELECT expires_at FROM compression_locks WHERE session_id = 's1'`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Second)
	if got, err := l.Acquire(ctx, "s1", time.Minute); err != nil || !got {
		t.Fatalf("renew = %v, %v", got, err)
	}
	var second int64
	if err := db.QueryRow(`SELECT expires_at FROM compression_locks WHERE session_id = 's1'`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if second <= first {
		t.Fatalf("renew did not move the expiry: %d -> %d", first, second)
	}

	if err := l.Release(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM compression_locks`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("rows after release = %d", n)
	}
}

// The instruction until the upstream tests are read: take the lease and back
// off if it is held, never compress optimistically.
func TestSQLLeaseBacksOffRatherThanRacing(t *testing.T) {
	ctx := context.Background()
	db := hermesDB(t)
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }

	mine := &SQLLease{DB: db, Holder: "relayd", Now: clock}
	theirs := &SQLLease{DB: db, Holder: "hermes-itself", Now: clock}

	if got, err := theirs.Acquire(ctx, "s1", time.Minute); err != nil || !got {
		t.Fatalf("the other holder should get it first: %v %v", got, err)
	}
	got, err := mine.Acquire(ctx, "s1", time.Minute)
	if err != nil {
		t.Fatalf("contention is an outcome, not an error: %v", err)
	}
	if got {
		t.Fatal("we took a lease somebody else was holding")
	}

	// Releasing somebody else's lease is a no-op, not a theft.
	if err := mine.Release(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	var holder string
	if err := db.QueryRow(`SELECT holder FROM compression_locks WHERE session_id = 's1'`).Scan(&holder); err != nil {
		t.Fatal(err)
	}
	if holder != "hermes-itself" {
		t.Fatalf("holder = %q", holder)
	}

	// Once it expires, we may take it — a relayd killed mid-compaction must not
	// lock a session out of its own runtime's compression forever.
	now = now.Add(2 * time.Minute)
	if got, err := mine.Acquire(ctx, "s1", time.Minute); err != nil || !got {
		t.Fatalf("expired lease not reclaimed: %v %v", got, err)
	}
}

func TestSQLLeaseNeedsItsInputs(t *testing.T) {
	ctx := context.Background()
	if _, err := (&SQLLease{Holder: "relayd"}).Acquire(ctx, "s1", time.Minute); err == nil {
		t.Fatal("no database handle must be an error")
	}
	if _, err := (&SQLLease{DB: hermesDB(t)}).Acquire(ctx, "s1", time.Minute); err == nil {
		t.Fatal("an anonymous holder defeats the point of a lease")
	}
	if _, err := (&SQLLease{DB: hermesDB(t), Holder: "relayd"}).Acquire(ctx, "", time.Minute); err == nil {
		t.Fatal("a lease needs a session id")
	}
}

func TestSQLLeaseHonoursColumnOverrides(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "other.db")
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE locks (sid TEXT PRIMARY KEY, who TEXT NOT NULL, until INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}

	l := &SQLLease{
		DB: db, Holder: "relayd",
		Table: "locks", SessionColumn: "sid", HolderColumn: "who", ExpiresColumn: "until",
		Encode: func(t time.Time) int64 { return t.Unix() },
	}
	if got, err := l.Acquire(ctx, "s1", time.Minute); err != nil || !got {
		t.Fatalf("acquire = %v, %v", got, err)
	}
	var until int64
	if err := db.QueryRow(`SELECT until FROM locks WHERE sid = 's1'`).Scan(&until); err != nil {
		t.Fatal(err)
	}
	if until > 1e12 {
		t.Fatalf("until = %d: the encoder was ignored", until)
	}
}

func TestMemoryLease(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	a := NewMemoryLease("a")
	a.Now = func() time.Time { return now }
	b := NewMemoryLease("b")
	b.held = a.held // share the map, as two holders share a table
	b.Now = func() time.Time { return now }

	if got, _ := a.Acquire(ctx, "s1", time.Minute); !got {
		t.Fatal("first acquire should win")
	}
	if got, _ := b.Acquire(ctx, "s1", time.Minute); got {
		t.Fatal("second holder must back off")
	}
	if got, _ := a.Acquire(ctx, "s1", time.Minute); !got {
		t.Fatal("renewing our own lease should succeed")
	}
	now = now.Add(2 * time.Minute)
	if got, _ := b.Acquire(ctx, "s1", time.Minute); !got {
		t.Fatal("an expired lease should be takeable")
	}
	if _, err := a.Acquire(ctx, "", time.Minute); err == nil {
		t.Fatal("a lease needs a session id")
	}
}

// Only Hermes has a lease. The other four are not "unlocked", they have no such
// table.
func TestLeaseForOnlyGatesHermes(t *testing.T) {
	for _, rt := range adapter.Runtimes() {
		m, _ := MechanismFor(rt)
		l := LeaseFor(m, nil)
		if rt == adapter.Hermes {
			if l != nil {
				t.Fatal("Hermes with no lease available must not fall through to NoLease")
			}
			continue
		}
		if _, ok := l.(NoLease); !ok {
			t.Fatalf("%s: lease = %T, want NoLease", rt, l)
		}
	}

	m, _ := MechanismFor(adapter.Hermes)
	if l := LeaseFor(m, NewMemoryLease("relayd")); l == nil {
		t.Fatal("Hermes with a lease available must use it")
	}
}
