package orchestrator_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/orchestrator"
	"github.com/luthor007/relay/relayd/internal/routing"
	"github.com/luthor007/relay/relayd/internal/summarize"
)

// fakeSessions is the registry, without one.
type fakeSessions struct {
	mu      sync.Mutex
	live    []orchestrator.Session
	started []string
	stopped []string
}

func (f *fakeSessions) List(context.Context) ([]orchestrator.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]orchestrator.Session(nil), f.live...), nil
}

func (f *fakeSessions) Start(_ context.Context, runtime, workspace, prompt string) (orchestrator.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, prompt)
	s := orchestrator.Session{ID: "sess_new", Runtime: "claude-code", Subject: prompt, Workspace: workspace}
	f.live = append(f.live, s)
	return s, nil
}

func (f *fakeSessions) Send(context.Context, string, string) error { return nil }

func (f *fakeSessions) Stop(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, id)
	return nil
}

func (f *fakeSessions) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started)
}

// TestEveryToolDescriptionSaysWhenToCallIt.
//
// The allowlist in ORCHESTRATOR.md §3b and the "prefer adding to a session over
// starting a second" judgement are not in a system prompt here — they are in
// the tool descriptions, next to the schema the model is already reading. That
// only works if every description actually carries a trigger clause, and a
// description that drifts back to "what it does" is the kind of regression
// nobody notices until the should-call rate quietly drops.
func TestEveryToolDescriptionSaysWhenToCallIt(t *testing.T) {
	box := orchestrator.Toolbox(&fakeSessions{}, nil)
	if len(box) == 0 {
		t.Fatal("no tools")
	}
	if err := box.Validate(); err != nil {
		t.Fatalf("the shipped tool set does not validate: %v", err)
	}
	for _, b := range box.Decls() {
		if !strings.Contains(b.Description, "Call this ") {
			t.Errorf("tool %q describes what it does but never when to call it:\n  %s",
				b.Name, b.Description)
		}
	}
}

// everything is the fully-equipped orchestrator: every dependency present, so
// every tool is built. Most tests want a narrower one; this is for the
// properties that have to hold across the whole surface.
func everything() orchestrator.Deps {
	return orchestrator.Deps{
		Sessions:     &fakeSessions{},
		Memory:       stubMemory{},
		Notebook:     &stubNotebook{},
		Capabilities: &stubCapabilities{},
		Skills:       orchestrator.NewSkillBook(),
	}
}

// TestTheToolSetStaysSmall. Anthropic's tool guidance names a bloated set with
// ambiguous decision points as the most common failure, and this is the only
// form of that rule a test can hold: a ceiling, with the review forced when
// somebody wants to cross it.
//
// Ten rather than six now that capabilities, memory writes and skills are here.
// The number is not the point; being made to argue for it is. The ambiguity
// half of the rule is enforced for real by ValidateTools, which rejects two
// tools nothing distinguishes.
func TestTheToolSetStaysSmall(t *testing.T) {
	box := orchestrator.ToolboxFor(everything())
	if len(box) > 10 {
		t.Errorf("the orchestrator has %d tools; it drives runtimes, it does not become one", len(box))
	}
	if err := box.Validate(); err != nil {
		t.Fatalf("the full tool set does not validate: %v", err)
	}
	for _, d := range box.Decls() {
		if !strings.Contains(d.Description, "Call this ") {
			t.Errorf("tool %q never says when to call it:\n  %s", d.Name, d.Description)
		}
	}
}

// TestNoToolExecutes is the architecture, as a test.
//
// Relay chooses which agent does the work and gives it what it needs; it does
// not drive the browser, it tells an agent with a browser what to do. Every
// tool is a read, an instruction to a session, or a change to what is available
// — and there is no fourth category to put an executing tool in without first
// inventing a name for it and saying why.
func TestNoToolExecutes(t *testing.T) {
	box := orchestrator.ToolboxFor(everything())
	instructing := 0
	for _, d := range box.Decls() {
		kind, ok := orchestrator.KindOf(d.Name)
		if !ok {
			t.Errorf("tool %q has no Kind, so nobody decided whether it executes", d.Name)
			continue
		}
		switch kind {
		case orchestrator.KindRead, orchestrator.KindProvision:
		case orchestrator.KindInstruct:
			instructing++
		default:
			t.Errorf("tool %q is a %q, which is not one of the three", d.Name, kind)
		}
	}
	if instructing == 0 {
		t.Error("nothing instructs a session, so no work can ever happen")
	}
}

