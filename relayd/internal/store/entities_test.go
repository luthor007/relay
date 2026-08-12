package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/store"
)

func TestSevenEntitiesRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openMain(t)
	now := time.Unix(1770000000, 0).UTC()

	if err := db.PutDevice(ctx, store.Device{
		ID: "dev-1", Kind: "glasses", Name: "Relay", PairedAt: now, LastSeen: now,
	}); err != nil {
		t.Fatalf("PutDevice: %v", err)
	}

	sess := store.Session{
		ID: "s-1", Runtime: "claude-code", NativeID: "uuid-1", Agent: "opus",
		Subject: "payments refactor", Workspace: "/repo/api", GitBranch: "payments",
		Entities: []string{"stripe", "payments"}, CreatedAt: now, LastActive: now,
		State: store.SessionRunning, CostUSD: f64(0.42),
	}
	if err := db.PutSession(ctx, sess); err != nil {
		t.Fatalf("PutSession: %v", err)
	}

	got, err := db.GetSession(ctx, "s-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Subject != sess.Subject || len(got.Entities) != 2 || got.Entities[1] != "payments" {
		t.Fatalf("session did not round trip: %+v", got)
	}
	if got.CostUSD == nil || *got.CostUSD != 0.42 {
		t.Fatalf("cost did not round trip: %v", got.CostUSD)
	}
	// ADAPTERS.md §5: what a runtime cannot report stays nil, never zero.
	if got.TokensTotal != nil || got.ContextWindow != nil {
		t.Fatalf("unreported usage must stay nil, got tokens=%v window=%v", got.TokensTotal, got.ContextWindow)
	}

	if err := db.PutTurn(ctx, store.Turn{
		ID: "t-1", SessionID: "s-1", Role: "agent", Text: "tests pass",
		At: now, OK: true, StopReason: "end_turn", Duration: 1500 * time.Millisecond,
	}); err != nil {
		t.Fatalf("PutTurn: %v", err)
	}
	turns, err := db.ListTurns(ctx, "s-1", 0)
	if err != nil || len(turns) != 1 || turns[0].Duration != 1500*time.Millisecond {
		t.Fatalf("ListTurns: %v %+v", err, turns)
	}

	if err := db.PutToolCall(ctx, store.ToolCall{
		ID: "tc-1", SessionID: "s-1", TurnID: "t-1", Tool: "Bash",
		Target: "go test ./...", ArgsDigest: "sha256:abc", At: now, ResultStatus: "completed",
	}); err != nil {
		t.Fatalf("PutToolCall: %v", err)
	}
	calls, err := db.ListToolCalls(ctx, "s-1")
	if err != nil || len(calls) != 1 || calls[0].ArgsDigest != "sha256:abc" {
		t.Fatalf("ListToolCalls: %v %+v", err, calls)
	}

	if err := db.PutEpisode(ctx, store.Episode{
		ID: "ep-1", StartedAt: now, Kind: "meeting", Transcript: "…",
		Participants: []string{"alex", "sam"},
	}); err != nil {
		t.Fatalf("PutEpisode: %v", err)
	}
	if err := db.PutCommitment(ctx, store.Commitment{
		ID: "c-1", EpisodeID: "ep-1", Text: "send the BOM", CreatedAt: now,
	}); err != nil {
		t.Fatalf("PutCommitment: %v", err)
	}
	open, err := db.ListCommitments(ctx, true)
	if err != nil || len(open) != 1 || open[0].Text != "send the BOM" {
		t.Fatalf("ListCommitments: %v %+v", err, open)
	}
	if !open[0].DoneAt.IsZero() {
		t.Fatalf("an undone commitment must have a zero DoneAt, got %v", open[0].DoneAt)
	}

	if err := db.PutGrant(ctx, store.Grant{
		ID: "g-1", Connector: "gmail", Scopes: []string{"read"}, GrantedAt: now,
	}); err != nil {
		t.Fatalf("PutGrant: %v", err)
	}
	grants, err := db.ListGrants(ctx)
	if err != nil || len(grants) != 1 || grants[0].Scopes[0] != "read" {
		t.Fatalf("ListGrants: %v %+v", err, grants)
	}

	devices, err := db.ListDevices(ctx)
	if err != nil || len(devices) != 1 {
		t.Fatalf("ListDevices: %v %+v", err, devices)
	}
	eps, err := db.ListEpisodes(ctx, 0)
	if err != nil || len(eps) != 1 || len(eps[0].Participants) != 2 {
		t.Fatalf("ListEpisodes: %v %+v", err, eps)
	}
}

