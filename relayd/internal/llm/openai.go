package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// openaiProvider speaks the OpenAI chat-completions shape, which covers
// OpenRouter — the recommended provider — and most of ORCHESTRATOR.md §2b's
// vendor list, including every Custom Provider that advertises itself as
// OpenAI-compatible.
type openaiProvider struct {
	cfg Config
}

var _ Provider = (*openaiProvider)(nil)

func (p *openaiProvider) Vendor() string { return p.cfg.Vendor }
func (p *openaiProvider) Model() string  { return p.cfg.Model }
func (p *openaiProvider) API() API       { return APIOpenAI }

// openaiFunction carries the arguments as a JSON *string*, which is the one
// real difference from the Anthropic wire: there the input is an object. Every
// conversion between the two lives in this file so nothing above it has to know
// which provider it is talking to.
type openaiFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openaiToolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function openaiFunction `json:"function"`
}

type openaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`
	// ToolCalls is set on an assistant message that requested tools.
	ToolCalls []openaiToolCall `json:"tool_calls,omitempty"`
	// ToolCallID is set on a role:"tool" message answering one call. Unlike the
	// Anthropic wire, which batches every result into one user message, this
	// shape needs one message per result.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type openaiToolDef struct {
	Type     string             `json:"type"`
	Function openaiFunctionDecl `json:"function"`
}

type openaiFunctionDecl struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
	Strict      bool           `json:"strict,omitempty"`
}

type openaiRequest struct {
	Model       string          `json:"model"`
	Messages    []openaiMessage `json:"messages"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	Stop        []string        `json:"stop,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	Tools       []openaiToolDef `json:"tools,omitempty"`
	// ToolChoice is a string for three of the four modes and an object for the
	// fourth, so it stays as any rather than growing a marshaller.
	ToolChoice any `json:"tool_choice,omitempty"`
	// ParallelToolCalls is a pointer because false is the meaningful value and
	// omitting it is the default.
	ParallelToolCalls *bool              `json:"parallel_tool_calls,omitempty"`
	ReasoningEffort   string             `json:"reasoning_effort,omitempty"`
	ResponseFormat    *openaiResponseFmt `json:"response_format,omitempty"`
}

type openaiResponseFmt struct {
	Type       string           `json:"type"`
	JSONSchema openaiJSONSchema `json:"json_schema"`
}

type openaiJSONSchema struct {
	// Name is required here and has no equivalent on the Anthropic wire, so an
	// unnamed schema gets one rather than a 400 nobody can read.
	Name   string         `json:"name"`
	Schema map[string]any `json:"schema"`
	Strict bool           `json:"strict,omitempty"`
}

type openaiUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type openaiResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      openaiMessage `json:"message"`
		Delta        openaiMessage `json:"delta"`
		FinishReason string        `json:"finish_reason"`
	} `json:"choices"`
	Usage *openaiUsage `json:"usage"`
}

func (p *openaiProvider) body(req Request, stream bool) openaiRequest {
	model := req.Model
	if model == "" {
		model = p.cfg.Model
	}
	msgs := make([]openaiMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, openaiMessage{Role: string(RoleSystem), Content: req.System})
	}
	for _, m := range req.Messages {
		// One message per tool result, and they precede the text for the same
		// reason as on the other wire: the result answers the call that came
		// before it, and anything else is a reordering the provider notices.
		for _, r := range m.ToolResults {
			content := r.Content
			if r.IsError {
				// There is no is_error field here. Saying so in the content is
				// the only way the model learns the call failed rather than
				// returned this text as data.
				content = "error: " + content
			}
			msgs = append(msgs, openaiMessage{
				Role: "tool", ToolCallID: r.CallID, Content: content,
			})
		}
		if m.Text == "" && len(m.ToolCalls) == 0 {
			continue
		}
		out := openaiMessage{Role: string(m.Role), Content: m.Text}
		for _, c := range m.ToolCalls {
			args := string(c.Input)
			if args == "" {
				args = "{}"
			}
			out.ToolCalls = append(out.ToolCalls, openaiToolCall{
				ID: c.ID, Type: "function",
				Function: openaiFunction{Name: c.Name, Arguments: args},
			})
		}
		msgs = append(msgs, out)
	}
	body := openaiRequest{
		Model:           model,
		Messages:        msgs,
		MaxTokens:       req.MaxTokens,
		Temperature:     req.Temperature,
		Stop:            req.Stop,
		Stream:          stream,
		Tools:           openaiTools(req.Tools),
		ToolChoice:      openaiChoice(req.ToolChoice),
		ReasoningEffort: req.Effort,
	}
	if req.ToolChoice != nil && req.ToolChoice.DisableParallel {
		off := false
		body.ParallelToolCalls = &off
	}
	if req.Format != nil {
		name := req.Format.Name
		if name == "" {
			name = "response"
		}
		body.ResponseFormat = &openaiResponseFmt{
			Type: "json_schema",
			JSONSchema: openaiJSONSchema{
				Name: name, Schema: req.Format.Schema, Strict: req.Format.Strict,
			},
		}
	}
	return body
}

func openaiTools(tools []Tool) []openaiToolDef {
	if len(tools) == 0 {
		return nil
	}
	out := make([]openaiToolDef, 0, len(tools))
	for _, t := range tools {
		out = append(out, openaiToolDef{
			Type: "function",
			Function: openaiFunctionDecl{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Schema,
				Strict:      t.Strict,
			},
		})
	}
	return out
}

// openaiChoice maps our four modes onto this wire's spelling. Only ChoiceAny
// differs in name — "required" here, "any" there — and getting that one wrong
// silently drops the constraint that keeps §3b's small model from answering a
// routing question in prose.
func openaiChoice(c *ToolChoice) any {
	if c == nil || c.Mode == "" {
		return nil
	}
	switch c.Mode {
	case ChoiceAny:
		return "required"
	case ChoiceNone:
		return "none"
	case ChoiceTool:
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": c.Name},
		}
	default:
		return "auto"
	}
}

// openaiCalls converts this wire's calls into ours, parsing the arguments
// string into the object shape the rest of the package uses. Invalid JSON
// becomes an empty object rather than an error: the model asked for a tool and
// the handler is better placed to say what was wrong with the input than a
// decoder is.
func openaiCalls(in []openaiToolCall) []ToolCall {
	if len(in) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(in))
	for _, c := range in {
		raw := json.RawMessage(c.Function.Arguments)
		if !json.Valid(raw) {
			raw = json.RawMessage("{}")
		}
		out = append(out, ToolCall{ID: c.ID, Name: c.Function.Name, Input: raw})
	}
	return out
}

func bearer(h http.Header, key string) { h.Set("Authorization", "Bearer "+key) }

func (p *openaiProvider) Complete(ctx context.Context, req Request) (Response, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	start := time.Now()
	resp, err := post(ctx, p.cfg, "/chat/completions", p.body(req, false), bearer)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()

	var out openaiResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Response{}, err
	}
	if len(out.Choices) == 0 {
		return Response{}, errors.New("llm: provider returned no choices")
	}

	r := Response{
		Model:        out.Model,
		Text:         out.Choices[0].Message.Content,
		FinishReason: out.Choices[0].FinishReason,
		Latency:      time.Since(start),
		ToolCalls:    openaiCalls(out.Choices[0].Message.ToolCalls),
	}
	if out.Usage != nil {
		r.Usage = Usage{
			InputTokens:  out.Usage.PromptTokens,
			OutputTokens: out.Usage.CompletionTokens,
			TotalTokens:  out.Usage.TotalTokens,
		}
	}
	return r, nil
}

func (p *openaiProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	resp, err := post(ctx, p.cfg, "/chat/completions", p.body(req, true), bearer)
	if err != nil {
		return nil, err
	}
	return &openaiStream{sse: newSSEReader(resp.Body)}, nil
}

func (p *openaiProvider) Probe(ctx context.Context) ProbeResult {
	return runProbe(ctx, p, p.cfg)
}

type openaiStream struct {
	sse  *sseReader
	done bool
}

func (s *openaiStream) Recv() (Delta, error) {
	if s.done {
		return Delta{}, io.EOF
	}
	for {
		_, data, err := s.sse.next()
		if err != nil {
			return Delta{}, err
		}
		if data == "[DONE]" {
			s.done = true
			return Delta{Done: true}, nil
		}
		var chunk openaiResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// A frame we cannot parse is not a reason to drop the stream; the
			// next one usually parses. Keep reading.
			continue
		}
		d := Delta{}
		if len(chunk.Choices) > 0 {
			d.Text = chunk.Choices[0].Delta.Content
		}
		if chunk.Usage != nil {
			d.Usage = &Usage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
				TotalTokens:  chunk.Usage.TotalTokens,
			}
		}
		if d.Text == "" && d.Usage == nil {
			continue
		}
		return d, nil
	}
}

func (s *openaiStream) Close() error { return s.sse.Close() }
