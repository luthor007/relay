package codex

import (
	"context"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/logx"
)

func readConfig(t *testing.T, f *fakeServer, a *Adapter, result any) ApprovalGuard {
	t.Helper()
	type res struct {
		g   ApprovalGuard
		err error
	}
	ch := make(chan res, 1)
	go func() {
		g, err := a.CheckApprovals(context.Background(), "/w")
		ch <- res{g, err}
	}()
	m := f.expect(t, "config/read")
	f.reply(m.ID, result)
	r := <-ch
	if r.err != nil {
		t.Fatalf("config/read: %v", r.err)
	}
	return r.g
}

// TestConfigReadIsThePreflightTrapCheck. codex-methods.md calls config/read the
// trap detector: it is how Relay finds out that this machine's Codex will never
// ask for approval, before a session exists to find out the hard way.
func TestConfigReadIsThePreflightTrapCheck(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{})

	g := readConfig(t, f, a, map[string]any{
		"config": map[string]any{"approval_policy": "on-request", "approvals_reviewer": "user"},
	})
	if !g.OK() {
		t.Fatalf("a healthy config was reported as failing: %s", g.Why())
	}

	g = readConfig(t, f, a, map[string]any{
		"layers": []any{map[string]any{"values": map[string]any{"approval_policy": "never"}}},
	})
	if g.OK() {
		t.Fatal(`approval_policy "never" must fail the guard, wherever in the tree it sits`)
	}
	if !strings.Contains(g.Why(), "never") {
		t.Errorf("why = %q", g.Why())
	}

	g = readConfig(t, f, a, map[string]any{
		"config": map[string]any{"approval_policy": "on-request", "approvals_reviewer": "auto_review"},
	})
	if g.OK() {
		t.Fatal("auto_review routes approvals to a subagent; the guard must fail")
	}
	if !strings.Contains(g.Why(), "subagent") {
		t.Errorf("why = %q", g.Why())
	}
}

// TestAnUnknownGuardIsNotAPassingGuard. There is no ServerResponse.json, so a
// key we did not find may be somewhere we did not look. Saying "fine" would be
// exactly the invention this package refuses to make.
func TestAnUnknownGuardIsNotAPassingGuard(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{})

	g := readConfig(t, f, a, map[string]any{"model": "gpt-5-codex"})
	if g.Found {
		t.Fatal("nothing was found")
	}
	if g.OK() {
		t.Fatal("an unknown guard must not read as a passing one")
	}
	if !strings.Contains(g.Why(), "unknown rather than fine") {
		t.Errorf("why = %q", g.Why())
	}
}

// TestGranularApprovalPolicyIsNotNever. AskForApproval's other branch is
// `{granular:{…}}`, which narrows which approvals arrive rather than switching
// them off.
func TestGranularApprovalPolicyIsNotNever(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{})

	g := readConfig(t, f, a, map[string]any{
		"config": map[string]any{
			"approval_policy": map[string]any{"granular": map[string]any{
				"mcp_elicitations": true, "rules": true, "sandbox_approval": true,
			}},
			"approvals_reviewer": "user",
		},
	})
	if !g.OK() {
		t.Fatalf("a granular policy is not never: %s", g.Why())
	}
	if g.Policy != "granular" {
		t.Errorf("policy = %q", g.Policy)
	}
}

