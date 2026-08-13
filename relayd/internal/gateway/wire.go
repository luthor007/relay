package gateway

import (
	"encoding/json"
	"runtime"
)

// Frame types. Every frame on this socket is one of three things.
const (
	frameReq   = "req"
	frameRes   = "res"
	frameEvent = "event"
)

// Events relayd cares about. The gateway emits more; these are the ones with a
// meaning in Relay's loop.
const (
	// EventConnectChallenge is the gateway's opening frame, and the thing the
	// connect frame answers.
	EventConnectChallenge = "connect.challenge"
	// EventSessionsChanged fires on create, send, run start and run end. It is
	// the closest thing the bus has to relayd's registry change feed.
	EventSessionsChanged = "sessions.changed"
	// EventAgent carries one run's lifecycle, assistant deltas and tool calls,
	// distinguished by the payload's stream field.
	EventAgent = "agent"
	// EventChat carries the assembled assistant message, delta then final.
	EventChat = "chat"
	// EventSessionMessage carries a persisted message, user or assistant.
	EventSessionMessage = "session.message"
	// EventExecApprovalRequested is a command waiting on a human.
	//
	// Read the note on [ExecApprovalRequest] before building a surface on it:
	// sessions driven by the claude-cli runtime never raise one.
	EventExecApprovalRequested = "exec.approval.requested"
	// EventExecApprovalResolved is that command being allowed or denied, by
	// this client or by another one — which is what lets relayd retract a ping
	// when the user answered in their terminal instead.
	EventExecApprovalResolved = "exec.approval.resolved"
	// EventTick is the gateway's heartbeat, every 30s by its own policy.
	EventTick = "tick"
	// EventShutdown is the gateway saying it is going away on purpose.
	EventShutdown = "shutdown"
	// EventHealth is the periodic health snapshot.
	EventHealth = "health"
)

// Agent event streams, the payload's stream field on [EventAgent].
const (
	StreamLifecycle = "lifecycle"
	StreamAssistant = "assistant"
	StreamTool      = "tool"
)

// frame is any inbound frame. Everything variable is left raw so one decode
// pass can tell a response from an event without deciding what either means.
type frame struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	OK      bool            `json:"ok"`
	Payload json.RawMessage `json:"payload"`
	Error   *ErrorShape     `json:"error"`
	Event   string          `json:"event"`
	Seq     uint64          `json:"seq"`
}

