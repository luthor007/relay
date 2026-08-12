// Package llm is the provider-abstracted model client for the orchestrator's
// two models.
//
// ORCHESTRATOR.md §3b: the orchestrator runs two models with different jobs.
// The big one holds the MCP registry and a shell and does the work; the small
// one speaks, narrates progress from structured events, and keeps the user
// updated. The reason is latency, not price — SYSTEM.md §7b measured the budget
// and the agent dominates it by an order of magnitude.
//
// Three things this package is careful about:
//
//   - Credentials are stored as REFERENCES — env var, file path, or exec —
//     never pasted inline. That is OpenClaw's shape and ORCHESTRATOR.md §2 says
//     to copy it. See [CredentialRef].
//   - Every credential is probed with one real call before the installer exits.
//     A pairing code that works, glasses that pair, and silence the first time
//     someone speaks is the worst place to discover a bad key. See [Probe].
//   - The transport is injectable, so nothing here makes a network call in a
//     test.
package llm

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// API is the wire shape a provider speaks. Two cover the whole vendor list:
// OpenAI-compatible carries OpenRouter and most of the rest, and
// Anthropic-compatible carries the one that is not.
type API string

const (
	APIOpenAI    API = "openai"
	APIAnthropic API = "anthropic"
)

// Role is a message role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one turn of context sent to a model.
//
// ToolCalls and ToolResults are what make the history round-trip: a model that
// asked for a tool must see its own request and the answer on the next call,
// and both wires reject a conversation where a tool call has no matching
// result. See [ToolCall].
type Message struct {
	Role Role
	Text string

	// ToolCalls belong to an assistant message.
	ToolCalls []ToolCall
	// ToolResults belong to a user message and answer the preceding assistant
	// message's calls.
	ToolResults []ToolResult
}

// Request is a completion request.
//
// It used to say it needed no tool calling, on the grounds that this client
// only narrates and the agent runtimes do the work. Half of that is still
// true and is why the tool surface here stays small — relayd is not a coding
// agent and must not grow into one — but ORCHESTRATOR.md §3b gives the big
// model tools, sessions and memory writes, so the fields below exist.
type Request struct {
	// Model overrides the provider's configured model.
	Model string
	// System is the system prompt.
	System   string
	Messages []Message
	// MaxTokens caps the reply. ADAPTERS.md §6 budgets speech by seconds:
	// ~160 characters for a completed turn, so this is usually small.
	MaxTokens   int
	Temperature *float64
	Stop        []string

	// Tools is the set the model may call. Nil is the narration path, which
	// is most calls.
	Tools []Tool
	// ToolChoice constrains tool use. Nil means the provider default, which
	// is [ChoiceAuto] on both wires.
	ToolChoice *ToolChoice

	// Format constrains the reply to a JSON schema. Nil is prose.
	//
	// It exists for the calls whose answer is a decision rather than a
	// sentence — "is this worth writing down as a playbook" has to be a field,
	// not something inferred from whether the model used the word "skill".
	Format *OutputFormat

	// Effort trades thoroughness against tokens. Lower effort means fewer and
	// more-consolidated tool calls, less preamble and terser confirmations —
	// which is the small model's whole job description, so the voice path sets
	// it low and the work path leaves it alone. Passed through as the wire's
	// own spelling; an empty string sends nothing.
	Effort string
}

// OutputFormat constrains a reply to a JSON schema.
//
// Both wires support this and both reject the same constructs — no recursion,
// no numeric or length bounds, and every object needs additionalProperties:
// false. A schema using them must leave Strict off and validate on this side.
type OutputFormat struct {
	// Name identifies the schema. Required on the OpenAI wire, ignored on the
	// Anthropic one.
	Name   string
	Schema map[string]any
	// Strict asks the provider to guarantee the reply validates.
	Strict bool
}

// Usage is what the provider reported about the call.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
}

