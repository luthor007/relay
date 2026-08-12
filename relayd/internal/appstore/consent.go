package appstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// The permission sheet.
//
// APP-PLATFORM.md §6: "Install resolves the package, shows the permission sheet
// with each `reason`, waits for consent, then provisions the container. The
// phone app does the same through UI, against the same API on the box."
//
// Same sheet, therefore, and not two implementations of one idea. [NewSheet]
// builds it from the registry entry; `relay install` prints [Sheet.Text] and
// the API serves the struct. When the copy is wrong it is wrong in one place,
// and when a scope is added the sheet cannot forget it — [NewSheet] reads the
// manifest, and an unknown scope never reaches here because [Manifest.Validate]
// refuses it first.

// SheetRow is one permission, as it appears to the person deciding.
type SheetRow struct {
	Scope Scope `json:"scope"`
	// Grants is this box's description of the scope. Ours, identical for every
	// app, so an app cannot describe what it is asking for.
	Grants string `json:"grants"`
	// Reason is the app author's sentence, verbatim. Never rewritten, never
	// truncated, never generated — it is the thing the review reviewed.
	Reason string `json:"reason"`
}

// Upgrade describes what changed when an installed app is being replaced.
//
// It exists because a silent upgrade is how an app that asked for memory.read
// ends up holding glasses.camera. New scopes and changed reasons re-ask.
type Upgrade struct {
	FromVersion string `json:"from_version"`
	// NewScopes were not granted before.
	NewScopes []Scope `json:"new_scopes,omitempty"`
	// ChangedReasons kept the scope and changed the sentence the user agreed
	// to. That is a new sentence, so it is a new decision.
	ChangedReasons []Scope `json:"changed_reasons,omitempty"`
	// Dropped were granted before and are not requested now. Not a reason to
	// re-ask — it is strictly less access — but it is worth showing.
	Dropped []Scope `json:"dropped,omitempty"`
}

// NeedsConsent reports whether this upgrade widens what the app may do.
func (u Upgrade) NeedsConsent() bool { return len(u.NewScopes) > 0 || len(u.ChangedReasons) > 0 }

