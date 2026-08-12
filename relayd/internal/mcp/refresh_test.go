package mcp_test

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/mcp"
)

type fakeSessions struct {
	live     []mcp.SessionInfo
	restarts []string
	err      error
}

func (f *fakeSessions) LiveSessions() []mcp.SessionInfo { return f.live }

func (f *fakeSessions) Restart(_ context.Context, id string) error {
	if f.err != nil {
		return f.err
	}
	f.restarts = append(f.restarts, id)
	return nil
}

func findSession(t *testing.T, res mcp.RefreshResult, id string) mcp.SessionRefresh {
	t.Helper()
	for _, s := range res.Sessions {
		if s.Session == id {
			return s
		}
	}
	t.Fatalf("no outcome for session %q in %+v", id, res.Sessions)
	return mcp.SessionRefresh{}
}

// ADAPTERS.md §2, verified against the recorded trace: `system/init` carries the
// whole tool list and is re-emitted at the head of every turn. So Claude Code
// needs nothing — and saying that is the answer, not restarting it.
func TestClaudeCodePicksItUpOnItsNextTurn(t *testing.T) {
	sessions := &fakeSessions{live: []mcp.SessionInfo{{ID: "s1", Runtime: "claude-code"}}}
	g := mcp.NewGateway(mcp.Options{Sessions: sessions})

	res := g.Refresh(context.Background(), "prusa connected")
	got := findSession(t, res, "s1")
	if got.Action != mcp.RefreshNextTurn {
		t.Fatalf("want next_turn, got %s (%s)", got.Action, got.Detail)
	}
	if !strings.Contains(got.Detail, "every turn") {
		t.Fatalf("the detail must say why nothing was done: %q", got.Detail)
	}
	if len(sessions.restarts) != 0 {
		t.Fatal("a session that re-enumerates every turn must not be restarted")
	}
	if res.NeedsUser() {
		t.Fatal("nothing is owed by the user here")
	}
}

// ADAPTERS.md §8 leaves loadSession unprobed on all three ACP runtimes, and
// registry.RestartPolicy refuses to substitute a fresh session for a resumed one
// by default for exactly that reason. A restart that silently loses the
// conversation to make a tool visible is worse than the invisible tool.
func TestACPSessionIsNotRestartedBehindTheUsersBack(t *testing.T) {
	sessions := &fakeSessions{live: []mcp.SessionInfo{
		{ID: "s2", Runtime: "openclaw", CanResume: false},
	}}
	g := mcp.NewGateway(mcp.Options{Sessions: sessions})

	res := g.Refresh(context.Background(), "prusa connected")
	got := findSession(t, res, "s2")
	if got.Action != mcp.RefreshManual {
		t.Fatalf("want manual, got %s", got.Action)
	}
	if len(sessions.restarts) != 0 {
		t.Fatalf("nothing may be restarted: %v", sessions.restarts)
	}
	if !strings.Contains(got.Detail, "forgotten the conversation") {
		t.Fatalf("the user has to be told why Relay did not do it: %q", got.Detail)
	}
	if !res.NeedsUser() {
		t.Fatal("this outcome is owed to the user and NeedsUser must say so")
	}
	if !strings.Contains(res.Note, "Restart") {
		t.Fatalf("the note must say what to do: %q", res.Note)
	}
}

func TestResumableSessionIsRestartedAndSaidSo(t *testing.T) {
	sessions := &fakeSessions{live: []mcp.SessionInfo{
		{ID: "s3", Runtime: "codex", CanResume: true},
	}}
	g := mcp.NewGateway(mcp.Options{Sessions: sessions})

	res := g.Refresh(context.Background(), "prusa connected")
	got := findSession(t, res, "s3")
	if got.Action != mcp.RefreshRestarted {
		t.Fatalf("want restarted, got %s (%s)", got.Action, got.Detail)
	}
	if len(sessions.restarts) != 1 || sessions.restarts[0] != "s3" {
		t.Fatalf("the session should have been restarted once: %v", sessions.restarts)
	}
	if !strings.Contains(res.Note, "restarted") {
		t.Fatalf("the note must name what happened: %q", res.Note)
	}
}

