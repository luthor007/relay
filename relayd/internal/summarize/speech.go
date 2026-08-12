package summarize

import (
	"regexp"
	"strings"
	"time"
	"unicode"
)

// CharsPerSecond is how fast synthesised speech runs, from ADAPTERS.md §6. It
// is the reason the caps below are in characters at all: a 400-character
// summary is nearly half a minute in someone's ear, and nobody reads a
// character count and hears the length. Divide by this and you do.
const CharsPerSecond = 14

// Moment is when we are speaking, which is the only thing that sets the budget.
type Moment string

const (
	// MomentAck is the immediate acknowledgement — ORCHESTRATOR.md §3b's
	// ~400 ms first word, the thing that stops eight seconds of silence
	// reading as broken.
	MomentAck Moment = "ack"
	// MomentProgress is a mid-task update.
	MomentProgress Moment = "progress"
	// MomentCompleted is the turn boundary. Two short sentences.
	MomentCompleted Moment = "completed"
	// MomentNeedsInput is a blocked session asking. The cap covers the
	// question; the options are spoken after it and are not counted against it,
	// because they are the agent's words and get said verbatim.
	MomentNeedsInput Moment = "needs_input"
)

// Caps, in characters, from ADAPTERS.md §6.
const (
	CapAck        = 40
	CapProgress   = 90
	CapCompleted  = 160
	CapNeedsInput = 120
	// CapOption bounds one spoken option name. ADAPTERS.md §4 says we speak the
	// names the agent gave us, so this clips rather than rewrites.
	CapOption = 48
)

// Cap is the character budget for this moment.
func (m Moment) Cap() int {
	switch m {
	case MomentAck:
		return CapAck
	case MomentProgress:
		return CapProgress
	case MomentNeedsInput:
		return CapNeedsInput
	default:
		return CapCompleted
	}
}

// Budget is the cap expressed as time in someone's ear.
func (m Moment) Budget() time.Duration {
	return time.Duration(float64(m.Cap()) / CharsPerSecond * float64(time.Second))
}

// Source records whether a line was phrased by the small model or built from a
// template.
type Source string

const (
	// SourceModel is the small model's phrasing, having passed the checks.
	SourceModel Source = "model"
	// SourceTemplate is the deterministic fallback: no model configured, the
	// model failed, or its answer was rejected. It is the honest floor — every
	// word of it comes from an event.
	SourceTemplate Source = "template"
)

// Speech is one thing to say.
type Speech struct {
	Moment Moment
	Text   string
	// Options are spoken after Text, verbatim, for a needs-input ping.
	Options []string
	// Standing flags options that grant something beyond this one action.
	// ORCHESTRATOR.md §4b: the orchestrator never picks one of these itself.
	Standing []bool
	// Offer is the follow-up a failed turn ends with. It is already included in
	// Text; it is repeated here so a caller can wire the answer.
	Offer  string
	Source Source
	// Grounded is false when there were no observable events to speak from. The
	// line is deliberately vague in that case — ORCHESTRATOR.md §3b: given no
	// event, say "still working" or say nothing, never invent a specific.
	Grounded  bool
	Truncated bool
	Cap       int
}

// Chars is the spoken length of the whole utterance, options included.
func (s Speech) Chars() int {
	n := len([]rune(s.Text))
	for _, o := range s.Options {
		n += len([]rune(o)) + 2
	}
	return n
}

// Duration is how long this takes to say.
func (s Speech) Duration() time.Duration {
	return time.Duration(float64(s.Chars()) / CharsPerSecond * float64(time.Second))
}

// WithinCap reports whether the spoken text respects its moment's budget. The
// options are excluded, per ADAPTERS.md §6's "~120 chars + options".
func (s Speech) WithinCap() bool { return len([]rune(s.Text)) <= s.Cap }

// ------------------------------------------------------------- enforcement --

var (
	// spaces collapses every run of whitespace. Speech has no line breaks, and
	// a newline read by a TTS engine is either a pause or nothing, depending on
	// the engine.
	spaces = regexp.MustCompile(`\s+`)
	// markup strips characters that a TTS voice either reads aloud or chokes
	// on. This is formatting removal, not prose parsing.
	markup = regexp.MustCompile("[`*_#>|]+")
	// bullets removes list markers left at the start of a clipped line.
	bullets = regexp.MustCompile(`(?m)^\s*[-*•]\s+`)
)

