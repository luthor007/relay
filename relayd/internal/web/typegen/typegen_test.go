package typegen

import (
	"os"
	"strings"
	"testing"
)

// TestGeneratedTypesAreCurrent is the whole point of this package.
//
// The console's types are checked in so that `npm run build` needs no Go
// toolchain, and checked-in generated code rots unless something fails when it
// does. This is that something: rename a field in internal/api and this test
// fails with the file that has to be regenerated, before anyone opens a screen
// and finds an undefined.
func TestGeneratedTypesAreCurrent(t *testing.T) {
	want, err := Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	path, err := OutputFile()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// The console is not part of the public repo, so its generated types
		// are not there to compare against. That is a missing sibling, not
		// stale output — the drift this guards against can only happen where
		// both halves exist, and there it still fails.
		t.Skipf("the console is not in this tree (%s); nothing to check against", path)
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) == string(want) {
		return
	}

	t.Errorf("%s is out of date.\n\nRun:  cd relayd && go generate ./internal/web/...\n\n%s",
		path, firstDifference(string(got), string(want)))
}

// firstDifference reports the first line that differs, which is almost always
// the renamed field and is far more useful than a 400-line diff.
func firstDifference(got, want string) string {
	g := strings.Split(got, "\n")
	w := strings.Split(want, "\n")
	for i := range max(len(g), len(w)) {
		gl, wl := "", ""
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl {
			return "first difference at line " + itoa(i+1) + ":\n  on disk:   " + gl + "\n  generated: " + wl
		}
	}
	return "files differ only in length"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestEveryRootIsReachable checks that nothing in Roots quietly stopped being
// emitted — a struct that walks to nothing produces an empty interface, which
// compiles in TypeScript and means the screens have no fields at all.
func TestEveryRootIsReachable(t *testing.T) {
	out, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, r := range Roots() {
		decl := "export interface " + r.Name + " {\n"
		i := strings.Index(text, decl)
		if i < 0 {
			t.Errorf("root %s was not emitted", r.Name)
			continue
		}
		body := text[i+len(decl):]
		if strings.HasPrefix(body, "}") {
			t.Errorf("root %s emitted an empty interface — did its fields lose their json tags?", r.Name)
		}
	}
	for _, st := range sourceTypes() {
		if !strings.Contains(text, "export interface "+st.Name+" {\n") {
			t.Errorf("source type %s (%s) was not emitted", st.Name, st.Go)
		}
	}
}

// TestNullabilityIsExpressed pins the three encoding/json rules that a
// hand-written client gets wrong, because getting them wrong is silent:
//
//   - a *float64 with no omitempty marshals to null, never to 0. rest.go is
//     explicit that cost is nil rather than zero wherever a runtime cannot
//     report it, and a client typed as `number` renders "$0.00" for "nobody
//     knows".
//   - a nil slice with no omitempty marshals to null, not [].
//   - omitempty means the key is absent, which is optional, not nullable.
func TestNullabilityIsExpressed(t *testing.T) {
	out, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)

	for _, want := range []string{
		"cost_usd: number | null;",
		"tokens: number | null;",
		"incidents: Incident[] | null;",
		"version?: string;",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("generated types are missing %q", want)
		}
	}
}

// TestUnionsCoverKnownStates keeps the status pill honest: a state that exists
// in Go must exist in the TypeScript union.
func TestUnionsCoverKnownStates(t *testing.T) {
	out, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, want := range []string{
		`export type SessionState = "running" | "awaiting" | "idle" | "closed";`,
		`export type ChangeKind = "added" | "updated" | "closed";`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("generated types are missing:\n%s", want)
		}
	}
}
