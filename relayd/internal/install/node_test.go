package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/detect"
)

// The Node bootstrap, which exists so that one command works on a wiped Mac
// mini. These run against a tarball built in memory: the point is what the
// installer does with what it is handed, and nodejs.org is not part of that.

// fakeNodeTar builds a tarball shaped like a real Node release — one root
// directory with bin/node inside it.
func fakeNodeTar(t *testing.T, root string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	write := func(name string, mode int64, body string) {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	write(root+"/bin/node", 0o755, "#!/bin/sh\necho v24.19.0\n")
	write(root+"/bin/npm", 0o755, "#!/bin/sh\necho 10.0.0\n")
	write(root+"/bin/npx", 0o755, "#!/bin/sh\n")
	write(root+"/README.md", 0o644, "node")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// nodeServer answers the two URLs the bootstrap fetches. sum is what it claims
// the checksum is, so a test can lie about it.
func nodeServer(t *testing.T, archive string, tarball []byte, sum string) *http.Client {
	t.Helper()
	if sum == "" {
		h := sha256.Sum256(tarball)
		sum = hex.EncodeToString(h[:])
	}
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		body := ""
		switch {
		case strings.HasSuffix(r.URL.Path, "SHASUMS256.txt"):
			body = fmt.Sprintf("%s  %s\n%s  node-other.tar.gz\n", sum, archive, sum)
		case strings.HasSuffix(r.URL.Path, archive):
			return &http.Response{
				StatusCode: 200, Header: http.Header{},
				Body: io.NopCloser(bytes.NewReader(tarball)), Request: r,
			}, nil
		default:
			return &http.Response{StatusCode: 404, Header: http.Header{},
				Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
}

func nodeOpts(t *testing.T, client *http.Client) Options {
	t.Helper()
	home := t.TempDir()
	return Options{
		Prompt: NewScript(map[string]string{}), HTTPClient: client,
		Env: detect.Env{Home: home, Exec: &detect.FakeExec{}, FS: &detect.MemFS{}},
	}.withDefaults()
}

func TestNodeIsUnpackedAndLinkedIntoLocalBin(t *testing.T) {
	archive := nodeArchive("darwin", "arm64")
	root := strings.TrimSuffix(archive, ".tar.gz")
	opts := nodeOpts(t, nodeServer(t, archive, fakeNodeTar(t, root), ""))

	dir, err := installNode(context.Background(), opts, archive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bin", "node")); err != nil {
		t.Fatalf("node is not where installNode says it is: %v", err)
	}
	// The symlinks are the whole point — install.sh already put ~/.local/bin on
	// PATH, so this is what makes npm reachable to the next step.
	for _, name := range []string{"node", "npm", "npx"} {
		link := filepath.Join(opts.Env.Home, ".local", "bin", name)
		target, err := os.Readlink(link)
		if err != nil {
			t.Errorf("%s was not linked: %v", name, err)
			continue
		}
		if !strings.HasSuffix(target, filepath.Join("bin", name)) {
			t.Errorf("%s points at %s", name, target)
		}
	}
}

// A language runtime is the worst thing to install unverified: everything Relay
// installs afterwards executes inside it.
func TestAWrongChecksumInstallsNothing(t *testing.T) {
	archive := nodeArchive("darwin", "arm64")
	root := strings.TrimSuffix(archive, ".tar.gz")
	tarball := fakeNodeTar(t, root)
	opts := nodeOpts(t, nodeServer(t, archive, tarball, strings.Repeat("00", 32)))

	_, err := installNode(context.Background(), opts, archive)
	if err == nil {
		t.Fatal("a mismatched checksum installed anyway")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("err = %v, want it to name the mismatch", err)
	}
	if _, statErr := os.Stat(filepath.Join(opts.Env.Home, ".local", "bin", "node")); statErr == nil {
		t.Error("a rejected download left a node behind")
	}
}

// No checksums, no install. Not a warning — the one case where "carry on" is
// clearly wrong.
func TestNoPublishedChecksumsIsFatal(t *testing.T) {
	archive := nodeArchive("darwin", "arm64")
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 500, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}
	if _, err := installNode(context.Background(), nodeOpts(t, client), archive); err == nil {
		t.Fatal("installed without verifying anything")
	}
}

// A tarball entry that escapes its directory is a download that is not what it
// claims to be, so it is refused rather than sanitised.
func TestAnArchiveThatEscapesItsDirectoryIsRefused(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := "owned"
	if err := tw.WriteHeader(&tar.Header{
		Name: "../../../../tmp/relay-escape", Mode: 0o644,
		Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()

	if _, err := untarGz(bytes.NewReader(buf.Bytes()), t.TempDir()); err == nil {
		t.Fatal("an escaping entry was unpacked")
	}
}

func TestNodeArchiveNamesOnlyThePlatformsWeShip(t *testing.T) {
	for _, c := range []struct{ goos, goarch, want string }{
		{"darwin", "arm64", "node-" + NodePin + "-darwin-arm64.tar.gz"},
		{"darwin", "amd64", "node-" + NodePin + "-darwin-x64.tar.gz"},
		{"linux", "amd64", "node-" + NodePin + "-linux-x64.tar.gz"},
		{"linux", "arm64", "node-" + NodePin + "-linux-arm64.tar.gz"},
		{"windows", "amd64", ""},
		{"linux", "riscv64", ""},
	} {
		if got := nodeArchive(c.goos, c.goarch); got != c.want {
			t.Errorf("nodeArchive(%s,%s) = %q, want %q", c.goos, c.goarch, got, c.want)
		}
	}
}

// The pinned version has to satisfy the bus, or the bootstrap installs a Node
// the next step rejects.
func TestThePinnedNodeSatisfiesTheBus(t *testing.T) {
	if !nodeOK(NodePin) {
		t.Fatalf("NodePin %s is outside the ranges OpenClaw accepts", NodePin)
	}
}

// Installing Node is not the same as being able to use it.
//
// install.sh runs `$INSTALL_DIR/relay setup` by absolute path and warns, two
// lines earlier, that $INSTALL_DIR is not on PATH — which on a fresh Mac mini it
// is not. So the bootstrap would link node into ~/.local/bin, look for `node` on
// a PATH without it, and report failure about a Node it had just installed
// successfully. Everything after — npm for the runtimes, npm for OpenClaw,
// openclaw itself — resolves through that same PATH.
func TestTheInstalledNodeIsPutOnThisProcessPath(t *testing.T) {
	archive := nodeArchive("darwin", "arm64")
	root := strings.TrimSuffix(archive, ".tar.gz")
	opts := nodeOpts(t, nodeServer(t, archive, fakeNodeTar(t, root), ""))

	bin := filepath.Join(opts.Env.Home, ".local", "bin")
	t.Setenv("PATH", "/usr/bin:/bin")
	if _, err := installNode(context.Background(), opts, archive); err != nil {
		t.Fatal(err)
	}
	if !pathHas(os.Getenv("PATH"), bin) {
		t.Fatalf("PATH is %q, without the directory node was just installed into (%s)",
			os.Getenv("PATH"), bin)
	}
	// And exactly once, however many times it runs.
	before := os.Getenv("PATH")
	if _, err := installNode(context.Background(), opts, archive); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("PATH") != before {
		t.Errorf("PATH grew a duplicate entry: %q", os.Getenv("PATH"))
	}
}

// The case a v0.2.0 machine is in: Relay downloaded Node on a previous run, and
// it is on disk, correct, and invisible — because nothing puts ~/.local/bin on
// PATH and setup is invoked by absolute path. Re-downloading 50MB to arrive at
// the same file is the wrong answer to "is there a usable Node".
func TestAPreviouslyInstalledNodeIsAdoptedRatherThanRefetched(t *testing.T) {
	archive := nodeArchive("darwin", "arm64")
	root := strings.TrimSuffix(archive, ".tar.gz")

	var fetches int
	counting := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		fetches++
		return nodeServer(t, archive, fakeNodeTar(t, root), "").Transport.RoundTrip(r)
	})}
	// The real executor, because this test is about whether a symlinked node
	// actually answers — which a FakeExec cannot tell us, it having never run
	// anything. The tarball's bin/node is a shell script that prints a version.
	opts := nodeOpts(t, counting)
	opts.Env.Exec = detect.OS().Exec

	// First run: nothing here, so it downloads.
	t.Setenv("PATH", "/usr/bin:/bin")
	if _, err := installNode(context.Background(), opts, archive); err != nil {
		t.Fatal(err)
	}
	after := fetches
	if after == 0 {
		t.Fatal("the first run fetched nothing")
	}

	// Second run, in a process that cannot see it: adopted, not refetched.
	t.Setenv("PATH", "/usr/bin:/bin")
	if !adoptLocalNode(context.Background(), opts) {
		t.Fatal("a Node already on disk was not adopted")
	}
	if fetches != after {
		t.Errorf("made %d more requests adopting a Node that was already here", fetches-after)
	}
	if !pathHas(os.Getenv("PATH"), filepath.Join(opts.Env.Home, ".local", "bin")) {
		t.Error("adopting it did not make it visible")
	}
}

// Where npm puts what it installs.
//
// `npm prefix -g` under Relay's own Node answers the distribution directory —
// ~/.local/node-<version>-<os>-<arch> — not ~/.local/bin. So `npm install -g
// @anthropic-ai/claude-code` lands a `claude` binary in a directory nothing on
// the machine has ever looked in. Relay installed Claude Code, Codex and
// OpenClaw exactly that way and then reported all three as missing on the next
// run, which is how the first person to try it found out.
func TestNpmsGlobalBinEndsUpOnPath(t *testing.T) {
	archive := nodeArchive("darwin", "arm64")
	root := strings.TrimSuffix(archive, ".tar.gz")
	opts := nodeOpts(t, nodeServer(t, archive, fakeNodeTar(t, root), ""))

	t.Setenv("PATH", "/usr/bin:/bin")
	dir, err := installNode(context.Background(), opts, archive)
	if err != nil {
		t.Fatal(err)
	}
	path := os.Getenv("PATH")
	for _, want := range []string{
		filepath.Join(opts.Env.Home, ".local", "bin"), // node, npm, npx
		filepath.Join(dir, "bin"),                     // everything npm -g installs
	} {
		if !pathHas(path, want) {
			t.Errorf("PATH is missing %s\ngot: %s", want, path)
		}
	}
}

// An agent runtime Relay installs has to be reachable by the user's own shell,
// not just by Relay. install.sh warns about exactly one directory, so that is
// where everything npm installs globally has to end up.
func TestGlobalsAreLinkedIntoTheDirectoryTheUserWasToldAbout(t *testing.T) {
	archive := nodeArchive("darwin", "arm64")
	root := strings.TrimSuffix(archive, ".tar.gz")
	opts := nodeOpts(t, nodeServer(t, archive, fakeNodeTar(t, root), ""))

	dir, err := installNode(context.Background(), opts, archive)
	if err != nil {
		t.Fatal(err)
	}
	// npm installing @anthropic-ai/claude-code puts `claude` here.
	if err := os.WriteFile(filepath.Join(dir, "bin", "claude"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	linkGlobals(opts)

	link := filepath.Join(opts.Env.Home, ".local", "bin", "claude")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("claude was installed and never linked where anyone would find it: %v", err)
	}
	// Compare resolved paths: on macOS the temp dir arrives as /var/... and comes
	// back as /private/var/....
	want, _ := filepath.EvalSymlinks(filepath.Join(dir, "bin"))
	if got, _ := filepath.EvalSymlinks(filepath.Dir(target)); got != want {
		t.Errorf("claude points at %s, want it inside %s", target, want)
	}
}

// A user's own Node is theirs. Shadowing their globals in ~/.local/bin would be
// both rude and confusing.
func TestAUsersOwnNodeIsLeftAlone(t *testing.T) {
	home := t.TempDir()
	elsewhere := t.TempDir() // stands in for nvm or Homebrew
	if err := os.MkdirAll(filepath.Join(elsewhere, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"node", "claude"} {
		if err := os.WriteFile(filepath.Join(elsewhere, "bin", n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, "bin", "node"), filepath.Join(bin, "node")); err != nil {
		t.Fatal(err)
	}

	linkGlobals(Options{Env: detect.Env{Home: home}})

	if _, err := os.Lstat(filepath.Join(bin, "claude")); err == nil {
		t.Error("linked a binary out of a Node that Relay does not own")
	}
}

// The second run must not disown the first.
//
// From a real machine: Claude Code was installed on run one and reported "not
// installed" on run two, because detection happens before anything puts either
// of Relay's own directories on PATH.
func TestASecondRunFindsWhatTheFirstOneInstalled(t *testing.T) {
	archive := nodeArchive("darwin", "arm64")
	root := strings.TrimSuffix(archive, ".tar.gz")
	opts := nodeOpts(t, nodeServer(t, archive, fakeNodeTar(t, root), ""))

	dir, err := installNode(context.Background(), opts, archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A fresh process: neither directory is on PATH, as in a shell that has
	// never sourced anything Relay wrote.
	t.Setenv("PATH", "/usr/bin:/bin")

	restorePath(opts)

	if _, err := exec.LookPath("claude"); err != nil {
		t.Errorf("a runtime installed by an earlier run is still invisible: %v", err)
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Errorf("Relay's own Node is still invisible: %v", err)
	}
}

// And it stays quiet about it: the detection report immediately after is where
// the user reads what is on the machine.
func TestRestoringThePathSaysNothing(t *testing.T) {
	opts := nodeOpts(t, nil)
	if err := os.MkdirAll(filepath.Join(opts.Env.Home, ".local", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	restorePath(opts)
	if out := opts.Prompt.(*Script).Output(); strings.TrimSpace(out) != "" {
		t.Errorf("restorePath printed:\n%s", out)
	}
}
