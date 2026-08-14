package install

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/config"
	"github.com/luthor007/relay/relayd/internal/detect"
	"github.com/luthor007/relay/relayd/internal/vault"
)

// The bus step — OpenClaw's Gateway, which drives the agent runtimes.
//
// This is the one step where Relay depends on software it does not ship. That
// is worth saying plainly rather than hiding behind a spinner, because it costs
// something real: relayd is a single static Go binary with no toolchain, and
// this step puts a Node runtime and a few hundred npm packages next to it. The
// trade is that session bookkeeping, turn-taking and the ~17 agent harnesses
// OpenClaw already drives stop being ours to build.
//
// Four rules, and the second one is the one that took a day to learn:
//
//  1. **A configured Gateway is adopted, never re-configured.** Somebody who
//     already runs OpenClaw has models, agents and channels set up. Walking
//     them through it again would be the same mistake as asking which
//     environment variable holds a ChatGPT subscription: the machine can answer
//     this, so it should.
//  2. **Relay's key is not OpenClaw's key, and none is passed.** This rule used
//     to read "the credential is passed as a REFERENCE, never a value", which
//     was the right instinct about argv and the wrong idea about whose key it
//     is. Relay's is the orchestrator's — the model that routes utterances.
//     The Gateway's runtimes want the machine's own agent logins, and asking
//     the installed 2026.7.1-2 to take a foreign key instead fails the onboard
//     outright rather than degrading. Measured, not reasoned: see busOnboard.
//  3. **Their wizard never runs.** `openclaw onboard --non-interactive` takes
//     every answer as a flag, so Relay asks in Relay's voice, in Relay's steps,
//     and the user never meets a second setup.
//  4. **Their daemon owns the process; Relay owns the contract.** Relay never
//     runs `gateway run` itself — two supervisors on one port is a crash loop,
//     and the daemon's Node path moves with every npm upgrade, which only they
//     track. What Relay owns is that the port answers, and it checks.

// BusOutcome is what the bus step decided.
type BusOutcome struct {
	// Present and Version describe the OpenClaw on this machine, if any.
	Present bool
	Version string
	// Installed is true when this run put it there.
	Installed bool
	// Adopted is true when an existing configured Gateway was taken as-is.
	Adopted bool
	// Configured is true when the Gateway can serve: onboarded, with a model.
	Configured bool
	// Port is where the Gateway listens, read from its own config.
	Port int
	// Live is true when that port answered. Nothing else here proves it runs:
	// a written config is a plan, and this step exists to tell the two apart.
	Live bool
	// Registered is true when this run put the Gateway under its own service
	// manager, so it comes back after a reboot.
	Registered bool
	// Detail is what the last attempt to start it said, quoted rather than
	// summarised.
	Detail string
	// Config is what relayd needs to dial this Gateway: the socket, and a
	// REFERENCE to its token. Zero when there is nothing to dial.
	Config config.Bus
	// AgentAuth names the login the Gateway was configured with, or is empty
	// when it was configured without one. A Gateway with no agent login is a
	// running Gateway that cannot yet run a session, and the two must not read
	// the same in a summary.
	AgentAuth string
	// Skipped records why the step did nothing, when it did nothing.
	Skipped  string
	Warnings []string
}

// OK reports whether the bus is usable.
//
// Skipping is OK. A box with no Gateway still drives Claude Code and Codex
// through Relay's own adapters — that is what those adapters are for, and
// reporting a deliberate skip as a failed install would be wrong about the
// thing that matters.
func (b BusOutcome) OK() bool { return b.Configured || b.Skipped != "" }

// Line is the summary row.
func (b BusOutcome) Line() string {
	switch {
	case b.Skipped != "":
		return "not used — " + b.Skipped
	case b.Live:
		s := fmt.Sprintf("OpenClaw %s — answering on port %d", b.Version, b.Port)
		if b.Registered {
			s += ", and at boot"
		}
		if b.AgentAuth == "" {
			// A Gateway that answers and cannot run a session is not a working
			// bus, and a summary that says "answering" without this reads as one.
			s += ", with no agent login yet"
		}
		return s
	case b.Configured:
		// Said this way round on purpose. "Configured" alone read as working,
		// and the whole point of the health check is that it is not the same
		// claim. It is not started either, on purpose — see chooseBus step 6.
		return "OpenClaw " + b.Version + " — configured, not started; Relay drives Claude Code and Codex directly"
	case b.Present:
		return "OpenClaw " + b.Version + " — present, not configured"
	}
	return "not installed"
}

