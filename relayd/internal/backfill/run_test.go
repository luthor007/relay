package backfill

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/detect"
	"github.com/luthor007/relay/relayd/internal/index"
	"github.com/luthor007/relay/relayd/internal/store"
)

func runnerFixture(t *testing.T) (*Runner, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	env := fixtureEnv(t)
	env.Exec = openCodeExec(t)

	cc := NewClaudeCode(env)
	cc.Dir = testdata(t, "claudecode")
	cx := NewCodex(env)
	cx.Dir = testdata(t, "codex")
	oc := NewOpenClaw(env)
	oc.Dir = testdata(t, "openclaw")
	ocode := NewOpenCode(env)
	ocode.Dir = testdata(t, "opencode")
	hm := NewHermes(env)
	hm.DBPath = buildHermesDB(t, "schema.sql", "seed.sql")

	return &Runner{
		Indexer: index.New(db, nil),
		Readers: []Reader{hm, cc, cx, ocode, oc},
	}, db
}

func TestRunIndexesEverySessionOnce(t *testing.T) {
	r, db := runnerFixture(t)
	ctx := context.Background()

	rep, err := r.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Interrupted {
		t.Fatal("run reported itself interrupted")
	}

	scanned, indexed, skipped, failed := rep.Totals()
	// 3 hermes + 3 claude code + 2 codex + 1 opencode + 3 openclaw
	if scanned != 12 || indexed != 12 {
		t.Fatalf("scanned %d, indexed %d, want 12/12 (%d skipped, %d failed)", scanned, indexed, skipped, failed)
	}
	if failed != 0 {
		for _, rr := range rep.Runtimes {
			for _, f := range rr.Failures {
				t.Errorf("%s/%s: %v", rr.Runtime, f.SessionID, f.Err)
			}
		}
	}

	var rows int
	if err := db.SQL().QueryRow(`SELECT count(*) FROM session_index`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 12 {
		t.Fatalf("%d rows in the index", rows)
	}

	// Every row is a pointer into something that still exists on disk, or an
	// explicit re-read instruction where the transcript is not a file.
	rowsIter, err := db.SQL().Query(`SELECT runtime, session_id, path FROM session_index`)
	if err != nil {
		t.Fatal(err)
	}
	defer rowsIter.Close()
	for rowsIter.Next() {
		var rt, id, path string
		if err := rowsIter.Scan(&rt, &id, &path); err != nil {
			t.Fatal(err)
		}
		if path == "" {
			t.Errorf("%s/%s has no pointer", rt, id)
			continue
		}
		if strings.HasPrefix(path, "opencode://") {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s/%s points at %s which does not exist: %v", rt, id, path, err)
		}
	}

	// The corpus is lopsided by design, and the report has to be able to say so.
	rt, share := rep.Dominant()
	if rt == "" || share <= 0 {
		t.Error("Dominant must name a runtime so the installer does not imply five equal peers")
	}
}

// MEMORY.md §4: incremental and resumable, keyed on (runtime, session_id,
// mtime). A second run must do no work.
func TestRunIsIncremental(t *testing.T) {
	r, _ := runnerFixture(t)
	ctx := context.Background()

	if _, err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := r.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scanned, indexed, skipped, _ := second.Totals()
	if indexed != 0 || skipped != scanned {
		t.Fatalf("second run indexed %d and skipped %d of %d — nothing changed, so nothing should be re-read", indexed, skipped, scanned)
	}
}

// An hour-long step that cannot be interrupted is a worse bug than an
// hour-long step. Cancel mid-run, then resume: no session is indexed twice and
// none is lost.
func TestRunSurvivesInterruption(t *testing.T) {
	r, db := runnerFixture(t)
	ctx, cancel := context.WithCancel(context.Background())

	stopAfter := 4
	seen := 0
	r.Progress = func(p Progress) {
		seen++
		if seen >= stopAfter {
			cancel()
		}
	}

	rep, err := r.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if !rep.Interrupted {
		t.Error("the report must say it was interrupted")
	}
	_, partial, _, _ := rep.Totals()
	if partial == 0 || partial >= 12 {
		t.Fatalf("interrupted run indexed %d; expected some but not all", partial)
	}

	var rows int
	if err := db.SQL().QueryRow(`SELECT count(*) FROM session_index`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != partial {
		t.Fatalf("%d rows durable but the report claims %d indexed", rows, partial)
	}

	// Resume.
	r.Progress = nil
	rep2, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, indexed2, skipped2, _ := rep2.Totals()
	if indexed2+skipped2 != 12 {
		t.Fatalf("resume covered %d sessions, want 12", indexed2+skipped2)
	}
	if skipped2 != partial {
		t.Fatalf("resume re-read %d already-indexed sessions", 12-indexed2-partial)
	}

	if err := db.SQL().QueryRow(`SELECT count(*) FROM session_index`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 12 {
		t.Fatalf("%d rows after resume", rows)
	}
}

// One malformed transcript must not cost the other 4,378 messages.
func TestRunContinuesPastAFailedSession(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	r := &Runner{
		Indexer: index.New(db, nil),
		Readers: []Reader{&flakyReader{}},
	}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, indexed, _, failed := rep.Totals()
	if indexed != 2 || failed != 1 {
		t.Fatalf("indexed %d, failed %d — want the two good sessions kept and the bad one recorded", indexed, failed)
	}
	if len(rep.Runtimes[0].Failures) != 1 || rep.Runtimes[0].Failures[0].SessionID != "bad" {
		t.Fatalf("failures %+v", rep.Runtimes[0].Failures)
	}
}

func TestRunProgressCarriesACountAndAnETA(t *testing.T) {
	r, _ := runnerFixture(t)

	tick := 0
	now := time.UnixMilli(1_770_000_000_000)
	r.Now = func() time.Time {
		now = now.Add(250 * time.Millisecond)
		return now
	}

	var last Progress
	var sawETA bool
	r.Progress = func(p Progress) {
		tick++
		last = p
		if p.OverallTotal != 12 {
			t.Errorf("progress total %d, want a real total from the scan pass", p.OverallTotal)
		}
		if p.ETA > 0 {
			sawETA = true
		}
	}

	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tick != 12 {
		t.Fatalf("%d progress ticks for 12 sessions", tick)
	}
	if last.OverallDone != 12 {
		t.Errorf("final tick says %d/%d", last.OverallDone, last.OverallTotal)
	}
	if !sawETA {
		t.Error("MEMORY.md §12.1 asks for a running ETA, not a fixed promise")
	}
}

// The whole point of the ordering rule, end to end: a credential that exists in
// a transcript on disk must never reach the database.
func TestRunNeverIndexesACredential(t *testing.T) {
	key, kind := corpusSecret(t)

	dir := t.TempDir()
	project := filepath.Join(dir, "projects", "-home-user-src-relay")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(project, "11112222-3333-4444-5555-666677778888.jsonl")
	rec := map[string]any{
		"type": "user", "uuid": "u1", "cwd": "/home/user/src/relay",
		"timestamp": "2026-08-09T10:00:00.000Z",
		"aiTitle":   "rotate the key",
		"message":   map[string]any{"role": "user", "content": "here is the key: " + key},
	}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(transcript, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	db, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cc := NewClaudeCode(fixtureEnv(t))
	cc.Dir = dir
	r := &Runner{Indexer: index.New(db, nil), Readers: []Reader{cc}}

	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Runtimes[0].SecretsTier1 == 0 {
		t.Fatalf("the %s credential was not detected", kind)
	}

	// The raw transcript is untouched — we index, we do not rewrite anyone's
	// history.
	after, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), key) {
		t.Fatal("backfill modified the transcript on disk; MEMORY.md §3 keeps it in place, unmoved")
	}

	// The database has never seen it.
	assertDBHasNo(t, db, key)

	markers, err := db.ListSecretMarkers(context.Background(), "claude-code", "11112222-3333-4444-5555-666677778888")
	if err != nil {
		t.Fatal(err)
	}
	if len(markers) == 0 {
		t.Fatal("no marker was written; search, summaries and embeddings would see nothing at all")
	}
}

func assertDBHasNo(t *testing.T, db *store.DB, needle string) {
	t.Helper()
	rows, err := db.SQL().Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	rows.Close()

	for _, tbl := range tables {
		cols, err := db.SQL().Query(`SELECT name FROM pragma_table_info(?)`, tbl)
		if err != nil {
			continue
		}
		var names []string
		for cols.Next() {
			var n string
			if err := cols.Scan(&n); err != nil {
				break
			}
			names = append(names, n)
		}
		cols.Close()
		for _, c := range names {
			var n int
			q := fmt.Sprintf(`SELECT count(*) FROM %q WHERE CAST(%q AS TEXT) LIKE ?`, tbl, c)
			if err := db.SQL().QueryRow(q, "%"+needle+"%").Scan(&n); err != nil {
				continue
			}
			if n != 0 {
				t.Fatalf("credential reached %s.%s", tbl, c)
			}
		}
	}
}

// corpusSecret takes a synthetic credential from the measured corpus rather
// than inventing one — testdata/secrets is the single sanctioned home for
// credential-shaped strings, and it is excluded from the public repo.
func corpusSecret(t *testing.T) (value, kind string) {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "testdata", "secrets", "corpus.jsonl"))
	if err != nil {
		// Absent by design in the public repo; see internal/index/ruleset.go.
		// Skipping says that, where failing would only say the tree is split.
		t.Skipf("the measured corpus is not in this tree (%v); it is excluded from the public repo", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var rec struct {
			Kind, Expect, Text string
		}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		if rec.Expect != "secret" || rec.Kind != "stripe_secret" {
			continue
		}
		if _, after, ok := strings.Cut(rec.Text, "sk_"); ok {
			return "sk_" + strings.Fields(after)[0], rec.Kind
		}
	}
	t.Fatal("no stripe_secret record in the corpus")
	return "", ""
}

// flakyReader has three sessions, one of which cannot be read.
type flakyReader struct{}

func (f *flakyReader) Runtime() adapter.Runtime { return adapter.Hermes }

func (f *flakyReader) Scan(context.Context) (ScanResult, error) {
	return ScanResult{
		Runtime: adapter.Hermes,
		Status:  StoreOK,
		Refs: []Ref{
			{Runtime: adapter.Hermes, SessionID: "good-1", Path: "/tmp/a", MTime: time.Unix(1, 0), Size: 1},
			{Runtime: adapter.Hermes, SessionID: "bad", Path: "/tmp/b", MTime: time.Unix(1, 0), Size: 1},
			{Runtime: adapter.Hermes, SessionID: "good-2", Path: "/tmp/c", MTime: time.Unix(1, 0), Size: 1},
		},
	}, nil
}

func (f *flakyReader) Read(_ context.Context, ref Ref) (Session, error) {
	if ref.SessionID == "bad" {
		return Session{}, errors.New("transcript is not JSON")
	}
	return Session{
		Runtime:   adapter.Hermes,
		SessionID: ref.SessionID,
		Path:      ref.Path,
		Title:     ref.SessionID,
	}, nil
}

// A machine with none of the five runtimes installed is a machine backfill has
// to succeed on. It is also the only machine CI will ever have.
func TestNewRunnerOnABareMachine(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	env := fixtureEnv(t)
	env.Exec = &detect.FakeExec{}

	r := NewRunner(index.New(db, nil), env)
	if len(r.Readers) != 5 {
		t.Fatalf("%d readers, want one per runtime", len(r.Readers))
	}

	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("a bare machine must not be an error: %v", err)
	}
	scanned, indexed, _, failed := rep.Totals()
	if scanned != 0 || indexed != 0 || failed != 0 {
		t.Fatalf("scanned %d indexed %d failed %d on a machine with no runtimes", scanned, indexed, failed)
	}
	for _, rr := range rep.Runtimes {
		if rr.Status != StoreAbsent {
			t.Errorf("%s: status %q, want absent — nothing is installed", rr.Runtime, rr.Status)
		}
		if len(rr.Roots) == 0 {
			t.Errorf("%s: reported nothing without saying where it looked", rr.Runtime)
		}
	}
	if rt, share := rep.Dominant(); rt != "" || share != 0 {
		t.Errorf("Dominant on an empty corpus: %q %v", rt, share)
	}
}
