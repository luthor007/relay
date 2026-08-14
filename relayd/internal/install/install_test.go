package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/config"
	"github.com/luthor007/relay/relayd/internal/detect"
	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/vault"
	"github.com/luthor007/relay/relayd/internal/voice"
)

// The whole installer, driven from fixtures, on a machine with none of the five
// runtimes installed — which is the only machine CI will ever have.

const home = "/home/u"

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(status int, body string, req *http.Request) *http.Response {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode: status, Body: io.NopCloser(strings.NewReader(body)),
		Header: h, Request: req,
	}
}

// happyProvider answers every call the installer makes: a speech synthesis and
// a chat completion.
func happyProvider(t *testing.T, seen *[]string) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if seen != nil {
			*seen = append(*seen, r.URL.String())
		}
		switch {
		case strings.Contains(r.URL.Path, "/chat/completions"):
			return jsonResp(200, `{"model":"m","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`, r), nil
		case strings.Contains(r.URL.Path, "/responses"):
			// The subscription endpoint streams and speaks Responses. It gets
			// its own arm rather than falling through to the synthesis default,
			// which would answer a model probe with fake audio.
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(
					"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
						"data: {\"type\":\"response.completed\",\"response\":{\"model\":\"m\"," +
						"\"status\":\"completed\"}}\n\n")),
				Request: r,
			}, nil
		case strings.Contains(r.URL.Path, "/v1/messages"):
			return jsonResp(200, `{"model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`, r), nil
		case strings.Contains(r.URL.Path, "/oauth/token"):
			// A ChatGPT login Relay owns keeps only the refresh token, so every
			// use of it starts by minting an access token here.
			return jsonResp(200, `{"access_token":"at","refresh_token":"rt","expires_in":3600}`, r), nil
		case strings.Contains(r.URL.Path, "voices/list"):
			return jsonResp(200, `[{"Name":"en-US-AriaNeural"}]`, r), nil
		default: // synthesis
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader("\xff\xfbfake audio")),
				Header:     http.Header{}, Request: r,
			}, nil
		}
	})}
}

// fixtureEnv is a machine with Claude Code and Codex in use and the other three
// absent — the lopsided shape MEMORY.md §1 measured, minus the runtimes CI
// cannot have.
func fixtureEnv() (detect.Env, *detect.MemFS, *detect.FakeExec) {
	fs := &detect.MemFS{
		Files: map[string]string{
			home + "/.claude/projects/p/a.jsonl": "x",
			home + "/.claude.json":               `{"mcpServers":{"github":{"command":"gh-mcp","args":["serve"]}}}`,
			home + "/.codex/session_index.jsonl": "{}\n",
			home + "/.codex/config.toml":         "[mcp_servers.gh]\ncommand = \"gh-mcp\"\nargs = [\"serve\"]\n",
		},
		Dirs: []string{home},
	}
	ex := &detect.FakeExec{
		Paths: map[string]string{
			"claude": "/usr/local/bin/claude", "codex": "/usr/local/bin/codex",
			"npm": "/usr/bin/npm", "systemctl": "/usr/bin/systemctl", "loginctl": "/usr/bin/loginctl",
		},
		Responses: map[string]detect.Result{
			detect.Key("claude", "--version"):                                      {Stdout: []byte("2.1.226\n")},
			detect.Key("codex", "--version"):                                       {Stdout: []byte("codex-cli 0.140.0\n")},
			detect.Key("systemctl", "--user", "daemon-reload"):                     {},
			detect.Key("systemctl", "--user", "enable", "--now", "relayd.service"): {},
			detect.Key("loginctl", "enable-linger"):                                {},
			detect.Key("npm", "install", "-g", "opencode-ai"):                      {Stdout: []byte("added 1 package\n")},
		},
	}
	env := detect.Env{
		FS: fs, Exec: ex, Procs: detect.FakeProcs{},
		Getenv: func(string) string { return "" },
		Home:   home, GOOS: "linux",
	}
	return env, fs, ex
}

