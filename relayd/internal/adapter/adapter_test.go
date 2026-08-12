package adapter_test

import (
	"errors"
	"testing"

	"github.com/luthor007/relay/relayd/internal/adapter"
)

// ADAPTERS.md §1: five runtimes, three protocols, and one adapter covers three
// of the five.
func TestFiveRuntimesThreeProtocols(t *testing.T) {
	seen := map[adapter.Protocol]int{}
	for _, r := range adapter.Runtimes() {
		p := r.Protocol()
		if p == "" {
			t.Fatalf("%s has no protocol", r)
		}
		seen[p]++
	}
	if len(adapter.Runtimes()) != 5 {
		t.Fatalf("%d runtimes, want 5", len(adapter.Runtimes()))
	}
	if len(seen) != 3 {
		t.Fatalf("%d protocols, want 3: %v", len(seen), seen)
	}
	if seen[adapter.ProtocolACP] != 3 {
		t.Fatalf("ACP covers %d runtimes, want 3", seen[adapter.ProtocolACP])
	}
	if adapter.Runtime("nonesuch").Protocol() != "" {
		t.Fatal("an unknown runtime must have no protocol rather than a guessed one")
	}
}

// The coverage table is uneven, and the point of the descriptor is that the
// orchestrator can read the unevenness rather than discover it at runtime.
func TestBaselineMatchesTheCoverageTable(t *testing.T) {
	cc := adapter.Baseline(adapter.ClaudeCode)
	codex := adapter.Baseline(adapter.Codex)
	acp := adapter.Baseline(adapter.OpenClaw)

	// Mid-turn steering: present on two, verified absent on ACP.
	if !cc.Has(adapter.CapSteer) || !codex.Has(adapter.CapSteer) {
		t.Fatal("steering is verified present on Claude Code and Codex")
	}
	if acp.Get(adapter.CapSteer) != adapter.SupportNo {
		t.Fatalf("ACP steering is %s, want no — it is verified absent", acp.Get(adapter.CapSteer))
	}

	// PlanUpdated: native on Codex and ACP, absent on Claude Code, where the
	// most it can be is synthesized from tool activity.
	if !codex.Has(adapter.CapPlan) || !acp.Has(adapter.CapPlan) {
		t.Fatal("plans are native on Codex and ACP")
	}
	if cc.Get(adapter.CapPlan) != adapter.SupportSynthesized {
		t.Fatalf("Claude Code plan support is %s, want synthesized", cc.Get(adapter.CapPlan))
	}
	if cc.Get(adapter.CapPlan).Observed() {
		t.Fatal("a synthesized capability is not an observed one; that distinction is the grounding rule")
	}

	// Per-turn cost: USD on one, tokens on another, nothing on the third.
	if !cc.Has(adapter.CapCostUSD) {
		t.Fatal("Claude Code reports total_cost_usd")
	}
	if codex.Get(adapter.CapCostUSD) != adapter.SupportNo || !codex.Has(adapter.CapTokens) {
		t.Fatal("Codex has tokens but no money anywhere in its contract")
	}
	if acp.Get(adapter.CapCostUSD) != adapter.SupportNo || acp.Get(adapter.CapTokens) != adapter.SupportNo {
		t.Fatal("ACP 0.4.5 has no token, cost or usage field at all")
	}

	// The three ACP runtimes share one baseline, because they share one adapter.
	for _, r := range []adapter.Runtime{adapter.Hermes, adapter.OpenCode} {
		other := adapter.Baseline(r)
		for _, cap := range adapter.CapabilityNames() {
			if other.Get(cap) != acp.Get(cap) {
				t.Fatalf("%s differs from openclaw on %s", r, cap)
			}
		}
	}
}

// ADAPTERS.md §8 leaves several rows open, and "unknown" is a different answer
// from "no". Marking loadSession as no would tell the registry to stop trying;
// marking it yes would claim reattach works, which §4 says we must not do.
func TestUnverifiedCapabilitiesAreUnknownNotNo(t *testing.T) {
	acp := adapter.Baseline(adapter.OpenClaw)
	for _, cap := range []adapter.Capability{adapter.CapResume, adapter.CapReasoning} {
		if got := acp.Get(cap); got != adapter.SupportUnknown {
			t.Fatalf("%s is %s, want unknown until it is probed on a real runtime", cap, got)
		}
		if acp.Note(cap) == "" {
			t.Fatalf("%s is unknown with no note explaining why", cap)
		}
	}
	// Prompt capabilities are negotiated per session and default false in ACP.
	for _, cap := range []adapter.Capability{
		adapter.CapPromptImage, adapter.CapPromptAudio, adapter.CapPromptEmbeddedContext,
	} {
		if acp.Get(cap) != adapter.SupportUnknown {
			t.Fatalf("%s must be unknown before the handshake", cap)
		}
	}
}

