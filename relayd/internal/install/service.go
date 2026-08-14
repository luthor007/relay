package install

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/luthor007/relay/relayd/internal/detect"
)

// Step 4 of ORCHESTRATOR.md §2 — register to start on boot.
//
// "Any always-on machine" is the premise of the whole pricing story, so this
// step is not optional decoration: a Relay that stops when you close a terminal
// is a Relay that is not there when you speak to it.
//
// Two platforms, two mechanisms, and one trap on each. On darwin a LaunchAgent
// loaded without RunAtLoad starts once and never again. On linux a systemd
// *user* unit stops when the user logs out unless lingering is enabled, which
// is the difference between a box that survives a reboot and one that appears
// to work until the first time nobody is logged in.

// ServiceKind is which init system was used.
type ServiceKind string

const (
	ServiceNone    ServiceKind = "none"
	ServiceLaunchd ServiceKind = "launchd"
	ServiceSystemd ServiceKind = "systemd"
)

// DefaultServiceName is the unit/label name.
const DefaultServiceName = "relayd"

// LaunchdLabel is the reverse-DNS label on darwin.
const LaunchdLabel = "glass.relay.relayd"

// ServiceOutcome is what boot registration did.
type ServiceOutcome struct {
	Kind     ServiceKind
	UnitPath string
	// Enabled is whether the init system accepted it.
	Enabled bool
	// Lingering records whether a linux user unit will survive logout.
	Lingering bool
	// Ran is every command executed, so the console and a support thread can
	// see exactly what happened to the machine.
	Ran      []string
	Note     string
	Warnings []string
}

// Line is the summary row.
func (s ServiceOutcome) Line() string {
	switch {
	case s.Kind == ServiceNone:
		return ""
	case s.Enabled && s.Kind == ServiceSystemd && !s.Lingering:
		return string(s.Kind) + " — enabled, but it will stop when you log out (see the note)"
	case s.Enabled:
		return string(s.Kind) + " — enabled at " + s.UnitPath
	default:
		return string(s.Kind) + " — unit written to " + s.UnitPath + ", not enabled"
	}
}

func registerService(ctx context.Context, opts Options) (ServiceOutcome, error) {
	name := opts.ServiceName
	if name == "" {
		name = DefaultServiceName
	}
	binary := opts.BinaryPath
	if binary == "" {
		binary = "relayd"
	}

	switch opts.Env.GOOS {
	case "darwin":
		return registerLaunchd(ctx, opts, binary)
	case "linux":
		return registerSystemd(ctx, opts, name, binary)
	default:
		out := ServiceOutcome{Kind: ServiceNone}
		out.Note = fmt.Sprintf(
			"Relay does not register a boot service on %s. Start relayd however this system "+
				"starts things, and it will behave identically.", opts.Env.GOOS)
		opts.Prompt.Say("  %s", wrapIndent(out.Note, 2, 76))
		return out, nil
	}
}

func registerLaunchd(ctx context.Context, opts Options, binary string) (ServiceOutcome, error) {
	out := ServiceOutcome{Kind: ServiceLaunchd}
	dir := opts.Env.Home + "/Library/LaunchAgents"
	out.UnitPath = fmt.Sprintf("%s/%s.plist", dir, LaunchdLabel)
	logDir := opts.Env.Home + "/Library/Logs/Relay"

	if err := opts.FS.MkdirAll(dir, 0o755); err != nil {
		return out, err
	}
	if err := opts.FS.MkdirAll(logDir, 0o755); err != nil {
		return out, err
	}
	plist := launchdPlist(LaunchdLabel, binary, opts.ConfigPath, logDir)
	if err := opts.FS.WriteFile(out.UnitPath, []byte(plist), 0o644); err != nil {
		return out, err
	}

	uid := opts.UID
	target := fmt.Sprintf("gui/%d", uid)
	// bootout first so a re-run replaces rather than collides. Its failure is
	// the normal case on a first install and is not worth reporting.
	runQuiet(ctx, opts, &out, "launchctl", "bootout", target+"/"+LaunchdLabel)

	if err := run(ctx, opts, &out, "launchctl", "bootstrap", target, out.UnitPath); err != nil {
		// Older systems only have load -w, and it works.
		if err2 := run(ctx, opts, &out, "launchctl", "load", "-w", out.UnitPath); err2 != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf(
				"the LaunchAgent is written to %s but launchctl would not load it: %v. "+
					"Load it with `launchctl bootstrap %s %s`.", out.UnitPath, err, target, out.UnitPath))
			return out, nil
		}
	}
	out.Enabled = true
	out.Lingering = true // a LaunchAgent survives logout by itself
	out.Note = "relayd starts at login and is restarted if it exits."
	return out, nil
}

