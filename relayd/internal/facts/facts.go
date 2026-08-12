// Package facts is MEMORY.md §11 step 5b: the distilled tier.
//
// "prefers Supabase over Firebase", "deploys on Vercel", "writes Go for
// daemons, TypeScript for anything with a UI". §5 calls this what makes the
// product feel like it knows you, and — in the same breath — the layer most
// likely to be confidently wrong. Its five rules are the whole design of this
// package, and each one is a behaviour here rather than a principle in a
// comment:
//
// **Every fact carries evidence.** [Observation] cannot be reconciled without
// at least one [Evidence] that names a runtime, a session and a date, and there
// is no exported call in this package that writes a fact row any other way.
// [Store.Sweep] then deletes — not downgrades — any fact whose evidence has
// gone, because §5 says a fact that cannot point at where it came from is
// deleted rather than kept at low confidence.
//
// **Facts decay on last observation, not creation.** [Fact.Strength] is the
// confidence halved every [DefaultHalfLife] since `last_seen`, and `last_seen`
// comes from the *evidence dates*, never from the clock — so backfilling a 2024
// session writes a 2024 fact that is already weak, rather than a fresh one.
//
// **Contradictions replace, they do not accumulate.** [Store.Reconcile]
// supersedes an old fact when the new evidence *names* it — either through
// [Observation.Replaces] or by mentioning the old object in the new sentence.
// It never supersedes on a guess: deciding that two objects are alternatives
// because they share a predicate is how a true fact gets deleted. Silence is
// handled by decay instead, which is the honest division of labour between the
// two rules.
//
// **Visible and editable**, which is DASHBOARD.md §3.3. The data model is what
// serves that screen: evidence with dates, a confidence, superseded facts kept
// with their date under a toggle, `edited_at` so a human correction is not
// quietly re-derived over the top, and a soft delete so a fact the user removed
// does not come back on the next turn.
//
// **Nothing in this tier is a secret.** Every string this package is about to
// write goes through internal/index's measured detector first, and an
// observation whose sentence contains a credential is rejected outright rather
// than stored with a marker in it. "Uses Stripe" is a fact; the key is
// internal/vault's problem.
package facts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"strings"
	"time"
	"unicode"
)

// Errors.
var (
	// ErrNoEvidence is the rule that cannot be bent: a fact that cannot point
	// at where it came from is deleted, not kept at low confidence.
	ErrNoEvidence = errors.New("facts: a fact with no evidence is not a fact")
	ErrNotFound   = errors.New("facts: no such fact")
	// ErrNoRedactor mirrors summarize.ErrNoRedactor. A fact store that will
	// happily write whatever it is handed looks exactly like a clean tier right
	// up until a key is on the review screen.
	ErrNoRedactor = errors.New("facts: no secret detector, and writing text without one is not allowed")
)

// Predicate is the small closed set the fact table documents. Keeping it closed
// is what makes contradiction detection possible at all: two facts can only
// disagree if they are answers to the same kind of question.
type Predicate string

const (
	// Prefers is a choice between alternatives — "prefers Supabase over
	// Firebase". The one predicate that routinely supersedes.
	Prefers Predicate = "prefers"
	// Uses is a service or tool in the stack — "uses Stripe for payments".
	// Multi-valued: using Stripe does not stop you using Twilio.
	Uses Predicate = "uses"
	// DeploysOn is where the work runs — "deploys on Vercel".
	DeploysOn Predicate = "deploys_on"
	// Writes is a language choice — "writes Go for daemons".
	Writes Predicate = "writes"
)

// Predicates lists every predicate this tier stores.
func Predicates() []Predicate { return []Predicate{Prefers, Uses, DeploysOn, Writes} }

// ParsePredicate normalises what a model returned. It accepts the spellings a
// small model actually produces — spaces for underscores, a trailing "s" or
// its absence — and refuses everything else, because an open predicate set
// turns the review screen into a list of one-off sentences nobody can act on.
func ParsePredicate(s string) (Predicate, bool) {
	k := strings.ToLower(strings.TrimSpace(s))
	k = strings.ReplaceAll(k, " ", "_")
	k = strings.ReplaceAll(k, "-", "_")
	switch k {
	case "prefers", "prefer", "preference", "prefers_over":
		return Prefers, true
	case "uses", "use", "used", "using":
		return Uses, true
	case "deploys_on", "deploy_on", "deploys", "deploy", "hosts_on", "hosted_on":
		return DeploysOn, true
	case "writes", "write", "writes_in", "codes_in", "programs_in":
		return Writes, true
	}
	return "", false
}

// DefaultSubject is who a fact is about. The tier is single-user today; the
// column exists so it does not have to be a migration when it is not.
const DefaultSubject = "user"

// Evidence is one place a fact was observed: a pointer into a transcript, with
// a date, never a copy of it.
//
// The date is not decoration. Decay runs on the newest evidence date, so
// evidence without one cannot be weighed and is refused.
type Evidence struct {
	Runtime    string
	SessionID  string
	Path       string
	ByteOffset int64
	Quote      string
	At         time.Time
}

// Valid reports what is wrong with a piece of evidence, or nil.
func (e Evidence) Valid() error {
	switch {
	case strings.TrimSpace(e.Runtime) == "":
		return errors.New("facts: evidence with no runtime cannot be reopened")
	case strings.TrimSpace(e.SessionID) == "":
		return errors.New("facts: evidence with no session cannot be reopened")
	case e.At.IsZero():
		return errors.New("facts: evidence with no date cannot be decayed")
	}
	return nil
}

