package compaction

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/facts"
	"github.com/luthor007/relay/relayd/internal/store"
)

func mainDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedSession(t *testing.T, db *store.DB) SessionView {
	t.Helper()
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 0)

	if err := db.PutSession(ctx, store.Session{
		ID:         "relay-1",
		Runtime:    string(adapter.Codex),
		NativeID:   "thread_abc",
		Subject:    "payments retry loop",
		Workspace:  "/repos/api",
		CreatedAt:  at,
		LastActive: at.Add(time.Hour),
		State:      store.SessionIdle,
	}); err != nil {
		t.Fatal(err)
	}

	for i, text := range []string{
		"start on the payments retry loop",
		"make the idempotency key stable across retries",
	} {
		if err := db.PutTurn(ctx, store.Turn{
			ID: "t" + itoa(i), SessionID: "relay-1", Role: "user", Text: text, At: at.Add(time.Duration(i) * time.Minute), OK: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// An agent turn, which must not be mistaken for what the user asked for.
	if err := db.PutTurn(ctx, store.Turn{
		ID: "t-agent", SessionID: "relay-1", Role: "agent", Text: "I have rewritten the retry loop", At: at.Add(3 * time.Minute), OK: true,
	}); err != nil {
		t.Fatal(err)
	}

	for i, target := range []string{"pkg/pay/retry.go", "pkg/pay/idem.go"} {
		if err := db.PutToolCall(ctx, store.ToolCall{
			ID: "tc" + itoa(i), SessionID: "relay-1", Tool: "Edit", Target: target, At: at.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}

	return SessionView{ID: "relay-1", Runtime: adapter.Codex, Workspace: "/repos/api"}
}

func TestStoreBriefsReadsTheIndexAndTheRegistry(t *testing.T) {
	ctx := context.Background()
	db := mainDB(t)
	v := seedSession(t, db)

	// The session summary, keyed on the runtime's own id, not Relay's.
	if _, err := db.SQL().ExecContext(ctx, `
		INSERT INTO summary (kind, runtime, session_id, path, text, model, created_at)
		VALUES ('session', ?, 'thread_abc', '/rollouts/abc.jsonl', ?, 'small', 1)`,
		string(adapter.Codex), "reworking how payment retries pick an idempotency key"); err != nil {
		t.Fatal(err)
	}

	b := builder(t, BriefOptions{Budget: 4000})
	sb, err := NewStoreBriefs(db, b, nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := sb.Brief(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	if got.Work != "reworking how payment retries pick an idempotency key" {
		t.Fatalf("work = %q, want the indexed summary", got.Work)
	}
	if len(got.Files) != 2 {
		t.Fatalf("files = %v", got.Files)
	}
	text := got.Text()
	if !strings.Contains(text, "pkg/pay/retry.go") || !strings.Contains(text, "/repos/api") {
		t.Fatalf("brief:\n%s", text)
	}
	if strings.Contains(text, "I have rewritten") {
		t.Fatal("agent prose is not brief material; the index already summarised it")
	}
}

// No summary row yet — the common case on a box where backfill has run but
// summarisation has not. The brief falls back to what the user last asked for.
func TestStoreBriefsFallsBackToTheUserTurns(t *testing.T) {
	ctx := context.Background()
	db := mainDB(t)
	v := seedSession(t, db)

	b := builder(t, BriefOptions{Budget: 4000})
	sb, err := NewStoreBriefs(db, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := sb.Brief(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	// The registry row's subject is the first fallback.
	if got.Work != "payments retry loop" {
		t.Fatalf("work = %q", got.Work)
	}

	// With no subject either, the newest user turn stands in.
	if _, err := db.SQL().ExecContext(ctx, `UPDATE session SET subject = '' WHERE id = 'relay-1'`); err != nil {
		t.Fatal(err)
	}
	got, err = sb.Brief(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	if got.Work != "make the idempotency key stable across retries" {
		t.Fatalf("work = %q, want the newest user turn", got.Work)
	}
}

func TestStoreBriefsHasNothingToCarryForAnEmptySession(t *testing.T) {
	ctx := context.Background()
	db := mainDB(t)
	b := builder(t, BriefOptions{})
	sb, err := NewStoreBriefs(db, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sb.Brief(ctx, SessionView{ID: "nobody", Runtime: adapter.Codex}); !errors.Is(err, ErrNothingToCarry) {
		t.Fatalf("err = %v, want ErrNothingToCarry — a brief about a session we know nothing about is a fabrication", err)
	}
}

func TestNewStoreBriefsRefusesTheWrongDatabase(t *testing.T) {
	vault, err := store.OpenVault(filepath.Join(t.TempDir(), "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer vault.Close()

	b := builder(t, BriefOptions{})
	if _, err := NewStoreBriefs(vault, b, nil); err == nil {
		t.Fatal("the vault is never indexed and holds no summaries; asking it for a brief must fail loudly")
	}
	if _, err := NewStoreBriefs(mainDB(t), nil, nil); !errors.Is(err, ErrNoRedactor) {
		t.Fatalf("err = %v", err)
	}
}

func TestFactStoreSentences(t *testing.T) {
	ctx := context.Background()
	db := mainDB(t)

	fs, err := facts.Open(db, facts.Options{Redactor: facts.Detector()})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1_700_000_000, 0)
	ev := []facts.Evidence{{Runtime: string(adapter.Codex), SessionID: "relay-1", At: at}}

	res, err := fs.Reconcile(ctx, []facts.Observation{
		{Predicate: facts.Uses, Object: "Go", Text: "writes Go", Confidence: 0.8, Evidence: ev},
		{Predicate: facts.DeploysOn, Object: "fly.io", Text: "deploys the api repo on fly.io", Confidence: 0.6, Evidence: ev},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 2 {
		t.Fatalf("created = %v, rejected = %v", res.Created, res.Rejected)
	}

	src := FactStore{Store: fs, Now: func() time.Time { return at }}
	got, err := src.Sentences(ctx, "/repos/api", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("sentences = %v", got)
	}
	if !strings.Contains(got[0], "api") {
		t.Fatalf("the fact naming this repo should come first: %v", got)
	}

	// The limit is respected, and a nil store is not an error.
	if got, _ := src.Sentences(ctx, "", 1); len(got) != 1 {
		t.Fatalf("limit ignored: %v", got)
	}
	if got, err := (FactStore{}).Sentences(ctx, "", 5); err != nil || got != nil {
		t.Fatalf("no fact tier must be silence, not an error: %v %v", got, err)
	}
}

func TestStoreBriefsIncludesFacts(t *testing.T) {
	ctx := context.Background()
	db := mainDB(t)
	v := seedSession(t, db)

	b := builder(t, BriefOptions{Budget: 4000})
	sb, err := NewStoreBriefs(db, b, staticFacts{"writes Go", "deploys on fly.io"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := sb.Brief(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Facts) != 2 {
		t.Fatalf("facts = %v", got.Facts)
	}
	if !strings.Contains(got.Text(), "Standing facts") {
		t.Fatalf("brief:\n%s", got.Text())
	}
}

type staticFacts []string

func (s staticFacts) Sentences(context.Context, string, int) ([]string, error) {
	return []string(s), nil
}

func TestBaseName(t *testing.T) {
	cases := map[string]string{
		"/repos/api":  "api",
		"/repos/api/": "api",
		"api":         "api",
		"":            "",
	}
	for in, want := range cases {
		if got := baseName(in); got != want {
			t.Errorf("baseName(%q) = %q, want %q", in, got, want)
		}
	}
}
