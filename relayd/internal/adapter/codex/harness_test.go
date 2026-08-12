package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/logx"
)

// fakeServer is a `codex app-server` that does what the test says.
//
// Both directions are pumped by goroutines with unbounded queues, so neither
// side can wedge the other: the adapter's reader goroutine answers some server
// requests inline (a refusal, a decode failure) and a synchronous pipe would
// deadlock the moment it did.
type fakeServer struct {
	t *testing.T

	toClient   *io.PipeWriter // server → client
	clientRead *io.PipeReader
	clientTo   *io.PipeWriter // client → server
	serverRead *io.PipeReader

	inbox chan *message // what the client sent
	out   chan []byte   // what to send the client

	closed chan struct{}
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	cr, sw := io.Pipe() // server writes, client reads
	sr, cw := io.Pipe() // client writes, server reads

	f := &fakeServer{
		t:          t,
		toClient:   sw,
		clientRead: cr,
		clientTo:   cw,
		serverRead: sr,
		inbox:      make(chan *message, 256),
		out:        make(chan []byte, 256),
		closed:     make(chan struct{}),
	}

	go func() {
		br := bufio.NewReaderSize(f.serverRead, 1<<16)
		for {
			m, err := decode(ndjson{}, br)
			if err != nil {
				close(f.inbox)
				return
			}
			f.inbox <- m
		}
	}()
	go func() {
		for b := range f.out {
			if _, err := f.toClient.Write(append(b, '\n')); err != nil {
				return
			}
		}
		_ = f.toClient.Close()
	}()

	t.Cleanup(f.stop)
	return f
}

func (f *fakeServer) stop() {
	select {
	case <-f.closed:
		return
	default:
	}
	close(f.closed)
	close(f.out)
	_ = f.clientTo.Close()
	_ = f.serverRead.Close()
}

// attach wires an Adapter to this server, answering the mandatory handshake.
func (f *fakeServer) attach(t *testing.T, opts Options) *Adapter {
	t.Helper()
	if opts.Log == nil {
		opts.Log = logx.Discard()
	}
	if opts.Clock == nil {
		opts.Clock = fixedClock()
	}
	opts.DrainGrace = 200 * time.Millisecond
	opts.StartTimeout = 5 * time.Second

	done := make(chan *Adapter, 1)
	errc := make(chan error, 1)
	go func() {
		a, err := Attach(context.Background(), f.clientRead, f.clientTo, func() error { return nil }, opts)
		if err != nil {
			errc <- err
			return
		}
		done <- a
	}()

	init := f.expect(t, "initialize")
	f.reply(init.ID, map[string]any{})

	select {
	case a := <-done:
		t.Cleanup(func() { _ = a.Close(context.Background()) })
		return a
	case err := <-errc:
		t.Fatalf("attach: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("attach: timed out")
	}
	return nil
}

// open answers thread/start with a thread/started notification, which is the
// only place a thread id is contractually available.
func (f *fakeServer) open(t *testing.T, a *Adapter, opts adapter.SessionOptions, threadID string) *Session {
	t.Helper()
	type res struct {
		s   adapter.Session
		err error
	}
	ch := make(chan res, 1)
	go func() {
		s, err := a.Start(context.Background(), opts)
		ch <- res{s, err}
	}()

	start := f.expect(t, "thread/start")
	f.notify("thread/started", map[string]any{"thread": minimalThread(threadID)})
	f.reply(start.ID, map[string]any{})

	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("thread/start: %v", r.err)
		}
		return r.s.(*Session)
	case <-time.After(5 * time.Second):
		t.Fatal("thread/start: timed out")
	}
	return nil
}

func minimalThread(id string) map[string]any {
	return map[string]any{
		"id": id, "sessionId": id, "cwd": "/w", "createdAt": 1, "updatedAt": 1,
		"preview": "", "status": map[string]any{"type": "idle"}, "turns": []any{},
		"source": "appServer", "modelProvider": "openai", "cliVersion": "0.140.0",
		"ephemeral": false,
	}
}

