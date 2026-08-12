package capture

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Consent, server side.
//
// ARCHITECTURE.md §6: two-party consent jurisdictions include Quebec — the
// user's home market — as well as California, Illinois, Washington,
// Pennsylvania, Massachusetts and Florida. Recording a conversation without all
// parties' consent is a legal problem, not a design preference. The two
// architectural requirements that section states are a bystander-visible
// recording indication and **capture defaults to off in a new location or with
// new voices present, until confirmed**.
//
// The phone already implements this: `ConsentPolicy` is a pure function and
// `ConsentGate` holds the signals and produces the one boolean the capture path
// reads. This is the same machine on the box, and it exists for three reasons
// the phone's copy cannot cover:
//
//   - **The box owns two of the three signals.** The phone's own header says so:
//     place familiarity and speaker diarisation happen here, not on the handset.
//     A gate that lives only on the phone is a gate whose two hardest inputs are
//     permanently [FamiliarityUnknown].
//   - **The server must refuse, not filter.** A daemon that accepts audio and
//     drops it afterwards has already received the recording. The refusal has to
//     happen before the bytes are stored, and it has to say why.
//   - **Bulk audio is judged against the past.** A night's sync arrives hours
//     after it was recorded, so "may we ingest this" is a question about the
//     consent that was in force *then*. [Gate.AllowedAt] answers it from a
//     history, and audio older than any recorded consent is refused.

// Scope is what the user granted and for how long. Mirrors
// `ConsentPolicy.Scope` in apps/android/relay-bridge.
type Scope string

const (
	// ScopeNone is the default for a fresh install: nothing.
	ScopeNone Scope = "none"
	// ScopeSession covers this conversation only and lapses when capture stops.
	// It exists because "record this meeting" is a real request that must not
	// silently become "record everything from now on".
	ScopeSession Scope = "session"
	// ScopeFamiliarPlaces covers everywhere the user has already confirmed.
	ScopeFamiliarPlaces Scope = "familiar_places"
	// ScopeAlways covers everything, everywhere. Deliberate, revocable in one
	// tap, and still not consent from the people in the room.
	ScopeAlways Scope = "always"
)

// Valid reports whether s is one of the four scopes. An unknown scope from the
// wire is refused rather than treated as the nearest match: guessing upward
// grants capture nobody asked for, and guessing downward hides a bug.
func (s Scope) Valid() bool {
	switch s {
	case ScopeNone, ScopeSession, ScopeFamiliarPlaces, ScopeAlways:
		return true
	}
	return false
}

// Familiarity is what is known about a place or the voices in it.
type Familiarity string

const (
	// FamiliarityKnown: seen before and confirmed by the user.
	FamiliarityKnown Familiarity = "known"
	// FamiliarityNew: new. Ask.
	FamiliarityNew Familiarity = "new"
	// FamiliarityUnknown: no signal available. It asks, because defaulting an
	// unknown to "allow" would make the whole rule decorative.
	FamiliarityUnknown Familiarity = ""
)

// Signals are the inputs to a decision.
type Signals struct {
	Place            Familiarity
	UnfamiliarVoices Familiarity
	// UserInitiated is the wearer deliberately starting this — a tap, the wake
	// word, or "start recording" in the app.
	UserInitiated bool
	// TwoPartyJurisdiction is true where all parties must consent. Quebec is
	// one, and it is the default.
	TwoPartyJurisdiction bool
}

// Decision is the policy's answer. Confirm means capture stays **off** until a
// human answers — it is not a banner over a running recording.
type Decision struct {
	// Allow is the only field the capture path may branch on.
	Allow bool
	// Question is what to put in front of the user, or empty.
	Question string
	// Why is the reason, in words a notification can show verbatim.
	Why string
}

// Confirming reports whether a person has to answer before capture may run.
func (d Decision) Confirming() bool { return !d.Allow && d.Question != "" }

