// Package install is the installer — SYSTEM.md §10 step 1 and ORCHESTRATOR.md
// §2.
//
//	curl -fsSL https://relay.glass/install | sh
//
// It is the highest-leverage piece in the milestone because it is what makes
// the pricing story true: run one command, pay nothing. Everything else in
// Relay is downstream of somebody getting through this in one sitting.
//
// The order is fixed by dependency and by what a person can stand:
//
//  1. say what access this takes, before taking any        (§4b)
//  2. detect what is already installed                     (§2 step 1)
//  3. offer to install the rest                            (§2 step 2)
//  4. choose a voice — a step, not an advanced setting     (§2a)
//  5. choose the two orchestrator models                   (§2b)
//  6. choose an embedding model                            (§2c)
//  7. reconcile the MCP servers the runtimes already have  (MEMORY.md §7)
//  8. register to start on boot                            (§2 step 4)
//  9. print a pairing code                                 (§2 step 5)
//  10. and only then start the backfill                    (MEMORY.md §4)
//
// The last step being last is not a detail. Nobody should watch a progress bar
// before their glasses work.
//
// The embedding step being *before* the pairing code is also not a detail, and
// for the opposite reason. The backfill handed to relayd at the end is what
// creates the vector index, and a vec0 column's width is fixed at create time —
// so the embedding model has to be chosen before any of it runs. Choosing it
// afterwards is re-embedding everything, which is `relay reindex` and a
// deliberate flow rather than an accident. See embed.go.
//
// Two rules run through all of it. **Every credential is tested with one real
// call before the installer exits** — a pairing code that works, glasses that
// pair, and silence the first time someone speaks is the worst possible place
// to discover a bad key. And **nothing is collected up front beyond the
// baseline**: asking for every scope at install is the single best way to lose
// the install, and it is the pattern that gets software flagged as
// malware-shaped.
package install

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/config"
	"github.com/luthor007/relay/relayd/internal/detect"
	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/logx"
	"github.com/luthor007/relay/relayd/internal/summarize"
	"github.com/luthor007/relay/relayd/internal/vault"
)

// Options is everything the installer needs, with every edge injected.
type Options struct {
	// Env is the machine. detect.OS() for a real run.
	Env detect.Env
	// FS is the write side. detect.OSWriteFS{} for a real run.
	FS detect.WriteFS
	// Prompt is the human.
	Prompt Prompter

	// ConfigPath is where config.toml goes.
	ConfigPath string
	// Config is what to start from; the zero value means config.Default().
	Config config.Config

	// HTTPClient is used for every credential probe, so a test makes no network
	// call at all.
	HTTPClient *http.Client
	// Vault stores a pasted secret and hands back a vault: reference. Nil means
	// pasted secrets are refused, which is the safe default rather than an
	// inline credential in a config file.
	Vault Vault

	// Detect tunes the detection pass — an OpenClaw profile, mostly.
	Detect detect.Options

	// EmbedRuntime is the local embedding runtime. Nil means Ollama wired to
	// Env — a test supplies a fixture, which is the only way the download path
	// is ever exercised here.
	EmbedRuntime EmbedRuntime
	// Index is the summary index, which `relay reindex` clears and `relay
	// doctor` reads. Nil means there is no index on this machine yet — relayd
	// has never run — which is a normal state and not an error.
	Index Index
	// ProbeEmbed overrides the embedding probe. Nil means llm.ProbeEmbedConfig,
	// which is what a real run uses; it exists so a test can state a provider's
	// answer outright instead of hand-rolling a wire fixture for a width it
	// wants to reject.
	ProbeEmbed func(context.Context, llm.EmbedConfig) llm.EmbedCheck

	// Gateway describes Relay's own MCP server. Zero until the gateway ships
	// (build order step 6), and the MCP step refuses to rewrite anybody's
	// config while it is zero. See mcp.go.
	Gateway MCPGateway
	// GatewayNote is why Gateway is zero, when a caller checked and found
	// nothing answering. Printed by the MCP step rather than by the check, so
	// it lands in context instead of before the installer's first word.
	GatewayNote string

	// ReadCodexAuth overrides detection of an existing Codex CLI login. Nil
	// reads the real machine — the Keychain and CODEX_HOME/auth.json.
	ReadCodexAuth func(llm.CodexOptions) (llm.CodexAuth, error)
	// CodexDeviceLogin and CodexBrowserLogin override the two sign-in flows.
	// Nil runs the real ones, which talk to auth.openai.com and, in the browser
	// case, listen on a loopback port. Both are injected in tests for the
	// obvious reason: neither belongs in one.
	CodexDeviceLogin  func(context.Context, func(llm.CodexDevicePrompt) error) (llm.CodexTokens, error)
	CodexBrowserLogin func(context.Context, func(string) error, func() (string, error)) (llm.CodexTokens, error)

	// Diagnose overrides the model that reads a failed probe out loud. Nil
	// means the real one, which is itself off unless a key for it is in the
	// environment — so a test that sets neither makes no call and asserts on an
	// installer that behaves exactly as it did before the feature existed.
	Diagnose func(context.Context, DiagnoseFacts) string
	// Redact is the secret detector every string is put through before it goes
	// to a model provider. Nil means the measured one in internal/index.
	Redact summarize.Redactor

	// BinaryPath is where relayd was installed, for the service unit.
	BinaryPath string
	// ServiceName overrides the unit name, for tests.
	ServiceName string
	// SkipService skips boot registration.
	SkipService bool
	// UID is the launchd domain to bootstrap into. Zero means this process's.
	UID int

	// nodeAsk is the once-per-run guard on the Node question. A pointer because
	// Options is passed by value everywhere in this package, and a second step
	// finding Node still missing means the user already said no.
	nodeAsk *bool

	// codexRun remembers a ChatGPT sign-in this run performed, so the second
	// model does not ask for a second one. Signing in is the slowest thing in
	// the whole install — a code typed on a phone, a browser round trip — and
	// asking twice for the same account is asking the user to do the slow part
	// again to arrive where they already are. Pointer for the same reason as
	// nodeAsk.
	codexRun *codexMemory

	Now  func() time.Time
	Rand io.Reader
	Log  *slog.Logger
}

