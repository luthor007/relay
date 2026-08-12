// Package appruntime joins the app store to the app runtime.
//
// `internal/appstore` records what the user consented to. `internal/apps` runs
// the code. Neither imports the other, deliberately — the store has to work on a
// box with no runtime, and the runtime has to be testable without an install
// flow — so `appstore.Provisioner` is the seam between them, and it has had **no
// implementation** since both were written. `relay install` therefore recorded
// consent and printed `appstore.NoRuntimeNote`: your permissions are saved and
// nothing was started.
//
// This is that implementation, and it is the only thing standing between an
// installed app and a running one.
//
// # What provisioning actually is here
//
// `APP-PLATFORM.md` §5 calls it a container, and on this box it is three
// directories and a sandboxed Node process — `apps.Runtime` already builds that,
// with the namespace isolation, the egress guard and the resource caps. So
// provisioning is: fetch the package, stage it, and register it with the
// dispatcher so a phrase or a schedule can wake it.
//
// The registry entry names a **git repository**, not a tarball
// (`appstore.EntrySource`), so fetching is a clone. That is a real dependency on
// `git` being present, which is safe to assume on a box whose entire purpose is
// running coding agents, and it is behind [Fetcher] anyway — the tests use a
// directory, and so does anyone developing an app locally.
//
// # The rule that survives from the store
//
// `appstore.Provisioner`'s contract: *"Provision is called only after consent,
// and only with the scopes on the record. A provisioner that mints a capability
// the record does not carry has broken the only guarantee the sheet makes."*
//
// So the consent handed to `apps.Install` is built from `rec.Grants` — the
// user's decision — and never from the manifest, which is the app's request. The
// two differ exactly when somebody declined something, which is the case that
// matters.
package appruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/apps"
	"github.com/luthor007/relay/relayd/internal/appstore"
)

// PackagesDir is where staged app packages live.
//
// Deliberately **not** `appstore.StoreRoot`, which is `<data>/apps` and holds
// the install records — one subdirectory per app id, containing the consent and
// the state. Staging packages into the same tree would put a third party's files
// inside the directory that records what the user agreed to, and the first
// collision would be an app id that is also a record filename.
//
// It is a function here, and the daemon and the CLI both call it, because the
// one thing worse than choosing the wrong directory is choosing two.
func PackagesDir(dataDir string) string { return filepath.Join(dataDir, "app-packages") }

// Fetcher puts an app's package on disk.
//
// An interface because the production path clones a repository and every test
// path copies a directory — and because an app author working locally should not
// have to push to git to try their own app.
type Fetcher interface {
	// Fetch places the package at dest, which does not exist yet. dest must
	// contain relay.json at its top level when this returns.
	Fetch(ctx context.Context, src appstore.EntrySource, dest string) error
	// Describe names the mechanism, for the install record.
	Describe() string
}

// GitFetcher clones the entry's repository.
type GitFetcher struct {
	// Git is the binary. Empty means "git" on PATH.
	Git string
	// Timeout bounds one clone.
	Timeout time.Duration
}

// DefaultCloneTimeout bounds a clone.
const DefaultCloneTimeout = 2 * time.Minute

func (g GitFetcher) Describe() string { return "git" }

func (g GitFetcher) Fetch(ctx context.Context, src appstore.EntrySource, dest string) error {
	if strings.TrimSpace(src.Git) == "" {
		return errors.New("appruntime: the registry entry has no repository to clone")
	}
	bin := g.Git
	if bin == "" {
		bin = "git"
	}
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = DefaultCloneTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Cloned into a staging directory and then moved, so a clone interrupted
	// half way never leaves something that looks like an installed app. The
	// runtime reads whatever is at the destination and would happily run a
	// truncated package.
	staging := dest + ".fetching"
	_ = os.RemoveAll(staging)
	defer os.RemoveAll(staging)

	// Depth 1 at a named revision. A branch name is accepted by the registry and
	// is worse — appstore.EntrySource says so — but that is the registry's
	// argument to have, not this function's.
	args := []string{"clone", "--depth", "1", "--quiet"}
	if src.Rev != "" {
		args = append(args, "--branch", src.Rev)
	}
	args = append(args, src.Git, staging)

	cmd := exec.CommandContext(ctx, bin, args...)
	// A clone must not be able to ask for credentials: on a headless box a
	// prompt is a hang, and a private repository in a registry is a
	// misconfiguration to report rather than a password to collect.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("appruntime: could not clone %s: %v: %s", src.Git, err, strings.TrimSpace(string(out)))
	}

	root := staging
	if src.Subdir != "" {
		root = filepath.Join(staging, filepath.Clean("/"+src.Subdir))
	}
	if _, err := os.Stat(filepath.Join(root, "relay.json")); err != nil {
		return fmt.Errorf("appruntime: %s has no relay.json where the registry said it would", src.Git)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	return os.Rename(root, dest)
}

