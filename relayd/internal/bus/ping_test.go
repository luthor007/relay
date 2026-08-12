package bus_test

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/bus"
	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/logx"
)

// recorder is a Delivery that a test can wait on.
type recorder struct {
	mu       sync.Mutex
	pings    []bus.Ping
	retracts []string
	got      chan struct{}
}

func newRecorder() *recorder { return &recorder{got: make(chan struct{}, 64)} }

func (r *recorder) Deliver(_ context.Context, p bus.Ping) error {
	r.mu.Lock()
	r.pings = append(r.pings, p)
	r.mu.Unlock()
	select {
	case r.got <- struct{}{}:
	default:
	}
	return nil
}

func (r *recorder) Retract(_ context.Context, id, reason string) error {
	r.mu.Lock()
	r.retracts = append(r.retracts, id+":"+reason)
	r.mu.Unlock()
	select {
	case r.got <- struct{}{}:
	default:
	}
	return nil
}

func (r *recorder) snapshot() []bus.Ping {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bus.Ping(nil), r.pings...)
}

func (r *recorder) retractions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.retracts...)
}

func (r *recorder) wait(t *testing.T, n int) []bus.Ping {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		if p := r.snapshot(); len(p) >= n {
			return p
		}
		select {
		case <-r.got:
		case <-deadline:
			t.Fatalf("timed out waiting for %d pings, have %d", n, len(r.snapshot()))
		}
	}
}

func fastPinger(t *testing.T, o bus.PingOptions) (*bus.Pinger, chan event.Event, func()) {
	t.Helper()
	if o.BatchWindow == 0 {
		o.BatchWindow = 40 * time.Millisecond
	}
	if o.RepingAfter == 0 {
		o.RepingAfter = 60 * time.Millisecond
	}
	if o.GapTimeout == 0 {
		o.GapTimeout = 200 * time.Millisecond
	}
	if o.UtteranceTimeout == 0 {
		o.UtteranceTimeout = 200 * time.Millisecond
	}
	o.Log = logx.Discard()
	seq := 0
	if o.NewID == nil {
		var mu sync.Mutex
		o.NewID = func() string {
			mu.Lock()
			defer mu.Unlock()
			seq++
			return "ping-" + strconv.Itoa(seq)
		}
	}

	p := bus.NewPinger(o)
	in := make(chan event.Event, 32)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = p.Run(ctx, in) }()
	return p, in, func() {
		cancel()
		<-done
	}
}

func ask(t *testing.T, session, prompt string, kind event.InputKind) *event.NeedsInput {
	t.Helper()
	return event.NewNeedsInput(meta(session, 1), event.InputSpec{
		Ask:     kind,
		Prompt:  prompt,
		Options: []event.Option{{ID: "yes", Name: "Allow once", Kind: event.OptionAllowOnce}},
	}, func(context.Context, event.Reply) error { return nil })
}

// ADAPTERS.md §7: "Three sessions *asking* is three pings, because each one
// needs a distinct answer."
func TestThreeBlockedSessionsAreThreePings(t *testing.T) {
	rec := newRecorder()
	_, in, stop := fastPinger(t, bus.PingOptions{Delivery: rec})
	defer stop()

	for _, s := range []string{"payments", "docs", "migration"} {
		in <- ask(t, s, "may I run this?", event.InputPermission)
	}

	pings := rec.wait(t, 3)
	sessions := map[string]bool{}
	for _, p := range pings {
		if p.Class != bus.ClassBlocking {
			t.Fatalf("blocked question produced a %s ping", p.Class)
		}
		if len(p.Sessions) != 1 {
			t.Fatalf("a blocking ping batched %d sessions", len(p.Sessions))
		}
		if p.Ask == nil {
			t.Fatal("blocking ping has no reply path")
		}
		sessions[p.Sessions[0]] = true
	}
	if len(sessions) != 3 {
		t.Fatalf("got pings for %d distinct sessions, want 3", len(sessions))
	}
}