// nodeRanges is OpenClaw's engines.node, checked here rather than left to an
// npm error nobody reads.
//
// This is not defensive programming for a hypothetical: the machine this was
// written on ran Node 25.8.0, which satisfies none of the three ranges, and
// `npm i -g openclaw` fails on it with a message about engines that reads like
// a bug in Relay. Relay has never had a runtime prerequisite before, so the
// first one it acquires had better explain itself.
var nodeRanges = []struct{ min, max string }{
	{"22.22.3", "23.0.0"},
	{"24.15.0", "25.0.0"},
	{"25.9.0", ""},
}

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// nodeOK reports whether a `node --version` string satisfies OpenClaw.
func nodeOK(v string) bool {
	got, ok := parseVersion(v)
	if !ok {
		return false
	}
	for _, r := range nodeRanges {
		lo, _ := parseVersion(r.min)
		if cmpVersion(got, lo) < 0 {
			continue
		}
		if r.max == "" {
			return true
		}
		hi, _ := parseVersion(r.max)
		if cmpVersion(got, hi) < 0 {
			return true
		}
	}
	return false
}

func parseVersion(s string) ([3]int, bool) {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return [3]int{}, false
	}
	var out [3]int
	for i := 0; i < 3; i++ {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

func cmpVersion(a, b [3]int) int {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] - b[i]
		}
	}
	return 0
}

// nodeAdvice is what to do about a Node that will not run OpenClaw. It names
// the versions rather than saying "upgrade Node", because the acceptable set is
// three disjoint ranges and "latest" is not necessarily in any of them.
const nodeAdvice = "OpenClaw needs Node 22.22.3+, 24.15+ or 25.9+. Install one and re-run " +
	"`relay setup` — `nvm install 24` is the shortest path, and it does not have to become " +
	"your default."

// busVersion is the version out of `openclaw --version`, which answers
// "OpenClaw 2026.7.1-2 (0790d9f)" — a whole sentence, and every line that
// prints it here has already said the name and does not want the commit.
func busVersion(s string) string {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "OpenClaw"))
	if i := strings.Index(s, " ("); i > 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// BusPin is the OpenClaw version Relay installs and expects.
//
// Pinned, not `latest`, and the reason is not caution in the abstract: their
// onboarding changed shape between releases — the stepped wizard is now behind
// `--classic` — and the `--non-interactive` flag surface Relay drives is the
// part that has to stay still. A floor that moves under a dependency is a
// dependency that breaks on someone else's schedule.
const BusPin = "2026.7.1-2"

// busAck is the acknowledgement OpenClaw requires before it will configure
// itself unattended, and which Relay therefore cannot skip.
//
// `--accept-risk` is mandatory for `--non-interactive`. Relay could pass it
// silently, and that would be accepting a risk on behalf of somebody who was
// never shown it. So it is shown. That this is also the security note Relay
// otherwise lacks is a happy accident of the dependency.
const busAck = `The Gateway runs agents on this machine with your account's permissions: your ` +
	`shell, your files, your repos.

Anything reaching it can ask an agent to act. It listens on loopback only, and Relay pairs ` +
	`your phone to itself rather than to it.

It also cannot ask you anything. Driven through the Gateway, Claude Code runs with ` +
	`permissions bypassed or denied outright — measured, not assumed — while Relay's own path ` +
	`stops and asks you before a command runs. So Relay does not route work through it, and ` +
	`installing it now changes nothing about how your agents run.`

