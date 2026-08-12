package orchestrator

import (
	"fmt"
	"sort"
	"strings"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/compaction"
)

// Harness is what the big model needs to know about one agent runtime before it
// sends work there.
//
// Relay drives five harnesses that are genuinely different products, and the
// orchestrator has been choosing between them on the basis of nothing. A model
// that knows Claude Code takes a `--session-id` we chose, that Codex refuses
// same-turn steering during a review, and that the three ACP runtimes report no
// token usage at all is a model that can pick the right one and prompt it
// properly. That is the "super prompter" job, and it is the one thing the
// orchestrator can do that no individual runtime can: each of them sees only
// itself.
//
// Two halves, with different provenance, and the split matters:
//
//   - [Harness.Capabilities] and [Harness.Compaction] are **computed** from
//     `adapter.Baseline` and `compaction.MechanismFor`. They cannot drift from
//     what the adapters actually do, because they are the same data the
//     adapters read.
//   - [Harness.Strengths] and [Harness.Prompting] are **curated** from
//     ADAPTERS.md and MEMORY.md. They are prose, they are a version behind the
//     binary the moment either ships, and the doc comment on each says where it
//     came from so it can be checked.
//
// Nothing here is guessed. A runtime whose behaviour is unverified says so —
// ADAPTERS.md §8 leaves rows open on purpose, and a brief that filled them in
// with plausible text would be worse than a brief that admits the gap, because
// the model would act on it.
type Harness struct {
	Runtime adapter.Runtime
	// Summary is what this runtime is, in one sentence.
	Summary string
	// Protocol is how Relay talks to it.
	Protocol string
	// Strengths is what to send here rather than elsewhere.
	Strengths []string
	// Prompting is how to get good work out of it.
	Prompting []string
	// Commands are the in-band controls, as the runtime spells them.
	Commands []Command
	// Traps are the things that go wrong, and they are the most valuable field.
	Traps []string
}

// Command is one in-band control.
type Command struct {
	Name string
	Does string
}

// entitlement is the one routing rule that is not about capability.
//
// ORCHESTRATOR.md: Claude-model work goes to Claude Code because that is where
// the subscription works. It is a billing fact rather than a technical one, and
// it is the single most common reason to override every other consideration.
const entitlementNote = "Claude-model work belongs here: a Claude Max or Pro " +
	"subscription powers Claude Code and does not power an API key elsewhere, so " +
	"sending Claude work to another runtime spends money the user has already spent."

// harnesses is the curated half. Everything here traces to ADAPTERS.md, MEMORY.md
// or a probe recorded in this repository.
var harnesses = map[adapter.Runtime]Harness{
	adapter.ClaudeCode: {
		Runtime:  adapter.ClaudeCode,
		Summary:  "Anthropic's own coding agent. The most instrumented of the five and the only one that reports cost in money.",
		Protocol: "stream-json over stdio, with a permission-prompt MCP tool for approvals",
		Strengths: []string{
			"Long multi-file edits in a repository it can read whole.",
			"Anything where you want the cost afterwards: it is the only runtime reporting USD.",
			entitlementNote,
		},
		Prompting: []string{
			"Say the outcome, not the steps. It plans better than it follows.",
			"Name files and paths explicitly when you have them — it reads before editing and a named path skips a search.",
			"It reaches for tools conservatively, so a request that needs one should say when to use it.",
		},
		Commands: []Command{
			{"/compact", "summarise the conversation and free the window"},
			{"/clear", "start fresh, losing the context"},
		},
		Traps: []string{
			"Approvals are silently disabled in an auto or bypass permission mode: the permission-prompt tool never fires, so a session in that mode will never ask before acting. Relay refuses to start one.",
			"There is no plan event in stream-json. Any plan is inferred from tool activity and is marked synthesized; do not narrate it as the agent's stated plan.",
			"Per-turn cost must come from modelUsage. result.usage sums the requests in a turn and reads high.",
			"A compaction produces no event — the only corroboration is the next context reading.",
		},
	},

	adapter.Codex: {
		Runtime:  adapter.Codex,
		Summary:  "OpenAI's agent, driven over a JSON-RPC app-server. The only one with a first-class steering call.",
		Protocol: "app-server JSON-RPC",
		Strengths: []string{
			"Work you expect to redirect mid-flight: turn/steer is a protocol call rather than a queued message.",
			"Tasks where a stated plan matters — turn/plan/updated is native, not inferred.",
			"Compaction is a protocol call with an observable result, which no other runtime offers.",
		},
		Prompting: []string{
			"Steering works, so a shorter opening prompt and a correction is a reasonable strategy here and nowhere else.",
			"It reports tokens but never money; do not promise the user a cost from this runtime.",
		},
		Commands: []Command{
			{"thread/compact/start", "compact, and report an item/completed of type contextCompaction"},
			{"turn/steer", "push new instructions into a running turn"},
		},
		Traps: []string{
			"NEVER raise model_auto_compact_token_limit to or above model_context_window. It converts a graceful pause into a terminal ContextWindowExceeded that can only be answered by starting a new thread.",
			"Review turns and manual compaction turns reject same-turn steering; wait for the run to finish.",
			"modelContextWindow is nullable even when present, so a pressure reading needs a fallback.",
		},
	},

	adapter.OpenClaw: {
		Runtime:  adapter.OpenClaw,
		Summary:  "A general-purpose agent with a large skill ecosystem and its own gateway. Reached over ACP.",
		Protocol: "ACP",
		Strengths: []string{
			"Anything covered by an existing skill: it ships dozens, as <name>/SKILL.md, and reads more from disk.",
			"Long-running and messaging-shaped work — it has queue modes for talking over a running turn.",
		},
		Prompting: []string{
			"Check its skills before writing instructions from scratch; a skill it already has beats a paragraph you wrote.",
			"It recovers on context overflow independently of its threshold, so it tolerates a long conversation better than its settings suggest.",
		},
		Commands: []Command{
			{"/compact", "summarise and free the window"},
		},
		Traps: []string{
			"ACP has no usage object at all: no tokens, no cost, no window. Context pressure here is a turn count, not a percentage.",
			"Whether it can reload a session is unverified (ADAPTERS.md §8). Do not promise a resume you have not seen work.",
		},
	},

	adapter.Hermes: {
		Runtime:  adapter.Hermes,
		Summary:  "A self-improving agent: it writes skills from experience and rewrites them during use. Reached over ACP.",
		Protocol: "ACP",
		Strengths: []string{
			"Repeated work: it turns a sequence it has done into a skill, so the second time is cheaper than the first.",
			"Tasks where cross-session recall matters — it keeps its own searchable session history.",
		},
		Prompting: []string{
			"Tell it when something is worth remembering. It curates its own memory and a nudge lands better than a restatement.",
			"It titles its own sessions, so a subject you set is a hint rather than the last word.",
		},
		Commands: []Command{
			{"/compress", "compact — but take the compression lease first"},
		},
		Traps: []string{
			"compression_locks is a real lease with upstream concurrency tests behind it. Take it; never race it. Relay skips a Hermes session rather than compact one it cannot lease.",
			"Its compression threshold defaults low enough to fight an external idle pass. Raising it is the one setting change worth making.",
			"ACP, so no usage object — same blindness as OpenClaw.",
		},
	},

	adapter.OpenCode: {
		Runtime:  adapter.OpenCode,
		Summary:  "A lightweight open-source agent with an HTTP server alongside its ACP interface.",
		Protocol: "ACP, plus an HTTP server for some operations",
		Strengths: []string{
			"Small, well-scoped edits where startup cost matters.",
			"Anything you want to drive from HTTP as well as from a session.",
		},
		Prompting: []string{
			"Keep the scope tight. It is the lightest of the five and does best with a bounded task.",
		},
		Commands: []Command{
			{"POST /session/{id}/summarize", "compact — over HTTP, outside ACP entirely"},
		},
		Traps: []string{
			"Its MCP config entry is spelled differently from the other four: type \"remote\" and \"local\", not \"http\" and a command string.",
			"Disabling its auto-compaction sets finish: \"error\" and idles the session. Never turn it off.",
			"ACP, so no usage object.",
		},
	},
}