// "Three sessions finishing inside a minute is one ping — 'payments and docs are
// done, the migration failed' — not three."
func TestThreeCompletionsAreOnePing(t *testing.T) {
	rec := newRecorder()
	names := map[string]string{"s1": "payments", "s2": "docs", "s3": "the migration"}
	_, in, stop := fastPinger(t, bus.PingOptions{
		Delivery: rec,
		Namer:    func(id string) string { return names[id] },
	})
	defer stop()

	in <- event.TurnCompleted{Meta: meta("s1", 1), OK: true, StopReason: event.StopEndTurn}
	in <- event.TurnCompleted{Meta: meta("s2", 1), OK: true, StopReason: event.StopEndTurn}
	in <- event.TurnCompleted{Meta: meta("s3", 1), OK: false, StopReason: event.StopError}

	pings := rec.wait(t, 1)
	if len(pings) != 1 {
		t.Fatalf("got %d pings, want 1", len(pings))
	}
	p := pings[0]
	if p.Class != bus.ClassInformational {
		t.Fatalf("class = %s", p.Class)
	}
	if len(p.Sessions) != 3 {
		t.Fatalf("sessions = %v", p.Sessions)
	}
	if want := "payments and docs are done, the migration failed"; p.Line != want {
		t.Fatalf("line = %q, want %q", p.Line, want)
	}

	// And nothing else arrives: completions never repeat.
	time.Sleep(200 * time.Millisecond)
	if got := len(rec.snapshot()); got != 1 {
		t.Fatalf("a completion ping repeated: %d pings", got)
	}
}

func TestCompletionLineIsCappedForSpeech(t *testing.T) {
	rec := newRecorder()
	_, in, stop := fastPinger(t, bus.PingOptions{
		Delivery: rec,
		Namer:    func(id string) string { return strings.Repeat("very long session name ", 3) + id },
	})
	defer stop()

	for i := 0; i < 6; i++ {
		in <- event.TurnCompleted{Meta: meta("s"+strconv.Itoa(i), 1), OK: true}
	}
	p := rec.wait(t, 1)[0]
	if len(p.Line) > bus.SpeechCap+3 { // +3 for the multi-byte ellipsis
		t.Fatalf("line is %d chars, cap is %d: %q", len(p.Line), bus.SpeechCap, p.Line)
	}
	if !strings.HasSuffix(p.Line, "…") {
		t.Fatalf("truncation is not marked: %q", p.Line)
	}
}

// "If unheard, re-ping once at 2 min, then hold."
func TestBlockingPingRepeatsOnceThenHolds(t *testing.T) {
	rec := newRecorder()
	_, in, stop := fastPinger(t, bus.PingOptions{
		Delivery:    rec,
		RepingAfter: 50 * time.Millisecond,
	})
	defer stop()

	in <- ask(t, "s1", "may I?", event.InputPermission)

	pings := rec.wait(t, 2)
	if pings[0].ID != pings[1].ID {
		t.Fatalf("the re-ping got a new id (%s vs %s) — a phone would stack two notifications",
			pings[0].ID, pings[1].ID)
	}
	if pings[0].Repeat != 0 || pings[1].Repeat != 1 {
		t.Fatalf("repeat counters = %d, %d", pings[0].Repeat, pings[1].Repeat)
	}

	// Then hold: no third.
	time.Sleep(300 * time.Millisecond)
	if got := len(rec.snapshot()); got != 2 {
		t.Fatalf("blocking ping fired %d times, want exactly 2", got)
	}
}

func TestAcknowledgedBlockingPingDoesNotRepeat(t *testing.T) {
	rec := newRecorder()
	p, in, stop := fastPinger(t, bus.PingOptions{
		Delivery:    rec,
		RepingAfter: 150 * time.Millisecond,
	})
	defer stop()

	in <- ask(t, "s1", "may I?", event.InputPermission)
	first := rec.wait(t, 1)[0]
	p.Heard(first.ID)

	time.Sleep(400 * time.Millisecond)
	if got := len(rec.snapshot()); got != 1 {
		t.Fatalf("an acknowledged ping repeated: %d pings", got)
	}
}

// Codex's serverRequest/resolved: the approval was answered in a terminal. A
// ping that outlives its question wakes someone to approve what is already
// approved.
func TestWithdrawnQuestionIsNeverPinged(t *testing.T) {
	rec := newRecorder()
	_, in, stop := fastPinger(t, bus.PingOptions{Delivery: rec})
	defer stop()

	n := ask(t, "s1", "may I?", event.InputPermission)
	n.Withdraw("answered in a terminal")
	in <- n

	time.Sleep(250 * time.Millisecond)
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("pinged about a withdrawn question: %+v", got[0])
	}
}

func TestAnsweredQuestionRetractsItsPing(t *testing.T) {
	rec := newRecorder()
	_, in, stop := fastPinger(t, bus.PingOptions{
		Delivery:    rec,
		RepingAfter: time.Hour, // never repeat during this test
	})
	defer stop()

	n := ask(t, "s1", "may I?", event.InputPermission)
	in <- n
	p := rec.wait(t, 1)[0]

	if err := n.Reply(context.Background(), event.Reply{OptionID: "yes", Decision: event.DecisionAllow}); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(2 * time.Second)
	for {
		if r := rec.retractions(); len(r) > 0 {
			if !strings.HasPrefix(r[0], p.ID+":") {
				t.Fatalf("retracted %q, expected the ping %s", r[0], p.ID)
			}
			return
		}
		select {
		case <-rec.got:
		case <-deadline:
			t.Fatal("an answered question never retracted its ping")
		}
	}
}