func chooseBus(ctx context.Context, opts Options, rep detect.Report, models ModelsOutcome) (BusOutcome, error) {
	p := opts.Prompt
	var out BusOutcome

	// The heading used to say "OpenClaw's Gateway runs the agent sessions. Relay
	// drives it." Relay does not: internal/gateway is imported by nothing and
	// config.Bus is validated on load and never read. relayd runs Claude Code
	// and Codex through internal/adapter, which is also the only path that can
	// ask a human before a command runs.
	p.Section("The agent bus",
		"OpenClaw's Gateway can drive seventeen agent harnesses. Relay does not use it yet — "+
			"it runs Claude Code and Codex directly, which is the only path that stops to ask "+
			"you before a command runs.")

	// 1. What is here?
	found, _ := rep.Get(adapter.OpenClaw)
	out.Present = found.Installed
	out.Version = busVersion(found.Version)

	if out.Present && busConfigured(ctx, opts) {
		out.Adopted, out.Configured = true, true
		p.Say("  Found a configured OpenClaw %s. Using it as it is.", out.Version)
		return busServe(ctx, opts, out)
	}

	// 2. Node, before npm gets a chance to fail confusingly.
	if !out.Present {
		ok, err := ensureNode(ctx, opts, "The bus")
		if err != nil {
			return out, err
		}
		if !ok {
			out.Skipped = "this machine has no Node the bus can run on"
			p.Say("  Relay drives Claude Code and Codex directly meanwhile.")
			return out, nil
		}
	}

	// 3. The acknowledgement, which is theirs to require and ours to show.
	ok, err := p.Confirm(Confirm{
		ID: "bus.ack", Prompt: "Understood?", Body: busAck, Default: true,
	})
	if err != nil {
		return out, err
	}
	if !ok {
		out.Skipped = "you declined"
		p.Say("  Relay will drive Claude Code and Codex directly.")
		return out, nil
	}

	// 4. Install, if it is not here.
	if !out.Present {
		yes, err := p.Confirm(Confirm{
			ID:     "bus.install",
			Prompt: "Install OpenClaw?",
			Body:   "Runs: npm install -g openclaw@" + BusPin,
			// Every other install in this installer defaults to no, and this
			// one is not special enough to be the exception.
			Default: false,
		})
		if err != nil {
			return out, err
		}
		if !yes {
			out.Skipped = "you declined the install"
			return out, nil
		}
		if err := busInstall(ctx, opts, &out); err != nil {
			return out, err
		}
		if !out.Installed {
			return out, nil
		}
	}

	// 5. Configure it, without their wizard. What it is given is decided by
	// chooseBusAuth: the machine's own Claude Code login where there is one, a
	// key the user hands over on purpose, or nothing.
	cc, _ := rep.Get(adapter.ClaudeCode)
	auth, err := chooseBusAuth(ctx, opts, cc.Installed, models)
	if err != nil {
		return out, err
	}
	busOnboard(ctx, opts, auth, &out)
	if !out.Configured {
		return out, nil
	}

	// 6. And stop there. Step 6 used to start it and prove it answered, which
	// was the right shape when the plan was for relayd to dial it. Until
	// something does, starting it buys an idle daemon and costs a process that
	// executes tool calls without asking anyone.
	opts.Prompt.Say("  %s", wrapIndent("Configured, and not started: nothing in Relay dials it "+
		"yet. `openclaw gateway install` runs it at boot when you want it.", 2, 76))
	return out, nil
}

func busInstall(ctx context.Context, opts Options, out *BusOutcome) error {
	res, err := opts.Env.Exec.Run(ctx, detect.Cmd{
		Name: "npm", Args: []string{"install", "-g", "openclaw@" + BusPin},
		Timeout: 10 * time.Minute,
	})
	switch {
	case err != nil:
		w := fmt.Sprintf("OpenClaw did not install: %v", err)
		out.Warnings, out.Skipped = append(out.Warnings, w), "it did not install"
		opts.Prompt.Say("  %s", w)
	case res.Code != 0:
		detail := res.Err()
		if detail == "" {
			detail = res.Out()
		}
		w := fmt.Sprintf("OpenClaw did not install (exit %d): %s", res.Code, firstLine(detail))
		out.Warnings, out.Skipped = append(out.Warnings, w), "it did not install"
		opts.Prompt.Say("  %s", w)
	default:
		linkGlobals(opts)
		out.Present, out.Installed, out.Version = true, true, BusPin
		opts.Prompt.Say("  OpenClaw %s installed.", BusPin)
	}
	return nil
}