func (o Options) nodeAsked() bool { return o.nodeAsk != nil && *o.nodeAsk }

// codexMemory is one ChatGPT login, held for the length of a run.
type codexMemory struct {
	out codexOutcome
	ok  bool
}

// rememberCodex records a sign-in so later questions can offer it.
func (o Options) rememberCodex(out codexOutcome) {
	if o.codexRun != nil {
		o.codexRun.out, o.codexRun.ok = out, true
	}
}

// recallCodex returns the sign-in this run already performed, if any.
func (o Options) recallCodex() (codexOutcome, bool) {
	if o.codexRun == nil || !o.codexRun.ok {
		return codexOutcome{}, false
	}
	return o.codexRun.out, true
}

func (o Options) markNodeAsked() {
	if o.nodeAsk != nil {
		*o.nodeAsk = true
	}
}

// Vault is the slice of the credential vault this package needs. MEMORY.md §6
// owns the rest of it.
type Vault interface {
	Put(ctx context.Context, in vault.Input) (vault.Entry, error)
}

// Result is what the run did. It is the return value the CLI prints and the
// thing tests assert against.
type Result struct {
	Report detect.Report

	// AccessRequested is exactly what this install took. ORCHESTRATOR.md §4b:
	// baseline only, and a test asserts this list never grows here.
	AccessRequested []string

	Runtimes RuntimeOutcome
	Voice    VoiceOutcome
	Models   ModelsOutcome
	// Bus is OpenClaw's Gateway, which runs the agent sessions.
	Bus BusOutcome
	// ShellPath is what was done about ~/.local/bin not being on the user's PATH.
	ShellPath ShellPathOutcome
	Embedding EmbeddingOutcome
	MCP       MCPOutcome
	Service   ServiceOutcome
	// Relay is SYSTEM.md §7's answer to "can my phone reach this from outside
	// the house". Empty is a real choice, not a failure.
	Relay RelayOutcome
	// Discovery is MEMORY.md §6's config scan: what it proposed, and what it
	// could not read.
	Discovery DiscoveryOutcome
	// Entitlements is MEMORY.md §8's declaration: what the user says they pay
	// for, which is the one input the routing table never had.
	Entitlements EntitlementsOutcome

	Pairing  string
	Backfill BackfillNotice

	Config     config.Config
	ConfigPath string

	// Warnings are everything that went wrong without being fatal. An installer
	// that aborts half way is worse than one that finishes and says what it
	// could not do.
	Warnings []string
}

