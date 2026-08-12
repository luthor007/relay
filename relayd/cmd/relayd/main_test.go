package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/api"
	"github.com/luthor007/relay/relayd/internal/vault"
)

// TestMain keeps this package's tests off the machine's real OS keychain.
//
// [vault.MemoryKeyring] with FailAll is exactly "a machine with no keychain",
// which is the environment the container these tests were written in provided
// for free and macOS does not. Every vault opened here — by the daemon under
// test and by the assertions that read it back — therefore takes the file
// backend, keyed by the 0600 vault.key beside the database. That is what lets a
// test open the daemon's own vault and reveal what it stored: both sides derive
// the same key from the same file, in the same t.TempDir, with no shared
// process state and nothing left behind on the developer's machine.
func TestMain(m *testing.M) {
	vaultOpen = func(ctx context.Context, opts vault.Options) (vault.Vault, error) {
		opts.Keyring = &vault.MemoryKeyring{FailAll: true}
		return vault.Open(ctx, opts)
	}
	os.Exit(m.Run())
}

func TestVersionFlagExitsCleanly(t *testing.T) {
	if err := run(context.Background(), []string{"--version"}, nil); err != nil {
		t.Fatal(err)
	}
}

// DASHBOARD.md §4: exposure is a deliberate flag with a warning, not something
// a config file can do quietly. relayd refuses rather than obeys.
func TestRefusesToExposeWithoutTheFlag(t *testing.T) {
	dir := t.TempDir()
	err := run(context.Background(), []string{
		"--listen", "0.0.0.0:0", "--data-dir", dir, "--config", filepath.Join(dir, "none.toml"),
	}, nil)
	if !errors.Is(err, api.ErrExposed) {
		t.Fatalf("err = %v, want ErrExposed", err)
	}
}

func TestBadQuietHoursFailsFast(t *testing.T) {
	dir := t.TempDir()
	err := run(context.Background(), []string{
		"--listen", "127.0.0.1:0", "--data-dir", dir,
		"--config", filepath.Join(dir, "none.toml"), "--quiet-hours", "10pm-7am",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "quiet hours") {
		t.Fatalf("err = %v", err)
	}
}

// The whole daemon, end to end: config, store, registry, bus, ping policy, API,
// listener, and a graceful shutdown. No agent runtime is installed in a build
// container, so the honest expectation is that relayd starts anyway and says it
// found none.
func TestDaemonStartsServesAndStopsCleanly(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addrs := make(chan net.Addr, 1)
	errc := make(chan error, 1)
	go func() {
		errc <- run(ctx, []string{
			"--listen", "127.0.0.1:0",
			"--data-dir", dir,
			"--config", filepath.Join(dir, "none.toml"),
			"--token", "test-token",
			"--log-level", "error",
		}, func(a net.Addr) { addrs <- a })
	}()

	var addr net.Addr
	select {
	case addr = <-addrs:
	case err := <-errc:
		t.Fatalf("relayd exited before serving: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("relayd never came up")
	}

	base := "http://" + addr.String()

	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("/healthz = %d", resp.StatusCode)
	}

	// The token is required, and it is the one that was passed in.
	resp, err = http.Get(base + "/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /v1/sessions = %d", resp.StatusCode)
	}

	req, _ := http.NewRequest("GET", base+"/v1/health", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var h api.Health
	if err := json.Unmarshal(body, &h); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	if !h.OK || len(h.Runtimes) != 5 {
		t.Fatalf("health = %+v", h)
	}
	// relayd must start whatever is installed rather than refusing: a machine
	// with two of the five is the normal case (ORCHESTRATOR.md §1), and health
	// says which, which is the whole point of DASHBOARD.md §3.5.
	//
	// The assertion is the invariant, not the inventory. It used to read "only
	// claude-code may claim an adapter", which is true of a build container and
	// of nowhere else — on the author's Mac all five are installed and this
	// failed on codex, reporting a defect that was not there. What must hold on
	// every machine is the other direction: relayd may claim it can drive a
	// runtime only where that runtime's binary actually exists.
	// Gated on Detected, because Installed is a tri-state and this struct is
	// emphatic about it: with no detection pass finished, Installed is *unknown*
	// rather than false, and asserting on it anyway is the same "empty history
	// reported as success" mistake MEMORY.md §1 names.
	for _, rt := range h.Runtimes {
		if !rt.Detected {
			continue
		}
		if rt.Adapter && !rt.Installed {
			t.Fatalf("%s claims an adapter but detection found no binary", rt.Runtime)
		}
		if len(rt.Missing) == 0 && !rt.Installed {
			t.Fatalf("%s is not installed yet reports no capability gaps at all", rt.Runtime)
		}
	}

	// The store landed where it was told to. SYSTEM.md §8: one file, and backup
	// is a file copy.
	if _, err := os.Stat(filepath.Join(dir, "relay.db")); err != nil {
		t.Fatalf("no database at %s: %v", dir, err)
	}

	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("shutdown returned %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("relayd did not shut down")
	}
}