// HarnessFor returns the brief for one runtime.
func HarnessFor(rt adapter.Runtime) (Harness, bool) {
	h, ok := harnesses[rt]
	return h, ok
}

// Harnesses returns every brief, in a stable order.
func Harnesses() []Harness {
	out := make([]Harness, 0, len(harnesses))
	for _, h := range harnesses {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Runtime < out[j].Runtime })
	return out
}

// Brief renders one harness for the model.
//
// Capabilities and the compaction mechanism are appended from the live tables
// rather than written here, so the half of this document that can go stale
// silently is the half that cannot: if an adapter narrows a capability after a
// handshake, the brief narrows with it.
func (h Harness) Brief() string {
	var b strings.Builder

	fmt.Fprintf(&b, "# %s\n%s\nProtocol: %s\n", h.Runtime, h.Summary, h.Protocol)

	section(&b, "Send work here when", h.Strengths)
	section(&b, "Prompting", h.Prompting)

	if len(h.Commands) > 0 {
		b.WriteString("\nCommands\n")
		for _, c := range h.Commands {
			fmt.Fprintf(&b, "  %s — %s\n", c.Name, c.Does)
		}
	}

	// Computed, not curated. adapter.Baseline is the documented coverage table
	// and carries its own reason per row; a gap with a reason is far more
	// useful to a model than a gap.
	caps := adapter.Baseline(h.Runtime)
	if missing := caps.Missing(); len(missing) > 0 {
		b.WriteString("\nCannot do, or unverified\n")
		for _, c := range missing {
			// Support has three states and the middle one matters: "unknown"
			// is a row ADAPTERS.md §8 leaves open, and telling the model it
			// cannot do something when nobody has checked is a different lie
			// from telling it the truth.
			state := caps.Get(c)
			if note := caps.Note(c); note != "" {
				fmt.Fprintf(&b, "  %s (%s) — %s\n", c, state, note)
				continue
			}
			fmt.Fprintf(&b, "  %s (%s)\n", c, state)
		}
	}

	if m, ok := compaction.MechanismFor(h.Runtime); ok {
		fmt.Fprintf(&b, "\nCompaction: %s", m.Method)
		if m.RequiresLease {
			b.WriteString(" (takes a lease first)")
		}
		if !m.Observable {
			b.WriteString(" — produces no event, so completion cannot be confirmed directly")
		}
		b.WriteString("\n")
	}

	// Last, because it is the part worth reading twice.
	section(&b, "Traps", h.Traps)
	return b.String()
}

func section(b *strings.Builder, title string, lines []string) {
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s\n", title)
	for _, l := range lines {
		fmt.Fprintf(b, "  - %s\n", l)
	}
}

// Roster is the one-line-per-runtime summary, for choosing between them.
//
// Separate from [Harness.Brief] because they answer different questions and
// cost different amounts of context: "which of these five" is a list, and "how
// do I drive this one" is a page. Loading five pages to answer the first
// question is how a tool set stops being worth having.
func Roster() string {
	var b strings.Builder
	b.WriteString("Agent runtimes on this machine. Ask for a brief before sending work to one you have not used.\n\n")
	for _, h := range Harnesses() {
		fmt.Fprintf(&b, "%-12s %s\n", h.Runtime, h.Summary)
	}
	return b.String()
}