// OK reports whether every credential this install configured actually works.
//
// The embedding step counts, and "none" counts as working: MEMORY.md §3's
// lexical-only mode is a supported configuration that says so on every query,
// and reporting a choice somebody made on purpose as a failed install would be
// wrong about the thing that matters.
func (r Result) OK() bool { return r.Voice.OK() && r.Models.OK() && r.Embedding.OK() }

// BackfillNotice is what the installer says about the history import, after the
// pairing code has already printed.
type BackfillNotice struct {
	Runtimes []adapter.Runtime
	Bytes    int64
	// Started is false here: the installer hands the work to relayd rather than
	// doing it, because backfill is incremental, resumable, and an hour or two
	// of work that must not stand between a person and a working device.
	Started bool
	Message string
}

// BaselineAccess is ORCHESTRATOR.md §4b's entire install-time ask.
//
// Shell, filesystem and process control are already implied by the user running
// this command on their own machine. Everything else — Gmail, a calendar, a
// printer on the LAN — waits, and is proposed later in context from something
// observed. A consent sheet with fourteen items is not read; it is abandoned or
// blind-accepted, and neither outcome is one you want to defend later.
func BaselineAccess() []string {
	return []string{"shell", "filesystem", "process control"}
}

// accessBody is ORCHESTRATOR.md §4b's disclosure, cut to what a person reads.
//
// It used to be four sentences about grant granularity and read/write splits.
// All of it was true and none of it was needed here: nothing on this screen is
// being granted, so the paragraph explaining how grants work was answering a
// question nobody had yet. What is left is the two facts that matter before the
// first question — what this takes now, and that it takes nothing else.
const accessBody = `Shell, filesystem and processes on this machine, under your account.

Nothing else — no mail, no calendar, no repos. Those get asked for later, one at a time, ` +
	`when something you did makes one useful.`

// stopIfCancelled turns a cancelled context into the end of the run.
//
// Ctrl-C cancels the context but does not, on its own, stop anything: the
// installer kept walking its steps with every exec and every network call
// failing instantly, asking four more questions and reporting "did not install:
// context canceled" as though npm had refused. Seen on the first clean-machine
// run, where somebody interrupted at the Claude Code prompt and the installer
// carried on to the voice menu. An interrupt is an answer, and the answer is no.
func stopIfCancelled(ctx context.Context, p Prompter) error {
	if err := ctx.Err(); err != nil {
		p.Say("\n  Stopped. Nothing further was installed, and what you already " +
			"answered is not saved — `relay setup` starts again from the top.")
		return err
	}
	return nil
}

