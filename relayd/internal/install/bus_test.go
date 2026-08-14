package install

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/detect"
)

// The bus step. OpenClaw's Gateway is the one thing Relay depends on that it
// does not ship, so what these pin is mostly restraint: what it refuses to do
// when the machine is not ready, what it refuses to put on a command line, and
// what it refuses to claim about a Gateway it has not heard from.

// busExec answers openclaw by subcommand rather than by the whole command line.
// The onboard flag list is fifteen words long, and keying a fixture on it would
// make every test in here a test of the flags.
type busExec struct {
	*detect.FakeExec
	// Verbs maps "onboard", "gateway install", "gateway status" onto answers.
	Verbs map[string]detect.Result
	// Hook runs before the answer, so a fixture can do what starting a service
	// does: make the port start answering.
	Hook func(detect.Cmd)
	// configured is whether an openclaw.json exists here yet. It is state, not
	// a canned answer, because the two calls that read it have to disagree
	// before and after onboarding — a fixture that answers "configured" to a
	// box nothing has configured cannot fail the way a real one does, and did
	// not: `config get models` was asked of every onboarded Gateway on the
	// planet and none of them have that key.
	configured bool
}

func (b *busExec) Run(ctx context.Context, c detect.Cmd) (detect.Result, error) {
	if b.Hook != nil {
		b.Hook(c)
	}
	switch verb := busVerb(c); verb {
	case "onboard":
		b.Calls = append(b.Calls, c)
		if res, ok := busOnboardRefusal(c); ok {
			return res, nil
		}
		b.configured = true
		return b.Verbs[verb], nil

	case "config get gateway.port":
		b.Calls = append(b.Calls, c)
		if !b.configured {
			// Word for word what 2026.7.1-2 prints, exit 1 and all.
			return detect.Result{Code: 1, Stderr: []byte(
				"Config path not found: gateway.port. Run openclaw config validate " +
					"to inspect config shape.\n")}, nil
		}
		return detect.Result{Stdout: []byte("19311\n")}, nil
	}
	if r, ok := b.Verbs[busVerb(c)]; ok {
		b.Calls = append(b.Calls, c)
		return r, nil
	}
	return b.FakeExec.Run(ctx, c)
}

// busOnboardRefusal is the flag validation the real onboard does before it
// writes anything, and the reason this fixture has any opinion about flags at
// all. Keying only on the subcommand made every onboard succeed, so the
// installer shipped a flag combination that the installed binary rejects
// outright on the one path that matters — a machine with a model credential,
// which is every machine that reaches this step.
func busOnboardRefusal(c detect.Cmd) (detect.Result, bool) {
	var auth string
	var haveURL, haveModel bool
	for i, a := range c.Args {
		switch a {
		case "--auth-choice":
			if i+1 < len(c.Args) {
				auth = c.Args[i+1]
			}
		case "--custom-base-url":
			haveURL = true
		case "--custom-model-id":
			haveModel = true
		}
	}
	switch {
	case auth == "custom-api-key" && (!haveURL || !haveModel):
		return detect.Result{Code: 1, Stderr: []byte(
			"Auth choice \"custom-api-key\" requires a base URL and model ID.\n" +
				"Use --custom-base-url and --custom-model-id.\n")}, true
	case auth == "claude-cli":
		return detect.Result{Code: 1, Stderr: []byte(
			"Auth choice \"claude-cli\" is deprecated. Use \"--auth-choice anthropic-cli\".\n")}, true
	}
	return detect.Result{}, false
}

// busVerb names an openclaw call by its subcommand, ignoring flags and the
// global --profile that may sit in front of it.
func busVerb(c detect.Cmd) string {
	if c.Name != "openclaw" {
		return ""
	}
	var parts []string
	skip := false
	for _, a := range c.Args {
		switch {
		case skip:
			skip = false
		case a == "--profile":
			skip = true
		case strings.HasPrefix(a, "--"):
		default:
			parts = append(parts, a)
		}
	}
	if len(parts) > 0 && parts[0] == "onboard" {
		return "onboard"
	}
	return strings.Join(parts, " ")
}