// Decide is `ConsentPolicy.decide`, ported. It is a pure function on purpose:
// the state lives in [Gate], and a policy with no state is a policy that can be
// exhaustively tested.
func Decide(scope Scope, s Signals) Decision {
	switch scope {
	case ScopeSession:
		if s.UserInitiated {
			return Decision{Allow: true, Why: "the user started this session"}
		}
		return Decision{
			Question: "Start recording?",
			Why:      "session consent covers one conversation and this is a new one",
		}

	case ScopeFamiliarPlaces:
		switch {
		case s.Place == FamiliarityNew:
			return Decision{
				Question: "You are somewhere new. Record here?",
				Why:      "capture defaults to off in a new location — ARCHITECTURE.md §6",
			}
		case s.Place == FamiliarityUnknown && !s.UserInitiated:
			return Decision{
				Question: "Record here?",
				Why:      "the box has not confirmed this place yet, and an unknown place is treated as new",
			}
		case s.UnfamiliarVoices == FamiliarityNew && s.TwoPartyJurisdiction:
			return Decision{
				Question: "Someone new is here. Keep recording?",
				Why:      "all parties must consent in this jurisdiction",
			}
		}
		return Decision{Allow: true, Why: "a confirmed place with no new voices"}

	case ScopeAlways:
		if s.UnfamiliarVoices == FamiliarityNew && s.TwoPartyJurisdiction {
			// "Always" is the wearer's consent. It is not the other person's,
			// and it cannot be.
			return Decision{
				Question: "Someone new is here. Keep recording?",
				Why:      "always-on covers you, not the people around you",
			}
		}
		return Decision{Allow: true, Why: "always-on capture is enabled"}
	}

	return Decision{Why: "capture has not been turned on"}
}

// IndicatorRequired reports whether a bystander-visible recording indicator
// must be showing.
//
// Always true, and a function rather than a constant so the day someone wants
// an exception they have to add the branch and justify it in a diff. There is
// no configuration that turns this off, on the phone or here.
func IndicatorRequired() bool { return true }

// indicatorMissing is the refusal when nothing can show that recording is
// happening. Same sentence the Android gate uses, so the two surfaces do not
// explain the same refusal two different ways.
const indicatorMissing = "no recording indicator can be shown, and ARCHITECTURE.md §6 makes one a requirement"

// ErrConsent is every refusal this file produces. It wraps a [Decision] so a
// caller can render the question rather than only the failure.
type ErrConsent struct {
	Device   string
	Decision Decision
	// At is the moment the audio was captured, which for bulk sync is hours
	// before the moment it was offered.
	At time.Time
}

func (e *ErrConsent) Error() string {
	return fmt.Sprintf("capture: consent does not cover this (device %s): %s", e.Device, e.Decision.Why)
}

// ErrNoConsent is the sentinel behind every [ErrConsent], so callers can test
// for a refusal without type-asserting.
var ErrNoConsent = errors.New("capture: consent does not cover this")

func (e *ErrConsent) Is(target error) bool { return target == ErrNoConsent }

// record is one interval of consent state, closed by the next one.
type record struct {
	At       time.Time
	Decision Decision
	Scope    Scope
	Signals  Signals
}

// Gate is [Decide] with a caller: it holds the signals, remembers what was
// answered, and keeps the history that [Gate.AllowedAt] reads.
//
// One gate per device. Safe for concurrent use — the WebSocket reader, the bulk
// uploader and the diariser all touch it.
type Gate struct {
	now func() time.Time

	mu                   sync.Mutex
	scope                Scope
	confirmedPlaces      map[string]bool
	placeID              string
	placeKnown           bool // whether placeID was ever set
	voices               Familiarity
	userInitiated        bool
	confirmedHere        bool
	declined             bool
	indicator            bool
	twoPartyJurisdiction bool
	history              []record
}

// GateOptions configures a [Gate].
type GateOptions struct {
	// Scope is what the user has already granted, restored across a restart.
	Scope Scope
	// ConfirmedPlaces is what they have already said yes to. Confirming a place
	// has to outlive the process, or the user is asked again every morning and
	// a prompt people see daily is a prompt they dismiss without reading.
	ConfirmedPlaces []string
	// IndicatorVisible is whether the phone reports that it can show a
	// recording indicator. No default on purpose — see [IndicatorRequired].
	IndicatorVisible bool
	// TwoPartyJurisdiction defaults to true. Quebec is the home market.
	TwoPartyJurisdiction *bool
	// Since is when the restored Scope was granted. It defaults to Now, and it
	// is persisted alongside the scope for a reason worth stating: a gate that
	// forgets when consent started refuses every night's audio after a restart,
	// because [Gate.AllowedAt] judges a recording against the consent in force
	// when it happened and a daemon restarted at 03:00 would have no consent on
	// record for 14:00. That is the same outcome as never having consented, and
	// it would look like a sync bug rather than a consent bug.
	Since time.Time
	// Now is the clock. Tests inject one; nothing here reads the wall clock
	// directly.
	Now func() time.Time
}

