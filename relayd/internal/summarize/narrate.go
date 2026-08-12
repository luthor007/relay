package summarize

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/logx"
)

// OfferError is what a failed turn ends with. ADAPTERS.md §6: say what failed
// and stop, do not read a stack trace aloud — offer it.
const OfferError = "Want the error?"

// NarratorTimeout bounds one narration call. The small model's whole job is to
// arrive before the big one (ORCHESTRATOR.md §3b's ~400 ms to first word), so a
// slow narration is a failed narration: past this the template speaks and the
// user hears something true immediately instead of something better, later.
const NarratorTimeout = 2500 * time.Millisecond

// NarratorOptions configures a Narrator.
type NarratorOptions struct {
	// Model is the small model. Nil is supported and means every line comes
	// from the deterministic template — which is a working product, just a
	// blunter one.
	Model       llm.Provider
	Log         *slog.Logger
	Temperature *float64
	Timeout     time.Duration
	Now         func() time.Time

	// Redact is applied to the digest before anything is phrased or spoken.
	// Nil means [Detector], which is what every caller wants; the field exists
	// so a test can assert on the seam, not so it can be switched off.
	Redact Redactor
}

// Narrator turns a [Digest] into something a person hears.
//
// It never returns an error. A narration that fails is a narration that falls
// back to the template — silence after someone speaks reads as broken, and a
// blunt true sentence beats a polished absent one.
type Narrator struct {
	model   llm.Provider
	log     *slog.Logger
	temp    *float64
	timeout time.Duration
	redact  Redactor
}

// NewNarrator builds a Narrator.
func NewNarrator(o NarratorOptions) *Narrator {
	n := &Narrator{model: o.Model, log: o.Log, temp: o.Temperature, timeout: o.Timeout, redact: o.Redact}
	if n.log == nil {
		n.log = logx.Discard()
	}
	if n.redact == nil {
		n.redact = Detector()
	}
	if n.timeout == 0 {
		n.timeout = NarratorTimeout
	}
	return n
}

// AckHint is the little that is known at acknowledgement time — before any
// agent event exists, which is the entire point of the acknowledgement.
type AckHint struct {
	// Subject is what the user's utterance was about, as the router understood
	// it: "payments branch", "the api repo".
	Subject string
	// Verb is what is about to happen: "checking", "starting", "asking".
	Verb string
}

// Ack is the immediate "on it". ~40 characters, ~3 seconds.
func (n *Narrator) Ack(ctx context.Context, h AckHint) Speech {
	sp := Speech{Moment: MomentAck, Cap: CapAck, Source: SourceTemplate, Grounded: h.Subject != ""}
	base := "On it"
	if h.Verb != "" {
		base = strings.ToUpper(h.Verb[:1]) + h.Verb[1:]
	}
	line := base + "."
	if h.Subject != "" {
		line = base + " — " + h.Subject + "."
	}
	sp.Text, sp.Truncated = Fit(line, CapAck)
	_ = ctx
	return sp
}

// Progress is a mid-task update. ~90 characters.
func (n *Narrator) Progress(ctx context.Context, d Digest) Speech {
	return n.speak(ctx, MomentProgress, d)
}

// Completed is the turn boundary. ~160 characters — two short sentences.
func (n *Narrator) Completed(ctx context.Context, d Digest) Speech {
	return n.speak(ctx, MomentCompleted, d)
}

// NeedsInput is a blocked session asking. ~120 characters plus the options,
// which are spoken verbatim because they are the agent's words.
func (n *Narrator) NeedsInput(ctx context.Context, d Digest) Speech {
	return n.speak(ctx, MomentNeedsInput, d)
}

// Narrate dispatches on the moment.
func (n *Narrator) Narrate(ctx context.Context, m Moment, d Digest) Speech {
	return n.speak(ctx, m, d)
}

