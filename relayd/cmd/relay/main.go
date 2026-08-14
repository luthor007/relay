// Command relay is the installer and the setup CLI.
//
//	curl -fsSL https://relay.glass/install | sh
//
// That script fetches one static binary and runs `relay setup`, which is
// ORCHESTRATOR.md §2: detect what is already installed, offer the rest, choose
// a voice, choose the two orchestrator models, reconcile the MCP servers the
// runtimes already have, register to start on boot, and print a pairing code.
// Every credential is tested with one real call before it exits.
//
//	relay                 # same as relay setup
//	relay setup [--yes]   # the whole flow
//	relay detect [--json] # what is on this machine, and nothing else
//	relay voice           # re-run the voice choice
//	relay models          # re-run the two model choices
//	relay embed           # re-run the embedding choice
//	relay reindex [--force]  # re-embed after a model change
//	relay mcp [rollback <manifest>]
//	relay doctor          # re-probe every credential, one real call each
//	relay service install|uninstall
//	relay pair            # a fresh pairing code
//	relay status
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/luthor007/relay/relayd/internal/config"
	"github.com/luthor007/relay/relayd/internal/detect"
	"github.com/luthor007/relay/relayd/internal/install"
	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/pairing"
	"github.com/luthor007/relay/relayd/internal/search"
	"github.com/luthor007/relay/relayd/internal/store"
	"github.com/luthor007/relay/relayd/internal/vault"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

// vaultOpen is [vault.Open], indirected so the CLI tests can run commands
// in-process without them reaching for the developer's own OS keychain. See the
// identical seam in cmd/relayd for why; `relay backfill` opens the vault on
// every run, so this package raises the same dialog for the same reason.
//
// Nothing but a test should replace it.
var vaultOpen = vault.Open

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		// Ctrl-C is not an error to report back at somebody who just pressed
		// it. The step that stopped has already said so in its own words;
		// printing "relay: context canceled" underneath adds a fault where the
		// user made a choice. 130 is the conventional exit for SIGINT.
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, "relay:", err)
		os.Exit(1)
	}
}

const usage = `relay — set up the Relay orchestrator on this machine.

  relay setup [--yes]              detect, choose a voice and two models, start on boot
  relay detect [--json]            what agent runtimes are here, and what history they hold
  relay voice                      choose a voice again
  relay models                     choose the orchestrator models again
  relay embed                      choose an embedding model again
  relay entitlements               record which subscriptions you pay for, so work is routed there
  relay reindex [--force]          re-embed the index after an embedding-model change
  relay mcp                        reconcile the MCP servers your runtimes already have
  relay mcp rollback <manifest>    undo an MCP adoption
  relay doctor                     re-probe every credential with one real call
  relay service install|uninstall  boot registration only
  relay pair                       print a fresh pairing code
  relay status                     what is configured
  relay version

Flags common to most commands:
  --config <path>     config.toml (default: the platform config directory)
  --relayd <path>     the relayd binary to register on boot
  --openclaw-profile  OpenClaw runs with --profile <name>, which relocates its state
  --openclaw-dev      OpenClaw runs with --dev
`

