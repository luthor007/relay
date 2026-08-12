package appstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/appstore"
)

func newStore(t *testing.T) *appstore.Store {
	t.Helper()
	st, err := appstore.OpenStore(appstore.StoreRoot(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1780000000, 0).UTC()
	st.SetClock(func() time.Time { at = at.Add(time.Second); return at })
	return st
}

// recorder is a Consenter that answers a fixed way and keeps the sheets it was
// shown, so a test can assert both the answer and what was in front of it.
type recorder struct {
	answer bool
	err    error
	sheets []appstore.Sheet
}

func (r *recorder) Review(_ context.Context, s appstore.Sheet) (bool, error) {
	r.sheets = append(r.sheets, s)
	return r.answer, r.err
}

func installer(t *testing.T, reg string, c appstore.Consenter) (*appstore.Installer, *appstore.Store) {
	t.Helper()
	st := newStore(t)
	in := &appstore.Installer{
		Registry: appstore.New(fixtureSource(t, reg)),
		Store:    st,
		Consent:  c,
		Now:      func() time.Time { return time.Unix(1780000000, 0).UTC() },
	}
	return in, st
}

// The flow of APP-PLATFORM.md §6, in order: resolve, show, wait, provision.
// The fourth step cannot happen on this box, and that is the interesting part.
func TestInstallAsksThenRecords(t *testing.T) {
	yes := &recorder{answer: true}
	in, st := installer(t, "registry", yes)

	res, err := in.Install(context.Background(), "dev.alexis.standup-notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(yes.sheets) != 1 {
		t.Fatalf("consent was asked %d times", len(yes.sheets))
	}

	rec, err := st.Get("standup-notes")
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID() != "dev.alexis.standup-notes" {
		t.Errorf("id = %q", rec.ID())
	}
	// What was granted is what was shown, sentence for sentence.
	if len(rec.Grants) != len(yes.sheets[0].Rows) {
		t.Fatalf("granted %d, showed %d", len(rec.Grants), len(yes.sheets[0].Rows))
	}
	for i, g := range rec.Grants {
		if g.Scope != yes.sheets[0].Rows[i].Scope || g.Reason != yes.sheets[0].Rows[i].Reason {
			t.Errorf("grant %d = %+v, sheet row = %+v", i, g, yes.sheets[0].Rows[i])
		}
	}
	// Provenance is kept, because "where did this come from" is the first
	// question after anything goes wrong.
	if rec.Registry == "" || rec.Origin.Git == "" || rec.Review == "" {
		t.Errorf("record does not carry its provenance: %+v", rec)
	}

	// The honesty requirement. No app runtime exists, so nothing was
	// provisioned, and neither the record nor the result says otherwise.
	if rec.State != appstore.StateAwaitingRuntime {
		t.Errorf("state = %q, want %q", rec.State, appstore.StateAwaitingRuntime)
	}
	if rec.Running() {
		t.Error("an app with no container must not report as running")
	}
	if res.Note == "" || !strings.Contains(res.Note, "will not run") {
		t.Errorf("note = %q; an install that started nothing has to say so", res.Note)
	}
	if rec.ContainerID != "" {
		t.Errorf("container id = %q with no runtime to create one", rec.ContainerID)
	}

	events, err := st.Log(rec.ID(), 0)
	if err != nil {
		t.Fatal(err)
	}
	kinds := kindsOf(events)
	if kinds != "installed,provision.deferred" {
		t.Errorf("log kinds = %q", kinds)
	}
}

func kindsOf(events []appstore.Event) string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = string(e.Kind)
	}
	return strings.Join(out, ",")
}

// A refusal writes nothing at all — not a record, not a directory. A trace of
// having said no is not a thing to keep.
func TestDeclineWritesNothing(t *testing.T) {
	no := &recorder{answer: false}
	in, st := installer(t, "registry", no)

	res, err := in.Install(context.Background(), "dev.alexis.standup-notes")
	if !errors.Is(err, appstore.ErrDeclined) {
		t.Fatalf("err = %v, want ErrDeclined", err)
	}
	if res.Sheet.AppID == "" {
		t.Error("the sheet that was declined should come back with the error")
	}
	apps, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 0 {
		t.Fatalf("declining installed %d apps", len(apps))
	}
	ents, err := os.ReadDir(st.Root())
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Errorf("declining left %d entries on disk", len(ents))
	}
}

func TestInstallRefusesAnAppTheRegistryDoesNotList(t *testing.T) {
	in, _ := installer(t, "registry", &recorder{answer: true})
	_, err := in.Install(context.Background(), "dev.nobody.nothing")
	if !errors.Is(err, appstore.ErrNotListed) {
		t.Errorf("err = %v", err)
	}
}

