package claudecode

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/adapter"
)

// flagValue returns the argument after name, and whether name was present.
func flagValue(args []string, name string) (string, bool) {
	for i, a := range args {
		if a == name {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", true
		}
	}
	return "", false
}

// The start line ADAPTERS.md §2 records, flag by flag. Every one of these is
// load-bearing and a missing one fails quietly rather than loudly, which is why
// it is asserted rather than reviewed.
func TestBuildArgsStart(t *testing.T) {
	args, err := buildArgs(argSpec{
		SessionID:      "b393be4c-99d7-4d92-ada2-df47ce494ffe",
		MCPConfigPath:  "/tmp/mcp.json",
		PermissionTool: PermissionToolName(),
		Model:          "claude-opus-5",
	})
	if err != nil {
		t.Fatal(err)
	}
	if args[0] != "-p" {
		t.Errorf("--permission-prompt-tool works only with -p, and -p must come first: %v", args)
	}
	for flag, want := range map[string]string{
		"--input-format":           "stream-json",
		"--output-format":          "stream-json",
		"--mcp-config":             "/tmp/mcp.json",
		"--permission-prompt-tool": "mcp__relay_permission__approve",
		"--session-id":             "b393be4c-99d7-4d92-ada2-df47ce494ffe",
		"--model":                  "claude-opus-5",
		// Mandatory, not tidy: without it the user's own settings.json — and
		// its permissions.defaultMode — leaks into a headless run.
		"--setting-sources": "",
		// Explicit and non-auto, so no setting anywhere decides it for us.
		"--permission-mode": "default",
	} {
		got, ok := flagValue(args, flag)
		if !ok {
			t.Errorf("missing %s in %v", flag, args)
			continue
		}
		if got != want {
			t.Errorf("%s = %q, want %q", flag, got, want)
		}
	}
	for _, flag := range []string{"--verbose", "--include-partial-messages", "--replay-user-messages", "--strict-mcp-config"} {
		if !contains(args, flag) {
			t.Errorf("missing %s in %v", flag, args)
		}
	}
	if contains(args, "--resume") || contains(args, "--fork-session") {
		t.Errorf("a fresh session must not resume anything: %v", args)
	}
}

func TestBuildArgsResumeAndFork(t *testing.T) {
	resume, err := buildArgs(argSpec{Resume: "old-id", MCPConfigPath: "/tmp/m.json"})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := flagValue(resume, "--resume"); v != "old-id" {
		t.Errorf("--resume = %q", v)
	}
	if contains(resume, "--session-id") {
		t.Error("a plain resume must not also rename the session")
	}
	if contains(resume, "--fork-session") {
		t.Error("a plain resume must not fork")
	}

	fork, err := buildArgs(argSpec{Resume: "old-id", Fork: true, SessionID: "new-id", MCPConfigPath: "/tmp/m.json"})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(fork, "--fork-session") {
		t.Errorf("fork args = %v", fork)
	}
	if v, _ := flagValue(fork, "--session-id"); v != "new-id" {
		t.Errorf("a fork gets a new name: %v", fork)
	}

	if _, err := buildArgs(argSpec{MCPConfigPath: "/tmp/m.json"}); err == nil {
		t.Error("a new session with no --session-id must be refused: we name sessions, we do not discover their names")
	}
	if _, err := buildArgs(argSpec{SessionID: "x"}); err == nil {
		t.Error("--strict-mcp-config with no --mcp-config would leave the permission tool unreachable")
	}
	if _, err := buildArgs(argSpec{SessionID: "x", Fork: true, MCPConfigPath: "/m.json"}); err == nil {
		t.Error("a fork needs something to fork from")
	}
}

func TestBuildMCPConfig(t *testing.T) {
	b, err := buildMCPConfig("http://127.0.0.1:9/mcp/s1", []adapter.MCPServer{
		{Name: "prusa", Command: "/usr/bin/prusa-mcp", Args: []string{"--stdio"}, Env: []string{"PRUSA_HOST=printer.local"}},
		{Name: "notion", URL: "https://mcp.example/notion"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var cfg mcpConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	// With --strict-mcp-config this file is the complete set, so MEMORY.md §7's
	// registry has to be injected through it — grant once, revoke once.
	if len(cfg.Servers) != 3 {
		t.Fatalf("servers = %v", cfg.Servers)
	}
	if cfg.Servers[MCPServerName].URL != "http://127.0.0.1:9/mcp/s1" || cfg.Servers[MCPServerName].Type != "http" {
		t.Errorf("permission server = %+v", cfg.Servers[MCPServerName])
	}
	if cfg.Servers["prusa"].Env["PRUSA_HOST"] != "printer.local" {
		t.Errorf("prusa = %+v", cfg.Servers["prusa"])
	}
	if cfg.Servers["notion"].Type != "http" {
		t.Errorf("a URL server must be typed http: %+v", cfg.Servers["notion"])
	}

	if _, err := buildMCPConfig("x", []adapter.MCPServer{{Name: MCPServerName}}); err == nil {
		t.Error("a user server must not be able to shadow the permission prompt")
	}
	if _, err := buildMCPConfig("x", []adapter.MCPServer{{Name: "bad", Env: []string{"NOTKV"}}}); err == nil {
		t.Error("a malformed env entry must be refused rather than silently dropped")
	}
	if names := mcpServerNames(b); strings.Join(names, ",") != "notion,prusa,relay_permission" {
		t.Errorf("names = %v", names)
	}
}
