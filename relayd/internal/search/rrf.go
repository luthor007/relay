package search

import (
	"sort"

	"github.com/luthor007/relay/relayd/internal/store"
)

// DefaultRRFK is the rank-fusion constant. 60 is the value from the original
// reciprocal-rank-fusion paper and the one every later comparison uses as its
// baseline; it is here as a named constant rather than a literal because
// changing it changes result ordering and that should be a visible decision.
//
// What k does: score = 1/(k+rank). A large k flattens the curve, so the two
// lists have to agree to move anything; a small k makes rank 1 dominate, which
// reintroduces exactly the "one retriever wins outright" behaviour hybrid search
// exists to avoid.
const DefaultRRFK = 60.0

// List is one retriever's ranked output.
//
// Weight scales that retriever's whole contribution. It defaults to 1 and is not
// a score normalisation — the ranks are still fused as ranks. It exists so a
// caller can say "trust the lexical half more for this query", which is a policy
// decision the router makes and this package does not.
type List struct {
	Name   string
	Hits   []store.Hit
	Weight float64
}

// Fused is one document's fused result.
type Fused struct {
	SummaryID int64
	// Score is the sum of weight/(k+rank) across the lists that returned it.
	// It is meaningful only relative to other scores from the same Fuse call.
	Score float64
	// Ranks is the 1-based position this document held in each list that
	// returned it. A list that did not return it is absent, not zero — "ranked
	// last" and "never retrieved" are different facts and the console shows the
	// difference.
	Ranks map[string]int
	// Scores is each list's own raw score, kept for display. BM25 and L2
	// distance are not comparable with each other; do not add them.
	Scores map[string]float64
}

// Rank returns this document's position in the named list, and whether the list
// returned it at all.
func (f Fused) Rank(list string) (int, bool) {
	r, ok := f.Ranks[list]
	return r, ok
}

// Fuse combines ranked lists by reciprocal-rank fusion.
//
// The two halves of MEMORY.md §3's retrieval report scores that cannot be
// compared: SQLite's bm25() is negative and unbounded, vec0's distance is a
// positive L2 magnitude. Fusing on rank sidesteps the whole normalisation
// problem, which is why this takes position and not similarity.
//
// A hit's Rank field is used when set, and its position in the slice otherwise,
// so a caller that has already ranked does not have to renumber.
//
// Ties break on the best single rank the document achieved, and then on summary
// id, so the order is total and stable across runs. A search that returns two
// different orderings for the same corpus is a search nobody trusts.
func Fuse(k float64, lists ...List) []Fused {
	if k <= 0 {
		k = DefaultRRFK
	}
	byID := map[int64]*Fused{}
	best := map[int64]int{}

	for _, l := range lists {
		w := l.Weight
		if w == 0 {
			w = 1
		}
		for i, h := range l.Hits {
			rank := h.Rank
			if rank <= 0 {
				rank = i + 1
			}
			f, ok := byID[h.SummaryID]
			if !ok {
				f = &Fused{
					SummaryID: h.SummaryID,
					Ranks:     map[string]int{},
					Scores:    map[string]float64{},
				}
				byID[h.SummaryID] = f
				best[h.SummaryID] = rank
			}
			// A list that returns the same document twice must not be counted
			// twice; keep the better rank.
			if prev, dup := f.Ranks[l.Name]; dup {
				if rank >= prev {
					continue
				}
				f.Score -= w / (k + float64(prev))
			}
			f.Ranks[l.Name] = rank
			f.Scores[l.Name] = h.Score
			f.Score += w / (k + float64(rank))
			if rank < best[h.SummaryID] {
				best[h.SummaryID] = rank
			}
		}
	}

	out := make([]Fused, 0, len(byID))
	for _, f := range byID {
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		bi, bj := best[out[i].SummaryID], best[out[j].SummaryID]
		if bi != bj {
			return bi < bj
		}
		return out[i].SummaryID < out[j].SummaryID
	})
	return out
}
