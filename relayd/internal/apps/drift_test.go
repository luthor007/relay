package apps

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The drift guard.
//
// `apps/sdk/src/manifest.ts` is the manifest format. This package validates the
// same file, and two implementations of one format is how an app installs with
// permissions nobody reviewed: the sheet the author saw would come from one
// parser and the grant the daemon minted from another.
//
// So this test re-parses the TypeScript on every run rather than trusting a
// comment that says the two agree. It is the same trick `glasses/bridge` uses
// against `commands.py` and the Android catalog uses against both — a
// cross-language constant that only a test is holding is a constant that drifts.

func sdkManifestSource(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "apps", "sdk", "src", "manifest.ts")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("the SDK is not in this tree: %v", err)
	}
	return string(b)
}

func TestScopesDoNotDriftFromTheSDK(t *testing.T) {
	src := sdkManifestSource(t)

	block := between(t, src, "export const PermissionScope = {", "} as const;")
	re := regexp.MustCompile(`(?m)^\s*\w+:\s*"([a-z.]+)",`)
	var fromTS []string
	for _, m := range re.FindAllStringSubmatch(block, -1) {
		fromTS = append(fromTS, m[1])
	}
	if len(fromTS) == 0 {
		t.Fatal("found no scopes in the SDK; the guard has stopped guarding")
	}

	var fromGo []string
	for _, s := range Scopes() {
		fromGo = append(fromGo, string(s))
	}
	sort.Strings(fromTS)
	sortedGo := append([]string(nil), fromGo...)
	sort.Strings(sortedGo)
	if strings.Join(fromTS, ",") != strings.Join(sortedGo, ",") {
		t.Errorf("scope vocabularies have drifted.\n  sdk: %v\n   go: %v", fromTS, sortedGo)
	}

	// And every one of them has a plain-English sentence for the install sheet,
	// because a scope with no sentence is a row the user cannot read.
	for _, s := range Scopes() {
		if s.Grants() == "" {
			t.Errorf("%s has no grant sentence", s)
		}
	}
}

func TestTriggerVocabularyDoesNotDriftFromTheSDK(t *testing.T) {
	src := sdkManifestSource(t)

	// The union runs to the next top-level declaration, not to the first
	// semicolon: `{ type: "phrase"; match: string }` has one inside it.
	union := between(t, src, "export type Trigger =", "\nexport ")
	types := regexp.MustCompile(`\{\s*type:\s*"(\w+)"`).FindAllStringSubmatch(union, -1)
	var fromTS []string
	for _, m := range types {
		fromTS = append(fromTS, m[1])
	}
	var fromGo []string
	for _, tt := range TriggerTypes() {
		fromGo = append(fromGo, string(tt))
	}
	if strings.Join(fromTS, ",") != strings.Join(fromGo, ",") {
		t.Errorf("trigger types have drifted.\n  sdk: %v\n   go: %v", fromTS, fromGo)
	}

	gestureLine := ""
	for _, line := range strings.Split(union, "\n") {
		if strings.Contains(line, "gesture:") {
			gestureLine = line
		}
	}
	if gestureLine == "" {
		t.Fatal("the SDK's touch trigger no longer names its gestures")
	}
	var gestures []string
	for _, m := range regexp.MustCompile(`"(\w+)"`).FindAllStringSubmatch(gestureLine, -1) {
		if m[1] != "touch" {
			gestures = append(gestures, m[1])
		}
	}
	var goGestures []string
	for _, g := range Gestures() {
		goGestures = append(goGestures, string(g))
	}
	if strings.Join(gestures, ",") != strings.Join(goGestures, ",") {
		t.Errorf("gestures have drifted.\n  sdk: %v\n   go: %v", gestures, goGestures)
	}
}

func TestMemoryEventsDoNotDriftFromTheSDK(t *testing.T) {
	src := sdkManifestSource(t)
	block := between(t, src, "export type MemoryEvent =", "\nexport ")
	var fromTS []string
	for _, m := range regexp.MustCompile(`"([a-z.]+)"`).FindAllStringSubmatch(block, -1) {
		fromTS = append(fromTS, m[1])
	}
	var fromGo []string
	for _, e := range MemoryEvents() {
		fromGo = append(fromGo, string(e))
	}
	if strings.Join(fromTS, ",") != strings.Join(fromGo, ",") {
		t.Errorf("pipeline events have drifted.\n  sdk: %v\n   go: %v", fromTS, fromGo)
	}
	// APP-PLATFORM.md §4's table names three of them; the SDK adds the fourth.
	// If the doc and the SDK ever disagree, the SDK is the one this package
	// follows, and this assertion is where that decision is written down.
	for _, want := range []string{"meeting.ended", "commitment.detected", "day.synced"} {
		if !strings.Contains(block, want) {
			t.Errorf("§4 names %s and the SDK no longer does", want)
		}
	}
}

func TestValidationConstantsDoNotDriftFromTheSDK(t *testing.T) {
	src := sdkManifestSource(t)

	for _, tc := range []struct {
		name  string
		start string
		got   string
	}{
		{"id pattern", "const ID_PATTERN = ", idPattern.String()},
		{"semver pattern", "const SEMVER_PATTERN = ", semverPattern.String()},
	} {
		i := strings.Index(src, tc.start)
		if i < 0 {
			t.Fatalf("the SDK no longer declares %s", tc.name)
		}
		rest := src[i+len(tc.start):]
		end := strings.Index(rest, "\n")
		line := strings.TrimSpace(rest[:end])
		line = strings.TrimSuffix(line, ";")
		line = strings.TrimPrefix(strings.TrimSuffix(line, "/"), "/")
		if line != tc.got {
			t.Errorf("%s has drifted.\n  sdk: %s\n   go: %s", tc.name, line, tc.got)
		}
	}

	m := regexp.MustCompile(`p\.reason\.trim\(\)\.length\s*<\s*(\d+)`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("the SDK no longer enforces a floor on a permission reason")
	}
	if m[1] != itoa(MinReasonLength) {
		t.Errorf("the reason floor has drifted: sdk %s, go %d", m[1], MinReasonLength)
	}

	// The two rules §3 states as "enforced rather than trusting apps to honour"
	// have to still be in the SDK, because this package's copy of them is only
	// half the story: the author sees the SDK's refusal first.
	for _, want := range []string{"net.fetch requires allowedHosts", "allowedHosts declared without"} {
		if !strings.Contains(src, want) {
			t.Errorf("the SDK no longer refuses %q", want)
		}
	}
}

// between returns the text between the first occurrence of start and the next
// occurrence of end after it.
func between(t *testing.T, src, start, end string) string {
	t.Helper()
	i := strings.Index(src, start)
	if i < 0 {
		t.Fatalf("the SDK no longer contains %q", start)
	}
	rest := src[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("no %q after %q", end, start)
	}
	return rest[:j]
}
