package acp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/luthor007/relay/relayd/internal/adapter"
)

// ProtocolVersion is ACP's wire version — a uint16 bumped only for breaking
// changes. It is the one constant that is *not* recoverable from the vendored
// schema: it lives in the package's typescript/schema.ts, and acp-methods.md
// records it. Today it is 1.
const ProtocolVersion = 1

// The wire method names, exactly as they appear in the schema's `x-method`
// annotations. Nothing in this package spells a method inline.
const (
	// client → agent
	methodInitialize   = "initialize"
	methodAuthenticate = "authenticate"
	methodSessionNew   = "session/new"
	methodSessionLoad  = "session/load"
	methodPrompt       = "session/prompt"
	methodCancel       = "session/cancel"
	methodSetMode      = "session/set_mode"
	methodSetModel     = "session/set_model"

	// agent → client
	methodSessionUpdate      = "session/update"
	methodRequestPermission  = "session/request_permission"
	methodFSReadTextFile     = "fs/read_text_file"
	methodFSWriteTextFile    = "fs/write_text_file"
	methodTerminalCreate     = "terminal/create"
	methodTerminalOutput     = "terminal/output"
	methodTerminalWaitExit   = "terminal/wait_for_exit"
	methodTerminalKill       = "terminal/kill"
	methodTerminalRelease    = "terminal/release"
	extensionMethodPrefix    = "_"
	clientCapabilityRequired = "client capability not advertised"
)

// AgentMethods is the eight client→agent methods and ClientMethods the nine
// agent→client ones. Seventeen in total, and a schema that grows an eighteenth
// is the signal ADAPTERS.md §4 has to be re-read.
func AgentMethods() []string {
	return []string{
		methodInitialize, methodAuthenticate, methodSessionNew, methodSessionLoad,
		methodPrompt, methodCancel, methodSetMode, methodSetModel,
	}
}

// ClientMethods lists what the agent may call on us.
func ClientMethods() []string {
	return []string{
		methodSessionUpdate, methodRequestPermission,
		methodFSReadTextFile, methodFSWriteTextFile,
		methodTerminalCreate, methodTerminalOutput, methodTerminalWaitExit,
		methodTerminalKill, methodTerminalRelease,
	}
}

// RefusedClientMethods is the seven the adapter answers -32601 to. They are
// refused rather than "must-answer" because Relay never advertised the matching
// client capability, and an agent that calls one anyway is out of contract.
func RefusedClientMethods() []string {
	return []string{
		methodFSReadTextFile, methodFSWriteTextFile,
		methodTerminalCreate, methodTerminalOutput, methodTerminalWaitExit,
		methodTerminalKill, methodTerminalRelease,
	}
}

// JSON-RPC error codes: the five stock ones plus ACP's two additions.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
	// CodeAuthRequired is ACP's -32000. session/new answers with it when the
	// runtime is not logged in; recovery is authenticate with one of the ids
	// from the handshake, and that belongs in the installer.
	CodeAuthRequired = -32000
	// CodeResourceNotFound is ACP's -32002.
	CodeResourceNotFound = -32002
)

// ---------- JSON-RPC envelope ----------

// message is one line on the wire. ACP is stock JSON-RPC 2.0 (unlike Codex,
// which omits the envelope field), so writes always carry "jsonrpc":"2.0";
// reads tolerate its absence rather than rejecting a runtime over a field
// nothing is keyed on.
type message struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type messageKind int

const (
	kindUnknown messageKind = iota
	kindRequest
	kindNotification
	kindResponse
	kindError
)

// kind demultiplexes on field presence. An id with a method is a request; a
// method alone is a notification; an id with a result or an error is a
// response. A response to a method that returns nothing carries `"result":{}`,
// so Result must be checked for presence, not for emptiness.
func (m *message) kind() messageKind {
	switch {
	case m.Method != "" && len(m.ID) > 0 && !isJSONNull(m.ID):
		return kindRequest
	case m.Method != "":
		return kindNotification
	case m.Error != nil:
		return kindError
	case len(m.Result) > 0:
		return kindResponse
	}
	return kindUnknown
}