func (n *Narrator) speak(ctx context.Context, m Moment, d Digest) Speech {
	// Before the template, not after. Every return path below hands back either
	// the template or something derived from the brief, and both are built from
	// this digest — so redacting once here is what makes all of them safe, and
	// it happens before the length budgets are applied rather than after, so a
	// replacement cannot push a line past its cap.
	d = d.Redacted(n.redact)

	tpl := Template(m, d)
	if n.model == nil {
		return tpl
	}

	if d.Empty() {
		// Nothing observable happened, so there is nothing for a model to
		// phrase that would not be an invention. ORCHESTRATOR.md §3b: given no
		// event, say "still working" or say nothing — never a specific. Not
		// asking is a stronger guarantee than asking and checking.
		return tpl
	}
	brief := Brief(m, d)
	if strings.TrimSpace(brief) == "" {
		return tpl
	}

	cctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()

	resp, err := n.model.Complete(cctx, llm.Request{
		System:      SpeechPrompt(m),
		Messages:    []llm.Message{{Role: llm.RoleUser, Text: brief}},
		MaxTokens:   m.Cap()/3 + 24,
		Temperature: n.temp,
		// The voice model does not hold tools (ORCHESTRATOR.md §3b gives them
		// to the big one), and saying so on the wire is cheaper than trusting
		// that nobody ever configures a provider that adds some. A narrator
		// that emits a tool call produces silence, which is the one thing this
		// path exists to prevent.
		ToolChoice: &llm.ToolChoice{Mode: llm.ChoiceNone},
		// Low effort is the whole job description here: fewer tokens, no
		// preamble, terser confirmations. ADAPTERS.md §6 budgets a completed
		// turn at ~160 characters, so thoroughness is not the trade this call
		// wants to make.
		Effort: "low",
	})
	if err != nil {
		n.log.Warn("narration fell back to the template", "moment", string(m), "err", err)
		return tpl
	}

	line, ok := Accept(resp.Text, brief, m)
	if !ok {
		n.log.Info("narration rejected, using the template",
			"moment", string(m), "reason", rejectReason(resp.Text, brief, m))
		return tpl
	}

	sp := tpl
	sp.Source = SourceModel
	if tpl.Offer != "" {
		sp.Text, sp.Truncated = FitWithOffer(line, tpl.Offer, m.Cap())
	} else {
		sp.Text, sp.Truncated = Fit(line, m.Cap())
	}
	return sp
}

// SpeechPrompt is the small model's instruction for one moment.
//
// It states the three rules from ADAPTERS.md §6 and ORCHESTRATOR.md §3b's
// grounding rule. None of them is trusted: [Accept] checks the answer, [Fit]
// enforces the length, and a failure of either is a fallback rather than a
// worse line going out.
func SpeechPrompt(m Moment) string {
	var b strings.Builder
	b.WriteString("You are the voice of a coding assistant. You are given structured events from an agent turn and you say one thing out loud.\n\n")
	fmt.Fprintf(&b, "Hard limit: %d characters. Speech runs about %d characters a second, so this is about %.0f seconds in someone's ear.\n",
		m.Cap(), CharsPerSecond, m.Budget().Seconds())
	b.WriteString("Lead with the outcome. The listener is walking and keeps the first clause and little else.\n")
	b.WriteString("No preamble. Never open with \"I've finished\", \"I'm happy to report\", \"Here's a summary\", \"Let me\", or an acknowledgement.\n")
	b.WriteString("Say only what the events say. If the events do not say it, do not say it. No invented file names, numbers or reasons.\n")
	b.WriteString("Plain spoken English. No markdown, no lists, no code, no file contents, no URLs, no paths longer than a filename.\n")

	switch m {
	case MomentAck:
		b.WriteString("This is the immediate acknowledgement. One short clause, naming what you are about to do.\n")
	case MomentProgress:
		b.WriteString("This is a mid-task update. One clause about what is happening right now.\n")
	case MomentNeedsInput:
		b.WriteString("The session is blocked and needs an answer. State the question. Do not list the options; they are read out separately.\n")
	default:
		b.WriteString("The turn has finished. At most two short sentences.\n")
		b.WriteString("If it failed: name what failed and stop. Never read an error message or a stack trace — end with \"" + OfferError + "\".\n")
		b.WriteString("Example of the right shape: \"Tests pass. Two files changed.\"\n")
		b.WriteString("Example of the wrong shape: \"I've finished working on the payments branch and I can report that the tests are passing.\"\n")
	}
	return b.String()
}

