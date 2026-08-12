package routing_test

import (
	"context"
	"testing"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/routing"
)

// used is a runtime this machine has actually run.
func used(rt adapter.Runtime) routing.RuntimeProfile {
	return routing.RuntimeProfile{
		Runtime: rt, Installed: true, Attached: true,
		History: routing.HistorySome, Capabilities: adapter.Baseline(rt),
	}
}

// neverRun is MEMORY.md §1's real finding: installed, never opened. Two of the
// five runtimes on the measured machine looked exactly like this.
func neverRun(rt adapter.Runtime) routing.RuntimeProfile {
	p := used(rt)
	p.History = routing.HistoryNone
	return p
}

func runtimeRouter(t *testing.T, ents routing.Entitlements, prefs routing.Preferences, ps ...routing.RuntimeProfile) *routing.RuntimeRouter {
	t.Helper()
	r, err := routing.NewRuntimeRouter(routing.RuntimeOptions{
		Profiles:     routing.StaticProfiles(ps...),
		Entitlements: ents,
		Preferences:  prefs,
	})
	if err != nil {
		t.Fatalf("NewRuntimeRouter: %v", err)
	}
	return r
}

// MEMORY.md §8's priority order, step by step and in order.
func TestRuntimePriorityOrder(t *testing.T) {
	ctx := context.Background()

	t.Run("1. continuity beats everything", func(t *testing.T) {
		// The user has a Claude subscription, which would otherwise send this
		// to Claude Code. A live Codex session already on the work wins.
		r := runtimeRouter(t, routing.Entitlements{routing.ClaudeSubscription}, nil,
			used(adapter.ClaudeCode), used(adapter.Codex))
		live := routing.SessionView{ID: "s1", Runtime: adapter.Codex, Workspace: "/repo/api"}
		got, err := r.Choose(ctx, routing.RuntimeRequest{Continuity: &live})
		if err != nil {
			t.Fatal(err)
		}
		if got.Runtime != adapter.Codex || got.Reason != routing.RuntimeContinuity {
			t.Fatalf("got %s via %s, want codex via continuity", got.Runtime, got.Reason)
		}
	})

	t.Run("2. an explicit choice beats entitlement", func(t *testing.T) {
		r := runtimeRouter(t, routing.Entitlements{routing.ClaudeSubscription}, nil,
			used(adapter.ClaudeCode), used(adapter.Codex))
		got, _ := r.Choose(ctx, routing.RuntimeRequest{Runtime: adapter.Codex})
		if got.Runtime != adapter.Codex || got.Reason != routing.RuntimeExplicit {
			t.Fatalf("got %s via %s, want codex via explicit", got.Runtime, got.Reason)
		}
	})

	t.Run("2b. a learned preference beats entitlement", func(t *testing.T) {
		prefs := routing.StaticPreference{
			Runtime: adapter.Codex, Evidence: "you always use Codex for Rust",
			When: func(req routing.RuntimeRequest) bool { return req.Text != "" },
		}
		r := runtimeRouter(t, routing.Entitlements{routing.ClaudeSubscription}, prefs,
			used(adapter.ClaudeCode), used(adapter.Codex))
		got, _ := r.Choose(ctx, routing.RuntimeRequest{Text: "fix the borrow checker error"})
		if got.Runtime != adapter.Codex || got.Reason != routing.RuntimeLearned {
			t.Fatalf("got %s via %s, want codex via a learned preference", got.Runtime, got.Reason)
		}
		if got.Because == "" {
			t.Error("a learned preference has to carry its evidence")
		}
	})

	t.Run("3. entitlement decides when nothing else does", func(t *testing.T) {
		r := runtimeRouter(t, routing.Entitlements{routing.ClaudeSubscription}, nil,
			used(adapter.ClaudeCode), used(adapter.Codex), used(adapter.OpenCode))
		got, _ := r.Choose(ctx, routing.RuntimeRequest{Text: "run the tests"})
		if got.Runtime != adapter.ClaudeCode {
			t.Fatalf("got %s via %s; a Claude subscription must reach Claude Code", got.Runtime, got.Reason)
		}
		if got.Reason != routing.RuntimeEntitlement {
			t.Errorf("reason = %s, want entitlement", got.Reason)
		}
		if got.Entitlement != routing.ClaudeSubscription {
			t.Errorf("entitlement = %s, want %s", got.Entitlement, routing.ClaudeSubscription)
		}
	})

	t.Run("4. capability filters on the shared MCP registry", func(t *testing.T) {
		a := used(adapter.ClaudeCode)
		b := used(adapter.Codex)
		b.Tools = []string{"gmail", "github"}
		a.Tools = []string{"github"}
		r := runtimeRouter(t, routing.Entitlements{routing.APIKeysOnly}, nil, a, b)
		got, _ := r.Choose(ctx, routing.RuntimeRequest{Tools: []string{"gmail"}})
		if got.Runtime != adapter.Codex {
			t.Fatalf("got %s; only codex has gmail", got.Runtime)
		}
	})

	t.Run("5. load prefers the idle one", func(t *testing.T) {
		busy := used(adapter.ClaudeCode)
		busy.Busy = true
		idle := used(adapter.Codex)
		r := runtimeRouter(t, routing.Entitlements{routing.APIKeysOnly}, nil, busy, idle)
		got, _ := r.Choose(ctx, routing.RuntimeRequest{})
		if got.Runtime != adapter.Codex || got.Reason != routing.RuntimeLoad {
			t.Fatalf("got %s via %s, want the idle codex via load", got.Runtime, got.Reason)
		}
	})
}

