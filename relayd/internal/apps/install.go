package apps

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Install is APP-PLATFORM.md §6's middle step: "shows the permission sheet with
// each `reason`, waits for consent, then provisions the container."
//
// Resolving the package is the CLI's job and is not here. What is here is the
// part that must not be reimplemented per front end: the sheet, the shape of a
// consent, the refusal to grant anything the sheet did not show, and the
// directory layout that makes the read-only root and the writable scratch
// separate things rather than a comment.

// ErrNotRequested is a consent naming a scope the manifest never asked for.
//
// It is an error rather than a silent trim because the sheet is the only thing
// that was reviewed: a front end that can grant a scope which never appeared on
// it has a bug, and swallowing that turns the bug into a permission.
var ErrNotRequested = errors.New("apps: consent names a scope the manifest did not request")

// Consent is the user's answer to the install sheet.
type Consent struct {
	// Granted is what the user accepted. It may be narrower than the manifest
	// requested — APP-PLATFORM.md's `AppContext.granted` is explicit that an app
	// must cope with having been declined.
	Granted []Scope
	At      time.Time
	// By is who consented, for the record: "console", "phone", "cli".
	By string
}

// Layout is where one app's three directories live.
//
// They are three and not one on purpose. §5: "a read-only root, its own writable
// scratch, and no access to the agent's workspace or other apps' data."
type Layout struct {
	// Root is the app package. Made read-only at install, and the only path the
	// sandbox lets the app read.
	Root string
	// Scratch is the app's writable working area. Emptied between invocations by
	// [Runtime] when Options.CleanScratch is set — a scratch that survives is a
	// place to accumulate state the user cannot see.
	Scratch string
	// Data is the backing store for `ctx.storage`. It is *not* visible to the
	// app process: storage is a capability the host serves, so the file lives on
	// relayd's side of the boundary and an app cannot read another app's by
	// walking up a directory.
	Data string
}

// resolved returns the layout with every path canonicalised.
//
// The sandbox is expressed in paths, so the paths have to be the ones the
// operating system will actually compare against. See [Runtime.Invoke], which
// is the one caller and does it before anything else reads a path.
func (l Layout) resolved() Layout {
	return Layout{
		Root:    resolveLinks(l.Root),
		Scratch: resolveLinks(l.Scratch),
		Data:    resolveLinks(l.Data),
	}
}

// Installed is an app that has been installed and consented to.
type Installed struct {
	Manifest Manifest
	// Granted is the intersection of what was requested and what was consented,
	// sorted. It is the only thing capability minting reads.
	Granted []Scope
	Consent Consent
	Layout  Layout
	// Entry is the app's entry point, absolute, inside Root.
	Entry string
}

// Has reports whether a scope was granted.
func (i Installed) Has(s Scope) bool {
	for _, g := range i.Granted {
		if g == s {
			return true
		}
	}
	return false
}

// Declined is what the manifest asked for and the user did not give. Apps are
// told, because "the camera is not available" and "you have no camera" are
// different sentences for an app to say.
func (i Installed) Declined() []Scope {
	var out []Scope
	for _, s := range i.Manifest.Scopes() {
		if !i.Has(s) {
			out = append(out, s)
		}
	}
	return out
}

// AllowedHosts is the egress allowlist actually in force.
//
// Empty unless `net.fetch` was granted: a manifest may declare hosts and have
// the scope declined, and a host list without the scope authorises nothing. The
// allowlist is read from here and never from the manifest directly, so there is
// one place where "declared" becomes "granted".
func (i Installed) AllowedHosts() []string {
	if !i.Has(ScopeNetFetch) {
		return nil
	}
	return append([]string(nil), i.Manifest.AllowedHosts...)
}

// Timeout is the wall-clock ceiling for one invocation of this app.
func (i Installed) Timeout(max time.Duration) time.Duration {
	d := time.Duration(i.Manifest.TimeoutMs) * time.Millisecond
	if d <= 0 {
		d = DefaultTimeoutMs * time.Millisecond
	}
	if max > 0 && d > max {
		return max
	}
	return d
}

// Slug is the app id with everything a strict MCP client rejects replaced.
// internal/mcp's ToolName note: the strictest of the five runtime validators
// accepts only [A-Za-z0-9_-], and an app id is reverse-DNS.
func (i Installed) Slug() string { return Slug(i.Manifest.ID) }