func isJSONNull(b json.RawMessage) bool {
	return len(b) == 4 && string(b) == "null"
}

// RPCError is a JSON-RPC error object. Code is the part callers switch on:
// CodeAuthRequired is the one with a documented recovery.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if len(e.Data) > 0 {
		return fmt.Sprintf("acp: rpc error %d: %s (%s)", e.Code, e.Message, string(e.Data))
	}
	return fmt.Sprintf("acp: rpc error %d: %s", e.Code, e.Message)
}

// ---------- initialize ----------

type fileSystemCapability struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

type clientCapabilities struct {
	FS       fileSystemCapability `json:"fs"`
	Terminal bool                 `json:"terminal"`
}

// relayClientCapabilities is ADAPTERS.md §4's decision, in one place: all three
// false. Everything the agent does to the filesystem or a shell then has to
// come back as a tool_call we can narrate.
func relayClientCapabilities() clientCapabilities { return clientCapabilities{} }

type initializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities clientCapabilities `json:"clientCapabilities"`
}

// PromptCapabilities is what the agent will accept in a prompt beyond the
// baseline of text and resource_link. Each defaults to false.
type PromptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
}

// MCPCapabilities is which MCP transports the agent can be handed. stdio is
// mandatory for every agent and therefore has no flag.
type MCPCapabilities struct {
	HTTP bool `json:"http"`
	SSE  bool `json:"sse"`
}

// AgentCapabilities is what came back from the handshake.
type AgentCapabilities struct {
	LoadSession        bool               `json:"loadSession"`
	PromptCapabilities PromptCapabilities `json:"promptCapabilities"`
	MCPCapabilities    MCPCapabilities    `json:"mcpCapabilities"`
}

// AuthMethod is one entry of the handshake's authMethods[]. Its Id is what
// authenticate takes after a -32000.
type AuthMethod struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description *string `json:"description,omitempty"`
}

type initializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
	AuthMethods       []AuthMethod      `json:"authMethods"`
}

type authenticateParams struct {
	MethodID string `json:"methodId"`
}

// ---------- sessions ----------

type envVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type httpHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// mcpServer is the union from the schema, flattened. The `type` field is absent
// for stdio (the stdio branch has no discriminator at all) and "http" or "sse"
// for the two URL transports.
type mcpServer struct {
	Type    string        `json:"type,omitempty"`
	Name    string        `json:"name"`
	Command string        `json:"command,omitempty"`
	Args    []string      `json:"args,omitempty"`
	Env     []envVariable `json:"env,omitempty"`
	URL     string        `json:"url,omitempty"`
	Headers []httpHeader  `json:"headers,omitempty"`
}

type newSessionParams struct {
	CWD        string      `json:"cwd"`
	MCPServers []mcpServer `json:"mcpServers"`
}

// SessionMode is one of the modes an agent can operate in.
type SessionMode struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description *string `json:"description,omitempty"`
}

// SessionModeState is the set of modes plus the current one.
type SessionModeState struct {
	CurrentModeID  string        `json:"currentModeId"`
	AvailableModes []SessionMode `json:"availableModes"`
}

// ModelInfo is one selectable model. UNSTABLE upstream.
type ModelInfo struct {
	ModelID     string  `json:"modelId"`
	Name        string  `json:"name"`
	Description *string `json:"description,omitempty"`
}

// SessionModelState is the set of models plus the current one. UNSTABLE
// upstream — it and session/set_model are the only members of the surface the
// spec has not settled.
type SessionModelState struct {
	CurrentModelID  string      `json:"currentModelId"`
	AvailableModels []ModelInfo `json:"availableModels"`
}

type newSessionResult struct {
	SessionID string             `json:"sessionId"`
	Modes     *SessionModeState  `json:"modes,omitempty"`
	Models    *SessionModelState `json:"models,omitempty"`
}

