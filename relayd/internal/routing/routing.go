package routing

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/store"
)

// Request is one utterance and whatever context arrived with it.
//
// Text is the only required field. Everything else is a hint the phone may not
// have: the glasses know nothing about the user's shell, so Workspace is empty
// far more often than it is set, and a router that requires it does not work at
// the glasses at all.
type Request struct {
	Text string
	At   time.Time

	// Workspace is where the user is, when something knows — the console, or a
	// phone paired to a laptop. Empty is normal.
	Workspace string

	// Confidence is the recogniser's, 0 when unreported. A low-confidence
	// utterance is never used to *switch* sessions silently: mishearing "talk
	// to the refactor one" is a wrong continue with extra steps.
	Confidence float64

	// Source is "glasses" or "phone".
	Source string
}

// MinSwitchConfidence is how sure the recogniser has to be before a spoken
// reference moves the conversation to another session on its own. Below it the
// router still resolves the reference but asks for confirmation, because the
// cost of acting on a misheard session name is the wrong-continue failure this
// package exists to avoid. Zero means unreported, which is not the same as low
// and is not penalised.
const MinSwitchConfidence = 0.55

// Kind is what the router decided to do with an utterance.
type Kind string

const (
	// KindContinue sends the turn to a session that already exists.
	KindContinue Kind = "continue"
	// KindNew starts a session. Runtime is set, and how it was chosen is in
	// RuntimeChoice.
	KindNew Kind = "new"
	// KindAsk is the honest outcome when the router is not sure. It is not an
	// error: ORCHESTRATOR.md §4 prefers a router that asks to one that is right
	// 80% of the time and silent about it.
	KindAsk Kind = "ask"
	// KindControl is an utterance that is about the sessions rather than for
	// one of them — stop, undo, list, an answer to a blocked question.
	KindControl Kind = "control"
)

// Reason is why the router decided what it decided. It is spoken material as
// well as a debugging aid: the console shows it next to the announcement so a
// user who disagrees can see what the router thought it knew.
type Reason string

const (
	// ReasonExplicit is the escape hatch: the user said which session.
	ReasonExplicit Reason = "you said so"
	// ReasonFocus is the session the conversation is already in. This is the
	// manual path's normal answer and it is deliberately sticky — a terminal
	// does not re-derive which window you are typing into on every keystroke.
	ReasonFocus Reason = "it is the session you are in"
	// ReasonOnlyLive is the single live session. With one session there is no
	// routing decision to make; the announcement still names it so a wrong
	// guess is caught by the human.
	ReasonOnlyLive Reason = "it is the only session running"
	// ReasonNothingLive is a new session because there is nothing to continue.
	ReasonNothingLive Reason = "nothing is running"
	// ReasonAmbiguous is why an ask is an ask.
	ReasonAmbiguous Reason = "more than one session could take this"
	// ReasonAutomatic is the scorer, which is off unless Options.Auto is set.
	ReasonAutomatic Reason = "recency, repo and subject match"
	// ReasonTieBreak is the LLM tie-break, which only runs when the scorer is
	// on and two candidates are too close to separate.
	ReasonTieBreak Reason = "a tie-break between two close matches"
	// ReasonControl is a control verb.
	ReasonControl Reason = "a control command"
	// ReasonNewTopic is the scorer deciding nothing on the list is close
	// enough, which is a new session rather than a bad continue.
	ReasonNewTopic Reason = "nothing running looks related"
)

// Candidate is one session the router considered, with its score.
type Candidate struct {
	Session SessionView
	Score   float64
	// Why names the strongest signal, for the ask: "same repo", "you were just
	// in it".
	Why string
}

