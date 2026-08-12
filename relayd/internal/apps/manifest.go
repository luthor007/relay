package apps

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The manifest is `apps/sdk/src/manifest.ts`, not a second format invented here.
//
// Two implementations of one file format is how a package installs with
// permissions nobody reviewed: the sheet the SDK's author saw and the grant the
// daemon minted would come from different parsers. So the vocabularies below —
// the nine scopes, the five trigger types, the three gestures, the four memory
// events, the id and version patterns, the ten-character reason floor — are
// checked against the TypeScript on every run by TestManifestDoesNotDriftFromSDK,
// which re-parses that file rather than trusting this comment.
//
// One divergence exists and is deliberate: the TypeScript casts `triggers`
// without looking inside it, and this parser validates trigger shape. A trigger
// this runtime cannot honour is an app that can never be woken, and accepting it
// silently is the failure APP-PLATFORM.md §4 exists to prevent. It is stricter
// in the same format, never a different one.

// Scope is one permission from APP-PLATFORM.md §3.
//
// The vocabulary is closed, and closing it is load-bearing rather than tidy:
// this is the list §3's last rule is about. There is no scope for driving the
// capture indicators, there is no scope for suppressing them, and the only way
// to add one is to add a constant here — where [TestNoCapabilityCanAddressTheIndicators]
// and the review of this file are waiting.
type Scope string

const (
	// ScopeGlassesAudio is the live microphone during an open voice session.
	ScopeGlassesAudio Scope = "glasses.audio"
	// ScopeGlassesCamera captures a still. Never silent: the indicator LEDs are
	// wired to capture, and [Glasses.Capture] cannot reach the camera without
	// going through them first.
	ScopeGlassesCamera Scope = "glasses.camera"
	// ScopeGlassesSpeaker speaks through the glasses.
	ScopeGlassesSpeaker Scope = "glasses.speaker"
	// ScopeGlassesTouch receives tap and gesture events.
	ScopeGlassesTouch Scope = "glasses.touch"
	// ScopeMemoryRead searches and reads the user's episodes and transcripts.
	ScopeMemoryRead Scope = "memory.read"
	// ScopeMemoryWrite adds notes, commitments and tags.
	ScopeMemoryWrite Scope = "memory.write"
	// ScopeAgentSession sends prompts to the user's agent and reads replies.
	ScopeAgentSession Scope = "agent.session"
	// ScopeNetFetch is outbound HTTP, restricted to the manifest's allowedHosts.
	ScopeNetFetch Scope = "net.fetch"
	// ScopeSchedule wakes the app on a cron-like schedule.
	ScopeSchedule Scope = "schedule"
)

// Scopes is every scope, in the order APP-PLATFORM.md §3's table lists them.
func Scopes() []Scope {
	return []Scope{
		ScopeGlassesAudio, ScopeGlassesCamera, ScopeGlassesSpeaker, ScopeGlassesTouch,
		ScopeMemoryRead, ScopeMemoryWrite, ScopeAgentSession, ScopeNetFetch, ScopeSchedule,
	}
}

// Valid reports whether s is one of the nine. An unknown scope is refused rather
// than ignored: a manifest asking for something this runtime does not understand
// is a manifest whose install sheet would be a lie by omission.
func (s Scope) Valid() bool {
	for _, k := range Scopes() {
		if k == s {
			return true
		}
	}
	return false
}

// Grants is the sentence shown next to the app author's own reason on the
// install sheet — APP-PLATFORM.md §3's right-hand column, verbatim. The author
// says why; this says what.
func (s Scope) Grants() string {
	switch s {
	case ScopeGlassesAudio:
		return "live microphone during an open voice session"
	case ScopeGlassesCamera:
		return "capture a still; never silent capture — the LEDs light"
	case ScopeGlassesSpeaker:
		return "speak through the glasses"
	case ScopeGlassesTouch:
		return "tap and gesture events"
	case ScopeMemoryRead:
		return "search and read your episodes and transcripts"
	case ScopeMemoryWrite:
		return "add notes, commitments and tags"
	case ScopeAgentSession:
		return "send prompts to your agent and read the replies"
	case ScopeNetFetch:
		return "outbound HTTP, to a host allowlist declared in the manifest"
	case ScopeSchedule:
		return "wake on a cron-like schedule"
	}
	return ""
}