// Brief is the structured input the small model sees.
//
// It is built from events and nothing else. That is ADAPTERS.md §6's first rule
// — summarise events, not the transcript — and it is also the cheapest
// hallucination defence available: the model cannot repeat command output it
// was never shown. The digest holds no tool output at all, only byte counts, so
// there is nothing here to leak.
func Brief(m Moment, d Digest) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	if d.Runtime != "" {
		w("runtime: %s", d.Runtime)
	}
	if d.Completed {
		outcome := "succeeded"
		if !d.OK {
			outcome = "failed"
		}
		if d.StopReason != "" {
			w("outcome: %s (%s)", outcome, d.StopReason)
		} else {
			w("outcome: %s", outcome)
		}
		if dur := d.Duration(); dur > 0 {
			w("duration: %s", dur.Round(time.Second))
		}
	} else {
		w("outcome: still running")
	}

	if d.PlanObserved && len(d.Plan) > 0 {
		done, total := d.PlanProgress()
		w("plan: %d of %d steps done", done, total)
		if s, ok := d.CurrentStep(); ok {
			w("plan now: %s", clip(Clean(s.Text), 120))
		}
	} else if !d.PlanObserved {
		// Said explicitly so the model does not read an absent plan as an
		// invitation to infer one. Claude Code emits no PlanUpdated at all.
		w("plan: not reported by this runtime")
	}

	if len(d.Tools) > 0 {
		var lines []string
		for _, t := range d.Tools {
			name := t.Tool
			if name == "" {
				name = "tool"
			}
			line := name
			if t.Target != "" {
				line += " on " + clip(Clean(t.Target), 60)
			}
			switch t.Status {
			case event.ToolFailed:
				line += " — failed"
			case event.ToolCompleted:
				line += " — ok"
			case event.ToolPending, event.ToolInProgress:
				line += " — running"
			}
			lines = append(lines, line)
		}
		w("tools: %s", strings.Join(lines, "; "))
	}

	for i, e := range d.Errors {
		code := ""
		if i < len(d.ErrorCodes) && d.ErrorCodes[i] != "" {
			code = d.ErrorCodes[i] + ": "
		}
		w("error: %s%s", code, clip(Clean(e), 160))
	}

	if d.Question != nil {
		w("blocked on: %s", clip(Clean(d.Question.Prompt), 200))
		if d.Question.Tool != "" {
			w("about tool: %s", clip(Clean(d.Question.Tool), 60))
		}
		if len(d.Question.Options) > 0 {
			w("options: %s", strings.Join(d.Question.Options, " / "))
		}
	}

	if t := strings.TrimSpace(d.Text); t != "" {
		w("it said: %s", clip(Clean(t), 400))
	} else if d.SawReasoning {
		w("it said: nothing yet, still thinking")
	}

	if d.Usage != nil {
		if p, ok := d.Usage.ContextPressure(); ok {
			w("context used: %.0f%%", p*100)
		}
	}
	return b.String()
}

// ------------------------------------------------------------- acceptance --

// Accept checks a model's line against the rules the prompt asked for and
// returns it cleaned, or false.
//
// Checking rather than trusting is the point. ORCHESTRATOR.md §3b names
// narration drift as one of the two ways the two-model split breaks — the small
// model says "running the tests" while the big one is doing something else —
// and calls it a plumbing problem rather than a prompt-engineering one. This is
// the plumbing.
func Accept(text, brief string, m Moment) (string, bool) {
	line := TrimOpener(Clean(text))
	if line == "" {
		return "", false
	}
	if HasPreamble(line) {
		return "", false
	}
	if hasUnspeakableToken(line) {
		return "", false
	}
	if !Grounded(line, brief) {
		return "", false
	}
	fitted, _ := Fit(line, m.Cap())
	if fitted == "" {
		return "", false
	}
	return fitted, true
}

func rejectReason(text, brief string, m Moment) string {
	line := TrimOpener(Clean(text))
	switch {
	case line == "":
		return "empty"
	case HasPreamble(line):
		return "preamble"
	case hasUnspeakableToken(line):
		return "unspeakable token"
	case !Grounded(line, brief):
		return "ungrounded identifier"
	}
	_ = m
	return "unknown"
}

