package install

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/detect"
)

// MEMORY.md §7, the write half.

// While Relay's own MCP gateway does not exist, adoption would point five
// runtimes at nothing and leave a machine with seven working servers holding
// none. So the reconciliation runs, the union is presented and recorded, and
// nothing on the machine is touched — and the installer says that in words.
func TestMCPDoesNotAdoptBeforeTheGatewayExists(t *testing.T) {
	opts, script, fs := newOpts(t, baseAnswers(), nil)
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}

	if !res.MCP.Accepted {
		t.Fatal("the user said yes")
	}
	if res.MCP.Adopted {
		t.Fatal("nothing may be rewritten while there is no gateway to point at")
	}
	if !strings.Contains(res.MCP.Note, "gateway") {
		t.Errorf("note = %q; the reason has to be stated", res.MCP.Note)
	}

	// The originals are exactly as they were.
	if !strings.Contains(fs.Files[home+"/.claude.json"], "gh-mcp") {
		t.Error("Claude Code's config was modified")
	}
	if !strings.Contains(fs.Files[home+"/.codex/config.toml"], "mcp_servers.gh") {
		t.Error("Codex's config was modified")
	}

	// But the union is written down, ready for the gateway.
	reg, ok := fs.Files[res.MCP.Registry]
	if !ok {
		t.Fatalf("no registry written (%q)", res.MCP.Registry)
	}
	if !strings.Contains(reg, "gh-mcp") || !strings.Contains(reg, "claude-code") {
		t.Errorf("registry:\n%s", reg)
	}
	if !strings.Contains(script.Output(), "Manage them in one place?") {
		t.Error("MEMORY.md §7's question has to actually be asked")
	}
}

// With a gateway, the same code adopts for real: every runtime points at Relay
// and every original is recoverable.
func TestMCPAdoptionRewritesAndIsReversible(t *testing.T) {
	opts, _, fs := newOpts(t, baseAnswers(), func(o *Options) {
		o.Gateway = MCPGateway{Name: "relay", Command: "/usr/local/bin/relayd", Args: []string{"mcp"}}
	})
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !res.MCP.Adopted {
		t.Fatalf("nothing adopted; warnings=%v", res.MCP.Warnings)
	}
	if len(res.MCP.Backups) != 2 {
		t.Fatalf("backups = %+v, want one per runtime with a config file", res.MCP.Backups)
	}

	// Claude Code now points at Relay, and only at Relay.
	claude := fs.Files[home+"/.claude.json"]
	if !strings.Contains(claude, "relayd") || strings.Contains(claude, "gh-mcp") {
		t.Errorf("claude config after adoption:\n%s", claude)
	}
	// Codex is TOML, and the same surgical edit applies there.
	codex := fs.Files[home+"/.codex/config.toml"]
	if !strings.Contains(codex, "relayd") {
		t.Errorf("codex config after adoption:\n%s", codex)
	}

	// The manifest is a manifest.
	var m MCPManifest
	if err := json.Unmarshal([]byte(fs.Files[res.MCP.ManifestPath]), &m); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if len(m.Backups) != 2 || m.Gateway.Command == "" {
		t.Fatalf("manifest = %+v", m)
	}

	// And it undoes.
	if _, err := RollbackMCP(fs, res.MCP.ManifestPath); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if !strings.Contains(fs.Files[home+"/.claude.json"], "gh-mcp") {
		t.Errorf("rollback did not restore Claude Code:\n%s", fs.Files[home+"/.claude.json"])
	}
	if !strings.Contains(fs.Files[home+"/.codex/config.toml"], "mcp_servers.gh") {
		t.Errorf("rollback did not restore Codex:\n%s", fs.Files[home+"/.codex/config.toml"])
	}
}

