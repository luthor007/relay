package codex

import "encoding/json"

// The subset of codex-cli 0.140.0's contract that Relay reads or writes.
// Everything here has a row in docs/fixtures/adapters/codex-methods.md; nothing
// here is remembered. Optional fields are pointers or nullable strings because
// the schemas mark a great many things `["string","null"]` and "absent" has to
// stay distinguishable from "empty".

// ---------- client → server params ----------

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Title   string `json:"title,omitempty"`
}

// initializeCapabilities is `InitializeCapabilities`. All three default false /
// null, and the flag that matters is ExperimentalApi: everything the schemas
// mark EXPERIMENTAL — item/tool/requestUserInput among them — is presumed off
// unless we ask (codex-methods.md §3).
type initializeCapabilities struct {
	ExperimentalApi           bool     `json:"experimentalApi"`
	OptOutNotificationMethods []string `json:"optOutNotificationMethods,omitempty"`
	// RequestAttestation stays false. Setting it true is what makes Codex send
	// `attestation/generate`, which Relay cannot answer.
	RequestAttestation bool `json:"requestAttestation"`
}

type initializeParams struct {
	ClientInfo   clientInfo              `json:"clientInfo"`
	Capabilities *initializeCapabilities `json:"capabilities,omitempty"`
}

// threadStartParams is `ThreadStartParams`. Every field is optional, but
// ApprovalPolicy and ApprovalsReviewer are always sent: leaving them to the
// user's config.toml is how the approvals trap gets switched on under us
// (ADAPTERS.md §3, codex-methods.md §3).
type threadStartParams struct {
	Cwd               string         `json:"cwd,omitempty"`
	Model             string         `json:"model,omitempty"`
	Sandbox           string         `json:"sandbox,omitempty"`
	ApprovalPolicy    string         `json:"approvalPolicy,omitempty"`
	ApprovalsReviewer string         `json:"approvalsReviewer,omitempty"`
	Config            map[string]any `json:"config,omitempty"`
}

type threadResumeParams struct {
	ThreadID          string         `json:"threadId"`
	Cwd               string         `json:"cwd,omitempty"`
	Model             string         `json:"model,omitempty"`
	Sandbox           string         `json:"sandbox,omitempty"`
	ApprovalPolicy    string         `json:"approvalPolicy,omitempty"`
	ApprovalsReviewer string         `json:"approvalsReviewer,omitempty"`
	Config            map[string]any `json:"config,omitempty"`
}

type threadForkParams struct {
	ThreadID          string         `json:"threadId"`
	Cwd               string         `json:"cwd,omitempty"`
	Model             string         `json:"model,omitempty"`
	Sandbox           string         `json:"sandbox,omitempty"`
	ApprovalPolicy    string         `json:"approvalPolicy,omitempty"`
	ApprovalsReviewer string         `json:"approvalsReviewer,omitempty"`
	Config            map[string]any `json:"config,omitempty"`
}

type threadIDParams struct {
	ThreadID string `json:"threadId"`
}

// userInput is the `text` variant of `UserInput`. The other four — image,
// localImage, skill, mention — are gated by prompt capabilities Relay has not
// probed on Codex, so [adapter.CheckTurn] refuses those blocks before we get
// here rather than the adapter inventing a shape for them.
type userInput struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func textInput(s string) []userInput { return []userInput{{Type: "text", Text: s}} }

type turnStartParams struct {
	ThreadID            string      `json:"threadId"`
	Input               []userInput `json:"input"`
	Model               string      `json:"model,omitempty"`
	Cwd                 string      `json:"cwd,omitempty"`
	ApprovalPolicy      string      `json:"approvalPolicy,omitempty"`
	ApprovalsReviewer   string      `json:"approvalsReviewer,omitempty"`
	ClientUserMessageID string      `json:"clientUserMessageId,omitempty"`
}

