package apps

import (
	"strings"
	"testing"
)

// The structural half of APP-PLATFORM.md §3's last rule.
//
// "There is no 'record without indication' scope, and there never will be. The
// LEDs are wired to capture and apps cannot address them."
//
// A promise about the future needs something that fails when the future
// disagrees. This is it: the capability table and the scope vocabulary are both
// closed lists, and these tests walk them.

func TestNoCapabilityCanAddressTheIndicators(t *testing.T) {
	// Words a capability that reached the LEDs would have to use somewhere in
	// its name. The list is short and blunt on purpose — this is a tripwire for
	// the reviewer, not a classifier.
	forbidden := []string{
		"led", "indicator", "light", "lamp", "silent", "covert", "stealth",
		"suppress", "hide", "conceal", "discreet", "quiet",
	}
	for _, c := range capabilities {
		hay := strings.ToLower(string(c.Method) + " " + c.Object + " " + c.Member)
		for _, word := range forbidden {
			if strings.Contains(hay, word) {
				t.Errorf("capability %s looks like it addresses the capture indication. "+
					"§3 says there is no such capability and never will be", c.Method)
			}
		}
	}
	for _, s := range Scopes() {
		hay := strings.ToLower(string(s) + " " + s.Grants())
		for _, word := range []string{"led", "indicator", "silent", "covert", "suppress"} {
			if strings.Contains(hay, word) && !strings.Contains(hay, "never silent") {
				t.Errorf("scope %s reads like it could pay for silent capture", s)
			}
		}
	}
	// And the camera scope says what it is, in the sentence a user reads.
	if !strings.Contains(ScopeGlassesCamera.Grants(), "never silent") {
		t.Errorf("the camera scope's sentence must say the LEDs light: %q", ScopeGlassesCamera.Grants())
	}
}

func TestEveryCapabilityNamesTheScopeThatPaysForIt(t *testing.T) {
	// The scope-free set, and every member of it has an argument written down
	// next to its row. Four are self-directed — an app talking to its own
	// storage and its own log. The two `ui` methods are the exception and the
	// only one: they leave the box, and they are still free because a view
	// reaches nothing of the user's — it cannot read, cannot fetch, cannot
	// capture, carries no URL, and tells the app nothing back. Any *seventh* is
	// a capability that reaches the user's box for free and should be argued for
	// in review rather than added quietly.
	free := map[Method]bool{
		MethodStorageGet: true, MethodStorageSet: true, MethodStorageDelete: true, MethodLog: true,
		MethodUIRender: true, MethodUIAsk: true,
	}
	for _, c := range capabilities {
		if c.Requires == "" && !free[c.Method] {
			t.Errorf("%s costs no scope and is not one of the capabilities that argument covers", c.Method)
		}
		if c.Requires != "" && !c.Requires.Valid() {
			t.Errorf("%s requires %q, which is not a scope", c.Method, c.Requires)
		}
		if c.Member == "" {
			t.Errorf("%s does not land anywhere on ctx", c.Method)
		}
	}
}

func TestMintingIsExactlyTheGrantedSet(t *testing.T) {
	cases := []struct {
		granted []Scope
		want    []Method
	}{
		{nil, []Method{
			MethodLog, MethodStorageDelete, MethodStorageGet, MethodStorageSet,
			MethodUIAsk, MethodUIRender,
		}},
		{[]Scope{ScopeMemoryRead}, []Method{
			MethodLog, MethodMemoryExtract, MethodMemoryGet, MethodMemoryRecentEpisode,
			MethodMemorySearch, MethodStorageDelete, MethodStorageGet, MethodStorageSet,
			MethodUIAsk, MethodUIRender,
		}},
		{[]Scope{ScopeGlassesCamera}, []Method{
			MethodGlassesCapture, MethodLog, MethodStorageDelete, MethodStorageGet,
			MethodStorageSet, MethodUIAsk, MethodUIRender,
		}},
	}
	for _, tc := range cases {
		got := Methods(tc.granted)
		if len(got) != len(tc.want) {
			t.Fatalf("granted %v -> %v, want %v", tc.granted, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("granted %v -> %v, want %v", tc.granted, got, tc.want)
				break
			}
		}
	}
}

func TestAnObjectWithNoGrantedMembersIsNeverCreated(t *testing.T) {
	objects := Objects([]Scope{ScopeMemoryRead})
	for _, o := range objects {
		if o == "glasses" || o == "agent" {
			t.Errorf("ctx.%s appears for an app with only memory.read", o)
		}
	}
	// `ui` is there for every app, including this one, because drawing costs no
	// scope. `glasses` and `agent` are not, which is the property under test.
	if len(objects) != 3 || objects[0] != "memory" || objects[1] != "storage" || objects[2] != "ui" {
		t.Errorf("objects = %v, want memory, storage and ui", objects)
	}
}

func TestRequiredScopeKnowsWhatIsPartOfTheSDK(t *testing.T) {
	if _, ok := RequiredScope("glasses.setIndicator"); ok {
		t.Error("a method nobody declared must not be recognised")
	}
	sc, ok := RequiredScope(MethodMemoryWrite)
	if !ok || sc != ScopeMemoryWrite {
		t.Errorf("memory.write costs %q ok=%v", sc, ok)
	}
	sc, ok = RequiredScope(MethodLog)
	if !ok || sc != "" {
		t.Errorf("log is free and known: %q %v", sc, ok)
	}
}

func TestDescribeNeverMentionsWhatTheAppDoesNotHave(t *testing.T) {
	s := Describe([]Scope{ScopeMemoryRead})
	if strings.Contains(s, "camera") || strings.Contains(s, "not") {
		t.Errorf("the console sentence should say what the app can do, not what it cannot: %q", s)
	}
	if Describe(nil) == "" {
		t.Error("an app with no scopes still needs a sentence")
	}
}
