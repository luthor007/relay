package codex

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/logx"
)

// TestHandshakeAsksForTheExperimentalAPI. Everything the schemas mark
// EXPERIMENTAL — item/tool/requestUserInput among them, which is a needs-input
// source — is presumed off unless the client asks. The flag fails loudly per
// method, so asking costs nothing.
func TestHandshakeAsksForTheExperimentalAPI(t *testing.T) {
	f := newFakeServer(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := Attach(context.Background(), f.clientRead, f.clientTo, func() error { return nil },
			Options{Log: logx.Discard(), Clock: fixedClock()})
		if err != nil {
			t.Errorf("attach: %v", err)
		}
	}()

	m := f.expect(t, "initialize")
	var p struct {
		ClientInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
		Capabilities struct {
			ExperimentalApi    bool `json:"experimentalApi"`
			RequestAttestation bool `json:"requestAttestation"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		t.Fatal(err)
	}
	if p.ClientInfo.Name != "relay" {
		t.Errorf("clientInfo.name = %q; Codex logs this", p.ClientInfo.Name)
	}
	if !p.Capabilities.ExperimentalApi {
		t.Error("experimentalApi must be requested, or item/tool/requestUserInput never arrives")
	}
	if p.Capabilities.RequestAttestation {
		t.Error("requestAttestation must stay false: it makes Codex send a request Relay cannot answer")
	}
	if strings.Contains(string(m.Params), "jsonrpc") {
		t.Error("no jsonrpc envelope field, on any message")
	}
	f.reply(m.ID, map[string]any{})
	<-done
}

// TestResumeReattachesToAThread. thread/resume rejoins a running thread and
// loads a stopped one; either way the id comes back on thread/started.
func TestResumeReattachesToAThread(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{})

	type res struct {
		s   adapter.Session
		err error
	}
	ch := make(chan res, 1)
	go func() {
		s, err := a.Resume(context.Background(),
			adapter.SessionRef{Runtime: adapter.Codex, ID: "relay-7", Native: testThread, Workspace: "/w"},
			adapter.SessionOptions{PermissionMode: "on-request"})
		ch <- res{s, err}
	}()

	m := f.expect(t, "thread/resume")
	var p threadResumeParams
	if err := json.Unmarshal(m.Params, &p); err != nil {
		t.Fatal(err)
	}
	if p.ThreadID != testThread {
		t.Errorf("threadId = %q", p.ThreadID)
	}
	if p.ApprovalPolicy != "on-request" || p.ApprovalsReviewer != "user" {
		t.Errorf("resume must re-assert both approval settings, got %q/%q", p.ApprovalPolicy, p.ApprovalsReviewer)
	}
	f.notify("thread/started", map[string]any{"thread": minimalThread(testThread)})
	f.reply(m.ID, map[string]any{})

	r := <-ch
	if r.err != nil {
		t.Fatalf("resume: %v", r.err)
	}
	if r.s.ID() != "relay-7" {
		t.Errorf("relay id = %q", r.s.ID())
	}
	if r.s.Native() != testThread {
		t.Errorf("native id = %q", r.s.Native())
	}
}

// TestForkStartsANewThreadFromAnOldOne. The forked thread gets a fresh id, so
// nothing may be pre-registered against the source thread's.
func TestForkStartsANewThreadFromAnOldOne(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{})

	type res struct {
		s   adapter.Session
		err error
	}
	ch := make(chan res, 1)
	go func() {
		s, err := a.Fork(context.Background(),
			adapter.SessionRef{Runtime: adapter.Codex, Native: testThread},
			adapter.SessionOptions{ID: "relay-fork", Workspace: "/w", PermissionMode: "on-request"})
		ch <- res{s, err}
	}()

	m := f.expect(t, "thread/fork")
	var p threadForkParams
	if err := json.Unmarshal(m.Params, &p); err != nil {
		t.Fatal(err)
	}
	if p.ThreadID != testThread {
		t.Errorf("fork source = %q", p.ThreadID)
	}

	forked := minimalThread("thread-forked")
	forked["forkedFromId"] = testThread
	f.notify("thread/started", map[string]any{"thread": forked})
	f.reply(m.ID, map[string]any{})

	r := <-ch
	if r.err != nil {
		t.Fatalf("fork: %v", r.err)
	}
	if r.s.Native() != "thread-forked" {
		t.Errorf("the fork bound to %q, not to the new thread", r.s.Native())
	}
	if !a.Capabilities().Has(adapter.CapFork) {
		t.Error("CapFork should be yes on Codex")
	}
}

// TestCloseDetachesBeforeTearingDown. thread/unsubscribe is the only half of
// the subscription pair that exists; without it app-server keeps serialising
// events at a conversation nobody is watching.
func TestCloseDetachesBeforeTearingDown(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{})
	s := f.open(t, a, defaultSessionOptions(), testThread)

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for range s.Events() {
		}
	}()

	done := make(chan error, 1)
	go func() { done <- s.Close(context.Background()) }()

	m := f.expect(t, "thread/unsubscribe")
	f.reply(m.ID, map[string]any{})
	if err := <-done; err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Events() must close exactly once, when the session ends")
	}
}

// TestAStalledConsumerDoesNotDeadlockTheRuntime is adapter.Session's contract,
// verbatim: "a consumer that stops reading is a bug in the consumer, but a
// deadlocked runtime is a bug in the adapter". A bounded channel written from
// the reader goroutine would stall Codex — including stalling the answers to
// approval requests, which is the deadlock that matters.
func TestAStalledConsumerDoesNotDeadlockTheRuntime(t *testing.T) {
	f := newFakeServer(t)
	a := f.attach(t, Options{})
	s := f.open(t, a, defaultSessionOptions(), testThread)

	// Nobody reads s.Events() at all.
	for i := 0; i < 500; i++ {
		f.notify("item/agentMessage/delta", map[string]any{
			"threadId": testThread, "turnId": "turn-1", "itemId": "m1", "delta": "x",
		})
	}
	// The connection is still live: a request round-trips.
	done := make(chan error, 1)
	go func() { done <- s.Compact(context.Background()) }()
	m := f.expect(t, "thread/compact/start")
	f.reply(m.ID, map[string]any{})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("compact: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("500 unread events wedged the connection")
	}

	// Closing still terminates, and says how much the dead consumer missed.
	closed := make(chan error, 1)
	go func() { closed <- s.Close(context.Background()) }()
	unsub := f.expect(t, "thread/unsubscribe")
	f.reply(unsub.ID, map[string]any{})
	if err := <-closed; err != nil {
		t.Fatalf("close: %v", err)
	}
	if s.DroppedEvents() == 0 {
		t.Error("a consumer that stopped reading should be reported, not silently forgiven")
	}
}

func TestQueueClosesExactlyOnceAndDrains(t *testing.T) {
	q := newQueue()
	for i := 0; i < 5; i++ {
		q.push(event.TextDelta{Text: "x"})
	}
	got := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range q.events() {
			got++
		}
	}()
	q.closeAndDrain(time.Second)
	<-done
	if got != 5 {
		t.Fatalf("drained %d of 5 events before the channel closed", got)
	}
	// Pushing after close is a no-op rather than a panic on a closed channel.
	q.push(event.TextDelta{Text: "late"})
}

func TestAbandonedQueueDoesNotLeakItsPump(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		q := newQueue()
		q.push(event.TextDelta{Text: "unread"})
		q.closeAndDrain(0) // nobody is reading; abandon immediately
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+5 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutines went from %d to %d; the pumps leaked", before, runtime.NumGoroutine())
}

// TestConnectionFailureIsFatalForEverySession.
func TestConnectionFailureIsFatalForEverySession(t *testing.T) {
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

	_ = f.toClient.CloseWithError(errWire)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the session must end when the app-server connection does")
	}
	if len(got) != 1 {
		t.Fatalf("got %s", describeAll(got))
	}
	if e := got[0].(event.Error); !e.Fatal {
		t.Error("a failed connection is not a turn-level error")
	}
}
