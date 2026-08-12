package main

// APP-PLATFORM.md §6 — the four app commands.
//
//	relay install dev.alexis.standup-notes
//	relay list
//	relay logs standup-notes
//	relay remove standup-notes
//
// They live in their own file, and everything they do lives in
// internal/appstore, for one reason worth stating: **the phone app drives the
// same flow through the API on the box.** If the sheet were composed here, the
// phone would have a second copy of the consent copy and the two would drift,
// and the sentence somebody agreed to would stop being the sentence somebody
// reviewed. So this file resolves flags, asks the terminal question, and prints
// what appstore returns. It decides nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/appruntime"
	"github.com/luthor007/relay/relayd/internal/apps"
	"github.com/luthor007/relay/relayd/internal/appstore"
	"github.com/luthor007/relay/relayd/internal/config"
	"github.com/luthor007/relay/relayd/internal/index"
	"github.com/luthor007/relay/relayd/internal/install"
)

// appsUsage is printed after the setup usage. It is scanned by
// TestEveryAppCommandExists the same way main_test.go scans `usage`: a command
// that only exists in the help text is worse than one that does not exist.
const appsUsage = `
Apps (APP-PLATFORM.md §6 — they run on this box, not on the author's server):
  relay install <app-id>           resolve, show the permission sheet, ask, provision
  relay list [--json]              what is installed, and what it may do
  relay logs <app> [--lines N]     an app's log
  relay remove <app>               revoke its permissions and destroy its container

  --registry <spec>   where apps are resolved from. A fork is a config change:
                      github:owner/repo@ref | https://host/path/ | /path/on/disk
                      Also RELAY_APP_REGISTRY, or one line in <config dir>/app-registry.
`

// appRegistry backs the --registry flag. It lives here rather than on
// `globals` so main.go carries one registration line and nothing else of this
// feature; run() builds a fresh FlagSet per invocation, which resets it.
var appRegistry string

// registerAppFlags is called from run()'s flag set.
func registerAppFlags(fs *flag.FlagSet) {
	fs.StringVar(&appRegistry, "registry", "",
		"app registry: github:owner/repo@ref, an https:// base URL, or a directory")
}

// appFlags parses the flags that may legally follow a positional argument.
//
// The shared flag set stops at the first non-flag word, so `relay logs
// standup-notes --lines 20` leaves the flags unparsed. Rather than teach
// main.go about them, positionals are moved to the end and parsed here.
type appFlags struct {
	lines int
	rest  []string
}

func parseAppFlags(args []string, stderr io.Writer) (appFlags, error) {
	var a appFlags
	fs := flag.NewFlagSet("relay apps", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.IntVar(&a.lines, "lines", 50, "how many log lines")
	fs.StringVar(&appRegistry, "registry", appRegistry,
		"app registry: github:owner/repo@ref, an https:// base URL, or a directory")
	if err := fs.Parse(flagsFirst(args)); err != nil {
		return a, err
	}
	a.rest = fs.Args()
	return a, nil
}

// flagsFirst moves leading positional arguments to the end so a single
// FlagSet.Parse sees every flag, whichever side of the app name it was typed.
func flagsFirst(args []string) []string {
	var pos, flags []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") && args[i] != "-" {
			flags = append(flags, args[i:]...)
			break
		}
		pos = append(pos, args[i])
	}
	return append(flags, pos...)
}

// provisioner builds the app runtime, or returns nil.
//
// Nil is a supported and common outcome: a box with no Node runs no third-party
// apps, and `Installer.Install` already prints `appstore.NoRuntimeNote` and
// records the consent without claiming anything started. What changed is that
// nil is now a fact about the machine rather than about this codebase — until
// `internal/appruntime` existed, `appstore.Provisioner` had no implementation
// anywhere and every install ended that way.
//
// The dispatcher built here is deliberately short-lived. This process installs
// and exits; the daemon holds the one that matters, reattaches every installed
// app on start, and is what actually triggers them.
func (g globals) provisioner() appstore.Provisioner {
	dataDir, err := g.dataDir()
	if err != nil {
		return nil
	}
	detector, err := index.NewDetector()
	if err != nil {
		return nil
	}
	rt, err := apps.New(context.Background(), apps.Options{
		RuntimeDir: filepath.Join(dataDir, "app-runtime"),
		Redact:     detector,
		AccessLog:  &apps.MemoryAccessLog{},
		EgressLog:  &apps.MemoryEgressLog{},
		Limits:     apps.DefaultLimits(),
	})
	if err != nil {
		return nil
	}
	d, err := apps.NewDispatcher(apps.DispatcherOptions{Runtime: rt, Location: time.Local})
	if err != nil {
		return nil
	}
	p, err := appruntime.New(appruntime.Options{
		Runtime: rt, Dispatcher: d, Dir: appruntime.PackagesDir(dataDir),
	})
	if err != nil {
		return nil
	}
	return p
}