// Slug renders an app id as a wire-safe identifier.
func Slug(id string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(id) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// InstallOptions configures [Install].
type InstallOptions struct {
	// Dir is the parent directory apps are installed under. One subdirectory per
	// app id.
	Dir string
	// Entry is the app's entry point relative to its package root. Defaults to
	// "src/index.ts", which is APP-PLATFORM.md §2's layout.
	Entry string
	Now   func() time.Time
}

// DefaultEntry is the entry point APP-PLATFORM.md §2 shows.
const DefaultEntry = "src/index.ts"

// Install provisions an app's directories and records the grant.
//
// source is the app package as delivered — a directory containing `relay.json`
// and the app's source. It is copied rather than referenced, because an
// installed app whose code can be changed underneath it by whatever wrote the
// source directory is an installed app whose review means nothing.
func Install(m Manifest, c Consent, source string, o InstallOptions) (Installed, error) {
	var out Installed
	if err := m.Validate(); err != nil {
		return out, err
	}
	if o.Dir == "" {
		return out, errors.New("apps: install needs a directory to install into")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Entry == "" {
		o.Entry = DefaultEntry
	}

	granted, err := grant(m, c)
	if err != nil {
		return out, err
	}

	base := filepath.Join(o.Dir, Slug(m.ID))
	lay := Layout{
		Root:    filepath.Join(base, "root"),
		Scratch: filepath.Join(base, "scratch"),
		Data:    filepath.Join(base, "data"),
	}
	// Thaw before clearing. A previous install froze this tree to 0400/0500,
	// and RemoveAll on a frozen tree fails for anyone who is not root — so
	// reinstalling or upgrading an app was broken for every real user while
	// passing in a container that runs as uid 0.
	thawTree(lay.Root)
	if err := os.RemoveAll(lay.Root); err != nil {
		return out, fmt.Errorf("apps: clear app root: %w", err)
	}
	for _, d := range []string{lay.Scratch, lay.Data} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return out, fmt.Errorf("apps: create %s: %w", d, err)
		}
	}
	if err := copyTree(source, lay.Root); err != nil {
		return out, fmt.Errorf("apps: copy app package: %w", err)
	}
	entry := filepath.Join(lay.Root, filepath.FromSlash(o.Entry))
	if _, err := os.Stat(entry); err != nil {
		return out, fmt.Errorf("apps: entry point %s: %w", o.Entry, err)
	}
	// Read-only, last, so the copy above could write. This is defence in depth
	// rather than the boundary — the boundary is the sandbox, which does not
	// give the app a writable handle on this tree at all — but a mode bit costs
	// nothing and catches the case where the sandbox degraded.
	if err := freezeTree(lay.Root); err != nil {
		return out, fmt.Errorf("apps: make app root read-only: %w", err)
	}

	c.At = nonZeroTime(c.At, o.Now())
	return Installed{
		Manifest: m,
		Granted:  granted,
		Consent:  c,
		Layout:   lay,
		Entry:    entry,
	}, nil
}

// Attach builds an [Installed] for an app whose directories already exist. It is
// the read path — a daemon starting up against apps installed on a previous run
// — and it deliberately does not touch the filesystem beyond checking the entry
// point is there, so a missing app fails at attach rather than at trigger time.
func Attach(m Manifest, c Consent, lay Layout, entry string) (Installed, error) {
	if err := m.Validate(); err != nil {
		return Installed{}, err
	}
	granted, err := grant(m, c)
	if err != nil {
		return Installed{}, err
	}
	if !filepath.IsAbs(entry) {
		entry = filepath.Join(lay.Root, filepath.FromSlash(entry))
	}
	if _, err := os.Stat(entry); err != nil {
		return Installed{}, fmt.Errorf("apps: entry point: %w", err)
	}
	return Installed{Manifest: m, Granted: granted, Consent: c, Layout: lay, Entry: entry}, nil
}

func grant(m Manifest, c Consent) ([]Scope, error) {
	requested := map[Scope]bool{}
	for _, s := range m.Scopes() {
		requested[s] = true
	}
	seen := map[Scope]bool{}
	var out []Scope
	for _, s := range c.Granted {
		if !requested[s] {
			return nil, fmt.Errorf("%w: %s", ErrNotRequested, s)
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", src)
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case !d.Type().IsRegular():
			// A symlink in an app package is a way to reach outside the root
			// after the copy. Refusing it is the only answer that stays true
			// once the tree is read-only.
			return fmt.Errorf("apps: %s is not a regular file; app packages are files and directories only", rel)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o600)
	})
}

func freezeTree(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		return os.Chmod(path, 0o400)
	})
	if err != nil {
		return err
	}
	// Directories last and deepest-first: chmod 0500 on a parent before its
	// children have been walked is a walk that cannot continue.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i], 0o500); err != nil {
			return err
		}
	}
	return nil
}

// thawTree undoes [freezeTree], because a tree frozen at 0400/0500 does not
// delete. Best-effort by design: it is only ever a prelude to RemoveAll, and a
// path that cannot be chmod'd will surface as the RemoveAll error rather than
// as a second, less useful one here.
//
// Root does not need this — uid 0 ignores the permission bits entirely — which
// is exactly why its absence went unnoticed: the tests were written in a
// container running as root and every one of them passed there.
func thawTree(root string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			_ = os.Chmod(path, 0o700)
		} else {
			_ = os.Chmod(path, 0o600)
		}
		return nil
	})
}

// Uninstall removes an app's directories. It restores write permission first,
// because [freezeTree] took it away and a read-only tree does not delete.
func Uninstall(lay Layout) error {
	thawTree(lay.Root)
	return os.RemoveAll(filepath.Dir(lay.Root))
}

func nonZeroTime(t, fallback time.Time) time.Time {
	if t.IsZero() {
		return fallback
	}
	return t
}
