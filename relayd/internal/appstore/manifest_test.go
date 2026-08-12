package appstore_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/appstore"
)

// repoFile finds a path relative to the repository root, walking up from the
// test's working directory. Missing is a skip rather than a failure: relayd is
// extractable on its own, and a test that fails because a sibling directory is
// absent teaches people to ignore it.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		p := filepath.Join(dir, rel)
		if _, err := os.Stat(p); err == nil {
			return p
		}
		dir = filepath.Dir(dir)
	}
	t.Skipf("%s not found from the test's working directory", rel)
	return ""
}

// The Go manifest parser and apps/sdk/src/manifest.ts describe the same file
// and cannot be one implementation: one runs in the author's editor and one
// runs on the box, which does not get to assume the author ran anything. They
// can be pinned, and this is the pin — the same trick the Android command
// catalog uses against glasses/protocol/commands.py (APPS-SCOPE.md §5.2).
func TestManifestRulesMatchTheSDK(t *testing.T) {
	src, err := os.ReadFile(repoFile(t, "apps/sdk/src/manifest.ts"))
	if err != nil {
		t.Fatal(err)
	}
	ts := string(src)

	// 1. The nine scopes, and no tenth on either side. Read out of the
	// PermissionScope block itself rather than by pattern-matching the whole
	// file, so a scope with no dot in it (`schedule`) cannot slip past.
	_, after, ok := strings.Cut(ts, "export const PermissionScope = {")
	if !ok {
		t.Fatal("manifest.ts no longer declares PermissionScope the way this guard reads it")
	}
	block, _, ok := strings.Cut(after, "} as const;")
	if !ok {
		t.Fatal("PermissionScope block is unterminated")
	}
	scopeRe := regexp.MustCompile(`(?m)^\s*\w+:\s*"([a-z.]+)",`)
	var fromTS []string
	for _, m := range scopeRe.FindAllStringSubmatch(block, -1) {
		fromTS = append(fromTS, m[1])
	}
	if len(fromTS) == 0 {
		t.Fatal("found no scopes in manifest.ts — the drift guard has itself drifted")
	}
	inGo := map[string]bool{}
	for _, s := range appstore.Scopes() {
		inGo[string(s)] = true
	}
	for _, s := range fromTS {
		if !inGo[s] {
			t.Errorf("the SDK declares scope %q and this box does not know it", s)
		}
	}
	if len(fromTS) != len(appstore.Scopes()) {
		t.Errorf("SDK has %d scopes, Go has %d: %v vs %v",
			len(fromTS), len(appstore.Scopes()), fromTS, appstore.Scopes())
	}

	// 2. The two patterns, character for character. A box that accepts an id
	// the SDK rejects accepts an app the author's toolchain never validated.
	for _, tc := range []struct{ name, want string }{
		{"ID_PATTERN", `^[a-z0-9]+(\.[a-z0-9][a-z0-9-]*)+$`},
		{"SEMVER_PATTERN", `^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`},
	} {
		re := regexp.MustCompile(`const ` + tc.name + ` = /(.*)/;`)
		m := re.FindStringSubmatch(ts)
		if m == nil {
			t.Fatalf("manifest.ts no longer declares %s the way this guard reads it", tc.name)
		}
		if m[1] != tc.want {
			t.Errorf("%s in the SDK is %q; manifest.go compiles %q", tc.name, m[1], tc.want)
		}
	}

	// 3. The floor on a permission reason.
	m := regexp.MustCompile(`p\.reason\.trim\(\)\.length < (\d+)`).FindStringSubmatch(ts)
	if m == nil {
		t.Fatal("manifest.ts no longer bounds reason length the way this guard reads it")
	}
	if m[1] != "10" {
		t.Errorf("the SDK's minimum reason length is %s and manifest.go's is 10", m[1])
	}
}