// maxSpokenToken is the longest single token worth saying out loud. Anything
// longer is a path, a hash or a URL, and a TTS voice reading one is the same
// experience as reading a stack trace aloud.
const maxSpokenToken = 40

func hasUnspeakableToken(s string) bool {
	for _, f := range strings.Fields(s) {
		if len([]rune(f)) > maxSpokenToken {
			return true
		}
		if strings.HasPrefix(f, "http://") || strings.HasPrefix(f, "https://") {
			return true
		}
	}
	return false
}

// Grounded reports whether every identifier the line names also appears in the
// brief.
//
// It is a mechanical check, not a semantic one, and it is deliberately narrow:
// it catches the invented specific — a file, a package, a repo, an error code
// that was never in the events — and lets ordinary English through. A vague
// true update beats a precise invented one, so a line that names nothing passes
// and a line that names something absent does not.
func Grounded(line, brief string) bool {
	haystack := strings.ToLower(brief)
	for _, f := range strings.Fields(line) {
		tok := strings.Trim(f, ".,;:!?()[]{}\"'—")
		if !looksLikeIdentifier(tok) {
			continue
		}
		if !strings.Contains(haystack, strings.ToLower(tok)) {
			return false
		}
	}
	return true
}

func looksLikeIdentifier(tok string) bool {
	if len(tok) < 4 {
		return false
	}
	var letters, digits, seps int
	for _, r := range tok {
		switch {
		case unicode.IsDigit(r):
			digits++
		case unicode.IsLetter(r):
			letters++
		case r == '_' || r == '/' || r == '.' || r == '-':
			seps++
		}
	}
	if letters == 0 {
		return false
	}
	if seps > 0 && digits+letters > 0 {
		return true
	}
	if digits > 0 {
		return true
	}
	// A shouted acronym: ECONNREFUSED, SIGSEGV.
	return strings.ToUpper(tok) == tok && strings.ToLower(tok) != tok
}

// ---------------------------------------------------------------- template --

// Template builds the deterministic line for a moment. Every word of it comes
// from an event, which is what makes it a safe fallback rather than a worse
// one.
func Template(m Moment, d Digest) Speech {
	sp := Speech{Moment: m, Cap: m.Cap(), Source: SourceTemplate}

	switch m {
	case MomentAck:
		sp.Text, sp.Truncated = Fit("On it.", CapAck)
		return sp

	case MomentNeedsInput:
		return questionTemplate(d)

	case MomentProgress:
		return progressTemplate(d)
	}

	if !d.Completed {
		return progressTemplate(d)
	}
	return completedTemplate(d)
}

func progressTemplate(d Digest) Speech {
	sp := Speech{Moment: MomentProgress, Cap: CapProgress, Source: SourceTemplate}

	if step, ok := d.CurrentStep(); ok && strings.TrimSpace(step.Text) != "" {
		sp.Grounded = true
		sp.Text, sp.Truncated = Fit(sentence(step.Text), CapProgress)
		return sp
	}
	if len(d.Tools) > 0 {
		t := d.Tools[len(d.Tools)-1]
		sp.Grounded = true
		line := "Running " + orDefault(t.Tool, "a tool")
		if t.Target != "" {
			line += " on " + clip(Clean(t.Target), 40)
		}
		sp.Text, sp.Truncated = Fit(sentence(line), CapProgress)
		return sp
	}
	if d.SawReasoning {
		// True and vague, which ORCHESTRATOR.md §3b prefers to precise and
		// invented.
		sp.Text = "Still thinking."
		return sp
	}
	sp.Text = "Still working."
	return sp
}

