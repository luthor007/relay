package mcpbridge_test

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/luthor007/relay/relayd/internal/apps/mcpbridge"
)

// One wire format, two implementations, and the second one is not in this
// language.
//
// `apps/sdk/src/ui.ts` is what an app author reads and what validates a view
// inside the sandbox; this package is what validates it on the way out. That
// split is the same one internal/apps and internal/appstore have for the
// manifest, and it has the same failure mode: the SDK accepts a view this host
// refuses, and an app that worked on the author's machine draws nothing on
// somebody's phone.
//
// So the version, the block kinds, the closed key sets and every cap are read
// out of the TypeScript on every run rather than trusted to a comment. Reading
// the source rather than importing it keeps a Go test from needing a JS
// toolchain, and makes a rename a clear failure here instead of a confusing one
// somewhere else.

func sdkSource(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "apps", "sdk", "src", name)
	src, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("the SDK is not present at %s: %v", path, err)
	}
	return string(src)
}

func TestVocabularyVersionDoesNotDriftFromSDK(t *testing.T) {
	src := sdkSource(t, "ui.ts")
	for _, tc := range []struct {
		name    string
		pattern string
		want    int
	}{
		{"VOCABULARY_VERSION", `export const VOCABULARY_VERSION = (\d+);`, mcpbridge.VocabularyVersion},
		{"ENVELOPE_VERSION", `export const ENVELOPE_VERSION = (\d+);`, mcpbridge.EnvelopeVersion},
	} {
		m := regexp.MustCompile(tc.pattern).FindStringSubmatch(src)
		if m == nil {
			t.Fatalf("the SDK no longer declares %s the way this guard reads it", tc.name)
		}
		got, _ := strconv.Atoi(m[1])
		if got != tc.want {
			t.Errorf("the SDK stamps %s %d and this host expects %d. A view stamped by one and "+
				"refused by the other is an app that worked on its author's machine and draws "+
				"nothing on somebody's phone", tc.name, got, tc.want)
		}
	}

	m := regexp.MustCompile(`export const RENDER_FRAME = "([^"]+)";`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("the SDK no longer declares RENDER_FRAME the way this guard reads it")
	}
	if m[1] != mcpbridge.RenderFrameType {
		t.Errorf("the SDK sends %q frames and this host reads %q", m[1], mcpbridge.RenderFrameType)
	}
}

func TestBlockKindsDoNotDriftFromSDK(t *testing.T) {
	src := sdkSource(t, "ui.ts")
	m := regexp.MustCompile(`export const BLOCK_KINDS = \[([^\]]*)\] as const;`).FindStringSubmatch(src)
	if m == nil {
		t.Skip("the SDK no longer declares BLOCK_KINDS the way this guard reads it")
	}
	sdk := quoted(m[1])

	var here []string
	for _, k := range mcpbridge.BlockKinds() {
		here = append(here, string(k))
	}
	if strings.Join(sdk, ",") != strings.Join(here, ",") {
		t.Errorf("the SDK offers %v and this host draws %v.\n"+
			"A kind the SDK offers and the host cannot draw is a block that silently "+
			"disappears; a kind the host draws and the SDK will not build is dead code.",
			sdk, here)
	}
}

// Every cap, pinned. A cap the SDK enforces and this host does not is a view
// that gets further than the author's tests said it would.
func TestLimitsDoNotDriftFromSDK(t *testing.T) {
	src := sdkSource(t, "ui.ts")
	block := regexp.MustCompile(`(?s)export const LIMITS = Object\.freeze\(\{(.*?)\n\}\);`).FindStringSubmatch(src)
	if block == nil {
		t.Skip("the SDK no longer declares LIMITS the way this guard reads it")
	}

	sdk := map[string]int{}
	for _, m := range regexp.MustCompile(`(?m)^\s*(\w+):\s*([0-9]+(?:\s*\*\s*[0-9]+)*),`).FindAllStringSubmatch(block[1], -1) {
		sdk[m[1]] = product(m[2])
	}
	if len(sdk) == 0 {
		t.Skip("no caps could be read out of the SDK's LIMITS")
	}

	v := reflect.ValueOf(mcpbridge.Limits)
	seen := map[string]bool{}
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		key := lowerFirst(name)
		want, ok := sdk[key]
		if !ok {
			t.Errorf("this host caps %s at %d and the SDK has no such cap, so an app "+
				"cannot find out about it until a view is refused", key, v.Field(i).Int())
			continue
		}
		seen[key] = true
		if got := int(v.Field(i).Int()); got != want {
			t.Errorf("%s: the SDK allows %d and this host allows %d", key, want, got)
		}
	}
	for key := range sdk {
		if !seen[key] {
			t.Errorf("the SDK caps %s and this host does not, so a view the SDK refused "+
				"would be accepted off the wire", key)
		}
	}
}

