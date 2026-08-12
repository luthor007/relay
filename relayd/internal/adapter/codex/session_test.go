package codex

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/logx"
)

func startTurn(t *testing.T, f *fakeServer, s *Session, text, turnID string) string {
	t.Helper()
	type res struct {
		id  string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		id, err := s.Send(context.Background(), adapter.Turn{Text: text})
		ch <- res{id, err}
	}()
	start := f.expect(t, "turn/start")
	f.notify("turn/started", map[string]any{
		"threadId": testThread,
		"turn":     map[string]any{"id": turnID, "status": "inProgress", "items": []any{}},
	})
	f.reply(start.ID, map[string]any{})
	r := <-ch
	if r.err != nil {
		t.Fatalf("turn/start: %v", r.err)
	}
	return r.id
}

// TestTurnIDComesFromTheNotification. There is no ServerResponse.json, so
// nothing may be read out of a result — including the turn id.
func TestTurnIDComesFromTheNotification(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{})
	s := f.open(t, a, defaultSessionOptions(), testThread)

	id := startTurn(t, f, s, "hello", "turn-42")
	if id != "turn-42" {
		t.Fatalf("turn id = %q", id)
	}
	if s.ActiveTurn() != "turn-42" {
		t.Fatalf("active turn = %q", s.ActiveTurn())
	}
}

// TestSteerRequiresTheActiveTurn. expectedTurnId is a precondition, not a
// convenience: "the request fails when it does not match the currently active
// turn". Guessing it here would defeat the point.
func TestSteerRequiresTheActiveTurn(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{})
	s := f.open(t, a, defaultSessionOptions(), testThread)
	startTurn(t, f, s, "go", "turn-1")

	if err := s.Steer(context.Background(), "", adapter.Turn{Text: "x"}); !errors.Is(err, adapter.ErrTurnNotActive) {
		t.Errorf("steer with no turn id = %v, want ErrTurnNotActive", err)
	}
	if err := s.Steer(context.Background(), "turn-9", adapter.Turn{Text: "x"}); !errors.Is(err, adapter.ErrTurnNotActive) {
		t.Errorf("steer against a stale turn = %v, want ErrTurnNotActive", err)
	}

	go func() {
		m := f.expect(t, "turn/steer")
		f.reply(m.ID, map[string]any{})
	}()
	if err := s.Steer(context.Background(), "turn-1", adapter.Turn{Text: "actually, do the other thing"}); err != nil {
		t.Fatalf("steer against the active turn: %v", err)
	}
}

// TestSteerAfterTheTurnEndsIsNotActive. The classification is by observation —
// the turn stopped being active — rather than by matching the error's prose.
func TestSteerAfterTheTurnEndsIsNotActive(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{})
	s := f.open(t, a, defaultSessionOptions(), testThread)
	startTurn(t, f, s, "go", "turn-1")

	go func() {
		m := f.expect(t, "turn/steer")
		f.notify("turn/completed", map[string]any{
			"threadId": testThread,
			"turn":     map[string]any{"id": "turn-1", "status": "completed", "items": []any{}},
		})
		// Give the reader a moment to apply the completion before the failure
		// lands, which is the race a real Codex would produce.
		time.Sleep(20 * time.Millisecond)
		f.replyError(m.ID, -32602, "expected turn id did not match")
	}()

	err := s.Steer(context.Background(), "turn-1", adapter.Turn{Text: "too late"})
	if !errors.Is(err, adapter.ErrTurnNotActive) {
		t.Fatalf("steer = %v, want ErrTurnNotActive", err)
	}
}

// TestApprovalPolicyNeverIsRefusedUpFront. Both Claude Code and Codex have a
// setting that silently stops them asking; the failure presents as "the glasses
// never ask me anything", which reads as a feature.
func TestApprovalPolicyNeverIsRefusedUpFront(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{})

	opts := defaultSessionOptions()
	opts.PermissionMode = "never"
	if _, err := a.Start(context.Background(), opts); err == nil {
		t.Fatal(`approvalPolicy "never" must be refused, not accepted quietly`)
	}

	opts = defaultSessionOptions()
	opts.Extra = map[string]string{"approvalsReviewer": "auto_review"}
	if _, err := a.Start(context.Background(), opts); err == nil {
		t.Fatal("approvalsReviewer auto_review routes approvals to a subagent; it must be refused")
	}
}