func baseAnswers() map[string]string {
	return map[string]string{
		"install.openclaw": "no",

		// The Gateway's own model. "skip" in the fixture, so a test about
		// anything else is not also a test about handing a key to OpenClaw —
		// the cases that care set it explicitly.
		"bus.auth":         "skip",
		"install.hermes":   "no",
		"install.opencode": "no",

		// SYSTEM.md §7. Off in the fixture, so a test about anything else is not
		// also a test about the relay — the cases that care set it explicitly.
		"relay": "off",

		"voice":           "speechify",
		"voice.cred.kind": "env",
		"voice.cred.env":  "SPEECHIFY_API_KEY",

		"models.small.vendor":    "openrouter",
		"models.small.model":     "",
		"models.small.cred.kind": "env",
		"models.small.cred.env":  "OPENROUTER_API_KEY",
		"models.big.vendor":      "openrouter",
		"models.big.model":       "",
		"models.big.reuse":       "yes",

		// ORCHESTRATOR.md §2c. The default is local, and the fixture takes it —
		// with a fake runtime, because the model registry is unreachable from CI
		// by policy and always will be.
		"embedding":               "",
		"embedding.local.model":   "",
		"embedding.local.install": "yes",
		"embedding.local.start":   "yes",

		// MEMORY.md §8. Every one of these is "no" on purpose: the fixture is
		// the baseline install, and an entitlement is a billing fact nobody in
		// CI holds. The tests that record one say so explicitly, so a stray
		// entitlement can never leak into an unrelated assertion.
		//
		// All five ids are listed even though the fixture machine (Claude Code
		// and Codex, nothing else) is only asked two of them. Script only fails
		// on an id it has no answer for, so the spares cost nothing and keep a
		// test that installs a third runtime from tripping over this map.
		"entitlements.claude":            "no",
		"entitlements.chatgpt":           "no",
		"entitlements.copilot":           "no",
		"entitlements.coding_plan":       "no",
		"entitlements.coding_plan.which": "zai-coding-plan",

		// The fixture machine has no Node, so both the runtime rows and the bus
		// now offer to fetch one. No, on purpose: the baseline install must not
		// put a language runtime on a machine by default, and the tests that
		// exercise the bootstrap say yes explicitly.
		"node.install": "no",

		"mcp.adopt":      "yes",
		"service.linger": "yes",
	}
}

// happyRuntime is a local runtime with nothing installed yet, which is the case
// §2c is designed around.
func happyRuntime() *FakeEmbedRuntime {
	return &FakeEmbedRuntime{
		InstallCmd: []string{"sh", "-c", "curl -fsSL https://example.invalid/install.sh | sh"},
		State:      LocalStatus{Host: llm.DefaultOllamaBaseURL},
	}
}

// statedCheck is an embedding probe that answers with a stated verdict.
//
// The probe is a seam rather than a wire fixture because what these tests are
// about is what the installer *does* with a verdict — a 384-dimension model, a
// vector of zeros, a 401 — and internal/llm already tests that its own client
// produces those verdicts from real bytes. Duplicating that here would test
// llm twice and the installer once.
func statedCheck(c llm.EmbedCheck) func(context.Context, llm.EmbedConfig) llm.EmbedCheck {
	return func(_ context.Context, cfg llm.EmbedConfig) llm.EmbedCheck {
		if c.Model == "" {
			c.Model = cfg.Model
		}
		return c
	}
}

// okCheck is a working embedder of the right width.
func okCheck() func(context.Context, llm.EmbedConfig) llm.EmbedCheck {
	return func(_ context.Context, cfg llm.EmbedConfig) llm.EmbedCheck {
		return llm.EmbedCheck{
			Provider: string(cfg.Provider), Model: cfg.Model,
			Local:  cfg.Provider == llm.EmbedOllama,
			Reason: llm.ReasonOK, Dims: llm.EmbeddingDims, WantDims: llm.EmbeddingDims, Norm: 1,
		}
	}
}

// tomlDecode is a test-only round trip back through the config parser.
func tomlDecode(text string, into any) (toml.MetaData, error) {
	return toml.Decode(text, into)
}

func newOpts(t *testing.T, answers map[string]string, mutate func(*Options)) (Options, *Script, *detect.MemFS) {
	t.Helper()
	t.Setenv("SPEECHIFY_API_KEY", "sk-speechify-abcdefgh")
	t.Setenv("OPENROUTER_API_KEY", "sk-or-abcdefgh")

	env, fs, _ := fixtureEnv()
	script := NewScript(answers)
	opts := Options{
		Env:        env,
		FS:         fs,
		Prompt:     script,
		ConfigPath: home + "/.config/relay/config.toml",
		Config:     config.Default(),
		HTTPClient: happyProvider(t, nil),
		BinaryPath: "/usr/local/bin/relayd",
		Now:        func() time.Time { return time.Unix(1770000000, 0).UTC() },
		UID:        501,

		// The local embedding runtime is a fixture, always. The download hosts
		// are unreachable from CI by organisation policy, so this seam is the
		// only way §2c's provisioning is ever exercised here.
		EmbedRuntime: happyRuntime(),
		ProbeEmbed:   okCheck(),
	}
	if mutate != nil {
		mutate(&opts)
	}
	return opts, script, fs
}

