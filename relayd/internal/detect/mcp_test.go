package detect

import (
	"context"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/adapter"
)

// MEMORY.md §7: the runtimes arrive with servers already connected, so the
// installer enumerates and reconciles rather than starting empty.

func mcpEnv() Env {
	home := "/home/u"
	return Env{
		FS: &MemFS{
			Files: map[string]string{
				// Claude Code, read from its own config file.
				home + "/.claude.json": `{
					"mcpServers": {
						"github": {"command": "gh-mcp", "args": ["serve"]},
						"gmail":  {"command": "npx", "args": ["-y", "gmail-mcp"], "env": {"GMAIL_PROFILE": "work"}}
					}
				}`,
				home + "/.claude/projects/p/a.jsonl": "{}",

				// Codex, TOML. Same github server, different name — one server.
				home + "/.codex/config.toml": `
[mcp_servers.gh]
command = "gh-mcp"
args = ["serve"]

[mcp_servers.printer]
url = "http://prusa.lan:8080/mcp"
`,
				home + "/.codex/session_index.jsonl": "{}\n",

				// OpenCode, whose command is the whole argv.
				home + "/.config/opencode/opencode.json": `{
					"mcp": {
						"github": {"type": "local", "command": ["gh-mcp", "serve"]},
						"weather": {"type": "remote", "url": "https://wx.example/mcp"}
					}
				}`,
				home + "/.local/share/opencode/x": "y",

				// Hermes is installed and its config is unreadable — that must be
				// reported as unreadable, never as zero servers.
				home + "/.hermes/state.db": "sqlite",
			},
			Dirs: []string{home},
		},
		Exec: &FakeExec{
			Paths: map[string]string{
				"claude": "/usr/local/bin/claude", "codex": "/usr/local/bin/codex",
				"opencode": "/usr/local/bin/opencode", "hermes": "/usr/local/bin/hermes",
			},
		},
		Getenv: func(string) string { return "" },
		Home:   home,
		GOOS:   "linux",
	}
}

func TestReadMCPDeduplicatesByCommandAndArgs(t *testing.T) {
	env := mcpEnv()
	rep := Detect(context.Background(), env, Options{SkipProcesses: true})
	inv := ReadMCP(context.Background(), env, rep)

	// gh-mcp serve appears in Claude Code as "github", in Codex as "gh" and in
	// OpenCode as "github". MEMORY.md §7: the same server configured three
	// times is one server.
	var gh *MCPEntry
	for i := range inv.Servers {
		if inv.Servers[i].Command == "gh-mcp" {
			gh = &inv.Servers[i]
		}
	}
	if gh == nil {
		t.Fatalf("gh-mcp missing from the union: %+v", inv.Servers)
	}
	if len(gh.Runtimes) != 3 {
		t.Errorf("gh-mcp claimed by %v, want all three", gh.Runtimes)
	}
	if gh.Names[adapter.Codex] != "gh" {
		t.Errorf("Codex calls it %q; the union has to keep every name or the user cannot find their row", gh.Names[adapter.Codex])
	}
	if !gh.Shared() {
		t.Error("a server three runtimes have is shared")
	}

	// Four distinct servers: gh-mcp, gmail, the printer URL, the weather URL.
	if len(inv.Servers) != 4 {
		for _, s := range inv.Servers {
			t.Logf("  %s %s %v", s.Name, s.Display(), s.Runtimes)
		}
		t.Fatalf("union has %d servers, want 4", len(inv.Servers))
	}
}

func TestReadMCPHeadline(t *testing.T) {
	env := mcpEnv()
	rep := Detect(context.Background(), env, Options{SkipProcesses: true})
	inv := ReadMCP(context.Background(), env, rep)

	h := inv.Headline()
	if !strings.Contains(h, "4 MCP servers") || !strings.Contains(h, "3 tools") {
		t.Errorf("headline = %q, want MEMORY.md §7's shape", h)
	}
	if !strings.HasSuffix(h, "Manage them in one place?") {
		t.Errorf("headline = %q", h)
	}
}

