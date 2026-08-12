package routing

import (
	"sort"
	"strings"

	"github.com/luthor007/relay/relayd/internal/adapter"
)

// Entitlement is something the user already pays for.
//
// Nothing in this package infers one. A Claude Code binary on PATH is not
// evidence of a Claude Max plan, and a runtime being installed says nothing
// about which credential is behind it — the installer or the console records
// what the user tells it, and an empty set means the entitlement step is
// skipped with a note rather than guessed at. Guessing here produces exactly
// the failure MEMORY.md §8 is about: a metered bill nobody expected.
type Entitlement string

const (
	// ClaudeSubscription is Claude Max or Claude Pro.
	ClaudeSubscription Entitlement = "claude-subscription"
	// ChatGPTSubscription is ChatGPT Plus or Pro.
	ChatGPTSubscription Entitlement = "chatgpt-subscription"
	// CopilotSubscription is GitHub Copilot.
	CopilotSubscription Entitlement = "github-copilot"

	// The four coding plans MEMORY.md §8 names.
	ZAIPlan     Entitlement = "zai-coding-plan"
	MiniMaxPlan Entitlement = "minimax-coding-plan"
	QwenPlan    Entitlement = "qwen-coding-plan"
	KimiPlan    Entitlement = "kimi-coding-plan"

	// APIKeysOnly is the last row of the table: raw keys, no subscription, so
	// any runtime is sanctioned and the choice falls through to capability and
	// load.
	APIKeysOnly Entitlement = "api-keys"
)

// Entitlements is what the user has.
type Entitlements []Entitlement

// Has reports membership.
func (e Entitlements) Has(want Entitlement) bool {
	for _, v := range e {
		if v == want {
			return true
		}
	}
	return false
}

// Empty reports whether nothing is known. An empty set is the honest default
// and it is not the same as APIKeysOnly: "we do not know what you pay for" and
// "you pay for nothing" lead to different behaviour, and only the second one
// licenses picking a runtime on load alone.
func (e Entitlements) Empty() bool { return len(e) == 0 }

// ModelFamily is which family of model the work wants. MEMORY.md §8's table is
// specifically about *Claude-model work* going to Claude Code, and applying
// that row to a request that never asked for Claude would be a different rule.
type ModelFamily string

const (
	// FamilyUnspecified is the normal case: the user said "run the tests" and
	// named no model. Every entitlement row applies, subscriptions first,
	// because a subscription is free at the margin and an API key is not.
	FamilyUnspecified ModelFamily = ""
	FamilyClaude      ModelFamily = "claude"
	FamilyGPT         ModelFamily = "gpt"
	// FamilyOther is a model family none of the subscription rows cover.
	FamilyOther ModelFamily = "other"
)

// RuntimeSupport is one runtime's standing under one entitlement, carrying the
// same three-way answer the adapter capability descriptor uses.
//
// adapter.SupportUnknown is not a soft no. It means nobody has probed it, and
// [RuntimeRouter] treats it as unroutable-without-asking for the same reason
// ADAPTERS.md §8 keeps its unverified list: a claim we cannot source is a claim
// that will be wrong in front of a user.
type RuntimeSupport struct {
	Runtime adapter.Runtime
	Support adapter.Support
	// Note says where the answer came from, or what probe would settle it.
	Note string
}

// EntitlementRow is one line of MEMORY.md §8's table.
type EntitlementRow struct {
	Entitlement Entitlement
	Label       string
	// Family is which model family this row governs. FamilyUnspecified means
	// the row applies whatever was asked for.
	Family ModelFamily
	// Runtimes are the sanctioned clients, in preference order.
	Runtimes []RuntimeSupport
	// Any marks the last row: any runtime is fine, pick on capability and load.
	Any bool
	// Because is the reason, in the user's terms. It reaches the console and
	// the spoken explanation when someone asks why.
	Because string
	// Source is which doc or probe this row rests on.
	Source string
}

