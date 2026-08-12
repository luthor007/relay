package episode

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Commitment is SYSTEM.md §5's entity, plus the evidence that makes it
// defensible.
//
// MEMORY.md §5's first rule about facts applies here for the same reason: "A
// fact that cannot point at where it came from is deleted, not kept at low
// confidence." A commitment is a reminder with somebody's name on it, and one
// that cannot be traced to a sentence is a reminder the user cannot argue with.
type Commitment struct {
	Text string
	// Speaker is who made it. The wearer's own commitments are the killer
	// output (ARCHITECTURE.md §4); a commitment made *to* the wearer is worth
	// keeping too, and the two are told apart here rather than downstream.
	Speaker string
	// OwedTo is who it was made to, where the sentence or the room says.
	OwedTo string
	At     time.Time
	// DueAt is resolved against [Commitment.At], and is zero when the sentence
	// carried no date. Zero means "no date was said", never "today".
	DueAt time.Time
	// Evidence is the sentence, verbatim.
	Evidence string
	// Cue is which pattern fired, so a wrong extraction can be traced to a rule
	// rather than to a vibe.
	Cue string
}

// Decision is something that was settled.
type Decision struct {
	Text     string
	Speaker  string
	At       time.Time
	Evidence string
	Cue      string
}

// Extraction is one episode's structured layer.
type Extraction struct {
	Commitments []Commitment
	Decisions   []Decision
	Notes       []string
}

// Extractor turns an episode into its structured layer.
//
// An interface with one deterministic implementation, so a model-backed pass can
// be added later as a *proposal* on top of this floor rather than as a
// replacement for it.
type Extractor interface {
	Extract(e Episode, o Options) Extraction
}

// Rules is the deterministic extractor. See the package doc for why this is
// rules and not a model.
type Rules struct{}

// Extract runs the rules over an episode.
func Extract(e Episode, o Options) Extraction { return Rules{}.Extract(e, o) }

// Extract implements [Extractor].
func (Rules) Extract(e Episode, o Options) Extraction {
	o = o.withDefaults()
	var ex Extraction

	others := otherParticipants(e, o.Wearer)
	for _, u := range e.Utterances {
		if u.Gap || strings.TrimSpace(u.Text) == "" {
			continue
		}
		for _, sentence := range splitSentences(u.Text) {
			lower := strings.ToLower(sentence)

			if cue, ok := matchCue(lower, decisionCues); ok {
				ex.Decisions = append(ex.Decisions, Decision{
					Text: sentence, Speaker: u.Speaker, At: u.At, Evidence: sentence, Cue: cue,
				})
				continue
			}
			if cue, ok := matchCue(lower, commitmentCues); ok {
				c := Commitment{
					Text: sentence, Speaker: u.Speaker, At: u.At, Evidence: sentence, Cue: cue,
				}
				c.OwedTo = owedTo(sentence, u.Speaker, others, o.Wearer)
				c.DueAt = ParseDue(sentence, u.At)
				ex.Commitments = append(ex.Commitments, c)
				continue
			}
			if note, ok := salient(sentence); ok && len(ex.Notes) < o.MaxNotes {
				ex.Notes = append(ex.Notes, note)
			}
		}
	}
	return ex
}

// commitmentCues are first-person future forms. They are prefixes of a clause
// rather than substrings anywhere, because "I'll" inside "he said I'll never"
// is a report and not a promise — [matchCue] anchors them at a clause boundary.
//
// The French entries are not decoration: Quebec is the home market
// (ARCHITECTURE.md §6 names it as the jurisdiction that makes consent a legal
// question) and a francophone user whose commitments are silently unextracted
// gets a memory that works for half their day.
var commitmentCues = []string{
	"i'll ", "i will ", "i am going to ", "i'm going to ", "i can ", "i shall ",
	"let me ", "i'll get ", "i promise ", "i'll have ", "i've got to ", "i need to ",
	"remind me to ", "i owe you ", "i'll take care of ",
	"je vais ", "j'envoie ", "je t'envoie ", "je m'occupe ", "je te ", "je vous ",
}

// decisionCues are settled outcomes. They are checked *before* commitments,
// because "we decided I'll take the Stripe work" is a decision that happens to
// contain a promise and filing it twice would double-count it.
var decisionCues = []string{
	"we decided ", "we've decided ", "we have decided ", "we decided,",
	"the decision is ", "decision: ", "let's go with ", "lets go with ",
	"we're going with ", "we are going with ", "we'll go with ",
	"we settled on ", "we agreed to ", "we agreed on ", "agreed: ",
	"on a décidé ", "on décide ", "on va prendre ",
}

