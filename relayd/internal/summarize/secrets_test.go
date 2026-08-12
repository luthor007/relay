package summarize_test

import (
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/index"
	"github.com/luthor007/relay/relayd/internal/summarize"
)

// The detector is internal/index's, not a second copy.
//
// There is exactly one measured ruleset — MEMORY.md §12.2's 70.6% tier-1 recall
// and 92.9% combined were scored against it — and internal/index owns it,
// compiles it in, and holds the measurement with its own tests. Duplicating the
// patterns here would produce two detectors that agree today and diverge on the
// first vendor prefix somebody adds to one of them. This assertion is what
// notices if that interface ever drifts apart.
func TestDetectorSatisfiesTheRedactor(t *testing.T) {
	var _ summarize.Redactor = index.MustDetector()
	var _ summarize.Redactor = summarize.Detector()

	clean, findings := summarize.Detector().Redact(
		"deploy used GITLAB_TOKEN=glpat-TESTONLYneverIssuedToAnybody06 and it worked")
	if len(findings) == 0 {
		t.Fatal("nothing detected")
	}
	if strings.Contains(clean, "glpat-TESTONLYneverIssuedToAnybody06") {
		t.Fatalf("key survived: %s", clean)
	}
}

// MEMORY.md §12.2 rule 1, restated where this package acts on it: a tier-2 hit
// gets the text redacted but must never auto-create a vault entry, because one
// in four of them would be a checksum.
func TestOnlyTierOneIsProposable(t *testing.T) {
	det := summarize.Detector()

	_, vendor := det.Redact("OPENAI_API_KEY=sk-u8dwaEdKMAgaAhy1rYctQHM1LFmsa7upEsEI7HaXV9Yoxr6X")
	var anyProposable bool
	for _, f := range vendor {
		if summarize.Proposable(f) {
			anyProposable = true
		}
	}
	if !anyProposable {
		t.Fatal("a vendor-shaped key produced no vault candidate")
	}

	// A SHA-256 digest is indistinguishable from a 64-character app secret.
	_, shape := det.Redact("sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08")
	if len(shape) == 0 {
		t.Fatal("tier 2 did not fire on 64 hex characters")
	}
	for _, f := range shape {
		if summarize.Proposable(f) {
			t.Fatalf("a %s finding is proposable", f.Tier)
		}
	}
}

func TestMarkerIDIsStableAndScopedToTheTurn(t *testing.T) {
	f := index.Finding{RuleID: "stripe_secret", Tier: index.TierVendor, Start: 12, End: 40}
	a := summarize.MarkerID("claude-code", "s1", "t1", f)
	if a != summarize.MarkerID("claude-code", "s1", "t1", f) {
		t.Fatal("unstable marker id")
	}
	if a == summarize.MarkerID("claude-code", "s1", "t2", f) {
		t.Fatal("marker id ignores the turn")
	}
	if a == summarize.MarkerID("codex", "s1", "t1", f) {
		t.Fatal("marker id ignores the runtime")
	}
}