// The example app the SDK ships has to be an app this box will actually take.
// It is the first thing anyone copies.
func TestTheSDKExampleParses(t *testing.T) {
	b, err := os.ReadFile(repoFile(t, "apps/sdk/examples/standup-notes/relay.json"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := appstore.ParseManifest(b)
	if err != nil {
		t.Fatalf("the SDK's own example does not install: %v", err)
	}
	if m.ID != "dev.alexis.standup-notes" || m.ShortName() != "standup-notes" {
		t.Errorf("id = %q, short = %q", m.ID, m.ShortName())
	}
	if m.TimeoutMS != 60_000 {
		t.Errorf("timeoutMs = %d, want the manifest's 60000", m.TimeoutMS)
	}
	if len(m.Permissions) != 4 {
		t.Fatalf("permissions = %d", len(m.Permissions))
	}
	// The fixture registry serves this manifest byte-for-byte. If the example
	// changes, the fixture has to change with it.
	fixture, err := os.ReadFile(filepath.Join("..", "..", "testdata", "appstore", "registry",
		"apps", "dev.alexis.standup-notes.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range m.Permissions {
		if !strings.Contains(string(fixture), p.Reason) {
			t.Errorf("the registry fixture no longer carries the SDK example's reason %q", p.Reason)
		}
	}
}

func TestEveryScopeHasAGrantSentence(t *testing.T) {
	for _, s := range appstore.Scopes() {
		if !s.Known() {
			t.Errorf("%s is listed and unknown", s)
		}
		if strings.TrimSpace(s.Grants()) == "" {
			t.Errorf("%s has no sentence, so the permission sheet would show a bare scope id", s)
		}
	}
	if appstore.Scope("memory.everything").Known() {
		t.Error("an invented scope must not be known")
	}
	if appstore.Scope("memory.everything").Grants() != "" {
		t.Error("an unknown scope must not produce a sentence")
	}
}

func TestManifestRefusals(t *testing.T) {
	base := `{
	  "id": "dev.you.app", "name": "App", "version": "1.0.0",
	  "description": "Does a thing.", "author": {"name": "You"},
	  "permissions": [{"scope": "memory.read", "reason": "To find the meeting you just left."}],
	  "triggers": [{"type": "phrase", "match": "hello"}]
	}`
	if _, err := appstore.ParseManifest([]byte(base)); err != nil {
		t.Fatalf("the baseline manifest must parse: %v", err)
	}

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"not json", `{`, "valid JSON"},
		{"id not reverse-dns", strings.Replace(base, `"dev.you.app"`, `"App"`, 1), "reverse-DNS"},
		{"version not semver", strings.Replace(base, `"1.0.0"`, `"v1"`, 1), "semver"},
		{"no name", strings.Replace(base, `"name": "App"`, `"name": " "`, 1), "name is required"},
		{"no author", strings.Replace(base, `{"name": "You"}`, `{}`, 1), "author.name"},
		{
			"unknown scope",
			strings.Replace(base, `"memory.read"`, `"memory.everything"`, 1),
			"not a scope this box knows",
		},
		{
			// The rule that makes the whole sheet worth reading.
			"reason is a restatement of the scope",
			strings.Replace(base, `"To find the meeting you just left."`, `"Memory."`, 1),
			"a reason a user can read",
		},
		{"no triggers", strings.Replace(base, `[{"type": "phrase", "match": "hello"}]`, `[]`, 1),
			"at least one trigger"},
		{
			"trigger this box cannot fire",
			strings.Replace(base, `"type": "phrase", "match": "hello"`, `"type": "telepathy"`, 1),
			"could never fire it",
		},
		{
			"phrase trigger with no phrase",
			strings.Replace(base, `"type": "phrase", "match": "hello"`, `"type": "phrase"`, 1),
			"with no match",
		},
		{
			"invented gesture",
			strings.Replace(base, `"type": "phrase", "match": "hello"`,
				`"type": "touch", "gesture": "quadrupleTap"`, 1),
			"doubleTap, tripleTap or longPress",
		},
		{
			"net.fetch with no host list",
			strings.Replace(base, `"scope": "memory.read"`, `"scope": "net.fetch"`, 1),
			"net.fetch requires allowedHosts",
		},
		{
			"hosts without net.fetch",
			strings.Replace(base, `"triggers"`, `"allowedHosts": ["api.example.com"], "triggers"`, 1),
			"without the net.fetch permission",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := appstore.ParseManifest([]byte(tc.body))
			if err == nil {
				t.Fatalf("accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}

	t.Run("duplicate scope", func(t *testing.T) {
		body := strings.Replace(base,
			`[{"scope": "memory.read", "reason": "To find the meeting you just left."}]`,
			`[{"scope": "memory.read", "reason": "To find the meeting you just left."},`+
				`{"scope": "memory.read", "reason": "And also to read everything else."}]`, 1)
		_, err := appstore.ParseManifest([]byte(body))
		if err == nil || !strings.Contains(err.Error(), "duplicate permission scope") {
			t.Fatalf("err = %v; two reasons for one scope means the sheet shows one of them", err)
		}
	})
}

// An allowlist that allows everything is not an allowlist, and the sheet would
// present it as a restriction.
func TestAllowedHostsMustBeHosts(t *testing.T) {
	tmpl := `{
	  "id": "dev.you.app", "name": "App", "version": "1.0.0",
	  "description": "Does a thing.", "author": {"name": "You"},
	  "permissions": [{"scope": "net.fetch", "reason": "To ask the transit agency about your line."}],
	  "allowedHosts": [%s],
	  "triggers": [{"type": "phrase", "match": "hello"}]
	}`
	for _, tc := range []struct{ host, want string }{
		{`"*"`, "not an allowlist"},
		{`"https://api.example.com"`, "name the host alone"},
		{`"api.example.com/v1"`, "name the host alone"},
		{`"API.example.com"`, "lowercase"},
		{`"*example.com"`, "whole leading label"},
		{`""`, "empty host"},
	} {
		_, err := appstore.ParseManifest([]byte(strings.ReplaceAll(tmpl, "%s", tc.host)))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("allowedHosts %s: err = %v, want %q", tc.host, err, tc.want)
		}
	}
	// The ones that must be accepted, including a whole-label wildcard.
	ok := strings.ReplaceAll(tmpl, "%s", `"api.stm.info", "*.transitfeeds.example"`)
	if _, err := appstore.ParseManifest([]byte(ok)); err != nil {
		t.Errorf("a real allowlist was refused: %v", err)
	}
}

// A manifest written for a later platform still installs; a scope this box
// cannot enforce does not. The asymmetry is the point.
func TestUnknownTopLevelFieldsAreIgnored(t *testing.T) {
	body := `{
	  "id": "dev.you.app", "name": "App", "version": "1.0.0",
	  "description": "Does a thing.", "author": {"name": "You"},
	  "permissions": [{"scope": "memory.read", "reason": "To find the meeting you just left."}],
	  "triggers": [{"type": "phrase", "match": "hello"}],
	  "somethingFromNextYear": {"nested": true}
	}`
	m, err := appstore.ParseManifest([]byte(body))
	if err != nil {
		t.Fatalf("an unknown field must not block an install: %v", err)
	}
	if m.TimeoutMS != appstore.DefaultTimeoutMS {
		t.Errorf("timeoutMs = %d, want the default %d", m.TimeoutMS, appstore.DefaultTimeoutMS)
	}
}