// DirFetcher copies from a directory on this machine.
//
// For local development and for tests. It reads [appstore.EntrySource.Git] as a
// path, which is a small abuse of the field and the alternative — a second
// source shape in the registry — is a larger one.
type DirFetcher struct{}

func (DirFetcher) Describe() string { return "a local directory" }

func (DirFetcher) Fetch(_ context.Context, src appstore.EntrySource, dest string) error {
	from := src.Git
	if src.Subdir != "" {
		from = filepath.Join(from, filepath.Clean("/"+src.Subdir))
	}
	if _, err := os.Stat(filepath.Join(from, "relay.json")); err != nil {
		return fmt.Errorf("appruntime: %s has no relay.json", from)
	}
	return copyTree(from, dest)
}

// Options configures a [Provisioner].
type Options struct {
	// Runtime runs the apps. Required.
	Runtime *apps.Runtime
	// Dispatcher is what wakes them. Required: a provisioned app nothing can
	// trigger is the same as an unprovisioned one, and this package exists to
	// stop that being the outcome.
	Dispatcher *apps.Dispatcher
	// Dir is where staged packages live, one subdirectory per app.
	Dir string
	// Fetch places a package on disk. Nil uses [GitFetcher].
	Fetch Fetcher
	Now   func() time.Time
}

// Provisioner implements [appstore.Provisioner].
type Provisioner struct {
	opts Options
}

var _ appstore.Provisioner = (*Provisioner)(nil)

