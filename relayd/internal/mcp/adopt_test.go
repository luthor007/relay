package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/detect"
	"github.com/luthor007/relay/relayd/internal/install"
	"github.com/luthor007/relay/relayd/internal/mcp"
)

// MEMORY.md §7's write half is gated on install.Options.Gateway being non-zero.
// The installer already has the whole rewrite, the backups and the rollback; the
// only thing it was missing was something real to point at. This is the test
// that the switch can now be flipped.
func TestDescriptorTurnsTheInstallersSwitchOn(t *testing.T) {
	zero := install.MCPGateway{}
	if !zero.Zero() {
		t.Fatal("the installer's own precondition changed")
	}

	stdio := mcp.StdioDescriptor("relay", "/usr/local/bin/relay")
	if stdio.Zero() {
		t.Fatal("a stdio descriptor must be usable")
	}
	g := stdio.Install()
	if g.Zero() {
		t.Fatal("install.MCPGateway is still zero, so the installer would still change nothing")
	}
	if g.Name != "relay" || g.Command != "/usr/local/bin/relay" ||
		len(g.Args) != 2 || g.Args[0] != "mcp" || g.Args[1] != "serve" {
		t.Fatalf("descriptor did not survive the conversion: %+v", g)
	}

	http := mcp.HTTPDescriptor("relay", "http://127.0.0.1:8765/")
	if http.Install().URL != "http://127.0.0.1:8765"+mcp.HTTPPrefix {
		t.Fatalf("http descriptor: %q", http.Install().URL)
	}
	if (mcp.Descriptor{}).Zero() != true {
		t.Fatal("an empty descriptor must read as zero")
	}
}

func TestGatewayReportsHowItIsReached(t *testing.T) {
	d := mcp.StdioDescriptor("relay", "/opt/relay")
	g := mcp.NewGateway(mcp.Options{Endpoint: d})
	got := g.Descriptor()
	if got.Name != d.Name || got.Command != d.Command || got.URL != d.URL ||
		strings.Join(got.Args, " ") != strings.Join(d.Args, " ") {
		t.Fatalf("descriptor round trip: %+v", got)
	}
}

// Adoption is only defensible because rollback works: §7 step 4 says keep the
// originals, and a user who does not like what happened gets their servers back
// in the files they were in.
func TestRollbackRestoresTheOriginals(t *testing.T) {
	const (
		claudePath = "/home/user/.claude.json"
		codexPath  = "/home/user/.codex/config.toml"
		backupDir  = "/home/user/.relay/mcp-rollback/20260810-120000"
	)
	originalClaude := `{"mcpServers":{"gh-mcp":{"command":"gh-mcp"}},"theme":"dark"}`
	originalCodex := "[mcp_servers.gh]\ncommand = \"gh-mcp\"\n"

	manifest := install.MCPManifest{
		At:      time.Unix(1_760_000_000, 0).UTC(),
		Gateway: mcp.StdioDescriptor("relay", "/usr/local/bin/relay").Install(),
		Backups: []install.MCPBackup{
			{Runtime: adapter.ClaudeCode, Original: claudePath, Copy: backupDir + "/claude-code.json"},
			{Runtime: adapter.Codex, Original: codexPath, Copy: backupDir + "/codex.toml"},
		},
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	fs := &detect.MemFS{Files: map[string]string{
		// The adopted state: both configs point at Relay only.
		claudePath: `{"mcpServers":{"relay":{"command":"/usr/local/bin/relay"}},"theme":"dark"}`,
		codexPath:  "[mcp_servers.relay]\ncommand = \"/usr/local/bin/relay\"\n",
		// The originals, as the installer kept them.
		backupDir + "/claude-code.json": originalClaude,
		backupDir + "/codex.toml":       originalCodex,
		backupDir + "/manifest.json":    string(body),
	}}

	g := mcp.NewGateway(mcp.Options{Sessions: &fakeSessions{
		live: []mcp.SessionInfo{{ID: "s1", Runtime: "claude-code"}},
	}})
	res, err := g.Rollback(context.Background(), fs, backupDir+"/manifest.json")
	if err != nil {
		t.Fatal(err)
	}

	if fs.Files[claudePath] != originalClaude {
		t.Fatalf("Claude Code's original did not come back:\n%s", fs.Files[claudePath])
	}
	if fs.Files[codexPath] != originalCodex {
		t.Fatalf("Codex's original did not come back:\n%s", fs.Files[codexPath])
	}
	if len(res.Restored) != 2 {
		t.Fatalf("restored = %v", res.Restored)
	}

	// A rollback moves the tool list just as much as an adoption does, so the
	// same "say which it did" applies.
	if len(res.Refresh.Sessions) != 1 || res.Refresh.Sessions[0].Action != mcp.RefreshNextTurn {
		t.Fatalf("the rollback must report its effect on running sessions: %+v", res.Refresh)
	}
	if !strings.Contains(res.Refresh.Reason, "rolled back") {
		t.Fatalf("reason = %q", res.Refresh.Reason)
	}
}

func TestRollbackOfAMissingManifestFails(t *testing.T) {
	fs := &detect.MemFS{Files: map[string]string{}}
	if _, err := mcp.Rollback(fs, "/nope/manifest.json"); err == nil {
		t.Fatal("a missing manifest must be an error, not a silent success")
	}
}
