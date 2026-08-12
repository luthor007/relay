package install

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/config"
	"github.com/luthor007/relay/relayd/internal/detect"
	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/search"
)

// ORCHESTRATOR.md §2c, driven from fixtures.
//
// Nothing here downloads anything and nothing here opens a socket. The local
// runtime is a FakeEmbedRuntime and the probe is either a stated answer or a
// RoundTripper, which is not only a testing convenience: ollama.com,
// registry.ollama.ai and huggingface.co are unreachable from this build
// environment by organisation policy, so a fixture is the *only* way this path
// is exercised at all. The real download is unexercised, and that is stated
// rather than implied.

// --------------------------------------------------------------- the menu --

// Only models that can write into the index are offered. A row that cannot is a
// download followed by a refusal, which is the failure the catalog exists to
// prevent.
func TestEveryOfferedLocalModelFitsTheIndex(t *testing.T) {
	answers := baseAnswers()
	opts, script, _ := newOpts(t, answers, nil)
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}

	out := script.Output()
	start := strings.Index(out, "Which local model?")
	if start < 0 {
		t.Fatal("no local model menu in the output")
	}
	section := out[start:]
	if end := strings.Index(section, "## "); end > 0 {
		section = section[:end]
	}

	for _, m := range llm.LocalEmbedModels() {
		row := "  - " + m.ID + ":"
		switch {
		case m.FitsIndex() && !strings.Contains(section, row):
			t.Errorf("%s fits the index and is not offered", m.ID)
		case !m.FitsIndex() && strings.Contains(section, row):
			t.Errorf("%s is %d dimensions and must not be a selectable row", m.ID, m.Dims)
		}
	}
	// The ones that are left out are named, with their width, rather than
	// silently missing — otherwise the menu just looks arbitrary.
	if !strings.Contains(section, "mxbai-embed-large at 1024") ||
		!strings.Contains(section, "all-minilm at 384") {
		t.Errorf("the excluded models must be named with their widths:\n%s", section)
	}
}

// The refusal happens at selection, before anything is pulled. After a download
// is too late; after a backfill is a disaster.
func TestWrongWidthModelIsRefusedBeforeAnythingIsDownloaded(t *testing.T) {
	answers := baseAnswers()
	answers["embedding.local.model"] = "other"
	answers["embedding.local.other"] = "mxbai-embed-large"

	rt := happyRuntime()
	opts, script, _ := newOpts(t, answers, func(o *Options) { o.EmbedRuntime = rt })

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}

	out := script.Output()
	if !strings.Contains(out, "1024") || !strings.Contains(out, "768") {
		t.Errorf("the refusal must print both widths:\n%s", out)
	}
	for _, c := range rt.Calls {
		if strings.HasPrefix(c, "pull:mxbai") {
			t.Fatalf("mxbai-embed-large was pulled anyway: %v", rt.Calls)
		}
	}
	// Three refusals fall back to the recommendation rather than looping.
	if res.Embedding.Model != llm.RecommendedLocalEmbed().ID {
		t.Errorf("model = %q, want the recommendation after a refusal", res.Embedding.Model)
	}
}

// A model not in the catalog is allowed through — the list is a menu, not a
// cage — and the user is told plainly that the check is still coming.
func TestUnknownModelIsAllowedAndTheProbeDecides(t *testing.T) {
	answers := baseAnswers()
	answers["embedding.local.model"] = "other"
	answers["embedding.local.other"] = "somebody-elses-embedder"

	opts, script, _ := newOpts(t, answers, nil)
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if res.Embedding.Model != "somebody-elses-embedder" {
		t.Errorf("model = %q", res.Embedding.Model)
	}
	if !strings.Contains(script.Output(), "does not know how wide") {
		t.Errorf("an unknown width must be said out loud:\n%s", script.Output())
	}
	if !res.Embedding.OK() {
		t.Errorf("the probe answered at the right width, so it should be accepted: %s",
			res.Embedding.Line())
	}
}

