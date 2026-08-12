package compaction

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
)

type fakeSessions struct {
	views []SessionView
	err   error
}

func (f *fakeSessions) Candidates(context.Context) ([]SessionView, error) { return f.views, f.err }

type fakeActor struct {
	compacted []string
	flushed   []string
	handedOff []string
	started   []string
	briefs    []Brief
	err       error
}

func (a *fakeActor) Compact(_ context.Context, v SessionView) error {
	a.compacted = append(a.compacted, v.ID)
	return a.err
}
func (a *fakeActor) Flush(_ context.Context, v SessionView) error {
	a.flushed = append(a.flushed, v.ID)
	return a.err
}
func (a *fakeActor) Handoff(_ context.Context, v SessionView, b Brief) error {
	a.handedOff = append(a.handedOff, v.ID)
	a.briefs = append(a.briefs, b)
	return a.err
}
func (a *fakeActor) StartNew(_ context.Context, v SessionView) error {
	a.started = append(a.started, v.ID)
	return a.err
}

type fakeBriefs struct {
	brief Brief
	err   error
}

func (f fakeBriefs) Brief(context.Context, SessionView) (Brief, error) { return f.brief, f.err }

var sweepNow = time.Unix(1_700_000_000, 0)

func sweeper(t *testing.T, o SweeperOptions) (*Sweeper, *fakeActor) {
	t.Helper()
	act := &fakeActor{}
	if o.Actor == nil {
		o.Actor = act
	}
	if o.Now == nil {
		o.Now = func() time.Time { return sweepNow }
	}
	s, err := NewSweeper(o)
	if err != nil {
		t.Fatal(err)
	}
	return s, act
}

func fullView(t *testing.T, id string, rt adapter.Runtime, used int64) SessionView {
	t.Helper()
	r, err := FromLatestRequest(rt, "m", used, 1000, 5, Windows{})
	if err != nil {
		t.Fatal(err)
	}
	return SessionView{
		ID:         id,
		Runtime:    rt,
		Reading:    r,
		LastActive: sweepNow.Add(-30 * time.Minute),
	}
}

func TestSweeperNeedsItsSeams(t *testing.T) {
	if _, err := NewSweeper(SweeperOptions{Actor: &fakeActor{}}); err == nil {
		t.Fatal("a sweeper with nowhere to get sessions is not a sweeper")
	}
	if _, err := NewSweeper(SweeperOptions{Sessions: &fakeSessions{}}); err == nil {
		t.Fatal("deciding without acting is a report, not a sweep")
	}
}

func TestSweepCompactsAnIdleSession(t *testing.T) {
	views := &fakeSessions{views: []SessionView{fullView(t, "s1", adapter.Codex, 800)}}
	s, act := sweeper(t, SweeperOptions{Sessions: views})

	res, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Acted != 1 || len(act.compacted) != 1 {
		t.Fatalf("acted = %d, compacted = %v", res.Acted, act.compacted)
	}
	out := res.Outcomes[0]
	if !out.Done || out.Decision.Action != ActionCompact {
		t.Fatalf("outcome = %+v", out)
	}
	// Codex is the one runtime whose compaction we can actually see.
	if out.Degraded != "" {
		t.Fatalf("Codex compaction should not be degraded: %q", out.Degraded)
	}
}

