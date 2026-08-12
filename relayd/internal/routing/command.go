package routing

import (
	"regexp"
	"strings"

	"github.com/luthor007/relay/relayd/internal/adapter"
)

// CommandKind is one of the closed set of things a user can say *about* the
// sessions rather than *to* one of them.
//
// The set is closed on purpose. ORCHESTRATOR.md §4's escape hatch is only worth
// anything if it is always correct, and a grammar that guesses is not an escape
// hatch — it is the router again, wearing a hat. Everything here is matched
// against anchored phrases; nothing is inferred.
type CommandKind string

const (
	// CmdNone is not a command. It is the common case and it is the reason
	// [ParseCommand] returns a kind rather than an error.
	CmdNone CommandKind = ""
	// CmdNewSession is "new session", with optional runtime, subject and a
	// trailing instruction: "new codex session on the api repo — run the tests".
	CmdNewSession CommandKind = "new_session"
	// CmdSwitch is "talk to the refactor one", "switch to payments".
	CmdSwitch CommandKind = "switch"
	// CmdUndo is "undo that" — move the last turn somewhere else.
	CmdUndo CommandKind = "undo"
	// CmdStop cancels the running turn.
	CmdStop CommandKind = "stop"
	// CmdStatus is "what is it doing", "is it done", "how long".
	CmdStatus CommandKind = "status"
	// CmdList is "what's running".
	CmdList CommandKind = "list"
	// CmdAnswer is a reply to a blocked session: "yes", "go ahead", "deny".
	// ADAPTERS.md §7 requires needs-input to be answerable by voice or the
	// feature is decorative, and this is the half of that which is routing's.
	CmdAnswer CommandKind = "answer"
)

// Command is a parsed control utterance.
type Command struct {
	Kind CommandKind

	// Ref is the session the user named, in their words: "refactor" from "talk
	// to the refactor one". It is resolved against the live list by the router,
	// never here.
	Ref string

	// Runtime is set when the user named one: "new codex session".
	Runtime adapter.Runtime
	// Subject is what a new session is for: "the api repo".
	Subject string
	// Rest is anything said after the command, which is the turn to send:
	// "new session, run the tests" leaves "run the tests".
	Rest string

	// Approve is set for CmdAnswer: true for yes, false for no.
	Approve bool

	// Phrase is what actually matched. It is in the announcement and in the
	// test table, so a change to the grammar shows up as a diff rather than as
	// a behaviour drift.
	Phrase string
}

// Answered reports whether this command is a yes/no to a blocked session.
func (c Command) Answered() bool { return c.Kind == CmdAnswer }

// pattern is one row of the grammar. Exactly one of exact, prefix or re is set.
//
// This is data with tests rather than prose in a prompt, which is the same rule
// ORCHESTRATOR.md §3b applies to the escalation allowlist and for the same
// reason: a grammar you can print is a grammar you can argue with.
type pattern struct {
	kind CommandKind
	// exact matches the whole normalized utterance.
	exact []string
	// prefix matches the start; what follows goes to the field named by take.
	prefix []string
	// re is a compiled pattern whose first group is the payload.
	re *regexp.Regexp
	// take says what the captured tail is.
	take takeKind
	// approve is CmdAnswer's polarity.
	approve bool
}

type takeKind uint8

const (
	takeNothing takeKind = iota
	// takeRest is a trailing instruction: "new session, run the tests".
	takeRest
	// takeRef is a session reference: "talk to the refactor one".
	takeRef
	// takeSubject is what a new session is about: "new session on the api repo".
	takeSubject
	// takeRuntimeSubject is the regexp form, group 1 runtime, group 2 subject.
	takeRuntimeSubject
)

// separators split a command from the instruction that follows it, on the
// *raw* text before normalisation — normalising first would eat the comma that
// carries the split. A spoken "new session, run the tests" arrives from ASR
// with a comma or with nothing; both work, and the whole string is the
// fallback.
//
// The instruction keeps its original casing and punctuation, because it is what
// gets sent to an agent: "run the tests --verbose" must not arrive as "run the
// tests verbose".
var separators = []string{",", ";", " — ", " – ", " -- ", " - ", " then ", " and then "}

