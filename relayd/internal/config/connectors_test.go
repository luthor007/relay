package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/config"
)

func writeConfig(t *testing.T, body string) (config.Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.Load(path)
}

// ORCHESTRATOR.md §4b's set is built from this section and nothing else, so a
// section that does not round-trip is a proposer with an empty set — which
// proposes nothing however often something is mentioned, silently.
func TestAConfiguredConnectorRoundTrips(t *testing.T) {
	c, err := writeConfig(t, `
listen = "127.0.0.1:0"

[connectors]
window = "72h"
cooldown = "240h"
min_episodes = 2

[connectors.prusa]
address = "http://prusa.local"
credential = "env:PRUSA_KEY"
storage = "usb"
`)
	if err != nil {
		t.Fatalf("a valid connector section did not load: %v", err)
	}
	if got := c.Connectors.Prusa.Address; got != "http://prusa.local" {
		t.Errorf("address = %q", got)
	}
	if got := c.Connectors.Prusa.Credential; got != "env:PRUSA_KEY" {
		t.Errorf("credential = %q", got)
	}
	if got := c.Connectors.Prusa.Storage; got != "usb" {
		t.Errorf("storage = %q", got)
	}
	if !c.Connectors.Prusa.Configured() || !c.Connectors.Any() {
		t.Error("a printer with an address and a credential is configured")
	}
	if got := c.Connectors.WindowDuration(); got != 72*time.Hour {
		t.Errorf("window = %s", got)
	}
	if got := c.Connectors.CooldownDuration(); got != 240*time.Hour {
		t.Errorf("cooldown = %s", got)
	}
	if c.Connectors.MinEpisodes != 2 {
		t.Errorf("min_episodes = %d", c.Connectors.MinEpisodes)
	}
}

// An absent section is the normal state on every machine, and it must be a
// clean "nothing configured" rather than a half-built connector.
func TestNoConnectorSectionIsNotAConnector(t *testing.T) {
	c, err := writeConfig(t, "listen = \"127.0.0.1:0\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Connectors.Any() {
		t.Error("a config with no [connectors] must not produce one")
	}
	// Zero, so cmd/relayd hands the proposer nothing and the package's own
	// documented defaults apply. A config-supplied zero must never mean "no
	// evidence required": "a proposal needs evidence" is proposal.go's first
	// property.
	if c.Connectors.WindowDuration() != 0 || c.Connectors.CooldownDuration() != 0 {
		t.Error("an unset window or cooldown must be zero so the package default wins")
	}
	if c.Connectors.MinEpisodes != 0 {
		t.Error("an unset min_episodes must be zero so DefaultMinEpisodes wins")
	}
}

// The PrusaLink key is a reference like every other credential in this file.
// The file is world-readable in every configuration where somebody has cat'd it
// into a support ticket, and a printer key opens a machine in another room.
func TestAPastedPrinterKeyIsRefused(t *testing.T) {
	_, err := writeConfig(t, `
[connectors.prusa]
address = "http://prusa.local"
credential = "prusa:AbCdEf123456"
`)
	if err == nil {
		t.Fatal("a pasted secret was accepted as the printer credential")
	}
	if !strings.Contains(err.Error(), "connectors.prusa.credential") {
		t.Errorf("the error does not name the field: %v", err)
	}
	if !strings.Contains(err.Error(), "pasted secret") {
		t.Errorf("the error does not say what is wrong: %v", err)
	}
}

// An address with no scheme is the mistake people actually make, and it fails
// far from here — at the first HTTP call, inside a tool the agent just gained.
func TestAPrinterAddressMustBeAURL(t *testing.T) {
	for _, addr := range []string{"prusa.local", "192.168.1.40", "ftp://prusa.local"} {
		_, err := writeConfig(t, "[connectors.prusa]\naddress = \""+addr+
			"\"\ncredential = \"env:K\"\n")
		if err == nil {
			t.Errorf("address = %q was accepted; PrusaLink is an HTTP API and this "+
				"would fail at the first tool call instead", addr)
		}
	}
}

// A connector offered and then not usable is worse than one that is absent: the
// user makes a grant decision and gets access that does not work.
func TestAPrinterWithNoCredentialIsRefused(t *testing.T) {
	_, err := writeConfig(t, "[connectors.prusa]\naddress = \"http://prusa.local\"\n")
	if err == nil {
		t.Fatal("a printer with no API key was accepted")
	}
	if !strings.Contains(err.Error(), "credential") {
		t.Errorf("the error does not say what is missing: %v", err)
	}
}

func TestConnectorWindowsMustBeDurations(t *testing.T) {
	for _, body := range []string{
		"[connectors]\nwindow = \"one week\"\n",
		"[connectors]\ncooldown = \"-30m\"\n",
		"[connectors]\nmin_episodes = -1\n",
	} {
		if _, err := writeConfig(t, body); err == nil {
			t.Errorf("accepted: %q", body)
		}
	}
}
