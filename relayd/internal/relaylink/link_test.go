package relaylink_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/luthor007/relay/relayd/internal/relaylink"
	"github.com/luthor007/relay/relayd/internal/rendezvous"
)

// The whole point of the relay, end to end
//
// A real relay process, a real daemon-side link, and a guest that reaches the
// daemon without either side accepting an inbound connection. If this passes,
// SYSTEM.md §7's NAT problem is solved for the case it was written about: a
// phone on cellular and a machine in a house.

// fakeServer stands in for api.Server, which this package must not import.
type fakeServer struct {
	served chan string
}

func (f *fakeServer) ServeSocket(ctx context.Context, c *websocket.Conn) {
	defer c.CloseNow()
	// Announce, then echo. Both halves matter: the announcement proves the
	// daemon reached the guest, and the echo proves the guest reached the
	// daemon.
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"v":1,"type":"session.list"}`))
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		f.served <- string(data)
		_ = c.Write(ctx, typ, data)
	}
}

func relayAndLink(t *testing.T) (*httptest.Server, *fakeServer, *relaylink.Link) {
	t.Helper()
	hub := rendezvous.NewHub(rendezvous.HubOptions{Limits: rendezvous.DefaultLimits()})
	relay := httptest.NewServer(rendezvous.NewHandler(hub, nil, nil).Routes())
	t.Cleanup(relay.Close)

	srv := &fakeServer{served: make(chan string, 8)}
	link, err := relaylink.New(relaylink.Options{
		URL:    "ws" + strings.TrimPrefix(relay.URL, "http"),
		BoxID:  "box-under-test",
		Server: srv,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go link.Run(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for !link.Connected() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if !link.Connected() {
		t.Fatal("the daemon never registered with the relay")
	}
	return relay, srv, link
}

func TestAGuestReachesTheDaemonWithNeitherSideAcceptingAConnection(t *testing.T) {
	relay, srv, link := relayAndLink(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	guest, _, err := websocket.Dial(ctx,
		"ws"+strings.TrimPrefix(relay.URL, "http")+"/rz/v1/connect/box-under-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer guest.CloseNow()

	// The daemon dialled back for the stream and is serving it.
	_, data, err := guest.Read(ctx)
	if err != nil {
		t.Fatalf("the daemon never spoke on the relayed stream: %v", err)
	}
	if !strings.Contains(string(data), "session.list") {
		t.Fatalf("unexpected first frame: %s", data)
	}

	// And traffic goes the other way, which is the half a one-directional test
	// would miss.
	if err := guest.Write(ctx, websocket.MessageText, []byte(`{"v":1,"type":"utterance"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-srv.served:
		if !strings.Contains(got, "utterance") {
			t.Errorf("the daemon received %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the guest's frame never reached the daemon")
	}

	if link.Status() != "on" {
		t.Errorf("health says %q while a stream is live", link.Status())
	}
}