type loadSessionParams struct {
	SessionID  string      `json:"sessionId"`
	CWD        string      `json:"cwd"`
	MCPServers []mcpServer `json:"mcpServers"`
}

type loadSessionResult struct {
	Modes  *SessionModeState  `json:"modes,omitempty"`
	Models *SessionModelState `json:"models,omitempty"`
}

type promptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []contentBlock `json:"prompt"`
}

type promptResult struct {
	StopReason string `json:"stopReason"`
}

type cancelParams struct {
	SessionID string `json:"sessionId"`
}

type setModeParams struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
}

type setModelParams struct {
	SessionID string `json:"sessionId"`
	ModelID   string `json:"modelId"`
}

// ---------- content ----------

type contentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	Data     string          `json:"data,omitempty"`
	MIMEType string          `json:"mimeType,omitempty"`
	URI      string          `json:"uri,omitempty"`
	Name     string          `json:"name,omitempty"`
	Resource json.RawMessage `json:"resource,omitempty"`
}

type textResourceContents struct {
	URI      string `json:"uri"`
	Text     string `json:"text"`
	MIMEType string `json:"mimeType,omitempty"`
}

type blobResourceContents struct {
	URI      string `json:"uri"`
	Blob     string `json:"blob"`
	MIMEType string `json:"mimeType,omitempty"`
}

// encodeBlocks turns a Relay turn into ACP content blocks. It never guesses a
// capability: the caller has already run adapter.CheckTurn against the
// handshake's promptCapabilities, so anything past text and resource_link that
// reaches here was admitted deliberately.
func encodeBlocks(t adapter.Turn) ([]contentBlock, error) {
	var out []contentBlock
	if t.Text != "" {
		out = append(out, contentBlock{Type: "text", Text: t.Text})
	}
	for i, b := range t.Blocks {
		switch b.Kind {
		case adapter.BlockText:
			out = append(out, contentBlock{Type: "text", Text: b.Text})
		case adapter.BlockResourceLink:
			name := b.Text
			if name == "" {
				name = b.URI
			}
			if b.URI == "" {
				return nil, fmt.Errorf("acp: block %d is a resource_link with no uri", i)
			}
			out = append(out, contentBlock{Type: "resource_link", Name: name, URI: b.URI, MIMEType: b.MIMEType})
		case adapter.BlockImage:
			out = append(out, contentBlock{
				Type:     "image",
				Data:     base64.StdEncoding.EncodeToString(b.Data),
				MIMEType: b.MIMEType,
				URI:      b.URI,
			})
		case adapter.BlockAudio:
			out = append(out, contentBlock{
				Type:     "audio",
				Data:     base64.StdEncoding.EncodeToString(b.Data),
				MIMEType: b.MIMEType,
			})
		case adapter.BlockEmbeddedContext:
			var res any
			if b.Text != "" || len(b.Data) == 0 {
				res = textResourceContents{URI: b.URI, Text: b.Text, MIMEType: b.MIMEType}
			} else {
				res = blobResourceContents{
					URI:      b.URI,
					Blob:     base64.StdEncoding.EncodeToString(b.Data),
					MIMEType: b.MIMEType,
				}
			}
			raw, err := json.Marshal(res)
			if err != nil {
				return nil, fmt.Errorf("acp: block %d: %w", i, err)
			}
			out = append(out, contentBlock{Type: "resource", Resource: raw})
		default:
			return nil, fmt.Errorf("acp: block %d has unknown kind %q", i, b.Kind)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("acp: a prompt must carry at least one content block")
	}
	return out, nil
}

// blockText is the speakable part of a content block, and "" for everything
// else. An image in an agent_message_chunk has no TextDelta to become, and
// inventing one would be exactly the kind of prose-shaped guess §5 forbids —
// the caller counts the drop instead.
func blockText(b contentBlock) string {
	if b.Type == "text" {
		return b.Text
	}
	return ""
}

// ---------- session/update ----------

type sessionNotification struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
}

