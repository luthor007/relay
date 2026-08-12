package llm

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Tool is one callable the model may request.
//
// ORCHESTRATOR.md §3b gives the big model "routing judgement, tools, sessions,
// memory writes", so this file is the half of the two-model split that was
// missing: [Request] used to carry no tools at all and its own comment said
// neither model needed them, which is the contradiction BUILD-PROMPT.md's
// two-model row names. §3b wins — the comment was wrong.
//
// Three fields exist for rules the orchestrator has to enforce rather than for
// anything the wire needs:
//
//   - Description is required to say *when* to call the tool, not only what it
//     does. Recent Opus models reach for tools conservatively, and a trigger
//     clause is what moves the should-call rate. It is also where §3b's
//     escalation allowlist actually lives: "call this for status questions" is
//     a tool description, not a prompt paragraph.
//   - ParallelSafe is the distinction a harness cannot recover later. A
//     read-only search and a session start look identical once they are both
//     "a tool call", so the loop must be told which ones may overlap; anything
//     unmarked is serialised, because guessing wrong here starts two agent
//     sessions instead of one.
//   - MaxResultBytes is a context-window safety rail. One `grep` over a large
//     repository can spend the whole window in a single result, and the model
//     never gets to decide it would rather not have read it.
type Tool struct {
	// Name is the wire name. Lowercase with underscores, specific rather than
	// general: search_memory beats memory.
	Name string

	// Description tells the model what the tool does *and when to reach for
	// it*. See the type comment: the trigger clause is the load-bearing half.
	Description string

	// Schema is the JSON Schema for the input, as a map so a caller can build
	// one without a struct-tag round trip. It must be an object schema.
	Schema map[string]any

	// Strict asks the provider to guarantee the input validates against
	// Schema. Both wires support it and both reject the same constructs —
	// no recursion, no numeric or length bounds — so a schema that uses them
	// must leave this off and validate in the handler.
	Strict bool

	// ParallelSafe marks a tool with no side effects that may run concurrently
	// with other parallel-safe calls in the same batch. Default false: the
	// loop serialises anything it has not been told is safe.
	ParallelSafe bool

	// MaxResultBytes caps one result from this tool. Zero means
	// [DefaultMaxResultBytes].
	MaxResultBytes int
}

// DefaultMaxResultBytes caps a single tool result before it reaches the model.
//
// Claude Code caps tool responses at 25,000 tokens; at roughly four bytes per
// token that is the figure below. The cap is a truncation rather than an error
// because a truncated result the model can act on beats a failure it cannot,
// and [Truncate] keeps both ends so the tail — where the error usually is —
// survives.
const DefaultMaxResultBytes = 100 << 10

// ToolCall is one request from the model to run a tool.
type ToolCall struct {
	// ID correlates the call with its result. Both wires require the result to
	// carry it back and reject a batch with any call unanswered.
	ID    string
	Name  string
	Input json.RawMessage
}

// Arg reads one string field out of the raw input without a struct.
//
// It exists for the narration path: an event needs a human-readable target —
// a path, a query, a session name — and building a typed struct per tool just
// to fill in one label is more ceremony than the label is worth.
func (c ToolCall) Arg(name string) string {
	if len(c.Input) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(c.Input, &m); err != nil {
		return ""
	}
	switch v := m[name].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// ToolResult is what the tool returned, on its way back to the model.
type ToolResult struct {
	CallID  string
	Content string

	// IsError tells the model the call failed so it can adapt rather than
	// treat an error string as data. A denied call is an error result and not
	// a dropped one: both wires reject a turn with an unanswered tool_use.
	IsError bool
}

// ToolChoiceMode is how hard the provider pushes the model toward a tool.
type ToolChoiceMode string

const (
	// ChoiceAuto lets the model decide. The default, and right for the big
	// model.
	ChoiceAuto ToolChoiceMode = "auto"
	// ChoiceAny requires at least one tool call. This is what makes §3b's
	// under-escalation failure impossible to reach by accident: the small
	// model does not get to answer in prose when the question was "who handles
	// this", it has to name a tool.
	ChoiceAny ToolChoiceMode = "any"
	// ChoiceNone forbids tools for this call — the narration turn.
	ChoiceNone ToolChoiceMode = "none"
	// ChoiceTool names one required tool.
	ChoiceTool ToolChoiceMode = "tool"
)

// ToolChoice constrains tool use for one request.
type ToolChoice struct {
	Mode ToolChoiceMode
	// Name is required when Mode is [ChoiceTool].
	Name string
	// DisableParallel forces at most one tool call per response. Useful when a
	// batch would be ambiguous to narrate, and the honest default is off.
	DisableParallel bool
}

// Truncate applies the tool's result cap, keeping the head and the tail.
//
// Dropping the tail would be the wrong half: a compiler invocation puts its
// summary at the end, a stack trace puts the cause at the top and the site at
// the bottom, and a truncation notice in the middle is something the model can
// reason about. The marker says how much went missing so it can ask for a
// narrower call rather than guess.
func (t Tool) Truncate(s string) (string, bool) {
	max := t.MaxResultBytes
	if max <= 0 {
		max = DefaultMaxResultBytes
	}
	if len(s) <= max {
		return s, false
	}

	// Leave room for the marker itself, then split what is left between the
	// two ends. A cap too small to hold a marker is a caller error, not a
	// reason to return something that lies about being complete.
	const markerBudget = 96
	if max <= markerBudget {
		return fmt.Sprintf("[relay: result of %d bytes dropped; the cap is %d]", len(s), max), true
	}
	keep := max - markerBudget
	head := keep * 2 / 3
	tail := keep - head

	marker := fmt.Sprintf("\n\n[relay: %d bytes omitted here — narrow the call to see them]\n\n",
		len(s)-keep)
	return trimToRune(s[:head]) + marker + trimToRuneStart(s[len(s)-tail:]), true
}

// trimToRune and trimToRuneStart keep a byte-sliced string valid UTF-8. A
// mid-rune cut produces a replacement character in the model's context, which
// is a small thing that looks exactly like data corruption.
func trimToRune(s string) string {
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func trimToRuneStart(s string) string {
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[1:]
	}
	return s
}

// ValidateTools checks a tool set for the mistakes that are mechanically
// detectable.
//
// It deliberately does not try to detect the failure mode Anthropic's own tool
// guidance calls the most common one — a bloated set with ambiguous decision
// points — because that judgement is "could a human engineer say which tool
// applies here", and a linter that pretends to answer it would give false
// confidence. What it can catch is the set being self-contradictory: two tools
// with the same name, or two with the same description, which is that failure
// mode in its one machine-readable form.
func ValidateTools(tools []Tool) error {
	byName := make(map[string]bool, len(tools))
	byDesc := make(map[string]string, len(tools))
	for _, t := range tools {
		switch {
		case t.Name == "":
			return fmt.Errorf("llm: a tool has no name")
		case byName[t.Name]:
			return fmt.Errorf("llm: two tools are named %q", t.Name)
		case strings.TrimSpace(t.Description) == "":
			return fmt.Errorf("llm: tool %q has no description; the model chooses on it", t.Name)
		}
		if kind, _ := t.Schema["type"].(string); kind != "object" {
			return fmt.Errorf("llm: tool %q needs an object schema, got %q", t.Name, kind)
		}
		key := strings.ToLower(strings.Join(strings.Fields(t.Description), " "))
		if other, ok := byDesc[key]; ok {
			return fmt.Errorf("llm: tools %q and %q have the same description, so nothing distinguishes them",
				other, t.Name)
		}
		byName[t.Name] = true
		byDesc[key] = t.Name
	}
	return nil
}
