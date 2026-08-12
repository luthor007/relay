package rendezvous_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/luthor007/relay/relayd/internal/rendezvous"
)

// The relay over a real socket
//
// The hub tests cover the rules with a fake Conn. These cover the thing the fake
// cannot: that the transport carries frames unchanged, that a refusal reaches
// the client as a sentence rather than a dropped connection, and that a box and
// a phone which have never heard of each other end up talking.

func relayServer(t *testing.T, tune ...func(*rendezvous.Limits)) (*httptest.Server, *rendezvous.Hub) {
	t.Helper()
	l := rendezvous.DefaultLimits()
	for _, f := range tune {
		f(&l)
	}
	h := rendezvous.NewHub(rendezvous.HubOptions{Limits: l})
	srv := httptest.NewServer(rendezvous.NewHandler(h, nil, nil).Routes())
	t.Cleanup(srv.Close)
	return srv, h
}

func dialRelay(t *testing.T, srv *httptest.Server, path string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + path
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

func writeText(t *testing.T, c *websocket.Conn, text string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte(text)); err != nil {
		t.Fatal(err)
	}
}

func readAny(t *testing.T, c *websocket.Conn) (websocket.MessageType, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return typ, data
}

func TestAPhoneAndABoxPairThroughTheRelay(t *testing.T) {
	srv, _ := relayServer(t)

	box := dialRelay(t, srv, "/rz/v1/host/7K")
	// The host registration is asynchronous on the server, and the relay is
	// deliberately not a queue, so a real client waits the same way: it dials
	// the slot before printing the code.
	time.Sleep(50 * time.Millisecond)
	phone := dialRelay(t, srv, "/rz/v1/join/7K")

	// pairing.ts's first message, carried without the relay understanding it.
	writeText(t, phone, `{"v":1,"kind":"pair.hello","slot":"7K","pake":"AAAA"}`)
	typ, data := readAny(t, box)
	if typ != websocket.MessageText || !strings.Contains(string(data), "pair.hello") {
		t.Fatalf("the box did not receive the hello: %s", data)
	}

	writeText(t, box, `{"v":1,"kind":"pair.accept","pake":"BBBB","confirm":"CCCC"}`)
	_, data = readAny(t, phone)
	if !strings.Contains(string(data), "pair.accept") {
		t.Fatalf("the phone did not receive the accept: %s", data)
	}

	// And a binary frame, which is what a sealed channel sends.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := phone.Write(ctx, websocket.MessageBinary, []byte{0x01, 0x00, 0xfe}); err != nil {
		t.Fatal(err)
	}
	typ, data = readAny(t, box)
	if typ != websocket.MessageBinary || len(data) != 3 || data[2] != 0xfe {
		t.Fatalf("a binary frame did not survive: typ=%v data=%v", typ, data)
	}
}

func TestAPhoneJoiningAnEmptySlotIsToldWhyRatherThanDropped(t *testing.T) {
	srv, _ := relayServer(t)
	phone := dialRelay(t, srv, "/rz/v1/join/ZZ")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := phone.Read(ctx)
	if err == nil {
		t.Fatal("the connection stayed open on an empty slot")
	}
	// The user typed a code and it did not work. "Nothing is waiting on that
	// pairing slot" tells them to check the code; a dropped socket tells them
	// nothing and they blame the network.
	if !strings.Contains(err.Error(), "pairing slot") {
		t.Errorf("the refusal did not reach the client as a sentence: %v", err)
	}
}

func TestAConsoleReachesABoxThroughTheRelay(t *testing.T) {
	// The decision recorded in this session: the console goes through the relay
	// rather than through a control plane in the data path. This is that path,
	// end to end, with nobody authenticated by the relay — relayd's own auth is
	// inside the pipe and unchanged.
	srv, hub := relayServer(t)

	control := dialRelay(t, srv, "/rz/v1/box/box-1")
	waitFor(t, func() bool { return hub.Connected("box-1") })

	console := dialRelay(t, srv, "/rz/v1/connect/box-1")

	// The box is offered a stream id and dials back for it.
	_, offer := readAny(t, control)
	var msg struct {
		Kind   string `json:"kind"`
		Stream string `json:"stream"`
	}
	if err := json.Unmarshal(offer, &msg); err != nil {
		t.Fatalf("the offer is not JSON: %s", offer)
	}
	if msg.Kind != "rz.connect" || msg.Stream == "" {
		t.Fatalf("unexpected offer: %s", offer)
	}

	boxSide := dialRelay(t, srv, "/rz/v1/stream/"+msg.Stream)
	time.Sleep(50 * time.Millisecond)

	writeText(t, console, `{"v":1,"type":"session.command"}`)
	_, data := readAny(t, boxSide)
	if !strings.Contains(string(data), "session.command") {
		t.Fatalf("the box did not receive the console's frame: %s", data)
	}
	writeText(t, boxSide, `{"v":1,"type":"session.list"}`)
	_, data = readAny(t, console)
	if !strings.Contains(string(data), "session.list") {
		t.Fatalf("the console did not receive the reply: %s", data)
	}
}

func TestReachingAMachineThatIsOffIsASentence(t *testing.T) {
	srv, _ := relayServer(t)
	phone := dialRelay(t, srv, "/rz/v1/connect/box-nope")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := phone.Read(ctx)
	if err == nil || !strings.Contains(err.Error(), "not connected to the relay") {
		t.Fatalf("an absent box did not produce a readable reason: %v", err)
	}
}

func TestHealthzReportsCountsAndNamesNobody(t *testing.T) {
	srv, hub := relayServer(t)
	control := dialRelay(t, srv, "/rz/v1/box/box-secret-name")
	_ = control
	waitFor(t, func() bool { return hub.Connected("box-secret-name") })

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])

	if !strings.Contains(body, `"boxes":1`) {
		t.Errorf("healthz does not report the count: %s", body)
	}
	// The thing that must never be here. A relay that can answer "which boxes
	// are online" is holding a presence list for every customer.
	if strings.Contains(body, "box-secret-name") {
		t.Errorf("healthz named a machine: %s", body)
	}
}