// Run performs the install.
func Run(ctx context.Context, opts Options) (Result, error) {
	opts = opts.withDefaults()
	res := Result{
		ConfigPath:      opts.ConfigPath,
		Config:          opts.Config,
		AccessRequested: BaselineAccess(),
	}

	p := opts.Prompt
	// No preamble. Telling somebody what they are about to do is not doing it,
	// and "this takes a few minutes, the slow part is authentication" is a
	// sentence they can only act on by waiting. The first thing on screen is
	// the first thing that is true.
	p.Section("Relay", "")

	// 1. Say what this takes, before it takes anything.
	p.Section("What this install takes", accessBody)

	if err := stopIfCancelled(ctx, p); err != nil {
		return res, err
	}

	// 2. Detect — after putting an earlier run's work back on PATH, so that a
	// second run reports what is installed rather than what it can currently see.
	restorePath(opts)
	rep := detect.Detect(ctx, opts.Env, opts.Detect)
	res.Report = rep
	reportTo(p, rep)

	if err := stopIfCancelled(ctx, p); err != nil {
		return res, err
	}

	// 3. Offer to install the rest.
	rt, err := offerRuntimes(ctx, opts, rep)
	if err != nil {
		return res, err
	}
	res.Runtimes = rt
	res.Warnings = append(res.Warnings, rt.Warnings...)

	if err := stopIfCancelled(ctx, p); err != nil {
		return res, err
	}

	// 4. Voice.
	v, err := verifyVoice(ctx, opts)
	if err != nil {
		return res, err
	}
	res.Voice = v
	res.Config.Voice = v.Config
	res.Warnings = append(res.Warnings, v.Warnings...)

	if err := stopIfCancelled(ctx, p); err != nil {
		return res, err
	}

	// 5. The two models.
	m, err := chooseModels(ctx, opts)
	if err != nil {
		return res, err
	}
	res.Models = m
	res.Config.Models = m.Config
	res.Warnings = append(res.Warnings, m.Warnings...)

	if err := stopIfCancelled(ctx, p); err != nil {
		return res, err
	}

	// 5a. The bus. Still after the models, but no longer because it borrows
	// their credential: it does not, and trying to cost a whole install (see
	// bus.go rule 2). It stays here because the model menu has just explained
	// which plan powers which runtime, and the Gateway is the second consumer
	// of that same answer.
	bus, err := chooseBus(ctx, opts, rep, m)
	if err != nil {
		return res, err
	}
	res.Bus = bus
	res.Config.Bus = bus.Config
	res.Warnings = append(res.Warnings, bus.Warnings...)

	// The MCP step can only point runtimes at a gateway that exists, and until
	// now nothing ever set one — so it recorded an inventory and changed
	// nothing, on every install. See mcp.go's Zero check.
	if opts.Gateway.Zero() && bus.Config.URL != "" {
		opts.Gateway = MCPGateway{
			Name:    "relay",
			Command: "openclaw",
			Args:    []string{"mcp", "serve"},
		}
	}

	if err := stopIfCancelled(ctx, p); err != nil {
		return res, err
	}

	// 5b. MEMORY.md §6's second arrival path, and the only moment it can
	// honestly run: the runtimes' own config files are enumerable now, with the
	// user watching. It proposes and never stores — see discover.go.
	res.Discovery = discoverConfigKeys(ctx, opts)

	if err := stopIfCancelled(ctx, p); err != nil {
		return res, err
	}

	// 5c. MEMORY.md §8's missing input. It sits here, after the model menu, on
	// purpose: the menu has just printed claudePreamble — "your Claude Max plan
	// still powers Claude Code and cannot power our orchestrator" — which is
	// exactly the context this question needs, and the subscription auth rows
	// the user may have just picked are quoted back rather than promoted into
	// an answer. See entitlements.go.
	ents, err := chooseEntitlements(ctx, opts, rep, rt.Installed, m)
	if err != nil {
		return res, err
	}
	res.Entitlements = ents
	res.Config.Routing.Entitlements = ents.Entitlements

	if err := stopIfCancelled(ctx, p); err != nil {
		return res, err
	}

	// 6. The embedding model, before the pairing code and therefore before the
	// backfill. The width cannot change once the index exists.
	opts.Config = res.Config
	emb, err := chooseEmbedding(ctx, opts)
	if err != nil {
		return res, err
	}
	res.Embedding = emb
	res.Warnings = append(res.Warnings, emb.Warnings...)

	if err := stopIfCancelled(ctx, p); err != nil {
		return res, err
	}

	// 7. MCP, reconciled from five.
	mcp, err := reconcileMCP(ctx, opts, rep)
	if err != nil {
		return res, err
	}
	res.MCP = mcp
	res.Warnings = append(res.Warnings, mcp.Warnings...)

	if err := stopIfCancelled(ctx, p); err != nil {
		return res, err
	}

	// 7b. The relay, before the config is written and therefore before the
	// pairing code — a phone that pairs over the relay needs the daemon to
	// already be dialling it. Until this step existed, `relay setup` wrote every
	// section of the config except this one, and the feature could only be
	// turned on by editing a file the user had never opened.
	relayChoice, err := chooseRelay(ctx, opts)
	if err != nil {
		return res, err
	}
	res.Relay = relayChoice
	res.Config.Relay.URL = relayChoice.URL
	res.Warnings = append(res.Warnings, relayChoice.Warnings...)

	// Record what detection found, so relayd does not re-derive it and so an
	// OpenClaw state directory we had to ask for is not assumed next time.
	res.Config.Runtimes = runtimeConfig(rep, res.Config.Runtimes)

	res.Config.Embedding = emb.Config
	if err := writeConfig(opts.FS, opts.ConfigPath, res.Config); err != nil {
		return res, fmt.Errorf("install: write %s: %w", opts.ConfigPath, err)
	}
	p.Say("\nConfiguration written to %s", opts.ConfigPath)
	p.Say("  Credentials are stored as references, not values. Nothing in that file is a secret.")

	// 8. Boot registration.
	if !opts.SkipService {
		svc, err := registerService(ctx, opts)
		if err != nil {
			res.Warnings = append(res.Warnings, err.Error())
		}
		res.Service = svc
		// The service's own warnings belong in the final list too: "the unit is
		// written but not enabled" is exactly the kind of thing that gets
		// scrolled past and then discovered after a reboot.
		res.Warnings = append(res.Warnings, svc.Warnings...)
		reportService(p, svc)
	}

	// 8b. And make it typeable. Everything above installed things into
	// ~/.local/bin and then told the user to run them; this is where that stops
	// being a lie on a machine whose shell has never looked there.
	sp, err := offerShellPath(opts)
	if err != nil {
		return res, err
	}
	res.ShellPath = sp
	res.Warnings = append(res.Warnings, sp.Warnings...)

	// 9. The pairing code — before any long-running work.
	code, err := PairingCode(opts.Rand)
	if err != nil {
		return res, err
	}
	res.Pairing = code
	p.Section("Pair your glasses", "Open the Relay app and enter this code. It is valid for 10 minutes.")
	p.Say("\n      %s\n", code)

	// 10. Backfill, last, and announced rather than awaited.
	res.Backfill = backfillNotice(rep, emb)
	p.Section("Your history", res.Backfill.Message)

	summarise(p, res)
	return res, nil
}