type globals struct {
	configPath      string
	relaydPath      string
	yes             bool
	force           bool
	jsonOut         bool
	openclawProfile string
	openclawDev     bool
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cmd := "setup"
	// `relay --help` asks for the usage block, not for the flag package's
	// dump of every flag followed by a non-zero exit.
	if len(args) > 0 && (!strings.HasPrefix(args[0], "-") || args[0] == "-h" || args[0] == "--help") {
		cmd, args = args[0], args[1:]
	}

	var g globals
	fs := flag.NewFlagSet("relay "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&g.configPath, "config", "", "path to config.toml")
	fs.StringVar(&g.relaydPath, "relayd", "", "path to the relayd binary")
	fs.BoolVar(&g.yes, "yes", false, "take every default and ask nothing (unattended)")
	fs.BoolVar(&g.force, "force", false, "re-embed even when the index and the config already agree")
	fs.BoolVar(&g.jsonOut, "json", false, "machine-readable output where the command has it")
	fs.StringVar(&g.openclawProfile, "openclaw-profile", "",
		"OpenClaw runs with --profile <name>, which relocates its state directory")
	fs.BoolVar(&g.openclawDev, "openclaw-dev", false, "OpenClaw runs with --dev")
	registerAppFlags(fs) // apps.go — APP-PLATFORM.md §6
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()

	switch cmd {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		fmt.Fprint(stdout, appsUsage)
		return nil
	case "version", "--version":
		fmt.Fprintln(stdout, "relay", version)
		return nil
	case "detect":
		return cmdDetect(ctx, g, stdout)
	case "setup":
		return cmdSetup(ctx, g, stdout)
	// APP-PLATFORM.md §6, in apps.go. `relay install` with no argument is still
	// the old alias for `relay setup`; with an app id it installs an app.
	case "install":
		return cmdAppInstall(ctx, g, rest, stdout, stderr)
	case "list":
		return cmdAppList(g, rest, stdout, stderr)
	case "logs":
		return cmdAppLogs(g, rest, stdout, stderr)
	case "remove":
		return cmdAppRemove(ctx, g, rest, stdout, stderr)
	case "voice":
		return cmdVoice(ctx, g, stdout)
	case "models":
		return cmdModels(ctx, g, stdout)
	case "embed":
		return cmdEmbed(ctx, g, stdout)
	// MEMORY.md §8. A plan bought or lost changes nothing on the machine, so
	// this is the one setup answer that goes stale without anything to notice
	// it — and the config file is the only other place it can be changed.
	case "entitlements":
		return cmdEntitlements(ctx, g, stdout)
	case "reindex":
		return cmdReindex(ctx, g, stdout)
	// MEMORY.md §11 step 2b. The readers were written and had no caller, so
	// nothing on this machine had ever been indexed.
	case "backfill":
		return backfillCmd(ctx, g, rest, stdout)
	case "mcp":
		return cmdMCP(ctx, g, rest, stdout)
	case "doctor":
		return cmdDoctor(ctx, g, stdout)
	case "service":
		return cmdService(ctx, g, rest, stdout)
	case "pair":
		return cmdPair(g, stdout)
	case "status":
		return cmdStatus(g, stdout)
	}
	fmt.Fprint(stderr, usage)
	fmt.Fprint(stderr, appsUsage)
	return fmt.Errorf("unknown command %q", cmd)
}

// options assembles everything the installer needs from the real machine.
func (g globals) options(ctx context.Context, out io.Writer) (install.Options, error) {
	path := g.configPath
	if path == "" {
		p, err := config.Path()
		if err != nil {
			return install.Options{}, err
		}
		path = p
	}
	cfg, err := config.Load(path)
	if err != nil {
		return install.Options{}, err
	}

	var prompt install.Prompter
	if g.yes {
		prompt = &install.Auto{Out: out}
	} else {
		t := install.NewTerminal()
		t.Out = out
		prompt = t
	}

	opts := install.Options{
		Env:        detect.OS(),
		FS:         detect.OSWriteFS{},
		Prompt:     prompt,
		ConfigPath: path,
		Config:     cfg,
		BinaryPath: g.relayd(),
		Detect: detect.Options{
			OpenClawProfile: g.openclawProfile,
			OpenClawDev:     g.openclawDev,
		},
	}
	// The vault is optional: it is what makes "type it now" available, and
	// without it a typed secret is refused rather than written to a file.
	//
	// Its failure used to be swallowed, which is the worst way for this
	// particular thing to fail: every credential stored as `vault:<id>` — a
	// ChatGPT sign-in, a typed API key — then resolves to nothing, so setup
	// re-asks for all of them and the user is told only that they were asked.
	if v, err := openVault(ctx, cfg); err == nil {
		opts.Vault = v
		// See vault.RotateCodex: a refresh token that rotates and is not
		// written back leaves the vault holding a spent one, and this process
		// is where a ChatGPT login is created and first used.
		if r, ok := v.(vault.Rotatable); ok {
			llm.CodexPersist = vault.RotateCodex(r)
		}
	} else {
		fmt.Fprintf(out, "  Relay's vault did not open: %v\n"+
			"  Credentials kept in it cannot be read, so this run will ask for them again.\n", err)
	}
	// The index is optional too, and for a sharper reason: opening it would
	// CREATE it, and a `relay doctor` that leaves a database behind on a box
	// where relayd has never run is a side effect nobody asked for. So it is
	// attached only when the file is already there.
	if idx, err := openIndex(cfg); err == nil && idx != nil {
		opts.Index = idx
	}
	// MEMORY.md §7 step 4, switched on: the field that turns the MCP write half
	// from an enumeration into an adoption. It stays zero unless the gateway is
	// answering right now — see gatewayIfLive.
	opts.Gateway, opts.GatewayNote = gatewayIfLive(ctx, cfg, out)
	return opts, nil
}

