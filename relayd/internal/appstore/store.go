package appstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/index"
)

// What is installed, on disk.
//
// A directory per app under <data dir>/apps, not a table: SYSTEM.md §8's
// storage argument is "one file, backup is a file copy", and an app's record is
// a manifest snapshot plus a log. Keeping it out of relay.db also means an app
// the user removes leaves nothing behind in the database that holds their
// memory, which is the property §5 is trying to have.
//
//	<data>/apps/dev.alexis.standup-notes/installed.json
//	<data>/apps/dev.alexis.standup-notes/log.jsonl

// State is what actually happened, not what was intended.
type State string

const (
	// StateProvisioned means a container exists and the app can be woken.
	StateProvisioned State = "provisioned"
	// StateAwaitingRuntime means consent was given and recorded, and no
	// container was created because this box has no app runtime
	// (APP-PLATFORM.md §8 step 2, unbuilt). The app will not run.
	//
	// It is a state and not an error because the grant is real and worth
	// keeping: re-provisioning later must not re-ask a question already
	// answered. It is never described as installed-and-running anywhere.
	StateAwaitingRuntime State = "awaiting-runtime"
	// StateFailed means provisioning was attempted and did not work.
	StateFailed State = "failed"
)

// Installed is the record written at install time.
type Installed struct {
	Manifest Manifest `json:"manifest"`
	// Grants is what the user actually agreed to, with the sentences they were
	// shown. Stored separately from the manifest because the manifest is the
	// app's claim and this is the user's decision — and after an upgrade that
	// was declined, the two differ.
	Grants []Permission `json:"grants"`
	// Registry is the spec it was resolved from.
	Registry string      `json:"registry"`
	Origin   EntrySource `json:"origin"`
	Review   string      `json:"review,omitempty"`

	InstalledAt time.Time `json:"installed_at"`
	ConsentedAt time.Time `json:"consented_at"`

	State State `json:"state"`
	// StateReason is a sentence a person can act on. Required whenever State is
	// not StateProvisioned.
	StateReason string `json:"state_reason,omitempty"`
	// ContainerID is set only by a provisioner that made one.
	ContainerID string `json:"container_id,omitempty"`
}

// ID is the app id.
func (i Installed) ID() string { return i.Manifest.ID }

// ShortName is what the user types.
func (i Installed) ShortName() string { return i.Manifest.ShortName() }

// Running reports whether anything can actually invoke this app.
func (i Installed) Running() bool { return i.State == StateProvisioned }

// Store is the on-disk record of installed apps.
type Store struct {
	root string
	det  *index.Detector
	now  func() time.Time
}

// OpenStore opens (and creates) the apps directory.
func OpenStore(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("appstore: no apps directory")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	det, err := index.NewDetector()
	if err != nil {
		return nil, err
	}
	return &Store{root: root, det: det, now: time.Now}, nil
}

// Root is the directory the store lives in.
func (s *Store) Root() string { return s.root }

// SetClock is for tests.
func (s *Store) SetClock(f func() time.Time) { s.now = f }

func (s *Store) dir(id string) string { return filepath.Join(s.root, id) }

// Put writes an install record, atomically.
func (s *Store) Put(rec Installed) error {
	if !idPattern.MatchString(rec.Manifest.ID) {
		return fmt.Errorf("appstore: refusing to write a record for %q", rec.Manifest.ID)
	}
	if rec.State != StateProvisioned && rec.StateReason == "" {
		// A state that is not "running" and does not say why is exactly the
		// silent degradation this codebase refuses everywhere else.
		return fmt.Errorf("appstore: %s is %s with no reason recorded", rec.Manifest.ID, rec.State)
	}
	dir := s.dir(rec.Manifest.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, "installed.json"), append(b, '\n'), 0o600)
}

// ErrNotInstalled is returned by Get and Remove.
var ErrNotInstalled = errors.New("appstore: not installed")

// Get finds an app by full id or by short name.
func (s *Store) Get(name string) (Installed, error) {
	all, err := s.List()
	if err != nil {
		return Installed{}, err
	}
	var hits []Installed
	for _, a := range all {
		if a.ID() == name || a.ShortName() == name {
			hits = append(hits, a)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return Installed{}, fmt.Errorf("appstore: %s is %w", name, ErrNotInstalled)
	default:
		ids := make([]string, len(hits))
		for i, h := range hits {
			ids[i] = h.ID()
		}
		sort.Strings(ids)
		return Installed{}, fmt.Errorf("appstore: %q matches %s — use the full id",
			name, strings.Join(ids, " and "))
	}
}

// List reads every install record, sorted by id.
//
// A record that will not parse is reported as an error rather than skipped: an
// app the user granted permissions to and this cannot read is not an app that
// should quietly vanish from `relay list`.
func (s *Store) List() ([]Installed, error) {
	ents, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Installed
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.root, e.Name(), "installed.json"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var rec Installed
		if err := json.Unmarshal(b, &rec); err != nil {
			return nil, fmt.Errorf("appstore: %s/installed.json is unreadable: %w", e.Name(), err)
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out, nil
}

// Remove deletes an app's record and its log, and returns what was removed.
func (s *Store) Remove(name string) (Installed, error) {
	rec, err := s.Get(name)
	if err != nil {
		return Installed{}, err
	}
	if err := os.RemoveAll(s.dir(rec.ID())); err != nil {
		return Installed{}, err
	}
	return rec, nil
}

// ---------------------------------------------------------------- log

// EventKind is what happened. Only kinds this package can actually observe are
// defined here; the app runtime will append its own through [Store.Append] when
// it exists, and until then nothing invents an invocation that never happened.
type EventKind string

const (
	EventInstalled EventKind = "installed"
	EventUpgraded  EventKind = "upgraded"
	EventDeclined  EventKind = "declined"
	EventRemoved   EventKind = "removed"
	// EventProvisionDeferred records that consent was given and no container
	// was created.
	EventProvisionDeferred EventKind = "provision.deferred"
	EventProvisionFailed   EventKind = "provision.failed"
	EventProvisioned       EventKind = "provisioned"
)

// Event is one line of an app's log.
type Event struct {
	At      time.Time `json:"at"`
	Kind    EventKind `json:"kind"`
	Message string    `json:"message"`
	// Redacted names the secret rules that fired on this line before it was
	// written. Empty for everything this package writes; the field exists
	// because the app runtime will write app-produced text here.
	Redacted []string `json:"redacted,omitempty"`
}

// Append writes one event.
//
// **Every line goes through the secret detector before it is written**, not
// after. Nothing in this package produces a credential, but an app's stdout is
// untrusted text that will land here the moment the runtime exists, and a key
// written to a log file is as unrecoverable as a key written to an index. The
// ordering is enforced by this function being the only writer.
func (s *Store) Append(id string, kind EventKind, format string, args ...any) error {
	dir := s.dir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	msg := fmt.Sprintf(format, args...)
	clean, findings := s.det.Redact(msg)
	ev := Event{At: s.now().UTC(), Kind: kind, Message: clean}
	for _, f := range findings {
		ev.Redacted = append(ev.Redacted, f.Label)
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "log.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// Log reads the last n events for an app, oldest first. n <= 0 means all.
func (s *Store) Log(id string, n int) ([]Event, error) {
	b, err := os.ReadFile(filepath.Join(s.dir(id), "log.jsonl"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return nil, fmt.Errorf("appstore: %s log line is unreadable: %w", id, err)
		}
		out = append(out, ev)
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}

// writeFileAtomic writes through a temporary file and renames, so a record is
// never half-written — a truncated installed.json is an app with permissions
// nobody can read back.
func writeFileAtomic(path string, b []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
