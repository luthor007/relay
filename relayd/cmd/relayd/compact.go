package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/compaction"
	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/registry"
	"github.com/luthor007/relay/relayd/internal/store"
)

// compactSweepEvery is how often the idle pass looks.
//
// A minute rather than a second because of what the pass is for: MEMORY.md §9
// compacts on *idle*, and idleness is measured in minutes. A tighter loop would
// not compact anything sooner — [compaction.Policy] still requires the session
// to have been quiet — and would wake the machine sixty times as often for an
// answer that is almost always "nothing to do".
const compactSweepEvery = time.Minute

// SubsystemCompaction is the health-screen name for the idle pass.
const SubsystemCompaction = "compaction"

// startCompaction wires MEMORY.md §9's idle pass and returns a stop function.
//
// internal/compaction is 5,490 lines with its own tests and had no caller at
// all: nothing compacted anything, and a session that filled its window died at
// it. This is the join. It is the same defect as the router, the console
// database and the skill book — a package written, tested, green, and reachable
// by nothing — and it was the largest instance of it left in the tree.
//
// The sweeper does not own a goroutine or a clock by design, so the timer lives
// here where the daemon's lifetime is.
func startCompaction(ctx context.Context, reg *registry.Registry, db *store.DB, report func(string, string), log *slog.Logger) func() {
	if report == nil {
		report = func(string, string) {}
	}
	if reg == nil {
		report(SubsystemCompaction, "no session registry")
		return func() {}
	}

	sweeper, err := compaction.NewSweeper(compaction.SweeperOptions{
		Sessions: compactionSessions{reg: reg},
		Actor: &compaction.TurnCompactor{
			Sessions: registrySessionSource{reg: reg},
			// Handoff and StartNew stay unwired. Handing work to a fresh
			// session is routing's job (ORCHESTRATOR.md §4) — it picks the
			// runtime, announces the choice and can be undone — and none of
			// that belongs to a timer. Nil returns ErrNotWired, which the
			// sweeper reports as a degraded outcome and falls back to
			// compaction for, rather than swallowing.
		},
		// Hermes's compression_locks lease. Without it Hermes sessions are
		// skipped rather than raced, which is MEMORY.md §12.5's standing
		// instruction — and skipping is the safe half: a Hermes session we do
		// not compact still has its own auto-compaction, which we never
		// disable.
		HermesLease: hermesLease(db, log),
		Log:         log,
	})
	if err != nil {
		log.Warn("relayd: no idle compaction; long sessions will hit their own limits",
			"error", err)
		report(SubsystemCompaction, err.Error())
		return func() {}
	}
	report(SubsystemCompaction, "on")

	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(compactSweepEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				res, err := sweeper.Sweep(ctx)
				if err != nil {
					log.Warn("relayd: compaction sweep failed", "error", err)
					continue
				}
				for _, o := range res.Outcomes {
					logOutcome(log, o)
				}
			}
		}
	}()
	return func() { <-done }
}

// logOutcome says what happened, and says the near-misses too.
//
// A sweep that decided to compact and did not is the interesting case: the
// lease was held, the budget was spent, the handoff had nothing to carry. A
// log that only recorded successes would make a session that never gets
// compacted look like a session that never needed it.
func logOutcome(log *slog.Logger, o compaction.Outcome) {
	switch {
	case o.Err != nil:
		log.Warn("relayd: compaction failed",
			"session", o.Session, "runtime", o.Runtime, "error", o.Err)
	case o.Skipped != "":
		log.Info("relayd: compaction skipped",
			"session", o.Session, "action", o.Decision.Action, "why", o.Skipped)
	case o.Degraded != "":
		log.Info("relayd: compaction degraded",
			"session", o.Session, "action", o.Decision.Action, "why", o.Degraded)
	case o.Done:
		log.Info("relayd: compacted a session on idle",
			"session", o.Session, "runtime", o.Runtime, "action", o.Decision.Action)
	}
}

