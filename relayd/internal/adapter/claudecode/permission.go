package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The MCP server that --permission-prompt-tool calls.
//
// This is the needs-input path for Claude Code, and ADAPTERS.md §2 records
// every shape below from a live probe against 2.1.226:
//
//	request   {"tool_name":"Bash",
//	           "input":{"command":"touch /tmp/x","description":"Create empty file"},
//	           "tool_use_id":"toolu_01SYENeww5TLrR6ZWAJ7T7LZ"}
//	          plus _meta["claudecode/toolUseId"] and a progressToken
//
//	response  a single MCP text block whose text is a JSON *string*, not an
//	          object:
//	            allow  {"behavior":"allow","updatedInput":{…},"updatedPermissions":[…]}
//	            deny   {"behavior":"deny","message":"…","interrupt":false}
//
// On deny the session continues: the denial arrives as a normal tool result
// with is_error true, the model narrates it, and the run still exits 0 with
// result.subtype "success". interrupt: true is different — it aborts the turn
// outright, and it is the hard stop for "no, stop" spoken at the glasses.
//
// We may block here for as long as we need. The default MCP tool-call timeout
// is 1e8 ms, about 27.8 hours, capped near 24.8 days and overridable per
// server; progress notifications do not extend it. Holding this call open while
// the user is pinged, walks away and answers an hour later is well inside the
// budget, and that is what makes ADAPTERS.md §7's voice-answerable blocking real
// rather than aspirational.
//
// Two things this server will not do:
//
//   - It never returns updatedPermissions. That field persists a standing
//     grant, and ORCHESTRATOR.md §4b requires consequential actions to be
//     confirmed every time. For the same reason no allow_always option is ever
//     offered to the user.
//   - It never rewrites updatedInput on its own. The field is optional and
//     falls back to the original; when supplied it is schema-validated against
//     the target tool and a mismatch raises InputValidationError. It is passed
//     through only when the orchestrator explicitly set one.

// DefaultPermissionTimeout is Claude Code's own default MCP tool-call timeout:
// 1e8 ms, about 27.8 hours.
const DefaultPermissionTimeout = 100_000_000 * time.Millisecond

// mcpProtocolVersion is what we advertise when a client does not name one.
// A client that names a version gets its own back, which is what the MCP
// handshake asks for and what keeps this working across client versions.
const mcpProtocolVersion = "2025-06-18"

// PermissionRequest is one approval question, as Claude Code sends it.
type PermissionRequest struct {
	ToolName  string         `json:"tool_name"`
	Input     map[string]any `json:"input"`
	ToolUseID string         `json:"tool_use_id"`
}

// PermissionDecision is the payload that goes back inside the text block.
//
// updatedPermissions is deliberately absent from this struct: see above.
// Interrupt is a pointer so the two payloads marshal to exactly the shapes
// ADAPTERS.md §2 recorded — deny always carries the flag, allow never does.
type PermissionDecision struct {
	Behavior     string         `json:"behavior"`
	UpdatedInput map[string]any `json:"updatedInput,omitempty"`
	Message      string         `json:"message,omitempty"`
	Interrupt    *bool          `json:"interrupt,omitempty"`
}

// Allowed reports whether this decision lets the tool run.
func (d PermissionDecision) Allowed() bool { return d.Behavior == "allow" }

// Interrupts reports whether this decision also aborts the turn.
func (d PermissionDecision) Interrupts() bool { return d.Interrupt != nil && *d.Interrupt }

// Allow lets the tool run. updatedInput is optional and falls back to the
// original; when supplied it is schema-validated against the target tool and a
// mismatch raises InputValidationError, so it is passed through only when the
// orchestrator explicitly set one.
func Allow(updatedInput map[string]any) PermissionDecision {
	return PermissionDecision{Behavior: "allow", UpdatedInput: updatedInput}
}