// matchCue reports whether a cue opens a clause in the sentence. Anchoring at
// the start or just after a comma, a semicolon or a conjunction is what keeps
// "he said I'll never do that" from being read as a promise.
func matchCue(lower string, cues []string) (string, bool) {
	for _, cue := range cues {
		if strings.HasPrefix(lower, cue) {
			return strings.TrimSpace(cue), true
		}
		for _, sep := range []string{", ", "; ", " and ", " but ", " so ", " then "} {
			if strings.Contains(lower, sep+cue) {
				return strings.TrimSpace(cue), true
			}
		}
	}
	return "", false
}

// owedTo works out who a commitment is to.
//
// Two sources, in order, and neither of them guesses: the sentence naming a
// person, then — only in a two-person episode — the other person in the room.
// Three people and no name in the sentence produces an empty OwedTo, because
// picking one of them would be inventing a fact about somebody.
func owedTo(sentence, speaker string, others []string, wearer string) string {
	lower := strings.ToLower(sentence)

	// A participant named in the sentence wins.
	for _, p := range others {
		if p == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(p)) {
			return p
		}
	}
	// "send it to Marc", "for Marc".
	if m := toName.FindStringSubmatch(sentence); m != nil {
		return m[2]
	}
	// "I'll send you the BOM", said by someone else, is owed to the wearer.
	if speaker != "" && speaker != wearer && youPronoun.MatchString(lower) {
		return wearer
	}
	if len(others) == 1 {
		return others[0]
	}
	return ""
}

var (
	toName     = regexp.MustCompile(`\b(to|for)\s+([A-Z][\p{L}'-]+)`)
	youPronoun = regexp.MustCompile(`\byou\b|\byour\b`)
)