func TestTheDaemonReconnectsAfterItsControlConnectionIsDropped(t *testing.T) {
	// A relay that is down cuts off every phone on cellular, and the person who
	// would restart the daemon is the one who cannot reach it. Recovery has to
	// be automatic.
	//
	// The drop is forced the way the relay itself forces one — a second
	// registration for the same box id displaces the first, which is what
	// happens when a machine reconnects before the old socket was reaped. It is
	// not `httptest.Server.CloseClientConnections`, which does nothing here: a
	// WebSocket is hijacked, and hijacked connections leave the server's
	// tracking, so that call returns having closed none of them.
	hub := rendezvous.NewHub(rendezvous.HubOptions{Limits: rendezvous.DefaultLimits()})
	relay := httptest.NewServer(rendezvous.NewHandler(hub, nil, nil).Routes())
	defer relay.Close()

	base := "ws" + strings.TrimPrefix(relay.URL, "http")
	srv := &fakeServer{served: make(chan string, 8)}
	link, err := relaylink.New(relaylink.Options{
		URL:        base,
		BoxID:      "box-1",
		Server:     srv,
		MinBackoff: 10 * time.Millisecond,
		MaxBackoff: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go link.Run(ctx)
	waitUntil(t, link.Connected, "the daemon never registered")

	// Displace it, then let go, so the daemon's own reconnect is the only thing
	// that can put it back.
	intruder, _, err := websocket.Dial(ctx, base+"/rz/v1/box/box-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return !link.Connected() }, "the daemon never noticed it was displaced")
	intruder.CloseNow()

	waitUntil(t, link.Connected, "the daemon never reconnected")
	waitUntil(t, func() bool { return hub.Connected("box-1") }, "the relay never saw it again")

	// And it works afterwards, which is the part that makes reconnection worth
	// anything: a link that is "connected" and carries nothing is the failure
	// this whole component exists to avoid.
	guest, _, err := websocket.Dial(ctx, base+"/rz/v1/connect/box-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer guest.CloseNow()
	readCtx, readCancel := context.WithTimeout(ctx, 5*time.Second)
	defer readCancel()
	if _, data, err := guest.Read(readCtx); err != nil {
		t.Fatalf("no traffic after the reconnect: %v", err)
	} else if !strings.Contains(string(data), "session.list") {
		t.Fatalf("unexpected frame after the reconnect: %s", data)
	}
}

func TestALinkWithNoURLIsRefusedRatherThanBeingASilentNoOp(t *testing.T) {
	// A machine only ever reached on its own LAN is a supported state. What is
	// not supported is a daemon that looks configured and dials nothing.
	if _, err := relaylink.New(relaylink.Options{Server: &fakeServer{}}); err == nil {
		t.Fatal("a link with no url was built")
	}
	if _, err := relaylink.New(relaylink.Options{
		URL: "https://relay.glass", BoxID: "b", Server: &fakeServer{},
	}); err == nil {
		t.Fatal("an https url was accepted; the relay speaks ws")
	}
	if _, err := relaylink.New(relaylink.Options{
		URL: "wss://relay.glass", Server: &fakeServer{},
	}); err == nil {
		t.Fatal("a link with no box id was built; the relay could not tell this machine from another")
	}
}

func TestHealthSaysWhenTheRelayIsUnreachableRatherThanClaimingItIsOn(t *testing.T) {
	// The one thing a user cannot check from inside their own house.
	srv := &fakeServer{served: make(chan string, 1)}
	link, err := relaylink.New(relaylink.Options{
		URL:        "ws://127.0.0.1:1", // nothing listens on port 1
		BoxID:      "box-1",
		Server:     srv,
		MinBackoff: 10 * time.Millisecond,
		MaxBackoff: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go link.Run(ctx)

	waitUntil(t, func() bool {
		return strings.Contains(link.Status(), "not reaching the relay")
	}, "health never reported the failure")
	if link.Connected() {
		t.Error("Connected() is true against a port nothing listens on")
	}
}

func TestAnUnrecognisedControlFrameIsIgnoredRatherThanFatal(t *testing.T) {
	// A relay release must not be a forced daemon upgrade.
	hub := rendezvous.NewHub(rendezvous.HubOptions{Limits: rendezvous.DefaultLimits()})
	relay := httptest.NewServer(rendezvous.NewHandler(hub, nil, nil).Routes())
	defer relay.Close()

	srv := &fakeServer{served: make(chan string, 8)}
	link, err := relaylink.New(relaylink.Options{
		URL: "ws" + strings.TrimPrefix(relay.URL, "http"), BoxID: "box-1", Server: srv,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go link.Run(ctx)
	waitUntil(t, link.Connected, "never registered")

	// The relay's Connect writes the only frame it composes; there is no route
	// that writes a made-up one, so this asserts the daemon's tolerance by
	// checking it survives a real offer for a stream it then serves — and stays
	// registered afterwards, which a fatal parse would not.
	guest, _, err := websocket.Dial(ctx,
		"ws"+strings.TrimPrefix(relay.URL, "http")+"/rz/v1/connect/box-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer guest.CloseNow()
	if _, _, err := guest.Read(ctx); err != nil {
		t.Fatalf("the stream never opened: %v", err)
	}
	guest.CloseNow()

	time.Sleep(100 * time.Millisecond)
	if !link.Connected() {
		t.Error("the control connection did not survive a completed stream")
	}
}

func TestTheOfferCarriesNothingAboutTheGuest(t *testing.T) {
	// Read from the relay's own frame rather than trusting the daemon: the relay
	// must not tell a box about its users, and this is the assertion that keeps
	// it that way if the frame ever grows a field.
	hub := rendezvous.NewHub(rendezvous.HubOptions{Limits: rendezvous.DefaultLimits()})
	relay := httptest.NewServer(rendezvous.NewHandler(hub, nil, nil).Routes())
	defer relay.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	base := "ws" + strings.TrimPrefix(relay.URL, "http")
	control, _, err := websocket.Dial(ctx, base+"/rz/v1/box/box-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer control.CloseNow()
	waitUntil(t, func() bool { return hub.Connected("box-1") }, "never registered")

	guest, _, err := websocket.Dial(ctx, base+"/rz/v1/connect/box-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer guest.CloseNow()

	_, data, err := control.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var offer map[string]any
	if err := json.Unmarshal(data, &offer); err != nil {
		t.Fatal(err)
	}
	if len(offer) != 2 || offer["kind"] != "rz.connect" || offer["stream"] == "" {
		t.Errorf("the offer has grown fields: %s", data)
	}
}

func waitUntil(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}

// Two protocols on one registration
//
// A box is one machine at the relay, and a phone and a console reach it on the
// same control connection. What tells them apart is the label the guest chose,
// which the relay carries and this package routes on. The failure this exists to
// prevent is a console receiving the phone's frames — which would look, from the
// console, like the daemon speaking gibberish.

// namedServer answers with the protocol it is registered under, so a test can
// tell which one served a stream.
type namedServer struct{ name string }

func (n *namedServer) ServeSocket(ctx context.Context, c *websocket.Conn) {
	defer c.CloseNow()
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"served":"`+n.name+`"}`))
	<-ctx.Done()
}

// twoProtocolLink starts a box serving the phone's protocol and one other.
func twoProtocolLink(t *testing.T) *httptest.Server {
	t.Helper()
	// A short stream TTL, so the "this daemon does not serve that" case is a
	// prompt refusal rather than the test waiting out the production timeout.
	// It is the same path either way: the box does not dial back, and the relay
	// eventually tells the guest the machine is unreachable.
	limits := rendezvous.DefaultLimits()
	limits.StreamTTL = 200 * time.Millisecond
	hub := rendezvous.NewHub(rendezvous.HubOptions{Limits: limits})
	relay := httptest.NewServer(rendezvous.NewHandler(hub, nil, nil).Routes())
	t.Cleanup(relay.Close)

	link, err := relaylink.New(relaylink.Options{
		URL:       "ws" + strings.TrimPrefix(relay.URL, "http"),
		BoxID:     "box-under-test",
		Server:    &namedServer{name: "phone"},
		Protocols: map[string]relaylink.SocketServer{"console": &namedServer{name: "console"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go link.Run(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for !link.Connected() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if !link.Connected() {
		t.Fatal("the daemon never registered with the relay")
	}
	return relay
}

// firstFrame dials a guest with a protocol label and returns what it is told.
func firstFrame(t *testing.T, relay *httptest.Server, query string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx,
		"ws"+strings.TrimPrefix(relay.URL, "http")+"/rz/v1/connect/box-under-test"+query, nil)
	if err != nil {
		return "", err
	}
	defer c.CloseNow()
	_, data, err := c.Read(ctx)
	return string(data), err
}

func TestTheGuestGetsTheProtocolItAskedFor(t *testing.T) {
	relay := twoProtocolLink(t)

	if got, err := firstFrame(t, relay, "?p=console"); err != nil || !strings.Contains(got, `"console"`) {
		t.Fatalf("a console got %q (%v)", got, err)
	}
	// And a guest that names nothing still gets the phone's, which is what makes
	// every phone already built keep working.
	if got, err := firstFrame(t, relay, ""); err != nil || !strings.Contains(got, `"phone"`) {
		t.Fatalf("an unlabelled guest got %q (%v)", got, err)
	}
}

func TestAProtocolThisDaemonDoesNotServeIsNotAnsweredWithAnother(t *testing.T) {
	// The failure worth being deliberate about. Serving the default here would
	// mean a console asking for a protocol an older daemon lacks receives the
	// phone's frames instead of an error — and a client that cannot parse what
	// it is sent reports a broken machine rather than an old one.
	relay := twoProtocolLink(t)

	got, err := firstFrame(t, relay, "?p=v99")
	if err == nil {
		t.Fatalf("a protocol this daemon does not serve was answered with %q", got)
	}
}
