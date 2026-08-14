package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/vault"
)

// The CLI is thin on purpose — the flow it drives is tested in internal/install
// against fixtures. What matters here is that every documented command exists,
// that the read-only ones touch nothing, and that `--json` is a shape somebody
// can script against.

// TestMain keeps this package's tests off the machine's real OS keychain; see
// the same seam in cmd/relayd for the reasoning. `relay backfill` opens the
// vault on every run, so without this a developer running `go test ./...` on a
// Mac gets a system dialog and a suite that waits for them to answer it.
func TestMain(m *testing.M) {
	vaultOpen = func(ctx context.Context, opts vault.Options) (vault.Vault, error) {
		opts.Keyring = &vault.MemoryKeyring{FailAll: true}
		return vault.Open(ctx, opts)
	}
	os.Exit(m.Run())
}

func TestEveryDocumentedCommandExists(t *testing.T) {
	// Every command named in the usage block must be reachable. A command that
	// only exists in the help text is worse than one that does not exist.
	for _, line := range strings.Split(usage, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "relay ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		cmd := fields[1]
		var out, errb bytes.Buffer
		err := run(context.Background(), []string{cmd, "--help"}, &out, &errb)
		if err != nil && strings.Contains(err.Error(), "unknown command") {
			t.Errorf("usage documents %q and run() does not know it", cmd)
		}
	}
}

func TestUnknownCommandPrintsUsage(t *testing.T) {
	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"nope"}, &out, &errb)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(errb.String(), "relay setup") {
		t.Errorf("stderr should carry the usage:\n%s", errb.String())
	}
}

func TestVersionAndHelp(t *testing.T) {
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"version"}, &out, &errb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "relay") {
		t.Errorf("version = %q", out.String())
	}

	out.Reset()
	if err := run(context.Background(), []string{"help"}, &out, &errb); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"relay setup", "relay detect", "relay doctor", "relay mcp",
		"relay embed", "relay reindex"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("usage is missing %q", want)
		}
	}
}

// `relay detect` reads the machine and changes nothing. It has to work on a
// box with none of the five runtimes installed — which is this one.
// bareMachine makes the host look like a fresh container: no runtimes on PATH,
// an empty HOME, and Ollama pointed at a port nothing listens on.
//
// Without this the test asserts the state of whatever machine ran it. It passed
// in the Linux sandbox it was written in and failed on the author's Mac, where
// all five runtimes and Ollama are installed — a test that only holds on one
// machine is not testing the code.
func bareMachine(t *testing.T) {
	t.Helper()
	empty := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", empty)
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:1")
	// The runtimes each honour their own relocation variable. Point them at a
	// path that does not exist, not merely an empty one: "the directory is not
	// there" and "the directory is there and holds nothing" are different states,
	// and only the first is what a fresh container looks like.
	gone := filepath.Join(empty, "does-not-exist")
	for _, v := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "OPENCLAW_STATE_DIR"} {
		t.Setenv(v, gone)
	}
}

func TestDetectRunsOnABareMachine(t *testing.T) {
	bareMachine(t)
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"detect"}, &out, &errb); err != nil {
		t.Fatalf("detect must never fail because nothing is installed: %v", err)
	}
	s := out.String()
	for _, rt := range []string{"Claude Code", "Codex", "Hermes", "OpenCode", "OpenClaw"} {
		if !strings.Contains(s, rt) {
			t.Errorf("detect output is missing %q:\n%s", rt, s)
		}
	}
	if !strings.Contains(s, "not installed") {
		t.Errorf("on a container with no runtimes, that is the expected answer:\n%s", s)
	}
}