// storeIndex is install.Index over the real database.
//
// Both methods are internal/search's, unmodified. The CLI owns opening the file
// and nothing else — deciding what a model mismatch means is search's job, and
// re-deriving it here would be a second opinion to keep in step with the first.
type storeIndex struct{ db *store.DB }

func (s storeIndex) Inspect(ctx context.Context) (search.EmbeddingState, error) {
	// A nil embedder is deliberate: this reports what the INDEX holds, and the
	// caller fills in what is configured. Building a real embedder here would
	// mean `relay doctor` could not report on an index whose provider is down,
	// which is the case it exists for.
	return search.InspectEmbedding(ctx, s.db, nil)
}

func (s storeIndex) Reset(ctx context.Context, model string) (int64, error) {
	return search.ResetEmbeddingIndex(ctx, s.db, model)
}

// openIndex attaches the summary index when there is one. A missing database is
// not an error — it is a box where relayd has not run yet.
func openIndex(cfg config.Config) (install.Index, error) {
	p, err := cfg.DBPath()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(p); err != nil {
		return nil, nil
	}
	db, err := store.Open(p)
	if err != nil {
		return nil, err
	}
	return storeIndex{db: db}, nil
}

// relayd finds the daemon: an explicit flag, then PATH, then next to this
// binary — which is where the install script puts it.
func (g globals) relayd() string {
	if g.relaydPath != "" {
		return g.relaydPath
	}
	if p, err := exec.LookPath("relayd"); err == nil {
		return p
	}
	if self, err := os.Executable(); err == nil {
		beside := filepath.Join(filepath.Dir(self), "relayd")
		if fi, err := os.Stat(beside); err == nil && !fi.IsDir() {
			return beside
		}
	}
	return "relayd"
}

func openVault(ctx context.Context, cfg config.Config) (install.Vault, error) {
	p, err := cfg.VaultPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	return vaultOpen(ctx, vault.Options{DBPath: p})
}

func cmdSetup(ctx context.Context, g globals, out io.Writer) error {
	opts, err := g.options(ctx, out)
	if err != nil {
		return err
	}
	res, err := install.Run(ctx, opts)
	if err != nil {
		return err
	}
	if !res.OK() {
		// Not an error exit: the install finished and the machine is usable.
		// The warnings already said what needs attention, and exiting non-zero
		// would make a curl | sh look like it failed when it did not.
		fmt.Fprintln(out, "\nSome credentials did not verify. Run `relay doctor` after fixing them.")
	}
	return nil
}