func registerSystemd(ctx context.Context, opts Options, name, binary string) (ServiceOutcome, error) {
	out := ServiceOutcome{Kind: ServiceSystemd}
	dir := opts.Env.Home + "/.config/systemd/user"
	if x := opts.Env.Getenv("XDG_CONFIG_HOME"); x != "" {
		dir = x + "/systemd/user"
	}
	unit := name + ".service"
	out.UnitPath = dir + "/" + unit

	if err := opts.FS.MkdirAll(dir, 0o755); err != nil {
		return out, err
	}
	if err := opts.FS.WriteFile(out.UnitPath, []byte(systemdUnit(binary, opts.ConfigPath)), 0o644); err != nil {
		return out, err
	}

	if _, err := opts.Env.Exec.LookPath("systemctl"); err != nil {
		out.Note = fmt.Sprintf(
			"There is no systemctl on this machine, so the unit at %s is written but not "+
				"enabled. Start relayd with whatever this system uses.", out.UnitPath)
		opts.Prompt.Say("  %s", wrapIndent(out.Note, 2, 76))
		return out, nil
	}

	if err := run(ctx, opts, &out, "systemctl", "--user", "daemon-reload"); err != nil {
		out.Warnings = append(out.Warnings, err.Error())
	}
	if err := run(ctx, opts, &out, "systemctl", "--user", "enable", "--now", unit); err != nil {
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"the unit is written to %s but systemd would not enable it: %v", out.UnitPath, err))
		return out, nil
	}
	out.Enabled = true

	// The trap. Without lingering, a user unit is stopped when the last session
	// for that user ends — so the box works until the first logout and then
	// quietly does not, which is the worst shape a failure can have.
	yes, err := opts.Prompt.Confirm(Confirm{
		ID:      "service.linger",
		Prompt:  "Keep relayd running when you log out?",
		Body:    "Runs: loginctl enable-linger",
		Default: true,
	})
	if err != nil {
		return out, err
	}
	if yes {
		if err := run(ctx, opts, &out, "loginctl", "enable-linger"); err != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf(
				"could not enable lingering (%v), so relayd will stop when you log out. "+
					"Run `sudo loginctl enable-linger %s` to fix it.", err, currentUser(opts)))
		} else {
			out.Lingering = true
		}
	} else {
		out.Note = "Lingering is off, so relayd stops when you log out. " +
			"Enable it later with `loginctl enable-linger`."
	}
	return out, nil
}

// UninstallService removes the boot registration.
func UninstallService(ctx context.Context, opts Options) (ServiceOutcome, error) {
	opts = opts.withDefaults()
	out := ServiceOutcome{}
	name := opts.ServiceName
	if name == "" {
		name = DefaultServiceName
	}
	switch opts.Env.GOOS {
	case "darwin":
		out.Kind = ServiceLaunchd
		out.UnitPath = opts.Env.Home + "/Library/LaunchAgents/" + LaunchdLabel + ".plist"
		runQuiet(ctx, opts, &out, "launchctl", "bootout", fmt.Sprintf("gui/%d/%s", opts.UID, LaunchdLabel))
		_ = opts.FS.Remove(out.UnitPath)
	case "linux":
		out.Kind = ServiceSystemd
		out.UnitPath = opts.Env.Home + "/.config/systemd/user/" + name + ".service"
		runQuiet(ctx, opts, &out, "systemctl", "--user", "disable", "--now", name+".service")
		_ = opts.FS.Remove(out.UnitPath)
		runQuiet(ctx, opts, &out, "systemctl", "--user", "daemon-reload")
	default:
		out.Kind = ServiceNone
	}
	return out, nil
}

func run(ctx context.Context, opts Options, out *ServiceOutcome, name string, args ...string) error {
	out.Ran = append(out.Ran, strings.TrimSpace(name+" "+strings.Join(args, " ")))
	res, err := opts.Env.Exec.Run(ctx, detect.Cmd{Name: name, Args: args})
	if err != nil {
		return err
	}
	if res.Code != 0 {
		// stderr first. Falling back to stdout takes its *last* line, because a
		// tool that succeeded partway and then failed leaves the useful sentence
		// at the end — `systemctl enable` prints "Created symlink …" and only
		// then fails, and reporting the symlink as the error is nonsense.
		msg := lastLine(res.Err())
		if msg == "" {
			msg = lastLine(res.Out())
		}
		return fmt.Errorf("%s exited %d: %s", name, res.Code, msg)
	}
	return nil
}

