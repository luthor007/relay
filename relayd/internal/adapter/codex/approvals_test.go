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

const testThread = "thread-1"

func approvalParams(turn, item string) map[string]any {
	return map[string]any{
		"threadId": testThread, "turnId": turn, "itemId": item,
		"startedAtMs": 1786000000000,
		"command":     "rm -rf build",
		"cwd":         "/w",
		"approvalId":  nil,
	}
}

// TestEveryServerRequestIsAnswered is the invariant that keeps Codex running:
// all ten block until the client replies, and an adapter that handles only the
// approval subset hangs the runtime the first time one of the others arrives.
func TestEveryServerRequestIsAnswered(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{
		UnverifiedReplies: map[string]ReplyEncoder{
			MethodFileChangeApproval: func(json.RawMessage, event.Reply) (any, error) { return "accept", nil },
			MethodToolUserInput:      func(json.RawMessage, event.Reply) (any, error) { return map[string]any{}, nil },
			MethodElicitation:        func(json.RawMessage, event.Reply) (any, error) { return map[string]any{}, nil },
		},
	})
	s := f.open(t, a, defaultSessionOptions(), testThread)

	answers := make(chan struct{}, 8)
	go func() {
		for e := range s.Events() {
			if q, ok := e.(*event.NeedsInput); ok {
				_ = q.Reply(context.Background(), event.Reply{Decision: event.DecisionAllow})
				answers <- struct{}{}
			}
		}
	}()

	blocking := []struct {
		method string
		params map[string]any
	}{
		{MethodCommandApproval, approvalParams("turn-1", "item-1")},
		{MethodFileChangeApproval, map[string]any{
			"threadId": testThread, "turnId": "turn-1", "itemId": "item-2", "startedAtMs": 1,
		}},
		{MethodPermissionsApproval, map[string]any{
			"threadId": testThread, "turnId": "turn-1", "itemId": "item-3", "startedAtMs": 1,
			"cwd":         "/w",
			"permissions": map[string]any{"network": map[string]any{"enabled": true}},
		}},
		{MethodToolUserInput, map[string]any{
			"threadId": testThread, "turnId": "turn-1", "itemId": "item-4",
			"questions": []any{map[string]any{"id": "q1", "header": "Token", "question": "paste the API key", "isSecret": true}},
		}},
		{MethodElicitation, map[string]any{
			"threadId": testThread, "serverName": "linear", "turnId": nil,
			"mode": "form", "message": "which team?", "requestedSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		}},
	}
	refused := []struct {
		method string
		params map[string]any
	}{
		{MethodDynamicToolCall, map[string]any{"threadId": testThread, "turnId": "turn-1", "callId": "c1", "tool": "whatever", "arguments": map[string]any{}}},
		{MethodAuthRefresh, map[string]any{"reason": "unauthorized"}},
		{MethodAttestation, map[string]any{}},
		{MethodApplyPatchLegacy, map[string]any{"callId": "c2", "conversationId": testThread, "fileChanges": map[string]any{}}},
		{MethodExecCommandLegacy, map[string]any{"callId": "c3", "conversationId": testThread, "command": []any{"ls"}, "cwd": "/w", "parsedCmd": []any{}}},
	}

	for i, c := range blocking {
		id := "blk-" + itoa(int64(i))
		f.request(id, c.method, c.params)
		reply := f.next(t)
		if requestKey(reply.ID) != requestKey(mustJSON(t, id)) {
			t.Fatalf("%s: replied to %s, not %s", c.method, reply.ID, id)
		}
		if reply.Error != nil {
			t.Errorf("%s: answered with an error: %v", c.method, reply.Error)
		}
	}
	for i, c := range refused {
		id := "ref-" + itoa(int64(i))
		f.request(id, c.method, c.params)
		reply := f.next(t)
		if requestKey(reply.ID) != requestKey(mustJSON(t, id)) {
			t.Fatalf("%s: replied to %s, not %s", c.method, reply.ID, id)
		}
		if reply.Error == nil {
			t.Errorf("%s: Relay wants nothing to do with this; it must be refused, not answered", c.method)
			continue
		}
		if reply.Error.Code != codeMethodNotFound {
			t.Errorf("%s: refused with %d, want %d", c.method, reply.Error.Code, codeMethodNotFound)
		}
	}

	for i := 0; i < len(blocking); i++ {
		select {
		case <-answers:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d blocking requests became a question", i, len(blocking))
		}
	}
}

// TestUnverifiedRepliesAreRefusedRatherThanGuessed. ADAPTERS.md §8 item 7:
// three of the five approval reply shapes are outside the vendored contract, so
// until they are probed they are answered with an error — visibly — instead of
// with a payload nobody has evidence for.
func TestUnverifiedRepliesAreRefusedRatherThanGuessed(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{}) // no encoders registered
	s := f.open(t, a, defaultSessionOptions(), testThread)

	seen := make(chan event.Event, 8)
	go func() {
		for e := range s.Events() {
			seen <- e
		}
	}()

	for i, method := range UnverifiedReplyMethods() {
		id := "u-" + itoa(int64(i))
		params := map[string]any{"threadId": testThread, "turnId": "turn-1", "itemId": "i", "startedAtMs": 1}
		if method == MethodToolUserInput {
			params["questions"] = []any{}
		}
		if method == MethodElicitation {
			params = map[string]any{"threadId": testThread, "serverName": "x", "mode": "form", "message": "m"}
		}
		f.request(id, method, params)
		reply := f.next(t)
		if reply.Error == nil {
			t.Fatalf("%s: answered with a guessed payload", method)
		}
		if reply.Error.Code != codeUnverified {
			t.Errorf("%s: code = %d, want %d", method, reply.Error.Code, codeUnverified)
		}
		if !strings.Contains(reply.Error.Message, "ADAPTERS.md") {
			t.Errorf("%s: the refusal should say where the open question is tracked, got %q", method, reply.Error.Message)
		}
	}

	// No question may be raised for a request we cannot answer: a NeedsInput
	// whose reply cannot be delivered is a hung session.
	select {
	case e := <-seen:
		t.Fatalf("a question was raised that nobody could answer: %s", describe(e))
	case <-time.After(150 * time.Millisecond):
	}

	// And the capability descriptor has to say so, or the console shows a
	// needs-input path that is only three-fifths there.
	waitFor(t, func() bool {
		return strings.Contains(s.Capabilities().Note(adapter.CapNeedsInput), "no verified reply shape")
	}, "the capability note to record the partial coverage")
}

