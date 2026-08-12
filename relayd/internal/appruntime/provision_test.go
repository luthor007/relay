package appruntime_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/appruntime"
	"github.com/luthor007/relay/relayd/internal/apps"
	"github.com/luthor007/relay/relayd/internal/appstore"
)

// An installed app actually runs
//
// `appstore.Provisioner` had no implementation anywhere, so `relay install`
// recorded consent and printed "no app runtime is attached on this box". These
// tests are the other side of that sentence: a package is fetched, staged,
// registered, and a phrase wakes it — through the real runtime, in the real
// sandbox, on real Node.

const manifest = `{
  "id": "dev.test.standup",
  "name": "Standup",
  "version": "1.0.0",
  "description": "Reads back what you committed to.",
  "author": {"name": "Alexis"},
  "permissions": [{"scope": "memory.read", "reason": "To read the meeting you just left."}],
  "triggers": [{"type": "phrase", "match": "wrap up the standup"}]
}`

const source = `
export default {
  async onTrigger(ctx) {
    await ctx.ui.card("Standup", { body: "It ran." });
  },
};
`

// stage writes an app package to a directory and returns it.
func stage(t *testing.T, m, code string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "relay.json"), []byte(m), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte(code), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

type screen struct{ drawn []apps.Rendered }

func (s *screen) Render(_ context.Context, r apps.Rendered) error {
	s.drawn = append(s.drawn, r)
	return nil
}
func (s *screen) Ask(context.Context, apps.Rendered) (bool, error) { return false, nil }

// harness builds a real runtime, dispatcher and provisioner.
func harness(t *testing.T) (*appruntime.Provisioner, *apps.Dispatcher, *screen) {
	t.Helper()
	return harnessOver(t, "")
}

