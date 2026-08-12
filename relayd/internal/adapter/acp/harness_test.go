package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/event"
)

// A fake ACP agent over a pair of in-memory pipes.
//
// None of the three runtimes is installed in this container, so every test in
// this package drives the adapter through Attach rather than Dial. That is the
// same seam the fixture replay uses, which is why the fixture is a test of the
// adapter and not just of the JSON.

type fakeAgent struct {
	t *testing.T

	// The adapter reads agentOut and writes agentIn.
	agentOutR *io.PipeReader
	agentOutW *io.PipeWriter
	agentInR  *io.PipeReader
	agentInW  *io.PipeWriter

	incoming chan *message

	mu     sync.Mutex
	closed bool
}

func newFakeAgent(t *testing.T) *fakeAgent {
	t.Helper()
	outR, outW := io.Pipe()
	inR, inW := io.Pipe()
	f := &fakeAgent{
		t:         t,
		agentOutR: outR, agentOutW: outW,
		agentInR: inR, agentInW: inW,
		incoming: make(chan *message, 64),
	}
	go f.readLoop()
	t.Cleanup(f.close)
	return f
}

func (f *fakeAgent) readLoop() {
	sc := bufio.NewScanner(f.agentInR)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m message
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		cp := m
		f.incoming <- &cp
	}
	close(f.incoming)
}

func (f *fakeAgent) close() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	f.mu.Unlock()
	_ = f.agentOutW.Close()
	_ = f.agentInR.Close()
}

func (f *fakeAgent) closer() error {
	f.close()
	return nil
}

// next waits for one message from the client.
func (f *fakeAgent) next() *message {
	f.t.Helper()
	select {
	case m, ok := <-f.incoming:
		if !ok {
			f.t.Fatal("the client closed the connection while a message was expected")
		}
		return m
	case <-time.After(5 * time.Second):
		f.t.Fatal("timed out waiting for a message from the client")
		return nil
	}
}

// tryNext is next without t.Fatal, for use off the test goroutine.
func (f *fakeAgent) tryNext(d time.Duration) (*message, bool) {
	select {
	case m, ok := <-f.incoming:
		return m, ok
	case <-time.After(d):
		return nil, false
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func (f *fakeAgent) writeRaw(b []byte) {
	f.t.Helper()
	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()
	if closed {
		return
	}
	if _, err := f.agentOutW.Write(append(b, '\n')); err != nil {
		f.t.Fatalf("writing to the client: %v", err)
	}
}

func (f *fakeAgent) write(v any) {
	f.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		f.t.Fatalf("encoding: %v", err)
	}
	f.writeRaw(b)
}

func (f *fakeAgent) respond(id json.RawMessage, result any) {
	f.t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		f.t.Fatalf("encoding result: %v", err)
	}
	f.write(message{JSONRPC: "2.0", ID: id, Result: raw})
}

func (f *fakeAgent) notify(method string, params any) {
	f.t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		f.t.Fatalf("encoding params: %v", err)
	}
	f.write(message{JSONRPC: "2.0", Method: method, Params: raw})
}

func (f *fakeAgent) request(id int, method string, params any) {
	f.t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		f.t.Fatalf("encoding params: %v", err)
	}
	f.write(message{JSONRPC: "2.0", ID: json.RawMessage(itoa(id)), Method: method, Params: raw})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func (f *fakeAgent) update(sessionID string, update any) {
	f.t.Helper()
	raw, err := json.Marshal(update)
	if err != nil {
		f.t.Fatalf("encoding update: %v", err)
	}
	f.notify(methodSessionUpdate, sessionNotification{SessionID: sessionID, Update: raw})
}

// testOptions is the shared configuration: quiet logs and short grace periods,
// so a hung adapter fails a test rather than stalling it.
func testOptions(t *testing.T, r adapter.Runtime) Options {
	t.Helper()
	return Options{
		Runtime:     r,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		DrainGrace:  200 * time.Millisecond,
		CallTimeout: 5 * time.Second,
		CancelGrace: 5 * time.Second,
		CostTimeout: time.Second,
	}
}

