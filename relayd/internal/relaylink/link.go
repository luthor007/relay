// Package relaylink is the daemon's side of the rendezvous relay.
//
// SYSTEM.md §7's whole design is that **both sides dial out**, so this is what
// makes a box behind NAT reachable: it holds one connection open to the relay,
// and when a phone or a console asks for this machine, the relay offers a stream
// id and this dials back for it.
//
// Everything it does is a dial. It opens no port, forwards nothing, and needs no
// router configuration — which is the point, because "port forward" is the
// option §7 lists as working *rarely*, and asking a customer to run WireGuard is
// the one it lists as working at the cost of them running WireGuard.
//
// # It adds no authority
//
// A stream that arrives through the relay is served by exactly the same code an
// inbound socket is — `api.Server.ServeSocket` for a phone,
// `api.Server.ServeHTTPSocket` for a console — one implementation each, no
// relay-only branch. A phone that reaches this daemon over the relay still
// presents the credential it derived at pairing, and a console still presents
// the token or the account session; both get exactly as far without one as they
// would on the LAN. The relay is a path, not a permission.
//
// The guest picks which of the two it wants with a label the relay carries
// verbatim, and that choice is not a privilege: an attacker who picks the
// console's protocol reaches the console's authenticator, which is the same
// authenticator the console reaches.
package relaylink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// SocketServer runs one protocol over a socket somebody else opened.
//
// Satisfied by `api.Server` twice over — `ServeSocket` is the phone's protocol
// and `ServeHTTPSocket` is the console's. It is an interface so this package does
// not import internal/api — the daemon's composition root joins them, the same
// way it joins the app host to the phone's screen.
type SocketServer interface {
	ServeSocket(ctx context.Context, c *websocket.Conn)
}

// Options configures a [Link].
type Options struct {
	// URL is the relay's base, ws:// or wss://. Empty disables the link
	// entirely, which is the correct state for a machine that is only ever
	// reached on its own LAN.
	URL string
	// BoxID is this machine's durable name at the relay. It is not a secret and
	// must not become one: anyone who learns it can open a socket to this
	// daemon, and get exactly as far as a stranger on the LAN — which is
	// nowhere, because the API authenticates.
	BoxID string
	// Server serves a stream that named no protocol, which is the phone.
	Server SocketServer
	// Protocols serves streams that named one, by label.
	//
	// The console is here rather than on a second control connection because a
	// box has one identity at the relay and one registration; giving the console
	// its own would mean a box could be reachable by a phone and not by a
	// console, or the reverse, with nothing reporting the difference.
	//
	// A label with no entry is refused by not dialling back, which the guest
	// sees as the machine being unreachable. That is a worse message than it
	// could be and is the right behaviour anyway: an older daemon genuinely
	// cannot serve a protocol it has never heard of, and dialling back to speak
	// the wrong one would be a console silently receiving the phone's frames.
	Protocols map[string]SocketServer
	// MaxStreams bounds concurrent inbound streams, so a relay that offered
	// thousands could not exhaust this process.
	MaxStreams int
	// Backoff bounds the reconnect delay.
	MinBackoff, MaxBackoff time.Duration

	// OnState is called whenever the link's health changes, with the same
	// sentence [Link.Status] would return.
	//
	// A callback rather than the daemon reading Status once at startup, because
	// this is the one subsystem whose state moves on its own: a snapshot taken
	// while the first dial is in flight would report "connecting" for the life
	// of the process, and a snapshot taken after would keep reporting "on" long
	// after the relay went away. Either is a health screen that lies about the
	// one thing a user cannot check from inside their own house.
	OnState func(status string)

	Dial func(ctx context.Context, url string) (*websocket.Conn, error)
	Log  *slog.Logger
}

// Defaults.
const (
	DefaultMaxStreams = 8
	DefaultMinBackoff = time.Second
	DefaultMaxBackoff = 2 * time.Minute
)

// Link keeps this machine registered with the relay.
type Link struct {
	opts Options
	log  *slog.Logger

	mu        sync.Mutex
	connected bool
	lastErr   string
	streams   int
}

// ErrNoURL is a link with nowhere to dial.
var ErrNoURL = errors.New("relaylink: no relay url")

