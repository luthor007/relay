package connector_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/connector"
)

var at = time.Date(2026, 8, 10, 9, 30, 0, 0, time.UTC)

// SYSTEM.md §3.4 fixes the shape so the orchestrator never learns a vendor's:
// { connector, kind, at, summary, entities[], payload }.
func TestEnvelopeIsTheDocumentedShape(t *testing.T) {
	n := connector.MustNormalizer()
	got, _, err := n.Normalize(connector.Envelope{
		Connector: "Prusa", Kind: "job.finished", At: at,
		Summary:  "The printer finished benchy.gcode.",
		Entities: []string{"benchy.gcode"},
		Payload:  map[string]any{"job": 12},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"connector", "kind", "at", "summary", "entities", "payload"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("the envelope is missing %q: %s", key, b)
		}
	}
	if len(m) != 6 {
		t.Fatalf("the envelope has fields SYSTEM.md §3.4 does not: %s", b)
	}
	if m["connector"] != "prusa" {
		t.Fatalf("the connector name is the grant key and is normalised: %v", m["connector"])
	}
}

// An adapter never emits an event it cannot observe. A connector that cannot say
// when, what or in one sentence has not observed anything.
func TestUnobservableEnvelopesAreRefused(t *testing.T) {
	n := connector.MustNormalizer()
	base := connector.Envelope{Connector: "prusa", Kind: "job.finished", At: at, Summary: "done"}

	cases := []struct {
		name string
		mut  func(e *connector.Envelope)
		want error
	}{
		{"no connector", func(e *connector.Envelope) { e.Connector = " " }, connector.ErrNoConnector},
		{"no kind", func(e *connector.Envelope) { e.Kind = "" }, connector.ErrNoKind},
		{"no time", func(e *connector.Envelope) { e.At = time.Time{} }, connector.ErrNoTime},
		{"no summary", func(e *connector.Envelope) { e.Summary = "  " }, connector.ErrNoSummary},
	}
	for _, c := range cases {
		e := base
		c.mut(&e)
		if _, _, err := n.Normalize(e); !errors.Is(err, c.want) {
			t.Errorf("%s: want %v, got %v", c.name, c.want, err)
		}
	}
}

// MEMORY.md §12.2: detect before writing, never after. A connector payload — a
// mail body, a webhook, a printer's error string — is exactly where a key turns
// up, and an envelope is on its way to the index.
func TestSecretsAreRedactedBeforeAnEnvelopeGoesAnywhere(t *testing.T) {
	n := connector.MustNormalizer()
	key := "ghp_000000000000000000000000000000000000"
	got, found, err := n.Normalize(connector.Envelope{
		Connector: "prusa", Kind: "printer.error", At: at,
		Summary:  "Upload failed with token " + key,
		Entities: []string{key},
		Payload: map[string]any{
			"header": map[string]any{"authorization": "Bearer " + key},
			"trace":  []any{"retry with " + key},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), key) {
		t.Fatalf("a secret survived into the envelope: %s", blob)
	}
	if !strings.Contains(got.Summary, "relay:redacted") {
		t.Fatalf("the marker should say what was removed: %q", got.Summary)
	}
	if len(found) == 0 {
		t.Fatal("the findings must come back so a vault proposal can be raised")
	}
}

func TestSummaryIsCappedAndEntitiesDeduped(t *testing.T) {
	n := connector.MustNormalizer()
	got, _, err := n.Normalize(connector.Envelope{
		Connector: "prusa", Kind: "k", At: at,
		Summary:  strings.Repeat("x", connector.MaxSummary+50),
		Entities: []string{"a", "a", " a ", "", "b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(got.Summary)) != connector.MaxSummary {
		t.Fatalf("summary length = %d", len([]rune(got.Summary)))
	}
	if len(got.Entities) != 2 || got.Entities[0] != "a" || got.Entities[1] != "b" {
		t.Fatalf("entities = %v", got.Entities)
	}
}

func TestEnvelopeLineReadsAsOneSentence(t *testing.T) {
	e := connector.Envelope{
		Connector: "prusa", Kind: "job.finished", At: at,
		Summary: "The printer finished benchy.gcode.", Entities: []string{"benchy.gcode"},
	}
	if got := e.Line(); !strings.HasPrefix(got, "prusa: ") || !strings.Contains(got, "benchy.gcode") {
		t.Fatalf("line = %q", got)
	}
}

func TestNormalizerWithNoDetectorRefuses(t *testing.T) {
	var n *connector.Normalizer
	_, _, err := n.Normalize(connector.Envelope{Connector: "c", Kind: "k", At: at, Summary: "s"})
	if err == nil {
		t.Fatal("writing connector text without ever looking at it must not be possible")
	}
}
