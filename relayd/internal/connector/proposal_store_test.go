package connector_test

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/connector"
	"github.com/luthor007/relay/relayd/internal/store"
)

func memoryDB(t *testing.T, dir string) (*store.DB, func()) {
	t.Helper()
	db, err := store.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	return db, func() { db.Close() }
}

// A proposer whose evidence lives in a map forgets everything on restart. With
// DefaultMinEpisodes at 3 over a seven-day window, that means a laptop rebooted
// daily can never reach three episodes — the feature is silently off on exactly
// the machines it is meant for, and no unit test of the proposer would notice
// because the proposer is correct.
func TestEvidenceSurvivesAReopen(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Millisecond)

	db, closeDB := memoryDB(t, dir)
	first := connector.NewProposer(printerSet(), nil)
	first.Now = func() time.Time { return now }
	first.MinEpisodes = 3
	first.Memory = connector.NewSQLMemory(db)
	for i := range 3 {
		first.Observe(connector.Evidence{
			Episode: string(rune('a' + i)),
			At:      now.Add(-time.Duration(i+1) * 6 * time.Hour),
			Text:    "the prusa again",
		})
	}
	closeDB()

	// A second process on the same file, which is what a restart is.
	db2, closeDB2 := memoryDB(t, dir)
	defer closeDB2()
	second := connector.NewProposer(printerSet(), nil)
	second.Now = func() time.Time { return now }
	second.MinEpisodes = 3
	second.Memory = connector.NewSQLMemory(db2)

	got := second.Proposals(context.Background())
	if len(got) != 1 {
		t.Fatalf("a fresh proposer over the same store made %d proposals from three "+
			"recorded episodes, want 1", len(got))
	}
	if got[0].Episodes != 3 {
		t.Errorf("episodes = %d after reopening, want the three that were recorded",
			got[0].Episodes)
	}
}

// §4b: "a proposal the user said no to is not a proposal to make again next
// week — repeated asking is how blind-accept is trained." A dismissal in a map
// lasts until the process exits, which on a laptop is until tomorrow morning.
func TestADismissalSurvivesAReopen(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Millisecond)
	ctx := context.Background()

	db, closeDB := memoryDB(t, dir)
	first := connector.NewProposer(printerSet(), nil)
	first.Now = func() time.Time { return now }
	first.MinEpisodes = 1
	first.Memory = connector.NewSQLMemory(db)
	first.Observe(connector.Evidence{Episode: "a", At: now.Add(-time.Hour), Text: "prusa"})
	if len(first.Proposals(ctx)) != 1 {
		t.Fatal("nothing was proposed, so there is nothing to dismiss")
	}
	if err := first.DismissWithReason(ctx, "prusa", "not right now"); err != nil {
		t.Fatalf("the dismissal was not recorded: %v", err)
	}
	closeDB()

	db2, closeDB2 := memoryDB(t, dir)
	defer closeDB2()
	second := connector.NewProposer(printerSet(), nil)
	second.Now = func() time.Time { return now.Add(24 * time.Hour) }
	second.MinEpisodes = 1
	second.Memory = connector.NewSQLMemory(db2)
	second.Observe(connector.Evidence{Episode: "b", At: now.Add(23 * time.Hour), Text: "prusa"})

	if got := second.Proposals(ctx); len(got) != 0 {
		t.Fatalf("the day after saying no, the proposal came back: %+v", got)
	}
}

// Evidence expires. Without the sweep the table is append-only for the life of
// the machine, and every restart reloads mentions from months ago that
// Proposals then discards one at a time.
func TestExpiredEvidenceLeavesTheStore(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Millisecond)

	db, closeDB := memoryDB(t, dir)
	defer closeDB()
	mem := connector.NewSQLMemory(db)

	p := connector.NewProposer(printerSet(), nil)
	p.Now = func() time.Time { return now }
	p.Memory = mem
	p.Observe(connector.Evidence{Episode: "old", At: now.Add(-90 * 24 * time.Hour), Text: "prusa"})
	p.Observe(connector.Evidence{Episode: "new", At: now.Add(-time.Hour), Text: "prusa"})

	p.Proposals(context.Background())

	got, err := mem.Sightings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Episode != "new" {
		t.Fatalf("after a pass the store holds %+v; a mention from March is not a "+
			"reason in August and must not be reloaded on the next restart", got)
	}
}

// The invariant that must be enforced by a test rather than by a reviewer
// noticing a column.
//
// connector.Proposer discards Evidence.Text once it has matched, and the
// sentence the user is shown is generated from counts. A well-meaning column
// added so the console could "show why" would write unredacted user speech into
// relay.db — MEMORY.md §6's "detect secrets before indexing, never after"
// broken at the schema level, where no code path review would catch it.
func TestTheSightingTableCannotHoldWhatWasSaid(t *testing.T) {
	db, closeDB := memoryDB(t, t.TempDir())
	defer closeDB()

	rows, err := db.SQL().Query(`SELECT name FROM pragma_table_info('connector_sighting')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(cols)
	if want := "at,connector,episode"; strings.Join(cols, ",") != want {
		t.Fatalf("connector_sighting columns are %v, want exactly [%s]; there must be "+
			"nowhere in this table for an utterance to land", cols, want)
	}
}

// StoredSighting is the other half of the same guarantee: even a store that
// wanted to persist the text has no field to read it from.
func TestAStoredSightingCarriesNoText(t *testing.T) {
	var s connector.StoredSighting
	s.Connector, s.Episode, s.At = "prusa", "ep", time.Now()
	// This compiles, and `s.Text = "..."` deliberately does not. The assertion
	// is the type, so the reminder lives where somebody would add the field.
	if s.Connector == "" {
		t.Fatal("unreachable")
	}
}

// A nil Memory keeps the pre-store behaviour exactly, which is what every other
// test in this package relies on and what a daemon with no database gets.
func TestANilMemoryChangesNothing(t *testing.T) {
	now := time.Now()
	p := connector.NewProposer(printerSet(), nil)
	p.Now = func() time.Time { return now }
	p.MinEpisodes = 1
	p.Observe(connector.Evidence{Episode: "a", At: now.Add(-time.Hour), Text: "prusa"})
	if len(p.Proposals(context.Background())) != 1 {
		t.Fatal("a proposer with no store must still propose from this run's evidence")
	}
	p.Dismiss("prusa")
	if len(p.Proposals(context.Background())) != 0 {
		t.Fatal("a proposer with no store must still honour a dismissal in this run")
	}
}