// busGateway is a Gateway on the loopback port: /health answers when it is up,
// and refuses the connection when it is not, which is what a stopped daemon
// does. Everything else goes to the provider fixture, because the rest of the
// installer still has to run.
type busGateway struct {
	up   bool
	next http.RoundTripper
	// URLs is every health check made, so a test can prove which port was asked.
	URLs []string
}

func (g *busGateway) attach(o *Options) {
	g.next = o.HTTPClient.Transport
	o.HTTPClient = &http.Client{Transport: g}
}

func (g *busGateway) RoundTrip(r *http.Request) (*http.Response, error) {
	if !strings.HasSuffix(r.URL.Path, "/health") {
		return g.next.RoundTrip(r)
	}
	g.URLs = append(g.URLs, r.URL.String())
	if !g.up {
		return nil, fmt.Errorf("dial tcp %s: connect: connection refused", r.URL.Host)
	}
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"ok":true,"status":"live"}`)),
		Request:    r,
	}, nil
}

// busEnv is a machine with a usable Node and, optionally, OpenClaw. The port in
// the fixture is deliberately not OpenClaw's default: a health check that only
// works on 18789 is a health check that assumed instead of asking.
// withClaudeCLILogin writes the file OpenClaw's non-interactive onboarding
// reads. Installed and signed in are different facts about a machine, and this
// is the second one.
func withClaudeCLILogin(t *testing.T) func(*Options) {
	t.Helper()
	return func(o *Options) {
		fs, ok := o.Env.FS.(*detect.MemFS)
		if !ok {
			t.Fatalf("fixture filesystem is %T, not a MemFS", o.Env.FS)
		}
		fs.Files[o.Env.Home+"/"+claudeCLICredentials] =
			`{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":9999999999999}}`
	}
}

func busEnv(t *testing.T, nodeVer string, openclaw bool) func(*Options) {
	t.Helper()
	busInstant(t)
	return func(o *Options) {
		ex, ok := o.Env.Exec.(*detect.FakeExec)
		if !ok {
			t.Fatalf("fixture exec is %T", o.Env.Exec)
		}
		if nodeVer != "" {
			ex.Paths["node"] = "/usr/bin/node"
			ex.Responses[detect.Key("node", "--version")] = detect.Result{Stdout: []byte(nodeVer + "\n")}
		}
		if openclaw {
			ex.Paths["openclaw"] = "/usr/local/bin/openclaw"
			// What it really answers, commit and all — detection keeps the whole
			// line, so anything that prints a version has to cut it down itself.
			ex.Responses[detect.Key("openclaw", "--version")] = detect.Result{
				Stdout: []byte("OpenClaw " + BusPin + " (0790d9f)\n")}
		}
		ex.Responses[detect.Key("npm", "install", "-g", "openclaw@"+BusPin)] = detect.Result{
			Stdout: []byte("added 565 packages\n")}
		o.Env.Exec = &busExec{FakeExec: ex, Verbs: map[string]detect.Result{
			"onboard":         {},
			"gateway status":  {Stdout: []byte(`{"service":{"loaded":false}}`)},
			"gateway install": {},
			"gateway start":   {},
			// An OpenClaw already on the box is one somebody already set up, so
			// its config answers before this installer runs anything.
		}, configured: openclaw}
	}
}

// busInstant removes the wait for a service manager that, in a test, was never
// going to start anything.
func busInstant(t *testing.T) {
	t.Helper()
	settle, poll := busSettle, busPoll
	busSettle, busPoll = 0, 0
	t.Cleanup(func() { busSettle, busPoll = settle, poll })
}

func busExecOf(t *testing.T, o Options) *busExec {
	t.Helper()
	ex, ok := o.Env.Exec.(*busExec)
	if !ok {
		t.Fatalf("fixture exec is %T", o.Env.Exec)
	}
	return ex
}

// busRan reports whether an openclaw subcommand ran.
func busRan(ex *busExec, verb string) (detect.Cmd, bool) {
	for _, c := range ex.Calls {
		if busVerb(c) == verb {
			return c, true
		}
	}
	return detect.Cmd{}, false
}

func busCall(ex *busExec, name, first string) (detect.Cmd, bool) {
	for _, c := range ex.Calls {
		if c.Name == name && len(c.Args) > 0 && c.Args[0] == first {
			return c, true
		}
	}
	return detect.Cmd{}, false
}

