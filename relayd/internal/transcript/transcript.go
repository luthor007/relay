package transcript

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/index"
)

// Finding is `internal/index`'s type rather than a second one, because there is
// exactly one measured ruleset and MEMORY.md §12.2's recall figures belong to
// it.
type Finding = index.Finding

// Redactor replaces credentials with markers.
//
// One method, and it is an interface so this package does not own a second copy
// of the ruleset — `*index.Detector` is the implementation. `internal/summarize`
// takes the identical shape for the identical reason: detection happens before
// anything is written, never after, because an embedded key cannot be
// unembedded and a key posted to a model provider has already left the machine.
type Redactor interface {
	Redact(text string) (string, []Finding)
}

// Detector returns the measured detector, for a caller with no reason to build
// its own.
func Detector() Redactor { return index.MustDetector() }

// Segment is one settled stretch of a transcript.
type Segment struct {
	Start, End time.Duration
	Text       string
	Speaker    string
	Confidence float64
	Source     Source
	// Gap marks a hole where audio was lost. Text is empty and [Segment.Render]
	// prints a marker, because a transcript that closes over its own holes is
	// the thing SYSTEM.md's "never emit what you cannot observe" rule exists to
	// prevent.
	Gap       bool
	GapReason string
}

// Render is the segment as it appears in the transcript text.
func (s Segment) Render() string {
	if s.Gap {
		return fmt.Sprintf("[relay:gap %s]", s.End-s.Start)
	}
	if s.Speaker == "" {
		return s.Text
	}
	return s.Speaker + ": " + s.Text
}

// Transcript is what a recognition produced.
type Transcript struct {
	StreamID   string
	StartedAt  time.Time
	Source     Source
	Recognizer string
	Segments   []Segment

	// Redactions is how many credentials were replaced with markers. The
	// findings themselves are not kept here: MEMORY.md §6's flow needs the
	// value to make a vault proposal and this type is written to a database.
	Redactions int
	// Notes are everything the pipeline could not observe, in words — a
	// recogniser that does not diarise, a provider that reports no confidence,
	// the gaps. Carried rather than dropped, because "we guessed" and "we
	// measured" are different facts.
	Notes []string
}

// Text is the transcript as one string, holes included.
func (t Transcript) Text() string {
	parts := make([]string, 0, len(t.Segments))
	for _, s := range t.Segments {
		if r := s.Render(); r != "" {
			parts = append(parts, r)
		}
	}
	return strings.Join(parts, "\n")
}

// Duration is the span from the first segment to the last.
func (t Transcript) Duration() time.Duration {
	if len(t.Segments) == 0 {
		return 0
	}
	return t.Segments[len(t.Segments)-1].End
}

// Speakers is every distinct speaker label, sorted. Empty when the recogniser
// did not diarise — which is a fact about the recogniser and is in [Notes].
func (t Transcript) Speakers() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range t.Segments {
		if s.Speaker == "" || seen[s.Speaker] {
			continue
		}
		seen[s.Speaker] = true
		out = append(out, s.Speaker)
	}
	sort.Strings(out)
	return out
}

// Gaps is how many holes the transcript carries.
func (t Transcript) Gaps() int {
	n := 0
	for _, s := range t.Segments {
		if s.Gap {
			n++
		}
	}
	return n
}

// Complete reports whether the transcript covers all of its audio.
func (t Transcript) Complete() bool { return t.Gaps() == 0 }

// BuilderOptions configures a [Builder].
type BuilderOptions struct {
	StreamID   string
	StartedAt  time.Time
	Source     Source
	Recognizer string
	// Redact is required. See [ErrNoRedactor].
	Redact Redactor
}

// Builder turns a stream of [Result] into a [Transcript].
//
// Two rules it enforces rather than documents:
//
//   - **Only finals become segments.** Partials exist so routing can start
//     before the speaker stops; storing them would put four copies of every
//     sentence in the memory.
//   - **Redaction happens on the way in.** [Builder.Add] runs the detector
//     before the text is held, so there is no moment at which an un-redacted
//     credential is inside a Transcript that something else could read.
type Builder struct {
	opts     BuilderOptions
	segments []Segment
	findings []Finding
	notes    []string
	noteSeen map[string]bool
}

// NewBuilder builds a builder. It refuses without a redactor, which is the same
// structural trick `internal/summarize` uses: there is no code path that writes
// a transcript without having looked for secrets first.
func NewBuilder(o BuilderOptions) (*Builder, error) {
	if o.Redact == nil {
		return nil, ErrNoRedactor
	}
	return &Builder{opts: o, noteSeen: map[string]bool{}}, nil
}

// Add takes one result. Partials are counted and dropped.
func (b *Builder) Add(r Result) {
	if !r.Final {
		return
	}
	if r.Gap {
		b.segments = append(b.segments, Segment{
			Start: r.Start, End: r.End, Speaker: r.Speaker, Source: r.Source,
			Gap: true, GapReason: r.GapReason,
		})
		b.note("audio was missing for " + (r.End - r.Start).String() + ": " + r.GapReason)
		return
	}
	if strings.TrimSpace(r.Text) == "" {
		return
	}

	redacted, found := b.opts.Redact.Redact(r.Text)
	if len(found) > 0 {
		b.findings = append(b.findings, found...)
		for _, f := range found {
			b.note(f.Sentence())
		}
	}
	src := r.Source
	if src == "" {
		src = b.opts.Source
	}
	b.segments = append(b.segments, Segment{
		Start: r.Start, End: r.End, Text: redacted, Speaker: r.Speaker,
		Confidence: r.Confidence, Source: src,
	})
}

// Note records something the pipeline could not observe.
func (b *Builder) Note(s string) { b.note(s) }

func (b *Builder) note(s string) {
	if s == "" || b.noteSeen[s] {
		return
	}
	b.noteSeen[s] = true
	b.notes = append(b.notes, s)
}

// Findings are the credentials that were replaced with markers. They carry the
// matched value, so they are in memory only: MEMORY.md §6's vault proposal flow
// needs it and nothing else may keep it.
func (b *Builder) Findings() []Finding { return append([]Finding(nil), b.findings...) }

// Build returns the transcript.
func (b *Builder) Build() Transcript {
	segs := append([]Segment(nil), b.segments...)
	sort.SliceStable(segs, func(i, j int) bool { return segs[i].Start < segs[j].Start })
	return Transcript{
		StreamID:   b.opts.StreamID,
		StartedAt:  b.opts.StartedAt,
		Source:     b.opts.Source,
		Recognizer: b.opts.Recognizer,
		Segments:   segs,
		Redactions: len(b.findings),
		Notes:      append([]string(nil), b.notes...),
	}
}