// Clean makes text speakable: one line, no markdown, no double spaces.
func Clean(s string) string {
	s = bullets.ReplaceAllString(s, "")
	s = markup.ReplaceAllString(s, "")
	s = spaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// openers are the one-word fillers that can be removed without touching the
// sentence underneath. A closed list, deliberately: anything longer than a word
// or two is a rewrite, and rewriting model prose is prose-parsing.
var openers = map[string]bool{
	"ok": true, "okay": true, "sure": true, "alright": true, "right": true,
	"well": true, "so": true, "great": true, "perfect": true,
	"certainly": true, "absolutely": true, "got": true,
}

// TrimOpener removes a leading one-word filler and its punctuation, and
// recapitalises what is left — "Okay, tests pass." becomes "Tests pass.", not
// "tests pass.", because a TTS voice does not fix the case for you.
func TrimOpener(s string) string {
	trimmed := false
	for i := 0; i < 2; i++ {
		t := strings.TrimSpace(s)
		if t == "" {
			return t
		}
		word := t
		if j := strings.IndexAny(t, " ,.!:—-"); j > 0 {
			word = t[:j]
		}
		if !openers[strings.ToLower(word)] {
			s = t
			break
		}
		rest := strings.TrimLeft(t[len(word):], " ,.!:—-")
		if rest == "" {
			s = t
			break
		}
		s = rest
		trimmed = true
	}
	s = strings.TrimSpace(s)
	if !trimmed || s == "" {
		return s
	}
	r := []rune(s)
	if unicode.IsLower(r[0]) {
		r[0] = unicode.ToUpper(r[0])
		s = string(r)
	}
	return s
}

// preambles are the phrasings ADAPTERS.md §6 forbids: everything that puts the
// speaker before the outcome. They are a rejection test, not a rewrite: output
// that matches one is thrown away and the deterministic template speaks
// instead. Editing prose to remove a preamble means parsing prose, which
// SYSTEM.md says is the wrong path — and a half-edited sentence is worse than a
// plain one.
var preambles = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^i(?:'ve| have| had)?\s+(?:just\s+)?(?:finished|completed|been|done|gone|worked|gotten)\b`),
	regexp.MustCompile(`(?i)^i(?:'m| am)\s+(?:happy|pleased|glad|able)\s+to\b`),
	regexp.MustCompile(`(?i)^(?:here'?s|here is|this is)\s+(?:a|the|my)?\s*(?:quick\s+|short\s+|brief\s+)?(?:summary|update|rundown|recap|report)\b`),
	regexp.MustCompile(`(?i)^let me\b`),
	regexp.MustCompile(`(?i)^as (?:requested|asked|instructed)\b`),
	regexp.MustCompile(`(?i)^just (?:to )?(?:let you know|letting you know|a quick note)\b`),
	regexp.MustCompile(`(?i)^(?:it|that) (?:looks|seems|appears)\s+(?:like|that)\b`),
	regexp.MustCompile(`(?i)^(?:in )?summary[,:]`),
	regexp.MustCompile(`(?i)\bi can report that\b`),
	regexp.MustCompile(`(?i)^(?:the )?(?:good|bad) news is\b`),
	regexp.MustCompile(`(?i)^(?:working|starting|beginning) on it\b`),
}

// HasPreamble reports whether the line leads with the speaker instead of the
// outcome.
//
// "Tests pass. Two files changed." beats "I've finished working on the payments
// branch and I can report that…" because a listener walking down a street keeps
// the first clause and loses the rest. The second one spends its entire first
// clause saying nothing.
func HasPreamble(s string) bool {
	t := TrimOpener(Clean(s))
	for _, re := range preambles {
		if re.MatchString(t) {
			return true
		}
	}
	return false
}

// Fit enforces a character cap in code, because a prompt is a request and this
// is a guarantee. It returns the fitted text and whether anything was cut.
//
// Cutting prefers a sentence boundary, then a word boundary. It never cuts
// mid-word: a clipped word is heard as a stutter and makes the whole line sound
// broken rather than short.
func Fit(text string, cap int) (string, bool) {
	s := TrimOpener(Clean(text))
	if cap <= 0 {
		return "", s != ""
	}
	r := []rune(s)
	if len(r) <= cap {
		return s, false
	}

	// The longest run of whole sentences that fits.
	if cut := lastSentenceEnd(r, cap); cut > 0 {
		return strings.TrimSpace(string(r[:cut])), true
	}
	// Otherwise the longest run of whole words.
	if cut := lastSpace(r, cap); cut > 0 {
		out := strings.TrimRight(strings.TrimSpace(string(r[:cut])), " ,;:-—")
		if out != "" {
			return out, true
		}
	}
	return strings.TrimSpace(string(r[:cap])), true
}

func lastSentenceEnd(r []rune, cap int) int {
	for i := cap - 1; i > 0; i-- {
		switch r[i] {
		case '.', '!', '?':
			// Not a decimal point or an ellipsis mid-number.
			if i+1 < len(r) && !unicode.IsSpace(r[i+1]) {
				continue
			}
			return i + 1
		}
	}
	return 0
}

func lastSpace(r []rune, cap int) int {
	for i := cap; i > 0; i-- {
		if unicode.IsSpace(r[i-1]) {
			return i - 1
		}
	}
	return 0
}

// FitWithOffer fits a line while guaranteeing the offer survives.
//
// ADAPTERS.md §6: if the turn failed, say what failed and stop — do not read a
// stack trace aloud, offer it. "Build broke on the auth module. Want the
// error?" The offer is the actionable half, so when the whole line does not fit
// the outcome is clipped and the offer is kept, not the other way round.
func FitWithOffer(outcome, offer string, cap int) (string, bool) {
	offer = Clean(offer)
	if offer == "" {
		return Fit(outcome, cap)
	}
	room := cap - len([]rune(offer)) - 1
	if room < 12 {
		// No room for both. The offer is what the listener can act on.
		out, _ := Fit(offer, cap)
		return out, true
	}
	head, cut := Fit(outcome, room)
	if head == "" {
		out, _ := Fit(offer, cap)
		return out, true
	}
	if !strings.HasSuffix(head, ".") && !strings.HasSuffix(head, "!") && !strings.HasSuffix(head, "?") {
		head += "."
	}
	joined := head + " " + offer
	if len([]rune(joined)) > cap {
		joined, _ = Fit(joined, cap)
		return joined, true
	}
	return joined, cut
}
