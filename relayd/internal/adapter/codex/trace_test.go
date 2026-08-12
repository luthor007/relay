package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/logx"
)

// traceRecord is one line of docs/fixtures/adapters/codex.trace.json.
type traceRecord struct {
	Dir    string          `json:"dir"`
	Seq    int             `json:"seq"`
	Kind   string          `json:"kind"`
	Method string          `json:"method"`
	Schema *string         `json:"schema"`
	Note   string          `json:"note"`
	Msg    json.RawMessage `json:"msg"`

	raw json.RawMessage
}

func loadTrace(t *testing.T) []traceRecord {
	t.Helper()
	b, err := os.ReadFile(repoFile(t, "docs/fixtures/adapters/codex.trace.json"))
	if err != nil {
		t.Fatalf("reading the trace: %v", err)
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(b, &raws); err != nil {
		t.Fatalf("parsing the trace: %v", err)
	}
	recs := make([]traceRecord, 0, len(raws))
	for i, r := range raws {
		var rec traceRecord
		if err := json.Unmarshal(r, &rec); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		rec.raw = r
		recs = append(recs, rec)
	}
	return recs
}

// traceServer replays the fixture as if it were `codex app-server`.
//
// It is a strict script, not a stub: every client message must arrive in the
// order the trace says, with exactly the params the trace says, and every
// server→client request must come back with the id it went out with. So the
// fixture is a two-sided contract — it pins what the adapter sends as tightly
// as what it must understand.
type traceServer struct {
	recs []traceRecord

	clientRead *io.PipeReader
	toClient   *io.PipeWriter
	clientTo   *io.PipeWriter
	serverRead *io.PipeReader

	inbox chan *message

	mu    sync.Mutex
	ids   map[string]json.RawMessage // trace id → the id the adapter actually used
	fail  error
	stage string

	done chan struct{}
}

func newTraceServer(t *testing.T, recs []traceRecord) *traceServer {
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	s := &traceServer{
		recs:       recs,
		clientRead: cr,
		toClient:   sw,
		clientTo:   cw,
		serverRead: sr,
		inbox:      make(chan *message, 64),
		ids:        map[string]json.RawMessage{},
		done:       make(chan struct{}),
	}
	go func() {
		br := bufio.NewReaderSize(s.serverRead, 1<<16)
		for {
			m, err := decode(ndjson{}, br)
			if err != nil {
				close(s.inbox)
				return
			}
			s.inbox <- m
		}
	}()
	go s.play()
	t.Cleanup(func() {
		_ = s.clientTo.Close()
		_ = s.serverRead.Close()
		_ = s.toClient.Close()
	})
	return s
}

func (s *traceServer) play() {
	defer close(s.done)
	for _, r := range s.recs {
		if r.Dir == "meta" {
			continue
		}
		s.mu.Lock()
		s.stage = fmt.Sprintf("seq %d %s %s %s", r.Seq, r.Dir, r.Kind, r.Method)
		s.mu.Unlock()

		switch {
		case r.Dir == "recv":
			if err := s.send(r); err != nil {
				s.stop(err)
				return
			}
		case r.Dir == "send":
			if err := s.expect(r); err != nil {
				s.stop(err)
				return
			}
		}
	}
}

// send writes a server→client message, rewriting a response's id to the one the
// adapter actually used for that call.
func (s *traceServer) send(r traceRecord) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(r.Msg, &m); err != nil {
		return err
	}
	if r.Kind == "response" || r.Kind == "error" {
		s.mu.Lock()
		actual, ok := s.ids[string(m["id"])]
		s.mu.Unlock()
		if !ok {
			return fmt.Errorf("seq %d: a response for trace id %s, which no client request claimed", r.Seq, m["id"])
		}
		m["id"] = actual
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if _, err := s.toClient.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

// expect blocks until the adapter sends the message the trace says it should.
func (s *traceServer) expect(r traceRecord) error {
	var want map[string]json.RawMessage
	if err := json.Unmarshal(r.Msg, &want); err != nil {
		return err
	}

	var got *message
	select {
	case m, ok := <-s.inbox:
		if !ok {
			return fmt.Errorf("seq %d: expected %s %s, the client closed the connection", r.Seq, r.Kind, r.Method)
		}
		got = m
	case <-time.After(10 * time.Second):
		return fmt.Errorf("seq %d: timed out waiting for %s %s", r.Seq, r.Kind, r.Method)
	}

	switch r.Kind {
	case "request":
		if got.Method != r.Method {
			return fmt.Errorf("seq %d: expected %s, the client sent %s", r.Seq, r.Method, got.Method)
		}
		if err := sameJSON(want["params"], got.Params); err != nil {
			return fmt.Errorf("seq %d: %s params: %w", r.Seq, r.Method, err)
		}
		s.mu.Lock()
		s.ids[string(want["id"])] = got.ID
		s.mu.Unlock()
	case "response":
		if got.Method != "" {
			return fmt.Errorf("seq %d: expected a response to %s, the client sent a %s", r.Seq, want["id"], got.Method)
		}
		if requestKey(got.ID) != requestKey(want["id"]) {
			return fmt.Errorf("seq %d: response id is %s, the server asked with %s", r.Seq, got.ID, want["id"])
		}
		if err := sameJSON(want["result"], got.Result); err != nil {
			return fmt.Errorf("seq %d: response result: %w", r.Seq, err)
		}
	}
	return nil
}

func (s *traceServer) stop(err error) {
	s.mu.Lock()
	if s.fail == nil {
		s.fail = err
	}
	s.mu.Unlock()
	_ = s.toClient.Close()
}

func (s *traceServer) err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fail
}

func (s *traceServer) where() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stage
}

