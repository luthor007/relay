package index

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Detector is the secret detector from MEMORY.md §12.2, compiled once.
//
// It runs BEFORE anything is written to the index, never after, because an
// embedded key cannot be unembedded. That ordering is not a convention here —
// [Indexer.Index] cannot write a row without going through [Detector.Redact]
// first, and TestIndexRedactsBeforeWriting is what holds it.
type Detector struct {
	rules []Rule
}

// Rule is one compiled pattern.
type Rule struct {
	ID      string
	Label   string
	Service string
	Tier    Tier

	re *regexp.Regexp
}

// Pattern is the source text of this rule's regexp.
func (r Rule) Pattern() string { return r.re.String() }

// NewDetector compiles the measured ruleset. It fails only if the ruleset in
// ruleset.go stops being RE2-safe, which is a programming error rather than a
// runtime condition.
func NewDetector() (*Detector, error) {
	d := &Detector{rules: make([]Rule, 0, len(ruleSpecs))}
	for _, s := range ruleSpecs {
		re, err := regexp.Compile(s.Pattern)
		if err != nil {
			return nil, fmt.Errorf("index: rule %s: %w", s.ID, err)
		}
		d.rules = append(d.rules, Rule{ID: s.ID, Label: s.Label, Service: s.Service, Tier: s.Tier, re: re})
	}
	return d, nil
}

// MustDetector is NewDetector for package-level initialisation and tests.
func MustDetector() *Detector {
	d, err := NewDetector()
	if err != nil {
		panic(err)
	}
	return d
}

// Rules lists the compiled ruleset, in order.
func (d *Detector) Rules() []Rule { return append([]Rule(nil), d.rules...) }

// Finding is one match.
//
// Value carries the matched text because the vault proposal flow in
// MEMORY.md §6 needs the credential itself to store it. It is in memory only:
// nothing in this package persists it, and it must be logged through
// logx.Secret if it has to be logged at all.
type Finding struct {
	RuleID  string
	Label   string
	Service string
	Tier    Tier

	// Start and End are byte offsets into the text that was scanned — which is
	// the extracted message text, not the transcript file. See
	// [Result.Markers] for why the stored marker points at the session rather
	// than at these offsets.
	Start, End int

	Value string
}

// Preview is the last four characters of the match, which is the most that may
// ever be shown or stored. Matches the vault's own last-four rule.
func (f Finding) Preview() string {
	r := []rune(f.Value)
	if len(r) <= 4 {
		return strings.Repeat("•", len(r))
	}
	return "…" + string(r[len(r)-4:])
}

// Sentence is the marker that replaces the credential everywhere it would
// otherwise have been indexed, summarised or embedded. MEMORY.md §6 words it
// exactly this way: "a Stripe secret key appeared in this session".
func (f Finding) Sentence() string {
	return "a " + f.Label + " appeared in this session"
}

// Marker is the inline placeholder written into the redacted text.
func (f Finding) Marker() string { return "[relay:redacted " + f.Label + "]" }

// Scan returns every match in text, sorted by position. Overlapping matches
// from different rules are all reported — the corpus's own scoring rule is
// "a record counts as caught when any rule matches" — and [Detector.Redact] is
// what merges them.
func (d *Detector) Scan(text string) []Finding {
	if text == "" {
		return nil
	}
	var out []Finding
	for _, r := range d.rules {
		for _, m := range r.re.FindAllStringIndex(text, -1) {
			out = append(out, Finding{
				RuleID:  r.ID,
				Label:   r.Label,
				Service: r.Service,
				Tier:    r.Tier,
				Start:   m[0],
				End:     m[1],
				Value:   text[m[0]:m[1]],
			})
		}
	}
	sortFindings(out)
	return out
}

// ScanTier is Scan restricted to one tier. The vault proposal path calls this
// with TierVendor; nothing else should.
func (d *Detector) ScanTier(text string, tier Tier) []Finding {
	var out []Finding
	for _, f := range d.Scan(text) {
		if f.Tier == tier {
			out = append(out, f)
		}
	}
	return out
}

// Redact replaces every match with its marker and returns the redacted text
// alongside the findings that survived overlap resolution.
//
// Overlaps are real and frequent: `OPENAI_API_KEY=sk-…` matches both
// openai_legacy (tier 1) and assigned_secret (tier 2). The surviving finding
// for a span is the lowest tier, then the longest match, so the marker names
// the vendor rather than the shape whenever a vendor rule fired.
func (d *Detector) Redact(text string) (string, []Finding) {
	all := d.Scan(text)
	if len(all) == 0 {
		return text, nil
	}
	kept := mergeFindings(all)

	var b strings.Builder
	b.Grow(len(text))
	prev := 0
	for _, f := range kept {
		b.WriteString(text[prev:f.Start])
		b.WriteString(f.Marker())
		prev = f.End
	}
	b.WriteString(text[prev:])
	return b.String(), kept
}

// mergeFindings collapses overlapping spans, keeping one representative each.
func mergeFindings(in []Finding) []Finding {
	var out []Finding
	for _, f := range in {
		if len(out) == 0 {
			out = append(out, f)
			continue
		}
		last := &out[len(out)-1]
		if f.Start >= last.End {
			out = append(out, f)
			continue
		}
		// Overlap. Keep whichever describes the span better, and widen the
		// span so the whole credential is covered either way.
		end := last.End
		if f.End > end {
			end = f.End
		}
		if better(f, *last) {
			start := last.Start
			if f.Start < start {
				start = f.Start
			}
			*last = f
			last.Start = start
		}
		last.End = end
	}
	return out
}

// better reports whether a should replace b as a span's representative:
// vendor rules beat shape rules, then the longer match wins.
func better(a, b Finding) bool {
	if a.Tier != b.Tier {
		return a.Tier < b.Tier
	}
	return (a.End - a.Start) > (b.End - b.Start)
}

func sortFindings(f []Finding) {
	sort.SliceStable(f, func(i, j int) bool {
		if f[i].Start != f[j].Start {
			return f[i].Start < f[j].Start
		}
		if f[i].Tier != f[j].Tier {
			return f[i].Tier < f[j].Tier
		}
		return (f[i].End - f[i].Start) > (f[j].End - f[j].Start)
	})
}
