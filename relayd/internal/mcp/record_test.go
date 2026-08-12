package mcp_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/mcp"
	"github.com/luthor007/relay/relayd/internal/store"
)

func openStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedSession(t *testing.T, db *store.DB, id string) {
	t.Helper()
	now := time.Now().UnixMilli()
	_, err := db.SQL().Exec(
		`INSERT INTO session (id, runtime, created_at, last_active, state)
		 VALUES (?, 'claude-code', ?, ?, 'running')`, id, now, now)
	if err != nil {
		t.Fatal(err)
	}
}

func seedGrant(t *testing.T, db *store.DB, id, connector string) {
	t.Helper()
	_, err := db.SQL().Exec(
		`INSERT INTO "grant" (id, connector, scopes, granted_at) VALUES (?, ?, '["prusa:read"]', ?)`,
		id, connector, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
}

// The trail is what makes DASHBOARD.md §3.4's "last used, and for what" column
// answerable at all, so a call has to land in tool_call and touch the grant.
func TestSQLRecorderFillsTheLastUsedColumn(t *testing.T) {
	db := openStore(t)
	seedSession(t, db, "s1")
	seedGrant(t, db, "g1", "prusa")

	rec, err := mcp.NewSQLRecorder(db)
	if err != nil {
		t.Fatal(err)
	}
	at := time.UnixMilli(1_700_000_000_000).UTC()
	err = rec.Record(context.Background(), mcp.Recorded{
		ID: "c1", Session: "s1", Connector: "prusa", Tool: "prusa_status",
		Target: "benchy.gcode", ArgsDigest: "abc", At: at, Status: "completed",
	})
	if err != nil {
		t.Fatal(err)
	}

	var tool, target string
	if err := db.SQL().QueryRow(`SELECT tool, target FROM tool_call WHERE id = 'c1'`).
		Scan(&tool, &target); err != nil {
		t.Fatal(err)
	}
	if tool != "prusa_status" || target != "benchy.gcode" {
		t.Fatalf("row is wrong: %q %q", tool, target)
	}

	var used int64
	if err := db.SQL().QueryRow(`SELECT last_used_at FROM "grant" WHERE id = 'g1'`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used != at.UnixMilli() {
		t.Fatalf("the grant's last-used time was not touched: %d", used)
	}
}

// MEMORY.md §12.2's ordering: detect before writing, never after. A target that
// carries a key must be redacted before it becomes a permanent row.
func TestSQLRecorderRedactsBeforeWriting(t *testing.T) {
	db := openStore(t)
	seedSession(t, db, "s1")

	rec, err := mcp.NewSQLRecorder(db)
	if err != nil {
		t.Fatal(err)
	}
	secret := "AKIAIOSFODNN7EXAMPLE"
	err = rec.Record(context.Background(), mcp.Recorded{
		ID: "c1", Session: "s1", Tool: "t", Target: "upload " + secret,
		At: time.Now(), Status: "completed",
	})
	if err != nil {
		t.Fatal(err)
	}
	var target string
	if err := db.SQL().QueryRow(`SELECT target FROM tool_call WHERE id = 'c1'`).Scan(&target); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(target, secret) {
		t.Fatalf("a secret reached the trail: %q", target)
	}
	if !strings.Contains(target, "relay:redacted") {
		t.Fatalf("the redaction marker should say what was removed: %q", target)
	}
}

// tool_call.session_id is a foreign key, so a call from a runtime Relay is not
// driving has nowhere to land. That is reported rather than papered over.
func TestUnattributedCallIsReportedNotSwallowed(t *testing.T) {
	db := openStore(t)
	rec, err := mcp.NewSQLRecorder(db)
	if err != nil {
		t.Fatal(err)
	}
	err = rec.Record(context.Background(), mcp.Recorded{
		ID: "c1", Session: "ghost", Tool: "t", At: time.Now(), Status: "completed",
	})
	if !errors.Is(err, mcp.ErrUnattributed) {
		t.Fatalf("want ErrUnattributed, got %v", err)
	}
	var n int
	if err := db.SQL().QueryRow(`SELECT count(*) FROM tool_call`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("nothing should have been written, got %d rows", n)
	}
}

func TestDigestIsStableAndNotTheArguments(t *testing.T) {
	a := mcp.Digest(map[string]any{"path": "x.gcode", "storage": "usb"})
	b := mcp.Digest(map[string]any{"storage": "usb", "path": "x.gcode"})
	if a != b {
		t.Fatal("the digest must not depend on map iteration order")
	}
	if strings.Contains(a, "x.gcode") {
		t.Fatalf("the digest is not a digest: %q", a)
	}
	if mcp.Digest(nil) != "" {
		t.Fatal("no arguments, no digest")
	}
}

func TestMemoryRecorderIsBounded(t *testing.T) {
	rec := &mcp.MemoryRecorder{Limit: 3}
	for i := range 10 {
		if err := rec.Record(context.Background(), mcp.Recorded{ID: string(rune('a' + i))}); err != nil {
			t.Fatal(err)
		}
	}
	got := rec.Calls()
	if len(got) != 3 || got[0].ID != "h" || got[2].ID != "j" {
		t.Fatalf("want the last three, got %+v", got)
	}
}
