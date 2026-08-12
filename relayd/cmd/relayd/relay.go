package main

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/coder/websocket"

	"github.com/luthor007/relay/relayd/internal/api"
	"github.com/luthor007/relay/relayd/internal/config"
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
		Protocols:  map[string]relaylink.SocketServer{ProtoConsole: httpSocket{srv}},
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
// Configured wins. Otherwise it is generated once and written beside the
// databases, because it has to survive a restart: a phone paired with this box
// reaches it by this id, and a daemon that minted a new one on every start would
// be a different machine every morning.
//
// The file is 0600 not because the id is secret — it is not, and config.Relay
// says so at length — but because everything else in the data directory is, and
// one world-readable file in there is an invitation to assume the rest are too.
func boxIdentity(configured, dataDir string) (string, error) {
	if id := strings.TrimSpace(configured); id != "" {
		return id, nil
	}
	if strings.TrimSpace(dataDir) == "" {
		return "", errors.New("no data directory to keep it in")
	}
	path := filepath.Join(dataDir, "box-id")

	if b, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id, nil
		}
		// An empty file is a half-finished write from a previous start. Falling
		// through regenerates it rather than registering with the relay under
		// the empty string, which would collide with every other box that did.
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	id, err := newBoxID()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	// Written through a temporary file: a torn write here is the empty-file case
	// above, and doing it atomically means that case only ever comes from a
	// power cut rather than from a crash mid-start.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return id, nil
}

// newBoxID mints an identifier.
//
// 80 bits, Crockford-ish base32 without padding, so it survives being read aloud
// or pasted into a URL. Not derived from anything about the machine — a
// hostname or a MAC would leak something about the household to a relay that is
// deliberately told as little as possible.
func newBoxID() (string, error) {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	enc := base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)
	return "box-" + enc.EncodeToString(b[:]), nil
}
