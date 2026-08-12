package orchestrator_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/facts"
	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/orchestrator"
	"github.com/luthor007/relay/relayd/internal/store"
)

func openFacts(t *testing.T) *facts.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	f, err := facts.Open(db, facts.Options{Redactor: facts.Detector()})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestRememberLandsInTheFactTierWithEvidence.
//
// MEMORY.md §5's first rule is that every fact carries evidence — a fact that
// cannot point at where it came from is deleted rather than kept at low
// confidence. "The user told me" is real provenance, so the utterance itself is
// the evidence, with the session it was said in. A month later the console can
// show the sentence next to the fact, which is the difference between a fact
// the user can check and one they can only trust.
func TestRememberLandsInTheFactTierWithEvidence(t *testing.T) {
	store := openFacts(t)
	nb := orchestrator.NotebookIn(store)

	err := nb.Remember(t.Context(), orchestrator.Fact{
		Text:    "deploys go out on Tuesdays",
		Session: "sess_1",
		At:      time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.List(t.Context(), facts.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("%d facts", len(got))
	}
	if got[0].Text != "deploys go out on Tuesdays" {
		t.Errorf("text = %q", got[0].Text)
	}
	if len(got[0].Evidence) == 0 {
		t.Fatal("no evidence; the tier should have refused this rather than storing it")
	}
	if got[0].Evidence[0].SessionID != "sess_1" {
		t.Errorf("evidence session = %q", got[0].Evidence[0].SessionID)
	}
	// Said directly, so high confidence — but not certain. The user can be
	// wrong and can change their mind, and §5's decay applies here too.
	if got[0].Confidence >= 1 || got[0].Confidence < 0.5 {
		t.Errorf("confidence = %v", got[0].Confidence)
	}
}

func TestTheNotebookFilesFactsByPredicate(t *testing.T) {
	store := openFacts(t)
	nb := orchestrator.NotebookIn(store)

	for _, text := range []string{
		"deploys on Fly.io",
		"prefers Go over TypeScript for daemons",
		"uses Stripe for payments",
	} {
		if err := nb.Remember(t.Context(), orchestrator.Fact{
			Text: text, Session: "s", At: time.Now(),
		}); err != nil {
			t.Fatalf("%q: %v", text, err)
		}
	}

	got, _ := store.List(t.Context(), facts.Filter{})
	seen := map[facts.Predicate]bool{}
	for _, f := range got {
		seen[f.Predicate] = true
	}
	for _, want := range []facts.Predicate{facts.DeploysOn, facts.Prefers, facts.Uses} {
		if !seen[want] {
			t.Errorf("nothing was filed under %q; got %v", want, seen)
		}
	}
}

// TestAnEmptyFactIsRefused — the tool says so rather than reporting success for
// something that was dropped.
func TestAnEmptyFactIsRefused(t *testing.T) {
	nb := orchestrator.NotebookIn(openFacts(t))
	if err := nb.Remember(t.Context(), orchestrator.Fact{Session: "s"}); err == nil {
		t.Fatal("an empty fact was accepted")
	}
}

// TestTheRememberToolReachesTheTier is the join: the model's tool call ends up
// in the fact table, not in a map that gets dropped at shutdown.
func TestTheRememberToolReachesTheTier(t *testing.T) {
	store := openFacts(t)
	box := orchestrator.ToolboxFor(orchestrator.Deps{
		Notebook: orchestrator.NotebookIn(store),
	})
	// describe_runtime is always present — it depends on nothing — so the
	// dependency-gated tool is the second one.
	if len(box) != 2 {
		t.Fatalf("%d tools", len(box))
	}

	res, err := runTool(t, box, t.Context(), llm.ToolCall{
		ID: "c", Name: orchestrator.ToolRemember,
		Input: []byte(`{"text":"the CRC on the glasses link is MODBUS, not ARC","subject":"glasses"}`),
	})
	if err != nil || res.IsError {
		t.Fatalf("res=%+v err=%v", res, err)
	}

	got, _ := store.List(t.Context(), facts.Filter{})
	if len(got) != 1 || !strings.Contains(got[0].Text, "MODBUS") {
		t.Fatalf("facts = %+v", got)
	}
}