// ReadsYourLife reports whether this scope reaches the user's own data or
// senses.
//
// It is the predicate behind APP-PLATFORM.md §3's exfiltration rule — "an app
// with memory.read and unrestricted network access is an exfiltration tool" —
// and [Runtime] uses it to decide whether a sandbox that cannot *enforce*
// network isolation is good enough to run this particular app on. Writing is not
// on the list: an app that can add a note but never read one has nothing to
// exfiltrate.
func (s Scope) ReadsYourLife() bool {
	switch s {
	case ScopeMemoryRead, ScopeGlassesAudio, ScopeGlassesCamera, ScopeAgentSession:
		return true
	}
	return false
}

// Permission is one requested scope and the sentence shown for it at install.
type Permission struct {
	Scope Scope `json:"scope"`
	// Reason is shown verbatim on the install sheet. Mandatory, and reviewed:
	// "Microphone access" restates the scope and tells the user nothing they did
	// not already see. See [Review].
	Reason string `json:"reason"`
}

// TriggerType is how an app gets woken — APP-PLATFORM.md §4. Apps do not poll.
type TriggerType string

const (
	// TriggerPhrase fires on a wake phrase in the live transcript.
	TriggerPhrase TriggerType = "phrase"
	// TriggerTouch fires on a gesture.
	TriggerTouch TriggerType = "touch"
	// TriggerMemory fires on a pipeline event.
	TriggerMemory TriggerType = "memory"
	// TriggerSchedule fires on a cron expression in the user's timezone.
	TriggerSchedule TriggerType = "schedule"
	// TriggerTool is the agent deciding to call the app as a tool. §4: this is
	// the interesting one — an installed app is automatically an MCP tool.
	TriggerTool TriggerType = "tool"
)

// TriggerTypes is every trigger type.
func TriggerTypes() []TriggerType {
	return []TriggerType{TriggerPhrase, TriggerTouch, TriggerMemory, TriggerSchedule, TriggerTool}
}

// Gesture is a touch trigger's gesture.
type Gesture string

const (
	GestureDoubleTap Gesture = "doubleTap"
	GestureTripleTap Gesture = "tripleTap"
	GestureLongPress Gesture = "longPress"
)

// Gestures is every gesture the glasses report.
func Gestures() []Gesture { return []Gesture{GestureDoubleTap, GestureTripleTap, GestureLongPress} }

// MemoryEvent is a pipeline event an app can be woken by.
type MemoryEvent string

const (
	// EventMeetingEnded fires when segmentation closes a meeting episode.
	EventMeetingEnded MemoryEvent = "meeting.ended"
	// EventCommitmentDetected fires when extraction finds a commitment.
	EventCommitmentDetected MemoryEvent = "commitment.detected"
	// EventDaySynced fires when the nightly bulk sync has landed and been
	// transcribed.
	EventDaySynced MemoryEvent = "day.synced"
	// EventEpisodeCreated fires for every episode, of any kind.
	EventEpisodeCreated MemoryEvent = "episode.created"
)

// MemoryEvents is every pipeline event.
func MemoryEvents() []MemoryEvent {
	return []MemoryEvent{EventMeetingEnded, EventCommitmentDetected, EventDaySynced, EventEpisodeCreated}
}

// Trigger is one entry in the manifest's `triggers` array. Exactly one of the
// per-type fields is meaningful, decided by Type.
type Trigger struct {
	Type TriggerType `json:"type"`

	// Match is the wake phrase, for TriggerPhrase.
	Match string `json:"match,omitempty"`
	// Gesture is the gesture, for TriggerTouch.
	Gesture Gesture `json:"gesture,omitempty"`
	// Event is the pipeline event, for TriggerMemory.
	Event MemoryEvent `json:"event,omitempty"`
	// Cron is the five-field expression, for TriggerSchedule.
	Cron string `json:"cron,omitempty"`
	// Description is what the agent reads to decide whether to call this app,
	// for TriggerTool.
	Description string `json:"description,omitempty"`
}

// Author identifies who wrote the app. url and email are optional; the name is
// not, because the install sheet has to say whose code this is.
type Author struct {
	Name  string `json:"name"`
	URL   string `json:"url,omitempty"`
	Email string `json:"email,omitempty"`
}

