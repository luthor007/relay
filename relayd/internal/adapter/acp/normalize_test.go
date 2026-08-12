package acp

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/event"
)

func TestSuffixOfIsArithmeticNotInference(t *testing.T) {
	for _, tc := range []struct{ prev, next, want string }{
		{"", "hello", "hello"},
		{"hello", "hello world", " world"},
		{"hello", "goodbye", "goodbye"}, // replaced, not appended
		{"hello", "hello", ""},
	} {
		if got := suffixOf(tc.prev, tc.next); got != tc.want {
			t.Errorf("suffixOf(%q,%q) = %q, want %q", tc.prev, tc.next, got, tc.want)
		}
	}
}

func TestRenderToolContent(t *testing.T) {
	old := "before"
	got := renderToolContent([]toolCallContent{
		{Type: "content", Content: &contentBlock{Type: "text", Text: "line one"}},
		{Type: "content", Content: &contentBlock{Type: "image", Data: "AQID", MIMEType: "image/png"}},
		{Type: "diff", Path: "src/a.ts", NewText: "abcd", OldText: &old},
		{Type: "terminal", TerminalID: "term_1"},
	})
	want := "line one\ndiff src/a.ts (+4 bytes)\nterminal term_1"
	if got != want {
		t.Errorf("renderToolContent =\n%q\nwant\n%q", got, want)
	}
}

// updateSession spins up a session and feeds it raw session/update payloads.
func updateSession(t *testing.T, opts func(*Options)) (*fakeAgent, *Session, *collector) {
	t.Helper()
	f := newFakeAgent(t)
	o := testOptions(t, adapter.OpenCode)
	if opts != nil {
		opts(&o)
	}
	a := dial(t, f, o, AgentCapabilities{})
	s := startSession(t, f, a, "sess_1")
	return f, s, collect(s)
}

func TestToolCallUpdatesMergeOntoTheToolCall(t *testing.T) {
	f, s, c := updateSession(t, nil)

	f.update("sess_1", map[string]any{
		"sessionUpdate": updateToolCall,
		"toolCallId":    "call_1",
		"title":         "Run npm test",
		"kind":          "execute",
		"status":        "pending",
		"rawInput":      map[string]any{"command": "npm test"},
	})
	// Only a toolCallId and a status: everything else must come from the
	// tool_call we already have.
	f.update("sess_1", map[string]any{
		"sessionUpdate": updateToolCallUpdate,
		"toolCallId":    "call_1",
		"status":        "in_progress",
	})
	f.update("sess_1", map[string]any{
		"sessionUpdate": updateToolCallUpdate,
		"toolCallId":    "call_1",
		"content":       []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "ok so far"}}},
	})
	// content replaces the collection, so the adapter must emit the new part
	// rather than the whole thing again.
	f.update("sess_1", map[string]any{
		"sessionUpdate": updateToolCallUpdate,
		"toolCallId":    "call_1",
		"status":        "completed",
		"content":       []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "ok so far and done"}}},
	})

	c.waitFor(t, "the completed status", func(e event.Event) bool {
		o, ok := e.(event.ToolOutput)
		return ok && o.Status == event.ToolCompleted
	})

	var started []event.ToolStarted
	var outs []event.ToolOutput
	for _, e := range c.all() {
		switch v := e.(type) {
		case event.ToolStarted:
			started = append(started, v)
		case event.ToolOutput:
			outs = append(outs, v)
		}
	}
	if len(started) != 1 || started[0].Tool != "execute" || started[0].Target != "Run npm test" {
		t.Fatalf("ToolStarted = %+v", started)
	}
	if len(outs) != 3 {
		t.Fatalf("got %d ToolOutput, want 3: %+v", len(outs), outs)
	}
	if outs[0].Status != event.ToolInProgress || outs[0].Chunk != "" {
		t.Errorf("status-only update = %+v", outs[0])
	}
	if outs[1].Chunk != "ok so far" {
		t.Errorf("first content chunk = %q", outs[1].Chunk)
	}
	if outs[2].Chunk != " and done" || outs[2].Status != event.ToolCompleted {
		t.Errorf("second content chunk = %+v, want only the new part", outs[2])
	}
	_ = s
}

