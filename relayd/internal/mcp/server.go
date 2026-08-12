package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/luthor007/relay/relayd/internal/adapter"
)

// Server is one client connection to the gateway: one runtime's MCP client,
// with whatever it told us about itself at initialize.
//
// It exists as a type rather than as a handler function because the tool-list
// refresh problem needs it. SYSTEM.md §9 problem 6 turns on whether *this*
// connection can be told that the list changed, and the honest answer differs
// per connection, not per runtime: the same runtime over stdio can be pushed a
// notification and over POST-only HTTP cannot.
type Server struct {
	gw *Gateway
	id string
	// canPush is whether this connection's transport has a server→client
	// channel at all. Both transports the gateway serves do — stdio inherently,
	// streamable HTTP through the stream the client opens with GET — so this is
	// the seam a third transport would have to answer honestly rather than a
	// field that is always true by accident.
	canPush bool

	mu          sync.Mutex
	session     string
	runtime     string
	client      Implementation
	version     string
	initialized bool
	listChanged bool
	notify      func(Note) error
}

// ID is the connection id.
func (s *Server) ID() string { return s.id }

// Session is the Relay session this connection belongs to, or "" when the
// caller never said.
func (s *Server) Session() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session
}

// Runtime is which of the five is on the other end, resolved from clientInfo,
// or "" when the name matched none of them.
func (s *Server) Runtime() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runtime
}

// Client is the clientInfo verbatim.
func (s *Server) Client() Implementation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

// Initialized reports whether the handshake completed.
func (s *Server) Initialized() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initialized
}

// CanNotify reports whether this connection has a channel back to the client.
// It is the fact the refresh planner is built on, and it is observed rather
// than assumed: a stdio pipe is bidirectional, an SSE stream is attached or it
// is not, and a bare POST is neither.
func (s *Server) CanNotify() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.notify != nil
}

// setNotify attaches or detaches the server→client channel.
func (s *Server) setNotify(fn func(Note) error) {
	s.mu.Lock()
	s.notify = fn
	s.mu.Unlock()
}

// Notify pushes one server→client message.
func (s *Server) Notify(n Note) error {
	s.mu.Lock()
	fn := s.notify
	s.mu.Unlock()
	if fn == nil {
		return errNoChannel
	}
	return fn(n)
}

var errNoChannel = errors.New("mcp: this connection has no channel back to the client")

// ToolsChanged pushes `notifications/tools/list_changed`.
func (s *Server) ToolsChanged() error {
	return s.Notify(Note{JSONRPC: "2.0", Method: MethodToolsListChanged})
}

// bind records what the caller said about itself.
func (s *Server) bind(session, runtime string) {
	s.mu.Lock()
	if session != "" {
		s.session = session
	}
	if runtime != "" && s.runtime == "" {
		s.runtime = runtime
	}
	s.mu.Unlock()
}

// RuntimeFor maps an MCP clientInfo.name onto one of the five runtimes.
//
// It matches on substring because each product decorates its own name
// ("claude-code", "Claude Code", "codex-cli"). A name that matches none of them
// returns "", and every consumer of that value treats it as unknown rather than
// guessing — an unattributed call is a real state and mislabelling one is worse
// than admitting it.
func RuntimeFor(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return ""
	}
	// OpenCode before OpenClaw is not enough — "opencode" and "openclaw" share
	// no prefix past "open", so plain containment is unambiguous here. Claude
	// Code is checked before a bare "claude" for the same reason.
	for _, rt := range []adapter.Runtime{
		adapter.ClaudeCode, adapter.Codex, adapter.OpenClaw, adapter.Hermes, adapter.OpenCode,
	} {
		if strings.Contains(n, string(rt)) {
			return string(rt)
		}
	}
	if strings.Contains(n, "claude") {
		return string(adapter.ClaudeCode)
	}
	return ""
}