// Table is MEMORY.md §8's entitlement table, as data.
//
// The order matters: rows are consulted top to bottom, so a user with both a
// Claude subscription and raw API keys gets Claude Code for Claude work rather
// than whatever is idle.
var Table = []EntitlementRow{
	{
		Entitlement: ClaudeSubscription,
		Label:       "Claude Max / Pro",
		Family:      FamilyClaude,
		Runtimes: []RuntimeSupport{{
			Runtime: adapter.ClaudeCode,
			Support: adapter.SupportYes,
			Note:    "Anthropic's own client, using its own login",
		}},
		Because: "the only sanctioned client for that credential",
		Source:  "MEMORY.md §8, ORCHESTRATOR.md §2b",
	},
	{
		Entitlement: ChatGPTSubscription,
		Label:       "ChatGPT Plus / Pro",
		Family:      FamilyGPT,
		Runtimes: []RuntimeSupport{{
			Runtime: adapter.Codex,
			Support: adapter.SupportYes,
			Note:    "Codex OAuth is the sanctioned path",
		}},
		Because: "Codex OAuth is the sanctioned path for a ChatGPT plan",
		Source:  "MEMORY.md §8, ORCHESTRATOR.md §2b",
	},
	{
		Entitlement: CopilotSubscription,
		Label:       "GitHub Copilot",
		Runtimes: []RuntimeSupport{
			{Runtime: adapter.OpenClaw, Support: adapter.SupportYes, Note: "exposes Copilot as a provider"},
			{Runtime: adapter.OpenCode, Support: adapter.SupportYes, Note: "exposes Copilot as a provider"},
		},
		Because: "both expose Copilot as a provider",
		Source:  "MEMORY.md §8",
	},

	// The coding plans. MEMORY.md §8's row is "whichever runtime lists that
	// provider", which is a lookup and not a constant — so each plan carries
	// what we can actually source, and SupportUnknown where we cannot.
	//
	// ORCHESTRATOR.md §2b read OpenClaw's own auth-choice list and recorded
	// that Qwen, MiniMax, Z.AI and Chutes carry OAuth or coding-plan rows.
	// Kimi/Moonshot is *not* in that list, so its OpenClaw cell is unknown
	// rather than yes. Hermes and OpenCode have never been probed for any of
	// the four; closing those cells needs the runtimes installed and
	// authenticated on the author's machine (ADAPTERS.md §8).
	codingPlan(ZAIPlan, "Z.AI coding plan", adapter.SupportYes,
		"named in OpenClaw's auth-choice list (ORCHESTRATOR.md §2b)"),
	codingPlan(MiniMaxPlan, "MiniMax coding plan", adapter.SupportYes,
		"named in OpenClaw's auth-choice list (ORCHESTRATOR.md §2b)"),
	codingPlan(QwenPlan, "Qwen coding plan", adapter.SupportYes,
		"named in OpenClaw's auth-choice list (ORCHESTRATOR.md §2b)"),
	codingPlan(KimiPlan, "Kimi coding plan", adapter.SupportUnknown,
		"ORCHESTRATOR.md §2b names Z.AI, MiniMax, Qwen and Chutes in OpenClaw's list; Kimi is not among them and has not been probed"),

	{
		Entitlement: APIKeysOnly,
		Label:       "Raw API keys only",
		Any:         true,
		Because:     "no subscription is in play, so pick on capability and load",
		Source:      "MEMORY.md §8",
	},
}

// codingPlan builds one coding-plan row. The three runtimes that could plausibly
// front a third-party provider are listed every time, with the support level we
// can actually defend for each.
func codingPlan(e Entitlement, label string, openclaw adapter.Support, note string) EntitlementRow {
	const unprobed = "not probed — needs the runtime installed and authenticated (ADAPTERS.md §8)"
	return EntitlementRow{
		Entitlement: e,
		Label:       label,
		Runtimes: []RuntimeSupport{
			{Runtime: adapter.OpenClaw, Support: openclaw, Note: note},
			{Runtime: adapter.OpenCode, Support: adapter.SupportUnknown, Note: unprobed},
			{Runtime: adapter.Hermes, Support: adapter.SupportUnknown, Note: unprobed},
		},
		Because: "whichever runtime lists that provider",
		Source:  "MEMORY.md §8",
	}
}

