package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/luthor007/relay/relayd/internal/api"
)

// The socket that arrives through the relay.
//
// It is the same protocol as the LAN socket, and it has had no authentication
// done for it: the handshake it came from terminated at the relay, which is a
// pipe. relaylink's own comment promises that a leaked box id "gets exactly as
// far as a stranger on the LAN — which is nowhere, because the API
// authenticates". These tests are that sentence.

// relayed serves ServeRelayedSocket over a test HTTP server, which is the
// closest thing to what the relay hands the daemon.
func relayed(t *testing.T, srv *api.Server) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		srv.ServeRelayedSocket(r.Context(), c)
	}))
}

func dialRelayed(t *testing.T, ts *httptest.Server) (*websocket.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	c, _, err := websocket.Dial(ctx, "ws"+ts.URL[len("http"):], nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c, ctx
}

func authFrame(t *testing.T, token string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"v": api.Version, "id": "a1", "type": api.TypeAuth, "at": time.Now().UnixMilli(),
		"payload": map[string]string{"token": token},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestARelayedSocketWithTheTokenIsServed(t *testing.T) {
	r := newRig(t, api.Options{})
	ts := relayed(t, r.Srv)
	defer ts.Close()

	c, ctx := dialRelayed(t, ts)
	if err := c.Write(ctx, websocket.MessageText, authFrame(t, r.Srv.Token())); err != nil {
		t.Fatal(err)
	}
	// The session list is the opening frame of a served socket; receiving it is
	// the proof that the protocol started.
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("an authenticated socket was not served: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("frame type = %v", typ)
	}
	var env api.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	if env.Type == "" || env.Type == api.TypeError {
		t.Errorf("first frame = %+v, want the session list", env)
	}
}

func TestARelayedSocketWithoutTheTokenIsRefused(t *testing.T) {
	r := newRig(t, api.Options{})
	ts := relayed(t, r.Srv)
	defer ts.Close()

	for _, tc := range []struct {
		name  string
		frame []byte
	}{
		{"a wrong token", authFrame(t, "not-the-token")},
		{"an empty token", authFrame(t, "")},
		{"some other frame first", []byte(`{"v":1,"id":"u1","type":"utterance","at":1,"payload":{"text":"hi"}}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ctx := dialRelayed(t, ts)
			if err := c.Write(ctx, websocket.MessageText, tc.frame); err != nil {
				t.Fatal(err)
			}
			if _, _, err := c.Read(ctx); err == nil {
				t.Error("the socket was served to a client that did not authenticate")
			}
		})
	}
}

// Silence is refused too, and bounded: a relayed socket that never says
// anything costs a goroutine here and a slot at the relay.
func TestARelayedSocketThatSaysNothingIsClosed(t *testing.T) {
	r := newRig(t, api.Options{})
	ts := relayed(t, r.Srv)
	defer ts.Close()

	c, ctx := dialRelayed(t, ts)
	done := make(chan error, 1)
	go func() { _, _, err := c.Read(ctx); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a silent socket was served")
		}
	case <-time.After(30 * time.Second):
		t.Error("a silent socket was left open past its deadline")
	}
}