// TestTheTrapIsDetectedWhenItIsSwitchedOnUnderUs. thread/settings/updated is the
// live check, and item/autoApprovalReview/* is the visible symptom.
func TestTheTrapIsDetectedWhenItIsSwitchedOnUnderUs(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{})
	s := f.open(t, a, defaultSessionOptions(), testThread)

	if !s.Capabilities().Has(adapter.CapNeedsInput) {
		t.Fatal("a freshly opened session should have a needs-input path")
	}

	f.notify("thread/settings/updated", map[string]any{
		"threadId": testThread,
		"threadSettings": map[string]any{
			"approvalPolicy":    "never",
			"approvalsReviewer": "user",
			"collaborationMode": map[string]any{"mode": "default", "settings": map[string]any{"model": "gpt-5-codex"}},
			"cwd":               "/w",
			"model":             "gpt-5-codex",
			"modelProvider":     "openai",
			"sandboxPolicy":     map[string]any{"type": "workspaceWrite"},
		},
	})

	waitFor(t, func() bool { return !s.Capabilities().Has(adapter.CapNeedsInput) }, "needs-input to be withdrawn")
	if note := s.Capabilities().Note(adapter.CapNeedsInput); !strings.Contains(note, "never") {
		t.Errorf("the note must say why: %q", note)
	}
	if err := s.Capabilities().Require(adapter.CapNeedsInput); !errors.Is(err, adapter.ErrUnsupported) {
		t.Errorf("Require = %v, want an *UnsupportedError", err)
	}
}

func TestAutoApprovalReviewWithdrawsNeedsInput(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{})
	s := f.open(t, a, defaultSessionOptions(), testThread)

	f.notify("item/autoApprovalReview/started", map[string]any{
		"threadId": testThread, "turnId": "turn-1", "reviewId": "r1",
		"startedAtMs": 1, "action": map[string]any{}, "review": map[string]any{},
	})
	waitFor(t, func() bool { return !s.Capabilities().Has(adapter.CapNeedsInput) },
		"the auto-review symptom to withdraw needs-input")
	if note := s.Capabilities().Note(adapter.CapNeedsInput); !strings.Contains(note, "subagent") {
		t.Errorf("note = %q", note)
	}
}

// TestBlockedStatusIsStateNotAQuestion. thread/status/changed says a session is
// stuck, and survives a reconnect — but the JSON-RPC request it refers to does
// not, so there is no reply path and it must not become a NeedsInput.
func TestBlockedStatusIsStateNotAQuestion(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{})
	s := f.open(t, a, defaultSessionOptions(), testThread)

	events := make(chan event.Event, 8)
	go func() {
		for e := range s.Events() {
			events <- e
		}
	}()

	f.notify("thread/status/changed", map[string]any{
		"threadId": testThread,
		"status":   map[string]any{"type": "active", "activeFlags": []any{"waitingOnApproval"}},
	})
	waitFor(t, func() bool { return len(s.Blocked()) == 1 }, "the blocked flag to be recorded")
	if s.Blocked()[0] != "waitingOnApproval" {
		t.Errorf("blocked = %v", s.Blocked())
	}
	select {
	case e := <-events:
		t.Fatalf("a status flag became an event nobody can answer: %s", describe(e))
	case <-time.After(150 * time.Millisecond):
	}

	f.notify("thread/status/changed", map[string]any{
		"threadId": testThread,
		"status":   map[string]any{"type": "idle"},
	})
	waitFor(t, func() bool { return len(s.Blocked()) == 0 }, "the blocked flag to clear")
}