// TestNoStandingGrantIsEverOffered. ORCHESTRATOR.md §4b requires consequential
// actions to be confirmed every time, and the schema offers three ways to say
// "and stop asking".
func TestNoStandingGrantIsEverOffered(t *testing.T) {
	for _, o := range commandDecisionOptions() {
		if o.Kind.Standing() {
			t.Errorf("option %q grants something beyond the action in front of us", o.ID)
		}
		switch o.ID {
		case "accept", "decline", "cancel":
		default:
			t.Errorf("option %q is not one of the three non-persisting decisions", o.ID)
		}
	}
	for _, r := range []event.Reply{
		{Decision: event.DecisionAllow},
		{Decision: event.DecisionDeny},
		{Decision: event.DecisionCancelled},
		{Decision: event.DecisionDeny, Interrupt: true},
		{OptionID: "accept"},
	} {
		got := commandDecision(r)
		switch got {
		case "accept", "decline", "cancel":
		default:
			t.Errorf("reply %+v produced %q, which persists a grant", r, got)
		}
	}
}

// TestDeclineVersusCancel is the hard stop. The schema's own words: decline
// means "the agent will continue the turn", cancel means "the turn will also be
// immediately interrupted".
func TestDeclineVersusCancel(t *testing.T) {
	if got := commandDecision(event.Reply{Decision: event.DecisionDeny}); got != "decline" {
		t.Errorf("a plain no = %q, want decline", got)
	}
	if got := commandDecision(event.Reply{Decision: event.DecisionDeny, Interrupt: true}); got != "cancel" {
		t.Errorf(`"no, stop" = %q, want cancel`, got)
	}
	if got := commandDecision(event.Reply{Decision: event.DecisionCancelled}); got != "cancel" {
		t.Errorf("a cancelled turn = %q, want cancel", got)
	}
}

