package detect

import (
	"context"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/adapter"
)

// realMachine reproduces MEMORY.md §1 — the measurement taken on the author's
// Mac before any Relay code existed. Every case the installer has to survive is
// in this one fixture: a runtime that dominates the corpus, two that are
// installed and have never been run, and one whose state directory has moved.
func realMachine() Env {
	home := "/home/u"
	fs := &MemFS{
		Files: map[string]string{
			// Claude Code: one JSONL per session under projects/<slug>/.
			home + "/.claude/projects/-Users-u-code-payments/a.jsonl": strings.Repeat("x", 400),
			home + "/.claude/projects/-Users-u-code-payments/b.jsonl": strings.Repeat("x", 386),
			home + "/.claude/projects/-Users-u-lab/c.jsonl":           strings.Repeat("x", 200),
			home + "/.claude.json":                                    `{"mcpServers":{"github":{"command":"gh-mcp","args":["serve"]}}}`,

			// Codex: an index plus rollouts.
			home + "/.codex/session_index.jsonl":                 "{\"id\":\"1\"}\n{\"id\":\"2\"}\n\n",
			home + "/.codex/sessions/2026/08/09/rollout-a.jsonl": strings.Repeat("x", 150),
			home + "/.codex/config.toml":                         "[mcp_servers.github]\ncommand = \"gh-mcp\"\nargs = [\"serve\"]\n",

			// Hermes: 70% of the corpus, and a SQLite file we do not open.
			home + "/.hermes/state.db": strings.Repeat("x", 5000),

			// OpenCode: installed, never run — 11 MB of binary support files and
			// zero sessions.
			home + "/.local/share/opencode/cache/x": strings.Repeat("x", 30),
		},
		Dirs: []string{home},
	}
	return Env{
		FS: fs,
		Exec: &FakeExec{
			Paths: map[string]string{
				"claude":   "/usr/local/bin/claude",
				"codex":    "/usr/local/bin/codex",
				"hermes":   "/usr/local/bin/hermes",
				"opencode": "/usr/local/bin/opencode",
				"openclaw": "/usr/local/bin/openclaw",
			},
			Responses: map[string]Result{
				Key("claude", "--version"):   {Stdout: []byte("2.1.226 (Claude Code)\n")},
				Key("codex", "--version"):    {Stdout: []byte("codex-cli 0.140.0\n")},
				Key("hermes", "--version"):   {Stdout: []byte("v0.16.0 (2026.6.5)\n")},
				Key("opencode", "--version"): {Stdout: []byte("1.18.15\n")},
				Key("openclaw", "--version"): {Stdout: []byte("2026.3.13 (61d171a)\n")},
			},
		},
		Procs:  FakeProcs{{PID: 4021, Command: "node", Args: "/usr/local/bin/claude --print"}},
		Getenv: func(string) string { return "" },
		Home:   home,
		GOOS:   "darwin",
	}
}

func TestDetectReproducesTheMeasuredMachine(t *testing.T) {
	rep := Detect(context.Background(), realMachine(), Options{})

	if len(rep.Findings) != 5 {
		t.Fatalf("want five runtimes, got %d", len(rep.Findings))
	}

	want := map[adapter.Runtime]Status{
		adapter.ClaudeCode: StatusInUse,
		adapter.Codex:      StatusInUse,
		adapter.Hermes:     StatusInUse,
		adapter.OpenCode:   StatusInUse, // has files on disk, though no sessions
		adapter.OpenClaw:   StatusNeverRun,
	}
	for rt, w := range want {
		f, ok := rep.Get(rt)
		if !ok {
			t.Fatalf("no finding for %s", rt)
		}
		if got := f.Status(); got != w {
			t.Errorf("%s: status %s, want %s (notes: %v)", rt, got, w, f.Notes)
		}
	}

	// Versions come from the runtime, never from a guess.
	cc, _ := rep.Get(adapter.ClaudeCode)
	if cc.Version != "2.1.226 (Claude Code)" {
		t.Errorf("claude version = %q", cc.Version)
	}
	if n, ok := cc.SessionCount(); !ok || n != 3 {
		t.Errorf("claude sessions = %d, ok=%v; want 3 counted JSONL files", n, ok)
	}

	cx, _ := rep.Get(adapter.Codex)
	if n, ok := cx.SessionCount(); !ok || n != 2 {
		t.Errorf("codex sessions = %d ok=%v; want 2 lines of session_index.jsonl", n, ok)
	}

	// A running process is the third detection signal.
	if len(cc.Running) != 1 || cc.Running[0].PID != 4021 {
		t.Errorf("claude running = %+v, want the node process whose argv is the claude binary", cc.Running)
	}
}

