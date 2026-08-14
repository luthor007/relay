package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// The Codex wire, which is the Responses API and not chat completions.
//
// A ChatGPT subscription bearer is refused by api.openai.com — the plan buys
// access to https://chatgpt.com/backend-api/codex and nothing else. That
// endpoint speaks Responses: one `input` array of typed items instead of
// `messages`, `instructions` instead of a system message, tool calls and their
// results as items in the same array rather than a parallel field.
//
// Two behaviours of that endpoint are not negotiable and are handled here
// rather than left for a caller to discover:
//
//   - it streams. A non-streaming request is refused, so [codexProvider.Complete]
//     streams and reassembles. Nothing above this file needs to know.
//
//   - it takes a fixed set of parameters and refuses the rest by name. This wire
//     used to send max_output_tokens clamped to a floor of 16, a floor invented
//     here and asserted against a test server, which is why it survived until a
//     real subscription answered:
//
//     http 400: {"detail":"Unsupported parameter: max_output_tokens"}
//
//     So neither max_output_tokens nor temperature is sent — the same two the
//     Codex CLI omits. A caller's Request may carry them; against this endpoint
//     they are dropped rather than turned into a 400 that reads like a broken
//     credential.
type codexProvider struct {
	cfg Config
}

var _ Provider = (*codexProvider)(nil)

func (p *codexProvider) Vendor() string { return p.cfg.Vendor }
func (p *codexProvider) Model() string  { return p.cfg.Model }
func (p *codexProvider) API() API       { return APICodex }

type codexContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// codexItem is one entry in the input array. It is four shapes in one struct
// because the wire distinguishes them by "type" alone: a message, a tool call
// the model made, the result of one, and reasoning it wants echoed back.
type codexItem struct {
	Type    string         `json:"type"`
	Role    string         `json:"role,omitempty"`
	Content []codexContent `json:"content,omitempty"`

	// function_call
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// function_call_output
	Output string `json:"output,omitempty"`
}

type codexTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
	Strict      bool           `json:"strict,omitempty"`
}

type codexReasoning struct {
	Effort string `json:"effort,omitempty"`
}

type codexTextFormat struct {
	Format codexFormat `json:"format"`
}

type codexFormat struct {
	Type   string         `json:"type"`
	Name   string         `json:"name,omitempty"`
	Schema map[string]any `json:"schema,omitempty"`
	Strict bool           `json:"strict,omitempty"`
}

type codexRequest struct {
	Model        string      `json:"model"`
	Instructions string      `json:"instructions,omitempty"`
	Input        []codexItem `json:"input"`
	// Stream is always true. The field stays explicit rather than implied.
	Stream bool `json:"stream"`
	// Store is always false. Relay is not building a thread on OpenAI's side,
	// and an account with retention disabled rejects the request otherwise.
	Store      bool             `json:"store"`
	Tools      []codexTool      `json:"tools,omitempty"`
	ToolChoice any              `json:"tool_choice,omitempty"`
	Parallel   *bool            `json:"parallel_tool_calls,omitempty"`
	Reasoning  *codexReasoning  `json:"reasoning,omitempty"`
	Text       *codexTextFormat `json:"text,omitempty"`
}

func (p *codexProvider) body(req Request) codexRequest {
	model := req.Model
	if model == "" {
		model = p.cfg.Model
	}

	items := make([]codexItem, 0, len(req.Messages)+1)
	for _, m := range req.Messages {
		// Results answer the calls that came before them, and the endpoint
		// rejects a conversation where a call has no matching output — the same
		// rule as both other wires, spelled differently.
		for _, r := range m.ToolResults {
			out := r.Content
			if r.IsError {
				out = "error: " + out
			}
			items = append(items, codexItem{
				Type: "function_call_output", CallID: r.CallID, Output: out,
			})
		}
		for _, c := range m.ToolCalls {
			args := string(c.Input)
			if args == "" {
				args = "{}"
			}
			items = append(items, codexItem{
				Type: "function_call", CallID: c.ID, Name: c.Name, Arguments: args,
			})
		}
		if m.Text == "" {
			continue
		}
		kind := "input_text"
		if m.Role == RoleAssistant {
			kind = "output_text"
		}
		items = append(items, codexItem{
			Type:    "message",
			Role:    string(m.Role),
			Content: []codexContent{{Type: kind, Text: m.Text}},
		})
	}

	body := codexRequest{
		Model:        model,
		Instructions: req.System,
		Input:        items,
		Stream:       true,
		Store:        false,
		Tools:        codexTools(req.Tools),
		ToolChoice:   codexChoice(req.ToolChoice),
	}
	if req.ToolChoice != nil && req.ToolChoice.DisableParallel {
		off := false
		body.Parallel = &off
	}
	if req.Effort != "" {
		body.Reasoning = &codexReasoning{Effort: req.Effort}
	}
	if req.Format != nil {
		name := req.Format.Name
		if name == "" {
			name = "response"
		}
		body.Text = &codexTextFormat{Format: codexFormat{
			Type: "json_schema", Name: name, Schema: req.Format.Schema, Strict: req.Format.Strict,
		}}
	}
	return body
}

