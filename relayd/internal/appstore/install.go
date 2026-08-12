package appstore

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

// The install flow, APP-PLATFORM.md §6:
//
//	resolve the package → show the permission sheet → wait for consent → provision
//
// Four steps, in that order, and the order is the security property. Nothing
// here fetches app code, mints a capability or starts a process before
// [Consenter.Review] has returned true, and the only writer of an install
// record is [Installer.Install] after it has.

// StoreRoot is where installed apps live under a data directory.
func StoreRoot(dataDir string) string { return filepath.Join(dataDir, "apps") }

// Provisioned is what a provisioner made.
type Provisioned struct {
	ContainerID string
	// Detail is one line about what exists now — the image, the scratch
	// directory, the egress proxy rule set. Shown after install.
	Detail string
}

// Provisioner creates the per-app container of APP-PLATFORM.md §5.
//
// It is an interface with no implementation in this package on purpose: the
// runtime is §8 step 2 and is not built. Making it a seam rather than a stub
// means the CLI can be finished, tested and shipped without anything in it
// pretending a container exists.
//
// The contract: Provision is called only after consent, and only with the
// scopes on the record. A provisioner that mints a capability the record does
// not carry has broken the only guarantee the sheet makes.
type Provisioner interface {
	// Describe names the runtime, for the log and for `relay list`.
	Describe() string
	Provision(ctx context.Context, app Installed) (Provisioned, error)
	// Deprovision destroys the container and the app's scratch storage.
	Deprovision(ctx context.Context, app Installed) error
}

// NoRuntimeNote is the sentence shown wherever an app has been granted its
// permissions and cannot run. It is one constant so the CLI, the API and the
// install record cannot describe the same condition three ways.
const NoRuntimeNote = "No app runtime is attached on this box (APP-PLATFORM.md §5), " +
	"so no container was created and this app will not run. Your consent is recorded: " +
	"provisioning it later will not ask again unless the app asks for more."

// Installer runs the flow. Every collaborator is an interface or a directory,
// so the whole thing runs in a test with no network and no container runtime.
type Installer struct {
	Registry *Registry
	Store    *Store
	Consent  Consenter
	// Provisioner is nil on a box with no app runtime, which is every box
	// today. Nil is a supported, reported state — not a panic and not a lie.
	Provisioner Provisioner
	Now         func() time.Time
}

func (in *Installer) now() time.Time {
	if in.Now != nil {
		return in.Now().UTC()
	}
	return time.Now().UTC()
}

// Result is what an install did.
type Result struct {
	Sheet     Sheet
	Installed Installed
	Upgraded  bool
	// AlreadyInstalled is set when the same version was already there with the
	// same grants, and nothing was asked or written.
	AlreadyInstalled bool
	// Note is what did not happen. Empty when the app is provisioned and
	// running; never empty when it is not.
	Note string
}