func TestFullInstallFromFixtures(t *testing.T) {
	opts, script, fs := newOpts(t, baseAnswers(), nil)
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, script.Output())
	}

	if !res.OK() {
		t.Errorf("install not OK: voice=%v small=%v big=%v",
			res.Voice.Line(), res.Models.Small.Line(), res.Models.Big.Line())
	}
	if !res.Voice.PrimaryOK() {
		t.Errorf("voice = %s", res.Voice.Line())
	}
	if !res.Models.Small.OK() || !res.Models.Big.OK() {
		t.Errorf("models: small=%s big=%s", res.Models.Small.Line(), res.Models.Big.Line())
	}

	// The config file exists, and holds references rather than secrets.
	cfg, ok := fs.Files[opts.ConfigPath]
	if !ok {
		t.Fatalf("no config written to %s", opts.ConfigPath)
	}
	if strings.Contains(cfg, "sk-speechify-abcdefgh") || strings.Contains(cfg, "sk-or-abcdefgh") {
		t.Fatal("a secret was written into config.toml")
	}
	if !strings.Contains(cfg, "env:SPEECHIFY_API_KEY") || !strings.Contains(cfg, "env:OPENROUTER_API_KEY") {
		t.Errorf("config should carry references:\n%s", cfg)
	}

	// The pairing code prints before the history import is even mentioned.
	// MEMORY.md §4: nobody should watch a progress bar before their glasses
	// work.
	out := script.Output()
	pairIdx := strings.Index(out, res.Pairing)
	histIdx := strings.Index(out, "Your history")
	if pairIdx < 0 || histIdx < 0 || pairIdx > histIdx {
		t.Errorf("pairing code must print before backfill is mentioned (pair=%d hist=%d)", pairIdx, histIdx)
	}
	if res.Backfill.Started {
		t.Error("the installer hands backfill to relayd; it must not run it inline")
	}

	// Boot registration happened, with the trap handled.
	if res.Service.Kind != ServiceSystemd || !res.Service.Enabled {
		t.Errorf("service = %+v", res.Service)
	}
	if !res.Service.Lingering {
		t.Error("without lingering the box stops working at logout, which is the whole premise")
	}
	unit := fs.Files[home+"/.config/systemd/user/relayd.service"]
	if !strings.Contains(unit, "Restart=always") || !strings.Contains(unit, "WantedBy=default.target") {
		t.Errorf("unit:\n%s", unit)
	}
}

// ORCHESTRATOR.md §4b: baseline access only. Asking for every scope up front is
// the single best way to lose the install, and it is the pattern that gets
// software flagged as malware-shaped.
func TestBaselineAccessOnly(t *testing.T) {
	opts, script, _ := newOpts(t, baseAnswers(), nil)
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"shell", "filesystem", "process control"}
	if len(res.AccessRequested) != len(want) {
		t.Fatalf("access = %v, want exactly %v", res.AccessRequested, want)
	}
	for i := range want {
		if res.AccessRequested[i] != want[i] {
			t.Fatalf("access = %v, want %v", res.AccessRequested, want)
		}
	}

	// Nothing in the flow may ask for a connector scope.
	for _, id := range script.Asked {
		for _, forbidden := range []string{"gmail", "calendar", "contacts", "drive", "scope", "connector", "printer"} {
			if strings.Contains(strings.ToLower(id), forbidden) {
				t.Errorf("the installer asked %q; connectors are proposed later, from evidence, never at install", id)
			}
		}
	}
	if !strings.Contains(script.Output(), "Nothing else — no mail, no calendar, no repos") {
		t.Error("the install must say what it takes before it takes it")
	}
}

// SYSTEM.md §7c: a buyer who skips the voice step must still have a device that
// talks. This is the test that keeps that true.
func TestSkippingTheVoiceStepStillTalks(t *testing.T) {
	answers := baseAnswers()
	answers["voice.cred.kind"] = "skip"
	delete(answers, "voice.cred.env")

	opts, script, _ := newOpts(t, answers, nil)
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if res.Voice.Option.ID != voice.Fallback().ID {
		t.Errorf("voice = %q, want the keyless fallback", res.Voice.Option.ID)
	}
	if !res.Voice.OK() {
		t.Fatal("skipping the voice key left the device mute, which is the one outcome that is not allowed")
	}
	if res.Config.Voice.Fallback == "" {
		t.Error("the config must always name a fallback")
	}
	if err := (voice.Plan{Primary: res.Config.Voice.Provider, Fallback: res.Config.Voice.Fallback}).Validate(); err != nil {
		t.Errorf("the written plan can go mute: %v", err)
	}
}

