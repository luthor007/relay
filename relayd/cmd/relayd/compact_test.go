package main

import (
	"context"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/compaction"
	"github.com/luthor007/relay/relayd/internal/logx"
	"github.com/luthor007/relay/relayd/internal/registry"
	"github.com/luthor007/relay/relayd/internal/store"
)

// TestCompactionIsConstructed is the regression test for the join that did not
// exist.
//
// internal/compaction is 5,490 lines with its own passing tests and had no
// caller: nothing compacted anything, so a session that filled its window died
// at it while the suite stayed green. That is the defect this file exists to
// catch, and it is the same one that hid the router, the console's database and
// the skill book.
func TestCompactionIsConstructed(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir + "/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	reg, err := registry.New(registry.Options{DB: db, Log: logx.Discard()})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var reported string
	stop := startCompaction(ctx, reg, db,
		func(name, status string) {
			if name == SubsystemCompaction {
				reported = status
			}
		}, logx.Discard())
	if stop == nil {
		t.Fatal("startCompaction returned no stop function")
	}
	if reported != "on" {
		t.Fatalf("compaction reported %q, want on", reported)
	}

	// It has to shut down with the daemon. A sweep goroutine that outlives the
	// context holds the process open, and a compaction fired during shutdown
	// sends a turn to a session that is already closing.
	cancel()
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the compaction sweep did not stop with the daemon")
	}
}

// TestAnEmptyMachineDecidesNothing. Decide answers ActionNone for almost every
// session, which is what makes it safe to hand it every live one — and a sweep
// on a machine with nothing running must not act on anything at all.
func TestAnEmptyMachineDecidesNothing(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir + "/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	reg, err := registry.New(registry.Options{DB: db, Log: logx.Discard()})
	if err != nil {
		t.Fatal(err)
	}

	got, err := compactionSessions{reg: reg}.Candidates(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("an empty registry offered %d candidates", len(got))
	}
}

// TestAnUnmeasuredSessionStillHasAReading.
//
// MEMORY.md §9: a runtime that reports tokens without reporting the window must
// degrade to "compact on idle after N turns" rather than silently never
// compacting. ACP reports no usage at all, so this is the normal case on three
// of the five runtimes, not the edge case.
func TestAnUnmeasuredSessionStillHasAReading(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir + "/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	reg, err := registry.New(registry.Options{DB: db, Log: logx.Discard()})
	if err != nil {
		t.Fatal(err)
	}

	row := store.Session{ID: "sess_1", Runtime: "openclaw"}
	r := readingFor(t.Context(), reg, row)

	if r.Known() {
		t.Error("a session with no reported window claimed a real pressure reading")
	}
	if r.Degraded() == "" {
		t.Error("an unmeasured reading does not say why; the console would show a blank rather than a reason")
	}
	if _, ok := r.Pressure(); ok {
		t.Error("an unmeasured reading produced a percentage")
	}
}

// TestPressureComesFromInputTokensNotTheTotal.
//
// The total includes output, and output is not context pressure. Using it
// would overstate every session — and overstating pressure means compacting
// sessions that did not need it, which costs ten to sixty seconds of silence
// each time.
func TestPressureComesFromInputTokensNotTheTotal(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir + "/relay.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	reg, err := registry.New(registry.Options{DB: db, Log: logx.Discard()})
	if err != nil {
		t.Fatal(err)
	}

	input, total, window := int64(70_000), int64(95_000), int64(100_000)
	r := readingFor(t.Context(), reg, store.Session{
		ID:            "sess_1",
		Runtime:       "claude-code",
		TokensInput:   &input,
		TokensTotal:   &total,
		ContextWindow: &window,
	})

	p, ok := r.Pressure()
	if !ok {
		t.Fatalf("no pressure from a fully reported session: %+v", r)
	}
	if p > 0.75 {
		t.Errorf("pressure = %.2f; that is the total including output, not what occupies the window", p)
	}
	if p < 0.65 {
		t.Errorf("pressure = %.2f, want about 0.70", p)
	}
}

// TestTheSweeperRefusesToDecideWithoutActing is a property of the package worth
// asserting from the outside: a sweeper with no actor is a report, not a sweep,
// and building one silently would mean a daemon that logs decisions forever and
// compacts nothing.
func TestTheSweeperRefusesToDecideWithoutActing(t *testing.T) {
	_, err := compaction.NewSweeper(compaction.SweeperOptions{
		Sessions: compactionSessions{},
	})
	if err == nil {
		t.Fatal("built a sweeper with no actor")
	}
}
