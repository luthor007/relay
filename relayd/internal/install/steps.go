package install

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/config"
	"github.com/luthor007/relay/relayd/internal/detect"
	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/search"
	"github.com/luthor007/relay/relayd/internal/voice"
)

// The steps of Run, individually re-runnable.
//
// Setup is not a one-shot ritual: a key expires, a model id changes, somebody
// adds a runtime three weeks later. Every step has to be repeatable on its own
// without re-doing the others, or the answer to "my voice stopped working" is
// "run the whole installer again", which nobody does.

// RunVoice re-runs the voice step and saves the result.
func RunVoice(ctx context.Context, opts Options) (VoiceOutcome, error) {
	opts = opts.withDefaults()
	v, err := verifyVoice(ctx, opts)
	if err != nil {
		return v, err
	}
	opts.Config.Voice = v.Config
	if err := writeConfig(opts.FS, opts.ConfigPath, opts.Config); err != nil {
		return v, err
	}
	opts.Prompt.Say("\nSaved to %s", opts.ConfigPath)
	return v, nil
}

// RunModels re-runs the two-model step and saves the result.
func RunModels(ctx context.Context, opts Options) (ModelsOutcome, error) {
	opts = opts.withDefaults()
	m, err := chooseModels(ctx, opts)
	if err != nil {
		return m, err
	}
	opts.Config.Models = m.Config
	if err := writeConfig(opts.FS, opts.ConfigPath, opts.Config); err != nil {
		return m, err
	}
	opts.Prompt.Say("\nSaved to %s", opts.ConfigPath)
	return m, nil
}

// RunEmbedding re-runs the embedding choice and saves the result.
//
// It is the step most likely to be re-run of any of them: it is the one whose
// third row ("none, for now") is a real answer people take at install and come
// back to, and it is the one a model change forces. So it warns loudly when the
// index already holds vectors from another model, and points at `relay reindex`
// rather than silently leaving search degraded.
func RunEmbedding(ctx context.Context, opts Options) (EmbeddingOutcome, error) {
	opts = opts.withDefaults()
	e, err := chooseEmbedding(ctx, opts)
	if err != nil {
		return e, err
	}
	opts.Config.Embedding = e.Config
	if err := writeConfig(opts.FS, opts.ConfigPath, opts.Config); err != nil {
		return e, err
	}
	opts.Prompt.Say("\nSaved to %s", opts.ConfigPath)

	// The index is the authority on what was already written, and disagreeing
	// with it is not an error — it is a re-embed waiting to be asked for.
	if opts.Index != nil && e.Config.Model != "" {
		st, err := opts.Index.Inspect(ctx)
		was := st.Indexed
		if err == nil && was != "" && was != e.Config.Model {
			opts.Prompt.Say("\n%s", wrapIndent(fmt.Sprintf(
				"The index on this machine was embedded with %s. Vectors from two models are not "+
					"comparable, so search will run keyword-only until they agree. Run `relay "+
					"reindex` to rebuild them with %s — that re-embeds existing summaries, which "+
					"is minutes, and does not re-summarise anything.", was, e.Config.Model), 0, 76))
		}
	}
	return e, nil
}

// RunEntitlements re-runs the entitlement question and saves the result.
//
// This is the step most likely to be re-run after the install of any of them,
// because the fact it records is the one that changes without anything on the
// machine changing: a plan is bought, a plan lapses, a company moves everyone
// onto Copilot. There is no console editor for it and no schema column, so this
// command and the config file are the only two ways to change it — which is
// said out loud in the handoff rather than half-built as an API surface the
// vault and connector rows would collide with.
//
// It re-detects, because which questions are worth asking depends on which
// runtimes are here, and a runtime installed since setup should bring its
// question with it.
func RunEntitlements(ctx context.Context, opts Options) (EntitlementsOutcome, error) {
	opts = opts.withDefaults()
	rep := detect.Detect(ctx, opts.Env, opts.Detect)
	// No ModelsOutcome to quote: the auth *method* behind a configured model is
	// not written to the config file, only the vendor and the model id. Passing
	// a zero value prints no context line rather than a guessed one.
	e, err := chooseEntitlements(ctx, opts, rep, nil, ModelsOutcome{})
	if err != nil {
		return e, err
	}
	if e.Skipped != "" {
		opts.Prompt.Say("\n%s. Nothing was changed.", e.Skipped)
		return e, nil
	}
	opts.Config.Routing.Entitlements = e.Entitlements
	if err := writeConfig(opts.FS, opts.ConfigPath, opts.Config); err != nil {
		return e, err
	}
	opts.Prompt.Say("\nSaved to %s", opts.ConfigPath)
	return e, nil
}