// Handle processes one JSON-RPC message and returns the response, or nil for a
// notification.
func (s *Server) Handle(ctx context.Context, req Request) *Response {
	switch req.Method {
	case "initialize":
		return s.initialize(req)

	case "notifications/initialized":
		s.mu.Lock()
		s.initialized = true
		s.mu.Unlock()
		return nil

	case "notifications/cancelled", "notifications/progress", "notifications/roots/list_changed":
		return nil

	case "ping":
		if req.Notification() {
			return nil
		}
		return result(req.ID, map[string]any{})

	case "tools/list":
		if req.Notification() {
			return nil
		}
		tools := s.gw.Tools(ctx)
		list := make([]any, 0, len(tools))
		for _, t := range tools {
			list = append(list, t.descriptor())
		}
		return result(req.ID, map[string]any{"tools": list})

	case "tools/call":
		if req.Notification() {
			return nil
		}
		return s.call(ctx, req)

	// The gateway serves tools and nothing else. Answering these with an empty
	// list rather than "no such method" is deliberate: several MCP clients treat
	// a method error at startup as a broken server and drop the connection,
	// which would take the tools with it.
	case "resources/list":
		return result(req.ID, map[string]any{"resources": []any{}})
	case "resources/templates/list":
		return result(req.ID, map[string]any{"resourceTemplates": []any{}})
	case "prompts/list":
		return result(req.ID, map[string]any{"prompts": []any{}})

	default:
		if req.Notification() {
			return nil
		}
		return failure(req.ID, CodeMethodNotFound, "no such method: "+req.Method, nil)
	}
}

func (s *Server) initialize(req Request) *Response {
	var p initializeParams
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &p)
	}
	version := p.ProtocolVersion
	if version == "" {
		version = ProtocolVersion
	}

	s.mu.Lock()
	s.client = p.ClientInfo
	s.version = version
	if rt := RuntimeFor(p.ClientInfo.Name); rt != "" {
		s.runtime = rt
	}
	// listChanged says the server may send the notification, which is a
	// property of the transport rather than of this instant: a streamable-HTTP
	// client opens its stream after initialize, so deciding on whether one is
	// attached right now would answer false to every HTTP client forever. What
	// stays strictly observed is the refresh planner, which reports "notified"
	// only for a write that actually landed.
	s.listChanged = s.canPush
	canPush := s.canPush
	s.mu.Unlock()

	caps := serverCapabilities{Tools: &toolsCapability{ListChanged: canPush}}
	return result(req.ID, initializeResult{
		ProtocolVersion: version,
		Capabilities:    caps,
		ServerInfo:      Implementation{Name: s.gw.opts.Name, Version: s.gw.opts.Version},
		Instructions: "Relay's shared tool bus. Every connector you can see here has been " +
			"granted by the person who owns this machine; anything not granted is not " +
			"listed. Tools whose description says Relay confirms out loud will stop and " +
			"ask before they run, every time.",
	})
}

func (s *Server) call(ctx context.Context, req Request) *Response {
	var p callToolParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return failure(req.ID, CodeInvalidParams, "params: "+err.Error(), nil)
	}
	if strings.TrimSpace(p.Name) == "" {
		return failure(req.ID, CodeInvalidParams, "name is required", nil)
	}

	c := Call{
		Tool:      p.Name,
		Arguments: p.Arguments,
		Session:   s.Session(),
		Runtime:   s.Runtime(),
		Client:    s.Client().Name,
	}
	// _meta is where a runtime can name the session and turn it is calling from.
	// The per-session endpoint is the primary source; this is the fallback for a
	// runtime pointed at the shared one.
	if v, ok := p.Meta["relay/session"].(string); ok && v != "" && c.Session == "" {
		c.Session = v
		s.bind(v, "")
	}
	if v, ok := p.Meta["relay/turn"].(string); ok {
		c.Turn = v
	}

	res, err := s.gw.Call(ctx, c)
	switch {
	case err == nil:
		out := CallToolResult{Content: []Content{TextContent(res.Text)}, IsError: res.IsError}
		if res.Structured != nil {
			out.StructuredContent = res.Structured
		}
		return result(req.ID, out)

	case errors.Is(err, ErrNoSuchTool):
		return failure(req.ID, CodeMethodNotFound, err.Error(), nil)

	case errors.Is(err, ErrNotGranted):
		var ng *NotGrantedError
		var data any
		if errors.As(err, &ng) {
			data = map[string]any{
				"connector": ng.Connector,
				"access":    string(ng.Access),
				"scope":     ng.Access.Scope(ng.Connector),
			}
		}
		return failure(req.ID, CodeNotGranted, err.Error(), data)

	case errors.Is(err, ErrDenied), errors.Is(err, ErrNoConfirmer), errors.Is(err, ErrUnanswered):
		return failure(req.ID, CodeDenied, err.Error(), nil)

	default:
		// A tool that ran and failed is a result the model can react to, not a
		// protocol error. Only failures before the tool ran reach the branches
		// above.
		return result(req.ID, CallToolResult{
			Content: []Content{TextContent(err.Error())},
			IsError: true,
		})
	}
}

// ------------------------------------------------------------------ stdio --