// Four of the five cannot tell us a compaction finished. We asked; that is all
// we know, and the outcome says so rather than claiming an observation.
func TestUnobservableCompactionIsReportedAsSuch(t *testing.T) {
	for _, rt := range []adapter.Runtime{adapter.ClaudeCode, adapter.OpenClaw, adapter.OpenCode} {
		views := &fakeSessions{views: []SessionView{fullView(t, "s1", rt, 800)}}
		s, _ := sweeper(t, SweeperOptions{Sessions: views})
		res, err := s.Sweep(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		out := res.Outcomes[0]
		if !out.Done {
			t.Fatalf("%s: outcome = %+v", rt, out)
		}
		if !strings.Contains(out.Degraded, "no completion we can observe") {
			t.Fatalf("%s: degraded = %q", rt, out.Degraded)
		}
	}
}

func TestHermesIsSkippedWhenTheLeaseIsHeld(t *testing.T) {
	views := &fakeSessions{views: []SessionView{fullView(t, "s1", adapter.Hermes, 800)}}

	other := NewMemoryLease("hermes-itself")
	other.Now = func() time.Time { return sweepNow }
	if got, _ := other.Acquire(context.Background(), "s1", time.Minute); !got {
		t.Fatal("setup")
	}
	ours := NewMemoryLease("relayd")
	ours.held = other.held
	ours.Now = func() time.Time { return sweepNow }

	s, act := sweeper(t, SweeperOptions{Sessions: views, HermesLease: ours})
	res, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(act.compacted) != 0 {
		t.Fatal("we compressed a session Hermes was already compressing")
	}
	if !strings.Contains(res.Outcomes[0].Skipped, "lease is held") {
		t.Fatalf("skipped = %q", res.Outcomes[0].Skipped)
	}
	if res.Acted != 0 {
		t.Fatalf("acted = %d", res.Acted)
	}
}

// No lease available at all: skipped as a handoff candidate rather than raced.
func TestHermesWithoutALeaseIsNotCompacted(t *testing.T) {
	views := &fakeSessions{views: []SessionView{fullView(t, "s1", adapter.Hermes, 800)}}
	s, act := sweeper(t, SweeperOptions{Sessions: views})

	res, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(act.compacted) != 0 {
		t.Fatalf("compacted = %v: with no lease, we do not compress", act.compacted)
	}
	d := res.Outcomes[0].Decision
	if d.Action != ActionHandoff {
		t.Fatalf("action = %q (%s); a session we cannot compact still has a handoff", d.Action, d.Reason)
	}
	if !strings.Contains(d.Reason, "lease") {
		t.Fatalf("reason = %q", d.Reason)
	}
}

func TestHandoffUsesTheBrief(t *testing.T) {
	v := fullView(t, "s1", adapter.Codex, 800)
	v.Compactions = 2
	v.LastCompaction = sweepNow.Add(-8 * time.Hour)

	b := builder(t, BriefOptions{})
	brief, err := b.Build(BriefInput{Session: "s1", Summary: "the retry loop"})
	if err != nil {
		t.Fatal(err)
	}

	s, act := sweeper(t, SweeperOptions{
		Sessions: &fakeSessions{views: []SessionView{v}},
		Briefs:   fakeBriefs{brief: brief},
	})
	res, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(act.handedOff) != 1 {
		t.Fatalf("handed off = %v (%s)", act.handedOff, res.Outcomes[0].Decision.Reason)
	}
	if act.briefs[0].Work != "the retry loop" {
		t.Fatalf("brief = %+v", act.briefs[0])
	}
	if len(act.compacted) != 0 {
		t.Fatal("a handoff does not also compact")
	}
}

// The visible degrade: a handoff we cannot write becomes a compaction, and the
// reason travels with the outcome instead of being logged and forgotten.
func TestHandoffWithNothingToCarryDegradesToCompaction(t *testing.T) {
	v := fullView(t, "s1", adapter.Codex, 800)
	v.Compactions = 2
	v.LastCompaction = sweepNow.Add(-8 * time.Hour)

	s, act := sweeper(t, SweeperOptions{
		Sessions: &fakeSessions{views: []SessionView{v}},
		Briefs:   fakeBriefs{err: ErrNothingToCarry},
	})
	res, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(act.compacted) != 1 || len(act.handedOff) != 0 {
		t.Fatalf("compacted = %v, handed off = %v", act.compacted, act.handedOff)
	}
	if !strings.Contains(res.Outcomes[0].Degraded, "nothing observed to carry") {
		t.Fatalf("degraded = %q", res.Outcomes[0].Degraded)
	}
	if !res.Outcomes[0].Done {
		t.Fatal("the fallback did happen")
	}
}

func TestHandoffWithNoBriefBuilderSaysSo(t *testing.T) {
	v := fullView(t, "s1", adapter.Codex, 800)
	v.Compactions = 2
	v.LastCompaction = sweepNow.Add(-8 * time.Hour)

	s, act := sweeper(t, SweeperOptions{Sessions: &fakeSessions{views: []SessionView{v}}})
	res, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(act.compacted) != 1 {
		t.Fatalf("compacted = %v", act.compacted)
	}
	if !strings.Contains(res.Outcomes[0].Degraded, "no brief builder") {
		t.Fatalf("degraded = %q", res.Outcomes[0].Degraded)
	}
}

// Compaction is ten to sixty seconds during which that session cannot answer.
// Doing five at once means every session the user might speak to is busy.
func TestSweepActsOnOneSessionAtATime(t *testing.T) {
	views := &fakeSessions{views: []SessionView{
		fullView(t, "s1", adapter.Codex, 800),
		fullView(t, "s2", adapter.Codex, 850),
		fullView(t, "s3", adapter.Codex, 900),
	}}
	s, act := sweeper(t, SweeperOptions{Sessions: views})

	res, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Considered != 3 {
		t.Fatalf("considered = %d", res.Considered)
	}
	if res.Acted != 1 || len(act.compacted) != 1 {
		t.Fatalf("acted = %d, compacted = %v", res.Acted, act.compacted)
	}
	var deferred int
	for _, o := range res.Outcomes {
		if strings.Contains(o.Skipped, "one session at a time") {
			deferred++
		}
	}
	if deferred != 2 {
		t.Fatalf("deferred = %d, want 2", deferred)
	}
}

func TestSweepFlushesAndStartsNew(t *testing.T) {
	flush := fullView(t, "s1", adapter.Codex, 600)
	s, act := sweeper(t, SweeperOptions{Sessions: &fakeSessions{views: []SessionView{flush}}})
	if _, err := s.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(act.flushed) != 1 {
		t.Fatalf("flushed = %v", act.flushed)
	}

	drifted := fullView(t, "s2", adapter.Codex, 800)
	drifted.Signals = Signals{WorkspaceChanged: true}
	s2, act2 := sweeper(t, SweeperOptions{Sessions: &fakeSessions{views: []SessionView{drifted}}})
	if _, err := s2.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(act2.started) != 1 {
		t.Fatalf("started = %v", act2.started)
	}
}

func TestSweepReportsAFailureWithoutStopping(t *testing.T) {
	views := &fakeSessions{views: []SessionView{fullView(t, "s1", adapter.Codex, 800)}}
	boom := errors.New("runtime said no")
	s, _ := sweeper(t, SweeperOptions{Sessions: views, Actor: &fakeActor{err: boom}})

	res, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatalf("one broken runtime must not fail the sweep: %v", err)
	}
	if !errors.Is(res.Outcomes[0].Err, boom) {
		t.Fatalf("err = %v", res.Outcomes[0].Err)
	}
	if res.Outcomes[0].Done || res.Acted != 0 {
		t.Fatal("a failed action is not a completed one")
	}
}

