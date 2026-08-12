package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// install.sh is what people are asked to pipe into a shell, so it gets tested
// like code rather than trusted like documentation.
//
// The whole thing runs here against a local release server: it downloads, it
// verifies a real SHA-256, it unpacks, it installs, and it refuses a corrupted
// archive. The one part that cannot run in CI is the interactive `relay setup`
// it ends with, which is why the test drives it with RELAY_NO_SETUP.

// oneLineFunc matches a whole shell function written on a single line.
var oneLineFunc = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\(\)\s*\{.*\}$`)

func installScript(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		p := filepath.Join(dir, "install.sh")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		dir = filepath.Dir(dir)
	}
	t.Skip("install.sh not found from the test's working directory")
	return ""
}

func sh(t *testing.T, script string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestInstallScriptIsValidPOSIXShell(t *testing.T) {
	script := installScript(t)
	out, err := exec.Command("/bin/sh", "-n", script).CombinedOutput()
	if err != nil {
		t.Fatalf("sh -n failed: %v\n%s", err, out)
	}
}

// The one real hazard of curl | sh: a truncated download running half an
// install. Everything in the script is a definition and exactly one line acts,
// so a cut connection does nothing at all.
func TestInstallScriptOnlyActsOnItsLastLine(t *testing.T) {
	b, err := os.ReadFile(installScript(t))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if last != `main "$@"` {
		t.Fatalf("last line is %q, want `main \"$@\"`", last)
	}
	// Nothing above it may execute at top level. Anything that is not a
	// comment, a blank line, a variable assignment, a `set`, a trap or part of
	// a function body would run during a partial download.
	depth := 0
	for i, line := range lines[:len(lines)-1] {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		// A whole function on one line: say() { printf ...; }
		if oneLineFunc.MatchString(s) {
			continue
		}
		if strings.HasSuffix(s, "{") || strings.HasSuffix(s, "() {") {
			depth++
			continue
		}
		if s == "}" {
			depth--
			continue
		}
		if depth > 0 {
			continue
		}
		if strings.HasPrefix(s, "set ") || strings.Contains(s, "=") {
			continue
		}
		t.Errorf("line %d executes before main: %q", i+1, s)
	}
}

func TestInstallScriptDryRunDownloadsNothing(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer srv.Close()

	out, err := sh(t, installScript(t), []string{"RELAY_BASE_URL=" + srv.URL}, "--dry-run")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if hits != 0 {
		t.Errorf("--dry-run made %d requests", hits)
	}
	for _, want := range []string{"platform", "install to", "Nothing was downloaded"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output missing %q:\n%s", want, out)
		}
	}
}

// releaseServer serves a real release: a VERSION file, a tar.gz with two fake
// binaries, and a checksums.txt. corrupt makes the archive not match its sum.
func releaseServer(t *testing.T, version string, corrupt bool) *httptest.Server {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"relay", "relayd"} {
		body := "#!/bin/sh\necho " + name + " \"$@\"\n"
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o755, Size: int64(len(body)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	archive := buf.Bytes()

	sum := sha256.Sum256(archive)
	digest := hex.EncodeToString(sum[:])
	if corrupt {
		digest = strings.Repeat("0", 64)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/VERSION", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, version)
	})
	mux.HandleFunc("/"+version+"/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		for _, os_ := range []string{"linux", "darwin"} {
			for _, arch := range []string{"amd64", "arm64"} {
				fmt.Fprintf(w, "%s  relay_%s_%s_%s.tar.gz\n", digest, version, os_, arch)
			}
		}
	})
	mux.HandleFunc("/"+version+"/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".tar.gz") {
			http.NotFound(w, r)
			return
		}
		w.Write(archive)
	})
	return httptest.NewServer(mux)
}

func TestInstallScriptInstallsAndVerifies(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	srv := releaseServer(t, "v9.9.9", false)
	defer srv.Close()

	dir := t.TempDir()
	out, err := sh(t, installScript(t), []string{
		"RELAY_BASE_URL=" + srv.URL,
		"RELAY_INSTALL_DIR=" + dir,
		"RELAY_NO_SETUP=1",
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, bin := range []string{"relay", "relayd"} {
		fi, err := os.Stat(filepath.Join(dir, bin))
		if err != nil {
			t.Fatalf("%s was not installed: %v\n%s", bin, err, out)
		}
		if fi.Mode()&0o111 == 0 {
			t.Errorf("%s is not executable", bin)
		}
	}
	// The version came from the VERSION file, not from a guess.
	if !strings.Contains(out, "v9.9.9") {
		t.Errorf("output does not name the resolved version:\n%s", out)
	}
	// A directory that is not on PATH is called out rather than left to fail
	// later as "command not found".
	if !strings.Contains(out, "not on your PATH") {
		t.Errorf("expected a PATH warning for %s:\n%s", dir, out)
	}
}

// The check that matters: a binary whose checksum does not match is never
// installed, and the script says so rather than continuing.
func TestInstallScriptRefusesABadChecksum(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	if _, err := exec.LookPath("sha256sum"); err != nil {
		if _, err := exec.LookPath("shasum"); err != nil {
			t.Skip("no sha256sum or shasum, so the script correctly declines to verify")
		}
	}
	srv := releaseServer(t, "v9.9.9", true)
	defer srv.Close()

	dir := t.TempDir()
	out, err := sh(t, installScript(t), []string{
		"RELAY_BASE_URL=" + srv.URL,
		"RELAY_INSTALL_DIR=" + dir,
		"RELAY_NO_SETUP=1",
	})
	if err == nil {
		t.Fatalf("a corrupted archive was accepted:\n%s", out)
	}
	if !strings.Contains(out, "checksum mismatch") {
		t.Errorf("output should say why:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "relay")); err == nil {
		t.Error("a binary was installed despite the mismatch")
	}
}

func TestInstallScriptRejectsUnknownOptions(t *testing.T) {
	out, err := sh(t, installScript(t), nil, "--wat")
	if err == nil {
		t.Fatalf("want a non-zero exit:\n%s", out)
	}
	if !strings.Contains(out, "unknown option") {
		t.Errorf("output = %q", out)
	}
}