func decodeFrame(data []byte) (*frame, error) {
	var f frame
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// request is one outbound call.
type request struct {
	Type   string          `json:"type"`
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// Event is one event frame, handed to Options.OnEvent.
type Event struct {
	// Name is the event, e.g. [EventSessionsChanged].
	Name string
	// Payload is the event's body, undecoded. What is in it depends entirely on
	// Name, and the gateway adds fields between releases, so this package
	// decodes only what relayd reads and leaves the rest here for whoever needs
	// more.
	Payload json.RawMessage
	// Seq is the gateway's per-connection sequence number, when it sent one.
	Seq uint64
}

// Decode reads the payload into v.
func (e Event) Decode(v any) error { return json.Unmarshal(e.Payload, v) }

// ErrorShape is the gateway's own error body.
type ErrorShape struct {
	Code         string          `json:"code"`
	Message      string          `json:"message"`
	Details      json.RawMessage `json:"details,omitempty"`
	Retryable    bool            `json:"retryable,omitempty"`
	RetryAfterMs int             `json:"retryAfterMs,omitempty"`
}

// Error codes the gateway sends. There are exactly these, plus NOT_LINKED and
// AGENT_TIMEOUT which its own source marks as having no emitter left.
const (
	CodeInvalidRequest   = "INVALID_REQUEST"
	CodeForbidden        = "FORBIDDEN"
	CodeApprovalNotFound = "APPROVAL_NOT_FOUND"
	CodeUnavailable      = "UNAVAILABLE"
	CodeNotPaired        = "NOT_PAIRED"
)

// connectParams is the connect frame's body.
type connectParams struct {
	MinProtocol int             `json:"minProtocol"`
	MaxProtocol int             `json:"maxProtocol"`
	Client      clientInfo      `json:"client"`
	Role        string          `json:"role"`
	Scopes      []string        `json:"scopes"`
	Caps        []string        `json:"caps"`
	Commands    []string        `json:"commands"`
	Permissions map[string]bool `json:"permissions"`
	Auth        *connectAuth    `json:"auth,omitempty"`
	Locale      string          `json:"locale,omitempty"`
	UserAgent   string          `json:"userAgent,omitempty"`
}

type clientInfo struct {
	ID       string `json:"id"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
	Mode     string `json:"mode"`
}

type connectAuth struct {
	Token string `json:"token,omitempty"`
}

// Hello is hello-ok: what the gateway says about itself once a connect is
// accepted.
type Hello struct {
	Protocol int `json:"protocol"`
	Server   struct {
		Version string `json:"version"`
		ConnID  string `json:"connId"`
	} `json:"server"`
	Features struct {
		// Methods is NOT a full enumeration of what this gateway can do —
		// sessions.steer, sessions.get, sessions.usage and push.test are all
		// real, callable, and absent from it. It is here to be logged and
		// compared between versions, and gating a call on it is a bug.
		Methods []string `json:"methods"`
		// Events has the same caveat and is worth rather more: it is how a
		// newer gateway announces a feed this client could subscribe to.
		Events       []string `json:"events"`
		Capabilities []string `json:"capabilities"`
	} `json:"features"`
	Auth struct {
		Role   string   `json:"role"`
		Scopes []string `json:"scopes"`
	} `json:"auth"`
	Policy struct {
		MaxPayload       int64 `json:"maxPayload"`
		MaxBufferedBytes int64 `json:"maxBufferedBytes"`
		TickIntervalMs   int64 `json:"tickIntervalMs"`
	} `json:"policy"`
}

// AgentRuntime is which harness actually runs a session's turns.
//
// It matters more to relayd than its size suggests. Source "model" means the
// binding came from config — from the agentRuntime mapped onto the model ref by
// onboarding — not from anything the caller asked for, because sessions.create
// has no runtime field. And id "claude-cli" is the one where the exec approval
// bus is not in play at all: that runtime answers Claude Code's tool-permission
// requests itself, with allow-everything or deny-everything, so no
// [EventExecApprovalRequested] will ever arrive for it.
type AgentRuntime struct {
	ID     string `json:"id"`
	Source string `json:"source,omitempty"`
}

// SessionRow is one session in [SessionsListResult] or a sessions.changed
// snapshot.
//
// The gateway's row carries around sixty fields. These are the ones a voice
// orchestrator needs to name a session, decide whether it is busy, and know
// what is running it; the rest are readable through the raw event payload.
type SessionRow struct {
	// Key is the address of a session everywhere in this protocol —
	// "agent:main:dashboard:<uuid>". Not the same as SessionID.
	Key string `json:"key"`
	// SessionID names the transcript file. It is also the id the correlation to
	// a native Claude Code transcript hangs off, via sessions.json's
	// claudeCliSessionId.
	SessionID     string        `json:"sessionId,omitempty"`
	AgentID       string        `json:"agentId,omitempty"`
	Kind          string        `json:"kind,omitempty"`
	Label         string        `json:"label,omitempty"`
	DisplayName   string        `json:"displayName,omitempty"`
	DerivedTitle  string        `json:"derivedTitle,omitempty"`
	LastMessage   string        `json:"lastMessagePreview,omitempty"`
	Model         string        `json:"model,omitempty"`
	ModelProvider string        `json:"modelProvider,omitempty"`
	AgentRuntime  *AgentRuntime `json:"agentRuntime,omitempty"`
	// Status is running | done | failed | killed | timeout, and is absent on a
	// session that has never run.
	Status         string  `json:"status,omitempty"`
	LastRunError   string  `json:"lastRunError,omitempty"`
	UpdatedAt      int64   `json:"updatedAt,omitempty"`
	LastActivityAt int64   `json:"lastActivityAt,omitempty"`
	CreatedAt      int64   `json:"createdAt,omitempty"`
	Archived       bool    `json:"archived,omitempty"`
	TotalTokens    int64   `json:"totalTokens,omitempty"`
	ContextTokens  int64   `json:"contextTokens,omitempty"`
	EstimatedCost  float64 `json:"estimatedCostUsd,omitempty"`
	// SpawnedCwd is where the harness actually ran. Worth reading rather than
	// assuming: the probe set spawnedCwd on a claude-cli session and the run's
	// own pwd still reported the agent workspace.
	SpawnedCwd string `json:"spawnedCwd,omitempty"`
	ExecCwd    string `json:"execCwd,omitempty"`
}

// SessionsChanged is the sessions.changed event.
//
// The gateway sends this in two shapes: a flat one on create/send, where the
// row's fields are spliced into the payload itself, and a nested one on run
// start/end, where they are under `session` and the payload carries phase and
// runId. Both are decoded here; [SessionsChanged.Row] picks whichever arrived.
type SessionsChanged struct {
	SessionKey string      `json:"sessionKey"`
	SessionID  string      `json:"sessionId,omitempty"`
	AgentID    string      `json:"agentId,omitempty"`
	Reason     string      `json:"reason,omitempty"`
	Phase      string      `json:"phase,omitempty"`
	RunID      string      `json:"runId,omitempty"`
	TS         int64       `json:"ts,omitempty"`
	Session    *SessionRow `json:"session,omitempty"`
	Flat       SessionRow  `json:"-"`
}

// UnmarshalJSON reads both shapes.
func (s *SessionsChanged) UnmarshalJSON(b []byte) error {
	type raw SessionsChanged
	var r raw
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	// The flat shape has no `key`, so fill it from sessionKey rather than leave
	// a row that names nothing.
	if err := json.Unmarshal(b, &r.Flat); err != nil {
		return err
	}
	if r.Flat.Key == "" {
		r.Flat.Key = r.SessionKey
	}
	*s = SessionsChanged(r)
	return nil
}

// Row is the session this event is about.
func (s SessionsChanged) Row() SessionRow {
	if s.Session != nil {
		return *s.Session
	}
	return s.Flat
}

// AgentEvent is the agent event: one run, told in pieces.
type AgentEvent struct {
	RunID       string          `json:"runId"`
	AgentID     string          `json:"agentId,omitempty"`
	SessionKey  string          `json:"sessionKey,omitempty"`
	SessionID   string          `json:"sessionId,omitempty"`
	Stream      string          `json:"stream"`
	Data        json.RawMessage `json:"data"`
	Seq         uint64          `json:"seq,omitempty"`
	TS          int64           `json:"ts,omitempty"`
	IsHeartbeat bool            `json:"isHeartbeat,omitempty"`
}

// Lifecycle is the [StreamLifecycle] body: a run starting or ending.
type Lifecycle struct {
	Phase     string `json:"phase"`
	StartedAt int64  `json:"startedAt,omitempty"`
	EndedAt   int64  `json:"endedAt,omitempty"`
}

// Assistant is the [StreamAssistant] body. Text is the whole answer so far and
// Delta is what this frame added, so a consumer can take either without
// accumulating.
type Assistant struct {
	Text  string `json:"text"`
	Delta string `json:"delta,omitempty"`
}

// ToolCall is the [StreamTool] body, sent once at phase "start" with the
// arguments and again at phase "result".
type ToolCall struct {
	Phase      string          `json:"phase"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"toolCallId,omitempty"`
	Args       json.RawMessage `json:"args,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	IsError    bool            `json:"isError,omitempty"`
}

// Decisions a reviewer may return. The enum is closed and "approve" is not in
// it — a word that reads correct and is rejected on the wire, which is the
// worst possible moment to find out. [Client.ExecApprovalResolve] refuses
// anything else before sending.
const (
	DecisionAllowOnce   = "allow-once"
	DecisionAllowAlways = "allow-always"
	DecisionDeny        = "deny"
)

// ExecApprovalRequest is a command waiting on a human.
//
// One type for both directions: it is the params of exec.approval.request and
// the `request` object inside [EventExecApprovalRequested] and
// [EventExecApprovalResolved], with the same field names in both.
//
// # Which sessions raise one
//
// Not the ones relayd most needs. For the claude-cli agent runtime the gateway
// does not ask at all: it computes a permission mode up front and answers
// Claude Code's tool-permission requests inline, so the session either runs
// every tool with nobody asked (the default policy — security "full", ask
// "off", which becomes --permission-mode bypassPermissions) or refuses every
// tool with "OpenClaw exec policy denied Claude native tool use". The probe
// watched an admin-scoped observer through both and saw zero approval frames.
// The bus itself works — an approval raised by one client reaches another — and
// the ACP runtime is the path that would restore a real ask.
type ExecApprovalRequest struct {
	ID          string   `json:"id,omitempty"`
	Command     string   `json:"command,omitempty"`
	CommandArgv []string `json:"commandArgv,omitempty"`
	Cwd         string   `json:"cwd,omitempty"`
	Host        string   `json:"host,omitempty"`
	NodeID      string   `json:"nodeId,omitempty"`
	// Security is deny | allowlist | full and Ask is off | on-miss | always:
	// the policy in force when the command was reached.
	Security    string `json:"security,omitempty"`
	Ask         string `json:"ask,omitempty"`
	WarningText string `json:"warningText,omitempty"`
	// AllowedDecisions is what this particular approval may be answered with.
	// Usually allow-once and deny; allow-always is offered only sometimes, and
	// sending it when it was not offered is refused.
	AllowedDecisions []string `json:"allowedDecisions,omitempty"`
	AgentID          string   `json:"agentId,omitempty"`
	SessionKey       string   `json:"sessionKey,omitempty"`
	SessionID        string   `json:"sessionId,omitempty"`
	RunID            string   `json:"runId,omitempty"`
	ToolCallID       string   `json:"toolCallId,omitempty"`
	ResolvedPath     string   `json:"resolvedPath,omitempty"`
	// TimeoutMs bounds how long the request waits for a reviewer before it
	// expires with a null decision.
	TimeoutMs int `json:"timeoutMs,omitempty"`
	// SuppressDelivery raises the approval without broadcasting it to anyone.
	// It exists, it is silent, and the probe lost an afternoon to it: with this
	// set no client sees the request, including an admin-scoped one.
	SuppressDelivery bool `json:"suppressDelivery,omitempty"`
}

// ExecApprovalRequested is the payload of [EventExecApprovalRequested].
type ExecApprovalRequested struct {
	ID          string              `json:"id"`
	Request     ExecApprovalRequest `json:"request"`
	CreatedAtMs int64               `json:"createdAtMs,omitempty"`
	ExpiresAtMs int64               `json:"expiresAtMs,omitempty"`
}

// ExecApprovalResolved is the payload of [EventExecApprovalResolved]. It
// arrives on every subscribed client, including the one that raised the
// request and whichever one answered it.
type ExecApprovalResolved struct {
	ID         string              `json:"id"`
	Decision   string              `json:"decision"`
	ResolvedBy string              `json:"resolvedBy,omitempty"`
	TS         int64               `json:"ts,omitempty"`
	Request    ExecApprovalRequest `json:"request"`
}

// ExecApprovalDecision is what exec.approval.request and
// exec.approval.waitDecision answer with. Decision is empty when the request
// expired without anyone answering.
type ExecApprovalDecision struct {
	ID             string `json:"id"`
	Decision       string `json:"decision,omitempty"`
	CreatedAtMs    int64  `json:"createdAtMs,omitempty"`
	ExpiresAtMs    int64  `json:"expiresAtMs,omitempty"`
	TerminalReason string `json:"terminalReason,omitempty"`
}

// ExecApprovalPending is one row of exec.approval.list.
//
// The list is scoped by the same visibility rule the broadcast is: without
// operator.admin a client sees only approvals raised on its own connection or
// bound to its own paired device id, and an empty array is what "you were not
// allowed to see them" looks like.
type ExecApprovalPending struct {
	ID          string              `json:"id"`
	Request     ExecApprovalRequest `json:"request"`
	CreatedAtMs int64               `json:"createdAtMs,omitempty"`
	ExpiresAtMs int64               `json:"expiresAtMs,omitempty"`
}

// platform is what the connect frame reports running on. The gateway uses it
// for diagnostics, and its own clients spell darwin this way.
func platform() string {
	switch runtime.GOOS {
	case "darwin":
		return "macos"
	default:
		return runtime.GOOS
	}
}
