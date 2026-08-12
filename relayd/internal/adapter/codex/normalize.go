package codex

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/luthor007/relay/relayd/internal/event"
)

// metaSource stamps every event with ADAPTERS.md §5's envelope. Seq is
// per-session monotonic and assigned here, which is safe because every event is
// constructed on the connection's single reader goroutine and therefore in wire
// order.
type metaSource struct {
	runtime string
	session string
	now     func() time.Time
	seq     atomic.Uint64
	replay  atomic.Bool
}

func (m *metaSource) meta(turn string) event.Meta {
	return event.Meta{
		Runtime: m.runtime,
		Session: m.session,
		Turn:    turn,
		At:      m.now(),
		Seq:     m.seq.Add(1),
		Replay:  m.replay.Load(),
	}
}

// hooks are the things a normalizer observes that are not events: turn
// boundaries the caller is waiting on, and the two notifications that narrow a
// session's capabilities out from under it.
type hooks struct {
	onThreadStarted   func(thread)
	onTurnStarted     func(turnID string)
	onTurnCompleted   func(turnID string)
	onSettings        func(threadSettings)
	onAutoApproval    func(reviewID string)
	onStatus          func(threadStatus)
	onRequestResolved func(requestID json.RawMessage)
	onThreadGone      func(method string)
	onCompaction      func()
}

// normalizer maps Codex app-server notifications onto the nine normalized
// events. It owns the per-item bookkeeping the mapping needs and knows nothing
// about transports, so the app-server session and the `codex exec --json`
// fallback share exactly one implementation of the mapping table.
type normalizer struct {
	emit func(event.Event)
	src  *metaSource
	log  *slog.Logger
	h    hooks

	mu sync.Mutex
	// streamedText and streamedReasoning record which items already produced
	// deltas. item/completed is authoritative over deltas, so re-emitting the
	// completed text would speak the whole answer a second time; but a runtime
	// that never streamed still has to produce its text once.
	streamedText      map[string]bool
	streamedReasoning map[string]bool
	streamedOutput    map[string]bool
	// summaryPart tracks item/reasoning/summaryPartAdded so two separate
	// thoughts are not concatenated into one sentence.
	summaryPart map[string]int64
	// toolLabel remembers what a ToolStarted called an item, so a later
	// ToolOutput can be correlated without re-deriving it.
	toolLabel map[string]string

	lastUsage   *event.Usage
	turnUsage   map[string]*event.Usage
	compactions int
}

func newNormalizer(emit func(event.Event), src *metaSource, log *slog.Logger, h hooks) *normalizer {
	if log == nil {
		log = slog.Default()
	}
	return &normalizer{
		emit:              emit,
		src:               src,
		log:               log,
		h:                 h,
		streamedText:      map[string]bool{},
		streamedReasoning: map[string]bool{},
		streamedOutput:    map[string]bool{},
		summaryPart:       map[string]int64{},
		toolLabel:         map[string]string{},
		turnUsage:         map[string]*event.Usage{},
	}
}