func argvOf(c detect.Cmd) string { return strings.Join(append([]string{c.Name}, c.Args...), " ") }

// The Node ranges are three disjoint windows, so "newer is fine" is wrong in a
// way that costs a failed install. 25.8.0 is the version this was written on
// and it satisfies none of them.
func TestNodeRangesAreCheckedNotAssumed(t *testing.T) {
	for v, want := range map[string]bool{
		"v22.22.3": true, "v22.22.4": true, "v24.19.0": true, "v25.9.0": true, "v26.0.0": true,
		"v22.14.0": false, "v23.0.0": false, "v24.14.9": false, "v25.8.0": false, "": false,
	} {
		if got := nodeOK(v); got != want {
			t.Errorf("nodeOK(%q) = %v, want %v", v, got, want)
		}
	}
}

// A Node that cannot run it stops the step before npm gets a chance to fail
// with a message about engines that reads like a bug in Relay.
func TestBusStopsOnAnUnusableNodeAndSaysWhy(t *testing.T) {
	answers := baseAnswers()
	opts, script, _ := newOpts(t, answers, busEnv(t, "v25.8.0", false))
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if res.Bus.Skipped == "" {
		t.Error("an unusable Node must skip the bus, not attempt it")
	}
	// The offer is made before the skip: a machine with no usable Node is now
	// asked whether Relay should fetch one, and only declining ends the step.
	// The fixture answers no, which is what "bus.node" being absent from the
	// answers means to Script.
	out := script.Output()
	if !strings.Contains(out, "Install Node?") {
		t.Errorf("an unusable Node must be offered a fix, not just a lecture:\n%s", out)
	}
	// And the install still finishes: the bus is not required for a box that
	// drives Claude Code and Codex directly.
	if !res.Bus.OK() {
		t.Error("a skipped bus is a supported state, not a failed install")
	}
	ex := busExecOf(t, opts)
	if _, called := busCall(ex, "npm", "install"); called {
		t.Error("npm ran despite the Node gate")
	}
	// Nothing is registered on a machine that has no bus to register.
	if _, called := busRan(ex, "gateway install"); called {
		t.Error("a skipped bus still installed a service")
	}
}

// Somebody who already runs OpenClaw is not walked through setting it up again —
// and a Gateway that is already answering is left exactly as it is. Registering
// a second supervisor on a port somebody else's process holds is a crash loop.
func TestAnAnsweringGatewayIsAdoptedAndLeftAlone(t *testing.T) {
	gw := &busGateway{up: true}
	answers := baseAnswers()
	opts, script, _ := newOpts(t, answers, func(o *Options) {
		busEnv(t, "v24.19.0", true)(o)
		gw.attach(o)
	})
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if !res.Bus.Adopted || !res.Bus.Configured || !res.Bus.Live {
		t.Errorf("bus = %+v, want adopted and live", res.Bus)
	}
	if res.Bus.Port != 19311 {
		t.Errorf("port = %d, want the one in their config (19311), not an assumed default", res.Bus.Port)
	}
	// Every row that prints this has already said the name.
	if res.Bus.Version != BusPin {
		t.Errorf("version = %q, want %q", res.Bus.Version, BusPin)
	}
	if strings.Contains(res.Bus.Line(), "OpenClaw OpenClaw") {
		t.Errorf("summary = %q", res.Bus.Line())
	}
	ex := busExecOf(t, opts)
	for _, verb := range []string{"onboard", "gateway install", "gateway start"} {
		if _, called := busRan(ex, verb); called {
			t.Errorf("%q ran against a Gateway that was already answering", verb)
		}
	}
	for _, id := range script.Asked {
		if strings.HasPrefix(id, "bus.") {
			t.Errorf("asked %q about a Gateway that is already set up", id)
		}
	}
}

