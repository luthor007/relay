package install

import (
	"context"
	"fmt"
	"strings"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/detect"
	"github.com/luthor007/relay/relayd/internal/llm"
)

// MEMORY.md §8's one missing input: what the user already pays for.
//
// The table in internal/routing is complete and tested — Claude-model work goes
// to Claude Code because a Claude Max plan is the only credential Claude Code
// can use, and sending that work anywhere else spends money the user has
// already spent. It had never fired on a real machine, because nothing anywhere
// recorded an entitlement, so the set was empty on every install and step 3 of
// the priority order was skipped forever.
//
// Three rules shape this step, and all three are load-bearing:
//
//  1. **An entitlement is declared, never inferred.** MEMORY.md §8 says so in
//     as many words and routing/entitlement.go's doc comment repeats it: a
//     Claude Code binary on PATH is not evidence of a Claude Max plan. The
//     installer already knows which auth method backs each orchestrator model,
//     including the subscription rows, and promoting that into an entitlement
//     would be one line and would be wrong — that answer is about the
//     orchestrator's own credential, not about which agent runtime a
//     subscription pays for. So the auth answer is quoted back as *context* in
//     the question body and the default is still no.
//
//  2. **Every question defaults to no.** `relay setup --yes` runs through
//     [Auto], which returns each confirmation's default, so an unattended
//     install records nothing at all. An entitlement overrides capability
//     comparison; one invented by a script is a routing decision nobody made.
//
//  3. **Only ask what could matter here.** A question about a Copilot
//     subscription on a machine with neither OpenClaw nor OpenCode records a
//     fact that can never fire. Each question is gated on the detection report
//     (plus anything step 2 just installed), so the common two-runtime machine
//     MEMORY.md §1 measured sees one or two questions and a bare machine sees
//     none.
//
// What is deliberately *not* recorded is routing.APIKeysOnly. It is the last
// row of the table — "no subscription, pick on capability and load" — and today
// it is behaviourally identical to the empty set, because [routing.RuntimeRouter]
// falls through to capability and load either way. Recording it would be a
// claim about the user's billing with no effect on anything, which is the kind
// of stored fact that goes stale silently.

// EntitlementsOutcome is what this step recorded.
type EntitlementsOutcome struct {
	// Entitlements are ids from config.KnownEntitlements, in table order.
	Entitlements []string
	// Asked is every question this step put to the user, so a test can assert
	// that a bare machine was asked nothing.
	Asked []string
	// Skipped says why nothing was asked, when nothing was.
	Skipped string
}

// entitlementQuestion is one row of the ask.
type entitlementQuestion struct {
	// id is the question id, stable, and answered by name in a scripted run.
	id string
	// entitlement is what a yes records.
	entitlement string
	prompt      string
	body        string
	// runtimes are the runtimes this entitlement can route to. The question is
	// only asked when at least one of them is on this machine.
	runtimes []adapter.Runtime
}

// entitlementQuestions is the ask, in routing.Table's order.
//
// The bodies say what the answer is *for* in the same voice as claudePreamble,
// because "do you have a Claude subscription?" with no context reads as a
// licence check. It is not one: nothing here gives Relay the subscription, and
// a Claude plan still only works inside Claude Code.
func entitlementQuestions() []entitlementQuestion {
	return []entitlementQuestion{
		{
			id:          "entitlements.claude",
			entitlement: "claude-subscription",
			prompt:      "Do you pay for Claude Max or Claude Pro?",
			body: "If you do, Claude-model work should go to Claude Code — that is the only " +
				"client that plan works in, and running the same work through an API key " +
				"bills you twice for it.",
			runtimes: []adapter.Runtime{adapter.ClaudeCode},
		},
		{
			id:          "entitlements.chatgpt",
			entitlement: "chatgpt-subscription",
			prompt:      "Do you pay for ChatGPT Plus or Pro?",
			body:        "If you do, GPT work should go to Codex, which is where that plan signs in.",
			runtimes:    []adapter.Runtime{adapter.Codex},
		},
		{
			id:          "entitlements.copilot",
			entitlement: "github-copilot",
			prompt:      "Do you pay for GitHub Copilot?",
			body:        "OpenClaw and OpenCode both expose Copilot as a provider.",
			runtimes:    []adapter.Runtime{adapter.OpenClaw, adapter.OpenCode},
		},
	}
}

// codingPlanChoices are MEMORY.md §8's four coding plans.
//
// Kimi is on the list and its routing row is SupportUnknown: ORCHESTRATOR.md
// §2b read OpenClaw's own auth-choice list and Kimi is not in it. Recording it
// is still worth doing — the router reports "may front that plan, nobody has
// checked" instead of silently behaving as if the plan does not exist, which is
// the ADAPTERS.md §8 discipline and is the difference between a gap somebody
// can close and a gap nobody can see.
func codingPlanChoices() []Choice {
	return []Choice{
		{ID: "zai-coding-plan", Label: "Z.AI coding plan"},
		{ID: "minimax-coding-plan", Label: "MiniMax coding plan"},
		{ID: "qwen-coding-plan", Label: "Qwen coding plan"},
		{ID: "kimi-coding-plan", Label: "Kimi coding plan",
			Hint: "no runtime has been probed for this one, so Relay will not spend it on a guess"},
	}
}

const entitlementsPreamble = `Relay routes work to whichever agent runtime it should, and the ` +
	`sharpest input is what you already pay for. A Claude Max plan makes Claude Code free at ` +
	`the margin; the same work through an API key is a bill you did not expect.

Relay will not guess this. A runtime being installed says nothing about which subscription ` +
	`is behind it, and guessing wrong is the exact failure this step exists to prevent. So it ` +
	`asks, the default to every question is no, and nothing here is checked against a provider.

To be clear about what this is not: it does not give Relay your subscription, and it does ` +
	`not change where a plan works. Your Claude plan still only works inside Claude Code. ` +
	`This decides where Relay sends work, nothing else, and you can change it later with ` +
	"`relay entitlements`."

