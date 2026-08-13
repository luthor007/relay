// Package gateway speaks OpenClaw's Gateway WebSocket protocol.
//
// OPENCLAW-MIGRATION.md's decision is that the Gateway is the bus: it owns the
// session registry, turn-taking and the agent harnesses, and Relay keeps the
// loop around it — speech, routing, memory, the phone, the installer. This
// package is the Go half of that seam and nothing else. It holds one socket
// open, calls methods, hands events to whoever asked for them, and reconnects.
// It is wired to nothing: no registry, no adapter, no config. The composition
// root joins them, the way it joins every other subsystem.
//
// # It is written against the wire, not against the source
//
// Every shape here was checked against a capture of a running gateway —
// docs/fixtures/openclaw/, openclaw 2026.7.1-2, taken by a plain WebSocket
// client with no openclaw process in the loop — rather than against the
// TypeScript schemas, because the installed build and the published source
// disagree and the wire is what relayd actually meets. The tests in this
// package read that capture; they open no socket.
//
// Three findings shape the whole package:
//
//   - client.id and client.mode are CLOSED enums with no slot for a third-party
//     daemon. relayd has to name itself one of theirs, and [ClientCLI] is what
//     the probe used. A connect naming anything else is rejected at the
//     handshake with "must be equal to one of the allowed values", so [New]
//     refuses it here instead, where the message can say why.
//
//   - hello-ok's features.methods is not a full enumeration. sessions.steer,
//     sessions.get, sessions.usage and others are real, callable, and missing
//     from it. So nothing here gates a call on that list: [Call] sends any
//     method and lets the gateway answer.
//
//   - one request can be answered twice. The `agent` method sends an acceptance
//     frame and then a final frame carrying the same id — the gateway's own
//     source says so in a comment at agent-run-dispatch.ts:288. The first answer
//     wins and the second is dropped, rather than being treated as a protocol
//     violation by a client that would otherwise be right to complain.
package gateway

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

// Protocol is the gateway protocol version this client speaks.
//
// The handshake asks for exactly this at both ends of the range. A gateway that
// cannot speak 4 is one nobody has read relayd against, and negotiating down to
// a version this package has never seen would mean discovering the differences
// one frame at a time, in production, during a turn.
const Protocol = 4

// DefaultPort is where `openclaw gateway` listens when nothing says otherwise
// (openclaw src/config/paths.ts). The installer should pass the port it
// actually configured rather than lean on this.
const DefaultPort = 18789

// Client ids the gateway accepts. This is a closed enum on their side, and
// there is no entry for a daemon like relayd — the probe's connect with
// client.id "relay-probe" was rejected outright.
//
// The list is the one the INSTALLED 2026.7.1-2 accepts. The 2026.8.1 source adds
// four more (openclaw-browser-copilot, openclaw-linux, openclaw-watchos,
// openclaw-worker); they are deliberately absent until a capture shows the
// gateway relayd talks to taking them.
const (
	// ClientCLI is what relayd claims. It is a lie in the sense that relayd is
	// not the openclaw CLI, and it is the least wrong of the available lies:
	// the alternatives all claim to be a UI, a phone, or a node host, and each
	// of those carries admission behaviour relayd would then be inheriting by
	// accident. Worth raising upstream — a pinned dependency should have a slot
	// for its dependents.
	ClientCLI         = "cli"
	ClientGatewayHost = "gateway-client"
	ClientWebchat     = "webchat"
	ClientWebchatUI   = "webchat-ui"
	ClientControlUI   = "openclaw-control-ui"
	ClientTUI         = "openclaw-tui"
	ClientMacOS       = "openclaw-macos"
	ClientIOS         = "openclaw-ios"
	ClientAndroid     = "openclaw-android"
	ClientNodeHost    = "node-host"
	ClientTest        = "test"
	ClientFingerprint = "fingerprint"
	ClientProbe       = "openclaw-probe"
)

// Client modes, the coarse category the gateway groups clients by. Also closed:
// the probe's `mode: "operator"` was rejected alongside its client id.
const (
	ModeCLI     = "cli"
	ModeWebchat = "webchat"
	ModeUI      = "ui"
	ModeBackend = "backend"
	ModeNode    = "node"
	ModeWorker  = "worker"
	ModeProbe   = "probe"
	ModeTest    = "test"
)

// Operator scopes. relayd asks for a role of "operator" and these on top.
const (
	ScopeRead        = "operator.read"
	ScopeWrite       = "operator.write"
	ScopeAdmin       = "operator.admin"
	ScopeApprovals   = "operator.approvals"
	ScopePairing     = "operator.pairing"
	ScopeTalkSecrets = "operator.talk.secrets"
)