// Relay's key never reaches OpenClaw, and the onboard argv is one the installed
// binary accepts.
//
// This test used to assert the opposite — that the credential reference was
// forwarded — and it passed against a fixture that ignored flags. The real
// 2026.7.1-2 refuses `--auth-choice custom-api-key` without a base URL and a
// model ID, before writing anything, so the forwarding it was protecting
// configured no Gateway at all on every machine that had a model credential.
// What the Gateway needs is the machine's own Claude Code login, which costs
// nothing and is the only binding sessions.create can resolve.
func TestRelaysKeyStaysRelaysAndTheOnboardIsOneOpenClawAccepts(t *testing.T) {
	// Split, for the reason diagnose_test.go gives.
	secret := "sk-or-" + "v1-" + strings.Repeat("0", 48)
	t.Setenv("OPENROUTER_API_KEY", secret)

	gw := &busGateway{}
	answers := baseAnswers()
	answers["bus.ack"] = "yes"
	answers["bus.install"] = "yes"
	opts, script, _ := newOpts(t, answers, func(o *Options) {
		busEnv(t, "v24.19.0", false)(o)
		gw.attach(o)
		// Signed in, which is what anthropic-cli requires and what installed
		// alone does not prove.
		withClaudeCLILogin(t)(o)
		// Onboarding starts the daemon it just installed, so the port answers
		// by the time the health check runs.
		ex := o.Env.Exec.(*busExec)
		ex.Hook = func(c detect.Cmd) {
			if busVerb(c) == "onboard" {
				gw.up = true
			}
		}
	})
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if !res.Bus.Installed {
		t.Fatalf("bus = %+v, want installed", res.Bus)
	}

	ex := busExecOf(t, opts)
	cmd, ok := busRan(ex, "onboard")
	if !ok {
		t.Fatal("the Gateway was never onboarded")
	}
	argv := argvOf(cmd)
	// argv is world-readable on Linux, so neither the secret nor the reference
	// that names it belongs on this command line.
	if strings.Contains(argv, secret) {
		t.Errorf("the resolved key was put on a command line:\n%s", argv)
	}
	if strings.Contains(argv, "OPENROUTER_API_KEY") || strings.Contains(argv, "--custom-api-key") {
		t.Errorf("Relay's orchestrator key was offered to the Gateway:\n%s", argv)
	}
	// The auth choice that actually binds the claude-cli runtime. `claude-cli`
	// is the deprecated spelling and is rejected, and anthropic-cli is only
	// accepted on a machine where the Claude CLI is signed in.
	if !strings.Contains(argv, "--auth-choice anthropic-cli") {
		t.Errorf("the Gateway got no runtime binding, so sessions.create has "+
			"no model that resolves:\n%s", argv)
	}
	// Their wizard must never appear, and the parts Relay owns are skipped.
	if !strings.Contains(argv, "--non-interactive") {
		t.Error("onboard was run interactively; the user would meet a second wizard")
	}
	for _, want := range []string{"--skip-channels", "--skip-skills", "--gateway-bind loopback"} {
		if !strings.Contains(argv, want) {
			t.Errorf("missing %q in:\n%s", want, argv)
		}
	}
	// Their onboarding registers and starts the service, so Relay does not have
	// to do it a second time.
	if !strings.Contains(argv, "--install-daemon") {
		t.Errorf("the Gateway was configured but never registered to run:\n%s", argv)
	}
	if !res.Bus.Live || !res.Bus.Registered {
		t.Errorf("bus = %+v, want live and registered", res.Bus)
	}
	if _, called := busRan(ex, "gateway install"); called {
		t.Error("the service was installed twice: once by onboard, once by Relay")
	}
}

