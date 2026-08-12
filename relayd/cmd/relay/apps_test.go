package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The CLI is thin — the flow is tested in internal/appstore against fixtures.
// What matters here is that the four documented commands exist, that install
// puts the real permission sheet in front of a person before writing anything,
// and that nothing prints a success for something that did not happen.

// appBox is a whole box in a temp directory: its own config, its own data, and
// a registry that is a directory on disk. Nothing here touches the network.
func appBox(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("RELAY_CONFIG_DIR", dir)
	t.Setenv("RELAY_DATA_DIR", dir)
	t.Setenv("RELAY_APP_REGISTRY", "")
	reg, err := filepath.Abs(filepath.Join("..", "..", "testdata", "appstore", "registry"))
	if err != nil {
		t.Fatal(err)
	}
	return []string{"--registry", reg}
}

// relayIn runs the CLI with stdin wired to answers, the way a person would.
func relayIn(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	// The terminal prompter reads os.Stdin, so a consent answer is given the
	// same way a person gives one.
	old := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(stdin); err != nil {
		t.Fatal(err)
	}
	w.Close()
	os.Stdin = r
	defer func() { os.Stdin = old; r.Close() }()

	var out, errb bytes.Buffer
	e := run(context.Background(), args, &out, &errb)
	return out.String(), errb.String(), e
}

func TestEveryAppCommandExists(t *testing.T) {
	for _, line := range strings.Split(appsUsage, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "relay ") {
			continue
		}
		cmd := strings.Fields(line)[1]
		var out, errb bytes.Buffer
		err := run(context.Background(), []string{cmd, "--help"}, &out, &errb)
		if err != nil && strings.Contains(err.Error(), "unknown command") {
			t.Errorf("the apps usage documents %q and run() does not know it", cmd)
		}
	}
	// And the help text carries them, since that is where people look.
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"help"}, &out, &errb); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"relay install", "relay list", "relay logs", "relay remove", "--registry"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("`relay help` is missing %q", want)
		}
	}
}

// `relay install` with no argument is still the setup alias, and the two cannot
// collide: setup takes no arguments and an app id is a word with dots in it.
func TestInstallWithNoArgumentIsStillSetup(t *testing.T) {
	appBox(t)
	// Reaching install.Run needs a machine; asserting that it does not take the
	// app path is enough, and it is the only thing this alias has to keep.
	out, _, err := relayIn(t, "", "install", "--config", filepath.Join(t.TempDir(), "config.toml"))
	if err == nil && strings.Contains(out, "permission") {
		t.Errorf("bare `relay install` took the app path:\n%s", out)
	}
}

func TestListOnAFreshBox(t *testing.T) {
	flags := appBox(t)
	out, _, err := relayIn(t, "", append([]string{"list"}, flags...)...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No apps installed") {
		t.Errorf("out = %q", out)
	}
}

