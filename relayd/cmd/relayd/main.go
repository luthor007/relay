// Command relayd is the orchestrator: one static binary, no runtime to install.
//
// That is the pricing story rather than a preference (SYSTEM.md §8). It opens
// one SQLite file, drives whichever agent runtimes are installed, keeps one
// session list across all of them, and serves the phone and the console on
// loopback.
//
//	relayd                      # 127.0.0.1:8787, token printed on start
//	relayd --lan                # exposed on this network, on purpose
//	relayd --quiet-hours 22:00-07:00
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/luthor007/relay/relayd/internal/api"
	"github.com/luthor007/relay/relayd/internal/audit"
	"github.com/luthor007/relay/relayd/internal/bus"
	"github.com/luthor007/relay/relayd/internal/config"
	"github.com/luthor007/relay/relayd/internal/logx"
	"github.com/luthor007/relay/relayd/internal/orchestrator"
	"github.com/luthor007/relay/relayd/internal/pairing"
	"github.com/luthor007/relay/relayd/internal/registry"
	"github.com/luthor007/relay/relayd/internal/store"
	"github.com/luthor007/relay/relayd/internal/vault"
)

// auditFile opens the on-disk audit log, degrading to an in-memory one rather
// than refusing to start.
//
// The degradation is logged at warn precisely because it matters: DASHBOARD.md
// §4 says the value of the log is that the evidence exists in a place the user
// can see without our help, and an in-memory log loses it on restart. Better a
// daemon that runs and says so than one that will not start.
func auditFile(cfg config.Config, log *slog.Logger) audit.Log {
	path, err := cfg.AuditPath()
	if err == nil {
		var f *audit.File
		if f, err = audit.OpenFile(path); err == nil {
			return f
		}
	}
	log.Warn("relayd: audit log is in memory only; it will not survive a restart",
		"error", err)
	return audit.NewMemory()
}

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

// vaultOpen is [vault.Open], indirected so the wiring tests can start the real
// daemon without it reaching for the developer's own OS keychain.
//
// This is not a general-purpose hook and nothing but a test should replace it.
// It exists because the tests below run the real run() in-process, and
// vault.Open defaults to the real keychain under the service name "relay" — the
// same name a production install uses. On Linux that is invisible: no D-Bus, so
// the probe fails instantly and the vault degrades to the file backend, which is
// why the whole suite was green in the container this was written in. On macOS
// the identical call is interactive: it raises a system dialog and blocks the
// suite on a human, and anything it does store lands in that human's login
// keychain. A test must not depend on, or write to, the machine it runs on.
var vaultOpen = vault.Open

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], nil); err != nil {
		fmt.Fprintln(os.Stderr, "relayd:", err)
		os.Exit(1)
	}
}

type flags struct {
	configPath string
	dataDir    string
	listen     string
	lan        bool
	token      string
	quietHours string
	logLevel   string
	logFormat  string
	showVer    bool
}

