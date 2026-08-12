package routing

import (
	"strings"

	"github.com/luthor007/relay/relayd/internal/summarize"
)

// AnnounceCap is the character budget for a routing announcement.
//
// It is ADAPTERS.md §6's *progress* budget rather than its ~40-character
// acknowledgement budget, and that is a deliberate departure with a reason:
// MEMORY.md §8's own worked example — "starting a new Codex session on the API
// repo" — is 44 characters, so the ack cap cannot hold the sentence the design
// asks us to say. Clipping it would remove "on the api repo", which is exactly
// the clause that lets a human catch a wrong guess, and an announcement that
// drops its object is worse than one that runs a second longer.
//
// ADAPTERS.md §6 carries this as its own row now; if that table changes, this
// constant changes with it.
const AnnounceCap = summarize.CapProgress

// Announce builds the one clause spoken *before* acting.
//
// This is ORCHESTRATOR.md §4's rule 2 and it is the cheapest of the three
// guardrails: a wrong guess that was announced is a correction, and a wrong
// guess that was silent is a discovery, usually much later, usually in the
// wrong session's context. SYSTEM.md §7b notes the same line doubles as the
// acknowledgement that fills the silence while the big model thinks.
//
// It is deterministic. No model phrases this, ever — the announcement is the
// mechanism by which the user audits the router, and a paraphrased audit trail
// is not one.
func Announce(d Decision) string {
	line := announceLine(d)
	out, _ := summarize.Fit(line, AnnounceCap)
	return out
}

func announceLine(d Decision) string {
	switch d.Kind {
	case KindContinue:
		return continueLine(d)
	case KindNew:
		return newLine(d)
	case KindAsk:
		return d.Question
	case KindControl:
		return controlLine(d)
	}
	return ""
}

func continueLine(d Decision) string {
	name := strings.TrimSpace(d.Subject)
	if name == "" {
		name = "that session"
	}
	switch d.Reason {
	case ReasonExplicit:
		return "Switching to " + name + "."
	case ReasonOnlyLive:
		return "Adding that to " + name + ", the only one running."
	default:
		return "Adding that to " + name + "."
	}
}

func newLine(d Decision) string {
	var b strings.Builder
	b.WriteString("Starting a new ")
	if d.Runtime != "" {
		b.WriteString(RuntimeLabel(d.Runtime))
		b.WriteString(" ")
	}
	b.WriteString("session")
	switch {
	case strings.TrimSpace(d.Subject) != "":
		b.WriteString(" for ")
		b.WriteString(strings.TrimSpace(d.Subject))
	case baseName(d.Workspace) != "":
		b.WriteString(" on ")
		b.WriteString(baseName(d.Workspace))
	}
	b.WriteString(".")
	return b.String()
}

func controlLine(d Decision) string {
	if d.Command == nil {
		return ""
	}
	name := strings.TrimSpace(d.Subject)
	switch d.Command.Kind {
	case CmdStop:
		if name != "" {
			return "Stopping " + name + "."
		}
		return "Stopping."
	case CmdUndo:
		return "Taking that back."
	case CmdStatus:
		if name != "" {
			return "Checking " + name + "."
		}
		return "Checking."
	case CmdList:
		return "Here is what is running."
	case CmdAnswer:
		if name != "" {
			return "Answering " + name + "."
		}
		return "Answering."
	}
	return ""
}

// AskLine builds the question for an ambiguous route.
//
// It names the candidates rather than asking an open question, because "which
// session?" spoken into a pair of glasses is answered by silence. Two names is
// the useful maximum out loud; the rest are on the phone.
func AskLine(cs []Candidate) string {
	switch len(cs) {
	case 0:
		return "I do not have a session for that. Start a new one?"
	case 1:
		return "Put that in " + cs[0].Session.Name() + "?"
	default:
		return "Which one — " + cs[0].Session.Name() + ", or " + cs[1].Session.Name() + "?"
	}
}

// UnknownRefLine is what the router says when a spoken session name matches
// nothing. It repeats the name back, because the common cause is a
// misrecognition and hearing it back is how the user finds that out.
func UnknownRefLine(ref string, cs []Candidate) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return AskLine(cs)
	}
	if len(cs) == 0 {
		return "I have no session called " + ref + ". Start one?"
	}
	return "I have no session called " + ref + ". " + AskLine(cs)
}
