package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/mcp"
)

func post(t *testing.T, h http.Handler, path string, body any, headers map[string]string) (*httptest.ResponseRecorder, mcp.Response) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp mcp.Response
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("undecodable response %q: %v", rec.Body.String(), err)
		}
	}
	return rec, resp
}

func rpc(id int, method string, params any) map[string]any {
	m := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		m["params"] = params
	}
	return m
}

func resultMap(t *testing.T, r mcp.Response) map[string]any {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("unexpected error response: %+v", r.Error)
	}
	b, err := json.Marshal(r.Result)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestInitializeEchoesTheClientsProtocolVersion(t *testing.T) {
	g, _ := newGateway(t, mcp.Options{Name: "relay", Version: "7"})
	h := g.HTTPHandler()

	_, resp := post(t, h, mcp.HTTPPrefix, rpc(1, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"clientInfo":      map[string]any{"name": "claude-code", "version": "2.1.226"},
	}), nil)

	m := resultMap(t, resp)
	if m["protocolVersion"] != "2024-11-05" {
		t.Fatalf("a client that names a version gets its own back, got %v", m["protocolVersion"])
	}
	info := m["serverInfo"].(map[string]any)
	if info["name"] != "relay" || info["version"] != "7" {
		t.Fatalf("serverInfo is wrong: %v", info)
	}
}

// listChanged is a property of the transport, not of this instant: a
// streamable-HTTP client opens its stream after initialize, so answering on
// whether one is attached right now would tell every HTTP client "never" and it
// would never open one. What stays strictly observed is the refresh planner,
// which reports "notified" only for a write that landed — see
// TestPushBeatsEverythingElse and TestACPSessionIsNotRestartedBehindTheUsersBack.
func TestListChangedFollowsTheTransport(t *testing.T) {
	g, _ := newGateway(t, mcp.Options{})
	_, resp := post(t, g.HTTPHandler(), mcp.HTTPPrefix, rpc(1, "initialize", map[string]any{}), nil)

	caps := resultMap(t, resp)["capabilities"].(map[string]any)
	tools := caps["tools"].(map[string]any)
	if tools["listChanged"] != true {
		t.Fatalf("streamable HTTP has a server→client stream, got %v", tools["listChanged"])
	}

	// But no stream is open yet, so nothing may claim a session was told.
	res := g.Refresh(context.Background(), "granted")
	for _, s := range res.Sessions {
		if s.Action == mcp.RefreshNotified {
			t.Fatalf("nothing was delivered, so nothing may be reported as notified: %+v", s)
		}
	}
}

// A connection for a session the registry no longer has, with no stream open, is
// dead weight and must not accumulate for the life of the process.
func TestConnectionsForDeadSessionsAreSwept(t *testing.T) {
	sessions := &fakeSessions{live: []mcp.SessionInfo{{ID: "alive", Runtime: "claude-code"}}}
	g, _ := newGateway(t, mcp.Options{Sessions: sessions})
	h := g.HTTPHandler()

	post(t, h, mcp.HTTPPrefix+"alive", rpc(1, "ping", nil), nil)
	post(t, h, mcp.HTTPPrefix+"gone", rpc(1, "ping", nil), nil)
	if got := len(g.Connections()); got != 2 {
		t.Fatalf("want 2 connections, got %d", got)
	}

	g.Refresh(context.Background(), "granted")
	left := g.Connections()
	if len(left) != 1 || left[0].Session() != "alive" {
		t.Fatalf("only the live session's connection should remain: %d", len(left))
	}
}

func TestToolsListShowsOnlyGrantedHalves(t *testing.T) {
	g, _ := newGateway(t, mcp.Options{Grants: grantSet{"printer:read": true}})
	_, resp := post(t, g.HTTPHandler(), mcp.HTTPPrefix, rpc(1, "tools/list", nil), nil)

	list := resultMap(t, resp)["tools"].([]any)
	if len(list) != 1 {
		t.Fatalf("want only the granted read tool, got %d", len(list))
	}
	tool := list[0].(map[string]any)
	if tool["name"] != "printer_status" {
		t.Fatalf("wrong tool: %v", tool["name"])
	}
	meta := tool["_meta"].(map[string]any)
	if meta["relay/connector"] != "printer" || meta["relay/access"] != "read" || meta["relay/consequential"] != false {
		t.Fatalf("_meta must let a client act on the grant without parsing English: %v", meta)
	}
}