func otherParticipants(e Episode, wearer string) []string {
	var out []string
	for _, p := range e.Participants {
		if p != wearer {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// splitSentences breaks an utterance into sentences. Deliberately simple: the
// input is already one speaker's settled line, and a clever splitter would be
// the prose-parsing this whole system is built to avoid.
func splitSentences(text string) []string {
	var (
		out []string
		buf strings.Builder
	)
	for _, r := range text {
		buf.WriteRune(r)
		if r == '.' || r == '?' || r == '!' {
			if s := strings.TrimSpace(buf.String()); s != "" {
				out = append(out, s)
			}
			buf.Reset()
		}
	}
	if s := strings.TrimSpace(buf.String()); s != "" {
		out = append(out, s)
	}
	return out
}

// ---------------------------------------------------------------- due dates --

var weekdays = map[string]time.Weekday{
	"sunday": time.Sunday, "monday": time.Monday, "tuesday": time.Tuesday,
	"wednesday": time.Wednesday, "thursday": time.Thursday, "friday": time.Friday,
	"saturday": time.Saturday,
	"dimanche": time.Sunday, "lundi": time.Monday, "mardi": time.Tuesday,
	"mercredi": time.Wednesday, "jeudi": time.Thursday, "vendredi": time.Friday,
	"samedi": time.Saturday,
}

var (
	inN     = regexp.MustCompile(`\bin\s+(\d+)\s+(hour|day|week)s?\b`)
	dansN   = regexp.MustCompile(`\bdans\s+(\d+)\s+(heure|jour|semaine)s?\b`)
	weekday = regexp.MustCompile(`\b(sunday|monday|tuesday|wednesday|thursday|friday|saturday|dimanche|lundi|mardi|mercredi|jeudi|vendredi|samedi)\b`)
)

// EndOfDayHour is when "end of day" and a bare weekday fall due. 17:00 in the
// utterance's own location, which is the clock the speaker meant.
const EndOfDayHour = 17

// ParseDue resolves a due date from a sentence, relative to when it was said.
//
// It returns the zero time when the sentence named no date, and that zero is
// load-bearing: a commitment with no date is a commitment with no date, and
// defaulting it to "today" would put a deadline on somebody's word that they
// never gave.
//
// Everything it understands is listed here. The list is short on purpose —
// SYSTEM.md's rule against parsing prose applies to dates as much as to
// terminal output, and a date parser that is clever is a date parser that is
// confidently wrong on the day it matters.
func ParseDue(sentence string, said time.Time) time.Time {
	if said.IsZero() {
		return time.Time{}
	}
	lower := strings.ToLower(sentence)
	day := time.Date(said.Year(), said.Month(), said.Day(), EndOfDayHour, 0, 0, 0, said.Location())

	switch {
	case strings.Contains(lower, "tomorrow"), strings.Contains(lower, "demain"):
		return day.AddDate(0, 0, 1)
	case strings.Contains(lower, "tonight"), strings.Contains(lower, "ce soir"),
		strings.Contains(lower, "end of day"), strings.Contains(lower, "eod"),
		strings.Contains(lower, "today"), strings.Contains(lower, "aujourd'hui"):
		return day
	case strings.Contains(lower, "end of the week"), strings.Contains(lower, "fin de semaine"):
		return nextWeekday(day, time.Friday)
	case strings.Contains(lower, "next week"), strings.Contains(lower, "la semaine prochaine"):
		return day.AddDate(0, 0, 7)
	case strings.Contains(lower, "next month"), strings.Contains(lower, "le mois prochain"):
		return day.AddDate(0, 1, 0)
	}

	if m := inN.FindStringSubmatch(lower); m != nil {
		return addUnits(said, day, m[1], m[2])
	}
	if m := dansN.FindStringSubmatch(lower); m != nil {
		unit := map[string]string{"heure": "hour", "jour": "day", "semaine": "week"}[m[2]]
		return addUnits(said, day, m[1], unit)
	}
	if m := weekday.FindStringSubmatch(lower); m != nil {
		return nextWeekday(day, weekdays[m[1]])
	}
	return time.Time{}
}

func addUnits(said, day time.Time, nStr, unit string) time.Time {
	n, err := strconv.Atoi(nStr)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	switch unit {
	case "hour":
		// Hours are relative to the moment, not to the end of the day: "in two
		// hours" said at nine means eleven.
		return said.Add(time.Duration(n) * time.Hour)
	case "day":
		return day.AddDate(0, 0, n)
	case "week":
		return day.AddDate(0, 0, 7*n)
	}
	return time.Time{}
}

// nextWeekday is the next occurrence of a weekday, where "Friday" said on a
// Friday means the Friday coming rather than today. Somebody who means today
// says today.
func nextWeekday(day time.Time, want time.Weekday) time.Time {
	delta := (int(want) - int(day.Weekday()) + 7) % 7
	if delta == 0 {
		delta = 7
	}
	return day.AddDate(0, 0, delta)
}

// ------------------------------------------------------------------- notes --

var (
	// identifier is MEMORY.md §3's "names something" test, reused: a compound
	// identifier, a token mixing letters and digits, or a shouted acronym. The
	// same three mechanical tests, because the same property — this sentence
	// refers to a specific thing — is what makes a note worth keeping.
	identifier = regexp.MustCompile(`[A-Za-z][A-Za-z0-9]*[_./-][A-Za-z0-9_./-]*[A-Za-z0-9]|\b[A-Za-z]+\d[A-Za-z0-9]*\b|\b[A-Z]{3,}\b`)
	number     = regexp.MustCompile(`\b\d+(\.\d+)?\s*(%|k|m|ms|s|v|ma|mm|mhz|khz|gb|mb|kb|hours?|days?|dollars?)?\b`)
	properName = regexp.MustCompile(`\S\s+([A-Z][\p{L}'-]{2,})`)
)

// minNoteChars keeps "yeah, sure" out of a digest.
const minNoteChars = 20

// maxNoteChars is what a note may be before it stops being a note. It is not a
// speech budget — ADAPTERS.md §6 owns that, and a digest is read as often as it
// is heard — but a paragraph in a bullet list is not a bullet.
const maxNoteChars = 240

// salient reports whether a sentence is worth keeping as a note.
//
// It keeps sentences that *name something*: an identifier, a measurement, or a
// proper noun that is not just the start of the sentence. A day's transcript is
// mostly conversational filler, and the difference between a useful digest and
// an unread one is whether it is filler.
func salient(sentence string) (string, bool) {
	s := strings.TrimSpace(sentence)
	if len(s) < minNoteChars {
		return "", false
	}
	if len(s) > maxNoteChars {
		s = strings.TrimSpace(s[:maxNoteChars]) + "…"
	}
	if identifier.MatchString(s) || properName.MatchString(s) {
		return s, true
	}
	if number.MatchString(s) && strings.ContainsAny(s, "0123456789") {
		return s, true
	}
	return "", false
}

// CommitmentID is deterministic, so re-extracting a day updates rows rather
// than accumulating copies. store.PutCommitment upserts on the id, and that only
// helps if the id is stable across runs.
func CommitmentID(episodeID string, c Commitment) string {
	h := sha256.Sum256([]byte(strings.Join([]string{
		"commitment", episodeID, c.Text,
		strconv.FormatInt(c.At.UTC().UnixMilli(), 10),
	}, "\x00")))
	return hex.EncodeToString(h[:16])
}