// chooseEntitlements asks what the user pays for, and records only that.
func chooseEntitlements(
	ctx context.Context,
	opts Options,
	rep detect.Report,
	installed []adapter.Runtime,
	models ModelsOutcome,
) (EntitlementsOutcome, error) {
	var out EntitlementsOutcome
	p := opts.Prompt

	present := presentRuntimes(rep, installed)

	var ask []entitlementQuestion
	for _, q := range entitlementQuestions() {
		if anyPresent(present, q.runtimes) {
			ask = append(ask, q)
		}
	}
	// The coding plans front many models through one of three ACP runtimes, so
	// they share a question rather than getting four.
	planRuntimes := []adapter.Runtime{adapter.OpenClaw, adapter.OpenCode, adapter.Hermes}
	askPlans := anyPresent(present, planRuntimes)

	if len(ask) == 0 && !askPlans {
		// Nothing on this machine is covered by any row of the table, so every
		// possible answer would record a fact that can never fire. Saying
		// nothing is better than a section whose only content is that it does
		// not apply.
		out.Skipped = "no runtime here is covered by a subscription Relay can route to"
		return out, nil
	}

	p.Section("What do you already pay for?", entitlementsPreamble)
	if note := subscriptionContext(models); note != "" {
		p.Say("\n  %s", wrapIndent(note, 2, 76))
	}

	for _, q := range ask {
		out.Asked = append(out.Asked, q.id)
		yes, err := p.Confirm(Confirm{
			ID:     q.id,
			Prompt: q.prompt,
			Body:   q.body,
			// No. Always no. See rule 2 above.
			Default: false,
		})
		if err != nil {
			return out, err
		}
		if yes {
			out.Entitlements = append(out.Entitlements, q.entitlement)
		}
	}

	if askPlans {
		out.Asked = append(out.Asked, "entitlements.coding_plan")
		yes, err := p.Confirm(Confirm{
			ID:     "entitlements.coding_plan",
			Prompt: "Do you pay for a coding plan (Z.AI, MiniMax, Qwen, Kimi)?",
			Body: "These front many models through one subscription, and OpenClaw, OpenCode " +
				"and Hermes can each sign in to some of them.",
			Default: false,
		})
		if err != nil {
			return out, err
		}
		if yes {
			out.Asked = append(out.Asked, "entitlements.coding_plan.which")
			which, err := p.Select(Question{
				ID:      "entitlements.coding_plan.which",
				Title:   "Which plan?",
				Body:    "One of the four MEMORY.md §8 names. Run this step again to add another.",
				Choices: codingPlanChoices(),
				Default: "zai-coding-plan",
			})
			if err != nil {
				return out, err
			}
			out.Entitlements = append(out.Entitlements, which)
		}
	}

	switch len(out.Entitlements) {
	case 0:
		p.Say("\n  Nothing recorded. Routing will pick on capability and load, which is the " +
			"honest answer when nobody has said otherwise.")
	default:
		p.Say("\n  Recorded: %s", strings.Join(out.Entitlements, ", "))
	}
	return out, nil
}

// presentRuntimes is what this machine has after step 2 — detected, or just
// installed by the offer the user accepted a moment ago.
//
// StatusAbsent is the only exclusion. A runtime that is installed and never run
// still counts here: the entitlement is a billing fact and it does not become
// truer or falser depending on whether the binary has been opened. Routing's
// own never-route-without-history rule handles that separately and outranks
// this, which is why recording an entitlement for an unused runtime is honest
// rather than misleading.
func presentRuntimes(rep detect.Report, installed []adapter.Runtime) map[adapter.Runtime]bool {
	out := map[adapter.Runtime]bool{}
	for _, f := range rep.Findings {
		if f.Status() != detect.StatusAbsent {
			out[f.Runtime] = true
		}
	}
	for _, rt := range installed {
		out[rt] = true
	}
	return out
}

func anyPresent(present map[adapter.Runtime]bool, want []adapter.Runtime) bool {
	for _, rt := range want {
		if present[rt] {
			return true
		}
	}
	return false
}

// subscriptionContext quotes the models step's own answer back, when it chose a
// subscription row.
//
// It is context and never an inference. Choosing "OpenAI Codex (ChatGPT OAuth)"
// for the big model is strong evidence the user has a ChatGPT plan — and the
// next question still defaults to no, because "strong evidence" is how a
// metered bill arrives. The point of printing it is that a user who just typed
// that answer should not have to wonder whether we forgot.
func subscriptionContext(m ModelsOutcome) string {
	var seen []string
	for _, c := range []ModelChoice{m.Small, m.Big} {
		if c.Auth.Kind != llm.AuthSubscription || c.Auth.Label == "" {
			continue
		}
		line := fmt.Sprintf("%s for the %s model", c.Auth.Label, c.Role)
		if !contains(seen, line) {
			seen = append(seen, line)
		}
	}
	if len(seen) == 0 {
		return ""
	}
	return "You chose " + strings.Join(seen, " and ") + " a moment ago. That is the " +
		"orchestrator's own credential and it is not the same question as this one, so " +
		"nothing below has been answered for you."
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// line summarises the outcome for the installer's closing report.
func (e EntitlementsOutcome) line() string {
	if len(e.Entitlements) > 0 {
		return strings.Join(e.Entitlements, ", ")
	}
	if e.Skipped != "" {
		return "none — " + e.Skipped
	}
	return "none recorded — routing falls back to capability and load"
}