// busOnboard configures the Gateway from the answers Relay already has, and
// lets their onboarding register the service while it is in there.
//
// No credential crosses this boundary, which is stronger than the reference
// rule it replaces and was measured rather than reasoned. Relay's own key is
// the ORCHESTRATOR's key — an OpenRouter or Anthropic key for the model that
// routes utterances — and OpenClaw's only slot for a foreign key is
// `--auth-choice custom-api-key`, which the installed 2026.7.1-2 refuses
// without `--custom-base-url` and `--custom-model-id`: "Auth choice
// "custom-api-key" requires a base URL and model ID." It refuses before
// writing anything, so passing the reference did not configure a Gateway with
// a key it could not resolve — it configured no Gateway at all, on every
// machine where the model step produced a credential, which is every machine
// that got that far.
//
// What the Gateway actually needs is the machine's own agent logins, and
// `--auth-choice anthropic-cli` is the one that gives it them: it writes the
// agents.defaults.models[anthropic/*].agentRuntime = claude-cli bindings that
// make sessions.create resolve to a real Claude Code turn.
//
// That flag used to be passed whenever Claude Code was INSTALLED, which is a
// different fact from signed in, and a clean machine has the first without the
// second:
//
//	the Gateway was not configured: Auth choice "anthropic-cli" requires
//	Claude CLI auth on this host.
//
// It refuses before writing anything, so a machine that had just installed
// Claude Code got no Gateway at all. The precondition is checked in
// claudeCLIState now, in the one place OpenClaw's non-interactive path looks,
// and a machine that does not meet it is onboarded with `--auth-choice skip`,
// which configures a Gateway with no agent login rather than no Gateway.
// Where the Claude CLI keeps its login, and which half of it OpenClaw can read.
//
// Both places, measured on a real Mac on 2026-08-14:
//
//	Keychain "Claude Code-credentials"   the live login
//	~/.claude/.credentials.json          present, and expired in June
//
// Claude Code on macOS writes the Keychain. The file is what it used to write,
// and a machine that has only ever run the current version does not have one at
// all — which is what a clean Mac mini looked like after signing in
// successfully and being told it had not.
//
// OpenClaw's interactive path reads the Keychain first and falls back to the
// file. Its NON-INTERACTIVE path — the one Relay drives — passes
// allowKeychainPrompt: false and reads only the file, because a Keychain dialog
// in an unattended run is a question nobody is there to answer. So on macOS
// `--auth-choice anthropic-cli` is not available to Relay at all, and the
// earlier "verified" run of it here passed only because of that stale June file.
//
// Hence two states rather than one: signed in, and bindable from here.
const (
	claudeCLICredentials  = ".claude/.credentials.json"
	claudeKeychainService = "Claude Code-credentials"
)

type claudeLogin int

const (
	// claudeNoLogin: no login this machine can show us.
	claudeNoLogin claudeLogin = iota
	// claudeKeychainOnly: signed in, and invisible to a non-interactive onboard.
	claudeKeychainOnly
	// claudeBindable: signed in, in the file OpenClaw's non-interactive path reads.
	claudeBindable
)

// claudeCLIState reports what this machine can prove about a Claude Code login.
func claudeCLIState(ctx context.Context, opts Options) claudeLogin {
	home := opts.Env.Home
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return claudeNoLogin
		}
	}
	read := os.ReadFile
	if opts.Env.FS != nil {
		read = opts.Env.FS.ReadFile
	}
	if b, err := read(filepath.Join(home, claudeCLICredentials)); err == nil {
		// The file exists on a machine that has merely started Claude Code.
		// What OpenClaw requires from it is this object.
		var f struct {
			OAuth json.RawMessage `json:"claudeAiOauth"`
		}
		if json.Unmarshal(b, &f) == nil && len(f.OAuth) > 0 {
			return claudeBindable
		}
	}
	// Existence only. `security find-generic-password` without -w returns the
	// item's attributes and does not unlock the secret, so it asks the user for
	// nothing — which matters in the middle of an installer.
	if opts.Env.GOOS == "darwin" && opts.Env.Exec != nil {
		res, err := opts.Env.Exec.Run(ctx, detect.Cmd{
			Name: "security", Args: []string{"find-generic-password", "-s", claudeKeychainService},
			Timeout: 10 * time.Second,
		})
		if err == nil && res.Code == 0 {
			return claudeKeychainOnly
		}
	}
	return claudeNoLogin
}

