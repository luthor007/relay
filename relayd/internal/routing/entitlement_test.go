package routing_test

import (
	"testing"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/routing"
)

// MEMORY.md §8's table, row by row. This is the test that has to hold: for a
// heavy user it is the difference between a subscription they already pay for
// and a metered bill they did not expect.
func TestEntitlementTable(t *testing.T) {
	tests := []struct {
		name   string
		ents   routing.Entitlements
		family routing.ModelFamily
		want   []adapter.Runtime
		any    bool
	}{
		{
			name: "Claude Max routes Claude-model work to Claude Code",
			ents: routing.Entitlements{routing.ClaudeSubscription},
			want: []adapter.Runtime{adapter.ClaudeCode},
		},
		{
			name:   "Claude Max, explicitly Claude work",
			ents:   routing.Entitlements{routing.ClaudeSubscription},
			family: routing.FamilyClaude,
			want:   []adapter.Runtime{adapter.ClaudeCode},
		},
		{
			name:   "a Claude plan does not claim GPT work",
			ents:   routing.Entitlements{routing.ClaudeSubscription},
			family: routing.FamilyGPT,
			want:   nil,
		},
		{
			name: "ChatGPT routes to Codex",
			ents: routing.Entitlements{routing.ChatGPTSubscription},
			want: []adapter.Runtime{adapter.Codex},
		},
		{
			name: "Copilot routes to OpenClaw or OpenCode",
			ents: routing.Entitlements{routing.CopilotSubscription},
			want: []adapter.Runtime{adapter.OpenClaw, adapter.OpenCode},
		},
		{
			name: "raw API keys sanction anything",
			ents: routing.Entitlements{routing.APIKeysOnly},
			any:  true,
		},
		{
			name: "both subscriptions, Claude work",
			ents: routing.Entitlements{routing.ChatGPTSubscription, routing.ClaudeSubscription},
			// Family unspecified means both rows apply, in table order, and the
			// table puts Claude first.
			want: []adapter.Runtime{adapter.ClaudeCode, adapter.Codex},
		},
		{
			name: "nothing declared sanctions nothing",
			ents: nil,
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := routing.Sanctioned(tc.ents, tc.family)
			var runtimes []adapter.Runtime
			var sawAny bool
			for _, s := range got {
				if s.Any {
					sawAny = true
					continue
				}
				runtimes = append(runtimes, s.Runtime)
			}
			if sawAny != tc.any {
				t.Errorf("any = %v, want %v", sawAny, tc.any)
			}
			if len(runtimes) != len(tc.want) {
				t.Fatalf("runtimes = %v, want %v", runtimes, tc.want)
			}
			for i := range runtimes {
				if runtimes[i] != tc.want[i] {
					t.Fatalf("runtimes = %v, want %v", runtimes, tc.want)
				}
			}
		})
	}
}

// The coding-plan rows are a lookup — "whichever runtime lists that provider" —
// and the lookup has holes. A hole is SupportUnknown with the probe that would
// close it, never a yes we cannot source.
func TestCodingPlanRowsDoNotClaimWhatWasNotProbed(t *testing.T) {
	for _, plan := range []routing.Entitlement{
		routing.ZAIPlan, routing.MiniMaxPlan, routing.QwenPlan, routing.KimiPlan,
	} {
		row, ok := routing.Row(plan)
		if !ok {
			t.Fatalf("no table row for %s", plan)
		}
		for _, rs := range row.Runtimes {
			if rs.Support == adapter.SupportYes && rs.Note == "" {
				t.Errorf("%s/%s claims yes with no source", plan, rs.Runtime)
			}
			if rs.Support == adapter.SupportUnknown && rs.Note == "" {
				t.Errorf("%s/%s is unknown with no note saying what would settle it", plan, rs.Runtime)
			}
			if rs.Runtime == adapter.ClaudeCode || rs.Runtime == adapter.Codex {
				t.Errorf("%s lists %s, which fronts one vendor's own models", plan, rs.Runtime)
			}
		}
	}

	// Kimi specifically: ORCHESTRATOR.md §2b read OpenClaw's auth list and Kimi
	// is not in it. Claiming it would be inventing a capability.
	row, _ := routing.Row(routing.KimiPlan)
	for _, rs := range row.Runtimes {
		if rs.Runtime == adapter.OpenClaw && rs.Support == adapter.SupportYes {
			t.Error("Kimi is not in the OpenClaw auth list ORCHESTRATOR.md §2b recorded; this row cannot be yes")
		}
	}
}

func TestEveryRowCitesItsSource(t *testing.T) {
	for _, row := range routing.Table {
		if row.Source == "" {
			t.Errorf("entitlement %s has no source", row.Entitlement)
		}
		if row.Because == "" {
			t.Errorf("entitlement %s has no reason a user could hear", row.Entitlement)
		}
	}
}

func TestFamilyOf(t *testing.T) {
	for _, tc := range []struct {
		text string
		want routing.ModelFamily
	}{
		{"ask claude to look at this", routing.FamilyClaude},
		{"get gpt to review it", routing.FamilyGPT},
		{"run the tests", routing.FamilyUnspecified},
		{"", routing.FamilyUnspecified},
	} {
		if got := routing.FamilyOf(tc.text); got != tc.want {
			t.Errorf("FamilyOf(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

// An empty entitlement set is not the same as "raw API keys only". One means we
// do not know what the user pays for; the other means they pay for nothing, and
// only the second licenses picking on load alone.
func TestEmptyIsNotTheSameAsAPIKeysOnly(t *testing.T) {
	if (routing.Entitlements{}).Empty() != true {
		t.Fatal("an empty set should report Empty")
	}
	if (routing.Entitlements{routing.APIKeysOnly}).Empty() {
		t.Fatal("APIKeysOnly is a declaration, not an absence")
	}
}
