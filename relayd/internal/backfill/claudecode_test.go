package backfill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/index"
)

func fixtureEnv(t *testing.T) Env {
	t.Helper()
	return Env{
		Home:   t.TempDir(),
		Getenv: func(string) string { return "" },
		GOOS:   "linux",
	}
}

func claudeReader(t *testing.T) *ClaudeCode {
	t.Helper()
	r := NewClaudeCode(fixtureEnv(t))
	r.Dir = testdata(t, "claudecode")
	return r
}

func testdata(t *testing.T, parts ...string) string {
	t.Helper()
	p := filepath.Join(append([]string{"..", "..", "testdata", "backfill"}, parts...)...)
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
	return abs
}

func refByID(t *testing.T, refs []Ref, id string) Ref {
	t.Helper()
	for _, r := range refs {
		if r.SessionID == id {
			return r
		}
	}
	t.Fatalf("no ref for %s in %d refs", id, len(refs))
	return Ref{}
}

func TestClaudeCodeScanFindsOneSessionPerFile(t *testing.T) {
	res, err := claudeReader(t).Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StoreOK {
		t.Fatalf("status %q, notes %v", res.Status, res.Notes)
	}
	if len(res.Refs) != 3 {
		t.Fatalf("found %d sessions, want 3", len(res.Refs))
	}
	for _, r := range res.Refs {
		if r.Runtime != adapter.ClaudeCode {
			t.Errorf("runtime %q", r.Runtime)
		}
		if r.MTimeFrom != "file mtime" {
			t.Errorf("%s: resume key is %q; one file is one session here", r.SessionID, r.MTimeFrom)
		}
		if r.Size == 0 || r.MTime.IsZero() {
			t.Errorf("%s: incomplete resume key %+v", r.SessionID, r)
		}
		if !strings.HasSuffix(r.Path, r.SessionID+".jsonl") {
			t.Errorf("%s: path %q does not point at its own transcript", r.SessionID, r.Path)
		}
	}
}

func TestClaudeCodeReadUsesTheRuntimesOwnTitle(t *testing.T) {
	r := claudeReader(t)
	scan, err := r.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ref := refByID(t, scan.Refs, "3f2b1c9e-0a4d-4c11-9d2e-77c0b5a41f00")

	s, err := r.Read(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}

	if s.Title != "wire the Stripe webhook into relayd" {
		t.Errorf("title %q — aiTitle is free metadata and must win", s.Title)
	}
	if !s.TitleSource.Generated() {
		t.Errorf("title source %q; the runtime wrote this one, so the summariser can skip it", s.TitleSource)
	}
	if s.Workspace != "/home/user/src/relay" {
		t.Errorf("workspace %q", s.Workspace)
	}
	if s.GitBranch != "main" {
		t.Errorf("branch %q", s.GitBranch)
	}
	if s.Model != "claude-opus-4-20250514" {
		t.Errorf("model %q", s.Model)
	}
	if s.Messages != 4 {
		t.Errorf("messages %d, want 4", s.Messages)
	}
	if s.ToolCalls != 1 {
		t.Errorf("tool calls %d, want the one Read block", s.ToolCalls)
	}
	if s.StartedAt.IsZero() || !s.EndedAt.After(s.StartedAt) {
		t.Errorf("times %v → %v", s.StartedAt, s.EndedAt)
	}
	if s.TokensTotal == nil || *s.TokensTotal != 71797 {
		t.Errorf("tokens %v, want the sum of every request's usage", s.TokensTotal)
	}
	if s.CostUSD != nil {
		t.Errorf("cost %v — Claude Code transcripts carry no currency, so this stays nil", *s.CostUSD)
	}
	if !strings.Contains(s.Text, "dropping events") || !strings.Contains(s.Text, "never verifies the signature") {
		t.Errorf("message text not extracted: %q", s.Text)
	}
	if !strings.Contains(s.Text, "func handle(") {
		t.Error("tool_result content must be extracted: it is where credentials most often appear")
	}
}

