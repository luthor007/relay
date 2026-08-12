package orchestrator_test

import (
	"context"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/mcp"
	"github.com/luthor007/relay/relayd/internal/orchestrator"
)

func staging() orchestrator.Skill {
	return orchestrator.Skill{
		Name:  "check_staging_health",
		Title: "Check staging health",
		When:  "when the user asks whether staging is up",
		Steps: "Open the staging dashboard. Read the error rate for the last hour. Report the number.",
		Needs: []string{"browser"},
	}
}

// TestAnAuthoredSkillReachesEveryRuntime is the point of the type.
//
// The gateway is already in each runtime's config, so a skill written once is
// visible to Claude Code, Codex and the three ACP runtimes without touching a
// config file again. That is "grant once, works in all five" applied to
// something Relay wrote rather than something a vendor shipped.
func TestAnAuthoredSkillReachesEveryRuntime(t *testing.T) {
	book := orchestrator.NewSkillBook()
	if err := book.Author(t.Context(), staging()); err != nil {
		t.Fatal(err)
	}

	// It is an mcp.Provider, which is the whole distribution mechanism.
	var p mcp.Provider = book
	tools := p.Tools(t.Context())
	if len(tools) != 1 {
		t.Fatalf("%d tools on the bus, want 1", len(tools))
	}

	tool := tools[0]
	if tool.Connector != orchestrator.SkillConnector {
		t.Errorf("connector = %q; a tool with no connector is a tool with no grant", tool.Connector)
	}
	if !strings.Contains(tool.Description, "Call this when the user asks whether staging is up") {
		t.Errorf("the description lost the trigger:\n%s", tool.Description)
	}
	if !strings.Contains(tool.Description, "browser") {
		t.Errorf("the description does not say what the agent needs:\n%s", tool.Description)
	}
}

// TestASkillHandsOverInstructionsAndDoesNotAct. The handler returns text. There
// is nowhere for it to go and nothing for it to call: Relay has no browser and
// the agent that called this tool does.
func TestASkillHandsOverInstructionsAndDoesNotAct(t *testing.T) {
	book := orchestrator.NewSkillBook()
	if err := book.Author(t.Context(), staging()); err != nil {
		t.Fatal(err)
	}
	tool := book.Tools(t.Context())[0]

	res, err := tool.Handler(t.Context(), mcp.Call{
		Tool:      tool.Name,
		Arguments: map[string]any{"context": "the release-2 branch"},
		Runtime:   "claude-code",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"staging dashboard", "error rate", "release-2 branch", "browser"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("the handed-over instructions lost %q:\n%s", want, res.Text)
		}
	}
	if res.IsError {
		t.Error("returning instructions is not a failure")
	}
	// Reading a playbook changes nothing outside this machine, so it is not
	// consequential. Whatever the agent then does is confirmed by the tools
	// that actually do it — asking twice trains people to stop reading the ask.
	if tool.Consequential() {
		t.Error("reading a playbook should not require a spoken confirmation")
	}
}

// TestASkillImprovesInPlace is Hermes's idea worth stealing: a playbook gets
// better during use, so a second version is the normal case rather than a
// conflict.
func TestASkillImprovesInPlace(t *testing.T) {
	book := orchestrator.NewSkillBook()
	if err := book.Author(t.Context(), staging()); err != nil {
		t.Fatal(err)
	}

	better := staging()
	better.Steps = "Open the staging dashboard. Read the error rate AND the p99 latency. Report both."
	if err := book.Author(t.Context(), better); err != nil {
		t.Fatal(err)
	}

	list, _ := book.List(t.Context())
	if len(list) != 1 {
		t.Fatalf("improving a skill made %d of them", len(list))
	}
	if !strings.Contains(list[0].Steps, "p99") {
		t.Errorf("the improvement was lost: %q", list[0].Steps)
	}
}