// Deny refuses the tool. With interrupt false the session continues — the
// denial arrives as a tool result with is_error true and the model narrates it.
// With interrupt true the turn is aborted outright, which is the hard stop for
// "no, stop" spoken at the glasses.
func Deny(message string, interrupt bool) PermissionDecision {
	if message == "" {
		message = "denied"
	}
	return PermissionDecision{Behavior: "deny", Message: message, Interrupt: &interrupt}
}

// approver answers a permission question. *Session implements it; the tests
// implement it directly so the MCP layer can be exercised on its own.
type approver interface {
	Approve(ctx context.Context, req PermissionRequest) (PermissionDecision, error)
}

// --- JSON-RPC ---

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcInternalError  = -32603
)

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Meta      map[string]any  `json:"_meta"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolCallResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

// mcpServer is the transport-agnostic half: it takes a decoded JSON-RPC
// request and produces a response, or nil for a notification.
type mcpServer struct {
	log      *slog.Logger
	approver approver
	name     string
	version  string
}

func newMCPServer(a approver, log *slog.Logger) *mcpServer {
	if log == nil {
		log = slog.Default()
	}
	return &mcpServer{log: log, approver: a, name: "relay-permission", version: "1"}
}

func (s *mcpServer) handle(ctx context.Context, req rpcRequest) *rpcResponse {
	notification := len(req.ID) == 0 || string(req.ID) == "null"

	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := p.ProtocolVersion
		if version == "" {
			version = mcpProtocolVersion
		}
		return ok(req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.name, "version": s.version},
		})

	case "notifications/initialized", "notifications/cancelled":
		return nil

	case "ping":
		return ok(req.ID, map[string]any{})

	case "tools/list":
		return ok(req.ID, map[string]any{"tools": []any{s.toolDescriptor()}})

	case "tools/call":
		if notification {
			return nil
		}
		return s.call(ctx, req)

	default:
		if notification {
			s.log.Debug("claudecode: ignoring MCP notification", "method", req.Method)
			return nil
		}
		return fail(req.ID, rpcMethodNotFound, "no such method: "+req.Method)
	}
}

func (s *mcpServer) toolDescriptor() map[string]any {
	return map[string]any{
		"name": MCPToolName,
		"description": "Relay's permission prompt. Claude Code calls this instead of asking in a terminal; " +
			"the answer comes back from whoever is wearing the glasses.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tool_name":   map[string]any{"type": "string"},
				"input":       map[string]any{"type": "object"},
				"tool_use_id": map[string]any{"type": "string"},
			},
			"required": []string{"tool_name", "input"},
		},
	}
}

func (s *mcpServer) call(ctx context.Context, req rpcRequest) *rpcResponse {
	var p toolCallParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return fail(req.ID, rpcInvalidParams, "params: "+err.Error())
	}
	if p.Name != MCPToolName {
		return fail(req.ID, rpcMethodNotFound, "no such tool: "+p.Name)
	}

	var pr PermissionRequest
	if len(p.Arguments) > 0 {
		if err := json.Unmarshal(p.Arguments, &pr); err != nil {
			return fail(req.ID, rpcInvalidParams, "arguments: "+err.Error())
		}
	}
	// _meta["claudecode/toolUseId"] carries the same id and is the fallback
	// when arguments omit it.
	if pr.ToolUseID == "" {
		if v, okv := p.Meta["claudecode/toolUseId"].(string); okv {
			pr.ToolUseID = v
		}
	}
	if pr.ToolName == "" {
		return fail(req.ID, rpcInvalidParams, "tool_name is required")
	}

	decision, err := s.approver.Approve(ctx, pr)
	if err != nil {
		// A question nobody could answer must not hang the runtime. Denying is
		// the safe direction: the session continues, the model narrates the
		// refusal, and nothing destructive ran because we were unreachable.
		s.log.Warn("claudecode: permission question failed, denying",
			"tool", pr.ToolName, "tool_use_id", pr.ToolUseID, "err", err)
		decision = Deny("Relay could not reach anyone to approve this: "+err.Error(), false)
	}

	payload, err := json.Marshal(decision)
	if err != nil {
		return fail(req.ID, rpcInternalError, err.Error())
	}
	// The text of the single block is the JSON *string*, not an object.
	return ok(req.ID, toolCallResult{Content: []contentBlock{{Type: "text", Text: string(payload)}}})
}

func ok(id json.RawMessage, result any) *rpcResponse {
	return &rpcResponse{JSONRPC: "2.0", ID: idOrNull(id), Result: result}
}

func fail(id json.RawMessage, code int, msg string) *rpcResponse {
	return &rpcResponse{JSONRPC: "2.0", ID: idOrNull(id), Error: &rpcError{Code: code, Message: msg}}
}

func idOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

// --- transports ---

// ServeStdio runs the MCP server over a newline-delimited JSON-RPC pipe, which
// is what an MCP stdio server is. It exists so a helper process can bridge to
// relayd on a runtime where holding an HTTP request open for 27 hours is not
// acceptable; the in-process HTTP handler is the default.
func (s *mcpServer) ServeStdio(ctx context.Context, r io.Reader, w io.Writer) error {
	br := bufio.NewReader(r)
	enc := json.NewEncoder(w)
	for {
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
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.log.Warn("claudecode: undecodable MCP line", "err", err)
			continue
		}
		resp := s.handle(ctx, req)
		if resp == nil {
			continue
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
}

// permissionHub routes MCP calls to the session they belong to. One adapter
// runs one hub and one loopback listener; each session gets its own path, so a
// call can never be answered by the wrong conversation.
type permissionHub struct {
	log *slog.Logger

	mu       sync.RWMutex
	sessions map[string]approver
}

func newPermissionHub(log *slog.Logger) *permissionHub {
	if log == nil {
		log = slog.Default()
	}
	return &permissionHub{log: log, sessions: map[string]approver{}}
}

func (h *permissionHub) register(id string, a approver) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessions[id] = a
}

func (h *permissionHub) unregister(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sessions, id)
}

func (h *permissionHub) lookup(id string) (approver, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	a, okv := h.sessions[id]
	return a, okv
}

// pathPrefix is where the hub is mounted.
const pathPrefix = "/mcp/"

// endpoint builds the URL for one session.
func endpointFor(base, sessionID string) string {
	return strings.TrimSuffix(base, "/") + pathPrefix + sessionID
}

// ServeHTTP implements the streamable-HTTP MCP transport, JSON responses only.
// It binds to loopback and every request is one JSON-RPC message.
func (h *permissionHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// GET is the SSE half of streamable HTTP. We never push
		// server-initiated messages, so there is nothing to stream.
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "this MCP endpoint accepts POST only", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, pathPrefix)
	if id == "" || strings.Contains(id, "/") {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	a, okv := h.lookup(id)
	if !okv {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "undecodable JSON-RPC", http.StatusBadRequest)
		return
	}

	srv := newMCPServer(a, h.log)
	resp := srv.handle(r.Context(), req)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.log.Warn("claudecode: could not write MCP response", "err", err)
	}
}

// readLine reads one NDJSON line of any length. bufio.Scanner would cap it.
func readLine(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadBytes('\n')
	if len(line) > 0 && err == io.EOF {
		return line, nil
	}
	return line, err
}

// prompt renders a permission question into something short enough to speak.
// ADAPTERS.md §6 budgets ~120 characters plus the options for needs-input, and
// it never invents a description: it prefers what the tool itself said, then
// the field that names what the tool acts on, then the bare tool name.
func (p PermissionRequest) prompt() string {
	if d, okv := p.Input["description"].(string); okv && strings.TrimSpace(d) != "" {
		return clip(p.ToolName+": "+strings.TrimSpace(d), 160)
	}
	if t := toolTarget(p.ToolName, p.Input); t != "" {
		return clip(p.ToolName+": "+t, 160)
	}
	return clip(p.ToolName, 160)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

var errNoAnswer = errors.New("claudecode: nobody answered the permission question")

func timeoutError(d time.Duration) error {
	return fmt.Errorf("%w within %s", errNoAnswer, d)
}
