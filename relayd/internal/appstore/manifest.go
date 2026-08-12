// Package appstore is APP-PLATFORM.md §6: resolving an app from the registry,
// showing the permission sheet consent is given against, and keeping the record
// of what is installed on this box.
//
// Three things shape every file here.
//
// **The consent copy lives in one place.** `relay install` and the phone app
// (which drives the same API on the box, §6) must not each own a rendering of
// the permission sheet, because two renderings drift and the one people read
// stops being the one people agreed to. [NewSheet] builds the sheet; the CLI
// prints [Sheet.Text] and the API serves the same struct as JSON.
//
// **Resolution is an interface, and a fork is a config change.** §6 says
// forking `github.com/luthor007/relay-apps` is supported, so the registry is a
// [Source] — a GitHub repo, any HTTPS base URL, or a directory on disk —
// selected by a spec string. Nothing in this package assumes a central
// authority, a build service, or an API only we could run.
//
// **Nothing claims to have happened that did not.** The container runtime is
// APP-PLATFORM.md §8 step 2. An install records the grant and, with no
// [Provisioner] attached, reports [StateAwaitingRuntime] in plain words rather
// than saying "provisioned" and leaving the user to discover otherwise.
//
// # The seam to the runtime, and a duplication to remove
//
// [Provisioner] is the whole interface between this package and the app
// runtime: Describe, Provision, Deprovision. Attaching one is the only change
// `relay install` needs to start creating containers, and [Installer] calls it
// strictly after consent.
//
// internal/apps — the runtime — was written concurrently with this package and
// carries its own manifest parser, scope list and sheet rows, because it is the
// side that has to *enforce* a scope while this is the side that has to
// *resolve and describe* one before any code has arrived on the box. Two
// parsers of one file in one binary is a duplication to remove rather than to
// enshrine. Until a pass reconciles them,
// TestScopeVocabularyMatchesTheRuntime pins the closed list of scopes across
// the two by reading the source, so the sheet cannot describe a scope the
// runtime does not enforce, or omit one it does.
package appstore

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Scope is one of APP-PLATFORM.md §3's permission scopes.
//
// The list is closed. An unknown scope is a manifest this box refuses rather
// than a permission it silently ignores: an ignored scope reads to the app
// author as granted and to the user as absent from the sheet.
type Scope string

const (
	ScopeGlassesAudio   Scope = "glasses.audio"
	ScopeGlassesCamera  Scope = "glasses.camera"
	ScopeGlassesSpeaker Scope = "glasses.speaker"
	ScopeGlassesTouch   Scope = "glasses.touch"
	ScopeMemoryRead     Scope = "memory.read"
	ScopeMemoryWrite    Scope = "memory.write"
	ScopeAgentSession   Scope = "agent.session"
	ScopeNetFetch       Scope = "net.fetch"
	ScopeSchedule       Scope = "schedule"
)

// scopeGrants is APP-PLATFORM.md §3's "Grants" column, in the second person,
// because it is read by the person deciding — not by the app author.
//
// This map is the only description of a scope in the system. The sheet, the
// API and any future UI all read it, so "what memory.read means" cannot be one
// sentence in the CLI and a different one on the phone.
var scopeGrants = map[Scope]string{
	ScopeGlassesAudio:   "live microphone while a voice session is open",
	ScopeGlassesCamera:  "take a photo — never silently; the indicator LEDs light",
	ScopeGlassesSpeaker: "speak through your glasses",
	ScopeGlassesTouch:   "tap and gesture events",
	ScopeMemoryRead:     "search and read your episodes and transcripts",
	ScopeMemoryWrite:    "add notes, commitments and tags to your memory",
	ScopeAgentSession:   "send prompts to your agent and read the replies",
	ScopeNetFetch:       "reach the internet, limited to the hosts listed below",
	ScopeSchedule:       "wake on a schedule",
}

// Scopes lists every scope, in APP-PLATFORM.md §3's table order.
func Scopes() []Scope {
	return []Scope{
		ScopeGlassesAudio, ScopeGlassesCamera, ScopeGlassesSpeaker, ScopeGlassesTouch,
		ScopeMemoryRead, ScopeMemoryWrite, ScopeAgentSession, ScopeNetFetch, ScopeSchedule,
	}
}

// Known reports whether this box understands the scope at all.
func (s Scope) Known() bool { _, ok := scopeGrants[s]; return ok }

// Grants is the sentence shown next to the scope on the permission sheet.
func (s Scope) Grants() string { return scopeGrants[s] }

// Permission is one requested scope and the reason for it.
type Permission struct {
	Scope Scope `json:"scope"`
	// Reason is shown verbatim at install and is never paraphrased, truncated
	// or generated. APP-PLATFORM.md §2: a vague reason is a review rejection,
	// which only means anything if the reviewed sentence is the shown one.
	Reason string `json:"reason"`
}