// dataDir is this machine's data directory, configured or default.
func (g globals) dataDir() (string, error) {
	cfg, err := config.Load(g.configFile())
	if err != nil {
		return "", err
	}
	if cfg.DataDir != "" {
		return cfg.DataDir, nil
	}
	return config.DataDir()
}

// appstoreFor assembles the registry client and the on-disk store from this
// machine's configuration.
func (g globals) appstoreFor() (*appstore.Registry, *appstore.Store, error) {
	cfg, err := config.Load(g.configFile())
	if err != nil {
		return nil, nil, err
	}
	dataDir := cfg.DataDir
	if dataDir == "" {
		if dataDir, err = config.DataDir(); err != nil {
			return nil, nil, err
		}
	}
	st, err := appstore.OpenStore(appstore.StoreRoot(dataDir))
	if err != nil {
		return nil, nil, err
	}
	spec, err := appstore.ResolveSpec(appRegistry, configDirOf(g))
	if err != nil {
		return nil, nil, err
	}
	src, err := appstore.ParseSpec(spec)
	if err != nil {
		return nil, nil, err
	}
	return appstore.New(src), st, nil
}

// configDirOf is the directory the registry override file would sit in. It is
// derived from --config when one was given, so a test (or a second box on one
// machine) that relocates the config relocates the fork setting with it.
func configDirOf(g globals) string {
	if g.configPath != "" {
		return dirOf(g.configPath)
	}
	dir, err := config.ConfigDir()
	if err != nil {
		return ""
	}
	return dir
}

func dirOf(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i > 0 {
		return p[:i]
	}
	return "."
}

// cmdAppInstall is `relay install <app-id>`.
//
// With no argument it is still the old alias for `relay setup`, which is what
// install.sh's predecessors and a few READMEs call. An app id is unambiguous —
// it has a dot in it and setup takes no arguments — so the two cannot collide.
func cmdAppInstall(ctx context.Context, g globals, rest []string, stdout, stderr io.Writer) error {
	if len(rest) == 0 {
		return cmdSetup(ctx, g, stdout)
	}
	a, err := parseAppFlags(rest, stderr)
	if err != nil {
		return err
	}
	if len(a.rest) != 1 {
		return errors.New("relay install takes one app id, like dev.alexis.standup-notes")
	}
	reg, st, err := g.appstoreFor()
	if err != nil {
		return err
	}

	inst := &appstore.Installer{
		Registry: reg,
		Store:    st,
		Consent:  g.consenter(stdout),
		// The provisioner, which is the one change that makes `relay install`
		// actually start something. Nil is still a supported outcome and still
		// prints appstore.NoRuntimeNote — a box with no Node installs nothing
		// and says so — but it is now a fact about the machine rather than
		// about this codebase.
		Provisioner: g.provisioner(),
	}
	res, err := inst.Install(ctx, a.rest[0])
	switch {
	case errors.Is(err, appstore.ErrDeclined):
		fmt.Fprintln(stdout, "Not installed. Nothing was written and no permission was granted.")
		return nil
	case err != nil:
		return err
	}

	switch {
	case res.AlreadyInstalled:
		fmt.Fprintf(stdout, "%s %s is already installed with these permissions.\n",
			res.Installed.Manifest.Name, res.Installed.Manifest.Version)
	case res.Upgraded:
		fmt.Fprintf(stdout, "\nUpgraded %s to %s.\n", res.Installed.Manifest.Name, res.Installed.Manifest.Version)
	default:
		fmt.Fprintf(stdout, "\nInstalled %s %s.\n", res.Installed.Manifest.Name, res.Installed.Manifest.Version)
	}
	if res.Note != "" {
		fmt.Fprintf(stdout, "\n! %s\n", appstore.Wrap(res.Note, 76))
	}
	fmt.Fprintf(stdout, "\n  relay logs %s\n  relay remove %s\n",
		res.Installed.ShortName(), res.Installed.ShortName())
	return nil
}

// consenter shows the sheet and asks. It renders appstore's Sheet and adds
// nothing to it — the words are appstore's, on purpose.
func (g globals) consenter(out io.Writer) appstore.Consenter {
	if g.yes {
		// `--yes` takes every default. There is no default for "may this
		// third-party code read your transcripts", so an unattended run
		// refuses instead of answering on the user's behalf.
		return appstore.DenyAll
	}
	t := install.NewTerminal()
	t.Out = out
	return appstore.ConsentFunc(func(_ context.Context, s appstore.Sheet) (bool, error) {
		fmt.Fprint(out, s.Text())
		return t.Confirm(install.Confirm{ID: "app.consent", Prompt: s.Question()})
	})
}

