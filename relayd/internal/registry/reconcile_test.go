package registry_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/registry"
	"github.com/luthor007/relay/relayd/internal/store"
)

// stubReader stands in for the readers the backfill phase will write against
// ~/.claude/projects, ~/.hermes/state.db and ~/.codex/session_index.jsonl. None
// of those runtimes exist in a build container, so the contract is what gets
// tested here and the readers get tested against real files on the author's Mac.
type stubReader struct {
	rt   adapter.Runtime
	scan registry.Scan
	err  error
}

func (s stubReader) Runtime() adapter.Runtime { return s.rt }
func (s stubReader) Scan(context.Context) (registry.Scan, error) {
	return s.scan, s.err
}

func TestReconcileAddsSessionsWeNeverWatched(t *testing.T) {
	h := newHarness(t, registry.Options{})
	start := time.Now().Add(-2 * time.Hour).Truncate(time.Millisecond)

	res, err := h.Reg.Reconcile(context.Background(), stubReader{
		rt: adapter.Codex,
		scan: registry.Scan{
			Complete: true,
			Sessions: []registry.Observed{{
				NativeID:    "thread-1",
				Title:       "the payments refactor",
				Workspace:   "/repo",
				GitBranch:   "payments",
				Model:       "gpt-5.6",
				StartedAt:   start,
				LastActive:  start.Add(time.Hour),
				TokensTotal: event.I64(12000),
				Path:        "/home/u/.codex/sessions/2026/08/10/rollout-x.jsonl",
				Ended:       true,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 1 {
		t.Fatalf("added = %v", res.Added)
	}

	rows, err := h.Reg.List(context.Background(), store.SessionFilter{Runtime: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	row := rows[0]
	if row.Subject != "the payments refactor" || row.NativeID != "thread-1" {
		t.Fatalf("row = %+v", row)
	}
	if row.State != store.SessionClosed {
		t.Fatalf("an ended session reconciles to closed, got %s", row.State)
	}
	if row.TokensTotal == nil || *row.TokensTotal != 12000 {
		t.Fatalf("tokens = %v", row.TokensTotal)
	}
	// Codex carries no dollar figure anywhere in its contract, and nil must stay
	// nil rather than becoming a zero the console renders as "free".
	if row.CostUSD != nil {
		t.Fatalf("cost = %v, want nil", *row.CostUSD)
	}
}

func TestReconcileNeverOverwritesALiveSession(t *testing.T) {
	h := newHarness(t, registry.Options{})
	e, fs := h.start(t, "payments")
	fs.Emit(event.TurnStarted{Meta: fs.Meta("turn-1")})
	waitState(t, e, store.SessionRunning)

	res, err := h.Reg.Reconcile(context.Background(), stubReader{
		rt: adapter.ClaudeCode,
		scan: registry.Scan{
			Complete: true,
			Sessions: []registry.Observed{{
				NativeID: e.Row().NativeID,
				Title:    "something stale from the file on disk",
				Ended:    true,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != e.ID() {
		t.Fatalf("skipped = %v, want the live session", res.Skipped)
	}
	row, err := h.Reg.Session(context.Background(), e.ID())
	if err != nil {
		t.Fatal(err)
	}
	if row.Subject != "payments" || row.State != store.SessionRunning {
		t.Fatalf("a live session was overwritten by a file on disk: %+v", row)
	}
}

// The rule that stops a bad reader from emptying the table. MEMORY.md §4: a
// reader that assumes OpenClaw's default state directory "will silently find
// nothing and report an empty history as success".
func TestAPartialScanReportsMissingButDoesNotCloseAnything(t *testing.T) {
	h := newHarness(t, registry.Options{})

	if _, err := h.Reg.Reconcile(context.Background(), stubReader{
		rt: adapter.OpenClaw,
		scan: registry.Scan{
			Complete: true,
			Sessions: []registry.Observed{{NativeID: "agent:main:main", Title: "notes", Live: true}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Now a reader that could not resolve the relocatable state dir and found
	// nothing at all.
	res, err := h.Reg.Reconcile(context.Background(), stubReader{
		rt:   adapter.OpenClaw,
		scan: registry.Scan{Complete: false, Note: "could not resolve OPENCLAW_STATE_DIR"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Missing) != 1 {
		t.Fatalf("missing = %v", res.Missing)
	}
	rows, err := h.Reg.List(context.Background(), store.SessionFilter{Runtime: "openclaw"})
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].State == store.SessionClosed {
		t.Fatal("an incomplete scan closed a session; an empty read must never look like a clean run")
	}
	if res.Note == "" {
		t.Fatal("a partial scan must carry its reason through to the console")
	}
}

func TestACompleteScanClosesWhatTheRuntimeNoLongerHas(t *testing.T) {
	h := newHarness(t, registry.Options{})
	if _, err := h.Reg.Reconcile(context.Background(), stubReader{
		rt: adapter.Hermes,
		scan: registry.Scan{
			Complete: true,
			Sessions: []registry.Observed{{NativeID: "h-1", Title: "docs", Live: true}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	res, err := h.Reg.Reconcile(context.Background(), stubReader{
		rt:   adapter.Hermes,
		scan: registry.Scan{Complete: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Missing) != 1 {
		t.Fatalf("missing = %v", res.Missing)
	}
	rows, _ := h.Reg.List(context.Background(), store.SessionFilter{Runtime: "hermes"})
	if rows[0].State != store.SessionClosed {
		t.Fatalf("state = %s, want closed", rows[0].State)
	}
	var found bool
	for _, i := range h.Reg.Incidents() {
		if i.Kind == registry.IncidentReconcileMissing {
			found = true
		}
	}
	if !found {
		t.Fatal("closing a session because the runtime forgot it is worth an incident")
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	h := newHarness(t, registry.Options{})
	scan := registry.Scan{
		Complete: true,
		Sessions: []registry.Observed{{
			NativeID: "s-1", Title: "docs", Workspace: "/repo",
			LastActive: time.Now().Truncate(time.Millisecond), Live: true,
		}},
	}
	first, err := h.Reg.Reconcile(context.Background(), stubReader{rt: adapter.OpenCode, scan: scan})
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.Reg.Reconcile(context.Background(), stubReader{rt: adapter.OpenCode, scan: scan})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Added) != 1 {
		t.Fatalf("first pass added %v", first.Added)
	}
	if len(second.Added) != 0 || len(second.Updated) != 0 {
		t.Fatalf("second pass churned: added=%v updated=%v", second.Added, second.Updated)
	}
	rows, _ := h.Reg.List(context.Background(), store.SessionFilter{Runtime: "opencode"})
	if len(rows) != 1 {
		t.Fatalf("a second pass duplicated the row: %d", len(rows))
	}
}

func TestReconcileAllKeepsGoingWhenOneReaderFails(t *testing.T) {
	h := newHarness(t, registry.Options{})
	boom := errors.New("hermes state.db is locked")
	h.Reg.AddReconciler(stubReader{rt: adapter.Hermes, err: boom})
	h.Reg.AddReconciler(stubReader{
		rt: adapter.Codex,
		scan: registry.Scan{
			Complete: true,
			Sessions: []registry.Observed{{NativeID: "thread-9", Title: "migration"}},
		},
	})

	res, err := h.Reg.ReconcileAll(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the reader's error carried through", err)
	}
	if len(res) != 2 {
		t.Fatalf("results = %d, want one per reader", len(res))
	}
	rows, _ := h.Reg.List(context.Background(), store.SessionFilter{Runtime: "codex"})
	if len(rows) != 1 {
		t.Fatal("one reader failing stopped the others")
	}
}
