package acp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/event"
)

var (
	fullOptions = []PermissionOption{
		{OptionID: "o1", Name: "Allow", Kind: "allow_once"},
		{OptionID: "o2", Name: "Always allow", Kind: "allow_always"},
		{OptionID: "o3", Name: "Reject", Kind: "reject_once"},
	}
	standingOnly = []PermissionOption{
		{OptionID: "s1", Name: "Always allow", Kind: "allow_always"},
		{OptionID: "s2", Name: "Always reject", Kind: "reject_always"},
	}
)

func askPermission(t *testing.T, f *fakeAgent, c *collector, id int, tc map[string]any, opts []PermissionOption) *event.NeedsInput {
	t.Helper()
	f.request(id, methodRequestPermission, map[string]any{
		"sessionId": "sess_1",
		"toolCall":  tc,
		"options":   opts,
	})
	return c.needsInput(t)
}

func permissionSession(t *testing.T) (*fakeAgent, *Session, *collector) {
	t.Helper()
	f := newFakeAgent(t)
	a := dial(t, f, testOptions(t, adapter.OpenCode), AgentCapabilities{})
	s := startSession(t, f, a, "sess_1")
	return f, s, collect(s)
}

func outcomeOf(t *testing.T, m *message) permissionOutcome {
	t.Helper()
	var r requestPermissionResult
	if err := json.Unmarshal(m.Result, &r); err != nil {
		t.Fatalf("permission response: %v (%s)", err, string(m.Result))
	}
	return r.Outcome
}

func TestPermissionSpeaksTheOptionsItWasGiven(t *testing.T) {
	ctx := context.Background()
	f, _, c := permissionSession(t)
	n := askPermission(t, f, c, 101,
		map[string]any{"toolCallId": "call_1", "title": "Run npm test", "kind": "execute"},
		fullOptions)

	if n.Prompt != "Run npm test" {
		t.Errorf("prompt = %q", n.Prompt)
	}
	if n.Ask != event.InputPermission {
		t.Errorf("ask = %q", n.Ask)
	}
	if !n.Deadline.IsZero() {
		t.Error("ACP puts no deadline on a permission request; the agent waits as long as we take")
	}
	names := []string{}
	for _, o := range n.Options {
		names = append(names, o.Name)
	}
	if strings.Join(names, ",") != "Allow,Always allow,Reject" {
		t.Errorf("options = %v; the agent's own names go through verbatim", names)
	}

	if err := n.Reply(ctx, event.Reply{OptionID: "not-offered"}); !errors.Is(err, event.ErrUnknownOption) {
		t.Fatalf("replying with an option the agent never offered returned %v", err)
	}
	if err := n.Reply(ctx, event.Reply{OptionID: "o1"}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	got := outcomeOf(t, f.next())
	if got.Outcome != outcomeSelected || got.OptionID != "o1" {
		t.Errorf("outcome = %+v", got)
	}
	if err := n.Reply(ctx, event.Reply{OptionID: "o3"}); !errors.Is(err, event.ErrAnswered) {
		t.Errorf("a second reply returned %v, want ErrAnswered", err)
	}
}

func TestDecisionsMapOntoNonStandingOptions(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		reply   event.Reply
		wantOpt string
	}{
		{"allow", event.Reply{Decision: event.DecisionAllow}, "o1"},
		{"deny", event.Reply{Decision: event.DecisionDeny}, "o3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, c := permissionSession(t)
			n := askPermission(t, f, c, 101, map[string]any{"toolCallId": "call_1", "title": "x"}, fullOptions)
			if err := n.Reply(ctx, tc.reply); err != nil {
				t.Fatalf("Reply: %v", err)
			}
			got := outcomeOf(t, f.next())
			if got.Outcome != outcomeSelected || got.OptionID != tc.wantOpt {
				t.Errorf("outcome = %+v, want %s", got, tc.wantOpt)
			}
		})
	}
}

// TestStandingOptionsAreNeverChosenForTheUser: ORCHESTRATOR.md §4b requires
// consequential choices to be made by a human every time, and "always allow" is
// exactly that. The option is still offered; we just will not pick it.
func TestStandingOptionsAreNeverChosenForTheUser(t *testing.T) {
	ctx := context.Background()
	f, _, c := permissionSession(t)
	n := askPermission(t, f, c, 101, map[string]any{"toolCallId": "call_1", "title": "x"}, standingOnly)

	if len(n.Options) != 2 || !n.Options[0].Kind.Standing() || !n.Options[1].Kind.Standing() {
		t.Fatalf("options = %+v", n.Options)
	}
	err := n.Reply(ctx, event.Reply{Decision: event.DecisionAllow})
	if !errors.Is(err, event.ErrUnknownOption) {
		t.Fatalf("Reply returned %v; a standing grant must not be selected on the user's behalf", err)
	}
	if !strings.Contains(err.Error(), "chosen by a human") {
		t.Errorf("the error should say why: %v", err)
	}
	// The question is still open, and a human can still pick the standing one.
	if n.Answered() {
		t.Fatal("a refused auto-selection must leave the question open")
	}
	if err := n.Reply(ctx, event.Reply{OptionID: "s1"}); err != nil {
		t.Fatalf("Reply with an explicit standing choice: %v", err)
	}
	if got := outcomeOf(t, f.next()); got.OptionID != "s1" {
		t.Errorf("outcome = %+v", got)
	}
}

