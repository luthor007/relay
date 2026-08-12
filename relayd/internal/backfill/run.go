package backfill

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/index"
	"github.com/luthor007/relay/relayd/internal/logx"
)

// Runner drives every reader once.
//
// MEMORY.md §4: backfill is **incremental and resumable**, keyed on (runtime,
// session_id, mtime). 3.6 GB summarised through a small model is an hour or two
// of work and it must survive being interrupted — so the loop checks the index
// before it opens a transcript, records every session as it finishes it, and
// treats cancellation as an ordinary outcome rather than as a failure. Re-run
// it and it picks up where it stopped.
//
// It also runs **after the pairing code prints** (§4 again): nobody should
// watch a progress bar before their glasses work. That is the caller's
// sequencing to get right; what this type provides is a [Runner.Progress] hook
// with a count and a running ETA, which MEMORY.md §12.1 asks for explicitly
// because the hour-or-two estimate has never been measured.
type Runner struct {
	Indexer *index.Indexer
	Readers []Reader

	Log *slog.Logger

	// Progress is called after each session. Never nil-checked by callers.
	Progress func(Progress)

	Now func() time.Time
}

// NewRunner wires the five readers to an indexer. Everything else on [Runner]
// is optional.
//
// The caller owns the sequencing MEMORY.md §4 asks for: this runs *after* the
// pairing code prints. It is a long job and it is resumable, so starting it in
// the background once the user is unblocked is the intended shape.
func NewRunner(ix *index.Indexer, env Env) *Runner {
	return &Runner{Indexer: ix, Readers: Readers(env)}
}

// Progress is one tick of the backfill.
type Progress struct {
	Runtime   adapter.Runtime
	SessionID string

	// Done and Total are within this runtime. Total is what Scan found, so it
	// is exact rather than estimated.
	Done, Total int

	// OverallDone and OverallTotal are across every runtime scanned so far.
	OverallDone, OverallTotal int

	Elapsed time.Duration

	// ETA is a projection from the rate so far, and it is a projection rather
	// than a promise — which is precisely why §12.1 asks for a running estimate
	// instead of a fixed one.
	ETA time.Duration

	Skipped bool
	Err     error
}

// RuntimeReport is what one reader did.
type RuntimeReport struct {
	Runtime adapter.Runtime
	Status  StoreStatus

	Scanned int
	Indexed int
	Skipped int
	Failed  int

	// Secrets counts findings across every session of this runtime, split by
	// tier because only tier 1 may ever become a vault proposal.
	SecretsTier1 int
	SecretsTier2 int

	Bytes   int64
	Elapsed time.Duration

	Roots    []string
	Notes    []string
	Failures []Failure

	// Err is the scan error for an unreadable store.
	Err error
}

// Failure is one session that could not be read. One malformed transcript must
// not cost the other 4,378 messages, so these are collected and the run
// continues.
type Failure struct {
	SessionID string
	Path      string
	Err       error
}

// Report is the whole pass.
type Report struct {
	Runtimes []RuntimeReport
	Started  time.Time
	Finished time.Time

	// Interrupted is true when the context was cancelled mid-run. Everything
	// already indexed is durable, and the next run resumes.
	Interrupted bool
}

// Totals sums the per-runtime counts.
func (r Report) Totals() (scanned, indexed, skipped, failed int) {
	for _, rr := range r.Runtimes {
		scanned += rr.Scanned
		indexed += rr.Indexed
		skipped += rr.Skipped
		failed += rr.Failed
	}
	return
}

// Dominant names the runtime holding most of the corpus, and by how much.
//
// MEMORY.md §1 measured one runtime at 70% of a real machine. The installer
// should say so rather than implying five equal peers, and a caller cannot
// compute it from a single total.
func (r Report) Dominant() (adapter.Runtime, float64) {
	var total int64
	var best RuntimeReport
	for _, rr := range r.Runtimes {
		total += rr.Bytes
		if rr.Bytes > best.Bytes {
			best = rr
		}
	}
	if total == 0 || best.Runtime == "" {
		return "", 0
	}
	return best.Runtime, float64(best.Bytes) / float64(total)
}

func (r *Runner) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

func (r *Runner) log() *slog.Logger {
	if r.Log == nil {
		return logx.Discard()
	}
	return r.Log
}