// TestCompactIsExposed. MEMORY.md §9 drives idle compaction, and
// thread/compact/start takes {threadId} and nothing else — no threshold, no
// mode — so the policy is entirely ours.
func TestCompactIsExposed(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{})
	s := f.open(t, a, defaultSessionOptions(), testThread)

	var c Compactor = s
	go func() {
		m := f.expect(t, "thread/compact/start")
		var p threadIDParams
		if err := jsonUnmarshal(m.Params, &p); err != nil || p.ThreadID != testThread {
			t.Errorf("compact params = %s", m.Params)
		}
		f.reply(m.ID, map[string]any{})
	}()
	if err := c.Compact(context.Background()); err != nil {
		t.Fatalf("compact: %v", err)
	}

	// The live compaction signal is an item, not the deprecated
	// thread/compacted notification.
	f.notify("item/completed", map[string]any{
		"threadId": testThread, "turnId": "turn-1", "completedAtMs": 1,
		"item": map[string]any{"id": "ci", "type": "contextCompaction"},
	})
	waitFor(t, func() bool { return s.Compactions() == 1 }, "the compaction item to be counted")
}

// TestTurnCompletedMapsStopReasons, including the one MEMORY.md §9 singles out:
// contextWindowExceeded is a distinct code, so the adapter recognises it
// without matching prose, and it is retryable after a compaction.
func TestTurnCompletedMapsStopReasons(t *testing.T) {
	cases := []struct {
		status string
		code   string
		want   event.StopReason
		ok     bool
	}{
		{"completed", "", event.StopEndTurn, true},
		{"interrupted", "", event.StopCancelled, false},
		{"failed", "usageLimitExceeded", event.StopError, false},
		{"failed", "contextWindowExceeded", event.StopMaxTokens, false},
	}
	for _, c := range cases {
		var got []event.Event
		n := newNormalizer(func(e event.Event) { got = append(got, e) }, &metaSource{now: fixedClock()}, logx.Discard(), hooks{})
		turn := map[string]any{"id": "t1", "status": c.status, "items": []any{}, "durationMs": 1500}
		if c.code != "" {
			turn["error"] = map[string]any{"message": "boom", "codexErrorInfo": c.code}
		}
		n.handle("turn/completed", mustJSON(t, map[string]any{"threadId": "x", "turn": turn}))
		if len(got) != 1 {
			t.Fatalf("%s: %d events", c.status, len(got))
		}
		tc := got[0].(event.TurnCompleted)
		if tc.StopReason != c.want {
			t.Errorf("%s/%s: stop = %q, want %q", c.status, c.code, tc.StopReason, c.want)
		}
		if tc.OK != c.ok {
			t.Errorf("%s/%s: ok = %v", c.status, c.code, tc.OK)
		}
		if tc.Duration != 1500*time.Millisecond {
			t.Errorf("duration = %v", tc.Duration)
		}
		if tc.Usage != nil {
			t.Error("no tokenUsage arrived, so Usage must be nil rather than a zeroed struct")
		}
	}
}

// TestRetryableErrorDoesNotPing. willRetry decides: a retryable error is not a
// user-facing failure and must not wake anyone.
func TestRetryableErrorDoesNotPing(t *testing.T) {
	for _, retry := range []bool{true, false} {
		var got []event.Event
		n := newNormalizer(func(e event.Event) { got = append(got, e) }, &metaSource{now: fixedClock()}, logx.Discard(), hooks{})
		n.handle("error", mustJSON(t, map[string]any{
			"threadId": "x", "turnId": "t1", "willRetry": retry,
			"error": map[string]any{"message": "upstream hiccup", "codexErrorInfo": "serverOverloaded"},
		}))
		e := got[0].(event.Error)
		if e.Code != "serverOverloaded" {
			t.Errorf("code = %q", e.Code)
		}
		want := event.PingInformational
		if retry {
			want = event.PingNone
		}
		if e.Ping() != want {
			t.Errorf("willRetry=%v pings %s, want %s", retry, e.Ping(), want)
		}
	}
}