// The whole flow through the CLI: resolve, show the sheet with every reason
// verbatim, wait for the answer, write the record, and say plainly that nothing
// was started.
func TestInstallShowsTheSheetAndWaits(t *testing.T) {
	flags := appBox(t)
	out, _, err := relayIn(t, "y\n", append([]string{"install", "dev.alexis.standup-notes"}, flags...)...)
	if err != nil {
		t.Fatal(err)
	}

	// Each reason, verbatim, straight out of the manifest the SDK ships.
	for _, reason := range []string{
		"To read the transcript of the meeting you just left.",
		"To save the notes and commitments it extracts back to your memory.",
		"To summarise the meeting using your own agent and your own model.",
		"To read the commitments back to you when you ask.",
	} {
		if !strings.Contains(out, reason) {
			t.Errorf("the sheet did not show %q verbatim:\n%s", reason, out)
		}
	}
	if !strings.Contains(out, "Install Standup Notes with these permissions?") {
		t.Errorf("no consent question:\n%s", out)
	}
	if !strings.Contains(out, "Installed Standup Notes 1.0.0") {
		t.Errorf("out = %s", out)
	}
	// The honesty line, and what it says now depends on the machine. Before
	// `internal/appruntime` existed there was no provisioner anywhere, so this
	// always read "will not run". Now a box with Node tries, and this fixture's
	// repository does not exist — so the install reports the failure with the
	// reason rather than claiming a success it did not have. Either sentence is
	// honest; a bare "Installed" would not be.
	if !strings.Contains(out, "will not run") && !strings.Contains(out, "could not provision it") {
		t.Errorf("install claimed more than it did:\n%s", out)
	}

	out, _, err = relayIn(t, "", append([]string{"list"}, flags...)...)
	if err != nil {
		t.Fatal(err)
	}
	// `awaiting-runtime` on a box with no Node, `failed` on one where the
	// provisioner tried and could not fetch the package. The row must never say
	// the app is running when it is not, which is the property under test.
	if strings.Contains(out, "provisioned") {
		t.Errorf("an app that never started is listed as running:\n%s", out)
	}
	for _, want := range []string{"Standup Notes 1.0.0", "memory.read"} {
		if !strings.Contains(out, want) {
			t.Errorf("list is missing %q:\n%s", want, out)
		}
	}
}

func TestInstallDeclinedWritesNothing(t *testing.T) {
	flags := appBox(t)
	out, _, err := relayIn(t, "n\n", append([]string{"install", "dev.alexis.standup-notes"}, flags...)...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Not installed") {
		t.Errorf("out = %s", out)
	}
	out, _, err = relayIn(t, "", append([]string{"list"}, flags...)...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No apps installed") {
		t.Errorf("a declined install left something behind:\n%s", out)
	}
}

// `--yes` takes every default. There is no default for a permission sheet.
func TestUnattendedInstallRefuses(t *testing.T) {
	flags := appBox(t)
	_, _, err := relayIn(t, "",
		append([]string{"install", "--yes", "dev.alexis.standup-notes"}, flags...)...)
	if err == nil || !strings.Contains(err.Error(), "needs a person") {
		t.Fatalf("err = %v", err)
	}
}