// Trigger is APP-PLATFORM.md §4. One struct with a discriminant rather than
// five types, because it is stored, shipped as JSON and shown in a list.
type Trigger struct {
	Type string `json:"type"`
	// Match is the wake phrase, for type "phrase".
	Match string `json:"match,omitempty"`
	// Gesture is doubleTap | tripleTap | longPress, for type "touch".
	Gesture string `json:"gesture,omitempty"`
	// Event is the pipeline event, for type "memory".
	Event string `json:"event,omitempty"`
	// Cron is the schedule, in the user's timezone, for type "schedule".
	Cron string `json:"cron,omitempty"`
	// Description is what the agent is told the tool does, for type "tool".
	Description string `json:"description,omitempty"`
}

// Author identifies the person who wrote the app. They never receive user data
// (§1), so this is provenance, not a service relationship.
type Author struct {
	Name  string `json:"name"`
	URL   string `json:"url,omitempty"`
	Email string `json:"email,omitempty"`
}

// Manifest is `relay.json`, the Go mirror of apps/sdk/src/manifest.ts.
//
// The two parsers are held together by TestManifestRulesMatchTheSDK, which
// re-parses the TypeScript on every run. They cannot be one implementation —
// one is the box and one is the author's toolchain — but they can be pinned.
type Manifest struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Version      string       `json:"version"`
	Description  string       `json:"description"`
	Author       Author       `json:"author"`
	Permissions  []Permission `json:"permissions"`
	Triggers     []Trigger    `json:"triggers"`
	AllowedHosts []string     `json:"allowedHosts,omitempty"`
	TimeoutMS    int          `json:"timeoutMs,omitempty"`
}