// The eight sessionUpdate discriminants. A ninth is a change to ADAPTERS.md §5.
const (
	updateUserMessageChunk  = "user_message_chunk"
	updateAgentMessageChunk = "agent_message_chunk"
	updateAgentThoughtChunk = "agent_thought_chunk"
	updateToolCall          = "tool_call"
	updateToolCallUpdate    = "tool_call_update"
	updatePlan              = "plan"
	updateAvailableCommands = "available_commands_update"
	updateCurrentMode       = "current_mode_update"
)

// UpdateVariants lists the eight in schema order.
func UpdateVariants() []string {
	return []string{
		updateUserMessageChunk, updateAgentMessageChunk, updateAgentThoughtChunk,
		updateToolCall, updateToolCallUpdate, updatePlan,
		updateAvailableCommands, updateCurrentMode,
	}
}

type updateEnvelope struct {
	SessionUpdate string `json:"sessionUpdate"`
}

type chunkUpdate struct {
	Content contentBlock `json:"content"`
}

type toolCallLocation struct {
	Path string  `json:"path"`
	Line *uint32 `json:"line,omitempty"`
}

type toolCallContent struct {
	Type string `json:"type"`
	// content
	Content *contentBlock `json:"content,omitempty"`
	// diff
	Path    string  `json:"path,omitempty"`
	NewText string  `json:"newText,omitempty"`
	OldText *string `json:"oldText,omitempty"`
	// terminal
	TerminalID string `json:"terminalId,omitempty"`
}

// toolCallUpdate covers both `tool_call` and `tool_call_update`: the former
// requires toolCallId and title, the latter only toolCallId, and every other
// field is nullable on both. Pointers keep "absent" distinguishable from
// "explicitly cleared" — an empty content array means replace-with-nothing,
// which is not the same as saying nothing about content.
type toolCallUpdate struct {
	ToolCallID string              `json:"toolCallId"`
	Title      *string             `json:"title,omitempty"`
	Kind       *string             `json:"kind,omitempty"`
	Status     *string             `json:"status,omitempty"`
	Content    *[]toolCallContent  `json:"content,omitempty"`
	Locations  *[]toolCallLocation `json:"locations,omitempty"`
	RawInput   json.RawMessage     `json:"rawInput,omitempty"`
	RawOutput  json.RawMessage     `json:"rawOutput,omitempty"`
}

// PlanEntry is one line of the agent's own plan.
type PlanEntry struct {
	Content  string `json:"content"`
	Priority string `json:"priority"`
	Status   string `json:"status"`
}

type planUpdate struct {
	Entries []PlanEntry `json:"entries"`
}

// AvailableCommandInput is the schema's one-branch union: all text typed after
// the command name becomes the input.
type AvailableCommandInput struct {
	Hint string `json:"hint"`
}

// AvailableCommand is one slash command the runtime currently offers. The
// available_commands_update variant is ACP's answer to SYSTEM.md §9's
// tool-list-refresh problem: the set is pushed, not polled.
type AvailableCommand struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Input       *AvailableCommandInput `json:"input,omitempty"`
}

type availableCommandsUpdate struct {
	AvailableCommands []AvailableCommand `json:"availableCommands"`
}

type currentModeUpdate struct {
	CurrentModeID string `json:"currentModeId"`
}

// ---------- session/request_permission ----------

// PermissionOption is one answer the agent will accept. Kind is a UI hint, not
// a fixed menu — the array is agent-supplied and open-ended.
type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

type requestPermissionParams struct {
	SessionID string             `json:"sessionId"`
	ToolCall  toolCallUpdate     `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
}

type permissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

type requestPermissionResult struct {
	Outcome permissionOutcome `json:"outcome"`
}

// ---------- helpers ----------

func splitEnv(kv string) (string, string) {
	if i := strings.IndexByte(kv, '='); i >= 0 {
		return kv[:i], kv[i+1:]
	}
	return kv, ""
}

// rawObject decodes a rawInput/rawOutput that may be any JSON value at all.
// ToolStarted.RawInput is a map, so a non-object raw input is reported as
// absent rather than coerced into a shape it does not have.
func rawObject(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}