func TestConsequentialToolSaysSoInItsDescription(t *testing.T) {
	g, _ := newGateway(t, mcp.Options{Grants: grantSet{"printer:write": true}})
	_, resp := post(t, g.HTTPHandler(), mcp.HTTPPrefix, rpc(1, "tools/list", nil), nil)

	tool := resultMap(t, resp)["tools"].([]any)[0].(map[string]any)
	desc, _ := tool["description"].(string)
	if !strings.Contains(desc, "confirms it out loud") {
		t.Fatalf("an agent should see, without calling it, that this one stops and asks: %q", desc)
	}
	ann := tool["annotations"].(map[string]any)
	if ann["destructiveHint"] != true || ann["openWorldHint"] != true {
		t.Fatalf("annotations must mark the consequence: %v", ann)
	}
	meta := tool["_meta"].(map[string]any)
	if meta["relay/consequential"] != true {
		t.Fatalf("_meta must mark it too: %v", meta)
	}
}

// "You have not connected this" and "that tool does not exist" lead to
// different next steps for the user, so they are different codes.
func TestNotGrantedIsItsOwnErrorCode(t *testing.T) {
	g, _ := newGateway(t, mcp.Options{})
	_, resp := post(t, g.HTTPHandler(), mcp.HTTPPrefix,
		rpc(1, "tools/call", map[string]any{"name": "printer_status"}), nil)

	if resp.Error == nil || resp.Error.Code != mcp.CodeNotGranted {
		t.Fatalf("want CodeNotGranted, got %+v", resp.Error)
	}
	data, ok := resp.Error.Data.(map[string]any)
	if !ok || data["scope"] != "printer:read" {
		t.Fatalf("the error should name the scope that would fix it: %v", resp.Error.Data)
	}

	_, resp = post(t, g.HTTPHandler(), mcp.HTTPPrefix,
		rpc(2, "tools/call", map[string]any{"name": "nope"}), nil)
	if resp.Error == nil || resp.Error.Code != mcp.CodeMethodNotFound {
		t.Fatalf("want CodeMethodNotFound for an unknown tool, got %+v", resp.Error)
	}
}

func TestDeniedConfirmationIsItsOwnErrorCode(t *testing.T) {
	g, _ := newGateway(t, mcp.Options{
		Grants:  grantSet{"printer:write": true},
		Confirm: mcp.ConfirmerFunc(func(context.Context, mcp.Confirmation) error { return mcp.ErrDenied }),
	})
	_, resp := post(t, g.HTTPHandler(), mcp.HTTPPrefix,
		rpc(1, "tools/call", map[string]any{"name": "printer_print"}), nil)

	if resp.Error == nil || resp.Error.Code != mcp.CodeDenied {
		t.Fatalf("want CodeDenied, got %+v", resp.Error)
	}
}

// A tool that ran and failed is a result the model can react to, not a protocol
// error. MCP separates them deliberately and so does the gateway.
func TestToolFailureComesBackAsAResult(t *testing.T) {
	g := mcp.NewGateway(mcp.Options{Grants: grantSet{"x:read": true}})
	g.Register(context.Background(), mcp.ProviderFunc{
		Name: "p",
		Fn: func(context.Context) []mcp.Tool {
			return []mcp.Tool{{
				Name: "x_boom", Connector: "x", Access: mcp.AccessRead,
				Handler: func(context.Context, mcp.Call) (mcp.Result, error) {
					return mcp.Result{}, io.ErrUnexpectedEOF
				},
			}}
		},
	})
	_, resp := post(t, g.HTTPHandler(), mcp.HTTPPrefix,
		rpc(1, "tools/call", map[string]any{"name": "x_boom"}), nil)

	m := resultMap(t, resp)
	if m["isError"] != true {
		t.Fatalf("a failing tool is an isError result, got %v", m)
	}
}

func TestEndpointPathNamesTheSession(t *testing.T) {
	rec := &mcp.MemoryRecorder{}
	g, _ := newGateway(t, mcp.Options{Grants: grantSet{"printer:read": true}, Record: rec})

	post(t, g.HTTPHandler(), mcp.HTTPPrefix+"sess-42",
		rpc(1, "tools/call", map[string]any{"name": "printer_status"}), nil)

	calls := rec.Calls()
	if len(calls) != 1 || calls[0].Session != "sess-42" {
		t.Fatalf("the endpoint path must attribute the call: %+v", calls)
	}
}

func TestMetaCanNameTheSessionOnTheSharedEndpoint(t *testing.T) {
	rec := &mcp.MemoryRecorder{}
	g, _ := newGateway(t, mcp.Options{Grants: grantSet{"printer:read": true}, Record: rec})

	post(t, g.HTTPHandler(), mcp.HTTPPrefix, rpc(1, "tools/call", map[string]any{
		"name":  "printer_status",
		"_meta": map[string]any{"relay/session": "sess-7", "relay/turn": "turn-3"},
	}), nil)

	calls := rec.Calls()
	if len(calls) != 1 || calls[0].Session != "sess-7" || calls[0].Turn != "turn-3" {
		t.Fatalf("_meta is the fallback attribution: %+v", calls)
	}
}

