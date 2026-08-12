package summarize

import (
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/event"
)

// Limits on what a digest keeps. They are small on purpose: this is narration
// material, not a record. The transcript stays on disk and the index holds a
// pointer to it.
const (
	// MaxDigestText is how much assistant prose a digest carries. The outcome
	// is usually in the first or last sentence; the middle is where the diffs
	// are.
	MaxDigestText = 2000
	// MaxDigestErrors is how many error messages are kept.
	MaxDigestErrors = 3
	// MaxErrorChars is how much of one error message is kept. Enough to say
	// what broke, never enough to read a stack trace aloud.
	MaxErrorChars = 300
	// MaxDigestTools is how many distinct tool calls are kept.
	MaxDigestTools = 40
)

// ToolUse is one tool call, as observed.
//
// It carries no output. That is structural rather than an optimisation:
// ADAPTERS.md §6 says to summarise events and not the transcript, and a type
// that cannot hold a diff cannot leak one into a prompt or into someone's ear.
// OutputBytes is how much came back, which is a fact about the turn; what came
// back is in the transcript, where it belongs.
type ToolUse struct {
	ID     string
	Tool   string
	Target string
	Status event.ToolStatus
	// OutputBytes is the volume of output, never the output.
	OutputBytes int
	At          time.Time
}

// Failed reports whether the runtime said this tool call failed. A tool whose
// status was never reported is not failed — it is unknown, and the narrator
// says less rather than guessing.
func (t ToolUse) Failed() bool { return t.Status == event.ToolFailed }

// Question is a NeedsInput reduced to what gets spoken.
type Question struct {
	Ask     event.InputKind
	Prompt  string
	Options []string
	// Standing marks options that grant something beyond the action in front of
	// us. ORCHESTRATOR.md §4b: the orchestrator must never pick one of these on
	// the user's behalf, so they are flagged rather than filtered — the names
	// are still spoken, a human still chooses.
	Standing []bool
	Tool     string
	Target   string
}

// Digest is everything observable about one turn, in structured form.
//
// It is the only thing the narrator is allowed to speak from. If a runtime
// cannot report something, the corresponding field is empty or nil and the
// spoken line is shorter — never invented. ADAPTERS.md §5's coverage table is
// uneven by runtime and the unevenness has to survive all the way to the ear.
type Digest struct {
	Runtime   string
	Session   string
	Turn      string
	StartedAt time.Time
	EndedAt   time.Time

	// Completed is true once TurnCompleted arrived. A digest read before that
	// is a progress digest, and the narrator uses the mid-task budget.
	Completed  bool
	OK         bool
	StopReason event.StopReason

	Tools []ToolUse

	// Plan is the agent's own stated plan. PlanObserved distinguishes "this
	// runtime has no plan event" from "it has one and said nothing yet":
	// Claude Code emits no PlanUpdated at all (ADAPTERS.md §5), so narration
	// there falls back to tool activity rather than pretending to a plan.
	Plan            []event.PlanStep
	PlanObserved    bool
	PlanSynthesized bool
	PlanNote        string

	// Text is assistant prose, clipped. Reasoning is never included: it is not
	// spoken on any runtime.
	Text           string
	SawReasoning   bool
	Errors         []string
	ErrorCodes     []string
	ErrorFatal     bool
	ErrorRetryable bool

	Question *Question
	Usage    *event.Usage

	// Replay marks a turn we are re-reading rather than watching happen.
	Replay bool
	// Events is how many events the digest was built from. Zero means there is
	// nothing to speak from, which is a fact the narrator has to act on.
	Events int
}