// A surgical edit: everything in the file that is not the MCP list survives.
func TestAdoptionPreservesUnrelatedSettings(t *testing.T) {
	original := []byte(`{"theme":"dark","mcpServers":{"gh":{"command":"gh-mcp"}},"telemetry":false}`)
	out, err := pointAtGateway(adapter.ClaudeCode, original,
		MCPGateway{Name: "relay", Command: "relayd", Args: []string{"mcp"}})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["theme"] != "dark" {
		t.Errorf("unrelated setting lost: %v", doc)
	}
	if doc["telemetry"] != false {
		t.Errorf("unrelated setting lost: %v", doc)
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if len(servers) != 1 {
		t.Fatalf("servers = %v", servers)
	}
	if _, ok := servers["relay"]; !ok {
		t.Errorf("servers = %v, want just relay", servers)
	}
}

// OpenCode's shape is different — the whole argv is the command — and adopting
// has to speak each runtime's own vocabulary.
func TestGatewayEntrySpeaksEachRuntimesVocabulary(t *testing.T) {
	g := MCPGateway{Name: "relay", Command: "relayd", Args: []string{"mcp"}}

	oc := gatewayEntry(adapter.OpenCode, g)
	argv, ok := oc["command"].([]string)
	if !ok || len(argv) != 2 || argv[0] != "relayd" {
		t.Errorf("opencode entry = %v", oc)
	}
	if oc["type"] != "local" {
		t.Errorf("opencode entry = %v", oc)
	}

	cc := gatewayEntry(adapter.ClaudeCode, g)
	if cc["command"] != "relayd" {
		t.Errorf("claude entry = %v", cc)
	}

	remote := gatewayEntry(adapter.Hermes, MCPGateway{Name: "relay", URL: "http://127.0.0.1:8787/mcp"})
	if remote["url"] != "http://127.0.0.1:8787/mcp" || remote["type"] != "http" {
		t.Errorf("remote entry = %v", remote)
	}
}

// A runtime that answered only through its CLI has no file to rewrite and no
// file to restore, so it is left alone and named.
func TestCLIOnlyRuntimeIsNotRewritten(t *testing.T) {
	answers := baseAnswers()
	opts, _, _ := newOpts(t, answers, func(o *Options) {
		o.Gateway = MCPGateway{Name: "relay", Command: "relayd"}
		ex := o.Env.Exec.(*detect.FakeExec)
		ex.Responses[detect.Key("claude", "mcp", "list", "--json")] = detect.Result{
			Stdout: []byte(`{"mcpServers":{"live":{"command":"live-mcp"}}}`),
		}
	})
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(res.MCP.Warnings, " ")
	if !strings.Contains(joined, "answered through its CLI") {
		t.Errorf("warnings = %v", res.MCP.Warnings)
	}
	for _, b := range res.MCP.Backups {
		if b.Runtime == adapter.ClaudeCode {
			t.Error("a CLI-only runtime must not be rewritten, because it could not be restored")
		}
	}
}

// MEMORY.md §7's catch: some runtimes enumerate tools once per session, so a
// change does not reach a session already running. The orchestrator says which.
func TestRunningRuntimesAreNamedForRestart(t *testing.T) {
	opts, script, _ := newOpts(t, baseAnswers(), func(o *Options) {
		o.Gateway = MCPGateway{Name: "relay", Command: "relayd"}
		o.Env.Procs = detect.FakeProcs{{PID: 77, Command: "codex", Args: "codex app-server"}}
	})
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rt := range res.MCP.NeedRestart {
		if rt == adapter.Codex {
			found = true
		}
	}
	if !found {
		t.Fatalf("NeedRestart = %v, want codex", res.MCP.NeedRestart)
	}
	if !strings.Contains(script.Output(), "on restart") {
		t.Error("the user has to be told, or the tool they just connected is invisible for no reason they can see")
	}
}

func TestDecliningLeavesEverythingAlone(t *testing.T) {
	answers := baseAnswers()
	answers["mcp.adopt"] = "no"
	opts, _, fs := newOpts(t, answers, func(o *Options) {
		o.Gateway = MCPGateway{Name: "relay", Command: "relayd"}
	})
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.MCP.Accepted || res.MCP.Adopted {
		t.Fatal("no means no")
	}
	if !strings.Contains(fs.Files[home+"/.claude.json"], "gh-mcp") {
		t.Error("a declined adoption must not have touched anything")
	}
}

func TestRollbackReportsAMissingBackup(t *testing.T) {
	fs := &detect.MemFS{Files: map[string]string{}}
	m := MCPManifest{At: time.Now(), Backups: []MCPBackup{{
		Runtime: adapter.Codex, Original: "/x/config.toml", Copy: "/gone/codex.toml",
	}}}
	b, _ := json.Marshal(m)
	_ = fs.WriteFile("/m.json", b, 0o600)
	if _, err := RollbackMCP(fs, "/m.json"); err == nil {
		t.Fatal("a rollback that could not restore a file must say so")
	}
}