// A model that pulls and then reports the wrong width has to fail at setup,
// with the model named and both numbers printed. store.PutSummary,
// store.SearchVector and search.New all refuse it too — but all three run after
// the backfill, and discovering it there costs an hour or two.
func TestModelThatReturnsTheWrongWidthFailsAtSetup(t *testing.T) {
	opts, script, fs := newOpts(t, baseAnswers(), func(o *Options) {
		o.ProbeEmbed = statedCheck(llm.EmbedCheck{
			Provider: "ollama", Model: "nomic-embed-text", Local: true,
			Reason: llm.ReasonWrongWidth, Dims: 384, WantDims: llm.EmbeddingDims,
			Detail: "nomic-embed-text returned 384 dimensions and the index is 768 wide",
		})
	})
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}

	if res.Embedding.Check.Reason != llm.ReasonWrongWidth {
		t.Fatalf("reason = %q, want %q", res.Embedding.Check.Reason, llm.ReasonWrongWidth)
	}
	// The model and both numbers have to be on the screen, not just in a field.
	out := script.Output()
	for _, want := range []string{"nomic-embed-text", "384", "768"} {
		if !strings.Contains(out, want) {
			t.Errorf("the failure must name %q:\n%s", want, out)
		}
	}
	if len(res.Embedding.Warnings) == 0 {
		t.Error("a wrong-width model must leave a warning on the install")
	}
	// And nothing broken gets written down: an embedder that cannot write to
	// the index is not configured as one.
	if res.Embedding.Config.Configured() {
		t.Errorf("config = %+v, want no embedder", res.Embedding.Config)
	}
	if strings.Contains(fs.Files[opts.ConfigPath], "nomic-embed-text") {
		t.Errorf("a model that failed its probe was written into config.toml:\n%s",
			fs.Files[opts.ConfigPath])
	}
}

// An all-zero vector is worse than an error: it indexes, it queries, and it
// puts arbitrary neighbours at the top of every result list.
func TestDegenerateVectorIsRefused(t *testing.T) {
	opts, script, _ := newOpts(t, baseAnswers(), func(o *Options) {
		o.ProbeEmbed = statedCheck(llm.EmbedCheck{
			Provider: "ollama", Model: "nomic-embed-text", Local: true,
			Reason: llm.ReasonDegenerate, Dims: llm.EmbeddingDims, WantDims: llm.EmbeddingDims,
			Detail: "nomic-embed-text returned 768 zeros",
		})
	})
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if res.Embedding.Check.Reason != llm.ReasonDegenerate {
		t.Fatalf("reason = %q, want %q", res.Embedding.Check.Reason, llm.ReasonDegenerate)
	}
	if res.Embedding.Config.Configured() {
		t.Error("a degenerate embedder must not be written into the config")
	}
	if !res.OK() {
		t.Error("a failed embedder is still not a failed install: the box works, search is worse")
	}
}

// ------------------------------------------------------------ provisioning --

func TestLocalEmbeddingIsInstalledPulledAndProbed(t *testing.T) {
	rt := happyRuntime()
	opts, script, fs := newOpts(t, baseAnswers(), func(o *Options) { o.EmbedRuntime = rt })

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}

	if res.Embedding.Kind != EmbedLocal {
		t.Fatalf("kind = %q, want local (%s)", res.Embedding.Kind, res.Embedding.Line())
	}
	if !res.Embedding.OK() {
		t.Fatalf("embedding not OK: %s", res.Embedding.Line())
	}
	if !res.Embedding.Installed || !res.Embedding.Pulled {
		t.Errorf("installed=%v pulled=%v — §2c provisions the local option rather than "+
			"sending the user to a README", res.Embedding.Installed, res.Embedding.Pulled)
	}

	// Install, then pull, and only then a real call. In that order.
	want := "status,install,status,pull:" + llm.RecommendedLocalEmbed().ID
	if strings.Join(rt.Calls, ",") != want {
		t.Errorf("calls = %v, want %v", rt.Calls, want)
	}

	if res.Config.Embedding.Provider != config.EmbedProviderOllama ||
		res.Config.Embedding.Dims != config.EmbeddingDims {
		t.Errorf("config = %+v", res.Config.Embedding)
	}
	// The local option needs no credential, which is most of the point of it.
	if res.Config.Embedding.Credential != "" {
		t.Errorf("the local embedder must need no credential; got %q", res.Config.Embedding.Credential)
	}
	// And the default endpoint is not pinned into the file, because a repeated
	// default is a pin that ignores whoever moves OLLAMA_HOST next.
	if res.Config.Embedding.BaseURL != "" {
		t.Errorf("base_url = %q, want empty for the default host", res.Config.Embedding.BaseURL)
	}

	written := fs.Files[opts.ConfigPath]
	if !strings.Contains(written, "[embedding]") ||
		!strings.Contains(written, llm.RecommendedLocalEmbed().ID) {
		t.Errorf("config.toml should carry the embedding table:\n%s", written)
	}
	// A written config has to survive being read back — including validation,
	// which refuses any width but the index's.
	if err := res.Config.Validate(); err != nil {
		t.Errorf("the installer wrote a config that does not validate: %v", err)
	}

	// The exact install command is shown before it is agreed to, like every
	// other install in this installer.
	if !strings.Contains(script.Output(), "Relay would run: sh -c curl") {
		t.Errorf("the install command must be printed before it runs:\n%s", script.Output())
	}
}