// TestCompletedItemDoesNotSpeakTheAnswerTwice. item/completed is authoritative
// over the deltas, but only one of the two may reach the TTS layer.
func TestCompletedItemDoesNotSpeakTheAnswerTwice(t *testing.T) {
	var got []event.Event
	n := newNormalizer(func(e event.Event) { got = append(got, e) }, &metaSource{now: fixedClock()}, logx.Discard(), hooks{})
	n.handle("item/agentMessage/delta", mustJSON(t, map[string]any{
		"threadId": "x", "turnId": "t1", "itemId": "m1", "delta": "hello",
	}))
	n.handle("item/completed", mustJSON(t, map[string]any{
		"threadId": "x", "turnId": "t1", "completedAtMs": 1,
		"item": map[string]any{"id": "m1", "type": "agentMessage", "text": "hello"},
	}))
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1: %s", len(got), describeAll(got))
	}

	// A runtime that never streamed still has to produce its text once.
	got = nil
	n2 := newNormalizer(func(e event.Event) { got = append(got, e) }, &metaSource{now: fixedClock()}, logx.Discard(), hooks{})
	n2.handle("item/completed", mustJSON(t, map[string]any{
		"threadId": "x", "turnId": "t1", "completedAtMs": 1,
		"item": map[string]any{"id": "m2", "type": "agentMessage", "text": "hello"},
	}))
	if len(got) != 1 || describe(got[0]) != `text "hello"` {
		t.Fatalf("unstreamed text: %s", describeAll(got))
	}
}

// TestReasoningKeepsTheSummaryDistinction. ADAPTERS.md §3: raw thinking is
// never spoken and the summary is speakable, and flattening the two loses the
// only thing that makes Codex's reasoning stream usable.
func TestReasoningKeepsTheSummaryDistinction(t *testing.T) {
	var got []event.Reasoning
	n := newNormalizer(func(e event.Event) {
		if r, ok := e.(event.Reasoning); ok {
			got = append(got, r)
		}
	}, &metaSource{now: fixedClock()}, logx.Discard(), hooks{})

	n.handle("item/reasoning/textDelta", mustJSON(t, map[string]any{
		"threadId": "x", "turnId": "t1", "itemId": "r1", "contentIndex": 0, "delta": "raw",
	}))
	n.handle("item/reasoning/summaryTextDelta", mustJSON(t, map[string]any{
		"threadId": "x", "turnId": "t1", "itemId": "r1", "summaryIndex": 0, "delta": "summary",
	}))
	if len(got) != 2 {
		t.Fatalf("got %d reasoning events", len(got))
	}
	if got[0].Summary {
		t.Error("item/reasoning/textDelta is raw thinking, never a summary")
	}
	if !got[1].Summary {
		t.Error("item/reasoning/summaryTextDelta is the speakable one")
	}
	for _, r := range got {
		if r.Ping() != event.PingNone {
			t.Error("reasoning never pings")
		}
	}
}

// TestSummaryPartBoundaryIsNotSwallowed. Two summary parts are two thoughts;
// concatenating them makes one wrong sentence.
func TestSummaryPartBoundaryIsNotSwallowed(t *testing.T) {
	var got []event.Event
	n := newNormalizer(func(e event.Event) { got = append(got, e) }, &metaSource{now: fixedClock()}, logx.Discard(), hooks{})
	part := func(i int) {
		n.handle("item/reasoning/summaryPartAdded", mustJSON(t, map[string]any{
			"threadId": "x", "turnId": "t1", "itemId": "r1", "summaryIndex": i,
		}))
	}
	delta := func(i int, s string) {
		n.handle("item/reasoning/summaryTextDelta", mustJSON(t, map[string]any{
			"threadId": "x", "turnId": "t1", "itemId": "r1", "summaryIndex": i, "delta": s,
		}))
	}
	part(0)
	delta(0, "first thought")
	part(1)
	delta(1, "second thought")

	want := []string{
		`reasoning_summary "first thought"`,
		`reasoning_summary "\n\n"`,
		`reasoning_summary "second thought"`,
	}
	if len(got) != len(want) {
		t.Fatalf("got %s, want %v", describeAll(got), want)
	}
	for i := range want {
		if describe(got[i]) != want[i] {
			t.Errorf("%d: got %s, want %s", i, describe(got[i]), want[i])
		}
	}
}

// TestNonTextBlocksAreRefusedRatherThanDropped. A glasses photo on a runtime
// whose prompt capabilities nobody probed is refused at the boundary.
func TestNonTextBlocksAreRefusedRatherThanDropped(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{})
	s := f.open(t, a, defaultSessionOptions(), testThread)

	_, err := s.Send(context.Background(), adapter.Turn{
		Text:   "what is this",
		Blocks: []adapter.Block{{Kind: adapter.BlockImage, MIMEType: "image/png", Data: []byte{1}}},
	})
	if !errors.Is(err, adapter.ErrUnsupported) {
		t.Fatalf("an image prompt = %v, want ErrUnsupported", err)
	}
	if s.Capabilities().Get(adapter.CapPromptImage) != adapter.SupportUnknown {
		t.Error("UserInput has image variants but nobody has probed them; Unknown is the honest level")
	}
}