func runQuiet(ctx context.Context, opts Options, out *ServiceOutcome, name string, args ...string) {
	_ = run(ctx, opts, out, name, args...)
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

func currentUser(opts Options) string {
	if u := opts.Env.Getenv("USER"); u != "" {
		return u
	}
	return "$USER"
}

func reportService(p Prompter, s ServiceOutcome) {
	if s.Kind == ServiceNone && s.Note == "" {
		return
	}
	p.Section("Starting on boot", "")
	if s.UnitPath != "" {
		p.Say("  %s", s.UnitPath)
	}
	for _, c := range s.Ran {
		p.Say("    $ %s", c)
	}
	if s.Note != "" {
		p.Say("  %s", wrapIndent(s.Note, 2, 76))
	}
	for _, w := range s.Warnings {
		p.Say("  ! %s", wrapIndent(w, 4, 76))
	}
}

// servicePATH is the PATH a booted relayd gets, and it exists because launchd
// does not give it one worth having.
//
// A LaunchAgent inherits a minimal PATH — roughly /usr/bin:/bin:/usr/sbin:/sbin
// — not the shell's. So a relayd that finds `openclaw` perfectly well when you
// run it by hand cannot find it at all after a reboot, and neither can it find
// `node`, which OpenClaw needs to start. The failure is silent and looks like
// the Gateway being down.
//
// This is deliberately not the installing user's whole environment: copying
// $PATH into a plist bakes one shell's state into a boot-time service. It is
// the small set of places the things relayd shells out to actually live, plus
// wherever the current process found node — because on a machine using nvm that
// directory is version-numbered and unguessable.
func servicePATH() string {
	dirs := []string{
		"/opt/homebrew/bin", // Homebrew on Apple silicon
		"/usr/local/bin",    // Homebrew on Intel, and most manual installs
		"/usr/bin", "/bin", "/usr/sbin", "/sbin",
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append([]string{filepath.Join(home, ".local", "bin")}, dirs...)
	}
	// nvm puts node under a version-numbered directory that nothing can guess,
	// so the one this process is using is the only reliable source.
	if p, err := exec.LookPath("node"); err == nil {
		// Both the directory node is reached through and the one it really
		// lives in. When Relay installed Node itself, ~/.local/bin/node is a
		// symlink and the distribution's own bin is where npm puts every agent
		// runtime — so a boot service given only the first can start Node and
		// still not find `openclaw`.
		cands := []string{filepath.Dir(p)}
		if real, rerr := filepath.EvalSymlinks(p); rerr == nil {
			cands = append(cands, filepath.Dir(real))
		}
		for _, d := range cands {
			if d != "" && !contains(dirs, d) {
				dirs = append([]string{d}, dirs...)
			}
		}
	}
	return strings.Join(dirs, ":")
}

func launchdPlist(label, binary, configPath, logDir string) string {
	args := "    <string>" + xmlEscape(binary) + "</string>\n"
	if configPath != "" {
		args += "    <string>--config</string>\n    <string>" + xmlEscape(configPath) + "</string>\n"
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + xmlEscape(label) + `</string>
  <key>ProgramArguments</key>
  <array>
` + args + `  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>` + xmlEscape(servicePATH()) + `</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ProcessType</key>
  <string>Background</string>
  <key>StandardOutPath</key>
  <string>` + xmlEscape(logDir) + `/relayd.log</string>
  <key>StandardErrorPath</key>
  <string>` + xmlEscape(logDir) + `/relayd.err.log</string>
</dict>
</plist>
`
}

func systemdUnit(binary, configPath string) string {
	exec := binary
	if configPath != "" {
		exec += " --config " + configPath
	}
	return `[Unit]
Description=Relay orchestrator
Documentation=https://relay.glass
After=network-online.target

[Service]
Type=simple
ExecStart=` + exec + `
Restart=always
RestartSec=5
# relayd binds loopback by default; nothing here needs elevated privileges.
NoNewPrivileges=true

[Install]
WantedBy=default.target
`
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// osUID is os.Getuid behind a name that says why it is not in the Env seam: it
// is a pure read of the process's own identity, with no machine state to fake.
func osUID() int { return os.Getuid() }
