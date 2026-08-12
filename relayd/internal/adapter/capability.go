package adapter

import (
	"maps"
	"sort"
)

// Capability names something a runtime may or may not be able to do. The set
// comes from ADAPTERS.md §5's coverage table plus the two handshake results
// that decide behaviour (ACP's loadSession and promptCapabilities).
type Capability string

const (
	// CapSteer is mid-turn steering: pushing an utterance into a turn that is
	// already running. Verified present on Claude Code and Codex, verified
	// absent on ACP — which covers three of the five runtimes.
	CapSteer Capability = "steer"

	// CapPlan is a native PlanUpdated. Codex's turn/plan/updated and ACP's plan
	// session update are native; Claude Code has none, so it is Synthesized at
	// best and the event carries a flag saying so.
	CapPlan Capability = "plan"

	// CapReasoning is a thinking stream. Protocol-native on all three, but
	// whether each ACP runtime actually emits agent_thought_chunk is unverified
	// (ADAPTERS.md §8).
	CapReasoning Capability = "reasoning"

	// CapNeedsInput is a blocking question we can answer. This is the one to
	// re-check per session rather than per runtime: both Claude Code and Codex
	// have a settings trap that silently disables approvals.
	CapNeedsInput Capability = "needs_input"

	// CapResume is reattaching to an existing session. ACP's
	// agentCapabilities.loadSession is per-runtime and per-version and has not
	// been probed on any of the three, so it starts SupportUnknown there and
	// §4 must not claim reattach works until it has.
	CapResume Capability = "resume"

	// CapFork is starting a new session from an existing one's history.
	CapFork Capability = "fork"

	// CapCancel is stopping a running turn.
	CapCancel Capability = "cancel"

	// CapCostUSD is a per-turn figure in money. Claude Code has it; Codex
	// carries tokens but no dollar figure anywhere in its contract; ACP 0.4.5
	// has no cost field at all, so metering there is out-of-band and
	// per-runtime.
	CapCostUSD Capability = "cost_usd"

	// CapTokens is a per-turn token count.
	CapTokens Capability = "tokens"

	// CapContextWindow is the denominator MEMORY.md §9 needs to compact on idle
	// at ~70%. Codex's modelContextWindow is nullable even when present, so a
	// fallback denominator is required regardless.
	CapContextWindow Capability = "context_window"

	// CapPromptImage, CapPromptAudio and CapPromptEmbeddedContext are ACP's
	// promptCapabilities, each defaulting to false. The baseline everywhere is
	// text and resource_link only.
	CapPromptImage           Capability = "prompt_image"
	CapPromptAudio           Capability = "prompt_audio"
	CapPromptEmbeddedContext Capability = "prompt_embedded_context"
)

// Capabilities lists every capability in a stable order.
func CapabilityNames() []Capability {
	return []Capability{
		CapSteer, CapPlan, CapReasoning, CapNeedsInput, CapResume, CapFork,
		CapCancel, CapCostUSD, CapTokens, CapContextWindow,
		CapPromptImage, CapPromptAudio, CapPromptEmbeddedContext,
	}
}

// Support is how well a runtime does something.
type Support uint8

const (
	// SupportUnknown means nobody has looked yet — a live handshake or a probe
	// on the author's machine decides. It is the honest default for anything
	// ADAPTERS.md §8 lists as needing a runtime installed, and the orchestrator
	// must treat it as "do not rely on this" rather than as no.
	SupportUnknown Support = iota

	// SupportNo means the runtime cannot do it, and the orchestrator degrades
	// visibly rather than pretending.
	SupportNo

	// SupportYes means observed, in the protocol, and safe to depend on.
	SupportYes

	// SupportSynthesized means Relay can produce it by inference rather than by
	// observation — Claude Code plans built out of tool activity. Events
	// produced this way are marked, and the small model says less rather than
	// guessing more.
	SupportSynthesized
)

