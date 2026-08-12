package claudecode

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/logx"
)

// The vendored recording *is* the contract (BUILD-PROMPT.md, "what is
// deliberately not in this repo"). These tests read it in place rather than
// copying it under testdata, so a format change breaks CI here and nowhere
// else has to be kept in sync.
const fixturePath = "../../../../docs/fixtures/adapters/claude-code.trace.json"

const goldenPath = "../../../testdata/claudecode/trace.golden.jsonl"

var update = flag.Bool("update", false, "rewrite the golden file from the fixture")

// loadFixture returns the 49 recorded events as NDJSON lines.
func loadFixture(t *testing.T) [][]byte {
	t.Helper()
	b, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("the vendored trace is missing: %v", err)
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("the vendored trace is not a JSON array: %v", err)
	}
	out := make([][]byte, 0, len(raw))
	for _, r := range raw {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, line)
	}
	return out
}

// splitAtResults cuts the recording into one chunk per turn, ending each chunk
// on its result event. That is what a real process does: it says nothing until
// a turn is written to its stdin.
func splitAtResults(t *testing.T, lines [][]byte) [][]byte {
	t.Helper()
	var chunks [][]byte
	var cur []byte
	for _, l := range lines {
		cur = append(cur, l...)
		cur = append(cur, '\n')
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(l, &head); err != nil {
			t.Fatal(err)
		}
		if head.Type == "result" {
			chunks = append(chunks, cur)
			cur = nil
		}
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected the fixture to hold two turns, got %d chunks", len(chunks))
	}
	return chunks
}

func fixedClock() func() time.Time {
	n := int64(0)
	base := time.Date(2026, 8, 10, 2, 45, 20, 0, time.UTC)
	return func() time.Time {
		n++
		return base.Add(time.Duration(n) * time.Millisecond)
	}
}

// newTestAdapter wires an adapter onto a scripted process. PermissionBaseURL is
// set so no listener is opened; the permission tests start a real one.
func newTestAdapter(t *testing.T, proc *scriptedProcess) (*Adapter, *scriptLauncher) {
	t.Helper()
	l := &scriptLauncher{proc: proc}
	a := New(Options{
		Launcher:          l,
		Log:               logx.Discard(),
		Now:               fixedClock(),
		PermissionBaseURL: "http://127.0.0.1:1/permissions",
		ConfigDir:         t.TempDir(),
		Home:              t.TempDir(),
	})
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	return a, l
}

func drain(t *testing.T, s adapter.Session) []event.Event {
	t.Helper()
	var out []event.Event
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-timeout:
			t.Fatalf("timed out waiting for the event stream to close after %d events", len(out))
		}
	}
}

