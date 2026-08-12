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

func promptText(t *testing.T, m *message) string {
	t.Helper()
	if m.Method != methodPrompt {
		t.Fatalf("client sent %q, want session/prompt", m.Method)
	}
	var p promptParams
	if err := json.Unmarshal(m.Params, &p); err != nil {
		t.Fatalf("prompt params: %v", err)
	}
	if len(p.Prompt) == 0 {
		t.Fatal("a prompt must carry at least one content block")
	}
	return p.Prompt[0].Text
}

func expectQuiet(t *testing.T, f *fakeAgent) {
	t.Helper()
	if m, ok := f.tryNext(150 * time.Millisecond); ok {
		t.Fatalf("client sent an unexpected %q", m.Method)
	}
}

// TestSteerIsUnsupported is ADAPTERS.md §4's central negative, enforced at the
// adapter boundary rather than left to a comment.
func TestSteerIsUnsupported(t *testing.T) {
	f := newFakeAgent(t)
	a := dial(t, f, testOptions(t, adapter.OpenCode), AgentCapabilities{})
	s := startSession(t, f, a, "sess_1")

	err := s.Steer(context.Background(), "t1", adapter.Turn{Text: "also do this"})
	if !errors.Is(err, adapter.ErrUnsupported) {
		t.Fatalf("Steer returned %v, want an *UnsupportedError", err)
	}
	var ue *adapter.UnsupportedError
	if !errors.As(err, &ue) || ue.Capability != adapter.CapSteer {
		t.Fatalf("Steer error = %#v", err)
	}
	if !strings.Contains(ue.Note, "eight branches") {
		t.Errorf("the error should carry the reason from the capability descriptor, got %q", ue.Note)
	}
}

// TestAdditionsQueueAndKeepTheirOrder is the first half of §4's table.
func TestAdditionsQueueAndKeepTheirOrder(t *testing.T) {
	ctx := context.Background()
	f := newFakeAgent(t)
	a := dial(t, f, testOptions(t, adapter.Hermes), AgentCapabilities{})
	s := startSession(t, f, a, "sess_1")
	c := collect(s)

	first, err := s.Send(ctx, adapter.Turn{Text: "one"})
	if err != nil {
		t.Fatal(err)
	}
	m1 := f.next()
	if got := promptText(t, m1); got != "one" {
		t.Fatalf("first prompt = %q", got)
	}

	second, err := s.Deliver(ctx, adapter.Turn{Text: "two"}, ModeQueue)
	if err != nil {
		t.Fatal(err)
	}
	third, err := s.Deliver(ctx, adapter.Turn{Text: "three"}, ModeAuto)
	if err != nil {
		t.Fatal(err)
	}
	if second.Disposition != DispositionQueued || third.Disposition != DispositionQueued {
		t.Fatalf("dispositions = %q, %q; both should queue behind a running turn", second.Disposition, third.Disposition)
	}
	if third.QueueDepth != 2 {
		t.Errorf("queue depth = %d, want 2", third.QueueDepth)
	}
	// Queued means queued: nothing goes out while a turn is running.
	expectQuiet(t, f)

	f.respond(m1.ID, promptResult{StopReason: "end_turn"})
	m2 := f.next()
	if got := promptText(t, m2); got != "two" {
		t.Fatalf("second prompt = %q, want the first addition", got)
	}
	f.respond(m2.ID, promptResult{StopReason: "end_turn"})
	m3 := f.next()
	if got := promptText(t, m3); got != "three" {
		t.Fatalf("third prompt = %q", got)
	}
	f.respond(m3.ID, promptResult{StopReason: "end_turn"})

	c.waitFor(t, "three completions", func(event.Event) bool { return len(c.completions()) >= 3 })
	ids := []string{}
	for _, tc := range c.completions() {
		ids = append(ids, tc.Turn)
	}
	want := []string{first, second.TurnID, third.TurnID}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("completion %d is turn %q, want %q", i, ids[i], want[i])
		}
	}
}