// turnSteerParams is the request no other runtime but Claude Code can match.
// ExpectedTurnID is a precondition, not a hint: "the request fails when it does
// not match the currently active turn".
type turnSteerParams struct {
	ThreadID            string      `json:"threadId"`
	ExpectedTurnID      string      `json:"expectedTurnId"`
	Input               []userInput `json:"input"`
	ClientUserMessageID string      `json:"clientUserMessageId,omitempty"`
}

type turnInterruptParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

type configReadParams struct {
	Cwd           string `json:"cwd,omitempty"`
	IncludeLayers bool   `json:"includeLayers,omitempty"`
}

// ---------- server → client shapes ----------

type thread struct {
	ID             string  `json:"id"`
	SessionID      string  `json:"sessionId"`
	Cwd            string  `json:"cwd"`
	Preview        string  `json:"preview"`
	Name           *string `json:"name"`
	ForkedFromID   *string `json:"forkedFromId"`
	ParentThreadID *string `json:"parentThreadId"`
	CliVersion     string  `json:"cliVersion"`
	ModelProvider  string  `json:"modelProvider"`
	// Turns is empty on `thread/started` by contract — that is not "a thread
	// with no history", it is "this payload does not carry history".
	Turns []turnPayload `json:"turns"`
}

type turnError struct {
	Message           string          `json:"message"`
	AdditionalDetails *string         `json:"additionalDetails"`
	CodexErrorInfo    json.RawMessage `json:"codexErrorInfo"`
}

