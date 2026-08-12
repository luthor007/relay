package summarize

import (
	"strings"
	"time"
)

// Clustering defaults. They exist to hit MEMORY.md §3's arithmetic: ~22,000
// chunks across the whole corpus, roughly one summary per session plus one per
// turn-cluster. One summary per *turn* would put the chunk count back into six
// figures and undo the entire reason for summarising first.
const (
	DefaultClusterTurns = 8
	DefaultClusterGap   = 30 * time.Minute
	DefaultClusterChars = 6000
)

// SourceTurn is one turn as a backfill reader found it. It is the input to
// clustering and carries a pointer into the transcript, never a copy of it.
type SourceTurn struct {
	Index      int
	Role       string // user | agent
	At         time.Time
	Text       string
	Tools      []string
	ByteOffset int64
	ByteLength int64
}

// ClusterPolicy decides where one turn-cluster ends and the next begins.
type ClusterPolicy struct {
	// MaxTurns caps a cluster's length.
	MaxTurns int
	// MaxGap starts a new cluster after a pause. A session resumed after an
	// hour is usually a different piece of work wearing the same session's
	// name, which is the same signal MEMORY.md §9 uses for compact-versus-new.
	MaxGap time.Duration
	// MaxChars caps how much text a cluster feeds the summariser.
	MaxChars int
}

func (p ClusterPolicy) withDefaults() ClusterPolicy {
	if p.MaxTurns <= 0 {
		p.MaxTurns = DefaultClusterTurns
	}
	if p.MaxGap <= 0 {
		p.MaxGap = DefaultClusterGap
	}
	if p.MaxChars <= 0 {
		p.MaxChars = DefaultClusterChars
	}
	return p
}

// ClusterInput is one turn-cluster, ready to summarise.
type ClusterInput struct {
	Index      int
	ByteOffset int64
	ByteLength int64
	StartedAt  time.Time
	EndedAt    time.Time
	Turns      int
	Tools      []string
	// Excerpt is the text the summariser sees.
	Excerpt string
}

// Cluster groups turns into the chunks that get summarised and embedded.
//
// The byte span of a cluster is the span of its turns, so every summary keeps
// a pointer back into the original transcript and the raw bytes stay on disk,
// in place, unmoved.
func Cluster(turns []SourceTurn, p ClusterPolicy) []ClusterInput {
	p = p.withDefaults()
	var out []ClusterInput

	var cur *ClusterInput
	var body strings.Builder
	var chars int
	seenTool := map[string]bool{}

	flush := func() {
		if cur == nil {
			return
		}
		cur.Excerpt = strings.TrimSpace(body.String())
		out = append(out, *cur)
		cur = nil
		body.Reset()
		chars = 0
		seenTool = map[string]bool{}
	}

	for _, t := range turns {
		start := cur == nil
		if !start {
			if cur.Turns >= p.MaxTurns {
				start = true
			} else if chars >= p.MaxChars {
				start = true
			} else if !t.At.IsZero() && !cur.EndedAt.IsZero() && t.At.Sub(cur.EndedAt) > p.MaxGap {
				start = true
			}
		}
		if start {
			flush()
			cur = &ClusterInput{
				Index:      len(out),
				ByteOffset: t.ByteOffset,
				StartedAt:  t.At,
			}
		}

		cur.Turns++
		if !t.At.IsZero() {
			if cur.StartedAt.IsZero() || t.At.Before(cur.StartedAt) {
				cur.StartedAt = t.At
			}
			if t.At.After(cur.EndedAt) {
				cur.EndedAt = t.At
			}
		}
		if end := t.ByteOffset + t.ByteLength; end-cur.ByteOffset > cur.ByteLength {
			cur.ByteLength = end - cur.ByteOffset
		}
		for _, tool := range t.Tools {
			if tool == "" || seenTool[tool] {
				continue
			}
			seenTool[tool] = true
			cur.Tools = append(cur.Tools, tool)
		}

		if text := strings.TrimSpace(t.Text); text != "" && chars < p.MaxChars {
			role := t.Role
			if role == "" {
				role = "turn"
			}
			line := role + ": " + clip(Clean(text), p.MaxChars-chars)
			body.WriteString(line)
			body.WriteString("\n")
			chars += len(line)
		}
	}
	flush()
	return out
}