func (s Support) String() string {
	switch s {
	case SupportNo:
		return "no"
	case SupportYes:
		return "yes"
	case SupportSynthesized:
		return "synthesized"
	default:
		return "unknown"
	}
}

// Observed reports whether an event of this capability would be something the
// adapter actually saw. Synthesized is deliberately false: ADAPTERS.md §5's
// rule is that an adapter never emits an event it cannot observe, and a
// synthesized plan is inference wearing the same shape.
func (s Support) Observed() bool { return s == SupportYes }

// Capabilities is the capability descriptor an orchestrator reads before it
// asks for something. It is immutable; With returns a copy.
type Capabilities struct {
	runtime  Runtime
	protocol Protocol
	support  map[Capability]Support
	notes    map[Capability]string
}

// NewCapabilities builds a descriptor. Anything omitted from set is
// SupportUnknown, which is the correct answer for a capability nobody probed.
func NewCapabilities(r Runtime, set map[Capability]Support) Capabilities {
	c := Capabilities{
		runtime:  r,
		protocol: r.Protocol(),
		support:  make(map[Capability]Support, len(set)),
		notes:    map[Capability]string{},
	}
	maps.Copy(c.support, set)
	return c
}

// Runtime is which runtime this describes.
func (c Capabilities) Runtime() Runtime { return c.runtime }

// Protocol is the wire protocol it is driven over.
func (c Capabilities) Protocol() Protocol { return c.protocol }

// Get returns the support level, SupportUnknown if never set.
func (c Capabilities) Get(cap Capability) Support { return c.support[cap] }

// Has is shorthand for "observed, and safe to depend on".
func (c Capabilities) Has(cap Capability) bool { return c.support[cap] == SupportYes }

// Note returns why a capability is where it is — the string the console shows
// next to a missing feature so "no cost data" reads as a fact rather than a bug.
func (c Capabilities) Note(cap Capability) string { return c.notes[cap] }

// With returns a copy with one capability changed. Adapters start from
// [Baseline] and narrow at handshake time: an ACP initialize response reports
// agentCapabilities, a Claude Code system/init reports permissionMode.
func (c Capabilities) With(cap Capability, s Support, note string) Capabilities {
	out := Capabilities{
		runtime:  c.runtime,
		protocol: c.protocol,
		support:  make(map[Capability]Support, len(c.support)+1),
		notes:    make(map[Capability]string, len(c.notes)+1),
	}
	maps.Copy(out.support, c.support)
	maps.Copy(out.notes, c.notes)
	out.support[cap] = s
	if note != "" {
		out.notes[cap] = note
	} else {
		delete(out.notes, cap)
	}
	return out
}

// All returns a copy of the support map.
func (c Capabilities) All() map[Capability]Support {
	out := make(map[Capability]Support, len(c.support))
	maps.Copy(out, c.support)
	return out
}

// Missing lists the capabilities that are not SupportYes, in stable order.
// This is what the console renders as "what this runtime cannot do".
func (c Capabilities) Missing() []Capability {
	var out []Capability
	for _, k := range CapabilityNames() {
		if c.support[k] != SupportYes {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Require returns nil when the capability is observed and an *UnsupportedError
// otherwise. This is the check an adapter method makes before doing something
// the runtime cannot do — it returns an error the caller can degrade against,
// never a panic.
func (c Capabilities) Require(cap Capability) error {
	if c.support[cap] == SupportYes {
		return nil
	}
	return &UnsupportedError{
		Runtime:    c.runtime,
		Capability: cap,
		Support:    c.support[cap],
		Note:       c.notes[cap],
	}
}

// CheckTurn reports whether a turn's content is admissible on this runtime.
// A glasses photo on a runtime that never advertised image support is refused
// here rather than dropped silently downstream.
func CheckTurn(c Capabilities, t Turn) error {
	for _, need := range t.Requires() {
		if err := c.Require(need); err != nil {
			return err
		}
	}
	return nil
}