// DefaultScopes is what relayd needs to be the box's operator.
//
// ScopeAdmin is in the list for one specific reason, and dropping it silently
// breaks the feature that matters most: approval visibility is scoped, and a
// client without operator.admin sees only approvals raised on its own
// connection or bound to its own paired device id. relayd is the surface for
// approvals it did not raise — that is the entire point of it — so without
// admin it connects successfully, subscribes successfully, and never sees a
// single approval anyone else's session asked for.
var DefaultScopes = []string{ScopeRead, ScopeWrite, ScopeAdmin, ScopeApprovals}

// DefaultCaps asks for tool-event frames, which is how relayd learns a session
// is running a command rather than only that it is busy.
var DefaultCaps = []string{"tool-events"}

// Defaults for the reconnect loop and the handshake.
const (
	DefaultHandshakeTimeout = 20 * time.Second
	DefaultMinBackoff       = time.Second
	DefaultMaxBackoff       = time.Minute
	// DefaultReadLimit is the gateway's own maxPayload (26214400 in the
	// capture's hello-ok policy). It has to be set explicitly because the
	// websocket library defaults to 32 KiB, and hello-ok alone is 15 KiB — a
	// sessions.list of any size would close the socket with a limit error that
	// reads like a network fault.
	DefaultReadLimit = 26214400
)

// Conn is one open socket carrying JSON text frames.
//
// An interface rather than *websocket.Conn so a test can play a captured frame
// log back through the client without opening anything, which is the same
// reason internal/llm takes an HTTP client rather than calling http.Post.
type Conn interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, data []byte) error
	Close() error
}

// Token resolves the gateway's auth token.
//
// A function rather than a string because it is called on every connect: a
// token rotated while relayd is running is presented on the next reconnect
// instead of requiring a restart, and the secret can live in the vault behind a
// SecretRef rather than in this struct. OpenClaw's onboarding writes it in
// plaintext into openclaw.json; the installer is expected to move it, and this
// signature is what lets it.
type Token func(ctx context.Context) (string, error)

// StaticToken is a token already in hand. Fine for a test or for a config that
// carries the value; a vault-backed resolver is the shape to prefer.
func StaticToken(s string) Token {
	return func(context.Context) (string, error) {
		if strings.TrimSpace(s) == "" {
			return "", errors.New("gateway: empty auth token")
		}
		return s, nil
	}
}

// Options configures a [Client].
type Options struct {
	// URL is the gateway socket. ws:// and wss:// are what it is; http:// and
	// https:// are accepted and rewritten, because that is what `openclaw
	// gateway status` prints and therefore what a person will paste.
	URL string

	// Token resolves the credential presented in the connect frame. A client
	// with no token can still connect to a gateway that wants none; one that
	// wants a token answers the handshake with an error naming that.
	Token Token

	// ClientID and Mode are how relayd names itself. Both are closed enums —
	// see [ClientCLI]. Empty means ClientCLI / ModeCLI.
	ClientID string
	Mode     string

	// Version and UserAgent are cosmetic, and show up in the gateway's log
	// beside every call relayd makes, which is the only reason to set them.
	Version   string
	UserAgent string

	// Scopes defaults to [DefaultScopes]. Read the note there before trimming
	// it: operator.admin is load-bearing for approvals.
	Scopes []string
	// Caps defaults to [DefaultCaps].
	Caps []string

	// OnEvent receives every event frame, in arrival order.
	//
	// It is called on the read loop, so it must not block: everything else on
	// this socket — including the answer to a call already in flight — is
	// behind it. Publish to a bus and return.
	OnEvent func(Event)

	// OnState is called when the link's health changes, with the sentence
	// [Client.Status] would return. A callback rather than a snapshot for the
	// reason relaylink gives: this is a subsystem whose state moves on its own,
	// so anything read once at startup reports a moment that has passed.
	OnState func(status string)

	// Dial opens the socket. Defaults to a real WebSocket dial.
	Dial func(ctx context.Context, url string) (Conn, error)

	HandshakeTimeout       time.Duration
	MinBackoff, MaxBackoff time.Duration
	// ReadLimit bounds one inbound frame. Defaults to [DefaultReadLimit].
	ReadLimit int64

	Log *slog.Logger
}