func TestRestartRefusalFallsBackToTellingTheUser(t *testing.T) {
	sessions := &fakeSessions{
		live: []mcp.SessionInfo{{ID: "s4", Runtime: "hermes", CanResume: true}},
		err:  mcp.ErrRestartUnavailable,
	}
	g := mcp.NewGateway(mcp.Options{Sessions: sessions})

	got := findSession(t, g.Refresh(context.Background(), "x"), "s4")
	if got.Action != mcp.RefreshManual {
		t.Fatalf("want manual, got %s", got.Action)
	}
	if !strings.Contains(got.Detail, "yours to do") {
		t.Fatalf("the detail must hand it back plainly: %q", got.Detail)
	}
}

// A connection that can be pushed to wins over everything else: the tools are
// there immediately, which is the good case the whole design is aiming at.
func TestPushBeatsEverythingElse(t *testing.T) {
	sessions := &fakeSessions{live: []mcp.SessionInfo{{ID: "s5", Runtime: "opencode"}}}
	g := mcp.NewGateway(mcp.Options{Sessions: sessions})

	defer stdioSession(t, g, "s5")()

	res := g.Refresh(context.Background(), "prusa connected")
	got := findSession(t, res, "s5")
	if got.Action != mcp.RefreshNotified {
		t.Fatalf("want notified, got %s (%s)", got.Action, got.Detail)
	}
	if len(sessions.restarts) != 0 {
		t.Fatal("a session that was told does not need restarting")
	}
	if !strings.Contains(res.Note, "told directly") {
		t.Fatalf("the note must say which was done: %q", res.Note)
	}
}

func TestNoSessionsIsStillAnAnswer(t *testing.T) {
	g := mcp.NewGateway(mcp.Options{Sessions: &fakeSessions{}})
	res := g.Refresh(context.Background(), "x")
	if len(res.Sessions) != 0 {
		t.Fatalf("no sessions, no outcomes: %+v", res.Sessions)
	}
	if !strings.Contains(res.Note, "No agent sessions are running") {
		t.Fatalf("silence is not an answer: %q", res.Note)
	}
}

// Every outcome is named in one sentence, so "say which it did" is a property of
// the note rather than of whoever renders it.
func TestNoteNamesEveryOutcome(t *testing.T) {
	sessions := &fakeSessions{live: []mcp.SessionInfo{
		{ID: "cc", Runtime: "claude-code"},
		{ID: "acp", Runtime: "hermes"},
		{ID: "cx", Runtime: "codex", CanResume: true},
	}}
	g := mcp.NewGateway(mcp.Options{Sessions: sessions})
	note := g.Refresh(context.Background(), "x").Note

	for _, want := range []string{"next turn", "restarted", "Restart"} {
		if !strings.Contains(note, want) {
			t.Fatalf("the note is missing %q: %s", want, note)
		}
	}
}

// stdioSession opens a stdio connection bound to a Relay session, which is the
// shape of a runtime Relay launched and pointed at the gateway. It returns a
// function that tears the connection down.
func stdioSession(t *testing.T, g *mcp.Gateway, session string) func() {
	t.Helper()
	clientR, clientW := io.Pipe()
	serverR, serverW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = g.ServeStdio(ctx, clientR, serverW) }()

	// Drain the server's output so a push never blocks the test.
	drained := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, serverR); close(drained) }()

	enc := json.NewEncoder(clientW)
	if err := enc.Encode(rpc(1, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "opencode"},
	})); err != nil {
		t.Fatal(err)
	}
	// A call is what carries _meta, and _meta is how a runtime on the shared
	// endpoint says which session it is. The tool does not have to exist.
	if err := enc.Encode(rpc(2, "tools/call", map[string]any{
		"name":  "nothing_at_all",
		"_meta": map[string]any{"relay/session": session},
	})); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		bound := false
		for _, c := range g.Connections() {
			if c.Session() == session && c.CanNotify() {
				bound = true
			}
		}
		if bound {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the stdio connection never bound to a session")
		}
		time.Sleep(2 * time.Millisecond)
	}

	return func() {
		cancel()
		_ = clientW.Close()
		_ = serverW.Close()
		<-drained
	}
}
