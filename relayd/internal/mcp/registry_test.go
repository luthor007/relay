package mcp_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/luthor007/relay/relayd/internal/mcp"
	"github.com/luthor007/relay/relayd/internal/registry"
	"github.com/luthor007/relay/relayd/internal/store"
)

// The registry has no public restart that preserves history for a session that
// is still running, and inventing one here would assert a capability
// ADAPTERS.md §8 says nobody has probed. The binding says so out loud, and the
// planner falls through to telling the user.
func TestRegistrySessionsRefusesToRestart(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reg, err := registry.New(registry.Options{DB: db})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Shutdown(context.Background())

	src := mcp.RegistrySessions{Registry: reg}
	if got := src.LiveSessions(); len(got) != 0 {
		t.Fatalf("a fresh registry drives nothing: %+v", got)
	}
	err = src.Restart(context.Background(), "anything")
	if !errors.Is(err, mcp.ErrRestartUnavailable) {
		t.Fatalf("want ErrRestartUnavailable, got %v", err)
	}

	// And a nil registry is an empty list, not a panic: the gateway may be
	// wired before the registry is.
	if got := (mcp.RegistrySessions{}).LiveSessions(); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

// ORCHESTRATOR.md §4b: "Memory becomes a tool, not a special case. […]
// Installed apps are tools too. That is the same registry; there is no second
// mechanism." This test is what makes that structural rather than aspirational:
// a memory provider registers through exactly the same door a connector does,
// and is subject to exactly the same grant check.
func TestMemoryIsAProviderLikeAnyOther(t *testing.T) {
	ctx := context.Background()
	g := mcp.NewGateway(mcp.Options{Grants: grantSet{"memory:read": true}})
	g.Register(ctx, mcp.ProviderFunc{
		Name: "memory",
		Fn: func(context.Context) []mcp.Tool {
			run := func(context.Context, mcp.Call) (mcp.Result, error) {
				return mcp.Result{Text: "three hits"}, nil
			}
			return []mcp.Tool{
				{Name: mcp.ToolName("memory", "search"), Connector: "memory", Access: mcp.AccessRead, Handler: run},
				{Name: mcp.ToolName("memory", "remember"), Connector: "memory", Access: mcp.AccessWrite, Handler: run},
			}
		},
	})

	visible := names(g.Tools(ctx))
	if len(visible) != 1 || visible[0] != "memory_search" {
		t.Fatalf("memory gets the same read/write split as a connector: %v", visible)
	}
	if _, err := g.Call(ctx, mcp.Call{Tool: "memory_remember"}); !errors.Is(err, mcp.ErrNotGranted) {
		t.Fatalf("memory's write half is a separate grant too, got %v", err)
	}
}