func completedTemplate(d Digest) Speech {
	sp := Speech{Moment: MomentCompleted, Cap: CapCompleted, Source: SourceTemplate}

	if !d.OK || len(d.Errors) > 0 || len(d.FailedTools()) > 0 {
		outcome, offerable := failureOutcome(d)
		sp.Grounded = offerable || d.StopReason != ""
		if !offerable {
			// A turn the user cancelled, or one that hit its own limit, has no
			// error to read. Offering one would be a promise we cannot keep,
			// and "Want the error?" answered with "there isn't one" is worse
			// than not asking.
			sp.Text, sp.Truncated = Fit(outcome, CapCompleted)
			return sp
		}
		sp.Offer = OfferError
		sp.Text, sp.Truncated = FitWithOffer(outcome, OfferError, CapCompleted)
		return sp
	}

	lead, grounded := successLead(d)
	detail := successDetail(d)
	sp.Grounded = grounded || detail != ""
	line := lead
	if detail != "" {
		line = lead + " " + detail
	}
	sp.Text, sp.Truncated = Fit(line, CapCompleted)
	return sp
}

// failureOutcome names what failed, and says whether there is an error to
// offer. The two are not the same: a cancelled turn failed and has nothing to
// read out.
func failureOutcome(d Digest) (string, bool) {
	if failed := d.FailedTools(); len(failed) > 0 {
		t := failed[0]
		line := orDefault(t.Tool, "A tool") + " failed"
		if t.Target != "" {
			line += " on " + clip(Clean(t.Target), 48)
		}
		return sentence(line), true
	}
	if len(d.Errors) > 0 {
		return sentence(clip(Clean(d.Errors[0]), 90)), true
	}
	switch d.StopReason {
	case event.StopCancelled:
		return "Stopped.", false
	case event.StopMaxTokens:
		return "It ran out of room.", false
	case event.StopMaxTurnRequests:
		return "It hit its step limit.", false
	case event.StopRefusal:
		return "It declined that one.", false
	}
	return "That turn failed.", false
}

func successLead(d Digest) (string, bool) {
	if s := firstSentence(d.Text); s != "" && len([]rune(s)) <= 90 {
		return sentence(s), true
	}
	if d.PlanObserved {
		if done, total := d.PlanProgress(); total > 0 && done == total {
			return "Done, all " + itoa(total) + " steps.", true
		}
	}
	return "Done.", false
}

func successDetail(d Digest) string {
	names := d.ToolNames()
	switch {
	case len(names) == 0:
		return ""
	case len(names) == 1:
		t := d.Tools[len(d.Tools)-1]
		if t.Target != "" {
			return sentence(names[0] + " on " + clip(Clean(t.Target), 40))
		}
		return sentence("Ran " + names[0])
	default:
		return sentence(itoa(len(d.Tools)) + " tool calls, last was " + names[len(names)-1])
	}
}

func questionTemplate(d Digest) Speech {
	sp := Speech{Moment: MomentNeedsInput, Cap: CapNeedsInput, Source: SourceTemplate}
	q := d.Question
	if q == nil {
		sp.Text = "A session needs an answer."
		return sp
	}
	sp.Grounded = true

	line := strings.TrimSpace(Clean(q.Prompt))
	if line == "" {
		switch {
		case q.Tool != "":
			line = "It wants to run " + clip(Clean(q.Tool), 40)
		default:
			line = "It needs your permission"
		}
	}
	sp.Text, sp.Truncated = Fit(sentence(line), CapNeedsInput)

	for i, o := range q.Options {
		opt, _ := Fit(o, CapOption)
		if opt == "" {
			continue
		}
		sp.Options = append(sp.Options, opt)
		standing := false
		if i < len(q.Standing) {
			standing = q.Standing[i]
		}
		sp.Standing = append(sp.Standing, standing)
	}
	return sp
}

// ------------------------------------------------------------------ helpers --

func sentence(s string) string {
	s = strings.TrimSpace(Clean(s))
	if s == "" {
		return ""
	}
	r := []rune(s)
	if unicode.IsLower(r[0]) {
		r[0] = unicode.ToUpper(r[0])
		s = string(r)
	}
	switch r[len(r)-1] {
	case '.', '!', '?', '…':
		return s
	}
	return s + "."
}

func firstSentence(s string) string {
	s = strings.TrimSpace(Clean(s))
	if s == "" {
		return ""
	}
	r := []rune(s)
	for i, c := range r {
		if c == '.' || c == '!' || c == '?' {
			if i+1 < len(r) && !unicode.IsSpace(r[i+1]) {
				continue
			}
			return strings.TrimSpace(string(r[:i+1]))
		}
	}
	return s
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
