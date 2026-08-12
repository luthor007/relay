package appstore_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/appstore"
)

// Everything here runs against relayd/testdata/appstore, on disk. No test in
// this package reaches the network: resolution is behind [appstore.Source] for
// exactly that reason, and the one HTTP test below serves itself from
// httptest on loopback.

func fixtureRegistry(t *testing.T) *appstore.Registry {
	t.Helper()
	return appstore.New(fixtureSource(t, "registry"))
}

func fixtureSource(t *testing.T, name string) appstore.Source {
	t.Helper()
	src, err := appstore.NewDirSource(filepath.Join("..", "..", "testdata", "appstore", name))
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func TestResolveFromADirectory(t *testing.T) {
	e, err := fixtureRegistry(t).Resolve(context.Background(), "dev.alexis.standup-notes")
	if err != nil {
		t.Fatal(err)
	}
	if e.Manifest.Name != "Standup Notes" || e.Manifest.Version != "1.0.0" {
		t.Errorf("manifest = %+v", e.Manifest)
	}
	// A manifest with no code behind it is not installable, so the entry has to
	// say where the source is and which commit was reviewed.
	if e.Source.Git != "https://github.com/luthor007/standup-notes" || e.Source.Rev != "v1.0.0" {
		t.Errorf("source = %+v", e.Source)
	}
	if e.Review == "" {
		t.Error("the entry should record the pull request that listed it")
	}
}

func TestResolveRefusals(t *testing.T) {
	reg := fixtureRegistry(t)
	ctx := context.Background()

	if _, err := reg.Resolve(ctx, "dev.nobody.nothing"); !errors.Is(err, appstore.ErrNotListed) {
		t.Errorf("err = %v, want ErrNotListed", err)
	}
	// The registry is named in the refusal: with forks supported, "not found"
	// without saying which registry was asked is unactionable.
	_, err := reg.Resolve(ctx, "dev.nobody.nothing")
	if err == nil || !strings.Contains(err.Error(), "testdata") {
		t.Errorf("err = %v, want it to name the registry it asked", err)
	}

	// A file name and a manifest id that disagree: one of them is what the
	// reviewer read and the other is what would be installed.
	_, err = reg.Resolve(ctx, "com.example.mismatched")
	if err == nil || !strings.Contains(err.Error(), "must be the same app") {
		t.Errorf("err = %v", err)
	}

	// Manifest rules are enforced at resolve, before a sheet is built from it.
	_, err = reg.Resolve(ctx, "com.example.vague")
	if err == nil || !strings.Contains(err.Error(), "a reason a user can read") {
		t.Errorf("err = %v", err)
	}

	if _, err := reg.Resolve(ctx, "not an id"); err == nil ||
		!strings.Contains(err.Error(), "not an app id") {
		t.Errorf("err = %v", err)
	}
}

func TestListReadsTheIndex(t *testing.T) {
	got, err := fixtureRegistry(t).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"com.example.commute-brief", "dev.alexis.standup-notes"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("list = %v, want %v", got, want)
	}
}

// A registry with no index.json is not broken and must not report zero apps —
// it cannot enumerate, which is a different fact.
func TestARegistryWithNoIndexSaysSo(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "apps"), 0o700); err != nil {
		t.Fatal(err)
	}
	src, err := appstore.NewDirSource(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = appstore.New(src).List(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no index.json") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "still resolve by id") {
		t.Errorf("the message should say what still works: %v", err)
	}
}

// Forking is a config change, not a patch: the same client, the same code
// path, a different spec string.
func TestAForkIsAConfigChange(t *testing.T) {
	ctx := context.Background()
	upstream, err := fixtureRegistry(t).Resolve(ctx, "dev.alexis.standup-notes")
	if err != nil {
		t.Fatal(err)
	}
	fork, err := appstore.New(fixtureSource(t, "fork")).Resolve(ctx, "dev.alexis.standup-notes")
	if err != nil {
		t.Fatal(err)
	}
	if upstream.Manifest.Version == fork.Manifest.Version {
		t.Fatal("the fixture fork must differ from upstream or it proves nothing")
	}
	if len(fork.Manifest.Permissions) <= len(upstream.Manifest.Permissions) {
		t.Error("the fork's app asks for more, which is what makes the upgrade test meaningful")
	}
}

func TestParseSpec(t *testing.T) {
	dir := t.TempDir()

	for _, tc := range []struct{ spec, wantDescribe string }{
		{"", appstore.DefaultRegistry},
		{"github:luthor007/relay-apps@main", "github:luthor007/relay-apps@main"},
		{"github:someone/their-apps@v2", "github:someone/their-apps@v2"},
		{"https://apps.example.com/registry/", "https://apps.example.com/registry/"},
		{"http://localhost:9000/", "http://localhost:9000/"},
		{dir, dir},
		{"file://" + dir, "file://" + dir},
	} {
		src, err := appstore.ParseSpec(tc.spec)
		if err != nil {
			t.Errorf("ParseSpec(%q): %v", tc.spec, err)
			continue
		}
		if src.Describe() != tc.wantDescribe {
			t.Errorf("ParseSpec(%q).Describe() = %q, want %q", tc.spec, src.Describe(), tc.wantDescribe)
		}
	}

	for _, tc := range []struct{ spec, want string }{
		{"http://apps.example.com/", "plain HTTP"},
		{"ftp://apps.example.com/", "not a registry this box knows"},
		{"github:justowner", "github:owner/repo@ref"},
		{filepath.Join(dir, "nope"), "no such file"},
	} {
		if _, err := appstore.ParseSpec(tc.spec); err == nil ||
			!strings.Contains(err.Error(), tc.want) {
			t.Errorf("ParseSpec(%q) err = %v, want %q", tc.spec, err, tc.want)
		}
	}
}

