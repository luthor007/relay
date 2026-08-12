package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/event"
)

// docs/fixtures/adapters/acp.trace.json is a complete ACP session — handshake,
// session/new, four turns, all eight session/update variants, a permission
// request answered, an out-of-contract fs/read_text_file refused, a cancel and
// the redirect that follows it.
//
// It is SCHEMA-DERIVED, NOT RECORDED, because no ACP runtime exists in this
// container. Two tests keep that honest: one validates every message against
// the vendored schema, and one replays the whole file through the adapter and
// asserts the normalized events that come out. The second is the reason the
// fixture is a test of the adapter rather than of the JSON.

type traceRecord struct {
	Dir    string          `json:"dir"`
	Seq    int             `json:"seq"`
	Kind   string          `json:"kind"`
	Method string          `json:"method"`
	Schema *string         `json:"schema"`
	Note   string          `json:"note"`
	Msg    json.RawMessage `json:"msg"`
}

func loadTrace(t *testing.T) []traceRecord {
	t.Helper()
	b, err := os.ReadFile(repoFile(t, "docs/fixtures/adapters/acp.trace.json"))
	if err != nil {
		t.Fatalf("reading acp.trace.json: %v", err)
	}
	var recs []traceRecord
	if err := json.Unmarshal(b, &recs); err != nil {
		t.Fatalf("parsing acp.trace.json: %v", err)
	}
	if len(recs) < 20 {
		t.Fatalf("the trace has only %d records; it is meant to be a complete session", len(recs))
	}
	if recs[0].Dir != "meta" {
		t.Fatal("the first record must be the meta record")
	}
	return recs
}

// TestTraceSaysItIsNotARecording keeps the label attached. A synthetic fixture
// that is honestly labelled is useful; one that pretends to be a recording is
// worse than none.
func TestTraceSaysItIsNotARecording(t *testing.T) {
	recs := loadTrace(t)
	var meta struct {
		Provenance string   `json:"provenance"`
		Warning    string   `json:"warning"`
		Generator  string   `json:"generator"`
		Unverified []string `json:"unverified"`
	}
	if err := json.Unmarshal(recs[0].Msg, &meta); err != nil {
		t.Fatalf("parsing the meta record: %v", err)
	}
	if meta.Provenance != "SCHEMA-DERIVED, NOT RECORDED" {
		t.Errorf("provenance is %q; the fixture must say what it is", meta.Provenance)
	}
	if !strings.Contains(meta.Warning, "NOT a recorded ACP session") {
		t.Error("the warning no longer says the trace is not a recording")
	}
	if meta.Generator != "relayd/testdata/acp/gen_trace.py" {
		t.Errorf("generator is %q", meta.Generator)
	}
	if len(meta.Unverified) < 4 {
		t.Errorf("the meta record lists %d unverified claims; it had six, and shrinking that list is a claim about a runtime nobody has run", len(meta.Unverified))
	}
}

