package gateway

import (
	"context"
	"fmt"
	"strings"
)

// The methods this file wraps are the ones Relay's loop is built on. Everything
// else the gateway answers — sessions.send, sessions.messages.subscribe,
// sessions.describe, models.list, chat.send, config.get — is reachable through
// [Client.Call] with a map or a struct, and deliberately has no wrapper here: a
// typed method is a claim that this package has read the wire for it, and these
// are the ones it has.

// Method names, so a caller passing one to [Client.Call] and a wrapper here
// cannot drift apart.
const (
	MethodSessionsList      = "sessions.list"
	MethodSessionsCreate    = "sessions.create"
	MethodSessionsSubscribe = "sessions.subscribe"
	MethodSessionsAbort     = "sessions.abort"
	MethodAgent             = "agent"

	MethodExecApprovalList         = "exec.approval.list"
	MethodExecApprovalGet          = "exec.approval.get"
	MethodExecApprovalRequest      = "exec.approval.request"
	MethodExecApprovalResolve      = "exec.approval.resolve"
	MethodExecApprovalWaitDecision = "exec.approval.waitDecision"
)

// SessionsListParams filters sessions.list. The zero value asks for the
// gateway's own bounded default, which is what relayd wants at startup.
type SessionsListParams struct {
	Limit  int    `json:"limit,omitempty"`
	Offset int    `json:"offset,omitempty"`
	Agent  string `json:"agentId,omitempty"`
	// ActiveMinutes limits the answer to sessions touched recently.
	ActiveMinutes int `json:"activeMinutes,omitempty"`
	// IncludeLastMessage reads the tail of every transcript to fill
	// SessionRow.LastMessage. One file read per session — bound it with Limit.
	IncludeLastMessage bool `json:"includeLastMessage,omitempty"`
	// IncludeDerivedTitles reads the head of every transcript to name a session
	// after its first user message. Same cost, same warning.
	IncludeDerivedTitles bool   `json:"includeDerivedTitles,omitempty"`
	SortBy               string `json:"sortBy,omitempty"`
	Label                string `json:"label,omitempty"`
	Search               string `json:"search,omitempty"`
}

// SessionsListResult is what sessions.list answers.
type SessionsListResult struct {
	TS         int64        `json:"ts,omitempty"`
	Count      int          `json:"count"`
	TotalCount int          `json:"totalCount,omitempty"`
	HasMore    bool         `json:"hasMore,omitempty"`
	Sessions   []SessionRow `json:"sessions"`
}

// SessionsList reads the gateway's session registry.
func (c *Client) SessionsList(ctx context.Context, p SessionsListParams) (SessionsListResult, error) {
	var out SessionsListResult
	err := c.Call(ctx, MethodSessionsList, p, &out)
	return out, err
}

// SessionsCreateParams creates or adopts a session.
//
// Model is a model ref, and it is config-coupled rather than self-describing:
// the provider-namespaced form the runtime actually uses (claude-cli/...) is
// rejected by the allowlist even when that provider exists, while the
// anthropic/... ref onboarding mapped onto that runtime is accepted. So read
// models.list and pass what it offers rather than hardcoding a ref that reads
// correct.
//
// There is no runtime field, here or on sessions.patch. Which harness runs the
// session follows from the model, through config, and the gateway reports what
// it resolved as [AgentRuntime] afterwards.
type SessionsCreateParams struct {
	// Key adopts an existing session instead of making one.
	Key     string `json:"key,omitempty"`
	AgentID string `json:"agentId,omitempty"`
	Label   string `json:"label,omitempty"`
	Model   string `json:"model,omitempty"`
	// Message starts the first turn in the same call. Without it the session is
	// created idle and RunStarted comes back false.
	Message string `json:"message,omitempty"`
	Task    string `json:"task,omitempty"`
	// Cwd is where the session works. Gateway paths outside the configured
	// agent workspaces need operator.admin, and the claude-cli runtime ignores
	// the per-session variant of this entirely — the probe set it and the run's
	// own pwd still answered with the agent workspace.
	Cwd string `json:"cwd,omitempty"`
	// Worktree puts the session in a managed git worktree, which is the
	// gateway's own answer to several sessions sharing one checkout.
	Worktree      bool   `json:"worktree,omitempty"`
	WorktreeName  string `json:"worktreeName,omitempty"`
	ProjectID     string `json:"projectId,omitempty"`
	ThinkingLevel string `json:"thinkingLevel,omitempty"`
	Incognito     bool   `json:"incognito,omitempty"`
	ParentKey     string `json:"parentSessionKey,omitempty"`
}