// NewGate builds a gate and records its opening verdict, so a question asked
// about a moment before anything happened has an answer.
func NewGate(o GateOptions) *Gate {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	twoParty := true
	if o.TwoPartyJurisdiction != nil {
		twoParty = *o.TwoPartyJurisdiction
	}
	scope := o.Scope
	if !scope.Valid() {
		scope = ScopeNone
	}
	g := &Gate{
		now:                  now,
		scope:                scope,
		confirmedPlaces:      map[string]bool{},
		voices:               FamiliarityUnknown,
		indicator:            o.IndicatorVisible,
		twoPartyJurisdiction: twoParty,
	}
	for _, p := range o.ConfirmedPlaces {
		g.confirmedPlaces[p] = true
	}
	g.publishAtLocked(o.Since)
	return g
}

// Grant records the user choosing a scope — onboarding, or the settings screen.
//
// Widening the scope clears a previous refusal, because the refusal answered a
// question asked under the old one. An unknown scope is refused.
func (g *Gate) Grant(s Scope) error {
	if !s.Valid() {
		return fmt.Errorf("capture: unknown consent scope %q", s)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.scope == s {
		return nil
	}
	g.scope = s
	g.declined = false
	g.publishLocked()
	return nil
}

// Revoke withdraws consent and forgets the confirmed places.
//
// Keeping them would mean re-granting consent silently restores every "yes" the
// user has ever given, which is not what withdrawing means.
func (g *Gate) Revoke() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.scope = ScopeNone
	g.confirmedPlaces = map[string]bool{}
	g.placeID, g.placeKnown = "", false
	g.voices = FamiliarityUnknown
	g.userInitiated = false
	g.confirmedHere = false
	g.declined = false
	g.publishLocked()
}

// SetIndicatorVisible records whether the phone can show a recording indicator.
func (g *Gate) SetIndicatorVisible(v bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.indicator == v {
		return
	}
	g.indicator = v
	g.publishLocked()
}

// EnterPlace records where the wearer is. The empty string means unknown.
//
// Everything transient resets: a new place is a new conversation, so a "start
// capture" at the last one does not carry, and the voices are unknown again
// until diarisation says otherwise.
func (g *Gate) EnterPlace(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.placeKnown && g.placeID == id {
		return
	}
	g.placeID, g.placeKnown = id, id != ""
	g.voices = FamiliarityUnknown
	g.userInitiated = false
	g.confirmedHere = false
	g.declined = false
	g.publishLocked()
}

// ObserveVoices records the diariser's verdict on the voices it can hear. This
// is the signal the phone cannot produce.
func (g *Gate) ObserveVoices(f Familiarity) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.voices == f {
		return
	}
	g.voices = f
	// A refusal is not cleared here. "No" meant no, and a voice leaving the
	// room is not the user changing their mind.
	g.publishLocked()
}

// StartSession records the wearer deliberately starting capture.
func (g *Gate) StartSession() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.userInitiated && !g.declined {
		return
	}
	g.userInitiated = true
	g.declined = false
	g.publishLocked()
}

// EndSession records capture stopping. Session consent lapses with it, by
// definition.
func (g *Gate) EndSession() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.userInitiated && !g.confirmedHere {
		return
	}
	g.userInitiated = false
	g.confirmedHere = false
	g.publishLocked()
}

// Answer records the user's reply to [Decision.Question].
//
// A "yes" confirms this place, this conversation and the voices currently in it
// — and nothing beyond them. It does not widen the scope: saying yes in a
// meeting must not turn on always-on capture.
func (g *Gate) Answer(approve bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !approve {
		g.declined = true
		g.publishLocked()
		return
	}
	g.declined = false
	g.userInitiated = true
	g.confirmedHere = true
	if g.voices == FamiliarityNew {
		g.voices = FamiliarityKnown
	}
	if g.placeID != "" {
		g.confirmedPlaces[g.placeID] = true
	}
	g.publishLocked()
}

// Verdict is the decision in force right now.
func (g *Gate) Verdict() Decision {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.history[len(g.history)-1].Decision
}

// Scope is the granted scope, for persistence across a restart.
func (g *Gate) Scope() Scope {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.scope
}

