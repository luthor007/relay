// Package event is the normalized event model that every adapter emits.
//
// ADAPTERS.md §5 is the spec and this package is a transcription of it: nine
// event kinds, three of which reach the user unprompted. Three runtime
// protocols — Claude Code stream-json, Codex app-server JSON-RPC, and ACP —
// collapse to exactly these types and nothing else.
//
// Two rules from the docs are enforced here rather than left to convention:
//
//   - An adapter never emits an event it cannot observe (ADAPTERS.md §5). The
//     shape of that rule in Go is that anything a runtime may not be able to
//     report is a pointer or a flag: [TurnCompleted.Usage] is nil on ACP
//     because the protocol has no token, cost or usage field at all, and
//     [PlanUpdated.Synthesized] is true when a plan was inferred from tool
//     activity rather than stated by the agent.
//   - A replayed event is not news. ACP's session/load replays a whole
//     conversation back as session/update notifications before it resolves,
//     and Claude Code echoes injected turns with isReplay. [Meta.Replay] marks
//     those, and every Ping method returns [PingNone] for them, so the
//     orchestrator cannot wake someone at 3am about a turn from two weeks ago.
//
// The event set is sealed. Adding a tenth kind is a change to ADAPTERS.md §5
// first and this file second, in that order.
package event

import "time"

// Kind names one of the nine normalized events.
type Kind string

const (
	KindTurnStarted   Kind = "turn_started"
	KindTextDelta     Kind = "text_delta"
	KindReasoning     Kind = "reasoning"
	KindToolStarted   Kind = "tool_started"
	KindToolOutput    Kind = "tool_output"
	KindPlanUpdated   Kind = "plan_updated"
	KindNeedsInput    Kind = "needs_input"
	KindTurnCompleted Kind = "turn_completed"
	KindError         Kind = "error"
)

// Kinds lists every event kind in the order ADAPTERS.md §5 gives them.
func Kinds() []Kind {
	return []Kind{
		KindTurnStarted, KindTextDelta, KindReasoning, KindToolStarted,
		KindToolOutput, KindPlanUpdated, KindNeedsInput, KindTurnCompleted,
		KindError,
	}
}

// Ping is how loudly an event reaches a user who did not ask for it.
//
// ADAPTERS.md §7: NeedsInput is blocking — it may interrupt, it never batches,
// and quiet hours do not apply to it, because a session blocked at 3am is still
// blocked at 8am. TurnCompleted and Error are informational — they wait for a
// gap, they batch, and they are silent during quiet hours.
type Ping uint8

const (
	// PingNone means the event never reaches the user on its own.
	PingNone Ping = iota
	// PingInformational waits for a gap in the conversation and may batch.
	PingInformational
	// PingBlocking may interrupt, never batches, and is never suppressed.
	PingBlocking
)

func (p Ping) String() string {
	switch p {
	case PingBlocking:
		return "blocking"
	case PingInformational:
		return "informational"
	default:
		return "none"
	}
}

// PingKinds returns the three kinds marked ⚑PING in ADAPTERS.md §5. Whether a
// particular value of one of those kinds actually pings is decided by its Ping
// method — a replayed or retryable event does not.
func PingKinds() []Kind { return []Kind{KindNeedsInput, KindTurnCompleted, KindError} }

// Meta is carried by every event. Runtime is a plain string rather than
// adapter.Runtime because adapter imports event, not the other way round.
type Meta struct {
	Runtime string    // "claude-code" | "codex" | "openclaw" | "hermes" | "opencode"
	Session string    // Relay's session id, not the runtime's
	Turn    string    // empty only where a runtime emits outside any turn
	At      time.Time // when the adapter observed it, not when we processed it
	Seq     uint64    // per-session monotonic, assigned by the adapter

	// Replay marks an event the adapter is re-reading rather than watching
	// happen: ACP session/load replays the conversation, Claude Code echoes
	// injected turns with isReplay. Replayed events never ping.
	Replay bool
}

// Envelope satisfies the Meta accessor on [Event] for every embedding type.
func (m Meta) Envelope() Meta { return m }

