package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/llm"
)

// scriptedTransport answers each call with the next body in the script and
// records what was sent. No test in this package opens a socket.
type scriptedTransport struct {
	mu     sync.Mutex
	script []string
	sent   []string
	n      int
}

func (s *scriptedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(r.Body)

	s.mu.Lock()
	s.sent = append(s.sent, string(raw))
	i := s.n
	s.n++
	s.mu.Unlock()

	if i >= len(s.script) {
		return nil, errors.New("scripted transport: ran out of script")
	}
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(s.script[i])),
		Request:    r,
	}, nil
}

func (s *scriptedTransport) body(i int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.sent) {
		return ""
	}
	return s.sent[i]
}

func (s *scriptedTransport) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// anthropicToolUse and friends build the wire bodies the provider will decode.
func anthropicToolUse(id, name, input string) string {
	return `{"model":"claude-opus-5","stop_reason":"tool_use","content":[
		{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":` + input + `}],
		"usage":{"input_tokens":10,"output_tokens":5}}`
}

func anthropicText(text string) string {
	return `{"model":"claude-opus-5","stop_reason":"end_turn","content":[
		{"type":"text","text":"` + text + `"}],
		"usage":{"input_tokens":10,"output_tokens":5}}`
}

func openaiToolCall(id, name, args string) string {
	raw, _ := json.Marshal(args)
	return `{"model":"anthropic/opus-5","choices":[{"message":{"role":"assistant",
		"tool_calls":[{"id":"` + id + `","type":"function","function":{"name":"` + name + `",
		"arguments":` + string(raw) + `}}]},"finish_reason":"tool_calls"}]}`
}

func provider(t *testing.T, api llm.API, tr http.RoundTripper) llm.Provider {
	t.Helper()
	vendor, model := "openrouter", "anthropic/opus-5"
	if api == llm.APIAnthropic {
		vendor, model = "anthropic", "claude-opus-5"
	}
	p, err := llm.New(llm.Config{
		Vendor: vendor, API: api, Model: model,
		Credential: llm.CredentialRef{Kind: llm.RefInline, Value: "sk-test-abcd"},
		HTTPClient: &http.Client{Transport: tr},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// searchTool is the shape of a real orchestrator tool: read-only, so
// parallel-safe, and a description that says when to call it.
func searchTool(run llm.ToolFunc) llm.Binding {
	return llm.Binding{
		Tool: llm.Tool{
			Name:         "search_memory",
			Description:  "Search past sessions. Call this when the user asks what was decided, tried, or discussed before.",
			Schema:       map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}},
			ParallelSafe: true,
		},
		Run: run,
	}
}

func startTool(run llm.ToolFunc) llm.Binding {
	return llm.Binding{
		Tool: llm.Tool{
			Name:        "start_session",
			Description: "Start a new agent session. Call this when the user asks for work that is not already running.",
			Schema:      map[string]any{"type": "object", "properties": map[string]any{"prompt": map[string]any{"type": "string"}}},
		},
		Run: run,
	}
}

// TestLoopRunsAToolAndFeedsTheResultBack is the whole point of the file: the
// orchestrator can now do the thing ORCHESTRATOR.md §3b gives the big model,
// which internal/llm previously said in a comment it would never need.
func TestLoopRunsAToolAndFeedsTheResultBack(t *testing.T) {
	tr := &scriptedTransport{script: []string{
		anthropicToolUse("call_1", "search_memory", `{"query":"crc"}`),
		anthropicText("You settled on MODBUS."),
	}}

	var sawQuery string
	loop := &llm.Loop{
		Provider: provider(t, llm.APIAnthropic, tr),
		Tools: llm.Toolbox{searchTool(func(_ context.Context, c llm.ToolCall) (llm.ToolResult, error) {
			sawQuery = c.Arg("query")
			return llm.ToolResult{Content: "the CRC was MODBUS, not ARC"}, nil
		})},
	}

	res, err := loop.Run(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "what did we decide about the CRC"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sawQuery != "crc" {
		t.Errorf("the tool got query %q", sawQuery)
	}
	if res.Text != "You settled on MODBUS." {
		t.Errorf("final text = %q", res.Text)
	}
	if res.Turns != 2 || res.ToolCalls != 1 {
		t.Errorf("turns=%d toolCalls=%d, want 2 and 1", res.Turns, res.ToolCalls)
	}
	if !res.Stop.OK() {
		t.Errorf("stop = %q", res.Stop)
	}

	// The second request has to carry the tool result back, keyed to the call
	// id. Both wires reject the turn otherwise, and a loop that drops it turns
	// into an infinite retry of the same call.
	second := tr.body(1)
	for _, want := range []string{`"tool_result"`, `"call_1"`, "MODBUS, not ARC"} {
		if !strings.Contains(second, want) {
			t.Errorf("the follow-up request is missing %s:\n%s", want, second)
		}
	}
}

// TestLoopAnswersEveryCallEvenWhenDenied covers the rule that makes denial
// safe: a dropped tool_use is a 400 from both providers, so "deny" has to mean
// an error result rather than silence.
func TestLoopAnswersEveryCallEvenWhenDenied(t *testing.T) {
	tr := &scriptedTransport{script: []string{
		anthropicToolUse("call_1", "start_session", `{"prompt":"delete prod"}`),
		anthropicText("I did not start it."),
	}}

	var ran atomic.Bool
	loop := &llm.Loop{
		Provider: provider(t, llm.APIAnthropic, tr),
		Tools: llm.Toolbox{startTool(func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
			ran.Store(true)
			return llm.ToolResult{Content: "started"}, nil
		})},
		Hooks: llm.Hooks{
			BeforeTool: func(_ context.Context, c llm.ToolCall) llm.Decision {
				return llm.Decision{Verdict: llm.VerdictDeny, Reason: "that would touch production"}
			},
		},
	}

	if _, err := loop.Run(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "clean up prod"}},
	}); err != nil {
		t.Fatal(err)
	}
	if ran.Load() {
		t.Error("the denied tool ran anyway")
	}

	second := tr.body(1)
	if !strings.Contains(second, `"call_1"`) {
		t.Errorf("the denied call was never answered:\n%s", second)
	}
	if !strings.Contains(second, "touch production") {
		t.Errorf("the model was not told why:\n%s", second)
	}
	if !strings.Contains(second, `"is_error":true`) {
		t.Errorf("the denial was reported as data rather than an error:\n%s", second)
	}
}

