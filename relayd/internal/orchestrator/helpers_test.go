package orchestrator_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/luthor007/relay/relayd/internal/llm"
)

// countingTransport answers from a script and counts calls. Nothing in this
// package opens a socket.
type countingTransport struct {
	script []string
	onCall func()

	calls atomic.Int64

	mu   sync.Mutex
	sent []string
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if c.onCall != nil {
		c.onCall()
	}
	raw, _ := io.ReadAll(r.Body)

	c.mu.Lock()
	c.sent = append(c.sent, string(raw))
	c.mu.Unlock()

	i := int(c.calls.Add(1)) - 1
	if i >= len(c.script) {
		return nil, errors.New("countingTransport: ran out of script")
	}
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(c.script[i])),
		Request:    r,
	}, nil
}

func (c *countingTransport) body(i int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if i >= len(c.sent) {
		return ""
	}
	return c.sent[i]
}

func testProvider(t *testing.T, tr http.RoundTripper) llm.Provider {
	t.Helper()
	p, err := llm.New(llm.Config{
		Vendor: "anthropic", API: llm.APIAnthropic, Model: "claude-opus-5",
		Credential: llm.CredentialRef{Kind: llm.RefInline, Value: "sk-test-abcd"},
		HTTPClient: &http.Client{Transport: tr},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func toolUse(id, name, input string) string {
	return `{"model":"claude-opus-5","stop_reason":"tool_use","content":[
		{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":` + input + `}],
		"usage":{"input_tokens":10,"output_tokens":5}}`
}

func textReply(text string) string {
	return `{"model":"claude-opus-5","stop_reason":"end_turn","content":[
		{"type":"text","text":"` + text + `"}],"usage":{"input_tokens":10,"output_tokens":5}}`
}

// runTool finds a binding by the call's own name and runs it.
//
// By name rather than by index: the toolbox order is not a contract, and a test
// that indexes into it breaks whenever a tool is added — which is exactly the
// change most likely to need the test.
func runTool(t *testing.T, box llm.Toolbox, ctx context.Context, call llm.ToolCall) (llm.ToolResult, error) {
	t.Helper()
	for _, b := range box {
		if b.Tool.Name == call.Name {
			return b.Run(ctx, call)
		}
	}
	t.Fatalf("no tool named %q in the box", call.Name)
	return llm.ToolResult{}, nil
}
