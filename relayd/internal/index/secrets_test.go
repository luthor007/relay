package index

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// corpusRecord is one line of testdata/secrets/corpus.jsonl.
type corpusRecord struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Expect string `json:"expect"`
	Text   string `json:"text"`
	Note   string `json:"note"`
}

func corpusPath(t *testing.T, name string) string {
	t.Helper()
	// internal/index -> relayd/testdata/secrets
	p := filepath.Join("..", "..", "testdata", "secrets", name)
	if _, err := os.Stat(p); err != nil {
		// Skip, not fail, and say why. testdata/secrets is excluded from the
		// public repo on purpose — see ruleset.go — so in a clone of that repo
		// this file is absent by design and a red suite would be reporting the
		// split rather than a defect. In the private repo it is always present,
		// which is where the recall figures are actually defended.
		t.Skipf("the measured corpus is not in this tree (%v); it is excluded from the public repo, see internal/index/ruleset.go", err)
	}
	return p
}

func loadCorpus(t *testing.T) []corpusRecord {
	t.Helper()
	f, err := os.Open(corpusPath(t, "corpus.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var out []corpusRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r corpusRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("corpus line: %v", err)
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRulesetIsTheMeasuredRuleset is the guard that lets ruleset.go be Go
// source instead of a copy of rules.json. If anyone edits one and not the
// other, MEMORY.md §12.2's recall figures stop describing the shipping
// detector, and this goes red.
func TestRulesetIsTheMeasuredRuleset(t *testing.T) {
	b, err := os.ReadFile(corpusPath(t, "rules.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Tier1 []struct{ ID, Pattern, Label string } `json:"tier1"`
		Tier2 []struct{ ID, Pattern, Label string } `json:"tier2"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		t.Fatal(err)
	}

	type row struct {
		id, pattern, label string
		tier               Tier
	}
	var want []row
	for _, r := range file.Tier1 {
		want = append(want, row{r.ID, r.Pattern, r.Label, TierVendor})
	}
	for _, r := range file.Tier2 {
		want = append(want, row{r.ID, r.Pattern, r.Label, TierShape})
	}

	got := MustDetector().Rules()
	if len(got) != len(want) {
		t.Fatalf("ruleset has %d rules, rules.json has %d", len(got), len(want))
	}
	for i, w := range want {
		g := got[i]
		if g.ID != w.id {
			t.Errorf("rule %d: id %q, rules.json says %q", i, g.ID, w.id)
		}
		if g.Pattern() != w.pattern {
			t.Errorf("rule %s: pattern\n  got  %q\n  want %q", w.id, g.Pattern(), w.pattern)
		}
		if g.Label != w.label {
			t.Errorf("rule %s: label %q, rules.json says %q", w.id, g.Label, w.label)
		}
		if g.Tier != w.tier {
			t.Errorf("rule %s: tier %v, rules.json puts it in %v", w.id, g.Tier, w.tier)
		}
	}
}

// TestRecallMatchesMemory122 reproduces the exact numbers written into
// MEMORY.md §12.2. They are load-bearing: §6's design changed because tier 1
// misses three credentials in ten, and if that stops being true the document is
// wrong rather than the test.
func TestRecallMatchesMemory122(t *testing.T) {
	d := MustDetector()
	records := loadCorpus(t)

	var secrets, cleans int
	var t1Caught, t12Caught, t1FP, t12FP int

	for _, r := range records {
		hasT1 := len(d.ScanTier(r.Text, TierVendor)) > 0
		hasAny := len(d.Scan(r.Text)) > 0
		switch r.Expect {
		case "secret":
			secrets++
			if hasT1 {
				t1Caught++
			}
			if hasAny {
				t12Caught++
			}
		case "clean":
			cleans++
			if hasT1 {
				t1FP++
			}
			if hasAny {
				t12FP++
			}
		default:
			t.Fatalf("%s: unknown expect %q", r.ID, r.Expect)
		}
	}

	// The corpus itself: 127 records, 85 synthetic credentials, 42 hard
	// negatives. Asserted so a corpus edit is visible here too.
	if secrets != 85 || cleans != 42 {
		t.Fatalf("corpus is %d secrets / %d clean; MEMORY.md §12.2 measured 85 / 42", secrets, cleans)
	}
	if t1Caught != 60 {
		t.Errorf("tier 1 recall %d/85, MEMORY.md §12.2 says 60/85 (70.6%%)", t1Caught)
	}
	if t1FP != 1 {
		t.Errorf("tier 1 false positives %d/42, MEMORY.md §12.2 says 1/42 (2.4%%)", t1FP)
	}
	if t12Caught != 79 {
		t.Errorf("tier 1+2 recall %d/85, MEMORY.md §12.2 says 79/85 (92.9%%)", t12Caught)
	}
	if t12FP != 11 {
		t.Errorf("tier 1+2 false positives %d/42, MEMORY.md §12.2 says 11/42 (26.2%%)", t12FP)
	}
}

// TestAdversarialRecordsAreStillMissed keeps the honest half of §12.2 honest.
// Six positives are missed by both tiers on purpose. If a future rule catches
// one, that is good news that has to be written into the document, not a silent
// improvement — so this test names them.
func TestAdversarialRecordsAreStillMissed(t *testing.T) {
	d := MustDetector()
	missed := map[string]bool{}
	for _, r := range loadCorpus(t) {
		if r.Expect != "secret" {
			continue
		}
		if len(d.Scan(r.Text)) == 0 {
			missed[r.Kind] = true
		}
	}
	for _, kind := range []string{"adversarial_encoded", "adversarial_json", "adversarial_prose", "adversarial_wrapped"} {
		if !missed[kind] {
			t.Errorf("%s is now caught. That is an improvement — record it in MEMORY.md §12.2 before changing this test", kind)
		}
	}
}

func TestRedactReplacesAndNamesTheVendor(t *testing.T) {
	d := MustDetector()
	// A vendor key inside a secret-named assignment: both tiers fire on
	// overlapping spans, and the marker must name the vendor.
	text := "STRIPE_SECRET_KEY=sk_test_" + strings.Repeat("A1b2", 6) + " and nothing else"

	red, findings := d.Redact(text)
	if strings.Contains(red, "sk_test_") {
		t.Fatalf("redacted text still carries the key: %q", red)
	}
	if !strings.Contains(red, "[relay:redacted Stripe secret key]") {
		t.Fatalf("marker missing from %q", red)
	}
	if len(findings) != 1 {
		t.Fatalf("overlapping matches should collapse to one, got %d: %+v", len(findings), findings)
	}
	if findings[0].Tier != TierVendor || findings[0].Service != "stripe" {
		t.Fatalf("kept the shape rule over the vendor rule: %+v", findings[0])
	}
	if got := findings[0].Sentence(); got != "a Stripe secret key appeared in this session" {
		t.Fatalf("marker sentence %q is not MEMORY.md §6's wording", got)
	}
	if !strings.HasSuffix(red, " and nothing else") {
		t.Fatalf("redaction ate the tail: %q", red)
	}
}

func TestPreviewNeverShowsMoreThanFour(t *testing.T) {
	f := Finding{Value: "sk_test_abcdefghijklmnop"}
	if got := f.Preview(); got != "…mnop" {
		t.Fatalf("preview %q", got)
	}
	short := Finding{Value: "abc"}
	if got := short.Preview(); strings.Contains(got, "abc") {
		t.Fatalf("short value leaked: %q", got)
	}
}

func TestTierTwoIsNeverAVaultCandidate(t *testing.T) {
	if TierShape.ProposeToVault() {
		t.Fatal("tier 2 must never reach the vault: one in four is a checksum (MEMORY.md §12.2)")
	}
	if !TierVendor.ProposeToVault() {
		t.Fatal("tier 1 is the auto-path")
	}
}

func TestScanIsEmptyOnCleanText(t *testing.T) {
	d := MustDetector()
	if got := d.Scan(""); got != nil {
		t.Fatalf("empty text produced %v", got)
	}
	if got := d.Scan("the deploy went fine, no keys here"); len(got) != 0 {
		t.Fatalf("false positive on ordinary prose: %+v", got)
	}
}