// "No MCP servers" and "we could not read this runtime's config" lead to
// opposite decisions, and only one of them is recoverable. So an unreadable
// runtime is reported as unreadable, with the places we looked.
func TestUnreadableRuntimeIsNotReportedAsEmpty(t *testing.T) {
	env := mcpEnv()
	rep := Detect(context.Background(), env, Options{SkipProcesses: true})
	inv := ReadMCP(context.Background(), env, rep)

	var hermes *MCPOrigin
	for i := range inv.Origins {
		if inv.Origins[i].Runtime == adapter.Hermes {
			hermes = &inv.Origins[i]
		}
	}
	if hermes == nil {
		t.Fatal("no origin recorded for hermes")
	}
	if hermes.Readable {
		t.Fatal("hermes has no readable MCP config in this fixture")
	}
	if !strings.Contains(hermes.Reason, "config.json") {
		t.Errorf("reason should name what we tried: %q", hermes.Reason)
	}
	if len(inv.Unreadable()) == 0 {
		t.Error("Unreadable() has to surface it, or the union silently under-reports")
	}
}

func TestNotInstalledSaysSo(t *testing.T) {
	env := mcpEnv()
	rep := Detect(context.Background(), env, Options{SkipProcesses: true})
	inv := ReadMCP(context.Background(), env, rep)
	for _, o := range inv.Origins {
		if o.Runtime == adapter.OpenClaw {
			if o.Reason != "not installed" {
				t.Errorf("openclaw reason = %q", o.Reason)
			}
			return
		}
	}
	t.Fatal("no openclaw origin")
}

// A `mcp list --json` answer beats the config file, because it is the runtime's
// own view of its configuration.
func TestCommandOutputWinsOverConfigFile(t *testing.T) {
	env := mcpEnv()
	env.Exec.(*FakeExec).Responses = map[string]Result{
		Key("claude", "mcp", "list", "--json"): {
			Stdout: []byte(`{"mcpServers":{"live":{"command":"live-mcp"}}}`),
		},
	}
	rep := Detect(context.Background(), env, Options{SkipProcesses: true, Only: []adapter.Runtime{adapter.ClaudeCode}})
	inv := ReadMCP(context.Background(), env, rep)

	o := inv.Origins[0]
	if !o.Readable || o.Origin != "claude mcp list --json" {
		t.Fatalf("origin = %+v", o)
	}
	if len(o.Servers) != 1 || o.Servers[0].Name != "live" {
		t.Errorf("servers = %+v", o.Servers)
	}
	if o.FromFile != "" {
		t.Error("a command answer has no file to roll back")
	}
}

func TestServerKeyIgnoresName(t *testing.T) {
	a := MCPServer{Name: "github", Command: "gh-mcp", Args: []string{"serve"}}
	b := MCPServer{Name: "gh", Command: "gh-mcp", Args: []string{"serve"}}
	if a.Key() != b.Key() {
		t.Error("the same command under two names is one server")
	}
	c := MCPServer{Name: "gh", Command: "gh-mcp", Args: []string{"serve", "--all"}}
	if a.Key() == c.Key() {
		t.Error("different args are different servers")
	}
	u := MCPServer{Name: "x", URL: "https://example/mcp/"}
	if u.Key() != (MCPServer{URL: "https://example/mcp"}).Key() {
		t.Error("a trailing slash is not a different server")
	}
}

func TestToAdapterCarriesEnvDeterministically(t *testing.T) {
	s := MCPServer{Name: "gmail", Command: "npx", Args: []string{"-y", "gmail-mcp"},
		Env: map[string]string{"B": "2", "A": "1"}}
	got := s.ToAdapter()
	if len(got.Env) != 2 || got.Env[0] != "A=1" || got.Env[1] != "B=2" {
		t.Errorf("env = %v, want sorted K=V", got.Env)
	}
}
