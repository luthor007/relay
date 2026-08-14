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

// Installing Ollama on a Mac with no Homebrew.
//
// What a clean Mac mini got instead, having just chosen the local embedder:
//
//	Ollama is not installed here and Relay has no install command it can run
//	on this machine. Install it from https://ollama.com/download.
//
// which was true of the code and not of the machine: the vendor publishes a
// tarball and a sha256sum.txt beside the .dmg, which is the same deal Relay
// already takes from nodejs.org for Node.

// fakeOllamaTar is the shape the real archive has: flat, no root directory, the
// binary next to the libraries it loads.
func fakeOllamaTar(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		mode int64
		body string
	}{
		{"ollama", 0o755, "#!/bin/sh\necho ollama\n"},
		{"libggml-base.dylib", 0o644, "not really a library"},
	} {
		if err := tw.WriteHeader(&tar.Header{
			Name: f.name, Mode: f.mode, Size: int64(len(f.body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// ollamaServer serves the release the way GitHub does, with the checksum file
// naming assets as "./ollama-darwin.tgz".
func ollamaServer(t *testing.T, tarball []byte, sum string) *http.Client {
	t.Helper()
	if sum == "" {
		h := sha256.Sum256(tarball)
		sum = hex.EncodeToString(h[:])
	}
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "sha256sum.txt"):
			body := fmt.Sprintf("%s  ./ollama-darwin.tgz\n%s  ./Ollama.dmg\n", sum, sum)
			return &http.Response{StatusCode: 200, Header: http.Header{},
				Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		case strings.HasSuffix(r.URL.Path, "ollama-darwin.tgz"):
			return &http.Response{StatusCode: 200, Header: http.Header{},
				Body: io.NopCloser(bytes.NewReader(tarball)), Request: r}, nil
		}
		return &http.Response{StatusCode: 404, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}
}

func TestOllamaIsDownloadedAndLinkedWhereItCanBeFound(t *testing.T) {
	home := t.TempDir()
	opts := Options{
		Prompt: NewScript(map[string]string{}), HTTPClient: ollamaServer(t, fakeOllamaTar(t), ""),
		Env: detect.Env{Home: home, GOOS: "darwin", Exec: &detect.FakeExec{}, FS: &detect.MemFS{}},
	}.withDefaults()

	dir, err := installOllama(context.Background(), opts, "ollama-darwin.tgz")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ollama")); err != nil {
		t.Fatalf("the binary is not where installOllama says it is: %v", err)
	}
	// The libraries have to land beside it, or the binary starts and cannot
	// load them — the failure that made a flat archive worth testing.
	if _, err := os.Stat(filepath.Join(dir, "libggml-base.dylib")); err != nil {
		t.Errorf("the archive's libraries did not come with it: %v", err)
	}
	target, err := os.Readlink(filepath.Join(home, ".local", "bin", "ollama"))
	if err != nil {
		t.Fatalf("nothing linked ollama where the user's shell looks: %v", err)
	}
	if target != filepath.Join(dir, "ollama") {
		t.Errorf("link points at %s, want the binary in %s", target, dir)
	}
}

// A download whose bytes do not match the vendor's own checksum is not
// installed, and does not leave half of itself behind.
func TestAnOllamaDownloadThatDoesNotMatchIsNotInstalled(t *testing.T) {
	home := t.TempDir()
	opts := Options{
		Prompt: NewScript(map[string]string{}),
		// A sum for different bytes than the ones served.
		HTTPClient: ollamaServer(t, fakeOllamaTar(t), strings.Repeat("a", 64)),
		Env:        detect.Env{Home: home, GOOS: "darwin", Exec: &detect.FakeExec{}, FS: &detect.MemFS{}},
	}.withDefaults()

	if _, err := installOllama(context.Background(), opts, "ollama-darwin.tgz"); err == nil {
		t.Fatal("a mismatched download was installed")
	}
	for _, path := range []string{
		filepath.Join(home, ".local", "ollama-"+OllamaPin),
		filepath.Join(home, ".local", ".relay-ollama-staging"),
		filepath.Join(home, ".local", "bin", "ollama"),
	} {
		if _, err := os.Lstat(path); err == nil {
			t.Errorf("%s was left behind", path)
		}
	}
}

// The server has to be running for the pull that follows, and running again
// after a reboot — otherwise the install succeeds and search is keyword-only
// anyway, which is the state this whole step exists to leave behind.
func TestAnOllamaRelayInstalledIsStartedAndComesBack(t *testing.T) {
	home := t.TempDir()
	plistPath := filepath.Join(home, "Library", "LaunchAgents", ollamaServeLabel+".plist")
	ex := &detect.FakeExec{
		Paths: map[string]string{"launchctl": "/bin/launchctl"},
		Responses: map[string]detect.Result{
			detect.Key("launchctl", "bootstrap", "gui/501", plistPath): {},
		},
	}
	fs := &detect.MemFS{}
	opts := Options{
		Prompt: NewScript(map[string]string{}), FS: fs, UID: 501,
		Env: detect.Env{Home: home, GOOS: "darwin", Exec: ex, FS: fs},
	}.withDefaults()

	if err := serveOllama(context.Background(), opts, filepath.Join(home, ".local", "ollama")); err != nil {
		t.Fatal(err)
	}

	plist := fs.Files[plistPath]
	if plist == "" {
		t.Fatal("no LaunchAgent was written, so nothing starts the server at login")
	}
	for _, want := range []string{"<string>serve</string>", "<key>KeepAlive</key>", "<key>RunAtLoad</key>"} {
		if !strings.Contains(plist, want) {
			t.Errorf("the agent does not %s:\n%s", want, plist)
		}
	}
	var loaded bool
	for _, c := range ex.Calls {
		if c.Name == "launchctl" && len(c.Args) > 0 && (c.Args[0] == "bootstrap" || c.Args[0] == "load") {
			loaded = true
		}
	}
	if !loaded {
		t.Errorf("the agent was written and never loaded, so it starts at the next reboot "+
			"and not now: %+v", ex.Calls)
	}
}