// No Claude Code login on the box means no binding to make, so onboarding is
// told that in the one word it accepts — `skip` — rather than being sent a
// choice it will refuse. The Gateway is still configured: it drives seventeen
// harnesses and Claude Code is only the one Relay can prove.
func TestWithoutClaudeCodeTheGatewayIsStillConfiguredWithoutTheBinding(t *testing.T) {
	gw := &busGateway{}
	answers := baseAnswers()
	answers["bus.ack"] = "yes"
	answers["bus.install"] = "yes"
	opts, script, _ := newOpts(t, answers, func(o *Options) {
		busEnv(t, "v24.19.0", false)(o)
		gw.attach(o)
		ex := o.Env.Exec.(*busExec)
		delete(ex.Paths, "claude")
		ex.Hook = func(c detect.Cmd) {
			if busVerb(c) == "onboard" {
				gw.up = true
			}
		}
	})
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if !res.Bus.Configured {
		t.Errorf("bus = %+v, want configured", res.Bus)
	}
	cmd, ok := busRan(busExecOf(t, opts), "onboard")
	if !ok {
		t.Fatal("the Gateway was never onboarded")
	}
	argv := argvOf(cmd)
	if strings.Contains(argv, "anthropic-cli") {
		t.Errorf("an auth choice was sent for a login this machine does not have:\n%s", argv)
	}
	// Omitting the flag entirely is not the same thing: their non-interactive
	// path wants to be told, and `skip` is what it is told.
	if !strings.Contains(argv, "--auth-choice skip") {
		t.Errorf("onboarding was left to guess at the auth choice:\n%s", argv)
	}
	// And the summary does not claim a Gateway that can run a session.
	if res.Bus.AgentAuth != "" {
		t.Errorf("AgentAuth = %q, want empty on a machine with no agent login", res.Bus.AgentAuth)
	}
}

// The failure a clean machine actually hit: Claude Code installed twenty
// minutes earlier, never signed in, and onboarding refusing the whole config
// over it —
//
//	the Gateway was not configured: Auth choice "anthropic-cli" requires
//	Claude CLI auth on this host.
//
// Installed is not signed in, and the difference is one file.
func TestClaudeCodeInstalledButNotSignedInStillGetsAGateway(t *testing.T) {
	gw := &busGateway{}
	answers := baseAnswers()
	answers["bus.ack"] = "yes"
	answers["bus.install"] = "yes"
	opts, script, _ := newOpts(t, answers, func(o *Options) {
		busEnv(t, "v24.19.0", false)(o)
		gw.attach(o)
		// Installed — the fixture's exec has `claude` on PATH — and no
		// credentials file anywhere.
		o.Env.Exec.(*busExec).Hook = func(c detect.Cmd) {
			if busVerb(c) == "onboard" {
				gw.up = true
			}
		}
	})
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if !res.Bus.Configured {
		t.Fatalf("bus = %+v, want a configured Gateway rather than none", res.Bus)
	}
	cmd, _ := busRan(busExecOf(t, opts), "onboard")
	if argv := argvOf(cmd); !strings.Contains(argv, "--auth-choice skip") {
		t.Errorf("sent a choice that onboarding refuses:\n%s", argv)
	}
	// And it says what to do about it, naming the command that fixes it.
	out := script.Output()
	if !strings.Contains(out, "not signed in") || !strings.Contains(out, "`claude`") {
		t.Errorf("the user is not told how to give the Gateway a login:\n%s", out)
	}
}

// busConfigured is the sole judge of whether onboarding worked, so the key it
// asks for is pinned. `models` was the previous answer and no onboarded config
// has ever had it.
func TestTheConfiguredProbeAsksForAKeyOpenClawWrites(t *testing.T) {
	opts, _, _ := newOpts(t, baseAnswers(), busEnv(t, "v24.19.0", true))
	if !busConfigured(context.Background(), opts) {
		t.Fatal("a configured Gateway was read as unconfigured")
	}
	ex := busExecOf(t, opts)
	if _, asked := busRan(ex, "config get gateway.port"); !asked {
		t.Error("busConfigured asked something other than gateway.port")
	}
	if _, asked := busRan(ex, "config get models"); asked {
		t.Error("busConfigured is back to asking for a key OpenClaw does not write")
	}
}

// The acknowledgement is OpenClaw's requirement, and Relay may not answer it on
// the user's behalf. Declining it is a supported outcome.
func TestDecliningTheAcknowledgementSkipsTheBus(t *testing.T) {
	answers := baseAnswers()
	answers["bus.ack"] = "no"
	opts, script, _ := newOpts(t, answers, busEnv(t, "v24.19.0", false))
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if res.Bus.Skipped == "" || res.Bus.Configured {
		t.Errorf("bus = %+v, want skipped", res.Bus)
	}
	ex := busExecOf(t, opts)
	if _, called := busCall(ex, "npm", "install"); called {
		t.Error("declining the risk note still installed software")
	}
	if !strings.Contains(script.Output(), "shell, your files, your repos") {
		t.Error("the note has to say what the Gateway can reach")
	}
}

