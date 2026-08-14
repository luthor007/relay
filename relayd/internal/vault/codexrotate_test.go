package vault_test

import (
	"context"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/vault"
)

// A ChatGPT login that worked exactly once.
//
// OpenAI rotates: a refresh returns a new refresh token and invalidates the one
// that bought it. llm.resolveCodexVault calls llm.CodexPersist on every
// rotation — and nothing assigned it, so the vault kept the sign-in token
// forever and the next process to use it got
//
//	http 401 … "code": "refresh_token_reused"
//
// which is what a real machine reported on its second `relay setup`.

func TestARotatedTokenReplacesTheSpentOne(t *testing.T) {
	v := open(t, vault.Options{})
	ctx := context.Background()

	first, err := v.Put(ctx, vault.Input{
		Service: "openai", Label: "ChatGPT (alexis@example.com)", Secret: "refresh-0",
		Source: vault.Provenance{Kind: vault.SourceTyped},
	})
	if err != nil {
		t.Fatal(err)
	}

	persist := vault.RotateCodex(v)
	if persist == nil {
		t.Fatal("no persister")
	}
	if err := persist(ctx, first.ID, "refresh-1"); err != nil {
		t.Fatal(err)
	}

	// The next process reads this, and it must be the token that is still good.
	got, err := v.Reveal(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != "refresh-1" {
		t.Errorf("vault holds %q, want the rotated token", got)
	}

	// Under the same id, so config.toml's `codex:vault:<id>` still points at it.
	e, err := v.Get(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	// And still recognisable: a rotation must not turn a named login into an
	// anonymous row in the credentials screen.
	if e.Service != "openai" || !strings.Contains(e.Label, "ChatGPT") {
		t.Errorf("entry lost its identity: %+v", e)
	}
	// Provenance is about origin, and the origin did not change: this is the
	// same ChatGPT sign-in, with the value the provider handed back in place of
	// the one it just invalidated.
	if e.Source.Kind != vault.SourceTyped {
		t.Errorf("provenance = %q, want the original kept", e.Source.Kind)
	}
}

// An empty rotation is refused rather than stored: writing "" over a working
// refresh token would lock the user out in a way no error message explains.
func TestAnEmptyRotationIsRefused(t *testing.T) {
	v := open(t, vault.Options{})
	ctx := context.Background()
	e, err := v.Put(ctx, vault.Input{
		Service: "openai", Label: "ChatGPT", Secret: "refresh-0",
		Source: vault.Provenance{Kind: vault.SourceTyped},
	})
	if err != nil {
		t.Fatal(err)
	}
	persist := vault.RotateCodex(v)
	if err := persist(ctx, e.ID, ""); err == nil {
		t.Error("stored an empty rotated token")
	}
	if got, _ := v.Reveal(ctx, e.ID); got != "refresh-0" {
		t.Errorf("the good token was overwritten: %q", got)
	}
}