// expect waits for the next client message and asserts its method.
func (f *fakeServer) expect(t *testing.T, method string) *message {
	t.Helper()
	select {
	case m, ok := <-f.inbox:
		if !ok {
			t.Fatalf("expected %s, connection closed", method)
		}
		if m.Method != method {
			t.Fatalf("expected %s, got %s", method, m.Method)
		}
		return m
	case <-time.After(5 * time.Second):
		t.Fatalf("expected %s, timed out", method)
	}
	return nil
}

func (f *fakeServer) next(t *testing.T) *message {
	t.Helper()
	select {
	case m, ok := <-f.inbox:
		if !ok {
			t.Fatal("connection closed")
		}
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a client message")
	}
	return nil
}

func (f *fakeServer) write(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		f.t.Fatalf("marshal: %v", err)
	}
	select {
	case <-f.closed:
	case f.out <- b:
	}
}

func (f *fakeServer) reply(id json.RawMessage, result any) {
	f.write(map[string]any{"id": json.RawMessage(id), "result": result})
}

func (f *fakeServer) replyError(id json.RawMessage, code int, msg string) {
	f.write(map[string]any{"id": json.RawMessage(id), "error": map[string]any{"code": code, "message": msg}})
}

func (f *fakeServer) notify(method string, params any) {
	f.write(map[string]any{"method": method, "params": params})
}

func (f *fakeServer) request(id any, method string, params any) {
	f.write(map[string]any{"id": id, "method": method, "params": params})
}

func fixedClock() func() time.Time {
	base := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	n := 0
	return func() time.Time {
		n++
		return base.Add(time.Duration(n) * time.Millisecond)
	}
}

func defaultSessionOptions() adapter.SessionOptions {
	return adapter.SessionOptions{
		ID:             "relay-session-1",
		Workspace:      "/w",
		PermissionMode: "on-request",
	}
}

// collect drains a session's events until the channel closes or n arrive.
func collect(t *testing.T, ch <-chan event.Event, n int, timeout time.Duration) []event.Event {
	t.Helper()
	var out []event.Event
	deadline := time.After(timeout)
	for len(out) < n {
		select {
		case e, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, e)
		case <-deadline:
			t.Fatalf("collected %d of %d events before timing out: %s", len(out), n, describeAll(out))
		}
	}
	return out
}

func describeAll(evs []event.Event) string {
	var b []byte
	for i, e := range evs {
		if i > 0 {
			b = append(b, ' ')
		}
		b = append(b, describe(e)...)
	}
	return string(b)
}

// describe renders an event as one comparable line. The golden lists in these
// tests are written in this shape, so a diff reads as a sentence.
func describe(e event.Event) string {
	m := e.Envelope()
	switch v := e.(type) {
	case event.TurnStarted:
		return "turn_started " + m.Turn
	case event.TextDelta:
		return "text " + quote(v.Text)
	case event.Reasoning:
		if v.Summary {
			return "reasoning_summary " + quote(v.Text)
		}
		return "reasoning " + quote(v.Text)
	case event.ToolStarted:
		return "tool_started " + v.ID + " " + v.Tool + " " + quote(v.Target)
	case event.ToolOutput:
		return "tool_output " + v.ID + " " + string(v.Status) + " " + quote(v.Chunk)
	case event.PlanUpdated:
		s := "plan"
		for _, st := range v.Steps {
			s += " [" + string(st.Status) + " " + st.Text + "]"
		}
		return s
	case *event.NeedsInput:
		return "needs_input " + string(v.Ask) + " " + quote(v.Prompt)
	case event.TurnCompleted:
		s := "turn_completed " + m.Turn + " " + string(v.StopReason)
		if v.Usage != nil && v.Usage.TotalTokens != nil {
			s += " tokens=" + itoa(*v.Usage.TotalTokens)
		}
		if v.Usage != nil && v.Usage.CostUSD != nil {
			s += " HAS-USD-WHICH-CODEX-DOES-NOT-REPORT"
		}
		return s
	case event.Error:
		return "error " + v.Code + " retryable=" + btoa(v.Retryable) + " " + quote(v.Message)
	}
	return "unknown"
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func btoa(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// repoFile finds a path relative to the repository root by walking up from the
// package directory, so the fixtures can live in docs/ where the rest of the
// contract does.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		p := filepath.Join(dir, rel)
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not find %s above %s", rel, dir)
	return ""
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

var errWire = errors.New("codex: the pipe broke")