// TestApprovalDecisionReachesTheWire end-to-end: the reply the orchestrator
// forms resolves that exact JSON-RPC request.
func TestApprovalDecisionReachesTheWire(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{})
	s := f.open(t, a, defaultSessionOptions(), testThread)

	questions := make(chan *event.NeedsInput, 4)
	go func() {
		for e := range s.Events() {
			if q, ok := e.(*event.NeedsInput); ok {
				questions <- q
			}
		}
	}()

	f.request("srv-9", MethodCommandApproval, approvalParams("turn-7", "item-9"))
	q := <-questions

	if q.Ping() != event.PingBlocking {
		t.Errorf("a blocked session must ping blocking, got %s", q.Ping())
	}
	if q.Envelope().Turn != "turn-7" {
		t.Errorf("question is attributed to turn %q", q.Envelope().Turn)
	}
	if q.Tool == nil || q.Tool.ID != "item-9" {
		t.Errorf("the question does not carry the item it is about: %+v", q.Tool)
	}

	if err := q.Reply(context.Background(), event.Reply{OptionID: "cancel"}); err != nil {
		t.Fatalf("reply: %v", err)
	}
	reply := f.next(t)
	var got string
	if err := json.Unmarshal(reply.Result, &got); err != nil {
		t.Fatalf("the result is not a bare decision: %s", reply.Result)
	}
	if got != "cancel" {
		t.Errorf("result = %q, want cancel", got)
	}

	// Single-shot: the same question cannot be answered twice.
	if err := q.Reply(context.Background(), event.Reply{OptionID: "accept"}); !errors.Is(err, event.ErrAnswered) {
		t.Errorf("second reply = %v, want ErrAnswered", err)
	}
}

// TestUnofferedOptionsNeverReachCodex. ADAPTERS.md §4: options are
// agent-supplied and open-ended, and we never send back one that was not in the
// array.
func TestUnofferedOptionsNeverReachCodex(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{})
	s := f.open(t, a, defaultSessionOptions(), testThread)

	questions := make(chan *event.NeedsInput, 4)
	go func() {
		for e := range s.Events() {
			if q, ok := e.(*event.NeedsInput); ok {
				questions <- q
			}
		}
	}()

	f.request("srv-o", MethodCommandApproval, approvalParams("turn-1", "item-1"))
	q := <-questions

	err := q.Reply(context.Background(), event.Reply{OptionID: "acceptForSession"})
	if !errors.Is(err, event.ErrUnknownOption) {
		t.Fatalf("reply with a standing grant = %v, want ErrUnknownOption", err)
	}
	select {
	case m := <-f.inbox:
		t.Fatalf("a rejected option still reached the wire: %s", m.Result)
	case <-time.After(100 * time.Millisecond):
	}
	// The question is still open, so a second, valid answer works.
	if err := q.Reply(context.Background(), event.Reply{OptionID: "decline"}); err != nil {
		t.Fatalf("the question should still be answerable: %v", err)
	}
	f.next(t)
}

// TestServerRequestResolvedWithdrawsThePing. An approval answered in a terminal
// must retract Relay's question, or the user is woken to approve something that
// is already approved.
func TestServerRequestResolvedWithdrawsThePing(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{})
	s := f.open(t, a, defaultSessionOptions(), testThread)

	questions := make(chan *event.NeedsInput, 4)
	go func() {
		for e := range s.Events() {
			if q, ok := e.(*event.NeedsInput); ok {
				questions <- q
			}
		}
	}()

	f.request("srv-x", MethodCommandApproval, approvalParams("turn-1", "item-1"))
	q := <-questions

	f.notify("serverRequest/resolved", map[string]any{"threadId": testThread, "requestId": "srv-x"})

	select {
	case <-q.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the question was never withdrawn")
	}
	if err := q.Reply(context.Background(), event.Reply{OptionID: "accept"}); !errors.Is(err, event.ErrWithdrawn) {
		t.Errorf("replying to a withdrawn question = %v, want ErrWithdrawn", err)
	}
	if _, ok := q.Outcome(); ok {
		t.Error("a withdrawn question has no outcome")
	}
}

// TestSecretAnswersAreMarked. MEMORY.md §6: an isSecret answer goes through the
// credential vault and never into the index, and the adapter is the only place
// that knows.
func TestSecretAnswersAreMarked(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{UnverifiedReplies: map[string]ReplyEncoder{
		MethodToolUserInput: func(json.RawMessage, event.Reply) (any, error) { return map[string]any{}, nil },
	}})
	s := f.open(t, a, defaultSessionOptions(), testThread)

	questions := make(chan *event.NeedsInput, 4)
	go func() {
		for e := range s.Events() {
			if q, ok := e.(*event.NeedsInput); ok {
				questions <- q
			}
		}
	}()

	f.request("srv-s", MethodToolUserInput, map[string]any{
		"threadId": testThread, "turnId": "turn-1", "itemId": "item-1",
		"questions": []any{map[string]any{
			"id": "q1", "header": "Deploy key", "question": "paste it", "isSecret": true,
		}},
		"autoResolutionMs": 60000,
	})
	q := <-questions

	if q.Ask != event.InputToolValue {
		t.Errorf("ask = %q, want tool_value", q.Ask)
	}
	if q.Tool == nil || q.Tool.Kind != "tool_value_secret" {
		t.Fatalf("a secret answer must be marked; got %+v", q.Tool)
	}
	if v, _ := q.Tool.RawInput["secret"].(bool); !v {
		t.Error("RawInput must carry the secret flag for the vault to route on")
	}
	if q.Deadline.IsZero() {
		t.Error("autoResolutionMs is a deadline the runtime will act on; carry it")
	}
	_ = q.Reply(context.Background(), event.Reply{Decision: event.DecisionDeny})
	f.next(t)
}

