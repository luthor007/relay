package orchestrator_test

import (
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/orchestrator"
)

// TestEveryRuntimeHasABrief. Relay drives five and the orchestrator has been
// choosing between them on the basis of the name. A runtime with no brief is
// one the model will pick badly.
func TestEveryRuntimeHasABrief(t *testing.T) {
	want := []adapter.Runtime{
		adapter.ClaudeCode, adapter.Codex, adapter.OpenClaw, adapter.Hermes, adapter.OpenCode,
	}
	for _, rt := range want {
		h, ok := orchestrator.HarnessFor(rt)
		if !ok {
			t.Errorf("no brief for %s", rt)
			continue
		}
		if h.Summary == "" || h.Protocol == "" {
			t.Errorf("%s: summary=%q protocol=%q", rt, h.Summary, h.Protocol)
		}
		// Traps are the most valuable field and the easiest to leave empty.
		// Every one of the five has at least one thing that goes wrong.
		if len(h.Traps) == 0 {
			t.Errorf("%s lists no traps; all five have them", rt)
		}
		if len(h.Prompting) == 0 {
			t.Errorf("%s says nothing about how to prompt it", rt)
		}
	}
	if got := len(orchestrator.Harnesses()); got != len(want) {
		t.Errorf("%d briefs for %d runtimes", got, len(want))
	}
}

// TestTheBriefCarriesTheComputedHalf is the property that keeps this honest.
//
// Strengths and prompting are curated prose and will go stale. Capabilities and
// the compaction mechanism are read from adapter.Baseline and
// compaction.MechanismFor — the same tables the adapters use — so they cannot
// drift from what the code does. If someone narrows a capability, the brief
// narrows with it.
func TestTheBriefCarriesTheComputedHalf(t *testing.T) {
	h, _ := orchestrator.HarnessFor(adapter.Codex)
	brief := h.Brief()

	// Codex compacts by protocol call. That string is in compaction's table,
	// not in this package.
	if !strings.Contains(brief, "thread/compact/start") {
		t.Errorf("the brief does not carry the compaction mechanism:\n%s", brief)
	}

	// And the capability gaps come with the reason the coverage table gives.
	acp, _ := orchestrator.HarnessFor(adapter.OpenClaw)
	ab := acp.Brief()
	if !strings.Contains(ab, "Cannot do, or unverified") {
		t.Errorf("no capability section for an ACP runtime:\n%s", ab)
	}
	if !strings.Contains(strings.ToLower(ab), "unknown") && !strings.Contains(ab, "resume") {
		t.Errorf("ACP's unverified resume is not surfaced:\n%s", ab)
	}
}

// TestTheDangerousCodexSettingIsInTheBrief.
//
// Raising model_auto_compact_token_limit to or above model_context_window turns
// a graceful pause into a terminal ContextWindowExceeded that can only be
// answered by starting a new thread. It is the single most dangerous write in
// the whole runtime survey, `compaction.Refuse` blocks it in code, and a model
// about to suggest it to a user should have read about it first.
func TestTheDangerousCodexSettingIsInTheBrief(t *testing.T) {
	h, _ := orchestrator.HarnessFor(adapter.Codex)
	b := h.Brief()
	for _, want := range []string{"model_auto_compact_token_limit", "model_context_window"} {
		if !strings.Contains(b, want) {
			t.Errorf("the Codex brief never mentions %s:\n%s", want, b)
		}
	}
}

// TestHermesLeaseIsInTheBrief — MEMORY.md §12.5's standing instruction. A model
// that suggests compacting Hermes without the lease is suggesting a race with
// upstream concurrency tests behind it.
func TestHermesLeaseIsInTheBrief(t *testing.T) {
	h, _ := orchestrator.HarnessFor(adapter.Hermes)
	b := h.Brief()
	if !strings.Contains(b, "compression_locks") {
		t.Errorf("the Hermes brief does not mention the lease:\n%s", b)
	}
	if !strings.Contains(b, "takes a lease first") {
		t.Errorf("the computed compaction line does not flag the lease:\n%s", b)
	}
}

// TestTheRosterIsShortEnoughToAlwaysLoad. "Which of these five" and "how do I
// drive this one" are different questions with different context costs, and
// loading five pages to answer the first is how a tool set stops being worth
// having.
func TestTheRosterIsShort(t *testing.T) {
	roster := orchestrator.Roster()
	for _, rt := range []string{"claude-code", "codex", "openclaw", "hermes", "opencode"} {
		if !strings.Contains(roster, rt) {
			t.Errorf("the roster omits %s", rt)
		}
	}
	if n := len(strings.Split(strings.TrimSpace(roster), "\n")); n > 10 {
		t.Errorf("the roster is %d lines; it is meant to be a list, not the briefs", n)
	}
	// One brief should be substantially longer than the whole roster, or the
	// split is not buying anything.
	h, _ := orchestrator.HarnessFor(adapter.ClaudeCode)
	if len(h.Brief()) < len(roster) {
		t.Error("a brief is shorter than the roster; the two-tier split is pointless")
	}
}

// TestDescribeRuntimeIsAlwaysAvailable — it depends on nothing, and it is the
// tool that makes choosing between runtimes possible at all.
func TestDescribeRuntimeIsAlwaysAvailable(t *testing.T) {
	box := orchestrator.ToolboxFor(orchestrator.Deps{})
	res, err := runTool(t, box, t.Context(), llm.ToolCall{
		ID: "c", Name: orchestrator.ToolDescribeRuntime, Input: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "claude-code") {
		t.Errorf("an empty runtime did not return the roster:\n%s", res.Content)
	}

	res, err = runTool(t, box, t.Context(), llm.ToolCall{
		ID: "c", Name: orchestrator.ToolDescribeRuntime, Input: []byte(`{"runtime":"codex"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "turn/steer") {
		t.Errorf("the Codex brief did not come back:\n%s", res.Content)
	}
}

// TestAnUnknownRuntimeGetsTheRealNames. The model guessed a name and needs the
// five, not a refusal it cannot act on.
func TestAnUnknownRuntimeGetsTheRealNames(t *testing.T) {
	box := orchestrator.ToolboxFor(orchestrator.Deps{})
	res, err := runTool(t, box, t.Context(), llm.ToolCall{
		ID: "c", Name: orchestrator.ToolDescribeRuntime, Input: []byte(`{"runtime":"cursor"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("an unknown runtime was reported as a success")
	}
	if !strings.Contains(res.Content, "claude-code") {
		t.Errorf("the model was not told what the real names are:\n%s", res.Content)
	}
}

// TestTheEntitlementRuleIsWhereTheModelWillSeeIt.
//
// A Claude Max subscription powers Claude Code and does not power an API key
// elsewhere. It is a billing fact rather than a technical one, it overrides
// every capability comparison, and there is nowhere else the model would learn
// it.
func TestTheEntitlementRuleIsInTheClaudeBrief(t *testing.T) {
	h, _ := orchestrator.HarnessFor(adapter.ClaudeCode)
	b := strings.ToLower(h.Brief())
	if !strings.Contains(b, "subscription") {
		t.Errorf("the Claude Code brief does not mention the subscription:\n%s", h.Brief())
	}
}