// Client is a live connection to one gateway.
//
// The zero value is not usable; build one with [New] and give it a goroutine
// running [Client.Run].
type Client struct {
	opts Options
	log  *slog.Logger
	url  string

	// wmu serialises socket writes. One writer at a time is the socket's rule,
	// and calls are made from whatever goroutine the daemon happens to be on.
	wmu sync.Mutex

	mu        sync.Mutex
	conn      Conn
	connected bool
	ready     chan struct{}
	hello     *Hello
	lastErr   string
	n         uint64
	pending   map[string]chan answer
	sticky    []stickyCall
}

// stickyCall is a call the gateway forgets when the socket drops — a
// subscription — recorded so it can be made again on the next one.
type stickyCall struct {
	method string
	params json.RawMessage
}

// ErrNotConnected is a call made while there is no live socket.
//
// Deliberately not a queue: a voice command held for thirty seconds and then
// delivered to a gateway that has meanwhile restarted is worse than one that
// failed immediately and said so. Callers that want to wait can [Client.Wait].
var ErrNotConnected = errors.New("gateway: not connected")

// New builds a client, or refuses the options that would fail at the handshake.
func New(o Options) (*Client, error) {
	u, err := normaliseURL(o.URL)
	if err != nil {
		return nil, err
	}
	if o.ClientID == "" {
		o.ClientID = ClientCLI
	}
	if o.Mode == "" {
		o.Mode = ModeCLI
	}
	if !validClientID(o.ClientID) {
		return nil, fmt.Errorf("gateway: %q is not a client id the gateway accepts; it is a closed enum with no entry for relayd, so use %q", o.ClientID, ClientCLI)
	}
	if !validMode(o.Mode) {
		return nil, fmt.Errorf("gateway: %q is not a client mode the gateway accepts; use %q", o.Mode, ModeCLI)
	}
	if o.Version == "" {
		o.Version = "0.0.1"
	}
	if o.UserAgent == "" {
		o.UserAgent = "relayd"
	}
	if len(o.Scopes) == 0 {
		o.Scopes = DefaultScopes
	}
	if len(o.Caps) == 0 {
		o.Caps = DefaultCaps
	}
	if o.HandshakeTimeout <= 0 {
		o.HandshakeTimeout = DefaultHandshakeTimeout
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = DefaultMinBackoff
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = DefaultMaxBackoff
	}
	if o.ReadLimit <= 0 {
		o.ReadLimit = DefaultReadLimit
	}
	if o.Dial == nil {
		o.Dial = dialWS(o.ReadLimit)
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Client{
		opts:    o,
		log:     o.Log,
		url:     u,
		ready:   make(chan struct{}),
		pending: map[string]chan answer{},
	}, nil
}

// URL is where this client dials.
func (c *Client) URL() string { return c.url }

// Run holds the connection open until ctx ends, reconnecting forever.
//
// Forever, and with no attempt limit, for the same reason relaylink reconnects
// forever: the gateway is where every session lives, and a box that gave up
// after five tries is a box whose owner has to walk over to it — which is the
// thing this product exists to avoid. A gateway that is down is also a gateway
// that launchd is restarting, so the recovery is usually seconds away.
func (c *Client) Run(ctx context.Context) {
	delay := c.opts.MinBackoff
	for {
		err := c.session(ctx)
		if ctx.Err() != nil {
			c.down(nil)
			return
		}
		c.down(err)
		if err != nil {
			c.log.Warn("gateway: disconnected", "url", c.url, "error", err, "retry_in", delay)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(jitter(delay)):
		}
		delay *= 2
		if delay > c.opts.MaxBackoff {
			delay = c.opts.MaxBackoff
		}
		if err == nil {
			// A clean close is the gateway restarting under launchd, not a
			// fault. Start again at the bottom of the backoff rather than
			// treating a goodbye as evidence of illness.
			delay = c.opts.MinBackoff
		}
	}
}

// session is one connection: dial, handshake, replay, then read until it ends.
func (c *Client) session(ctx context.Context) error {
	conn, err := c.opts.Dial(ctx, c.url)
	if err != nil {
		return err
	}
	defer conn.Close()

	hello, err := c.handshake(ctx, conn)
	if err != nil {
		return err
	}
	// The subscriptions to re-make are the ones that existed BEFORE this
	// connection was published. Reading them later — from inside the replay
	// goroutine, whenever it happens to be scheduled — means a caller that
	// subscribes the moment the link comes up has its subscription replayed on
	// the connection it was just made on, and the gateway is asked for the same
	// feed twice.
	resubscribe := c.subscriptions()
	c.up(conn, hello)
	c.log.Info("gateway: connected",
		"url", c.url, "server", hello.Server.Version, "conn_id", hello.Server.ConnID,
		"scopes", strings.Join(hello.Auth.Scopes, ","))

	// Re-made after the pump is running, because each one is an ordinary call
	// and needs an answer.
	go c.replay(ctx, resubscribe)

	for {
		data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		c.dispatch(data)
	}
}

// handshake answers the challenge and reads hello-ok.
//
// It runs before the read pump so nothing else can be in flight, which is true
// of the protocol too: the gateway rejects any method before connect.
func (c *Client) handshake(ctx context.Context, conn Conn) (*Hello, error) {
	ctx, cancel := context.WithTimeout(ctx, c.opts.HandshakeTimeout)
	defer cancel()

	// The gateway opens with connect.challenge and the connect frame is the
	// reply to it. Waiting for it rather than sending straight away costs one
	// round trip and buys the correct diagnosis: a socket that never sends one
	// is not a gateway, and "no connect.challenge" says that, where a rejected
	// connect frame would blame the credential.
	//
	// The nonce it carries is what a device signature would cover. relayd
	// authenticates with a token today, so it is read and dropped.
	for {
		data, err := conn.Read(ctx)
		if err != nil {
			return nil, fmt.Errorf("gateway: no connect.challenge from %s: %w", c.url, err)
		}
		f, err := decodeFrame(data)
		if err != nil {
			continue
		}
		if f.Type == frameEvent {
			if f.Event == EventConnectChallenge {
				break
			}
			c.emit(f)
			continue
		}
	}

	params := connectParams{
		MinProtocol: Protocol,
		MaxProtocol: Protocol,
		Client: clientInfo{
			ID:       c.opts.ClientID,
			Version:  c.opts.Version,
			Platform: platform(),
			Mode:     c.opts.Mode,
		},
		Role:        "operator",
		Scopes:      c.opts.Scopes,
		Caps:        c.opts.Caps,
		Commands:    []string{},
		Permissions: map[string]bool{},
		Locale:      "en-US",
		UserAgent:   c.opts.UserAgent,
	}
	if c.opts.Token != nil {
		tok, err := c.opts.Token(ctx)
		if err != nil {
			return nil, fmt.Errorf("gateway: no auth token: %w", err)
		}
		params.Auth = &connectAuth{Token: tok}
	}

	id := c.nextID()
	if err := c.send(ctx, conn, id, "connect", params); err != nil {
		return nil, err
	}

	for {
		data, err := conn.Read(ctx)
		if err != nil {
			return nil, fmt.Errorf("gateway: connect was not answered: %w", err)
		}
		f, err := decodeFrame(data)
		if err != nil {
			continue
		}
		switch {
		case f.Type == frameEvent:
			c.emit(f)
		case f.Type == frameRes && f.ID == id:
			if f.Error != nil {
				return nil, &Error{Method: "connect", ErrorShape: *f.Error}
			}
			if !f.OK {
				return nil, &Error{Method: "connect", ErrorShape: ErrorShape{
					Code:    CodeUnavailable,
					Message: "the gateway refused the connect without saying why",
				}}
			}
			var hello Hello
			if err := json.Unmarshal(f.Payload, &hello); err != nil {
				return nil, fmt.Errorf("gateway: hello-ok did not decode: %w", err)
			}
			if hello.Protocol != Protocol {
				return nil, fmt.Errorf("gateway: server speaks protocol %d, this client speaks %d", hello.Protocol, Protocol)
			}
			// Scopes are granted, not taken. A gateway that withheld admin
			// leaves relayd connected and blind to other clients' approvals, so
			// say it here rather than let it read as "no approvals happened".
			for _, want := range c.opts.Scopes {
				if !contains(hello.Auth.Scopes, want) {
					c.log.Warn("gateway: a scope was asked for and not granted",
						"scope", want, "granted", strings.Join(hello.Auth.Scopes, ","))
				}
			}
			return &hello, nil
		}
	}
}

// subscriptions is the sticky call list as it stands.
func (c *Client) subscriptions() []stickyCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]stickyCall, len(c.sticky))
	copy(out, c.sticky)
	return out
}

