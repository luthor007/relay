package main

import (
	"context"
	"log/slog"

	"github.com/coder/websocket"

	"github.com/luthor007/relay/relayd/internal/api"
	"github.com/luthor007/relay/relayd/internal/config"
	"github.com/luthor007/relay/relayd/internal/pairing"
	"github.com/luthor007/relay/relayd/internal/relaylink"
)

// SubsystemRelay is SYSTEM.md §7 on the health screen.
const SubsystemRelay = "relay"

// ProtoConsole is the label a console puts on `/rz/v1/connect/{id}?p=…`.
//
// Versioned in the name rather than negotiated, because the alternative is a
// handshake and a handshake is a thing to get wrong on a socket where the box
// speaks first. A v2 is a second entry in the map, served alongside this one for
// as long as consoles in the wild ask for it.
const ProtoConsole = "console.v1"

// ProtoPhone is the label a phone puts on the same route.
//
// The phone's protocol over the relay is the phone's protocol on the LAN, frame
// for frame — see [api.Server.ServeSocket], which was written for exactly this
// and says so. The single difference is where the credential goes: a bearer
// header on the LAN, an auth frame through the relay, because the relay
// terminates the handshake the header would have ridden on.
const ProtoPhone = "phone.v1"

// phoneSocket adapts the API server's phone half, authenticated.
//
// Not *api.Server directly: ServeSocket does no authentication by design, and
// a relayed stream has had none done for it. ServeRelayedSocket is the one that
// demands the credential first.
type phoneSocket struct{ srv *api.Server }

func (p phoneSocket) ServeSocket(ctx context.Context, c *websocket.Conn) {
	p.srv.ServeRelayedSocket(ctx, c)
}

// httpSocket adapts the API server's console half to [relaylink.SocketServer].
//
// The adapter exists because *api.Server has two methods with this shape and an
// interface can only match one. Which one a stream gets is decided here, in the
// composition root, rather than by anything downstream sniffing the traffic.
type httpSocket struct{ srv *api.Server }

func (h httpSocket) ServeSocket(ctx context.Context, c *websocket.Conn) {
	h.srv.ServeHTTPSocket(ctx, c)
}

// startRelay dials the rendezvous relay, or explains why it did not.
//
// The sentence it returns is the product on a machine that is not configured for
// one: "nothing is configured, so this machine is reachable only on its own LAN"
// is a complete and true answer, and it is what stops this being a blank on the
// health screen that nobody can act on.
//
// It is also the one subsystem whose health a user cannot check for themselves.
// Everything else on that screen can be verified by looking at the machine in
// front of you; "can my phone reach this from outside the house" cannot, and the
// failure is silent until someone is standing in a car park.
func startRelay(ctx context.Context, cfg config.Config, dataDir string, srv *api.Server, log *slog.Logger) (*relaylink.Link, string) {
	if !cfg.Relay.Enabled() {
		return nil, "no relay is configured, so this machine is reachable only on its own network"
	}
	if srv == nil {
		return nil, "there is no API server to serve relayed connections"
	}

	id, err := boxIdentity(cfg.Relay.BoxID, dataDir)
	if err != nil {
		// A generated id that cannot be persisted is worse than none: every
		// restart would look like a new machine to every phone that had paired
		// with this one, and the symptom is pairing that silently stops working
		// after a reboot.
		log.Warn("relayd: could not settle this machine's relay identity", "error", err)
		return nil, "this machine has no durable relay identity: " + err.Error()
	}

	link, err := relaylink.New(relaylink.Options{
		URL:    cfg.Relay.URL,
		BoxID:  id,
		Server: srv,
		// The console's half. `CONTROL-PLANE.md` §3 puts the cloud console on
		// the far side of the relay talking to this box directly, which needs
		// HTTP over a socket — [api.Server.ServeHTTPSocket] — rather than the
		// phone's session protocol.
		//
		// Both are the same server, and that is the whole point: a route or an
		// authorization check added to internal/api is reachable and enforced on
		// every path the day it lands, with no relay-only branch to keep in
		// step.
		Protocols: map[string]relaylink.SocketServer{
			ProtoConsole: httpSocket{srv},
			// SYSTEM.md §7's actual promise: a phone on cellular reaching a
			// machine behind NAT. Without this entry the relay carried the
			// console and nothing else, and the app could only ever work on
			// the same network as the box.
			ProtoPhone: phoneSocket{srv},
		},
		MaxStreams: cfg.Relay.MaxStreams,
		// The health line is pushed rather than sampled. This subsystem's state
		// moves on its own — a relay goes away, a box reconnects — and a status
		// read once at startup would report "connecting" for the life of the
		// process. It is also the only line on that screen a user cannot verify
		// by looking at the machine in front of them.
		OnState: func(status string) { srv.SetSubsystem(SubsystemRelay, status) },
		Log:     log,
	})
	if err != nil {
		return nil, "the relay is configured and unusable: " + err.Error()
	}

	go link.Run(ctx)
	return link, ""
}

// relayStatus is the health line.
//
// It reads the link rather than the config, so deleting the join in main.go
// turns this off. A status built from cfg.Relay would claim the subsystem was on
// with nothing dialling — this codebase's own recurring defect, reproduced
// inside the report that exists to catch it.
func relayStatus(link *relaylink.Link, why string) string {
	if link != nil {
		return link.Status()
	}
	if why != "" {
		return why
	}
	return "no relay is configured, so this machine is reachable only on its own network"
}

// boxIdentity returns this machine's durable name at the relay.
//
// It lives in internal/pairing now, because `relay pair` has to read the same
// file this writes: a pairing link naming a different box than the daemon
// registered is a link that connects to nothing.
func boxIdentity(configured, dataDir string) (string, error) {
	return pairing.BoxID(configured, dataDir)
}