// Re-running install with nothing changed must not ask again. Re-asking a
// question whose answer is already on disk is how people learn to click
// through permission sheets.
func TestReinstallingTheSameVersionAsksNothing(t *testing.T) {
	yes := &recorder{answer: true}
	in, _ := installer(t, "registry", yes)
	ctx := context.Background()

	if _, err := in.Install(ctx, "dev.alexis.standup-notes"); err != nil {
		t.Fatal(err)
	}
	res, err := in.Install(ctx, "dev.alexis.standup-notes")
	if err != nil {
		t.Fatal(err)
	}
	if !res.AlreadyInstalled {
		t.Error("want AlreadyInstalled")
	}
	if len(yes.sheets) != 1 {
		t.Errorf("consent was asked %d times for one unchanged app", len(yes.sheets))
	}
	// It still reports the state honestly rather than saying "already
	// installed" and stopping.
	if res.Note == "" {
		t.Error("an app that is installed and cannot run still has to say so")
	}
}

// The upgrade path, across a registry fork: more scopes means a new decision.
func TestUpgradeThroughAForkReAsks(t *testing.T) {
	ctx := context.Background()
	yes := &recorder{answer: true}
	in, st := installer(t, "registry", yes)
	if _, err := in.Install(ctx, "dev.alexis.standup-notes"); err != nil {
		t.Fatal(err)
	}

	// A fork is a config change: same client, same store, different Source.
	in.Registry = appstore.New(fixtureSource(t, "fork"))
	res, err := in.Install(ctx, "dev.alexis.standup-notes")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Upgraded {
		t.Error("want Upgraded")
	}
	if len(yes.sheets) != 2 {
		t.Fatalf("consent was asked %d times", len(yes.sheets))
	}
	up := yes.sheets[1].Upgrade
	if up == nil || len(up.NewScopes) != 1 || up.NewScopes[0] != appstore.ScopeGlassesCamera {
		t.Fatalf("second sheet's upgrade = %+v", up)
	}

	rec, err := st.Get("standup-notes")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Manifest.Version != "1.1.0" {
		t.Errorf("version = %q", rec.Manifest.Version)
	}
	if _, ok := rec.Manifest.Reason(appstore.ScopeGlassesCamera); !ok {
		t.Error("the new scope is not on the record")
	}
	if !strings.Contains(rec.Registry, "fork") {
		t.Errorf("registry = %q; the record should say which registry it came from", rec.Registry)
	}
	if kinds := logKinds(t, st, rec.ID()); kinds != "installed,provision.deferred,upgraded,provision.deferred" {
		t.Errorf("log kinds = %q", kinds)
	}
}

// A declined upgrade leaves the previous grant exactly as it was. The app the
// user already agreed to keeps working; the version they refused does not
// arrive.
func TestDeclinedUpgradeKeepsTheOldGrant(t *testing.T) {
	ctx := context.Background()
	c := &recorder{answer: true}
	in, st := installer(t, "registry", c)
	if _, err := in.Install(ctx, "dev.alexis.standup-notes"); err != nil {
		t.Fatal(err)
	}

	c.answer = false
	in.Registry = appstore.New(fixtureSource(t, "fork"))
	if _, err := in.Install(ctx, "dev.alexis.standup-notes"); !errors.Is(err, appstore.ErrDeclined) {
		t.Fatalf("err = %v", err)
	}

	rec, err := st.Get("standup-notes")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Manifest.Version != "1.0.0" {
		t.Errorf("version = %q, want the version that was agreed to", rec.Manifest.Version)
	}
	if _, ok := rec.Manifest.Reason(appstore.ScopeGlassesCamera); ok {
		t.Error("a declined upgrade granted the scope anyway")
	}
	if !strings.Contains(logKinds(t, st, rec.ID()), "declined") {
		t.Error("a declined upgrade of an installed app is worth recording")
	}
}

