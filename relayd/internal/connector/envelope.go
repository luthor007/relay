package connector

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/index"
)

// The normalized envelope — SYSTEM.md §3.4, verbatim:
//
//	Each connector adapter normalises to one envelope, so the orchestrator never
//	learns a vendor's shape:
//
//	  { connector: "gmail", kind: "message.received",
//	    at: <ts>, summary: str, entities: [str], payload: {...} }
//
// Two rules are enforced on the way in rather than trusted:
//
//   - **Nothing is emitted that was not observed.** An envelope with no
//     timestamp, no kind or no summary is refused. ADAPTERS.md §5's rule for
//     adapters applies identically here: a connector that cannot say when
//     something happened has not observed it happening.
//   - **Secrets are detected before the text is written anywhere, never after.**
//     Summary, entities and every string in the payload go through the same
//     internal/index detector the transcript path uses. MEMORY.md §12.2: an
//     embedded key cannot be unembedded, and a connector payload — a mail body,
//     a webhook, a printer's error string — is exactly where one turns up.

// Envelope is one thing a connector observed.
//
// At marshals as RFC 3339 rather than the unix milliseconds the store uses,
// because an envelope's audience is a language model reading structuredContent
// off the MCP bus, and a model reads a date better than it reads an integer.
type Envelope struct {
	Connector string         `json:"connector"`
	Kind      string         `json:"kind"`
	At        time.Time      `json:"at"`
	Summary   string         `json:"summary"`
	Entities  []string       `json:"entities"`
	Payload   map[string]any `json:"payload,omitempty"`
}

// Limits on an envelope. They are caps rather than errors: a connector that
// produced a slightly long summary should still be heard.
const (
	// MaxSummary is the summary budget. ADAPTERS.md §6 gives ~160 characters
	// for something spoken; this is a little more because a summary may be read
	// rather than said, and it is still short enough to be one sentence.
	MaxSummary = 200
	// MaxEntities caps the entity list. An envelope naming fifty things has
	// named nothing.
	MaxEntities = 32
)

// Errors from validation.
var (
	ErrNoConnector = errors.New("connector: an envelope must name its connector")
	ErrNoKind      = errors.New("connector: an envelope must have a kind")
	ErrNoTime      = errors.New("connector: an envelope must say when it happened")
	ErrNoSummary   = errors.New("connector: an envelope must have a summary")
)

// Validate checks the envelope is one the orchestrator can act on.
func (e Envelope) Validate() error {
	switch {
	case strings.TrimSpace(e.Connector) == "":
		return ErrNoConnector
	case strings.TrimSpace(e.Kind) == "":
		return ErrNoKind
	case e.At.IsZero():
		return ErrNoTime
	case strings.TrimSpace(e.Summary) == "":
		return ErrNoSummary
	}
	return nil
}

// Normalizer validates and redacts envelopes.
type Normalizer struct {
	det *index.Detector
}

// NewNormalizer compiles the standard secret ruleset.
func NewNormalizer() (*Normalizer, error) {
	d, err := index.NewDetector()
	if err != nil {
		return nil, err
	}
	return &Normalizer{det: d}, nil
}

// MustNormalizer is NewNormalizer for package-level initialisation and tests.
func MustNormalizer() *Normalizer {
	n, err := NewNormalizer()
	if err != nil {
		panic(err)
	}
	return n
}

// Normalize validates an envelope and returns it with every string redacted.
//
// The findings come back so a caller can raise MEMORY.md §6's "I found what
// looks like a Twilio token" proposal. They carry the matched value in memory
// and must never be logged or stored — that is the same contract index.Finding
// already documents.
func (n *Normalizer) Normalize(e Envelope) (Envelope, []index.Finding, error) {
	if err := e.Validate(); err != nil {
		return Envelope{}, nil, err
	}
	if n == nil || n.det == nil {
		// A normalizer with no detector would write connector text to the index
		// without ever having looked at it. Refusing is the only safe answer,
		// and it is a programming error rather than a runtime condition.
		return Envelope{}, nil, errors.New("connector: normalizer has no secret detector")
	}

	out := Envelope{
		Connector: strings.ToLower(strings.TrimSpace(e.Connector)),
		Kind:      strings.TrimSpace(e.Kind),
		At:        e.At.UTC(),
	}

	var found []index.Finding
	summary, f := n.det.Redact(clip(strings.TrimSpace(e.Summary), MaxSummary))
	out.Summary = summary
	found = append(found, f...)

	seen := map[string]bool{}
	for _, ent := range e.Entities {
		s := strings.TrimSpace(ent)
		if s == "" {
			continue
		}
		red, ef := n.det.Redact(s)
		found = append(found, ef...)
		if seen[red] {
			continue
		}
		seen[red] = true
		out.Entities = append(out.Entities, red)
		if len(out.Entities) == MaxEntities {
			break
		}
	}
	if out.Entities == nil {
		out.Entities = []string{}
	}

	payload, pf := n.redactAny(e.Payload)
	found = append(found, pf...)
	if m, ok := payload.(map[string]any); ok {
		out.Payload = m
	}
	return out, found, nil
}

// redactAny walks a decoded JSON value and redacts every string in it. Keys are
// left alone: a key is a vendor's field name, and a secret that appeared as one
// would be a stranger thing than this is designed for.
func (n *Normalizer) redactAny(v any) (any, []index.Finding) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		s, f := n.det.Redact(t)
		return s, f
	case []any:
		out := make([]any, len(t))
		var found []index.Finding
		for i, item := range t {
			r, f := n.redactAny(item)
			out[i] = r
			found = append(found, f...)
		}
		return out, found
	case map[string]any:
		out := make(map[string]any, len(t))
		var found []index.Finding
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			r, f := n.redactAny(t[k])
			out[k] = r
			found = append(found, f...)
		}
		return out, found
	default:
		return v, nil
	}
}

// Line renders an envelope as one sentence, for a ping or a log. It is built
// only from the envelope's own fields.
func (e Envelope) Line() string {
	s := e.Summary
	if len(e.Entities) > 0 {
		s += " (" + strings.Join(e.Entities, ", ") + ")"
	}
	return fmt.Sprintf("%s: %s", e.Connector, s)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