// TestElicitationMayBelongToNoTurn. turnId is nullable on this one request
// only: MCP models elicitation as a standalone server-to-client request.
func TestElicitationMayBelongToNoTurn(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{UnverifiedReplies: map[string]ReplyEncoder{
		MethodElicitation: func(json.RawMessage, event.Reply) (any, error) { return map[string]any{}, nil },
	}})
	s := f.open(t, a, defaultSessionOptions(), testThread)

	questions := make(chan *event.NeedsInput, 4)
	go func() {
		for e := range s.Events() {
			if q, ok := e.(*event.NeedsInput); ok {
				questions <- q
			}
		}
	}()

	f.request(11, MethodElicitation, map[string]any{
		"threadId": testThread, "serverName": "linear", "turnId": nil,
		"mode": "url", "message": "authorise Relay", "url": "https://example.invalid/oauth",
		"elicitationId": "e1",
	})
	q := <-questions
	if q.Envelope().Turn != "" {
		t.Errorf("turn = %q; an elicitation with a null turnId belongs to no turn", q.Envelope().Turn)
	}
	if q.Ask != event.InputElicitation {
		t.Errorf("ask = %q", q.Ask)
	}
	if !strings.Contains(q.Prompt, "linear") || !strings.Contains(q.Prompt, "authorise Relay") {
		t.Errorf("prompt = %q", q.Prompt)
	}
	_ = q.Reply(context.Background(), event.Reply{Decision: event.DecisionDeny})

	// A numeric request id must come back as a number, not as a string.
	reply := f.next(t)
	if string(reply.ID) != "11" {
		t.Errorf("id echoed as %s, want the integer it arrived as", reply.ID)
	}
}

// TestCancelResolvesOutstandingQuestions. turn/interrupt does not answer a
// pending approval, and a turn cannot unwind while Codex is still blocked on
// one.
func TestCancelResolvesOutstandingQuestions(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{})
	s := f.open(t, a, defaultSessionOptions(), testThread)

	questions := make(chan *event.NeedsInput, 4)
	go func() {
		for e := range s.Events() {
			if q, ok := e.(*event.NeedsInput); ok {
				questions <- q
			}
		}
	}()

	f.notify("turn/started", map[string]any{
		"threadId": testThread,
		"turn":     map[string]any{"id": "turn-1", "status": "inProgress", "items": []any{}},
	})
	waitFor(t, func() bool { return s.ActiveTurn() == "turn-1" }, "the turn to open")

	f.request("srv-c", MethodCommandApproval, approvalParams("turn-1", "item-1"))
	q := <-questions

	done := make(chan error, 1)
	go func() { done <- s.Cancel(context.Background(), "turn-1") }()

	// The approval is answered first, so the turn has something to unwind into.
	answer := f.next(t)
	if answer.Method != "" {
		t.Fatalf("expected the approval to be answered before turn/interrupt, got %s", answer.Method)
	}
	var decision string
	_ = json.Unmarshal(answer.Result, &decision)
	if decision != "cancel" {
		t.Errorf("a cancelled turn answers %q, want cancel", decision)
	}

	interrupt := f.expect(t, "turn/interrupt")
	f.reply(interrupt.ID, map[string]any{})
	if err := <-done; err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !q.Answered() {
		t.Error("the question must not outlive the turn it belonged to")
	}
}

// TestRequestForAnUnknownThreadIsStillAnswered. Silence is the one thing that
// hangs app-server for good.
func TestRequestForAnUnknownThreadIsStillAnswered(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{})
	_ = a

	f.request("orphan", MethodCommandApproval, approvalParams("turn-1", "item-1"))
	reply := f.next(t)
	if reply.Error == nil {
		t.Fatal("a request for a thread we do not own must still be answered")
	}
}

func TestRefusalsNameTheirReason(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{Log: logx.Discard()})
	_ = a
	f.request(1, MethodAttestation, map[string]any{})
	reply := f.next(t)
	if reply.Error == nil || !strings.Contains(reply.Error.Message, "requestAttestation") {
		t.Fatalf("the refusal should explain itself: %+v", reply.Error)
	}
}