// These four rules are duplicated from apps/sdk/src/manifest.ts and pinned to
// it by a test. Duplicated rather than shared because the SDK runs in the
// author's editor and this runs on the box, and the box does not get to trust
// that the author ran anything at all.
var (
	idPattern     = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9][a-z0-9-]*)+$`)
	semverPattern = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)
)

// minReasonLen is the SDK's floor on a permission reason. It does not make a
// reason good — only a human reviewer does that — but it does reject the empty
// string and the one-word restatement of the scope.
const minReasonLen = 10

// DefaultTimeoutMS is the wall-clock ceiling per invocation when a manifest
// does not set one. Matches the SDK's default.
const DefaultTimeoutMS = 30_000

// knownTriggers is APP-PLATFORM.md §4, with the field each type needs.
//
// This is stricter than the SDK, deliberately: the SDK checks only that
// `triggers` is a non-empty array, because an author's editor should not refuse
// a trigger a newer box might know. The box is the thing that has to *fire*
// them, and an app installed with a trigger nothing can fire is an app that
// silently never runs.
var knownTriggers = map[string]string{
	"phrase":   "match",
	"touch":    "gesture",
	"memory":   "event",
	"schedule": "cron",
	"tool":     "description",
}

var knownGestures = map[string]bool{"doubleTap": true, "tripleTap": true, "longPress": true}

// ParseManifest reads and validates a `relay.json`.
//
// Strict, and strict before anything is shown to a user: a manifest that
// half-parses produces a permission sheet that half-describes the app.
//
// Unknown *top-level* fields are ignored, matching the SDK, so a manifest
// written for a later version of the platform still installs. Unknown scopes
// and unknown trigger types are refused, because both are things the box would
// otherwise have to pretend to honour.
func ParseManifest(b []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("appstore: manifest is not valid JSON: %w", err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	if m.TimeoutMS == 0 {
		m.TimeoutMS = DefaultTimeoutMS
	}
	return m, nil
}

// Validate is ParseManifest's rules, available on a Manifest that arrived some
// other way — from the registry entry, or from an installed record being
// re-checked after an upgrade of this binary.
func (m Manifest) Validate() error {
	if !idPattern.MatchString(m.ID) {
		return fmt.Errorf("appstore: id must be reverse-DNS like %q, got %q", "dev.you.app-name", m.ID)
	}
	if !semverPattern.MatchString(m.Version) {
		return fmt.Errorf("appstore: %s: version must be semver, got %q", m.ID, m.Version)
	}
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("appstore: %s: name is required", m.ID)
	}
	if strings.TrimSpace(m.Description) == "" {
		return fmt.Errorf("appstore: %s: description is required", m.ID)
	}
	if strings.TrimSpace(m.Author.Name) == "" {
		return fmt.Errorf("appstore: %s: author.name is required", m.ID)
	}

	seen := make(map[Scope]bool, len(m.Permissions))
	for i, p := range m.Permissions {
		if !p.Scope.Known() {
			return fmt.Errorf("appstore: %s: permissions[%d].scope %q is not a scope this box knows; "+
				"the nine are %s", m.ID, i, p.Scope, joinScopes(Scopes()))
		}
		if len(strings.TrimSpace(p.Reason)) < minReasonLen {
			return fmt.Errorf("appstore: %s: permissions[%d] (%s) needs a reason a user can read — "+
				"it is shown verbatim on the install sheet", m.ID, i, p.Scope)
		}
		if seen[p.Scope] {
			return fmt.Errorf("appstore: %s: duplicate permission scope %q — "+
				"two reasons for one scope means the sheet shows one of them", m.ID, p.Scope)
		}
		seen[p.Scope] = true
	}

	if len(m.Triggers) == 0 {
		return fmt.Errorf("appstore: %s: an app needs at least one trigger, or nothing can start it", m.ID)
	}
	for i, t := range m.Triggers {
		field, ok := knownTriggers[t.Type]
		if !ok {
			return fmt.Errorf("appstore: %s: triggers[%d].type %q is not one of %s — "+
				"this box could never fire it", m.ID, i, t.Type, joinStrings(triggerTypes()))
		}
		if t.field(field) == "" {
			return fmt.Errorf("appstore: %s: triggers[%d] is a %q trigger with no %s",
				m.ID, i, t.Type, field)
		}
		if t.Type == "touch" && !knownGestures[t.Gesture] {
			return fmt.Errorf("appstore: %s: triggers[%d].gesture %q is not doubleTap, tripleTap or longPress",
				m.ID, i, t.Gesture)
		}
	}

	// §3's first runtime-enforced rule. An app holding memory.read plus
	// unrestricted egress is an exfiltration tool, so the hosts are named at
	// install time and enforced by the proxy rather than by the app.
	if seen[ScopeNetFetch] && len(m.AllowedHosts) == 0 {
		return fmt.Errorf("appstore: %s: net.fetch requires allowedHosts — "+
			"outbound traffic is default-deny and the hosts are declared up front", m.ID)
	}
	if len(m.AllowedHosts) > 0 && !seen[ScopeNetFetch] {
		return fmt.Errorf("appstore: %s: allowedHosts is declared without the net.fetch permission", m.ID)
	}
	for i, h := range m.AllowedHosts {
		if err := validateHost(h); err != nil {
			return fmt.Errorf("appstore: %s: allowedHosts[%d]: %w", m.ID, i, err)
		}
	}
	return nil
}

func (t Trigger) field(name string) string {
	switch name {
	case "match":
		return t.Match
	case "gesture":
		return t.Gesture
	case "event":
		return t.Event
	case "cron":
		return t.Cron
	case "description":
		return t.Description
	}
	return ""
}

func triggerTypes() []string { return []string{"phrase", "touch", "memory", "schedule", "tool"} }

// validateHost keeps an allowlist an allowlist.
//
// A bare "*" is refused rather than normalised: it is unrestricted egress
// wearing the costume of a declaration, and the sheet would show it as a
// restriction. A scheme or a path is refused because the proxy matches hosts,
// so "https://api.example.com/v1" would allow the whole host anyway while the
// sheet implied one path.
func validateHost(h string) error {
	switch {
	case h == "":
		return fmt.Errorf("empty host")
	case h == "*":
		return fmt.Errorf(`"*" is not an allowlist. Name the hosts, or drop net.fetch`)
	case strings.Contains(h, "://") || strings.Contains(h, "/"):
		return fmt.Errorf("%q is a URL; the egress proxy matches hosts, so name the host alone", h)
	case strings.ContainsAny(h, " \t"):
		return fmt.Errorf("%q contains whitespace", h)
	case h != strings.ToLower(h):
		return fmt.Errorf("%q must be lowercase, so two spellings cannot mean two rules", h)
	case strings.HasPrefix(h, "*") && !strings.HasPrefix(h, "*."):
		return fmt.Errorf("%q — a wildcard is only allowed as a whole leading label, like *.example.com", h)
	}
	return nil
}

// ShortName is the last label of the id — what `relay logs` and `relay remove`
// take, because nobody types a reverse-DNS id twice.
func (m Manifest) ShortName() string {
	if i := strings.LastIndex(m.ID, "."); i >= 0 {
		return m.ID[i+1:]
	}
	return m.ID
}

// ScopeList is the granted scopes in manifest order.
func (m Manifest) ScopeList() []Scope {
	out := make([]Scope, 0, len(m.Permissions))
	for _, p := range m.Permissions {
		out = append(out, p.Scope)
	}
	return out
}

// Reason returns the verbatim reason given for a scope.
func (m Manifest) Reason(s Scope) (string, bool) {
	for _, p := range m.Permissions {
		if p.Scope == s {
			return p.Reason, true
		}
	}
	return "", false
}

func joinScopes(s []Scope) string {
	out := make([]string, len(s))
	for i := range s {
		out[i] = string(s[i])
	}
	return joinStrings(out)
}

func joinStrings(s []string) string { return strings.Join(s, ", ") }