// Install resolves an app, asks, and records the answer.
func (in *Installer) Install(ctx context.Context, id string) (Result, error) {
	if in.Registry == nil || in.Store == nil {
		return Result{}, errors.New("appstore: installer needs a registry and a store")
	}
	consent := in.Consent
	if consent == nil {
		consent = DenyAll
	}

	entry, err := in.Registry.Resolve(ctx, id)
	if err != nil {
		return Result{}, err
	}

	var prev *Installed
	if existing, err := in.Store.Get(entry.Manifest.ID); err == nil {
		prev = &existing
	} else if !errors.Is(err, ErrNotInstalled) {
		return Result{}, err
	}

	sheet := NewSheet(entry, in.Registry.Describe(), prev)

	// Same version, same grants: there is nothing to decide and nothing to
	// write. Re-asking a question whose answer is already on disk trains people
	// to click through permission sheets, which is the failure mode that makes
	// every sheet after it worthless.
	if prev != nil && prev.Manifest.Version == entry.Manifest.Version &&
		sheet.Upgrade != nil && !sheet.Upgrade.NeedsConsent() && len(sheet.Upgrade.Dropped) == 0 {
		return Result{
			Sheet: sheet, Installed: *prev, AlreadyInstalled: true,
			Note: noteFor(*prev),
		}, nil
	}

	ok, err := consent.Review(ctx, sheet)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		// A refusal is recorded only where the app is already known. Writing a
		// directory for an app the user just declined would leave a trace of a
		// decision to say no, which is not a thing to keep.
		if prev != nil {
			_ = in.Store.Append(entry.Manifest.ID, EventDeclined,
				"declined %s %s", entry.Manifest.Name, entry.Manifest.Version)
		}
		return Result{Sheet: sheet}, ErrDeclined
	}

	now := in.now()
	rec := Installed{
		Manifest:    entry.Manifest,
		Grants:      sheet.Grants(),
		Registry:    in.Registry.Describe(),
		Origin:      entry.Source,
		Review:      entry.Review,
		InstalledAt: now,
		ConsentedAt: now,
	}
	if prev != nil {
		// The install date is when this app first arrived; the consent date is
		// when these words were agreed to. They are different facts.
		rec.InstalledAt = prev.InstalledAt
	}

	res := Result{Sheet: sheet, Upgraded: prev != nil}
	switch {
	case in.Provisioner == nil:
		rec.State = StateAwaitingRuntime
		rec.StateReason = NoRuntimeNote
	default:
		p, err := in.Provisioner.Provision(ctx, rec)
		if err != nil {
			rec.State = StateFailed
			rec.StateReason = fmt.Sprintf("%s could not provision it: %v", in.Provisioner.Describe(), err)
		} else {
			rec.State = StateProvisioned
			rec.ContainerID = p.ContainerID
		}
	}

	if err := in.Store.Put(rec); err != nil {
		return Result{}, err
	}
	kind := EventInstalled
	verb := "installed"
	if prev != nil {
		kind, verb = EventUpgraded, "upgraded to"
	}
	_ = in.Store.Append(rec.ID(), kind, "%s %s %s from %s (%s)",
		verb, rec.Manifest.Name, rec.Manifest.Version, rec.Registry, joinScopes(rec.Manifest.ScopeList()))
	switch rec.State {
	case StateAwaitingRuntime:
		_ = in.Store.Append(rec.ID(), EventProvisionDeferred, "%s", NoRuntimeNote)
	case StateFailed:
		_ = in.Store.Append(rec.ID(), EventProvisionFailed, "%s", rec.StateReason)
	case StateProvisioned:
		_ = in.Store.Append(rec.ID(), EventProvisioned, "container %s created by %s",
			rec.ContainerID, in.Provisioner.Describe())
	}

	res.Installed = rec
	res.Note = noteFor(rec)
	return res, nil
}

// noteFor is the one place that decides what to say about an app that is not
// running. An empty note means the app is genuinely provisioned.
func noteFor(rec Installed) string {
	switch rec.State {
	case StateProvisioned:
		return ""
	case StateAwaitingRuntime:
		return NoRuntimeNote
	default:
		return rec.StateReason
	}
}

// Remove uninstalls an app.
//
// The container is destroyed first, then the record — the other order leaves a
// running container with nothing on disk that names it.
func (in *Installer) Remove(ctx context.Context, name string) (Installed, error) {
	rec, err := in.Store.Get(name)
	if err != nil {
		return Installed{}, err
	}
	if rec.State == StateProvisioned {
		if in.Provisioner == nil {
			// A record says a container exists and nothing here can destroy it.
			// Refusing is the honest answer: removing the record would leave the
			// container running and unlisted, which is worse than not removing.
			return Installed{}, fmt.Errorf("appstore: %s has a container (%s) and this box has no "+
				"app runtime to destroy it. Removing the record would leave it running and "+
				"unlisted", rec.ID(), rec.ContainerID)
		}
		if err := in.Provisioner.Deprovision(ctx, rec); err != nil {
			return Installed{}, fmt.Errorf("appstore: %s: %w", rec.ID(), err)
		}
	}
	if _, err := in.Store.Remove(rec.ID()); err != nil {
		return Installed{}, err
	}
	return rec, nil
}
