package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// runClient starts a client against a server and waits for its handshake.
func runClient(t *testing.T, o Options) (*Client, context.Context) {
	t.Helper()
	c, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go c.Run(ctx)

	wait, cancelWait := context.WithTimeout(ctx, 3*time.Second)
	defer cancelWait()
	if err := c.Wait(wait); err != nil {
		t.Fatalf("never connected: %v (%s)", err, c.Status())
	}
	return c, ctx
}

func TestTheConnectFrameIsTheOneTheGatewayAccepted(t *testing.T) {
	recs := loadCapture(t, "01-handshake.jsonl")
	srv := newReplayServer(serverFrames(recs, ""))

	c, _ := runClient(t, Options{
		URL:     "ws://127.0.0.1:19311",
		Token:   StaticToken("a-token"),
		Version: "0.1.0",
		Dial: func(dctx context.Context, u string) (Conn, error) {
			go srv.serve(context.Background())
			return srv.dial(dctx, u)
		},
	})

	reqs := srv.requests()
	if len(reqs) != 1 || reqs[0].Method != "connect" {
		t.Fatalf("first frame was %v, wanted one connect", srv.methods())
	}

	var p connectParams
	if err := json.Unmarshal(reqs[0].Params, &p); err != nil {
		t.Fatalf("connect params: %v", err)
	}
	if p.MinProtocol != 4 || p.MaxProtocol != 4 {
		t.Errorf("asked for protocol %d..%d, the gateway speaks 4", p.MinProtocol, p.MaxProtocol)
	}
	// The two fields the probe proved are closed enums.
	if p.Client.ID != ClientCLI || p.Client.Mode != ModeCLI {
		t.Errorf("client %q/%q would be rejected at the handshake", p.Client.ID, p.Client.Mode)
	}
	if p.Role != "operator" {
		t.Errorf("role %q, wanted operator", p.Role)
	}
	if p.Auth == nil || p.Auth.Token != "a-token" {
		t.Errorf("the token did not reach the connect frame: %+v", p.Auth)
	}
	if !contains(p.Scopes, ScopeAdmin) {
		t.Errorf("scopes %v have no operator.admin, so approvals raised elsewhere would be invisible", p.Scopes)
	}

	hello := c.Hello()
	if hello == nil {
		t.Fatal("no hello after a successful handshake")
	}
	if hello.Protocol != 4 {
		t.Errorf("protocol %d", hello.Protocol)
	}
	if hello.Server.Version == "" || hello.Server.ConnID == "" {
		t.Errorf("hello-ok did not decode: %+v", hello.Server)
	}
	if len(hello.Features.Methods) == 0 || len(hello.Features.Events) == 0 {
		t.Error("features did not decode")
	}
	if hello.Policy.MaxPayload == 0 {
		t.Error("policy.maxPayload did not decode, so nothing sizes the read limit")
	}
	if !strings.Contains(c.Status(), hello.Server.Version) {
		t.Errorf("status %q does not name the server version", c.Status())
	}
}