func codexTools(tools []Tool) []codexTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]codexTool, 0, len(tools))
	for _, t := range tools {
		// Responses flattens the function one level: no nested "function"
		// object, unlike chat completions.
		out = append(out, codexTool{
			Type: "function", Name: t.Name, Description: t.Description,
			Parameters: t.Schema, Strict: t.Strict,
		})
	}
	return out
}

func codexChoice(c *ToolChoice) any {
	if c == nil || c.Mode == "" {
		return nil
	}
	switch c.Mode {
	case ChoiceAny:
		return "required"
	case ChoiceNone:
		return "none"
	case ChoiceTool:
		return map[string]any{"type": "function", "name": c.Name}
	default:
		return "auto"
	}
}

// codexHeaders authenticates the call and names the account.
//
// The account id is decoded from the bearer rather than carried alongside it,
// which means a refreshed token can never be paired with a stale account id —
// the two cannot drift because there is only one of them.
func codexHeaders(h http.Header, key string) {
	h.Set("Authorization", "Bearer "+key)
	if id := decodeCodexClaims(key).accountID; id != "" {
		h.Set("ChatGPT-Account-Id", id)
	}
	h.Set("Accept", "text/event-stream")
	codexUserAgent(h)
}

// codexEvent is the subset of the event stream that carries an answer.
type codexEvent struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`
	Item  struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"item"`
	Response struct {
		Model  string `json:"model"`
		Status string `json:"status"`
		Usage  *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"response"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (p *codexProvider) Complete(ctx context.Context, req Request) (Response, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	start := time.Now()
	resp, err := post(ctx, p.cfg, "/responses", p.body(req), codexHeaders)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()

	out := Response{Model: p.cfg.Model}
	var text strings.Builder
	sse := newSSEReader(resp.Body)
	for {
		_, data, err := sse.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Response{}, err
		}
		if data == "[DONE]" {
			break
		}
		var ev codexEvent
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "response.output_text.delta":
			text.WriteString(ev.Delta)
		case "response.output_item.done":
			if ev.Item.Type == "function_call" {
				args := json.RawMessage(ev.Item.Arguments)
				if !json.Valid(args) {
					args = json.RawMessage("{}")
				}
				id := ev.Item.CallID
				if id == "" {
					id = ev.Item.ID
				}
				out.ToolCalls = append(out.ToolCalls,
					ToolCall{ID: id, Name: ev.Item.Name, Input: args})
			}
		case "response.completed", "response.incomplete":
			if ev.Response.Model != "" {
				out.Model = ev.Response.Model
			}
			if u := ev.Response.Usage; u != nil {
				out.Usage = Usage{
					InputTokens:  u.InputTokens,
					OutputTokens: u.OutputTokens,
					TotalTokens:  u.TotalTokens,
				}
			}
			out.FinishReason = ev.Response.Status
			if d := ev.Response.IncompleteDetails; d != nil && d.Reason != "" {
				out.FinishReason = d.Reason
			}
		case "response.failed", "error":
			// A failure arrives as an event with a 200 around it, so it has to
			// be turned back into an error here or the caller sees an empty
			// success and reports the model as silent.
			msg := "the Codex endpoint failed the response"
			if ev.Response.Error != nil && ev.Response.Error.Message != "" {
				msg = ev.Response.Error.Message
			} else if ev.Error != nil && ev.Error.Message != "" {
				msg = ev.Error.Message
			}
			return Response{}, &HTTPError{Status: http.StatusBadGateway, Body: msg}
		}
	}
	out.Text = text.String()
	out.Latency = time.Since(start)
	return out, nil
}

func (p *codexProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	resp, err := post(ctx, p.cfg, "/responses", p.body(req), codexHeaders)
	if err != nil {
		return nil, err
	}
	return &codexStream{sse: newSSEReader(resp.Body)}, nil
}

func (p *codexProvider) Probe(ctx context.Context) ProbeResult {
	return runProbe(ctx, p, p.cfg)
}

type codexStream struct {
	sse  *sseReader
	done bool
}

func (s *codexStream) Recv() (Delta, error) {
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
		var ev codexEvent
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "response.output_text.delta":
			if ev.Delta == "" {
				continue
			}
			return Delta{Text: ev.Delta}, nil
		case "response.completed", "response.incomplete":
			s.done = true
			d := Delta{Done: true}
			if u := ev.Response.Usage; u != nil {
				d.Usage = &Usage{
					InputTokens:  u.InputTokens,
					OutputTokens: u.OutputTokens,
					TotalTokens:  u.TotalTokens,
				}
			}
			return d, nil
		}
	}
}

func (s *codexStream) Close() error { return s.sse.Close() }