// TestBareCancelHoldsTheQueue: "no, stop" must not be followed by the addition
// queued a minute earlier. The additions are held, not lost, and Pending is how
// the orchestrator sees them.
func TestBareCancelHoldsTheQueue(t *testing.T) {
	ctx := context.Background()
	f := newFakeAgent(t)
	a := dial(t, f, testOptions(t, adapter.OpenCode), AgentCapabilities{})
	s := startSession(t, f, a, "sess_1")
	c := collect(s)

	first, _ := s.Send(ctx, adapter.Turn{Text: "one"})
	m1 := f.next()
	add, _ := s.Deliver(ctx, adapter.Turn{Text: "also update the changelog"}, ModeQueue)

	done := make(chan error, 1)
	go func() { done <- s.Cancel(ctx, first) }()

	m := f.next()
	if m.Method != methodCancel {
		t.Fatalf("client sent %q, want session/cancel", m.Method)
	}
	if len(m.ID) != 0 {
		t.Error("session/cancel is a notification and must carry no id")
	}
	f.respond(m1.ID, promptResult{StopReason: "cancelled"})
	if err := <-done; err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	c.waitFor(t, "the cancelled completion", func(e event.Event) bool {
		tc, ok := e.(event.TurnCompleted)
		return ok && tc.StopReason == event.StopCancelled
	})
	expectQuiet(t, f)

	if !s.Held() {
		t.Error("a bare cancel must hold the queue")
	}
	pending := s.Pending()
	if len(pending) != 1 || pending[0].ID != add.TurnID {
		t.Fatalf("Pending() = %+v; the addition must survive the cancel", pending)
	}

	s.Flush()
	m2 := f.next()
	if got := promptText(t, m2); got != "also update the changelog" {
		t.Fatalf("after Flush the queued addition should go out, got %q", got)
	}
	f.respond(m2.ID, promptResult{StopReason: "end_turn"})
}

// TestRefusalHoldsTheQueue: a refusal drops the user prompt and everything
// after it from the next prompt, so a queued follow-up would land on top of
// context that is gone.
func TestRefusalHoldsTheQueue(t *testing.T) {
	ctx := context.Background()
	f := newFakeAgent(t)
	a := dial(t, f, testOptions(t, adapter.Hermes), AgentCapabilities{})
	s := startSession(t, f, a, "sess_1")
	c := collect(s)

	_, _ = s.Send(ctx, adapter.Turn{Text: "one"})
	m1 := f.next()
	_, _ = s.Deliver(ctx, adapter.Turn{Text: "two"}, ModeQueue)
	f.respond(m1.ID, promptResult{StopReason: "refusal"})

	e := c.waitFor(t, "the refusal", func(e event.Event) bool {
		tc, ok := e.(event.TurnCompleted)
		return ok && tc.StopReason == event.StopRefusal
	}).(event.TurnCompleted)
	if e.OK {
		t.Error("a refusal is not a successful turn")
	}
	if e.StopReason.Retryable() {
		t.Error("a refusal is not retryable: the instruction has to be carried again")
	}
	expectQuiet(t, f)
	if !s.Held() || len(s.Pending()) != 1 {
		t.Errorf("held=%v pending=%d, want the queue held with one entry", s.Held(), len(s.Pending()))
	}
}

// TestRedirectWhenIdleJustStarts: there is nothing to cancel, so a redirect is
// an ordinary turn and says so.
func TestRedirectWhenIdleJustStarts(t *testing.T) {
	ctx := context.Background()
	f := newFakeAgent(t)
	a := dial(t, f, testOptions(t, adapter.OpenClaw), AgentCapabilities{})
	s := startSession(t, f, a, "agent:main:main")

	d, err := s.Deliver(ctx, adapter.Turn{Text: "do X instead"}, ModeRedirect)
	if err != nil {
		t.Fatal(err)
	}
	if d.Disposition != DispositionStarted || d.CancelledTurn != "" {
		t.Fatalf("delivery = %+v", d)
	}
	m := f.next()
	if got := promptText(t, m); got != "do X instead" {
		t.Fatalf("prompt = %q", got)
	}
	f.respond(m.ID, promptResult{StopReason: "end_turn"})
}