// ID is this evidence's stable row id within a fact.
//
// Deterministic on purpose: MEMORY.md §4 re-runs extraction for a session on
// every TurnCompleted, so the same observation arrives again every few seconds.
// A random id would pile up a thousand copies of one citation and inflate the
// fact's confidence with them; this makes re-observing the same place a no-op.
func (e Evidence) ID(factID string) string {
	return digest(factID, e.Runtime, e.SessionID, itoa(e.ByteOffset))
}

// Observation is a candidate fact with the evidence for it. It is the only
// input to [Store.Reconcile], and it cannot express a fact without evidence.
type Observation struct {
	// Subject defaults to [DefaultSubject].
	Subject   string
	Predicate Predicate
	Object    string
	// Text is the sentence a human reads on DASHBOARD.md §3.3's screen.
	Text string
	// Confidence is what this observation alone is worth, 0–1.
	Confidence float64

	// Evidence is required. One entry is enough; zero is a rejection.
	Evidence []Evidence

	// Replaces names objects this observation contradicts — the extractor's way
	// of saying "they moved off Firebase". Supersession happens only when
	// something names the old fact, here or in Text.
	Replaces []string
}

// Fact is one stored, distilled claim.
type Fact struct {
	ID        string
	Subject   string
	Predicate Predicate
	Object    string
	Text      string

	// Confidence is the accumulated belief from the evidence, before decay.
	// [Fact.Strength] is the number to reason with.
	Confidence float64

	FirstSeen time.Time
	// LastSeen is the newest evidence date, and what decay runs on.
	LastSeen time.Time

	SupersededBy string
	SupersededAt time.Time
	EditedAt     time.Time
	DeletedAt    time.Time

	Evidence []Evidence
}

// Superseded reports whether a later fact replaced this one.
func (f Fact) Superseded() bool { return !f.SupersededAt.IsZero() }

// Edited reports whether a human corrected this fact. An edited fact is not
// re-derived over the top: the extractor may refresh its dates and evidence,
// never its words.
func (f Fact) Edited() bool { return !f.EditedAt.IsZero() }

// Deleted reports whether the user removed this fact.
func (f Fact) Deleted() bool { return !f.DeletedAt.IsZero() }

// Live reports whether this fact should be believed at all — not superseded,
// not deleted. Strength is what says how much.
func (f Fact) Live() bool { return !f.Superseded() && !f.Deleted() }

// DefaultHalfLife is how long an unrepeated fact takes to be worth half as
// much. Four months: long enough that a quarterly habit survives, short enough
// that §5's "a preference from 2024 that has not recurred" is near zero by the
// time it is asked about.
const DefaultHalfLife = 120 * 24 * time.Hour

// StaleBelow is the strength under which a fact stops being offered to routing.
// It is a floor, not a delete: the row keeps answering "you used to".
const StaleBelow = 0.05

// Strength is the confidence after decay at a moment. Decay is on last
// observation, not creation, so a long-held habit that still shows up stays
// strong and a one-off from two years ago does not.
func (f Fact) Strength(at time.Time) float64 {
	return Decay(f.Confidence, f.LastSeen, at, DefaultHalfLife)
}

// Decay halves conf for every halfLife between lastSeen and at.
func Decay(conf float64, lastSeen, at time.Time, halfLife time.Duration) float64 {
	if halfLife <= 0 {
		halfLife = DefaultHalfLife
	}
	if lastSeen.IsZero() || !at.After(lastSeen) {
		return clamp(conf)
	}
	periods := at.Sub(lastSeen).Seconds() / halfLife.Seconds()
	return clamp(conf * math.Exp2(-periods))
}

// combine folds a new observation's confidence into an existing belief.
//
// Noisy-OR: two independent sightings at 0.5 make 0.75, three make 0.875, and
// nothing ever reaches 1. The word "independent" is load-bearing and is
// enforced at the call site — [Store.Reconcile] only combines when the
// observation brought evidence the fact did not already have, so the live
// path's re-run on every TurnCompleted cannot talk itself into certainty from
// one session.
func combine(existing, incoming float64) float64 {
	e, i := clamp(existing), clamp(incoming)
	return clamp(1 - (1-e)*(1-i))
}

func clamp(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// FactID is the identity of a claim: subject, predicate and object.
//
// Deterministic, so re-observing the same claim finds the same row without a
// lookup and "the same fact twice" is not a state this tier can reach. It also
// means a fact the user deleted keeps its id, which is how [Store.Reconcile]
// knows not to resurrect it on the next turn.
func FactID(subject string, p Predicate, object string) string {
	return digest(normSubject(subject), string(p), normObject(object))
}

func normSubject(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return DefaultSubject
	}
	return s
}

// normObject folds case and punctuation so "Supabase", "supabase" and
// "Supabase." are one fact rather than three.
func normObject(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_' || r == '.' || r == '/':
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// mentions reports whether text names object as a whole word or phrase. It is
// the "the new evidence names the old fact" test, and it is deliberately dumb:
// a substring match would make "Fire" supersede "Firebase".
func mentions(text, object string) bool {
	obj := normObject(object)
	if obj == "" {
		return false
	}
	hay := " " + normObject(text) + " "
	return strings.Contains(hay, " "+obj+" ")
}

func digest(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:16])
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