// Missing capabilities produce an error the orchestrator can degrade against,
// never a panic.
func TestRequireReturnsADegradableError(t *testing.T) {
	acp := adapter.Baseline(adapter.Hermes)

	err := acp.Require(adapter.CapSteer)
	if err == nil {
		t.Fatal("steering on ACP must not be allowed")
	}
	if !errors.Is(err, adapter.ErrUnsupported) {
		t.Fatalf("%v does not match ErrUnsupported", err)
	}
	var ue *adapter.UnsupportedError
	if !errors.As(err, &ue) {
		t.Fatalf("%v is not an *UnsupportedError", err)
	}
	if ue.Runtime != adapter.Hermes || ue.Capability != adapter.CapSteer {
		t.Fatalf("error lost its subject: %+v", ue)
	}
	if ue.Note == "" {
		t.Fatal("the error should carry the reason so a user-facing message can explain the gap")
	}

	if err := acp.Require(adapter.CapCancel); err != nil {
		t.Fatalf("cancel is supported on ACP: %v", err)
	}
	// Unknown is not a licence to try.
	if err := acp.Require(adapter.CapResume); err == nil {
		t.Fatal("an unprobed capability must not be treated as available")
	}
}

// Capabilities are immutable, so an adapter can hand one out without the
// caller being able to widen it.
func TestWithIsACopy(t *testing.T) {
	base := adapter.Baseline(adapter.OpenCode)
	narrowed := base.With(adapter.CapResume, adapter.SupportYes, "loadSession advertised at handshake")

	if base.Get(adapter.CapResume) != adapter.SupportUnknown {
		t.Fatal("With mutated the original")
	}
	if !narrowed.Has(adapter.CapResume) {
		t.Fatal("With did not apply")
	}
	if narrowed.Note(adapter.CapResume) != "loadSession advertised at handshake" {
		t.Fatalf("note is %q", narrowed.Note(adapter.CapResume))
	}
	if narrowed.Runtime() != adapter.OpenCode || narrowed.Protocol() != adapter.ProtocolACP {
		t.Fatal("With lost the runtime or protocol")
	}

	// The Claude Code permissions.defaultMode trap, modelled: an adapter that
	// sees an auto permission mode on system/init turns needs-input OFF rather
	// than reporting a capability it cannot observe.
	auto := adapter.Baseline(adapter.ClaudeCode).
		With(adapter.CapNeedsInput, adapter.SupportNo, `permissionMode is "auto"; the prompt tool is never called`)
	if auto.Has(adapter.CapNeedsInput) {
		t.Fatal("an auto permission mode must disable the needs-input capability")
	}

	all := base.All()
	all[adapter.CapSteer] = adapter.SupportYes
	if base.Has(adapter.CapSteer) {
		t.Fatal("All returned the live map")
	}
}

func TestMissingIsStable(t *testing.T) {
	m := adapter.Baseline(adapter.OpenClaw).Missing()
	if len(m) == 0 {
		t.Fatal("ACP is missing several capabilities")
	}
	for i := 1; i < len(m); i++ {
		if m[i-1] >= m[i] {
			t.Fatalf("Missing is not sorted: %v", m)
		}
	}
	if adapter.Baseline(adapter.Codex).Has(adapter.CapCostUSD) {
		t.Fatal("Codex cost should be in Missing")
	}
}

// A glasses photo cannot enter a prompt on a runtime that never advertised
// image support, and that is refused here rather than dropped silently.
func TestCheckTurnGatesRichContent(t *testing.T) {
	acp := adapter.Baseline(adapter.OpenCode)

	text := adapter.Turn{Text: "run the tests"}
	if len(text.Requires()) != 0 {
		t.Fatalf("plain text requires %v, want nothing", text.Requires())
	}
	if err := adapter.CheckTurn(acp, text); err != nil {
		t.Fatalf("text is the baseline everywhere: %v", err)
	}

	link := adapter.Turn{Text: "look at this", Blocks: []adapter.Block{{Kind: adapter.BlockResourceLink, URI: "file:///x"}}}
	if err := adapter.CheckTurn(acp, link); err != nil {
		t.Fatalf("resource_link is in the baseline too: %v", err)
	}

	photo := adapter.Turn{
		Text:   "what is this",
		Blocks: []adapter.Block{{Kind: adapter.BlockImage, MIMEType: "image/jpeg", Data: []byte{0xff}}},
	}
	if got := photo.Requires(); len(got) != 1 || got[0] != adapter.CapPromptImage {
		t.Fatalf("Requires = %v", got)
	}
	if err := adapter.CheckTurn(acp, photo); !errors.Is(err, adapter.ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported", err)
	}

	withImages := acp.With(adapter.CapPromptImage, adapter.SupportYes, "")
	if err := adapter.CheckTurn(withImages, photo); err != nil {
		t.Fatalf("after the handshake advertised image: %v", err)
	}
}

func TestSupportStrings(t *testing.T) {
	for s, want := range map[adapter.Support]string{
		adapter.SupportUnknown:     "unknown",
		adapter.SupportNo:          "no",
		adapter.SupportYes:         "yes",
		adapter.SupportSynthesized: "synthesized",
	} {
		if s.String() != want {
			t.Fatalf("%d.String() = %q, want %q", s, s.String(), want)
		}
	}
}