// TestTraceValidatesAgainstTheVendoredSchema is the fixture's claim about
// itself, checked. A re-vendored schema with a renamed field turns this red.
func TestTraceValidatesAgainstTheVendoredSchema(t *testing.T) {
	s := loadACPSchema(t)
	recs := loadTrace(t)

	checked := 0
	for _, r := range recs {
		if r.Dir == "meta" {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal(r.Msg, &msg); err != nil {
			t.Fatalf("seq %d: %v", r.Seq, err)
		}
		if msg["jsonrpc"] != "2.0" {
			t.Errorf("seq %d: ACP is stock JSON-RPC 2.0 and every message must say so", r.Seq)
		}
		if r.Schema == nil {
			// JSON-RPC error objects are the only thing with no definition
			// anywhere in the ACP schema.
			if r.Kind != "error" {
				t.Errorf("seq %d: only JSON-RPC errors may carry schema:null, got kind %q", r.Seq, r.Kind)
			}
			continue
		}
		name := (*r.Schema)[strings.LastIndex(*r.Schema, "/")+1:]
		def, ok := s.defs[name]
		if !ok {
			t.Errorf("seq %d: %s is not in the vendored schema", r.Seq, *r.Schema)
			continue
		}
		var payload any
		switch r.Kind {
		case "request", "notification":
			payload = msg["params"]
		case "response":
			payload = msg["result"]
		default:
			t.Errorf("seq %d: kind %q should not name a schema", r.Seq, r.Kind)
			continue
		}
		if problems := s.validate(fmt.Sprintf("seq %d", r.Seq), def, payload); len(problems) > 0 {
			t.Errorf("seq %d does not satisfy %s:\n  %s", r.Seq, *r.Schema, strings.Join(problems, "\n  "))
		}
		checked++
	}
	if checked < 30 {
		t.Errorf("only %d messages were validated; the trace is meant to cover a whole session", checked)
	}
}

// TestTraceMethodsAreAllInTheSurface guards against the fixture drifting into
// a method the adapter does not implement.
func TestTraceMethodsAreAllInTheSurface(t *testing.T) {
	known := map[string]bool{}
	for _, m := range append(AgentMethods(), ClientMethods()...) {
		known[m] = true
	}
	for _, r := range loadTrace(t) {
		if r.Method != "" && !known[r.Method] {
			t.Errorf("seq %d names %q, which is not one of the seventeen ACP methods", r.Seq, r.Method)
		}
	}
}

// ---- replay ----

type replayer struct {
	t     *testing.T
	f     *fakeAgent
	recs  []traceRecord
	idmap map[string]json.RawMessage

	mu   sync.Mutex
	err  error
	done chan struct{}
}

func newReplayer(t *testing.T, f *fakeAgent, recs []traceRecord) *replayer {
	return &replayer{t: t, f: f, recs: recs, idmap: map[string]json.RawMessage{}, done: make(chan struct{})}
}

// fail records the first mismatch and tears the agent down, so the client side
// fails with a real error instead of hanging. t.Fatal cannot be used here: this
// runs on a goroutine that is not the test's.
func (r *replayer) fail(format string, a ...any) {
	r.mu.Lock()
	if r.err == nil {
		r.err = fmt.Errorf(format, a...)
	}
	r.mu.Unlock()
	r.f.close()
}

func (r *replayer) result() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *replayer) run() {
	defer close(r.done)
	i := 1 // record 0 is meta
	for i < len(r.recs) {
		if r.recs[i].Dir == "recv" {
			if !r.emit(r.recs[i]) {
				return
			}
			i++
			continue
		}
		j := i
		for j < len(r.recs) && r.recs[j].Dir == "send" {
			j++
		}
		if !r.matchGroup(r.recs[i:j]) {
			return
		}
		i = j
	}
}

// emit writes one agent→client message, substituting the real request id on a
// response because the client numbers its own requests.
func (r *replayer) emit(rec traceRecord) bool {
	var msg map[string]any
	if err := json.Unmarshal(rec.Msg, &msg); err != nil {
		r.fail("seq %d: %v", rec.Seq, err)
		return false
	}
	if rec.Kind == "response" || rec.Kind == "error" {
		key := idKey(msg["id"])
		actual, ok := r.idmap[key]
		if !ok {
			r.fail("seq %d: responding to trace id %s, which the client never sent", rec.Seq, key)
			return false
		}
		var v any
		if err := json.Unmarshal(actual, &v); err != nil {
			r.fail("seq %d: %v", rec.Seq, err)
			return false
		}
		msg["id"] = v
	}
	b, err := json.Marshal(msg)
	if err != nil {
		r.fail("seq %d: %v", rec.Seq, err)
		return false
	}
	r.f.writeRaw(b)
	return true
}

// matchGroup consumes one run of consecutive client→agent messages.
//
// Order inside a run is deliberately not asserted. Answering an outstanding
// permission request with `cancelled`, sending session/cancel, and refusing an
// fs/read_text_file are three independent obligations that all fall due at the
// same moment; the protocol fixes that they all happen, not the order they
// happen in.
func (r *replayer) matchGroup(group []traceRecord) bool {
	matched := make([]bool, len(group))
	for k := 0; k < len(group); k++ {
		m, ok := r.f.tryNext(10 * time.Second)
		if !ok {
			var want []string
			for i, g := range group {
				if !matched[i] {
					want = append(want, fmt.Sprintf("seq %d %s %s", g.Seq, g.Kind, g.Method))
				}
			}
			r.fail("timed out waiting for the client; still expecting %v", want)
			return false
		}
		idx := r.findMatch(group, matched, m)
		if idx < 0 {
			r.fail("unexpected client message: kind=%v method=%q id=%s", m.kind(), m.Method, string(m.ID))
			return false
		}
		matched[idx] = true
		if !r.compare(group[idx], m) {
			return false
		}
	}
	return true
}

func (r *replayer) findMatch(group []traceRecord, matched []bool, m *message) int {
	for i, g := range group {
		if matched[i] {
			continue
		}
		switch m.kind() {
		case kindRequest, kindNotification:
			if (g.Kind == "request" || g.Kind == "notification") && g.Method == m.Method {
				return i
			}
		case kindResponse, kindError:
			var want map[string]any
			if err := json.Unmarshal(g.Msg, &want); err != nil {
				continue
			}
			if (g.Kind == "response" || g.Kind == "error") && idKey(want["id"]) == string(m.ID) {
				return i
			}
		}
	}
	return -1
}