// ConfirmedPlaces is the set to persist across a restart, sorted.
func (g *Gate) ConfirmedPlaces() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.confirmedPlaces))
	for p := range g.confirmedPlaces {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// AllowedAt reports the decision that was in force at t.
//
// This is the question bulk sync asks. A night's audio is offered hours after
// it was recorded, and the only honest test is whether consent covered the
// moment of *recording*. Audio from before any recorded consent is refused,
// with a decision that says so — not treated as covered by whatever was granted
// afterwards, which would retroactively consent to a recording nobody agreed to.
func (g *Gate) AllowedAt(t time.Time) Decision {
	g.mu.Lock()
	defer g.mu.Unlock()

	if t.IsZero() {
		return Decision{Why: "this audio carries no capture time, so no consent can be matched to it"}
	}
	first := g.history[0]
	if t.Before(first.At) {
		return Decision{Why: "no consent was on record when this audio was captured"}
	}
	// The history is short — a handful of transitions a day — so a linear scan
	// backwards is both the simplest and the fastest thing here.
	for i := len(g.history) - 1; i >= 0; i-- {
		if !g.history[i].At.After(t) {
			return g.history[i].Decision
		}
	}
	return first.Decision
}

// History is every consent transition, oldest first. The console renders it;
// "when did this box think it was allowed to record" should be answerable.
func (g *Gate) History() []Record {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Record, 0, len(g.history))
	for _, r := range g.history {
		out = append(out, Record{At: r.At, Scope: r.Scope, Decision: r.Decision, Signals: r.Signals})
	}
	return out
}

// Record is one consent transition, for display and for audit.
type Record struct {
	At       time.Time
	Scope    Scope
	Decision Decision
	Signals  Signals
}

func (g *Gate) publishLocked() { g.publishAtLocked(time.Time{}) }

// publishAtLocked appends a transition, stamped at `when` if it is set and at
// the clock otherwise. Only the opening record uses `when`, and only so a
// restored grant keeps the date it was actually made.
func (g *Gate) publishAtLocked(when time.Time) {
	d := g.evaluateLocked()
	sig := g.signalsLocked()
	if n := len(g.history); n > 0 {
		last := g.history[n-1]
		if last.Decision == d && last.Scope == g.scope && last.Signals == sig {
			return
		}
	}
	stamp := when
	if stamp.IsZero() {
		stamp = g.now()
	}
	g.history = append(g.history, record{At: stamp, Decision: d, Scope: g.scope, Signals: sig})
}

func (g *Gate) signalsLocked() Signals {
	return Signals{
		Place:                g.placeFamiliarityLocked(),
		UnfamiliarVoices:     g.voices,
		UserInitiated:        g.userInitiated,
		TwoPartyJurisdiction: g.twoPartyJurisdiction,
	}
}

func (g *Gate) placeFamiliarityLocked() Familiarity {
	switch {
	case g.confirmedHere:
		return FamiliarityKnown
	case g.placeID == "":
		return FamiliarityUnknown
	case g.confirmedPlaces[g.placeID]:
		return FamiliarityKnown
	}
	return FamiliarityNew
}

func (g *Gate) evaluateLocked() Decision {
	d := Decide(g.scope, g.signalsLocked())

	// The indicator check runs last and can only ever subtract. Putting it
	// before the policy would let a missing indicator masquerade as a consent
	// question, and the user would answer the wrong problem.
	if IndicatorRequired() && !g.indicator {
		return Decision{Why: indicatorMissing}
	}
	if g.declined {
		return Decision{Why: "the user said no to recording here"}
	}
	return d
}

// Registry is one [Gate] per device.
//
// SYSTEM.md §5 has no user table — one box, one person — but a person has more
// than one device, and consent granted on a phone is not consent granted by a
// laptop that has never been paired.
type Registry struct {
	opts GateOptions

	mu    sync.Mutex
	gates map[string]*Gate
}

// NewRegistry builds a registry whose gates start from opts.
//
// A gate is created on first contact with a device, and it inherits
// [GateOptions.Since] rather than being stamped at the moment of contact. The
// difference shows up exactly once and it matters: the nightly sync is the first
// thing a restarted daemon hears from a phone, and it is carrying audio from
// fourteen hours earlier.
func NewRegistry(o GateOptions) *Registry {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Since.IsZero() {
		o.Since = o.Now()
	}
	return &Registry{opts: o, gates: map[string]*Gate{}}
}

// Gate returns the gate for a device, creating it on first use. A device that
// has never granted anything gets [ScopeNone], which refuses.
func (r *Registry) Gate(device string) *Gate {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.gates[device]
	if !ok {
		g = NewGate(r.opts)
		r.gates[device] = g
	}
	return g
}

// Devices lists every device with a gate, sorted.
func (r *Registry) Devices() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.gates))
	for d := range r.gates {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// Check is the one call the ingest paths make: may this device's audio, taken
// at this moment, be stored?
func (r *Registry) Check(device string, at time.Time) error {
	d := r.Gate(device).AllowedAt(at)
	if d.Allow {
		return nil
	}
	return &ErrConsent{Device: device, Decision: d, At: at}
}