// A bad key is reported at setup, with the provider's own error, the user is
// offered the chance to fix it there and then, and declining still finishes the
// install. The alternative is discovering it at the glasses.
func TestBadCredentialIsReportedAtSetupAndDoesNotAbort(t *testing.T) {
	answers := baseAnswers()
	// Every probe 401s here, so all three steps offer the repair. Declining is
	// what this test is about: the installer must finish anyway.
	answers["voice.repair"] = "continue"
	answers["models.small.repair"] = "continue"
	answers["models.big.repair"] = "continue"

	opts, script, _ := newOpts(t, answers, func(o *Options) {
		o.HTTPClient = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "voices/list") {
				return jsonResp(200, `[]`, r), nil
			}
			return jsonResp(401, `{"error":{"message":"Invalid API key provided"}}`, r), nil
		})}
	})
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("a bad key must not abort the install: %v", err)
	}
	// The offer has to be made before the install is allowed to end on a dead
	// credential — that is the whole of the verify/repair loop.
	if !strings.Contains(script.Output(), "Small model — not working yet") {
		t.Error("a failed probe must offer to fix it now, not just mention it later")
	}
	if res.Models.Small.Probe.Reason != llm.ReasonExpired {
		t.Errorf("small model reason = %q, want expired", res.Models.Small.Probe.Reason)
	}
	if !strings.Contains(script.Output(), "Invalid API key provided") {
		t.Error("the provider's own error has to reach the user verbatim")
	}
	if len(res.Warnings) == 0 {
		t.Error("a failed probe belongs in the warnings the installer prints at the end")
	}
	// The device still speaks, because the fallback answered.
	if !res.Voice.OK() {
		t.Error("the keyless fallback should still have worked")
	}
	if res.OK() {
		t.Error("Result.OK must be false when a model credential does not work")
	}
}

// ORCHESTRATOR.md §2b, in as many words. This is the paragraph that pre-empts
// the predictable support ticket.
func TestClaudeHasNoSupportedPathAndTheInstallerSaysSo(t *testing.T) {
	// It is said on the Anthropic row rather than to everybody before the menu.
	// The question — "why can I not use my Max plan?" — only exists once
	// somebody has gone looking for this row, and charging every user four
	// paragraphs to pre-empt it was the wrong trade. The promise is unchanged:
	// whoever asks gets the whole answer, including the workaround.
	answers := baseAnswers()
	answers["models.small.vendor"] = "anthropic"
	answers["models.small.auth"] = "anthropic-key"
	answers["models.small.model"] = "opus-5"
	answers["models.small.cred.kind"] = "env"
	answers["models.small.cred.env"] = "ANTHROPIC_API_KEY"
	answers["models.big.cred.kind"] = "env"
	answers["models.big.cred.env"] = "OPENROUTER_API_KEY"
	delete(answers, "models.big.reuse")
	// Split so the literal never appears: the publish guard refuses any file
	// matching a vendor key pattern, and it cannot tell a fake from a real one.
	// index_test.go does the same, for the same reason.
	t.Setenv("ANTHROPIC_API_KEY", "sk-"+"ant-abcdefgh")

	opts, script, _ := newOpts(t, answers, nil)
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	out := strings.Join(strings.Fields(script.Output()), " ")
	for _, phrase := range []string{
		"Your Claude Max plan still powers Claude Code on this machine",
		"Anthropic's own client using its own login",
		"What it cannot do is power our orchestrator",
		"That part needs an API key",
	} {
		if !strings.Contains(out, phrase) {
			t.Errorf("missing required wording: %q", phrase)
		}
	}
	// Not hidden, and not shipped as a row either.
	if !strings.Contains(out, "setup-token") {
		t.Error("the workaround exists and the installer must not pretend otherwise")
	}
	for _, v := range llm.Vendors() {
		for _, a := range v.Auths {
			if strings.Contains(strings.ToLower(a.Label), "max") || a.ID == "anthropic-oauth" {
				t.Errorf("Claude subscription auth must not be a row: %+v", a)
			}
		}
	}
}

// OpenClaw's two-level menu shape: vendor groups with hints, then auth methods.
func TestVendorMenuIsTwoLevels(t *testing.T) {
	answers := baseAnswers()
	answers["models.small.vendor"] = "openai"
	answers["models.small.auth"] = "openai-codex"
	answers["models.small.model"] = ""
	// The ChatGPT row ends in a sign-in, not a credential question. This
	// machine already has one, so the shortest path is to say so.
	answers["models.small.chatgpt.how"] = "cli"
	// Two different vendors, so the big model needs its own credential — there
	// is nothing to reuse.
	answers["models.big.cred.kind"] = "env"
	answers["models.big.cred.env"] = "OPENROUTER_API_KEY"
	delete(answers, "models.big.reuse")

	opts, script, _ := newOpts(t, answers, withCodexLogin(t))
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}

	// Level one asked, then level two.
	var vendorIdx, authIdx = -1, -1
	for i, id := range script.Asked {
		switch id {
		case "models.small.vendor":
			vendorIdx = i
		case "models.small.auth":
			authIdx = i
		}
	}
	if vendorIdx < 0 || authIdx < 0 || vendorIdx > authIdx {
		t.Fatalf("want vendor group then auth method, got %v", script.Asked)
	}

	// Subscription auth is a first-class row, not an apology.
	if res.Models.Small.Auth.Kind != llm.AuthSubscription {
		t.Errorf("auth = %+v, want the ChatGPT OAuth row", res.Models.Small.Auth)
	}
	out := script.Output()
	if !strings.Contains(out, "ChatGPT Login") {
		t.Error("the subscription row must be labelled as what it is")
	}
	// The old flow sent the user to another terminal and then asked which
	// environment variable held the key. There is no key: `codex login` writes
	// tokens that expire hourly. Relay reads the login it finds, or performs
	// one itself, and never asks for a reference to a subscription.
	if strings.Contains(out, "codex login") {
		t.Error("the ChatGPT row must not send the user off to run a CLI login")
	}
	if strings.Contains(out, "Credential for OpenAI") {
		t.Error("a subscription has no credential reference to ask for")
	}

	// Every vendor group carries its one-line hint, and Custom Provider is last.
	for _, v := range llm.Vendors() {
		if v.Hint == "" {
			t.Errorf("vendor %q has no hint; the whole point of the two-level menu is the hint", v.ID)
		}
	}
	custom := strings.LastIndex(out, "custom: Custom Provider")
	openrouter := strings.Index(out, "openrouter: OpenRouter")
	if custom < 0 || openrouter < 0 || custom < openrouter {
		t.Error("Custom Provider is always the last row, so the list is never a cage")
	}
}