// Event is the sealed union of the nine kinds in ADAPTERS.md §5.
//
// [NeedsInput] is implemented by pointer because it carries the reply channel
// back to the runtime and must not be copied; the other eight are values.
type Event interface {
	Kind() Kind
	Envelope() Meta
	Ping() Ping

	relayEvent()
}

// TurnStarted is the opening boundary of a turn.
//
// Do not build it from Claude Code's system/status{"requesting"} — that fires
// once per API request, and two turns produced three of them in the recorded
// fixture (ADAPTERS.md §2).
type TurnStarted struct {
	Meta
}

func (TurnStarted) Kind() Kind  { return KindTurnStarted }
func (TurnStarted) Ping() Ping  { return PingNone }
func (TurnStarted) relayEvent() {}

// TextDelta is assistant text, token by token. This is the input to streaming
// TTS, which is why SYSTEM.md §7b's largest available latency win lives here.
type TextDelta struct {
	Meta
	Text string
}

func (TextDelta) Kind() Kind  { return KindTextDelta }
func (TextDelta) Ping() Ping  { return PingNone }
func (TextDelta) relayEvent() {}

// Reasoning is model thinking. ADAPTERS.md §5: never spoken, on any runtime.
//
// Summary distinguishes Codex's item/reasoning/summaryTextDelta from its
// item/reasoning/textDelta. Both are Reasoning and neither is spoken; the flag
// exists so the summariser can prefer the summary stream as narration material.
type Reasoning struct {
	Meta
	Text    string
	Summary bool
}

func (Reasoning) Kind() Kind  { return KindReasoning }
func (Reasoning) Ping() Ping  { return PingNone }
func (Reasoning) relayEvent() {}

// ToolStatus is the lifecycle of a tool call. ACP's ToolCallStatus is
// pending → in_progress → completed | failed, and a tool_call_update may carry
// only a toolCallId with every other field null — so "" is a real value meaning
// "this update said nothing about status".
type ToolStatus string

const (
	ToolUnknown    ToolStatus = ""
	ToolPending    ToolStatus = "pending"
	ToolInProgress ToolStatus = "in_progress"
	ToolCompleted  ToolStatus = "completed"
	ToolFailed     ToolStatus = "failed"
)

// ToolStarted is a tool call beginning. Target is the human-readable thing it
// acts on — a path, a command, a URL — and may be empty when the runtime did
// not say. Do not infer one from RawInput; say what is known instead.
type ToolStarted struct {
	Meta
	ID       string // the runtime's tool call id, for correlating ToolOutput
	Tool     string
	Target   string
	RawInput map[string]any
}

func (ToolStarted) Kind() Kind  { return KindToolStarted }
func (ToolStarted) Ping() Ping  { return PingNone }
func (ToolStarted) relayEvent() {}

// ToolOutput is a chunk of output, a status change, or both. Adapters merge
// updates onto the ToolStarted they already have rather than expecting each
// update to be self-describing.
type ToolOutput struct {
	Meta
	ID     string
	Chunk  string
	Status ToolStatus
}

func (ToolOutput) Kind() Kind  { return KindToolOutput }
func (ToolOutput) Ping() Ping  { return PingNone }
func (ToolOutput) relayEvent() {}

// PlanStatus is a plan step's state, shared by Codex turn/plan/updated and
// ACP's plan session update.
type PlanStatus string

const (
	PlanPending    PlanStatus = "pending"
	PlanInProgress PlanStatus = "in_progress"
	PlanCompleted  PlanStatus = "completed"
)

// PlanStep is one line of the agent's own plan.
type PlanStep struct {
	Text   string
	Status PlanStatus
}

// PlanUpdated is the agent stating its plan in structured form — the best
// narration material there is, because §3b's grounding rule is satisfied by
// construction.
//
// Native on Codex and ACP. Claude Code has no plan event, so a Claude Code
// adapter that emits this at all must set Synthesized, and the orchestrator
// must treat a synthesized plan as inference rather than observation.
type PlanUpdated struct {
	Meta
	Steps       []PlanStep
	Explanation string
	Synthesized bool
}