func TestDetectJSONIsScriptable(t *testing.T) {
	bareMachine(t)
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"detect", "--json"}, &out, &errb); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Summary  string `json:"summary"`
		Findings []struct {
			Runtime         string `json:"runtime"`
			Status          string `json:"status"`
			Sessions        *int   `json:"sessions"`
			StateDirTrusted bool   `json:"state_dir_trusted"`
		} `json:"findings"`
		Ollama struct {
			Status     string   `json:"status"`
			Installed  bool     `json:"installed"`
			Running    bool     `json:"running"`
			Host       string   `json:"host"`
			HostSource string   `json:"host_source"`
			Models     []string `json:"models"`
		} `json:"ollama"`
		Embedding struct {
			Configured bool   `json:"configured"`
			Provider   string `json:"provider"`
			Model      string `json:"model"`
			Local      bool   `json:"local"`
			Pulled     *bool  `json:"pulled"`
		} `json:"embedding"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	if len(doc.Findings) != 5 {
		t.Fatalf("want five runtimes, got %d", len(doc.Findings))
	}
	for _, f := range doc.Findings {
		if f.Status == "" {
			t.Errorf("%s has no status", f.Runtime)
		}
		// nil, not 0: a store nobody opened has no count, and JSON must carry
		// that difference to whatever reads it.
		if f.Sessions != nil && f.Status == "absent" {
			t.Errorf("%s is absent and reports %d sessions", f.Runtime, *f.Sessions)
		}
	}

	// ORCHESTRATOR.md §2c: the embedding model is part of what is on this
	// machine, and `relay detect --json` is the first thing a support
	// conversation asks for.
	if doc.Ollama.Status == "" {
		t.Error("detect --json must report the local embedding runtime")
	}
	if doc.Ollama.Host == "" || doc.Ollama.HostSource == "" {
		t.Errorf("ollama = %+v; the host and how we know it both matter", doc.Ollama)
	}
	// This container has no Ollama, so the honest answer is absent — and, more
	// importantly, "pulled" must be nil rather than false, because nobody could
	// ask. Same rule detect.Finding follows for a store it never opened.
	if doc.Ollama.Installed || doc.Ollama.Running {
		t.Errorf("ollama = %+v on a machine with none", doc.Ollama)
	}
	if doc.Embedding.Model == "" {
		t.Error("detect --json must name the configured embedding model")
	}
	if doc.Embedding.Local && doc.Embedding.Pulled != nil {
		t.Errorf("pulled = %v, want nil when the runtime could not be asked", *doc.Embedding.Pulled)
	}
}

// `relay status` names the embedder, because "search got worse" is a support
// question and the first thing to establish is whether one is configured.
func TestStatusNamesTheEmbedder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(
		"listen = \"127.0.0.1:8787\"\n\n[embedding]\n  provider = \"none\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"status", "--config", path}, &out, &errb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "keyword-only") {
		t.Errorf("status = %q", out.String())
	}
}

// `relay reindex` on a box where relayd has never run must not create a
// database as a side effect of being asked a question.
func TestReindexWithoutAnIndexCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RELAY_CONFIG_DIR", dir)
	t.Setenv("RELAY_DATA_DIR", dir)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"reindex"}, &out, &errb); err != nil {
		t.Fatalf("reindex on a fresh box is not an error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "relay.db")); err == nil {
		t.Error("reindex created a database just by being run")
	}
	if !strings.Contains(out.String(), "nothing to re-embed") {
		t.Errorf("out = %q", out.String())
	}
}

func TestStatusReadsAConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	var out, errb bytes.Buffer
	// A missing config file is not an error: a fresh box runs on defaults.
	if err := run(context.Background(), []string{"status", "--config", path}, &out, &errb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "127.0.0.1:8787") {
		t.Errorf("status = %q", out.String())
	}
}

// `relay pair` prints what the phone needs, and says so plainly when there is
// nothing to print.
//
// It used to print a six-character code — with a dash in it, which is what the
// old assertion checked — that nothing on either side has ever verified. A test
// that passes on a costume is worse than no test.
func TestPairSaysWhatIsMissingRatherThanInventingACode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_DATA_HOME", dir)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"pair"}, &out, &errb); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "has not started relayd yet") {
		t.Errorf("pair on a machine with no identity = %q", s)
	}
	// And it must not print something that looks like a credential.
	if strings.Contains(s, "relay://") {
		t.Errorf("invented a pairing link out of nothing:\n%s", s)
	}
}

func TestMCPRollbackNeedsAManifest(t *testing.T) {
	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"mcp", "rollback"}, &out, &errb)
	if err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Errorf("err = %v", err)
	}
}

func TestServiceRejectsAnUnknownAction(t *testing.T) {
	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"service", "restart"}, &out, &errb)
	if err == nil || !strings.Contains(err.Error(), "unknown action") {
		t.Errorf("err = %v", err)
	}
}