var grammar = []pattern{
	// ---- new session. The most important row in the package: it is the one
	// that is always correct, and the one users reach for first.
	{kind: CmdNewSession, exact: []string{
		"new session", "start a new session", "open a new session",
		"start a new one", "new one", "fresh session", "start over",
		"start fresh", "begin a new session",
	}},
	{kind: CmdNewSession, prefix: []string{
		"new session on ", "new session for ", "new session about ",
		"start a new session on ", "start a new session for ",
		"start a new session about ", "open a new session on ",
	}, take: takeSubject},
	{kind: CmdNewSession, prefix: []string{
		"new session ", "start a new session ",
	}, take: takeRest},
	// "new codex session", "start a new claude code session on the api repo"
	{kind: CmdNewSession, re: regexp.MustCompile(
		`^(?:start |open |begin )?(?:a )?new (claude code|claude|codex|openclaw|open claw|hermes|opencode|open code) session(?: (?:on|for|about) (.+))?$`,
	), take: takeRuntimeSubject},

	// ---- undo. Before switch, because "put that in the api docs" is a request
	// to move the turn that already went somewhere else, not a request to
	// change the subject going forward. Getting that backwards leaves the
	// mis-routed turn where it landed, which is the failure undo exists for.
	{kind: CmdUndo, exact: []string{
		"undo", "undo that", "undo it", "no undo that", "wrong session",
		"that was the wrong session", "wrong one", "not that one",
		"that went to the wrong session",
	}},
	{kind: CmdUndo, prefix: []string{
		"undo that and ", "undo and ", "no put that in the ", "no put that in ",
		"put that in the ", "put that in ", "move that to the ", "move that to ",
		"that belongs in the ", "that belongs in ",
	}, take: takeRef},

	// ---- switch. The other half of the escape hatch.
	{kind: CmdSwitch, prefix: []string{
		"talk to the ", "talk to ", "switch to the ", "switch to ",
		"go back to the ", "go back to ", "back to the ", "back to ",
	}, take: takeRef},
	{kind: CmdSwitch, re: regexp.MustCompile(
		`^in the (.+) session$`,
	), take: takeRef},
	{kind: CmdSwitch, re: regexp.MustCompile(
		`^(?:tell|ask) (?:the )?(.+?) (?:one|session) (?:to|that) (.+)$`,
	), take: takeRef},

	// ---- stop.
	{kind: CmdStop, exact: []string{
		"stop", "stop it", "stop that", "cancel", "cancel that", "cancel it",
		"never mind", "nevermind", "abort", "quit that",
	}},

	// ---- status. The small model answers these alone (ORCHESTRATOR.md §3b),
	// and they are also routing commands because "what is it doing" has to pick
	// a session before it can answer.
	{kind: CmdStatus, exact: []string{
		"status", "what is it doing", "whats it doing", "what s it doing",
		"what is it up to", "is it done", "is it finished", "are you done",
		"how long", "how long left", "how much longer", "how is it going",
		"hows it going", "how s it going", "any progress", "whats happening",
		"what is happening",
	}},

	// ---- list.
	{kind: CmdList, exact: []string{
		"what is running", "whats running", "what s running", "list sessions",
		"what sessions are there", "what are you working on", "show me the sessions",
		"what have you got running", "session list",
	}},

	// ---- answer. Short, closed, and never inferred from anything longer.
	{kind: CmdAnswer, approve: true, exact: []string{
		"yes", "yeah", "yep", "yes please", "go ahead", "do it", "approve",
		"approved", "allow", "allow it", "sure", "ok", "okay", "confirm",
		"confirmed",
	}},
	{kind: CmdAnswer, approve: false, exact: []string{
		"no", "nope", "no thanks", "deny", "denied", "dont", "do not",
		"reject", "decline", "no dont", "dont do that", "do not do that",
	}},
}

// ParseCommand matches an utterance against the grammar.
//
// It returns CmdNone for anything it does not recognise, which is most
// utterances. There is deliberately no fuzzy matching and no model: the escape
// hatch's whole value is that it is always correct, and a probabilistic escape
// hatch from a probabilistic router is not an escape from anything.
func ParseCommand(text string) Command {
	head, rest := splitInstruction(text)
	norm := normalize(head)
	if norm == "" {
		return Command{}
	}

	for _, p := range grammar {
		for _, ex := range p.exact {
			if norm == ex {
				return Command{Kind: p.kind, Phrase: ex, Approve: p.approve, Rest: rest}
			}
		}
	}

	for _, p := range grammar {
		for _, pre := range p.prefix {
			if !strings.HasPrefix(norm, pre) {
				continue
			}
			tail := strings.TrimSpace(strings.TrimPrefix(norm, pre))
			if tail == "" {
				continue
			}
			c := Command{Kind: p.kind, Phrase: strings.TrimSpace(pre), Approve: p.approve, Rest: rest}
			assign(&c, p.take, tail, rawTail(head, words(pre)))
			return c
		}
	}

	for _, p := range grammar {
		if p.re == nil {
			continue
		}
		m := p.re.FindStringSubmatch(norm)
		if m == nil {
			continue
		}
		c := Command{Kind: p.kind, Phrase: p.re.String(), Approve: p.approve, Rest: rest}
		switch p.take {
		case takeRuntimeSubject:
			c.Runtime = ParseRuntime(m[1])
			if len(m) > 2 {
				c.Subject = cleanRef(m[2])
			}
		case takeRef:
			c.Ref = cleanRef(m[1])
			if len(m) > 2 && m[2] != "" && c.Rest == "" {
				c.Rest = strings.TrimSpace(m[2])
			}
		default:
			assign(&c, p.take, m[1], "")
		}
		return c
	}

	return Command{}
}

