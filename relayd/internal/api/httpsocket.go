package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/coder/websocket"
)

// The console's HTTP API, carried over one socket.
//
// # Why this exists
//
// A cloud customer's box is on a Fly Machine behind NAT, and the only path to it
// is the rendezvous relay — which carries WebSocket frames, in both directions,
// and nothing else. The console is an HTTP client. Something has to bridge that,
// and there were two candidates.
//
// The obvious one was a proxy: the console talks HTTPS to us, we forward to the
// box. `CONTROL-PLANE.md` §3 rules it out in as many words — *"the control plane
// terminating the session and forwarding with an identity header would put our
// infrastructure in the data path of every console request, which is precisely
// what `CLOUD.md` §4's 'a breach of our infrastructure does not expose anyone's
// recorded life' forbids. The relay already cannot read the traffic; adding a
// proxy that can would undo that."* This supersedes `DASHBOARD.md` §2's word
// "proxy", which was written before the relay existed.
//
// So the console tunnels instead. It opens one relayed socket and sends its
// requests as frames; this reads them, replays each one into the *same*
// [http.Handler] an inbound request would reach, and sends the response back.
// Nothing in between can read it: the relay sees frames it does not parse, and
// we run no other hop.
//
// # What it deliberately is not
//
// **It is not a second API.** [Server.ServeHTTP] is the handler on both paths,
// so a route added to `routes()` is reachable through the tunnel the day it
// lands, and an authorization check added to [Server.guard] applies to tunnelled
// requests without anybody remembering to. This is the same property
// [Server.ServeSocket] has for the phone, and for the same reason: a relay-only
// branch is where a permission check goes missing.
//
// **It is not authority.** A frame arriving here has proved nothing. The
// `Authorization` header it carries is handed to the same [Authenticator] an
// inbound request's would be — on the cloud tier that is the Supabase verifier,
// which checks the signature, the issuer, the audience, the expiry and that
// `sub` is the account this box belongs to. Holding the socket gets a stranger
// exactly as far as holding a TCP connection on the LAN does, which is nowhere.
//
// **It is not a general proxy.** The path must be a rooted path on this box. A
// tunnelled request naming another host would turn every customer's box into an
// SSRF pivot for anyone who can reach the relay, and the box is the machine with
// the vault on it.
//
// # Headers are an allowlist, and that is the security-relevant decision here
//
// [tunnelHeaders] is short and closed. The reason is [Server.origin]: it reads
// `X-Forwarded-For` when the deployment says a proxy is in front, so a tunnel
// that forwarded arbitrary headers would let a guest choose the address written
// into the audit log — an audit log that records an attacker's chosen origin is
// worse than one that records none. A denylist would have to be revisited every
// time the server starts trusting a new header, and would be wrong in the gap.

// tunnelLimits bound one socket.
const (
	// maxTunnelFrame is the largest request frame accepted. The relay caps
	// frames at 1 MiB; this is under it so the refusal comes from the box, with
	// a sentence, rather than from the pipe closing.
	maxTunnelFrame = 512 << 10
	// maxTunnelInFlight is how many requests may be open at once on one socket.
	// The console issues a handful per screen plus one long-lived SSE stream.
	maxTunnelInFlight = 32
	// tunnelChunk is the largest body frame written back. Responses larger than
	// this arrive in several, which the client concatenates.
	tunnelChunk = 64 << 10
)

// tunnelHeaders is every request header carried through, lowercased.
//
// See the package comment: this is closed on purpose. `Last-Event-ID` is here
// because SSE reconnects use it, and `Authorization` because the whole design is
// that the box authenticates the caller itself.
var tunnelHeaders = map[string]bool{
	"authorization": true,
	"content-type":  true,
	"accept":        true,
	"last-event-id": true,
	"user-agent":    true,
}

// tunnelReplyHeaders is every response header carried back, lowercased.
//
// Also an allowlist, for a smaller but real reason: a tunnel that forwards
// everything will one day forward a `Set-Cookie` into a browser sitting at *our*
// origin — a cookie the console never asked for, from a machine the browser has
// no other relationship with.
var tunnelReplyHeaders = map[string]bool{
	"content-type":  true,
	"cache-control": true,
	"retry-after":   true,
}

// tunnelRequest is one console → box frame.
type tunnelRequest struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // "req" or "cancel"

	Method  string            `json:"method,omitempty"`
	Path    string            `json:"path,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	// Base64 says Body is base64. Absent for the JSON bodies the console
	// actually sends; see tunnelReply.Base64 for why it exists at all.
	Base64 bool `json:"b64,omitempty"`
}

// tunnelReply is one box → console frame.
//
// A response is `head`, then zero or more `body`, then `end` — rather than one
// frame carrying status and body together. That shape is forced by `/v1/events`:
// an SSE response never completes, so a one-shot frame could not carry it, and
// having two shapes would mean the console had two code paths with the
// streaming one exercised only in production.
type tunnelReply struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // "head" | "body" | "end" | "error"

	Status  int               `json:"status,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Data    string            `json:"data,omitempty"`
	// Base64 says Data is base64 rather than text. Everything this API returns
	// is JSON or SSE, so it is never set in practice — but a frame is a JSON
	// string, JSON strings hold text, and silently mangling a response that
	// turned out not to be UTF-8 is the kind of corruption that is discovered
	// months later in somebody's transcript.
	Base64  bool   `json:"b64,omitempty"`
	Message string `json:"message,omitempty"`
}