// RunService re-runs boot registration on its own, for a machine where the
// install finished but the unit needs replacing — a moved binary, a changed
// config path.
func RunService(ctx context.Context, opts Options) (ServiceOutcome, error) {
	opts = opts.withDefaults()
	out, err := registerService(ctx, opts)
	reportService(opts.Prompt, out)
	return out, err
}

// RunMCP re-runs the MCP reconciliation.
func RunMCP(ctx context.Context, opts Options) (MCPOutcome, error) {
	opts = opts.withDefaults()
	rep := detect.Detect(ctx, opts.Env, opts.Detect)
	return reconcileMCP(ctx, opts, rep)
}

// Doctor re-probes every credential the config names, with one real call each.
//
// It is the same check the installer does before it exits, available afterwards
// — because the failure this prevents (a key that stopped working, discovered
// as silence at the glasses) does not only happen at install time.
type Doctor struct {
	// Bus is the Gateway, when one is configured.
	Bus   BusHealth
	Voice []voice.Check
	Small llm.ProbeResult
	Big   llm.ProbeResult
	// SmallProbed and BigProbed are false when nothing was configured to probe.
	SmallProbed bool
	BigProbed   bool

	// Embed is the embedder's one real call, and it is here because "search got
	// worse" is going to be a support question. The fastest answer to it is a
	// page that already says the embedder is down, rather than a round trip
	// asking somebody to check whether Ollama is running.
	Embed  llm.EmbedCheck
	Probed bool
	// EmbedConfig is what the config file asks for, and Runtime is what the
	// local runtime is actually doing. Those disagree in exactly the case that
	// matters: a model configured, a service not running.
	EmbedConfig config.Embedding
	Runtime     LocalStatus
	// Index is what the index itself holds, when there is one. A mismatch
	// between Index.Indexed and EmbedConfig.Model is why search silently
	// halved, and it is not something either probe above can see.
	Index    search.EmbeddingState
	HasIndex bool
}

// EmbedOK reports whether search has both halves.
//
// No embedder configured counts as OK: MEMORY.md §3's lexical-only mode is a
// supported state, and a doctor that reports a deliberate choice as a fault
// teaches people to ignore it.
func (d Doctor) EmbedOK() bool {
	if !d.EmbedConfig.Configured() {
		return true
	}
	if d.HasIndex && d.Index.Mismatch() {
		return false
	}
	return d.Probed && d.Embed.OK()
}

// OK reports whether everything answered.
func (d Doctor) OK() bool {
	speaks := false
	for _, c := range d.Voice {
		if c.OK() {
			speaks = true
		}
	}
	return speaks && d.SmallProbed && d.Small.OK() && d.BigProbed && d.Big.OK() &&
		d.EmbedOK() && d.Bus.OK()
}

// BusHealth is what `relay doctor` can say about the Gateway without a token:
// whether something is listening and calling itself live.
//
// It is deliberately the unauthenticated /health endpoint rather than a socket
// handshake. Doctor's job is to tell you which subsystem is the problem, and
// "the port answers but the token is wrong" is a different sentence from "there
// is nothing there" — collapsing them into one failed handshake would lose the
// half that says where to look.
type BusHealth struct {
	Configured bool
	URL        string
	Live       bool
	Detail     string
}

// Line is the doctor row.
func (b BusHealth) Line() string {
	switch {
	case !b.Configured:
		return "not configured — relayd drives Claude Code and Codex directly"
	case b.Live:
		return b.URL + " — live"
	case b.Detail != "":
		return b.URL + " — " + b.Detail
	}
	return b.URL + " — not answering"
}