// A moved OLLAMA_HOST is recorded, because that one is not a default.
func TestMovedLocalHostIsRecorded(t *testing.T) {
	rt := happyRuntime()
	rt.State.Host = "http://box.lan:11434"

	opts, _, _ := newOpts(t, baseAnswers(), func(o *Options) { o.EmbedRuntime = rt })
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Config.Embedding.BaseURL != "http://box.lan:11434" {
		t.Errorf("base_url = %q", res.Config.Embedding.BaseURL)
	}
}

// The step runs before the pairing code, because the backfill that follows the
// pairing code is what creates the index, and the width is fixed at creation.
func TestEmbeddingStepRunsBeforeThePairingCode(t *testing.T) {
	opts, script, _ := newOpts(t, baseAnswers(), nil)
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	out := script.Output()

	modelsIdx := strings.Index(out, "Choose the orchestrator models")
	embedIdx := strings.Index(out, "Choose an embedding model")
	pairIdx := strings.Index(out, res.Pairing)
	histIdx := strings.Index(out, "Your history")

	if modelsIdx < 0 || embedIdx < 0 || pairIdx < 0 || histIdx < 0 {
		t.Fatalf("missing sections: models=%d embed=%d pair=%d hist=%d",
			modelsIdx, embedIdx, pairIdx, histIdx)
	}
	if !(modelsIdx < embedIdx && embedIdx < pairIdx && pairIdx < histIdx) {
		t.Errorf("order must be models → embedding → pairing → backfill; got %d %d %d %d",
			modelsIdx, embedIdx, pairIdx, histIdx)
	}
}

// Installed and running are two different states, and the difference is the
// failure that otherwise surfaces as a connection refused mid-install.
func TestServiceNotRunningIsNamedAndRecheckable(t *testing.T) {
	rt := happyRuntime()
	rt.State = LocalStatus{
		Installed: true, Running: false, Host: llm.DefaultOllamaBaseURL,
		Detail: "dial tcp 127.0.0.1:11434: connect: connection refused",
	}
	rt.StartsOnStatusCall = 2 // the user goes and starts it

	opts, script, _ := newOpts(t, baseAnswers(), func(o *Options) { o.EmbedRuntime = rt })
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}

	out := script.Output()
	if !strings.Contains(out, "nothing is answering on "+llm.DefaultOllamaBaseURL) {
		t.Errorf("the not-running state must be named with the host:\n%s", out)
	}
	if !strings.Contains(out, "Relay will not start it for you") {
		t.Errorf("Relay must say it is not guessing a start command:\n%s", out)
	}
	if !strings.Contains(out, "connection refused") {
		t.Errorf("the transport error must survive to the screen:\n%s", out)
	}
	if !res.Embedding.OK() {
		t.Errorf("after the service came up the step should succeed: %s", res.Embedding.Line())
	}
	// Already installed, so nothing was installed again.
	if res.Embedding.Installed {
		t.Error("Ollama was already installed; the step must not reinstall it")
	}
}

func TestServiceThatNeverComesUpFallsBackToKeywordOnly(t *testing.T) {
	rt := happyRuntime()
	rt.State = LocalStatus{Installed: true, Host: llm.DefaultOllamaBaseURL, Detail: "connection refused"}

	opts, script, _ := newOpts(t, baseAnswers(), func(o *Options) { o.EmbedRuntime = rt })
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}

	if res.Embedding.Kind != EmbedNone {
		t.Fatalf("kind = %q, want a fall back to none", res.Embedding.Kind)
	}
	// Not a failed install: the box works, search is worse, one command fixes
	// it, and nothing was embedded so nothing has to be undone.
	if !res.OK() {
		t.Error("a missing embedder is not a failed install")
	}
	if !strings.Contains(strings.Join(res.Embedding.Warnings, " "), "relay embed") {
		t.Errorf("warnings = %v, want one naming the command that fixes it", res.Embedding.Warnings)
	}
	for _, c := range rt.Calls {
		if strings.HasPrefix(c, "pull:") {
			t.Errorf("nothing should be pulled into a runtime that is not running: %v", rt.Calls)
		}
	}
}