// TestDropRemovesAQueuedUtterance is the undo half of announce-and-undo.
func TestDropRemovesAQueuedUtterance(t *testing.T) {
	ctx := context.Background()
	f := newFakeAgent(t)
	a := dial(t, f, testOptions(t, adapter.OpenCode), AgentCapabilities{})
	s := startSession(t, f, a, "sess_1")

	_, _ = s.Send(ctx, adapter.Turn{Text: "one"})
	m1 := f.next()
	add, _ := s.Deliver(ctx, adapter.Turn{Text: "two"}, ModeQueue)

	if !s.Drop(add.TurnID) {
		t.Fatal("Drop should have found the queued turn")
	}
	if s.Drop(add.TurnID) {
		t.Error("Drop is not idempotent-true; the second call should report nothing to drop")
	}
	f.respond(m1.ID, promptResult{StopReason: "end_turn"})
	expectQuiet(t, f)
}

// TestCancelOnAStaleTurnIsRefused: cancelling a turn that is no longer the
// running one must not silently cancel a different turn.
func TestCancelOnAStaleTurnIsRefused(t *testing.T) {
	ctx := context.Background()
	f := newFakeAgent(t)
	a := dial(t, f, testOptions(t, adapter.Hermes), AgentCapabilities{})
	s := startSession(t, f, a, "sess_1")

	if err := s.Cancel(ctx, "nothing-is-running"); !errors.Is(err, adapter.ErrTurnNotActive) {
		t.Fatalf("Cancel with no turn returned %v", err)
	}
	_, _ = s.Send(ctx, adapter.Turn{Text: "one"})
	m1 := f.next()
	if err := s.Cancel(ctx, "some-other-turn"); !errors.Is(err, adapter.ErrTurnNotActive) {
		t.Fatalf("Cancel on a stale id returned %v", err)
	}
	f.respond(m1.ID, promptResult{StopReason: "end_turn"})
}

// TestStopReasonsAllSurvive: all five reach TurnCompleted, and an undocumented
// sixth is passed through rather than flattened.
func TestStopReasonsAllSurvive(t *testing.T) {
	ctx := context.Background()
	for _, reason := range []string{"end_turn", "max_tokens", "max_turn_requests", "refusal", "cancelled", "something_new"} {
		t.Run(reason, func(t *testing.T) {
			f := newFakeAgent(t)
			a := dial(t, f, testOptions(t, adapter.OpenCode), AgentCapabilities{})
			s := startSession(t, f, a, "sess_1")
			c := collect(s)

			_, _ = s.Send(ctx, adapter.Turn{Text: "go"})
			m := f.next()
			f.respond(m.ID, promptResult{StopReason: reason})

			e := c.waitFor(t, "a completion", func(e event.Event) bool {
				_, ok := e.(event.TurnCompleted)
				return ok
			}).(event.TurnCompleted)
			if string(e.StopReason) != reason {
				t.Errorf("stop reason = %q, want %q", e.StopReason, reason)
			}
			if e.OK != (reason == "end_turn") {
				t.Errorf("ok = %v for %q", e.OK, reason)
			}
			if e.Usage != nil {
				t.Error("ACP carries no usage; nil means the console shows a gap rather than a free turn")
			}
			if e.Ping() != event.PingInformational {
				t.Error("TurnCompleted is an informational ping")
			}
		})
	}
}