// replay re-makes the subscriptions a new socket has never heard of.
func (c *Client) replay(ctx context.Context, calls []stickyCall) {
	for _, s := range calls {
		if err := c.Call(ctx, s.method, json.RawMessage(s.params), nil); err != nil {
			if ctx.Err() != nil {
				return
			}
			// Warned rather than fatal: dropping the connection over one
			// refused subscription would cost the others too, and the state
			// callback already tells the health screen the link is unwell.
			c.log.Warn("gateway: a subscription did not survive the reconnect",
				"method", s.method, "error", err)
		}
	}
}

// up records a live connection.
func (c *Client) up(conn Conn, hello *Hello) {
	c.mu.Lock()
	c.conn = conn
	c.connected = true
	c.hello = hello
	c.lastErr = ""
	close(c.ready)
	c.mu.Unlock()
	c.notify()
}

// down clears it, and fails everything that was waiting on it.
func (c *Client) down(err error) {
	c.mu.Lock()
	if !c.connected && c.conn == nil && len(c.pending) == 0 {
		if err != nil {
			c.lastErr = err.Error()
		}
		c.mu.Unlock()
		c.notify()
		return
	}
	if c.connected {
		c.connected = false
		c.ready = make(chan struct{})
	}
	c.conn = nil
	c.hello = nil
	if err != nil {
		c.lastErr = err.Error()
	}
	waiting := c.pending
	c.pending = map[string]chan answer{}
	c.mu.Unlock()

	// A call in flight when the socket died gets an error naming that, rather
	// than its own timeout thirty seconds later: the difference decides whether
	// the daemon retries or reports.
	lost := ErrNotConnected
	if err != nil {
		lost = fmt.Errorf("%w: %w", ErrNotConnected, err)
	}
	for _, ch := range waiting {
		select {
		case ch <- answer{err: lost}:
		default:
		}
	}
	c.notify()
}