// TestMCPToolCallProgressBecomesToolOutput, and the label carries the server so
// two MCP servers offering the same tool name stay distinguishable.
func TestMCPToolCallMapping(t *testing.T) {
	var got []event.Event
	n := newNormalizer(func(e event.Event) { got = append(got, e) }, &metaSource{now: fixedClock()}, logx.Discard(), hooks{})

	n.handle("item/started", mustJSON(t, map[string]any{
		"threadId": "x", "turnId": "t1", "startedAtMs": 1,
		"item": map[string]any{
			"id": "mc1", "type": "mcpToolCall", "server": "linear", "tool": "create_issue",
			"status": "inProgress", "arguments": map[string]any{"title": "fix it"},
		},
	}))
	n.handle("item/mcpToolCall/progress", mustJSON(t, map[string]any{
		"threadId": "x", "turnId": "t1", "itemId": "mc1", "message": "creating…",
	}))
	n.handle("item/completed", mustJSON(t, map[string]any{
		"threadId": "x", "turnId": "t1", "completedAtMs": 2,
		"item": map[string]any{
			"id": "mc1", "type": "mcpToolCall", "server": "linear", "tool": "create_issue",
			"status": "completed", "arguments": map[string]any{},
		},
	}))

	want := []string{
		`tool_started mc1 mcp:linear/create_issue "create_issue"`,
		`tool_output mc1  "creating…"`,
		`tool_output mc1 completed ""`,
	}
	if len(got) != len(want) {
		t.Fatalf("got %s", describeAll(got))
	}
	for i := range want {
		if describe(got[i]) != want[i] {
			t.Errorf("%d: got %s, want %s", i, describe(got[i]), want[i])
		}
	}
	ts := got[0].(event.ToolStarted)
	if ts.RawInput["server"] != "linear" {
		t.Errorf("raw input = %v", ts.RawInput)
	}
	if _, invented := ts.RawInput["inferred"]; invented {
		t.Error("RawInput must be what the runtime said, never an inference")
	}
}

// TestAggregatedOutputOnlyLandsWhenNothingWasStreamed.
func TestAggregatedOutputOnlyLandsWhenNothingWasStreamed(t *testing.T) {
	item := map[string]any{
		"id": "c1", "type": "commandExecution", "command": "ls", "cwd": "/w",
		"commandActions": []any{}, "status": "completed",
		"aggregatedOutput": "a\nb\n",
	}
	completed := mustJSON(t, map[string]any{
		"threadId": "x", "turnId": "t1", "completedAtMs": 1, "item": item,
	})

	var got []event.Event
	n := newNormalizer(func(e event.Event) { got = append(got, e) }, &metaSource{now: fixedClock()}, logx.Discard(), hooks{})
	n.handle("item/completed", completed)
	if len(got) != 1 || describe(got[0]) != `tool_output c1 completed "a\nb\n"` {
		t.Fatalf("unstreamed command: %s", describeAll(got))
	}

	got = nil
	n2 := newNormalizer(func(e event.Event) { got = append(got, e) }, &metaSource{now: fixedClock()}, logx.Discard(), hooks{})
	n2.handle("item/commandExecution/outputDelta", mustJSON(t, map[string]any{
		"threadId": "x", "turnId": "t1", "itemId": "c1", "delta": "a\n",
	}))
	n2.handle("item/commandExecution/outputDelta", mustJSON(t, map[string]any{
		"threadId": "x", "turnId": "t1", "itemId": "c1", "delta": "b\n",
	}))
	n2.handle("item/completed", completed)
	if len(got) != 3 {
		t.Fatalf("streamed command: %s", describeAll(got))
	}
	if describe(got[2]) != `tool_output c1 completed ""` {
		t.Errorf("the aggregate repeated the deltas: %s", describe(got[2]))
	}
}

// TestDeclinedIsAFailureNotACompletion. `declined` is a Codex-only status and
// it means the tool did not run.
func TestDeclinedIsAFailureNotACompletion(t *testing.T) {
	if toolStatus("declined") != event.ToolFailed {
		t.Errorf("declined = %q", toolStatus("declined"))
	}
	if toolStatus("") != event.ToolUnknown {
		t.Error(`"" must stay "this update said nothing about status"`)
	}
	if toolStatus("somethingNew") != event.ToolUnknown {
		t.Error("an unrecognised status is unknown, not a guess")
	}
}

// TestUnknownNotificationsAreLoggedNotDropped is the same rule the ACP adapter
// keeps for `_`-prefixed methods: that log line is how we find out Codex
// shipped something new.
func TestUnknownNotificationsDoNotProduceEvents(t *testing.T) {
	var got []event.Event
	n := newNormalizer(func(e event.Event) { got = append(got, e) }, &metaSource{now: fixedClock()}, logx.Discard(), hooks{})
	for _, m := range []string{
		"thread/realtime/transcript/delta", // observable but not drivable, §3
		"item/plan/delta",                  // EXPERIMENTAL, later
		"model/rerouted",
		"something/nobody/has/seen",
	} {
		n.handle(m, mustJSON(t, map[string]any{"threadId": "x"}))
	}
	if len(got) != 0 {
		t.Fatalf("an unhandled notification invented events: %s", describeAll(got))
	}
}