// TestTraceFixture is the regression test the fixture exists for: the whole
// recorded session goes through the adapter and the normalized events are
// compared against a golden file. Run with -update to regenerate.
func TestTraceFixture(t *testing.T) {
	lines := loadFixture(t)
	if len(lines) != 49 {
		t.Fatalf("the vendored trace changed shape: %d events, expected 49. "+
			"That is the point of this test — re-read ADAPTERS.md §2 before touching the golden.", len(lines))
	}
	chunks := splitAtResults(t, lines)

	proc := newScriptedProcess(scriptOptions{Chunks: chunks})
	a, l := newTestAdapter(t, proc)

	ctx := context.Background()
	s, err := a.Start(ctx, adapter.SessionOptions{
		ID:        "b393be4c-99d7-4d92-ada2-df47ce494ffe",
		Workspace: "/work",
	})
	if err != nil {
		t.Fatal(err)
	}

	turn1, err := s.Send(ctx, adapter.Turn{
		ID:   "turn-1",
		Text: "Use the Bash tool to run exactly: echo hello-from-relay-fixture . Then reply DONE.",
	})
	if err != nil {
		t.Fatal(err)
	}
	turn2, err := s.Send(ctx, adapter.Turn{
		ID:   "turn-2",
		Text: "Say exactly: SECOND-TURN. Nothing else.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if turn1 != "turn-1" || turn2 != "turn-2" {
		t.Fatalf("Send did not honour the turn ids it was given: %q %q", turn1, turn2)
	}

	events := drain(t, s)
	got := renderEvents(events)

	if *update {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("golden updated")
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("golden missing (run go test -run TestTraceFixture -update): %v", err)
	}
	if got != string(want) {
		t.Errorf("normalized events changed.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}

	// The exact two lines that were injected into stdin. ADAPTERS.md §2 records
	// this shape verbatim and it is what makes "continue an existing session"
	// real for this runtime.
	in := proc.Input()
	if len(in) != 2 {
		t.Fatalf("expected two turns on stdin, got %d", len(in))
	}
	const wantLine = `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Say exactly: SECOND-TURN. Nothing else."}]}}`
	if in[1] != wantLine {
		t.Errorf("stdin turn line\n got: %s\nwant: %s", in[1], wantLine)
	}

	// The command line, in full, because a flag that silently goes missing is
	// how the permission prompt stops firing.
	spec := l.Spec()
	for _, want := range []string{
		"--input-format", "--output-format", "--verbose",
		"--include-partial-messages", "--replay-user-messages",
		"--setting-sources", "--strict-mcp-config", "--permission-prompt-tool",
		"--session-id", "--permission-mode",
	} {
		if !contains(spec.Args, want) {
			t.Errorf("the spawn is missing %s: %v", want, spec.Args)
		}
	}
}

// TestTraceObservations checks the state the adapter keeps alongside the event
// stream — the parts ADAPTERS.md §2 says arrive free and the console needs.
func TestTraceObservations(t *testing.T) {
	chunks := splitAtResults(t, loadFixture(t))
	proc := newScriptedProcess(scriptOptions{Chunks: chunks})
	a, _ := newTestAdapter(t, proc)

	ctx := context.Background()
	sess, err := a.Start(ctx, adapter.SessionOptions{ID: "b393be4c-99d7-4d92-ada2-df47ce494ffe", Workspace: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	s := sess.(*Session)
	if _, err := s.Send(ctx, adapter.Turn{ID: "turn-1", Text: "Use the Bash tool to run exactly: echo hello-from-relay-fixture . Then reply DONE."}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(ctx, adapter.Turn{ID: "turn-2", Text: "Say exactly: SECOND-TURN. Nothing else."}); err != nil {
		t.Fatal(err)
	}
	drain(t, s)

	init := s.Init()
	if init == nil {
		t.Fatal("no system/init was recorded")
	}
	if init.Version != "2.1.226" {
		t.Errorf("claude_code_version = %q", init.Version)
	}
	if init.Model != "claude-opus-5[1m]" {
		t.Errorf("init model = %q; it is the *decorated* id and a routing table must never key on it", init.Model)
	}
	if len(init.Tools) != 153 {
		t.Errorf("tools = %d, want 153", len(init.Tools))
	}
	if !contains(init.Capabilities, "interrupt_receipt_v1") {
		t.Errorf("init capabilities = %v", init.Capabilities)
	}
	if len(init.MCPServers) == 0 {
		t.Error("MEMORY.md §7 reconciliation needs mcp_servers from init")
	}

	// The permission trap, straight out of the recording: it was captured with
	// permissionMode "auto", which is exactly the state where the prompt tool
	// is never called. The fixture doubles as the regression test for the
	// detector.
	if init.PermissionMode != "auto" {
		t.Fatalf("the fixture's permissionMode is %q; this test exists because it is %q", init.PermissionMode, "auto")
	}
	if got := s.Capabilities().Get(adapter.CapNeedsInput); got != adapter.SupportNo {
		t.Errorf("CapNeedsInput = %v, want no: the runtime said approvals are off", got)
	}
	if note := s.Capabilities().Note(adapter.CapNeedsInput); note == "" {
		t.Error("a capability that was switched off must say why")
	}
	var found bool
	for _, h := range s.Hazards() {
		if h.Kind == HazardRuntimeReported && h.Value == "auto" {
			found = true
			if h.Remedy == "" || strings.Contains(strings.ToLower(h.Remedy), "bypass") {
				t.Errorf("a remedy must move toward asking, never toward a bypass: %q", h.Remedy)
			}
		}
	}
	if !found {
		t.Error("system/init reported permissionMode auto and no hazard was raised")
	}

	// Hooks: three, correlated by hook_id rather than by order, all successful.
	hooks := s.Hooks()
	if len(hooks) != 3 {
		t.Fatalf("hooks = %d, want 3", len(hooks))
	}
	for _, h := range hooks {
		if !h.Done || !h.OK() || h.Event != "SessionStart" {
			t.Errorf("hook %+v", h)
		}
	}

	// The live context: the *most recent request's* usage, which reads
	// 33,497 → 33,609 → 33,637 across the fixture's three requests. Never
	// result.usage, which sums a turn's requests and would report 52,001.
	cs := s.Context()
	if cs.Used != 33637 {
		t.Errorf("live context = %d, want 33637 (ADAPTERS.md §2)", cs.Used)
	}
	if cs.Window != 1000000 {
		t.Errorf("context window = %d, want 1000000 from modelUsage", cs.Window)
	}
	if p, ok := cs.Pressure(); !ok || p > 0.05 {
		t.Errorf("context pressure = %v ok=%v; MEMORY.md §9 must not compact a session this empty", p, ok)
	}

	res := s.LastResult()
	if res == nil || res.Result != "SECOND-TURN" {
		t.Fatalf("last result = %+v", res)
	}
	if res.CanonicalModel != "claude-opus-5" || res.Provider != "firstParty" {
		t.Errorf("model = %q / %q; only canonicalModel is the real name", res.CanonicalModel, res.Provider)
	}
	// The whole result event is surfaced, not just the turn boundary: latency,
	// iteration count and denial count all arrive free.
	if res.NumTurns != 1 || res.DurationAPIMS != 4450 || res.DurationMS != 1490 {
		t.Errorf("result timings = %+v", res)
	}
	if res.TTFTMs != 1456 || res.TTFTStreamMs != 833 || res.TimeToRequestMs != 25 {
		t.Errorf("latency = %d / %d / %d", res.TTFTMs, res.TTFTStreamMs, res.TimeToRequestMs)
	}
	if res.PermissionDenials != 0 || res.APIErrorStatus != "" || res.MaxOutputTokens != 64000 {
		t.Errorf("result = %+v", res)
	}
	if res.OutputTokens != 10 || res.CacheReadTokens != 33607 {
		t.Errorf("turn tokens = %+v", res)
	}
	// Per-turn cost is the delta; total_cost_usd is cumulative for the session.
	if got, want := res.TotalCostUSD, 0.19693700000000003; got != want {
		t.Errorf("cumulative cost = %v, want %v", got, want)
	}
	if got := res.CostUSD; got < 0.0173 || got > 0.0174 {
		t.Errorf("per-turn cost = %v, want ~0.01734 (the delta, not the total)", got)
	}

	// rate_limit_event is recorded but "allowed" is not news, so nothing is
	// emitted for it.
	rl := s.RateLimit()
	if rl == nil || rl.Status != "allowed" || rl.RateLimitType != "five_hour" {
		t.Errorf("rate limit = %+v", rl)
	}

	// system/status fired three times for two turns, which is why it is never
	// used as a turn boundary.
	if got := s.Unseen()["status:requesting"]; got != 3 {
		t.Errorf("system/status count = %d, want 3 (once per API request, not per turn)", got)
	}
	if got := s.Unseen()["!malformed"]; got != 0 {
		t.Errorf("%d lines of the vendored trace did not decode", got)
	}
}

// TestNoPlanUpdated is the grounding rule, enforced.
//
// ADAPTERS.md §5 marks PlanUpdated ✗ for Claude Code and the same section
// forbids emitting an event an adapter cannot observe. This adapter resolves
// that by never emitting one and saying so in its capabilities.
func TestNoPlanUpdated(t *testing.T) {
	chunks := splitAtResults(t, loadFixture(t))
	proc := newScriptedProcess(scriptOptions{Chunks: chunks})
	a, _ := newTestAdapter(t, proc)

	if got := a.Capabilities().Get(adapter.CapPlan); got != adapter.SupportNo {
		t.Fatalf("CapPlan = %v, want no", got)
	}
	if a.Capabilities().Note(adapter.CapPlan) == "" {
		t.Error("a capability that is off must carry the reason")
	}

	ctx := context.Background()
	s, err := a.Start(ctx, adapter.SessionOptions{ID: "b393be4c-99d7-4d92-ada2-df47ce494ffe", Workspace: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(ctx, adapter.Turn{ID: "t1", Text: "Use the Bash tool to run exactly: echo hello-from-relay-fixture . Then reply DONE."}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(ctx, adapter.Turn{ID: "t2", Text: "Say exactly: SECOND-TURN. Nothing else."}); err != nil {
		t.Fatal(err)
	}
	for _, ev := range drain(t, s) {
		if ev.Kind() == event.KindPlanUpdated {
			t.Fatalf("a plan was emitted for a runtime that has none: %#v", ev)
		}
	}
}

// --- golden rendering ---

type goldenEvent struct {
	Seq        uint64         `json:"seq"`
	Kind       string         `json:"kind"`
	Turn       string         `json:"turn"`
	Ping       string         `json:"ping"`
	Replay     bool           `json:"replay,omitempty"`
	Text       string         `json:"text,omitempty"`
	ToolID     string         `json:"tool_id,omitempty"`
	Tool       string         `json:"tool,omitempty"`
	Target     string         `json:"target,omitempty"`
	RawInput   map[string]any `json:"raw_input,omitempty"`
	Chunk      string         `json:"chunk,omitempty"`
	Status     string         `json:"status,omitempty"`
	OK         *bool          `json:"ok,omitempty"`
	StopReason string         `json:"stop_reason,omitempty"`
	DurationMS int64          `json:"duration_ms,omitempty"`
	Usage      *goldenUsage   `json:"usage,omitempty"`
	Code       string         `json:"code,omitempty"`
	Message    string         `json:"message,omitempty"`
	Ask        string         `json:"ask,omitempty"`
	Prompt     string         `json:"prompt,omitempty"`
	Options    []string       `json:"options,omitempty"`
}

// goldenUsage renders the pointers as strings so "not reported" and "zero" look
// different in the file, which is the distinction ADAPTERS.md §5 insists on.
type goldenUsage struct {
	CostUSD       string `json:"cost_usd"`
	Input         string `json:"input_tokens"`
	Cached        string `json:"cached_input_tokens"`
	Output        string `json:"output_tokens"`
	Total         string `json:"total_tokens"`
	Reasoning     string `json:"reasoning_output_tokens"`
	ContextWindow string `json:"context_window"`
}

func nilOrInt(p *int64) string {
	if p == nil {
		return "nil"
	}
	return strconv.FormatInt(*p, 10)
}

func nilOrFloat(p *float64) string {
	if p == nil {
		return "nil"
	}
	return strconv.FormatFloat(*p, 'g', -1, 64)
}

func renderEvents(events []event.Event) string {
	var b strings.Builder
	for _, ev := range events {
		m := ev.Envelope()
		g := goldenEvent{
			Seq:    m.Seq,
			Kind:   string(ev.Kind()),
			Turn:   m.Turn,
			Ping:   ev.Ping().String(),
			Replay: m.Replay,
		}
		switch e := ev.(type) {
		case event.TextDelta:
			g.Text = e.Text
		case event.Reasoning:
			g.Text = e.Text
		case event.ToolStarted:
			g.ToolID, g.Tool, g.Target, g.RawInput = e.ID, e.Tool, e.Target, e.RawInput
		case event.ToolOutput:
			g.ToolID, g.Chunk, g.Status = e.ID, e.Chunk, string(e.Status)
		case event.TurnCompleted:
			ok := e.OK
			g.OK = &ok
			g.StopReason = string(e.StopReason)
			g.DurationMS = e.Duration.Milliseconds()
			if e.Usage != nil {
				g.Usage = &goldenUsage{
					CostUSD:       nilOrFloat(e.Usage.CostUSD),
					Input:         nilOrInt(e.Usage.InputTokens),
					Cached:        nilOrInt(e.Usage.CachedInputTokens),
					Output:        nilOrInt(e.Usage.OutputTokens),
					Total:         nilOrInt(e.Usage.TotalTokens),
					Reasoning:     nilOrInt(e.Usage.ReasoningOutputTokens),
					ContextWindow: nilOrInt(e.Usage.ContextWindow),
				}
			}
		case event.Error:
			g.Code, g.Message = e.Code, e.Message
		case *event.NeedsInput:
			g.Ask, g.Prompt = string(e.Ask), e.Prompt
			for _, o := range e.Options {
				g.Options = append(g.Options, o.ID+"/"+string(o.Kind))
			}
		}
		line, err := json.Marshal(g)
		if err != nil {
			panic(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
