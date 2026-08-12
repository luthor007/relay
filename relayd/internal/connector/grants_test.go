package connector_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/audit"
	"github.com/luthor007/relay/relayd/internal/connector"
	"github.com/luthor007/relay/relayd/internal/mcp"
	"github.com/luthor007/relay/relayd/internal/store"
)

func newGrants(t *testing.T) (*connector.Grants, *audit.Memory) {
	t.Helper()
	log := audit.NewMemory()
	return &connector.Grants{
		Store: connector.NewMemoryStore(),
		Audit: log,
		Now:   func() time.Time { return at },
		NewID: func() string { return "grant-1" },
	}, log
}

// ORCHESTRATOR.md §4b rule 1: nothing is auto-granted. Not on install, not on
// suggestion, not ever. The only door into the store refuses a request that does
// not carry a decision.
func TestAGrantWithoutADecisionIsRefused(t *testing.T) {
	g, log := newGrants(t)
	ctx := context.Background()

	_, _, err := g.Grant(ctx, connector.GrantRequest{
		Connector: "prusa", Access: mcp.AccessRead, From: "proposal-9",
	})
	if !errors.Is(err, connector.ErrNotDecided) {
		t.Fatalf("want ErrNotDecided, got %v", err)
	}
	if ok, _ := g.Allowed(ctx, "prusa", mcp.AccessRead); ok {
		t.Fatal("nothing was granted, so nothing may be allowed")
	}
	entries, err := log.List(ctx, audit.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused request is not a mutation and writes nothing: %+v", entries)
	}
}

