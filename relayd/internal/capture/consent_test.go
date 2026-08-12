package capture

import (
	"errors"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// A fake clock, because every consent question in this file is a question about
// *when*.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func TestDecideMatchesTheAndroidPolicy(t *testing.T) {
	// One row per branch of ConsentPolicy.decide in
	// apps/android/relay-bridge/.../ConsentPolicy.kt. The two copies have to
	// agree: a phone that thinks it may record and a box that refuses the audio
	// is a recording that happened and was thrown away, which is the worst of
	// both.
	tests := []struct {
		name    string
		scope   Scope
		signals Signals
		allow   bool
		ask     bool
	}{
		{"none refuses", ScopeNone, Signals{}, false, false},
		{"session without a start asks", ScopeSession, Signals{}, false, true},
		{"session the user started", ScopeSession, Signals{UserInitiated: true}, true, false},
		{"familiar places, new place asks", ScopeFamiliarPlaces,
			Signals{Place: FamiliarityNew, UserInitiated: true}, false, true},
		{"familiar places, unknown place asks", ScopeFamiliarPlaces,
			Signals{Place: FamiliarityUnknown}, false, true},
		{"familiar places, known place allows", ScopeFamiliarPlaces,
			Signals{Place: FamiliarityKnown}, true, false},
		{"familiar places, new voice asks in a two-party jurisdiction", ScopeFamiliarPlaces,
			Signals{Place: FamiliarityKnown, UnfamiliarVoices: FamiliarityNew, TwoPartyJurisdiction: true},
			false, true},
		{"familiar places, new voice allowed in a one-party jurisdiction", ScopeFamiliarPlaces,
			Signals{Place: FamiliarityKnown, UnfamiliarVoices: FamiliarityNew},
			true, false},
		{"always allows", ScopeAlways, Signals{}, true, false},
		{"always still asks about a new voice", ScopeAlways,
			Signals{UnfamiliarVoices: FamiliarityNew, TwoPartyJurisdiction: true}, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(tc.scope, tc.signals)
			if d.Allow != tc.allow {
				t.Fatalf("Allow = %v, want %v (why: %s)", d.Allow, tc.allow, d.Why)
			}
			if d.Confirming() != tc.ask {
				t.Fatalf("Confirming = %v, want %v (question: %q)", d.Confirming(), tc.ask, d.Question)
			}
			if d.Why == "" {
				t.Fatal("every decision has to carry a reason in words")
			}
		})
	}
}

func TestNoIndicatorMeansNoCapture(t *testing.T) {
	c := &clock{t: at("2026-08-10T09:00:00Z")}
	g := NewGate(GateOptions{Scope: ScopeAlways, IndicatorVisible: false, Now: c.now})

	if v := g.Verdict(); v.Allow {
		t.Fatal("always-on consent must not survive a phone that cannot show a recording indicator")
	}
	if v := g.Verdict(); v.Why != indicatorMissing {
		t.Fatalf("Why = %q, want the indicator refusal", v.Why)
	}
	// And the refusal is not a question: the user cannot answer their way out
	// of a missing indicator.
	if g.Verdict().Confirming() {
		t.Fatal("a missing indicator must not masquerade as a consent question")
	}

	c.add(time.Minute)
	g.SetIndicatorVisible(true)
	if !g.Verdict().Allow {
		t.Fatalf("with an indicator, always-on should allow: %s", g.Verdict().Why)
	}
}

func TestConsentIsJudgedAgainstWhenTheAudioWasRecorded(t *testing.T) {
	// The whole point of the history. A night's sync arrives at 03:00 carrying
	// audio from 14:00, and the only honest question is what consent was in
	// force at 14:00.
	c := &clock{t: at("2026-08-10T08:00:00Z")}
	g := NewGate(GateOptions{IndicatorVisible: true, Now: c.now})

	c.add(6 * time.Hour) // 14:00 — still ScopeNone
	c.add(2 * time.Hour) // 16:00
	if err := g.Grant(ScopeAlways); err != nil {
		t.Fatal(err)
	}
	c.add(11 * time.Hour) // 03:00 the next day, the sync

	if d := g.AllowedAt(at("2026-08-10T14:00:00Z")); d.Allow {
		t.Fatal("audio from before consent was granted must not be retroactively covered")
	}
	if d := g.AllowedAt(at("2026-08-10T17:00:00Z")); !d.Allow {
		t.Fatalf("audio from after consent was granted should be covered: %s", d.Why)
	}
	if d := g.AllowedAt(at("2026-08-10T06:00:00Z")); d.Allow {
		t.Fatal("audio from before the gate existed must be refused")
	}
	if d := g.AllowedAt(time.Time{}); d.Allow {
		t.Fatal("audio with no capture time cannot be matched to any consent")
	}
}