// Redacted returns a copy with every free-text field passed through r.
//
// A digest is built from events, not from a transcript, which is why it was
// treated as safe to narrate directly — but "built from events" is not the same
// as "carries no secrets". A tool target is the command the agent ran, and
// `echo GITLAB_TOKEN=… >> .env` is a command. So is a failing assertion that
// prints a key, which arrives as an error string.
//
// [Summarizer.SummarizeTurn] already redacts its own brief before the model
// sees it. The narrator is a second consumer of the same digest and had no such
// step: it put the tool target into the prompt it posts to the small model, and
// into the line it speaks. This method is what both paths share, so a third
// consumer cannot reintroduce the gap by forgetting.
//
// Everything free-text is covered. Ids, statuses and timings are not text a
// human or an agent wrote, and redacting them would only corrupt them.
func (d Digest) Redacted(r Redactor) Digest {
	if r == nil {
		return d
	}
	clean := func(s string) string {
		if s == "" {
			return s
		}
		out, _ := r.Redact(s)
		return out
	}

	d.Text = clean(d.Text)
	d.PlanNote = clean(d.PlanNote)

	d.Tools = append([]ToolUse(nil), d.Tools...)
	for i := range d.Tools {
		d.Tools[i].Target = clean(d.Tools[i].Target)
	}

	d.Plan = append([]event.PlanStep(nil), d.Plan...)
	for i := range d.Plan {
		d.Plan[i].Text = clean(d.Plan[i].Text)
	}

	d.Errors = append([]string(nil), d.Errors...)
	for i := range d.Errors {
		d.Errors[i] = clean(d.Errors[i])
	}

	if d.Question != nil {
		q := *d.Question
		q.Prompt = clean(q.Prompt)
		q.Target = clean(q.Target)
		// The options are the agent's own words and are spoken verbatim, which
		// is exactly why they get the same treatment as everything else here.
		q.Options = append([]string(nil), q.Options...)
		for i := range q.Options {
			q.Options[i] = clean(q.Options[i])
		}
		d.Question = &q
	}
	return d
}

// Empty reports whether the turn produced nothing observable to speak about.
func (d Digest) Empty() bool {
	return len(d.Tools) == 0 && len(d.Plan) == 0 && strings.TrimSpace(d.Text) == "" &&
		len(d.Errors) == 0 && d.Question == nil
}

// FailedTools returns the tool calls the runtime reported as failed.
func (d Digest) FailedTools() []ToolUse {
	var out []ToolUse
	for _, t := range d.Tools {
		if t.Failed() {
			out = append(out, t)
		}
	}
	return out
}

// ToolNames returns the distinct tool names in order of first use.
func (d Digest) ToolNames() []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range d.Tools {
		if t.Tool == "" || seen[t.Tool] {
			continue
		}
		seen[t.Tool] = true
		out = append(out, t.Tool)
	}
	return out
}

// CurrentStep returns the plan step the agent says it is on.
func (d Digest) CurrentStep() (event.PlanStep, bool) {
	for _, s := range d.Plan {
		if s.Status == event.PlanInProgress {
			return s, true
		}
	}
	return event.PlanStep{}, false
}

// PlanProgress returns completed and total plan steps.
func (d Digest) PlanProgress() (done, total int) {
	for _, s := range d.Plan {
		total++
		if s.Status == event.PlanCompleted {
			done++
		}
	}
	return done, total
}

// Duration is how long the turn took, zero when it cannot be computed.
func (d Digest) Duration() time.Duration {
	if d.StartedAt.IsZero() || d.EndedAt.IsZero() || d.EndedAt.Before(d.StartedAt) {
		return 0
	}
	return d.EndedAt.Sub(d.StartedAt)
}

// ------------------------------------------------------------- the digester --

// Digester folds an event stream into a [Digest]. It is not safe for concurrent
// use; one digester belongs to one turn.
type Digester struct {
	d      Digest
	byTool map[string]int
	text   strings.Builder
	over   bool
}

// NewDigester starts a digest.
func NewDigester() *Digester {
	return &Digester{byTool: map[string]int{}}
}

