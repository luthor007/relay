package install

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Putting Node on a machine that has none.
//
// This exists because of one question with an embarrassing answer: does
// `curl -fsSL https://relay.glass/install | sh` work on a wiped Mac mini? It did
// not, and the reason was four links down a chain nobody had walked. Every agent
// runtime installs with `npm install -g`; npm comes with Node; macOS ships no
// Node; and the only way this installer knew to get one was Homebrew, which a
// clean machine also does not have. So the honest result on a fresh box was a
// Relay that talks and thinks and has no agents at all — which is the product.
//
// The fix is for Relay to fetch Node itself, and it is deliberately the same
// shape as install.sh fetching Relay:
//
//   - the official build from nodejs.org, at a pinned version;
//   - checked against their published SHASUMS256.txt, and refused on mismatch —
//     an unverified language runtime is a worse thing to install than an
//     unverified anything else, because everything downstream runs inside it;
//   - unpacked under the user's own directory, no sudo;
//   - symlinked into ~/.local/bin, which install.sh already puts on PATH and
//     already warns about — **nothing edits a shell profile**, because
//     install.sh promises in as many words that it does not.
//
// It is offered, never assumed, and the question defaults to no like every
// other install here.

// NodePin is the Node this installs.
//
// Pinned rather than "latest" for the reason BusPin is: a version that moves on
// its own is a dependency that breaks on someone else's schedule. 24.19.0 is
// inside OpenClaw's `>=24.15.0 <25` window and is an LTS line, so it satisfies
// the bus without being the newest thing available — which is the correct
// posture for a runtime the user did not ask for and will not think about again.
const NodePin = "v24.19.0"

// NodeDist is where official builds live. Overridable for a mirror, and for the
// test that must not reach the network.
var NodeDist = "https://nodejs.org/dist"

// nodeArchive is the tarball name for this machine, and the "" that means we do
// not ship one for it.
func nodeArchive(goos, goarch string) string {
	var os_, arch string
	switch goos {
	case "darwin":
		os_ = "darwin"
	case "linux":
		os_ = "linux"
	default:
		return ""
	}
	switch goarch {
	case "amd64":
		arch = "x64"
	case "arm64":
		arch = "arm64"
	default:
		return ""
	}
	return fmt.Sprintf("node-%s-%s-%s.tar.gz", NodePin, os_, arch)
}

// ensureNode makes a usable Node available, and reports whether one is.
//
// why is the step asking — "the agent runtimes" or "the bus" — because a
// question about installing a language runtime is much easier to answer when it
// says what wanted it.
func ensureNode(ctx context.Context, opts Options, why string) (bool, error) {
	if v, ok := nodeVersion(ctx, opts); ok && nodeOK(v) {
		return true, nil
	}
	// A Node this installer put here on an earlier run is still here, and is
	// invisible for exactly the reason it was invisible the first time: nothing
	// puts ~/.local/bin on PATH, and setup is usually invoked by absolute path.
	// Downloading 50MB again to arrive at the same file would be the wrong
	// answer to "is there a usable Node", so look before fetching.
	if adoptLocalNode(ctx, opts) {
		return true, nil
	}
	// Asked once per run. A second step finding Node still missing means the
	// user already declined, and asking again is nagging.
	if opts.nodeAsked() {
		return false, nil
	}
	opts.markNodeAsked()

	p := opts.Prompt
	archive := nodeArchive(runtime.GOOS, runtime.GOARCH)
	if archive == "" {
		p.Say("  %s needs Node, and Relay has no build for %s/%s. %s",
			why, runtime.GOOS, runtime.GOARCH, nodeAdvice)
		return false, nil
	}

	yes, err := p.Confirm(Confirm{
		ID:     "node.install",
		Prompt: "Install Node?",
		Body: fmt.Sprintf("%s needs it, and this machine has none. Relay downloads %s from "+
			"nodejs.org, checks it against their published checksums, and unpacks it into "+
			"~/.local — no sudo, and nothing touches your shell profile.", why, NodePin),
		Default: false,
	})
	if err != nil {
		// A prompt error is the scripted run meeting a question nobody decided
		// an answer for, and this package's whole discipline is that such a run
		// fails rather than taking a default. Swallowing it here would make
		// adding a question to the flow invisible to every test.
		return false, err
	}
	if !yes {
		p.Say("  %s", wrapIndent(nodeAdvice, 2, 76))
		return false, nil
	}

	dir, err := installNode(ctx, opts, archive)
	if err != nil {
		p.Say("  Node did not install: %s", err.Error())
		return false, nil
	}
	p.Say("  Node %s in %s.", NodePin, shortSource(dir, opts.Env.Home))

	// Re-check through the same seam everything else uses, rather than trust
	// the unpack. A Node that is on disk and not on PATH is the failure this
	// whole function exists to avoid, and it looks identical to success.
	if v, ok := nodeVersion(ctx, opts); !ok || !nodeOK(v) {
		p.Say("  Installed, but `node` still does not answer. Add %s to your PATH.",
			filepath.Join(opts.Env.Home, ".local", "bin"))
		return false, nil
	}
	return true, nil
}