// TestDenyIsTerminal is OpenClaw's before_tool_call rule, and the one asymmetry
// in Decision.Merge that has to be right: a policy with no opinion, or a later
// permissive one, must not clear a block.
func TestDenyIsTerminal(t *testing.T) {
	deny := llm.Decision{Verdict: llm.VerdictDeny, Reason: "no"}
	allow := llm.Decision{Verdict: llm.VerdictAllow}
	ask := llm.Decision{}

	if got := deny.Merge(allow); got.Verdict != llm.VerdictDeny {
		t.Error("an allow cleared a deny")
	}
	if got := deny.Merge(ask); got.Verdict != llm.VerdictDeny {
		t.Error("a policy with no opinion cleared a deny")
	}
	if got := allow.Merge(deny); got.Verdict != llm.VerdictDeny {
		t.Error("a later deny did not take")
	}
	if got := ask.Merge(allow); got.Verdict != llm.VerdictAllow {
		t.Error("an allow did not resolve an ask")
	}
	// The zero value is Ask rather than Allow, so a policy that forgets to
	// decide has not thereby approved anything.
	if (llm.Decision{}).Verdict != llm.VerdictAsk {
		t.Error("the zero decision is not Ask")
	}
}

// TestAskWithNobodyListeningDenies is the fail-closed case. A question raised
// where no console, phone or test is subscribed cannot be answered, and an
// unanswerable question must never become consent.
func TestAskWithNobodyListeningDenies(t *testing.T) {
	tr := &scriptedTransport{script: []string{
		anthropicToolUse("call_1", "start_session", `{"prompt":"go"}`),
		anthropicText("I could not."),
	}}

	var ran atomic.Bool
	loop := &llm.Loop{
		Provider: provider(t, llm.APIAnthropic, tr),
		Tools: llm.Toolbox{startTool(func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
			ran.Store(true)
			return llm.ToolResult{Content: "started"}, nil
		})},
		Hooks: llm.Hooks{BeforeTool: func(context.Context, llm.ToolCall) llm.Decision {
			return llm.Decision{} // Ask, with no Emit wired.
		}},
	}

	if _, err := loop.Run(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "go"}},
	}); err != nil {
		t.Fatal(err)
	}
	if ran.Load() {
		t.Error("a question nobody could answer was treated as approval")
	}
}

