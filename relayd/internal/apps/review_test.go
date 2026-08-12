package apps

import (
	"strings"
	"testing"
)

func manifestWithReason(scope Scope, reason string) Manifest {
	return Manifest{
		ID: "dev.you.app", Name: "App", Version: "1.0.0",
		Description: "Does a thing worth describing at length.",
		Author:      Author{Name: "You", URL: "https://example.com"},
		Permissions: []Permission{{Scope: scope, Reason: reason}},
		Triggers:    []Trigger{{Type: TriggerPhrase, Match: "go"}},
	}
}

func findingFor(fs []ReviewFinding, rule string) (ReviewFinding, bool) {
	for _, f := range fs {
		if f.Rule == rule {
			return f, true
		}
	}
	return ReviewFinding{}, false
}

func TestAReasonThatRestatesTheScopeIsRejected(t *testing.T) {
	// APP-PLATFORM.md §2's example of what does not count: "Microphone access"
	// tells a user nothing they did not already see.
	cases := []struct {
		scope  Scope
		reason string
	}{
		{ScopeGlassesAudio, "Microphone access"},
		{ScopeGlassesAudio, "For recording audio"},
		{ScopeGlassesCamera, "Camera access needed"},
		{ScopeMemoryRead, "To read your memory"},
		{ScopeNetFetch, "Network requests"},
		{ScopeGlassesSpeaker, "Speaker access"},
	}
	for _, tc := range cases {
		fs := Review(manifestWithReason(tc.scope, tc.reason))
		if _, ok := findingFor(fs, "reason-restates-scope"); !ok {
			t.Errorf("%q against %s should be rejected as a restatement; got %v", tc.reason, tc.scope, fs)
		}
		if !Rejected(fs) {
			t.Errorf("%q should be a rejection", tc.reason)
		}
	}
}

func TestARealReasonPasses(t *testing.T) {
	// The reasons the shipped example uses, plus a few in the same spirit.
	cases := []struct {
		scope  Scope
		reason string
	}{
		{ScopeMemoryRead, "To read the transcript of the meeting you just left."},
		{ScopeMemoryWrite, "To save the notes and commitments it extracts back to your memory."},
		{ScopeAgentSession, "To summarise the meeting using your own agent and your own model."},
		{ScopeGlassesSpeaker, "To read the commitments back to you when you ask."},
		{ScopeGlassesAudio, "To hear your answer when it asks whether to file a commitment."},
		{ScopeGlassesCamera, "To photograph the whiteboard when you say 'capture this'."},
	}
	for _, tc := range cases {
		fs := Review(manifestWithReason(tc.scope, tc.reason))
		if Rejected(fs) {
			t.Errorf("%q against %s should pass review, got %v", tc.reason, tc.scope, fs)
		}
	}
}

func TestBoilerplateAndSilenceAreCaught(t *testing.T) {
	fs := Review(manifestWithReason(ScopeMemoryRead, "This is required for the app to work properly."))
	if _, ok := findingFor(fs, "reason-boilerplate"); !ok {
		t.Errorf("boilerplate should be caught: %v", fs)
	}
	fs = Review(manifestWithReason(ScopeAgentSession, "Summarisation of the meeting transcript."))
	if _, ok := findingFor(fs, "reason-states-no-purpose"); !ok {
		t.Errorf("a reason that does not say what it is for should be flagged: %v", fs)
	}
	if Rejected(fs) {
		t.Error("…as a warning, not a rejection: this rule is a heuristic")
	}
}

func TestEgressPlusReadIsAlwaysWorthAReviewersAttention(t *testing.T) {
	m := Manifest{
		ID: "dev.you.app", Name: "App", Version: "1.0.0",
		Description: "Reads your meetings and posts them to your wiki.",
		Author:      Author{Name: "You", URL: "https://example.com"},
		Permissions: []Permission{
			{Scope: ScopeMemoryRead, Reason: "To read the transcript of the meeting you just left."},
			{Scope: ScopeNetFetch, Reason: "To post the summary to the wiki you configured."},
		},
		Triggers:     []Trigger{{Type: TriggerPhrase, Match: "go"}},
		AllowedHosts: []string{"wiki.example.com"},
	}
	f, ok := findingFor(Review(m), "egress-plus-read")
	if !ok {
		t.Fatalf("§3's exfiltration combination should be surfaced: %v", Review(m))
	}
	if !strings.Contains(f.Message, "wiki.example.com") {
		t.Errorf("the finding should name the hosts: %q", f.Message)
	}
	if Rejected(Review(m)) {
		t.Error("it is a warning: this is a legitimate app shape, it just needs eyes on the host list")
	}
}

func TestAWildcardOverAPublicSuffixIsRejected(t *testing.T) {
	m := Manifest{
		ID: "dev.you.app", Name: "App", Version: "1.0.0",
		Description: "Talks to everything.", Author: Author{Name: "You", URL: "https://example.com"},
		Permissions:  []Permission{{Scope: ScopeNetFetch, Reason: "To reach the services you configure."}},
		Triggers:     []Trigger{{Type: TriggerPhrase, Match: "go"}},
		AllowedHosts: []string{"*.com"},
	}
	if _, ok := findingFor(Review(m), "wildcard-too-broad"); !ok {
		t.Errorf("an allowlist that allows anybody should be rejected: %v", Review(m))
	}
	m.AllowedHosts = []string{"*.internal.example.com"}
	if _, ok := findingFor(Review(m), "wildcard-too-broad"); ok {
		t.Error("a wildcard over one's own subdomain is exactly what wildcards are for")
	}
}

func TestAnUnreachableAuthorIsAWarning(t *testing.T) {
	m := manifestWithReason(ScopeMemoryRead, "To read the transcript of the meeting you just left.")
	m.Author = Author{Name: "Anonymous"}
	if _, ok := findingFor(Review(m), "author-unreachable"); !ok {
		t.Errorf("a user with a question about this code has nowhere to go: %v", Review(m))
	}
}

func TestFindingsReadLikeAReviewerWroteThem(t *testing.T) {
	fs := Review(manifestWithReason(ScopeGlassesAudio, "Microphone access"))
	if len(fs) == 0 {
		t.Fatal("expected findings")
	}
	s := fs[0].String()
	for _, want := range []string{"reject", "reason-restates-scope", "glasses.audio"} {
		if !strings.Contains(s, want) {
			t.Errorf("%q does not contain %q", s, want)
		}
	}
}
