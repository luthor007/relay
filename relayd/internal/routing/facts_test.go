package routing_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/routing"
	"github.com/luthor007/relay/relayd/internal/store"
)

func openMain(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func facts(t *testing.T, db *store.DB) *routing.FactPreferences {
	t.Helper()
	p, err := routing.NewFactPreferences(db)
	if err != nil {
		t.Fatal(err)
	}
	p.SetClock(now)
	return p
}

// MEMORY.md §8 step 2: a preference stated once, or learned from §5, decides
// the runtime before the entitlement table gets a say.
func TestLearnedPreferenceIsScoped(t *testing.T) {
	ctx := context.Background()
	db := openMain(t)

	if err := routing.PutPreferenceFact(ctx, db, routing.PreferenceFact{
		ID: "f1", Runtime: adapter.Codex, Confidence: 0.9,
		Text: "Always uses Codex for Rust", Session: "s1", Quote: "let's do this one in codex",
		At: now().Add(-24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	p := facts(t, db)

	t.Run("in scope", func(t *testing.T) {
		rt, why, ok := p.Preferred(ctx, routing.RuntimeRequest{Text: "fix the rust borrow checker error"})
		if !ok || rt != adapter.Codex {
			t.Fatalf("got %q ok=%v, want codex", rt, ok)
		}
		if why == "" {
			t.Error("the evidence has to come with it — that is what gets spoken when someone asks why")
		}
	})

	t.Run("out of scope", func(t *testing.T) {
		// "Always uses Codex for Rust" is not "always uses Codex". Applying it
		// to unrelated work is how one observation becomes a global override.
		if rt, _, ok := p.Preferred(ctx, routing.RuntimeRequest{Text: "update the react components"}); ok {
			t.Fatalf("a Rust-scoped preference reached %q for React work", rt)
		}
	})
}

// A fact whose object is not one of the five runtimes says nothing about
// routing, however true it is.
func TestNonRuntimeFactsAreNotRoutingPreferences(t *testing.T) {
	ctx := context.Background()
	db := openMain(t)
	if _, err := db.SQL().ExecContext(ctx, `
        INSERT INTO fact (id, subject, predicate, object, text, confidence, first_seen, last_seen)
        VALUES ('f2', 'user', 'prefers', 'typescript', 'Prefers TypeScript over JavaScript', 0.9, ?, ?)`,
		now().UnixMilli(), now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	p := facts(t, db)
	if rt, _, ok := p.Preferred(ctx, routing.RuntimeRequest{Text: "write a typescript helper"}); ok {
		t.Fatalf("a language preference produced a runtime preference: %q", rt)
	}
}

// MEMORY.md §5 decays on last observation. A preference last seen a year ago
// stops steering anything, which is the whole point of decaying rather than
// deleting.
func TestOldPreferencesDecayOut(t *testing.T) {
	ctx := context.Background()
	db := openMain(t)
	if err := routing.PutPreferenceFact(ctx, db, routing.PreferenceFact{
		ID: "f3", Runtime: adapter.OpenCode, Confidence: 0.9,
		Text: "Uses OpenCode for the website", Session: "s1",
		At: now().Add(-2 * 365 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	p := facts(t, db)
	if rt, _, ok := p.Preferred(ctx, routing.RuntimeRequest{Text: "tweak the website header"}); ok {
		t.Fatalf("a two-year-old preference still routed to %q", rt)
	}
}

// MEMORY.md §5: a fact that cannot point at where it came from is deleted, not
// kept at low confidence.
func TestAPreferenceWithoutEvidenceIsRefused(t *testing.T) {
	ctx := context.Background()
	db := openMain(t)
	err := routing.PutPreferenceFact(ctx, db, routing.PreferenceFact{
		ID: "f4", Runtime: adapter.Codex, Text: "Uses Codex", Confidence: 0.9,
	})
	if err == nil {
		t.Fatal("a fact with no evidence was accepted")
	}
	var n int
	if err := db.SQL().QueryRowContext(ctx, `SELECT count(*) FROM fact WHERE id = 'f4'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("the evidence-less fact was left behind (%d rows); the write has to roll back", n)
	}
}

// The facts tier lives in the main database. Pointing this at the vault would
// silently answer "no preferences" forever, which is worse than failing.
func TestFactPreferencesRefuseTheVault(t *testing.T) {
	vault, err := store.OpenVault(filepath.Join(t.TempDir(), "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer vault.Close()
	if _, err := routing.NewFactPreferences(vault); err == nil {
		t.Fatal("the vault was accepted as a facts source")
	}
}

// A preference with no scope at all is a global habit and applies everywhere.
func TestGlobalPreferenceApplies(t *testing.T) {
	ctx := context.Background()
	db := openMain(t)
	if err := routing.PutPreferenceFact(ctx, db, routing.PreferenceFact{
		ID: "f5", Runtime: adapter.ClaudeCode, Confidence: 0.85,
		Text: "Always uses Claude Code", Session: "s1", At: now(),
	}); err != nil {
		t.Fatal(err)
	}
	p := facts(t, db)
	rt, _, ok := p.Preferred(ctx, routing.RuntimeRequest{Text: "anything at all"})
	if !ok || rt != adapter.ClaudeCode {
		t.Fatalf("got %q ok=%v", rt, ok)
	}
}

// And the whole loop: a stated preference beats the entitlement table, which is
// step 2 beating step 3.
func TestFactPreferenceBeatsEntitlement(t *testing.T) {
	ctx := context.Background()
	db := openMain(t)
	if err := routing.PutPreferenceFact(ctx, db, routing.PreferenceFact{
		ID: "f6", Runtime: adapter.Codex, Confidence: 0.9,
		Text: "Always uses Codex for the api repo", Session: "s1", At: now(),
	}); err != nil {
		t.Fatal(err)
	}
	rr, err := routing.NewRuntimeRouter(routing.RuntimeOptions{
		Profiles:     routing.StaticProfiles(used(adapter.ClaudeCode), used(adapter.Codex)),
		Entitlements: routing.Entitlements{routing.ClaudeSubscription},
		Preferences:  facts(t, db),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := rr.Choose(ctx, routing.RuntimeRequest{Text: "add an endpoint", Workspace: "/repo/api"})
	if got.Runtime != adapter.Codex || got.Reason != routing.RuntimeLearned {
		t.Fatalf("got %s via %s, want codex via a learned preference", got.Runtime, got.Reason)
	}
}
