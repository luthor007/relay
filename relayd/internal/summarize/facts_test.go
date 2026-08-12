package summarize_test

import (
	"context"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/store"
	"github.com/luthor007/relay/relayd/internal/summarize"
)

func seedSummaries(t *testing.T, db *store.DB) {
	t.Helper()
	ctx := context.Background()
	for _, text := range []string{
		"Migrated the auth tables from Firebase to Supabase and redeployed on Vercel.",
		"Wrote the ingest daemon in Go; the dashboard stayed TypeScript.",
	} {
		if _, err := db.PutSummary(ctx, store.Summary{
			Runtime: "claude-code", SessionID: "s1", Path: "/t/s1.jsonl", Text: text,
		}, nil); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

// Facts are proposals with evidence attached, and this package does not write
// them. MEMORY.md §11 puts extraction and the review screen together at step
// 5b, and §5 explains why: an unexamined inference store poisons every routing
// decision downstream, silently, forever.
func TestFactsAreEvidencedProposalsAndNothingIsWritten(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	seedSummaries(t, db)

	model := &fakeModel{Reply: `[
	  {"predicate":"prefers","object":"Supabase","text":"prefers Supabase over Firebase","confidence":0.8},
	  {"predicate":"deploys_on","object":"Vercel","text":"deploys on Vercel","confidence":0.7},
	  {"predicate":"writes","object":"Go","text":"writes Go for daemons","confidence":0.6}
	]`}
	fx := &summarize.LLMFacts{DB: db, Model: model}

	res, err := fx.Extract(ctx, summarize.FactScope{Runtime: "claude-code", SessionID: "s1"})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(res.Facts) != 3 {
		t.Fatalf("%d facts: %+v", len(res.Facts), res.Facts)
	}
	for _, f := range res.Facts {
		// A fact that cannot point at where it came from is deleted, not kept
		// at low confidence.
		if len(f.Evidence) == 0 {
			t.Fatalf("unevidenced fact: %+v", f)
		}
		for _, e := range f.Evidence {
			if e.SessionID != "s1" || e.Path == "" || e.Quote == "" {
				t.Fatalf("evidence does not point anywhere: %+v", e)
			}
		}
		if f.Confidence <= 0 || f.Confidence > 1 {
			t.Fatalf("confidence %v", f.Confidence)
		}
	}

	// Nothing was written to the fact tier.
	var n int
	if err := db.SQL().QueryRowContext(ctx, `SELECT count(*) FROM fact`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d facts written before there is a screen to review them on", n)
	}
}

// Extraction reads the session's own summaries, which are already redacted.
// MEMORY.md §5's last rule — nothing in this tier is a secret — is therefore
// true by construction rather than by a second detection pass.
func TestFactsReadOnlyThatSessionsSummaries(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	seedSummaries(t, db)
	if _, err := db.PutSummary(ctx, store.Summary{
		Runtime: "codex", SessionID: "other", Text: "Something about Rust and Postgres.",
	}, nil); err != nil {
		t.Fatal(err)
	}

	model := &fakeModel{Reply: `[]`}
	fx := &summarize.LLMFacts{DB: db, Model: model}
	if _, err := fx.Extract(ctx, summarize.FactScope{Runtime: "claude-code", SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	p := model.lastPrompt()
	if strings.Contains(p, "Rust") {
		t.Fatalf("another session's summaries leaked into the prompt:\n%s", p)
	}
	if !strings.Contains(p, "Supabase") {
		t.Fatalf("this session's summaries missing:\n%s", p)
	}
}

func TestFactsToleratesFencedJSONAndRefusesJunk(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	seedSummaries(t, db)

	fenced := &summarize.LLMFacts{DB: db, Model: &fakeModel{
		Reply: "```json\n[{\"predicate\":\"uses\",\"object\":\"Stripe\",\"text\":\"uses Stripe for payments\"}]\n```",
	}}
	res, err := fenced.Extract(ctx, summarize.FactScope{Runtime: "claude-code", SessionID: "s1"})
	if err != nil || len(res.Facts) != 1 {
		t.Fatalf("fenced JSON not parsed: %+v %v", res, err)
	}
	if res.Facts[0].Confidence != 0.5 {
		t.Fatalf("missing confidence not defaulted: %v", res.Facts[0].Confidence)
	}

	junk := &summarize.LLMFacts{DB: db, Model: &fakeModel{Reply: "I could not find any facts, sorry!"}}
	res, err = junk.Extract(ctx, summarize.FactScope{Runtime: "claude-code", SessionID: "s1"})
	if err != nil {
		t.Fatalf("unparseable output became an error: %v", err)
	}
	if len(res.Facts) != 0 || res.Skipped == "" {
		t.Fatalf("junk accepted: %+v", res)
	}
}

func TestFactsWithNothingToReadOrNoModel(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)

	empty := &summarize.LLMFacts{DB: db, Model: &fakeModel{Reply: "[]"}}
	res, err := empty.Extract(ctx, summarize.FactScope{Runtime: "claude-code", SessionID: "nope"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ModelCalls != 0 || res.Skipped == "" {
		t.Fatalf("called a model with nothing to read: %+v", res)
	}

	seedSummaries(t, db)
	none := &summarize.LLMFacts{DB: db}
	res, err = none.Extract(ctx, summarize.FactScope{Runtime: "claude-code", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped == "" {
		t.Fatal("no model configured was not reported")
	}

	// The explicit no-op, so "facts are off" appears in the wiring rather than
	// being inferred from a nil pointer.
	res, err = summarize.NoFacts{}.Extract(ctx, summarize.FactScope{})
	if err != nil || len(res.Facts) != 0 || res.Skipped == "" {
		t.Fatalf("NoFacts: %+v %v", res, err)
	}
}

func TestFactsAreCapped(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	seedSummaries(t, db)

	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < 40; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"predicate":"uses","object":"Thing` + itoa(i) + `","text":"uses Thing` + itoa(i) + `"}`)
	}
	b.WriteString("]")

	fx := &summarize.LLMFacts{DB: db, Model: &fakeModel{Reply: b.String()}}
	res, err := fx.Extract(ctx, summarize.FactScope{Runtime: "claude-code", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	// A session that appears to yield forty durable preferences has produced
	// forty guesses.
	if len(res.Facts) != summarize.MaxFactsPerSession {
		t.Fatalf("%d facts, cap is %d", len(res.Facts), summarize.MaxFactsPerSession)
	}
}
