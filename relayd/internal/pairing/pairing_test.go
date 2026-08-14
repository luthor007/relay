package pairing_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/pairing"
)

// The token has to survive a restart, because the phone keeps it.
//
// It did not: relayd minted a fresh one on every start, so a phone paired on
// Tuesday was unauthorized on Wednesday — and the symptom on the phone is
// indistinguishable from the app being broken.
func TestTheTokenSurvivesARestart(t *testing.T) {
	dir := t.TempDir()

	first, err := pairing.Token("", dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) < 12 {
		t.Errorf("token %q is too short to be one", first)
	}
	second, err := pairing.Token("", dir)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("the token changed across a restart: %q then %q", first, second)
	}

	// And it is not world-readable, sitting as it does beside the databases.
	info, err := os.Stat(filepath.Join(dir, pairing.TokenFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode().Perm())
	}
}

// A configured token wins, which is how `relayd -token` pins one.
func TestAConfiguredTokenIsUsedAsIs(t *testing.T) {
	dir := t.TempDir()
	got, err := pairing.Token("pinned", dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "pinned" {
		t.Errorf("token = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, pairing.TokenFile)); err == nil {
		t.Error("a pinned token was written to disk anyway")
	}
}

// A half-written file is regenerated rather than run with: the empty string as
// a token authenticates nobody, and as a box id collides with every other box
// that did the same.
func TestAnEmptyFileIsReplaced(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, pairing.TokenFile), []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := pairing.Token("", dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) == "" {
		t.Error("ran with an empty token")
	}
}

// The link carries all three facts, and Read does not invent any of them.
func TestTheLinkCarriesEverythingThePhoneNeeds(t *testing.T) {
	link, err := pairing.Link("wss://rz.relay.glass", "box-abc", "tok-123")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"relay://", "box-abc", "tok-123", "rz.relay.glass"} {
		if !strings.Contains(link, want) {
			t.Errorf("link %q is missing %q", link, want)
		}
	}
}

// A box with no relay cannot be reached from outside its network, and saying so
// is the whole value of the message.
func TestNoRelayIsAnExplanationNotALink(t *testing.T) {
	if _, err := pairing.Link("", "box-abc", "tok"); err == nil {
		t.Fatal("produced a pairing link for a box with no relay")
	}
	if _, err := pairing.Link("wss://rz.relay.glass", "", ""); err == nil {
		t.Fatal("produced a pairing link with no identity in it")
	}
}

// Read reports absence rather than minting, so a command that only describes
// the machine cannot change it.
func TestReadDoesNotMint(t *testing.T) {
	dir := t.TempDir()
	if _, ok := pairing.Read(dir, pairing.TokenFile); ok {
		t.Fatal("read a token that was never written")
	}
	if _, err := os.Stat(filepath.Join(dir, pairing.TokenFile)); err == nil {
		t.Error("reading created the file")
	}
}