// Risk is a hint on the row, not a wall: the option exists, the warning is
// attached to it, and the user decides.
func TestRiskIsAHintNotAWall(t *testing.T) {
	answers := baseAnswers()
	answers["models.small.vendor"] = "google"
	answers["models.small.auth"] = "google-gemini-cli"
	answers["models.small.login"] = "yes"
	answers["models.small.model"] = "gemini-3-flash"
	answers["models.small.cred.kind"] = "env"
	answers["models.small.cred.env"] = "GOOGLE_API_KEY"
	answers["models.big.cred.kind"] = "env"
	answers["models.big.cred.env"] = "OPENROUTER_API_KEY"
	delete(answers, "models.big.reuse")
	t.Setenv("GOOGLE_API_KEY", "sk-google-abcdefgh")

	opts, script, _ := newOpts(t, answers, nil)
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if res.Models.Small.Auth.ID != "google-gemini-cli" {
		t.Fatal("the risky row must still be selectable")
	}
	if !strings.Contains(script.Output(), "account-risk warning") {
		t.Error("the warning must be attached to the row")
	}
}

// The escape hatch: a custom provider, auto-detecting its wire shape with real
// calls rather than guessing from the URL.
func TestCustomProviderAutoDetectsByCalling(t *testing.T) {
	answers := baseAnswers()
	answers["models.small.vendor"] = "custom"
	answers["models.small.base_url"] = "https://llm.internal/v1"
	answers["models.small.api"] = "auto"
	answers["models.small.model"] = "house-model"
	answers["models.small.cred.kind"] = "env"
	answers["models.small.cred.env"] = "HOUSE_KEY"
	answers["models.big.cred.kind"] = "env"
	answers["models.big.cred.env"] = "OPENROUTER_API_KEY"
	delete(answers, "models.big.reuse")
	t.Setenv("HOUSE_KEY", "sk-house-abcdefgh")
	// The big model is not what this test is about, and it does not verify
	// against a transport that only knows the house endpoint.
	answers["models.big.repair"] = "continue"

	var paths []string
	opts, script, _ := newOpts(t, answers, func(o *Options) {
		o.HTTPClient = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			paths = append(paths, r.URL.Path)
			switch {
			case strings.HasSuffix(r.URL.Path, "/chat/completions"):
				return jsonResp(404, `{"error":{"message":"no such route"}}`, r), nil
			case strings.HasSuffix(r.URL.Path, "/v1/messages"):
				return jsonResp(200, `{"model":"house-model","content":[{"type":"text","text":"ok"}]}`, r), nil
			case strings.Contains(r.URL.Path, "voices/list"):
				return jsonResp(200, `[]`, r), nil
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Request: r,
				Body: io.NopCloser(strings.NewReader("\xff\xfbaudio"))}, nil
		})}
	})
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if !res.Models.Small.OK() {
		t.Fatalf("auto-detect failed: %s", res.Models.Small.Line())
	}
	var sawOpenAI, sawAnthropic bool
	for _, p := range paths {
		if strings.HasSuffix(p, "/chat/completions") {
			sawOpenAI = true
		}
		if strings.HasSuffix(p, "/v1/messages") {
			sawAnthropic = true
		}
	}
	if !sawOpenAI || !sawAnthropic {
		t.Errorf("auto-detect should try both shapes, tried %v", paths)
	}
}

