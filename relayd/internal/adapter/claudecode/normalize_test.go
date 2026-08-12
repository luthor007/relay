package claudecode

import (
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/logx"
)

// collector runs the normalizer on its own, without a process or a session.
type collector struct {
	n      *normalizer
	events []event.Event
	inits  []InitInfo
}

func newCollector(t *testing.T, replay bool) *collector {
	t.Helper()
	c := &collector{}
	c.n = newNormalizer(normOptions{
		Runtime: "claude-code",
		Session: "s1",
		Log:     logx.Discard(),
		Now:     fixedClock(),
		Out:     func(ev event.Event) { c.events = append(c.events, ev) },
		OnInit:  func(i InitInfo) { c.inits = append(c.inits, i) },
		Replay:  replay,
	})
	return c
}

func (c *collector) push(lines ...string) {
	for _, l := range lines {
		c.n.push([]byte(l))
	}
}

func (c *collector) kinds() []event.Kind {
	out := make([]event.Kind, 0, len(c.events))
	for _, e := range c.events {
		out = append(out, e.Kind())
	}
	return out
}

func kindsEqual(t *testing.T, got []event.Kind, want ...event.Kind) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", got, want)
		}
	}
}

// The two `user` shapes share a type and mean opposite things. ADAPTERS.md §2:
// discriminate on key *presence*, never on isReplay == false.
func TestUserShapesAreDiscriminatedOnKeyPresence(t *testing.T) {
	c := newCollector(t, false)
	c.n.queueTurn("t1", "hello")
	c.push(
		`{"type":"user","isReplay":true,"message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_1","type":"tool_result","content":"out","is_error":false}]},"tool_use_result":{"stdout":"out"}}`,
	)
	kindsEqual(t, c.kinds(), event.KindTurnStarted, event.KindToolOutput)
	if c.events[0].Envelope().Turn != "t1" {
		t.Errorf("the echo must adopt the id of the turn we sent, got %q", c.events[0].Envelope().Turn)
	}

	// isReplay: false is still an echo, not a tool result.
	c2 := newCollector(t, false)
	c2.n.queueTurn("t1", "hello")
	c2.push(`{"type":"user","isReplay":false,"message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`)
	kindsEqual(t, c2.kinds(), event.KindTurnStarted)
}

