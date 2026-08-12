package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/luthor007/relay/relayd/internal/event"
)

// InitInfo is system/init, which Claude Code re-emits at the head of every turn.
// That repetition is the answer to SYSTEM.md §9 problem 6 (tool-list refresh)
// for this runtime, and it is also where the permission check lives.
type InitInfo struct {
	Version        string
	Model          string // the *decorated* id, e.g. "claude-opus-5[1m]"
	CWD            string
	SessionID      string
	PermissionMode string
	APIKeySource   string
	OutputStyle    string
	Tools          []string
	MCPServers     []MCPServerStatus
	Capabilities   []string
	At             time.Time
}

// MCPServerStatus is one entry of system/init's mcp_servers. MEMORY.md §7's
// registry reconciliation reads Claude Code's side of the picture from here.
type MCPServerStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// HookRun is one SessionStart-style hook, matched from hook_started to
// hook_response by hook_id. Hooks run concurrently, so the responses do not
// come back in the order the starts went out.
type HookRun struct {
	ID       string
	Name     string
	Event    string
	Started  time.Time
	Finished time.Time
	Done     bool
	ExitCode int
	Outcome  string
	Stdout   string
	Stderr   string
}

// OK reports whether a finished hook succeeded. An unfinished hook is not a
// failure — it is a hook we are still waiting on.
func (h HookRun) OK() bool { return !h.Done || (h.ExitCode == 0 && h.Outcome == "success") }

// ResultInfo is a result event kept whole, because ADAPTERS.md §6 summarises
// from it and DASHBOARD.md renders it. Result is the final assistant text and
// is §6's input to the small model.
type ResultInfo struct {
	Subtype        string
	Result         string
	StopReason     string
	TerminalReason string
	IsError        bool
	// APIErrorStatus is null on a healthy turn and is the reason on an
	// unhealthy one.
	APIErrorStatus string
	// NumTurns is model iterations *within this turn* — a tool round trip makes
	// it 2. It is not cumulative.
	NumTurns      int
	DurationMS    int64
	DurationAPIMS int64

	// Latency, free with every turn: time to first token, time to first
	// streamed token, and time spent before the request went out.
	TTFTMs          int64
	TTFTStreamMs    int64
	TimeToRequestMs int64

	// PermissionDenials is how many tools were refused during this turn. Empty
	// on every turn of the vendored fixture, which was recorded in a permission
	// mode that never asked.
	PermissionDenials int

	// Tokens for this turn, summed over the requests it took. Not the live
	// context — see ContextState.
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64

	// TotalCostUSD is cumulative for the *session*; CostUSD is this turn, which
	// is the delta between consecutive result events. ADAPTERS.md §2: the
	// numbers are free, the subtraction is ours.
	TotalCostUSD float64
	CostUSD      float64
	HaveCost     bool

	CanonicalModel  string
	Provider        string
	ContextWindow   int64
	MaxOutputTokens int64
	At              time.Time
}

// ContextState is the live context size and its denominator.
//
// This is deliberately *not* carried on TurnCompleted.Usage. result.usage sums
// every request in a turn — turn 1 of the vendored fixture reports 51,997
// cache-read tokens for a context that was actually ~33,600 — so dividing it by
// contextWindow would overstate pressure by the number of tool round trips and
// compact a session that is barely full. The live figure is the *most recent
// request's* usage, from message_start or message_delta, and it reads
// 33,497 → 33,609 → 33,637 across the fixture's three requests.
type ContextState struct {
	Used   int64
	Window int64
	// Model is the decorated model id the window came from.
	Model string
}

// Pressure is Used/Window, and ok is false until both are known. MEMORY.md §9
// compacts on idle at ~0.70.
func (c ContextState) Pressure() (float64, bool) {
	if c.Window <= 0 || c.Used <= 0 {
		return 0, false
	}
	return float64(c.Used) / float64(c.Window), true
}

// queuedTurn is a turn we have written to stdin and not yet seen acknowledged.
type queuedTurn struct {
	id   string
	text string
}