func TestPlanIsNativeNotSynthesized(t *testing.T) {
	f, _, c := updateSession(t, nil)
	f.update("sess_1", map[string]any{
		"sessionUpdate": updatePlan,
		"entries": []any{
			map[string]any{"content": "first", "priority": "high", "status": "in_progress"},
			map[string]any{"content": "second", "priority": "low", "status": "pending"},
		},
	})
	e := c.waitFor(t, "a plan", func(e event.Event) bool {
		_, ok := e.(event.PlanUpdated)
		return ok
	}).(event.PlanUpdated)
	if e.Synthesized {
		t.Error("ACP plans are stated by the agent, not inferred by us")
	}
	if len(e.Steps) != 2 || e.Steps[0].Status != event.PlanInProgress || e.Steps[1].Status != event.PlanPending {
		t.Errorf("steps = %+v", e.Steps)
	}
	if e.Ping() != event.PingNone {
		t.Error("PlanUpdated is narration material, not a ping")
	}
}

func TestUnknownUpdateVariantIsCountedNotGuessed(t *testing.T) {
	f, s, _ := updateSession(t, nil)
	f.update("sess_1", map[string]any{"sessionUpdate": "some_ninth_variant", "whatever": 1})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.UnknownUpdates()["some_ninth_variant"] == 1 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("UnknownUpdates() = %v", s.UnknownUpdates())
}

func TestNonTextAgentContentIsDroppedAndCounted(t *testing.T) {
	f, s, c := updateSession(t, nil)
	f.update("sess_1", map[string]any{
		"sessionUpdate": updateAgentMessageChunk,
		"content":       map[string]any{"type": "image", "data": "AQID", "mimeType": "image/png"},
	})
	f.update("sess_1", map[string]any{
		"sessionUpdate": updateAgentMessageChunk,
		"content":       map[string]any{"type": "text", "text": "spoken"},
	})
	c.waitFor(t, "the text delta", func(e event.Event) bool {
		td, ok := e.(event.TextDelta)
		return ok && td.Text == "spoken"
	})
	if s.DroppedContent() != 1 {
		t.Errorf("DroppedContent() = %d, want 1 — an image has no TextDelta to become", s.DroppedContent())
	}
	for _, e := range c.all() {
		if td, ok := e.(event.TextDelta); ok && td.Text == "" {
			t.Error("an empty TextDelta is noise dressed as information")
		}
	}
}

func TestUserMessageChunkGoesToTheHookNotTheStream(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	f, _, c := updateSession(t, func(o *Options) {
		o.OnUserMessage = func(_ string, text string, _ bool) {
			mu.Lock()
			seen = append(seen, text)
			mu.Unlock()
		}
	})
	f.update("sess_1", map[string]any{
		"sessionUpdate": updateUserMessageChunk,
		"content":       map[string]any{"type": "text", "text": "what I said"},
	})
	f.update("sess_1", map[string]any{
		"sessionUpdate": updateAgentMessageChunk,
		"content":       map[string]any{"type": "text", "text": "what it said"},
	})
	c.waitFor(t, "the agent text", func(e event.Event) bool {
		td, ok := e.(event.TextDelta)
		return ok && td.Text == "what it said"
	})
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "what I said" {
		t.Errorf("hook saw %v", seen)
	}
	if txt := c.text(); strings.Contains(txt, "what I said") {
		t.Error("a user_message_chunk must not enter the assistant text stream")
	}
}