// TestAskBlocksUntilAnswered covers the path Claude Code promotes to a tool and
// the reason it does: the loop stops, a human answers through the same
// event.NeedsInput the runtime adapters raise, and only then does anything run.
func TestAskBlocksUntilAnswered(t *testing.T) {
	tr := &scriptedTransport{script: []string{
		anthropicToolUse("call_1", "start_session", `{"prompt":"ship it"}`),
		anthropicText("Started."),
	}}

	var ran atomic.Bool
	var asked atomic.Int32

	loop := &llm.Loop{
		Provider: provider(t, llm.APIAnthropic, tr),
		Tools: llm.Toolbox{startTool(func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
			ran.Store(true)
			return llm.ToolResult{Content: "session sess_1 started"}, nil
		})},
		Hooks: llm.Hooks{BeforeTool: func(context.Context, llm.ToolCall) llm.Decision {
			return llm.Decision{Reason: "Start a session for “ship it”?"}
		}},
		Emit: func(e event.Event) {
			q, ok := e.(*event.NeedsInput)
			if !ok {
				return
			}
			asked.Add(1)
			// A blocking question, so quiet hours never suppress it.
			if q.Ping() != event.PingBlocking {
				t.Errorf("a blocked session pings %v", q.Ping())
			}
			if ran.Load() {
				t.Error("the tool ran before the question was answered")
			}
			go func() {
				_ = q.Reply(context.Background(), event.Reply{OptionID: "allow", Decision: event.DecisionAllow})
			}()
		},
	}

	if _, err := loop.Run(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "ship it"}},
	}); err != nil {
		t.Fatal(err)
	}
	if asked.Load() != 1 {
		t.Errorf("asked %d times, want 1", asked.Load())
	}
	if !ran.Load() {
		t.Error("the approved tool never ran")
	}
}

// TestOnlyParallelSafeToolsOverlap is the distinction a harness cannot recover
// after the fact. Two searches may run together; two session starts may not,
// and getting that wrong starts two agents where the user asked for one.
func TestOnlyParallelSafeToolsOverlap(t *testing.T) {
	batch := func(name string) string {
		return `{"model":"claude-opus-5","stop_reason":"tool_use","content":[
			{"type":"tool_use","id":"a","name":"` + name + `","input":{}},
			{"type":"tool_use","id":"b","name":"` + name + `","input":{}}],
			"usage":{"input_tokens":1,"output_tokens":1}}`
	}

	for _, tc := range []struct {
		name    string
		bind    func(llm.ToolFunc) llm.Binding
		overlap bool
	}{
		{"search_memory", searchTool, true},
		{"start_session", startTool, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := &scriptedTransport{script: []string{batch(tc.name), anthropicText("done")}}

			var mu sync.Mutex
			var live, peak int
			run := func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
				mu.Lock()
				live++
				if live > peak {
					peak = live
				}
				mu.Unlock()
				time.Sleep(20 * time.Millisecond)
				mu.Lock()
				live--
				mu.Unlock()
				return llm.ToolResult{Content: "ok"}, nil
			}

			loop := &llm.Loop{
				Provider: provider(t, llm.APIAnthropic, tr),
				Tools:    llm.Toolbox{tc.bind(run)},
			}
			if _, err := loop.Run(context.Background(), llm.Request{
				Messages: []llm.Message{{Role: llm.RoleUser, Text: "go"}},
			}); err != nil {
				t.Fatal(err)
			}

			mu.Lock()
			defer mu.Unlock()
			if tc.overlap && peak < 2 {
				t.Errorf("%s is parallel-safe but ran one at a time", tc.name)
			}
			if !tc.overlap && peak > 1 {
				t.Errorf("%s ran %d at once; anything unmarked must be serialised", tc.name, peak)
			}
		})
	}
}

// TestAnUnsafeToolIsABarrier covers the mixed batch, which is the case the
// obvious implementation gets wrong: a read already in flight must not overlap
// a write just because the read was declared safe.
func TestAnUnsafeToolIsABarrier(t *testing.T) {
	tr := &scriptedTransport{script: []string{
		`{"model":"claude-opus-5","stop_reason":"tool_use","content":[
			{"type":"tool_use","id":"a","name":"search_memory","input":{}},
			{"type":"tool_use","id":"b","name":"start_session","input":{}}],
			"usage":{"input_tokens":1,"output_tokens":1}}`,
		anthropicText("done"),
	}}

	var mu sync.Mutex
	live := map[string]bool{}
	var overlapped bool
	watch := func(name string) llm.ToolFunc {
		return func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
			mu.Lock()
			live[name] = true
			if len(live) > 1 {
				overlapped = true
			}
			mu.Unlock()
			time.Sleep(25 * time.Millisecond)
			mu.Lock()
			delete(live, name)
			mu.Unlock()
			return llm.ToolResult{Content: "ok"}, nil
		}
	}

	loop := &llm.Loop{
		Provider: provider(t, llm.APIAnthropic, tr),
		Tools: llm.Toolbox{
			searchTool(watch("search_memory")),
			startTool(watch("start_session")),
		},
	}
	if _, err := loop.Run(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "go"}},
	}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if overlapped {
		t.Error("a read ran concurrently with a write; an unsafe call has to be a barrier")
	}
}