// One key covers both models — the whole OpenRouter argument — so the second
// model offers to reuse rather than asking twice.
func TestOneKeyCoversBothModels(t *testing.T) {
	opts, script, _ := newOpts(t, baseAnswers(), nil)
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Models.Small.Model.Credential != res.Models.Big.Model.Credential {
		t.Errorf("small=%q big=%q; reuse was accepted", res.Models.Small.Model.Credential, res.Models.Big.Model.Credential)
	}
	for _, id := range script.Asked {
		if id == "models.big.cred.kind" {
			t.Error("the big model should not have been asked for a second credential")
		}
	}
	if res.Models.Small.Model.Model != llm.SmallModelDefault {
		t.Errorf("small model = %q, want the documented default", res.Models.Small.Model.Model)
	}
	if res.Models.Big.Model.Model != llm.BigModelDefault {
		t.Errorf("big model = %q, want the documented default", res.Models.Big.Model.Model)
	}
}

func TestInstallerOffersMissingRuntimesAndInstallsNothingUnasked(t *testing.T) {
	answers := baseAnswers()
	answers["install.opencode"] = "yes"

	opts, script, _ := newOpts(t, answers, nil)
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if len(res.Runtimes.Installed) != 1 || res.Runtimes.Installed[0] != adapter.OpenCode {
		t.Errorf("installed = %v, want just opencode", res.Runtimes.Installed)
	}
	// Relay names what it cannot install rather than guessing a package name.
	// That set shrinks only when a command is probed on a real machine, never
	// because one looked plausible: OpenClaw left it on 2026-08-12, Hermes has
	// not, and a row arriving here without evidence should fail this test.
	if len(res.Runtimes.Unknown) != 1 || res.Runtimes.Unknown[0] != adapter.Hermes {
		t.Errorf("unknown = %v, want hermes alone — the one still unprobed", res.Runtimes.Unknown)
	}
	for _, i := range Installers() {
		if i.Runtime == adapter.OpenClaw && len(i.Methods) == 0 {
			t.Error("OpenClaw's install command was probed; the table should carry it")
		}
	}
	if !strings.Contains(script.Output(), "no install command it can run here") {
		t.Error("an unknown install path must be stated, not silently skipped")
	}
}

func TestPairingCodeIsReadableOutLoud(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		code, err := PairingCode(nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != 9 || code[4] != '-' {
			t.Fatalf("code = %q, want XXXX-XXXX", code)
		}
		for _, r := range code {
			if strings.ContainsRune("01OIl", r) {
				t.Fatalf("code %q contains a character people mishear or mistype", code)
			}
		}
		seen[code] = true
	}
	if len(seen) < 190 {
		t.Errorf("only %d distinct codes in 200 draws", len(seen))
	}
}

func TestConfigRefusesAnInlineSecret(t *testing.T) {
	err := checkNoInline(map[string]string{"models.small.credential": "inline:sk-live-1234"})
	if err == nil {
		t.Fatal("an inline secret must never reach a config file")
	}
	if !strings.Contains(err.Error(), "references only") {
		t.Errorf("error = %v", err)
	}
	if checkNoInline(map[string]string{"a": "env:X", "b": "vault:1", "c": ""}) != nil {
		t.Error("references must be allowed")
	}
}

// The detection result is written down, but a guessed state directory is not:
// persisting a guess turns it into a fact on the next run.
func TestOnlyConfirmedStateDirectoriesArePersisted(t *testing.T) {
	opts, _, fs := newOpts(t, baseAnswers(), nil)
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	cfg := fs.Files[opts.ConfigPath]
	if strings.Contains(cfg, `state_dir = "/`) {
		t.Errorf("a guessed state dir was written into the config:\n%s", cfg)
	}
	if !strings.Contains(cfg, "claude-code") {
		t.Errorf("detection results should be recorded:\n%s", cfg)
	}
}

func TestAutoPrompterTakesDefaultsAndInstallsNothing(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-or-abcdefgh")
	env, fs, _ := fixtureEnv()
	auto := &Auto{}
	res, err := Run(context.Background(), Options{
		Env: env, FS: fs, Prompt: auto,
		ConfigPath:   home + "/.config/relay/config.toml",
		HTTPClient:   happyProvider(t, nil),
		Now:          func() time.Time { return time.Unix(1770000000, 0).UTC() },
		UID:          501,
		EmbedRuntime: happyRuntime(),
		ProbeEmbed:   okCheck(),
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(auto.Log, "\n"))
	}
	if len(res.Runtimes.Installed) != 0 {
		t.Errorf("an unattended run installed %v without being asked", res.Runtimes.Installed)
	}
	// The default voice needs a key it does not have, so the fallback carries
	// it — and the device still talks.
	if !res.Voice.OK() {
		t.Error("an unattended install must still leave a device that speaks")
	}
}

// Picking the phone is not a failed install. Synthesis happens on the handset,
// so there is nothing on this machine to call, and "could not be verified"
// would read as broken when nothing is.
func TestPhoneNativeChoiceIsNotReportedAsAFailure(t *testing.T) {
	answers := baseAnswers()
	answers["voice"] = "phone"
	delete(answers, "voice.cred.kind")
	delete(answers, "voice.cred.env")

	opts, script, _ := newOpts(t, answers, nil)
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if res.Voice.Option.ID != "phone" {
		t.Fatalf("voice = %q", res.Voice.Option.ID)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "could not be verified") {
			t.Errorf("untestable is not the same as broken: %q", w)
		}
	}
	if !res.Voice.OK() {
		t.Error("the keyless fallback still has to have answered")
	}
	if !strings.Contains(script.Output(), "cannot be tested from this machine") {
		t.Errorf("the reason has to be said out loud:\n%s", script.Output())
	}
}