func parseFlags(args []string) (flags, error) {
	var f flags
	fs := flag.NewFlagSet("relayd", flag.ContinueOnError)
	fs.StringVar(&f.configPath, "config", "", "path to config.toml (default: the platform config dir)")
	fs.StringVar(&f.dataDir, "data-dir", "", "where relay.db and vault.db live")
	fs.StringVar(&f.listen, "listen", "", "address to serve on (default 127.0.0.1:8787)")
	fs.BoolVar(&f.lan, "lan", false,
		"serve on the network rather than only this machine — read the warning before using it")
	fs.StringVar(&f.token, "token", "", "API token (default: a fresh one, printed on start)")
	fs.StringVar(&f.quietHours, "quiet-hours", "",
		"do not speak completions in this window, e.g. 22:00-07:00 (blocked sessions still ping)")
	fs.StringVar(&f.logLevel, "log-level", "", "debug | info | warn | error")
	fs.StringVar(&f.logFormat, "log-format", "", "text | json")
	fs.BoolVar(&f.showVer, "version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	return f, nil
}

// run is main with its two untestable edges injected: the context that a signal
// cancels, and a callback fired once the listener is up so a test can find the
// port it was given.
func run(ctx context.Context, args []string, ready func(net.Addr)) error {
	f, err := parseFlags(args)
	if err != nil {
		return err
	}
	if f.showVer {
		fmt.Println("relayd", version)
		return nil
	}

	cfgPath := f.configPath
	if cfgPath == "" {
		cfgPath, err = config.Path()
		if err != nil {
			return err
		}
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if f.listen != "" {
		cfg.Listen = f.listen
	}
	if f.dataDir != "" {
		cfg.DataDir = f.dataDir
	}
	if f.logLevel != "" {
		cfg.Log.Level = f.logLevel
	}
	if f.logFormat != "" {
		cfg.Log.Format = f.logFormat
	}

	log := logx.New(logx.Options{Level: cfg.Log.Level, Format: cfg.Log.Format})

	// DASHBOARD.md §4: loopback by default, exposure is a deliberate flag with a
	// warning. A config file that quietly says 0.0.0.0 does not count as
	// consent, so this refuses rather than obeys.
	if err := api.CheckBind(cfg.Listen, f.lan); err != nil {
		return err
	}

	quiet, err := bus.ParseQuietHours(f.quietHours)
	if err != nil {
		return err
	}

	dbPath, err := cfg.DBPath()
	if err != nil {
		return err
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	eventBus := bus.New(bus.Options{Log: log})
	reg, err := registry.New(registry.Options{DB: db, Bus: eventBus, Log: log})
	if err != nil {
		return err
	}

	// A row that says "running" after a restart is a lie with a plausible shape.
	// Detach them before anything reads the list.
	rec, err := reg.Recover(ctx)
	if err != nil {
		return err
	}
	if n := len(rec.Detached); n > 0 {
		log.Warn("relayd: detached sessions left over from a previous run",
			"count", n, "detail", "they are idle now; resume one to keep working on it")
	}

	started := startAdapters(ctx, reg, cfg, log)

	// The vault is a different file from the main database and shares nothing
	// with it (MEMORY.md §6). A machine with no keychain still gets a vault —
	// the file backend — because refusing to start over a missing keyring would
	// take out the whole daemon for one subsystem.
	// secrets is the privileged handle and credentials is the display one.
	// They are the same object and the split is deliberate: api.CredentialStore
	// has no Reveal, so the HTTP surface cannot return a secret even by
	// mistake, and the one caller that legitimately needs plaintext — resolving
	// a "vault:" model credential — has to name vault.Vault to get it.
	var secrets vault.Vault
	var credentials api.CredentialStore
	if vaultPath, err := cfg.VaultPath(); err != nil {
		log.Warn("relayd: no vault path; credential screens will be empty", "error", err)
	} else if v, err := vaultOpen(ctx, vault.Options{DBPath: vaultPath}); err != nil {
		log.Warn("relayd: vault unavailable; credential screens will be empty", "error", err)
	} else {
		defer v.Close()
		secrets, credentials = v, v
	}

	// Every credential and connector mutation, on disk, displayed by the console
	// itself (DASHBOARD.md §4). A memory log would satisfy the interface and
	// lose the evidence on restart, which is the one thing an audit log is for.
	auditLog := auditFile(cfg, log)

	gate := bus.NewSpeechGate()

	// ORCHESTRATOR.md §3b, wired: the small model speaks and the big one holds
	// the tools. Both are optional — an install with no keys still lists
	// sessions, still pings, and still answers the allowlist, and says so when
	// asked for anything that needs a model.
	// ORCHESTRATOR.md §4b's shared bus, constructed: one MCP gateway, grant-gated,
	// carrying the skills the orchestrator writes for itself. This is the half
	// the daemon was missing — reconcileMCP had nothing real to point the five
	// runtimes at, so it enumerated them and changed nothing.
	// Skills are exported next to the database, so a directory another agent can
	// be pointed at lives with the rest of the machine's Relay state.
	skillsDir := filepath.Join(filepath.Dir(dbPath), orchestrator.SkillsDirName)

	lookups := credentialLookup(secrets, log)

	// ORCHESTRATOR.md §4b's set: what could be connected on this machine. It is
	// built before the bus because the bus serves its tools — without that
	// registration, accepting a proposal would write a grant row and change
	// nothing any runtime can see.
	connectors, connectorsWhy := buildConnectors(cfg, lookups, log)
	tools := newToolBus(ctx, db, auditLog, reg, connectors, skillsDir, log)

	// The proposer holds the read-only mcp.Grants and never the *Grants that
	// could record one. Rule 1 — nothing is auto-granted, not on install, not on
	// suggestion, not ever — is therefore a property of what this object can
	// reach rather than of anyone's manners.
	proposer := newProposer(cfg, connectors, tools, db, log)

	small, big := buildModels(cfg, lookups, log)
	orch, err := orchestrator.New(orchestrator.Options{
		Small: small,
		Big:   big,
		Deps: orchestrator.Deps{
			Sessions:     orchestrator.FromRegistry(reg),
			Capabilities: tools.capabilities(),
			Skills:       tools.skillBook(),
			// MEMORY.md §5's durable facts, with a writer at last. The tier
			// already enforces the five rules — evidence required, decay on
			// last observation, contradictions supersede, editable, nothing
			// secret — so this is an adapter and not a second store.
			Notebook: notebook(db, log),
		},
		Emit: eventBus.Publish,
		Log:  log,
	})
	if err != nil {
		log.Warn("relayd: no work model; utterances that need one will say so",
			"error", err)
		orch = nil
	}

	// ORCHESTRATOR.md §4, wired: a spoken sentence reaches the router, the router
	// announces its choice before acting, and undo moves the last turn. All three
	// were implemented and none of them were reachable.
	//
	// MEMORY.md §8's entitlements come in here as data the user declared. They
	// are read from the config and never inferred: a Claude Code binary on PATH
	// is not evidence of a Claude Max plan, and an entitlement outranks
	// capability comparison, so a guessed one spends money on a runtime the
	// user does not pay for.
	utterances := newUtteranceRouter(ctx, reg, orch, entitlements(cfg, log), proposer, log)

	// The token the phone will present, resolved before the server exists so a
	// machine with nowhere to keep it fails loudly here rather than serving an
	// API that nothing can pair with.
	apiToken, err := pairing.Token(f.token, cfg.DataDir)
	if err != nil {
		return fmt.Errorf("relayd: no API token: %w", err)
	}

	srv, err := api.New(api.Options{
		Registry: reg,
		// Without this the console renders empty in the shipped daemon while
		// every api test passes, because the tests pass a DB and main did not.
		// That gap is why internal/e2e exists.
		DB:          db,
		Credentials: credentials,
		// MEMORY.md §6's confirmation queue, wired. Without this the console
		// listed the index's raw secret markers and answered every accept with
		// 501, so no proposal had ever been accepted end to end — the screens
		// were live and the flow behind them was not.
		Proposals: proposals(secrets),
		// ORCHESTRATOR.md §4b's queue, which is a different thing from the one
		// above: that asks whether a key found in a transcript should be saved,
		// this asks whether a service the user keeps mentioning should be
		// connected. Without this field the route answers 503 with the reason,
		// so a proposal could be made and never seen.
		ConnectorProposals: connectorProposalHandler(proposer, tools, connectors),
		Audit:              auditLog,
		Utterances:         utteranceHandler(utterances),
		Gateway:            tools.handler(),
		Gate:               gate,
		// Durable, not fresh-per-start. A token that changed on every restart
		// was a phone that unpaired itself at every reboot — and the only place
		// it was ever published was a log line at startup, which under launchd
		// nobody reads. See internal/pairing.
		Token:     apiToken,
		Listen:    cfg.Listen,
		LAN:       f.lan,
		Version:   version,
		StartedAt: time.Now(),
		Log:       log,
	})
	if err != nil {
		return err
	}

	// ADAPTERS.md §7's policy, wired: the bus feeds the pinger, the pinger
	// decides, and the API delivers. Nothing in between re-decides.
	pinger := bus.NewPinger(bus.PingOptions{
		Delivery: srv,
		Gate:     gate,
		Quiet:    quiet,
		// The ping says "payments is done", not a uuid. An unnamed session says
		// "session 3f2a…" rather than an invented subject.
		Namer: reg.Subject,
		Log:   log,
	})
	srv.SetPinger(pinger)

	// Every subsystem says whether it is on, and why not when it is not. See
	// api.Health.Subsystems: this is how "built, tested, and constructed by
	// nothing" becomes a line on a screen instead of a silence.
	srv.SetSubsystem("tool_bus", statusOf(tools != nil, "no database or audit log"))
	srv.SetSubsystem("work_model", statusOf(big != nil, "no big model configured"))
	srv.SetSubsystem("voice_model", statusOf(small != nil, "no small model configured; narration uses templates"))
	srv.SetSubsystem("orchestrator", statusOf(orch != nil, "no work model, so nothing escalates"))
	srv.SetSubsystem("credential_proposals",
		statusOf(secrets != nil, "no vault, so nothing can be proposed or accepted"))
	// MEMORY.md §8, both halves, and they are reported separately because they
	// fail separately: the router can exist with nothing recorded, and a set
	// can be recorded with no router to consult it.
	//
	// Both strings are computed from the CONSTRUCTED router, never from cfg.
	// Reporting what a local variable holds would leave the health screen
	// claiming an entitlement after the join carrying it into the router was
	// deleted — this codebase's own defect, reproduced inside the report that
	// exists to catch it.
	srv.SetSubsystem(SubsystemRuntimeRouting, runtimeRoutingStatus(utterances))
	srv.SetSubsystem(SubsystemEntitlements, entitlementStatus(utterances))
	// ORCHESTRATOR.md §4b. Read from the CONSTRUCTED proposer for the same
	// reason the two lines above are: a status computed from cfg would keep
	// saying "on" after the join carrying the set into the daemon was deleted.
	srv.SetSubsystem(SubsystemConnectorProposals,
		connectorProposalStatus(proposer, tools, connectorsWhy))

	// SYSTEM.md §7. The daemon dials the relay rather than listening for it, so
	// this is what makes a box behind NAT reachable at all — and it is the one
	// subsystem whose health nobody can check from inside their own house.
	relayLink, relayWhy := startRelay(ctx, cfg, cfg.DataDir, srv, log)
	srv.SetSubsystem(SubsystemRelay, relayStatus(relayLink, relayWhy))

	// APP-PLATFORM.md. `internal/apps` was thirty files of tested runtime that
	// this file did not import: an app could be installed, its permissions
	// recorded, and nothing on the machine could ever trigger it, because
	// appstore.Provisioner had no implementation anywhere.
	platform, appsWhy := startApps(ctx, cfg.DataDir, newPhoneScreen(srv), log)
	srv.SetSubsystem(SubsystemApps, platform.status(appsWhy))
	utterances.SetApps(platform)

	// Server.Speak is the announcement path — it exists for exactly this and its
	// own comment says so. Closing the loop here rather than in api keeps the
	// dependency pointing one way: the API carries the utterance, this decides
	// what it means.
	if utterances != nil {
		utterances.setSpeaker(srv.Speak)
	}

	// MEMORY.md §4's live ingestion, wired: every completed turn writes a
	// summary, updates the session row, and re-runs fact extraction for that
	// session. summarize.Live and facts.Bridge were both written and both
	// constructed by nothing, so the automatic half of memory did not exist —
	// only the remember tool the model has to choose to call.
	memoryDone := startMemory(ctx, memoryDeps{
		db: db, bus: eventBus, cfg: cfg,
		small: small, big: big,
		// The embedder resolves its own credential, and until this was threaded
		// through it had no resolver at all — so an `embedding.credential =
		// "vault:<id>"` was unresolvable in the shipped daemon, silently, and
		// search fell back to lexical-only for a reason nothing reported.
		lookups: lookups,
		// The second §4b evidence feed. See the comment at the call site in
		// memory.go: it is real and it is unobserved in a container with no
		// agent runtime installed, and the row says so rather than counting it.
		proposer: proposer,
		report:   srv.SetSubsystem, log: log,
	})

	// MEMORY.md §9's idle pass, wired: compact at ~70% when a session is quiet,
	// never mid-turn, and never by disabling any runtime's own auto-compaction.
	// internal/compaction was 5,490 tested lines that nothing constructed, so
	// nothing compacted anything and a long session died at its own window.
	compactionDone := startCompaction(ctx, reg, db, srv.SetSubsystem, log)

	// Filter{Pings: true} is the whole subscription: the three kinds that reach
	// a user unprompted, and a replayed one already reports PingNone, so
	// reattaching to a session cannot fire a completion ping for every turn in
	// its history.
	pings := eventBus.Subscribe("pinger", bus.Filter{Pings: true})
	defer pings.Close()
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		_ = pinger.Run(ctx, pings.C())
	}()

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("relayd: listen on %s: %w", cfg.Listen, err)
	}

	// The endpoint is relayd's own listener, and it is only certain now: a
	// config that said :0, or a port already taken, would have made anything
	// derived from cfg.Listen a guess — and a guessed URL written into five
	// runtimes' configs is MEMORY.md §7's hazard rather than a rounding error.
	tools.setEndpoint(ln.Addr().String())

	banner(ln.Addr().String(), f, srv, started, quiet, dbPath)
	if ready != nil {
		ready(ln.Addr())
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx, ln) }()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, net.ErrClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("relayd: shutting down")
	}

	// Graceful: sessions first, then adapters, then the socket. What does not
	// stop inside the deadline is named rather than waited on forever — a hung
	// runtime must not hold the daemon open (SYSTEM.md §6.2).
	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res := reg.Shutdown(shutCtx)
	if len(res.Failed) > 0 || res.TimedOut {
		log.Warn("relayd: some sessions did not stop cleanly",
			"failed", res.Failed, "timed_out", res.TimedOut)
	}
	_ = ln.Close()
	<-pingDone
	compactionDone()
	memoryDone()
	eventBus.Close()
	return nil
}

// banner is the pairing-code moment: the token is printed on start, once, the
// same way the pairing code is (DASHBOARD.md §4).
func banner(addr string, f flags, srv *api.Server, runtimes []string, quiet bus.QuietHours, dbPath string) {
	fmt.Fprintf(os.Stdout, "\nrelayd %s\n", version)
	fmt.Fprintf(os.Stdout, "  console  http://%s/?token=%s\n", addr, srv.Token())
	fmt.Fprintf(os.Stdout, "  token    %s\n", srv.Token())
	fmt.Fprintf(os.Stdout, "  data     %s\n", filepath.Clean(dbPath))
	if len(runtimes) == 0 {
		fmt.Fprintf(os.Stdout, "  runtimes none found — run the installer, or set [runtimes] in the config\n")
	} else {
		fmt.Fprintf(os.Stdout, "  runtimes %v\n", runtimes)
	}
	if quiet.Enabled() {
		fmt.Fprintf(os.Stdout, "  quiet    %s (completions only; a blocked session still pings)\n", quiet)
	}
	if f.lan {
		fmt.Fprintf(os.Stdout, "\n%s\n", api.LANWarning(addr))
	}
	fmt.Fprintln(os.Stdout)
}