// cmdAppList is `relay list`.
func cmdAppList(g globals, rest []string, stdout, stderr io.Writer) error {
	a, err := parseAppFlags(rest, stderr)
	if err != nil {
		return err
	}
	if len(a.rest) != 0 {
		return fmt.Errorf("relay list takes no arguments (got %s)", strings.Join(a.rest, " "))
	}
	_, st, err := g.appstoreFor()
	if err != nil {
		return err
	}
	apps, err := st.List()
	if err != nil {
		return err
	}
	if g.jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"apps": appsJSON(apps)})
	}
	if len(apps) == 0 {
		fmt.Fprintf(stdout, "No apps installed.\n\n  relay install dev.alexis.standup-notes\n")
		return nil
	}
	for _, app := range apps {
		fmt.Fprintf(stdout, "%s %s  %s\n", app.Manifest.Name, app.Manifest.Version, app.ID())
		fmt.Fprintf(stdout, "  %s\n", app.Manifest.Description)
		fmt.Fprintf(stdout, "  state     %s\n", app.State)
		if app.StateReason != "" {
			fmt.Fprintf(stdout, "            %s\n",
				strings.ReplaceAll(appstore.Wrap(app.StateReason, 62), "\n", "\n            "))
		}
		fmt.Fprintf(stdout, "  granted   %s\n", scopeLine(app))
		fmt.Fprintf(stdout, "  from      %s\n", app.Registry)
		fmt.Fprintln(stdout)
	}
	return nil
}

func scopeLine(app appstore.Installed) string {
	if len(app.Grants) == 0 {
		return "nothing"
	}
	out := make([]string, 0, len(app.Grants))
	for _, g := range app.Grants {
		out = append(out, string(g.Scope))
	}
	return strings.Join(out, ", ")
}

type appJSON struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	State       string   `json:"state"`
	StateReason string   `json:"state_reason,omitempty"`
	Running     bool     `json:"running"`
	Scopes      []string `json:"scopes"`
	Registry    string   `json:"registry"`
	Origin      string   `json:"origin"`
	InstalledAt string   `json:"installed_at"`
}

func appsJSON(apps []appstore.Installed) []appJSON {
	out := make([]appJSON, 0, len(apps))
	for _, a := range apps {
		scopes := make([]string, 0, len(a.Grants))
		for _, g := range a.Grants {
			scopes = append(scopes, string(g.Scope))
		}
		out = append(out, appJSON{
			ID: a.ID(), Name: a.Manifest.Name, Version: a.Manifest.Version,
			State: string(a.State), StateReason: a.StateReason,
			// Running is the field a script should branch on, and it is false
			// for everything today. "Installed" and "running" are different
			// facts and this is where they stop being conflated.
			Running:  a.Running(),
			Scopes:   scopes,
			Registry: a.Registry, Origin: a.Origin.Git,
			InstalledAt: a.InstalledAt.Format(time.RFC3339),
		})
	}
	return out
}

// cmdAppLogs is `relay logs <app>`.
func cmdAppLogs(g globals, rest []string, stdout, stderr io.Writer) error {
	a, err := parseAppFlags(rest, stderr)
	if err != nil {
		return err
	}
	if len(a.rest) != 1 {
		return errors.New("relay logs takes one app name, like `relay logs standup-notes`")
	}
	_, st, err := g.appstoreFor()
	if err != nil {
		return err
	}
	view, err := st.View(a.rest[0], a.lines)
	if err != nil {
		return err
	}
	if g.jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(view)
	}
	fmt.Fprintf(stdout, "%s %s — %s\n\n", view.App.Manifest.Name, view.App.Manifest.Version, view.App.State)
	for _, ev := range view.Events {
		fmt.Fprintf(stdout, "%s  %-18s %s\n", ev.At.Format(time.RFC3339), ev.Kind, ev.Message)
		if len(ev.Redacted) > 0 {
			fmt.Fprintf(stdout, "%s  %-18s (redacted before writing: %s)\n",
				strings.Repeat(" ", len(time.RFC3339)), "", strings.Join(ev.Redacted, ", "))
		}
	}
	if view.Note != "" {
		fmt.Fprintf(stdout, "\n! %s\n", appstore.Wrap(view.Note, 76))
	}
	return nil
}

// cmdAppRemove is `relay remove <app>`.
func cmdAppRemove(ctx context.Context, g globals, rest []string, stdout, stderr io.Writer) error {
	a, err := parseAppFlags(rest, stderr)
	if err != nil {
		return err
	}
	if len(a.rest) != 1 {
		return errors.New("relay remove takes one app name, like `relay remove standup-notes`")
	}
	_, st, err := g.appstoreFor()
	if err != nil {
		return err
	}
	inst := &appstore.Installer{Store: st}
	rec, err := inst.Remove(ctx, a.rest[0])
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Removed %s %s.\n", rec.Manifest.Name, rec.Manifest.Version)
	fmt.Fprintf(stdout, "Its permissions are revoked and its record is gone.\n")
	if rec.State != appstore.StateProvisioned {
		fmt.Fprintf(stdout, "It had no container to destroy — it was %s.\n", rec.State)
	}
	return nil
}
