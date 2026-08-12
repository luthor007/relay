package backfill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/detect"
	"github.com/luthor007/relay/relayd/internal/index"
)

func fixtureFile(t *testing.T, parts ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{testdata(t, "opencode")}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func openCodeExec(t *testing.T) *detect.FakeExec {
	t.Helper()
	return &detect.FakeExec{
		Paths: map[string]string{"opencode": "/usr/local/bin/opencode"},
		Responses: map[string]detect.Result{
			detect.Key("opencode", "sessions", "list", "--json"): {Stdout: fixtureFile(t, "sessions-list.json")},
			detect.Key("opencode", "export", "ses_01H9", "--sanitize"): {
				Stdout: fixtureFile(t, "export-ses_01H9.json"),
			},
		},
	}
}

func openCodeReader(t *testing.T, exec detect.Exec) *OpenCode {
	t.Helper()
	env := fixtureEnv(t)
	env.Exec = exec
	o := NewOpenCode(env)
	o.Dir = testdata(t, "opencode")
	return o
}

func TestOpenCodeScanRecordsWhichCommandAnswered(t *testing.T) {
	o := openCodeReader(t, openCodeExec(t))
	res, err := o.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StoreOK || len(res.Refs) != 1 {
		t.Fatalf("status %q with %d refs: %v", res.Status, len(res.Refs), res.Notes)
	}
	notes := strings.Join(res.Notes, "\n")
	if !strings.Contains(notes, "opencode sessions list --json") {
		t.Errorf("the enumeration command was not recorded: %v", res.Notes)
	}
	if !strings.Contains(notes, "guess") {
		t.Errorf("an unprobed command must be labelled a guess: %v", res.Notes)
	}
	if !strings.HasSuffix(res.Refs[0].Path, "storage/session/ses_01H9.json") {
		t.Errorf("the pointer should prefer the file on disk, got %q", res.Refs[0].Path)
	}
}

func TestOpenCodeReadAlwaysSanitizes(t *testing.T) {
	exec := openCodeExec(t)
	o := openCodeReader(t, exec)
	scan, _ := o.Scan(context.Background())

	s, err := o.Read(context.Background(), scan.Refs[0])
	if err != nil {
		t.Fatal(err)
	}

	var sawSanitize bool
	for _, c := range exec.Calls {
		if len(c.Args) > 0 && c.Args[0] == "export" {
			for _, a := range c.Args {
				if a == "--sanitize" {
					sawSanitize = true
				}
			}
		}
	}
	if !sawSanitize {
		t.Fatal("export must always pass --sanitize: it is a redaction primitive that already exists (MEMORY.md §4, §6)")
	}

	if s.Title != "port the connector tests to vitest" || s.TitleSource != index.TitleStored {
		t.Errorf("title %q from %q", s.Title, s.TitleSource)
	}
	if s.Workspace != "/home/user/src/relay/connector" || s.GitBranch != "main" {
		t.Errorf("%+v", s)
	}
	if s.Model != "claude-sonnet-4" {
		t.Errorf("model %q", s.Model)
	}
	if s.Messages != 3 || s.ToolCalls != 1 {
		t.Errorf("counts %d/%d", s.Messages, s.ToolCalls)
	}
	if s.CostUSD == nil || *s.CostUSD != 0.0812 {
		t.Errorf("cost %v", s.CostUSD)
	}
	if s.TokensTotal == nil || *s.TokensTotal != 9000 {
		t.Errorf("tokens %v", s.TokensTotal)
	}
	if !strings.Contains(s.Text, "still runs on jest") || !strings.Contains(s.Text, "18 tests pass") {
		t.Errorf("messages not extracted from either content or parts: %q", s.Text)
	}
	if !strings.Contains(strings.Join(s.Notes, "\n"), "--sanitize") {
		t.Errorf("the sanitize step should be visible in the notes: %v", s.Notes)
	}
}

// The measured case: installed, 11 MB, zero sessions. Success.
func TestOpenCodeEmptyListIsSuccess(t *testing.T) {
	exec := &detect.FakeExec{
		Paths: map[string]string{"opencode": "/usr/local/bin/opencode"},
		Responses: map[string]detect.Result{
			detect.Key("opencode", "sessions", "list", "--json"): {Stdout: []byte("[]")},
		},
	}
	o := NewOpenCode(withExec(fixtureEnv(t), exec))
	o.Dir = filepath.Join(t.TempDir(), "no-storage")

	res, err := o.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StoreEmpty {
		t.Fatalf("status %q, want empty", res.Status)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "installed, never run") {
		t.Errorf("the measured case should be named: %v", res.Notes)
	}
}

func TestOpenCodeNotInstalledIsAbsent(t *testing.T) {
	o := NewOpenCode(withExec(fixtureEnv(t), &detect.FakeExec{}))
	res, err := o.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StoreAbsent {
		t.Fatalf("status %q", res.Status)
	}
}

// The failure this reader exists to avoid: the binary is there, nothing
// answers, and we report an empty history as though it were a clean install.
func TestOpenCodeUnaskableStoreIsUnreadableNotEmpty(t *testing.T) {
	exec := &detect.FakeExec{Paths: map[string]string{"opencode": "/usr/local/bin/opencode"}}
	o := NewOpenCode(withExec(fixtureEnv(t), exec))
	o.Dir = filepath.Join(t.TempDir(), "nothing")

	res, err := o.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StoreUnreadable {
		t.Fatalf("status %q — no enumeration command answered, so this is not an empty history", res.Status)
	}
	if len(res.Roots) < len(enumerationCommands) {
		t.Errorf("every command tried must be reported: %v", res.Roots)
	}
	notes := strings.Join(res.Notes, "\n")
	for _, args := range enumerationCommands {
		if !strings.Contains(notes, strings.Join(args, " ")) {
			t.Errorf("command %v missing from the notes", args)
		}
	}
}

// When nothing answers but the storage directory is readable, use it — and say
// that the layout is a guess.
func TestOpenCodeFallsBackToStorageLayout(t *testing.T) {
	exec := &detect.FakeExec{Paths: map[string]string{"opencode": "/usr/local/bin/opencode"}}
	o := openCodeReader(t, exec)

	res, err := o.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StoreOK || len(res.Refs) != 1 || res.Refs[0].SessionID != "ses_01H9" {
		t.Fatalf("status %q, refs %+v", res.Status, res.Refs)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "has not been probed") {
		t.Errorf("the unprobed layout was not disclosed: %v", res.Notes)
	}
}

func withExec(env Env, e detect.Exec) Env {
	env.Exec = e
	return env
}