// dial attaches and answers the initialize handshake with caps.
func dial(t *testing.T, f *fakeAgent, opts Options, caps AgentCapabilities) *Adapter {
	t.Helper()
	type result struct {
		a   *Adapter
		err error
	}
	done := make(chan result, 1)
	go func() {
		a, err := Attach(context.Background(), f.agentOutR, f.agentInW, f.closer, opts)
		done <- result{a, err}
	}()

	m := f.next()
	if m.Method != methodInitialize {
		t.Fatalf("first client message was %q, want initialize", m.Method)
	}
	f.respond(m.ID, initializeResult{
		ProtocolVersion:   ProtocolVersion,
		AgentCapabilities: caps,
		AuthMethods:       []AuthMethod{{ID: "oauth-personal", Name: "Log in"}},
	})

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Attach: %v", r.err)
		}
		t.Cleanup(func() { _ = r.a.Close(context.Background()) })
		return r.a
	case <-time.After(5 * time.Second):
		t.Fatal("Attach did not return")
		return nil
	}
}

// startSession runs Start and answers session/new with nativeID.
func startSession(t *testing.T, f *fakeAgent, a *Adapter, nativeID string) *Session {
	t.Helper()
	type result struct {
		s   adapter.Session
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, err := a.Start(context.Background(), adapter.SessionOptions{
			ID:        "relay-session-1",
			Workspace: "/Users/USER/src/relay",
		})
		done <- result{s, err}
	}()

	m := f.next()
	if m.Method != methodSessionNew {
		t.Fatalf("client sent %q, want session/new", m.Method)
	}
	f.respond(m.ID, newSessionResult{
		SessionID: nativeID,
		Modes: &SessionModeState{
			CurrentModeID:  "ask",
			AvailableModes: []SessionMode{{ID: "ask", Name: "Ask"}, {ID: "code", Name: "Code"}},
		},
	})

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Start: %v", r.err)
		}
		return r.s.(*Session)
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return")
		return nil
	}
}

// ---- event collection ----

type collector struct {
	mu     sync.Mutex
	events []event.Event
	needs  chan *event.NeedsInput
	done   chan struct{}
}

func collect(s adapter.Session) *collector {
	c := &collector{needs: make(chan *event.NeedsInput, 16), done: make(chan struct{})}
	go func() {
		defer close(c.done)
		for e := range s.Events() {
			c.mu.Lock()
			c.events = append(c.events, e)
			c.mu.Unlock()
			if n, ok := e.(*event.NeedsInput); ok {
				c.needs <- n
			}
		}
	}()
	return c
}

func (c *collector) all() []event.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]event.Event(nil), c.events...)
}

func (c *collector) kinds() []event.Kind {
	out := []event.Kind{}
	for _, e := range c.all() {
		out = append(out, e.Kind())
	}
	return out
}

func (c *collector) needsInput(t *testing.T) *event.NeedsInput {
	t.Helper()
	select {
	case n := <-c.needs:
		return n
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a NeedsInput")
		return nil
	}
}

// waitFor blocks until an event satisfying pred has been seen.
func (c *collector) waitFor(t *testing.T, what string, pred func(event.Event) bool) event.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range c.all() {
			if pred(e) {
				return e
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; saw %v", what, c.kinds())
	return nil
}

func (c *collector) completions() []event.TurnCompleted {
	var out []event.TurnCompleted
	for _, e := range c.all() {
		if tc, ok := e.(event.TurnCompleted); ok {
			out = append(out, tc)
		}
	}
	return out
}

func (c *collector) text() string {
	var b []byte
	for _, e := range c.all() {
		if td, ok := e.(event.TextDelta); ok {
			b = append(b, td.Text...)
		}
	}
	return string(b)
}

// ---- repo paths ----

// repoFile resolves a repo-relative path by walking up from the package
// directory until the vendored fixtures are found.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "docs", "fixtures", "adapters", "acp-schema.json")); err == nil {
			return filepath.Join(dir, rel)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not find the repo root from %s", dir)
	return ""
}