// hermesLease opens the compression lease, or reports why it could not.
func hermesLease(db *store.DB, log *slog.Logger) compaction.Lease {
	if db == nil {
		return nil
	}
	// Hermes keeps its lease in its *own* database, not ours, and finding that
	// database is per-install. Until the path is discovered the honest answer
	// is no lease — which makes the sweeper skip Hermes sessions rather than
	// race them, and skipping is the safe half: their own auto-compaction is
	// still on, because nothing here ever disables it.
	log.Debug("relayd: no Hermes compression lease configured; those sessions are skipped rather than raced")
	return nil
}

// registrySessionSource is [compaction.SessionSource]. The signature differs
// from the registry's own Session accessor — that one reads a row, this one
// hands over the live session — so it is a named type rather than a cast.
type registrySessionSource struct{ reg *registry.Registry }

func (r registrySessionSource) Session(id string) (adapter.Session, bool) {
	e, ok := r.reg.Get(id)
	if !ok || e == nil {
		return nil, false
	}
	s := e.Adapter()
	return s, s != nil
}

// compactionSessions adapts the registry to [compaction.Sessions].
//
// Returning every live session is fine — Decide answers ActionNone for almost
// all of them — so this does no filtering of its own beyond what it can observe.
type compactionSessions struct{ reg *registry.Registry }

func (c compactionSessions) Candidates(ctx context.Context) ([]compaction.SessionView, error) {
	live := c.reg.Live()
	out := make([]compaction.SessionView, 0, len(live))
	for _, e := range live {
		row := e.Row()
		out = append(out, compaction.SessionView{
			ID:         row.ID,
			Runtime:    adapter.Runtime(row.Runtime),
			Workspace:  row.Workspace,
			LastActive: row.LastActive,
			// A compaction queued behind a live turn *is* the silence the whole
			// package exists to prevent, so this flag is load-bearing rather
			// than informational.
			InTurn:  e.Turn() != "",
			Reading: readingFor(ctx, c.reg, row),
			// Anything the adapter knows that compaction cannot see. A runtime
			// that reports the capability absent for this session is a better
			// reason to skip than a guess from the runtime name.
			CompactUnavailable: compactUnavailable(e),
		})
	}
	return out, nil
}

// readingFor measures context pressure from what the runtime actually reported.
//
// Every field is a pointer on the way in and nil means "this runtime does not
// report it" — never zero. A missing window degrades to [compaction.Unmeasured],
// which MEMORY.md §9 handles by compacting on turn count instead, rather than
// silently never compacting.
func readingFor(ctx context.Context, reg *registry.Registry, row store.Session) compaction.Reading {
	rt := adapter.Runtime(row.Runtime)

	// Turn count is the fallback numerator when a runtime reports no window,
	// so it is worth one query per live session per minute. Live sessions are
	// few; this is not the sweep's cost.
	turns := 0
	if d, err := reg.Detail(ctx, row.ID); err == nil {
		turns = len(d.Turns)
	}

	// The model is not on the row — no runtime reports it in a form worth
	// storing — so the Windows table cannot be consulted. That is fine
	// wherever the runtime reports its own window, and where it does not the
	// reading degrades to turn count, which is what MEMORY.md §9 asks for.
	const model = ""

	if row.ContextWindow == nil || *row.ContextWindow <= 0 || row.TokensInput == nil {
		return compaction.Unmeasured(rt, model, turns)
	}
	// InputTokens, not TotalTokens: the total includes output, and output is
	// not context pressure. FromThreadTotal sums input and cached input, which
	// is what actually occupies the window.
	r, err := compaction.FromThreadTotal(rt, model, &event.Usage{
		InputTokens:   row.TokensInput,
		ContextWindow: row.ContextWindow,
	}, turns, compaction.Windows{})
	if err != nil {
		return compaction.Unmeasured(rt, model, turns)
	}
	return r
}

// compactUnavailable reports a per-session reason the sweeper cannot work out
// for itself.
//
// Empty today: the sweeper already refuses a runtime with no documented
// mechanism, and no adapter reports compaction as a per-session capability. It
// stays as a named seam because that is where an adapter's answer belongs when
// one starts giving it, and the alternative is a caller guessing from the
// runtime name.
func compactUnavailable(*registry.Entry) string { return "" }