func TestToolResultFailureIsAFailedStatus(t *testing.T) {
	c := newCollector(t, false)
	c.push(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_1","type":"tool_result","content":[{"type":"text","text":"boom"}],"is_error":true}]}}`)
	kindsEqual(t, c.kinds(), event.KindToolOutput)
	got := c.events[0].(event.ToolOutput)
	if got.Status != event.ToolFailed || got.Chunk != "boom" {
		t.Errorf("tool output = %+v", got)
	}
}

// system/status fires once per API request. Two turns produced three of them in
// the recording, so it can never be a turn boundary.
func TestStatusIsNotATurnBoundary(t *testing.T) {
	c := newCollector(t, false)
	c.push(
		`{"type":"system","subtype":"status","status":"requesting"}`,
		`{"type":"system","subtype":"status","status":"requesting"}`,
	)
	if len(c.events) != 0 {
		t.Fatalf("status produced %v", c.kinds())
	}
	if c.n.unseen()["status:requesting"] != 2 {
		t.Errorf("status should still be counted: %v", c.n.unseen())
	}
}

// A reader that hard-fails on an unknown system.subtype crashes on the very
// first line of a real session, because hooks arrive before init and were
// undocumented until the fixture was recorded.
func TestUnknownShapesAreNeverFatal(t *testing.T) {
	c := newCollector(t, false)
	c.push(
		`{"type":"system","subtype":"a_subtype_from_the_future"}`,
		`{"type":"a_type_from_the_future","payload":{"x":1}}`,
		`{"type":"stream_event","event":{"type":"a_stream_type_from_the_future"}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"abc"}}}`,
		`not json at all`,
		``,
	)
	if len(c.events) != 0 {
		t.Fatalf("unknown shapes produced %v", c.kinds())
	}
	u := c.n.unseen()
	for _, k := range []string{
		"system:a_subtype_from_the_future", "type:a_type_from_the_future",
		"stream:a_stream_type_from_the_future", "delta:citations_delta",
		"delta:signature_delta", "!malformed",
	} {
		if u[k] != 1 {
			t.Errorf("unseen[%q] = %d, want 1: %v", k, u[k], u)
		}
	}
}

// thinking_delta is not in the vendored fixture. The adapter handles it because
// the delta exists in the protocol, and it emits Reasoning only when one
// actually arrives — observation, not invention.
func TestThinkingDeltaBecomesReasoning(t *testing.T) {
	c := newCollector(t, false)
	c.push(
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m1","usage":{"input_tokens":1}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}}`,
	)
	kindsEqual(t, c.kinds(), event.KindReasoning)
	if r := c.events[0].(event.Reasoning); r.Text != "hmm" || r.Ping() != event.PingNone {
		t.Errorf("reasoning = %+v", r)
	}
}

// The tool call arrives twice — assembled on the assistant event and again when
// its content block stops. ToolStarted must be emitted once.
func TestToolStartedIsEmittedOnce(t *testing.T) {
	c := newCollector(t, false)
	c.push(
		`{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\": \"ls"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"}"}}}`,
		`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":1}}`,
	)
	kindsEqual(t, c.kinds(), event.KindToolStarted)
	ts := c.events[0].(event.ToolStarted)
	if ts.Tool != "Bash" || ts.Target != "ls" || ts.RawInput["command"] != "ls" {
		t.Errorf("tool started = %+v", ts)
	}
}

// Without the assistant event, the accumulated input_json fragments are what we
// have; they concatenate to the object.
func TestToolStartedFromStreamedArgumentsAlone(t *testing.T) {
	c := newCollector(t, false)
	c.push(
		`{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_2","name":"Read","input":{}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":""}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\": \"/a/b.go\"}"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":1}}`,
	)
	kindsEqual(t, c.kinds(), event.KindToolStarted)
	if got := c.events[0].(event.ToolStarted).Target; got != "/a/b.go" {
		t.Errorf("target = %q", got)
	}
}

// Truncated arguments must still report the call: the tool is about to run and
// the user is about to see it happen. A nil RawInput is "not reported".
func TestToolStartedSurvivesUnparsableArguments(t *testing.T) {
	c := newCollector(t, false)
	c.push(
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_3","name":"Bash","input":{}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\": \"ls"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
	)
	kindsEqual(t, c.kinds(), event.KindToolStarted)
	ts := c.events[0].(event.ToolStarted)
	if ts.RawInput != nil || ts.Target != "" {
		t.Errorf("a half-streamed input must not be guessed at: %+v", ts)
	}
}

// assistant is per content block, not per message. Text that already streamed
// must not be emitted twice; text that never streamed must not be lost.
func TestAssistantTextIsNotDoubleCounted(t *testing.T) {
	c := newCollector(t, false)
	c.push(
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m1","usage":{"input_tokens":1}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}}`,
		`{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"hi"}]}}`,
	)
	kindsEqual(t, c.kinds(), event.KindTextDelta)

	c2 := newCollector(t, false)
	c2.push(`{"type":"assistant","message":{"id":"m9","content":[{"type":"text","text":"no partials were requested"}]}}`)
	kindsEqual(t, c2.kinds(), event.KindTextDelta)
}

// A failing SessionStart hook is a user-visible misconfiguration and a common
// reason a session behaves oddly from the first turn. Responses are correlated
// by hook_id because hooks run concurrently.
func TestHookFailureIsReported(t *testing.T) {
	c := newCollector(t, false)
	c.push(
		`{"type":"system","subtype":"hook_started","hook_id":"a","hook_name":"SessionStart:startup","hook_event":"SessionStart"}`,
		`{"type":"system","subtype":"hook_started","hook_id":"b","hook_name":"SessionStart:startup","hook_event":"SessionStart"}`,
		`{"type":"system","subtype":"hook_response","hook_id":"b","hook_name":"SessionStart:startup","exit_code":0,"outcome":"success"}`,
		`{"type":"system","subtype":"hook_response","hook_id":"a","hook_name":"SessionStart:startup","exit_code":2,"outcome":"error","stderr":"no such file"}`,
	)
	kindsEqual(t, c.kinds(), event.KindError)
	e := c.events[0].(event.Error)
	if e.Code != "hook_failed" || e.Fatal {
		t.Errorf("hook error = %+v", e)
	}
	hooks := c.n.hookRuns()
	if len(hooks) != 2 || hooks[0].ID != "a" || !hooks[1].OK() || hooks[0].OK() {
		t.Errorf("hooks = %+v", hooks)
	}
}

// "allowed" is the ordinary state and is not news; anything else is.
func TestRateLimitOnlyReportsWhenItRestricts(t *testing.T) {
	c := newCollector(t, false)
	c.push(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rateLimitType":"five_hour","resetsAt":1786345200}}`)
	if len(c.events) != 0 {
		t.Fatalf("an allowed quota produced %v", c.kinds())
	}
	c.push(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour","resetsAt":1786345200}}`)
	kindsEqual(t, c.kinds(), event.KindError)
	e := c.events[0].(event.Error)
	if e.Code != "rate_limit" || e.Retryable {
		t.Errorf("rate limit error = %+v: Claude Code does not retry this by itself", e)
	}
	if e.Ping() != event.PingInformational {
		t.Errorf("ping = %v", e.Ping())
	}
	if c.n.rateLimitInfo().Status != "rejected" {
		t.Error("the raw struct must still be surfaced for the console")
	}
}

func TestResultStopReasons(t *testing.T) {
	for _, tc := range []struct {
		line string
		want event.StopReason
		ok   bool
	}{
		{`{"type":"result","subtype":"success","stop_reason":"end_turn","is_error":false,"duration_ms":10}`, event.StopEndTurn, true},
		{`{"type":"result","subtype":"success","stop_reason":"max_tokens","is_error":false}`, event.StopMaxTokens, false},
		{`{"type":"result","subtype":"error_max_turns","is_error":true}`, event.StopMaxTurnRequests, false},
		{`{"type":"result","subtype":"error_during_execution","is_error":true}`, event.StopError, false},
		{`{"type":"result","subtype":"a_subtype_from_the_future","is_error":false}`, event.StopError, false},
	} {
		c := newCollector(t, false)
		c.push(tc.line)
		var tc2 *event.TurnCompleted
		for _, e := range c.events {
			if v, ok := e.(event.TurnCompleted); ok {
				tc2 = &v
			}
		}
		if tc2 == nil {
			t.Fatalf("%s produced no TurnCompleted", tc.line)
		}
		if tc2.StopReason != tc.want || tc2.OK != tc.ok {
			t.Errorf("%s -> %v ok=%v, want %v ok=%v", tc.line, tc2.StopReason, tc2.OK, tc.want, tc.ok)
		}
	}
}

// Per-turn cost is the delta between consecutive result events. total_cost_usd
// and modelUsage are cumulative for the session; result.usage is per turn.
func TestPerTurnCostIsADelta(t *testing.T) {
	c := newCollector(t, false)
	c.push(
		`{"type":"result","subtype":"success","stop_reason":"end_turn","is_error":false,"total_cost_usd":1.0,"usage":{"input_tokens":1,"output_tokens":2}}`,
		`{"type":"result","subtype":"success","stop_reason":"end_turn","is_error":false,"total_cost_usd":1.25,"usage":{"input_tokens":1,"output_tokens":3}}`,
	)
	costs := []float64{}
	for _, e := range c.events {
		if tc, ok := e.(event.TurnCompleted); ok && tc.Usage != nil && tc.Usage.CostUSD != nil {
			costs = append(costs, *tc.Usage.CostUSD)
		}
	}
	if len(costs) != 2 || costs[0] != 1.0 || costs[1] != 0.25 {
		t.Errorf("per-turn costs = %v, want [1 0.25]", costs)
	}
	if r := c.n.lastResult(); r.TotalCostUSD != 1.25 {
		t.Errorf("cumulative total = %v", r.TotalCostUSD)
	}
}

// A turn's usage must not carry a context window, because result.usage sums the
// turn's requests and dividing that by the window is the eightfold
// overstatement ADAPTERS.md §2 warns about. The live figure lives on the
// session instead.
func TestTurnUsageCarriesNoContextWindow(t *testing.T) {
	c := newCollector(t, false)
	c.push(
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m1","usage":{"input_tokens":2,"cache_read_input_tokens":18502,"cache_creation_input_tokens":14993}}}}`,
		`{"type":"result","subtype":"success","stop_reason":"end_turn","is_error":false,"total_cost_usd":0.1,`+
			`"usage":{"input_tokens":4,"cache_read_input_tokens":51997,"cache_creation_input_tokens":15105,"output_tokens":101},`+
			`"modelUsage":{"claude-opus-5[1m]":{"contextWindow":1000000,"canonicalModel":"claude-opus-5"}}}`,
	)
	var done event.TurnCompleted
	for _, e := range c.events {
		if v, ok := e.(event.TurnCompleted); ok {
			done = v
		}
	}
	if done.Usage.ContextWindow != nil {
		t.Errorf("ContextWindow on a turn-summed usage would overstate pressure; got %d", *done.Usage.ContextWindow)
	}
	if p, ok := done.Usage.ContextPressure(); ok {
		t.Errorf("ContextPressure must be uncomputable from a turn total, got %v", p)
	}
	cs := c.n.contextState()
	if cs.Used != 33497 || cs.Window != 1000000 {
		t.Errorf("live context = %+v, want 33497/1000000", cs)
	}
	if p, ok := cs.Pressure(); !ok || p > 0.04 {
		t.Errorf("pressure = %v ok=%v", p, ok)
	}
	// The turn totals are still exact for metering.
	if *done.Usage.CachedInputTokens != 51997 || *done.Usage.OutputTokens != 101 {
		t.Errorf("turn usage = %+v", done.Usage)
	}
}

// A resumed session may replay history before it does anything live. Nothing in
// the vendored trace shows what --resume emits, so the adapter marks everything
// replay until its own first turn is acknowledged: a replayed TurnCompleted
// must not ping.
func TestReplayedEventsNeverPing(t *testing.T) {
	c := newCollector(t, true)
	c.n.queueTurn("mine", "the live turn")
	c.push(
		`{"type":"user","isReplay":true,"message":{"role":"user","content":[{"type":"text","text":"a turn from two weeks ago"}]}}`,
		`{"type":"result","subtype":"success","stop_reason":"end_turn","is_error":false}`,
	)
	for _, e := range c.events {
		if e.Ping() != event.PingNone {
			t.Errorf("%v pinged while replaying", e.Kind())
		}
		if !e.Envelope().Replay {
			t.Errorf("%v is not marked as a replay", e.Kind())
		}
	}
	if c.events[0].Envelope().Turn == "mine" {
		t.Error("a replayed history message must not consume the id of a turn we are still waiting on")
	}

	// Our own turn comes back and the replay window closes.
	c.push(`{"type":"user","isReplay":true,"message":{"role":"user","content":[{"type":"text","text":"the live turn"}]}}`)
	last := c.events[len(c.events)-1]
	if last.Envelope().Turn != "mine" || last.Envelope().Replay {
		t.Errorf("our own turn = %+v", last.Envelope())
	}
}

// A second echo while a turn is open is a steer landing, not a new turn. There
// is no event for it in ADAPTERS.md §5 and inventing one would be adding a
// tenth kind.
func TestSteerEchoDoesNotStartATurn(t *testing.T) {
	c := newCollector(t, false)
	c.n.queueTurn("t1", "first")
	c.push(
		`{"type":"user","isReplay":true,"message":{"role":"user","content":[{"type":"text","text":"first"}]}}`,
		`{"type":"user","isReplay":true,"message":{"role":"user","content":[{"type":"text","text":"also update the changelog"}]}}`,
	)
	kindsEqual(t, c.kinds(), event.KindTurnStarted)
	if c.n.currentTurn() != "t1" {
		t.Errorf("current turn = %q", c.n.currentTurn())
	}
}

func TestInitIsSeenEveryTurn(t *testing.T) {
	c := newCollector(t, false)
	line := `{"type":"system","subtype":"init","claude_code_version":"2.1.226","model":"claude-opus-5[1m]","permissionMode":"auto","cwd":"/work","tools":["Bash"],"mcp_servers":[{"name":"x","status":"connected"}],"capabilities":["interrupt_receipt_v1"]}`
	c.push(line, line)
	if len(c.inits) != 2 {
		t.Fatalf("init callbacks = %d, want one per turn", len(c.inits))
	}
	if len(c.events) != 0 {
		t.Errorf("init is state, not an event: %v", c.kinds())
	}
	if c.n.initInfo().PermissionMode != "auto" {
		t.Error("permissionMode must be readable from init; it is the whole check")
	}
}

func TestToolTargetIsAVerbatimFieldRead(t *testing.T) {
	for _, tc := range []struct {
		tool  string
		input map[string]any
		want  string
	}{
		{"Bash", map[string]any{"command": "go test ./..."}, "go test ./..."},
		{"Read", map[string]any{"file_path": "/a/b"}, "/a/b"},
		{"Grep", map[string]any{"pattern": "TODO"}, "TODO"},
		{"WebFetch", map[string]any{"url": "https://x"}, "https://x"},
		// An unknown tool gets nothing rather than a guess, and the orchestrator
		// says what it knows: the tool's name.
		{"SomeFutureTool", map[string]any{"whatever": "x"}, ""},
		{"Bash", nil, ""},
	} {
		if got := toolTarget(tc.tool, tc.input); got != tc.want {
			t.Errorf("toolTarget(%q, %v) = %q, want %q", tc.tool, tc.input, got, tc.want)
		}
	}
}

func TestContextPressureNeedsBothHalves(t *testing.T) {
	if _, ok := (ContextState{Used: 100}).Pressure(); ok {
		t.Error("a numerator with no denominator is not a pressure")
	}
	if _, ok := (ContextState{Window: 1000}).Pressure(); ok {
		t.Error("a denominator with no numerator is not a pressure")
	}
	p, ok := ContextState{Used: 700, Window: 1000}.Pressure()
	if !ok || p != 0.7 {
		t.Errorf("pressure = %v %v", p, ok)
	}
}

func TestClipKeepsSpeechShort(t *testing.T) {
	long := PermissionRequest{ToolName: "Bash", Input: map[string]any{"command": string(make([]byte, 400))}}
	if n := len([]rune(long.prompt())); n > 160 {
		t.Errorf("prompt is %d runes; ADAPTERS.md §6 budgets speech by the second", n)
	}
}

// parent_tool_use_id is null on all 49 events of the vendored trace, so
// sub-agent attribution is unobserved. A non-null one is counted rather than
// interpreted — the first trace that carries one shows up instead of vanishing.
func TestParentToolUseIDIsCountedNotClaimed(t *testing.T) {
	c := newCollector(t, false)
	c.push(`{"type":"stream_event","parent_tool_use_id":"toolu_task_1","event":{"type":"message_stop"}}`)
	if c.n.unseen()["parent_tool_use_id"] != 1 {
		t.Errorf("unseen = %v", c.n.unseen())
	}
	if len(c.events) != 0 {
		t.Errorf("nothing should be emitted for it: %v", c.kinds())
	}
}

func TestHookRunOKBeforeItFinishes(t *testing.T) {
	h := HookRun{Started: time.Now()}
	if !h.OK() {
		t.Error("a hook we are still waiting on is not a failure")
	}
}