type blockState struct {
	kind     string
	toolID   string
	toolName string
	json     strings.Builder
}

// normalizer turns stream-json lines into ADAPTERS.md §5's nine events.
//
// It owns every piece of protocol state that outlives a single line — the open
// turn, the partially streamed tool arguments, the running cost total — and it
// is the only place in this package that decides what an event *means*.
type normalizer struct {
	log *slog.Logger
	now func() time.Time
	out func(event.Event)

	// onInit fires after every system/init, so the session can re-check the
	// permission mode at the head of every turn.
	onInit func(InitInfo)

	mu sync.Mutex

	runtime string
	session string
	seq     uint64

	queued   []queuedTurn
	turn     string
	turnOpen bool
	genTurn  int

	// replay is set while we believe the runtime is re-reading history rather
	// than working: from a --resume until our own first turn is acknowledged.
	// Replayed events never ping, which is what stops a reattach from firing a
	// completion ping for every turn in a session's past.
	replay bool

	msgID        string
	sawTextDelta map[string]bool
	blocks       map[int]*blockState
	emittedTools map[string]bool

	init    *InitInfo
	rate    *RateLimitInfo
	ctx     ContextState
	last    *ResultInfo
	prevTot float64
	haveTot bool
	hooks   map[string]*HookRun
	hookSeq []string
	unknown map[string]int
}

type normOptions struct {
	Runtime string
	Session string
	Log     *slog.Logger
	Now     func() time.Time
	Out     func(event.Event)
	OnInit  func(InitInfo)
	Replay  bool
}

func newNormalizer(o normOptions) *normalizer {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Out == nil {
		o.Out = func(event.Event) {}
	}
	return &normalizer{
		log:          o.Log,
		now:          o.Now,
		out:          o.Out,
		onInit:       o.OnInit,
		runtime:      o.Runtime,
		session:      o.Session,
		replay:       o.Replay,
		sawTextDelta: map[string]bool{},
		blocks:       map[int]*blockState{},
		emittedTools: map[string]bool{},
		hooks:        map[string]*HookRun{},
		unknown:      map[string]int{},
	}
}

// queueTurn records a turn written to stdin. The isReplay echo pops it, which
// is the only observable "your turn is now running" signal in the protocol.
func (n *normalizer) queueTurn(id, text string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.queued = append(n.queued, queuedTurn{id: id, text: text})
}

func (n *normalizer) setReplay(v bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.replay = v
}

// currentTurn is the open turn, or "" when no turn is running.
func (n *normalizer) currentTurn() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.turnOpen {
		return ""
	}
	return n.turn
}

func (n *normalizer) meta() event.Meta {
	n.seq++
	return event.Meta{
		Runtime: n.runtime,
		Session: n.session,
		Turn:    n.turn,
		At:      n.now(),
		Seq:     n.seq,
		Replay:  n.replay,
	}
}

// outOfBandMeta stamps an event that did not come off the stdout stream — a
// blocked permission call, a process that died. Seq is per-session monotonic
// and assigned by the adapter, so those events have to draw from the same
// counter as everything else or the ordering the orchestrator sorts by is a
// lie.
func (n *normalizer) outOfBandMeta(turn string) event.Meta {
	n.mu.Lock()
	defer n.mu.Unlock()
	m := n.meta()
	if turn != "" {
		m.Turn = turn
	}
	return m
}

