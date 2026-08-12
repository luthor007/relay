package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/luthor007/relay/relayd/internal/appruntime"
	"github.com/luthor007/relay/relayd/internal/appstore"
	"github.com/luthor007/relay/relayd/internal/rendezvous"
)

// A phone reaches the real daemon through a real relay
//
// The other wiring tests start the daemon and talk to it on its own port. This
// one never does: it starts a relay, points a config at it, starts the daemon,
// and then reaches the daemon *only* through the relay — which is the case
// SYSTEM.md §7 was written about and the one nothing could exercise before,
// because there was no relay to dial.

func TestThePhoneReachesTheDaemonThroughTheRelay(t *testing.T) {
	hub := rendezvous.NewHub(rendezvous.HubOptions{Limits: rendezvous.DefaultLimits()})
	relay := httptest.NewServer(rendezvous.NewHandler(hub, nil, nil).Routes())
	defer relay.Close()
	relayWS := "ws" + strings.TrimPrefix(relay.URL, "http")

	dir := t.TempDir()
	seedDataDir(t, dir)
	cfgPath := filepath.Join(dir, "relay.toml")
	// Loopback ws:// is the one place the scheme check allows plaintext, and
	// this is why: a test relay has no certificate.
	if err := os.WriteFile(cfgPath, []byte(
		"listen = \"127.0.0.1:0\"\n\n[relay]\nurl = \""+relayWS+"\"\nbox_id = \"box-under-test\"\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}

	base := startDaemonWithConfig(t, dir, cfgPath)

	// The daemon reports the relay from the live link, not from the config.
	waitForSubsystem(t, base, SubsystemRelay, "on")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	phone, _, err := websocket.Dial(ctx, relayWS+"/rz/v1/connect/box-under-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer phone.CloseNow()

	// The opening frame of the real phone protocol, over a socket neither side
	// accepted: the phone dialled the relay and so did the daemon.
	_, data, err := phone.Read(ctx)
	if err != nil {
		t.Fatalf("nothing arrived over the relayed stream: %v", err)
	}
	if !strings.Contains(string(data), `"type":"session.list"`) {
		t.Fatalf("the relayed socket is not speaking the phone protocol: %s", data)
	}

	// And the daemon answers on it. session.command → session.list is the
	// smallest round trip that proves the stream is the real API rather than
	// something that merely opened.
	if err := phone.Write(ctx, websocket.MessageText,
		[]byte(`{"v":1,"id":"r-1","type":"session.command","at":0,"payload":{"command":"list"}}`)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("the daemon never answered over the relay")
		}
		_, data, err := phone.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if strings.Contains(string(data), `"type":"session.list"`) ||
			strings.Contains(string(data), `"type":"ack"`) {
			return
		}
	}
}

func TestADaemonWithNoRelayConfiguredSaysSoRatherThanNothing(t *testing.T) {
	// The common case, and it has to be a sentence: a blank on the health screen
	// is indistinguishable from a broken subsystem, and "reachable only on its
	// own network" is a fact the user may want to act on.
	base := startDaemon(t, t.TempDir())
	got := subsystems(t, base)[SubsystemRelay]
	if !strings.Contains(got, "own network") {
		t.Fatalf("relay health on an unconfigured box is %q", got)
	}
}

func TestTheBoxIdentitySurvivesARestart(t *testing.T) {
	// A daemon that minted a new id on every start would be a different machine
	// every morning to every phone that had paired with it, and the symptom is
	// pairing that silently stops working after a reboot.
	dir := t.TempDir()
	first, err := boxIdentity("", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "box-") {
		t.Errorf("box id %q", first)
	}
	second, err := boxIdentity("", dir)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("the id changed across a restart: %q then %q", first, second)
	}

	// The file lives beside the databases and is not world-readable — not
	// because the id is secret, but because everything else in there is.
	info, err := os.Stat(filepath.Join(dir, "box-id"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("box-id is mode %o", perm)
	}

	// A half-written file from a previous start regenerates rather than
	// registering under the empty string, which every other torn box would
	// collide with.
	if err := os.WriteFile(filepath.Join(dir, "box-id"), []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	third, err := boxIdentity("", dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(third) == "" {
		t.Fatal("an empty box-id file produced an empty id")
	}

	// A configured id always wins, so a fleet can be named deliberately.
	if got, _ := boxIdentity("box-named", dir); got != "box-named" {
		t.Errorf("a configured id was ignored: %q", got)
	}
}

// waitForSubsystem polls /v1/health until a subsystem reports want.
//
// Polled rather than read once because the relay link dials in the background:
// asserting immediately after start would be asserting on a race, and the honest
// intermediate state is "connecting".
func waitForSubsystem(t *testing.T, base, name, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = subsystems(t, base)[name]
		if last == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never reported %q; last was %q", name, want, last)
}

// APP-PLATFORM.md, in the shipped daemon
//
// `internal/apps` was thirty files of tested runtime that `cmd/relayd` did not
// import, and `appstore.Provisioner` had no implementation anywhere — so an app
// could be installed, its permissions recorded, and nothing on the machine
// could ever trigger it. These assert through the daemon's own health surface,
// which is the only place that difference shows.

func TestTheDaemonReportsTheAppPlatform(t *testing.T) {
	base := startDaemon(t, t.TempDir())
	got := subsystems(t, base)[SubsystemApps]
	if got == "" {
		t.Fatal("the app platform is not reported at all")
	}
	// On this container Node is present, so the honest answer is that it is on
	// and nothing is installed. On a box without Node it says so instead —
	// either way it is a sentence, never a blank.
	if !strings.Contains(got, "no apps installed") && !strings.Contains(got, "no app runtime") {
		t.Errorf("unexpected app platform health: %q", got)
	}
}

func TestTheDaemonAndTheCLIAgreeOnWhereAppsLive(t *testing.T) {
	// The bug this exists to prevent: the daemon opening a different store than
	// `relay install` writes to. The symptom is an app that is listed by the CLI
	// and never runs, with nothing anywhere reporting a problem.
	dir := t.TempDir()
	if got, want := appstore.StoreRoot(dir), filepath.Join(dir, "apps"); got != want {
		t.Fatalf("the store root moved: %s", got)
	}
	// And staged packages must not land inside it: a third party's files inside
	// the directory that records what the user agreed to is one collision away
	// from an app id that is also a record filename.
	if pkgs := appruntime.PackagesDir(dir); strings.HasPrefix(pkgs, appstore.StoreRoot(dir)+string(filepath.Separator)) {
		t.Errorf("packages are staged inside the install records: %s", pkgs)
	}
}

// A console reaches the real daemon's HTTP API through a real relay
//
// The sibling of TestThePhoneReachesTheDaemonThroughTheRelay, and the join
// `CONTROL-PLANE.md` §3 rests on: the cloud console is a browser that can only
// reach a Fly Machine through the relay, and the relay carries WebSocket frames.
// If this passes, the hosted console needs no proxy of ours in the data path —
// which is the property §3 says a proxy would destroy.

func TestTheConsoleReachesTheDaemonsAPIThroughTheRelay(t *testing.T) {
	hub := rendezvous.NewHub(rendezvous.HubOptions{Limits: rendezvous.DefaultLimits()})
	relay := httptest.NewServer(rendezvous.NewHandler(hub, nil, nil).Routes())
	defer relay.Close()
	relayWS := "ws" + strings.TrimPrefix(relay.URL, "http")

	dir := t.TempDir()
	seedDataDir(t, dir)
	cfgPath := filepath.Join(dir, "relay.toml")
	if err := os.WriteFile(cfgPath, []byte(
		"listen = \"127.0.0.1:0\"\n\n[relay]\nurl = \""+relayWS+"\"\nbox_id = \"box-under-test\"\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}

	base := startDaemonWithConfig(t, dir, cfgPath)
	waitForSubsystem(t, base, SubsystemRelay, "on")

	// The same token the inbound path uses. That it is the same is the point:
	// the tunnel does not have its own credential, and a console on the relay
	// presents what a console on the LAN presents.
	token := "wiring-token"

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	console, _, err := websocket.Dial(ctx,
		relayWS+"/rz/v1/connect/box-under-test?p="+ProtoConsole, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer console.CloseNow()

	// One request frame, over a socket neither side accepted.
	frame := `{"id":"1","kind":"req","method":"GET","path":"/v1/health",` +
		`"headers":{"Authorization":"Bearer ` + token + `"}}`
	if err := console.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
		t.Fatal(err)
	}

	var status float64
	var body strings.Builder
	deadline := time.Now().Add(15 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("the daemon never answered the console over the relay")
		}
		_, data, err := console.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var reply map[string]any
		if err := json.Unmarshal(data, &reply); err != nil {
			t.Fatalf("the reply is not a frame: %s", data)
		}
		switch reply["kind"] {
		case "head":
			status, _ = reply["status"].(float64)
		case "body":
			s, _ := reply["data"].(string)
			body.WriteString(s)
		case "error":
			t.Fatalf("the tunnel refused the request: %s", data)
		case "end":
			if int(status) != 200 {
				t.Fatalf("GET /v1/health over the relay answered %d: %s", int(status), body.String())
			}
			// The real health document from the real daemon — the subsystem
			// report is assembled by the composition root, so its presence is
			// evidence this is not a canned answer from the tunnel.
			var health map[string]any
			if err := json.Unmarshal([]byte(body.String()), &health); err != nil {
				t.Fatalf("the body is not the health document: %v (%s)", err, body.String())
			}
			if _, ok := health["subsystems"]; !ok {
				t.Errorf("the health document came back without its subsystems: %s", body.String())
			}
			return
		}
	}
}

func TestAConsoleOnTheRelayStillHasToAuthenticate(t *testing.T) {
	// The relay is a public address and a box id is not a secret — config.Relay
	// says so at length. So the whole security of this path is that the box
	// authenticates the request itself, and the tunnel adds no authority.
	hub := rendezvous.NewHub(rendezvous.HubOptions{Limits: rendezvous.DefaultLimits()})
	relay := httptest.NewServer(rendezvous.NewHandler(hub, nil, nil).Routes())
	defer relay.Close()
	relayWS := "ws" + strings.TrimPrefix(relay.URL, "http")

	dir := t.TempDir()
	seedDataDir(t, dir)
	cfgPath := filepath.Join(dir, "relay.toml")
	if err := os.WriteFile(cfgPath, []byte(
		"listen = \"127.0.0.1:0\"\n\n[relay]\nurl = \""+relayWS+"\"\nbox_id = \"box-under-test\"\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	base := startDaemonWithConfig(t, dir, cfgPath)
	waitForSubsystem(t, base, SubsystemRelay, "on")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	console, _, err := websocket.Dial(ctx,
		relayWS+"/rz/v1/connect/box-under-test?p="+ProtoConsole, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer console.CloseNow()

	if err := console.Write(ctx, websocket.MessageText,
		[]byte(`{"id":"1","kind":"req","method":"GET","path":"/v1/sessions"}`)); err != nil {
		t.Fatal(err)
	}
	for {
		_, data, err := console.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var reply map[string]any
		if json.Unmarshal(data, &reply) != nil {
			continue
		}
		if reply["kind"] != "head" {
			continue
		}
		if status, _ := reply["status"].(float64); int(status) != 401 {
			t.Fatalf("an unauthenticated console got %d through the relay", int(status))
		}
		return
	}
}
