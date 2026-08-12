package mcp_test

import (
	"testing"

	"github.com/luthor007/relay/relayd/internal/mcp"
)

// The scope string is the contract between this package and everything that
// renders or stores a grant: internal/api's phraseScope reads the suffix, and
// the `grant` table stores the whole string.
func TestScopeRoundTrips(t *testing.T) {
	for _, half := range []mcp.Access{mcp.AccessRead, mcp.AccessWrite} {
		s := half.Scope("gmail")
		connector, got, ok := mcp.ParseScope(s)
		if !ok || connector != "gmail" || got != half {
			t.Fatalf("ParseScope(%q) = %q %q %v", s, connector, got, ok)
		}
	}
	for _, bad := range []string{"", "gmail", "gmail:", ":read", "gmail:admin"} {
		if _, _, ok := mcp.ParseScope(bad); ok {
			t.Fatalf("ParseScope(%q) should not parse", bad)
		}
	}
	tool := mcp.Tool{Connector: "gmail", Access: mcp.AccessWrite}
	if tool.Scope() != "gmail:write" {
		t.Fatalf("tool scope = %q", tool.Scope())
	}
	if mcp.ToolName("Prusa", "status") != "prusa_status" {
		t.Fatalf("tool name = %q", mcp.ToolName("Prusa", "status"))
	}
	if mcp.Access("sideways").Valid() {
		t.Fatal("there are exactly two halves")
	}
}