func TestASkillWithoutATriggerIsRefused(t *testing.T) {
	book := orchestrator.NewSkillBook()
	for _, tc := range []struct {
		name  string
		skill orchestrator.Skill
		want  string
	}{
		{"no trigger", orchestrator.Skill{Name: "x", Steps: "do things"}, "when to use it"},
		{"no steps", orchestrator.Skill{Name: "x", When: "when asked"}, "no instructions"},
		{"no name", orchestrator.Skill{When: "when asked", Steps: "do"}, "needs a name"},
		{"unusable name", orchestrator.Skill{Name: "check staging", When: "w", Steps: "s"}, "not a usable tool name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := book.Author(t.Context(), tc.skill)
			if err == nil {
				t.Fatalf("accepted a skill with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestAuthoringGoesThroughTheToolAndIsConfirmed: writing a playbook changes
// what every runtime on the machine sees, so it is one of the four
// consequential tools rather than something the model does quietly.
func TestAuthoringGoesThroughTheTool(t *testing.T) {
	book := orchestrator.NewSkillBook()
	box := orchestrator.ToolboxFor(orchestrator.Deps{Skills: book})
	// describe_runtime is always present — it depends on nothing — so the
	// dependency-gated tool is the second one.
	if len(box) != 2 {
		t.Fatalf("%d tools", len(box))
	}
	if !orchestrator.Consequential(orchestrator.ToolAuthorSkill) {
		t.Error("authoring a skill changes every runtime on the machine and is not confirmed")
	}

	res, err := runTool(t, box, t.Context(), llm.ToolCall{
		ID: "c", Name: orchestrator.ToolAuthorSkill,
		Input: []byte(`{"name":"check_staging_health","when":"when the user asks whether staging is up",
			"steps":"Open the dashboard. Read the error rate.","needs":["browser"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("authoring failed: %s", res.Content)
	}
	list, _ := book.List(t.Context())
	if len(list) != 1 || list[0].Name != "check_staging_health" {
		t.Fatalf("the skill never landed: %+v", list)
	}
	// The model has to learn the reach of what it just did, or it will not
	// mention it to the user.
	if !strings.Contains(res.Content, "every runtime") {
		t.Errorf("the result does not say how far the skill reaches: %q", res.Content)
	}
}

// TestABadSkillIsAnErrorResultNotAFailedRun: the model can fix a missing
// trigger on the next turn, and cannot fix a run that stopped.
func TestABadSkillIsAnErrorResultNotAFailedRun(t *testing.T) {
	box := orchestrator.ToolboxFor(orchestrator.Deps{Skills: orchestrator.NewSkillBook()})
	res, err := runTool(t, box, t.Context(), llm.ToolCall{
		ID: "c", Name: orchestrator.ToolAuthorSkill,
		Input: []byte(`{"name":"x","steps":"do things"}`),
	})
	if err != nil {
		t.Fatalf("a fixable mistake failed the run: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "when to use it") {
		t.Errorf("the model was not told what to fix: %+v", res)
	}
}

func TestRememberWritesAFact(t *testing.T) {
	nb := &stubNotebook{}
	box := orchestrator.ToolboxFor(orchestrator.Deps{Notebook: nb})
	res, err := runTool(t, box, t.Context(), llm.ToolCall{
		ID: "c", Name: orchestrator.ToolRemember,
		Input: []byte(`{"text":"deploys go out on Tuesdays","subject":"relay"}`),
	})
	if err != nil || res.IsError {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(nb.facts) != 1 || nb.facts[0].Text != "deploys go out on Tuesdays" {
		t.Fatalf("facts = %+v", nb.facts)
	}
	if nb.facts[0].At.IsZero() {
		t.Error("a fact with no time cannot be aged out or ordered")
	}
	// Remembering is reversible and low-stakes. Confirming every fact would
	// make "keep your memory up to date" cost a spoken interruption each time,
	// which is how a feature gets turned off.
	if orchestrator.Consequential(orchestrator.ToolRemember) {
		t.Error("remembering a fact should not need a spoken confirmation")
	}
}

// TestGrantingIsAskedAndSaysItCoversEveryRuntime — ORCHESTRATOR.md §4b rule 1:
// nothing is auto-granted. The model can ask; the human decides.
func TestGrantingIsAskedAndSaysItCoversEveryRuntime(t *testing.T) {
	if !orchestrator.Consequential(orchestrator.ToolGrantCapability) {
		t.Fatal("granting a capability is not confirmed")
	}
	box := orchestrator.ToolboxFor(orchestrator.Deps{Capabilities: &stubCapabilities{}})

	var grant llm.Binding
	for _, b := range box {
		if b.Tool.Name == orchestrator.ToolGrantCapability {
			grant = b
		}
	}
	if !strings.Contains(grant.Tool.Description, "every agent runtime") {
		t.Errorf("the description does not tell the model the grant covers all five:\n%s",
			grant.Tool.Description)
	}
	if !strings.Contains(grant.Tool.Description, "revoking once") {
		t.Errorf("the description does not mention that revoking is one action too:\n%s",
			grant.Tool.Description)
	}
}

func TestListCapabilitiesReportsAnEmptyMachineRatherThanNothing(t *testing.T) {
	box := orchestrator.ToolboxFor(orchestrator.Deps{Capabilities: emptyCaps{}})
	for _, b := range box {
		if b.Tool.Name != orchestrator.ToolListCapabilities {
			continue
		}
		res, err := b.Run(context.Background(), llm.ToolCall{ID: "c", Name: b.Tool.Name})
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(res.Content) == "" {
			t.Error("an empty machine returned an empty string, which reads as a broken tool")
		}
	}
}

type emptyCaps struct{}

func (emptyCaps) List(context.Context) ([]orchestrator.Capability, error) { return nil, nil }
func (emptyCaps) Grant(context.Context, string, string, string) error     { return nil }