// handle dispatches one notification. Unknown methods are logged rather than
// dropped: that log line is how we find out Codex shipped something new, which
// is the whole reason the schemas are vendored.
func (n *normalizer) handle(method string, params json.RawMessage) {
	switch method {

	// ---- thread lifecycle ----
	case "thread/started":
		var p threadStartedNote
		if !n.decode(method, params, &p) {
			return
		}
		if n.h.onThreadStarted != nil {
			n.h.onThreadStarted(p.Thread)
		}
	case "thread/settings/updated":
		var p settingsUpdatedNote
		if !n.decode(method, params, &p) {
			return
		}
		if n.h.onSettings != nil {
			n.h.onSettings(p.ThreadSettings)
		}
	case "thread/status/changed":
		var p statusChangedNote
		if !n.decode(method, params, &p) {
			return
		}
		if n.h.onStatus != nil {
			n.h.onStatus(p.Status)
		}
	case "thread/closed", "thread/deleted":
		if n.h.onThreadGone != nil {
			n.h.onThreadGone(method)
		}
	case "thread/name/updated":
		n.log.Debug("codex: thread renamed", "method", method)
	case "serverRequest/resolved":
		var p serverRequestResolvedNote
		if !n.decode(method, params, &p) {
			return
		}
		if n.h.onRequestResolved != nil {
			n.h.onRequestResolved(p.RequestID)
		}

	// ---- turn boundary ----
	case "turn/started":
		var p turnBoundaryNote
		if !n.decode(method, params, &p) {
			return
		}
		if n.h.onTurnStarted != nil {
			n.h.onTurnStarted(p.Turn.ID)
		}
		n.emit(event.TurnStarted{Meta: n.src.meta(p.Turn.ID)})
	case "turn/completed":
		var p turnBoundaryNote
		if !n.decode(method, params, &p) {
			return
		}
		n.emitTurnCompleted(p.Turn)
		if n.h.onTurnCompleted != nil {
			n.h.onTurnCompleted(p.Turn.ID)
		}

	// ---- streaming deltas ----
	case "item/agentMessage/delta":
		var p deltaNote
		if !n.decode(method, params, &p) {
			return
		}
		n.mark(n.streamedText, p.ItemID)
		n.emit(event.TextDelta{Meta: n.src.meta(p.TurnID), Text: p.Delta})
	case "item/reasoning/textDelta":
		var p deltaNote
		if !n.decode(method, params, &p) {
			return
		}
		n.mark(n.streamedReasoning, p.ItemID)
		n.emit(event.Reasoning{Meta: n.src.meta(p.TurnID), Text: p.Delta, Summary: false})
	case "item/reasoning/summaryTextDelta":
		var p deltaNote
		if !n.decode(method, params, &p) {
			return
		}
		n.mark(n.streamedReasoning, p.ItemID)
		n.emit(event.Reasoning{Meta: n.src.meta(p.TurnID), Text: p.Delta, Summary: true})
	case "item/reasoning/summaryPartAdded":
		var p deltaNote
		if !n.decode(method, params, &p) {
			return
		}
		n.summaryBoundary(p)
	case "item/commandExecution/outputDelta":
		var p deltaNote
		if !n.decode(method, params, &p) {
			return
		}
		n.mark(n.streamedOutput, p.ItemID)
		n.emit(event.ToolOutput{Meta: n.src.meta(p.TurnID), ID: p.ItemID, Chunk: p.Delta})
	case "item/fileChange/patchUpdated":
		var p patchUpdatedNote
		if !n.decode(method, params, &p) {
			return
		}
		n.mark(n.streamedOutput, p.ItemID)
		n.emit(event.ToolOutput{Meta: n.src.meta(p.TurnID), ID: p.ItemID, Chunk: renderChanges(p.Changes)})
	case "item/mcpToolCall/progress":
		var p mcpProgressNote
		if !n.decode(method, params, &p) {
			return
		}
		n.mark(n.streamedOutput, p.ItemID)
		n.emit(event.ToolOutput{Meta: n.src.meta(p.TurnID), ID: p.ItemID, Chunk: p.Message})

	// ---- item boundary ----
	case "item/started":
		var p itemBoundaryNote
		if !n.decode(method, params, &p) {
			return
		}
		n.itemStarted(p)
	case "item/completed":
		var p itemBoundaryNote
		if !n.decode(method, params, &p) {
			return
		}
		n.itemCompleted(p)

	// ---- plan, the best narration source in the system ----
	case "turn/plan/updated":
		var p planUpdatedNote
		if !n.decode(method, params, &p) {
			return
		}
		steps := make([]event.PlanStep, 0, len(p.Plan))
		for _, s := range p.Plan {
			steps = append(steps, event.PlanStep{Text: s.Step, Status: planStatus(s.Status)})
		}
		e := event.PlanUpdated{Meta: n.src.meta(p.TurnID), Steps: steps}
		if p.Explanation != nil {
			e.Explanation = *p.Explanation
		}
		n.emit(e)

	// ---- metering ----
	case "thread/tokenUsage/updated":
		var p tokenUsageNote
		if !n.decode(method, params, &p) {
			return
		}
		n.recordUsage(p)

	// ---- failure ----
	case "error":
		var p errorNote
		if !n.decode(method, params, &p) {
			return
		}
		n.emit(event.Error{
			Meta:      n.src.meta(p.TurnID),
			Code:      p.Error.code(),
			Message:   p.Error.Message,
			Retryable: p.WillRetry,
		})

	// ---- the approvals trap, visible ----
	case "item/autoApprovalReview/started", "item/autoApprovalReview/completed":
		var p struct {
			ReviewID string `json:"reviewId"`
		}
		_ = json.Unmarshal(params, &p)
		if n.h.onAutoApproval != nil {
			n.h.onAutoApproval(p.ReviewID)
		}

	// ---- notices: logged, never pinged ----
	case "deprecationNotice":
		n.log.Warn("codex: deprecation notice from app-server — re-vendor the schemas", "params", string(params))
	case "configWarning":
		n.log.Warn("codex: problem in the user's config.toml", "params", string(params))
	case "warning", "guardianWarning":
		n.log.Warn("codex: warning", "method", method, "params", string(params))

	default:
		n.log.Info("codex: unhandled notification", "method", method)
	}
}