func (PlanUpdated) Kind() Kind  { return KindPlanUpdated }
func (PlanUpdated) Ping() Ping  { return PingNone }
func (PlanUpdated) relayEvent() {}

// StopReason is why a turn ended. The five ACP values are the superset the
// other two runtimes map onto: Claude Code's result subtype and Codex's
// turn.status both collapse into these.
type StopReason string

const (
	StopEndTurn         StopReason = "end_turn"
	StopMaxTokens       StopReason = "max_tokens"
	StopMaxTurnRequests StopReason = "max_turn_requests"
	StopRefusal         StopReason = "refusal"
	StopCancelled       StopReason = "cancelled"
	StopError           StopReason = "error"
)

// OK reports whether the turn finished the work it was asked to do.
func (s StopReason) OK() bool { return s == StopEndTurn }

// Retryable reports whether re-prompting the same session can pick up where
// this stop left off.
//
// StopRefusal is deliberately false: ACP drops the user prompt and everything
// after it from the next prompt on a refusal, so the instruction has to be
// carried again rather than retried on top of what is no longer there.
func (s StopReason) Retryable() bool {
	switch s {
	case StopMaxTokens, StopMaxTurnRequests, StopCancelled:
		return true
	default:
		return false
	}
}

// Usage is what a runtime could tell us about the cost of a turn. Every field
// is a pointer and nil means "this runtime does not report it" — never zero.
//
// The coverage is genuinely uneven (ADAPTERS.md §5): Claude Code reports USD,
// Codex reports tokens but no money anywhere in its contract, and ACP 0.4.5 has
// no token, cost or usage field at all — so an ACP adapter sets Usage to nil
// and the console shows a gap rather than a zero.
type Usage struct {
	CostUSD               *float64
	InputTokens           *int64
	CachedInputTokens     *int64
	OutputTokens          *int64
	ReasoningOutputTokens *int64
	TotalTokens           *int64

	// ContextWindow is the denominator for MEMORY.md §9's compact-at-70%.
	// Codex's modelContextWindow is nullable, so this can be nil even when the
	// token counts are present, and the caller needs a fallback denominator.
	ContextWindow *int64
}

// F64 and I64 build the pointers Usage wants without a local variable.
func F64(v float64) *float64 { return &v }
func I64(v int64) *int64     { return &v }

// ContextPressure returns used/window as a fraction in [0,1] and whether it
// could be computed at all. MEMORY.md §9 compacts on idle at ~0.70.
func (u *Usage) ContextPressure() (float64, bool) {
	if u == nil || u.ContextWindow == nil || *u.ContextWindow <= 0 {
		return 0, false
	}
	var used int64
	var have bool
	for _, p := range []*int64{u.InputTokens, u.CachedInputTokens} {
		if p != nil {
			used += *p
			have = true
		}
	}
	if !have {
		return 0, false
	}
	return float64(used) / float64(*u.ContextWindow), true
}

// TurnCompleted is the turn boundary. ⚑PING — informational.
type TurnCompleted struct {
	Meta
	OK         bool
	StopReason StopReason
	Duration   time.Duration
	Usage      *Usage
}

func (TurnCompleted) Kind() Kind { return KindTurnCompleted }

func (e TurnCompleted) Ping() Ping {
	if e.Replay {
		return PingNone
	}
	return PingInformational
}

func (TurnCompleted) relayEvent() {}

// Error is a turn-level failure. ⚑PING — informational, unless it is not a
// failure yet.
//
// Retryable mirrors Codex's willRetry on its error notification: a retryable
// error is not a user-facing failure and must not wake anyone.
type Error struct {
	Meta
	Code      string
	Message   string
	Retryable bool
	// Fatal means the session is gone, not just this turn.
	Fatal bool
}

func (Error) Kind() Kind { return KindError }

func (e Error) Ping() Ping {
	if e.Replay || e.Retryable {
		return PingNone
	}
	return PingInformational
}

func (Error) relayEvent() {}