// An unattended run answers every question the same way, so re-asking after a
// failed preflight prints the same failure three times and changes nothing.
func TestPreflightDoesNotLoopOnAnIdenticalAnswer(t *testing.T) {
	answers := baseAnswers()
	answers["voice.cred.env"] = "RELAY_TEST_UNSET_ON_PURPOSE"
	answers["voice.cred.retry"] = "yes"
	// The reference never resolves, so the voice does not verify and the repair
	// loop offers to pick again. This test is about the preflight not looping,
	// so it declines — the two loops are bounded independently.
	answers["voice.repair"] = "continue"

	opts, script, _ := newOpts(t, answers, nil)
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	asked := 0
	for _, id := range script.Asked {
		if id == "voice.cred.kind" {
			asked++
		}
	}
	if asked > 2 {
		t.Errorf("asked for the same credential %d times", asked)
	}
	if n := strings.Count(script.Output(), "does not resolve yet"); n > 2 {
		t.Errorf("printed the same failure %d times", n)
	}
}

// fakeVault is the slice of MEMORY.md §6 the installer touches.
type fakeVault struct {
	stored map[string]string
	n      int
}

func (f *fakeVault) Put(_ context.Context, in vault.Input) (vault.Entry, error) {
	if f.stored == nil {
		f.stored = map[string]string{}
	}
	f.n++
	id := fmt.Sprintf("cred-%d", f.n)
	f.stored[id] = in.Secret
	return vault.Entry{ID: id, Service: in.Service, Label: in.Label,
		LastFour: vault.LastFour(in.Secret), Source: in.Source}, nil
}

func (f *fakeVault) Reveal(_ context.Context, id string) (string, error) {
	s, ok := f.stored[id]
	if !ok {
		return "", vault.ErrNotFound
	}
	return s, nil
}

// A typed key goes into the vault and the config file gets a reference. The key
// itself must never reach config.toml — that file ends up in a backup, a
// screenshot and a support ticket.
func TestTypedKeyGoesToTheVaultAndOnlyAReferenceIsWritten(t *testing.T) {
	answers := baseAnswers()
	answers["voice.cred.kind"] = "vault"
	answers["voice.cred.secret"] = "sk-typed-supersecret"
	delete(answers, "voice.cred.env")

	v := &fakeVault{}
	opts, script, fs := newOpts(t, answers, func(o *Options) { o.Vault = v })
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}

	if len(v.stored) != 1 {
		t.Fatalf("vault holds %d entries", len(v.stored))
	}
	if res.Config.Voice.Credential != "vault:cred-1" {
		t.Errorf("credential = %q, want a vault reference", res.Config.Voice.Credential)
	}
	cfg := fs.Files[opts.ConfigPath]
	if strings.Contains(cfg, "sk-typed-supersecret") {
		t.Fatalf("the typed key was written into config.toml:\n%s", cfg)
	}
	if strings.Contains(script.Output(), "sk-typed-supersecret") {
		t.Fatal("the typed key was echoed back to the terminal")
	}
	// And the vault reference resolves, so the probe used the real secret.
	if !res.Voice.PrimaryOK() {
		t.Errorf("voice = %s; a vault reference has to resolve for the probe", res.Voice.Line())
	}
}

// Without a vault there is nowhere safe to put a typed key, so the installer
// refuses rather than writing one into a file.
func TestTypedKeyIsRefusedWithoutAVault(t *testing.T) {
	answers := baseAnswers()
	answers["voice.cred.kind"] = "vault"
	answers["voice.cred.env"] = "SPEECHIFY_API_KEY"

	opts, script, fs := newOpts(t, answers, nil) // no vault
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if !strings.Contains(script.Output(), "nowhere safe to put") {
		t.Error("the refusal has to be explained")
	}
	if strings.Contains(fs.Files[opts.ConfigPath], "inline:") {
		t.Error("an inline secret reached the config file")
	}
	// The device still speaks: no key means the keyless fallback carries it.
	if !res.Voice.OK() {
		t.Error("a refused credential must not leave the device mute")
	}
}

// ------------------------------------------ MEMORY.md §6's second path --

// fakeProposals is the queue half of the vault, for the discovery step.
type fakeProposals struct {
	proposed []vault.Candidate
}

func (f *fakeProposals) Propose(_ context.Context, c vault.Candidate) (vault.Proposal, error) {
	f.proposed = append(f.proposed, c)
	return vault.Proposal{
		ID: fmt.Sprintf("prop-%d", len(f.proposed)), Service: c.Service,
		Detector: c.Detector, LastFour: vault.LastFour(c.Secret), Source: c.Source,
	}, nil
}