// The rule that outranks the whole priority order.
func TestNeverRouteToARuntimeWithNoHistory(t *testing.T) {
	ctx := context.Background()

	t.Run("an unused runtime is not a destination", func(t *testing.T) {
		r := runtimeRouter(t, routing.Entitlements{routing.APIKeysOnly}, nil,
			used(adapter.ClaudeCode), neverRun(adapter.OpenCode))
		got, _ := r.Choose(ctx, routing.RuntimeRequest{})
		if got.Runtime != adapter.ClaudeCode {
			t.Fatalf("got %s; the never-run runtime must not be chosen", got.Runtime)
		}
		var sawReason bool
		for _, rj := range got.Rejected {
			if rj.Runtime == adapter.OpenCode {
				sawReason = true
			}
		}
		if !sawReason {
			t.Error("the rejected runtime should be named with a reason, for the console")
		}
	})

	t.Run("an entitlement does not override it", func(t *testing.T) {
		// Copilot points at OpenClaw and OpenCode. Neither has ever been run;
		// Claude Code has. Sending the first voice command to a tool they have
		// never opened is the bad first impression MEMORY.md §8 names.
		r := runtimeRouter(t, routing.Entitlements{routing.CopilotSubscription}, nil,
			neverRun(adapter.OpenClaw), neverRun(adapter.OpenCode), used(adapter.ClaudeCode))
		got, _ := r.Choose(ctx, routing.RuntimeRequest{})
		if got.Runtime != adapter.ClaudeCode {
			t.Fatalf("got %s; an entitlement cannot send work to a runtime nobody has opened", got.Runtime)
		}
	})

	t.Run("an explicit request does override it", func(t *testing.T) {
		r := runtimeRouter(t, nil, nil, neverRun(adapter.OpenCode), used(adapter.ClaudeCode))
		got, _ := r.Choose(ctx, routing.RuntimeRequest{Runtime: adapter.OpenCode})
		if got.Runtime != adapter.OpenCode {
			t.Fatalf("got %s; asking for it by name is the one thing that licenses a first run", got.Runtime)
		}
	})

	t.Run("unknown history counts as none", func(t *testing.T) {
		p := used(adapter.OpenCode)
		p.History = routing.HistoryUnknown
		r := runtimeRouter(t, routing.Entitlements{routing.APIKeysOnly}, nil, p)
		got, _ := r.Choose(ctx, routing.RuntimeRequest{})
		if got.Chosen() {
			t.Fatalf("got %s; a runtime nobody looked at is not one to route to", got.Runtime)
		}
		if got.Ask == "" {
			t.Error("with nothing routable the router has to ask, not pick")
		}
	})

	t.Run("nothing usable is a question, not a pick", func(t *testing.T) {
		r := runtimeRouter(t, routing.Entitlements{routing.APIKeysOnly}, nil,
			neverRun(adapter.OpenClaw), neverRun(adapter.OpenCode))
		got, _ := r.Choose(ctx, routing.RuntimeRequest{})
		if got.Chosen() {
			t.Fatalf("got %s, want no choice at all", got.Runtime)
		}
		if got.Ask == "" || got.Reason != routing.RuntimeNone {
			t.Fatalf("reason = %q ask = %q; the honest outcome is a question", got.Reason, got.Ask)
		}
	})
}