// SessionsCreateResult is what sessions.create answers.
type SessionsCreateResult struct {
	OK bool `json:"ok"`
	// Key addresses the session in every later call.
	Key string `json:"key"`
	// SessionID names its transcript.
	SessionID string `json:"sessionId"`
	Entry     struct {
		SessionID   string `json:"sessionId"`
		SessionFile string `json:"sessionFile"`
		UpdatedAt   int64  `json:"updatedAt"`
		Label       string `json:"label,omitempty"`
	} `json:"entry"`
	// RunStarted is true only when Message was set.
	RunStarted bool `json:"runStarted"`
}

// SessionsCreate makes a session, and starts its first turn when Message is set.
func (c *Client) SessionsCreate(ctx context.Context, p SessionsCreateParams) (SessionsCreateResult, error) {
	var out SessionsCreateResult
	err := c.Call(ctx, MethodSessionsCreate, p, &out)
	return out, err
}

// SessionsSubscribe asks for [EventSessionsChanged] on every session.
//
// Sticky, so it survives a reconnect. Without that the socket comes back and
// the session feed does not, which looks from the outside like a very quiet
// afternoon.
func (c *Client) SessionsSubscribe(ctx context.Context) error {
	return c.Sticky(ctx, MethodSessionsSubscribe, nil, nil)
}

// SessionsAbortParams names what to stop: a session by Key, one run by RunID,
// or both to stop a run only if it is still the one on that session.
type SessionsAbortParams struct {
	Key     string `json:"key,omitempty"`
	RunID   string `json:"runId,omitempty"`
	AgentID string `json:"agentId,omitempty"`
	// ClearQueued also discards the followup and lane queues, so a "stop" means
	// stopped rather than stopped-then-the-next-one-starts.
	ClearQueued bool `json:"clearQueued,omitempty"`
}

// SessionsAbortResult is what sessions.abort answers. Status is "aborted" or
// "no-active-run" — the second is not a failure, it is the turn having already
// finished, which at voice latency happens often.
type SessionsAbortResult struct {
	OK           bool   `json:"ok"`
	AbortedRunID string `json:"abortedRunId,omitempty"`
	Status       string `json:"status"`
}

// SessionsAbort stops a run.
func (c *Client) SessionsAbort(ctx context.Context, p SessionsAbortParams) (SessionsAbortResult, error) {
	if strings.TrimSpace(p.Key) == "" && strings.TrimSpace(p.RunID) == "" {
		return SessionsAbortResult{}, fmt.Errorf("gateway: %s needs a session key or a run id", MethodSessionsAbort)
	}
	var out SessionsAbortResult
	err := c.Call(ctx, MethodSessionsAbort, p, &out)
	return out, err
}

// AgentParams runs one turn.
type AgentParams struct {
	Message    string `json:"message"`
	AgentID    string `json:"agentId,omitempty"`
	SessionKey string `json:"sessionKey,omitempty"`
	SessionID  string `json:"sessionId,omitempty"`
	Model      string `json:"model,omitempty"`
	Provider   string `json:"provider,omitempty"`
	Thinking   string `json:"thinking,omitempty"`
	Cwd        string `json:"cwd,omitempty"`
	// Timeout is the gateway's own cap on the turn, in seconds.
	Timeout int `json:"timeout,omitempty"`
	// Lane serialises turns that share a name, which is the gateway's answer to
	// two utterances arriving for one session at once.
	Lane string `json:"lane,omitempty"`
}

// AgentResult is what the agent method answers. Status is ok | error | timeout
// | in_flight.
type AgentResult struct {
	RunID      string `json:"runId"`
	Status     string `json:"status"`
	Summary    string `json:"summary,omitempty"`
	StopReason string `json:"stopReason,omitempty"`
	Result     any    `json:"result,omitempty"`
}

// Agent runs a turn and waits for it to finish.
//
// Two things to know before calling it. It is long: the answer comes when the
// run ends, which for a coding agent is minutes, so ctx has to carry a deadline
// that means the turn rather than a request. And it answers twice — an
// acceptance and then a final frame with the same id — of which this client
// takes the first and drops the second.
//
// For a turn relayd wants to watch rather than wait on, the shape to use is
// sessions.send through [Client.Call], which returns a run id immediately and
// leaves the progress to [EventAgent].
func (c *Client) Agent(ctx context.Context, p AgentParams) (AgentResult, error) {
	if strings.TrimSpace(p.Message) == "" {
		return AgentResult{}, fmt.Errorf("gateway: %s needs a message", MethodAgent)
	}
	var out AgentResult
	err := c.Call(ctx, MethodAgent, p, &out)
	return out, err
}