// New builds a link, or refuses when there is nothing to dial.
func New(o Options) (*Link, error) {
	if strings.TrimSpace(o.URL) == "" {
		return nil, ErrNoURL
	}
	if strings.TrimSpace(o.BoxID) == "" {
		return nil, errors.New("relaylink: no box id, so the relay could not tell this machine from another")
	}
	if o.Server == nil {
		return nil, errors.New("relaylink: no server to hand streams to")
	}
	u, err := url.Parse(o.URL)
	if err != nil || u.Host == "" || (u.Scheme != "ws" && u.Scheme != "wss") {
		return nil, fmt.Errorf("relaylink: %q is not a ws:// or wss:// url", o.URL)
	}
	if o.MaxStreams <= 0 {
		o.MaxStreams = DefaultMaxStreams
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = DefaultMinBackoff
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = DefaultMaxBackoff
	}
	if o.Dial == nil {
		o.Dial = dialWS
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Link{opts: o, log: o.Log}, nil
}

// Run holds the registration open until ctx ends, reconnecting forever.
//
// Forever is deliberate and is the difference between this and a health check: a
// relay that is down means every phone on cellular is cut off, and the recovery
// has to be automatic because the person who would restart the daemon is the one
// who cannot reach it.
func (l *Link) Run(ctx context.Context) {
	delay := l.opts.MinBackoff
	for {
		err := l.session(ctx)
		if ctx.Err() != nil {
			return
		}
		l.setState(false, err)
		if err != nil {
			l.log.Warn("relaylink: disconnected from the relay", "error", err, "retry_in", delay)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(jitter(delay)):
		}
		delay *= 2
		if delay > l.opts.MaxBackoff {
			delay = l.opts.MaxBackoff
		}
		if err == nil {
			// A clean end is a relay restart, not a fault. Start over at the
			// bottom of the backoff rather than treating a graceful goodbye as
			// evidence the relay is unwell.
			delay = l.opts.MinBackoff
		}
	}
}

// session is one control connection.
func (l *Link) session(ctx context.Context) error {
	control, err := l.opts.Dial(ctx, l.url("/rz/v1/box/"+url.PathEscape(l.opts.BoxID)))
	if err != nil {
		return err
	}
	defer control.CloseNow()

	l.setState(true, nil)
	l.log.Info("relaylink: registered with the relay", "url", l.opts.URL)
	defer l.setState(false, nil)

	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		_, data, err := control.Read(ctx)
		if err != nil {
			return err
		}
		var offer struct {
			Kind   string `json:"kind"`
			Stream string `json:"stream"`
			Proto  string `json:"proto"`
		}
		if err := json.Unmarshal(data, &offer); err != nil || offer.Kind != "rz.connect" {
			// A frame this daemon does not understand is a relay newer than it
			// is. Ignored rather than fatal, for the same reason the phone's link
			// tolerates unknown types: a relay release must not be a forced
			// daemon upgrade.
			l.log.Debug("relaylink: ignoring an unrecognised control frame")
			continue
		}
		server, ok := l.serverFor(offer.Proto)
		if !ok {
			l.log.Warn("relaylink: a guest asked for a protocol this daemon does not serve",
				"proto", offer.Proto)
			continue
		}
		if !l.takeStream() {
			// Refused by not dialling. The guest's Connect times out and it is
			// told the machine is unreachable, which is true — this one is at
			// its ceiling.
			l.log.Warn("relaylink: at the stream ceiling, ignoring an offer",
				"max", l.opts.MaxStreams)
			continue
		}
		wg.Add(1)
		go func(id string, srv SocketServer) {
			defer wg.Done()
			defer l.releaseStream()
			l.serve(ctx, id, srv)
		}(offer.Stream, server)
	}
}

// serverFor picks the protocol, or reports that this daemon does not have it.
func (l *Link) serverFor(proto string) (SocketServer, bool) {
	if proto == "" {
		return l.opts.Server, l.opts.Server != nil
	}
	s, ok := l.opts.Protocols[proto]
	return s, ok && s != nil
}

// serve dials back for one offered stream and hands it to the server.
func (l *Link) serve(ctx context.Context, streamID string, server SocketServer) {
	c, err := l.opts.Dial(ctx, l.url("/rz/v1/stream/"+url.PathEscape(streamID)))
	if err != nil {
		l.log.Warn("relaylink: could not dial back for a stream", "error", err)
		return
	}
	// The server owns the connection from here, including closing it.
	server.ServeSocket(ctx, c)
}

func (l *Link) url(path string) string {
	return strings.TrimRight(l.opts.URL, "/") + path
}

func (l *Link) takeStream() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.streams >= l.opts.MaxStreams {
		return false
	}
	l.streams++
	return true
}

func (l *Link) releaseStream() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.streams > 0 {
		l.streams--
	}
}

func (l *Link) setState(connected bool, err error) {
	l.mu.Lock()
	l.connected = connected
	if err != nil {
		l.lastErr = err.Error()
	} else if connected {
		l.lastErr = ""
	}
	status := l.status()
	notify := l.opts.OnState
	l.mu.Unlock()

	// Called outside the lock: the daemon's callback takes the API server's
	// mutex, and holding two locks in one order here and the other order there
	// is how a health endpoint deadlocks a reconnect.
	if notify != nil {
		notify(status)
	}
}

// Status is what /v1/health reports.
//
// It reads the live connection rather than the config, so a daemon that was
// configured with a relay and cannot reach it says so instead of claiming the
// feature is on. "Reachable from outside" is precisely the thing a user cannot
// check for themselves from inside the house.
func (l *Link) Status() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.status()
}

// status is Status without the lock.
func (l *Link) status() string {
	switch {
	case l.connected:
		return "on"
	case l.lastErr != "":
		return "configured, and not reaching the relay: " + l.lastErr
	default:
		return "connecting"
	}
}

// Connected reports whether the control connection is up.
func (l *Link) Connected() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.connected
}

func dialWS(ctx context.Context, u string) (*websocket.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(dialCtx, u, &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	return c, err
}

// jitter spreads reconnects.
//
// Without it every box on a relay that restarted dials back in the same
// millisecond, which is how a relay that came back up goes down again.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d/2 + time.Duration(rand.Int64N(int64(d)))
}