func sameJSON(want, got json.RawMessage) error {
	var a, b any
	if len(want) == 0 {
		want = json.RawMessage("null")
	}
	if len(got) == 0 {
		got = json.RawMessage("null")
	}
	if err := json.Unmarshal(want, &a); err != nil {
		return err
	}
	if err := json.Unmarshal(got, &b); err != nil {
		return err
	}
	if !reflect.DeepEqual(a, b) {
		return fmt.Errorf("\n want %s\n  got %s", want, got)
	}
	return nil
}

// TestReplayTheRecordedSession drives the real adapter, over the real codec,
// through the whole fixture, and asserts the normalized event stream.
func TestReplayTheRecordedSession(t *testing.T) {
	recs := loadTrace(t)
	srv := newTraceServer(t, recs)

	a, err := Attach(context.Background(), srv.clientRead, srv.clientTo, func() error { return nil },
		Options{Log: logx.Discard(), Clock: fixedClock(), DrainGrace: time.Second})
	if err != nil {
		t.Fatalf("initialize: %v (server was at %s: %v)", err, srv.where(), srv.err())
	}

	ctx := context.Background()
	sess, err := a.Start(ctx, adapter.SessionOptions{
		ID:             "relay-session-1",
		Workspace:      "/Users/USER/src/relay",
		Model:          "gpt-5-codex",
		PermissionMode: "on-request",
	})
	if err != nil {
		t.Fatalf("thread/start: %v (server at %s: %v)", err, srv.where(), srv.err())
	}
	s := sess.(*Session)

	if got, want := s.Native(), "01998e4a-7a1f-7c3a-9c22-6d3b1f0e5a10"; got != want {
		t.Errorf("thread id came out as %q; it must come from thread/started, not from a result", got)
	}

	// Answering the approval is what unblocks app-server, so it has to happen
	// while the stream is still running — exactly as it would in the product.
	var (
		mu       sync.Mutex
		got      []string
		answered = make(chan struct{})
	)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		once := sync.Once{}
		for e := range s.Events() {
			mu.Lock()
			got = append(got, describe(e))
			mu.Unlock()
			if q, ok := e.(*event.NeedsInput); ok {
				if err := q.Reply(context.Background(), event.Reply{OptionID: "accept"}); err != nil {
					t.Errorf("replying to the approval: %v", err)
				}
				once.Do(func() { close(answered) })
			}
		}
	}()

	turn1, err := s.Send(ctx, adapter.Turn{
		ID:   "relay-turn-1",
		Text: "check whether the tests pass, then tell me what broke",
	})
	if err != nil {
		t.Fatalf("turn/start: %v (server at %s: %v)", err, srv.where(), srv.err())
	}
	if turn1 != "turn_01HZX8QK4T7N9G" {
		t.Errorf("turn id = %q, want the one from turn/started", turn1)
	}

	select {
	case <-answered:
	case <-time.After(10 * time.Second):
		t.Fatalf("the approval was never raised (server at %s: %v)", srv.where(), srv.err())
	}

	turn2, err := s.Send(ctx, adapter.Turn{ID: "relay-turn-2", Text: "fix it"})
	if err != nil {
		t.Fatalf("second turn/start: %v (server at %s: %v)", err, srv.where(), srv.err())
	}

	// The steer has to land while turn 2 is still the active turn; that is the
	// precondition, and the trace is written so it is.
	if err := s.Steer(ctx, turn2, adapter.Turn{
		ID:   "relay-steer-1",
		Text: "no — keep the assertion, fix the migration instead",
	}); err != nil {
		t.Fatalf("turn/steer: %v (server at %s: %v)", err, srv.where(), srv.err())
	}

	waitFor(t, func() bool { return s.ActiveTurn() == "" }, "turn 2 to complete")

	if err := s.Close(ctx); err != nil {
		t.Fatalf("close: %v (server at %s: %v)", err, srv.where(), srv.err())
	}
	<-drained

	select {
	case <-srv.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("the server never finished the script; it stopped at %s", srv.where())
	}
	if err := srv.err(); err != nil {
		t.Fatalf("the adapter departed from the trace at %s: %v", srv.where(), err)
	}

	want := []string{
		// turn one
		"turn_started turn_01HZX8QK4T7N9G",
		`reasoning_summary "Running the test suite"`,
		`reasoning_summary "\n\n"`, // item/reasoning/summaryPartAdded, part 1
		`reasoning_summary "then reading whatever fails"`,
		`reasoning "The user wants the failing tests, so run go test first."`,
		"plan [in_progress run the test suite] [pending read the failures] [pending report back]",
		`tool_started item_c1 command "go test ./..."`,
		`needs_input permission "Codex wants to run: go test ./... — the command runs outside the sandbox's write roots"`,
		`tool_output item_c1  "ok  \tgithub.com/luthor007/relay/relayd/internal/event\t0.004s\n"`,
		`tool_output item_c1  "FAIL\tgithub.com/luthor007/relay/relayd/internal/store\t0.212s\n"`,
		`tool_output item_c1 failed ""`,
		`text "One package fails: "`,
		`text "internal/store, "`,
		`text "TestVaultIsNeverIndexed."`,
		"turn_completed turn_01HZX8QK4T7N9G end_turn tokens=8622",
		// turn two
		"turn_started turn_01HZX8R2M6B4KD",
		`text "I'll relax the assertion so the vault table "`,
		"plan [completed keep TestVaultIsNeverIndexed as written] [in_progress drop the FTS5 trigger from the vault migration]",
		`tool_started item_f1 file_change "/Users/USER/src/relay/relayd/internal/store/migrations/0002_vault.sql"`,
		`tool_output item_f1  "update /Users/USER/src/relay/relayd/internal/store/migrations/0002_vault.sql\n@@\n-CREATE VIRTUAL TABLE vault_fts USING fts5(secret);\n"`,
		`tool_output item_f1 completed ""`,
		`text "Dropped the FTS5 table from the vault migration; the assertion stands."`,
		"turn_completed turn_01HZX8R2M6B4KD end_turn tokens=9408",
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d:\n got: %s\nwant: %s",
			len(got), len(want), strings.Join(got, "\n      "), strings.Join(want, "\n      "))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d:\n got %s\nwant %s", i, got[i], want[i])
		}
	}
}

