package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/store"
)

// Reconciler reads one runtime's own session store, so the registry can be
// rebuilt from the outside rather than only from what relayd happened to watch.
//
// This is the seam that makes the registry honest across a restart, a crash, or
// a session someone started in a terminal five minutes ago. Every runtime keeps
// its own record and MEMORY.md §4 says where:
//
//	Claude Code  ~/.claude/projects/<slug>/<uuid>.jsonl, one file per session
//	Hermes       ~/.hermes/state.db, and `hermes sessions list`
//	Codex        ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl, indexed by
//	             ~/.codex/session_index.jsonl
//	OpenCode     `opencode export <id>` — and --sanitize redacts secrets
//	OpenClaw     <state>/agents/<agent>/sessions/sessions.json, where <state>
//	             is RELOCATABLE and must be resolved, never assumed
//
// The readers are the next phase's work (internal/backfill). This package
// defines the contract and the merge rules, so that when they land there is one
// place that decides what an outside observation is allowed to overwrite.
type Reconciler interface {
	// Runtime is which of the five this reads.
	Runtime() adapter.Runtime

	// Scan enumerates what the runtime itself believes exists.
	Scan(ctx context.Context) (Scan, error)
}

// Scan is one reader's answer.
type Scan struct {
	Sessions []Observed

	// Complete says this scan enumerated every session the runtime has.
	//
	// It is not a formality. Only a complete scan may conclude that a row we
	// have and the runtime does not is gone — a reader that looked at the last
	// thirty days, or that could not resolve OpenClaw's relocatable state
	// directory and found an empty list, would otherwise close every session in
	// the table and report it as success. MEMORY.md §4 names that exact failure.
	Complete bool

	// Note explains a partial scan, and the console shows it.
	Note string
}

// Observed is one session as the runtime's own store describes it.
//
// The transcript fields are a POINTER — path plus offset — and never a copy.
// MEMORY.md §3: the 3.6 GB stays on disk, in place, unmoved. This type carries
// them so the backfill readers and the registry agree on one shape, but
// reconciliation writes only the registry tier; the index tier belongs to
// internal/index and writing it from here would clobber its bookkeeping.
type Observed struct {
	NativeID string

	Title     string
	Workspace string
	GitBranch string
	Model     string

	StartedAt  time.Time
	LastActive time.Time

	Messages  int64
	ToolCalls int64

	// Nil where the runtime does not report it. ACP runtimes report neither, and
	// a zero would claim an observation that was never made.
	CostUSD     *float64
	TokensTotal *int64

	// Path and ByteOffset locate the transcript on disk.
	Path       string
	ByteOffset int64

	// Live is the runtime saying this session is currently active. A session
	// that is live in the runtime but not in our live map is one relayd is not
	// driving — it may have been started in a terminal.
	Live bool

	// Ended is the runtime saying this session is finished.
	Ended bool
}

// ReconcileResult is what one pass found.
type ReconcileResult struct {
	Runtime  adapter.Runtime `json:"runtime"`
	Scanned  int             `json:"scanned"`
	Complete bool            `json:"complete"`
	Note     string          `json:"note,omitempty"`

	// Added are sessions the runtime has and we did not.
	Added []string `json:"added,omitempty"`
	// Updated are rows whose metadata moved.
	Updated []string `json:"updated,omitempty"`
	// Missing are rows we have and the runtime does not. Reported always; acted
	// on only when the scan was complete.
	Missing []string `json:"missing,omitempty"`
	// Skipped are rows the registry is driving right now, which an outside
	// observation never overwrites.
	Skipped []string `json:"skipped,omitempty"`
}

// Reconcile merges one runtime's own view of its sessions into the registry.
//
// Three merge rules, in order of how much damage getting them wrong does:
//
//  1. **A live session is never overwritten.** If relayd is driving it, our
//     state is the truth and the runtime's file on disk is behind. Subject,
//     cost and state all stay.
//  2. **A row we do not have becomes one we do**, closed or idle depending on
//     what the runtime says, so the list is complete rather than only being
//     what this process watched.
//  3. **A row the runtime no longer has is closed, and only after a complete
//     scan.** Anything else, and a reader that found nothing would empty the
//     table and call it a clean run.
func (r *Registry) Reconcile(ctx context.Context, rec Reconciler) (ReconcileResult, error) {
	rt := rec.Runtime()
	res := ReconcileResult{Runtime: rt}

	scan, err := rec.Scan(ctx)
	if err != nil {
		return res, fmt.Errorf("registry: reconcile %s: %w", rt, err)
	}
	res.Scanned = len(scan.Sessions)
	res.Complete = scan.Complete
	res.Note = scan.Note

	seen := make(map[string]bool, len(scan.Sessions))
	for _, obs := range scan.Sessions {
		if obs.NativeID == "" {
			continue
		}
		seen[obs.NativeID] = true

		id, err := r.idForNative(ctx, string(rt), obs.NativeID)
		if err != nil {
			return res, err
		}
		if id == "" {
			row := observedRow(r.newID(), rt, obs, r.now())
			if err := r.db.PutSession(ctx, row); err != nil {
				return res, err
			}
			res.Added = append(res.Added, row.ID)
			r.changes.Publish(Change{Kind: ChangeAdded, Session: row, At: r.now()})
			continue
		}

		if _, live := r.Get(id); live {
			// Rule 1. We are driving it; the file on disk is behind us.
			res.Skipped = append(res.Skipped, id)
			continue
		}

		row, err := r.db.GetSession(ctx, id)
		if err != nil {
			return res, err
		}
		if merged, changed := mergeObserved(row, obs); changed {
			if err := r.db.PutSession(ctx, merged); err != nil {
				return res, err
			}
			res.Updated = append(res.Updated, id)
			r.changes.Publish(Change{Kind: ChangeUpdated, Session: merged, At: r.now()})
		}
	}

	// Rule 3.
	rows, err := r.db.ListSessions(ctx, store.SessionFilter{Runtime: string(rt)})
	if err != nil {
		return res, err
	}
	for _, row := range rows {
		if row.NativeID == "" || seen[row.NativeID] {
			continue
		}
		if _, live := r.Get(row.ID); live {
			continue
		}
		res.Missing = append(res.Missing, row.ID)
		if !scan.Complete {
			continue
		}
		if row.State == store.SessionClosed {
			continue
		}
		row.State = store.SessionClosed
		if err := r.db.PutSession(ctx, row); err != nil {
			return res, err
		}
		r.record(Incident{
			Runtime: string(rt), Session: row.ID, Kind: IncidentReconcileMissing,
			Message: "the runtime's own store no longer has this session",
		})
		r.changes.Publish(Change{Kind: ChangeClosed, Session: row, At: r.now()})
	}

	return res, nil
}

