package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// collector keeps every event the client handed over, in order.
type collector struct {
	mu   sync.Mutex
	list []Event
}

func (c *collector) add(e Event) {
	c.mu.Lock()
	c.list = append(c.list, e)
	c.mu.Unlock()
}

func (c *collector) all() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Event, len(c.list))
	copy(out, c.list)
	return out
}

// waitFor returns the nth event with this name, or fails.
func (c *collector) waitFor(t *testing.T, name string, nth int) Event {
	t.Helper()
	return c.waitMatch(t, name, func(Event) bool { return true }, nth)
}

// waitMatch returns the nth event with this name that ok accepts.
func (c *collector) waitMatch(t *testing.T, name string, ok func(Event) bool, nth int) Event {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		seen := 0
		for _, e := range c.all() {
			if e.Name != name || !ok(e) {
				continue
			}
			seen++
			if seen > nth {
				return e
			}
		}
		select {
		case <-deadline:
			var got []string
			for _, e := range c.all() {
				got = append(got, e.Name)
			}
			t.Fatalf("no matching %s event #%d; saw %v", name, nth, got)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestTheCapturedClaudeCodeTurnDrivesThisClient(t *testing.T) {
	recs := loadCapture(t, "03-session-create-and-turn-claude-cli.jsonl")
	srv := newReplayServer(serverFrames(recs, ""))
	col := &collector{}

	c, ctx := runClient(t, Options{
		URL:     "ws://127.0.0.1:19311",
		Token:   StaticToken("a-token"),
		OnEvent: col.add,
		Dial: func(dctx context.Context, u string) (Conn, error) {
			go srv.serve(context.Background())
			return srv.dial(dctx, u)
		},
	})

	// The same calls, in the same order, as the capture.
	if err := c.SessionsSubscribe(ctx); err != nil {
		t.Fatalf("sessions.subscribe: %v", err)
	}
	created, err := c.SessionsCreate(ctx, SessionsCreateParams{
		Model: "anthropic/claude-opus-4-8",
		Label: "probe-cc",
	})
	if err != nil {
		t.Fatalf("sessions.create: %v", err)
	}
	if !strings.HasPrefix(created.Key, "agent:main:") {
		t.Errorf("key %q is not a session key", created.Key)
	}
	if created.SessionID == "" || created.Entry.SessionFile == "" {
		t.Errorf("create did not decode: %+v", created)
	}
	if created.RunStarted {
		t.Error("runStarted is true for a create with no message")
	}

	// sessions.messages.subscribe and sessions.send have no wrapper on purpose;
	// this is what reaching past the typed methods looks like.
	if err := c.Sticky(ctx, "sessions.messages.subscribe", map[string]string{"key": created.Key}, nil); err != nil {
		t.Fatalf("sessions.messages.subscribe: %v", err)
	}
	var sent struct {
		RunID      string `json:"runId"`
		Status     string `json:"status"`
		MessageSeq int    `json:"messageSeq"`
	}
	err = c.Call(ctx, "sessions.send", map[string]any{
		"key":       created.Key,
		"message":   "Reply with exactly the token PROBE_OK and nothing else.",
		"timeoutMs": 90000,
	}, &sent)
	if err != nil {
		t.Fatalf("sessions.send: %v", err)
	}
	if sent.Status != "started" || sent.RunID == "" {
		t.Errorf("sessions.send answered %+v", sent)
	}

	// The run, as events.
	start := col.waitFor(t, EventAgent, 0)
	var lifecycle AgentEvent
	if err := start.Decode(&lifecycle); err != nil {
		t.Fatalf("agent event: %v", err)
	}
	if lifecycle.Stream != StreamLifecycle || lifecycle.RunID != sent.RunID {
		t.Errorf("first agent event was %+v", lifecycle)
	}
	var phase Lifecycle
	if err := json.Unmarshal(lifecycle.Data, &phase); err != nil || phase.Phase != "start" {
		t.Errorf("lifecycle data %s (%v)", lifecycle.Data, err)
	}

	// The assistant stream, which is the only place the answer appears while it
	// is still being written. Text is cumulative and Delta is the new part, so
	// a consumer can take either; both are asserted because taking the wrong
	// one produces "PPROBE_OK" and nobody notices until a user hears it.
	first := col.waitMatch(t, EventAgent, func(e Event) bool {
		var ev AgentEvent
		return e.Decode(&ev) == nil && ev.Stream == StreamAssistant
	}, 0)
	var ev AgentEvent
	if err := first.Decode(&ev); err != nil {
		t.Fatalf("assistant event: %v", err)
	}
	var a Assistant
	if err := json.Unmarshal(ev.Data, &a); err != nil {
		t.Fatalf("assistant data: %v", err)
	}
	if a.Text != "P" || a.Delta != "P" {
		t.Errorf("first assistant frame was %+v", a)
	}

	whole := col.waitMatch(t, EventAgent, func(e Event) bool {
		var ev AgentEvent
		if e.Decode(&ev) != nil || ev.Stream != StreamAssistant {
			return false
		}
		var a Assistant
		return json.Unmarshal(ev.Data, &a) == nil && a.Text == "PROBE_OK"
	}, 0)
	if whole.Name != EventAgent {
		t.Fatal("the answer never arrived whole")
	}

	// And the run ends, which is the frame a registry would move a session on.
	end := col.waitMatch(t, EventAgent, func(e Event) bool {
		var ev AgentEvent
		if e.Decode(&ev) != nil || ev.Stream != StreamLifecycle {
			return false
		}
		var l Lifecycle
		return json.Unmarshal(ev.Data, &l) == nil && l.Phase == "end"
	}, 0)
	var ended AgentEvent
	if err := end.Decode(&ended); err != nil || ended.RunID != sent.RunID {
		t.Errorf("the end frame belongs to another run: %+v (%v)", ended, err)
	}
}

func TestSessionsChangedReadsBothOfTheGatewaysShapes(t *testing.T) {
	recs := loadCapture(t, "03-session-create-and-turn-claude-cli.jsonl")

	var flat, nested *SessionsChanged
	for _, r := range serverFrames(recs, "") {
		var f frame
		if err := json.Unmarshal(r, &f); err != nil || f.Event != EventSessionsChanged {
			continue
		}
		var sc SessionsChanged
		if err := json.Unmarshal(f.Payload, &sc); err != nil {
			t.Fatalf("sessions.changed: %v", err)
		}
		if sc.Session == nil && flat == nil {
			c := sc
			flat = &c
		}
		if sc.Session != nil && nested == nil {
			c := sc
			nested = &c
		}
	}
	if flat == nil || nested == nil {
		t.Fatalf("the capture no longer has both shapes (flat=%v nested=%v)", flat != nil, nested != nil)
	}

	// The flat shape splices the row into the payload and never sends `key`.
	if flat.Row().Key != flat.SessionKey || flat.SessionKey == "" {
		t.Errorf("flat row did not take its key from sessionKey: %+v", flat.Row())
	}
	if flat.Reason == "" {
		t.Errorf("flat shape lost its reason: %+v", flat)
	}
	if flat.Row().AgentRuntime == nil || flat.Row().AgentRuntime.ID == "" {
		t.Errorf("agentRuntime did not decode: %+v", flat.Row())
	}

	// The nested shape is the run's phase, with the row underneath.
	if nested.Phase == "" || nested.RunID == "" {
		t.Errorf("nested shape lost the run: %+v", nested)
	}
	if nested.Row().Key == "" {
		t.Errorf("nested row did not decode: %+v", nested.Row())
	}
}

func TestARejectedModelKeepsTheGatewaysOwnMessage(t *testing.T) {
	recs := loadCapture(t, "07-sessions-create-model-rejected.jsonl")
	srv := newReplayServer(serverFrames(recs, ""))

	c, ctx := runClient(t, Options{
		URL:   "ws://127.0.0.1:19311",
		Token: StaticToken("a-token"),
		Dial: func(dctx context.Context, u string) (Conn, error) {
			go srv.serve(context.Background())
			return srv.dial(dctx, u)
		},
	})

	if _, err := c.SessionsCreate(ctx, SessionsCreateParams{}); err != nil {
		t.Fatalf("bare sessions.create: %v", err)
	}

	_, err := c.SessionsCreate(ctx, SessionsCreateParams{Model: "claude-cli/claude-opus-5"})
	if err == nil {
		t.Fatal("a rejected model ref came back as success")
	}
	if Code(err) != CodeInvalidRequest {
		t.Errorf("code %q, wanted %s", Code(err), CodeInvalidRequest)
	}
	// The provider's own sentence is the whole value of the error: it names the
	// ref, which is what tells a reader the allowlist is config-coupled.
	if !strings.Contains(err.Error(), "model not allowed: claude-cli/claude-opus-5") {
		t.Errorf("the gateway's message did not survive: %v", err)
	}
}

func TestAnApprovalRaisedElsewhereArrivesAndIsAnswered(t *testing.T) {
	// The capture's A socket is a second client that did not raise this
	// approval — which is exactly relayd's position.
	recs := loadCapture(t, "04-exec-approval-external-client.jsonl")
	srv := newReplayServer(serverFrames(recs, "A-observer"))
	col := &collector{}

	c, ctx := runClient(t, Options{
		URL:     "ws://127.0.0.1:19311",
		Token:   StaticToken("a-token"),
		Scopes:  []string{ScopeRead, ScopeApprovals, ScopeAdmin},
		OnEvent: col.add,
		Dial: func(dctx context.Context, u string) (Conn, error) {
			go srv.serve(context.Background())
			return srv.dial(dctx, u)
		},
	})

	var req ExecApprovalRequested
	if err := col.waitFor(t, EventExecApprovalRequested, 0).Decode(&req); err != nil {
		t.Fatalf("exec.approval.requested: %v", err)
	}
	if req.ID == "" || req.Request.Command == "" {
		t.Fatalf("the approval did not decode: %+v", req)
	}
	if req.Request.SessionKey == "" || req.Request.AgentID == "" {
		t.Errorf("the approval does not say whose it is: %+v", req.Request)
	}
	if len(req.Request.AllowedDecisions) == 0 || !contains(req.Request.AllowedDecisions, DecisionAllowOnce) {
		t.Errorf("allowedDecisions %v", req.Request.AllowedDecisions)
	}
	if req.ExpiresAtMs <= req.CreatedAtMs {
		t.Errorf("the approval carries no deadline: %+v", req)
	}

	if err := c.ExecApprovalResolve(ctx, req.ID, DecisionAllowOnce); err != nil {
		t.Fatalf("exec.approval.resolve: %v", err)
	}

	var sent map[string]string
	for _, r := range srv.requests() {
		if r.Method == MethodExecApprovalResolve {
			if err := json.Unmarshal(r.Params, &sent); err != nil {
				t.Fatalf("resolve params: %v", err)
			}
		}
	}
	if sent["id"] != req.ID || sent["decision"] != DecisionAllowOnce {
		t.Errorf("resolve sent %v", sent)
	}

	var resolved ExecApprovalResolved
	if err := col.waitFor(t, EventExecApprovalResolved, 0).Decode(&resolved); err != nil {
		t.Fatalf("exec.approval.resolved: %v", err)
	}
	if resolved.ID != req.ID || resolved.Decision != DecisionAllowOnce {
		t.Errorf("resolved %+v", resolved)
	}
	if resolved.ResolvedBy == "" {
		t.Error("resolved does not say who answered, so a ping cannot be retracted by name")
	}
}

func TestResolveRefusesTheWordThatReadsRight(t *testing.T) {
	c, err := New(Options{URL: "ws://127.0.0.1:19311"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// "approve" is rejected by the gateway with INVALID_REQUEST. Finding that
	// out over the wire, while a person waits, is the wrong time.
	err = c.ExecApprovalResolve(context.Background(), "an-id", "approve")
	if err == nil {
		t.Fatal("accepted a decision the gateway rejects")
	}
	if !strings.Contains(err.Error(), DecisionAllowOnce) || !strings.Contains(err.Error(), DecisionDeny) {
		t.Errorf("the refusal does not name the decisions that work: %v", err)
	}
	if err := c.ExecApprovalResolve(context.Background(), "", DecisionDeny); err == nil {
		t.Error("accepted a resolve with no approval id")
	}
}

func TestTheMethodsThatNeedATargetSaySoBeforeTheWire(t *testing.T) {
	c, err := New(Options{URL: "ws://127.0.0.1:19311"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if _, err := c.SessionsAbort(ctx, SessionsAbortParams{}); err == nil {
		t.Error("sessions.abort with neither a key nor a run id")
	}
	if _, err := c.Agent(ctx, AgentParams{}); err == nil {
		t.Error("agent with no message")
	}
	if _, err := c.ExecApprovalRequest(ctx, ExecApprovalRequest{}); err == nil {
		t.Error("exec.approval.request with no command")
	}
}