// ServeHTTPSocket runs the console's HTTP API over an already-established
// socket, and returns when the socket ends.
//
// The counterpart to [Server.ServeSocket]: that one speaks the phone's protocol,
// this one speaks HTTP. Both exist because a socket does not have to have
// arrived as an inbound request, and both hand the work to the same code the
// inbound path uses. The caller owns closing the connection.
func (s *Server) ServeHTTPSocket(parent context.Context, c *websocket.Conn) {
	defer c.CloseNow()

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	c.SetReadLimit(maxTunnelFrame)

	t := &tunnel{srv: s, c: c}

	// Every in-flight request is cancelled when this returns, which is what
	// stops an SSE stream whose console has closed the tab from holding a
	// registry watch open until the daemon restarts.
	defer t.cancelAll()
	defer t.wg.Wait()

	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			t.fail("", "the tunnel carries text frames")
			continue
		}
		var req tunnelRequest
		if err := json.Unmarshal(data, &req); err != nil {
			t.fail("", "that frame is not a request")
			continue
		}
		switch req.Kind {
		case "cancel":
			t.cancel(req.ID)
		case "req":
			t.start(ctx, req)
		default:
			t.fail(req.ID, "unknown frame kind "+req.Kind)
		}
	}
}

// tunnel is one socket's state.
type tunnel struct {
	srv *Server
	c   *websocket.Conn

	// write serialises frames. Several requests are in flight at once and
	// coder/websocket permits one writer; without this a body frame from one
	// response would interleave into the middle of another.
	write sync.Mutex

	wg sync.WaitGroup

	mu     sync.Mutex
	inWork map[string]context.CancelFunc
}

func (t *tunnel) start(parent context.Context, req tunnelRequest) {
	if req.ID == "" {
		t.fail("", "a request needs an id")
		return
	}

	r, err := t.build(req)
	if err != nil {
		t.fail(req.ID, err.Error())
		return
	}

	ctx, cancel := context.WithCancel(parent)

	t.mu.Lock()
	if len(t.inWork) >= maxTunnelInFlight {
		t.mu.Unlock()
		cancel()
		t.fail(req.ID, "too many requests are already open on this connection")
		return
	}
	if _, dup := t.inWork[req.ID]; dup {
		t.mu.Unlock()
		cancel()
		// Reusing an id would make two responses indistinguishable, and the
		// console demultiplexes on it.
		t.fail(req.ID, "that request id is already in flight")
		return
	}
	if t.inWork == nil {
		t.inWork = map[string]context.CancelFunc{}
	}
	t.inWork[req.ID] = cancel
	t.mu.Unlock()

	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer cancel()
		defer t.done(req.ID)

		w := &tunnelWriter{t: t, id: req.ID, header: http.Header{}}
		// The one line that makes this not a second API.
		t.srv.ServeHTTP(w, r.WithContext(ctx))
		w.finish()
	}()
}

// build turns a frame into the request the handler will see.
func (t *tunnel) build(req tunnelRequest) (*http.Request, error) {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
	default:
		return nil, errors.New("the tunnel does not carry " + method)
	}

	// Rooted paths only. `url.ParseRequestURI` accepts an absolute URI too —
	// that is what it is for — so the two checks below are the ones that keep
	// this from being an open proxy, and they are why the scheme and host are
	// tested rather than the string being trusted after it parses.
	if !strings.HasPrefix(req.Path, "/") {
		return nil, errors.New("the path must start with /")
	}
	// `//example.com/x` is a network-path reference — in a URL it names another
	// host. `url.ParseRequestURI` does not read it that way (in request mode the
	// whole string is the path), so it would arrive here as a local path that
	// `http.ServeMux` then 301s to a cleaned version. Nothing escapes the box,
	// but a caller writing this meant a host, and answering their redirect
	// rather than their intent is how a check that "passed" stops meaning
	// anything.
	if strings.HasPrefix(req.Path, "//") {
		return nil, errors.New("the tunnel only reaches this machine")
	}
	u, err := url.ParseRequestURI(req.Path)
	if err != nil {
		return nil, errors.New("that is not a path")
	}
	if u.Scheme != "" || u.Host != "" {
		return nil, errors.New("the tunnel only reaches this machine")
	}

	body := req.Body
	if req.Base64 {
		raw, err := base64.StdEncoding.DecodeString(req.Body)
		if err != nil {
			return nil, errors.New("the body is not valid base64")
		}
		body = string(raw)
	}

	header := http.Header{}
	for k, v := range req.Headers {
		if tunnelHeaders[strings.ToLower(strings.TrimSpace(k))] {
			header.Set(k, v)
		}
	}

	return &http.Request{
		Method:        method,
		URL:           u,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		RequestURI:    req.Path,
		// Named rather than invented. [Server.origin] falls through to this
		// string when it has no host:port to split, so the audit log records
		// "relay" — which is the true and complete answer to where the request
		// came from. Putting a plausible-looking address here would be the log
		// claiming to know something it does not.
		RemoteAddr: "relay",
		Host:       "box",
	}, nil
}