// harnessOver builds one over an existing apps directory, which is what a
// restarted daemon has.
func harnessOver(t *testing.T, appsDir string) (*appruntime.Provisioner, *apps.Dispatcher, *screen) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH")
	}
	dir := t.TempDir()
	if appsDir == "" {
		appsDir = filepath.Join(dir, "apps")
	}
	sc := &screen{}
	rt, err := apps.New(context.Background(), apps.Options{
		Node:       node,
		RuntimeDir: filepath.Join(dir, "runtime"),
		Redact:     apps.Detector(),
		Screen:     sc,
		AccessLog:  &apps.MemoryAccessLog{},
		EgressLog:  &apps.MemoryEgressLog{},
		Limits:     apps.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := apps.NewDispatcher(apps.DispatcherOptions{Runtime: rt})
	if err != nil {
		t.Fatal(err)
	}
	p, err := appruntime.New(appruntime.Options{
		Runtime:    rt,
		Dispatcher: d,
		Dir:        appsDir,
		Fetch:      appruntime.DirFetcher{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p, d, sc
}

func record(pkg string, scopes ...string) appstore.Installed {
	grants := make([]appstore.Permission, 0, len(scopes))
	for _, s := range scopes {
		grants = append(grants, appstore.Permission{Scope: appstore.Scope(s), Reason: "because"})
	}
	return appstore.Installed{
		Manifest: appstore.Manifest{ID: "dev.test.standup", Name: "Standup", Version: "1.0.0"},
		Grants:   grants,
		Origin:   appstore.EntrySource{Git: pkg},
		State:    appstore.StateProvisioned,
	}
}

func TestAProvisionedAppRunsWhenItsPhraseIsSpoken(t *testing.T) {
	p, d, sc := harness(t)
	pkg := stage(t, manifest, source)

	if _, err := p.Provision(context.Background(), record(pkg, "memory.read")); err != nil {
		t.Fatal(err)
	}

	// The whole point of provisioning: something can now trigger it. Before this
	// package existed the dispatcher was empty on every box.
	invocations := d.Phrase(context.Background(), "wrap up the standup")
	if len(invocations) != 1 {
		t.Fatalf("the phrase woke %d apps, want 1", len(invocations))
	}
	if inv := invocations[0]; inv.Outcome != apps.OutcomeCompleted {
		t.Fatalf("outcome %s: %s", inv.Outcome, inv.Error)
	}
	if len(sc.drawn) != 1 || sc.drawn[0].View.Blocks[0].Title != "Standup" {
		t.Fatalf("the app did not draw: %+v", sc.drawn)
	}
}

func TestTheAppGetsWhatTheUserConsentedToAndNotWhatItAskedFor(t *testing.T) {
	// appstore.Provisioner's contract, and the single way a provisioner can
	// break the only guarantee the install sheet makes. The manifest asks for
	// memory.read; the record carries no grant, because the user declined.
	p, d, _ := harness(t)
	pkg := stage(t, manifest, `
export default {
  async onTrigger(ctx) {
    if (ctx.memory) throw new Error("memory was minted and the user declined it");
    ctx.log("declined: " + ctx.declined.join(","));
  },
};
`)
	if _, err := p.Provision(context.Background(), record(pkg)); err != nil {
		t.Fatal(err)
	}
	inv := d.Phrase(context.Background(), "wrap up the standup")
	if len(inv) != 1 || inv[0].Outcome != apps.OutcomeCompleted {
		t.Fatalf("outcome %+v", inv)
	}
}

func TestAPackageThatClaimsADifferentIdIsRefused(t *testing.T) {
	// It would be installed under one name and run as another, which makes the
	// permission sheet a description of something else.
	p, _, _ := harness(t)
	other := strings.Replace(manifest, "dev.test.standup", "dev.test.impostor", 1)
	pkg := stage(t, other, source)

	_, err := p.Provision(context.Background(), record(pkg, "memory.read"))
	if err == nil {
		t.Fatal("a package claiming another id was provisioned")
	}
	if !strings.Contains(err.Error(), "impostor") {
		t.Errorf("the error does not name what it found: %v", err)
	}
}

func TestDeprovisioningRemovesTheAppAndItsPrivateStorage(t *testing.T) {
	p, d, _ := harness(t)
	pkg := stage(t, manifest, source)
	rec := record(pkg, "memory.read")
	if _, err := p.Provision(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if len(d.List()) != 1 {
		t.Fatal("not registered")
	}

	if err := p.Deprovision(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if len(d.List()) != 0 {
		t.Error("the app is still triggerable after being removed")
	}
	// §8's deprovision destroys the container and the app's storage. Storage
	// that survives an uninstall is data the user believes they deleted.
	if _, err := apps.ReadManifest(filepath.Join(p.Dir(), "dev.test.standup")); err == nil {
		t.Error("the package is still on disk")
	}
}

func TestASymlinkInAPackageIsRefused(t *testing.T) {
	// A symlink is a path out of the app's root, and the sandbox's read
	// allowance is expressed in paths.
	p, _, _ := harness(t)
	pkg := stage(t, manifest, source)
	if err := os.Symlink("/etc/passwd", filepath.Join(pkg, "src", "sneaky.ts")); err != nil {
		t.Skip("this filesystem does not do symlinks")
	}
	if _, err := p.Provision(context.Background(), record(pkg, "memory.read")); err == nil {
		t.Fatal("a package containing a symlink was staged")
	}
}

func TestAppsSurviveARestart(t *testing.T) {
	// The failure that made having no provisioner invisible: a dispatcher that
	// is empty on every start looks exactly like the app being broken.
	p, d, _ := harness(t)
	pkg := stage(t, manifest, source)
	rec := record(pkg, "memory.read")
	if _, err := p.Provision(context.Background(), rec); err != nil {
		t.Fatal(err)
	}

	// A fresh dispatcher, as a restarted daemon has.
	store, err := appstore.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(rec); err != nil {
		t.Fatal(err)
	}

	p2, d2, _ := harnessOver(t, p.Dir())
	if problems := p2.Load(store); len(problems) != 0 {
		t.Fatalf("reload reported %v", problems)
	}
	if len(d2.List()) != 1 {
		t.Fatal("the app is not triggerable after a restart")
	}
	inv := d2.Phrase(context.Background(), "wrap up the standup")
	if len(inv) != 1 || inv[0].Outcome != apps.OutcomeCompleted {
		t.Fatalf("it did not run after a restart: %+v", inv)
	}
	_ = d
}

func TestOneBrokenAppDoesNotStopTheOthers(t *testing.T) {
	// A daemon that refuses to start because of a third-party package is a
	// daemon third-party packages can take down.
	p, _, _ := harness(t)
	store, err := appstore.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	missing := record("/nowhere")
	missing.Manifest.ID = "dev.test.missing"
	if err := store.Put(missing); err != nil {
		t.Fatal(err)
	}
	problems := p.Load(store)
	if len(problems) != 1 {
		t.Fatalf("expected one problem reported, got %v", problems)
	}
	if !strings.Contains(problems[0].Error(), "dev.test.missing") {
		t.Errorf("the problem does not name the app: %v", problems[0])
	}
}