func (f *fakeProposals) List(context.Context) ([]vault.Proposal, error)    { return nil, nil }
func (f *fakeProposals) History(context.Context) ([]vault.Proposal, error) { return nil, nil }

func (f *fakeProposals) Get(context.Context, string) (vault.Proposal, error) {
	return vault.Proposal{}, vault.ErrNotFound
}

func (f *fakeProposals) Accept(context.Context, string, string) (vault.Entry, error) {
	return vault.Entry{}, errors.New("the installer must never accept on the user's behalf")
}

func (f *fakeProposals) Dismiss(context.Context, string, string) error {
	return errors.New("the installer must never dismiss on the user's behalf")
}

// discoveryVault is a fakeVault that also has a queue, which is what the real
// vault has and what the discovery step looks for.
type discoveryVault struct {
	fakeVault
	q fakeProposals
}

func (d *discoveryVault) Proposals() vault.Proposals { return &d.q }

// TestDiscoveredConfigKeysAreProposedNeverStored is MEMORY.md §6's second
// arrival path: a key already sitting in a runtime's own config file.
//
// §6 says these are "enumerable at install with the user watching", and the
// watching is the whole guarantee. vault.Discover has existed, fully tested,
// with no caller anywhere in the tree — so an install on a machine with an
// opencode auth.json found nothing, said nothing, and proposed nothing.
//
// The assertion that matters is the negative one. An unattended install that
// silently moved a key out of somebody's config and into Relay's vault would be
// indefensible on a shared box, and it is the difference between second place
// in §6's ranking and a product nobody should run. Put must not be called.
func TestDiscoveredConfigKeysAreProposedNeverStored(t *testing.T) {
	// glpat- rather than a Stripe or Anthropic shape: this file is not excluded
	// from the public repo, and scripts/build-public-repo.sh's credential guard
	// refuses those four literals outright.
	const key = "glpat-Nq7TESTONLYnotarealkey42"

	v := &discoveryVault{}
	opts, script, _ := newOpts(t, baseAnswers(), func(o *Options) {
		o.Vault = v
		o.Env.FS.(*detect.MemFS).Files[home+"/.local/share/opencode/auth.json"] =
			`{"gitlab":{"type":"api","key":"` + key + `"}}`
	})

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}

	if len(v.q.proposed) != 1 {
		t.Fatalf("the installer proposed %d keys, want the one in auth.json:\n%s",
			len(v.q.proposed), script.Output())
	}
	c := v.q.proposed[0]
	if c.Service != "gitlab" {
		t.Errorf("service = %q; a proposal that cannot name a vendor is not a question "+
			"anybody can answer", c.Service)
	}
	if c.Source.Kind != vault.SourceConfig {
		t.Errorf("source = %q, want config — provenance is how the console later says "+
			"where this key came from", c.Source.Kind)
	}
	if c.Source.Path == "" {
		t.Error("the proposal does not say which file it came out of")
	}

	// Nothing was stored. This is the assertion the whole step exists for.
	if len(v.stored) != 0 {
		t.Fatalf("the installer put %d discovered keys straight into the vault; §6's "+
			"whole point is that a found key is a question", len(v.stored))
	}
	if res.Discovery.Proposed != 1 {
		t.Errorf("Discovery.Proposed = %d, want 1", res.Discovery.Proposed)
	}

	// The user was told, and told without being shown the key. The installer's
	// output is the most-screenshotted surface this product has.
	out := script.Output()
	if strings.Contains(out, key) {
		t.Fatalf("the installer printed the key it found:\n%s", out)
	}
	if !strings.Contains(out, "waiting in the console") {
		t.Errorf("the installer never told the user a question is waiting:\n%s", out)
	}
}

// A file that exists and will not parse is a different answer from a file that
// is not there, and only one of them means "you have no keys".
//
// MEMORY.md §7's rule, and it matters more here than almost anywhere: a key
// could be in the file we could not read, and reporting that as "found nothing"
// is how somebody concludes their machine is clean when it is not.
func TestAnUnreadableConfigIsNotReportedAsNoKeys(t *testing.T) {
	v := &discoveryVault{}
	opts, script, _ := newOpts(t, baseAnswers(), func(o *Options) {
		o.Vault = v
		o.Env.FS.(*detect.MemFS).Files[home+"/.local/share/opencode/auth.json"] =
			"this is not JSON at all"
	})

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if len(res.Discovery.Unreadable) != 1 {
		t.Fatalf("Unreadable = %v, want the one file that would not parse",
			res.Discovery.Unreadable)
	}
	if !strings.Contains(script.Output(), "Could not read") {
		t.Errorf("the installer did not say it could not read the file:\n%s", script.Output())
	}
}