// TestInterruptIsRejectPlusCancel: ACP has no hard stop inside the permission
// response — rejecting a tool is not cancelling a turn.
func TestInterruptIsRejectPlusCancel(t *testing.T) {
	ctx := context.Background()
	f, s, c := permissionSession(t)
	turnID, err := s.Send(ctx, adapter.Turn{Text: "go"})
	if err != nil {
		t.Fatal(err)
	}
	prompt := f.next()

	n := askPermission(t, f, c, 101, map[string]any{"toolCallId": "call_1", "title": "rm -rf"}, fullOptions)
	if n.Turn != turnID {
		t.Errorf("the question belongs to turn %q, want %q", n.Turn, turnID)
	}

	done := make(chan error, 1)
	go func() { done <- n.Reply(ctx, event.Reply{Decision: event.DecisionDeny, Interrupt: true}) }()

	saw := map[string]bool{}
	for i := 0; i < 2; i++ {
		m := f.next()
		switch {
		case m.Method == methodCancel:
			saw["cancel"] = true
		case len(m.Result) > 0:
			if got := outcomeOf(t, m); got.OptionID == "o3" {
				saw["reject"] = true
			}
		}
	}
	if !saw["cancel"] || !saw["reject"] {
		t.Fatalf(`"no, stop" must be reject *plus* session/cancel; saw %v`, saw)
	}
	f.respond(prompt.ID, promptResult{StopReason: "cancelled"})
	if err := <-done; err != nil {
		t.Fatalf("Reply with interrupt: %v", err)
	}
	c.waitFor(t, "the cancelled completion", func(e event.Event) bool {
		tc, ok := e.(event.TurnCompleted)
		return ok && tc.StopReason == event.StopCancelled
	})
}

// TestInterruptWithNothingToRejectUsesTheCancelledOutcome: no option is
// invented; the protocol's own escape hatch is used instead.
func TestInterruptWithNothingToRejectUsesTheCancelledOutcome(t *testing.T) {
	ctx := context.Background()
	f, s, c := permissionSession(t)
	_, _ = s.Send(ctx, adapter.Turn{Text: "go"})
	prompt := f.next()

	n := askPermission(t, f, c, 101, map[string]any{"toolCallId": "call_1"},
		[]PermissionOption{{OptionID: "a1", Name: "Allow", Kind: "allow_once"}})

	done := make(chan error, 1)
	go func() { done <- n.Reply(ctx, event.Reply{Decision: event.DecisionDeny, Interrupt: true}) }()

	sawCancelled := false
	for i := 0; i < 2; i++ {
		m := f.next()
		if len(m.Result) > 0 && outcomeOf(t, m).Outcome == outcomeCancelled {
			sawCancelled = true
		}
	}
	if !sawCancelled {
		t.Error("with nothing to reject, the cancelled outcome is the only honest answer")
	}
	f.respond(prompt.ID, promptResult{StopReason: "cancelled"})
	<-done
}

// TestDenyWithNoRejectOptionIsAnError: never invent an option that was not in
// the array.
func TestDenyWithNoRejectOptionIsAnError(t *testing.T) {
	ctx := context.Background()
	f, _, c := permissionSession(t)
	n := askPermission(t, f, c, 101, map[string]any{"toolCallId": "call_1"},
		[]PermissionOption{{OptionID: "a1", Name: "Allow", Kind: "allow_once"}})
	if err := n.Reply(ctx, event.Reply{Decision: event.DecisionDeny}); !errors.Is(err, event.ErrUnknownOption) {
		t.Fatalf("Reply returned %v", err)
	}
	if err := n.Reply(ctx, event.Reply{}); err == nil {
		t.Error("a reply with neither an option nor a decision must be refused")
	}
}