func cmdDetect(ctx context.Context, g globals, out io.Writer) error {
	env := detect.OS()
	rep := detect.Detect(ctx, env, detect.Options{
		OpenClawProfile: g.openclawProfile,
		OpenClawDev:     g.openclawDev,
	})
	// The embedding runtime is detected here too, because "what is on this
	// machine" now includes the thing that computes the vectors — and because
	// `relay detect --json` is what a support conversation asks for first.
	ollama := detect.DetectOllama(ctx, env)
	cfg, _ := config.Load(g.configFile())

	if g.jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(reportJSON(rep, ollama, cfg.Embedding))
	}
	fmt.Fprintln(out, rep.Summary())
	fmt.Fprintln(out)
	for _, f := range rep.Findings {
		fmt.Fprintf(out, "%-12s %s\n", f.Label, f.Status().Line())
		if f.BinaryPath != "" {
			fmt.Fprintf(out, "             %s", f.BinaryPath)
			if f.Version != "" {
				fmt.Fprintf(out, "  %s", f.Version)
			}
			fmt.Fprintln(out)
		}
		if f.StateDir != "" {
			state := f.StateDir
			if !f.StateDirSource.Trusted() {
				state += "  (assumed: " + f.StateDirDetail + ")"
			}
			fmt.Fprintf(out, "             %s\n", state)
		}
		if n, ok := f.SessionCount(); ok {
			fmt.Fprintf(out, "             %d sessions\n", n)
		} else if f.SessionsNote != "" {
			fmt.Fprintf(out, "             %s\n", f.SessionsNote)
		}
		for _, note := range f.Notes {
			fmt.Fprintf(out, "             %s\n", note)
		}
	}

	fmt.Fprintf(out, "\n%-12s %s\n", "Ollama", ollama.Status().Line())
	if ollama.BinaryPath != "" {
		fmt.Fprintf(out, "             %s", ollama.BinaryPath)
		if ollama.Version != "" {
			fmt.Fprintf(out, "  %s", ollama.Version)
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintf(out, "             %s (%s)\n", ollama.Host, ollama.HostSource)
	if len(ollama.Models) > 0 {
		fmt.Fprintf(out, "             %s\n", strings.Join(ollama.ModelNames(), ", "))
	}
	if ollama.ServiceNote != "" {
		fmt.Fprintf(out, "             %s\n", ollama.ServiceNote)
	}
	for _, note := range ollama.Notes {
		fmt.Fprintf(out, "             %s\n", note)
	}
	fmt.Fprintf(out, "%-12s %s\n", "embedding", describeEmbedding(cfg.Embedding, ollama))
	return nil
}

// describeEmbedding says what is configured and, for a local model, whether the
// runtime actually holds it. Configured and present are different facts, and
// the gap between them is a search that quietly halves.
func describeEmbedding(e config.Embedding, o detect.Ollama) string {
	if !e.Configured() {
		return "none — search is keyword-only"
	}
	s := e.Model + " on " + e.Provider
	if e.Dims > 0 {
		s += fmt.Sprintf(" (%d dims)", e.Dims)
	}
	if !e.Local() {
		return s
	}
	switch {
	case !o.Reachable:
		return s + " — but the runtime is not answering"
	case o.Has(e.Model):
		return s + " — pulled and answering"
	default:
		return s + " — the runtime is up but does not have that model; run `relay embed`"
	}
}

// reportJSON is the stable shape of `relay detect --json`.
type findingJSON struct {
	Runtime         string   `json:"runtime"`
	Status          string   `json:"status"`
	Installed       bool     `json:"installed"`
	BinaryPath      string   `json:"binary_path,omitempty"`
	Version         string   `json:"version,omitempty"`
	StateDir        string   `json:"state_dir,omitempty"`
	StateDirSource  string   `json:"state_dir_source,omitempty"`
	StateDirTrusted bool     `json:"state_dir_trusted"`
	Sessions        *int     `json:"sessions"`
	Bytes           *int64   `json:"bytes"`
	Running         int      `json:"running"`
	Notes           []string `json:"notes,omitempty"`
}

// ollamaJSON and embeddingJSON are the stable shape of the two new sections.
type ollamaJSON struct {
	Status     string   `json:"status"`
	Installed  bool     `json:"installed"`
	Running    bool     `json:"running"`
	BinaryPath string   `json:"binary_path,omitempty"`
	Version    string   `json:"version,omitempty"`
	Host       string   `json:"host"`
	HostSource string   `json:"host_source"`
	Models     []string `json:"models"`
	Note       string   `json:"note,omitempty"`
}

type embeddingJSON struct {
	Configured bool   `json:"configured"`
	Provider   string `json:"provider,omitempty"`
	Model      string `json:"model,omitempty"`
	Dims       int    `json:"dims,omitempty"`
	Local      bool   `json:"local"`
	// Pulled is meaningful only for a local model: it is whether the runtime
	// actually holds it. nil means nobody could ask, which is not the same as
	// "no" — the same rule detect.Finding follows for a store it never opened.
	Pulled *bool `json:"pulled"`
}

func reportJSON(rep detect.Report, o detect.Ollama, e config.Embedding) map[string]any {
	out := make([]findingJSON, 0, len(rep.Findings))
	for _, f := range rep.Findings {
		out = append(out, findingJSON{
			Runtime: string(f.Runtime), Status: string(f.Status()), Installed: f.Installed,
			BinaryPath: f.BinaryPath, Version: f.Version,
			StateDir: f.StateDir, StateDirSource: string(f.StateDirSource),
			StateDirTrusted: f.StateDirSource.Trusted(),
			Sessions:        f.Sessions, Bytes: f.StoreBytes,
			Running: len(f.Running), Notes: f.Notes,
		})
	}
	emb := embeddingJSON{
		Configured: e.Configured(), Provider: e.Provider, Model: e.Model,
		Dims: e.Dims, Local: e.Local(),
	}
	if e.Configured() && e.Local() && o.Models != nil {
		has := o.Has(e.Model)
		emb.Pulled = &has
	}

	return map[string]any{
		"at":       rep.At,
		"goos":     rep.GOOS,
		"summary":  rep.Summary(),
		"findings": out,
		"ollama": ollamaJSON{
			Status: string(o.Status()), Installed: o.Installed, Running: o.Reachable,
			BinaryPath: o.BinaryPath, Version: o.Version,
			Host: o.Host, HostSource: o.HostSource,
			Models: o.ModelNames(), Note: o.ServiceNote,
		},
		"embedding": emb,
	}
}

func cmdVoice(ctx context.Context, g globals, out io.Writer) error {
	opts, err := g.options(ctx, out)
	if err != nil {
		return err
	}
	_, err = install.RunVoice(ctx, opts)
	return err
}

func cmdModels(ctx context.Context, g globals, out io.Writer) error {
	opts, err := g.options(ctx, out)
	if err != nil {
		return err
	}
	_, err = install.RunModels(ctx, opts)
	return err
}

func cmdEmbed(ctx context.Context, g globals, out io.Writer) error {
	opts, err := g.options(ctx, out)
	if err != nil {
		return err
	}
	_, err = install.RunEmbedding(ctx, opts)
	return err
}

func cmdEntitlements(ctx context.Context, g globals, out io.Writer) error {
	opts, err := g.options(ctx, out)
	if err != nil {
		return err
	}
	_, err = install.RunEntitlements(ctx, opts)
	return err
}

func cmdReindex(ctx context.Context, g globals, out io.Writer) error {
	opts, err := g.options(ctx, out)
	if err != nil {
		return err
	}
	_, err = install.RunReindex(ctx, opts, g.force)
	return err
}

func cmdMCP(ctx context.Context, g globals, rest []string, out io.Writer) error {
	if len(rest) > 0 && rest[0] == "rollback" {
		if len(rest) < 2 {
			return errors.New("relay mcp rollback needs the path of a manifest.json")
		}
		m, err := install.RollbackMCP(detect.OSWriteFS{}, rest[1])
		if err != nil {
			return err
		}
		for _, b := range m.Backups {
			fmt.Fprintf(out, "restored %s from %s\n", b.Original, b.Copy)
		}
		return nil
	}
	opts, err := g.options(ctx, out)
	if err != nil {
		return err
	}
	res, err := install.RunMCP(ctx, opts)
	if err != nil {
		return err
	}
	if res.ManifestPath != "" {
		fmt.Fprintf(out, "\nUndo with: relay mcp rollback %s\n", res.ManifestPath)
	}
	return nil
}

func cmdDoctor(ctx context.Context, g globals, out io.Writer) error {
	opts, err := g.options(ctx, out)
	if err != nil {
		return err
	}
	d := install.RunDoctor(ctx, opts)
	d.Print(opts.Prompt)
	if !d.OK() {
		fmt.Fprintln(out, "\nSomething above needs attention.")
	}
	return nil
}

func cmdService(ctx context.Context, g globals, rest []string, out io.Writer) error {
	action := "install"
	if len(rest) > 0 {
		action = rest[0]
	}
	opts, err := g.options(ctx, out)
	if err != nil {
		return err
	}
	switch action {
	case "install":
		res, err := install.RunService(ctx, opts)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, res.Line())
		return nil
	case "uninstall":
		res, err := install.UninstallService(ctx, opts)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "removed %s\n", res.UnitPath)
		return nil
	}
	return fmt.Errorf("relay service: unknown action %q (install or uninstall)", action)
}

