package compaction

import (
	"errors"
	"sort"
	"strings"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/index"
)

// Redactor replaces credentials with markers.
//
// It is internal/index's detector behind a one-method interface, not a second
// ruleset: MEMORY.md §12.2's measured recall belongs to that one, and a brief is
// text this process writes and then *sends into an agent session*, which is an
// outbound path. Detection happens before the text exists, never after, and
// making the detector a constructor argument is how that stops being a
// convention.
type Redactor interface {
	Redact(text string) (string, []index.Finding)
}

// Detector returns the measured detector.
func Detector() Redactor { return index.MustDetector() }

var (
	// ErrNoRedactor refuses to build briefs without a detector.
	ErrNoRedactor = errors.New("compaction: no secret detector, and writing a brief without one is not allowed")

	// ErrNothingToCarry means there was no observed material for a brief.
	//
	// This is the honest failure and it has to exist. A brief assembled out of
	// nothing would be a confident summary of a session nobody looked at, handed
	// to a fresh agent as though it were fact. A caller that gets this degrades
	// to compacting — which at least summarises something real — and says so.
	ErrNothingToCarry = errors.New("compaction: nothing observed to carry into a new session")
)

// Brief is the handoff: what the work is, what was decided, which files, which
// facts apply.
//
// It is the outcome only the orchestrator can produce. A runtime compacting
// summarises its own transcript with no idea what mattered; we have the index
// (MEMORY.md §3) and the facts (§5), so this is usually smaller and better
// targeted than the runtime's own compaction — and [Brief.Chars] against
// [BriefOptions.Budget] is that claim as an enforced number rather than an
// assertion.
type Brief struct {
	// Session is the session being left behind, so the fresh one can say where
	// it came from and a human can go and read the original.
	Session   string
	Runtime   adapter.Runtime
	Workspace string

	Work      string
	Decisions []string
	Files     []string
	Facts     []string
	Next      string

	// Truncated is set when material was dropped to fit the budget.
	Truncated bool
	// Dropped is how many items the budget cost.
	Dropped int
	// Redactions is how many credentials the detector replaced on the way in.
	// Non-zero is not a warning; it is the detector doing its job on text that
	// was about to be posted to a model.
	Redactions int
}

// Empty reports whether there is nothing worth sending.
func (b Brief) Empty() bool {
	return strings.TrimSpace(b.Work) == "" &&
		len(b.Decisions) == 0 && len(b.Files) == 0 && len(b.Facts) == 0 && b.Next == ""
}

// Text renders the brief. It is a fixed layout with labelled sections, built by
// this package and never by a model: a handoff that could hallucinate its own
// premise would be worse than the compaction it replaced.
//
// Sections with no material are omitted rather than emitted empty. There is no
// "Decisions: none" line, because a fresh agent reading one would reasonably
// conclude that nothing was decided, which is a claim nobody made.
func (b Brief) Text() string {
	var sb strings.Builder
	sb.WriteString("You are picking up work from an earlier session that ran out of room.")
	if b.Session != "" {
		sb.WriteString(" It was ")
		sb.WriteString(b.Session)
		if b.Runtime != "" {
			sb.WriteString(" on ")
			sb.WriteString(string(b.Runtime))
		}
		sb.WriteString(".")
	}
	sb.WriteString("\n")

	if b.Workspace != "" {
		sb.WriteString("\nWorking directory: ")
		sb.WriteString(b.Workspace)
		sb.WriteString("\n")
	}
	if w := strings.TrimSpace(b.Work); w != "" {
		sb.WriteString("\nThe work: ")
		sb.WriteString(w)
		sb.WriteString("\n")
	}
	writeList(&sb, "Decided so far:", b.Decisions)
	writeList(&sb, "Files in play:", b.Files)
	writeList(&sb, "Standing facts:", b.Facts)
	if n := strings.TrimSpace(b.Next); n != "" {
		sb.WriteString("\nNext: ")
		sb.WriteString(n)
		sb.WriteString("\n")
	}
	if b.Truncated {
		sb.WriteString("\n(Shortened to fit; the full session is still on disk.)\n")
	}
	return sb.String()
}

func writeList(sb *strings.Builder, header string, items []string) {
	if len(items) == 0 {
		return
	}
	sb.WriteString("\n")
	sb.WriteString(header)
	sb.WriteString("\n")
	for _, it := range items {
		sb.WriteString("- ")
		sb.WriteString(it)
		sb.WriteString("\n")
	}
}

// Chars is the rendered length in runes, which is what the budget is in.
func (b Brief) Chars() int { return len([]rune(b.Text())) }

// Turn is the brief as the first thing said to a fresh session.
func (b Brief) Turn() adapter.Turn {
	return adapter.Turn{Text: b.Text()}
}

// BriefOptions configures a builder.
type BriefOptions struct {
	// Redactor is required. There is no default, and a nil one is an error
	// rather than a fallback to no detection.
	Redactor Redactor

	// Budget is the rendered brief's ceiling in characters. The default is
	// deliberately small: the point of a brief is that it is shorter and better
	// aimed than a runtime's own compaction, and a brief that grew to the size
	// of a compaction would have quietly become one.
	Budget int

	MaxDecisions int
	MaxFiles     int
	MaxFacts     int
	// MaxLine clips one bullet. A 900-character "decision" is a paragraph of
	// transcript that escaped its summariser.
	MaxLine int
}

