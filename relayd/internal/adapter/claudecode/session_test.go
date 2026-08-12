package claudecode

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/logx"
)

func echoLine(text string) string {
	return `{"type":"user","isReplay":true,"message":{"role":"user","content":[{"type":"text","text":"` + text + `"}]}}` + "\n"
}

func startSession(t *testing.T, proc *scriptedProcess, opts adapter.SessionOptions) (*Adapter, *Session, *scriptLauncher) {
	t.Helper()
	if opts.Workspace == "" {
		opts.Workspace = "/work"
	}
	a, l := newTestAdapter(t, proc)
	s, err := a.Start(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return a, s.(*Session), l
}

func nextEvent(t *testing.T, s adapter.Session) event.Event {
	t.Helper()
	select {
	case ev, ok := <-s.Events():
		if !ok {
			t.Fatal("the event stream closed early")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an event")
		return nil
	}
}

// Steering is the same wire message as a turn — that is why this runtime has it
// and ACP does not. It is refused unless the named turn is the one running,
// because a steer aimed at a finished turn is a lost instruction.
func TestSteerRequiresTheRunningTurn(t *testing.T) {
	proc := newScriptedProcess(scriptOptions{
		Chunks:   [][]byte{[]byte(echoLine("first"))},
		KeepOpen: true,
	})
	_, s, _ := startSession(t, proc, adapter.SessionOptions{ID: "33333333-3333-3333-3333-333333333333"})
	ctx := context.Background()

	if err := s.Steer(ctx, "nothing", adapter.Turn{Text: "x"}); !errors.Is(err, adapter.ErrTurnNotActive) {
		t.Fatalf("steer with no turn running = %v", err)
	}

	id, err := s.Send(ctx, adapter.Turn{ID: "turn-a", Text: "first"})
	if err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, s); ev.Kind() != event.KindTurnStarted {
		t.Fatalf("first event = %v", ev.Kind())
	}

	if err := s.Steer(ctx, "some-other-turn", adapter.Turn{Text: "x"}); !errors.Is(err, adapter.ErrTurnNotActive) {
		t.Errorf("steer at the wrong turn = %v", err)
	}
	if err := s.Steer(ctx, id, adapter.Turn{Text: "also update the changelog"}); err != nil {
		t.Fatalf("steer at the running turn = %v", err)
	}

	in := proc.Input()
	if len(in) != 2 {
		t.Fatalf("stdin lines = %v", in)
	}
	if !strings.Contains(in[1], "also update the changelog") {
		t.Errorf("the steer line = %s", in[1])
	}
	if in[0][:len(`{"type":"user"`)] != `{"type":"user"` {
		t.Errorf("a steer and a turn are the same wire message: %s", in[1])
	}
}

// A glasses photo cannot enter a prompt on a runtime that never advertised
// image support, and it is refused here rather than dropped silently.
func TestSendRefusesUnobservableContent(t *testing.T) {
	proc := newScriptedProcess(scriptOptions{KeepOpen: true})
	_, s, _ := startSession(t, proc, adapter.SessionOptions{ID: "44444444-4444-4444-4444-444444444444"})
	ctx := context.Background()

	_, err := s.Send(ctx, adapter.Turn{Text: "look", Blocks: []adapter.Block{{Kind: adapter.BlockImage, Data: []byte{1}}}})
	if !errors.Is(err, adapter.ErrUnsupported) {
		t.Fatalf("an image turn = %v, want ErrUnsupported", err)
	}
	if _, err := s.Send(ctx, adapter.Turn{Text: "   "}); err == nil {
		t.Error("an empty turn must be refused")
	}

	if _, err := s.Send(ctx, adapter.Turn{
		Text:   "look at",
		Blocks: []adapter.Block{{Kind: adapter.BlockResourceLink, URI: "/a/b.go"}},
	}); err != nil {
		t.Fatal(err)
	}
	if in := proc.Input(); len(in) != 1 || !strings.Contains(in[0], "/a/b.go") {
		t.Errorf("a resource link goes in verbatim as text: %v", in)
	}
}

// The process dying is observable, so it is reported — with the tail of stderr,
// which is diagnostics and never parsed.
func TestProcessExitIsFatalAndObserved(t *testing.T) {
	proc := newScriptedProcess(scriptOptions{
		WaitErr: errors.New("exit status 1"),
		Stderr:  "line one\nline two\nError: could not authenticate\n",
	})
	_, s, _ := startSession(t, proc, adapter.SessionOptions{ID: "55555555-5555-5555-5555-555555555555"})

	var got []event.Event
	for ev := range s.Events() {
		got = append(got, ev)
	}
	if len(got) != 1 {
		t.Fatalf("events = %d", len(got))
	}
	e, ok := got[0].(event.Error)
	if !ok || !e.Fatal || e.Code != "process_exit" {
		t.Fatalf("event = %#v", got[0])
	}
	if !strings.Contains(e.Message, "could not authenticate") {
		t.Errorf("the stderr tail should be attached: %q", e.Message)
	}
	if e.Ping() != event.PingInformational {
		t.Errorf("ping = %v", e.Ping())
	}
	if s.ExitErr() == nil {
		t.Error("the exit status should be readable after the fact")
	}
}

// Closing is idempotent, closes the stream exactly once, and a session that has
// ended refuses work rather than blocking.
func TestCloseIsIdempotent(t *testing.T) {
	proc := newScriptedProcess(scriptOptions{KeepOpen: true})
	a, s, _ := startSession(t, proc, adapter.SessionOptions{ID: "66666666-6666-6666-6666-666666666666"})
	ctx := context.Background()

	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-s.Events(); ok {
		t.Error("the stream must be closed")
	}
	if _, err := s.Send(ctx, adapter.Turn{Text: "x"}); !errors.Is(err, adapter.ErrSessionClosed) {
		t.Errorf("send after close = %v", err)
	}
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Start(ctx, adapter.SessionOptions{ID: "x", Workspace: "/work"}); !errors.Is(err, adapter.ErrSessionClosed) {
		t.Errorf("start after the adapter closed = %v", err)
	}
}

// Resume reattaches by the runtime's own id; fork branches and gets a new name.
func TestResumeAndFork(t *testing.T) {
	proc := newScriptedProcess(scriptOptions{KeepOpen: true})
	a, l := newTestAdapter(t, proc)
	ctx := context.Background()

	s, err := a.Resume(ctx,
		adapter.SessionRef{Runtime: adapter.ClaudeCode, ID: "relay-1", Native: "b393be4c-99d7-4d92-ada2-df47ce494ffe"},
		adapter.SessionOptions{Workspace: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	args := l.Spec().Args
	if v, _ := flagValue(args, "--resume"); v != "b393be4c-99d7-4d92-ada2-df47ce494ffe" {
		t.Errorf("--resume = %q in %v", v, args)
	}
	if contains(args, "--session-id") {
		t.Errorf("a plain resume keeps the session's name: %v", args)
	}
	if s.Native() != "b393be4c-99d7-4d92-ada2-df47ce494ffe" || s.ID() != "relay-1" {
		t.Errorf("ids = %q / %q", s.ID(), s.Native())
	}
	_ = s.Close(ctx)

	proc2 := newScriptedProcess(scriptOptions{KeepOpen: true})
	l.proc = proc2
	if _, err := a.Resume(ctx,
		adapter.SessionRef{ID: "relay-2", Native: "old-native"},
		adapter.SessionOptions{Workspace: "/work", Extra: map[string]string{"fork": "true"}}); err != nil {
		t.Fatal(err)
	}
	args = l.Spec().Args
	if !contains(args, "--fork-session") {
		t.Errorf("fork args = %v", args)
	}
	if v, _ := flagValue(args, "--session-id"); v == "" {
		t.Errorf("a fork is a new session and needs a name: %v", args)
	}

	if _, err := a.Resume(ctx, adapter.SessionRef{}, adapter.SessionOptions{Workspace: "/work"}); !errors.Is(err, adapter.ErrSessionNotFound) {
		t.Errorf("resume with no id = %v", err)
	}
}

func TestStartValidatesTheWorkspace(t *testing.T) {
	proc := newScriptedProcess(scriptOptions{KeepOpen: true})
	a, _ := newTestAdapter(t, proc)
	ctx := context.Background()

	if _, err := a.Start(ctx, adapter.SessionOptions{ID: "x"}); err == nil {
		t.Error("a session needs a workspace")
	}
	if _, err := a.Start(ctx, adapter.SessionOptions{ID: "x", Workspace: "relative/path"}); err == nil {
		t.Error("the workspace must be absolute")
	}
}

// A caller that hands us something that is not a UUID still gets a named
// session: Relay's id and the runtime's id are allowed to differ.
func TestNonUUIDSessionIDStillNamesTheSession(t *testing.T) {
	proc := newScriptedProcess(scriptOptions{KeepOpen: true})
	a, l := newTestAdapter(t, proc)
	s, err := a.Start(context.Background(), adapter.SessionOptions{ID: "payments-branch", Workspace: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	if s.ID() != "payments-branch" {
		t.Errorf("Relay's id = %q", s.ID())
	}
	v, _ := flagValue(l.Spec().Args, "--session-id")
	if v == "payments-branch" || len(v) != 36 {
		t.Errorf("--session-id = %q, want a generated UUID", v)
	}
	if s.Native() != v {
		t.Errorf("Native = %q, want the UUID we named it with", s.Native())
	}
}

// The unverified interrupt is off unless a caller explicitly asks for it, and
// when it is on the bytes it writes are exactly these — asserted so a probe on
// a real machine has something to compare against.
func TestUnverifiedControlInterrupt(t *testing.T) {
	proc := newScriptedProcess(scriptOptions{KeepOpen: true})
	l := &scriptLauncher{proc: proc}
	a := New(Options{
		Launcher:                   l,
		Log:                        logx.Discard(),
		PermissionBaseURL:          "http://127.0.0.1:1",
		ConfigDir:                  t.TempDir(),
		Home:                       t.TempDir(),
		UnverifiedControlInterrupt: true,
	})
	defer func() { _ = a.Close(context.Background()) }()

	s, err := a.Start(context.Background(), adapter.SessionOptions{ID: "77777777-7777-7777-7777-777777777777", Workspace: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(context.Background(), "any"); err != nil {
		t.Fatal(err)
	}
	in := proc.Input()
	if len(in) != 1 || !strings.Contains(in[0], `"subtype":"interrupt"`) || !strings.Contains(in[0], `"type":"control_request"`) {
		t.Fatalf("interrupt line = %v", in)
	}
}

// The capability descriptor is what the orchestrator reads before it asks for
// anything, so the two rows this adapter narrows are asserted here.
func TestCapabilitiesAreNarrowedHonestly(t *testing.T) {
	a := New(Options{Log: logx.Discard()})
	c := a.Capabilities()

	if c.Get(adapter.CapPlan) != adapter.SupportNo {
		t.Errorf("CapPlan = %v", c.Get(adapter.CapPlan))
	}
	if c.Get(adapter.CapCancel) != adapter.SupportUnknown {
		t.Errorf("CapCancel = %v: the client-side interrupt message is not in the vendored trace", c.Get(adapter.CapCancel))
	}
	for _, cap := range []adapter.Capability{adapter.CapSteer, adapter.CapCostUSD, adapter.CapTokens, adapter.CapReasoning, adapter.CapNeedsInput} {
		if !c.Has(cap) {
			t.Errorf("%s = %v, want yes", cap, c.Get(cap))
		}
	}
	// Prompt content beyond text is unprobed on this runtime and must stay that
	// way until somebody looks.
	for _, cap := range []adapter.Capability{adapter.CapPromptImage, adapter.CapPromptAudio, adapter.CapPromptEmbeddedContext} {
		if c.Get(cap) != adapter.SupportUnknown {
			t.Errorf("%s = %v, want unknown", cap, c.Get(cap))
		}
	}
	if err := c.Require(adapter.CapPlan); !errors.Is(err, adapter.ErrUnsupported) {
		t.Errorf("Require(plan) = %v", err)
	}
}