// OK reports whether the bus is fine. An unconfigured bus is fine.
func (b BusHealth) OK() bool { return !b.Configured || b.Live }

// checkBus asks the Gateway's health endpoint whether it is up.
func checkBus(ctx context.Context, opts Options) BusHealth {
	var h BusHealth
	raw := strings.TrimSpace(opts.Config.Bus.URL)
	if raw == "" {
		return h
	}
	h.Configured, h.URL = true, raw

	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		h.Detail = "not a url"
		return h
	}
	// The socket is ws://; health is http:// on the same host.
	scheme := "http"
	if u.Scheme == "wss" || u.Scheme == "https" {
		scheme = "https"
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+u.Host+"/health", nil)
	if err != nil {
		h.Detail = err.Error()
		return h
	}
	// Options.HTTPClient is nil on the paths that never make a provider call —
	// `relay doctor` builds its own Options and leaves it unset — and a nil
	// *http.Client panics rather than erroring. Every other caller in this
	// package happens to be handed one, which is why this was not already true.
	client := opts.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		h.Detail = "not answering"
		return h
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		h.Detail = fmt.Sprintf("answered %d", resp.StatusCode)
		return h
	}
	h.Live = true
	return h
}

// RunDoctor probes everything in a config.
func RunDoctor(ctx context.Context, opts Options) Doctor {
	opts = opts.withDefaults()
	var d Doctor

	d.Bus = checkBus(ctx, opts)

	v := opts.Config.Voice
	primary := voice.Config{
		Option: v.Provider, Model: v.Model,
		HTTPClient: opts.HTTPClient, Lookup: opts.Lookup(),
	}
	if v.Credential != "" {
		if ref, err := llm.ParseRef(v.Credential); err == nil {
			primary.Credential = ref
		}
	}
	fallback := voice.Config{Option: v.Fallback, HTTPClient: opts.HTTPClient, Lookup: opts.Lookup()}
	if fallback.Option == "" {
		fallback.Option = voice.Fallback().ID
	}
	d.Voice = voice.ProbePlan(ctx, primary, fallback)

	d.Small, d.SmallProbed = probeModel(ctx, opts, opts.Config.Models.Small)
	d.Big, d.BigProbed = probeModel(ctx, opts, opts.Config.Models.Big)

	d.EmbedConfig = opts.Config.Embedding
	if opts.Index != nil {
		if st, err := opts.Index.Inspect(ctx); err == nil {
			d.Index, d.HasIndex = st, true
			if d.Index.Current == "" {
				d.Index.Current = d.EmbedConfig.Model
			}
		}
	}
	if d.EmbedConfig.Configured() {
		if d.EmbedConfig.Local() {
			d.Runtime = opts.runtime().Status(ctx)
		}
		d.Embed, d.Probed = probeEmbedding(ctx, opts, embedConfigFrom(opts, d.EmbedConfig))
	}
	return d
}

// embedConfigFrom turns the saved [embedding] table back into a client config.
//
// llm.EmbedProviderFor owns the one rule that matters here — "ollama" is the
// local runtime, anything else is a vendor id speaking OpenAI-compatible
// /v1/embeddings — so this does not get its own opinion about it.
func embedConfigFrom(opts Options, e config.Embedding) llm.EmbedConfig {
	provider, vendor := llm.EmbedProviderFor(e.Provider)
	return llm.EmbedConfig{
		Provider:   provider,
		Vendor:     vendor,
		Model:      e.Model,
		Dims:       llm.EmbeddingDims,
		BaseURL:    e.BaseURL,
		Credential: parseRefOrZero(e.Credential),
		HTTPClient: opts.HTTPClient,
		Lookup:     opts.Lookup(),
	}
}

// parseRefOrZero is the credential, or nothing. A reference that will not parse
// is not an error here: the probe reports it as missing_credential, which is
// what it is, rather than aborting `relay doctor` before it has printed the
// other four rows.
func parseRefOrZero(s string) llm.CredentialRef {
	ref, err := llm.ParseRef(s)
	if err != nil {
		return llm.CredentialRef{}
	}
	return ref
}