func logKinds(t *testing.T, st *appstore.Store, id string) string {
	t.Helper()
	events, err := st.Log(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	return kindsOf(events)
}

// ---------------------------------------------------------------- provisioner

// fakeRuntime stands in for APP-PLATFORM.md §8 step 2. It exists only in this
// test: shipping a stub provisioner would make `relay install` print a success
// for a container nobody built.
type fakeRuntime struct {
	fail        error
	provisioned []string
	destroyed   []string
}

func (f *fakeRuntime) Describe() string { return "fake-runtime" }

func (f *fakeRuntime) Provision(_ context.Context, app appstore.Installed) (appstore.Provisioned, error) {
	if f.fail != nil {
		return appstore.Provisioned{}, f.fail
	}
	f.provisioned = append(f.provisioned, app.ID())
	return appstore.Provisioned{ContainerID: "ctr-" + app.ShortName()}, nil
}

func (f *fakeRuntime) Deprovision(_ context.Context, app appstore.Installed) error {
	f.destroyed = append(f.destroyed, app.ID())
	return nil
}

func TestProvisionerIsCalledOnlyAfterConsent(t *testing.T) {
	ctx := context.Background()
	rt := &fakeRuntime{}
	no := &recorder{answer: false}
	in, _ := installer(t, "registry", no)
	in.Provisioner = rt

	if _, err := in.Install(ctx, "dev.alexis.standup-notes"); !errors.Is(err, appstore.ErrDeclined) {
		t.Fatal(err)
	}
	if len(rt.provisioned) != 0 {
		t.Fatalf("provisioned %v without consent", rt.provisioned)
	}

	no.answer = true
	res, err := in.Install(ctx, "dev.alexis.standup-notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.provisioned) != 1 {
		t.Fatalf("provisioned = %v", rt.provisioned)
	}
	if res.Installed.State != appstore.StateProvisioned || !res.Installed.Running() {
		t.Errorf("state = %q", res.Installed.State)
	}
	if res.Installed.ContainerID != "ctr-standup-notes" {
		t.Errorf("container = %q", res.Installed.ContainerID)
	}
	// The only case where there is nothing to warn about.
	if res.Note != "" {
		t.Errorf("note = %q, want empty for a provisioned app", res.Note)
	}
}

func TestProvisionFailureIsRecordedNotSwallowed(t *testing.T) {
	rt := &fakeRuntime{fail: errors.New("no such image")}
	in, st := installer(t, "registry", &recorder{answer: true})
	in.Provisioner = rt

	res, err := in.Install(context.Background(), "dev.alexis.standup-notes")
	if err != nil {
		t.Fatal(err)
	}
	if res.Installed.State != appstore.StateFailed {
		t.Errorf("state = %q", res.Installed.State)
	}
	// The grant is real and is kept: the user answered the question, and
	// re-asking it because a container failed to build is a different bug.
	rec, err := st.Get("standup-notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Grants) == 0 {
		t.Error("consent was thrown away because provisioning failed")
	}
	if !strings.Contains(res.Note, "no such image") {
		t.Errorf("note = %q, want the actual failure", res.Note)
	}
	if !strings.Contains(logKinds(t, st, rec.ID()), "provision.failed") {
		t.Error("the failure is not in the log")
	}
}

func TestRemove(t *testing.T) {
	ctx := context.Background()
	rt := &fakeRuntime{}
	in, st := installer(t, "registry", &recorder{answer: true})
	in.Provisioner = rt
	if _, err := in.Install(ctx, "dev.alexis.standup-notes"); err != nil {
		t.Fatal(err)
	}

	rec, err := in.Remove(ctx, "standup-notes")
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID() != "dev.alexis.standup-notes" {
		t.Errorf("removed %q", rec.ID())
	}
	if len(rt.destroyed) != 1 {
		t.Errorf("destroyed = %v; the container goes before the record", rt.destroyed)
	}
	if _, err := st.Get("standup-notes"); !errors.Is(err, appstore.ErrNotInstalled) {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(st.Root(), rec.ID())); !os.IsNotExist(err) {
		t.Error("the app's directory survived removal")
	}
	if _, err := in.Remove(ctx, "standup-notes"); !errors.Is(err, appstore.ErrNotInstalled) {
		t.Errorf("err = %v", err)
	}
}

// A record that says a container exists, on a box with nothing that can
// destroy one. Deleting the record would leave it running and unlisted.
func TestRemoveRefusesToOrphanAContainer(t *testing.T) {
	ctx := context.Background()
	rt := &fakeRuntime{}
	in, st := installer(t, "registry", &recorder{answer: true})
	in.Provisioner = rt
	if _, err := in.Install(ctx, "dev.alexis.standup-notes"); err != nil {
		t.Fatal(err)
	}

	in.Provisioner = nil
	_, err := in.Remove(ctx, "standup-notes")
	if err == nil || !strings.Contains(err.Error(), "leave it running and unlisted") {
		t.Fatalf("err = %v", err)
	}
	if _, err := st.Get("standup-notes"); err != nil {
		t.Errorf("the record was removed anyway: %v", err)
	}
}

// ---------------------------------------------------------------- the store

// A state that is not "running" and does not say why is the silent degradation
// this codebase refuses everywhere else.
func TestAStateWithoutAReasonIsRefused(t *testing.T) {
	st := newStore(t)
	rec := appstore.Installed{
		Manifest: appstore.Manifest{ID: "dev.you.app", Name: "App", Version: "1.0.0"},
		State:    appstore.StateAwaitingRuntime,
	}
	if err := st.Put(rec); err == nil || !strings.Contains(err.Error(), "no reason recorded") {
		t.Fatalf("err = %v", err)
	}
	rec.StateReason = "because"
	if err := st.Put(rec); err != nil {
		t.Fatal(err)
	}
}

func TestShortNameAmbiguityIsAnError(t *testing.T) {
	st := newStore(t)
	for _, id := range []string{"dev.alexis.notes", "com.example.notes"} {
		if err := st.Put(appstore.Installed{
			Manifest: appstore.Manifest{ID: id, Name: "Notes", Version: "1.0.0"},
			State:    appstore.StateProvisioned,
		}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := st.Get("notes")
	if err == nil || !strings.Contains(err.Error(), "use the full id") {
		t.Fatalf("err = %v", err)
	}
	if _, err := st.Get("dev.alexis.notes"); err != nil {
		t.Errorf("the full id must still work: %v", err)
	}
}

// Every line written to an app's log goes through the secret detector first.
// Nothing in this package produces a credential — the runtime will, the moment
// it exists, because an app's output is untrusted text — and a key written to
// a log file is as unrecoverable as one written to the index.
func TestLogLinesAreRedactedBeforeWriting(t *testing.T) {
	st := newStore(t)
	// Synthetic, and assembled here so no credential-shaped literal sits in the
	// source: scripts/build-public-repo.sh cannot tell one from the real thing.
	token := "glpat-" + strings.Repeat("A1b2c3d4", 3)
	if err := st.Append("dev.you.app", "runtime.stdout", "the app printed %s", token); err != nil {
		t.Fatal(err)
	}
	events, err := st.Log("dev.you.app", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	if strings.Contains(events[0].Message, token) {
		t.Errorf("the token survived into the log: %q", events[0].Message)
	}
	if len(events[0].Redacted) == 0 {
		t.Error("a redaction happened and the line does not say so")
	}
	// And on disk, which is the thing that actually matters.
	raw, err := os.ReadFile(filepath.Join(st.Root(), "dev.you.app", "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) {
		t.Error("the token is on disk")
	}
	var ev appstore.Event
	if err := json.Unmarshal(raw[:len(raw)-1], &ev); err != nil {
		t.Fatalf("the log is not one JSON object per line: %v", err)
	}
}

func TestLogTail(t *testing.T) {
	st := newStore(t)
	for i := 0; i < 5; i++ {
		if err := st.Append("dev.you.app", appstore.EventInstalled, "line %d", i); err != nil {
			t.Fatal(err)
		}
	}
	events, err := st.Log("dev.you.app", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Message != "line 3" || events[1].Message != "line 4" {
		t.Errorf("tail = %+v", events)
	}
	// An app with no log at all is not an error; it is a new app.
	if got, err := st.Log("dev.you.other", 0); err != nil || got != nil {
		t.Errorf("got %v, %v", got, err)
	}
}

func TestInstalledRecordsSurviveAReopen(t *testing.T) {
	dir := t.TempDir()
	st, err := appstore.OpenStore(appstore.StoreRoot(dir))
	if err != nil {
		t.Fatal(err)
	}
	in := &appstore.Installer{
		Registry: appstore.New(fixtureSource(t, "registry")),
		Store:    st,
		Consent:  &recorder{answer: true},
	}
	if _, err := in.Install(context.Background(), "dev.alexis.standup-notes"); err != nil {
		t.Fatal(err)
	}

	again, err := appstore.OpenStore(appstore.StoreRoot(dir))
	if err != nil {
		t.Fatal(err)
	}
	apps, err := again.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].ID() != "dev.alexis.standup-notes" {
		t.Fatalf("apps = %+v", apps)
	}
	if len(apps[0].Grants) != 4 {
		t.Errorf("grants = %+v", apps[0].Grants)
	}
}

// An unreadable record is not an app that quietly vanishes from `relay list`.
func TestAnUnreadableRecordIsReported(t *testing.T) {
	st := newStore(t)
	dir := filepath.Join(st.Root(), "dev.you.app")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "installed.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.List(); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("err = %v", err)
	}
}