// GitHub is where our registry happens to live, not a capability the design
// depends on: the spec expands to a plain HTTPS base URL and nothing else.
func TestGitHubSpecExpandsToRawFiles(t *testing.T) {
	src, err := appstore.ParseSpec("github:luthor007/relay-apps@main")
	if err != nil {
		t.Fatal(err)
	}
	h, ok := src.(*appstore.HTTPSource)
	if !ok {
		t.Fatalf("github spec produced %T, want the same HTTP source everything else uses", src)
	}
	want := "https://raw.githubusercontent.com/luthor007/relay-apps/main/"
	if h.Base != want {
		t.Errorf("base = %q, want %q", h.Base, want)
	}
	// Describe stays the spec the user configured, because that is what the
	// permission sheet should show.
	if h.Describe() != "github:luthor007/relay-apps@main" {
		t.Errorf("describe = %q", h.Describe())
	}
}

// The HTTP source, exercised end to end against a server on loopback. Nothing
// here leaves the machine.
func TestHTTPSource(t *testing.T) {
	body := mustRead(t, filepath.Join("..", "..", "testdata", "appstore", "registry",
		"apps", "dev.alexis.standup-notes.json"))
	big := strings.Repeat("x", 8192)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apps/dev.alexis.standup-notes.json":
			w.Write(body)
		case "/apps/com.example.huge.json":
			w.Write([]byte(big))
		case "/apps/com.example.broken.json":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	src := &appstore.HTTPSource{Base: srv.URL + "/", Name: "test", MaxBytes: 4096}
	reg := appstore.New(src)
	ctx := context.Background()

	if _, err := reg.Resolve(ctx, "dev.alexis.standup-notes"); err != nil {
		t.Fatalf("resolve over HTTP: %v", err)
	}
	// 404 is "not listed", which is a different thing from "the registry is
	// unreachable" and must not be reported as the same.
	if _, err := reg.Resolve(ctx, "dev.nobody.nothing"); !errors.Is(err, appstore.ErrNotListed) {
		t.Errorf("404 should mean not listed, got %v", err)
	}
	if _, err := reg.Resolve(ctx, "com.example.broken"); err == nil ||
		errors.Is(err, appstore.ErrNotListed) || !strings.Contains(err.Error(), "500") {
		t.Errorf("a 500 is not an answer about whether the app is listed: %v", err)
	}
	if _, err := reg.Resolve(ctx, "com.example.huge"); err == nil ||
		!strings.Contains(err.Error(), "larger than") {
		t.Errorf("err = %v, want the size cap to fire", err)
	}
}

// A Source is an interface anyone can hand a path to, and `..` on a DirSource
// reads the user's disk.
func TestSourcePathsCannotEscape(t *testing.T) {
	src := fixtureSource(t, "registry")
	for _, bad := range []string{"../secrets/rules.json", "/etc/passwd", "apps/../../go.mod", ""} {
		if _, err := src.Fetch(context.Background(), bad); err == nil {
			t.Errorf("Fetch(%q) was allowed", bad)
		}
	}
}

func TestResolveSpecPrecedence(t *testing.T) {
	dir := t.TempDir()

	// 4. Nothing set anywhere.
	t.Setenv(appstore.EnvRegistry, "")
	got, err := appstore.ResolveSpec("", dir)
	if err != nil || got != appstore.DefaultRegistry {
		t.Fatalf("got %q, %v; want the default", got, err)
	}

	// 3. A file in the config directory — one line, so forking is `echo` and
	// not an edit to a file another command rewrites.
	path := filepath.Join(dir, appstore.RegistryFile)
	if err := os.WriteFile(path, []byte("# a fork\n\ngithub:someone/their-apps@main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err = appstore.ResolveSpec("", dir); err != nil || got != "github:someone/their-apps@main" {
		t.Fatalf("got %q, %v; want the file's spec", got, err)
	}

	// 2. The environment beats the file.
	t.Setenv(appstore.EnvRegistry, "github:env/apps@main")
	if got, err = appstore.ResolveSpec("", dir); err != nil || got != "github:env/apps@main" {
		t.Fatalf("got %q, %v; want the environment", got, err)
	}

	// 1. The flag beats everything.
	if got, err = appstore.ResolveSpec("  /srv/apps  ", dir); err != nil || got != "/srv/apps" {
		t.Fatalf("got %q, %v; want the flag", got, err)
	}
}

// A registry entry that points at an ssh remote, an http remote, or no commit
// at all is refused: this is code that will run on the user's machine.
func TestEntrySourceIsChecked(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "apps"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `"manifest": {
	  "id": "dev.you.app", "name": "App", "version": "1.0.0",
	  "description": "Does a thing.", "author": {"name": "You"},
	  "permissions": [], "triggers": [{"type": "phrase", "match": "hello"}]
	}`
	for _, tc := range []struct{ source, want string }{
		{`{"git": "git@github.com:you/app.git", "rev": "v1"}`, "must be https"},
		{`{"git": "http://github.com/you/app", "rev": "v1"}`, "must be https"},
		{`{"git": "https://github.com/you/app"}`, "no source.rev"},
		{`{"rev": "v1"}`, "no source.git"},
		{`{"git": "https://github.com/you/app", "rev": "v1", "subdir": "../../etc"}`, "relative path"},
	} {
		body := "{" + manifest + `, "source": ` + tc.source + "}"
		if err := os.WriteFile(filepath.Join(dir, "apps", "dev.you.app.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		src, err := appstore.NewDirSource(dir)
		if err != nil {
			t.Fatal(err)
		}
		_, err = appstore.New(src).Resolve(context.Background(), "dev.you.app")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("source %s: err = %v, want %q", tc.source, err, tc.want)
		}
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