// assign puts a captured tail into the field the pattern asked for. raw is the
// same tail taken from the original text, which is what an instruction has to
// be: normalised text has lost the punctuation an agent needs.
func assign(c *Command, take takeKind, tail, raw string) {
	switch take {
	case takeRest:
		// "new session run the tests" — everything after the verb is the turn,
		// and the subject is left empty rather than guessed from it. A session
		// named after its first instruction is a session named wrong.
		if c.Rest == "" {
			if raw != "" {
				c.Rest = raw
			} else {
				c.Rest = tail
			}
		}
	case takeRef:
		c.Ref = cleanRef(tail)
	case takeSubject:
		c.Subject = cleanRef(tail)
		if rt := ParseRuntime(c.Subject); rt != "" {
			// "new session on codex" names a runtime, not a topic.
			c.Runtime = rt
			c.Subject = ""
		}
	}
}

// splitInstruction separates a command from an instruction said in the same
// breath, on the raw text.
func splitInstruction(text string) (head, rest string) {
	lower := strings.ToLower(text)
	lowest := -1
	var sep string
	for _, s := range separators {
		if i := strings.Index(lower, s); i >= 0 && (lowest < 0 || i < lowest) {
			lowest, sep = i, s
		}
	}
	if lowest < 0 {
		return strings.TrimSpace(text), ""
	}
	return strings.TrimSpace(text[:lowest]), strings.TrimSpace(text[lowest+len(sep):])
}

// words counts the words in a matched prefix.
func words(s string) int { return len(strings.Fields(s)) }

// rawTail drops the first n words of the original text, so what follows a
// command keeps the casing and punctuation it was said with.
func rawTail(text string, n int) string {
	f := strings.Fields(text)
	if n >= len(f) {
		return ""
	}
	// Find the start of word n in the original string rather than re-joining,
	// which would collapse the spacing inside the instruction.
	var seen, i int
	inWord := false
	for i = 0; i < len(text); i++ {
		sp := text[i] == ' ' || text[i] == '\t' || text[i] == '\n'
		switch {
		case !sp && !inWord:
			inWord = true
			if seen == n {
				return strings.TrimSpace(text[i:])
			}
		case sp && inWord:
			inWord = false
			seen++
		}
	}
	return ""
}

// cleanRef strips the filler words a spoken session reference collects: "the
// refactor one", "payments session".
func cleanRef(s string) string {
	fields := strings.Fields(normalize(s))
	var out []string
	for _, f := range fields {
		if f == "one" || f == "session" || f == "the" {
			continue
		}
		out = append(out, f)
	}
	return strings.Join(out, " ")
}

// ParseRuntime maps a spoken runtime name onto one of the five. It returns ""
// for anything else — an unrecognised runtime name is not a routing hint, it is
// part of the sentence.
func ParseRuntime(s string) adapter.Runtime {
	switch normalize(s) {
	case "claude code", "claudecode", "claude":
		return adapter.ClaudeCode
	case "codex":
		return adapter.Codex
	case "openclaw", "open claw":
		return adapter.OpenClaw
	case "hermes":
		return adapter.Hermes
	case "opencode", "open code":
		return adapter.OpenCode
	}
	return ""
}

// RuntimeLabel is how a runtime is said out loud.
func RuntimeLabel(r adapter.Runtime) string {
	switch r {
	case adapter.ClaudeCode:
		return "Claude Code"
	case adapter.Codex:
		return "Codex"
	case adapter.OpenClaw:
		return "OpenClaw"
	case adapter.Hermes:
		return "Hermes"
	case adapter.OpenCode:
		return "OpenCode"
	}
	return string(r)
}