// Row returns the table row for an entitlement.
func Row(e Entitlement) (EntitlementRow, bool) {
	for _, r := range Table {
		if r.Entitlement == e {
			return r, true
		}
	}
	return EntitlementRow{}, false
}

// Sanctioned returns the runtimes the user's entitlements point at for this
// family of work, best first, plus the row each one came from.
//
// A runtime whose cell is SupportUnknown is returned with its support intact
// rather than filtered out, because the caller has to be able to say "OpenCode
// might list that plan, nobody has checked" instead of silently behaving as if
// it does not exist.
func Sanctioned(ents Entitlements, family ModelFamily) []Sanction {
	var out []Sanction
	seen := map[adapter.Runtime]bool{}
	for _, row := range Table {
		if !ents.Has(row.Entitlement) {
			continue
		}
		if !familyMatches(row.Family, family) {
			continue
		}
		if row.Any {
			out = append(out, Sanction{Any: true, Row: row})
			continue
		}
		for _, rs := range row.Runtimes {
			if seen[rs.Runtime] {
				continue
			}
			seen[rs.Runtime] = true
			out = append(out, Sanction{Runtime: rs.Runtime, Support: rs.Support, Note: rs.Note, Row: row})
		}
	}
	return out
}

// familyMatches decides whether a row governs this request.
//
// A row with a family only applies to that family or to an unspecified one: a
// Claude Max plan is the right answer to "run the tests" and the wrong answer
// to "ask GPT-5 about this". A row with no family applies to everything, which
// is what Copilot and the coding plans are — they front many models.
func familyMatches(row, want ModelFamily) bool {
	if row == FamilyUnspecified {
		return true
	}
	if want == FamilyUnspecified {
		return true
	}
	return row == want
}

// Sanction is one entitlement-derived candidate.
type Sanction struct {
	Runtime adapter.Runtime
	Support adapter.Support
	Note    string
	// Any is the raw-API-keys row: no runtime is named because any is fine.
	Any bool
	Row EntitlementRow
}

// FamilyOf reads a model family out of an utterance, for the rare case where
// the user names one: "ask Claude about this", "get GPT to look at it".
//
// It is a small closed list, not a model catalog. An unrecognised name is
// FamilyUnspecified rather than FamilyOther, because "unspecified" routes on
// everything the user owns and "other" narrows it — and narrowing on a guess is
// how someone ends up paying per token with a subscription sitting unused.
func FamilyOf(text string) ModelFamily {
	t := normalize(text)
	for _, w := range strings.Fields(t) {
		switch w {
		case "claude", "opus", "sonnet", "haiku", "anthropic":
			return FamilyClaude
		case "gpt", "chatgpt", "openai", "o3", "codex":
			return FamilyGPT
		}
	}
	return FamilyUnspecified
}

// Entitled lists the entitlements in the table, in table order. It is what a
// console renders as the checklist a user fills in.
func Entitled() []Entitlement {
	out := make([]Entitlement, 0, len(Table))
	for _, r := range Table {
		out = append(out, r.Entitlement)
	}
	return out
}

// SortEntitlements normalises a user-supplied set into table order so two
// equivalent sets compare equal.
func SortEntitlements(e Entitlements) Entitlements {
	order := map[Entitlement]int{}
	for i, v := range Entitled() {
		order[v] = i
	}
	out := append(Entitlements(nil), e...)
	sort.SliceStable(out, func(i, j int) bool { return order[out[i]] < order[out[j]] })
	return out
}