func TestListJSONIsScriptable(t *testing.T) {
	flags := appBox(t)
	if _, _, err := relayIn(t, "y\n",
		append([]string{"install", "dev.alexis.standup-notes"}, flags...)...); err != nil {
		t.Fatal(err)
	}
	out, _, err := relayIn(t, "", append([]string{"list", "--json"}, flags...)...)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Apps []struct {
			ID          string   `json:"id"`
			State       string   `json:"state"`
			StateReason string   `json:"state_reason"`
			Running     bool     `json:"running"`
			Scopes      []string `json:"scopes"`
			Registry    string   `json:"registry"`
			Origin      string   `json:"origin"`
		} `json:"apps"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(doc.Apps) != 1 {
		t.Fatalf("apps = %d", len(doc.Apps))
	}
	a := doc.Apps[0]
	// Installed and running are different facts, and a script has to be able to
	// tell them apart without parsing prose.
	if a.Running {
		t.Error("running = true for an app that never started")
	}
	// Which state depends on the machine — `awaiting-runtime` where there is no
	// Node, `failed` where the provisioner tried and could not fetch this
	// fixture's repository. What a script must be able to rely on is that it is
	// neither "provisioned" nor silent about why.
	if a.State == "provisioned" || a.StateReason == "" {
		t.Errorf("state = %q, reason = %q", a.State, a.StateReason)
	}
	if len(a.Scopes) != 4 || a.Origin == "" || a.Registry == "" {
		t.Errorf("app = %+v", a)
	}
}

func TestLogsAndRemove(t *testing.T) {
	flags := appBox(t)
	if _, _, err := relayIn(t, "y\n",
		append([]string{"install", "dev.alexis.standup-notes"}, flags...)...); err != nil {
		t.Fatal(err)
	}

	// The short name is what people type, and --lines lands after it.
	out, _, err := relayIn(t, "", append([]string{"logs", "standup-notes", "--lines", "10"}, flags...)...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "installed") {
		t.Errorf("logs = %s", out)
	}
	// Either the runtime was never attached (`provision.deferred`, with the
	// note explaining it) or it was attached and failed (`failed`, with the
	// reason). The property is that the log accounts for why the app is not
	// running — a log that showed the install and stopped would leave the user
	// looking at an app that does nothing with nothing to read.
	deferred := strings.Contains(out, "provision.deferred") && strings.Contains(out, "No app runtime is attached")
	failed := strings.Contains(out, "failed")
	if !deferred && !failed {
		t.Errorf("the log does not account for why it is not running:\n%s", out)
	}

	out, _, err = relayIn(t, "", append([]string{"remove", "standup-notes"}, flags...)...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Removed Standup Notes") || !strings.Contains(out, "revoked") {
		t.Errorf("remove = %s", out)
	}
	if !strings.Contains(out, "no container to destroy") {
		t.Errorf("remove should not imply it tore down something that never existed:\n%s", out)
	}

	if _, _, err = relayIn(t, "", append([]string{"logs", "standup-notes"}, flags...)...); err == nil {
		t.Error("logs for a removed app should fail")
	}
}

// Switching registries is a config change. Here it is done three ways — the
// flag, the environment, and a one-line file in the config directory — and none
// of them is a patch.
func TestForkingTheRegistryIsAConfigChange(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RELAY_CONFIG_DIR", dir)
	t.Setenv("RELAY_DATA_DIR", dir)
	fork, err := filepath.Abs(filepath.Join("..", "..", "testdata", "appstore", "fork"))
	if err != nil {
		t.Fatal(err)
	}

	// 1. The environment.
	t.Setenv("RELAY_APP_REGISTRY", fork)
	out, _, err := relayIn(t, "y\n", "install", "dev.alexis.standup-notes")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1.1.0") {
		t.Errorf("the fork's version did not come through:\n%s", out)
	}
	// The fork's app asks for the camera, so the sheet has to carry the rule
	// the box enforces about it.
	if !strings.Contains(out, "indicator LEDs") {
		t.Errorf("camera scope without the LED notice:\n%s", out)
	}

	// 2. A file in the config directory, with the environment cleared.
	t.Setenv("RELAY_APP_REGISTRY", "")
	if err := os.WriteFile(filepath.Join(dir, "app-registry"), []byte(fork+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err = relayIn(t, "", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, fork) {
		t.Errorf("the record does not name the registry it came from:\n%s", out)
	}

	// 3. The flag beats both.
	upstream, err := filepath.Abs(filepath.Join("..", "..", "testdata", "appstore", "registry"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = relayIn(t, "n\n", "install", "com.example.commute-brief", "--registry", upstream)
	if err != nil {
		t.Fatal(err)
	}
	// The fork does not list that app, so reaching it at all proves the flag won.
}

func TestAppCommandArgumentErrors(t *testing.T) {
	flags := appBox(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"logs"}, "one app name"},
		{[]string{"remove"}, "one app name"},
		{[]string{"list", "extra"}, "takes no arguments"},
		{[]string{"install", "a", "b"}, "one app id"},
		{[]string{"remove", "nothing-here"}, "not installed"},
		{[]string{"install", "not an id"}, "not an app id"},
	} {
		_, _, err := relayIn(t, "", append(tc.args, flags...)...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("relay %s: err = %v, want %q", strings.Join(tc.args, " "), err, tc.want)
		}
	}
}

// The flags the shared flag set never sees, because it stops at the first
// positional argument.
func TestFlagsFirst(t *testing.T) {
	for _, tc := range []struct{ in, want []string }{
		{[]string{"app", "--lines", "5"}, []string{"--lines", "5", "app"}},
		{[]string{"--lines", "5", "app"}, []string{"--lines", "5", "app"}},
		{[]string{"app"}, []string{"app"}},
		{nil, nil},
	} {
		got := flagsFirst(tc.in)
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("flagsFirst(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