// The closed key set is what keeps a host from having to decide whether an
// unrecognised field was decoration or content, so the two sides must agree on
// exactly which fields exist.
func TestBlockFieldsDoNotDriftFromSDK(t *testing.T) {
	src := sdkSource(t, "ui.ts")
	block := regexp.MustCompile(`(?s)const ALLOWED_KEYS[^=]*= Object\.freeze\(\{(.*?)\n\}\);`).FindStringSubmatch(src)
	if block == nil {
		t.Skip("the SDK no longer declares ALLOWED_KEYS the way this guard reads it")
	}
	sdk := map[string][]string{}
	for _, m := range regexp.MustCompile(`(?m)^\s*(\w+):\s*\[([^\]]*)\],`).FindAllStringSubmatch(block[1], -1) {
		sdk[m[1]] = quoted(m[2])
	}
	if len(sdk) == 0 {
		t.Skip("no key sets could be read out of the SDK's ALLOWED_KEYS")
	}

	for _, kind := range mcpbridge.BlockKinds() {
		want, ok := sdk[string(kind)]
		if !ok {
			t.Errorf("the SDK has no field list for a %s block", kind)
			continue
		}
		got := mcpbridge.FieldsFor(kind)
		if strings.Join(want, ",") != strings.Join(got, ",") {
			t.Errorf("a %s block has %v in the SDK and %v here", kind, want, got)
		}
	}
}

// Only speech costs a permission, on both sides. A block the SDK thinks is free
// and this host charges for is an app that passes review and fails at runtime.
func TestBlockScopesDoNotDriftFromSDK(t *testing.T) {
	src := sdkSource(t, "ui.ts")
	block := regexp.MustCompile(`(?s)export const BLOCK_SCOPES[^=]*= Object\.freeze\(\{(.*?)\n\}\);`).FindStringSubmatch(src)
	if block == nil {
		t.Skip("the SDK no longer declares BLOCK_SCOPES the way this guard reads it")
	}
	sdk := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^\s*(\w+):\s*"([^"]+)",`).FindAllStringSubmatch(block[1], -1) {
		sdk[m[1]] = m[2]
	}
	for _, kind := range mcpbridge.BlockKinds() {
		scope, costs := mcpbridge.ScopeFor(kind)
		want, inSDK := sdk[string(kind)]
		if costs != inSDK {
			t.Errorf("a %s block costs a permission here=%v, in the SDK=%v", kind, costs, inSDK)
			continue
		}
		if costs && string(scope) != want {
			t.Errorf("a %s block costs %q here and %q in the SDK", kind, scope, want)
		}
	}
}

// The five codes an app branches on. A code relayd sends that the SDK does not
// name is an app that cannot tell "no glasses paired" from a bug in its own
// code; a code the SDK names and relayd never sends is a branch that never runs.
func TestCapabilityErrorCodesDoNotDriftFromTheRuntime(t *testing.T) {
	sdkSrc := sdkSource(t, "errors.ts")
	var sdk []string
	for _, m := range regexp.MustCompile(`(?m)^\s*\w+:\s*"([a-z_]+)",`).FindAllStringSubmatch(sdkSrc, -1) {
		sdk = append(sdk, m[1])
	}
	if len(sdk) == 0 {
		t.Skip("the SDK no longer declares CapabilityErrorCode the way this guard reads it")
	}

	runtimeSrc, err := os.ReadFile(filepath.Join("..", "wire.go"))
	if err != nil {
		t.Skipf("internal/apps is not present: %v", err)
	}
	var runtime []string
	for _, m := range regexp.MustCompile(`(?m)^\s*Code\w+\s*=\s*"([a-z_]+)"`).FindAllStringSubmatch(string(runtimeSrc), -1) {
		runtime = append(runtime, m[1])
	}
	if len(runtime) == 0 {
		t.Skip("internal/apps no longer declares its capability-channel codes the way this guard reads them")
	}

	if strings.Join(sorted(sdk), ",") != strings.Join(sorted(runtime), ",") {
		t.Errorf("the SDK names %v and the capability channel sends %v", sorted(sdk), sorted(runtime))
	}
}

// --- small helpers ----------------------------------------------------------

func quoted(s string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

func product(expr string) int {
	n := 1
	for _, part := range strings.Split(expr, "*") {
		v, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return -1
		}
		n *= v
	}
	return n
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
