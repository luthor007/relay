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