// Add folds one event in. Events from a different session or turn are ignored
// rather than merged, because a digest that mixes two turns narrates neither.
func (g *Digester) Add(ev event.Event) {
	if ev == nil {
		return
	}
	m := ev.Envelope()
	if g.d.Runtime == "" {
		g.d.Runtime, g.d.Session, g.d.Turn = m.Runtime, m.Session, m.Turn
	} else if m.Session != g.d.Session || (m.Turn != "" && g.d.Turn != "" && m.Turn != g.d.Turn) {
		return
	}
	g.d.Events++
	if m.Replay {
		g.d.Replay = true
	}
	if !m.At.IsZero() {
		if g.d.StartedAt.IsZero() || m.At.Before(g.d.StartedAt) {
			g.d.StartedAt = m.At
		}
		if m.At.After(g.d.EndedAt) {
			g.d.EndedAt = m.At
		}
	}

	switch e := ev.(type) {
	case event.TurnStarted:
		if !m.At.IsZero() {
			g.d.StartedAt = m.At
		}

	case event.TextDelta:
		if !g.over {
			if g.text.Len()+len(e.Text) > MaxDigestText {
				g.over = true
			} else {
				g.text.WriteString(e.Text)
			}
		}

	case event.Reasoning:
		// Never spoken, on any runtime. Recorded only so the narrator can say
		// "still thinking" instead of "still working" when that is all there is.
		g.d.SawReasoning = true

	case event.ToolStarted:
		if len(g.d.Tools) >= MaxDigestTools {
			return
		}
		key := toolKey(e.ID, e.Tool, len(g.d.Tools))
		if _, ok := g.byTool[key]; ok {
			return
		}
		g.byTool[key] = len(g.d.Tools)
		g.d.Tools = append(g.d.Tools, ToolUse{
			ID: e.ID, Tool: e.Tool, Target: e.Target, At: m.At,
		})

	case event.ToolOutput:
		// Merge onto the ToolStarted we already have. ACP's tool_call_update
		// may carry only a toolCallId with every other field null, so an update
		// that describes nothing must not create a phantom tool call.
		idx, ok := g.byTool[toolKey(e.ID, "", -1)]
		if !ok {
			if e.ID == "" {
				return
			}
			// An update for a tool we never saw start. Record the fact of it
			// without inventing a name.
			if len(g.d.Tools) >= MaxDigestTools {
				return
			}
			g.byTool[toolKey(e.ID, "", -1)] = len(g.d.Tools)
			g.d.Tools = append(g.d.Tools, ToolUse{ID: e.ID, At: m.At})
			idx = len(g.d.Tools) - 1
		}
		g.d.Tools[idx].OutputBytes += len(e.Chunk)
		if e.Status != event.ToolUnknown {
			g.d.Tools[idx].Status = e.Status
		}

	case event.PlanUpdated:
		g.d.PlanObserved = true
		g.d.Plan = append([]event.PlanStep(nil), e.Steps...)
		g.d.PlanSynthesized = e.Synthesized
		g.d.PlanNote = e.Explanation

	case *event.NeedsInput:
		q := &Question{Ask: e.Ask, Prompt: e.Prompt}
		for _, o := range e.Options {
			name := o.Name
			if name == "" {
				name = o.ID
			}
			q.Options = append(q.Options, name)
			q.Standing = append(q.Standing, o.Kind.Standing())
		}
		if e.Tool != nil {
			q.Tool = firstNonEmpty(e.Tool.Title, e.Tool.Name)
			q.Target = e.Tool.Kind
		}
		g.d.Question = q

	case event.TurnCompleted:
		g.d.Completed = true
		g.d.OK = e.OK
		g.d.StopReason = e.StopReason
		g.d.Usage = e.Usage
		if !m.At.IsZero() {
			g.d.EndedAt = m.At
		}
		if e.Duration > 0 && !g.d.EndedAt.IsZero() {
			g.d.StartedAt = g.d.EndedAt.Add(-e.Duration)
		}

	case event.Error:
		if len(g.d.Errors) < MaxDigestErrors {
			g.d.Errors = append(g.d.Errors, clip(firstLine(e.Message), MaxErrorChars))
			g.d.ErrorCodes = append(g.d.ErrorCodes, e.Code)
		}
		if e.Fatal {
			g.d.ErrorFatal = true
		}
		if e.Retryable {
			g.d.ErrorRetryable = true
		}
	}
}

// toolKey correlates a ToolOutput with its ToolStarted. Runtimes that report an
// id use it; the ones that do not fall back to position, which is the best that
// can be done without inventing a correlation the protocol never made.
func toolKey(id, tool string, pos int) string {
	if id != "" {
		return "id:" + id
	}
	if tool != "" && pos >= 0 {
		return "pos:" + tool + ":" + itoa(pos)
	}
	return ""
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// Digest returns the folded turn.
func (g *Digester) Digest() Digest {
	d := g.d
	d.Text = strings.TrimSpace(g.text.String())
	if g.over {
		d.Text += " …"
	}
	// Plan steps arrive in the agent's order; keep it. Tools are already in
	// arrival order. Sorting either would destroy the "what is it doing now"
	// signal, so this only stabilises the copy.
	d.Tools = append([]ToolUse(nil), d.Tools...)
	return d
}

// Reset clears the digester for another turn.
func (g *Digester) Reset() {
	g.d = Digest{}
	g.byTool = map[string]int{}
	g.text.Reset()
	g.over = false
}

// DigestOf folds a whole slice of events at once.
func DigestOf(events []event.Event) Digest {
	g := NewDigester()
	for _, ev := range events {
		g.Add(ev)
	}
	return g.Digest()
}

// ------------------------------------------------------------------ helpers --

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return strings.TrimRight(string(r[:n-1]), " ,;:-") + "…"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