func (o Options) withDefaults() Options {
	if o.Prompt == nil {
		o.Prompt = &Auto{}
	}
	if o.FS == nil {
		o.FS = detect.OSWriteFS{}
	}
	if o.nodeAsk == nil {
		asked := false
		o.nodeAsk = &asked
	}
	if o.codexRun == nil {
		o.codexRun = &codexMemory{}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Rand == nil {
		o.Rand = rand.Reader
	}
	if o.Log == nil {
		o.Log = logx.Discard()
	}
	if o.Config.Listen == "" {
		o.Config = config.Default()
	}
	if o.Env.GOOS == "" {
		o.Env.GOOS = detect.OS().GOOS
	}
	if o.UID == 0 {
		o.UID = osUID()
	}
	return o
}

// reportTo prints the detection table. MEMORY.md §1's own shape: what is here,
// how much of it there is, and which runtime dominates.
func reportTo(p Prompter, rep detect.Report) {
	p.Section("What is already here", rep.Summary())
	for _, f := range rep.Findings {
		line := fmt.Sprintf("  %-12s %s", f.Label, f.Status().Line())
		if f.Version != "" {
			line += "  " + f.Version
		}
		p.Say("%s", line)

		if n, ok := f.SessionCount(); ok && n > 0 {
			p.Say("               %s · %s", plural2n(n, "session"), humanBytes(f.StoreBytes))
		} else if b, ok := f.Bytes(); ok && b > 0 {
			p.Say("               %s · %s", humanBytes(f.StoreBytes), f.SessionsNote)
		}
		if f.StateDirExists && !f.StateDirSource.Trusted() {
			p.Say("               state dir assumed: %s (%s)", f.StateDir, f.StateDirDetail)
		}
		for _, n := range f.Notes {
			p.Say("               %s", wrapIndent(n, 15, 76))
		}
		if len(f.Running) > 0 {
			p.Say("               running now (pid %d)", f.Running[0].PID)
		}
	}
}

// runtimeConfig records what detection found, including a state directory that
// had to be asked for.
func runtimeConfig(rep detect.Report, existing map[string]config.RuntimeConfig) map[string]config.RuntimeConfig {
	out := map[string]config.RuntimeConfig{}
	for k, v := range existing {
		out[k] = v
	}
	for _, f := range rep.Findings {
		rc := out[string(f.Runtime)]
		rc.Enabled = f.Installed
		if f.BinaryPath != "" {
			rc.Command = f.BinaryPath
		}
		// Only a state directory we were actually told about is worth writing
		// down. Persisting a guess would turn it into a fact next run.
		if f.StateDirSource.Trusted() {
			rc.StateDir = f.StateDir
		}
		out[string(f.Runtime)] = rc
	}
	return out
}

func backfillNotice(rep detect.Report, emb EmbeddingOutcome) BackfillNotice {
	n := BackfillNotice{}
	for _, f := range rep.WithHistory() {
		n.Runtimes = append(n.Runtimes, f.Runtime)
		if b, ok := f.Bytes(); ok {
			n.Bytes += b
		}
	}
	if len(n.Runtimes) == 0 {
		n.Message = "No agent history found yet. Relay will index sessions as you have them."
		return n
	}
	names := make([]string, 0, len(n.Runtimes))
	for _, r := range n.Runtimes {
		names = append(names, string(r))
	}
	n.Message = fmt.Sprintf(
		"Indexing %s of history from %s in the background. Interrupting it costs nothing, "+
			"and Relay stores summaries, never copies.",
		humanBytes(&n.Bytes), strings.Join(names, ", "))

	// Say which half of retrieval this run is building, because it is the thing
	// that will be noticed later and it is fixable now.
	switch {
	case emb.Kind == EmbedNone:
		n.Message += "\n\nNo embedding model is configured, so this builds the keyword half only. " +
			"That is a working index — titles, repos, dates and every word of every summary — " +
			"and `relay embed` adds the meaning half later. Doing it later means re-embedding " +
			"what has already been indexed, which is minutes, not the hour or two this is."
	case emb.Kind == EmbedLocal:
		n.Message += fmt.Sprintf("\n\nSummaries are embedded on this machine with %s, so none of "+
			"them leave it.", emb.Model)
	case emb.Kind == EmbedHosted:
		n.Message += fmt.Sprintf("\n\nSummaries are embedded by %s, which means they are sent "+
			"there — once each, as they are indexed.", emb.Provider)
	}
	return n
}

func summarise(p Prompter, res Result) {
	p.Section("Done", "")
	p.Say("  voice     %s", res.Voice.Line())
	p.Say("  small     %s", res.Models.Small.Line())
	p.Say("  big       %s", res.Models.Big.Line())
	p.Say("  embedding %s", res.Embedding.Line())
	p.Say("  bus       %s", res.Bus.Line())
	if res.MCP.Line() != "" {
		p.Say("  mcp       %s", res.MCP.Line())
	}
	if res.Service.Line() != "" {
		p.Say("  boot      %s", res.Service.Line())
	}
	if res.Discovery.line() != "" {
		p.Say("  keys      %s", res.Discovery.line())
	}
	p.Say("  plans     %s", res.Entitlements.line())
	if len(res.Warnings) > 0 {
		p.Say("\n%d thing(s) need your attention:", len(res.Warnings))
		for _, w := range res.Warnings {
			p.Say("  - %s", wrapIndent(w, 4, 76))
		}
	}
}

// writeConfig encodes the config through the filesystem seam so a test never
// touches a real path. 0600, because it names credential references and the
// listen address.
func writeConfig(fsys detect.WriteFS, path string, cfg config.Config) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		return err
	}
	if err := fsys.MkdirAll(dirOf(path), 0o700); err != nil {
		return err
	}
	return fsys.WriteFile(path, buf.Bytes(), 0o600)
}