func TestRevokeForgetsConfirmedPlaces(t *testing.T) {
	c := &clock{t: at("2026-08-10T09:00:00Z")}
	g := NewGate(GateOptions{Scope: ScopeFamiliarPlaces, IndicatorVisible: true, Now: c.now})

	g.EnterPlace("kitchen")
	if g.Verdict().Allow {
		t.Fatal("a new place must ask before it records")
	}
	c.add(time.Minute)
	g.Answer(true)
	if !g.Verdict().Allow {
		t.Fatalf("after a yes, a confirmed place should record: %s", g.Verdict().Why)
	}
	if got := g.ConfirmedPlaces(); len(got) != 1 || got[0] != "kitchen" {
		t.Fatalf("ConfirmedPlaces = %v", got)
	}

	c.add(time.Minute)
	g.Revoke()
	if got := g.ConfirmedPlaces(); len(got) != 0 {
		t.Fatalf("revoking must forget the places, got %v", got)
	}

	// Re-granting must not silently restore every yes the user ever gave.
	c.add(time.Minute)
	if err := g.Grant(ScopeFamiliarPlaces); err != nil {
		t.Fatal(err)
	}
	g.EnterPlace("kitchen")
	if g.Verdict().Allow {
		t.Fatal("re-granting must not restore a forgotten confirmation")
	}
}

func TestANewPlaceClearsAStartedSession(t *testing.T) {
	// ConsentGate's rule: a session the wearer began at home is not consent for
	// the clinic they walked into.
	c := &clock{t: at("2026-08-10T09:00:00Z")}
	g := NewGate(GateOptions{Scope: ScopeFamiliarPlaces, IndicatorVisible: true, Now: c.now})
	g.StartSession()
	c.add(time.Minute)
	g.EnterPlace("clinic")
	if g.Verdict().Allow {
		t.Fatalf("walking somewhere new must stop capture: %s", g.Verdict().Why)
	}
}

func TestDeclineSticks(t *testing.T) {
	c := &clock{t: at("2026-08-10T09:00:00Z")}
	g := NewGate(GateOptions{Scope: ScopeAlways, IndicatorVisible: true, Now: c.now})
	g.ObserveVoices(FamiliarityNew)
	if g.Verdict().Allow {
		t.Fatal("a new voice in a two-party jurisdiction must ask")
	}
	c.add(time.Minute)
	g.Answer(false)
	c.add(time.Minute)
	g.ObserveVoices(FamiliarityKnown)
	if g.Verdict().Allow {
		t.Fatal(`"no" meant no; a voice leaving the room is not the user changing their mind`)
	}
}

func TestRegistryRefusesAnUnknownDevice(t *testing.T) {
	c := &clock{t: at("2026-08-10T09:00:00Z")}
	r := NewRegistry(GateOptions{IndicatorVisible: true, Now: c.now})

	err := r.Check("a-phone-nobody-paired", c.t)
	if err == nil {
		t.Fatal("a device that has granted nothing must be refused")
	}
	if !errors.Is(err, ErrNoConsent) {
		t.Fatalf("err = %v, want ErrNoConsent", err)
	}
	var ce *ErrConsent
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want an *ErrConsent carrying the decision", err)
	}
	if ce.Decision.Why == "" {
		t.Fatal("a refusal has to say why — the phone shows this sentence")
	}
}

func TestGrantRefusesAnUnknownScope(t *testing.T) {
	g := NewGate(GateOptions{IndicatorVisible: true, Now: (&clock{t: at("2026-08-10T09:00:00Z")}).now})
	if err := g.Grant(Scope("everything-forever")); err == nil {
		t.Fatal("an unknown scope must be refused, not rounded to the nearest one")
	}
}

func TestHistoryRecordsEveryTransition(t *testing.T) {
	c := &clock{t: at("2026-08-10T09:00:00Z")}
	g := NewGate(GateOptions{IndicatorVisible: true, Now: c.now})
	c.add(time.Minute)
	_ = g.Grant(ScopeAlways)
	c.add(time.Minute)
	g.Revoke()

	h := g.History()
	if len(h) != 3 {
		t.Fatalf("History has %d entries, want the open state plus two transitions: %+v", len(h), h)
	}
	if h[0].Decision.Allow || !h[1].Decision.Allow || h[2].Decision.Allow {
		t.Fatalf("history should read refuse → allow → refuse, got %v/%v/%v",
			h[0].Decision.Allow, h[1].Decision.Allow, h[2].Decision.Allow)
	}
	for i := 1; i < len(h); i++ {
		if h[i].At.Before(h[i-1].At) {
			t.Fatal("history must be ordered oldest first")
		}
	}
}

// A daemon restarted at 03:00 hears the nightly sync first, and it is carrying
// audio from fourteen hours earlier. A gate whose history began at the restart
// would refuse all of it — indistinguishable from never having consented, and it
// would look like a sync bug.
func TestRestoredConsentKeepsTheDateItWasGranted(t *testing.T) {
	granted := at("2026-07-01T12:00:00Z")
	c := &clock{t: at("2026-08-11T03:00:00Z")}
	r := NewRegistry(GateOptions{
		Scope: ScopeAlways, IndicatorVisible: true, Since: granted, Now: c.now,
	})

	if err := r.Check("phone", at("2026-08-10T14:00:00Z")); err != nil {
		t.Fatalf("yesterday's audio should be covered by consent granted in July: %v", err)
	}
	if err := r.Check("phone", at("2026-06-01T14:00:00Z")); err == nil {
		t.Fatal("audio from before the grant must still be refused")
	}
}