func TestASocketThatNeverChallengesIsNotAGateway(t *testing.T) {
	// A health event and then silence. Waiting for the challenge is what turns
	// this into "no connect.challenge" rather than a confusing auth error.
	srv := newReplayServer([]json.RawMessage{
		json.RawMessage(`{"type":"event","event":"health","payload":{"ok":true}}`),
	})

	var got []Event
	var mu sync.Mutex
	c, err := New(Options{
		URL:              "ws://127.0.0.1:1",
		HandshakeTimeout: 150 * time.Millisecond,
		MinBackoff:       time.Hour, // one attempt is all this test wants
		Dial: func(dctx context.Context, u string) (Conn, error) {
			go srv.serve(context.Background())
			return srv.dial(dctx, u)
		},
		OnEvent: func(e Event) {
			mu.Lock()
			got = append(got, e)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	deadline := time.After(3 * time.Second)
	for {
		if s := c.Status(); strings.Contains(s, "connect.challenge") {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("status never named the missing challenge: %q", c.Status())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if srv.methods() != nil {
		t.Errorf("sent %v before the challenge; the connect frame is a reply, not an opener", srv.methods())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Name != EventHealth {
		t.Errorf("events before the handshake were dropped: %+v", got)
	}
}

func TestNewRefusesANameTheGatewayWouldReject(t *testing.T) {
	_, err := New(Options{URL: "ws://127.0.0.1:19311", ClientID: "relay-probe"})
	if err == nil {
		t.Fatal("accepted client.id relay-probe, which the gateway rejects at the handshake")
	}
	if !strings.Contains(err.Error(), ClientCLI) {
		t.Errorf("the refusal does not say what to use instead: %v", err)
	}

	if _, err := New(Options{URL: "ws://127.0.0.1:19311", Mode: "operator"}); err == nil {
		t.Fatal("accepted mode operator, which the gateway rejects")
	}
}

func TestNewTakesTheURLPeopleActuallyPaste(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"http://127.0.0.1:18789", "ws://127.0.0.1:18789"},
		{"https://box.example/gw/", "wss://box.example/gw"},
		{"ws://127.0.0.1:19311", "ws://127.0.0.1:19311"},
	} {
		c, err := New(Options{URL: tc.in})
		if err != nil {
			t.Fatalf("New(%q): %v", tc.in, err)
		}
		if c.URL() != tc.want {
			t.Errorf("New(%q) dials %q, wanted %q", tc.in, c.URL(), tc.want)
		}
	}
	if _, err := New(Options{URL: "127.0.0.1:18789"}); err == nil {
		t.Error("accepted a url with no scheme")
	}
}

// helloFrame is the smallest hello-ok a synthetic server can answer with.
const helloFrame = `{"type":"res","ok":true,"payload":{"type":"hello-ok","protocol":4,` +
	`"server":{"version":"2026.7.1-2","connId":"test"},` +
	`"features":{"methods":["health"],"events":["tick"]},` +
	`"auth":{"role":"operator","scopes":["operator.read","operator.write","operator.admin","operator.approvals"]},` +
	`"policy":{"maxPayload":26214400,"maxBufferedBytes":1,"tickIntervalMs":30000}}}`

const challengeFrame = `{"type":"event","event":"connect.challenge","payload":{"nonce":"n","ts":1}}`

func TestASecondAnswerToOneRequestIsDropped(t *testing.T) {
	// The agent method answers twice with the SAME id, by design — the gateway's
	// own source says so. The first answer is the caller's; the second must not
	// panic, block, or be handed to whoever calls next.
	srv := newAnswerServer(func(r clientReq) []json.RawMessage {
		switch r.Method {
		case "connect":
			return []json.RawMessage{json.RawMessage(helloFrame)}
		case MethodAgent:
			return []json.RawMessage{
				json.RawMessage(`{"type":"res","ok":true,"payload":{"runId":"r1","status":"ok","summary":"completed"}}`),
				json.RawMessage(`{"type":"res","ok":true,"payload":{"runId":"r1","status":"ok","summary":"second"}}`),
			}
		case MethodSessionsCreate:
			return []json.RawMessage{json.RawMessage(`{"type":"res","ok":true,"payload":{"ok":true,"key":"k","sessionId":"s"}}`)}
		}
		return nil
	})
	c, ctx := runClient(t, Options{
		URL: "ws://127.0.0.1:19311",
		Dial: func(dctx context.Context, u string) (Conn, error) {
			go srv.serve(context.Background())
			return srv.dial(dctx, u)
		},
	})

	run, err := c.Agent(ctx, AgentParams{Message: "hello"})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	if run.Summary != "completed" {
		t.Errorf("took the second answer (%q), not the first", run.Summary)
	}

	// The dropped duplicate must not be waiting to answer the next call, which
	// is what a correlation map that reused ids would do.
	created, err := c.SessionsCreate(ctx, SessionsCreateParams{Label: "after"})
	if err != nil {
		t.Fatalf("sessions.create: %v", err)
	}
	if created.Key != "k" {
		t.Errorf("the next call got %+v", created)
	}
}

func TestACallInFlightFailsWhenTheSocketDies(t *testing.T) {
	srv := newReplayServer([]json.RawMessage{
		json.RawMessage(challengeFrame),
		json.RawMessage(helloFrame),
	})
	c, ctx := runClient(t, Options{
		URL:        "ws://127.0.0.1:19311",
		MinBackoff: time.Hour,
		Dial: func(dctx context.Context, u string) (Conn, error) {
			go srv.serve(context.Background())
			return srv.dial(dctx, u)
		},
	})

	done := make(chan error, 1)
	go func() {
		_, err := c.SessionsList(ctx, SessionsListParams{})
		done <- err
	}()

	// Wait for the call to be on the wire, then kill the socket under it.
	deadline := time.After(2 * time.Second)
	for len(srv.methods()) < 2 {
		select {
		case <-deadline:
			t.Fatal("the call never reached the socket")
		case <-time.After(5 * time.Millisecond):
		}
	}
	srv.conn.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a call outlived its socket")
		}
		if !errors.Is(err, ErrNotConnected) {
			t.Errorf("error does not say the connection went away: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the call hung after the socket died")
	}
}

func TestCallsBeforeAHandshakeAreRefusedRatherThanQueued(t *testing.T) {
	c, err := New(Options{URL: "ws://127.0.0.1:19311"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.SessionsList(context.Background(), SessionsListParams{}); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("wanted ErrNotConnected, got %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := c.Wait(ctx); err == nil {
		t.Error("Wait returned before any connection existed")
	}
}

func TestSubscriptionsAreMadeAgainAfterAReconnect(t *testing.T) {
	// A subscription lives in the gateway's memory and dies with the socket.
	// A reconnect that does not re-make it is a link that reports healthy and
	// reports nothing else ever again.
	frames := []json.RawMessage{
		json.RawMessage(challengeFrame),
		json.RawMessage(helloFrame),
		json.RawMessage(`{"type":"res","ok":true,"payload":{"subscribed":true}}`),
	}
	var mu sync.Mutex
	var servers []*replayServer
	dial := func(dctx context.Context, u string) (Conn, error) {
		srv := newReplayServer(frames)
		mu.Lock()
		servers = append(servers, srv)
		mu.Unlock()
		go srv.serve(context.Background())
		return srv.dial(dctx, u)
	}

	c, ctx := runClient(t, Options{
		URL:        "ws://127.0.0.1:19311",
		MinBackoff: 5 * time.Millisecond,
		MaxBackoff: 20 * time.Millisecond,
		Dial:       dial,
	})
	if err := c.SessionsSubscribe(ctx); err != nil {
		t.Fatalf("sessions.subscribe: %v", err)
	}

	mu.Lock()
	first := servers[0]
	mu.Unlock()
	first.conn.Close()

	deadline := time.After(3 * time.Second)
	for {
		mu.Lock()
		var second *replayServer
		if len(servers) > 1 {
			second = servers[1]
		}
		mu.Unlock()
		if second != nil && countOf(second.methods(), MethodSessionsSubscribe) == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("the second connection never re-subscribed")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Exactly once, on each connection. Re-reading the subscription list late —
	// after the caller has already subscribed on the new socket — asks the
	// gateway for the same feed twice, which it answers happily and which then
	// doubles every session event relayd sees.
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	for i, srv := range servers {
		if n := countOf(srv.methods(), MethodSessionsSubscribe); n > 1 {
			t.Errorf("connection %d subscribed %d times: %v", i, n, srv.methods())
		}
	}
}

func countOf(all []string, want string) int {
	n := 0
	for _, s := range all {
		if s == want {
			n++
		}
	}
	return n
}

func TestEncodeParamsSendsAnObjectRatherThanNothing(t *testing.T) {
	// Several of the gateway's validators are closed objects that reject a
	// missing params where they accept an empty one.
	b, err := encodeParams(nil)
	if err != nil || string(b) != "{}" {
		t.Fatalf("encodeParams(nil) = %q, %v", b, err)
	}
	b, err = encodeParams(json.RawMessage(nil))
	if err != nil || string(b) != "{}" {
		t.Fatalf("encodeParams(empty raw) = %q, %v", b, err)
	}
	b, err = encodeParams(SessionsAbortParams{Key: "k"})
	if err != nil || string(b) != `{"key":"k"}` {
		t.Fatalf("encodeParams(struct) = %q, %v", b, err)
	}
}

func TestCodeReadsTheGatewaysCodeAndNothingElse(t *testing.T) {
	err := &Error{Method: "sessions.create", ErrorShape: ErrorShape{
		Code: CodeInvalidRequest, Message: "model not allowed: claude-cli/claude-opus-5",
	}}
	if Code(err) != CodeInvalidRequest {
		t.Errorf("Code = %q", Code(err))
	}
	if Code(errors.New("socket closed")) != "" {
		t.Error("a plain error was read as a gateway refusal")
	}
	wrapped := errors.Join(errors.New("context"), err)
	if Code(wrapped) != CodeInvalidRequest {
		t.Error("Code does not see through a wrap")
	}
	if !strings.Contains(err.Error(), "model not allowed") {
		t.Errorf("the gateway's own message was lost: %v", err)
	}
}