// cmdPair prints what a phone needs to reach this machine.
//
// It used to print a six-character code that nothing checked and no daemon had
// ever heard of — a pairing flow's costume without the flow. What the app
// actually needs is the relay, this box's durable name at it, and the token;
// relayd keeps the last two beside its databases, so this reads them rather
// than inventing anything.
func cmdPair(g globals, out io.Writer) error {
	cfg, err := config.Load(g.configFile())
	if err != nil {
		return err
	}
	dir := cfg.DataDir
	if dir == "" {
		if d, err := config.DataDir(); err == nil {
			dir = d
		}
	}

	box, okBox := pairing.Read(dir, pairing.BoxIDFile)
	token, okToken := pairing.Read(dir, pairing.TokenFile)
	if !okBox || !okToken {
		fmt.Fprintln(out, "\nThis machine has not started relayd yet, so it has no durable identity "+
			"to pair with. Start it once — `relay service install` does, and so does a reboot — "+
			"then run this again.")
		return nil
	}

	link, err := pairing.Link(cfg.Relay.URL, box, token)
	if err != nil {
		// A LAN-only box is a real configuration, and the phone can still be
		// pointed at it by hand. Say so rather than failing.
		fmt.Fprintf(out, "\n  %s\n", err.Error())
		fmt.Fprintf(out, "\n  On this network, enter these in the Relay app instead:\n")
		fmt.Fprintf(out, "    Address  %s\n    Token    %s\n", cfg.Listen, token)
		return nil
	}

	fmt.Fprintf(out, "\n  %s\n\n", link)
	fmt.Fprintln(out, "  Open that on your phone — a message to yourself, or AirDrop — and it opens")
	fmt.Fprintln(out, "  the Relay app already paired. It carries the token, so treat it like one.")
	fmt.Fprintf(out, "\n  By hand: relay %s, box %s\n", cfg.Relay.URL, box)
	return nil
}

// configFile is the config path, resolved. An unresolvable one is not fatal for
// a read-only command: config.Load treats a missing file as defaults.
func (g globals) configFile() string {
	if g.configPath != "" {
		return g.configPath
	}
	p, err := config.Path()
	if err != nil {
		return ""
	}
	return p
}

func cmdStatus(g globals, out io.Writer) error {
	path := g.configFile()
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, install.DescribeConfig(cfg, path))
	return nil
}