func (n *normalizer) decode(method string, params json.RawMessage, dst any) bool {
	if err := json.Unmarshal(params, dst); err != nil {
		n.log.Error("codex: undecodable notification params", "method", method, "err", err)
		return false
	}
	return true
}

func (n *normalizer) mark(m map[string]bool, id string) {
	if id == "" {
		return
	}
	n.mu.Lock()
	m[id] = true
	n.mu.Unlock()
}

func (n *normalizer) marked(m map[string]bool, id string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return m[id]
}

// summaryBoundary handles item/reasoning/summaryPartAdded.
//
// The notification is a real observation — a new summary part opened — but the
// sealed event model has no field for it, and gluing two parts together turns
// two thoughts into one sentence. So a boundary *between* parts becomes a
// paragraph break in the Reasoning stream, and only ever between parts: the
// first part of an item produces nothing. Reasoning is never spoken, so this
// costs nothing at the TTS layer and buys the summariser a correct paragraph
// structure.
func (n *normalizer) summaryBoundary(p deltaNote) {
	n.mu.Lock()
	prev, seen := n.summaryPart[p.ItemID]
	n.summaryPart[p.ItemID] = p.SummaryIndex
	streamed := n.streamedReasoning[p.ItemID]
	n.mu.Unlock()

	if !seen || !streamed || p.SummaryIndex == prev {
		return
	}
	n.emit(event.Reasoning{Meta: n.src.meta(p.TurnID), Text: "\n\n", Summary: true})
}

func (n *normalizer) itemStarted(p itemBoundaryNote) {
	it := p.Item
	label, target, ok := toolIdentity(it)
	if !ok {
		// reasoning, agentMessage, userMessage and the rest have no tool
		// identity; their content arrives as deltas or on item/completed.
		n.log.Debug("codex: item started", "type", it.Type, "item", it.ID)
		return
	}
	n.mu.Lock()
	n.toolLabel[it.ID] = label
	n.mu.Unlock()

	n.emit(event.ToolStarted{
		Meta:     n.src.meta(p.TurnID),
		ID:       it.ID,
		Tool:     label,
		Target:   target,
		RawInput: rawInput(it),
	})
}

