package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/config"
)

// DASHBOARD.md §4: the console can write to the vault, which makes it the
// highest-value target in the system. Loopback by default is a decision, and
// widening it should be a visible one.
func TestDefaultsBindToLoopback(t *testing.T) {
	c := config.Default()
	if c.Listen != "127.0.0.1:8787" {
		t.Fatalf("default listen is %q, want loopback", c.Listen)
	}
	if strings.HasPrefix(c.Listen, "0.0.0.0") {
		t.Fatal("relayd must not default to every interface")
	}
	// The voice fallback is never empty: mute out of the box is the worst
	// possible first hour for a voice product.
	if c.Voice.Fallback == "" {
		t.Fatal("there must always be a keyless voice fallback")
	}
	if c.Models.Small.Model == "" || c.Models.Big.Model == "" {
		t.Fatal("both models need a default")
	}
	if c.Models.Small.Vendor != "openrouter" || c.Models.Big.Vendor != "openrouter" {
		t.Fatal("OpenRouter is the recommended provider for both")
	}
}

func TestLoadMissingFileIsDefaults(t *testing.T) {
	c, err := config.Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatalf("a missing config must not be an error: %v", err)
	}
	if c.Listen != config.DefaultListen {
		t.Fatalf("listen is %q", c.Listen)
	}
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	want := config.Default()
	want.Listen = "127.0.0.1:9999"
	want.Log.Level = "debug"
	want.Models.Small.Credential = "env:OPENROUTER_API_KEY"
	want.Models.Big.Credential = "exec:op read op://Private/OpenRouter/credential"
	want.Voice.Credential = "vault:simba-1"
	want.Runtimes = map[string]config.RuntimeConfig{
		"openclaw": {Enabled: true, Command: "openclaw", StateDir: "/home/u/.openclaw-dev"},
	}

	if err := config.Save(path, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config file is %o, want 0600", perm)
	}

	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Listen != want.Listen || got.Log.Level != "debug" {
		t.Fatalf("round trip: %+v", got)
	}
	if got.Models.Small.Credential != want.Models.Small.Credential {
		t.Fatalf("credential reference: %q", got.Models.Small.Credential)
	}
	// Never hardcode ~/.openclaw: the state directory relocates, and a reader
	// that assumes the default silently reports an empty history as success.
	if got.Runtimes["openclaw"].StateDir != "/home/u/.openclaw-dev" {
		t.Fatalf("runtime state dir: %+v", got.Runtimes)
	}
}

// A pasted secret in a config file ends up in a backup, a screenshot and a
// support ticket. References only.
//
// The check is necessarily partial: a bare "sk-or-v1-abc…" with no colon is
// syntactically indistinguishable from an env var name, and treating every
// bare string as suspect would reject the thing people most often type. What
// is catchable is a value that carries a colon and does not start with a
// reference prefix, which is what a pasted key usually looks like.
func TestPastedSecretWithAColonIsRefused(t *testing.T) {
	c := config.Default()
	c.Voice.Credential = "sk-proj:abcdef"
	err := c.Validate()
	if err == nil {
		t.Fatal("a colon-bearing value with an unknown prefix is almost certainly a pasted secret")
	}
	if !strings.Contains(err.Error(), "voice.credential") {
		t.Fatalf("the error should name the field: %v", err)
	}
	if !strings.Contains(err.Error(), "env:") {
		t.Fatalf("the error should say what to use instead: %v", err)
	}
}

func TestValidReferencesPass(t *testing.T) {
	c := config.Default()
	c.Models.Small.Credential = "env:OPENROUTER_API_KEY"
	c.Models.Big.Credential = "file:~/.config/relay/openrouter.key"
	c.Voice.Credential = "exec:op read op://Private/Simba/credential"
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	// A bare string is read as an env var name, which is what people type.
	c.Models.Small.Credential = "OPENROUTER_API_KEY"
	if err := c.Validate(); err != nil {
		t.Fatalf("bare env var name: %v", err)
	}
}

func TestCustomProviderNeedsABaseURL(t *testing.T) {
	c := config.Default()
	c.Models.Big.Vendor = "custom"
	if err := c.Validate(); err == nil {
		t.Fatal("a custom provider with no base_url must be caught at load, not at first speech")
	}
	c.Models.Big.BaseURL = "http://localhost:11434/v1"
	if err := c.Validate(); err != nil {
		t.Fatalf("with a base URL: %v", err)
	}
}

func TestPathsFollowTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, filepath.Join(dir, "cfg"))
	t.Setenv(config.EnvDataDir, filepath.Join(dir, "data"))

	cfgPath, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if cfgPath != filepath.Join(dir, "cfg", "config.toml") {
		t.Fatalf("config path is %s", cfgPath)
	}

	c := config.Default()
	db, err := c.DBPath()
	if err != nil {
		t.Fatal(err)
	}
	vaultPath, err := c.VaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if db != filepath.Join(dir, "data", "relay.db") {
		t.Fatalf("db path is %s", db)
	}
	// The vault is a different file. That is the whole point of it.
	if vaultPath == db {
		t.Fatal("the vault must not share a file with the index")
	}

	// An explicit data_dir wins over the environment.
	c.DataDir = filepath.Join(dir, "elsewhere")
	db, err = c.DBPath()
	if err != nil || db != filepath.Join(dir, "elsewhere", "relay.db") {
		t.Fatalf("explicit data dir: %s %v", db, err)
	}
}

func TestBadTomlIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("listen = \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path); err == nil {
		t.Fatal("a malformed config should fail loudly")
	}
}
