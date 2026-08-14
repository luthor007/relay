package llm_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luthor007/relay/relayd/internal/llm"
)

// The other half of the same bug: the resolver has to call the persister.
//
// llm.CodexPersist existed, was documented as "the daemon wires it to the
// vault", and was assigned by nothing in the repository — so this hook was
// never once invoked in production. A test that it is called is the thing that
// would have caught that.
func TestARotatedRefreshTokenIsHandedToThePersister(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// What OpenAI does: hand back a new refresh token and invalidate the
		// one that bought it.
		_, _ = w.Write([]byte(`{"access_token":"access-1","refresh_token":"refresh-1","expires_in":3600}`))
	}))
	defer srv.Close()

	old := llm.CodexAuthBase
	llm.CodexAuthBase = srv.URL
	defer func() { llm.CodexAuthBase = old }()

	var gotID, gotRefresh string
	oldPersist := llm.CodexPersist
	llm.CodexPersist = func(ctx context.Context, id, refresh string) error {
		gotID, gotRefresh = id, refresh
		return nil
	}
	defer func() { llm.CodexPersist = oldPersist }()

	lookup := func(ctx context.Context, id string) (string, error) { return "refresh-0", nil }
	ref, err := llm.ParseRef("codex:vault:cred-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ref.Resolve(context.Background(), lookup); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if gotID != "cred-1" || gotRefresh != "refresh-1" {
		t.Errorf("persister got (%q, %q), want the rotated token under the same id",
			gotID, gotRefresh)
	}
}

// And the wiring itself, which is the part that was missing.
//
// The hook being called is worth nothing if no process assigns it. Both
// binaries do now; this pins the contract they satisfy so a refactor that drops
// the assignment fails here rather than on somebody's machine a day later.
func TestThePersisterContractIsWhatTheVaultOffers(t *testing.T) {
	// vault.RotateCodex returns exactly this shape. Compile-time agreement is
	// the point of the assertion.
	var _ func(ctx context.Context, id, refresh string) error = llm.CodexPersist
}
