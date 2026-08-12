package mcp

import "encoding/json"

// ProtocolVersion is the MCP revision this gateway speaks when a client does
// not name one. A client that names a version gets its own back, which is what
// the handshake asks for and what keeps one gateway working across five
// runtimes' client libraries.
const ProtocolVersion = "2025-06-18"

// JSON-RPC 2.0 error codes, plus the two this gateway adds.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603

	// CodeNotGranted is a tool the caller may not use. It is a distinct code so
	// a runtime can tell "you have not connected this" apart from "that tool
	// does not exist" — the two lead to different next steps for the user.
	CodeNotGranted = -32001

	// CodeDenied is a consequential action the user refused at the glasses, or
	// one nobody could be asked about.
	CodeDenied = -32002
)

// Request is one JSON-RPC message from a client. A message with no id is a
// notification and gets no response.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Notification reports whether this message expects no reply.
func (r Request) Notification() bool {
	return len(r.ID) == 0 || string(r.ID) == "null"
}

// Response is one JSON-RPC reply.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is the error half of a response.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

// Note is a server→client message with no id: `notifications/tools/list_changed`
// is the only one this gateway sends, and it is the whole answer to SYSTEM.md
// §9 problem 6 for any client that can receive it.
type Note struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// MethodToolsListChanged is the notification a client that advertised
// capabilities.tools.listChanged gets when the tool list moves.
const MethodToolsListChanged = "notifications/tools/list_changed"

func result(id json.RawMessage, v any) *Response {
	return &Response{JSONRPC: "2.0", ID: idOrNull(id), Result: v}
}

func failure(id json.RawMessage, code int, msg string, data any) *Response {
	return &Response{JSONRPC: "2.0", ID: idOrNull(id), Error: &RPCError{Code: code, Message: msg, Data: data}}
}

func idOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

// ---------------------------------------------------------------- content --

// Content is one block of a tool result. Text is the only kind this gateway
// produces: a connector that wants to return an image returns a resource link
// in text rather than inlining bytes the five clients disagree about.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// TextContent builds one text block.
func TextContent(s string) Content { return Content{Type: "text", Text: s} }

// CallToolResult is the MCP shape of a tool's answer.
type CallToolResult struct {
	Content []Content `json:"content"`
	// StructuredContent is the machine-readable half. For a connector it is the
	// normalized envelope from SYSTEM.md §3.4, so an agent never learns a
	// vendor's shape even when it reads the payload.
	StructuredContent any  `json:"structuredContent,omitempty"`
	IsError           bool `json:"isError,omitempty"`
}

// ------------------------------------------------------------- handshake --

type initializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    clientCapabilities `json:"capabilities"`
	ClientInfo      Implementation     `json:"clientInfo"`
}

type clientCapabilities struct {
	Roots *struct {
		ListChanged bool `json:"listChanged"`
	} `json:"roots,omitempty"`
	Sampling     map[string]any `json:"sampling,omitempty"`
	Elicitation  map[string]any `json:"elicitation,omitempty"`
	Experimental map[string]any `json:"experimental,omitempty"`
}

// Implementation is the {name, version} both sides send.
type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version,omitempty"`
}

type initializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    serverCapabilities `json:"capabilities"`
	ServerInfo      Implementation     `json:"serverInfo"`
	Instructions    string             `json:"instructions,omitempty"`
}

type serverCapabilities struct {
	Tools *toolsCapability `json:"tools,omitempty"`
}

type toolsCapability struct {
	// ListChanged is advertised only when the transport can actually deliver a
	// notification. A POST-only HTTP client has no channel for one, and
	// advertising the capability there would be a promise the gateway cannot
	// keep — the exact shape of failure ADAPTERS.md §5 forbids.
	ListChanged bool `json:"listChanged"`
}

type callToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	Meta      map[string]any `json:"_meta,omitempty"`
}