func (n *normalizer) itemCompleted(p itemBoundaryNote) {
	it := p.Item
	switch it.Type {
	case "agentMessage":
		// The completed item is authoritative over the deltas — but only one of
		// the two may reach the TTS layer, or the answer is spoken twice.
		if n.marked(n.streamedText, it.ID) || it.Text == "" {
			return
		}
		n.emit(event.TextDelta{Meta: n.src.meta(p.TurnID), Text: it.Text})

	case "reasoning":
		if n.marked(n.streamedReasoning, it.ID) {
			return
		}
		if s := strings.Join(it.Summary, "\n\n"); s != "" {
			n.emit(event.Reasoning{Meta: n.src.meta(p.TurnID), Text: s, Summary: true})
		}
		if c := strings.Join(it.Content, "\n\n"); c != "" {
			n.emit(event.Reasoning{Meta: n.src.meta(p.TurnID), Text: c, Summary: false})
		}

	case "contextCompaction":
		// The live compaction signal; the thread/compacted notification is
		// deprecated. Not an event — MEMORY.md §9 reads it as state.
		n.mu.Lock()
		n.compactions++
		n.mu.Unlock()
		if n.h.onCompaction != nil {
			n.h.onCompaction()
		}
		n.log.Info("codex: thread context compacted", "item", it.ID)

	case "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall", "webSearch":
		out := event.ToolOutput{
			Meta:   n.src.meta(p.TurnID),
			ID:     it.ID,
			Status: toolStatus(it.Status),
		}
		// Only carry the aggregate when nothing was streamed, for the same
		// reason as agentMessage: it is the same bytes a second time.
		if !n.marked(n.streamedOutput, it.ID) {
			out.Chunk = completedChunk(it)
		}
		n.emit(out)

	default:
		n.log.Debug("codex: item completed", "type", it.Type, "item", it.ID)
	}
}

// toolIdentity names the tool an item represents. The five item types that are
// tool calls get a stable label; everything else returns false rather than
// being forced into a ToolStarted it is not.
func toolIdentity(it threadItem) (label, target string, ok bool) {
	switch it.Type {
	case "commandExecution":
		return "command", it.Command, true
	case "fileChange":
		return "file_change", changePaths(it.Changes), true
	case "mcpToolCall":
		l := it.Tool
		if it.Server != "" {
			l = it.Server + "/" + it.Tool
		}
		return "mcp:" + l, it.Tool, true
	case "dynamicToolCall":
		return "dynamic:" + it.Tool, it.Tool, true
	case "webSearch":
		return "web_search", it.Query, true
	}
	return "", "", false
}

