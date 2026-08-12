package install

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/detect"
)

func serviceOpts(t *testing.T, goos string, answers map[string]string) (Options, *detect.MemFS, *detect.FakeExec) {
	t.Helper()
	fs := &detect.MemFS{Dirs: []string{home}}
	ex := &detect.FakeExec{
		Paths: map[string]string{
			"launchctl": "/bin/launchctl", "systemctl": "/usr/bin/systemctl", "loginctl": "/usr/bin/loginctl",
		},
		Responses: map[string]detect.Result{
			detect.Key("launchctl", "bootstrap", "gui/501", home+"/Library/LaunchAgents/"+LaunchdLabel+".plist"): {},
			detect.Key("systemctl", "--user", "daemon-reload"):                                                   {},
			detect.Key("systemctl", "--user", "enable", "--now", "relayd.service"):                               {},
			detect.Key("loginctl", "enable-linger"):                                                              {},
		},
	}
	return Options{
		Env: detect.Env{FS: fs, Exec: ex, Getenv: func(string) string { return "" },
			Home: home, GOOS: goos},
		FS:         fs,
		Prompt:     NewScript(answers),
		ConfigPath: home + "/.config/relay/config.toml",
		BinaryPath: "/usr/local/bin/relayd",
		Now:        func() time.Time { return time.Unix(1770000000, 0).UTC() },
		UID:        501,
	}.withDefaults(), fs, ex
}

func TestLaunchdAgentRunsAtLoadAndIsKeptAlive(t *testing.T) {
	opts, fs, ex := serviceOpts(t, "darwin", map[string]string{})
	out, err := registerService(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Enabled {
		t.Fatalf("not enabled: %+v", out)
	}
	plist := fs.Files[out.UnitPath]
	// A LaunchAgent without RunAtLoad starts once and never again, which is the
	// failure that looks exactly like "it worked when I installed it".
	if !strings.Contains(plist, "<key>RunAtLoad</key>") || !strings.Contains(plist, "<key>KeepAlive</key>") {
		t.Errorf("plist:\n%s", plist)
	}
	if !strings.Contains(plist, "/usr/local/bin/relayd") {
		t.Errorf("plist does not launch relayd:\n%s", plist)
	}
	// A re-run must replace rather than collide.
	var booted bool
	for _, c := range ex.Calls {
		if c.Name == "launchctl" && len(c.Args) > 0 && c.Args[0] == "bootout" {
			booted = true
		}
	}
	if !booted {
		t.Error("a second install should boot the old agent out first")
	}
	if !out.Lingering {
		t.Error("a LaunchAgent survives logout by itself")
	}
}

func TestLaunchdFallsBackToLoadOnOlderSystems(t *testing.T) {
	opts, _, ex := serviceOpts(t, "darwin", map[string]string{})
	// bootstrap is unknown on this machine; load -w is not.
	ex.Responses = map[string]detect.Result{
		detect.Key("launchctl", "load", "-w", home+"/Library/LaunchAgents/"+LaunchdLabel+".plist"): {},
	}
	out, err := registerService(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Enabled {
		t.Fatalf("older launchctl path failed: %+v", out)
	}
}

// The linux trap: a user unit stops at logout unless lingering is on, so a box
// that is meant to stay on quietly does not.
func TestSystemdAsksAboutLingering(t *testing.T) {
	opts, fs, _ := serviceOpts(t, "linux", map[string]string{"service.linger": "yes"})
	out, err := registerService(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Enabled || !out.Lingering {
		t.Fatalf("service = %+v", out)
	}
	unit := fs.Files[out.UnitPath]
	for _, want := range []string{"Restart=always", "ExecStart=/usr/local/bin/relayd --config", "WantedBy=default.target"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing %q:\n%s", want, unit)
		}
	}
}

func TestDecliningLingeringIsSaidOutLoud(t *testing.T) {
	opts, _, _ := serviceOpts(t, "linux", map[string]string{"service.linger": "no"})
	out, err := registerService(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if out.Lingering {
		t.Fatal("the user declined")
	}
	if !strings.Contains(out.Note, "log out") {
		t.Errorf("note = %q; the consequence has to be stated", out.Note)
	}
	if !strings.Contains(out.Line(), "stop when you log out") {
		t.Errorf("summary line = %q", out.Line())
	}
}

func TestSystemdWithoutSystemctlWritesTheUnitAnyway(t *testing.T) {
	opts, fs, ex := serviceOpts(t, "linux", map[string]string{})
	ex.Paths = map[string]string{}
	out, err := registerService(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if out.Enabled {
		t.Fatal("there is no systemctl to enable it with")
	}
	if fs.Files[out.UnitPath] == "" {
		t.Error("the unit should still be on disk, ready for whatever does start things here")
	}
	if !strings.Contains(out.Note, "not") {
		t.Errorf("note = %q", out.Note)
	}
}

func TestUnsupportedPlatformDegradesVisibly(t *testing.T) {
	opts, _, _ := serviceOpts(t, "windows", map[string]string{})
	out, err := registerService(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != ServiceNone || out.Note == "" {
		t.Errorf("service = %+v; an unsupported platform gets a sentence, not silence", out)
	}
}

func TestUninstallRemovesTheUnit(t *testing.T) {
	opts, fs, _ := serviceOpts(t, "linux", map[string]string{"service.linger": "yes"})
	if _, err := registerService(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	unit := home + "/.config/systemd/user/relayd.service"
	if fs.Files[unit] == "" {
		t.Fatal("no unit to remove")
	}
	if _, err := UninstallService(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if _, still := fs.Files[unit]; still {
		t.Error("the unit is still there after uninstall")
	}
}

// `systemctl enable` prints "Created symlink …" and only then fails, so the
// useful sentence is the last line and not the first. Reporting the symlink as
// the error sends someone looking in the wrong place entirely.
func TestServiceFailureReportsTheUsefulLine(t *testing.T) {
	opts, _, ex := serviceOpts(t, "linux", map[string]string{"service.linger": "yes"})
	ex.Responses[detect.Key("systemctl", "--user", "enable", "--now", "relayd.service")] = detect.Result{
		Code:   1,
		Stdout: []byte("Created symlink /x → /y.\nFailed to connect to bus: No medium found\n"),
	}
	out, err := registerService(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(out.Warnings, " ")
	if !strings.Contains(joined, "Failed to connect to bus") {
		t.Errorf("warnings = %v", out.Warnings)
	}
	if strings.Contains(joined, "Created symlink") {
		t.Errorf("the symlink line is not the failure: %v", out.Warnings)
	}
}