func (r *replayer) compare(g traceRecord, m *message) bool {
	var want map[string]any
	if err := json.Unmarshal(g.Msg, &want); err != nil {
		r.fail("seq %d: %v", g.Seq, err)
		return false
	}
	switch g.Kind {
	case "request":
		r.idmap[idKey(want["id"])] = append(json.RawMessage(nil), m.ID...)
		return r.equal(g, "params", want["params"], m.Params)
	case "notification":
		return r.equal(g, "params", want["params"], m.Params)
	case "response":
		return r.equal(g, "result", want["result"], m.Result)
	case "error":
		got, err := json.Marshal(m.Error)
		if err != nil {
			r.fail("seq %d: %v", g.Seq, err)
			return false
		}
		return r.equal(g, "error", want["error"], got)
	}
	r.fail("seq %d: unknown kind %q", g.Seq, g.Kind)
	return false
}

func (r *replayer) equal(g traceRecord, field string, want any, gotRaw json.RawMessage) bool {
	var got any
	if len(gotRaw) > 0 {
		if err := json.Unmarshal(gotRaw, &got); err != nil {
			r.fail("seq %d: decoding %s: %v", g.Seq, field, err)
			return false
		}
	}
	if !reflect.DeepEqual(want, got) {
		wb, _ := json.Marshal(want)
		r.fail("seq %d (%s %s): %s mismatch\n want %s\n  got %s", g.Seq, g.Kind, g.Method, field, wb, string(gotRaw))
		return false
	}
	return true
}

