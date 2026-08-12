package acp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/event"
)

// toolState is what we know about one tool call so far.
//
// A tool_call_update may carry only a toolCallId with every other field null,
// so updates are merged onto the tool_call we already have rather than being
// treated as self-describing. `content` on an update *replaces* the collection,
// which is why the last rendering is kept: emitting the whole collection again
// on every update would repeat the entire output each time.
type toolState struct {
	title   string
	kind    string
	status  event.ToolStatus
	content string
}

// handleUpdate maps one session/update onto the normalized model. The eight
// variants are ADAPTERS.md §5's table; a ninth is counted and logged, never
// guessed at.
func (s *Session) handleUpdate(raw json.RawMessage) {
	var env updateEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		s.log.Warn("acp: session/update did not carry a sessionUpdate discriminant", "err", err)
		return
	}

	switch env.SessionUpdate {
	case updateAgentMessageChunk:
		s.chunk(raw, false)
	case updateAgentThoughtChunk:
		s.chunk(raw, true)
	case updateUserMessageChunk:
		s.userChunk(raw)
	case updateToolCall:
		s.toolCall(raw, false)
	case updateToolCallUpdate:
		s.toolCall(raw, true)
	case updatePlan:
		s.plan(raw)
	case updateAvailableCommands:
		s.availableCommands(raw)
	case updateCurrentMode:
		s.currentModeUpdate(raw)
	default:
		s.mu.Lock()
		s.unknownUpdates[env.SessionUpdate]++
		n := s.unknownUpdates[env.SessionUpdate]
		s.mu.Unlock()
		s.log.Warn("acp: session/update variant outside the documented eight — ADAPTERS.md §5 needs re-reading",
			"sessionUpdate", env.SessionUpdate, "count", n)
	}
}

// chunk handles agent_message_chunk → TextDelta and agent_thought_chunk →
// Reasoning. Reasoning is never spoken, on any runtime.
func (s *Session) chunk(raw json.RawMessage, thought bool) {
	var u chunkUpdate
	if err := json.Unmarshal(raw, &u); err != nil {
		s.log.Warn("acp: message chunk did not decode", "err", err, "thought", thought)
		return
	}
	text := blockText(u.Content)
	if text == "" {
		if u.Content.Type != "text" {
			s.mu.Lock()
			s.droppedContent++
			s.mu.Unlock()
			s.log.Debug("acp: non-text content in an agent message has no normalized event",
				"type", u.Content.Type, "thought", thought)
		}
		return
	}
	if thought {
		// agent_thought_chunk is protocol-native but whether each of the three
		// runtimes emits it was unverified. One arriving is an observation, so
		// the capability moves from unknown to yes — for this session only.
		s.observeReasoning()
		s.q.push(event.Reasoning{Meta: s.meta(""), Text: text})
		return
	}
	s.q.push(event.TextDelta{Meta: s.meta(""), Text: text})
}

func (s *Session) observeReasoning() {
	s.mu.Lock()
	already := s.caps.Get(adapter.CapReasoning) == adapter.SupportYes
	s.mu.Unlock()
	if already {
		return
	}
	s.narrow(adapter.CapReasoning, adapter.SupportYes,
		"observed: this session emitted agent_thought_chunk (ADAPTERS.md §8 item 4 asks for exactly this)")
}

// userChunk is replay or echo. There is no normalized event for user text — the
// nine are the agent's side of the conversation — so it is offered to the hook
// and otherwise dropped rather than dressed up as a TextDelta.
func (s *Session) userChunk(raw json.RawMessage) {
	if s.a.opts.OnUserMessage == nil {
		return
	}
	var u chunkUpdate
	if err := json.Unmarshal(raw, &u); err != nil {
		return
	}
	if text := blockText(u.Content); text != "" {
		s.a.opts.OnUserMessage(s.id, text, s.replaying.Load())
	}
}

