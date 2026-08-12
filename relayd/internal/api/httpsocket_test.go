package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/luthor007/relay/relayd/internal/api"
)

// The console's API, over a socket
//
// `CONTROL-PLANE.md` §3 puts the console on the far side of the relay, which
// carries WebSocket frames and nothing else. These assert the property the whole
// design rests on: a tunnelled request is served by the *same* handler an
// inbound one is, with the same authentication — so there is no second API to
// keep in step and no relay-only path where a check could go missing.

// reply is one box → console frame, as the console sees it.
type reply struct {
	ID      string            `json:"id"`
	Kind    string            `json:"kind"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Data    string            `json:"data"`
	Message string            `json:"message"`
}

// tunnelClient is the console half: it writes request frames and collects
// replies.
type tunnelClient struct {
	c  *websocket.Conn
	in chan reply
}

// openTunnel serves [api.Server.ServeHTTPSocket] over a real WebSocket.
//
// The socket arrives here as an inbound upgrade rather than through a relay,
// which is the point of the seam: the daemon hands relayed streams to the same
// method, so what these tests exercise is what a cloud console reaches.
func openTunnel(t *testing.T, r *rig) *tunnelClient {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		c, err := websocket.Accept(w, req, &websocket.AcceptOptions{
			CompressionMode: websocket.CompressionDisabled,
		})
		if err != nil {
			return
		}
		r.Srv.ServeHTTPSocket(req.Context(), c)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })

	tc := &tunnelClient{c: c, in: make(chan reply, 64)}
	go func() {
		for {
			_, data, err := c.Read(context.Background())
			if err != nil {
				close(tc.in)
				return
			}
			var got reply
			if json.Unmarshal(data, &got) == nil {
				tc.in <- got
			}
		}
	}()
	return tc
}

func (tc *tunnelClient) send(t *testing.T, frame map[string]any) {
	t.Helper()
	data, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tc.c.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}
}

// next returns the next frame for an id, skipping frames for other requests.
func (tc *tunnelClient) next(t *testing.T, id string) reply {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case got, ok := <-tc.in:
			if !ok {
				t.Fatal("the tunnel closed")
			}
			if got.ID == id || id == "" {
				return got
			}
		case <-deadline:
			t.Fatalf("nothing arrived for %q", id)
		}
	}
}

// do runs one request and returns the head frame and the whole body.
func (tc *tunnelClient) do(t *testing.T, frame map[string]any) (reply, string) {
	t.Helper()
	tc.send(t, frame)
	id, _ := frame["id"].(string)

	var head reply
	var body strings.Builder
	for {
		got := tc.next(t, id)
		switch got.Kind {
		case "head":
			head = got
		case "body":
			body.WriteString(got.Data)
		case "end":
			return head, body.String()
		case "error":
			return got, body.String()
		}
	}
}

func req(id, method, path, token string) map[string]any {
	frame := map[string]any{"id": id, "kind": "req", "method": method, "path": path}
	if token != "" {
		frame["headers"] = map[string]string{"Authorization": "Bearer " + token}
	}
	return frame
}

func TestATunnelledRequestReachesTheSameHandler(t *testing.T) {
	r := newRig(t, api.Options{})
	tc := openTunnel(t, r)

	head, body := tc.do(t, req("1", "GET", "/v1/health", r.Srv.Token()))
	if head.Kind != "head" || head.Status != http.StatusOK {
		t.Fatalf("GET /v1/health over the tunnel: %+v", head)
	}
	if ct := head.Headers["Content-Type"]; !strings.Contains(ct, "json") {
		t.Errorf("content type %q", ct)
	}

	// Decoded rather than string-matched: the point is that this is the real
	// response from the real handler, not a shape the tunnel invented.
	var health map[string]any
	if err := json.Unmarshal([]byte(body), &health); err != nil {
		t.Fatalf("the body is not the health document: %v (%s)", err, body)
	}
	if _, ok := health["ok"]; !ok {
		t.Errorf("health over the tunnel is missing its fields: %s", body)
	}
}

func TestTheTunnelAuthenticatesExactlyLikeEverythingElse(t *testing.T) {
	// The one that matters. A tunnel that served requests without going through
	// [api.Server.guard] would mean anyone who can reach the relay — which is
	// anyone at all, it is a public address — can read a stranger's sessions by
	// knowing a box id. Deleting the ServeHTTP call and answering frames
	// directly is exactly the mistake this catches.
	r := newRig(t, api.Options{})
	tc := openTunnel(t, r)

	head, _ := tc.do(t, req("1", "GET", "/v1/sessions", ""))
	if head.Status != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated tunnelled request got %d", head.Status)
	}

	head, _ = tc.do(t, req("2", "GET", "/v1/sessions", "not-the-token"))
	if head.Status != http.StatusUnauthorized {
		t.Fatalf("a wrong token got %d", head.Status)
	}

	head, _ = tc.do(t, req("3", "GET", "/v1/sessions", r.Srv.Token()))
	if head.Status != http.StatusOK {
		t.Fatalf("the real token got %d", head.Status)
	}
}

func TestTheTunnelOnlyReachesThisMachine(t *testing.T) {
	// Without these checks every customer's box is an SSRF pivot for anyone who
	// can open a stream to it — and the box is the machine with the vault on it.
	r := newRig(t, api.Options{})
	tc := openTunnel(t, r)

	for i, path := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"//example.com/v1/health",
		"https://example.com/v1/health",
		"v1/health",
		"",
	} {
		id := "p" + string(rune('0'+i))
		head, _ := tc.do(t, req(id, "GET", path, r.Srv.Token()))
		if head.Kind != "error" {
			t.Errorf("path %q was accepted: %+v", path, head)
		}
	}
}

func TestOneSocketCarriesManyRequestsAtOnce(t *testing.T) {
	// The console issues several per screen and holds an SSE stream open across
	// all of them. A tunnel that served one at a time would deadlock the moment
	// the stream started, because the stream never ends.
	r := newRig(t, api.Options{})
	tc := openTunnel(t, r)

	for i := 0; i < 4; i++ {
		tc.send(t, req("r"+string(rune('0'+i)), "GET", "/v1/health", r.Srv.Token()))
	}
	seen := map[string]int{}
	deadline := time.After(10 * time.Second)
	for len(seen) < 4 {
		select {
		case got, ok := <-tc.in:
			if !ok {
				t.Fatal("the tunnel closed")
			}
			if got.Kind == "end" {
				seen[got.ID]++
			}
		case <-deadline:
			t.Fatalf("only %d of 4 responses came back", len(seen))
		}
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("%s ended %d times", id, n)
		}
	}
}

func TestALiveStreamArrivesAsItIsProducedAndStopsWhenCancelled(t *testing.T) {
	// `/v1/events` never completes, which is the reason a response is head →
	// body* → end rather than one frame. If the writer buffered until the
	// handler returned, this would hang forever — and the console's whole live
	// view is that stream.
	r := newRig(t, api.Options{})
	tc := openTunnel(t, r)

	tc.send(t, req("s", "GET", "/v1/events", r.Srv.Token()))

	head := tc.next(t, "s")
	if head.Kind != "head" || head.Status != http.StatusOK {
		t.Fatalf("the stream did not open: %+v", head)
	}
	if ct := head.Headers["Content-Type"]; !strings.Contains(ct, "event-stream") {
		t.Errorf("the stream is not SSE: %q", ct)
	}

	// The opening `sessions` frame, delivered while the handler is still
	// running.
	body := tc.next(t, "s")
	if body.Kind != "body" || !strings.Contains(body.Data, "event: sessions") {
		t.Fatalf("the opening frame did not arrive as it was produced: %+v", body)
	}

	// And the console can end it. Without this an abandoned stream holds a
	// registry watch open until the daemon restarts.
	tc.send(t, map[string]any{"id": "s", "kind": "cancel"})
	for {
		got := tc.next(t, "s")
		if got.Kind == "end" {
			return
		}
		if got.Kind == "error" {
			t.Fatalf("cancelling produced an error: %+v", got)
		}
	}
}

func TestABodyGetsThrough(t *testing.T) {
	// GET-only would have passed every test above and shipped a console that
	// cannot answer a question or save a credential.
	r := newRig(t, api.Options{})
	tc := openTunnel(t, r)

	frame := req("1", "POST", "/v1/sessions/nope/turns", r.Srv.Token())
	frame["headers"] = map[string]string{
		"Authorization": "Bearer " + r.Srv.Token(),
		"Content-Type":  "application/json",
	}
	frame["body"] = `{"text":"hello"}`

	head, body := tc.do(t, frame)
	// A session that does not exist, which is a 404 from the real handler —
	// the point being that the handler parsed the body and got as far as
	// looking the session up, rather than refusing for want of one.
	if head.Status == http.StatusBadRequest || head.Status == http.StatusUnsupportedMediaType {
		t.Fatalf("the body did not survive the tunnel: %d %s", head.Status, body)
	}
	if head.Status != http.StatusNotFound {
		t.Logf("POST over the tunnel answered %d: %s", head.Status, body)
	}
}