// TestReplayReportsNoCostBecauseCodexReportsNone. The turn boundary carries
// tokens and a nil CostUSD, and the console has to show a gap rather than a
// free turn.
func TestUsageFromTheTraceHasTokensAndNoMoney(t *testing.T) {
	n := newNormalizer(func(event.Event) {}, &metaSource{now: fixedClock()}, logx.Discard(), hooks{})
	n.handle("thread/tokenUsage/updated", mustJSON(t, map[string]any{
		"threadId": "t", "turnId": "turn-1",
		"tokenUsage": map[string]any{
			"last":  map[string]any{"inputTokens": 10, "cachedInputTokens": 4, "outputTokens": 2, "reasoningOutputTokens": 1, "totalTokens": 12},
			"total": map[string]any{"inputTokens": 700, "cachedInputTokens": 300, "outputTokens": 20, "reasoningOutputTokens": 5, "totalTokens": 720},
			// modelContextWindow is nullable even when the counts are present.
			"modelContextWindow": 1000,
		},
	}))
	u := n.usage()
	if u == nil {
		t.Fatal("no usage recorded")
	}
	if u.CostUSD != nil {
		t.Error("Codex carries no dollar figure anywhere in its contract; CostUSD must stay nil")
	}
	p, ok := u.ContextPressure()
	if !ok {
		t.Fatal("context pressure could not be computed")
	}
	if p != 1.0 {
		t.Errorf("context pressure = %v, want (700+300)/1000", p)
	}
}

func TestNullContextWindowLeavesPressureUncomputable(t *testing.T) {
	n := newNormalizer(func(event.Event) {}, &metaSource{now: fixedClock()}, logx.Discard(), hooks{})
	n.handle("thread/tokenUsage/updated", mustJSON(t, map[string]any{
		"threadId": "t", "turnId": "turn-1",
		"tokenUsage": map[string]any{
			"last":               map[string]any{"inputTokens": 1, "cachedInputTokens": 0, "outputTokens": 0, "reasoningOutputTokens": 0, "totalTokens": 1},
			"total":              map[string]any{"inputTokens": 1, "cachedInputTokens": 0, "outputTokens": 0, "reasoningOutputTokens": 0, "totalTokens": 1},
			"modelContextWindow": nil,
		},
	}))
	u := n.usage()
	if u.ContextWindow != nil {
		t.Error("a null modelContextWindow must stay nil, not become zero")
	}
	if _, ok := u.ContextPressure(); ok {
		t.Error("MEMORY.md §9 needs a fallback denominator; the adapter must not invent one")
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