func TestCommandsAndModeAreSurfaced(t *testing.T) {
	var mu sync.Mutex
	var cmds []AvailableCommand
	var mode string
	f, s, _ := updateSession(t, func(o *Options) {
		o.OnCommands = func(_ string, c []AvailableCommand) { mu.Lock(); cmds = c; mu.Unlock() }
		o.OnModeChange = func(_ string, m string) { mu.Lock(); mode = m; mu.Unlock() }
	})
	f.update("sess_1", map[string]any{
		"sessionUpdate":     updateAvailableCommands,
		"availableCommands": []any{map[string]any{"name": "create_plan", "description": "plan first"}},
	})
	f.update("sess_1", map[string]any{"sessionUpdate": updateCurrentMode, "currentModeId": "code"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		okCmds, okMode := len(cmds) == 1, mode == "code"
		mu.Unlock()
		if okCmds && okMode && s.CurrentMode() == "code" && len(s.Commands()) == 1 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("commands=%v mode=%q session mode=%q", cmds, mode, s.CurrentMode())
}

func TestReasoningIsObservedBeforeItIsClaimed(t *testing.T) {
	f, s, c := updateSession(t, nil)
	if s.Capabilities().Get(adapter.CapReasoning) != adapter.SupportUnknown {
		t.Fatal("CapReasoning starts unknown: agent_thought_chunk is protocol-native but nobody has probed the three runtimes")
	}
	f.update("sess_1", map[string]any{
		"sessionUpdate": updateAgentThoughtChunk,
		"content":       map[string]any{"type": "text", "text": "hmm"},
	})
	c.waitFor(t, "reasoning", func(e event.Event) bool {
		_, ok := e.(event.Reasoning)
		return ok
	})
	if s.Capabilities().Get(adapter.CapReasoning) != adapter.SupportYes {
		t.Error("one observed thought chunk is enough to stop calling it unknown")
	}
}

// TestUpdatesBeforeRegistrationAreNotLost: an agent may push session/update the
// instant it answers session/new, which is before our own bookkeeping is done.
func TestUpdatesBeforeRegistrationAreNotLost(t *testing.T) {
	f := newFakeAgent(t)
	a := dial(t, f, testOptions(t, adapter.OpenCode), AgentCapabilities{})

	type res struct {
		s   adapter.Session
		err error
	}
	done := make(chan res, 1)
	go func() {
		s, err := a.Start(context.Background(), adapter.SessionOptions{Workspace: "/tmp/ws"})
		done <- res{s, err}
	}()
	m := f.next()

	// The update goes out on the same breath as the response, and ahead of it
	// on the wire.
	f.update("sess_1", map[string]any{
		"sessionUpdate":     updateAvailableCommands,
		"availableCommands": []any{map[string]any{"name": "create_plan", "description": "plan first"}},
	})
	f.respond(m.ID, newSessionResult{SessionID: "sess_1"})

	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	s := r.s.(*Session)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(s.Commands()) == 1 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the first available_commands_update of the session was lost to a race with registration")
}

func TestMalformedLinesDoNotKillTheSession(t *testing.T) {
	f, s, c := updateSession(t, nil)
	f.writeRaw([]byte("this is not json at all"))
	f.writeRaw([]byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_1","update":{}}}`))
	f.update("sess_1", map[string]any{
		"sessionUpdate": updateAgentMessageChunk,
		"content":       map[string]any{"type": "text", "text": "still here"},
	})
	c.waitFor(t, "text after the bad lines", func(e event.Event) bool {
		td, ok := e.(event.TextDelta)
		return ok && td.Text == "still here"
	})
	if s.UnknownUpdates()[""] != 1 {
		t.Errorf("an update with no discriminant should be counted, got %v", s.UnknownUpdates())
	}
}

func TestSequenceNumbersAreMonotonic(t *testing.T) {
	f, _, c := updateSession(t, nil)
	for i := 0; i < 5; i++ {
		f.update("sess_1", map[string]any{
			"sessionUpdate": updateAgentMessageChunk,
			"content":       map[string]any{"type": "text", "text": "x"},
		})
	}
	c.waitFor(t, "five deltas", func(event.Event) bool { return len(c.text()) >= 5 })
	var last uint64
	for _, e := range c.all() {
		if s := e.Envelope().Seq; s <= last {
			t.Fatalf("sequence went %d then %d", last, s)
		} else {
			last = s
		}
	}
}

func TestRawInputThatIsNotAnObjectIsReportedAbsent(t *testing.T) {
	if got := rawObject(json.RawMessage(`"just a string"`)); got != nil {
		t.Errorf("rawObject on a non-object = %v, want nil", got)
	}
	if got := rawObject(json.RawMessage(`{"a":1}`)); got["a"] != float64(1) {
		t.Errorf("rawObject = %v", got)
	}
}
