package summarize_test

import (
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/summarize"
)

func turns(n int, gapAfter map[int]time.Duration) []summarize.SourceTurn {
	out := make([]summarize.SourceTurn, n)
	at := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	var offset int64
	for i := 0; i < n; i++ {
		out[i] = summarize.SourceTurn{
			Index: i, Role: "user", At: at, Text: "turn number " + itoa(i),
			ByteOffset: offset, ByteLength: 100,
			Tools: []string{"Bash"},
		}
		offset += 100
		step := time.Minute
		if g, ok := gapAfter[i]; ok {
			step = g
		}
		at = at.Add(step)
	}
	return out
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

// The chunk arithmetic in MEMORY.md §3 — ~22,000 chunks, not ~875,000 — only
// holds if a cluster is many turns. One summary per turn would put the count
// back into six figures and undo the reason for summarising at all.
func TestClusterGroupsManyTurns(t *testing.T) {
	got := summarize.Cluster(turns(20, nil), summarize.ClusterPolicy{})
	if len(got) != 3 {
		t.Fatalf("20 turns became %d clusters, want 3 at the default of %d",
			len(got), summarize.DefaultClusterTurns)
	}
	total := 0
	for _, c := range got {
		total += c.Turns
	}
	if total != 20 {
		t.Fatalf("%d turns survived clustering, want 20", total)
	}
}

// A pause starts a new cluster. A session resumed after an hour is usually a
// different piece of work wearing the same session's name — the same signal
// MEMORY.md §9 uses for compact-versus-new.
func TestClusterSplitsOnAGap(t *testing.T) {
	got := summarize.Cluster(turns(6, map[int]time.Duration{2: 2 * time.Hour}), summarize.ClusterPolicy{})
	if len(got) != 2 {
		t.Fatalf("a two-hour gap produced %d clusters", len(got))
	}
	if got[0].Turns != 3 || got[1].Turns != 3 {
		t.Fatalf("split in the wrong place: %d / %d", got[0].Turns, got[1].Turns)
	}
	if !got[0].EndedAt.Before(got[1].StartedAt) {
		t.Fatal("cluster times overlap")
	}
}

// Every cluster keeps a byte span, because the summary it produces has to point
// back into the transcript. The 3.6 GB stays on disk, in place, unmoved.
func TestClusterKeepsTheByteSpan(t *testing.T) {
	got := summarize.Cluster(turns(10, nil), summarize.ClusterPolicy{MaxTurns: 5})
	if len(got) != 2 {
		t.Fatalf("%d clusters", len(got))
	}
	if got[0].ByteOffset != 0 || got[0].ByteLength != 500 {
		t.Fatalf("first span: %d+%d", got[0].ByteOffset, got[0].ByteLength)
	}
	if got[1].ByteOffset != 500 || got[1].ByteLength != 500 {
		t.Fatalf("second span: %d+%d", got[1].ByteOffset, got[1].ByteLength)
	}
}

func TestClusterCapsItsExcerpt(t *testing.T) {
	long := make([]summarize.SourceTurn, 4)
	for i := range long {
		long[i] = summarize.SourceTurn{
			Role: "agent", Text: strings.Repeat("x", 4000),
			ByteOffset: int64(i * 4000), ByteLength: 4000,
		}
	}
	got := summarize.Cluster(long, summarize.ClusterPolicy{MaxTurns: 10, MaxChars: 5000})
	for _, c := range got {
		if len(c.Excerpt) > 6000 {
			t.Fatalf("excerpt %d chars, cap 5000", len(c.Excerpt))
		}
	}
	if len(got) < 2 {
		t.Fatalf("16k of text stayed in %d cluster(s)", len(got))
	}
}

func TestClusterDeduplicatesTools(t *testing.T) {
	in := turns(4, nil)
	in[1].Tools = []string{"Bash", "Edit"}
	got := summarize.Cluster(in, summarize.ClusterPolicy{})
	if len(got) != 1 {
		t.Fatalf("%d clusters", len(got))
	}
	if len(got[0].Tools) != 2 {
		t.Fatalf("tools %v", got[0].Tools)
	}
}

func TestClusterOfNothing(t *testing.T) {
	if got := summarize.Cluster(nil, summarize.ClusterPolicy{}); len(got) != 0 {
		t.Fatalf("%d clusters from no turns", len(got))
	}
}