func busOnboard(ctx context.Context, opts Options, auth busAuth, out *BusOutcome) {
	args := []string{
		"onboard", "--non-interactive", "--accept-risk",
		"--flow", "quickstart",
		// Relay owns these. A Relay user configuring Telegram channels or
		// OpenClaw skills is a user in the wrong product.
		"--skip-channels", "--skip-skills", "--skip-ui", "--skip-search",
		"--gateway-auth", "token", "--gateway-bind", "loopback",
	}
	// Configured, not started, and not registered at boot.
	//
	// Nothing in relayd dials this Gateway yet, and its default policy runs a
	// harness's tool calls without asking anyone. An always-on process with
	// that property, doing nothing for anybody, is the one state worth
	// refusing: the config is written so the day relayd drives it is a restart
	// and not a reinstall, and `openclaw gateway install` turns it on for
	// somebody who wants it now.
	args = append(args, "--skip-daemon", "--skip-health")
	// `claude-cli` is rejected as deprecated; the live name is `anthropic-cli`.
	// The key, where there is one, travels in the environment — argv is
	// world-readable on Linux and a credential has no business in `ps`.
	args = append(args, "--auth-choice", auth.Choice)

	cmd := busCmd(opts, args, 5*time.Minute)
	cmd.Env = append(cmd.Env, auth.Env...)
	res, err := opts.Env.Exec.Run(ctx, cmd)

	// The exit code is not the question here, and believing it is cost a whole
	// install: onboarding exits 1 from its own health phase having already
	// written the config, so a machine that was configured reported itself
	// unconfigured. Ask the config instead, and let step 6 judge liveness.
	var detail string
	switch {
	case err != nil:
		detail = err.Error()
	case res.Code != 0:
		d := res.Err()
		if d == "" {
			d = res.Out()
		}
		detail = firstLine(d)
	}
	if !busConfigured(ctx, opts) {
		w := "the Gateway was not configured"
		if detail != "" {
			w += ": " + detail
		}
		out.Warnings, out.Skipped = append(out.Warnings, w), "it is not configured yet"
		opts.Prompt.Say("  %s", wrapIndent(w, 2, 76))
		opts.Prompt.Say("  `openclaw onboard` sets it up directly, and `relay setup` will adopt it.")
		return
	}
	out.Configured = true
	out.Registered = false
	if auth.Choice != "skip" {
		out.AgentAuth = auth.Choice
	}
	if out.AgentAuth == "" {
		// Said, and not warned about.
		//
		// It was a warning until a finished install ended with one thing
		// needing attention: "the Gateway has no agent login yet, so it cannot
		// run a session." Nothing in Relay asks it to run a session, and this
		// step does not start it — so that is a description of the design,
		// printed under a heading that means something is wrong, on an install
		// where nothing was.
		opts.Prompt.Say("  %s", wrapIndent("It has no agent login, which costs nothing while "+
			"nothing dials it. `openclaw onboard` gives it one, interactively, for the day "+
			"something does.", 2, 76))
	}
	if auth.Label != "" {
		opts.Prompt.Say("  Gateway configured, on loopback, with %s.", auth.Label)
	} else {
		opts.Prompt.Say("  Gateway configured, on loopback.")
	}
}

// busConfigured reports whether an OpenClaw here is already set up.
//
// It asks for `gateway.port`, and the key is the whole point: there is no
// top-level `models` in an onboarded config — the written keys are agents,
// gateway, session, tools, hooks, wizard, meta — so asking for one exits 1
// with "Config path not found: models" on a Gateway that is perfectly well
// configured. This step is the sole judge of whether onboarding worked, so
// that miss meant a successful onboard was reported as "it is not configured
// yet" and the whole bus step gave up on a machine it had just set up.
//
// `gateway.port` discriminates cleanly, and it is what busPort already reads:
// exit 1 and "Config path not found" before onboarding, exit 0 and the port
// after. It is also the right question — a Gateway with no port is one Relay
// cannot dial, whatever else it has.
func busConfigured(ctx context.Context, opts Options) bool {
	res, err := opts.Env.Exec.Run(ctx,
		busCmd(opts, []string{"config", "get", "gateway.port"}, 30*time.Second))
	if err != nil || res.Code != 0 {
		return false
	}
	n, convErr := strconv.Atoi(res.Out())
	return convErr == nil && n > 0
}

// busCmd is an openclaw invocation under the profile the user actually runs.
//
// --profile moves the config, the state directory and the daemon's launchd
// label together, so a command without it reads and registers a different
// machine's Gateway than the one relayd will talk to. detect already asks the
// same question of the same flags; this is the write half of it.
func busCmd(opts Options, args []string, timeout time.Duration) detect.Cmd {
	var pre []string
	switch {
	case opts.Detect.OpenClawProfile != "":
		pre = []string{"--profile", opts.Detect.OpenClawProfile}
	case opts.Detect.OpenClawDev:
		pre = []string{"--dev"}
	}
	return detect.Cmd{Name: "openclaw", Args: append(pre, args...), Timeout: timeout}
}

