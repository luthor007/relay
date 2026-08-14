package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Installing Ollama, on a machine that has no package manager.
//
// The embedding step used to end here on a stock Mac:
//
//	Ollama is not installed here and Relay has no install command it can run
//	on this machine. Install it from https://ollama.com/download and run
//	`relay embed`.
//
// True as written — the only macOS command Relay had was `brew install ollama`,
// and a clean Mac mini has no Homebrew — but it is the wrong end of the
// sentence. Relay already downloads and verifies Node from nodejs.org on
// exactly such a machine, and Ollama publishes a plain tarball next to the .dmg
// with a sha256sum.txt beside it. Same shape, same rules: pinned version,
// checksums fetched from the vendor, unpacked under ~/.local, no sudo, nothing
// written to a shell profile.
//
// Linux keeps the vendor's install.sh. Their Linux archives are .tar.zst, which
// the standard library cannot read, and their script is the documented path.

const (
	// OllamaPin is the version this installer downloads. Pinned rather than
	// "latest" for the reason NodePin is: the checksum file has to describe the
	// bytes that arrive, and "latest" moves between the two requests.
	OllamaPin = "v0.32.9"
	// OllamaRelease is where the pinned build and its checksums live.
	OllamaRelease = "https://github.com/ollama/ollama/releases/download"
	// OllamaDownloadMB is what the question says it will cost. Measured, not
	// estimated: 141 MB on 2026-08-14.
	OllamaDownloadMB = 141
)

// ollamaArchive is the asset name for a platform, or "" when Relay has no
// download for it. One universal archive covers both Mac architectures, so the
// question is only which operating system this is.
func ollamaArchive(goos string) string {
	if goos == "darwin" {
		return "ollama-darwin.tgz"
	}
	return ""
}

// InstallPlan is what Install will do, in the words the user agrees to before
// it does it.
type InstallPlan struct {
	// Cmd is somebody else's install command, run as-is and shown in full.
	// Empty when Relay does the download itself.
	Cmd []string
	// Body is the sentence under the question.
	Body string
	// OK is false when Relay has no way to install this here, which is a thing
	// it says rather than a thing it guesses around.
	OK bool
}

// installOllama downloads the pinned build, checks it against the vendor's own
// checksums, and unpacks it under ~/.local. It returns the directory.
func installOllama(ctx context.Context, opts Options, archive string) (string, error) {
	want, err := ollamaChecksum(ctx, opts, archive)
	if err != nil {
		return "", err
	}

	url := OllamaRelease + "/" + OllamaPin + "/" + archive
	opts.Prompt.Say("  Downloading %s", url)
	body, err := httpGet(ctx, opts, url)
	if err != nil {
		return "", err
	}
	defer body.Close()

	home := opts.Env.Home
	if home == "" {
		if home, err = os.UserHomeDir(); err != nil {
			return "", err
		}
	}
	local := filepath.Join(home, ".local")
	staging := filepath.Join(local, ".relay-ollama-staging")
	_ = os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)

	// Hash while unpacking rather than buffering 141MB. The archive is flat —
	// the binary and its libraries sit next to each other with no root
	// directory — so the staging directory itself is what gets moved.
	sum := sha256.New()
	if _, err := untarGz(io.TeeReader(body, sum), staging); err != nil {
		return "", err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != want {
		return "", fmt.Errorf("checksum mismatch: ollama published %s, the download hashed to %s",
			want, got)
	}
	if _, err := os.Stat(filepath.Join(staging, "ollama")); err != nil {
		return "", fmt.Errorf("the archive verified but has no ollama binary in it")
	}

	final := filepath.Join(local, "ollama-"+OllamaPin)
	_ = os.RemoveAll(final)
	if err := os.Rename(staging, final); err != nil {
		return "", err
	}

	// The binary finds its own libraries through the directory it really lives
	// in, so a link into ~/.local/bin is enough — measured on a real unpack
	// rather than assumed, because a wrong answer here is a dyld error nobody
	// can read.
	bin := filepath.Join(local, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return "", err
	}
	link := filepath.Join(bin, "ollama")
	_ = os.Remove(link)
	if err := os.Symlink(filepath.Join(final, "ollama"), link); err != nil {
		return "", err
	}
	addToPath(bin)
	return final, nil
}

// ollamaChecksum reads the sum for one asset out of the release's own
// sha256sum.txt, which lists names as "./ollama-darwin.tgz".
func ollamaChecksum(ctx context.Context, opts Options, archive string) (string, error) {
	body, err := httpGet(ctx, opts, OllamaRelease+"/"+OllamaPin+"/sha256sum.txt")
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
		if len(f) == 2 && strings.TrimPrefix(f[1], "./") == archive {
			return f[0], nil
		}
	}
	return "", fmt.Errorf("%s is not listed in ollama's checksums for %s", archive, OllamaPin)
}

// ollamaServeLabel is the LaunchAgent for a server Relay installed.
const ollamaServeLabel = "glass.relay.ollama"

// serveOllama starts the server and keeps it started.
//
// The rule this looks like it breaks — Relay does not start somebody else's
// daemon — is about a daemon somebody else installed, where the unit name is a
// guess and a wrong `systemctl start` is worse than a paragraph. This is the
// binary Relay just unpacked, at a path Relay chose, so there is nothing to
// guess: without this the download would finish and leave a server that has
// never run and does not come back after a reboot, which is a worse answer than
// the paragraph was.
func serveOllama(ctx context.Context, opts Options, dir string) error {
	if opts.Env.GOOS != "darwin" {
		// Their install.sh registers a systemd unit itself.
		return nil
	}
	agents := filepath.Join(opts.Env.Home, "Library", "LaunchAgents")
	logs := filepath.Join(opts.Env.Home, "Library", "Logs", "Relay")
	if err := opts.FS.MkdirAll(agents, 0o755); err != nil {
		return err
	}
	if err := opts.FS.MkdirAll(logs, 0o755); err != nil {
		return err
	}
	path := filepath.Join(agents, ollamaServeLabel+".plist")
	if err := opts.FS.WriteFile(path, []byte(ollamaPlist(dir, logs)), 0o644); err != nil {
		return err
	}

	target := fmt.Sprintf("gui/%d", opts.UID)
	var out ServiceOutcome
	runQuiet(ctx, opts, &out, "launchctl", "bootout", target+"/"+ollamaServeLabel)
	if err := run(ctx, opts, &out, "launchctl", "bootstrap", target, path); err != nil {
		if err2 := run(ctx, opts, &out, "launchctl", "load", "-w", path); err2 != nil {
			return fmt.Errorf("the LaunchAgent is written to %s but launchctl would not load it: %w",
				path, err)
		}
	}
	return nil
}

func ollamaPlist(dir, logDir string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + ollamaServeLabel + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + xmlEscape(filepath.Join(dir, "ollama")) + `</string>
    <string>serve</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>` + xmlEscape(filepath.Join(logDir, "ollama.log")) + `</string>
  <key>StandardErrorPath</key><string>` + xmlEscape(filepath.Join(logDir, "ollama.log")) + `</string>
</dict>
</plist>
`
}

// waitForOllama polls until the server answers, so the pull that follows meets a
// running server rather than a connection refused.
func waitForOllama(ctx context.Context, rt EmbedRuntime, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if rt.Status(ctx).Running {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(250 * time.Millisecond):
		}
	}
}
