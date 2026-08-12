package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/logx"
)

// The request shape ADAPTERS.md §2 recorded from a live 2.1.226 probe, sent as
// an MCP tools/call. _meta carries the same tool use id and a progressToken.
const fixtureCall = `{
 "jsonrpc":"2.0","id":7,"method":"tools/call",
 "params":{
   "name":"approve",
   "arguments":{"tool_name":"Bash",
                "input":{"command":"touch /tmp/x","description":"Create empty file"},
                "tool_use_id":"toolu_01SYENeww5TLrR6ZWAJ7T7LZ"},
   "_meta":{"claudecode/toolUseId":"toolu_01SYENeww5TLrR6ZWAJ7T7LZ","progressToken":3}
 }}`

// stubApprover answers with whatever it was built with, so the MCP layer can be
// tested without a session.
type stubApprover struct {
	decision PermissionDecision
	err      error
	seen     PermissionRequest
}

func (s *stubApprover) Approve(_ context.Context, r PermissionRequest) (PermissionDecision, error) {
	s.seen = r
	return s.decision, s.err
}

func rpc(t *testing.T, srv *mcpServer, body string) *rpcResponse {
	t.Helper()
	var req rpcRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	return srv.handle(context.Background(), req)
}

// payload pulls the JSON *string* out of the single text block and decodes it.
func payload(t *testing.T, resp *rpcResponse) (map[string]any, string) {
	t.Helper()
	if resp == nil {
		t.Fatal("no response")
	}
	if resp.Error != nil {
		t.Fatalf("rpc error: %+v", resp.Error)
	}
	res, ok := resp.Result.(toolCallResult)
	if !ok {
		t.Fatalf("result is %T, want toolCallResult", resp.Result)
	}
	if len(res.Content) != 1 {
		t.Fatalf("content has %d blocks, want exactly one (ADAPTERS.md §2)", len(res.Content))
	}
	if res.Content[0].Type != "text" {
		t.Fatalf("content block type = %q, want text", res.Content[0].Type)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].Text), &m); err != nil {
		t.Fatalf("the text of the block must itself be JSON, not an object: %v", err)
	}
	return m, res.Content[0].Text
}

func TestPermissionRequestShape(t *testing.T) {
	stub := &stubApprover{decision: Allow(nil)}
	srv := newMCPServer(stub, logx.Discard())

	resp := rpc(t, srv, fixtureCall)
	got, text := payload(t, resp)

	if got["behavior"] != "allow" {
		t.Errorf("behavior = %v", got["behavior"])
	}
	// The allow shape carries no interrupt flag, and never a standing grant.
	if _, ok := got["interrupt"]; ok {
		t.Errorf("allow must not carry interrupt: %s", text)
	}
	if strings.Contains(text, "updatedPermissions") {
		t.Errorf("ORCHESTRATOR.md §4b forbids a standing grant; the response was %s", text)
	}

	if stub.seen.ToolName != "Bash" ||
		stub.seen.ToolUseID != "toolu_01SYENeww5TLrR6ZWAJ7T7LZ" ||
		stub.seen.Input["command"] != "touch /tmp/x" {
		t.Errorf("the request did not decode: %+v", stub.seen)
	}
	if got, want := stub.seen.prompt(), "Bash: Create empty file"; got != want {
		t.Errorf("prompt = %q, want %q", got, want)
	}
}

func TestPermissionDenyAndInterruptShapes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		decision  PermissionDecision
		interrupt bool
	}{
		{"deny continues the session", Deny("not now", false), false},
		{"interrupt aborts the turn", Deny("no, stop", true), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newMCPServer(&stubApprover{decision: tc.decision}, logx.Discard())
			got, text := payload(t, rpc(t, srv, fixtureCall))
			if got["behavior"] != "deny" {
				t.Errorf("behavior = %v", got["behavior"])
			}
			// The flag is always present on a deny, so "continue" and "stop"
			// are never ambiguous on the wire.
			v, ok := got["interrupt"].(bool)
			if !ok {
				t.Fatalf("deny must carry interrupt explicitly: %s", text)
			}
			if v != tc.interrupt {
				t.Errorf("interrupt = %v, want %v", v, tc.interrupt)
			}
			if got["message"] == "" {
				t.Error("a deny must say why; the model narrates it")
			}
		})
	}
}

// A tool call whose approver fails must still answer, or the runtime blocks
// forever on a question nobody is listening to.
func TestPermissionUnreachableDenies(t *testing.T) {
	srv := newMCPServer(&stubApprover{err: errors.New("boom")}, logx.Discard())
	got, _ := payload(t, rpc(t, srv, fixtureCall))
	if got["behavior"] != "deny" {
		t.Fatalf("behavior = %v, want deny: failing closed is the safe direction", got["behavior"])
	}
	if v, _ := got["interrupt"].(bool); v {
		t.Error("an unreachable orchestrator should not also abort the turn")
	}
}