// An unprobed coding-plan cell must not be spent on a guess. It stays a
// candidate for capability and load, and the rejection says what would settle
// it — the same shape as an adapter capability that is SupportUnknown.
func TestUnknownCodingPlanSupportDoesNotDecide(t *testing.T) {
	ctx := context.Background()
	r := runtimeRouter(t, routing.Entitlements{routing.KimiPlan}, nil,
		used(adapter.OpenClaw), used(adapter.Codex))
	got, _ := r.Choose(ctx, routing.RuntimeRequest{})
	if got.Reason == routing.RuntimeEntitlement {
		t.Fatalf("an unprobed cell decided the route: %s via %s", got.Runtime, got.Reason)
	}
	var explained bool
	for _, rj := range got.Rejected {
		if rj.Runtime == adapter.OpenClaw && rj.Why != "" {
			explained = true
		}
	}
	if !explained {
		t.Error("the unknown cell should be recorded with why nobody knows")
	}
}

// Asking for a runtime that is not there is a sentence, not a silent
// substitution. Quietly using another one is how a user ends up paying for
// tokens on a plan they thought they were using.
func TestExplicitButAbsentRuntimeSaysSo(t *testing.T) {
	ctx := context.Background()
	r := runtimeRouter(t, nil, nil, used(adapter.ClaudeCode))
	got, _ := r.Choose(ctx, routing.RuntimeRequest{Runtime: adapter.Hermes})
	if got.Chosen() {
		t.Fatalf("got %s; asking for Hermes must not silently produce something else", got.Runtime)
	}
	if got.Ask == "" {
		t.Error("the user has to be told it is not installed")
	}
}

func TestSingleRuntimeMachineSaysSoPlainly(t *testing.T) {
	ctx := context.Background()
	r := runtimeRouter(t, routing.Entitlements{routing.APIKeysOnly}, nil, used(adapter.ClaudeCode))
	got, _ := r.Choose(ctx, routing.RuntimeRequest{})
	if got.Runtime != adapter.ClaudeCode {
		t.Fatalf("got %s", got.Runtime)
	}
	if got.Reason != routing.RuntimeOnlyOne {
		t.Errorf("reason = %q; with one runtime there is no load decision to narrate", got.Reason)
	}
}

// A busy runtime that cannot be steered is a worse destination than an idle
// one, and the answer comes from the capability descriptor rather than from a
// list of runtime names (ADAPTERS.md §4 — three of five cannot be steered).
func TestLoadReadsTheCapabilityDescriptor(t *testing.T) {
	ctx := context.Background()

	steerable := used(adapter.ClaudeCode)
	steerable.Busy = true
	unsteerable := used(adapter.OpenClaw)
	unsteerable.Busy = true

	if steerable.Steerable() == unsteerable.Steerable() {
		t.Fatalf("the baseline descriptors should differ on steering: claude-code=%v openclaw=%v",
			steerable.Steerable(), unsteerable.Steerable())
	}

	r := runtimeRouter(t, routing.Entitlements{routing.APIKeysOnly}, nil, steerable, unsteerable)
	got, _ := r.Choose(ctx, routing.RuntimeRequest{})
	if got.Runtime != adapter.ClaudeCode {
		t.Fatalf("got %s; the one that can take another instruction wins when both are busy", got.Runtime)
	}
}

func TestRuntimeRouterNeedsProfiles(t *testing.T) {
	if _, err := routing.NewRuntimeRouter(routing.RuntimeOptions{}); err == nil {
		t.Fatal("a runtime router with no profile source should not build")
	}
}

func TestChooseIsStable(t *testing.T) {
	// Same inputs, same answer, whatever order the map iteration takes.
	ctx := context.Background()
	ps := []routing.RuntimeProfile{used(adapter.OpenCode), used(adapter.Codex), used(adapter.ClaudeCode)}
	r := runtimeRouter(t, routing.Entitlements{routing.APIKeysOnly}, nil, ps...)
	first, _ := r.Choose(ctx, routing.RuntimeRequest{})
	for i := 0; i < 20; i++ {
		got, _ := r.Choose(ctx, routing.RuntimeRequest{})
		if got.Runtime != first.Runtime {
			t.Fatalf("run %d chose %s, first run chose %s", i, got.Runtime, first.Runtime)
		}
	}
}