// Relay never invents an install command. On a machine with no command it can
// run, it says so and gets out of the way.
func TestNoInstallCommandIsSaidRatherThanGuessed(t *testing.T) {
	rt := happyRuntime()
	rt.InstallCmd = nil

	opts, script, _ := newOpts(t, baseAnswers(), func(o *Options) { o.EmbedRuntime = rt })
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if res.Embedding.Kind != EmbedNone {
		t.Errorf("kind = %q", res.Embedding.Kind)
	}
	out := script.Output()
	if !strings.Contains(out, "no install command it can run") ||
		!strings.Contains(out, rt.Docs()) {
		t.Errorf("Relay must name the runtime and where to get it:\n%s", out)
	}
	for _, c := range rt.Calls {
		if c == "install" {
			t.Fatal("Relay ran an install it said it did not have")
		}
	}
}

// A pull that fails is a warning and a working box, not a dead install.
func TestFailedPullFallsBackRatherThanAborting(t *testing.T) {
	rt := happyRuntime()
	rt.PullErr = errors.New("exit 1: no space left on device")

	opts, script, _ := newOpts(t, baseAnswers(), func(o *Options) { o.EmbedRuntime = rt })
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("a failed pull must not abort the install: %v\n%s", err, script.Output())
	}
	if res.Embedding.Kind != EmbedNone {
		t.Errorf("kind = %q", res.Embedding.Kind)
	}
	if !strings.Contains(strings.Join(res.Embedding.Warnings, " "), "no space left on device") {
		t.Errorf("the real reason must survive: %v", res.Embedding.Warnings)
	}
	if res.Pairing == "" {
		t.Error("the install must still finish and print a pairing code")
	}
}

// Nothing installs without being asked, and the question defaults to no. An
// unattended run must not put a model runtime on somebody's machine.
func TestUnattendedRunInstallsNoLocalRuntime(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-or-abcdefgh")
	env, fs, _ := fixtureEnv()
	rt := happyRuntime()
	auto := &Auto{}

	res, err := Run(context.Background(), Options{
		Env: env, FS: fs, Prompt: auto,
		ConfigPath:   home + "/.config/relay/config.toml",
		HTTPClient:   happyProvider(t, nil),
		EmbedRuntime: rt,
		ProbeEmbed:   okCheck(),
		UID:          501,
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(auto.Log, "\n"))
	}
	for _, c := range rt.Calls {
		if c == "install" || strings.HasPrefix(c, "pull:") {
			t.Fatalf("an unattended run did %q without being asked", c)
		}
	}
	if res.Embedding.Kind != EmbedNone {
		t.Errorf("kind = %q; declining the install leaves keyword-only search", res.Embedding.Kind)
	}
}

// --------------------------------------------------------------- the none --

func TestNoEmbedderIsASupportedState(t *testing.T) {
	answers := baseAnswers()
	answers["embedding"] = "none"
	delete(answers, "embedding.local.model")
	delete(answers, "embedding.local.install")

	opts, script, fs := newOpts(t, answers, nil)
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}

	if !res.Embedding.OK() || !res.OK() {
		t.Fatal("choosing none on purpose is not a failed install")
	}
	if res.Config.Embedding.Configured() {
		t.Errorf("config = %+v", res.Config.Embedding)
	}
	if !strings.Contains(script.Output(), "keyword") {
		t.Errorf("the consequence must be stated:\n%s", script.Output())
	}
	// "none" is written down rather than left empty, so relayd reads a definite
	// answer instead of applying the default and turning the embedder back on.
	written := fs.Files[opts.ConfigPath]
	if !strings.Contains(written, `provider = "none"`) {
		t.Errorf("config.toml:\n%s", written)
	}
	// And it survives a round trip through the loader, which fills an *absent*
	// provider from the default.
	var loaded config.Config
	if _, err := tomlDecode(written, &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.Embedding.Configured() {
		t.Errorf("a deliberate 'none' was turned back on by the defaults: %+v", loaded.Embedding)
	}
}

