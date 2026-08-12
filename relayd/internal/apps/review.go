package apps

import (
	"fmt"
	"sort"
	"strings"
)

// Review is the machine-checkable half of APP-PLATFORM.md §5's review posture:
// "the registry is a git repository and every listed app is a reviewed pull
// request."
//
// It is deliberately *not* wired into [ParseManifest]. A reviewer rejects an app
// for a vague reason; a daemon that refused to install one would be applying an
// editorial judgement to software the user has already chosen, and the rules
// below are heuristics rather than facts. So this returns findings a reviewer
// (or a registry CI job) acts on, and the accept/reject boundary stays exactly
// where the SDK put it.
//
// The rule it encodes is §2's: *"Microphone access" tells a user nothing.* A
// reason that restates the scope, or that says only that the app needs it, has
// not told the user anything the scope name did not already say.

// Severity is how much a finding matters.
type Severity string

const (
	// SeverityReject is a finding a reviewer should not merge past.
	SeverityReject Severity = "reject"
	// SeverityWarn is worth a comment on the pull request.
	SeverityWarn Severity = "warn"
)

// ReviewFinding is one review remark about a manifest.
type ReviewFinding struct {
	Severity Severity
	// Scope is the permission the finding is about, empty for manifest-wide
	// findings.
	Scope Scope
	// Rule names the check, so a registry can suppress one without suppressing
	// all of them.
	Rule string
	// Message is what a reviewer would write.
	Message string
}

func (f ReviewFinding) String() string {
	if f.Scope != "" {
		return fmt.Sprintf("%s [%s] %s: %s", f.Severity, f.Rule, f.Scope, f.Message)
	}
	return fmt.Sprintf("%s [%s] %s", f.Severity, f.Rule, f.Message)
}

// purposeMarkers are the words a sentence uses when it is about to say why.
// A reason with none of them is describing the permission, not the purpose.
var purposeMarkers = []string{
	"to ", "so ", "so that", "for ", "when ", "because", "in order",
	"whenever", "after ", "before ", "while ",
}

// boilerplate is the set of phrases that fill the space without using it.
var boilerplate = []string{
	"required", "needed for the app", "app to work", "app to function",
	"core functionality", "basic functionality", "for functionality",
	"see above", "self explanatory", "self-explanatory", "obvious",
	"internal use", "various purposes", "general use",
}

// Review checks a parsed manifest against the review rules.
func Review(m Manifest) []ReviewFinding {
	var out []ReviewFinding
	add := func(sev Severity, sc Scope, rule, msg string) {
		out = append(out, ReviewFinding{Severity: sev, Scope: sc, Rule: rule, Message: msg})
	}

	for _, p := range m.Permissions {
		reason := strings.TrimSpace(p.Reason)
		lower := strings.ToLower(reason)

		if restatesScope(lower, p.Scope) {
			add(SeverityReject, p.Scope, "reason-restates-scope",
				fmt.Sprintf("%q restates the scope. The user already saw %q — say what the app does with it.",
					reason, p.Scope.Grants()))
			continue
		}
		if words := len(strings.Fields(reason)); words < 4 {
			add(SeverityReject, p.Scope, "reason-too-short",
				fmt.Sprintf("%q is %d words. A reason is a sentence the user can act on.", reason, words))
			continue
		}
		if hit, ok := containsAny(lower, boilerplate); ok {
			add(SeverityReject, p.Scope, "reason-boilerplate",
				fmt.Sprintf("%q says %q, which is true of every permission an app asks for.", reason, hit))
			continue
		}
		if _, ok := containsAny(lower, purposeMarkers); !ok {
			add(SeverityWarn, p.Scope, "reason-states-no-purpose",
				fmt.Sprintf("%q does not say what it is for. The install sheet already says what the scope grants.", reason))
		}
	}

	// §3's exfiltration rule, stated as a review remark rather than only as a
	// runtime refusal: an app that can read the user's life and also talk to the
	// network deserves a reviewer's attention on *which* hosts, every time.
	if m.Requests(ScopeNetFetch) {
		var reading []Scope
		for _, s := range m.Scopes() {
			if s.ReadsYourLife() {
				reading = append(reading, s)
			}
		}
		if len(reading) > 0 {
			add(SeverityWarn, ScopeNetFetch, "egress-plus-read",
				fmt.Sprintf("this app holds %s and also talks to %s. Check that every host is one the app's "+
					"description explains.", joinScopes(reading), strings.Join(m.AllowedHosts, ", ")))
		}
		for _, h := range m.AllowedHosts {
			if strings.HasPrefix(h, "*.") && isPublicSuffixish(h) {
				add(SeverityReject, ScopeNetFetch, "wildcard-too-broad",
					fmt.Sprintf("%q is a wildcard over a whole public suffix — it is an allowlist that allows anybody.", h))
			}
		}
	}

	if strings.TrimSpace(m.Description) != "" && len(strings.Fields(m.Description)) < 4 {
		add(SeverityWarn, "", "description-too-short",
			"the description is what a user reads before installing; one or two words is not one.")
	}
	if m.Author.URL == "" && m.Author.Email == "" {
		add(SeverityWarn, "", "author-unreachable",
			"the author gives no url and no email, so a user with a question about this code has nowhere to go.")
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return out[i].Severity == SeverityReject
		}
		return out[i].Scope < out[j].Scope
	})
	return out
}