// Onboarding exits 1 from its own health phase on a machine where nothing is
// listening yet — having already written the config. Believing that exit code
// cost the step its entire happy path: every fresh machine reported "it is not
// configured yet" about a Gateway that was configured.
func TestAWrittenConfigOutranksTheOnboardExitCode(t *testing.T) {
	gw := &busGateway{}
	answers := baseAnswers()
	answers["bus.ack"] = "yes"
	answers["bus.install"] = "yes"
	answers["bus.repair"] = "continue"
	opts, script, _ := newOpts(t, answers, func(o *Options) {
		busEnv(t, "v24.19.0", false)(o)
		gw.attach(o)
		ex := o.Env.Exec.(*busExec)
		ex.Verbs["onboard"] = detect.Result{
			Code: 1, Stderr: []byte(`{"ok":false,"phase":"gateway-health","classification":"not-listening"}`)}
	})
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if !res.Bus.Configured || res.Bus.Skipped != "" {
		t.Errorf("bus = %+v, want configured despite the exit code", res.Bus)
	}
	// But the claim stops there: nothing answered, so nothing says it did.
	if res.Bus.Live {
		t.Errorf("bus = %+v, want configured and not yet running", res.Bus)
	}
	if !strings.Contains(res.Bus.Line(), "not answering") {
		t.Errorf("summary = %q, which reads as working", res.Bus.Line())
	}
}

// A config file is a plan. The port is the fact, and it is asked for rather
// than assumed — on the port their own config names.
func TestTheGatewayIsHealthCheckedNotAssumed(t *testing.T) {
	gw := &busGateway{}
	answers := baseAnswers()
	answers["bus.ack"] = "yes"
	answers["bus.install"] = "yes"
	answers["bus.repair"] = "continue"
	opts, script, _ := newOpts(t, answers, func(o *Options) {
		busEnv(t, "v24.19.0", false)(o)
		gw.attach(o)
	})
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if len(gw.URLs) == 0 {
		t.Fatal("the installer never called the Gateway; it only read its config")
	}
	if !strings.Contains(gw.URLs[0], "127.0.0.1:19311/health") {
		t.Errorf("health check went to %s, not the port in their config", gw.URLs[0])
	}
	if res.Bus.Live {
		t.Error("a Gateway that never answered was reported as running")
	}
	// It is a warning, not a failure: the box still drives Claude Code directly.
	var named bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "19311") {
			named = true
		}
	}
	if !named {
		t.Errorf("nothing in the summary says the bus is down:\n%v", res.Warnings)
	}
	if !res.Bus.OK() {
		t.Error("a bus that is configured but down must not fail the install")
	}
}

// The repair loop every credential gets, for the one dependency that can be
// down rather than wrong: try again, and stop offering when it works.
func TestAStartThatDidNotTakeIsOfferedAgain(t *testing.T) {
	gw := &busGateway{}
	answers := baseAnswers()
	answers["bus.ack"] = "yes"
	answers["bus.install"] = "yes"
	answers["bus.repair"] = "fix"
	opts, script, _ := newOpts(t, answers, func(o *Options) {
		busEnv(t, "v24.19.0", false)(o)
		gw.attach(o)
		// launchd bootstrapped it and the process died on the first go, which
		// is what a service that "installed fine" and never bound the port
		// looks like from here.
		ex := o.Env.Exec.(*busExec)
		var installs int
		ex.Hook = func(c detect.Cmd) {
			if busVerb(c) == "gateway install" {
				installs++
				gw.up = installs > 1
			}
		}
	})
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if !res.Bus.Live || !res.Bus.Registered {
		t.Errorf("bus = %+v, want live after the retry", res.Bus)
	}
	var asked int
	for _, id := range script.Asked {
		if id == "bus.repair" {
			asked++
		}
	}
	if asked != 1 {
		t.Errorf("the repair was offered %d times, want exactly once", asked)
	}
	// The whole point of asking again is that the second answer is believed
	// only because the port answered, not because a command exited 0.
	if len(gw.URLs) < 2 {
		t.Errorf("the retry did not re-check the port: %v", gw.URLs)
	}
}