// Rule 2: read and write are separate grants, and the signature is what enforces
// it — Grant takes one half, so two halves is two decisions.
func TestReadAndWriteAreSeparateDecisions(t *testing.T) {
	g, _ := newGrants(t)
	ctx := context.Background()

	if _, _, err := g.Grant(ctx, connector.GrantRequest{
		Connector: "prusa", Access: mcp.AccessRead, Decided: true, By: "console",
		Opens: "tell you when a print finishes",
	}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := g.Allowed(ctx, "prusa", mcp.AccessRead); !ok {
		t.Fatal("the read half should be granted")
	}
	ok, reason := g.Allowed(ctx, "prusa", mcp.AccessWrite)
	if ok {
		t.Fatal("granting read must not open write")
	}
	if reason == "" {
		t.Fatal("a refusal has to say something a user would recognise")
	}

	if _, _, err := g.Grant(ctx, connector.GrantRequest{
		Connector: "prusa", Access: mcp.AccessWrite, Decided: true, By: "glasses",
	}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := g.Allowed(ctx, "prusa", mcp.AccessWrite); !ok {
		t.Fatal("the write half should now be granted")
	}
}

func TestGrantIsAudited(t *testing.T) {
	g, log := newGrants(t)
	ctx := context.Background()

	if _, _, err := g.Grant(ctx, connector.GrantRequest{
		Connector: "prusa", Access: mcp.AccessRead, Decided: true, By: "console",
		Opens: "tell you when a print finishes", From: "proposal-9",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Revoke(ctx, "prusa"); err != nil {
		t.Fatal(err)
	}

	entries, err := log.List(ctx, audit.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	want := []audit.Action{
		audit.ActionConnectorGrant, audit.ActionConnectorGrant,
		audit.ActionConnectorRevoke, audit.ActionConnectorRevoke,
	}
	if len(entries) != len(want) {
		t.Fatalf("want %d entries, got %d: %+v", len(want), len(entries), entries)
	}
	for i, w := range want {
		if entries[i].Action != w {
			t.Fatalf("entry %d: want %s, got %s", i, w, entries[i].Action)
		}
	}
	if entries[0].Detail["scope"] != "prusa:read" || entries[0].Detail["opens"] == "" {
		t.Fatalf("the attempt must record what was agreed to: %+v", entries[0].Detail)
	}
	if err := audit.Verify(entries); err != nil {
		t.Fatalf("the chain must hold: %v", err)
	}
}

// A grant with nowhere to record it is refused, for the same reason a vault
// write with nowhere to record it is.
func TestGrantWithNoAuditLogIsRefused(t *testing.T) {
	g := &connector.Grants{Store: connector.NewMemoryStore()}
	_, _, err := g.Grant(context.Background(), connector.GrantRequest{
		Connector: "prusa", Access: mcp.AccessRead, Decided: true,
	})
	if !errors.Is(err, connector.ErrNoAudit) {
		t.Fatalf("want ErrNoAudit, got %v", err)
	}
}

func TestRevokeReachesEveryRuntime(t *testing.T) {
	g, _ := newGrants(t)
	ctx := context.Background()
	if _, _, err := g.Grant(ctx, connector.GrantRequest{
		Connector: "prusa", Access: mcp.AccessRead, Decided: true, By: "console",
	}); err != nil {
		t.Fatal(err)
	}

	res, err := g.Revoke(ctx, "prusa")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Runtimes) != 5 {
		t.Fatalf("one revoke covers all five runtimes, got %d", len(res.Runtimes))
	}
	for _, r := range res.Runtimes {
		if !r.Reached {
			t.Fatalf("%s was not reached: %s", r.Runtime, r.Reason)
		}
	}
	if ok, _ := g.Allowed(ctx, "prusa", mcp.AccessRead); ok {
		t.Fatal("a revoked connector must be refused immediately")
	}
	if res.Note == "" {
		t.Fatal("a revoke has to say what it meant for running sessions")
	}
}

// A store that cannot answer is not a store that says yes.
func TestAnUnreadableStoreRefuses(t *testing.T) {
	g := &connector.Grants{Store: brokenStore{}, Audit: audit.NewMemory()}
	ok, reason := g.Allowed(context.Background(), "prusa", mcp.AccessRead)
	if ok {
		t.Fatal("an unreadable grant list must refuse")
	}
	if reason == "" {
		t.Fatal("and say so")
	}
}

type brokenStore struct{}

func (brokenStore) List(context.Context) ([]connector.Grant, error) {
	return nil, errors.New("disk on fire")
}
func (brokenStore) Live(context.Context, string) ([]connector.Grant, error) {
	return nil, errors.New("disk on fire")
}
func (brokenStore) Put(context.Context, connector.Grant) error { return nil }
func (brokenStore) Revoke(context.Context, string, time.Time) ([]string, error) {
	return nil, errors.New("disk on fire")
}

// The grants live in the `grant` table internal/api's connectors screen already
// reads. Writing anywhere else would give the console two sources of truth.
func TestSQLStoreUsesTheTableTheConsoleReads(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	g := &connector.Grants{
		Store: connector.NewSQLStore(db),
		Audit: audit.NewMemory(),
		Now:   func() time.Time { return at },
		NewID: func() string { return "g-1" },
	}
	ctx := context.Background()
	if _, _, err := g.Grant(ctx, connector.GrantRequest{
		Connector: "Prusa", Access: mcp.AccessRead, Decided: true, By: "console",
	}); err != nil {
		t.Fatal(err)
	}

	var connectorName, scopes string
	if err := db.SQL().QueryRow(
		`SELECT connector, scopes FROM "grant" WHERE id = 'g-1'`).Scan(&connectorName, &scopes); err != nil {
		t.Fatal(err)
	}
	if connectorName != "prusa" || scopes != `["prusa:read"]` {
		t.Fatalf("row = %q %q", connectorName, scopes)
	}

	// A second half updates the same row rather than creating a second
	// connection the console would render twice.
	if _, _, err := g.Grant(ctx, connector.GrantRequest{
		Connector: "prusa", Access: mcp.AccessWrite, Decided: true, By: "console",
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.SQL().QueryRow(`SELECT count(*) FROM "grant"`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want one grant row per connector, got %d", n)
	}

	all, err := g.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || len(all[0].Scopes) != 2 {
		t.Fatalf("grants = %+v", all)
	}

	if _, err := g.Revoke(ctx, "prusa"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := g.Allowed(ctx, "prusa", mcp.AccessRead); ok {
		t.Fatal("revoked means revoked")
	}
}

// The console revokes by writing the row directly. A cache in this package would
// keep the connector alive until something invalidated it, so there is not one.
func TestARevokeWrittenBehindOurBackTakesEffect(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	g := &connector.Grants{Store: connector.NewSQLStore(db), Audit: audit.NewMemory(), NewID: func() string { return "g-1" }}
	ctx := context.Background()
	if _, _, err := g.Grant(ctx, connector.GrantRequest{
		Connector: "prusa", Access: mcp.AccessRead, Decided: true,
	}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := g.Allowed(ctx, "prusa", mcp.AccessRead); !ok {
		t.Fatal("granted")
	}

	if _, err := db.SQL().Exec(`UPDATE "grant" SET revoked_at = ? WHERE id = 'g-1'`, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if ok, _ := g.Allowed(ctx, "prusa", mcp.AccessRead); ok {
		t.Fatal("the console's revoke did not take effect immediately")
	}
}