// Run scans every reader and indexes what has changed.
//
// A cancelled context returns the partial report together with the context's
// error: the caller usually wants both, and everything indexed before the
// cancellation is already committed.
func (r *Runner) Run(ctx context.Context) (Report, error) {
	rep := Report{Started: r.now()}

	// Scan everything first, so the progress total is real from the first tick
	// rather than climbing as runtimes are discovered.
	type scanned struct {
		reader Reader
		res    ScanResult
	}
	var passes []scanned
	overallTotal := 0

	for _, rd := range r.Readers {
		if err := ctx.Err(); err != nil {
			rep.Interrupted = true
			rep.Finished = r.now()
			return rep, err
		}
		res, err := rd.Scan(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				rep.Interrupted = true
				rep.Finished = r.now()
				return rep, err
			}
			res.Status = StoreUnreadable
			res.Err = err
		}
		passes = append(passes, scanned{rd, res})
		overallTotal += len(res.Refs)
		r.log().Info("backfill scan",
			"runtime", rd.Runtime(), "status", res.Status, "sessions", len(res.Refs))
	}

	overallDone := 0
	for _, p := range passes {
		rr := RuntimeReport{
			Runtime: p.reader.Runtime(),
			Status:  p.res.Status,
			Scanned: len(p.res.Refs),
			Roots:   p.res.Roots,
			Notes:   p.res.Notes,
			Err:     p.res.Err,
		}
		started := r.now()

		for i, ref := range p.res.Refs {
			if err := ctx.Err(); err != nil {
				rr.Elapsed = r.now().Sub(started)
				rep.Runtimes = append(rep.Runtimes, rr)
				rep.Interrupted = true
				rep.Finished = r.now()
				return rep, err
			}

			overallDone++
			prog := Progress{
				Runtime:      rr.Runtime,
				SessionID:    ref.SessionID,
				Done:         i + 1,
				Total:        len(p.res.Refs),
				OverallDone:  overallDone,
				OverallTotal: overallTotal,
				Elapsed:      r.now().Sub(rep.Started),
			}
			prog.ETA = eta(prog.Elapsed, overallDone, overallTotal)

			need, err := r.needs(ctx, ref)
			if err != nil {
				rr.Failed++
				rr.Failures = append(rr.Failures, Failure{ref.SessionID, ref.Path, err})
				prog.Err = err
				r.emit(prog)
				continue
			}
			if !need {
				rr.Skipped++
				prog.Skipped = true
				r.emit(prog)
				continue
			}

			s, err := p.reader.Read(ctx, ref)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					rr.Elapsed = r.now().Sub(started)
					rep.Runtimes = append(rep.Runtimes, rr)
					rep.Interrupted = true
					rep.Finished = r.now()
					return rep, err
				}
				rr.Failed++
				rr.Failures = append(rr.Failures, Failure{ref.SessionID, ref.Path, err})
				prog.Err = err
				r.emit(prog)
				r.log().Warn("backfill read failed",
					"runtime", rr.Runtime, "session", ref.SessionID, "path", ref.Path, "err", err)
				continue
			}

			res, err := r.Indexer.Index(ctx, s)
			if err != nil {
				rr.Failed++
				rr.Failures = append(rr.Failures, Failure{ref.SessionID, ref.Path, err})
				prog.Err = err
				r.emit(prog)
				continue
			}

			rr.Indexed++
			rr.Bytes += ref.Size
			for _, f := range res.Findings {
				switch f.Tier {
				case index.TierVendor:
					rr.SecretsTier1++
				default:
					rr.SecretsTier2++
				}
			}
			if len(res.Findings) > 0 {
				// The finding itself is never logged; only that there was one,
				// and which rule fired.
				r.log().Info("backfill found credentials",
					"runtime", rr.Runtime, "session", ref.SessionID,
					"tier1", len(res.VaultCandidates()), "total", len(res.Findings),
					"preview", logx.Secret(res.Findings[0].Preview()))
			}
			r.emit(prog)
		}

		rr.Elapsed = r.now().Sub(started)
		rep.Runtimes = append(rep.Runtimes, rr)
	}

	sort.SliceStable(rep.Runtimes, func(i, j int) bool { return rep.Runtimes[i].Bytes > rep.Runtimes[j].Bytes })
	rep.Finished = r.now()
	return rep, nil
}

func (r *Runner) needs(ctx context.Context, ref Ref) (bool, error) {
	if r.Indexer == nil {
		return false, fmt.Errorf("backfill: no indexer")
	}
	return r.Indexer.NeedsIndexing(ctx, ref.Runtime, ref.SessionID, ref.MTime, ref.Size)
}

func (r *Runner) emit(p Progress) {
	if r.Progress != nil {
		r.Progress(p)
	}
}

// eta projects the remaining time from the rate so far. It is deliberately
// simple: MEMORY.md §12.1 wants a running estimate that moves, not a fixed
// promise nobody has measured.
func eta(elapsed time.Duration, done, total int) time.Duration {
	if done <= 0 || total <= done {
		return 0
	}
	per := elapsed / time.Duration(done)
	return per * time.Duration(total-done)
}