// Response is a completed call.
type Response struct {
	Model        string
	Text         string
	FinishReason string
	Usage        Usage
	Latency      time.Duration

	// ToolCalls is what the model asked to run. Empty on a narration turn, and
	// empty is also how [Loop] knows the run is over.
	ToolCalls []ToolCall
}

// Delta is one chunk of a streaming call. Streaming is not decoration: it is
// SYSTEM.md §7b's largest available latency win, because time-to-first-audio
// becomes the model's first token rather than its last.
type Delta struct {
	Text  string
	Usage *Usage
	Done  bool
}

// Stream yields Deltas until io.EOF.
type Stream interface {
	Recv() (Delta, error)
	Close() error
}

// Provider is one configured model on one vendor.
type Provider interface {
	// Vendor is the provider id from the catalog, e.g. "openrouter".
	Vendor() string
	// Model is the configured model id.
	Model() string
	// API is the wire shape.
	API() API

	Complete(ctx context.Context, req Request) (Response, error)
	Stream(ctx context.Context, req Request) (Stream, error)

	// Probe makes one real call and reports a stable reason code. It never
	// returns an error: a failed probe is a result, and the installer prints
	// it rather than aborting on it.
	Probe(ctx context.Context) ProbeResult
}

// Config describes one model on one provider.
type Config struct {
	// Vendor is a catalog id, or any string for a custom provider.
	Vendor string
	API    API
	// BaseURL defaults to the catalog entry for Vendor.
	BaseURL string
	Model   string
	// Credential is a reference, not a secret.
	Credential CredentialRef
	// Headers are extra headers, e.g. OpenRouter's HTTP-Referer.
	Headers map[string]string
	// HTTPClient is the injection point. Tests supply a RoundTripper and this
	// package makes no network calls.
	HTTPClient *http.Client
	// Lookup resolves a "vault:<id>" reference. Wired by the orchestrator so
	// this package does not depend on the vault.
	Lookup SecretLookup
	// Timeout defaults to 60s, and to ProbeTimeout for probes.
	Timeout time.Duration
}

// ErrNoModel means the config named no model and none was supplied per request.
var ErrNoModel = errors.New("llm: no model configured")

// New builds a provider from a config.
func New(cfg Config) (Provider, error) {
	if cfg.API == "" {
		if v, ok := Vendor(cfg.Vendor); ok {
			cfg.API = v.API
		} else {
			cfg.API = APIOpenAI
		}
	}
	if cfg.BaseURL == "" {
		if v, ok := Vendor(cfg.Vendor); ok {
			cfg.BaseURL = v.BaseURL
		}
	}
	if cfg.BaseURL == "" {
		return nil, errors.New("llm: no base URL, and vendor " + cfg.Vendor + " is not in the catalog")
	}
	if cfg.Model == "" {
		return nil, ErrNoModel
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}

	switch cfg.API {
	case APIOpenAI:
		return &openaiProvider{cfg: cfg}, nil
	case APIAnthropic:
		return &anthropicProvider{cfg: cfg}, nil
	default:
		return nil, errors.New("llm: unknown API shape " + string(cfg.API))
	}
}

// Pair is ORCHESTRATOR.md §2b's two models: a small fast one that speaks and a
// big one that works.
//
// OpenRouter is the recommended provider for both, because one key covers them
// and either can be swapped later without re-running setup.
type Pair struct {
	Small Provider
	Big   Provider
}

// NewPair builds both models.
func NewPair(small, big Config) (*Pair, error) {
	s, err := New(small)
	if err != nil {
		return nil, err
	}
	b, err := New(big)
	if err != nil {
		return nil, err
	}
	return &Pair{Small: s, Big: b}, nil
}

// Probe tests both credentials with one real call each. ORCHESTRATOR.md §2
// requires this before the installer exits.
func (p *Pair) Probe(ctx context.Context) map[string]ProbeResult {
	out := map[string]ProbeResult{}
	if p.Small != nil {
		out["small"] = p.Small.Probe(ctx)
	}
	if p.Big != nil {
		out["big"] = p.Big.Probe(ctx)
	}
	return out
}