// TestSteeringLandsAtTheModelBoundary is the position OpenClaw drains at, and
// the position is the whole design: after the tool results are paired with the
// call that produced them, before the next model call.
func TestSteeringLandsAtTheModelBoundary(t *testing.T) {
	tr := &scriptedTransport{script: []string{
		anthropicToolUse("call_1", "search_memory", `{"query":"crc"}`),
		anthropicText("Using staging."),
	}}

	q := llm.NewQueue(llm.QueueSteer)
	loop := &llm.Loop{
		Provider: provider(t, llm.APIAnthropic, tr),
		Tools: llm.Toolbox{searchTool(func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
			// Spoken while the tool is running — the case that matters.
			q.Attach(func() {})
			if got := q.Push("actually, use staging"); got != llm.DispositionSteered {
				t.Errorf("disposition = %q", got)
			}
			return llm.ToolResult{Content: "found nothing"}, nil
		})},
		Hooks: llm.Hooks{Boundary: q.Boundary},
	}

	res, err := loop.Run(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "check the CRC"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The steered words must arrive after the tool results, not spliced in
	// between the call and its answer.
	var toolResultAt, steerAt = -1, -1
	for i, m := range res.Messages {
		if len(m.ToolResults) > 0 {
			toolResultAt = i
		}
		if m.Text == "actually, use staging" {
			steerAt = i
		}
	}
	if steerAt < 0 {
		t.Fatal("the steered utterance never reached the conversation")
	}
	if steerAt < toolResultAt {
		t.Errorf("steering landed at %d, before the tool results at %d: that splits a call from its answer",
			steerAt, toolResultAt)
	}
	if !strings.Contains(tr.body(1), "use staging") {
		t.Errorf("the next model call did not carry the steered words:\n%s", tr.body(1))
	}
}

// TestLoopLooksLikeAnAdapterOnTheBus is why the loop emits event.Event rather
// than its own vocabulary: the console, the pinger and the phone already
// understand these nine kinds, and none of them should be able to tell whether
// the work happened in Claude Code or in here.
func TestLoopLooksLikeAnAdapterOnTheBus(t *testing.T) {
	tr := &scriptedTransport{script: []string{
		anthropicToolUse("call_1", "search_memory", `{"query":"crc"}`),
		anthropicText("MODBUS."),
	}}

	var mu sync.Mutex
	var kinds []event.Kind
	var seqs []uint64

	loop := &llm.Loop{
		Provider: provider(t, llm.APIAnthropic, tr),
		Tools: llm.Toolbox{searchTool(func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
			return llm.ToolResult{Content: "MODBUS"}, nil
		})},
		Meta: event.Meta{Runtime: "relay", Session: "sess_1"},
		Emit: func(e event.Event) {
			mu.Lock()
			defer mu.Unlock()
			kinds = append(kinds, e.Kind())
			seqs = append(seqs, e.Envelope().Seq)
			if e.Envelope().Session != "sess_1" {
				t.Errorf("event %s lost its session", e.Kind())
			}
		},
	}
	if _, err := loop.Run(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "crc?"}},
	}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, want := range []event.Kind{
		event.KindTurnStarted, event.KindToolStarted,
		event.KindToolOutput, event.KindTurnCompleted,
	} {
		if !containsKind(kinds, want) {
			t.Errorf("the loop never emitted %s; got %v", want, kinds)
		}
	}
	// Per-session monotonic, as ADAPTERS.md §5 requires of an adapter.
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("sequence went %d then %d", seqs[i-1], seqs[i])
		}
	}
}

func containsKind(all []event.Kind, want event.Kind) bool {
	for _, k := range all {
		if k == want {
			return true
		}
	}
	return false
}

