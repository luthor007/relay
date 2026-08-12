package routing

import "strings"

// Class is what the small model is allowed to do with an utterance.
//
// ORCHESTRATOR.md §3b: the small model hears every utterance first and may
// answer a small, explicit set alone. Everything else escalates to the big
// model. The two failure modes are not symmetric — escalating unnecessarily
// costs a few cents, self-answering wrongly costs trust — so this is an
// allowlist with a default of escalate, and it starts almost empty.
type Class string

const (
	// ClassStatus is "what is it doing", "is it done", "how long". Answerable
	// from the registry with no reasoning on top.
	ClassStatus Class = "status"
	// ClassControl is "stop", "new session", "talk to the refactor one",
	// "undo that". The closed grammar in command.go, and the reason that
	// grammar is closed.
	ClassControl Class = "control"
	// ClassMemory is a read from the shared store with no reasoning on top.
	ClassMemory Class = "memory"
	// ClassEscalate is everything else, which is most things.
	ClassEscalate Class = "escalate"
)

// Verdict is one classification.
type Verdict struct {
	Class    Class
	Escalate bool
	// Rule names the allowlist row that matched, empty when none did.
	Rule string
	// Because is why, in one clause.
	Because string
	// Veto is the word that forced an escalation over a matching allowlist row.
	Veto string
	// Ack is always true. ORCHESTRATOR.md §3b lists acknowledgement as its own
	// allowlist row — "the immediate 'on it', always" — and it is not a
	// classification of the utterance but a property of every utterance: the
	// small model speaks first, whatever else happens next.
	Ack bool
}

// Allow is one row of the allowlist.
//
// Data, not prose in a prompt. A prompt that says "you may answer status
// questions" is a prompt whose behaviour changes when the model does; a table
// with tests is a table that fails CI when someone widens it.
type Allow struct {
	Class Class
	// Name is the row, for the verdict and for the test table.
	Name string
	// Exact matches the whole normalized utterance.
	Exact []string
	// Prefix matches the start of it. Prefixes are the dangerous half of this
	// file — every one of them is a way for a longer sentence to slip through —
	// so they are anchored and the veto list is applied on top of them.
	Prefix []string
}

// Allowlist is the whole of what the small model may answer alone.
//
// It is deliberately short. Growing it is a product decision with a real cost,
// and the row to justify is always "why is under-escalating this safe?" rather
// than "why not".
var Allowlist = []Allow{
	{
		Class: ClassStatus, Name: "what is it doing",
		Exact: []string{
			"what is it doing", "whats it doing", "what s it doing",
			"what is it up to", "what are they doing", "how is it going",
			"hows it going", "how s it going", "any progress",
			"whats happening", "what is happening", "status",
		},
		Prefix: []string{"what is it doing ", "whats it doing "},
	},
	{
		Class: ClassStatus, Name: "is it done",
		Exact: []string{
			"is it done", "is it finished", "are you done", "is that done",
			"did it finish", "is it still going", "is it done yet",
			"anything finished",
		},
	},
	{
		Class: ClassStatus, Name: "how long",
		Exact: []string{
			"how long", "how long left", "how much longer", "how long has it been",
			"how long will it take",
		},
		// The prefix is where the veto earns its place: "how long has the
		// refactor been running" matches this row and escalates anyway.
		Prefix: []string{"how long ", "how much longer "},
	},
	{
		Class: ClassStatus, Name: "what is running",
		Exact: []string{
			"what is running", "whats running", "what s running",
			"list sessions", "session list", "what sessions are there",
			"what have you got running", "show me the sessions",
			"what are you working on",
		},
	},
	{
		Class: ClassControl, Name: "control verb",
		// The control rows are not repeated here. ParseCommand is the closed
		// grammar and Classify consults it directly, so there is exactly one
		// definition of "stop" in this package and widening the grammar cannot
		// silently widen the allowlist behind it.
	},
	{
		Class: ClassMemory, Name: "memory lookup",
		Prefix: []string{
			"what did i decide ", "what did we decide ", "what did i say ",
			"what did we say ", "remind me what ", "what was the name of ",
			"what did i call ", "do i have a note ", "what do you know about ",
			"when did i last ", "what did i do ",
		},
	},
}

