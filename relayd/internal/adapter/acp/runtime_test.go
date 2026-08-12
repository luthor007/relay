package acp

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/event"
)

// TestLaunchCommands pins the three command lines from ADAPTERS.md §4.
func TestLaunchCommands(t *testing.T) {
	for _, tc := range []struct {
		runtime         adapter.Runtime
		sessionKey      string
		requireExisting bool
		want            string
	}{
		{adapter.OpenClaw, "agent:main:main", true, "openclaw acp --session agent:main:main --require-existing"},
		{adapter.OpenClaw, "", false, "openclaw acp --session agent:main:main"},
		{adapter.Hermes, "", false, "hermes acp"},
		{adapter.OpenCode, "", false, "opencode acp"},
		// A session key means nothing to the two runtimes that are not a bridge.
		{adapter.Hermes, "agent:main:main", true, "hermes acp"},
	} {
		cfg, ok := ConfigFor(tc.runtime)
		if !ok {
			t.Fatalf("no config for %s", tc.runtime)
		}
		bin, args := cfg.argv(tc.sessionKey, tc.requireExisting)
		got := strings.TrimSpace(bin + " " + strings.Join(args, " "))
		if got != tc.want {
			t.Errorf("%s: %q, want %q", tc.runtime, got, tc.want)
		}
	}
	if _, ok := ConfigFor(adapter.ClaudeCode); ok {
		t.Error("Claude Code does not speak ACP")
	}
	if _, ok := ConfigFor(adapter.Codex); ok {
		t.Error("Codex does not speak ACP")
	}
}

// TestOnlyOpenClawIsSessionScoped: the process argument is what makes one
// OpenClaw connection serve one Gateway session.
func TestOnlyOpenClawIsSessionScoped(t *testing.T) {
	for _, r := range Runtimes() {
		cfg, _ := ConfigFor(r)
		want := r == adapter.OpenClaw
		if cfg.SessionScopedProcess != want {
			t.Errorf("%s SessionScopedProcess = %v, want %v", r, cfg.SessionScopedProcess, want)
		}
		if cfg.BridgeOnly != want {
			t.Errorf("%s BridgeOnly = %v; only OpenClaw's ACP is a front for something else", r, cfg.BridgeOnly)
		}
	}
	if cfg, _ := ConfigFor(adapter.Hermes); !strings.Contains(cfg.OwnStore, "SQLite") {
		t.Error("Hermes keeps its sessions in SQLite, which is what lets the registry be reconciled after a crash")
	}
}

// TestCostIsPerRuntimeNotPerAdapter is ADAPTERS.md §8 item 3: OpenCode has
// `opencode stats`, Hermes has per-session figures in SQLite, and no OpenClaw
// equivalent has been found. Reporting the same answer for all three would be
// wrong twice over.
func TestCostIsPerRuntimeNotPerAdapter(t *testing.T) {
	oc := CostPlanFor(adapter.OpenCode)
	hm := CostPlanFor(adapter.Hermes)
	ow := CostPlanFor(adapter.OpenClaw)

	for _, p := range []CostPlan{oc, hm, ow} {
		if p.InProtocol {
			t.Errorf("%s claims cost in the protocol; ACP 0.4.5 has no cost, token or usage field at all", p.Runtime)
		}
		if p.Verified {
			t.Errorf("%s claims a verified cost source, but nobody has read one on a real install", p.Runtime)
		}
		if p.Detail == "" {
			t.Errorf("%s has no explanation for a missing figure", p.Runtime)
		}
	}
	if oc.Support != adapter.SupportUnknown || !strings.Contains(oc.Source, "opencode stats") {
		t.Errorf("OpenCode plan = %+v", oc)
	}
	if hm.Support != adapter.SupportUnknown || !strings.Contains(hm.Source, "estimated_cost_usd") {
		t.Errorf("Hermes plan = %+v", hm)
	}
	if ow.Support != adapter.SupportNo || ow.Source != "" {
		t.Errorf("OpenClaw plan = %+v; no store has been found, so the honest answer is no", ow)
	}
}

type fixedCost struct{ usd float64 }

func (c fixedCost) TurnCost(context.Context, TurnInfo) (*event.Usage, error) {
	return &event.Usage{CostUSD: event.F64(c.usd)}, nil
}
func (fixedCost) Describe() string { return "test ledger" }

type brokenCost struct{}

func (brokenCost) TurnCost(context.Context, TurnInfo) (*event.Usage, error) {
	return nil, context.DeadlineExceeded
}
func (brokenCost) Describe() string { return "a store that is not there" }

