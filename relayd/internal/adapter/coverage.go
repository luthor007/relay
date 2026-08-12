package adapter

// This file is ADAPTERS.md §5's coverage table as data, plus the two rows §8
// leaves open. It is the *documented* claim, not a probe result — an adapter
// starts from Baseline and narrows it with what the handshake actually says,
// because the doc is a version behind the binary the moment either ships.
//
// Foundation owns this file. The three adapter packages read it and call With;
// they do not edit it. When a probe on the author's machine closes one of the
// SupportUnknown rows below, the fix is one line here and one line in
// ADAPTERS.md §5 or §8, in the same commit.

// Baseline returns the documented capabilities of a runtime.
func Baseline(r Runtime) Capabilities {
	switch r {
	case ClaudeCode:
		return NewCapabilities(r, map[Capability]Support{
			CapSteer:         SupportYes,
			CapPlan:          SupportSynthesized,
			CapReasoning:     SupportYes,
			CapNeedsInput:    SupportYes,
			CapResume:        SupportYes,
			CapFork:          SupportYes,
			CapCancel:        SupportYes,
			CapCostUSD:       SupportYes,
			CapTokens:        SupportYes,
			CapContextWindow: SupportYes,

			CapPromptImage:           SupportUnknown,
			CapPromptAudio:           SupportUnknown,
			CapPromptEmbeddedContext: SupportUnknown,
		}).
			With(CapPlan, SupportSynthesized,
				"no plan event in stream-json; a plan can only be inferred from tool activity, and PlanUpdated.Synthesized says so").
			With(CapNeedsInput, SupportYes,
				"via the permission-prompt MCP tool, and only while permissions.defaultMode is not auto or a bypass mode — re-check permissionMode on every system/init").
			With(CapCostUSD, SupportYes,
				"result.total_cost_usd; per-turn cost must come from modelUsage rather than result.usage, which sums the requests in a turn")

	case Codex:
		return NewCapabilities(r, map[Capability]Support{
			CapSteer:         SupportYes,
			CapPlan:          SupportYes,
			CapReasoning:     SupportYes,
			CapNeedsInput:    SupportYes,
			CapResume:        SupportYes,
			CapFork:          SupportYes,
			CapCancel:        SupportYes,
			CapCostUSD:       SupportNo,
			CapTokens:        SupportYes,
			CapContextWindow: SupportYes,

			CapPromptImage:           SupportUnknown,
			CapPromptAudio:           SupportUnknown,
			CapPromptEmbeddedContext: SupportUnknown,
		}).
			With(CapSteer, SupportYes,
				"turn/steer, but expectedTurnId is a required precondition and the request fails once that turn is no longer active").
			With(CapCostUSD, SupportNo,
				"no dollar figure anywhere in the Codex contract; USD must be computed from a price table over thread/tokenUsage/updated").
			With(CapContextWindow, SupportYes,
				"modelContextWindow is nullable even when present, so MEMORY.md §9 needs a fallback denominator").
			With(CapNeedsInput, SupportYes,
				"server-to-client requests that block until answered, and only while approvalPolicy is not never and approvalsReviewer is user")

	case OpenClaw, Hermes, OpenCode:
		return NewCapabilities(r, map[Capability]Support{
			CapSteer:         SupportNo,
			CapPlan:          SupportYes,
			CapReasoning:     SupportUnknown,
			CapNeedsInput:    SupportYes,
			CapResume:        SupportUnknown,
			CapFork:          SupportNo,
			CapCancel:        SupportYes,
			CapCostUSD:       SupportNo,
			CapTokens:        SupportNo,
			CapContextWindow: SupportNo,

			CapPromptImage:           SupportUnknown,
			CapPromptAudio:           SupportUnknown,
			CapPromptEmbeddedContext: SupportUnknown,
		}).
			With(CapSteer, SupportNo,
				"verified absent in ACP 0.4.5: the ClientRequest union has eight branches and none of them steers. Cancel and re-prompt for a redirect, queue for an addition").
			With(CapReasoning, SupportUnknown,
				"agent_thought_chunk is protocol-native but whether each of the three runtimes emits it is unverified (ADAPTERS.md §8)").
			With(CapResume, SupportUnknown,
				"agentCapabilities.loadSession is per-runtime and per-version and has not been probed; until it has, the registry must start a new session and say so").
			With(CapCostUSD, SupportNo,
				"ACP 0.4.5 has no cost field; metering is out-of-band and per-runtime").
			With(CapTokens, SupportNo,
				`the word "token" appears twice in the whole schema, both times in the max_tokens stop reason`).
			With(CapContextWindow, SupportNo,
				"no usage object in the protocol, so context pressure for these three has to come from the runtime's own store")
	}

	return NewCapabilities(r, nil)
}