func TestSweepPropagatesAListingFailure(t *testing.T) {
	boom := errors.New("no database")
	s, _ := sweeper(t, SweeperOptions{Sessions: &fakeSessions{err: boom}})
	if _, err := s.Sweep(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

// On an idle sweep there is no utterance, so the silence is the gap — a session
// nobody has touched in a week is drifting whether or not anyone said so.
func TestIdleSilenceCountsAsTheGap(t *testing.T) {
	v := fullView(t, "s1", adapter.Codex, 800)
	v.LastActive = sweepNow.Add(-8 * 24 * time.Hour)

	s, _ := sweeper(t, SweeperOptions{Sessions: &fakeSessions{views: []SessionView{v}}})
	d := s.DecideFor(v)
	if d.Action != ActionNew {
		t.Fatalf("action = %q (%s), want new after eight days", d.Action, d.Reason)
	}

	// A caller that already knows the gap keeps it.
	v.Signals.Gap = time.Minute
	if d := s.DecideFor(v); d.Action != ActionCompact {
		t.Fatalf("action = %q (%s): a supplied gap must not be overwritten", d.Action, d.Reason)
	}
}

func TestUnknownRuntimeCannotBeCompacted(t *testing.T) {
	r, err := FromLatestRequest("gemini-cli", "m", 800, 1000, 5, Windows{})
	if err != nil {
		t.Fatal(err)
	}
	v := SessionView{ID: "s1", Runtime: "gemini-cli", Reading: r, LastActive: sweepNow.Add(-time.Hour)}

	s, act := sweeper(t, SweeperOptions{Sessions: &fakeSessions{views: []SessionView{v}}})
	res, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(act.compacted) != 0 {
		t.Fatal("we invented a way to compact a runtime we have never seen")
	}
	if res.Outcomes[0].Decision.Action != ActionHandoff {
		t.Fatalf("action = %q", res.Outcomes[0].Decision.Action)
	}
}

func TestSweepStopsOnACancelledContext(t *testing.T) {
	views := &fakeSessions{views: []SessionView{fullView(t, "s1", adapter.Codex, 800)}}
	s, act := sweeper(t, SweeperOptions{Sessions: views})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Sweep(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(act.compacted) != 0 {
		t.Fatal("a cancelled sweep must not start a compaction")
	}
}

func TestSweeperPolicyIsTheEffectiveOne(t *testing.T) {
	s, _ := sweeper(t, SweeperOptions{Sessions: &fakeSessions{}})
	if s.Policy().IdleAt != DefaultPolicy().IdleAt {
		t.Fatalf("policy = %+v", s.Policy())
	}
}