// The copy must not claim local saves money. It does not — under a dollar
// across the whole corpus — and a hint a user can disprove costs you the ones
// that are true.
func TestCopyDoesNotClaimLocalSavesMoney(t *testing.T) {
	opts, script, _ := newOpts(t, baseAnswers(), nil)
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	out := strings.ToLower(script.Output())

	start := strings.Index(out, "choose an embedding model")
	if start < 0 {
		t.Fatal("no embedding section in the output")
	}
	section := out[start:]
	if end := strings.Index(section, "## which"); end > 0 {
		section = section[:end]
	}
	for _, forbidden := range []string{"saves money", "cheaper", "free forever", "no api bill"} {
		if strings.Contains(section, forbidden) {
			t.Errorf("the embedding copy claims %q, which is not true at these volumes:\n%s",
				forbidden, section)
		}
	}
	// And it must make the argument it can actually stand behind, plus name the
	// inversion rather than leaving a reader to wonder.
	if !strings.Contains(section, "leaves this machine") && !strings.Contains(section, "leave") {
		t.Errorf("the privacy argument is the whole case for local and it is missing:\n%s", section)
	}
	if !strings.Contains(section, "opposite of the voice and model steps") {
		t.Errorf("the inverted recommendation must explain itself:\n%s", section)
	}
}

// ---------------------------------------------------------------- hosted --

func hostedAnswers() map[string]string {
	a := baseAnswers()
	a["embedding"] = "hosted"
	delete(a, "embedding.local.model")
	delete(a, "embedding.local.install")
	delete(a, "embedding.local.start")
	a["embedding.hosted.model"] = ""
	a["embedding.hosted.reuse"] = "yes"
	return a
}

func TestHostedEmbeddingReusesTheModelsKey(t *testing.T) {
	var probed llm.EmbedConfig
	opts, script, fs := newOpts(t, hostedAnswers(), func(o *Options) {
		o.ProbeEmbed = func(_ context.Context, cfg llm.EmbedConfig) llm.EmbedCheck {
			probed = cfg
			return llm.EmbedCheck{
				Provider: string(cfg.Provider), Model: cfg.Model,
				Reason: llm.ReasonOK, Dims: llm.EmbeddingDims, WantDims: llm.EmbeddingDims, Norm: 1,
			}
		}
	})

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if !res.Embedding.OK() || res.Embedding.Kind != EmbedHosted {
		t.Fatalf("embedding = %s", res.Embedding.Line())
	}
	if probed.Provider != llm.EmbedOpenAI || probed.Vendor != "openrouter" {
		t.Errorf("probed = %+v, want the OpenAI-compatible path on OpenRouter", probed)
	}
	// The key is reused from the models step rather than asked for twice.
	if probed.Credential.String() != "env:OPENROUTER_API_KEY" {
		t.Errorf("credential = %q", probed.Credential.String())
	}
	if res.Config.Embedding.Provider != "openrouter" {
		t.Errorf("config.provider = %q; for a hosted embedder the vendor id is the provider",
			res.Config.Embedding.Provider)
	}
	if strings.Contains(fs.Files[opts.ConfigPath], "sk-or-abcdefgh") {
		t.Fatal("a secret was written into config.toml")
	}
	if err := res.Config.Validate(); err != nil {
		t.Errorf("the installer wrote a config that does not validate: %v", err)
	}
	// The hosted row's honest tradeoff has to be on the screen.
	if !strings.Contains(script.Output(), "sent to a provider") {
		t.Errorf("the hosted row must say what leaves the machine:\n%s", script.Output())
	}
}

// A hosted provider that rejects the credential falls back to keyword-only
// rather than writing a broken embedder into the config, and reports what the
// provider actually said.
func TestHostedFailureFallsBackAndKeepsTheProvidersWords(t *testing.T) {
	opts, script, _ := newOpts(t, hostedAnswers(), func(o *Options) {
		o.ProbeEmbed = statedCheck(llm.EmbedCheck{
			Provider: "openai", Model: "openai/text-embedding-3-small",
			Reason: llm.ReasonExpired, WantDims: llm.EmbeddingDims,
			Detail: "http 401: invalid api key",
		})
	})
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if res.Embedding.Kind != EmbedNone {
		t.Errorf("kind = %q, want a fall back to keyword-only", res.Embedding.Kind)
	}
	if !strings.Contains(script.Output(), "invalid api key") {
		t.Errorf("the provider's own message must survive:\n%s", script.Output())
	}
	if res.Config.Embedding.Configured() {
		t.Errorf("config = %+v", res.Config.Embedding)
	}
}

// --------------------------------------------------------------- doctor --