// ServeStdio runs one MCP connection over a newline-delimited JSON-RPC pipe.
//
// stdio is the transport four of the five runtimes reach for by default, and it
// is bidirectional, so a connection opened this way can be told the tool list
// changed. That is the good case for SYSTEM.md §9 problem 6.
func (g *Gateway) ServeStdio(ctx context.Context, r io.Reader, w io.Writer) error {
	s := &Server{gw: g, id: g.opts.NewID(), canPush: true}

	var wmu sync.Mutex
	enc := json.NewEncoder(w)
	write := func(v any) error {
		wmu.Lock()
		defer wmu.Unlock()
		return enc.Encode(v)
	}
	s.setNotify(func(n Note) error { return write(n) })

	g.attach(s)
	defer g.detach(s.id)

	br := bufio.NewReader(r)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line, err := readLine(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			g.log.Warn("mcp: undecodable line on stdio", "err", err)
			if werr := write(failure(nil, CodeParseError, "undecodable JSON-RPC", nil)); werr != nil {
				return werr
			}
			continue
		}
		resp := s.Handle(ctx, req)
		if resp == nil {
			continue
		}
		if err := write(resp); err != nil {
			return err
		}
	}
}

func readLine(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadBytes('\n')
	if len(line) > 0 && errors.Is(err, io.EOF) {
		return line, nil
	}
	return line, err
}

// ------------------------------------------------------------------- HTTP --

// HTTPPrefix is where the gateway mounts. A trailing path segment names the
// Relay session, which is what lets a call be attributed without the runtime
// having to cooperate.
const HTTPPrefix = "/mcp/"

// SessionHeader is MCP's streamable-HTTP session header. The gateway mints one
// at initialize and honours it afterwards, which is what lets a POST and an SSE
// stream from the same client be the same connection.
const SessionHeader = "Mcp-Session-Id"

// MaxBodyBytes bounds one request body.
const MaxBodyBytes = 8 << 20

// HTTPHandler serves the gateway over streamable HTTP: POST for messages, GET
// for the server→client stream.
//
// Bind it to loopback. It carries the whole tool bus, and DASHBOARD.md §4's
// rule about deliberate non-loopback binds applies at least as hard here as it
// does to the console.
func (g *Gateway) HTTPHandler() http.Handler { return &httpHandler{gw: g} }

type httpHandler struct {
	gw *Gateway
}

// get returns the connection for a key, creating it if this is the first
// message on it. There is one connection registry and it lives on the gateway:
// a second map here would let a swept connection be resurrected by a POST and
// then never receive a notification, which is the failure this whole file is
// about.
func (h *httpHandler) get(key string, relaySession string) *Server {
	return h.gw.connection(key, relaySession, true)
}

func (h *httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	relaySession := strings.Trim(strings.TrimPrefix(r.URL.Path, HTTPPrefix), "/")
	if strings.Contains(relaySession, "/") {
		http.Error(w, "unknown endpoint", http.StatusNotFound)
		return
	}
	key := r.Header.Get(SessionHeader)
	if key == "" {
		if relaySession != "" {
			key = "path:" + relaySession
		} else {
			key = "shared"
		}
	}

	switch r.Method {
	case http.MethodGet:
		h.stream(w, r, key, relaySession)
	case http.MethodPost:
		h.post(w, r, key, relaySession)
	case http.MethodDelete:
		h.gw.detach(key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "this MCP endpoint accepts GET, POST and DELETE", http.StatusMethodNotAllowed)
	}
}

func (h *httpHandler) post(w http.ResponseWriter, r *http.Request, key, relaySession string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, failure(nil, CodeParseError, "undecodable JSON-RPC", nil))
		return
	}

	s := h.get(key, relaySession)
	resp := s.Handle(r.Context(), req)
	w.Header().Set(SessionHeader, key)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// stream is the server→client half. Without it the gateway can never tell an
// HTTP client that the tool list moved, and every mid-session grant on that
// client degrades to "restart it yourself".
func (h *httpHandler) stream(w http.ResponseWriter, r *http.Request, key, relaySession string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "this server cannot stream", http.StatusNotImplemented)
		return
	}
	s := h.get(key, relaySession)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set(SessionHeader, key)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	var mu sync.Mutex
	s.setNotify(func(n Note) error {
		b, err := json.Marshal(n)
		if err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		if _, err := fmt.Fprintf(w, "event: message\ndata: %s\n\n", b); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	})
	defer s.setNotify(nil)

	<-r.Context().Done()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