// AddReconciler registers a reader so ReconcileAll picks it up.
func (r *Registry) AddReconciler(rec Reconciler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reconcilers == nil {
		r.reconcilers = map[adapter.Runtime]Reconciler{}
	}
	r.reconcilers[rec.Runtime()] = rec
}

// ReconcileAll runs every registered reader. One reader failing does not stop
// the others: four runtimes reconciling and one erroring is a better answer than
// none, and the error is returned alongside the results rather than instead of
// them.
func (r *Registry) ReconcileAll(ctx context.Context) ([]ReconcileResult, error) {
	r.mu.RLock()
	recs := make([]Reconciler, 0, len(r.reconcilers))
	for _, rt := range adapter.Runtimes() {
		if rec, ok := r.reconcilers[rt]; ok {
			recs = append(recs, rec)
		}
	}
	r.mu.RUnlock()

	var out []ReconcileResult
	var errs []error
	for _, rec := range recs {
		res, err := r.Reconcile(ctx, rec)
		out = append(out, res)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return out, errors.Join(errs...)
}

func (r *Registry) idForNative(ctx context.Context, runtime, native string) (string, error) {
	var id string
	err := r.db.SQL().QueryRowContext(ctx,
		`SELECT id FROM session WHERE runtime = ? AND native_id = ?`, runtime, native).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("registry: look up %s/%s: %w", runtime, native, err)
	}
	return id, nil
}

func observedRow(id string, rt adapter.Runtime, obs Observed, now time.Time) store.Session {
	state := store.SessionClosed
	switch {
	case obs.Live:
		// The runtime has it open but relayd is not driving it — someone started
		// it in a terminal. Idle rather than running: we are not watching a turn.
		state = store.SessionIdle
	case obs.Ended:
		state = store.SessionClosed
	}
	created := obs.StartedAt
	if created.IsZero() {
		created = now
	}
	active := obs.LastActive
	if active.IsZero() {
		active = created
	}
	return store.Session{
		ID:          id,
		Runtime:     string(rt),
		NativeID:    obs.NativeID,
		Agent:       obs.Model,
		Subject:     obs.Title,
		Workspace:   obs.Workspace,
		GitBranch:   obs.GitBranch,
		Entities:    []string{},
		CreatedAt:   created,
		LastActive:  active,
		State:       state,
		CostUSD:     obs.CostUSD,
		TokensTotal: obs.TokensTotal,
	}
}

// mergeObserved folds an outside observation into a row we already have,
// without inventing anything. A field the runtime does not report leaves ours
// alone; nil stays nil rather than becoming zero.
func mergeObserved(row store.Session, obs Observed) (store.Session, bool) {
	changed := false
	set := func(dst *string, v string) {
		if v != "" && *dst != v {
			*dst = v
			changed = true
		}
	}
	set(&row.Subject, obs.Title)
	set(&row.Workspace, obs.Workspace)
	set(&row.GitBranch, obs.GitBranch)
	set(&row.Agent, obs.Model)

	if !obs.LastActive.IsZero() && obs.LastActive.After(row.LastActive) {
		row.LastActive = obs.LastActive
		changed = true
	}
	if !obs.StartedAt.IsZero() && (row.CreatedAt.IsZero() || obs.StartedAt.Before(row.CreatedAt)) {
		row.CreatedAt = obs.StartedAt
		changed = true
	}
	if obs.CostUSD != nil && (row.CostUSD == nil || *obs.CostUSD > *row.CostUSD) {
		v := *obs.CostUSD
		row.CostUSD = &v
		changed = true
	}
	if obs.TokensTotal != nil && (row.TokensTotal == nil || *obs.TokensTotal > *row.TokensTotal) {
		v := *obs.TokensTotal
		row.TokensTotal = &v
		changed = true
	}

	want := row.State
	switch {
	case obs.Ended:
		want = store.SessionClosed
	case obs.Live && row.State == store.SessionClosed:
		want = store.SessionIdle
	}
	if want != row.State {
		row.State = want
		changed = true
	}
	return row, changed
}