func dirOf(p string) string {
	i := strings.LastIndex(strings.TrimRight(p, "/"), "/")
	if i <= 0 {
		return "."
	}
	return p[:i]
}

// pairingAlphabet has no 0/O/1/I: this gets read aloud and typed on a phone.
const pairingAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

// PairingCode returns a code in the form XXXX-XXXX.
func PairingCode(r io.Reader) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	out := make([]byte, 0, 9)
	for i := 0; i < 8; i++ {
		n, err := rand.Int(r, big.NewInt(int64(len(pairingAlphabet))))
		if err != nil {
			return "", fmt.Errorf("install: pairing code: %w", err)
		}
		if i == 4 {
			out = append(out, '-')
		}
		out = append(out, pairingAlphabet[n.Int64()])
	}
	return string(out), nil
}

func plural2n(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func humanBytes(b *int64) string {
	if b == nil {
		return "size unknown"
	}
	v := *b
	switch {
	case v >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(v)/(1<<30))
	case v >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(v)/(1<<20))
	case v >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(v)/(1<<10))
	default:
		return fmt.Sprintf("%d B", v)
	}
}

func wrapIndent(s string, indent, width int) string {
	pad := strings.Repeat(" ", indent)
	return strings.ReplaceAll(wrap(s, width-indent), "\n", "\n"+pad)
}

// reasonAdvice turns a probe reason into the next thing to do about it. The
// installer's job is not to classify a failure, it is to end the failure.
func reasonAdvice(r llm.Reason) string {
	switch r {
	case llm.ReasonMissingCredential:
		return "nothing is configured for it yet"
	case llm.ReasonUnresolvedRef:
		return "the reference does not lead anywhere — check the variable, file or command"
	case llm.ReasonExpired:
		return "the provider rejected the credential — rotate it and run `relay models` again"
	case llm.ReasonUnavailable:
		return "not a credential problem: the call did not come back with a completion. " +
			"Usually a wrong model id, a rate limit, an outage, or this machine's outbound " +
			"network — do not rotate the key yet"
	}
	return ""
}