func TestPermissionHandshakeAndListing(t *testing.T) {
	srv := newMCPServer(&stubApprover{decision: Allow(nil)}, logx.Discard())

	resp := rpc(t, srv, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`)
	m, _ := resp.Result.(map[string]any)
	if m["protocolVersion"] != "2024-11-05" {
		t.Errorf("initialize must answer with the version the client named, got %v", m["protocolVersion"])
	}

	if r := rpc(t, srv, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); r != nil {
		t.Error("a notification must not get a response")
	}

	resp = rpc(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	lm, _ := resp.Result.(map[string]any)
	tools, _ := lm["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", lm["tools"])
	}
	if PermissionToolName() != "mcp__relay_permission__approve" {
		t.Errorf("--permission-prompt-tool = %q", PermissionToolName())
	}

	resp = rpc(t, srv, `{"jsonrpc":"2.0","id":3,"method":"resources/list"}`)
	if resp == nil || resp.Error == nil || resp.Error.Code != rpcMethodNotFound {
		t.Errorf("an unsupported method must return -32601, got %+v", resp)
	}

	resp = rpc(t, srv, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nope","arguments":{}}}`)
	if resp == nil || resp.Error == nil {
		t.Errorf("an unknown tool must error, got %+v", resp)
	}
}

// blocked drives the whole path: HTTP in, NeedsInput out, a voice answer back.
func TestPermissionBlocksUntilAnswered(t *testing.T) {
	proc := newScriptedProcess(scriptOptions{KeepOpen: true})
	l := &scriptLauncher{proc: proc}
	a := New(Options{
		Launcher:  l,
		Log:       logx.Discard(),
		ConfigDir: t.TempDir(),
		Home:      t.TempDir(),
	})
	hs := httptest.NewServer(a.Handler())
	defer hs.Close()
	a.opts.PermissionBaseURL = hs.URL
	defer func() { _ = a.Close(context.Background()) }()

	ctx := context.Background()
	sess, err := a.Start(ctx, adapter.SessionOptions{ID: "11111111-2222-3333-4444-555555555555", Workspace: "/work"})
	if err != nil {
		t.Fatal(err)
	}

	// Claude Code calls the tool. This request is held open until somebody
	// answers, which is the whole point: ADAPTERS.md §2 budgets ~27.8 hours.
	type result struct {
		body []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost,
			endpointFor(hs.URL, "11111111-2222-3333-4444-555555555555"),
			bytes.NewReader([]byte(fixtureCall)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		done <- result{body: buf.Bytes()}
	}()

	var ask *event.NeedsInput
	select {
	case ev := <-sess.Events():
		n, ok := ev.(*event.NeedsInput)
		if !ok {
			t.Fatalf("first event is %T, want *event.NeedsInput", ev)
		}
		ask = n
	case <-time.After(5 * time.Second):
		t.Fatal("the permission call did not raise a NeedsInput")
	}

	if ask.Ping() != event.PingBlocking {
		t.Errorf("a blocked session must ping blocking, got %v", ask.Ping())
	}
	if ask.Ask != event.InputPermission {
		t.Errorf("ask = %v", ask.Ask)
	}
	if ask.Prompt != "Bash: Create empty file" {
		t.Errorf("prompt = %q", ask.Prompt)
	}
	if ask.Tool == nil || ask.Tool.ID != "toolu_01SYENeww5TLrR6ZWAJ7T7LZ" {
		t.Errorf("tool ref = %+v", ask.Tool)
	}
	// ~27.8 hours of headroom, which is what makes "ping, walk away, answer an
	// hour later" real rather than aspirational.
	if d := time.Until(ask.Deadline); d < 24*time.Hour {
		t.Errorf("deadline is only %s away", d)
	}
	// No standing grant is ever offered, because ORCHESTRATOR.md §4b requires
	// consequential actions to be confirmed every time.
	for _, o := range ask.Options {
		if o.Kind.Standing() {
			t.Errorf("option %q offers a standing grant", o.ID)
		}
	}
	if len(ask.Options) != 3 {
		t.Fatalf("options = %+v", ask.Options)
	}

	select {
	case <-done:
		t.Fatal("the call returned before anyone answered")
	case <-time.After(50 * time.Millisecond):
	}

	if err := ask.Reply(ctx, event.Reply{OptionID: "allow"}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if !strings.Contains(string(r.body), `\"behavior\":\"allow\"`) {
			t.Errorf("response body = %s", r.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the answer never reached the blocked call")
	}

	// Single-shot: a second answer is refused rather than sent twice.
	if err := ask.Reply(ctx, event.Reply{OptionID: "deny"}); !errors.Is(err, event.ErrAnswered) {
		t.Errorf("second reply = %v, want ErrAnswered", err)
	}
}

// Cancel is the hard stop. The verified mechanism is answering an open
// permission question with interrupt: true, and that is what Cancel does.
func TestCancelInterruptsThroughThePermissionPrompt(t *testing.T) {
	proc := newScriptedProcess(scriptOptions{KeepOpen: true})
	a := New(Options{
		Launcher:  &scriptLauncher{proc: proc},
		Log:       logx.Discard(),
		ConfigDir: t.TempDir(),
		Home:      t.TempDir(),
	})
	hs := httptest.NewServer(a.Handler())
	defer hs.Close()
	a.opts.PermissionBaseURL = hs.URL
	defer func() { _ = a.Close(context.Background()) }()

	ctx := context.Background()
	sess, err := a.Start(ctx, adapter.SessionOptions{ID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", Workspace: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	s := sess.(*Session)

	// Nothing is blocked, so there is no observed way to cancel.
	err = s.Cancel(ctx, "no-such-turn")
	if !errors.Is(err, adapter.ErrUnsupported) {
		t.Fatalf("Cancel with nothing blocked = %v, want ErrUnsupported", err)
	}

	decided := make(chan PermissionDecision, 1)
	go func() {
		d, err := s.Approve(ctx, PermissionRequest{ToolName: "Bash", Input: map[string]any{"command": "rm -rf /"}, ToolUseID: "toolu_x"})
		if err != nil {
			t.Error(err)
		}
		decided <- d
	}()

	select {
	case ev := <-sess.Events():
		if _, ok := ev.(*event.NeedsInput); !ok {
			t.Fatalf("event = %T", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no question was raised")
	}

	if err := s.Cancel(ctx, ""); err != nil {
		t.Fatalf("Cancel with a question open = %v", err)
	}
	select {
	case d := <-decided:
		if d.Allowed() || !d.Interrupts() {
			t.Errorf("cancel produced %+v, want a deny that interrupts", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not unblock the permission call")
	}
}

func TestPermissionHubRoutesBySession(t *testing.T) {
	hub := newPermissionHub(logx.Discard())
	hub.register("s1", &stubApprover{decision: Allow(nil)})
	srv := httptest.NewServer(hub)
	defer srv.Close()

	resp, err := http.Post(endpointFor(srv.URL, "s2"), "application/json", strings.NewReader(fixtureCall))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown session = %d, want 404", resp.StatusCode)
	}

	resp, err = http.Get(endpointFor(srv.URL, "s1"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405: nothing is ever pushed from this endpoint", resp.StatusCode)
	}
}

func TestServeStdio(t *testing.T) {
	srv := newMCPServer(&stubApprover{decision: Deny("nope", true)}, logx.Discard())
	in := strings.NewReader(strings.ReplaceAll(fixtureCall, "\n", "") + "\n")
	var out bytes.Buffer
	if err := srv.ServeStdio(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Result struct {
			Content []contentBlock `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("stdio output %q: %v", out.String(), err)
	}
	if !strings.Contains(resp.Result.Content[0].Text, `"interrupt":true`) {
		t.Errorf("stdio payload = %q", resp.Result.Content[0].Text)
	}
}

func TestDecisionMapping(t *testing.T) {
	for _, tc := range []struct {
		reply     event.Reply
		behavior  string
		interrupt bool
	}{
		{event.Reply{OptionID: optAllow}, "allow", false},
		{event.Reply{OptionID: optDeny}, "deny", false},
		{event.Reply{OptionID: optDenyStop}, "deny", true},
		{event.Reply{Decision: event.DecisionAllow}, "allow", false},
		{event.Reply{Decision: event.DecisionDeny, Interrupt: true}, "deny", true},
		// ACP's cancelled outcome has no direct spelling here; Claude Code's
		// equivalent is a deny that also aborts the turn.
		{event.Reply{Decision: event.DecisionCancelled}, "deny", true},
	} {
		d, err := decisionFor(tc.reply)
		if err != nil {
			t.Fatalf("%+v: %v", tc.reply, err)
		}
		if d.Behavior != tc.behavior || d.Interrupts() != tc.interrupt {
			t.Errorf("%+v -> %+v, want %s interrupt=%v", tc.reply, d, tc.behavior, tc.interrupt)
		}
	}
	if _, err := decisionFor(event.Reply{}); err == nil {
		t.Error("an empty reply must be refused, not guessed at")
	}
}