func TestListSessionsFilters(t *testing.T) {
	ctx := context.Background()
	db := openMain(t)
	base := time.Unix(1770000000, 0)

	for i, s := range []store.Session{
		{ID: "a", Runtime: "codex", Workspace: "/repo/api", State: store.SessionRunning},
		{ID: "b", Runtime: "claude-code", Workspace: "/repo/api", State: store.SessionAwaiting},
		{ID: "c", Runtime: "claude-code", Workspace: "/repo/site", State: store.SessionIdle},
	} {
		s.CreatedAt = base
		s.LastActive = base.Add(time.Duration(i) * time.Minute)
		if err := db.PutSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	all, err := db.ListSessions(ctx, store.SessionFilter{})
	if err != nil || len(all) != 3 {
		t.Fatalf("all: %v %+v", err, all)
	}
	if all[0].ID != "c" {
		t.Fatalf("sessions must come back most-recently-active first, got %s", all[0].ID)
	}

	// DASHBOARD.md §3.1 puts blocked sessions at the top of the list, so
	// filtering for them has to be one query.
	blocked, err := db.ListSessions(ctx, store.SessionFilter{State: store.SessionAwaiting})
	if err != nil || len(blocked) != 1 || blocked[0].ID != "b" {
		t.Fatalf("awaiting: %v %+v", err, blocked)
	}

	byRuntime, err := db.ListSessions(ctx, store.SessionFilter{Runtime: "claude-code"})
	if err != nil || len(byRuntime) != 2 {
		t.Fatalf("by runtime: %v %+v", err, byRuntime)
	}
	byWorkspace, err := db.ListSessions(ctx, store.SessionFilter{Workspace: "/repo/api", Limit: 1})
	if err != nil || len(byWorkspace) != 1 {
		t.Fatalf("by workspace: %v %+v", err, byWorkspace)
	}
}

func TestGetSessionNotFound(t *testing.T) {
	_, err := openMain(t).GetSession(context.Background(), "nope")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestBackfillResumeKey(t *testing.T) {
	ctx := context.Background()
	db := openMain(t)
	mtime := time.Unix(1770000000, 0)

	need, err := db.NeedsIndexing(ctx, "codex", "abc", mtime, 4096)
	if err != nil || !need {
		t.Fatalf("an unseen session must need indexing: %v %v", err, need)
	}

	if err := db.PutSessionIndex(ctx, store.SessionIndex{
		ID: "si-1", Runtime: "codex", SessionID: "abc",
		Path:  "/home/u/.codex/sessions/2026/08/10/rollout-abc.jsonl",
		Title: "fix the webhook", SourceMTime: mtime, SourceSize: 4096,
		IndexedAt: mtime,
	}); err != nil {
		t.Fatalf("PutSessionIndex: %v", err)
	}

	need, err = db.NeedsIndexing(ctx, "codex", "abc", mtime, 4096)
	if err != nil || need {
		t.Fatalf("an unchanged session must not need re-indexing: %v %v", err, need)
	}
	need, err = db.NeedsIndexing(ctx, "codex", "abc", mtime.Add(time.Second), 4096)
	if err != nil || !need {
		t.Fatalf("a newer mtime must need re-indexing: %v %v", err, need)
	}
	need, err = db.NeedsIndexing(ctx, "codex", "abc", mtime, 8192)
	if err != nil || !need {
		t.Fatalf("a changed size must need re-indexing: %v %v", err, need)
	}

	// An index row written but not yet summarised is unfinished work, and
	// backfill has to pick it up again after an interruption.
	if err := db.PutSessionIndex(ctx, store.SessionIndex{
		ID: "si-2", Runtime: "hermes", SessionID: "def", Path: "/x",
		SourceMTime: mtime, SourceSize: 10,
	}); err != nil {
		t.Fatal(err)
	}
	need, err = db.NeedsIndexing(ctx, "hermes", "def", mtime, 10)
	if err != nil || !need {
		t.Fatalf("a row with no indexed_at must need indexing: %v %v", err, need)
	}
}

func TestSecretMarkerReplacesTheSecret(t *testing.T) {
	ctx := context.Background()
	db := openMain(t)

	if err := db.PutSecretMarker(ctx, store.SecretMarker{
		ID: "sm-1", Runtime: "claude-code", SessionID: "s1",
		Path: "/t.jsonl", ByteOffset: 900, Detector: "stripe_secret_key",
		Service: "stripe", At: time.Unix(1770000000, 0),
	}); err != nil {
		t.Fatalf("PutSecretMarker: %v", err)
	}

	// What goes into the index is the marker, not the key.
	if _, err := db.PutSummary(ctx, store.Summary{
		Runtime: "claude-code", SessionID: "s1", Path: "/t.jsonl",
		Text: "wired up billing; a Stripe secret key appeared in this session",
	}, nil); err != nil {
		t.Fatal(err)
	}

	markers, err := db.ListSecretMarkers(ctx, "claude-code", "s1")
	if err != nil || len(markers) != 1 || markers[0].Service != "stripe" {
		t.Fatalf("ListSecretMarkers: %v %+v", err, markers)
	}

	hits, err := db.SearchLexical(ctx, "stripe", 5)
	if err != nil || len(hits) != 1 {
		t.Fatalf("the marker should be searchable: %v %+v", err, hits)
	}
	s, err := db.GetSummary(ctx, hits[0].SummaryID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s.Text, "sk_live") {
		t.Fatalf("a secret reached the index: %q", s.Text)
	}
}

// The vault is a separate database that is never indexed. MEMORY.md §2 keeps
// the tiers apart precisely so a credential cannot turn up in a search result,
// and §6 says that ordering is not negotiable — so it is asserted rather than
// documented.
func TestVaultIsNeverIndexed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	main, err := store.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer main.Close()

	vault, err := store.OpenVault(filepath.Join(dir, "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer vault.Close()

	if vault.Path() == main.Path() {
		t.Fatal("the vault must be a separate file")
	}
	if vault.Kind() != store.KindVault || main.Kind() != store.KindMain {
		t.Fatalf("kinds are wrong: %s %s", vault.Kind(), main.Kind())
	}

	rows, err := vault.SQL().QueryContext(ctx,
		`SELECT name, COALESCE(sql, '') FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
		upper := strings.ToUpper(ddl)
		if strings.Contains(upper, "VIRTUAL TABLE") ||
			strings.Contains(upper, "FTS5") || strings.Contains(upper, "VEC0") {
			t.Fatalf("the vault has an index structure on %s: %s", name, ddl)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"credential", "credential_proposal"} {
		if !contains(tables, want) {
			t.Fatalf("vault is missing %s; has %v", want, tables)
		}
	}
	// And nothing from the index tier leaked into it.
	for _, unwanted := range []string{"summary", "summary_fts", "summary_vec", "session_index"} {
		if contains(tables, unwanted) {
			t.Fatalf("the vault must not carry %s", unwanted)
		}
	}

	// The main database has no credential table at all.
	var n int
	if err := main.SQL().QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE name LIKE 'credential%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("the main database has %d credential tables, want 0", n)
	}
}

func TestMetaRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openMain(t)

	dims, err := db.Meta(ctx, "embedding_dims")
	if err != nil || dims != "768" {
		t.Fatalf("embedding_dims: %v %q", err, dims)
	}
	if err := db.SetMeta(ctx, "embedding_model", "test-embed"); err != nil {
		t.Fatal(err)
	}
	got, err := db.Meta(ctx, "embedding_model")
	if err != nil || got != "test-embed" {
		t.Fatalf("embedding_model: %v %q", err, got)
	}
	missing, err := db.Meta(ctx, "nothing")
	if err != nil || missing != "" {
		t.Fatalf("a missing key should be empty and not an error: %v %q", err, missing)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func f64(v float64) *float64 { return &v }
