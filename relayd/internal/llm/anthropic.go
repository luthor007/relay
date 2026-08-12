package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// AnthropicVersion is the API version header. It is a date, and pinning it is
// how this client survives a provider change rather than discovering one.
const AnthropicVersion = "2023-06-01"

// anthropicProvider speaks the Messages API.
//
// Note what this is NOT for: ORCHESTRATOR.md §2b is explicit that a Claude
// subscription has no supported path into our orchestrator. This provider is
// for an Anthropic API key, or for any endpoint advertising itself as
// Anthropic-compatible under Custom Provider. A Claude Max plan still powers
// Claude Code on the same machine — Anthropic's own client, its own login,
// unchanged — and the installer says so in as many words.
type anthropicProvider struct {
	cfg Config
}

var _ Provider = (*anthropicProvider)(nil)

func (p *anthropicProvider) Vendor() string { return p.cfg.Vendor }
func (p *anthropicProvider) Model() string  { return p.cfg.Model }
func (p *anthropicProvider) API() API       { return APIAnthropic }

// anthropicBlock is the one content-block shape covering all three types this
// client sends and receives. The Messages API takes an array of blocks for
// every message, so there is no string-or-array union to carry around.
type anthropicBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
	Strict      bool           `json:"strict,omitempty"`
}

type anthropicToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

type anthropicFormat struct {
	Type   string         `json:"type"`
	Schema map[string]any `json:"schema"`
}

type anthropicOutputConfig struct {
	Effort string           `json:"effort,omitempty"`
	Format *anthropicFormat `json:"format,omitempty"`
}

