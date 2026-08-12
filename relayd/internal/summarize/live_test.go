package summarize_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/search"
	"github.com/luthor007/relay/relayd/internal/store"
	"github.com/luthor007/relay/relayd/internal/summarize"
)

// countingFacts records that extraction ran, and against which session.
type countingFacts struct {
	scopes []summarize.FactScope
}

func (c *countingFacts) Extract(_ context.Context, s summarize.FactScope) (summarize.FactResult, error) {
	c.scopes = append(c.scopes, s)
	return summarize.FactResult{}, nil
}

func newLive(t *testing.T, db *store.DB, model *fakeModel, fx summarize.FactExtractor) *summarize.Live {
	t.Helper()
	s := newSummarizer(t, db, model, search.NewHashEmbedder(store.EmbeddingDims))
	var nar *summarize.Narrator
	if model != nil {
		nar = summarize.NewNarrator(summarize.NarratorOptions{Model: model})
	} else {
		nar = summarize.NewNarrator(summarize.NarratorOptions{})
	}
	l, err := summarize.NewLive(summarize.LiveOptions{Summarizer: s, Narrator: nar, Facts: fx})
	if err != nil {
		t.Fatalf("new live: %v", err)
	}
	return l
}

func feed(t *testing.T, l *summarize.Live, events []event.Event) *summarize.Outcome {
	t.Helper()
	ctx := context.Background()
	var last *summarize.Outcome
	for _, ev := range events {
		out, err := l.Handle(ctx, ev)
		if err != nil {
			t.Fatalf("handle %s: %v", ev.Kind(), err)
		}
		if out != nil {
			last = out
		}
	}
	return last
}

// MEMORY.md §4, "Live": every TurnCompleted writes a turn summary, updates the
// session row, and re-runs fact extraction against that session only.
func TestEveryCompletedTurnDoesAllThreeThings(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	facts := &countingFacts{}
	l := newLive(t, db, nil, facts)
	l.Bind("claude-code", "s1", summarize.Pointer{Path: "/home/u/.claude/projects/api/s1.jsonl", ByteOffset: 8192})

	out := feed(t, l, passingTurn())
	if out == nil {
		t.Fatal("no outcome at the turn boundary")
	}

	// 1. a turn summary, embedded, pointing at the transcript.
	if !out.Written() {
		t.Fatalf("nothing written: %s", out.Skipped)
	}
	sum, err := db.GetSummary(ctx, out.SummaryID)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum.Kind != store.SummaryCluster {
		t.Fatalf("kind %q — a turn is a cluster-grain summary", sum.Kind)
	}
	if sum.Path == "" || sum.ByteOffset != 8192 {
		t.Fatalf("lost the pointer: %+v", sum)
	}
	if !strings.Contains(strings.ToLower(sum.Text), "bash") {
		t.Fatalf("summary says nothing about the turn: %q", sum.Text)
	}

	// 2. the session row.
	row, err := db.GetSessionIndex(ctx, "claude-code", "s1")
	if err != nil {
		t.Fatalf("session row: %v", err)
	}
	if row.Messages != 1 || row.ToolCalls != 1 {
		t.Fatalf("counts not updated: %+v", row)
	}
	if row.Title == "" {
		t.Fatalf("no title on the row: %+v", row)
	}
	if row.TokensTotal == nil || *row.TokensTotal != 34000 {
		t.Fatalf("usage not carried: %+v", row.TokensTotal)
	}

	// 3. fact extraction, scoped to this session and no other.
	if len(facts.scopes) != 1 {
		t.Fatalf("fact extraction ran %d times", len(facts.scopes))
	}
	if facts.scopes[0] != (summarize.FactScope{Runtime: "claude-code", SessionID: "s1"}) {
		t.Fatalf("wrong scope: %+v", facts.scopes[0])
	}

	// And the spoken line comes off the same digest as the summary, so the two
	// cannot disagree.
	if out.Speech.Text == "" {
		t.Fatal("no speech")
	}
	if !out.Speech.WithinCap() {
		t.Fatalf("%d chars: %q", len([]rune(out.Speech.Text)), out.Speech.Text)
	}
}

// A second turn on the same session accumulates rather than replacing.
func TestSecondTurnAccumulates(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	l := newLive(t, db, nil, nil)

	feed(t, l, passingTurn())

	second := make([]event.Event, 0, 3)
	m := func(seq int) event.Meta {
		v := meta(seq)
		v.Turn = "t2"
		return v
	}
	second = append(second,
		event.ToolStarted{Meta: m(10), ID: "x", Tool: "Edit", Target: "store/index.go"},
		event.ToolOutput{Meta: m(11), ID: "x", Status: event.ToolCompleted},
		event.TurnCompleted{Meta: m(12), OK: true, StopReason: event.StopEndTurn})
	out := feed(t, l, second)
	if !out.Written() {
		t.Fatalf("second turn not written: %s", out.Skipped)
	}

	row, err := db.GetSessionIndex(ctx, "claude-code", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if row.Messages != 2 || row.ToolCalls != 2 {
		t.Fatalf("counts: %+v", row)
	}
	var n int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT count(*) FROM summary WHERE runtime = 'claude-code' AND session_id = 's1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("%d summaries for two turns", n)
	}
}

