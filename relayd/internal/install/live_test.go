package install

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/config"
	"github.com/luthor007/relay/relayd/internal/detect"
)

// The run this package could not otherwise prove.
//
// Four bugs shipped in one day because each was verified against something this
// repository wrote: a max_output_tokens floor invented here and asserted against
// a fake server, a voice endpoint that never existed, a release built without
// the daemon in it, and a PATH question that read the installer's own PATH — the
// one restorePath had just added the directory to — and so never fired on any
// machine while passing every test.
//
// This is the antidote and it is deliberately crude: a real Env, the real
// filesystem, the real network, the user's real shell, a throwaway HOME, and
// every question answered with a bare return. It does not assert much, because
// what it is for is the class of failure that assertions in this package cannot
// see. Boot registration is off — a stray LaunchAgent pointing into a deleted
// temp directory is a real thing that has happened here.
//
//	RELAY_LIVE=1 go test ./internal/install/ -run TestLiveInstallerRun -v
func TestLiveInstallerRun(t *testing.T) {
	if os.Getenv("RELAY_LIVE") == "" {
		t.Skip("set RELAY_LIVE=1 to run the installer against this machine")
	}
	home := t.TempDir()
	env := detect.OS()
	env.Home = home
	// Everything answered with a bare return: this is the take-the-defaults run.
	in := strings.NewReader(strings.Repeat("\n", 400))

	opts := Options{
		Env:         env,
		FS:          detect.OSWriteFS{},
		Prompt:      &Terminal{In: in, Out: io.Discard, SecretFD: -1},
		ConfigPath:  home + "/config.toml",
		Config:      config.Default(),
		SkipService: true,
	}
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("the run did not finish: %v", err)
	}
	t.Logf("voice     %s", res.Voice.Line())
	t.Logf("small     %s", res.Models.Small.Line())
	t.Logf("big       %s", res.Models.Big.Line())
	t.Logf("bus       %s", res.Bus.Line())
	t.Logf("shellpath dir=%s added=%v already=%v profile=%s",
		res.ShellPath.Dir, res.ShellPath.Added, res.ShellPath.AlreadyThere, res.ShellPath.Profile)
	t.Logf("warnings  %d", len(res.Warnings))
	for _, w := range res.Warnings {
		t.Logf("  - %s", w)
	}
	if _, err := os.Stat(home + "/config.toml"); err != nil {
		t.Errorf("no config was written: %v", err)
	}
}