// rawInput is what the runtime said about the call, never an inference from it.
func rawInput(it threadItem) map[string]any {
	m := map[string]any{}
	switch it.Type {
	case "commandExecution":
		m["command"] = it.Command
		if it.Cwd != "" {
			m["cwd"] = it.Cwd
		}
		if it.Source != "" {
			m["source"] = it.Source
		}
	case "fileChange":
		paths := make([]any, 0, len(it.Changes))
		for _, c := range it.Changes {
			paths = append(paths, map[string]any{"path": c.Path, "kind": c.kindName()})
		}
		m["changes"] = paths
	case "mcpToolCall":
		m["server"] = it.Server
		m["tool"] = it.Tool
		if len(it.Arguments) > 0 {
			var v any
			if err := json.Unmarshal(it.Arguments, &v); err == nil {
				m["arguments"] = v
			}
		}
	case "dynamicToolCall":
		m["tool"] = it.Tool
	case "webSearch":
		m["query"] = it.Query
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

func completedChunk(it threadItem) string {
	switch it.Type {
	case "commandExecution":
		if it.AggregatedOutput != nil {
			return *it.AggregatedOutput
		}
	case "fileChange":
		return renderChanges(it.Changes)
	case "mcpToolCall":
		if len(it.Error) > 0 && string(it.Error) != "null" {
			return string(it.Error)
		}
	}
	return ""
}

func changePaths(changes []fileUpdateChange) string {
	switch len(changes) {
	case 0:
		return ""
	case 1:
		return changes[0].Path
	}
	return fmt.Sprintf("%s and %d more", changes[0].Path, len(changes)-1)
}

func renderChanges(changes []fileUpdateChange) string {
	var b strings.Builder
	for i, c := range changes {
		if i > 0 {
			b.WriteString("\n")
		}
		if k := c.kindName(); k != "" {
			b.WriteString(k)
			b.WriteString(" ")
		}
		b.WriteString(c.Path)
		if c.Diff != "" {
			b.WriteString("\n")
			b.WriteString(c.Diff)
		}
	}
	return b.String()
}

func planStatus(s string) event.PlanStatus {
	switch s {
	case "inProgress":
		return event.PlanInProgress
	case "completed":
		return event.PlanCompleted
	default:
		return event.PlanPending
	}
}

// toolStatus maps the three separate Codex status enums onto ACP's, which the
// event model uses as the superset. `declined` is a Codex-only value and it is
// a failure to complete, not a completion.
func toolStatus(s string) event.ToolStatus {
	switch s {
	case "inProgress":
		return event.ToolInProgress
	case "completed":
		return event.ToolCompleted
	case "failed", "declined":
		return event.ToolFailed
	case "":
		return event.ToolUnknown
	}
	return event.ToolUnknown
}

func (n *normalizer) recordUsage(p tokenUsageNote) {
	u := &event.Usage{
		// CostUSD stays nil, always. There is no dollar figure anywhere in the
		// Codex contract; USD has to be computed upstream from a price table.
		InputTokens:           event.I64(p.TokenUsage.Last.InputTokens),
		CachedInputTokens:     event.I64(p.TokenUsage.Last.CachedInputTokens),
		OutputTokens:          event.I64(p.TokenUsage.Last.OutputTokens),
		ReasoningOutputTokens: event.I64(p.TokenUsage.Last.ReasoningOutputTokens),
		TotalTokens:           event.I64(p.TokenUsage.Last.TotalTokens),
	}
	// Context pressure is a property of the whole thread, not of the last
	// request, so the denominator's numerator has to be the running total.
	pressure := &event.Usage{
		InputTokens:       event.I64(p.TokenUsage.Total.InputTokens),
		CachedInputTokens: event.I64(p.TokenUsage.Total.CachedInputTokens),
		OutputTokens:      event.I64(p.TokenUsage.Total.OutputTokens),
		TotalTokens:       event.I64(p.TokenUsage.Total.TotalTokens),
	}
	if p.TokenUsage.ModelContextWindow != nil {
		u.ContextWindow = event.I64(*p.TokenUsage.ModelContextWindow)
		pressure.ContextWindow = event.I64(*p.TokenUsage.ModelContextWindow)
	}

	n.mu.Lock()
	n.turnUsage[p.TurnID] = u
	n.lastUsage = pressure
	n.mu.Unlock()
}

// usage returns the running thread total, for MEMORY.md §9's idle compaction
// check. nil means Codex has not reported any yet — never a zero.
func (n *normalizer) usage() *event.Usage {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.lastUsage
}

func (n *normalizer) compactionCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.compactions
}

func (n *normalizer) emitTurnCompleted(t turnPayload) {
	n.mu.Lock()
	u := n.turnUsage[t.ID]
	delete(n.turnUsage, t.ID)
	n.mu.Unlock()

	stop := event.StopEndTurn
	switch t.Status {
	case "completed":
		stop = event.StopEndTurn
	case "interrupted":
		stop = event.StopCancelled
	case "failed":
		stop = event.StopError
		// contextWindowExceeded is a distinct code rather than prose, which is
		// what lets MEMORY.md §9 recognise the terminal case without matching
		// strings. It is retryable after a compaction, so it maps to
		// StopMaxTokens rather than to the flat error.
		if t.Error.code() == "contextWindowExceeded" {
			stop = event.StopMaxTokens
		}
	default:
		// `inProgress` on a turn/completed is a contract violation; say error
		// rather than inventing a clean ending.
		stop = event.StopError
		n.log.Warn("codex: turn/completed with a non-terminal status", "turn", t.ID, "status", t.Status)
	}

	e := event.TurnCompleted{
		Meta:       n.src.meta(t.ID),
		OK:         stop.OK(),
		StopReason: stop,
		Usage:      u,
	}
	if t.DurationMs != nil {
		e.Duration = time.Duration(*t.DurationMs) * time.Millisecond
	}
	n.emit(e)
}