// push decodes one line and emits whatever it normalizes to. A line that does
// not parse, or carries a type nobody has seen, is counted and logged — never
// fatal. The very first lines of a real session are hook events that
// ADAPTERS.md §2 did not document until the fixture was recorded, and an
// adapter that switches exhaustively on system.subtype falls over them.
func (n *normalizer) push(line []byte) {
	line = trimSpace(line)
	if len(line) == 0 {
		return
	}
	var w wireLine
	if err := json.Unmarshal(line, &w); err != nil {
		n.mu.Lock()
		n.unknown["!malformed"]++
		n.mu.Unlock()
		n.log.Warn("claudecode: undecodable line", "err", err, "bytes", len(line))
		return
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	// parent_tool_use_id is null on all 49 events of the vendored trace. A
	// sub-agent (Task) is the obvious thing it exists to attribute, but that is
	// inference — so the adapter counts a non-null one rather than claiming
	// sub-agent attribution it has never seen. The first trace that carries one
	// shows up in Unseen() instead of vanishing.
	if w.ParentToolUseID != nil && *w.ParentToolUseID != "" {
		n.unknown["parent_tool_use_id"]++
		n.log.Debug("claudecode: an event carried parent_tool_use_id, which has never been observed non-null",
			"parent_tool_use_id", *w.ParentToolUseID, "type", w.Type)
	}

	switch w.Type {
	case "system":
		n.system(&w)
	case "user":
		n.user(&w)
	case "assistant":
		n.assistant(&w)
	case "stream_event":
		n.streamEvent(&w)
	case "rate_limit_event":
		n.rateLimit(&w)
	case "result":
		n.result(&w)
	default:
		n.unknown["type:"+w.Type]++
		n.log.Debug("claudecode: unknown top-level type", "type", w.Type)
	}
}

func (n *normalizer) system(w *wireLine) {
	switch w.Subtype {
	case "init":
		i := InitInfo{
			Version:        w.Version,
			Model:          w.Model,
			CWD:            w.CWD,
			SessionID:      w.SessionID,
			PermissionMode: w.PermissionMode,
			APIKeySource:   w.APIKeySource,
			OutputStyle:    w.OutputStyle,
			Tools:          append([]string(nil), w.Tools...),
			Capabilities:   append([]string(nil), w.Capabilities...),
			At:             n.now(),
		}
		for _, m := range w.MCPServers {
			i.MCPServers = append(i.MCPServers, MCPServerStatus{Name: m.Name, Status: m.Status})
		}
		n.init = &i
		if n.onInit != nil {
			cb := n.onInit
			// The callback narrows capabilities and must not re-enter the
			// normalizer's lock; it is called with the lock held on purpose so
			// the session never sees a half-applied init, and it is documented
			// as forbidden to call back in.
			cb(i)
		}

	case "status":
		// Once per API *request*, not once per turn: two turns produced three of
		// these in the fixture. Never a turn boundary, so never an event.
		n.unknown["status:"+w.Status]++

	case "hook_started":
		h := &HookRun{ID: w.HookID, Name: w.HookName, Event: w.HookEvent, Started: n.now()}
		n.hooks[w.HookID] = h
		n.hookSeq = append(n.hookSeq, w.HookID)

	case "hook_response":
		h := n.hooks[w.HookID]
		if h == nil {
			// A response with no start. Record it rather than dropping it; the
			// pairing is by hook_id and we may simply have attached late.
			h = &HookRun{ID: w.HookID, Name: w.HookName, Event: w.HookEvent}
			n.hooks[w.HookID] = h
			n.hookSeq = append(n.hookSeq, w.HookID)
		}
		h.Done = true
		h.Finished = n.now()
		h.Outcome = w.Outcome
		h.Stdout = w.Stdout
		h.Stderr = w.Stderr
		if w.ExitCode != nil {
			h.ExitCode = *w.ExitCode
		}
		if !h.OK() {
			// A failing SessionStart hook is a common reason a session behaves
			// oddly from the first turn, and it is user-visible
			// misconfiguration rather than a model failure. It is observed, so
			// it is reportable.
			msg := fmt.Sprintf("hook %s exited %d (%s)", h.Name, h.ExitCode, h.Outcome)
			if s := strings.TrimSpace(h.Stderr); s != "" {
				msg += ": " + s
			}
			n.out(event.Error{
				Meta:    n.meta(),
				Code:    "hook_failed",
				Message: msg,
			})
		}

	default:
		n.unknown["system:"+w.Subtype]++
		n.log.Debug("claudecode: unknown system subtype", "subtype", w.Subtype)
	}
}

func (n *normalizer) user(w *wireLine) {
	switch {
	case w.IsReplay != nil:
		n.turnAck(w)
	case len(w.ToolUseResult) > 0 || hasToolResult(w.Message):
		n.toolResults(w)
	default:
		n.unknown["user:unknown-shape"]++
		n.log.Debug("claudecode: user event with neither isReplay nor tool_use_result")
	}
}

// turnAck handles the echo of a line we wrote to stdin.
//
// It is the acknowledgement that the turn was accepted, and this adapter uses
// it as TurnStarted. A second echo while a turn is already open is a mid-turn
// steer landing, which has no event of its own in ADAPTERS.md §5 — Steer
// reports success through its error return, and inventing an event for it would
// be adding a tenth kind.
func (n *normalizer) turnAck(w *wireLine) {
	text := messageText(w.Message)

	if n.turnOpen {
		n.log.Debug("claudecode: steer acknowledged", "turn", n.turn)
		return
	}

	id, ok := n.popQueued(text)
	if !ok {
		if n.replay {
			// While replaying history, an echo we did not send is a past turn
			// being read back. Emitting TurnStarted for it would be honest but
			// useless; emitting TurnCompleted for its result is what would wake
			// someone at 3am, and Meta.Replay stops that.
			n.log.Debug("claudecode: replayed user message", "chars", len(text))
		}
		n.genTurn++
		id = fmt.Sprintf("cc-%d", n.genTurn)
	} else if n.replay {
		// Our own turn came back, so whatever history there was is behind us.
		n.replay = false
	}

	n.turn = id
	n.turnOpen = true
	n.out(event.TurnStarted{Meta: n.meta()})
}

// popQueued takes the next turn we sent. While replaying it insists on a text
// match, so a replayed history message cannot consume the id of a turn we are
// still waiting on; once a turn of ours has been acknowledged it is plain FIFO.
func (n *normalizer) popQueued(text string) (string, bool) {
	if len(n.queued) == 0 {
		return "", false
	}
	head := n.queued[0]
	if n.replay && head.text != text {
		return "", false
	}
	if head.text != text {
		n.log.Debug("claudecode: turn echo text differs from what was sent",
			"turn", head.id, "sent", len(head.text), "echoed", len(text))
	}
	n.queued = n.queued[1:]
	return head.id, true
}

func (n *normalizer) toolResults(w *wireLine) {
	if w.Message == nil {
		return
	}
	for _, c := range w.Message.Content {
		if c.Type != "tool_result" {
			continue
		}
		status := event.ToolCompleted
		if c.IsError != nil && *c.IsError {
			status = event.ToolFailed
		}
		n.out(event.ToolOutput{
			Meta:   n.meta(),
			ID:     c.ToolUseID,
			Chunk:  flattenText(c.Content),
			Status: status,
		})
	}
}

// assistant is emitted once per completed *content block*, not once per
// message: the two assistant events in turn 1 share one message.id and carry
// one block each, and message.stop_reason is null on all four. So it is never
// "the finished message" and must not be counted as one.
//
// It is used here for two things only: the assembled tool_use input, and text
// for the case where partial messages were not requested and no text_delta was
// ever seen for this message.
func (n *normalizer) assistant(w *wireLine) {
	if w.Message == nil {
		return
	}
	id := w.Message.ID
	for _, c := range w.Message.Content {
		switch c.Type {
		case "tool_use":
			n.emitToolStarted(c.ID, c.Name, decodeObject(c.Input))
		case "text":
			if c.Text != "" && !n.sawTextDelta[id] {
				n.out(event.TextDelta{Meta: n.meta(), Text: c.Text})
			}
		case "thinking":
			if c.Thinking != "" && !n.sawTextDelta[id] {
				n.out(event.Reasoning{Meta: n.meta(), Text: c.Thinking})
			}
		}
	}
}

// emitToolStarted is idempotent per tool_use id, because the same call arrives
// twice: assembled on the assistant event and again when its content block
// stops. Whichever lands first wins; in the fixture that is the assistant
// event, which is also the one that carries the parsed input.
func (n *normalizer) emitToolStarted(id, name string, input map[string]any) {
	if id == "" || n.emittedTools[id] {
		return
	}
	n.emittedTools[id] = true
	n.out(event.ToolStarted{
		Meta:     n.meta(),
		ID:       id,
		Tool:     name,
		Target:   toolTarget(name, input),
		RawInput: input,
	})
}

func (n *normalizer) streamEvent(w *wireLine) {
	e := w.Event
	if e == nil {
		n.unknown["stream:missing-event"]++
		return
	}
	switch e.Type {
	case "message_start":
		if e.Message != nil {
			n.msgID = e.Message.ID
			n.noteContext(e.Message.Usage, e.Message.Model)
		}

	case "content_block_start":
		if e.Index == nil || e.ContentBlock == nil {
			return
		}
		b := &blockState{kind: e.ContentBlock.Type}
		if b.kind == "tool_use" {
			b.toolID = e.ContentBlock.ID
			b.toolName = e.ContentBlock.Name
		}
		n.blocks[*e.Index] = b

	case "content_block_delta":
		n.delta(e)

	case "content_block_stop":
		if e.Index == nil {
			return
		}
		b := n.blocks[*e.Index]
		if b == nil {
			return
		}
		delete(n.blocks, *e.Index)
		if b.kind == "tool_use" {
			// The fragments concatenate to the JSON object. If they do not
			// parse we still report the call, with a nil input, rather than
			// dropping a tool run the user is about to see happen.
			n.emitToolStarted(b.toolID, b.toolName, decodeObject(json.RawMessage(b.json.String())))
		}

	case "message_delta":
		// The only place stop_reason is populated, and it carries the request's
		// own usage — which is the live context, unlike result.usage.
		if e.Usage != nil {
			n.noteContext(e.Usage, "")
		}

	case "message_stop":
		// Payload is exactly {"type":"message_stop"}. Nothing to do: the turn
		// boundary is the result event, not this.

	default:
		n.unknown["stream:"+e.Type]++
		n.log.Debug("claudecode: unknown stream_event type", "type", e.Type)
	}
}

func (n *normalizer) delta(e *wireStreamEvent) {
	if e.Delta == nil {
		return
	}
	switch e.Delta.Type {
	case "text_delta":
		n.sawTextDelta[n.msgID] = true
		n.out(event.TextDelta{Meta: n.meta(), Text: e.Delta.Text})

	case "input_json_delta":
		if e.Index == nil {
			return
		}
		if b := n.blocks[*e.Index]; b != nil {
			b.json.WriteString(e.Delta.PartialJSON)
		}

	case "thinking_delta":
		// Not present in the vendored fixture — a session with extended
		// thinking would produce it. We emit Reasoning only when one actually
		// arrives, which is observation, not invention.
		n.sawTextDelta[n.msgID] = true
		n.out(event.Reasoning{Meta: n.meta(), Text: e.Delta.Thinking})

	case "signature_delta":
		// A cryptographic signature over a thinking block. Not text, never
		// spoken, nothing to normalize it to.
		n.unknown["delta:signature_delta"]++

	default:
		n.unknown["delta:"+e.Delta.Type]++
		n.log.Debug("claudecode: unknown delta type", "type", e.Delta.Type)
	}
}

func (n *normalizer) noteContext(u *wireUsage, model string) {
	if u == nil {
		return
	}
	if c := u.context(); c > 0 {
		n.ctx.Used = c
	}
	if model != "" {
		n.ctx.Model = model
	}
}

func (n *normalizer) rateLimit(w *wireLine) {
	if w.RateLimitInfo == nil {
		return
	}
	info := *w.RateLimitInfo
	n.rate = &info
	if !info.Restricting() {
		// "allowed" is the ordinary state and is not news. The struct is kept
		// so the console can show quota, but nothing is emitted, because there
		// is no event in ADAPTERS.md §5 for "everything is fine".
		return
	}
	msg := fmt.Sprintf("rate limit %s (%s)", info.Status, info.RateLimitType)
	if info.ResetsAt > 0 {
		msg += ", resets " + time.Unix(info.ResetsAt, 0).UTC().Format(time.RFC3339)
	}
	// Not Retryable: Error.Retryable means the runtime will retry by itself
	// (Codex's willRetry). Claude Code does not, so this is a real failure the
	// user should hear about, with the reset time in the message.
	n.out(event.Error{Meta: n.meta(), Code: "rate_limit", Message: msg})
}

func (n *normalizer) result(w *wireLine) {
	info := ResultInfo{
		Subtype:           w.Subtype,
		Result:            w.Result,
		StopReason:        w.StopReason,
		TerminalReason:    w.TerminalReason,
		APIErrorStatus:    string(nonNull(w.APIErrorStatus)),
		NumTurns:          w.NumTurns,
		DurationMS:        w.DurationMS,
		DurationAPIMS:     w.DurationAPIMS,
		PermissionDenials: len(w.PermissionDenials),
		At:                n.now(),
	}
	if w.IsError != nil {
		info.IsError = *w.IsError
	}
	for p, dst := range map[*int64]*int64{
		w.TTFTMs: &info.TTFTMs, w.TTFTStreamMS: &info.TTFTStreamMs, w.TimeToRequestMS: &info.TimeToRequestMs,
	} {
		if p != nil {
			*dst = *p
		}
	}
	if w.Usage != nil {
		info.InputTokens = w.Usage.InputTokens
		info.OutputTokens = w.Usage.OutputTokens
		info.CacheReadTokens = w.Usage.CacheReadInputTokens
		info.CacheCreationTokens = w.Usage.CacheCreationInputTokens
	}

	// Per-turn cost is the delta between consecutive result events:
	// total_cost_usd and modelUsage are cumulative for the whole session, while
	// result.usage is per-turn. The numbers are free; the subtraction is ours.
	if w.TotalCostUSD != nil {
		info.TotalCostUSD = *w.TotalCostUSD
		info.HaveCost = true
		if n.haveTot {
			info.CostUSD = *w.TotalCostUSD - n.prevTot
		} else {
			info.CostUSD = *w.TotalCostUSD
		}
		n.prevTot = *w.TotalCostUSD
		n.haveTot = true
	}

	for _, mu := range w.ModelUsage {
		if mu.ContextWindow > n.ctx.Window {
			n.ctx.Window = mu.ContextWindow
		}
		if mu.CanonicalModel != "" {
			info.CanonicalModel = mu.CanonicalModel
		}
		if mu.Provider != "" {
			info.Provider = mu.Provider
		}
		if mu.ContextWindow > info.ContextWindow {
			info.ContextWindow = mu.ContextWindow
		}
		if mu.MaxOutputTokens > info.MaxOutputTokens {
			info.MaxOutputTokens = mu.MaxOutputTokens
		}
	}
	n.last = &info

	stop, ok := mapStopReason(w.Subtype, w.StopReason, info.IsError)

	if info.IsError || len(nonNull(w.APIErrorStatus)) > 0 {
		msg := strings.TrimSpace(w.Result)
		if msg == "" {
			msg = w.Subtype
		}
		if s := string(nonNull(w.APIErrorStatus)); s != "" {
			msg += " (api_error_status " + s + ")"
		}
		n.out(event.Error{Meta: n.meta(), Code: w.Subtype, Message: msg})
	}

	n.out(event.TurnCompleted{
		Meta:       n.meta(),
		OK:         ok,
		StopReason: stop,
		Duration:   time.Duration(w.DurationMS) * time.Millisecond,
		Usage:      n.turnUsage(w, &info),
	})

	n.turnOpen = false
	n.turn = ""
}

// turnUsage builds the metering object for a finished turn.
//
// ContextWindow is deliberately left nil. It *is* reported — modelUsage carries
// contextWindow: 1000000 — but result.usage sums the requests in the turn, and
// dividing a summed numerator by that denominator is exactly the eightfold
// overstatement ADAPTERS.md §2 warns about. The live context is on the session
// instead, as ContextState, which is the doc's own recipe: numerator from
// message_start, denominator from modelUsage.
func (n *normalizer) turnUsage(w *wireLine, info *ResultInfo) *event.Usage {
	if w.Usage == nil && !info.HaveCost {
		return nil
	}
	u := &event.Usage{}
	if info.HaveCost {
		u.CostUSD = event.F64(info.CostUSD)
	}
	if w.Usage != nil {
		u.InputTokens = event.I64(w.Usage.InputTokens + w.Usage.CacheCreationInputTokens)
		u.CachedInputTokens = event.I64(w.Usage.CacheReadInputTokens)
		u.OutputTokens = event.I64(w.Usage.OutputTokens)
		u.TotalTokens = event.I64(w.Usage.total())
		if d := w.Usage.OutputTokensDetails; d != nil {
			u.ReasoningOutputTokens = event.I64(d.ThinkingTokens)
		}
	}
	return u
}

// mapStopReason collapses result.subtype and result.stop_reason onto the five
// ACP stop reasons the event model uses as its superset.
func mapStopReason(subtype, stop string, isErr bool) (event.StopReason, bool) {
	switch stop {
	case "max_tokens":
		return event.StopMaxTokens, false
	case "refusal":
		return event.StopRefusal, false
	}
	switch subtype {
	case "success":
		if isErr {
			return event.StopError, false
		}
		return event.StopEndTurn, true
	case "error_max_turns":
		return event.StopMaxTurnRequests, false
	case "error_during_execution", "error":
		return event.StopError, false
	}
	if isErr {
		return event.StopError, false
	}
	// An unrecognised subtype that did not report an error is reported as an
	// error rather than as success: claiming a turn finished when we cannot
	// tell is the failure mode that matters.
	return event.StopError, false
}

// toolTarget picks the field of a tool's structured input that names what it
// acts on. Every entry is a verbatim field read, never a synthesis: an unknown
// tool gets an empty target and the orchestrator says what it knows, which is
// the tool's name.
func toolTarget(tool string, input map[string]any) string {
	if input == nil {
		return ""
	}
	var keys []string
	switch tool {
	case "Bash", "BashOutput":
		keys = []string{"command"}
	case "Read", "Write", "Edit", "NotebookEdit":
		keys = []string{"file_path", "notebook_path"}
	case "Glob", "Grep":
		keys = []string{"pattern"}
	case "WebFetch":
		keys = []string{"url"}
	case "WebSearch":
		keys = []string{"query"}
	case "Task", "Skill":
		keys = []string{"description", "skill"}
	default:
		return ""
	}
	for _, k := range keys {
		if s, ok := input[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func hasToolResult(m *wireMessage) bool {
	if m == nil {
		return false
	}
	for _, c := range m.Content {
		if c.Type == "tool_result" {
			return true
		}
	}
	return false
}

func messageText(m *wireMessage) string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range m.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// nonNull returns raw unless it is absent or the JSON literal null, so
// "api_error_status": null does not read as a failure.
func nonNull(raw json.RawMessage) json.RawMessage {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	return json.RawMessage(s)
}

func trimSpace(b []byte) []byte { return bytes.TrimSpace(b) }

// --- observation accessors, all snapshots under the lock ---

func (n *normalizer) initInfo() *InitInfo {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.init == nil {
		return nil
	}
	c := *n.init
	return &c
}

func (n *normalizer) rateLimitInfo() *RateLimitInfo {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.rate == nil {
		return nil
	}
	c := *n.rate
	return &c
}

func (n *normalizer) contextState() ContextState {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ctx
}

func (n *normalizer) lastResult() *ResultInfo {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.last == nil {
		return nil
	}
	c := *n.last
	return &c
}

func (n *normalizer) hookRuns() []HookRun {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]HookRun, 0, len(n.hookSeq))
	for _, id := range n.hookSeq {
		if h := n.hooks[id]; h != nil {
			out = append(out, *h)
		}
	}
	return out
}

// unseen returns the wire shapes this adapter met and did not normalize, with
// counts. It is the honest record of what a version bump added, and the thing
// to look at when a runtime starts saying something new.
func (n *normalizer) unseen() map[string]int {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make(map[string]int, len(n.unknown))
	for k, v := range n.unknown {
		out[k] = v
	}
	return out
}