// New builds a provisioner, or refuses.
func New(o Options) (*Provisioner, error) {
	if o.Runtime == nil {
		return nil, errors.New("appruntime: no runtime")
	}
	if o.Dispatcher == nil {
		return nil, errors.New("appruntime: no dispatcher, so a provisioned app could never be triggered")
	}
	if strings.TrimSpace(o.Dir) == "" {
		return nil, errors.New("appruntime: no directory to stage apps in")
	}
	if o.Fetch == nil {
		o.Fetch = GitFetcher{}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Provisioner{opts: o}, nil
}

// Dir is where staged packages live.
func (p *Provisioner) Dir() string { return p.opts.Dir }

// Describe names the runtime, for the install record and `relay list`.
func (p *Provisioner) Describe() string {
	return "the sandboxed Node runtime (" + p.opts.Runtime.SandboxName() + ")"
}

// Provision stages an app and registers it.
func (p *Provisioner) Provision(ctx context.Context, rec appstore.Installed) (appstore.Provisioned, error) {
	var out appstore.Provisioned
	id := rec.Manifest.ID
	if id == "" {
		return out, errors.New("appruntime: the record has no app id")
	}
	dest := filepath.Join(p.opts.Dir, id)

	// A reinstall or an upgrade replaces the package. Removing first rather than
	// merging: a stale file from a previous version that the new one no longer
	// ships is a file the sandbox would still let the app read.
	if err := os.RemoveAll(dest); err != nil {
		return out, err
	}
	if err := p.opts.Fetch.Fetch(ctx, rec.Origin, dest); err != nil {
		return out, err
	}

	// The manifest is re-read from what was fetched rather than taken from the
	// record. The record's copy came from the registry, and the registry is not
	// the thing that will be executed — if the two disagree, the file on disk is
	// what the sandbox will run, so it is the one that has to be validated.
	m, err := apps.ReadManifest(dest)
	if err != nil {
		return out, fmt.Errorf("appruntime: %s does not carry a usable manifest: %w", id, err)
	}
	if m.ID != id {
		// A package that claims a different id than the registry entry would be
		// installed under one name and run as another.
		return out, fmt.Errorf("appruntime: the registry called this %s and the package calls itself %s", id, m.ID)
	}

	inst, err := apps.Install(m, consentFrom(rec), dest, apps.InstallOptions{
		Dir: p.opts.Dir,
		Now: p.opts.Now,
	})
	if err != nil {
		return out, err
	}
	if err := p.opts.Dispatcher.Add(inst); err != nil {
		return out, err
	}

	return appstore.Provisioned{
		ContainerID: id,
		Detail: fmt.Sprintf("%d capabilit%s, %s, fetched with %s",
			len(inst.Granted), plural(len(inst.Granted)),
			p.opts.Runtime.SandboxName(), p.opts.Fetch.Describe()),
	}, nil
}

// Deprovision removes an app and everything it wrote.
func (p *Provisioner) Deprovision(_ context.Context, rec appstore.Installed) error {
	id := rec.Manifest.ID
	p.opts.Dispatcher.Remove(id)
	// The whole layout: root, scratch and `ctx.storage`'s backing file. §8's
	// deprovision "destroys the container and the app's scratch storage", and an
	// app's private storage surviving an uninstall is data the user believes
	// they deleted.
	return os.RemoveAll(filepath.Join(p.opts.Dir, id))
}

// Load re-registers everything already installed, at daemon start.
//
// Without this the dispatcher is empty on every restart and an app that was
// installed yesterday silently stops being triggerable — which looks exactly
// like the app being broken, and is the failure that made having no provisioner
// invisible in the first place.
//
// A record that cannot be attached is reported and skipped, never fatal: one
// app with a missing directory must not stop the other four from working, and a
// daemon that refuses to start because of a third-party package is a daemon
// third-party packages can take down.
func (p *Provisioner) Load(store *appstore.Store) []error {
	if store == nil {
		return nil
	}
	records, err := store.List()
	if err != nil {
		return []error{err}
	}
	var problems []error
	for _, rec := range records {
		if !rec.Running() {
			// Installed but never provisioned — the state every app was in
			// before this package existed. Left alone rather than started: the
			// user consented to an install that reported it had not run.
			continue
		}
		id := rec.Manifest.ID
		dest := filepath.Join(p.opts.Dir, id)
		m, err := apps.ReadManifest(dest)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", id, err))
			continue
		}
		inst, err := apps.Attach(m, consentFrom(rec), apps.Layout{
			Root:    dest,
			Scratch: filepath.Join(p.opts.Dir, id+".scratch"),
			Data:    filepath.Join(p.opts.Dir, id+".data"),
		}, filepath.Join(dest, apps.DefaultEntry))
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", id, err))
			continue
		}
		if err := p.opts.Dispatcher.Add(inst); err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", id, err))
		}
	}
	return problems
}

// consentFrom is the user's decision, not the app's request.
//
// `appstore.Provisioner`'s contract in one function: the scopes come from
// rec.Grants, which is what the sheet showed and the user accepted. Reading them
// from rec.Manifest.Permissions instead would mint whatever the app asked for,
// which is the single way a provisioner can break the only guarantee the install
// sheet makes.
func consentFrom(rec appstore.Installed) apps.Consent {
	granted := make([]apps.Scope, 0, len(rec.Grants))
	for _, g := range rec.Grants {
		granted = append(granted, apps.Scope(g.Scope))
	}
	return apps.Consent{Granted: granted, At: rec.ConsentedAt, By: "install"}
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// copyTree copies a directory, refusing symlinks.
//
// A symlink in a fetched package is a path out of the app's root, and the
// sandbox's read allowance is expressed in paths. Refusing is the only answer
// that does not require reasoning about where each link points.
func copyTree(from, to string) error {
	return filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("appruntime: %s is a symlink, which an app package may not contain", rel)
		case info.IsDir():
			return os.MkdirAll(target, 0o700)
		case !info.Mode().IsRegular():
			return fmt.Errorf("appruntime: %s is not a regular file", rel)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
}