// Manifest is a parsed `relay.json`.
type Manifest struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Version     string       `json:"version"`
	Description string       `json:"description"`
	Author      Author       `json:"author"`
	Permissions []Permission `json:"permissions"`
	Triggers    []Trigger    `json:"triggers"`
	// AllowedHosts is the egress allowlist. Empty without net.fetch, and
	// net.fetch without it does not parse.
	AllowedHosts []string `json:"allowedHosts,omitempty"`
	// TimeoutMs is the wall-clock ceiling per invocation. Defaults to
	// DefaultTimeoutMs, and is clamped by the runtime's own ceiling — an app
	// does not get to declare that it may hold the box for an hour.
	TimeoutMs int `json:"timeoutMs,omitempty"`
}

// DefaultTimeoutMs matches the SDK's default.
const DefaultTimeoutMs = 30_000

// ManifestError is a manifest that will not install.
type ManifestError struct{ Message string }

func (e *ManifestError) Error() string { return "apps: " + e.Message }

func manifestErrf(format string, a ...any) error {
	return &ManifestError{Message: fmt.Sprintf(format, a...)}
}

// The two patterns, copied from the SDK and pinned against it by the drift test.
var (
	idPattern     = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9][a-z0-9-]*)+$`)
	semverPattern = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)
)

// MinReasonLength is the SDK's floor on a permission reason. It is a floor and
// not a review: ten characters keeps "yes" out, and [Review] is what catches
// "Microphone access".
const MinReasonLength = 10

type rawManifest struct {
	ID           *string            `json:"id"`
	Name         *string            `json:"name"`
	Version      *string            `json:"version"`
	Description  *string            `json:"description"`
	Author       *Author            `json:"author"`
	Permissions  *[]rawPermission   `json:"permissions"`
	Triggers     *[]json.RawMessage `json:"triggers"`
	AllowedHosts *[]string          `json:"allowedHosts"`
	TimeoutMs    *float64           `json:"timeoutMs"`
}

type rawPermission struct {
	Scope  *string `json:"scope"`
	Reason *string `json:"reason"`
}

type rawTrigger struct {
	Type        *string `json:"type"`
	Match       *string `json:"match"`
	Gesture     *string `json:"gesture"`
	Event       *string `json:"event"`
	Cron        *string `json:"cron"`
	Description *string `json:"description"`
}

// ParseManifest validates a `relay.json`.
//
// Strict, and strict early, for the reason the SDK states: a manifest that
// half-parses installs an app with permissions nobody reviewed.
func ParseManifest(data []byte) (Manifest, error) {
	var raw rawManifest
	dec := json.NewDecoder(strings.NewReader(string(data)))
	if err := dec.Decode(&raw); err != nil {
		return Manifest{}, manifestErrf("manifest is not valid JSON: %v", err)
	}

	var m Manifest
	if raw.ID == nil || !idPattern.MatchString(*raw.ID) {
		return m, manifestErrf(`id must be reverse-DNS like "dev.you.app-name", got %s`, quoteOrNull(raw.ID))
	}
	m.ID = *raw.ID

	if raw.Version == nil || !semverPattern.MatchString(*raw.Version) {
		return m, manifestErrf("version must be semver, got %s", quoteOrNull(raw.Version))
	}
	m.Version = *raw.Version

	if raw.Name == nil || strings.TrimSpace(*raw.Name) == "" {
		return m, manifestErrf("name is required")
	}
	m.Name = *raw.Name
	if raw.Description == nil || strings.TrimSpace(*raw.Description) == "" {
		return m, manifestErrf("description is required")
	}
	m.Description = *raw.Description

	if raw.Author == nil || strings.TrimSpace(raw.Author.Name) == "" {
		return m, manifestErrf("author.name is required")
	}
	m.Author = *raw.Author

	if raw.Permissions == nil {
		return m, manifestErrf("permissions must be an array (use [] for none)")
	}
	seen := map[Scope]bool{}
	for i, p := range *raw.Permissions {
		if p.Scope == nil || !Scope(*p.Scope).Valid() {
			return m, manifestErrf("permissions[%d].scope is not a known scope: %s", i, valueOrNull(p.Scope))
		}
		sc := Scope(*p.Scope)
		if p.Reason == nil || len(strings.TrimSpace(*p.Reason)) < MinReasonLength {
			return m, manifestErrf(
				"permissions[%d].reason must explain why, in a sentence a user can read (scope: %s)", i, sc)
		}
		if seen[sc] {
			return m, manifestErrf("duplicate permission scopes")
		}
		seen[sc] = true
		m.Permissions = append(m.Permissions, Permission{Scope: sc, Reason: *p.Reason})
	}

	if raw.Triggers == nil || len(*raw.Triggers) == 0 {
		return m, manifestErrf("an app needs at least one trigger, or nothing can start it")
	}
	for i, rt := range *raw.Triggers {
		t, err := parseTrigger(rt)
		if err != nil {
			return m, manifestErrf("triggers[%d]: %s", i, strings.TrimPrefix(err.Error(), "apps: "))
		}
		m.Triggers = append(m.Triggers, t)
	}

	hosts := []string{}
	if raw.AllowedHosts != nil {
		hosts = *raw.AllowedHosts
	}
	if seen[ScopeNetFetch] && len(hosts) == 0 {
		return m, manifestErrf("net.fetch requires allowedHosts — unrestricted egress plus memory.read is an " +
			"exfiltration tool, so the hosts are declared up front")
	}
	if raw.AllowedHosts != nil && !seen[ScopeNetFetch] {
		return m, manifestErrf("allowedHosts declared without the net.fetch permission")
	}
	for i, h := range hosts {
		if err := validHostPattern(h); err != nil {
			return m, manifestErrf("allowedHosts[%d]: %v", i, err)
		}
		m.AllowedHosts = append(m.AllowedHosts, strings.ToLower(strings.TrimSpace(h)))
	}

	m.TimeoutMs = DefaultTimeoutMs
	if raw.TimeoutMs != nil {
		m.TimeoutMs = int(*raw.TimeoutMs)
	}
	return m, nil
}

func parseTrigger(data json.RawMessage) (Trigger, error) {
	var rt rawTrigger
	if err := json.Unmarshal(data, &rt); err != nil {
		return Trigger{}, manifestErrf("not an object: %v", err)
	}
	if rt.Type == nil {
		return Trigger{}, manifestErrf("type is required")
	}
	t := Trigger{Type: TriggerType(*rt.Type)}
	switch t.Type {
	case TriggerPhrase:
		if rt.Match == nil || strings.TrimSpace(*rt.Match) == "" {
			return t, manifestErrf("a phrase trigger needs a non-empty match")
		}
		t.Match = strings.TrimSpace(*rt.Match)
	case TriggerTouch:
		if rt.Gesture == nil {
			return t, manifestErrf("a touch trigger needs a gesture")
		}
		g := Gesture(*rt.Gesture)
		if !g.valid() {
			return t, manifestErrf("unknown gesture %q — the glasses report doubleTap, tripleTap and longPress", *rt.Gesture)
		}
		t.Gesture = g
	case TriggerMemory:
		if rt.Event == nil {
			return t, manifestErrf("a memory trigger needs an event")
		}
		ev := MemoryEvent(*rt.Event)
		if !ev.valid() {
			return t, manifestErrf("unknown pipeline event %q", *rt.Event)
		}
		t.Event = ev
	case TriggerSchedule:
		if rt.Cron == nil {
			return t, manifestErrf("a schedule trigger needs a cron expression")
		}
		if _, err := ParseCron(*rt.Cron); err != nil {
			return t, manifestErrf("%v", err)
		}
		t.Cron = strings.TrimSpace(*rt.Cron)
	case TriggerTool:
		if rt.Description == nil || strings.TrimSpace(*rt.Description) == "" {
			return t, manifestErrf("a tool trigger needs a description — it is what the agent reads to decide whether to call it")
		}
		t.Description = strings.TrimSpace(*rt.Description)
	default:
		return t, manifestErrf("unknown trigger type %q — an app woken by nothing this runtime understands can never run", *rt.Type)
	}
	return t, nil
}

func (g Gesture) valid() bool {
	for _, k := range Gestures() {
		if k == g {
			return true
		}
	}
	return false
}

func (e MemoryEvent) valid() bool {
	for _, k := range MemoryEvents() {
		if k == e {
			return true
		}
	}
	return false
}

// hostPattern allows a hostname or a single leading-label wildcard. No bare
// wildcard, no scheme, no port, no path: an allowlist entry that matches
// everything is the thing the allowlist exists to prevent, and one carrying a
// path would suggest a granularity the proxy does not have.
var hostPattern = regexp.MustCompile(`^(\*\.)?[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

func validHostPattern(h string) error {
	s := strings.ToLower(strings.TrimSpace(h))
	switch {
	case s == "":
		return fmt.Errorf("empty host")
	case s == "*":
		return fmt.Errorf(`"*" is not an allowlist — declare the hosts this app talks to`)
	case strings.Contains(s, "/"), strings.Contains(s, ":"):
		return fmt.Errorf("%q must be a bare host, with no scheme, port or path", h)
	case !hostPattern.MatchString(s):
		return fmt.Errorf("%q is not a hostname or a *.suffix wildcard", h)
	}
	return nil
}

// Scopes lists the scopes this manifest requests, in manifest order.
func (m Manifest) Scopes() []Scope {
	out := make([]Scope, 0, len(m.Permissions))
	for _, p := range m.Permissions {
		out = append(out, p.Scope)
	}
	return out
}

// Requests reports whether the manifest asked for a scope.
func (m Manifest) Requests(s Scope) bool {
	for _, p := range m.Permissions {
		if p.Scope == s {
			return true
		}
	}
	return false
}

// Reason returns the author's sentence for a scope.
func (m Manifest) Reason(s Scope) string {
	for _, p := range m.Permissions {
		if p.Scope == s {
			return p.Reason
		}
	}
	return ""
}

// ToolTrigger returns the tool trigger, if the app declared one. An app with one
// is exposed to the user's agent as an MCP tool — APP-PLATFORM.md §4.
func (m Manifest) ToolTrigger() (Trigger, bool) {
	for _, t := range m.Triggers {
		if t.Type == TriggerTool {
			return t, true
		}
	}
	return Trigger{}, false
}

// Validate is the cross-field pass ParseManifest deliberately does not do,
// because these are questions about whether the app can *work* rather than about
// whether the file is well formed.
//
// It is what [Install] runs, and the errors are the ones worth surfacing at
// install time rather than as a trigger that silently never fires.
func (m Manifest) Validate() error {
	has := map[Scope]bool{}
	for _, s := range m.Scopes() {
		has[s] = true
	}
	for _, t := range m.Triggers {
		if t.Type == TriggerSchedule && !has[ScopeSchedule] {
			return manifestErrf("a schedule trigger without the schedule permission can never fire — " +
				"add it, with a reason, so the user sees it at install")
		}
	}
	if has[ScopeGlassesTouch] && !m.hasTrigger(TriggerTouch) {
		// Not fatal: an app may hold glasses.touch to read gestures during an
		// invocation started some other way. Nothing to do here, and saying so
		// beats a rule that looks like it fires and does not.
		_ = has
	}
	if m.TimeoutMs < 0 {
		return manifestErrf("timeoutMs cannot be negative")
	}
	return nil
}

func (m Manifest) hasTrigger(t TriggerType) bool {
	for _, x := range m.Triggers {
		if x.Type == t {
			return true
		}
	}
	return false
}

// SheetItem is one row of the install sheet.
type SheetItem struct {
	Scope Scope
	// Grants is what the scope actually gives, in Relay's words.
	Grants string
	// Reason is the author's sentence, verbatim. APP-PLATFORM.md §2: shown
	// verbatim at install, never paraphrased and never summarised — the point of
	// requiring it is that the user reads what the author wrote.
	Reason string
}

// Sheet is what the user is asked to accept, in a stable order.
//
// [Install] takes the answer to this and nothing wider: a consent that named a
// scope the manifest never requested is refused rather than granted, because the
// sheet is the only thing that was reviewed.
func Sheet(m Manifest) []SheetItem {
	out := make([]SheetItem, 0, len(m.Permissions))
	for _, p := range m.Permissions {
		out = append(out, SheetItem{Scope: p.Scope, Grants: p.Scope.Grants(), Reason: p.Reason})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Scope < out[j].Scope })
	return out
}

func quoteOrNull(s *string) string {
	if s == nil {
		return "undefined"
	}
	return fmt.Sprintf("%q", *s)
}

func valueOrNull(s *string) string {
	if s == nil {
		return "undefined"
	}
	return *s
}
