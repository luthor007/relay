package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/config"
	"github.com/luthor007/relay/relayd/internal/routing"
)

// The entitlement ids are duplicated so config stays a leaf package —
// routing -> llm -> config is a real import edge, so config importing routing
// would be a cycle. This test lives in package config_test, which may import
// both, and it is what keeps the duplication honest.
//
// Order matters as well as membership: routing.Table is consulted top to
// bottom, so the list config offers a user is the preference order the router
// will apply. Two lists that agree on membership and disagree on order would
// present the wrong recommendation first.
func TestKnownEntitlementsMirrorTheRoutingTable(t *testing.T) {
	want := routing.Entitled()
	got := config.KnownEntitlements
	if len(got) != len(want) {
		t.Fatalf("config knows %d entitlements and routing has %d rows:\n config: %v\nrouting: %v",
			len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != string(want[i]) {
			t.Fatalf("entitlement %d is %q in config and %q in routing", i, got[i], want[i])
		}
	}
}

// Every id config will accept must resolve to a row the router can act on.
// An id that validates and then matches nothing is worse than one that is
// refused: the user is told their subscription was recorded and it decides
// nothing, silently, forever.
func TestEveryKnownEntitlementHasARoutingRow(t *testing.T) {
	for _, id := range config.KnownEntitlements {
		if _, ok := routing.Row(routing.Entitlement(id)); !ok {
			t.Errorf("config accepts %q and routing has no row for it", id)
		}
	}
}

// MEMORY.md §8: "the entitlement set starts empty". Empty is not the same as
// api-keys — one means nobody has said, the other means the user said they hold
// no subscription — and only the second licenses picking on load alone. A
// default that guessed either way would be the inference §8 forbids.
func TestTheDefaultRecordsNoEntitlements(t *testing.T) {
	if got := config.Default().Routing.Entitlements; len(got) != 0 {
		t.Fatalf("the default config claims entitlements: %v", got)
	}
}

// A recorded entitlement has to survive the file, because the file is the only
// place it is stored: there is no schema column and no console editor for it.
func TestEntitlementsRoundTripThroughTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := config.Default()
	cfg.Routing.Entitlements = []string{"claude-subscription", "github-copilot"}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	back, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(back.Routing.Entitlements) != 2 ||
		back.Routing.Entitlements[0] != "claude-subscription" ||
		back.Routing.Entitlements[1] != "github-copilot" {
		t.Fatalf("entitlements = %v", back.Routing.Entitlements)
	}
}

// A config file written before this section existed must still load. Absent is
// the same as empty here, and it is the state of every machine installed so
// far.
func TestAbsentRoutingSectionIsAnEmptySet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("listen = \"127.0.0.1:8787\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(c.Routing.Entitlements) != 0 {
		t.Fatalf("entitlements = %v, want none", c.Routing.Entitlements)
	}
}

// A typo is refused rather than dropped, and the refusal says what to type.
//
// This is the case the whole field exists for. An entitlement overrides
// capability comparison, so "clause-subscription" silently ignored means the
// user believes their Claude work is going to Claude Code and it is not — a
// metered bill nobody expected, which is MEMORY.md §8's stated failure mode.
func TestAnUnknownEntitlementIsRefusedByName(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.Entitlements = []string{"claude-subscription", "clause-subscription"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("an unknown entitlement must not validate")
	}
	if !strings.Contains(err.Error(), "clause-subscription") {
		t.Errorf("the error does not name the bad id: %v", err)
	}
	// The valid list has to be in the message; a rejection with nothing to type
	// instead is a dead end in a file the user has to edit by hand.
	for _, want := range config.KnownEntitlements {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not offer %q: %v", want, err)
		}
	}
}