// "Quiet hours apply to completions only."
//
// The policy records the fact and no longer records the consequence — there is
// no Speak on a bus.Ping any more (PRODUCT.md §6b). So what this asserts is
// that quiet hours were consulted and found to apply. That they then produce a
// silent notification and no speech is the voice backend's rule, asserted where
// it lives, in TestQuietHoursPingNotifiesWithoutSpeaking.
func TestQuietHoursHoldSpeechNotNotification(t *testing.T) {
	quiet, err := bus.ParseQuietHours("22:00-07:00")
	if err != nil {
		t.Fatal(err)
	}
	threeAM := time.Date(2026, 8, 10, 3, 0, 0, 0, time.Local)

	rec := newRecorder()
	_, in, stop := fastPinger(t, bus.PingOptions{
		Delivery: rec,
		Quiet:    quiet,
		Now:      func() time.Time { return threeAM },
	})
	defer stop()

	in <- event.TurnCompleted{Meta: meta("s1", 1), OK: true}
	p := rec.wait(t, 1)[0]
	if !p.Quiet {
		t.Fatal("a completion raised at 3 a.m. did not record that quiet hours apply")
	}
	// The gap was there — an open gate. Recording both separately is the point:
	// a backend that wanted to show this ping silently on a screen needs to know
	// the difference between "held for quiet hours" and "nobody was listening".
	if !p.Gap {
		t.Fatal("the gap was open and the ping did not record it")
	}
}

// "A session blocked at 3 a.m. is still blocked at 8 a.m.; holding that ping
// just wastes eight hours." And ORCHESTRATOR.md §4b: a confirmation is not
// suppressible.
func TestQuietHoursDoNotSuppressABlockedSession(t *testing.T) {
	quiet, err := bus.ParseQuietHours("22:00-07:00")
	if err != nil {
		t.Fatal(err)
	}
	threeAM := time.Date(2026, 8, 10, 3, 0, 0, 0, time.Local)

	rec := newRecorder()
	_, in, stop := fastPinger(t, bus.PingOptions{
		Delivery:    rec,
		Quiet:       quiet,
		Now:         func() time.Time { return threeAM },
		RepingAfter: time.Hour,
	})
	defer stop()

	in <- ask(t, "s1", "send the email?", event.InputPermission)
	p := rec.wait(t, 1)[0]

	// Quiet false at 3 a.m. is the assertion, and it is not a rounding error:
	// §7 exempts blocking pings from quiet hours, so the hours were consulted
	// and found not to apply. Any backend reading Quiet — voice today, a screen
	// in v2 — therefore cannot suppress this ping without ignoring the field.
	if p.Quiet {
		t.Fatal("quiet hours were applied to a blocked session's ping")
	}
	if !p.Consequential {
		t.Fatal("a permission request must be marked consequential and unsuppressible")
	}
}

// A completion ping waits for a gap; a blocked one waits only for the current
// utterance to end.
func TestTurnTaking(t *testing.T) {
	t.Run("completion waits for a gap", func(t *testing.T) {
		gate := bus.NewSpeechGate()
		gate.StartSpeaking()

		rec := newRecorder()
		_, in, stop := fastPinger(t, bus.PingOptions{
			Delivery:   rec,
			Gate:       gate,
			GapTimeout: 3 * time.Second,
		})
		defer stop()

		in <- event.TurnCompleted{Meta: meta("s1", 1), OK: true}
		time.Sleep(200 * time.Millisecond)
		if got := rec.snapshot(); len(got) != 0 {
			t.Fatal("a completion ping interrupted speech")
		}

		gate.StopSpeaking()
		p := rec.wait(t, 1)[0]
		if !p.Gap {
			t.Fatal("the ping went out once the gap opened but did not record the gap")
		}
	})

	t.Run("blocked question waits for the utterance, not the conversation", func(t *testing.T) {
		gate := bus.NewSpeechGate()
		gate.StartUtterance()
		gate.StartSpeaking() // relay is mid-sentence too; that is not a reason to wait

		rec := newRecorder()
		_, in, stop := fastPinger(t, bus.PingOptions{
			Delivery:         rec,
			Gate:             gate,
			UtteranceTimeout: 3 * time.Second,
			RepingAfter:      time.Hour,
		})
		defer stop()

		in <- ask(t, "s1", "may I?", event.InputPermission)
		time.Sleep(200 * time.Millisecond)
		if got := rec.snapshot(); len(got) != 0 {
			t.Fatal("a blocking ping interrupted the user mid-utterance")
		}

		gate.EndUtterance() // still speaking; the blocking ping may interrupt that
		rec.wait(t, 1)
	})
}

