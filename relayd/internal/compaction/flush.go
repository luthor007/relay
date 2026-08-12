package compaction

import (
	"strconv"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
)

// NoReply is the convention OpenClaw's memoryFlush already uses: the agent
// answers with this and nothing else when it has nothing to add, and the user
// never sees the turn happened.
const NoReply = "NO_REPLY"

// FlushTurnPrefix marks a turn this package injected.
//
// It exists because a flush is a real turn on a real runtime, and every
// downstream consumer — pings (ADAPTERS.md §7), narration (ORCHESTRATOR.md §3b),
// the transcript, the cost meter — has to be able to tell it apart from
// something the user said. Marking it in the turn id is the only place all
// three protocols carry it for free: Claude Code takes our id directly, and
// [IsFlush] is the check.
const FlushTurnPrefix = "relay-flush:"

// FlushTurnID names a silent turn.
func FlushTurnID(session string, at time.Time) string {
	return FlushTurnPrefix + session + ":" + strconv.FormatInt(at.UnixMilli(), 10)
}

// IsFlush reports whether a turn id belongs to a silent memory pass. A caller
// that sees true must not speak it, must not ping about it, and must not count
// it as user activity — a flush that reset the idle timer would guarantee the
// idle compaction it exists to prepare for never happens.
func IsFlush(turnID string) bool { return strings.HasPrefix(turnID, FlushTurnPrefix) }

// FlushPrompt is the silent turn's instruction.
//
// Two things about its shape are deliberate. First, it asks for labelled lines
// rather than prose: SYSTEM.md's standing rule is that if you find yourself
// parsing prose you are on the wrong path, and a format this package chose,
// asked for and validates is not prose parsing. Second, it forbids tool use.
// The pass runs while nobody is listening, on a session about to be compacted;
// an agent that decided to run the test suite during it would turn a free
// operation into an expensive one nobody asked for.
func FlushPrompt() string {
	return strings.Join([]string{
		"System housekeeping, not a request from the user. Do not run any tools, do not change any files, and do not continue the work.",
		"",
		"This session is approaching its context limit and will shortly be compacted or replaced. Write down only what a fresh session would need in order to continue, using exactly these labels, one per line:",
		"",
		"WORK: one sentence saying what this session is doing.",
		"DECISION: one decision that was made and should not be revisited. Repeat the label for each.",
		"FILE: one path that is central to the work. Repeat the label for each.",
		"NEXT: one sentence saying the immediate next step.",
		"",
		"No preamble, no explanation, no other lines. Do not include secrets, tokens or keys.",
		"If nothing here has changed since the last time you were asked, reply with exactly " + NoReply + ".",
	}, "\n")
}

// FlushTurn is the whole silent turn, ready to send.
func FlushTurn(session string, at time.Time) adapter.Turn {
	return adapter.Turn{ID: FlushTurnID(session, at), Text: FlushPrompt()}
}

// Notes are what a flush came back with.
type Notes struct {
	Work      string
	Decisions []string
	Files     []string
	Next      string

	// Ignored counts lines that carried no label. It is reported rather than
	// silently dropped: a flush whose every line was ignored means the agent did
	// not follow the format, which is worth seeing before the material it was
	// supposed to produce turns up missing in a brief.
	Ignored int
	// Redactions is how many credentials the detector replaced, in case an agent
	// answered "DECISION: we are using sk-... as the test key" despite being
	// asked not to.
	Redactions int
}

// Empty reports whether a flush produced nothing usable.
func (n Notes) Empty() bool {
	return n.Work == "" && n.Next == "" && len(n.Decisions) == 0 && len(n.Files) == 0
}

// ReadFlush parses a flush reply. ok is false for NO_REPLY, for an empty reply,
// and for one that carried no labelled line at all.
//
// Redaction happens here rather than at the caller, because this is where text
// from a model first enters the process, and MEMORY.md §6's ordering — detect
// before anything stores, embeds or forwards it — is only enforceable at the
// entry point.
func ReadFlush(reply string, r Redactor) (Notes, bool) {
	var n Notes
	if r == nil || strings.TrimSpace(reply) == "" {
		return n, false
	}

	redacted, found := r.Redact(reply)
	n.Redactions = len(found)

	for _, raw := range strings.Split(redacted, "\n") {
		line := strings.TrimSpace(raw)
		line = strings.TrimLeft(line, "-*• \t")
		if line == "" {
			continue
		}
		if strings.EqualFold(line, NoReply) {
			continue
		}
		label, rest, ok := strings.Cut(line, ":")
		if !ok {
			n.Ignored++
			continue
		}
		rest = oneLine(rest)
		if rest == "" {
			n.Ignored++
			continue
		}
		switch strings.ToUpper(strings.TrimSpace(label)) {
		case "WORK":
			if n.Work == "" {
				n.Work = rest
			}
		case "DECISION":
			n.Decisions = append(n.Decisions, rest)
		case "FILE":
			n.Files = append(n.Files, rest)
		case "NEXT":
			n.Next = rest
		default:
			n.Ignored++
		}
	}
	if n.Empty() {
		return n, false
	}
	return n, true
}

// Merge folds flush notes into brief material, preferring what the flush said
// about the work in progress over an older stored summary — the flush was
// answered by the session itself, moments ago, about what it is doing now.
//
// It appends rather than replaces for decisions and files, because the stored
// side of those came from the index and the flush side came from the agent, and
// neither is a superset of the other. [BriefBuilder.Build] dedupes.
func (n Notes) Merge(in BriefInput) BriefInput {
	if n.Work != "" {
		in.Recent = append(in.Recent, n.Work)
		if in.Summary == "" {
			in.Summary = n.Work
		}
	}
	if n.Next != "" {
		in.Next = n.Next
	}
	in.Decisions = append(n.Decisions, in.Decisions...)
	in.Files = append(in.Files, n.Files...)
	return in
}
