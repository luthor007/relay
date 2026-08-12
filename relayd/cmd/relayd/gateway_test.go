package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/audit"
	"github.com/luthor007/relay/relayd/internal/connector"
	"github.com/luthor007/relay/relayd/internal/mcp"
	"github.com/luthor007/relay/relayd/internal/orchestrator"
	"github.com/luthor007/relay/relayd/internal/store"
)

// rpc sends one JSON-RPC message to the running daemon's tool bus and returns
// the decoded reply.
func rpc(t *testing.T, base, method string, params any) map[string]any {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately no Authorization header. The gateway cannot take one — the
	// mcp.json entry Relay writes has no header field on any of the five
	// runtimes — so if this ever starts needing a token, every agent on the
	// machine loses its tools and this test is where that is noticed.
	req, _ := http.NewRequest("POST", base+"/mcp/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp/ %s: %v", method, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /mcp/ %s: http %d: %s", method, resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("POST /mcp/ %s: undecodable reply %s", method, raw)
	}
	if e, ok := out["error"]; ok {
		t.Fatalf("POST /mcp/ %s: %v", method, e)
	}
	return out
}

func initialize(t *testing.T, base string) map[string]any {
	t.Helper()
	return rpc(t, base, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "claude-code", "version": "test"},
	})
}

// TestTheToolBusIsServed is the regression test for the join that did not exist.
//
// internal/mcp was written, tested, and constructed by nothing: the gateway had
// no caller, so /mcp/ answered 404 and install.Options.Gateway stayed zero,
// which is why reconcileMCP enumerated five runtimes' servers and changed
// nothing. This fails if the daemon ever stops serving the bus.
func TestTheToolBusIsServed(t *testing.T) {
	base := startDaemon(t, t.TempDir())

	out := initialize(t, base)
	result, _ := out["result"].(map[string]any)
	info, _ := result["serverInfo"].(map[string]any)
	if name, _ := info["name"].(string); name != "relay" {
		t.Fatalf("the server at /mcp/ calls itself %q, so `relay setup` will refuse to adopt it", name)
	}
}

// TestTheToolBusGrantsNothingByDefault is ORCHESTRATOR.md §4b rule 1 observed
// from outside the process.
//
// The gateway takes no API token, so this is the load-bearing half of its
// security story: a process on this machine may connect and still sees nothing
// until a human has granted a connector. A tools/list that came back populated
// on a fresh install would mean the default had flipped from DenyAll, which is
// the difference between "no connectors" and "every agent on the machine can
// send mail as you".
func TestTheToolBusGrantsNothingByDefault(t *testing.T) {
	base := startDaemon(t, t.TempDir())
	initialize(t, base)

	out := rpc(t, base, "tools/list", map[string]any{})
	result, _ := out["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	if len(tools) != 0 {
		t.Fatalf("a fresh machine exposes %d tools on the bus; nothing is auto-granted", len(tools))
	}
}

// TestALoopbackCallerReachesTheBus covers this side of the guard; the refusal
// of an off-machine caller is exercised in internal/api, where RemoteAddr can
// be set. A header a caller controls must never be what decides whether it is
// trusted, so sending one here should change nothing.
func TestALoopbackCallerReachesTheBus(t *testing.T) {
	base := startDaemon(t, t.TempDir())

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	req, _ := http.NewRequest("POST", base+"/mcp/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a loopback caller was refused: http %d", resp.StatusCode)
	}
}

// TestASkillReachesTheBusOnceGranted is the whole point of the three joins.
//
// The orchestrator writes a playbook; the book is a provider on the bus; the
// bus is served on the endpoint `relay setup` writes into five runtimes' config
// files. This walks that path from the outside: grant the connector, then ask
// the bus what tools it has, as Claude Code would.
func TestASkillReachesTheBusOnceGranted(t *testing.T) {
	dir := t.TempDir()
	base := startDaemon(t, dir)
	initialize(t, base)

	// The skill is written through the same store the orchestrator's
	// author_skill tool writes to — a second handle on the same file, which is
	// how the console will do it too. Reaching into the daemon's own object
	// would prove the object works and not that the wiring does.
	authorSkill(t, dir)

	grantSkills(t, dir)

	// The daemon caches the book in memory and loads it at startup, so a skill
	// written by another handle is picked up on the next start. That is the
	// restart this test is really about: a playbook has to survive one.
	base = startDaemon(t, dir)
	initialize(t, base)

	out := rpc(t, base, "tools/list", map[string]any{})
	result, _ := out["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("after granting relay-skills the bus has %d tools, want 1", len(tools))
	}
	tool, _ := tools[0].(map[string]any)
	name, _ := tool["name"].(string)
	if !strings.Contains(name, "check_staging_health") {
		t.Errorf("the tool on the bus is %q", name)
	}
	desc, _ := tool["description"].(string)
	if !strings.Contains(desc, "Call this when") {
		t.Errorf("the trigger did not survive the trip to the bus: %q", desc)
	}

	// And calling it hands over instructions rather than doing anything. Relay
	// has no browser; the agent that called this does.
	out = rpc(t, base, "tools/call", map[string]any{
		"name": name, "arguments": map[string]any{"context": "release-2"},
	})
	result, _ = out["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatal("the tool returned nothing")
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	for _, want := range []string{"staging dashboard", "release-2"} {
		if !strings.Contains(text, want) {
			t.Errorf("the handed-over instructions lost %q:\n%s", want, text)
		}
	}
}

// authorSkill writes a playbook through the store, the way author_skill does.
func authorSkill(t *testing.T, dir string) {
	t.Helper()
	db, err := store.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	book, err := orchestrator.OpenSkillBook(t.Context(), orchestrator.SkillsIn(db))
	if err != nil {
		t.Fatal(err)
	}
	if err := book.Author(t.Context(), orchestrator.Skill{
		Name:  "check_staging_health",
		Title: "Check staging health",
		When:  "when the user asks whether staging is up",
		Steps: "Open the staging dashboard. Read the error rate for the last hour. Report the number.",
		Needs: []string{"browser"},
	}); err != nil {
		t.Fatal(err)
	}
}

// grantSkills records the decision a human makes in the console. It goes
// through connector.Grants rather than writing a row, because Grants is what
// refuses a request that does not carry a decision — writing the row directly
// would test a path the product does not have.
func grantSkills(t *testing.T, dir string) {
	t.Helper()
	db, err := store.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	grants := connector.NewGrants(connector.NewSQLStore(db), audit.NewMemory())
	if _, _, err := grants.Grant(t.Context(), connector.GrantRequest{
		Connector: orchestrator.SkillConnector,
		Access:    mcp.AccessRead,
		Decided:   true,
		By:        "console",
		Opens:     "lets agents read the playbooks Relay has written",
	}); err != nil {
		t.Fatal(err)
	}
}

// TestGrantingRefusesWithoutADecision is ORCHESTRATOR.md §4b rule 1 at its
// narrowest point: there is no argument that makes Decided true, so no code
// path — including one the model reaches — can grant by asking nicely.
func TestGrantingRefusesWithoutADecision(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	grants := connector.NewGrants(connector.NewSQLStore(db), audit.NewMemory())
	if _, _, err := grants.Grant(t.Context(), connector.GrantRequest{
		Connector: orchestrator.SkillConnector,
		Access:    mcp.AccessRead,
		By:        "glasses",
	}); err == nil {
		t.Fatal("granted without a human decision")
	}
}