func (t *tunnel) cancel(id string) {
	t.mu.Lock()
	cancel := t.inWork[id]
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (t *tunnel) cancelAll() {
	t.mu.Lock()
	all := make([]context.CancelFunc, 0, len(t.inWork))
	for _, cancel := range t.inWork {
		all = append(all, cancel)
	}
	t.mu.Unlock()
	for _, cancel := range all {
		cancel()
	}
}

func (t *tunnel) done(id string) {
	t.mu.Lock()
	delete(t.inWork, id)
	t.mu.Unlock()
}

func (t *tunnel) send(reply tunnelReply) error {
	data, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	t.write.Lock()
	defer t.write.Unlock()
	// context.Background rather than the request's: a cancelled request still
	// has to say it ended, and writing the `end` frame with a dead context would
	// leave the console waiting for a response that was already over.
	return t.c.Write(context.Background(), websocket.MessageText, data)
}

func (t *tunnel) fail(id, message string) {
	_ = t.send(tunnelReply{ID: id, Kind: "error", Message: message})
}

// tunnelWriter is the [http.ResponseWriter] the handler writes into.
//
// It implements [http.Flusher] because [Server.handleSSE] refuses to run without
// one — and more to the point because the flush is the event: each flush becomes
// a frame, so a live stream arrives at the console as it is produced rather than
// when the response ends, which for SSE is never.
type tunnelWriter struct {
	t  *tunnel
	id string

	header http.Header
	buf    []byte

	wroteHead bool
	status    int
	failed    bool
}

var (
	_ http.ResponseWriter = (*tunnelWriter)(nil)
	_ http.Flusher        = (*tunnelWriter)(nil)
)

func (w *tunnelWriter) Header() http.Header { return w.header }

func (w *tunnelWriter) WriteHeader(status int) {
	if w.wroteHead {
		return
	}
	w.wroteHead = true
	w.status = status

	headers := map[string]string{}
	for k, v := range w.header {
		if tunnelReplyHeaders[strings.ToLower(k)] && len(v) > 0 {
			headers[k] = v[0]
		}
	}
	if err := w.t.send(tunnelReply{ID: w.id, Kind: "head", Status: status, Headers: headers}); err != nil {
		w.failed = true
	}
}

func (w *tunnelWriter) Write(p []byte) (int, error) {
	if !w.wroteHead {
		w.WriteHeader(http.StatusOK)
	}
	if w.failed {
		return 0, errors.New("api: the tunnel closed")
	}
	w.buf = append(w.buf, p...)
	// Buffered until Flush or the end of the handler, so an ordinary JSON
	// response is one frame however many times the encoder wrote. Except when
	// the buffer is large enough that holding it costs more than framing it.
	if len(w.buf) >= tunnelChunk {
		w.Flush()
	}
	return len(p), nil
}

func (w *tunnelWriter) Flush() {
	if !w.wroteHead {
		w.WriteHeader(http.StatusOK)
	}
	for len(w.buf) > 0 && !w.failed {
		chunk := w.buf
		if len(chunk) > tunnelChunk {
			chunk = chunk[:tunnelChunk]
		}
		w.buf = w.buf[len(chunk):]

		reply := tunnelReply{ID: w.id, Kind: "body"}
		if utf8.Valid(chunk) {
			reply.Data = string(chunk)
		} else {
			// See tunnelReply.Base64. Nothing this API returns should reach
			// here; the branch exists so that if something ever does, it
			// arrives intact rather than as replacement characters.
			reply.Data = base64.StdEncoding.EncodeToString(chunk)
			reply.Base64 = true
		}
		if err := w.t.send(reply); err != nil {
			w.failed = true
		}
	}
}

// finish flushes what is left and closes the response.
func (w *tunnelWriter) finish() {
	// A handler that returned without writing anything still produced a
	// response — 200 with an empty body — and the console has to be told, or it
	// waits forever on a request that is over.
	if !w.wroteHead {
		w.WriteHeader(http.StatusOK)
	}
	w.Flush()
	if !w.failed {
		_ = w.t.send(tunnelReply{ID: w.id, Kind: "end"})
	}
}