// A replayed turn is not news and it is not new data either: ACP's
// session/load replays the whole conversation before it resolves, and indexing
// it would write a second copy of history that backfill already owns.
func TestReplayedTurnsAreNotIndexed(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	l := newLive(t, db, nil, nil)

	replayed := make([]event.Event, 0, len(passingTurn()))
	for _, ev := range passingTurn() {
		switch e := ev.(type) {
		case event.ToolStarted:
			e.Replay = true
			replayed = append(replayed, e)
		case event.TurnCompleted:
			e.Replay = true
			replayed = append(replayed, e)
		default:
			replayed = append(replayed, ev)
		}
	}
	out := feed(t, l, replayed)
	if out == nil {
		t.Fatal("no outcome")
	}
	if out.Written() {
		t.Fatal("a replayed turn was written to the index")
	}
	if !strings.Contains(out.Skipped, "replay") {
		t.Fatalf("skip reason: %q", out.Skipped)
	}
	var n int
	if err := db.SQL().QueryRowContext(ctx, `SELECT count(*) FROM summary`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d summaries written from a replay", n)
	}
}

// The same TurnCompleted arriving twice — a reconnect, a duplicated bus
// delivery — must not double-index.
func TestDuplicateTurnCompletedIsIgnored(t *testing.T) {
	db := openDB(t)
	l := newLive(t, db, nil, nil)
	events := passingTurn()

	first := feed(t, l, events)
	if !first.Written() {
		t.Fatal("first turn not written")
	}
	again, err := l.Handle(context.Background(), events[len(events)-1])
	if err != nil {
		t.Fatal(err)
	}
	if again.Written() {
		t.Fatal("indexed the same turn twice")
	}
	if !strings.Contains(again.Skipped, "already") {
		t.Fatalf("skip reason: %q", again.Skipped)
	}
}

// The live path has no reader in front of it, so it is the first thing to see
// the text and it writes the marker. A key that arrives in a tool target must
// not reach the index or the model.
func TestLiveTurnRedactsAndMarks(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	const key = "glpat-TESTONLYneverIssuedToAnybody03"
	model := &fakeModel{Reply: "Wrote the key to the environment file."}
	l := newLive(t, db, model, nil)
	l.Bind("codex", "thread-9", summarize.Pointer{Path: "/home/u/.codex/sessions/x.jsonl"})

	m := func(seq int) event.Meta {
		return event.Meta{Runtime: "codex", Session: "thread-9", Turn: "t1", At: base.Add(time.Duration(seq) * time.Second)}
	}
	out := feed(t, l, []event.Event{
		event.ToolStarted{Meta: m(0), ID: "1", Tool: "Bash", Target: "echo GITLAB_TOKEN=" + key + " >> .env"},
		event.ToolOutput{Meta: m(1), ID: "1", Status: event.ToolCompleted},
		event.TurnCompleted{Meta: m(2), OK: true, StopReason: event.StopEndTurn},
	})
	if !out.Written() {
		t.Fatalf("not written: %s", out.Skipped)
	}

	for _, p := range model.allPrompts() {
		if strings.Contains(p, key) {
			t.Fatalf("key sent to a model:\n%s", p)
		}
	}
	sum, err := db.GetSummary(ctx, out.SummaryID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sum.Text, key) {
		t.Fatalf("key in the index: %q", sum.Text)
	}
	markers, err := db.ListSecretMarkers(ctx, "codex", "thread-9")
	if err != nil {
		t.Fatal(err)
	}
	if len(markers) == 0 {
		t.Fatal("no marker for a live finding")
	}
	if markers[0].VaultID != "" {
		t.Fatalf("marker claimed a vault entry: %+v", markers[0])
	}
}

// A turn summary written from events must be findable by the same hybrid
// search the backfilled ones are, or the live path has produced a write-only
// index.
func TestLiveTurnsAreSearchable(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	l := newLive(t, db, nil, nil)
	feed(t, l, failingTurn())

	s, err := search.New(search.Options{DB: db, Embedder: search.NewHashEmbedder(store.EmbeddingDims)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Search(ctx, search.Query{Text: "go build ./auth/..."})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatalf("live turn not searchable; degraded=%v", res.Degraded)
	}
	if !res.Hybrid() {
		t.Fatalf("degraded: %v", res.Degraded)
	}
}

func TestForgetDropsInFlightState(t *testing.T) {
	db := openDB(t)
	l := newLive(t, db, nil, nil)
	events := passingTurn()
	feed(t, l, events[:len(events)-1]) // everything but the boundary

	l.Forget("claude-code", "s1")

	// After forgetting, the completion arrives with no digest behind it. It is
	// still handled — the boundary is real — but it carries only what that one
	// event says.
	out, err := l.Handle(context.Background(), events[len(events)-1])
	if err != nil {
		t.Fatal(err)
	}
	if out == nil {
		t.Fatal("no outcome")
	}
	if len(out.Digest.Tools) != 0 {
		t.Fatalf("state survived Forget: %+v", out.Digest.Tools)
	}
}

func TestNonBoundaryEventsProduceNoOutcome(t *testing.T) {
	db := openDB(t)
	l := newLive(t, db, nil, nil)
	for _, ev := range passingTurn()[:4] {
		out, err := l.Handle(context.Background(), ev)
		if err != nil {
			t.Fatal(err)
		}
		if out != nil {
			t.Fatalf("%s produced an outcome", ev.Kind())
		}
	}
}
