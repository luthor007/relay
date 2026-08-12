package mcpbridge_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/apps"
	"github.com/luthor007/relay/relayd/internal/apps/mcpbridge"
	"github.com/luthor007/relay/relayd/internal/mcp"
)

// gateway wires a real [mcp.Gateway] with the bridge on it, because the claim
// being tested is "same registry as the gateway; there is no second mechanism".
// A test that called Provider.Tools directly would pass while the tools were
// invisible to every agent on the machine.
func gateway(t *testing.T, cat *catalogue, inv mcpbridge.Invoker, confirm mcp.Confirmer) (*mcp.Gateway, *mcpbridge.Provider) {
	t.Helper()
	p := mcpbridge.New(mcpbridge.Options{Catalogue: cat, Invoke: inv})
	gw := mcp.NewGateway(mcp.Options{
		Grants:  mcpbridge.Grants{Catalogue: cat},
		Confirm: confirm,
		Now:     func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	})
	gw.Register(context.Background(), p)
	return gw, p
}

func toolNames(list []mcp.Tool) []string {
	out := make([]string, 0, len(list))
	for _, t := range list {
		out = append(out, t.Name)
	}
	return out
}

// APP-PLATFORM.md §4: "An installed app is automatically exposed to the user's
// agent as an MCP tool, so 'wrap up the standup' works without a wake phrase."
func TestEveryInstalledAppIsAutomaticallyATool(t *testing.T) {
	ctx := context.Background()
	cat := &catalogue{list: []apps.Installed{install(t, standupManifest)}}
	gw, _ := gateway(t, cat, &recorder{}, nil)

	got := toolNames(gw.Tools(ctx))
	if len(got) != 1 {
		t.Fatalf("an installed app with a tool trigger should be one tool; got %v", got)
	}
	want := "app_dev_alexis_standup_notes_run"
	if got[0] != want {
		t.Fatalf("tool name is %q, want %q", got[0], want)
	}
	// internal/mcp's note: the strictest of the five runtime client validators
	// accepts only [A-Za-z0-9_-], and an app id is reverse-DNS.
	if !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(got[0]) {
		t.Fatalf("%q is not a name every runtime's MCP client will accept", got[0])
	}

	tool, ok := gw.Lookup(ctx, want)
	if !ok {
		t.Fatal("the tool is not on the bus")
	}
	if tool.Title != "Standup Notes" {
		t.Fatalf("title is %q", tool.Title)
	}
	// The author's own sentence, verbatim: it is what the reviewer read and what
	// the install sheet showed.
	if !strings.HasPrefix(tool.Description, "Summarise the most recent meeting into decisions and commitments.") {
		t.Fatalf("description does not lead with the manifest's own sentence: %q", tool.Description)
	}
	if !strings.Contains(tool.Description, "written by Alexis Massicotte") {
		t.Fatalf("an agent choosing between apps should see whose code it is: %q", tool.Description)
	}
}

func TestAnAppThatDidNotAskToBeCallableIsNotATool(t *testing.T) {
	ctx := context.Background()
	cat := &catalogue{list: []apps.Installed{install(t, quietManifest)}}
	gw, _ := gateway(t, cat, &recorder{}, nil)
	if got := gw.All(ctx); len(got) != 0 {
		t.Fatalf("an app with no tool trigger became a tool: %v", toolNames(got))
	}
}

// A tool an agent can see is a tool it will call. With no runtime on the box
// nothing can wake an app, so nothing is offered — and the reason is a sentence
// rather than an empty list.
func TestWithNoRuntimeNoAppIsATool(t *testing.T) {
	ctx := context.Background()
	cat := &catalogue{list: []apps.Installed{install(t, standupManifest)}}
	p := mcpbridge.New(mcpbridge.Options{Catalogue: cat})
	if got := p.Tools(ctx); len(got) != 0 {
		t.Fatalf("apps were offered with nothing able to run them: %v", toolNames(got))
	}
	if !strings.Contains(p.Degraded(), "no app runtime") {
		t.Fatalf("Degraded should say why the list is empty, got %q", p.Degraded())
	}
	working := mcpbridge.New(mcpbridge.Options{Catalogue: cat, Invoke: &recorder{}})
	if working.Degraded() != "" {
		t.Fatalf("a working provider should not be degraded: %q", working.Degraded())
	}
}