type anthropicRequest struct {
	Model         string                 `json:"model"`
	System        string                 `json:"system,omitempty"`
	Messages      []anthropicMessage     `json:"messages"`
	MaxTokens     int                    `json:"max_tokens"`
	Temperature   *float64               `json:"temperature,omitempty"`
	StopSequences []string               `json:"stop_sequences,omitempty"`
	Stream        bool                   `json:"stream,omitempty"`
	Tools         []anthropicTool        `json:"tools,omitempty"`
	ToolChoice    *anthropicToolChoice   `json:"tool_choice,omitempty"`
	OutputConfig  *anthropicOutputConfig `json:"output_config,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type anthropicResponse struct {
	Model      string           `json:"model"`
	Content    []anthropicBlock `json:"content"`
	StopReason string           `json:"stop_reason"`
	Usage      *anthropicUsage  `json:"usage"`
}

// defaultMaxTokens is required by the Messages API, which has no implicit cap.
const defaultMaxTokens = 1024

func (p *anthropicProvider) body(req Request, stream bool) anthropicRequest {
	model := req.Model
	if model == "" {
		model = p.cfg.Model
	}
	msgs := make([]anthropicMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		// The system prompt is its own field here, not a message.
		if m.Role == RoleSystem {
			continue
		}
		blocks := make([]anthropicBlock, 0, 1+len(m.ToolCalls)+len(m.ToolResults))
		// Tool results lead their message. The API requires them first in the
		// user turn that answers a tool call, and putting them after the text
		// is the kind of thing that works until a model sends two.
		for _, r := range m.ToolResults {
			blocks = append(blocks, anthropicBlock{
				Type:      "tool_result",
				ToolUseID: r.CallID,
				Content:   r.Content,
				IsError:   r.IsError,
			})
		}
		if m.Text != "" {
			blocks = append(blocks, anthropicBlock{Type: "text", Text: m.Text})
		}
		for _, c := range m.ToolCalls {
			input := c.Input
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			blocks = append(blocks, anthropicBlock{
				Type: "tool_use", ID: c.ID, Name: c.Name, Input: input,
			})
		}
		if len(blocks) == 0 {
			continue
		}
		msgs = append(msgs, anthropicMessage{Role: string(m.Role), Content: blocks})
	}
	max := req.MaxTokens
	if max <= 0 {
		max = defaultMaxTokens
	}
	out := anthropicRequest{
		Model:         model,
		System:        req.System,
		Messages:      msgs,
		MaxTokens:     max,
		Temperature:   req.Temperature,
		StopSequences: req.Stop,
		Stream:        stream,
		Tools:         anthropicTools(req.Tools),
		ToolChoice:    anthropicChoice(req.ToolChoice),
	}
	// Both ride on output_config, so one may not clobber the other.
	if req.Effort != "" || req.Format != nil {
		cfg := &anthropicOutputConfig{Effort: req.Effort}
		if req.Format != nil {
			cfg.Format = &anthropicFormat{Type: "json_schema", Schema: req.Format.Schema}
		}
		out.OutputConfig = cfg
	}
	return out
}

func anthropicTools(tools []Tool) []anthropicTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]anthropicTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, anthropicTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Schema,
			Strict:      t.Strict,
		})
	}
	return out
}

func anthropicChoice(c *ToolChoice) *anthropicToolChoice {
	if c == nil || c.Mode == "" {
		return nil
	}
	// The wire spells three of the four the same way we do; ChoiceTool is the
	// one that also carries a name.
	out := &anthropicToolChoice{
		Type:                   string(c.Mode),
		DisableParallelToolUse: c.DisableParallel,
	}
	if c.Mode == ChoiceTool {
		out.Name = c.Name
	}
	return out
}

func anthropicHeaders(h http.Header, key string) {
	h.Set("x-api-key", key)
	h.Set("anthropic-version", AnthropicVersion)
}

func (p *anthropicProvider) Complete(ctx context.Context, req Request) (Response, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	start := time.Now()
	resp, err := post(ctx, p.cfg, "/v1/messages", p.body(req, false), anthropicHeaders)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()

	var out anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Response{}, err
	}

	var text strings.Builder
	var calls []ToolCall
	for _, c := range out.Content {
		switch c.Type {
		case "text":
			text.WriteString(c.Text)
		case "tool_use":
			input := c.Input
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			calls = append(calls, ToolCall{ID: c.ID, Name: c.Name, Input: input})
		}
	}
	r := Response{
		Model:        out.Model,
		Text:         text.String(),
		FinishReason: out.StopReason,
		Latency:      time.Since(start),
		ToolCalls:    calls,
	}
	if out.Usage != nil {
		r.Usage = Usage{
			InputTokens:  out.Usage.InputTokens,
			OutputTokens: out.Usage.OutputTokens,
			TotalTokens:  out.Usage.InputTokens + out.Usage.OutputTokens,
		}
	}
	return r, nil
}

func (p *anthropicProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	resp, err := post(ctx, p.cfg, "/v1/messages", p.body(req, true), anthropicHeaders)
	if err != nil {
		return nil, err
	}
	return &anthropicStream{sse: newSSEReader(resp.Body)}, nil
}

func (p *anthropicProvider) Probe(ctx context.Context) ProbeResult {
	return runProbe(ctx, p, p.cfg)
}

type anthropicStream struct {
	sse  *sseReader
	done bool
}

type anthropicEvent struct {
	Type  string `json:"type"`
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
	Usage   *anthropicUsage `json:"usage"`
	Message *struct {
		Usage *anthropicUsage `json:"usage"`
	} `json:"message"`
}

func (s *anthropicStream) Recv() (Delta, error) {
	if s.done {
		return Delta{}, io.EOF
	}
	for {
		name, data, err := s.sse.next()
		if err != nil {
			return Delta{}, err
		}
		var ev anthropicEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		kind := ev.Type
		if kind == "" {
			kind = name
		}
		switch kind {
		case "content_block_delta":
			if ev.Delta.Text != "" {
				return Delta{Text: ev.Delta.Text}, nil
			}
		case "message_delta":
			if ev.Usage != nil {
				return Delta{Usage: &Usage{
					InputTokens:  ev.Usage.InputTokens,
					OutputTokens: ev.Usage.OutputTokens,
					TotalTokens:  ev.Usage.InputTokens + ev.Usage.OutputTokens,
				}}, nil
			}
		case "message_stop":
			s.done = true
			return Delta{Done: true}, nil
		case "error":
			s.done = true
			return Delta{}, &HTTPError{Status: http.StatusBadGateway, Body: data}
		}
	}
}

func (s *anthropicStream) Close() error { return s.sse.Close() }