// ExecApprovalList reads the approvals this client is allowed to see.
//
// An empty list means either that nothing is pending or that this connection
// was not permitted to know — without operator.admin the answer is scoped to
// approvals this client raised itself. The two cases are indistinguishable from
// here, which is the argument for asking for admin at connect.
func (c *Client) ExecApprovalList(ctx context.Context) ([]ExecApprovalPending, error) {
	var out []ExecApprovalPending
	err := c.Call(ctx, MethodExecApprovalList, nil, &out)
	return out, err
}

// ExecApprovalGetResult is one pending approval, summarised for a reviewer.
type ExecApprovalGetResult struct {
	ID               string   `json:"id"`
	CommandText      string   `json:"commandText,omitempty"`
	CommandPreview   string   `json:"commandPreview,omitempty"`
	AllowedDecisions []string `json:"allowedDecisions,omitempty"`
	Host             string   `json:"host,omitempty"`
	NodeID           string   `json:"nodeId,omitempty"`
	AgentID          string   `json:"agentId,omitempty"`
	ExpiresAtMs      int64    `json:"expiresAtMs,omitempty"`
}

// ExecApprovalGet reads one pending approval by id.
func (c *Client) ExecApprovalGet(ctx context.Context, id string) (ExecApprovalGetResult, error) {
	var out ExecApprovalGetResult
	if strings.TrimSpace(id) == "" {
		return out, fmt.Errorf("gateway: %s needs an approval id", MethodExecApprovalGet)
	}
	err := c.Call(ctx, MethodExecApprovalGet, struct {
		ID string `json:"id"`
	}{id}, &out)
	return out, err
}

// ExecApprovalRequest raises an approval and blocks until it is answered or
// expires.
//
// This is the direction relayd will use least and should understand best: it is
// how something relayd itself is about to run gets a human in front of it, and
// it is the only way to exercise the approval bus end to end without an agent.
// The answer's Decision is empty when nobody answered in time.
func (c *Client) ExecApprovalRequest(ctx context.Context, p ExecApprovalRequest) (ExecApprovalDecision, error) {
	var out ExecApprovalDecision
	if strings.TrimSpace(p.Command) == "" && len(p.CommandArgv) == 0 {
		return out, fmt.Errorf("gateway: %s needs a command", MethodExecApprovalRequest)
	}
	err := c.Call(ctx, MethodExecApprovalRequest, p, &out)
	return out, err
}

// ExecApprovalResolve answers an approval.
//
// decision must be one of [DecisionAllowOnce], [DecisionDeny] or, when the
// approval offered it, [DecisionAllowAlways]. Anything else is refused here
// rather than on the wire, because the word that reads most correct —
// "approve" — is not in the enum, and a person holding a phone waiting to
// approve a command is the worst audience for a validation error.
func (c *Client) ExecApprovalResolve(ctx context.Context, id, decision string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("gateway: %s needs an approval id", MethodExecApprovalResolve)
	}
	switch decision {
	case DecisionAllowOnce, DecisionAllowAlways, DecisionDeny:
	default:
		return fmt.Errorf("gateway: %q is not a decision; the gateway takes %q, %q or %q",
			decision, DecisionAllowOnce, DecisionDeny, DecisionAllowAlways)
	}
	return c.Call(ctx, MethodExecApprovalResolve, struct {
		ID       string `json:"id"`
		Decision string `json:"decision"`
	}{id, decision}, nil)
}

// ExecApprovalWaitDecision waits for someone else to answer an approval.
//
// It is how relayd follows an approval it did not raise — the user answering in
// their own terminal, or on another device — so that a ping can be retracted
// rather than repeated at two minutes.
func (c *Client) ExecApprovalWaitDecision(ctx context.Context, id string) (ExecApprovalDecision, error) {
	var out ExecApprovalDecision
	if strings.TrimSpace(id) == "" {
		return out, fmt.Errorf("gateway: %s needs an approval id", MethodExecApprovalWaitDecision)
	}
	err := c.Call(ctx, MethodExecApprovalWaitDecision, struct {
		ID string `json:"id"`
	}{id}, &out)
	return out, err
}
