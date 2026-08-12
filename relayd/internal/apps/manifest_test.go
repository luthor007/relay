package apps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseTheExampleAppFromTheSDK(t *testing.T) {
	// The manifest APP-PLATFORM.md §2 prints, as it actually ships. If this Go
	// parser and the TypeScript one disagree about it, the permission sheet the
	// author saw and the grant the daemon mints came from different parsers.
	path := filepath.Join("..", "..", "..", "apps", "sdk", "examples", "standup-notes", "relay.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("the SDK example is not here: %v", err)
	}
	m, err := ParseManifest(b)
	if err != nil {
		t.Fatalf("the shipped example must parse: %v", err)
	}
	if m.ID != "dev.alexis.standup-notes" || m.TimeoutMs != 60000 {
		t.Errorf("parsed %+v", m)
	}
	if len(m.Permissions) != 4 || len(m.Triggers) != 3 {
		t.Errorf("permissions=%d triggers=%d", len(m.Permissions), len(m.Triggers))
	}
	if _, ok := m.ToolTrigger(); !ok {
		t.Error("the example declares a tool trigger and it has to survive parsing")
	}
	if fs := Review(m); Rejected(fs) {
		t.Errorf("the example manifest should pass review: %v", fs)
	}
}

func TestManifestRefusals(t *testing.T) {
	base := map[string]string{
		"id":          `"dev.you.app"`,
		"name":        `"App"`,
		"version":     `"1.0.0"`,
		"description": `"Does a thing worth describing."`,
		"author":      `{ "name": "You" }`,
		"permissions": `[]`,
		"triggers":    `[{"type":"phrase","match":"go"}]`,
	}
	build := func(over map[string]string) []byte {
		fields := map[string]string{}
		for k, v := range base {
			fields[k] = v
		}
		for k, v := range over {
			if v == "" {
				delete(fields, k)
				continue
			}
			fields[k] = v
		}
		var parts []string
		for k, v := range fields {
			parts = append(parts, `"`+k+`": `+v)
		}
		return []byte("{" + strings.Join(parts, ",") + "}")
	}

	cases := []struct {
		name string
		over map[string]string
		want string
	}{
		{"no id", map[string]string{"id": ""}, "reverse-DNS"},
		{"id is not reverse-dns", map[string]string{"id": `"App"`}, "reverse-DNS"},
		{"version is not semver", map[string]string{"version": `"1.0"`}, "semver"},
		{"no author", map[string]string{"author": `{}`}, "author.name"},
		{"permissions missing entirely", map[string]string{"permissions": ""}, "must be an array"},
		{"unknown scope", map[string]string{
			"permissions": `[{"scope":"glasses.led","reason":"To light the indicator myself."}]`,
		}, "not a known scope"},
		{"reason too short", map[string]string{
			"permissions": `[{"scope":"memory.read","reason":"needed"}]`,
		}, "explain why"},
		{"duplicate scopes", map[string]string{
			"permissions": `[{"scope":"memory.read","reason":"To read the meeting."},` +
				`{"scope":"memory.read","reason":"To read it again, apparently."}]`,
		}, "duplicate"},
		{"no triggers", map[string]string{"triggers": `[]`}, "at least one trigger"},
		{"unknown trigger type", map[string]string{
			"triggers": `[{"type":"webhook","url":"https://example.com"}]`,
		}, "unknown trigger type"},
		{"phrase with no match", map[string]string{"triggers": `[{"type":"phrase"}]`}, "non-empty match"},
		{"unknown gesture", map[string]string{
			"triggers": `[{"type":"touch","gesture":"quadrupleTap"}]`,
		}, "unknown gesture"},
		{"unknown memory event", map[string]string{
			"triggers": `[{"type":"memory","event":"user.sighed"}]`,
		}, "unknown pipeline event"},
		{"bad cron", map[string]string{
			"triggers": `[{"type":"schedule","cron":"every morning"}]`,
		}, "five fields"},
		{"tool with no description", map[string]string{
			"triggers": `[{"type":"tool"}]`,
		}, "needs a description"},
		{"net.fetch with no hosts", map[string]string{
			"permissions": `[{"scope":"net.fetch","reason":"To post the summary to your wiki."}]`,
		}, "requires allowedHosts"},
		{"hosts without the scope", map[string]string{
			"triggers": `[{"type":"phrase","match":"go"}],"allowedHosts":["example.com"]`,
		}, "without the net.fetch permission"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseManifest(build(tc.over))
			if err == nil {
				t.Fatalf("expected a refusal mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestAnAllowlistThatAllowsEverythingIsRefused(t *testing.T) {
	for _, host := range []string{"*", "https://example.com", "example.com:443", "example.com/path", "localhost"} {
		src := `{"id":"dev.you.app","name":"A","version":"1.0.0","description":"Talks to a host.",
		 "author":{"name":"You"},
		 "permissions":[{"scope":"net.fetch","reason":"To post the summary to your own wiki."}],
		 "triggers":[{"type":"phrase","match":"go"}],
		 "allowedHosts":["` + host + `"]}`
		if _, err := ParseManifest([]byte(src)); err == nil {
			t.Errorf("allowedHosts %q should be refused", host)
		}
	}
	src := `{"id":"dev.you.app","name":"A","version":"1.0.0","description":"Talks to a host.",
	 "author":{"name":"You"},
	 "permissions":[{"scope":"net.fetch","reason":"To post the summary to your own wiki."}],
	 "triggers":[{"type":"phrase","match":"go"}],
	 "allowedHosts":["API.Example.com","*.internal.example.com"]}`
	m, err := ParseManifest([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if m.AllowedHosts[0] != "api.example.com" {
		t.Errorf("hosts should be lowercased for comparison, got %v", m.AllowedHosts)
	}
}

func TestAScheduleTriggerWithoutTheScopeCanNeverFire(t *testing.T) {
	src := `{"id":"dev.you.nightly","name":"Nightly","version":"1.0.0",
	 "description":"Posts a summary every night.","author":{"name":"You"},
	 "permissions":[],"triggers":[{"type":"schedule","cron":"0 22 * * *"}]}`
	m, err := ParseManifest([]byte(src))
	if err != nil {
		t.Fatalf("the file is well formed, so it parses: %v", err)
	}
	// …and is refused at install, where a cross-field rule belongs.
	if err := m.Validate(); err == nil {
		t.Fatal("a schedule trigger with no schedule permission must be refused at install")
	} else if !strings.Contains(err.Error(), "can never fire") {
		t.Errorf("the refusal must say what is wrong: %v", err)
	}
}

func TestTimeoutDefaultsAndClamps(t *testing.T) {
	m, err := ParseManifest([]byte(minimalManifest))
	if err != nil {
		t.Fatal(err)
	}
	if m.TimeoutMs != DefaultTimeoutMs {
		t.Errorf("timeoutMs = %d, want the SDK default %d", m.TimeoutMs, DefaultTimeoutMs)
	}
	inst := Installed{Manifest: m}
	if got := inst.Timeout(10 * time.Second); got != 10*time.Second {
		t.Errorf("an app does not get to declare it may hold the box longer than the runtime allows: %s", got)
	}
	if got := inst.Timeout(time.Minute); got != 30*time.Second {
		t.Errorf("under the ceiling the manifest wins: %s", got)
	}
}

func TestTheSheetShowsEveryReasonVerbatim(t *testing.T) {
	src := `{"id":"dev.you.app","name":"A","version":"1.0.0","description":"Does a thing.",
	 "author":{"name":"You"},
	 "permissions":[
	   {"scope":"memory.write","reason":"To save the notes it extracts back to your memory."},
	   {"scope":"memory.read","reason":"To read the transcript of the meeting you just left."}],
	 "triggers":[{"type":"phrase","match":"go"}]}`
	m, err := ParseManifest([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	sheet := Sheet(m)
	if len(sheet) != 2 {
		t.Fatalf("sheet = %+v", sheet)
	}
	if sheet[0].Scope != ScopeMemoryRead {
		t.Errorf("the sheet has to be in a stable order, got %s first", sheet[0].Scope)
	}
	for _, item := range sheet {
		if item.Reason != m.Reason(item.Scope) {
			t.Errorf("the reason must be verbatim: %q vs %q", item.Reason, m.Reason(item.Scope))
		}
		if item.Grants == "" {
			t.Errorf("%s has no plain-English grant sentence", item.Scope)
		}
	}
}

func TestAllowedHostsAreEmptyWhenTheScopeWasDeclined(t *testing.T) {
	m, err := ParseManifest([]byte(`{"id":"dev.you.app","name":"A","version":"1.0.0",
	 "description":"Talks to a host.","author":{"name":"You"},
	 "permissions":[{"scope":"net.fetch","reason":"To post the summary to your own wiki."}],
	 "triggers":[{"type":"phrase","match":"go"}],"allowedHosts":["api.example.com"]}`))
	if err != nil {
		t.Fatal(err)
	}
	declined := Installed{Manifest: m}
	if hosts := declined.AllowedHosts(); len(hosts) != 0 {
		t.Errorf("a host list without the scope authorises nothing, got %v", hosts)
	}
	granted := Installed{Manifest: m, Granted: []Scope{ScopeNetFetch}}
	if hosts := granted.AllowedHosts(); len(hosts) != 1 {
		t.Errorf("granted, the list is in force: %v", hosts)
	}
}