// Defaults for [BriefOptions].
const (
	DefaultBriefBudget = 2000
	DefaultMaxLine     = 200
)

func (o BriefOptions) withDefaults() BriefOptions {
	if o.Budget <= 0 {
		o.Budget = DefaultBriefBudget
	}
	if o.MaxDecisions <= 0 {
		o.MaxDecisions = 6
	}
	if o.MaxFiles <= 0 {
		o.MaxFiles = 8
	}
	if o.MaxFacts <= 0 {
		o.MaxFacts = 6
	}
	if o.MaxLine <= 0 {
		o.MaxLine = DefaultMaxLine
	}
	return o
}

// BriefBuilder assembles briefs. It holds no state between calls.
type BriefBuilder struct{ o BriefOptions }

// NewBriefBuilder refuses to exist without a detector.
func NewBriefBuilder(o BriefOptions) (*BriefBuilder, error) {
	if o.Redactor == nil {
		return nil, ErrNoRedactor
	}
	return &BriefBuilder{o: o.withDefaults()}, nil
}

// BriefInput is the observed material. Every field is something that already
// exists somewhere: Summary and Recent from internal/summarize's rows, Files
// from the tool_call table, Facts from the facts tier, Decisions from a
// [FlushTurn] reply or from the summariser.
type BriefInput struct {
	Session   string
	Runtime   adapter.Runtime
	Workspace string

	// Summary is the session summary. Recent are recent turn summaries, oldest
	// first.
	Summary string
	Recent  []string

	Decisions []string
	Files     []string
	Facts     []string
	Next      string
}

// Build assembles a brief, redacting everything on the way in.
func (b *BriefBuilder) Build(in BriefInput) (Brief, error) {
	out := Brief{
		Session:   in.Session,
		Runtime:   in.Runtime,
		Workspace: in.Workspace,
	}

	var n int
	clean := func(s string) string {
		red, found := b.o.Redactor.Redact(s)
		n += len(found)
		return clip(oneLine(red), b.o.MaxLine)
	}
	cleanAll := func(in []string, max int) []string {
		var seen = map[string]bool{}
		var out []string
		for _, s := range in {
			v := clean(s)
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
			if len(out) >= max {
				break
			}
		}
		return out
	}

	// The workspace is a path and paths are text; a token in a directory name
	// is unusual and is exactly the kind of thing that gets missed by only
	// scanning the parts that look like prose.
	out.Workspace = clean(in.Workspace)

	out.Work = clean(in.Summary)
	if out.Work == "" {
		// Fall back to the most recent turn summary. Newest is the useful one:
		// the session summary describes the whole thing, and the last turn
		// describes what it was actually doing when it ran out of room.
		for i := len(in.Recent) - 1; i >= 0; i-- {
			if v := clean(in.Recent[i]); v != "" {
				out.Work = v
				break
			}
		}
	}
	out.Next = clean(in.Next)
	out.Decisions = cleanAll(in.Decisions, b.o.MaxDecisions)
	out.Files = cleanAll(dedupeFiles(in.Files), b.o.MaxFiles)
	out.Facts = cleanAll(in.Facts, b.o.MaxFacts)
	out.Redactions = n

	if out.Work == "" && len(out.Decisions) == 0 && out.Next == "" {
		// Files and facts alone are not a brief. "Here are eight paths, good
		// luck" tells a fresh agent nothing about what it is meant to do with
		// them, and would read as a description of work that nobody wrote down.
		return Brief{}, ErrNothingToCarry
	}

	out.fit(b.o.Budget)
	return out, nil
}

// fit drops material until the rendered brief is inside the budget, lowest
// value first: facts, then files, then decisions. The work statement and the
// next step are never dropped — a brief without them is not a shorter brief,
// it is a different and useless artefact.
func (b *Brief) fit(budget int) {
	for b.Chars() > budget {
		switch {
		case len(b.Facts) > 0:
			b.Facts = b.Facts[:len(b.Facts)-1]
		case len(b.Files) > 0:
			b.Files = b.Files[:len(b.Files)-1]
		case len(b.Decisions) > 1:
			b.Decisions = b.Decisions[:len(b.Decisions)-1]
		default:
			// Everything droppable is gone and it is still over. Say so rather
			// than mutilating the one sentence that says what the work is.
			b.Truncated = true
			return
		}
		b.Dropped++
		b.Truncated = true
	}
}

// dedupeFiles keeps the most recently mentioned occurrence of each path and
// puts the shortest paths first, which in practice is the difference between a
// list of the repo's real subjects and a list of whatever the last tool call
// happened to touch.
func dedupeFiles(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for i := len(in) - 1; i >= 0; i-- {
		p := strings.TrimSpace(in[i])
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) < len(out[j]) })
	return out
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return strings.TrimRight(string(r[:n-1]), " ") + "…"
}