// Rejected reports whether any finding is a rejection.
func Rejected(fs []ReviewFinding) bool {
	for _, f := range fs {
		if f.Severity == SeverityReject {
			return true
		}
	}
	return false
}

// restatesScope reports whether the reason says nothing the scope did not.
//
// The test is deliberately narrow: every meaningful word in the reason also
// appears in the scope's own name or in what it grants. "Microphone access"
// against glasses.audio is a restatement; "To hear the room while you are in a
// meeting" is not, because "room" and "meeting" are the author's.
func restatesScope(lower string, s Scope) bool {
	vocab := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(string(s)+" "+s.Grants()), isNotWordByte) {
		vocab[stem(w)] = true
	}
	for _, w := range scopeSynonyms[s] {
		vocab[stem(w)] = true
	}
	words := strings.FieldsFunc(lower, isNotWordByte)
	meaningful := 0
	for _, w := range words {
		if stopWords[w] || len(w) < 3 {
			continue
		}
		meaningful++
		if !vocab[stem(w)] {
			return false
		}
	}
	return meaningful > 0
}

// scopeSynonyms are the words a lazy reason uses for a scope without using the
// scope's own vocabulary. "Microphone" never appears in `glasses.audio`.
var scopeSynonyms = map[Scope][]string{
	ScopeGlassesAudio:   {"microphone", "mic", "listen", "listening", "voice", "sound", "record", "recording"},
	ScopeGlassesCamera:  {"photo", "photos", "picture", "pictures", "image", "images", "snapshot", "shot"},
	ScopeGlassesSpeaker: {"speak", "speech", "talk", "audio", "voice", "say", "read", "aloud", "playback"},
	ScopeGlassesTouch:   {"tap", "taps", "gestures", "gesture", "button", "press"},
	ScopeMemoryRead:     {"history", "past", "archive", "recall", "retrieve", "look", "lookup", "access"},
	ScopeMemoryWrite:    {"save", "store", "record", "persist", "keep", "add", "writing"},
	ScopeAgentSession:   {"llm", "model", "ai", "assistant", "chat", "inference", "summarise", "summarize"},
	ScopeNetFetch:       {"internet", "network", "web", "api", "request", "requests", "call", "online", "server"},
	ScopeSchedule:       {"cron", "timer", "periodic", "recurring", "daily", "hourly", "weekly", "background"},
}

// stopWords carry no information about *purpose*. The second row is the one
// that matters: "access", "permission" and "required" are the words a
// restatement is built out of, and leaving them in would let "Microphone access"
// past the restatement rule and into the length rule — which rejects it for the
// wrong reason and would let "Microphone access for the app" through.
var stopWords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "of": true, "to": true,
	"for": true, "in": true, "on": true, "it": true, "is": true, "this": true, "that": true,
	"with": true, "your": true, "you": true, "my": true, "our": true, "we": true, "app": true,
	"needs": true, "need": true, "uses": true, "use": true, "used": true, "using": true,
	"can": true, "will": true, "its": true, "so": true, "at": true, "by": true, "from": true,

	"access": true, "accessing": true, "permission": true, "permissions": true,
	"required": true, "requires": true, "needed": true, "allow": true, "allows": true,
	"ability": true, "able": true, "feature": true, "support": true, "enable": true,
}

// stem is a two-rule stemmer: enough to tie "recording" to "record" and
// "photos" to "photo", and nothing like enough to be a linguistics claim.
func stem(w string) string {
	switch {
	case strings.HasSuffix(w, "ing") && len(w) > 5:
		return strings.TrimSuffix(w, "ing")
	case strings.HasSuffix(w, "es") && len(w) > 4:
		return strings.TrimSuffix(w, "es")
	case strings.HasSuffix(w, "s") && len(w) > 3:
		return strings.TrimSuffix(w, "s")
	}
	return w
}

func isNotWordByte(r rune) bool {
	return !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9')
}

func containsAny(s string, needles []string) (string, bool) {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return strings.TrimSpace(n), true
		}
	}
	return "", false
}

func joinScopes(ss []Scope) string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, string(s))
	}
	return strings.Join(out, " and ")
}

// isPublicSuffixish is a deliberately small check for "*.com"-shaped
// allowlists. It is not a public-suffix list — pulling one in would cost the
// static binary a megabyte of table to catch a case a human reviewer catches
// anyway — so it looks only at the shape of the last label.
func isPublicSuffixish(h string) bool {
	rest := strings.TrimPrefix(h, "*.")
	switch rest {
	case "com", "net", "org", "io", "dev", "app", "co", "ai", "cloud", "xyz":
		return true
	}
	return len(strings.Split(rest, ".")) == 1
}