// TestCostSourceFillsUsageWhenWired: report cost where it exists, report its
// absence where it does not.
func TestCostSourceFillsUsageWhenWired(t *testing.T) {
	ctx := context.Background()
	f := newFakeAgent(t)
	opts := testOptions(t, adapter.Hermes)
	opts.Cost = fixedCost{usd: 0.42}
	a := dial(t, f, opts, AgentCapabilities{})
	s := startSession(t, f, a, "sess_1")
	c := collect(s)

	if got := s.Capabilities().Get(adapter.CapCostUSD); got != adapter.SupportYes {
		t.Errorf("with a CostSource wired, CapCostUSD = %v, want yes", got)
	}
	if note := s.Capabilities().Note(adapter.CapCostUSD); !strings.Contains(note, "test ledger") {
		t.Errorf("the note should name the source: %q", note)
	}

	_, _ = s.Send(ctx, adapter.Turn{Text: "go"})
	m := f.next()
	f.respond(m.ID, promptResult{StopReason: "end_turn"})

	e := c.waitFor(t, "a completion", func(e event.Event) bool {
		_, ok := e.(event.TurnCompleted)
		return ok
	}).(event.TurnCompleted)
	if e.Usage == nil || e.Usage.CostUSD == nil || *e.Usage.CostUSD != 0.42 {
		t.Fatalf("usage = %+v", e.Usage)
	}
	if e.Usage.InputTokens != nil || e.Usage.ContextWindow != nil {
		t.Error("an out-of-band dollar figure says nothing about tokens; those stay nil")
	}
}

// TestBrokenCostSourceReportsNothingRatherThanZero.
func TestBrokenCostSourceReportsNothingRatherThanZero(t *testing.T) {
	ctx := context.Background()
	f := newFakeAgent(t)
	opts := testOptions(t, adapter.OpenCode)
	opts.Cost = brokenCost{}
	a := dial(t, f, opts, AgentCapabilities{})
	s := startSession(t, f, a, "sess_1")
	c := collect(s)

	_, _ = s.Send(ctx, adapter.Turn{Text: "go"})
	m := f.next()
	f.respond(m.ID, promptResult{StopReason: "end_turn"})

	e := c.waitFor(t, "a completion", func(e event.Event) bool {
		_, ok := e.(event.TurnCompleted)
		return ok
	}).(event.TurnCompleted)
	if e.Usage != nil {
		t.Errorf("usage = %+v; a failed lookup reports no cost, never a zero one", e.Usage)
	}
}

// TestCapabilitiesAreNarrowedPerRuntime: the descriptor differs between the
// three even though the adapter does not.
func TestCapabilitiesAreNarrowedPerRuntime(t *testing.T) {
	for _, r := range Runtimes() {
		f := newFakeAgent(t)
		a := dial(t, f, testOptions(t, r), AgentCapabilities{
			LoadSession:        true,
			PromptCapabilities: PromptCapabilities{Image: true},
		})
		caps := a.Capabilities()
		if caps.Runtime() != r {
			t.Errorf("descriptor names %s, want %s", caps.Runtime(), r)
		}
		if caps.Protocol() != adapter.ProtocolACP {
			t.Errorf("%s protocol = %s", r, caps.Protocol())
		}
		if caps.Get(adapter.CapResume) != adapter.SupportYes {
			t.Errorf("%s: loadSession true must narrow CapResume to yes", r)
		}
		if caps.Get(adapter.CapPromptImage) != adapter.SupportYes {
			t.Errorf("%s: an advertised image capability must show up", r)
		}
		if caps.Get(adapter.CapPromptAudio) != adapter.SupportNo {
			t.Errorf("%s: audio was not advertised, so it is a no, not an unknown", r)
		}
		if got, want := caps.Get(adapter.CapCostUSD), CostPlanFor(r).Support; got != want {
			t.Errorf("%s cost support = %v, want %v", r, got, want)
		}
		if caps.Get(adapter.CapPlan) != adapter.SupportYes {
			t.Errorf("%s: the plan session update is protocol-native", r)
		}
		if caps.Get(adapter.CapFork) != adapter.SupportNo {
			t.Errorf("%s: ACP has no fork", r)
		}
		missing := caps.Missing()
		if len(missing) == 0 {
			t.Errorf("%s claims to do everything; the coverage table is uneven on purpose", r)
		}
	}
}

func TestDialRejectsRuntimesThatDoNotSpeakACP(t *testing.T) {
	for _, r := range []adapter.Runtime{adapter.ClaudeCode, adapter.Codex, adapter.Runtime("nonsense")} {
		if _, err := Dial(context.Background(), Options{Runtime: r}); err == nil {
			t.Errorf("Dial accepted %s", r)
		} else if !strings.Contains(err.Error(), "not an ACP runtime") {
			t.Errorf("Dial(%s) = %v", r, err)
		}
	}
}

func TestRuntimesAreTheThreeACPOnes(t *testing.T) {
	want := []adapter.Runtime{adapter.OpenClaw, adapter.Hermes, adapter.OpenCode}
	if !reflect.DeepEqual(Runtimes(), want) {
		t.Errorf("Runtimes() = %v, want %v", Runtimes(), want)
	}
	for _, r := range want {
		if r.Protocol() != adapter.ProtocolACP {
			t.Errorf("%s is not marked as an ACP runtime in the shared table", r)
		}
	}
}