type stubNotebook struct {
	mu    sync.Mutex
	facts []orchestrator.Fact
}

func (s *stubNotebook) Remember(_ context.Context, f orchestrator.Fact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.facts = append(s.facts, f)
	return nil
}

type stubCapabilities struct {
	mu      sync.Mutex
	granted []string
}

func (s *stubCapabilities) List(context.Context) ([]orchestrator.Capability, error) {
	return []orchestrator.Capability{
		{Name: "browser", Half: "read", Summary: "drive a browser that is already signed in"},
		{Name: "grafana", Half: "read", Summary: "read the dashboards on your servers"},
	}, nil
}

func (s *stubCapabilities) Grant(_ context.Context, name, half, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.granted = append(s.granted, name+":"+half)
	return nil
}

type stubMemory struct{ hits []orchestrator.Hit }

func (s stubMemory) Search(context.Context, string, int) ([]orchestrator.Hit, error) {
	return s.hits, nil
}

// TestStatusNeverWakesTheBigModel is the cost story. ORCHESTRATOR.md §3b puts
// roughly 70% of utterances on the small path, and that ratio is what makes
// "routing calls included" affordable at $39 in CLOUD.md §1.
func TestStatusNeverWakesTheBigModel(t *testing.T) {
	tr := &countingTransport{}
	o, err := orchestrator.New(orchestrator.Options{
		Big:  testProvider(t, tr),
		Deps: orchestrator.Deps{Sessions: &fakeSessions{}},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, utterance := range []string{"what is it doing", "is it done", "stop", "status"} {
		out, err := o.Handle(context.Background(),
			orchestrator.Utterance{Text: utterance, Session: "sess_1"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if out.Escalated {
			t.Errorf("%q escalated; it is on the allowlist as %s", utterance, out.Class)
		}
	}
	if tr.calls.Load() != 0 {
		t.Errorf("the big model was called %d times for allowlisted utterances", tr.calls.Load())
	}
}

// TestTheAcknowledgementIsSpokenBeforeTheWorkStarts. SYSTEM.md §7b: the agent
// costs 1–10 s and eight seconds of silence reads as broken no matter what
// arrives afterwards. So the ack cannot wait on the model call, and the only
// way to know it does not is to look at the order.
func TestTheAcknowledgementIsSpokenBeforeTheWorkStarts(t *testing.T) {
	var spokeFirst atomic.Bool
	tr := &countingTransport{
		script: []string{textReply("Started it.")},
		onCall: func() {
			if !spokeFirst.Load() {
				t.Error("the big model was called before anything was said out loud")
			}
		},
	}

	o, err := orchestrator.New(orchestrator.Options{
		Big:  testProvider(t, tr),
		Deps: orchestrator.Deps{Sessions: &fakeSessions{}},
	})
	if err != nil {
		t.Fatal(err)
	}

	var lines []summarize.Speech
	out, err := o.Handle(context.Background(),
		orchestrator.Utterance{Text: "refactor the auth module", Session: "sess_1"},
		func(s summarize.Speech) {
			lines = append(lines, s)
			spokeFirst.Store(true)
		})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Escalated {
		t.Fatalf("a refactor did not escalate; class = %s", out.Class)
	}
	if len(lines) < 2 {
		t.Fatalf("spoke %d lines, want an ack and an outcome", len(lines))
	}
	if lines[0].Moment != summarize.MomentAck {
		t.Errorf("the first line was %q, not the acknowledgement", lines[0].Moment)
	}
	if strings.TrimSpace(lines[0].Text) == "" {
		t.Error("the acknowledgement was empty, which is the silence it exists to prevent")
	}
}

// TestAConsequentialToolIsAskedAndAReadIsNot is ORCHESTRATOR.md §4b's rule,
// and the direction of the default is the point: with no approver wired, a
// start is asked and therefore denied, while a list runs.
func TestAConsequentialToolIsAskedAndAReadIsNot(t *testing.T) {
	sessions := &fakeSessions{}
	tr := &countingTransport{script: []string{
		toolUse("call_1", "start_session", `{"prompt":"delete the branch"}`),
		textReply("I did not start it."),
	}}

	var asked int32
	o, err := orchestrator.New(orchestrator.Options{
		Big:  testProvider(t, tr),
		Deps: orchestrator.Deps{Sessions: sessions},
		Emit: func(e event.Event) {
			if q, ok := e.(*event.NeedsInput); ok {
				atomic.AddInt32(&asked, 1)
				// Deny, the way a user would.
				go func() {
					_ = q.Reply(context.Background(), event.Reply{
						OptionID: "deny", Decision: event.DecisionDeny,
					})
				}()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := o.Handle(context.Background(),
		orchestrator.Utterance{Text: "start work on the payments branch", Session: "sess_1"}, nil); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&asked) != 1 {
		t.Errorf("asked %d times about starting a session, want 1", asked)
	}
	if sessions.startCount() != 0 {
		t.Error("a denied start_session started a session anyway")
	}
}

func TestAReadOnlyToolRunsWithoutAsking(t *testing.T) {
	tr := &countingTransport{script: []string{
		toolUse("call_1", "list_sessions", `{}`),
		textReply("Two are running."),
	}}

	var asked int32
	o, err := orchestrator.New(orchestrator.Options{
		Big: testProvider(t, tr),
		Deps: orchestrator.Deps{Sessions: &fakeSessions{live: []orchestrator.Session{
			{ID: "sess_1", Runtime: "claude-code", Subject: "payments", State: "running"},
		}}},
		Emit: func(e event.Event) {
			if _, ok := e.(*event.NeedsInput); ok {
				atomic.AddInt32(&asked, 1)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.Handle(context.Background(),
		orchestrator.Utterance{Text: "what should we do about the payments work", Session: "sess_1"}, nil); err != nil {
		t.Fatal(err)
	}
	if asked != 0 {
		t.Errorf("listing sessions asked for approval %d times; it changes nothing", asked)
	}
	if !strings.Contains(tr.body(1), "payments") {
		t.Errorf("the session list never reached the model:\n%s", tr.body(1))
	}
}

// TestNoBigModelSaysSoRatherThanFailing: an install with no work model
// configured still answers status and control, and says one clear sentence
// about the rest instead of erroring at someone wearing glasses.
func TestNoBigModelSaysSoRatherThanFailing(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Options{Deps: orchestrator.Deps{Sessions: &fakeSessions{}}})
	if err != nil {
		t.Fatal(err)
	}
	var lines []summarize.Speech
	out, err := o.Handle(context.Background(),
		orchestrator.Utterance{Text: "refactor the auth module"},
		func(s summarize.Speech) { lines = append(lines, s) })
	if err != nil {
		t.Fatal(err)
	}
	if out.Class != routing.ClassEscalate {
		t.Errorf("class = %s", out.Class)
	}
	if len(lines) != 2 || strings.TrimSpace(lines[1].Text) == "" {
		t.Fatalf("spoke %+v; the user has to be told something", lines)
	}
}

// TestASpokenOutcomeNeverClaimsADeniedToolRan is ORCHESTRATOR.md §3b's
// narration drift rule where it actually bites.
//
// A model that has just been refused permission and says "Done — I started it"
// is not a hypothetical: it is the ordinary shape of a turn where the refusal
// arrived as a tool result the model then summarised optimistically. The events
// say the tool failed, and what the events say wins — which is only true
// because the small model is handed events and has no path to the transcript.
func TestASpokenOutcomeNeverClaimsADeniedToolRan(t *testing.T) {
	sessions := &fakeSessions{}
	tr := &countingTransport{script: []string{
		toolUse("call_1", "start_session", `{"prompt":"deploy to prod"}`),
		textReply("Done — I started the deploy session."),
	}}

	o, err := orchestrator.New(orchestrator.Options{
		Big:  testProvider(t, tr),
		Deps: orchestrator.Deps{Sessions: sessions},
		Emit: func(e event.Event) {
			if q, ok := e.(*event.NeedsInput); ok {
				go func() {
					_ = q.Reply(context.Background(), event.Reply{
						OptionID: "deny", Decision: event.DecisionDeny,
					})
				}()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var spoken []summarize.Speech
	if _, err := o.Handle(context.Background(),
		orchestrator.Utterance{Text: "deploy to prod", Session: "sess_1"},
		func(s summarize.Speech) { spoken = append(spoken, s) }); err != nil {
		t.Fatal(err)
	}
	if sessions.startCount() != 0 {
		t.Fatal("the denied session was started")
	}

	final := spoken[len(spoken)-1]
	if !final.Grounded {
		t.Error("the outcome was spoken without being grounded in an event")
	}
	for _, claim := range []string{"done", "started"} {
		if strings.Contains(strings.ToLower(final.Text), claim) {
			t.Errorf("spoke %q after the tool was denied; the events said it failed", final.Text)
		}
	}
}

// TestTheRunLooksLikeASessionOnTheBus — the console, the pinger and the phone
// all work on event.Event, and none of them should need to know this run
// happened inside relayd rather than inside a runtime.
func TestTheRunLooksLikeASessionOnTheBus(t *testing.T) {
	tr := &countingTransport{script: []string{
		toolUse("call_1", "list_sessions", `{}`),
		textReply("Nothing running."),
	}}

	var mu sync.Mutex
	seen := map[event.Kind]int{}
	sessions := map[string]bool{}

	o, err := orchestrator.New(orchestrator.Options{
		Big:  testProvider(t, tr),
		Deps: orchestrator.Deps{Sessions: &fakeSessions{}},
		Emit: func(e event.Event) {
			mu.Lock()
			defer mu.Unlock()
			seen[e.Kind()]++
			sessions[e.Envelope().Session] = true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.Handle(context.Background(),
		orchestrator.Utterance{Text: "what should I work on next", Session: "sess_42"}, nil); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, k := range []event.Kind{
		event.KindTurnStarted, event.KindToolStarted, event.KindToolOutput, event.KindTurnCompleted,
	} {
		if seen[k] == 0 {
			t.Errorf("never emitted %s; got %v", k, seen)
		}
	}
	if !sessions["sess_42"] || len(sessions) != 1 {
		t.Errorf("events carried sessions %v, want only sess_42", sessions)
	}
}

func TestConsequentialIsAboutReversibility(t *testing.T) {
	for tool, want := range map[string]bool{
		orchestrator.ToolStartSession:  true,
		orchestrator.ToolStopSession:   true,
		orchestrator.ToolListSessions:  false,
		orchestrator.ToolSearchMemory:  false,
		orchestrator.ToolSendToSession: false,
	} {
		if got := orchestrator.Consequential(tool); got != want {
			t.Errorf("Consequential(%q) = %v, want %v", tool, got, want)
		}
	}
}

func TestMemoryToolReportsAnEmptyIndexRatherThanNothing(t *testing.T) {
	box := orchestrator.Toolbox(nil, stubMemory{})
	// describe_runtime is always present — it depends on nothing — so the
	// dependency-gated tool is the second one.
	if len(box) != 2 {
		t.Fatalf("%d tools", len(box))
	}
	res, err := runTool(t, box, context.Background(), llm.ToolCall{ID: "c", Name: orchestrator.ToolSearchMemory, Input: []byte(`{"query":"crc"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Content) == "" {
		t.Error("an empty result set returned an empty string, which reads as a broken tool")
	}
}

func TestMemoryHitsAreRenderedForTheModel(t *testing.T) {
	when := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	box := orchestrator.Toolbox(nil, stubMemory{hits: []orchestrator.Hit{
		{SessionID: "s1", Title: "CRC-16 investigation", Snippet: "it was MODBUS", When: when},
	}})
	res, err := runTool(t, box, context.Background(), llm.ToolCall{ID: "c", Name: orchestrator.ToolSearchMemory, Input: []byte(`{"query":"crc"}`)})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CRC-16 investigation", "MODBUS", "2026-08-01"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("the rendered hit lost %q:\n%s", want, res.Content)
		}
	}
}