// Several clients drop a server that errors on an unimplemented method during
// startup, and dropping the connection takes the tools with it.
func TestUnimplementedListsAnswerEmptyRatherThanErroring(t *testing.T) {
	g, _ := newGateway(t, mcp.Options{})
	h := g.HTTPHandler()
	for _, method := range []string{"resources/list", "prompts/list", "resources/templates/list"} {
		_, resp := post(t, h, mcp.HTTPPrefix, rpc(1, method, nil), nil)
		if resp.Error != nil {
			t.Fatalf("%s must not error: %+v", method, resp.Error)
		}
	}
	_, resp := post(t, h, mcp.HTTPPrefix, rpc(1, "does/not/exist", nil), nil)
	if resp.Error == nil || resp.Error.Code != mcp.CodeMethodNotFound {
		t.Fatalf("an unknown method is still an error: %+v", resp.Error)
	}
}

func TestNotificationsGetNoResponse(t *testing.T) {
	g, _ := newGateway(t, mcp.Options{})
	rec, _ := post(t, g.HTTPHandler(), mcp.HTTPPrefix,
		map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("a notification gets 202 and no body, got %d %q", rec.Code, rec.Body.String())
	}
}

func TestRuntimeIsResolvedFromClientInfo(t *testing.T) {
	cases := map[string]string{
		"claude-code":    "claude-code",
		"Claude Code":    "claude-code",
		"codex-cli":      "codex",
		"opencode":       "opencode",
		"openclaw":       "openclaw",
		"hermes":         "hermes",
		"some-ide":       "",
		"":               "",
		"claude-desktop": "claude-code",
	}
	for in, want := range cases {
		if got := mcp.RuntimeFor(in); got != want {
			t.Errorf("RuntimeFor(%q) = %q, want %q", in, got, want)
		}
	}
}

// ------------------------------------------------------------------ stdio --

func TestStdioRoundTripAndPush(t *testing.T) {
	g, _ := newGateway(t, mcp.Options{Grants: grantSet{"printer:read": true}})

	clientR, clientW := io.Pipe()
	serverR, serverW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = g.ServeStdio(ctx, clientR, serverW) }()

	enc := json.NewEncoder(clientW)
	dec := json.NewDecoder(serverR)

	if err := enc.Encode(rpc(1, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "opencode"},
	})); err != nil {
		t.Fatal(err)
	}
	var resp mcp.Response
	if err := dec.Decode(&resp); err != nil {
		t.Fatal(err)
	}
	caps := resultMap(t, resp)["capabilities"].(map[string]any)["tools"].(map[string]any)
	if caps["listChanged"] != true {
		t.Fatal("stdio is bidirectional, so listChanged is a promise the gateway can keep")
	}

	// A push has to reach the client, or SYSTEM.md §9 problem 6 has no good case
	// at all.
	go g.Refresh(context.Background(), "a connector was granted")
	var note mcp.Note
	if err := dec.Decode(&note); err != nil {
		t.Fatal(err)
	}
	if note.Method != mcp.MethodToolsListChanged {
		t.Fatalf("want %s, got %s", mcp.MethodToolsListChanged, note.Method)
	}
}

// ------------------------------------------------------------------- SSE --

// flushRecorder is a ResponseWriter that records what was streamed, without
// opening a socket.
type flushRecorder struct {
	mu     sync.Mutex
	header http.Header
	buf    bytes.Buffer
	code   int
}

func (f *flushRecorder) Header() http.Header {
	if f.header == nil {
		f.header = http.Header{}
	}
	return f.header
}

func (f *flushRecorder) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.Write(b)
}

func (f *flushRecorder) WriteHeader(code int) { f.code = code }
func (f *flushRecorder) Flush()               {}

func (f *flushRecorder) body() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.String()
}

func TestSSEStreamCarriesTheToolsChangedNotification(t *testing.T) {
	g, _ := newGateway(t, mcp.Options{Grants: grantSet{"printer:read": true}})
	h := g.HTTPHandler()

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, mcp.HTTPPrefix+"sess-1", nil).WithContext(ctx)
	req.Header.Set("Accept", "text/event-stream")
	rec := &flushRecorder{}

	done := make(chan struct{})
	go func() { h.ServeHTTP(rec, req); close(done) }()

	// Wait for the stream to register itself.
	deadline := time.Now().Add(2 * time.Second)
	for {
		res := g.Refresh(context.Background(), "granted")
		if len(res.Sessions) == 1 && res.Sessions[0].Action == mcp.RefreshNotified {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("the SSE stream never became notifiable: %+v", res)
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	<-done

	body := rec.body()
	if !strings.Contains(body, mcp.MethodToolsListChanged) || !strings.HasPrefix(body, "event: message\ndata: ") {
		t.Fatalf("the stream must carry the notification as an SSE message: %q", body)
	}
}