// TestCancelResolvesOutstandingPermissions is mandatory: without it the agent's
// turn cannot unwind and session/prompt never resolves.
func TestCancelResolvesOutstandingPermissions(t *testing.T) {
	ctx := context.Background()
	f, s, c := permissionSession(t)
	turnID, _ := s.Send(ctx, adapter.Turn{Text: "go"})
	prompt := f.next()

	n := askPermission(t, f, c, 101, map[string]any{"toolCallId": "call_1"}, fullOptions)
	if s.OutstandingPermissions() != 1 {
		t.Fatalf("outstanding = %d", s.OutstandingPermissions())
	}

	done := make(chan error, 1)
	go func() { done <- s.Cancel(ctx, turnID) }()

	saw := map[string]bool{}
	for i := 0; i < 2; i++ {
		m := f.next()
		if m.Method == methodCancel {
			saw["cancel"] = true
		}
		if len(m.Result) > 0 && outcomeOf(t, m).Outcome == outcomeCancelled {
			saw["cancelled-outcome"] = true
		}
	}
	if !saw["cancel"] || !saw["cancelled-outcome"] {
		t.Fatalf("saw %v; both obligations are mandatory on cancel", saw)
	}
	if !s.Cancelling() {
		t.Error("between the notification and the resolution the session is stopping, not running")
	}

	f.respond(prompt.ID, promptResult{StopReason: "cancelled"})
	if err := <-done; err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !n.Answered() {
		t.Error("the question must be resolved so a pending ping can be retracted")
	}
	if err := n.Reply(ctx, event.Reply{OptionID: "o1"}); !errors.Is(err, event.ErrWithdrawn) {
		t.Errorf("replying after a cancel returned %v", err)
	}
	if s.OutstandingPermissions() != 0 {
		t.Errorf("outstanding = %d", s.OutstandingPermissions())
	}
}

// TestPermissionWithNoTitleSaysSo: toolCall is a ToolCallUpdate and only
// toolCallId is guaranteed, so there may be nothing human-readable to read
// aloud.
func TestPermissionWithNoTitleSaysSo(t *testing.T) {
	f, _, c := permissionSession(t)
	n := askPermission(t, f, c, 101,
		map[string]any{"toolCallId": "call_9", "rawInput": map[string]any{"command": "rm -rf /"}},
		fullOptions)

	if !strings.Contains(n.Prompt, "did not say what it does") || !strings.Contains(n.Prompt, "call_9") {
		t.Errorf("prompt = %q", n.Prompt)
	}
	if strings.Contains(n.Prompt, "rm -rf") {
		t.Error("the prompt must not infer a description out of rawInput")
	}
	if n.Tool == nil || n.Tool.Title != "" || n.Tool.RawInput["command"] != "rm -rf /" {
		t.Errorf("tool ref = %+v; rawInput is carried, just not spoken", n.Tool)
	}
}

// TestUnknownOptionKindIsCarriedNotDropped: PermissionOptionKind is a UI hint,
// not a fixed menu, and dropping an option removes an answer the agent accepts.
func TestUnknownOptionKindIsCarriedNotDropped(t *testing.T) {
	ctx := context.Background()
	f, _, c := permissionSession(t)
	n := askPermission(t, f, c, 101, map[string]any{"toolCallId": "call_1", "title": "x"},
		[]PermissionOption{{OptionID: "z1", Name: "Sandbox it", Kind: "run_in_sandbox_v2"}})

	if len(n.Options) != 1 || n.Options[0].Kind != event.OptionOther {
		t.Fatalf("options = %+v", n.Options)
	}
	if n.Options[0].Kind.Standing() {
		t.Error("an unrecognised kind must not read as a standing grant")
	}
	if err := n.Reply(ctx, event.Reply{OptionID: "z1"}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if got := outcomeOf(t, f.next()); got.OptionID != "z1" {
		t.Errorf("outcome = %+v", got)
	}
}

// TestPermissionForAnUnknownSessionIsAnswered: an unanswered JSON-RPC request
// stalls the agent, so even a request we cannot place gets a reply.
func TestPermissionForAnUnknownSessionIsAnswered(t *testing.T) {
	f := newFakeAgent(t)
	a := dial(t, f, testOptions(t, adapter.OpenCode), AgentCapabilities{})

	f.request(101, methodRequestPermission, map[string]any{
		"sessionId": "sess_nobody_opened",
		"toolCall":  map[string]any{"toolCallId": "call_1"},
		"options":   fullOptions,
	})
	// It is buffered rather than refused outright: a session/new may still be
	// in flight. Closing the adapter is what makes the answer due.
	if m, ok := f.tryNext(150 * time.Millisecond); ok {
		t.Fatalf("answered too early: %+v", m)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case m, ok := <-f.incoming:
		if !ok {
			t.Fatal("the connection closed without answering the buffered request")
		}
		if m.Error == nil || m.Error.Code != codeMethodNotFound {
			t.Fatalf("buffered request answered with %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the buffered permission request was never answered")
	}
}