// Decision is the router's answer. It is data, and the caller acts on it: this
// package decides and announces, it does not drive runtimes.
//
// Announcement is filled for every kind and is spoken **before** the action,
// which is ORCHESTRATOR.md §4's rule 2 and the only thing that turns a wrong
// guess into a correction instead of a discovery.
type Decision struct {
	Kind Kind

	// Session is set for KindContinue, and for a KindControl aimed at one
	// session.
	Session string
	// Runtime is set for KindNew.
	Runtime adapter.Runtime
	// Subject is what the new session is for, in the user's words. Empty is
	// allowed and produces a session named by id rather than an invented topic.
	Subject   string
	Workspace string

	// Text is what to send, which is the utterance minus any routing prefix:
	// "new session — run the tests" sends "run the tests".
	Text string

	Reason  Reason
	Because string
	// Announcement is the one clause spoken before acting.
	Announcement string
	// Question is set on KindAsk and is what gets asked out loud.
	Question string

	// Automatic is true when the scorer picked this rather than the manual
	// path. The console shows it, and Undo exists mostly for these.
	Automatic  bool
	Confidence float64

	Candidates []Candidate
	Command    *Command
	// RuntimeChoice is question 2's answer, present on KindNew.
	RuntimeChoice *RuntimeChoice
}

// Undoable reports whether this decision is one Undo can move. A control verb
// is not: "stop" did not put a turn anywhere.
func (d Decision) Undoable() bool { return d.Kind == KindContinue || d.Kind == KindNew }

// SessionView is one session, as much of it as routing needs to see.
//
// It is this package's own type rather than store.Session because routing wants
// two things the registry row does not carry — the files a session has touched,
// and its capability descriptor — and because a router that can only be tested
// against a live SQLite database is a router nobody tests.
type SessionView struct {
	ID        string
	Runtime   adapter.Runtime
	Subject   string
	Workspace string
	// Entities are the things this session is about: repo names, services, the
	// nouns the user says out loud.
	Entities []string
	// Files are paths this session has touched, most recent first. Optional:
	// FromRegistry fills it from the tool calls, and a caller that has none
	// leaves it empty rather than inventing one.
	Files []string

	LastActive time.Time
	State      store.SessionState
	// Turn is the id of the turn running right now, "" when idle.
	Turn         string
	Capabilities adapter.Capabilities
}

// Live reports whether this session can still take a turn.
func (v SessionView) Live() bool { return v.State != store.SessionClosed && v.State != "" }

// Busy reports whether a turn is running.
func (v SessionView) Busy() bool { return v.State == store.SessionRunning || v.Turn != "" }

// Awaiting reports whether the session is blocked on a human.
func (v SessionView) Awaiting() bool { return v.State == store.SessionAwaiting }

// Steerable reports whether an utterance can be pushed into the running turn.
// Three of the five runtimes cannot (ADAPTERS.md §4), and the answer comes from
// the session's capability descriptor rather than from a table of runtime names
// — a Claude Code session can lose a capability at handshake time.
func (v SessionView) Steerable() bool { return v.Capabilities.Has(adapter.CapSteer) }

// Name is what this session is called out loud. A session with no subject is
// named by runtime and a short id rather than by an invented topic, which is
// the same rule the pinger uses.
func (v SessionView) Name() string {
	if s := strings.TrimSpace(v.Subject); s != "" {
		return s
	}
	short := v.ID
	if len(short) > 6 {
		short = short[:6]
	}
	if v.Runtime != "" {
		return RuntimeLabel(v.Runtime) + " session " + short
	}
	return "session " + short
}

// terms is everything about this session a spoken reference could match.
func (v SessionView) terms() []string {
	var out []string
	out = append(out, tokens(v.Subject)...)
	for _, e := range v.Entities {
		out = append(out, tokens(e)...)
	}
	if base := baseName(v.Workspace); base != "" {
		out = append(out, tokens(base)...)
	}
	for _, f := range v.Files {
		out = append(out, tokens(baseName(f))...)
	}
	return out
}

// Sessions is the live list, as the router reads it.
type Sessions interface {
	Live(ctx context.Context) ([]SessionView, error)
}

// StaticSessions is a fixed list, for tests and for a caller that already has
// one.
type StaticSessions []SessionView