// The unit belongs to OpenClaw, and it may carry a port, a token or a wrapper
// this installer knows nothing about. One that is already loaded is started,
// never overwritten.
func TestAnAlreadyRegisteredServiceIsStartedNotReinstalled(t *testing.T) {
	gw := &busGateway{}
	answers := baseAnswers()
	opts, script, _ := newOpts(t, answers, func(o *Options) {
		busEnv(t, "v24.19.0", true)(o)
		gw.attach(o)
		ex := o.Env.Exec.(*busExec)
		ex.Verbs["gateway status"] = detect.Result{
			Stdout: []byte(`{"service":{"loaded":true,"runtime":{"status":"stopped"}}}`)}
		ex.Hook = func(c detect.Cmd) {
			if busVerb(c) == "gateway start" {
				gw.up = true
			}
		}
	})
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if !res.Bus.Live {
		t.Errorf("bus = %+v, want live", res.Bus)
	}
	ex := busExecOf(t, opts)
	if _, called := busRan(ex, "gateway install"); called {
		t.Error("a service that was already registered was reinstalled over")
	}
	if _, called := busRan(ex, "gateway start"); !called {
		t.Error("a registered but stopped service was never started")
	}
}

// `relay setup --no-service` is a decision about what this machine starts at
// boot, and the Gateway is something this machine would start at boot. It is
// not a licence to register one quietly.
func TestNoServiceRegistersNothingIncludingTheirs(t *testing.T) {
	gw := &busGateway{}
	answers := baseAnswers()
	answers["bus.ack"] = "yes"
	answers["bus.install"] = "yes"
	opts, script, _ := newOpts(t, answers, func(o *Options) {
		busEnv(t, "v24.19.0", false)(o)
		gw.attach(o)
		o.SkipService = true
	})
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	ex := busExecOf(t, opts)
	cmd, ok := busRan(ex, "onboard")
	if !ok {
		t.Fatal("the Gateway was never onboarded")
	}
	argv := argvOf(cmd)
	if strings.Contains(argv, "--install-daemon") {
		t.Errorf("--no-service still registered their daemon:\n%s", argv)
	}
	if !strings.Contains(argv, "--skip-daemon") {
		t.Errorf("onboarding was left to decide for itself:\n%s", argv)
	}
	for _, verb := range []string{"gateway install", "gateway start"} {
		if _, called := busRan(ex, verb); called {
			t.Errorf("%q ran under --no-service", verb)
		}
	}
	// And it is said out loud, because a configured Gateway nobody starts is
	// the state that looks like success and is not.
	if !strings.Contains(script.Output(), "gateway install") {
		t.Errorf("the way to start it later is never named:\n%s", script.Output())
	}
	if res.Bus.Live {
		t.Error("nothing was started, so nothing may claim to be running")
	}
	for _, id := range script.Asked {
		if id == "bus.repair" {
			t.Error("asked to repair a Gateway the user told it not to start")
		}
	}
}

// --profile moves the config, the state dir and the launchd label together. A
// command without it configures one Gateway and health-checks another.
func TestTheProfileReachesEveryGatewayCommand(t *testing.T) {
	gw := &busGateway{up: true}
	answers := baseAnswers()
	opts, script, _ := newOpts(t, answers, func(o *Options) {
		busEnv(t, "v24.19.0", true)(o)
		gw.attach(o)
		o.Detect.OpenClawProfile = "work"
	})
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	ex := busExecOf(t, opts)
	// Only the calls this step makes: detection has its own answer to the same
	// question, in internal/detect.
	mine := map[string]bool{
		"onboard": true, "config get models": true, "config get gateway.port": true,
		"gateway status": true, "gateway install": true, "gateway start": true,
	}
	var seen int
	for _, c := range ex.Calls {
		if !mine[busVerb(c)] {
			continue
		}
		seen++
		if len(c.Args) < 2 || c.Args[0] != "--profile" || c.Args[1] != "work" {
			t.Errorf("ran against the default profile: %s", argvOf(c))
		}
	}
	if seen == 0 {
		t.Fatal("no openclaw command ran at all")
	}
}