// TestThreadGoneEndsTheSession, fatally and once.
func TestThreadGoneEndsTheSession(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{})
	s := f.open(t, a, defaultSessionOptions(), testThread)

	var got []event.Event
	done := make(chan struct{})
	go func() {
		defer close(done)
		for e := range s.Events() {
			got = append(got, e)
		}
	}()

	f.notify("thread/deleted", map[string]any{"threadId": testThread})
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the event channel must close exactly once, when the session ends")
	}
	if len(got) != 1 {
		t.Fatalf("got %s", describeAll(got))
	}
	e := got[0].(event.Error)
	if !e.Fatal {
		t.Error("a deleted thread is not a turn-level failure; the session is gone")
	}
	if _, err := s.Send(context.Background(), adapter.Turn{Text: "hi"}); !errors.Is(err, adapter.ErrSessionClosed) {
		t.Errorf("send after the session died = %v", err)
	}
}

// TestSubAgentThreadIsNotMistakenForOurs. A thread with a parentThreadId is a
// sub-agent, and binding it to the open we have in flight would hand the caller
// the wrong conversation.
func TestSubAgentThreadIsNotMistakenForOurs(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{})

	type res struct {
		s   adapter.Session
		err error
	}
	ch := make(chan res, 1)
	go func() {
		s, err := a.Start(context.Background(), defaultSessionOptions())
		ch <- res{s, err}
	}()
	start := f.expect(t, "thread/start")

	sub := minimalThread("thread-sub")
	sub["parentThreadId"] = "thread-1"
	f.notify("thread/started", map[string]any{"thread": sub})
	f.notify("thread/started", map[string]any{"thread": minimalThread(testThread)})
	f.reply(start.ID, map[string]any{})

	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("start: %v", r.err)
		}
		if r.s.Native() != testThread {
			t.Fatalf("bound to %q, want %q", r.s.Native(), testThread)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("start never completed")
	}
}

// TestMCPServersRideInTheConfigObject. There is no mcpServers field on any
// thread method; MEMORY.md §7's shared registry has to go through `config`.
func TestMCPServersRideInTheConfigObject(t *testing.T) {
	cfg := mcpConfig([]adapter.MCPServer{
		{Name: "linear", Command: "linear-mcp", Args: []string{"--stdio"}, Env: []string{"TOKEN=abc"}},
		{Name: "docs", URL: "https://example.invalid/mcp"},
	}, logx.Discard())

	servers, ok := cfg["mcp_servers"].(map[string]any)
	if !ok {
		t.Fatalf("config = %v", cfg)
	}
	if len(servers) != 2 {
		t.Fatalf("got %d servers", len(servers))
	}
	linear := servers["linear"].(map[string]any)
	if linear["command"] != "linear-mcp" {
		t.Errorf("command = %v", linear["command"])
	}
	if env := linear["env"].(map[string]any); env["TOKEN"] != "abc" {
		t.Errorf("env = %v", env)
	}
	if mcpConfig(nil, logx.Discard()) != nil {
		t.Error("no servers means no config override at all")
	}
}

func TestCapabilitiesSayWhatCodexCannotDo(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{})
	c := a.Capabilities()

	if c.Has(adapter.CapCostUSD) {
		t.Error("there is no dollar figure anywhere in the Codex contract")
	}
	if !c.Has(adapter.CapSteer) {
		t.Error("Codex is one of only two runtimes that can steer; saying otherwise wastes it")
	}
	if !c.Has(adapter.CapPlan) {
		t.Error("turn/plan/updated is native")
	}
	if !c.Has(adapter.CapTokens) || !c.Has(adapter.CapContextWindow) {
		t.Error("thread/tokenUsage/updated carries both")
	}
	if note := c.Note(adapter.CapCostUSD); !strings.Contains(note, "price table") {
		t.Errorf("the note should say where USD has to come from instead: %q", note)
	}
	if c.Protocol() != adapter.ProtocolAppServer {
		t.Errorf("protocol = %q", c.Protocol())
	}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
