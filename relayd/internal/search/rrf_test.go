package search_test

import (
	"math"
	"testing"

	"github.com/luthor007/relay/relayd/internal/search"
	"github.com/luthor007/relay/relayd/internal/store"
)

func hits(ids ...int64) []store.Hit {
	out := make([]store.Hit, len(ids))
	for i, id := range ids {
		out[i] = store.Hit{SummaryID: id, Rank: i + 1}
	}
	return out
}

func TestFuseArithmetic(t *testing.T) {
	got := search.Fuse(60,
		search.List{Name: "a", Hits: hits(1, 2)},
		search.List{Name: "b", Hits: hits(2, 1)},
	)
	if len(got) != 2 {
		t.Fatalf("want 2 fused, got %d", len(got))
	}
	// Both documents were rank 1 in one list and rank 2 in the other, so both
	// score 1/61 + 1/62 and the tie breaks on id.
	want := 1.0/61 + 1.0/62
	for _, f := range got {
		if math.Abs(f.Score-want) > 1e-12 {
			t.Fatalf("summary %d: score %v, want %v", f.SummaryID, f.Score, want)
		}
	}
	if got[0].SummaryID != 1 {
		t.Fatalf("tie broke unstably: %v", got)
	}
}

func TestFuseAgreementBeatsASingleFirstPlace(t *testing.T) {
	// This is the property that makes fusion worth doing: a document both
	// retrievers put near the top outranks one that only a single retriever
	// loved. 2/62 > 1/61 + 1/70.
	got := search.Fuse(search.DefaultRRFK,
		search.List{Name: "bm25", Hits: hits(1, 2)},
		search.List{Name: "cosine", Hits: hits(3, 2)},
	)
	if got[0].SummaryID != 2 {
		t.Fatalf("agreement lost: %v", got)
	}
}

func TestFuseRecordsAbsenceAsAbsence(t *testing.T) {
	got := search.Fuse(search.DefaultRRFK,
		search.List{Name: "bm25", Hits: hits(7)},
		search.List{Name: "cosine", Hits: hits(8)},
	)
	for _, f := range got {
		if len(f.Ranks) != 1 {
			t.Fatalf("summary %d has ranks %v, want exactly one", f.SummaryID, f.Ranks)
		}
		if _, ok := f.Rank("nope"); ok {
			t.Fatal("a list that never ran reported a rank")
		}
	}
	// Zero must never be usable as "ranked here": absence is a missing key.
	if r, ok := got[0].Rank("cosine"); ok && r == 0 {
		t.Fatal("absence encoded as rank 0")
	}
}

func TestFuseWeights(t *testing.T) {
	// Weighting the lexical half up must be able to overturn a dense-only
	// first place, because that is how the router expresses "this query is an
	// identifier lookup".
	got := search.Fuse(search.DefaultRRFK,
		search.List{Name: "bm25", Hits: hits(1), Weight: 4},
		search.List{Name: "cosine", Hits: hits(2), Weight: 1},
	)
	if got[0].SummaryID != 1 {
		t.Fatalf("weight ignored: %v", got)
	}
}

func TestFuseDeduplicatesWithinAList(t *testing.T) {
	got := search.Fuse(search.DefaultRRFK, search.List{
		Name: "bm25",
		Hits: []store.Hit{{SummaryID: 1, Rank: 3}, {SummaryID: 1, Rank: 1}},
	})
	if len(got) != 1 {
		t.Fatalf("want 1 fused, got %d", len(got))
	}
	if r := got[0].Ranks["bm25"]; r != 1 {
		t.Fatalf("kept rank %d, want the better one (1)", r)
	}
	if math.Abs(got[0].Score-1.0/61) > 1e-12 {
		t.Fatalf("double-counted a duplicate: %v", got[0].Score)
	}
}

func TestFuseUsesSlicePositionWhenRankIsUnset(t *testing.T) {
	got := search.Fuse(search.DefaultRRFK, search.List{
		Name: "bm25",
		Hits: []store.Hit{{SummaryID: 9}, {SummaryID: 8}},
	})
	if got[0].SummaryID != 9 || got[0].Ranks["bm25"] != 1 {
		t.Fatalf("position not used as rank: %v", got)
	}
}

func TestFuseIsDeterministic(t *testing.T) {
	build := func() []search.Fused {
		return search.Fuse(search.DefaultRRFK,
			search.List{Name: "bm25", Hits: hits(5, 4, 3, 2, 1)},
			search.List{Name: "cosine", Hits: hits(1, 2, 3, 4, 5)},
		)
	}
	first := build()
	for i := 0; i < 20; i++ {
		next := build()
		for j := range first {
			if first[j].SummaryID != next[j].SummaryID {
				t.Fatalf("run %d differs at %d: %d vs %d", i, j, first[j].SummaryID, next[j].SummaryID)
			}
		}
	}
}