func idKey(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestReplayTraceThroughTheAdapter drives the whole fixture through the real
// adapter, once per runtime, and asserts the normalized events.
func TestReplayTraceThroughTheAdapter(t *testing.T) {
	for _, rt := range Runtimes() {
		t.Run(string(rt), func(t *testing.T) { replayOnce(t, rt) })
	}
}

func replayOnce(t *testing.T, rt adapter.Runtime) {
	ctx := context.Background()
	recs := loadTrace(t)
	f := newFakeAgent(t)
	r := newReplayer(t, f, recs)
	go r.run()

	opts := testOptions(t, rt)
	opts.CancelGrace = 10 * time.Second
	a, err := Attach(ctx, f.agentOutR, f.agentInW, f.closer, opts)
	if err != nil {
		t.Fatalf("Attach: %v (replay: %v)", err, r.result())
	}
	defer func() { _ = a.Close(ctx) }()

	sess, err := a.Start(ctx, adapter.SessionOptions{
		ID:        "relay-session-1",
		Workspace: "/Users/USER/src/relay",
		MCPServers: []adapter.MCPServer{{
			Name:    "relay-registry",
			Command: "/usr/local/bin/relay",
			Args:    []string{"mcp", "serve"},
			Env:     []string{"RELAY_SCOPE=session"},
		}},
	})
	if err != nil {
		t.Fatalf("Start: %v (replay: %v)", err, r.result())
	}
	s := sess.(*Session)
	c := collect(s)

	// ---- turn 1: a plan, two tool calls, one approval, end_turn ----
	turn1, err := s.Send(ctx, adapter.Turn{Text: "Get the flaky auth test passing, then run the suite."})
	if err != nil {
		t.Fatalf("Send: %v (replay: %v)", err, r.result())
	}

	n1 := c.needsInput(t)
	if n1.Prompt != "Run npm test -- auth" {
		t.Errorf("permission prompt = %q", n1.Prompt)
	}
	if n1.Ping() != event.PingBlocking {
		t.Error("a live NeedsInput must ping blocking")
	}
	if len(n1.Options) != 3 {
		t.Fatalf("got %d options, want the three the agent offered", len(n1.Options))
	}
	if !n1.Options[1].Kind.Standing() {
		t.Error("the second option is allow_always and must read as a standing grant")
	}
	if err := n1.Reply(ctx, event.Reply{OptionID: "o1"}); err != nil {
		t.Fatalf("Reply: %v (replay: %v)", err, r.result())
	}

	waitCompletions(t, c, 1)

	// ---- turn 2: queued addition, then a redirect ----
	d2, err := s.Deliver(ctx, adapter.Turn{Text: "Now migrate the whole auth module to the new session store."}, ModeAuto)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if d2.Disposition != DispositionStarted {
		t.Errorf("an idle session should start a turn, got %q", d2.Disposition)
	}

	add, err := s.Deliver(ctx, adapter.Turn{Text: "Also update the changelog."}, ModeQueue)
	if err != nil {
		t.Fatalf("Deliver(queue): %v", err)
	}
	if add.Disposition != DispositionQueued {
		t.Errorf("an addition while a turn runs must queue, got %q", add.Disposition)
	}

	n2 := c.needsInput(t)
	// toolCall is a ToolCallUpdate: only toolCallId is guaranteed, so there is
	// nothing human-readable to read aloud and the adapter must say so.
	if !strings.Contains(n2.Prompt, "did not say what it does") {
		t.Errorf("with no title the prompt must admit it, got %q", n2.Prompt)
	}
	if n2.Tool == nil || n2.Tool.ID != "call_3" || n2.Tool.Title != "" {
		t.Errorf("tool ref = %+v", n2.Tool)
	}

	red, err := s.Deliver(ctx, adapter.Turn{
		Text: "Forget the migration. Just fix the session store import in src/auth/session.ts.",
	}, ModeRedirect)
	if err != nil {
		t.Fatalf("Deliver(redirect): %v (replay: %v)", err, r.result())
	}
	if red.Disposition != DispositionRedirect {
		t.Errorf("disposition = %q, want redirect", red.Disposition)
	}
	if red.CancelledTurn != d2.TurnID {
		t.Errorf("redirect cancelled %q, want %q", red.CancelledTurn, d2.TurnID)
	}
	if !strings.Contains(red.CancelledText, "migrate the whole auth module") {
		t.Errorf("a redirect must report what it displaced, got %q", red.CancelledText)
	}

	// The outstanding question died with the turn; a ping that outlives its
	// question wakes someone to approve what nobody is waiting for.
	if !n2.Answered() {
		t.Error("the outstanding permission request must be resolved by the cancel")
	}
	if err := n2.Reply(ctx, event.Reply{OptionID: "p1"}); !isWithdrawn(err) {
		t.Errorf("replying to a withdrawn question returned %v", err)
	}

	// ---- everything settles: cancelled, the redirect, the queued addition ----
	waitCompletions(t, c, 4)

	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the replay did not finish")
	}
	if err := r.result(); err != nil {
		t.Fatalf("replay: %v", err)
	}

	// ---- the normalized stream ----
	comps := c.completions()
	gotStops := []event.StopReason{}
	for _, tc := range comps {
		gotStops = append(gotStops, tc.StopReason)
		if tc.Usage != nil {
			t.Errorf("turn %s carries a Usage; ACP 0.4.5 has no cost, token or usage field anywhere", tc.Turn)
		}
	}
	wantStops := []event.StopReason{event.StopEndTurn, event.StopCancelled, event.StopEndTurn, event.StopEndTurn}
	if !reflect.DeepEqual(gotStops, wantStops) {
		t.Errorf("stop reasons = %v, want %v", gotStops, wantStops)
	}
	if comps[0].Turn != turn1 || !comps[0].OK {
		t.Errorf("first completion = %+v", comps[0])
	}
	if comps[1].Turn != d2.TurnID || comps[1].OK {
		t.Errorf("the cancelled turn = %+v", comps[1])
	}
	if comps[1].StopReason.Retryable() != true {
		t.Error("a cancelled turn is retryable: it feeds the re-prompt")
	}
	if comps[2].Turn != red.TurnID {
		t.Errorf("the redirect turn = %q, want %q", comps[2].Turn, red.TurnID)
	}
	if comps[3].Turn != add.TurnID {
		t.Errorf("the queued addition was lost: fourth completion is %q, want %q", comps[3].Turn, add.TurnID)
	}

	// plan → PlanUpdated, native rather than synthesized.
	var plans []event.PlanUpdated
	for _, e := range c.all() {
		if p, ok := e.(event.PlanUpdated); ok {
			plans = append(plans, p)
		}
	}
	if len(plans) != 2 {
		t.Fatalf("got %d PlanUpdated, want 2", len(plans))
	}
	if plans[0].Synthesized || plans[1].Synthesized {
		t.Error("ACP plans are native; nothing here is inferred from tool activity")
	}
	if len(plans[0].Steps) != 3 || plans[0].Steps[0].Status != event.PlanInProgress {
		t.Errorf("first plan = %+v", plans[0].Steps)
	}
	if plans[1].Steps[2].Status != event.PlanInProgress || plans[1].Steps[0].Status != event.PlanCompleted {
		t.Errorf("second plan = %+v", plans[1].Steps)
	}

	// tool_call → ToolStarted, tool_call_update merged onto it.
	var started []event.ToolStarted
	var outputs []event.ToolOutput
	for _, e := range c.all() {
		switch v := e.(type) {
		case event.ToolStarted:
			started = append(started, v)
		case event.ToolOutput:
			outputs = append(outputs, v)
		}
	}
	if len(started) != 2 {
		t.Fatalf("got %d ToolStarted, want 2", len(started))
	}
	if started[0].ID != "call_1" || started[0].Tool != "execute" || started[0].Target != "Run npm test -- auth" {
		t.Errorf("first ToolStarted = %+v", started[0])
	}
	if started[0].RawInput["command"] != "npm test -- auth" {
		t.Errorf("rawInput = %v", started[0].RawInput)
	}
	if started[1].Tool != "edit" {
		t.Errorf("second ToolStarted = %+v", started[1])
	}
	if !hasOutput(outputs, "call_1", "1 failing: auth token expiry", event.ToolCompleted) {
		t.Errorf("call_1 outputs = %+v", outputs)
	}
	if !hasOutputContaining(outputs, "call_2", "diff src/auth/token.test.ts") {
		t.Errorf("a diff must be named rather than expanded; call_2 outputs = %+v", outputs)
	}

	// Reasoning is present and never spoken.
	sawThought := false
	for _, e := range c.all() {
		if rz, ok := e.(event.Reasoning); ok {
			sawThought = true
			if rz.Ping() != event.PingNone {
				t.Error("Reasoning must never ping")
			}
		}
	}
	if !sawThought {
		t.Error("agent_thought_chunk should have produced a Reasoning event")
	}

	if txt := c.text(); !strings.Contains(txt, "Reproducing the failure first.") ||
		!strings.Contains(txt, "Stopping; the store swap was half applied.") {
		t.Errorf("TextDelta stream lost content: %q", txt)
	}

	// The out-of-contract fs/read_text_file was refused, not faked.
	if got := a.Refused()[methodFSReadTextFile]; got != 1 {
		t.Errorf("fs/read_text_file refusals = %d, want 1", got)
	}
	if len(a.Extensions()) != 0 {
		t.Errorf("no `_`-prefixed extension appears in the trace, but Extensions() has %v", a.Extensions())
	}

	// available_commands_update and current_mode_update are surfaced, not swallowed.
	if len(s.Commands()) != 2 {
		t.Errorf("Commands() = %v", s.Commands())
	}
	if s.CurrentMode() != "code" {
		t.Errorf("CurrentMode() = %q, want code — the agent changed its own mode mid-session", s.CurrentMode())
	}

	// Capabilities narrowed by what was actually observed.
	caps := s.Capabilities()
	if caps.Get(adapter.CapReasoning) != adapter.SupportYes {
		t.Error("observing agent_thought_chunk should move CapReasoning to yes")
	}
	if caps.Get(adapter.CapResume) != adapter.SupportYes {
		t.Error("this trace advertises loadSession true, so CapResume must be yes")
	}
	if caps.Get(adapter.CapSteer) != adapter.SupportNo {
		t.Error("CapSteer is verified absent on ACP and must stay no")
	}
	if caps.Get(adapter.CapTokens) != adapter.SupportNo || caps.Get(adapter.CapContextWindow) != adapter.SupportNo {
		t.Error("ACP carries no tokens and no context window")
	}
	wantCost := CostPlanFor(rt).Support
	if got := caps.Get(adapter.CapCostUSD); got != wantCost {
		t.Errorf("cost support for %s = %v, want %v — the descriptor is per runtime, not per adapter", rt, got, wantCost)
	}
	if caps.Note(adapter.CapCostUSD) == "" {
		t.Error("a missing capability must carry the reason it is missing")
	}
}

func waitCompletions(t *testing.T, c *collector, n int) {
	t.Helper()
	c.waitFor(t, fmt.Sprintf("%d turn completions", n), func(e event.Event) bool {
		return len(c.completions()) >= n
	})
}

func hasOutput(list []event.ToolOutput, id, chunk string, status event.ToolStatus) bool {
	for _, o := range list {
		if o.ID == id && o.Chunk == chunk && o.Status == status {
			return true
		}
	}
	return false
}

func hasOutputContaining(list []event.ToolOutput, id, substr string) bool {
	for _, o := range list {
		if o.ID == id && strings.Contains(o.Chunk, substr) {
			return true
		}
	}
	return false
}

func isWithdrawn(err error) bool { return errors.Is(err, event.ErrWithdrawn) }