// Past the gap timeout the ping still goes out, carrying Gap false. §7's "the
// speech is dropped and the notification is not" is what the voice backend then
// does with that; see TestACompletionThatFoundNoGapNotifiesWithoutSpeaking.
func TestNoGapMeansNotifyWithoutSpeech(t *testing.T) {
	gate := bus.NewSpeechGate()
	gate.StartSpeaking()
	defer gate.StopSpeaking()

	rec := newRecorder()
	_, in, stop := fastPinger(t, bus.PingOptions{
		Delivery:   rec,
		Gate:       gate,
		GapTimeout: 80 * time.Millisecond,
	})
	defer stop()

	in <- event.TurnCompleted{Meta: meta("s1", 1), OK: true}
	p := rec.wait(t, 1)[0]
	if p.Gap {
		t.Fatal("claimed a gap while the gate was held shut for the whole timeout")
	}
	// It was still delivered. A completion nobody is ever told about is a
	// completion that did not happen as far as the user is concerned, so the
	// ping reaches the render layer either way and the layer decides.
	if p.Line == "" {
		t.Fatal("a completion that found no gap must still arrive with something to say")
	}
}

func TestRetryableErrorAndReplayNeverPing(t *testing.T) {
	rec := newRecorder()
	_, in, stop := fastPinger(t, bus.PingOptions{Delivery: rec})
	defer stop()

	// A retryable error is not a user-facing failure (Codex willRetry).
	in <- event.Error{Meta: meta("s1", 1), Message: "429", Retryable: true}

	// A replayed completion is history, not news: reattaching to a session must
	// not fire a ping for every turn in it.
	m := meta("s1", 2)
	m.Replay = true
	in <- event.TurnCompleted{Meta: m, OK: true}

	time.Sleep(250 * time.Millisecond)
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("pinged for %d events that must never ping", len(got))
	}
}

func TestStatsCountWhatHappened(t *testing.T) {
	rec := newRecorder()
	p, in, stop := fastPinger(t, bus.PingOptions{Delivery: rec, RepingAfter: time.Hour})
	defer stop()

	in <- ask(t, "s1", "may I?", event.InputPermission)
	in <- event.TurnCompleted{Meta: meta("s2", 1), OK: true}
	in <- event.TurnCompleted{Meta: meta("s3", 1), OK: true}
	rec.wait(t, 2)

	s := p.Stats()
	if s.Blocking != 1 || s.Informational != 1 {
		t.Fatalf("stats = %+v", s)
	}
	if s.Batched != 1 {
		t.Fatalf("batched = %d, want 1 (two completions, one ping)", s.Batched)
	}
}

func TestQuietHoursParsing(t *testing.T) {
	if q, err := bus.ParseQuietHours(""); err != nil || q.Enabled() {
		t.Fatalf("empty should disable: %v %v", q, err)
	}
	for _, bad := range []string{"22:00", "25:00-07:00", "22:61-07:00", "22:00-22:00", "x-y"} {
		if _, err := bus.ParseQuietHours(bad); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}
	q, err := bus.ParseQuietHours("22:00-07:00")
	if err != nil {
		t.Fatal(err)
	}
	if q.String() != "22:00-07:00" {
		t.Fatalf("round trip = %q", q.String())
	}
	day := func(h, m int) time.Time { return time.Date(2026, 8, 10, h, m, 0, 0, time.Local) }
	for _, c := range []struct {
		t    time.Time
		want bool
	}{
		{day(23, 30), true}, {day(3, 0), true}, {day(6, 59), true},
		{day(7, 0), false}, {day(12, 0), false}, {day(21, 59), false}, {day(22, 0), true},
	} {
		if got := q.Active(c.t); got != c.want {
			t.Fatalf("Active(%s) = %v, want %v", c.t.Format("15:04"), got, c.want)
		}
	}

	// A same-day window is the other half of the branch.
	work, err := bus.ParseQuietHours("09:00-17:00")
	if err != nil {
		t.Fatal(err)
	}
	if !work.Active(day(12, 0)) || work.Active(day(20, 0)) {
		t.Fatal("same-day window is wrong")
	}
}
