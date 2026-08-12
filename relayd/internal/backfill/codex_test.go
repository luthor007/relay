package backfill

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/index"
)

func codexReader(t *testing.T) *Codex {
	t.Helper()
	c := NewCodex(fixtureEnv(t))
	c.Dir = testdata(t, "codex")
	return c
}

func TestCodexScanWalksTheRolloutTree(t *testing.T) {
	res, err := codexReader(t).Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StoreOK {
		t.Fatalf("status %q: %v", res.Status, res.Notes)
	}
	if len(res.Refs) != 2 {
		t.Fatalf("%d rollouts, want 2 (the tree is the authority, not the index)", len(res.Refs))
	}
	for _, r := range res.Refs {
		if !isUUID(r.SessionID) {
			t.Errorf("session id %q is not the uuid from rollout-<iso>-<uuid>.jsonl", r.SessionID)
		}
		if filepath.Ext(r.Path) != ".jsonl" {
			t.Errorf("path %q", r.Path)
		}
	}
	// The malformed session_index.jsonl line must not break the scan; hints
	// are hints.
	if !strings.Contains(strings.Join(res.Notes, "\n"), "hints only") {
		t.Errorf("the bad index line was not disclosed: %v", res.Notes)
	}
}

func TestCodexReadParsesTheFourLineTypes(t *testing.T) {
	c := codexReader(t)
	scan, _ := c.Scan(context.Background())
	s, err := c.Read(context.Background(), refByID(t, scan.Refs, "9b7d4e2a-1c33-4f80-a1b2-cc0011223344"))
	if err != nil {
		t.Fatal(err)
	}

	// session_meta
	if s.Workspace != "/home/user/src/relay" {
		t.Errorf("cwd %q", s.Workspace)
	}
	if s.GitBranch != "feat/codex-adapter" {
		t.Errorf("branch %q — session_meta.git is the probed source", s.GitBranch)
	}
	// turn_context
	if s.Model != "gpt-5-codex" {
		t.Errorf("model %q", s.Model)
	}
	// response_item
	if s.ToolCalls != 1 {
		t.Errorf("tool calls %d, want the one function_call", s.ToolCalls)
	}
	if s.Messages != 2 {
		t.Errorf("messages %d, want the two response_item messages", s.Messages)
	}
	if !strings.Contains(s.Text, "JSONRPCMessage is untagged") {
		t.Errorf("assistant text missing: %q", s.Text)
	}
	if !strings.Contains(s.Text, "no matches") {
		t.Error("function_call_output must be extracted: command output is where credentials surface")
	}
	// event_msg / token_count
	if s.TokensTotal == nil || *s.TokensTotal != 19625 {
		t.Errorf("tokens %v, want the last cumulative total_token_usage", s.TokensTotal)
	}
	if s.CostUSD != nil {
		t.Errorf("cost %v — Codex records tokens and never currency", *s.CostUSD)
	}
	if !strings.Contains(strings.Join(s.Notes, "\n"), "never a currency amount") {
		t.Errorf("the absence of cost was not explained: %v", s.Notes)
	}

	// No title exists anywhere in a rollout, so it must be derived and said so.
	if s.TitleSource != index.TitleFirstMessage {
		t.Errorf("title source %q", s.TitleSource)
	}
	if !strings.HasPrefix(s.Title, "demux the app-server NDJSON") {
		t.Errorf("title %q", s.Title)
	}
	if s.StartedAt.IsZero() || !s.EndedAt.After(s.StartedAt) {
		t.Errorf("times %v → %v", s.StartedAt, s.EndedAt)
	}
}

func TestCodexReportsUnrecognisedLineTypes(t *testing.T) {
	c := codexReader(t)
	scan, _ := c.Scan(context.Background())
	s, err := c.Read(context.Background(), refByID(t, scan.Refs, "5511aa77-88bb-4cc0-9def-0123456789ab"))
	if err != nil {
		t.Fatal(err)
	}
	notes := strings.Join(s.Notes, "\n")
	if !strings.Contains(notes, "unrecognised kind \"compacted\"") {
		t.Errorf("an unknown line type was swallowed: %v", s.Notes)
	}
	if !strings.Contains(notes, "rollout format may have moved") {
		t.Errorf("the note should say what an unknown type implies: %v", s.Notes)
	}
	if s.TokensTotal != nil {
		t.Errorf("tokens %v — this rollout has no token_count", *s.TokensTotal)
	}
	if s.Messages != 1 {
		t.Errorf("messages %d — no response_item messages, so the user_message events are counted", s.Messages)
	}
}

func TestCodexAbsentStoreIsNotAnError(t *testing.T) {
	c := NewCodex(fixtureEnv(t))
	c.Dir = filepath.Join(t.TempDir(), "no-codex")
	res, err := c.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StoreAbsent || len(res.Refs) != 0 {
		t.Fatalf("status %q with %d refs", res.Status, len(res.Refs))
	}
}

// detect.go already found that a machine can have rollouts and no index. A
// reader that trusted session_index.jsonl would skip them.
func TestCodexWorksWithoutASessionIndex(t *testing.T) {
	dir := t.TempDir()
	src := testdata(t, "codex")
	copyTree(t, filepath.Join(src, "sessions"), filepath.Join(dir, "sessions"))

	c := NewCodex(fixtureEnv(t))
	c.Dir = dir
	res, err := c.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StoreOK || len(res.Refs) != 2 {
		t.Fatalf("status %q with %d refs: %v", res.Status, len(res.Refs), res.Notes)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "no session_index.jsonl") {
		t.Errorf("the missing index was not mentioned: %v", res.Notes)
	}
}

func TestRolloutIDTakesTheUUIDNotTheStamp(t *testing.T) {
	got := rolloutID("rollout-2026-08-09T14-22-05-9b7d4e2a-1c33-4f80-a1b2-cc0011223344.jsonl")
	if got != "9b7d4e2a-1c33-4f80-a1b2-cc0011223344" {
		t.Fatalf("id %q", got)
	}
	if got := rolloutID("something-else.jsonl"); got != "something-else" {
		t.Fatalf("fallback id %q", got)
	}
}