// busDefaultPort is OpenClaw's own default. Their config is asked first; this
// is only what to try when it cannot answer.
const busDefaultPort = 18789

// busSettle is how long a service manager gets to bring the process up before
// the port is called dead, and busPoll is how often it is asked meanwhile.
// Both are variables so a test can run the same code in no time at all.
var (
	busSettle = 15 * time.Second
	busPoll   = 500 * time.Millisecond
)

// busServe makes the Gateway answer, and reports it as working only when it did.
//
// Already answering is the case to get right, and it is the common one: an
// adopted Gateway is usually already up, and their onboarding starts the one it
// just installed. Neither wants a second supervisor pointed at the same port,
// so nothing is registered or started until the port has been asked.
func busServe(ctx context.Context, opts Options, out BusOutcome) (BusOutcome, error) {
	out.Port = busPort(ctx, opts)
	if busUp(ctx, opts, out.Port) {
		out.Live = true
		opts.Prompt.Say("  The Gateway answers on port %d.", out.Port)
		return out, nil
	}
	if opts.SkipService {
		w := fmt.Sprintf("the Gateway is configured but nothing is running it — "+
			"`openclaw gateway install` registers it and starts it on port %d", out.Port)
		out.Warnings = append(out.Warnings, w)
		opts.Prompt.Say("  %s", wrapIndent(w, 2, 76))
		return out, nil
	}

	// Everything else in this installer that can fail gets a verify/repair
	// loop, and a bus that is not up is the same class of problem as a key that
	// does not work: worth one more go, never worth trapping somebody in.
	return verify(ctx, opts, repair[BusOutcome]{
		ID:     "bus.repair",
		Title:  "The Gateway is not answering",
		Choose: func() (BusOutcome, error) { return busStart(ctx, opts, out) },
		OK:     func(b BusOutcome) bool { return b.Live },
		Trouble: func(b BusOutcome) string {
			s := fmt.Sprintf("Nothing is listening on port %d.", b.Port)
			if b.Detail != "" {
				s += " " + b.Detail
			}
			return s + "\n\nSomething else holding that port is the usual cause, and " +
				"`openclaw gateway status` names what."
		},
		FixLabel:      "Try starting it again",
		ContinueLabel: "Leave it stopped for now",
		GiveUp: "Leaving the Gateway stopped. Relay drives Claude Code and Codex directly " +
			"until it is up, and `openclaw gateway status` says what stopped it.",
	})
}

// busStart hands the Gateway to OpenClaw's own service manager, then waits for
// the port rather than for the exit code.
func busStart(ctx context.Context, opts Options, base BusOutcome) (BusOutcome, error) {
	out := base
	out.Warnings = append([]string(nil), base.Warnings...)
	out.Detail, out.Live = "", false
	p := opts.Prompt

	// An already-registered service is started, never reinstalled: that unit is
	// theirs, and it may carry a port, a token or a wrapper this installer
	// knows nothing about.
	verb := "install"
	if busRegistered(ctx, opts) {
		verb = "start"
	}
	cmd := busCmd(opts, []string{"gateway", verb}, 2*time.Minute)
	// Shown rather than asked about: this is the same yes `relay setup` already
	// takes for relayd's own boot registration, and the command is the honest
	// way to say what that means.
	p.Say("  Runs: %s %s", cmd.Name, strings.Join(cmd.Args, " "))

	res, err := opts.Env.Exec.Run(ctx, cmd)
	switch {
	case err != nil:
		out.Detail = err.Error()
	case res.Code != 0:
		d := res.Err()
		if d == "" {
			d = res.Out()
		}
		out.Detail = firstLine(d)
	default:
		out.Registered = out.Registered || verb == "install"
	}

	out.Live = busAwait(ctx, opts, out.Port)
	busHandoff(ctx, opts, &out)
	switch {
	case out.Live && out.Registered:
		p.Say("  The Gateway is up on port %d, and starts with this machine.", out.Port)
	case out.Live:
		p.Say("  The Gateway is up on port %d.", out.Port)
	default:
		w := fmt.Sprintf("the Gateway did not come up on port %d", out.Port)
		if out.Detail != "" {
			w += ": " + out.Detail
		}
		out.Warnings = append(out.Warnings, w)
		p.Say("  %s", wrapIndent(w, 2, 76))
	}
	return out, nil
}