// Veto is what forces an escalation even when an allowlist row matched.
//
// ORCHESTRATOR.md §3b: "anything touching a repo, a tool, or a decision
// escalates." These are those things as words. The list errs wide on purpose —
// a false escalation costs a few cents and a false self-answer costs trust, so
// "how long has the refactor been running" going up to the big model is the
// correct trade even though the status row matched it.
//
// It is applied to status and memory rows. It is *not* applied to control,
// because control is a closed anchored grammar where "stop" cannot become
// "stop and deploy" — a longer sentence simply does not match.
var Veto = []string{
	// repos and code
	"repo", "repository", "branch", "commit", "merge", "rebase", "pull",
	"push", "diff", "patch", "revert", "checkout", "clone", "pr",
	"refactor", "rewrite", "implement", "fix", "debug", "migrate",
	"file", "files", "function", "class", "module", "package", "test",
	"tests", "build", "compile", "lint",
	// tools and the world outside the machine
	"run", "execute", "deploy", "ship", "release", "install", "uninstall",
	"delete", "remove", "email", "mail", "send", "print", "buy", "order",
	"pay", "book", "call", "post", "publish", "upload", "download",
	"database", "migration", "server", "docker", "kubernetes", "terraform",
	"key", "token", "secret", "credential", "password",
	// decisions
	"should", "decide", "choose", "pick", "recommend", "compare", "plan",
	"design", "why", "explain", "how do i", "opinion",
}

// Classify decides whether the small model may answer alone.
//
// The order is: control grammar first (closed, anchored, never vetoed), then
// the allowlist rows with the veto applied on top, then escalate. Escalate is
// the default and every path that is not an explicit match ends there.
func Classify(text string) Verdict {
	v := Verdict{Ack: true, Class: ClassEscalate, Escalate: true,
		Because: "anything touching a repo, a tool, or a decision escalates"}

	norm := normalize(text)
	if norm == "" {
		v.Because = "nothing was said"
		return v
	}

	if cmd := ParseCommand(text); cmd.Kind != CmdNone {
		switch cmd.Kind {
		case CmdNewSession, CmdSwitch, CmdUndo, CmdStop, CmdAnswer, CmdList:
			return Verdict{Class: ClassControl, Rule: string(cmd.Kind), Ack: true,
				Because: "a control verb, from the closed command grammar"}
		case CmdStatus:
			return Verdict{Class: ClassStatus, Rule: string(cmd.Kind), Ack: true,
				Because: "a status question, answerable from the registry"}
		}
	}

	for _, row := range Allowlist {
		rest, ok := matches(row, norm)
		if !ok {
			continue
		}
		// The veto is applied to what is left after the row's own trigger
		// words, not to the whole utterance. "What did I decide" *is* the
		// memory row — vetoing it on its own verb would make the row
		// unreachable — while "what did I decide about the payments repo" still
		// escalates, because the part that is not the trigger names a repo.
		if w, ok := vetoed(rest); ok {
			v.Rule = row.Name
			v.Veto = w
			v.Because = "\"" + w + "\" touches a repo, a tool or a decision"
			return v
		}
		return Verdict{Class: row.Class, Rule: row.Name, Ack: true,
			Because: becauseFor(row.Class)}
	}
	return v
}

func becauseFor(c Class) string {
	switch c {
	case ClassStatus:
		return "a status question, answerable from the registry"
	case ClassMemory:
		return "a read from the shared store with no reasoning on top"
	case ClassControl:
		return "a control verb, from the closed command grammar"
	}
	return "escalated"
}

// matches reports whether a row fires, and what is left of the utterance once
// the row's own trigger words are removed. An exact match leaves nothing, which
// is why exact rows are the safe half of this file.
func matches(row Allow, norm string) (rest string, ok bool) {
	for _, e := range row.Exact {
		if norm == e {
			return "", true
		}
	}
	for _, p := range row.Prefix {
		if strings.HasPrefix(norm, p) {
			return strings.TrimSpace(strings.TrimPrefix(norm, p)), true
		}
	}
	return "", false
}

// vetoed reports the first veto word in the utterance.
func vetoed(norm string) (string, bool) {
	fields := strings.Fields(norm)
	set := make(map[string]bool, len(fields))
	for _, f := range fields {
		set[f] = true
	}
	for _, w := range Veto {
		if strings.ContainsRune(w, ' ') {
			if strings.Contains(norm, w) {
				return w, true
			}
			continue
		}
		if set[w] {
			return w, true
		}
	}
	return "", false
}

// Escalates is the one-line form for a caller that only needs the boolean.
func Escalates(text string) bool { return Classify(text).Escalate }