func TestCallingTheToolWakesTheAppWithTheAgentsArguments(t *testing.T) {
	ctx := context.Background()
	cat := &catalogue{list: []apps.Installed{install(t, standupManifest)}}
	rec := &recorder{outcome: mcpbridge.Outcome{Spoken: []string{"Saved. Three commitments."}}}
	gw, _ := gateway(t, cat, rec, nil)

	res, err := gw.Call(ctx, mcp.Call{
		Tool:      "app_dev_alexis_standup_notes_run",
		Arguments: map[string]any{"request": "wrap up the standup"},
		Session:   "sess-1",
		Runtime:   "claude-code",
	})
	if err != nil {
		t.Fatalf("the call failed: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("the app was woken %d times", len(rec.calls))
	}
	if rec.calls[0].Type != apps.TriggerTool {
		t.Fatalf("the app was woken by %q, want a tool trigger", rec.calls[0].Type)
	}
	if got := rec.calls[0].Arguments["request"]; got != "wrap up the standup" {
		t.Fatalf("the agent's arguments did not reach the app: %v", rec.calls[0].Arguments)
	}
	if res.Text != "Saved. Three commitments." {
		t.Fatalf("the agent read %q", res.Text)
	}
	if res.IsError {
		t.Fatal("a successful run was reported as an error")
	}
}

// ORCHESTRATOR.md §5's two halves meeting: the phone drew the view natively and
// the agent, which has no screen, reads the same view as text.
func TestTheAgentReadsTheViewThePhoneDrew(t *testing.T) {
	ctx := context.Background()
	cat := &catalogue{list: []apps.Installed{install(t, standupManifest)}}
	view := mustParse(t, `{"vocabulary":1,"blocks":[
		{"kind":"card","title":"Standup","fields":[{"label":"Length","value":"12 min"}]},
		{"kind":"speak","text":"Saved. Three commitments."}
	]}`)
	rec := &recorder{outcome: mcpbridge.Outcome{
		Views: []mcpbridge.View{view},
		// The speaker delivered the same sentence the view carried. Reading it
		// to the agent twice would have the model believe the app repeated
		// itself.
		Spoken: []string{"Saved. Three commitments."},
	}}
	gw, _ := gateway(t, cat, rec, nil)

	res, err := gw.Call(ctx, mcp.Call{Tool: "app_dev_alexis_standup_notes_run"})
	if err != nil {
		t.Fatal(err)
	}
	want := "Standup\nLength: 12 min\nSaved. Three commitments."
	if res.Text != want {
		t.Fatalf("the agent read:\n%s\nwant:\n%s", res.Text, want)
	}
	structured, ok := res.Structured.(map[string]any)
	if !ok {
		t.Fatalf("the structured half is %T", res.Structured)
	}
	if structured["app"] != "dev.alexis.standup-notes" || structured["status"] != "completed" {
		t.Fatalf("structured result is %v", structured)
	}
	if _, ok := structured["views"]; !ok {
		t.Fatal("the view should be in the structured half, so a console can redraw it")
	}
}

// De-duplication runs between the view and the speaker, never inside one
// source: a list with two identical rows is an app saying something twice on
// purpose, and collapsing it would misreport what the user was shown.
func TestRepeatedLinesInsideOneViewSurvive(t *testing.T) {
	ctx := context.Background()
	cat := &catalogue{list: []apps.Installed{install(t, standupManifest)}}
	view := mustParse(t, `{"vocabulary":1,"blocks":[{"kind":"list","items":[
		{"title":"Reply to Sam"},{"title":"Reply to Sam"}
	]}]}`)
	rec := &recorder{outcome: mcpbridge.Outcome{Views: []mcpbridge.View{view}}}
	gw, _ := gateway(t, cat, rec, nil)
	res, err := gw.Call(ctx, mcp.Call{Tool: "app_dev_alexis_standup_notes_run"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(res.Text, "Reply to Sam") != 2 {
		t.Fatalf("the agent read %q; the user was shown two rows", res.Text)
	}
}

// An app that files a note and says nothing has a real outcome. An empty tool
// result invites the model to decide the call failed.
func TestAnAppThatSaidNothingSaysSo(t *testing.T) {
	ctx := context.Background()
	cat := &catalogue{list: []apps.Installed{install(t, standupManifest)}}
	gw, _ := gateway(t, cat, &recorder{}, nil)
	res, err := gw.Call(ctx, mcp.Call{Tool: "app_dev_alexis_standup_notes_run"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Standup Notes ran and had nothing to say." {
		t.Fatalf("the agent read %q", res.Text)
	}
	if res.IsError {
		t.Fatal("saying nothing is not a failure")
	}
}

// MCP keeps these apart deliberately: a tool that ran and failed is something
// the model can react to; a tool that could not be called at all is a protocol
// error.
func TestTheAppsOwnFailureIsAResultAndRelaydsIsAnError(t *testing.T) {
	ctx := context.Background()
	cat := &catalogue{list: []apps.Installed{install(t, standupManifest)}}

	rec := &recorder{outcome: mcpbridge.Outcome{Failed: "TypeError: meeting.transcript is undefined"}}
	gw, _ := gateway(t, cat, rec, nil)
	res, err := gw.Call(ctx, mcp.Call{Tool: "app_dev_alexis_standup_notes_run"})
	if err != nil {
		t.Fatalf("an app that threw should be a result, not an error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Text, "TypeError") {
		t.Fatalf("the agent read %q (isError=%v)", res.Text, res.IsError)
	}

	broken := &recorder{failWith: errors.New("the sandbox would not start")}
	gw2, _ := gateway(t, cat, broken, nil)
	if _, err := gw2.Call(ctx, mcp.Call{Tool: "app_dev_alexis_standup_notes_run"}); err == nil {
		t.Fatal("relayd failing to run the app should be an error the agent cannot quote")
	}
}

func TestATimeoutSaysWhatItWas(t *testing.T) {
	ctx := context.Background()
	cat := &catalogue{list: []apps.Installed{install(t, standupManifest)}}
	rec := &recorder{outcome: mcpbridge.Outcome{TimedOut: true, Budget: 60 * time.Second}}
	gw, _ := gateway(t, cat, rec, nil)
	res, err := gw.Call(ctx, mcp.Call{Tool: "app_dev_alexis_standup_notes_run"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "1m0s budget") {
		t.Fatalf("the agent read %q", res.Text)
	}
}

// An agent holds a tool list for a whole turn, so the window between listing and
// calling is real.
func TestAnAppRemovedBetweenListAndCallDoesNotRun(t *testing.T) {
	ctx := context.Background()
	cat := &catalogue{list: []apps.Installed{install(t, standupManifest)}}
	rec := &recorder{}
	p := mcpbridge.New(mcpbridge.Options{Catalogue: cat, Invoke: rec})
	tools := p.Tools(ctx)
	if len(tools) != 1 {
		t.Fatalf("expected one tool, got %v", toolNames(tools))
	}

	cat.remove("dev.alexis.standup-notes")
	if _, err := tools[0].Handler(ctx, mcp.Call{Tool: tools[0].Name}); !errors.Is(err, mcpbridge.ErrGone) {
		t.Fatalf("a removed app ran anyway, or failed for the wrong reason: %v", err)
	}
	if len(rec.ran) != 0 {
		t.Fatalf("a removed app was invoked: %v", rec.ran)
	}
}

// ORCHESTRATOR.md §4b rule 3, reached through the app path: an app that can
// reach a host outside this machine confirms at the glasses, every time — and a
// gateway with no confirmation path refuses rather than running it.
func TestAnAppThatCanLeaveTheMachineConfirmsOutLoud(t *testing.T) {
	ctx := context.Background()
	cat := &catalogue{list: []apps.Installed{install(t, filerManifest)}}
	rec := &recorder{}
	gw, _ := gateway(t, cat, rec, nil)

	tool, ok := gw.Lookup(ctx, "app_dev_alexis_issue_filer_run")
	if !ok {
		t.Fatal("the app is not on the bus")
	}
	if !tool.Consequential() {
		t.Fatal("an app with net.fetch has effects outside this machine and must confirm")
	}
	if !strings.Contains(tool.Consequence, "api.linear.app") {
		t.Fatalf("the spoken confirmation must name the hosts: %q", tool.Consequence)
	}

	_, err := gw.Call(ctx, mcp.Call{Tool: tool.Name})
	if !errors.Is(err, mcp.ErrNoConfirmer) {
		t.Fatalf("a consequential app ran with nobody to ask: %v", err)
	}
	if len(rec.ran) != 0 {
		t.Fatalf("it ran before the confirmation: %v", rec.ran)
	}

	asked := 0
	gw2, _ := gateway(t, cat, rec, mcp.ConfirmerFunc(func(context.Context, mcp.Confirmation) error {
		asked++
		return nil
	}))
	if _, err := gw2.Call(ctx, mcp.Call{Tool: tool.Name}); err != nil {
		t.Fatalf("a confirmed call failed: %v", err)
	}
	if asked != 1 || len(rec.ran) != 1 {
		t.Fatalf("asked %d times, ran %d times", asked, len(rec.ran))
	}
}

// A note in the user's own memory and a sentence in the user's own ear stop at
// the machine boundary. Only egress crosses it.
func TestAnAppThatCannotLeaveTheMachineDoesNotInterrupt(t *testing.T) {
	ctx := context.Background()
	cat := &catalogue{list: []apps.Installed{install(t, standupManifest)}}
	gw, _ := gateway(t, cat, &recorder{}, nil)
	tool, _ := gw.Lookup(ctx, "app_dev_alexis_standup_notes_run")
	if tool.Consequential() {
		t.Fatalf("this app writes a note and speaks; neither leaves the box: %q", tool.Consequence)
	}
}

// A declined net.fetch leaves the manifest's hosts behind: the allowlist is read
// from the grant, never from the manifest, so there is one place where
// "declared" becomes "granted".
func TestADeclinedNetFetchRemovesTheConsequence(t *testing.T) {
	app := installGranting(t, filerManifest, []apps.Scope{apps.ScopeMemoryRead})
	if got := mcpbridge.ConsequenceFor(app); got != "" {
		t.Fatalf("an app whose egress was declined still claims a consequence: %q", got)
	}
	if got := mcpbridge.AccessFor(app); got != mcp.AccessRead {
		t.Fatalf("an app that can only read should spend the read half, got %q", got)
	}
}

// ORCHESTRATOR.md §4b rule 2: read and write are separate grants.
func TestTheHalfMatchesWhatRunningTheAppDoes(t *testing.T) {
	if got := mcpbridge.AccessFor(install(t, readerManifest)); got != mcp.AccessRead {
		t.Fatalf("a read-only app spends %q", got)
	}
	if got := mcpbridge.AccessFor(install(t, standupManifest)); got != mcp.AccessWrite {
		t.Fatalf("an app that writes notes and speaks spends %q", got)
	}
}

func TestTwoAppsAreTwoDistinctTools(t *testing.T) {
	ctx := context.Background()
	cat := &catalogue{list: []apps.Installed{
		install(t, standupManifest), install(t, readerManifest), install(t, quietManifest),
	}}
	gw, _ := gateway(t, cat, &recorder{}, nil)
	got := toolNames(gw.All(ctx))
	if len(got) != 2 {
		t.Fatalf("two apps declare tool triggers; got %v", got)
	}
	if got[0] == got[1] {
		t.Fatalf("two apps share one tool name: %v", got)
	}
	seen := map[string]bool{}
	for _, t2 := range gw.All(ctx) {
		if seen[t2.Connector] {
			t.Fatalf("two apps share one grant identity: %s", t2.Connector)
		}
		seen[t2.Connector] = true
	}
}

// The schema is one optional string, because relay.json has nowhere to declare
// one. Inventing a richer contract would put something in front of the model
// that no reviewer read.
func TestTheInputSchemaIsWhatTheManifestCanActuallyDeclare(t *testing.T) {
	ctx := context.Background()
	cat := &catalogue{list: []apps.Installed{install(t, standupManifest)}}
	gw, _ := gateway(t, cat, &recorder{}, nil)
	tool, _ := gw.Lookup(ctx, "app_dev_alexis_standup_notes_run")
	props, ok := tool.InputSchema["properties"].(map[string]any)
	if !ok || len(props) != 1 {
		t.Fatalf("input schema is %v", tool.InputSchema)
	}
	if _, ok := props["request"]; !ok {
		t.Fatalf("input schema is %v", tool.InputSchema)
	}
}

// Arguments the schema did not describe still reach the app. The gateway is not
// a validator, and dropping them silently is worse than delivering them.
func TestArgumentsBeyondTheSchemaStillReachTheApp(t *testing.T) {
	ctx := context.Background()
	cat := &catalogue{list: []apps.Installed{install(t, standupManifest)}}
	rec := &recorder{}
	gw, _ := gateway(t, cat, rec, nil)
	_, err := gw.Call(ctx, mcp.Call{
		Tool:      "app_dev_alexis_standup_notes_run",
		Arguments: map[string]any{"request": "wrap up", "since": "09:00"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.calls[0].Arguments["since"] != "09:00" {
		t.Fatalf("an argument was dropped on the way in: %v", rec.calls[0].Arguments)
	}
}

// "Your agent can call this on its own" is not one of the nine scopes and is
// still something the user is agreeing to.
func TestTheInstallSheetGetsASentenceAboutBeingCallable(t *testing.T) {
	note, ok := mcpbridge.InstallNote(install(t, standupManifest).Manifest)
	if !ok {
		t.Fatal("an app with a tool trigger owes the install sheet a sentence")
	}
	if !strings.Contains(note, "without a wake phrase") ||
		!strings.Contains(note, "Summarise the most recent meeting") {
		t.Fatalf("the note is %q", note)
	}
	if _, ok := mcpbridge.InstallNote(install(t, quietManifest).Manifest); ok {
		t.Fatal("an app with no tool trigger should produce no row at all")
	}
}