// code returns the string variant of `CodexErrorInfo` when there is one. The
// union also has two object variants carrying an upstream HTTP status; for
// those the key is the code, which is the only stable thing about them.
func (e *turnError) code() string {
	if e == nil || len(e.CodexErrorInfo) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(e.CodexErrorInfo, &s); err == nil {
		return s
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(e.CodexErrorInfo, &obj); err == nil {
		for k := range obj {
			return k
		}
	}
	return ""
}

type turnPayload struct {
	ID         string     `json:"id"`
	Status     string     `json:"status"` // completed | interrupted | failed | inProgress
	DurationMs *int64     `json:"durationMs"`
	StartedAt  *int64     `json:"startedAt"`
	Error      *turnError `json:"error"`
	ItemsView  string     `json:"itemsView"`
}

// threadItem is the 17-variant `ThreadItem` union, flattened. Discriminate on
// Type; the fields that do not belong to a variant stay zero.
type threadItem struct {
	ID   string `json:"id"`
	Type string `json:"type"`

	// agentMessage, plan
	Text string `json:"text"`

	// reasoning
	Content []string `json:"content"`
	Summary []string `json:"summary"`

	// commandExecution
	Command          string  `json:"command"`
	Cwd              string  `json:"cwd"`
	ExitCode         *int64  `json:"exitCode"`
	AggregatedOutput *string `json:"aggregatedOutput"`
	Source           string  `json:"source"`

	// fileChange
	Changes []fileUpdateChange `json:"changes"`

	// mcpToolCall
	Server string          `json:"server"`
	Tool   string          `json:"tool"`
	Error  json.RawMessage `json:"error"`

	// webSearch
	Query string `json:"query"`

	// imageView
	Path string `json:"path"`

	// commandExecution | fileChange | mcpToolCall | dynamicToolCall
	Status string `json:"status"`

	// mcpToolCall | dynamicToolCall
	Arguments json.RawMessage `json:"arguments"`

	// userMessage
	ClientID *string `json:"clientId"`
}

type fileUpdateChange struct {
	Path string          `json:"path"`
	Kind json.RawMessage `json:"kind"`
	Diff string          `json:"diff"`
}

// kindName pulls the tag out of `PatchChangeKind`, which is an object union
// (`{"type":"add"}`), not a bare string.
func (c fileUpdateChange) kindName() string {
	var k struct {
		Type string `json:"type"`
	}
	if len(c.Kind) == 0 {
		return ""
	}
	if err := json.Unmarshal(c.Kind, &k); err != nil {
		return ""
	}
	return k.Type
}

type tokenUsageBreakdown struct {
	InputTokens           int64 `json:"inputTokens"`
	CachedInputTokens     int64 `json:"cachedInputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	ReasoningOutputTokens int64 `json:"reasoningOutputTokens"`
	TotalTokens           int64 `json:"totalTokens"`
}

type threadTokenUsage struct {
	Last  tokenUsageBreakdown `json:"last"`
	Total tokenUsageBreakdown `json:"total"`
	// ModelContextWindow is nullable even when the counts are present, which is
	// why MEMORY.md §9's compact-at-70% needs a fallback denominator.
	ModelContextWindow *int64 `json:"modelContextWindow"`
}

type turnPlanStep struct {
	Step   string `json:"step"`
	Status string `json:"status"` // pending | inProgress | completed
}

type threadStatus struct {
	Type        string   `json:"type"` // notLoaded | idle | systemError | active
	ActiveFlags []string `json:"activeFlags"`
}

// threadSettings is the live check that the approvals trap has not been
// switched on under us mid-session.
type threadSettings struct {
	ApprovalPolicy    json.RawMessage `json:"approvalPolicy"`
	ApprovalsReviewer string          `json:"approvalsReviewer"`
	Model             string          `json:"model"`
	ModelProvider     string          `json:"modelProvider"`
	Cwd               string          `json:"cwd"`
}

// approvalPolicyName reads the string variant of `AskForApproval`. The union's
// other branch is `{granular:{…}}`, which is not "never" and so does not
// disable approvals — it narrows which ones arrive.
func (s threadSettings) approvalPolicyName() string {
	var str string
	if err := json.Unmarshal(s.ApprovalPolicy, &str); err == nil {
		return str
	}
	return "granular"
}

// ---------- notification envelopes ----------

type threadStartedNote struct {
	Thread thread `json:"thread"`
}

type turnBoundaryNote struct {
	ThreadID string      `json:"threadId"`
	Turn     turnPayload `json:"turn"`
}

type itemBoundaryNote struct {
	ThreadID string     `json:"threadId"`
	TurnID   string     `json:"turnId"`
	Item     threadItem `json:"item"`
}

type deltaNote struct {
	ThreadID     string `json:"threadId"`
	TurnID       string `json:"turnId"`
	ItemID       string `json:"itemId"`
	Delta        string `json:"delta"`
	ContentIndex int64  `json:"contentIndex"`
	SummaryIndex int64  `json:"summaryIndex"`
}

type mcpProgressNote struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Message  string `json:"message"`
}

type patchUpdatedNote struct {
	ThreadID string             `json:"threadId"`
	TurnID   string             `json:"turnId"`
	ItemID   string             `json:"itemId"`
	Changes  []fileUpdateChange `json:"changes"`
}

type planUpdatedNote struct {
	ThreadID    string         `json:"threadId"`
	TurnID      string         `json:"turnId"`
	Plan        []turnPlanStep `json:"plan"`
	Explanation *string        `json:"explanation"`
}

type tokenUsageNote struct {
	ThreadID   string           `json:"threadId"`
	TurnID     string           `json:"turnId"`
	TokenUsage threadTokenUsage `json:"tokenUsage"`
}

type errorNote struct {
	ThreadID  string    `json:"threadId"`
	TurnID    string    `json:"turnId"`
	Error     turnError `json:"error"`
	WillRetry bool      `json:"willRetry"`
}

type statusChangedNote struct {
	ThreadID string       `json:"threadId"`
	Status   threadStatus `json:"status"`
}

type settingsUpdatedNote struct {
	ThreadID       string         `json:"threadId"`
	ThreadSettings threadSettings `json:"threadSettings"`
}

type serverRequestResolvedNote struct {
	ThreadID  string          `json:"threadId"`
	RequestID json.RawMessage `json:"requestId"`
}

// threadIDOnly is the generic demultiplexing key. Every notification that
// belongs to a conversation carries threadId; the ones that do not are
// connection-level and are handled by the Adapter.
type threadIDOnly struct {
	ThreadID string `json:"threadId"`
}