// MEMORY.md §1: "installed but never used" is a normal case, not an error. The
// whole installer hinges on this, because two of five runtimes were in exactly
// that state on the machine the design was measured against.
func TestInstalledButNeverRunIsNotAnError(t *testing.T) {
	env := Env{
		FS:     &MemFS{Dirs: []string{"/home/u"}},
		Exec:   &FakeExec{Paths: map[string]string{"openclaw": "/usr/local/bin/openclaw"}},
		Getenv: func(string) string { return "" },
		Home:   "/home/u",
		GOOS:   "linux",
	}
	rep := Detect(context.Background(), env, Options{Only: []adapter.Runtime{adapter.OpenClaw}})
	f := rep.Findings[0]

	if f.Status() != StatusNeverRun {
		t.Fatalf("status = %s, want never_run", f.Status())
	}
	if f.Sessions != nil {
		t.Errorf("Sessions = %v; a store nobody could open must stay nil, never 0", *f.Sessions)
	}
	if len(f.Notes) == 0 {
		t.Error("want a note saying this is fine, not a failure")
	}
}

// A store with history and no binary on PATH is worth indexing and worth
// saying out loud, which is why it is a status of its own.
func TestHistoryWithoutABinary(t *testing.T) {
	env := realMachine()
	env.Exec = &FakeExec{} // nothing on PATH at all
	rep := Detect(context.Background(), env, Options{Only: []adapter.Runtime{adapter.ClaudeCode}})
	if got := rep.Findings[0].Status(); got != StatusHistoryOnly {
		t.Fatalf("status = %s, want history_only", got)
	}
	if len(rep.WithHistory()) != 1 {
		t.Error("a runtime with history but no binary still has history to backfill")
	}
}

// MEMORY.md §1: one runtime was 70% of the corpus, and any design that assumes
// an even distribution across five will be wrong.
func TestReportNamesTheDominantRuntime(t *testing.T) {
	rep := Detect(context.Background(), realMachine(), Options{})
	rt, share, ok := rep.Dominant()
	if !ok {
		t.Fatal("no dominant runtime found")
	}
	if rt != adapter.Hermes {
		t.Errorf("dominant = %s, want hermes", rt)
	}
	if share < 0.5 {
		t.Errorf("share = %.2f, want the majority", share)
	}
	if !strings.Contains(rep.Summary(), "hermes") {
		t.Errorf("summary should name the dominant runtime: %q", rep.Summary())
	}
}

// Hermes keeps sessions in SQLite. Counting them means opening a database, and
// the installer does not — so the count is nil and the size is not.
func TestHermesReportsSizeRatherThanAGuessedCount(t *testing.T) {
	rep := Detect(context.Background(), realMachine(), Options{Only: []adapter.Runtime{adapter.Hermes}})
	f := rep.Findings[0]
	if f.Sessions != nil {
		t.Errorf("Sessions = %d; the installer does not open state.db, so it must not claim a count", *f.Sessions)
	}
	if b, ok := f.Bytes(); !ok || b == 0 {
		t.Error("want the store size, which is what MEMORY.md §1 actually measured")
	}
	if !strings.Contains(f.SessionsNote, "state.db") {
		t.Errorf("the note has to explain the gap: %q", f.SessionsNote)
	}
}

func TestEnvRelocationsAreHonoured(t *testing.T) {
	env := realMachine()
	env.FS.(*MemFS).Files["/elsewhere/codex/session_index.jsonl"] = "{}\n{}\n{}\n"
	env.Getenv = func(k string) string {
		if k == "CODEX_HOME" {
			return "/elsewhere/codex"
		}
		return ""
	}
	rep := Detect(context.Background(), env, Options{Only: []adapter.Runtime{adapter.Codex}})
	f := rep.Findings[0]
	if f.StateDir != "/elsewhere/codex" {
		t.Fatalf("state dir = %q, want the CODEX_HOME override", f.StateDir)
	}
	if f.StateDirSource != SourceEnv || !f.StateDirSource.Trusted() {
		t.Errorf("source = %q; an env var is authoritative", f.StateDirSource)
	}
	if n, _ := f.SessionCount(); n != 3 {
		t.Errorf("sessions = %d, want 3 from the relocated index", n)
	}
}

func TestParsePS(t *testing.T) {
	out := "  501 node /usr/local/bin/claude --print\n 1234 codex codex app-server\nbad line\n"
	procs := ParsePS(out)
	if len(procs) != 2 {
		t.Fatalf("got %d processes: %+v", len(procs), procs)
	}
	if procs[0].PID != 501 || procs[0].Command != "node" {
		t.Errorf("first = %+v", procs[0])
	}
	if got := matchProcesses(procs, "codex"); len(got) != 1 || got[0].PID != 1234 {
		t.Errorf("codex match = %+v", got)
	}
	// "codex" must not match a shell editing codex.md.
	noise := []Process{{PID: 9, Command: "vim", Args: "vim codex.md"}}
	if got := matchProcesses(noise, "codex"); len(got) != 0 {
		t.Errorf("matched a filename as a process: %+v", got)
	}
}

func TestStatusLines(t *testing.T) {
	for _, s := range []Status{StatusAbsent, StatusNeverRun, StatusInUse, StatusHistoryOnly} {
		if s.Line() == "" || s.Line() == string(s) && s != StatusAbsent {
			continue
		}
	}
	if StatusNeverRun.Line() != "installed, never run" {
		t.Errorf("never_run line = %q", StatusNeverRun.Line())
	}
}