// adoptLocalNode reports whether a Node this installer previously unpacked is
// on disk, and if so makes it visible to this process.
func adoptLocalNode(ctx context.Context, opts Options) bool {
	home := opts.Env.Home
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return false
		}
	}
	bin := filepath.Join(home, ".local", "bin")
	if _, err := os.Stat(filepath.Join(bin, "node")); err != nil {
		return false
	}
	path := os.Getenv("PATH")
	if pathHas(path, bin) {
		// Already on PATH and still not answering, so it is broken rather than
		// hidden — and re-adding the same directory would not change that.
		return false
	}
	_ = os.Setenv("PATH", bin+string(os.PathListSeparator)+path)

	v, ok := nodeVersion(ctx, opts)
	if !ok || !nodeOK(v) {
		return false
	}
	opts.Prompt.Say("  Node %s, already installed here by Relay.", v)
	return true
}

// installNode downloads, verifies and unpacks. It returns where Node landed.
func installNode(ctx context.Context, opts Options, archive string) (string, error) {
	want, err := nodeChecksum(ctx, opts, archive)
	if err != nil {
		return "", err
	}

	url := NodeDist + "/" + NodePin + "/" + archive
	opts.Prompt.Say("  Downloading %s", url)
	body, err := httpGet(ctx, opts, url)
	if err != nil {
		return "", err
	}
	defer body.Close()

	// Hash while unpacking rather than buffering ~50MB, and refuse to leave the
	// result in place if the sum is wrong.
	sum := sha256.New()
	home := opts.Env.Home
	if home == "" {
		if home, err = os.UserHomeDir(); err != nil {
			return "", err
		}
	}
	local := filepath.Join(home, ".local")
	staging := filepath.Join(local, ".relay-node-staging")
	_ = os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)

	root, err := untarGz(io.TeeReader(body, sum), staging)
	if err != nil {
		return "", err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != want {
		return "", fmt.Errorf("checksum mismatch: nodejs.org published %s, the download hashed to %s",
			want, got)
	}

	final := filepath.Join(local, strings.TrimSuffix(archive, ".tar.gz"))
	_ = os.RemoveAll(final)
	if err := os.Rename(filepath.Join(staging, root), final); err != nil {
		return "", err
	}

	// The symlinks are the whole point: ~/.local/bin is where install.sh already
	// put relay, so it is already on PATH or already warned about.
	bin := filepath.Join(local, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return "", err
	}
	for _, name := range []string{"node", "npm", "npx"} {
		link := filepath.Join(bin, name)
		_ = os.Remove(link)
		if err := os.Symlink(filepath.Join(final, "bin", name), link); err != nil {
			return "", fmt.Errorf("could not link %s: %w", name, err)
		}
	}

	// And make it visible to THIS process, which is the whole difference
	// between installing Node and being able to use it.
	//
	// install.sh runs `$INSTALL_DIR/relay setup` by absolute path, and warns
	// two lines earlier that $INSTALL_DIR is not on PATH — which on a fresh Mac
	// mini it is not. So setup would symlink node into ~/.local/bin, look for
	// `node` on a PATH that does not contain it, and report "installed, but
	// node still does not answer" about a Node it had just installed
	// successfully. Every step after this one — npm for the runtimes, npm for
	// OpenClaw, openclaw itself — resolves through the same PATH.
	//
	// Scoped to this process and its children, which is exactly the blast
	// radius wanted: nothing is written to a shell profile, and the next login
	// is unaffected.
	if path := os.Getenv("PATH"); !pathHas(path, bin) {
		_ = os.Setenv("PATH", bin+string(os.PathListSeparator)+path)
	}
	return final, nil
}

// pathHas reports whether dir is already an entry in a PATH value.
func pathHas(path, dir string) bool {
	for _, p := range filepath.SplitList(path) {
		if p == dir {
			return true
		}
	}
	return false
}

// nodeChecksum reads the published sum for one archive.
//
// A missing or unreadable SHASUMS256.txt is fatal rather than a warning. This is
// the one place where "install it anyway" is clearly wrong: everything Relay
// then installs — every agent runtime, the bus — executes inside whatever this
// unpacks.
func nodeChecksum(ctx context.Context, opts Options, archive string) (string, error) {
	body, err := httpGet(ctx, opts, NodeDist+"/"+NodePin+"/SHASUMS256.txt")
	if err != nil {
		return "", fmt.Errorf("could not fetch checksums, so nothing was installed: %w", err)
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == archive {
			return f[0], nil
		}
	}
	return "", fmt.Errorf("%s is not listed in nodejs.org's checksums", archive)
}

func httpGet(ctx context.Context, opts Options, url string) (io.ReadCloser, error) {
	client := opts.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%s answered %d", url, resp.StatusCode)
	}
	return resp.Body, nil
}

// untarGz unpacks into dir and returns the archive's single root directory.
func untarGz(r io.Reader, dir string) (string, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return "", err
	}
	defer gz.Close()

	root := ""
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		// An entry that escapes the destination is how a tarball writes to
		// somewhere it was never given. Refuse rather than sanitise: a Node
		// tarball has no reason to contain one, so a path like this means the
		// download is not what it claims to be.
		clean := filepath.Clean(h.Name)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return "", fmt.Errorf("refusing an archive entry that escapes its directory: %q", h.Name)
		}
		if root == "" {
			root = strings.SplitN(clean, string(filepath.Separator), 2)[0]
		}
		target := filepath.Join(dir, clean)

		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)&0o777)
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return "", err
			}
			if err := f.Close(); err != nil {
				return "", err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}
			_ = os.Remove(target)
			if err := os.Symlink(h.Linkname, target); err != nil {
				return "", err
			}
		}
	}
	if root == "" {
		return "", fmt.Errorf("the archive was empty")
	}
	return root, nil
}