func TestDoctorReportsTheEmbedderSeparately(t *testing.T) {
	rt := happyRuntime()
	rt.State = LocalStatus{Installed: true, Host: llm.DefaultOllamaBaseURL,
		Detail: "dial tcp 127.0.0.1:11434: connect: connection refused"}

	script := NewScript(map[string]string{})
	env, fs, _ := fixtureEnv()
	cfg := config.Default()

	d := RunDoctor(context.Background(), Options{
		Env: env, FS: fs, Prompt: script, Config: cfg,
		HTTPClient:   happyProvider(t, nil),
		EmbedRuntime: rt,
		ProbeEmbed: statedCheck(llm.EmbedCheck{
			Provider: "ollama", Model: cfg.Embedding.Model, Local: true,
			Reason: llm.ReasonUnavailable, WantDims: llm.EmbeddingDims,
			Detail: "dial tcp 127.0.0.1:11434: connect: connection refused",
		}),
		Index: &fakeIndex{state: search.EmbeddingState{
			Indexed: cfg.Embedding.Model, Current: cfg.Embedding.Model,
			Summaries: 22000, Vectors: 22000, Dims: 768,
		}},
	})
	d.Print(script)

	if d.EmbedOK() || d.OK() {
		t.Fatal("an embedder that cannot be reached is not OK")
	}
	out := script.Output()
	// "search got worse" is the support question; the page has to answer it
	// without a round trip.
	if !strings.Contains(out, "the service is down") {
		t.Errorf("doctor must distinguish a down service from a bad model:\n%s", out)
	}
	if !strings.Contains(out, "127.0.0.1:11434") {
		t.Errorf("doctor must name the host:\n%s", out)
	}
}

func TestDoctorFlagsAnIndexModelMismatch(t *testing.T) {
	rt := happyRuntime()
	rt.State = LocalStatus{Installed: true, Running: true, Host: llm.DefaultOllamaBaseURL}

	script := NewScript(map[string]string{})
	env, fs, _ := fixtureEnv()
	cfg := config.Default()

	d := RunDoctor(context.Background(), Options{
		Env: env, FS: fs, Prompt: script, Config: cfg,
		HTTPClient: happyProvider(t, nil), EmbedRuntime: rt, ProbeEmbed: okCheck(),
		Index: &fakeIndex{state: search.EmbeddingState{
			Indexed: "granite-embedding:278m", Summaries: 100, Vectors: 100, Dims: 768,
		}},
	})
	d.Print(script)

	if d.EmbedOK() {
		t.Fatal("an index and a config that disagree is a fault, not a detail")
	}
	out := script.Output()
	if !strings.Contains(out, "relay reindex") {
		t.Errorf("doctor must name the command that fixes it:\n%s", out)
	}
	if !strings.Contains(out, "granite-embedding:278m") || !strings.Contains(out, cfg.Embedding.Model) {
		t.Errorf("doctor must name both models:\n%s", out)
	}
}

// No embedder configured is not a fault. A doctor that reports a deliberate
// choice as broken teaches people to ignore it.
func TestDoctorTreatsNoEmbedderAsFine(t *testing.T) {
	script := NewScript(map[string]string{})
	env, fs, _ := fixtureEnv()
	cfg := config.Default()
	cfg.Embedding = config.Embedding{Provider: config.EmbedProviderNone}

	d := RunDoctor(context.Background(), Options{
		Env: env, FS: fs, Prompt: script, Config: cfg,
		HTTPClient: happyProvider(t, nil), EmbedRuntime: happyRuntime(),
	})
	if !d.EmbedOK() {
		t.Error("keyword-only is a supported state")
	}
	d.Print(script)
	if !strings.Contains(script.Output(), "supported state") {
		t.Errorf("doctor:\n%s", script.Output())
	}
}

// --------------------------------------------------------------- reindex --

type fakeIndex struct {
	state   search.EmbeddingState
	resetTo string
	reset   bool
}

func (f *fakeIndex) Inspect(context.Context) (search.EmbeddingState, error) { return f.state, nil }

func (f *fakeIndex) Reset(_ context.Context, model string) (int64, error) {
	f.reset, f.resetTo = true, model
	n := f.state.Vectors
	f.state.Vectors, f.state.Indexed = 0, model
	return n, nil
}