func TestClaudeCodeFallsBackToTheSummaryRecord(t *testing.T) {
	r := claudeReader(t)
	scan, _ := r.Scan(context.Background())
	s, err := r.Read(context.Background(), refByID(t, scan.Refs, "8c71aa02-55d3-4e18-b0a1-2f9e4d6c1b33"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Title != "index every session ever, no embeddings yet" {
		t.Errorf("title %q", s.Title)
	}
	if s.TitleSource != index.TitleGenerated {
		t.Errorf("title source %q", s.TitleSource)
	}
	if s.GitBranch != "feat/backfill" {
		t.Errorf("branch %q", s.GitBranch)
	}
	var sawSidechain bool
	for _, n := range s.Notes {
		if strings.Contains(n, "sidechain") {
			sawSidechain = true
		}
	}
	if !sawSidechain {
		t.Errorf("sub-agent messages were counted but not mentioned: %v", s.Notes)
	}
}

func TestClaudeCodeDerivesWhatItCannotRead(t *testing.T) {
	r := claudeReader(t)
	scan, _ := r.Scan(context.Background())
	s, err := r.Read(context.Background(), refByID(t, scan.Refs, "aa11bb22-cc33-dd44-ee55-ff6677889900"))
	if err != nil {
		t.Fatal(err)
	}

	if s.TitleSource != index.TitleFirstMessage {
		t.Errorf("title source %q — this one was derived and must say so", s.TitleSource)
	}
	if !strings.HasSuffix(s.Title, "…") {
		t.Errorf("long first message should be clipped: %q", s.Title)
	}
	if s.Workspace != "/home/user/src/osmo" {
		t.Errorf("workspace %q, want the slug decoded", s.Workspace)
	}
	if s.TokensTotal != nil {
		t.Errorf("tokens %v — no usage in this transcript, so nil, never zero", *s.TokensTotal)
	}

	notes := strings.Join(s.Notes, "\n")
	if !strings.Contains(notes, "decoded from the directory slug") {
		t.Errorf("the lossy slug decode was not disclosed: %v", s.Notes)
	}
	if !strings.Contains(notes, "did not parse as JSON") {
		t.Errorf("the malformed line was skipped silently: %v", s.Notes)
	}
	if s.Messages != 2 {
		t.Errorf("messages %d — the malformed line must not be counted, the other two must", s.Messages)
	}
}

// MEMORY.md §1: two of five runtimes had no data. An absent store is success.
func TestClaudeCodeAbsentStoreIsNotAnError(t *testing.T) {
	r := NewClaudeCode(fixtureEnv(t))
	r.Dir = filepath.Join(t.TempDir(), "nothing-here")

	res, err := r.Scan(context.Background())
	if err != nil {
		t.Fatalf("an absent store must not be an error: %v", err)
	}
	if res.Status != StoreAbsent {
		t.Errorf("status %q", res.Status)
	}
	if len(res.Refs) != 0 {
		t.Errorf("refs %d", len(res.Refs))
	}
	if len(res.Roots) == 0 || len(res.Notes) == 0 {
		t.Error("an empty result must come with where we looked")
	}
}

func TestClaudeCodeInstalledButNeverRun(t *testing.T) {
	dir := t.TempDir()
	r := NewClaudeCode(fixtureEnv(t))
	r.Dir = dir

	res, err := r.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StoreEmpty {
		t.Fatalf("status %q, want empty: the config dir exists with no projects/", res.Status)
	}
}

func TestClaudeCodeHonoursCLAUDECONFIGDIR(t *testing.T) {
	dir := testdata(t, "claudecode")
	env := fixtureEnv(t)
	env.Getenv = func(k string) string {
		if k == "CLAUDE_CONFIG_DIR" {
			return dir
		}
		return ""
	}
	res, err := NewClaudeCode(env).Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StoreOK || len(res.Refs) != 3 {
		t.Fatalf("CLAUDE_CONFIG_DIR was ignored: %q, %d refs", res.Status, len(res.Refs))
	}
}