func (s *Session) toolCall(raw json.RawMessage, isUpdate bool) {
	var tc toolCallUpdate
	if err := json.Unmarshal(raw, &tc); err != nil {
		s.log.Warn("acp: tool call did not decode", "err", err, "update", isUpdate)
		return
	}
	if tc.ToolCallID == "" {
		s.log.Warn("acp: tool call with no toolCallId", "update", isUpdate)
		return
	}

	s.mu.Lock()
	st, known := s.tools[tc.ToolCallID]
	if !known {
		st = &toolState{}
		s.tools[tc.ToolCallID] = st
	}
	if tc.Title != nil {
		st.title = *tc.Title
	}
	if tc.Kind != nil {
		st.kind = *tc.Kind
	}
	if tc.Status != nil {
		st.status = event.ToolStatus(*tc.Status)
	}
	title, kind, status := st.title, st.kind, st.status

	var chunk string
	if tc.Content != nil {
		rendered := renderToolContent(*tc.Content)
		chunk = suffixOf(st.content, rendered)
		st.content = rendered
	}
	s.mu.Unlock()

	if !isUpdate {
		if kind == "" {
			// ToolKind's documented default. Saying "other" is honest; leaving
			// it empty would make the console render a nameless tool.
			kind = "other"
		}
		s.q.push(event.ToolStarted{
			Meta:     s.meta(""),
			ID:       tc.ToolCallID,
			Tool:     kind,
			Target:   title,
			RawInput: rawObject(tc.RawInput),
		})
		if chunk == "" {
			return
		}
	} else if !known {
		s.log.Warn("acp: tool_call_update for a tool call we never saw start", "toolCallId", tc.ToolCallID)
	}

	if chunk == "" && tc.Status == nil {
		// An update that said nothing we can report. Emitting an empty
		// ToolOutput would be noise dressed as information.
		return
	}
	out := event.ToolOutput{Meta: s.meta(""), ID: tc.ToolCallID, Chunk: chunk}
	if tc.Status != nil {
		out.Status = status
	}
	s.q.push(out)
}

// renderToolContent turns ToolCallContent into the one string ToolOutput
// carries. The three shapes are a ContentBlock, a diff, or a terminal id; a
// diff and a terminal are named rather than expanded, because the useful part
// of both is which file and which command, not the bytes.
func renderToolContent(cs []toolCallContent) string {
	var b strings.Builder
	for _, c := range cs {
		var line string
		switch c.Type {
		case "content":
			if c.Content != nil {
				line = blockText(*c.Content)
			}
		case "diff":
			line = fmt.Sprintf("diff %s (+%d bytes)", c.Path, len(c.NewText))
		case "terminal":
			line = "terminal " + c.TerminalID
		}
		if line == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	return b.String()
}

// suffixOf is the delta between two renderings of a replace-the-collection
// field. When the new rendering extends the old, only the new part is emitted;
// when it does not, the whole thing is, because the agent replaced rather than
// appended. This is arithmetic on a documented replacement, not an inference
// about what the tool meant.
func suffixOf(prev, next string) string {
	if prev != "" && strings.HasPrefix(next, prev) {
		return next[len(prev):]
	}
	return next
}

// plan maps the `plan` variant onto PlanUpdated. It is native here, not
// synthesized: the agent states its intent before acting, which is what makes
// it the best narration material there is.
func (s *Session) plan(raw json.RawMessage) {
	var p planUpdate
	if err := json.Unmarshal(raw, &p); err != nil {
		s.log.Warn("acp: plan did not decode", "err", err)
		return
	}
	steps := make([]event.PlanStep, 0, len(p.Entries))
	for _, e := range p.Entries {
		steps = append(steps, event.PlanStep{Text: e.Content, Status: event.PlanStatus(e.Status)})
	}
	s.q.push(event.PlanUpdated{Meta: s.meta(""), Steps: steps, Synthesized: false})
}

// availableCommands is ACP's answer to SYSTEM.md §9's tool-list-refresh
// problem: the set is pushed when it changes. It has no home among the nine
// normalized events, so it is stored and handed to the hook rather than
// swallowed.
func (s *Session) availableCommands(raw json.RawMessage) {
	var u availableCommandsUpdate
	if err := json.Unmarshal(raw, &u); err != nil {
		s.log.Warn("acp: available_commands_update did not decode", "err", err)
		return
	}
	s.mu.Lock()
	s.commands = u.AvailableCommands
	s.mu.Unlock()
	s.log.Info("acp: available commands changed", "count", len(u.AvailableCommands))
	if s.a.opts.OnCommands != nil {
		s.a.opts.OnCommands(s.id, append([]AvailableCommand(nil), u.AvailableCommands...))
	}
}

// currentModeUpdate is the agent changing its own mode, which can change
// permission behaviour underneath a session the registry believes it
// understands. Surfaced, never swallowed.
func (s *Session) currentModeUpdate(raw json.RawMessage) {
	var u currentModeUpdate
	if err := json.Unmarshal(raw, &u); err != nil {
		s.log.Warn("acp: current_mode_update did not decode", "err", err)
		return
	}
	s.mu.Lock()
	prev := s.currentMode
	s.currentMode = u.CurrentModeID
	s.mu.Unlock()
	s.log.Info("acp: session mode changed", "from", prev, "to", u.CurrentModeID)
	if s.a.opts.OnModeChange != nil {
		s.a.opts.OnModeChange(s.id, u.CurrentModeID)
	}
}