// Sheet is everything shown before consent, in one struct.
type Sheet struct {
	AppID       string `json:"app_id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Author      Author `json:"author"`
	// Registry is the spec the app was resolved from — part of the decision,
	// because a forked registry is a different review process.
	Registry string `json:"registry"`
	// Origin is the repository the code will be fetched from.
	Origin EntrySource `json:"origin"`
	// Review is the pull request that listed the app, when the registry
	// recorded one.
	Review       string     `json:"review,omitempty"`
	Rows         []SheetRow `json:"permissions"`
	AllowedHosts []string   `json:"allowed_hosts,omitempty"`
	Triggers     []string   `json:"triggers"`
	// Notices are the things this box enforces regardless of what the app says,
	// and the ones it cannot enforce. Both matter to the decision.
	Notices []string `json:"notices"`
	// Upgrade is set when something is already installed under this id.
	Upgrade *Upgrade `json:"upgrade,omitempty"`
}

// NewSheet builds the sheet for an entry. prev is the currently-installed app,
// or nil.
func NewSheet(e Entry, registry string, prev *Installed) Sheet {
	m := e.Manifest
	s := Sheet{
		AppID:        m.ID,
		Name:         m.Name,
		Version:      m.Version,
		Description:  m.Description,
		Author:       m.Author,
		Registry:     registry,
		Origin:       e.Source,
		Review:       e.Review,
		AllowedHosts: append([]string(nil), m.AllowedHosts...),
	}
	for _, p := range m.Permissions {
		s.Rows = append(s.Rows, SheetRow{Scope: p.Scope, Grants: p.Scope.Grants(), Reason: p.Reason})
	}
	for _, t := range m.Triggers {
		s.Triggers = append(s.Triggers, describeTrigger(t))
	}
	s.Notices = notices(m, registry)
	if prev != nil {
		up := diffGrants(*prev, m)
		up.FromVersion = prev.Manifest.Version
		s.Upgrade = &up
	}
	return s
}

// notices is what the box guarantees and what it does not.
//
// Each line is either enforced by the runtime or is a limit of the design. None
// of them is a promise about the app's behaviour, because the whole point of
// §5 is that we do not have one.
func notices(m Manifest, registry string) []string {
	has := func(s Scope) bool { _, ok := m.Reason(s); return ok }
	var out []string
	// §1. The single most load-bearing fact about the platform.
	out = append(out, "This app runs on your box. The author never receives your data.")
	if has(ScopeNetFetch) {
		out = append(out, "Outbound traffic is default-deny: a proxy allows only the hosts listed "+
			"above and blocks everything else.")
	} else {
		out = append(out, "It has no network access at all.")
	}
	if has(ScopeMemoryRead) {
		out = append(out, "Every memory read is recorded — you can see which app touched which episode.")
	}
	if has(ScopeGlassesCamera) {
		out = append(out, "Capture lights the indicator LEDs. There is no silent-capture scope and "+
			"apps cannot address the LEDs.")
	}
	out = append(out, fmt.Sprintf("Listed in %s. Listing means someone reviewed a pull request. "+
		"It is a human reading a manifest, not a guarantee.", registry))
	return out
}

func describeTrigger(t Trigger) string {
	switch t.Type {
	case "phrase":
		return fmt.Sprintf("when you say %q", t.Match)
	case "touch":
		return "on a " + t.Gesture
	case "memory":
		return "on " + t.Event
	case "schedule":
		return "on a schedule (" + t.Cron + ")"
	case "tool":
		return "when your agent decides to call it — " + t.Description
	}
	return t.Type
}

// diffGrants compares what is installed against what is being asked for.
func diffGrants(prev Installed, next Manifest) Upgrade {
	var u Upgrade
	old := make(map[Scope]string, len(prev.Grants))
	for _, g := range prev.Grants {
		old[g.Scope] = g.Reason
	}
	for _, p := range next.Permissions {
		reason, had := old[p.Scope]
		switch {
		case !had:
			u.NewScopes = append(u.NewScopes, p.Scope)
		case reason != p.Reason:
			u.ChangedReasons = append(u.ChangedReasons, p.Scope)
		}
	}
	asked := make(map[Scope]bool, len(next.Permissions))
	for _, p := range next.Permissions {
		asked[p.Scope] = true
	}
	for _, g := range prev.Grants {
		if !asked[g.Scope] {
			u.Dropped = append(u.Dropped, g.Scope)
		}
	}
	return u
}

// Grants is what consent, if given, grants — the exact permissions, with the
// exact sentences that were shown. Stored with the install so `relay list` can
// answer "what did I agree to" with the text, not a reconstruction.
func (s Sheet) Grants() []Permission {
	out := make([]Permission, 0, len(s.Rows))
	for _, r := range s.Rows {
		out = append(out, Permission{Scope: r.Scope, Reason: r.Reason})
	}
	return out
}

// Text renders the sheet for a terminal. The phone renders the same struct
// natively; this is the same content, wrapped.
func (s Sheet) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s %s\n", s.Name, s.Version)
	fmt.Fprintf(&b, "%s\n", Wrap(s.Description, 76))
	author := s.Author.Name
	if s.Author.URL != "" {
		author += " — " + s.Author.URL
	}
	fmt.Fprintf(&b, "by %s\n\n", author)

	fmt.Fprintf(&b, "  id        %s\n", s.AppID)
	fmt.Fprintf(&b, "  registry  %s\n", s.Registry)
	fmt.Fprintf(&b, "  code      %s", s.Origin.Git)
	if s.Origin.Rev != "" {
		fmt.Fprintf(&b, " @ %s", s.Origin.Rev)
	}
	b.WriteString("\n")
	if s.Review != "" {
		fmt.Fprintf(&b, "  reviewed  %s\n", s.Review)
	}

	if s.Upgrade != nil {
		fmt.Fprintf(&b, "\nUpgrading from %s.\n", s.Upgrade.FromVersion)
		if len(s.Upgrade.NewScopes) > 0 {
			fmt.Fprintf(&b, "  It is asking for permissions it did not have: %s\n",
				joinScopes(s.Upgrade.NewScopes))
		}
		if len(s.Upgrade.ChangedReasons) > 0 {
			fmt.Fprintf(&b, "  The reason changed for: %s\n", joinScopes(s.Upgrade.ChangedReasons))
		}
		if len(s.Upgrade.Dropped) > 0 {
			fmt.Fprintf(&b, "  It no longer wants: %s\n", joinScopes(s.Upgrade.Dropped))
		}
	}

	if len(s.Rows) == 0 {
		b.WriteString("\nIt asks for no permissions.\n")
	} else {
		b.WriteString("\nIt wants to:\n\n")
		for _, r := range s.Rows {
			fmt.Fprintf(&b, "  %-16s %s\n", r.Scope, r.Grants)
			// The author's sentence, on its own line, quoted so it reads as
			// theirs and not as ours — and deliberately NOT wrapped, truncated
			// or re-punctuated. Everything else on this sheet is wrapped at 76;
			// a reason is the one string that leaves this function byte for
			// byte as the reviewer read it, so a long one soft-wraps in the
			// terminal rather than being reflowed here.
			fmt.Fprintf(&b, "  %-16s “%s”\n", "", r.Reason)
		}
	}
	if len(s.AllowedHosts) > 0 {
		fmt.Fprintf(&b, "\n  hosts it may reach: %s\n", strings.Join(s.AllowedHosts, ", "))
	}
	if len(s.Triggers) > 0 {
		b.WriteString("\nIt runs:\n")
		for _, t := range s.Triggers {
			fmt.Fprintf(&b, "  - %s\n", t)
		}
	}
	b.WriteString("\n")
	for _, n := range s.Notices {
		fmt.Fprintf(&b, "%s\n", Wrap(n, 76))
	}
	return b.String()
}

// Question is the one-line consent question. Separate from Text so a UI can ask
// it its own way while the words stay ours.
func (s Sheet) Question() string {
	if s.Upgrade != nil {
		return fmt.Sprintf("Upgrade %s to %s with these permissions?", s.Name, s.Version)
	}
	return fmt.Sprintf("Install %s with these permissions?", s.Name)
}

// ErrDeclined is returned when consent was refused. It is not a failure: it is
// the flow working.
var ErrDeclined = errors.New("appstore: consent declined; nothing was installed")

// Consenter shows the sheet and waits for an answer.
//
// It is an interface because there are two consent surfaces — the terminal and
// the phone — and neither of them is allowed to be the place the sheet is
// composed. Implementations display [Sheet] and answer; they do not decide what
// it says.
type Consenter interface {
	Review(ctx context.Context, s Sheet) (bool, error)
}

// ConsentFunc adapts a function.
type ConsentFunc func(ctx context.Context, s Sheet) (bool, error)

func (f ConsentFunc) Review(ctx context.Context, s Sheet) (bool, error) { return f(ctx, s) }

// DenyAll is the consenter for an unattended run.
//
// `relay setup --yes` takes every default; there is no default for "may this
// third-party code read your transcripts", so an unattended install refuses
// rather than answering on the user's behalf.
var DenyAll Consenter = ConsentFunc(func(context.Context, Sheet) (bool, error) {
	return false, errors.New("appstore: installing an app needs a person to read the permission sheet; " +
		"there is no unattended answer to it")
})

// Wrap breaks a paragraph at width, preserving blank lines. Exported because
// the CLI wraps the same sentences this package composes, and a second
// implementation would wrap them differently.
func Wrap(s string, width int) string {
	var out []string
	for _, para := range strings.Split(s, "\n") {
		if strings.TrimSpace(para) == "" {
			out = append(out, "")
			continue
		}
		line := ""
		for _, word := range strings.Fields(para) {
			switch {
			case line == "":
				line = word
			case len([]rune(line))+1+len([]rune(word)) <= width:
				line += " " + word
			default:
				out = append(out, line)
				line = word
			}
		}
		if line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