func (s StaticSessions) Live(context.Context) ([]SessionView, error) {
	out := make([]SessionView, 0, len(s))
	for _, v := range s {
		if v.Live() {
			out = append(out, v)
		}
	}
	return out, nil
}

// SessionsFunc adapts a function to [Sessions].
type SessionsFunc func(context.Context) ([]SessionView, error)

func (f SessionsFunc) Live(c context.Context) ([]SessionView, error) { return f(c) }

// ------------------------------------------------------------------ text --

// normalize lowercases, strips punctuation that speech recognisers add, and
// collapses whitespace. It is the only text processing in this package that
// runs before matching, and it is deliberately dumb: this is a closed command
// grammar, not prose parsing.
func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '/' || r == '.':
			// Kept *inside* a word, so "internal/routing" and "file.go" survive
			// as one token. Trimmed at the edges below, so the full stop at the
			// end of "New session." does not stop it matching the grammar —
			// which is the whole difference between an escape hatch that works
			// and one that works when you speak without punctuation.
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	fields := strings.Fields(b.String())
	out := fields[:0]
	for _, f := range fields {
		if f = strings.Trim(f, "-_/."); f != "" {
			out = append(out, f)
		}
	}
	return strings.Join(out, " ")
}

// stopwords are words that carry no matching signal. The list is short on
// purpose — every word removed here is a word a session name cannot be matched
// on, and "the api one" has to still find the api session.
var stopwords = map[string]bool{
	"a": true, "an": true, "the": true, "to": true, "in": true, "on": true,
	"for": true, "of": true, "my": true, "that": true, "this": true,
	"one": true, "session": true, "please": true, "it": true, "and": true,
	"with": true, "about": true, "into": true, "at": true, "is": true,
}

// tokens splits normalized text into matchable words, dropping stopwords and
// splitting identifier separators so "payments-refactor" matches "payments".
func tokens(s string) []string {
	var out []string
	for _, f := range strings.Fields(normalize(s)) {
		for _, part := range strings.FieldsFunc(f, func(r rune) bool {
			return r == '-' || r == '_' || r == '/' || r == '.'
		}) {
			if part == "" || stopwords[part] {
				continue
			}
			out = append(out, part)
		}
	}
	return out
}

// baseName is the last path element, which is what a repo is called out loud.
func baseName(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// overlap is the fraction of want's tokens that appear in have. It is
// asymmetric on purpose: a two-word reference matching two of a session's
// fifteen entities is a good match, not a 13% one.
func overlap(want, have []string) float64 {
	if len(want) == 0 {
		return 0
	}
	set := make(map[string]bool, len(have))
	for _, h := range have {
		set[h] = true
	}
	var hit int
	for _, w := range want {
		if set[w] {
			hit++
		}
	}
	return float64(hit) / float64(len(want))
}

// agree is the symmetric form: how much two sets of words agree, measured
// against whichever is smaller.
//
// It exists because the interesting case is directional in the other direction
// from what you first reach for. "The payments refactor needs a test" contains
// the whole of a session called "the payments refactor" — that is a very strong
// signal — but only two of its own five words are in that subject, so the
// one-way measure scores it 0.4 and buries it under a session that is merely
// recent. Recency outranking a named subject is a wrong continue waiting to
// happen.
func agree(a, b []string) float64 {
	return math.Max(overlap(a, b), overlap(b, a))
}

// sortCandidates orders by score descending, then by recency, then by id so the
// order is stable across runs. A router whose answer depends on map iteration
// order is a router that cannot be tested.
func sortCandidates(cs []Candidate) {
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].Score != cs[j].Score {
			return cs[i].Score > cs[j].Score
		}
		if !cs[i].Session.LastActive.Equal(cs[j].Session.LastActive) {
			return cs[i].Session.LastActive.After(cs[j].Session.LastActive)
		}
		return cs[i].Session.ID < cs[j].Session.ID
	})
}