func (c *Client) notify() {
	if c.opts.OnState == nil {
		return
	}
	// Outside the lock. The daemon's callback takes the API server's mutex, and
	// holding two locks in one order here and the other order there is how a
	// health endpoint deadlocks a reconnect.
	c.opts.OnState(c.Status())
}

// Wait blocks until the client has finished a handshake, or ctx ends.
func (c *Client) Wait(ctx context.Context) error {
	c.mu.Lock()
	if c.connected {
		c.mu.Unlock()
		return nil
	}
	ready := c.ready
	c.mu.Unlock()

	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Connected reports whether there is a live socket.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// Hello is what the gateway said about itself at the last handshake, or nil.
func (c *Client) Hello() *Hello {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hello
}

// Status is one sentence for a health screen.
func (c *Client) Status() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.connected && c.hello != nil:
		return "on, openclaw " + c.hello.Server.Version
	case c.connected:
		return "on"
	case c.lastErr != "":
		return "configured, and not reaching the gateway: " + c.lastErr
	default:
		return "connecting"
	}
}

func validClientID(id string) bool {
	switch id {
	case ClientCLI, ClientGatewayHost, ClientWebchat, ClientWebchatUI, ClientControlUI,
		ClientTUI, ClientMacOS, ClientIOS, ClientAndroid, ClientNodeHost,
		ClientTest, ClientFingerprint, ClientProbe:
		return true
	}
	return false
}

func validMode(m string) bool {
	switch m {
	case ModeCLI, ModeWebchat, ModeUI, ModeBackend, ModeNode, ModeWorker, ModeProbe, ModeTest:
		return true
	}
	return false
}

func contains(all []string, want string) bool {
	for _, s := range all {
		if s == want {
			return true
		}
	}
	return false
}

// normaliseURL accepts what a person or `openclaw gateway status` would paste.
func normaliseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("gateway: no url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("gateway: %q is not a url: %w", raw, err)
	}
	switch u.Scheme {
	case "ws", "wss":
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("gateway: %q is not a ws:// or http:// url", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("gateway: %q names no host", raw)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u.String(), nil
}

func dialWS(readLimit int64) func(ctx context.Context, u string) (Conn, error) {
	return func(ctx context.Context, u string) (Conn, error) {
		dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		c, _, err := websocket.Dial(dialCtx, u, &websocket.DialOptions{
			CompressionMode: websocket.CompressionDisabled,
		})
		if err != nil {
			return nil, err
		}
		c.SetReadLimit(readLimit)
		return &wsConn{c: c}, nil
	}
}

// wsConn is the real socket behind [Conn].
type wsConn struct{ c *websocket.Conn }

func (w *wsConn) Read(ctx context.Context) ([]byte, error) {
	typ, data, err := w.c.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageText {
		// The gateway speaks JSON text. A binary frame is a protocol this
		// client has not been read against, and guessing at it is how a client
		// ends up half-implementing something nobody documented.
		return nil, fmt.Errorf("gateway: unexpected %s frame", typ)
	}
	return data, nil
}

func (w *wsConn) Write(ctx context.Context, data []byte) error {
	return w.c.Write(ctx, websocket.MessageText, data)
}

func (w *wsConn) Close() error {
	return w.c.CloseNow()
}

// jitter spreads reconnects, so that a gateway which just came back does not
// meet every client it dropped in the same millisecond.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d/2 + time.Duration(rand.Int64N(int64(d)))
}
