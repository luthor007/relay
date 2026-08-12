package claudecode

import (
	"encoding/json"
	"strings"
)

// The stream-json envelope, transcribed from
// docs/fixtures/adapters/claude-code.trace.json (claude-code 2.1.226) and
// ADAPTERS.md §2. Only fields this adapter actually reads are named; everything
// else is ignored rather than rejected, because a runtime that adds a field must
// not break a running session.
//
// Two discriminations in here are load-bearing and both are on *key presence*,
// not on a value:
//
//   - `user` with isReplay present is our injected turn coming back. `user` with
//     tool_use_result present is a tool result being fed to the model. Same
//     type, entirely different meaning. isReplay is *bool for exactly this
//     reason: comparing against false would mis-read a tool result as a turn.
//   - `total_cost_usd` is *float64 so "the runtime did not say" is
//     distinguishable from "this turn was free".

// wireLine is one NDJSON line from Claude Code's stdout.
type wireLine struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	SessionID       string  `json:"session_id"`
	UUID            string  `json:"uuid"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	Timestamp       string  `json:"timestamp"`

	// system/hook_started and system/hook_response. Undocumented before the
	// fixture was recorded; they arrive *before* system/init and are correlated
	// by hook_id rather than by order, because hooks run concurrently.
	HookID    string `json:"hook_id"`
	HookName  string `json:"hook_name"`
	HookEvent string `json:"hook_event"`
	ExitCode  *int   `json:"exit_code"`
	Outcome   string `json:"outcome"`
	Output    string `json:"output"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`

	// system/status. Once per API request, never a turn boundary.
	Status string `json:"status"`

	// system/init, re-emitted at the head of every turn.
	CWD            string        `json:"cwd"`
	Model          string        `json:"model"`
	PermissionMode string        `json:"permissionMode"`
	APIKeySource   string        `json:"apiKeySource"`
	OutputStyle    string        `json:"output_style"`
	Version        string        `json:"claude_code_version"`
	Tools          []string      `json:"tools"`
	MCPServers     []wireMCPStat `json:"mcp_servers"`
	Capabilities   []string      `json:"capabilities"`

	// user, assistant.
	Message       *wireMessage    `json:"message"`
	IsReplay      *bool           `json:"isReplay"`
	ToolUseResult json.RawMessage `json:"tool_use_result"`
	RequestID     string          `json:"request_id"`

	// stream_event.
	Event  *wireStreamEvent `json:"event"`
	TTFTMs *int64           `json:"ttft_ms"`

	// rate_limit_event.
	RateLimitInfo *RateLimitInfo `json:"rate_limit_info"`

	// result — the turn boundary.
	IsError           *bool                     `json:"is_error"`
	DurationMS        int64                     `json:"duration_ms"`
	DurationAPIMS     int64                     `json:"duration_api_ms"`
	NumTurns          int                       `json:"num_turns"`
	StopReason        string                    `json:"stop_reason"`
	TerminalReason    string                    `json:"terminal_reason"`
	TotalCostUSD      *float64                  `json:"total_cost_usd"`
	Usage             *wireUsage                `json:"usage"`
	ModelUsage        map[string]wireModelUsage `json:"modelUsage"`
	Result            string                    `json:"result"`
	APIErrorStatus    json.RawMessage           `json:"api_error_status"`
	PermissionDenials []json.RawMessage         `json:"permission_denials"`
	TTFTStreamMS      *int64                    `json:"ttft_stream_ms"`
	TimeToRequestMS   *int64                    `json:"time_to_request_ms"`
}

type wireMCPStat struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type wireMessage struct {
	ID         string        `json:"id"`
	Model      string        `json:"model"`
	Role       string        `json:"role"`
	Content    []wireContent `json:"content"`
	StopReason *string       `json:"stop_reason"`
	Usage      *wireUsage    `json:"usage"`
}

// wireContent is a content block in either direction. The tool_use and
// tool_result fields never appear together; one struct is simpler than four and
// the JSON is self-describing on Type.
type wireContent struct {
	Type string `json:"type"`

	// text / thinking
	Text     string `json:"text"`
	Thinking string `json:"thinking"`

	// tool_use — Input is empty on content_block_start and complete on the
	// matching assistant event.
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// tool_result — Content is a string on Bash and an array of blocks on tools
	// that return structured output, so it stays raw until it is flattened.
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   *bool           `json:"is_error"`
}

type wireStreamEvent struct {
	Type         string       `json:"type"`
	Index        *int         `json:"index"`
	Message      *wireMessage `json:"message"`
	ContentBlock *wireContent `json:"content_block"`
	Delta        *wireDelta   `json:"delta"`
	Usage        *wireUsage   `json:"usage"`
}

// wireDelta covers every delta type. The fixture contains only text_delta and
// input_json_delta; a session with extended thinking adds thinking_delta and
// signature_delta, and ADAPTERS.md §2 requires unknown delta types to be
// ignorable rather than fatal.
type wireDelta struct {
	Type        string  `json:"type"`
	Text        string  `json:"text"`
	PartialJSON string  `json:"partial_json"`
	Thinking    string  `json:"thinking"`
	Signature   string  `json:"signature"`
	StopReason  *string `json:"stop_reason"`
}

type wireUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`

	OutputTokensDetails *struct {
		ThinkingTokens int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

// context returns what ADAPTERS.md §2 calls the live context size for the
// request this usage block belongs to: input + cache_read + cache_creation.
func (u *wireUsage) context() int64 {
	if u == nil {
		return 0
	}
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

// total is every token the request touched, live context plus output.
func (u *wireUsage) total() int64 {
	if u == nil {
		return 0
	}
	return u.context() + u.OutputTokens
}

// wireModelUsage is result.modelUsage, keyed by the *decorated* model id
// ("claude-opus-5[1m]"). Only canonicalModel is the real model name, and a
// routing table must never be keyed on the decorated form.
type wireModelUsage struct {
	InputTokens              int64   `json:"inputTokens"`
	OutputTokens             int64   `json:"outputTokens"`
	CacheReadInputTokens     int64   `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64   `json:"cacheCreationInputTokens"`
	CostUSD                  float64 `json:"costUSD"`
	ContextWindow            int64   `json:"contextWindow"`
	MaxOutputTokens          int64   `json:"maxOutputTokens"`
	CanonicalModel           string  `json:"canonicalModel"`
	Provider                 string  `json:"provider"`
}

// RateLimitInfo is the payload of a rate_limit_event: quota state, worth
// surfacing before it bites. Only status "allowed" has been observed, so any
// other value is reported rather than interpreted.
type RateLimitInfo struct {
	Status                string `json:"status"`
	ResetsAt              int64  `json:"resetsAt"`
	RateLimitType         string `json:"rateLimitType"`
	OverageStatus         string `json:"overageStatus"`
	OverageDisabledReason string `json:"overageDisabledReason"`
	IsUsingOverage        bool   `json:"isUsingOverage"`
}

// Restricting reports whether the runtime said something other than "allowed".
// The fixture only ever contains "allowed"; treating everything else as
// restricting is the conservative reading, and the raw struct is surfaced so a
// console can show what was actually said.
func (r RateLimitInfo) Restricting() bool {
	return r.Status != "" && r.Status != "allowed"
}

// flattenText renders a tool_result's content as the text the model saw. It is
// a string on Bash and an array of content blocks elsewhere; anything else
// yields "" rather than a guess.
func flattenText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []wireContent
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, c := range blocks {
			if c.Type == "text" {
				b.WriteString(c.Text)
			}
		}
		return b.String()
	}
	return ""
}

// decodeObject unmarshals a tool_use input into a map. A tool call whose input
// is not an object yields nil, which the event model reads as "not reported".
func decodeObject(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// userTurnLine is the one NDJSON line that injects a turn into a live process.
// ADAPTERS.md §2 records it verbatim and it is confirmed working; the same line
// is a new turn when no turn is running and a mid-turn steer when one is.
func userTurnLine(text string) ([]byte, error) {
	type block struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type msg struct {
		Role    string  `json:"role"`
		Content []block `json:"content"`
	}
	type line struct {
		Type    string `json:"type"`
		Message msg    `json:"message"`
	}
	return json.Marshal(line{
		Type:    "user",
		Message: msg{Role: "user", Content: []block{{Type: "text", Text: text}}},
	})
}