// TestLoopStopsAtItsIterationLimit: a model that never stops asking must not
// spend the user's money until something else notices.
func TestLoopStopsAtItsIterationLimit(t *testing.T) {
	script := make([]string, 8)
	for i := range script {
		script[i] = anthropicToolUse("call_x", "search_memory", `{"query":"again"}`)
	}
	tr := &scriptedTransport{script: script}

	loop := &llm.Loop{
		Provider: provider(t, llm.APIAnthropic, tr),
		Tools: llm.Toolbox{searchTool(func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
			return llm.ToolResult{Content: "nothing"}, nil
		})},
		MaxIterations: 3,
	}
	res, err := loop.Run(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "loop forever"}},
	})
	if !errors.Is(err, llm.ErrMaxIterations) {
		t.Fatalf("err = %v, want ErrMaxIterations", err)
	}
	if res.Turns != 3 || tr.calls() != 3 {
		t.Errorf("turns=%d calls=%d, want 3 and 3", res.Turns, tr.calls())
	}
	if res.Stop != event.StopMaxTurnRequests {
		t.Errorf("stop = %q", res.Stop)
	}
	if !res.Stop.Retryable() {
		t.Error("hitting the limit should be retryable — the work is not refused, just unfinished")
	}
}

// TestOpenAIWireCarriesToolsBothWays keeps the recommended provider working.
// OpenRouter speaks this shape, so a tool loop that only worked on the
// Anthropic wire would be a tool loop that did not work by default.
func TestOpenAIWireCarriesToolsBothWays(t *testing.T) {
	tr := &scriptedTransport{script: []string{
		openaiToolCall("call_1", "search_memory", `{"query":"crc"}`),
		`{"model":"anthropic/opus-5","choices":[{"message":{"role":"assistant","content":"MODBUS."},
			"finish_reason":"stop"}]}`,
	}}

	loop := &llm.Loop{
		Provider: provider(t, llm.APIOpenAI, tr),
		Tools: llm.Toolbox{searchTool(func(_ context.Context, c llm.ToolCall) (llm.ToolResult, error) {
			if c.Arg("query") != "crc" {
				t.Errorf("the arguments string did not decode: %s", c.Input)
			}
			return llm.ToolResult{Content: "it was MODBUS"}, nil
		})},
	}
	res, err := loop.Run(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "crc?"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "MODBUS." {
		t.Errorf("text = %q", res.Text)
	}

	first := tr.body(0)
	if !strings.Contains(first, `"type":"function"`) || !strings.Contains(first, "search_memory") {
		t.Errorf("the tool was not declared on the OpenAI wire:\n%s", first)
	}
	second := tr.body(1)
	if !strings.Contains(second, `"role":"tool"`) || !strings.Contains(second, `"tool_call_id":"call_1"`) {
		t.Errorf("the result did not go back as a tool message:\n%s", second)
	}
}

// TestToolChoiceAnyIsSpelledPerWire covers the one mapping whose failure is
// silent. "any" is not a value this wire knows; a provider that ignores it lets
// the small model answer a routing question in prose, which is ORCHESTRATOR.md
// §3b's under-escalation failure — the expensive one.
func TestToolChoiceAnyIsSpelledPerWire(t *testing.T) {
	for _, tc := range []struct {
		api  llm.API
		body string
		want string
	}{
		{llm.APIOpenAI, `{"model":"m","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`, `"tool_choice":"required"`},
		{llm.APIAnthropic, anthropicText("hi"), `"tool_choice":{"type":"any"`},
	} {
		tr := &scriptedTransport{script: []string{tc.body}}
		p := provider(t, tc.api, tr)
		_, err := p.Complete(context.Background(), llm.Request{
			Messages:   []llm.Message{{Role: llm.RoleUser, Text: "who handles this"}},
			Tools:      []llm.Tool{searchTool(nil).Tool},
			ToolChoice: &llm.ToolChoice{Mode: llm.ChoiceAny},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(tr.body(0), tc.want) {
			t.Errorf("%s wire: missing %s in\n%s", tc.api, tc.want, tr.body(0))
		}
	}
}

// TestUnknownToolIsAnErrorResultNotADeadLoop: the model can recover from "no
// such tool" and cannot recover from a run that stopped without telling it why.
func TestUnknownToolIsAnErrorResultNotADeadLoop(t *testing.T) {
	tr := &scriptedTransport{script: []string{
		anthropicToolUse("call_1", "delete_everything", `{}`),
		anthropicText("Understood."),
	}}
	loop := &llm.Loop{
		Provider: provider(t, llm.APIAnthropic, tr),
		Tools: llm.Toolbox{searchTool(func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
			return llm.ToolResult{Content: "x"}, nil
		})},
	}
	if _, err := loop.Run(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "go"}},
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tr.body(1), "no tool named") {
		t.Errorf("the model was not told the tool does not exist:\n%s", tr.body(1))
	}
}