// Print writes the doctor's findings.
func (d Doctor) Print(p Prompter) {
	p.Section("Credentials", "One real call each. A credential that resolves is not the same as a "+
		"credential that works, and only one of those keeps the glasses talking.")
	for _, c := range d.Voice {
		p.Say("  %s", c.String())
	}
	p.Say("  bus    %s", d.Bus.Line())
	for _, m := range []struct {
		role   string
		res    llm.ProbeResult
		probed bool
	}{{"small", d.Small, d.SmallProbed}, {"big", d.Big, d.BigProbed}} {
		if !m.probed {
			p.Say("  %-6s not configured", m.role)
			continue
		}
		p.Say("  %-6s %s %s: %s", m.role, m.res.Vendor, m.res.Model, describeProbe(m.res))
		if !m.res.OK() {
			if advice := reasonAdvice(m.res.Reason); advice != "" {
				p.Say("         %s", advice)
			}
		}
	}
	d.printEmbed(p)
}

// printEmbed is the embedder's row. It reports three things separately —
// what is configured, whether the local service is up, and what the index
// holds — because they fail independently and a single "embedder: broken"
// would send people to the wrong one.
func (d Doctor) printEmbed(p Prompter) {
	if !d.EmbedConfig.Configured() {
		p.Say("  embed  not configured — search is keyword-only, which is a supported state. " +
			"`relay embed` adds the other half.")
		return
	}
	p.Say("  embed  %s", d.Embed.String())

	if d.EmbedConfig.Local() && !d.Runtime.Running {
		p.Say("         %s is not answering on %s. That is the whole fault: the model is fine "+
			"and the service is down.", config.EmbedProviderOllama, orNone(d.Runtime.Host))
		if d.Runtime.Detail != "" {
			p.Say("         %s", d.Runtime.Detail)
		}
	}
	if d.HasIndex && d.Index.Mismatch() {
		p.Say("         %s", wrapIndent(search.MismatchReason(d.Index.Indexed, d.Index.Current)+
			". Run `relay reindex`", 9, 76))
		return
	}
	if d.Probed && !d.Embed.OK() {
		if a := d.Embed.Advice(); a != "" {
			p.Say("         %s", wrapIndent(a, 9, 76))
		}
	}
	if d.HasIndex && d.Index.Unembedded() > 0 {
		p.Say("         %d of %d summaries have no vector yet — relayd is still working through "+
			"the backfill.", d.Index.Unembedded(), d.Index.Summaries)
	}
}

// DescribeConfig is the one-screen summary `relay status` prints.
func DescribeConfig(cfg config.Config, path string) string {
	return fmt.Sprintf(
		"config   %s\nlisten   %s\nvoice    %s (fallback %s)\nsmall    %s on %s\nbig      %s on %s\nembed    %s\nplans    %s",
		path, cfg.Listen,
		orNone(cfg.Voice.Provider), orNone(cfg.Voice.Fallback),
		orNone(cfg.Models.Small.Model), orNone(cfg.Models.Small.Vendor),
		orNone(cfg.Models.Big.Model), orNone(cfg.Models.Big.Vendor),
		describeEmbedding(cfg.Embedding),
		describeEntitlements(cfg.Routing))
}

// describeEntitlements is the one-line rendering of the [routing] table.
//
// It says what routing does without them rather than printing an empty list,
// because "none" on its own reads as a fault and this is a supported state:
// with nothing recorded, MEMORY.md §8 step 3 is skipped and the choice falls
// through to capability and load.
func describeEntitlements(r config.Routing) string {
	if len(r.Entitlements) == 0 {
		return "none recorded — routing falls back to capability and load"
	}
	return strings.Join(r.Entitlements, ", ")
}

// describeEmbedding is the one-line rendering of the [embedding] table.
func describeEmbedding(e config.Embedding) string {
	if !e.Configured() {
		return "none — search is keyword-only"
	}
	s := e.Model + " on " + e.Provider
	if e.Dims > 0 {
		s += fmt.Sprintf(" (%d dims)", e.Dims)
	}
	if e.Local() {
		s += ", on this machine"
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "not configured"
	}
	return s
}