func reindexOpts(t *testing.T, idx Index, answers map[string]string) (Options, *Script) {
	t.Helper()
	env, fs, _ := fixtureEnv()
	script := NewScript(answers)
	cfg := config.Default()
	return Options{
		Env: env, FS: fs, Prompt: script, Config: cfg,
		ConfigPath: home + "/.config/relay/config.toml",
		Index:      idx,
	}, script
}

func TestReindexClearsOnlyAfterConfirmation(t *testing.T) {
	idx := &fakeIndex{state: search.EmbeddingState{
		Indexed: "granite-embedding:278m", Summaries: 22000, Vectors: 22000, Dims: 768,
	}}
	opts, script := reindexOpts(t, idx, map[string]string{"reindex.confirm": "yes"})

	out, err := RunReindex(context.Background(), opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Needed || !out.Confirmed || out.Cleared != 22000 || !idx.reset {
		t.Fatalf("outcome = %+v", out)
	}
	if idx.resetTo != config.DefaultEmbedModel {
		t.Errorf("the index was handed to %q, want the configured model", idx.resetTo)
	}
	// The expensive part is the summaries and this does not touch them. Saying
	// so is the difference between a command people run and one they fear.
	body := script.Output()
	if !strings.Contains(body, "does not re-summarise") {
		t.Errorf("reindex must say what it does NOT redo:\n%s", body)
	}
	if !strings.Contains(body, "keyword-only") {
		t.Errorf("reindex must say search keeps working:\n%s", body)
	}
}

func TestReindexDeclinedChangesNothing(t *testing.T) {
	idx := &fakeIndex{state: search.EmbeddingState{
		Indexed: "granite-embedding:278m", Summaries: 10, Vectors: 10,
	}}
	opts, _ := reindexOpts(t, idx, map[string]string{"reindex.confirm": "no"})

	out, err := RunReindex(context.Background(), opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Confirmed || idx.reset {
		t.Fatalf("a declined reindex must change nothing: %+v", out)
	}
}

func TestReindexIsANoOpWhenNothingChanged(t *testing.T) {
	idx := &fakeIndex{state: search.EmbeddingState{
		Indexed: config.DefaultEmbedModel, Summaries: 10, Vectors: 10,
	}}
	opts, _ := reindexOpts(t, idx, map[string]string{})

	out, err := RunReindex(context.Background(), opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Needed || idx.reset {
		t.Fatalf("re-embedding with the same model would produce the same vectors: %+v", out)
	}
	if !strings.Contains(out.Message, "--force") {
		t.Errorf("message = %q", out.Message)
	}
}

func TestReindexWithNothingEmbeddedYetSaysSo(t *testing.T) {
	idx := &fakeIndex{state: search.EmbeddingState{Summaries: 500}}
	opts, _ := reindexOpts(t, idx, map[string]string{})

	out, err := RunReindex(context.Background(), opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Needed || idx.reset {
		t.Fatalf("outcome = %+v", out)
	}
	if !strings.Contains(out.Message, "nothing to clear") {
		t.Errorf("message = %q", out.Message)
	}
}

func TestReindexWithoutAnIndexSaysSo(t *testing.T) {
	opts, _ := reindexOpts(t, nil, map[string]string{})
	out, err := RunReindex(context.Background(), opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Needed {
		t.Errorf("outcome = %+v", out)
	}
	if !strings.Contains(out.Message, "nothing to re-embed") {
		t.Errorf("message = %q", out.Message)
	}
}

// ------------------------------------------------------------- re-running --

// `relay embed` stands alone, and when the index disagrees with the new choice
// it says so and names the command rather than leaving search quietly halved.
func TestRunEmbeddingWarnsAboutAnIndexItWouldOrphan(t *testing.T) {
	answers := map[string]string{
		"embedding":               "",
		"embedding.local.model":   "granite-embedding:278m",
		"embedding.local.install": "yes",
	}
	env, fs, _ := fixtureEnv()
	script := NewScript(answers)
	idx := &fakeIndex{state: search.EmbeddingState{
		Indexed: config.DefaultEmbedModel, Summaries: 100, Vectors: 100,
	}}

	out, err := RunEmbedding(context.Background(), Options{
		Env: env, FS: fs, Prompt: script, Config: config.Default(),
		ConfigPath:   home + "/.config/relay/config.toml",
		HTTPClient:   happyProvider(t, nil),
		EmbedRuntime: happyRuntime(),
		ProbeEmbed:   okCheck(),
		Index:        idx,
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if out.Config.Model != "granite-embedding:278m" {
		t.Fatalf("model = %q", out.Config.Model)
	}
	log := script.Output()
	if !strings.Contains(log, "relay reindex") {
		t.Errorf("changing the model must point at the re-embed:\n%s", log)
	}
	// And the choice is saved even though the index now disagrees — the disagreement
	// is the user's to resolve, and refusing to save would make it unresolvable.
	if !strings.Contains(fs.Files[home+"/.config/relay/config.toml"], "granite-embedding:278m") {
		t.Error("the new choice must be written even when a re-embed is pending")
	}
}

// ------------------------------------------------------------- the runtime --

// The real Ollama runtime, driven through the same FakeExec and RoundTripper
// every other detector uses. This is as close to the download path as anything
// gets here: the commands are asserted, and nothing runs them.
func TestOllamaRuntimeUsesTheDocumentedCommands(t *testing.T) {
	ex := &detect.FakeExec{
		Paths: map[string]string{"curl": "/usr/bin/curl"},
		Responses: map[string]detect.Result{
			detect.Key("sh", "-c", "curl -fsSL https://ollama.com/install.sh | sh"): {
				Stdout: []byte("installed\n")},
			detect.Key("ollama", "pull", "nomic-embed-text"): {Stdout: []byte("success\n")},
		},
	}
	env := detect.Env{FS: &detect.MemFS{}, Exec: ex, Getenv: func(string) string { return "" },
		Home: home, GOOS: "linux"}
	rt := (Options{Env: env, HTTPClient: happyProvider(t, nil)}).runtime()

	cmd := rt.InstallCommand()
	if len(cmd) != 3 || cmd[0] != "sh" || !strings.Contains(cmd[2], "ollama.com/install.sh") {
		t.Fatalf("install command = %v", cmd)
	}
	var said []string
	say := func(f string, a ...any) { said = append(said, f) }
	if err := rt.Install(context.Background(), say); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := rt.Pull(context.Background(), "nomic-embed-text", say); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(ex.Calls) != 2 {
		t.Fatalf("commands run = %d, want 2", len(ex.Calls))
	}
	if ex.Calls[1].Timeout < OllamaPullTimeout {
		t.Errorf("a model pull needs a generous timeout; got %s", ex.Calls[1].Timeout)
	}

	// No curl, no command — and no guess.
	noCurl := env
	noCurl.Exec = &detect.FakeExec{}
	if got := (Options{Env: noCurl}).runtime().InstallCommand(); got != nil {
		t.Errorf("install command without curl = %v, want none", got)
	}
	// And macOS without Homebrew is the same: a sentence, not a .dmg automated
	// behind somebody's back.
	mac := env
	mac.GOOS = "darwin"
	mac.Exec = &detect.FakeExec{}
	if got := (Options{Env: mac}).runtime().InstallCommand(); got != nil {
		t.Errorf("install command on a bare mac = %v, want none", got)
	}
}

// §2b's fourth borrowed rule applies here too: the list is never a cage. A
// hosted embedding menu without a custom row would be a whitelist, and this
// audience runs local inference servers that speak OpenAI's shape.
func TestHostedListIsNotACage(t *testing.T) {
	answers := hostedAnswers()
	answers["embedding.hosted.model"] = "custom"
	answers["embedding.hosted.base_url"] = "http://127.0.0.1:8080/v1"
	answers["embedding.hosted.custom_model"] = "my-own-embedder"
	answers["embedding.hosted.cred.kind"] = "env"
	answers["embedding.hosted.cred.env"] = "OPENROUTER_API_KEY"
	delete(answers, "embedding.hosted.reuse")

	var probed llm.EmbedConfig
	opts, script, _ := newOpts(t, answers, func(o *Options) {
		o.ProbeEmbed = func(_ context.Context, cfg llm.EmbedConfig) llm.EmbedCheck {
			probed = cfg
			return okCheck()(context.Background(), cfg)
		}
	})

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if probed.BaseURL != "http://127.0.0.1:8080/v1" || probed.Model != "my-own-embedder" {
		t.Fatalf("probed = %+v", probed)
	}
	// config.Validate refuses a custom provider with no base URL, so the URL
	// has to survive into the file rather than be reconstructed later.
	if res.Config.Embedding.BaseURL != "http://127.0.0.1:8080/v1" {
		t.Errorf("config = %+v", res.Config.Embedding)
	}
	if err := res.Config.Validate(); err != nil {
		t.Errorf("the installer wrote a config that does not validate: %v", err)
	}
}