// TestProtocolVersionMismatchDisconnects: negotiation is one round trip and
// there is no second, so skew is a startup failure with a clear message.
func TestProtocolVersionMismatchDisconnects(t *testing.T) {
	f := newFakeAgent(t)
	done := make(chan error, 1)
	go func() {
		_, err := Attach(context.Background(), f.agentOutR, f.agentInW, f.closer, testOptions(t, adapter.Hermes))
		done <- err
	}()
	m := f.next()
	f.respond(m.ID, initializeResult{ProtocolVersion: 99})

	select {
	case err := <-done:
		if !errors.Is(err, ErrProtocolVersion) {
			t.Fatalf("Attach returned %v, want ErrProtocolVersion", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Attach did not return")
	}
}

// TestAuthRequiredMapsToTheInstallerPath: -32000 on session/new is recoverable
// with authenticate, but not by voice.
func TestAuthRequiredMapsToTheInstallerPath(t *testing.T) {
	f := newFakeAgent(t)
	a := dial(t, f, testOptions(t, adapter.OpenCode), AgentCapabilities{})

	done := make(chan error, 1)
	go func() {
		_, err := a.Start(context.Background(), adapter.SessionOptions{Workspace: "/tmp/ws"})
		done <- err
	}()
	m := f.next()
	f.write(message{JSONRPC: "2.0", ID: m.ID, Error: &RPCError{Code: CodeAuthRequired, Message: "Authentication required"}})

	err := <-done
	if !errors.Is(err, adapter.ErrAuthRequired) {
		t.Fatalf("Start returned %v, want ErrAuthRequired", err)
	}
	if !strings.Contains(err.Error(), "oauth-personal") {
		t.Errorf("the error should name the auth methods the handshake offered, got %v", err)
	}
	if err := a.Authenticate(context.Background(), "no-such-method"); err == nil {
		t.Error("Authenticate should refuse a method the agent never offered")
	}
}

// TestResumeNeedsLoadSession: agentCapabilities.loadSession is per-runtime and
// per-version, and the registry must start a new session and say so rather than
// failing silently.
func TestResumeNeedsLoadSession(t *testing.T) {
	f := newFakeAgent(t)
	a := dial(t, f, testOptions(t, adapter.Hermes), AgentCapabilities{LoadSession: false})

	_, err := a.Resume(context.Background(), adapter.SessionRef{Native: "sess_old"}, adapter.SessionOptions{Workspace: "/tmp/ws"})
	if !errors.Is(err, adapter.ErrUnsupported) {
		t.Fatalf("Resume returned %v, want an *UnsupportedError", err)
	}
	if a.Capabilities().Get(adapter.CapResume) != adapter.SupportNo {
		t.Error("a false loadSession must narrow CapResume to no")
	}
}

// TestResumeReplaysWithoutPinging: session/load replays the whole conversation
// back as session/update before it resolves, and nobody gets woken about a turn
// from two weeks ago.
func TestResumeReplaysWithoutPinging(t *testing.T) {
	f := newFakeAgent(t)
	a := dial(t, f, testOptions(t, adapter.Hermes), AgentCapabilities{LoadSession: true})

	type res struct {
		s   adapter.Session
		err error
	}
	done := make(chan res, 1)
	go func() {
		s, err := a.Resume(context.Background(),
			adapter.SessionRef{ID: "relay-1", Native: "sess_old", Workspace: "/tmp/ws"},
			adapter.SessionOptions{})
		done <- res{s, err}
	}()

	m := f.next()
	if m.Method != methodSessionLoad {
		t.Fatalf("client sent %q, want session/load", m.Method)
	}
	f.update("sess_old", map[string]any{
		"sessionUpdate": updateAgentMessageChunk,
		"content":       map[string]any{"type": "text", "text": "from two weeks ago"},
	})
	f.respond(m.ID, loadSessionResult{})

	r := <-done
	if r.err != nil {
		t.Fatalf("Resume: %v", r.err)
	}
	s := r.s.(*Session)
	c := collect(s)
	c.waitFor(t, "the replayed text", func(e event.Event) bool {
		td, ok := e.(event.TextDelta)
		return ok && td.Text == "from two weeks ago"
	})
	for _, e := range c.all() {
		if !e.Envelope().Replay {
			t.Errorf("%T produced during session/load is not marked as replay", e)
		}
		if e.Ping() != event.PingNone {
			t.Errorf("%T replayed from history must not ping", e)
		}
	}
	if s.Replaying() {
		t.Error("Replaying should be false once session/load resolves")
	}
}

// TestOpenClawIsBoundToOneSessionAtLaunch: the bridge takes its session key as
// a process argument, so a different key needs a different process. Saying that
// beats silently talking to the wrong session.
func TestOpenClawIsBoundToOneSessionAtLaunch(t *testing.T) {
	f := newFakeAgent(t)
	opts := testOptions(t, adapter.OpenClaw)
	opts.SessionKey = "agent:main:main"
	a := dial(t, f, opts, AgentCapabilities{LoadSession: true})

	_, err := a.Resume(context.Background(), adapter.SessionRef{Native: "agent:other:other"},
		adapter.SessionOptions{Workspace: "/tmp/ws"})
	if !errors.Is(err, adapter.ErrSessionNotFound) {
		t.Fatalf("Resume returned %v, want ErrSessionNotFound", err)
	}
	if !strings.Contains(err.Error(), "--session") {
		t.Errorf("the error should name the flag that binds the process, got %v", err)
	}
}

// TestWorkspaceMustBeAbsolute: ACP requires it and the others assume it.
func TestWorkspaceMustBeAbsolute(t *testing.T) {
	f := newFakeAgent(t)
	a := dial(t, f, testOptions(t, adapter.OpenCode), AgentCapabilities{})
	for _, ws := range []string{"", "relative/path"} {
		if _, err := a.Start(context.Background(), adapter.SessionOptions{Workspace: ws}); err == nil {
			t.Errorf("Start accepted workspace %q", ws)
		}
	}
}

// TestHTTPMCPServerNeedsTheCapability: a URL server the agent cannot take is
// refused loudly, because a silently missing MCP server looks like a broken
// tool.
func TestHTTPMCPServerNeedsTheCapability(t *testing.T) {
	f := newFakeAgent(t)
	a := dial(t, f, testOptions(t, adapter.OpenCode), AgentCapabilities{})
	_, err := a.Start(context.Background(), adapter.SessionOptions{
		Workspace:  "/tmp/ws",
		MCPServers: []adapter.MCPServer{{Name: "remote", URL: "https://mcp.example/sse"}},
	})
	if !errors.Is(err, ErrTransportUnsupported) {
		t.Fatalf("Start returned %v, want ErrTransportUnsupported", err)
	}
}

// TestPromptCapabilitiesGateContent: a photo from the glasses cannot enter a
// prompt on a runtime that never advertised image support.
func TestPromptCapabilitiesGateContent(t *testing.T) {
	ctx := context.Background()
	f := newFakeAgent(t)
	a := dial(t, f, testOptions(t, adapter.OpenCode), AgentCapabilities{})
	s := startSession(t, f, a, "sess_1")

	_, err := s.Send(ctx, adapter.Turn{Blocks: []adapter.Block{{
		Kind: adapter.BlockImage, Data: []byte{1, 2, 3}, MIMEType: "image/png",
	}}})
	if !errors.Is(err, adapter.ErrUnsupported) {
		t.Fatalf("Send accepted an image on a runtime that did not advertise one: %v", err)
	}
	expectQuiet(t, f)

	f2 := newFakeAgent(t)
	a2 := dial(t, f2, testOptions(t, adapter.OpenCode), AgentCapabilities{
		PromptCapabilities: PromptCapabilities{Image: true},
	})
	s2 := startSession(t, f2, a2, "sess_2")
	if _, err := s2.Send(ctx, adapter.Turn{Blocks: []adapter.Block{{
		Kind: adapter.BlockImage, Data: []byte{1, 2, 3}, MIMEType: "image/png",
	}}}); err != nil {
		t.Fatalf("Send with image support advertised: %v", err)
	}
	m := f2.next()
	var p promptParams
	_ = json.Unmarshal(m.Params, &p)
	if p.Prompt[0].Type != "image" || p.Prompt[0].Data != "AQID" {
		t.Errorf("image block = %+v", p.Prompt[0])
	}
	f2.respond(m.ID, promptResult{StopReason: "end_turn"})
}

// TestSetModeAndSetModel: never send back something that was not offered, and
// keep the one UNSTABLE method behind an explicit opt-in.
func TestSetModeAndSetModel(t *testing.T) {
	ctx := context.Background()
	f := newFakeAgent(t)
	a := dial(t, f, testOptions(t, adapter.OpenCode), AgentCapabilities{})
	s := startSession(t, f, a, "sess_1")

	if err := s.SetMode(ctx, "no-such-mode"); err == nil {
		t.Error("SetMode accepted a mode the agent never advertised")
	}
	done := make(chan error, 1)
	go func() { done <- s.SetMode(ctx, "code") }()
	m := f.next()
	if m.Method != methodSetMode {
		t.Fatalf("client sent %q, want session/set_mode", m.Method)
	}
	f.respond(m.ID, map[string]any{})
	if err := <-done; err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	if s.CurrentMode() != "code" {
		t.Errorf("CurrentMode = %q", s.CurrentMode())
	}

	if err := s.SetModel(ctx, "anything"); err == nil || !strings.Contains(err.Error(), "UNSTABLE") {
		t.Errorf("SetModel should refuse by default and say why, got %v", err)
	}
}

// TestSetModelWhenOptedIn checks the opt-in path still validates the id.
func TestSetModelWhenOptedIn(t *testing.T) {
	ctx := context.Background()
	f := newFakeAgent(t)
	opts := testOptions(t, adapter.OpenCode)
	opts.AllowUnstableSetModel = true
	a := dial(t, f, opts, AgentCapabilities{})

	type res struct {
		s   adapter.Session
		err error
	}
	done := make(chan res, 1)
	go func() {
		s, err := a.Start(ctx, adapter.SessionOptions{Workspace: "/tmp/ws"})
		done <- res{s, err}
	}()
	m := f.next()
	f.respond(m.ID, newSessionResult{
		SessionID: "sess_1",
		Models: &SessionModelState{
			CurrentModelID:  "fast",
			AvailableModels: []ModelInfo{{ModelID: "fast", Name: "Fast"}, {ModelID: "deep", Name: "Deep"}},
		},
	})
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	s := r.s.(*Session)

	if err := s.SetModel(ctx, "nope"); err == nil {
		t.Error("SetModel accepted a model that was not offered")
	}
	go func() { _ = s.SetModel(ctx, "deep") }()
	m2 := f.next()
	if m2.Method != methodSetModel {
		t.Fatalf("client sent %q, want session/set_model", m2.Method)
	}
	f.respond(m2.ID, map[string]any{})
}

// TestRefusesFSAndTerminal: Relay declares all three client capabilities false,
// and a call that arrives anyway is refused rather than faked.
func TestRefusesFSAndTerminal(t *testing.T) {
	f := newFakeAgent(t)
	a := dial(t, f, testOptions(t, adapter.OpenClaw), AgentCapabilities{})
	_ = startSession(t, f, a, "sess_1")

	for i, method := range RefusedClientMethods() {
		f.request(500+i, method, map[string]any{"sessionId": "sess_1", "path": "/etc/passwd"})
		m := f.next()
		if m.Error == nil || m.Error.Code != codeMethodNotFound {
			t.Fatalf("%s was answered with %+v, want -32601", method, m.Error)
		}
		if !strings.Contains(m.Error.Message, "not advertised") {
			t.Errorf("%s refusal should say why: %q", method, m.Error.Message)
		}
	}
	refused := a.Refused()
	for _, method := range RefusedClientMethods() {
		if refused[method] != 1 {
			t.Errorf("Refused()[%s] = %d", method, refused[method])
		}
	}
}

// TestExtensionMethodsAreLoggedNotDropped: ACP's `_` extensions are invisible
// to the schema, so this counter is the only way we would ever find out a
// runtime shipped its own steering.
func TestExtensionMethodsAreLoggedNotDropped(t *testing.T) {
	f := newFakeAgent(t)
	a := dial(t, f, testOptions(t, adapter.OpenClaw), AgentCapabilities{})

	f.notify("_openclaw/telemetry", map[string]any{"n": 1})
	f.request(900, "_openclaw/steer", map[string]any{"text": "actually do this"})
	m := f.next()
	if m.Error == nil || m.Error.Code != codeMethodNotFound {
		t.Fatalf("an unknown extension request must be refused, got %+v", m)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ext := a.Extensions()
		if ext["_openclaw/telemetry"] == 1 && ext["_openclaw/steer"] == 1 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("Extensions() = %v", a.Extensions())
}

// TestClosingTheConnectionEndsTheSession: a dead runtime reaches a consumer
// that is only reading events.
func TestClosingTheConnectionEndsTheSession(t *testing.T) {
	ctx := context.Background()
	f := newFakeAgent(t)
	a := dial(t, f, testOptions(t, adapter.Hermes), AgentCapabilities{})
	s := startSession(t, f, a, "sess_1")
	c := collect(s)

	_, _ = s.Send(ctx, adapter.Turn{Text: "one"})
	_ = f.next()
	f.close()

	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the event channel did not close when the connection died")
	}
	comps := c.completions()
	if len(comps) == 0 || comps[len(comps)-1].StopReason != event.StopError {
		t.Errorf("a turn interrupted by a dead connection should complete with an error, got %+v", comps)
	}
	if _, err := s.Send(ctx, adapter.Turn{Text: "again"}); !errors.Is(err, adapter.ErrSessionClosed) {
		t.Errorf("Send on a closed session returned %v", err)
	}
}