// busHandoff turns a working Gateway into the two lines relayd needs.
//
// The token goes into Relay's vault and the config file gets `vault:<id>`.
// OpenClaw keeps its own plaintext copy in openclaw.json — that is theirs, and
// no reason for Relay to keep a second one in the file people cat into support
// tickets. With no vault the reference is refused rather than inlined, which is
// the rule every other credential in this installer follows.
func busHandoff(ctx context.Context, opts Options, out *BusOutcome) {
	if out.Port <= 0 {
		return
	}
	out.Config.URL = fmt.Sprintf("ws://127.0.0.1:%d", out.Port)

	res, err := opts.Env.Exec.Run(ctx,
		busCmd(opts, []string{"config", "get", "gateway.auth.token"}, 30*time.Second))
	token := ""
	if err == nil && res.Code == 0 {
		token = strings.TrimSpace(res.Out())
	}
	if token == "" || token == "undefined" {
		// A Gateway with auth off is a real configuration, and dialling it
		// needs no token at all.
		return
	}
	if opts.Vault == nil {
		out.Warnings = append(out.Warnings,
			"the Gateway needs a token and this run has no vault to keep it in, so relayd "+
				"cannot dial it yet — re-run `relay setup` with the vault open")
		return
	}
	entry, verr := opts.Vault.Put(ctx, vault.Input{
		Service: "bus", Label: "OpenClaw Gateway", Secret: token,
		Source: vault.Provenance{Kind: vault.SourceConfig, Runtime: "openclaw", At: opts.Now()},
	})
	if verr != nil {
		out.Warnings = append(out.Warnings, "could not store the Gateway token: "+verr.Error())
		return
	}
	out.Config.Token = "vault:" + entry.ID
}

// busPort is where the Gateway listens, asked of the config rather than
// assumed — the installer never chose it, and --profile moves it.
func busPort(ctx context.Context, opts Options) int {
	res, err := opts.Env.Exec.Run(ctx,
		busCmd(opts, []string{"config", "get", "gateway.port"}, 30*time.Second))
	if err == nil && res.Code == 0 {
		if n, convErr := strconv.Atoi(strings.TrimSpace(res.Out())); convErr == nil && n > 0 {
			return n
		}
	}
	return busDefaultPort
}

// busRegistered reports whether a Gateway service is already loaded in this
// machine's service manager.
func busRegistered(ctx context.Context, opts Options) bool {
	res, err := opts.Env.Exec.Run(ctx,
		busCmd(opts, []string{"gateway", "status", "--json"}, time.Minute))
	if err != nil || res.Code != 0 {
		return false
	}
	var st struct {
		Service struct {
			Loaded bool `json:"loaded"`
		} `json:"service"`
	}
	if json.Unmarshal([]byte(res.Out()), &st) != nil {
		return false
	}
	return st.Service.Loaded
}

// busUp asks the Gateway itself, over the port relayd will use.
//
// One real call, like every credential in this installer, and deliberately the
// cheapest true one: /health is unauthenticated on loopback and answers
// instantly, so it can be polled. It is a narrower claim than "it can serve" —
// /readyz answers that, and names the failing subsystems — but a Gateway that
// binds its port is a Gateway relayd can go on to interrogate, and one that
// does not is the failure this step exists to catch.
func busUp(ctx context.Context, opts Options, port int) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/health", port), nil)
	if err != nil {
		return false
	}
	res, err := busClient(opts).Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return false
	}
	var body struct {
		OK bool `json:"ok"`
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 4<<10))
	return err == nil && json.Unmarshal(b, &body) == nil && body.OK
}

// busAwait gives the service manager time to bind the port before judging it.
// A daemon that was told to start and has not finished booting Node yet is not
// a failure, it is a boot.
func busAwait(ctx context.Context, opts Options, port int) bool {
	deadline := time.Now().Add(busSettle)
	for {
		if busUp(ctx, opts, port) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(busPoll):
		}
	}
}

func busClient(opts Options) *http.Client {
	if opts.HTTPClient != nil {
		return opts.HTTPClient
	}
	return &http.Client{Timeout: 5 * time.Second}
}

func nodeVersion(ctx context.Context, opts Options) (string, bool) {
	res, err := opts.Env.Exec.Run(ctx, detect.Cmd{
		Name: "node", Args: []string{"--version"}, Timeout: 30 * time.Second,
	})
	if err != nil || res.Code != 0 {
		return "", false
	}
	return strings.TrimSpace(res.Out()), true
}
