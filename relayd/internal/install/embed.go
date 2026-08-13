package install

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/luthor007/relay/relayd/internal/config"
	"github.com/luthor007/relay/relayd/internal/llm"
)

// Step 3c — choosing an embedding model. ORCHESTRATOR.md §2c.
//
// This is a step rather than a setting for one reason, and it is a hard one:
// **the vector width is fixed when the index is created and cannot be changed
// afterwards.** summary_vec is a vec0 table declared float[768]; store fixes
// EmbeddingDims at 768 and store.ErrEmbeddingDims refuses anything else. The
// index also writes an embedding_model meta key on its first write and
// internal/search checks it on every search, because vectors from two models
// share a space only by coincidence. So the choice has to be made **before the
// backfill runs**, and changing it afterwards means re-embedding everything —
// which is `relay reindex`, a first-class flow, not an error message.
//
// # Why this step recommends local when §2a and §2b recommend hosted
//
// It is not an inconsistency, and a reader who notices it deserves the reason
// rather than a shrug. §2a sends a sentence of speech to a provider and §2b
// sends an utterance and a routing decision. This step sends **summaries of
// everything the user has ever worked on** — 3.6 GB of history distilled into
// the ~22,000 chunks that are, by construction, the most meaning-dense
// artefacts on the machine. The asset is different in kind, so the
// recommendation is different.
//
// **Cost is not the argument.** Embedding the whole corpus is roughly 4–5
// million tokens, which is under a dollar at hosted rates, once. Writing "local
// saves money" would be a hint a user can disprove in an afternoon, and a hint
// that can be disproved costs you the ones that are true. What local actually
// buys is that the summaries never leave the machine — the same argument
// MEMORY.md §6 and CLOUD.md §1 already make about credentials, applied to the
// more sensitive asset — plus an embedder that works with the network down.
// The price is an install and some CPU, and even that is small: embedding is
// minutes next to the hour or two MEMORY.md §4 budgets for summarisation.
//
// On Relay Cloud the box is ours, the model is preinstalled, and this step is
// informational.
//
// # What this file owns, and what it does not
//
// internal/llm owns the catalog, the two wire formats and the probe;
// internal/config owns the shape that gets written down. This file owns the
// *conversation* and the *provisioning*. That split is what makes the local
// recommendation honest: llm can call a model, but only the installer can put
// one on the machine, and "recommended — now go and read a README" is not a
// recommendation.

// EmbeddingKind is which of §2c's three rows was taken.
type EmbeddingKind string

// The three rows of §2c.
const (
	// EmbedLocal is a model this installer provisions and runs on the machine.
	EmbedLocal EmbeddingKind = "local"
	// EmbedHosted is a provider from the §2b vendor list.
	EmbedHosted EmbeddingKind = "hosted"
	// EmbedNone is the supported third state: search stays lexical-only and
	// says so. internal/search already degrades with a Degraded reason, and a
	// box whose embedder is down should get worse search, not no search.
	EmbedNone EmbeddingKind = "none"
)

// EmbeddingOutcome is what §2c decided.
type EmbeddingOutcome struct {
	Kind EmbeddingKind
	// Provider is what goes in config.toml: "ollama" for the local runtime, and
	// for a hosted one the §2b vendor id itself — because every hosted embedder
	// is OpenAI-compatible, so the vendor already determines the wire shape.
	// Vendor repeats it for a hosted choice and is empty for a local one, which
	// is what lets a caller ask "was this hosted?" without string-matching.
	Provider string
	Vendor   string
	Model    string

	// Runtime is the local runtime's state after the step ran.
	Runtime LocalStatus
	// Installed and Pulled record what this step actually put on the machine.
	Installed bool
	Pulled    bool

	// Check is the one real call. ORCHESTRATOR.md §2: nothing is trusted until
	// a provider has answered once. Probed is false when there was nothing to
	// call at all.
	Check  llm.EmbedCheck
	Probed bool

	Config   config.Embedding
	Warnings []string
}

// OK reports whether the chosen embedder answered with a usable vector.
//
// "None" is OK. It is a supported configuration — search runs lexical-only and
// says so on every query — and reporting a deliberate choice as a failed
// install would be wrong about the thing that matters.
func (e EmbeddingOutcome) OK() bool {
	if e.Kind == EmbedNone {
		return true
	}
	return e.Probed && e.Check.OK()
}

// Line is the summary row.
func (e EmbeddingOutcome) Line() string {
	switch {
	case e.Kind == EmbedNone:
		return "none — search is keyword-only, and says so on every query"
	case e.Probed && e.Check.OK():
		return fmt.Sprintf("%s on %s — verified, %d dimensions", e.Model, e.Provider, e.Check.Dims)
	case e.Probed:
		return fmt.Sprintf("%s on %s — %s", e.Model, e.Provider, e.Check.Reason)
	default:
		return fmt.Sprintf("%s on %s — not tested", e.Model, e.Provider)
	}
}

// --------------------------------------------------------------- the step --

// embeddingPreamble was four paragraphs: how search works, why the vector width
// is fixed at create time, why local is recommended here when it is not
// elsewhere, and why that is not a cost argument. Every word of it is the
// reasoning behind the default — and the user is being asked to accept a
// default, not to reconstruct it. The reasoning stays in ORCHESTRATOR.md §2c
// and in this file's own doc comment, where the person who needs it is looking.
const embeddingPreamble = `Search by meaning as well as by keyword. This one is asked now ` +
	`because the index cannot be re-sized later.`

func chooseEmbedding(ctx context.Context, opts Options) (EmbeddingOutcome, error) {
	p := opts.Prompt
	out := EmbeddingOutcome{}

	rt := opts.runtime()
	status := rt.Status(ctx)
	out.Runtime = status

	rec := llm.RecommendedLocalEmbed()
	local := Choice{
		ID: string(EmbedLocal), Label: "Local — " + rec.Label + " on " + rt.Name(),
		Recommended: true,
		Hint: fmt.Sprintf("nothing leaves this machine — the opposite of the voice and model "+
			"steps, because this sends summaries of everything you have worked on. %s download",
			rec.Size),
	}
	switch {
	case status.Running:
		local.Hint += " — " + rt.Name() + " is already running"
	case status.Installed:
		local.Hint += " — " + rt.Name() + " is installed but not answering"
	}

	id, err := p.Select(Question{
		ID:    "embedding",
		Title: "Search",
		Body:  embeddingPreamble,
		Choices: []Choice{
			local,
			{
				ID: string(EmbedHosted), Label: "Hosted — a provider from the same list as the models",
				Hint: "cheap; summaries are sent to a provider",
			},
			{
				ID: string(EmbedNone), Label: "None, for now", Last: true,
				Hint: "keyword-only search; `relay embed` adds it later",
			},
		},
		Default: string(EmbedLocal),
	})
	if err != nil {
		return out, err
	}

	switch EmbeddingKind(id) {
	case EmbedLocal:
		return chooseLocalEmbedding(ctx, opts, rt, status)
	case EmbedHosted:
		return chooseHostedEmbedding(ctx, opts)
	default:
		out.Kind = EmbedNone
		out.Config = config.Embedding{Provider: config.EmbedProviderNone}
		p.Say("  Keyword-only search. `relay embed` adds the rest later.")
		return out, nil
	}
}

// ------------------------------------------------------------------ local --

func chooseLocalEmbedding(ctx context.Context, opts Options, rt EmbedRuntime, status LocalStatus) (EmbeddingOutcome, error) {
	p := opts.Prompt
	out := EmbeddingOutcome{
		Kind: EmbedLocal, Provider: config.EmbedProviderOllama, Runtime: status,
	}

	model, err := askLocalModel(opts)
	if err != nil {
		return out, err
	}
	out.Model = model.ID

	// 1. The runtime itself.
	if !status.Installed && !status.Running {
		ok, err := provisionRuntime(ctx, opts, rt)
		if err != nil {
			return out, err
		}
		if !ok {
			return fallBackToNone(opts, out, fmt.Sprintf(
				"%s was not installed, so there is no local embedder yet — install it and run "+
					"`relay embed`", rt.Name()))
		}
		out.Installed = true
		status = rt.Status(ctx)
		out.Runtime = status
	}

	// 2. The service. Installed is not running, and the difference between them
	// is the whole reason this is checked rather than assumed.
	if !status.Running {
		status, err = waitForService(ctx, opts, rt, status)
		if err != nil {
			return out, err
		}
		out.Runtime = status
		if !status.Running {
			return fallBackToNone(opts, out, fmt.Sprintf(
				"%s is installed on this machine but nothing is answering on %s — start it and "+
					"run `relay embed`, nothing else needs redoing", rt.Name(), status.Host))
		}
	}

	// 3. The model.
	if !hasModel(status.Models, model.ID) {
		p.Say("  Pulling %s (%s). This is the only download in this step.", model.ID, model.Size)
		if err := rt.Pull(ctx, model.ID, p.Say); err != nil {
			return fallBackToNone(opts, out, fmt.Sprintf(
				"%s could not be pulled: %v — pull it yourself and run `relay embed`",
				model.ID, err))
		}
		out.Pulled = true
		p.Say("  %s is on this machine.", model.ID)
	} else {
		p.Say("  %s is already here, so there is nothing to download.", model.ID)
	}

	// 4. One real call, before the installer exits.
	cfg := llm.EmbedConfigFor(model)
	cfg.BaseURL = status.Host
	cfg.HTTPClient = opts.HTTPClient
	cfg.Lookup = opts.Lookup()

	out.Check, out.Probed = probeEmbedding(ctx, opts, cfg)
	p.Say("  %s", out.Check.String())
	if !out.Check.OK() {
		return fallBackToNone(opts, out, embedFailure(out.Check))
	}

	out.Config = config.Embedding{
		Provider: config.EmbedProviderOllama,
		Model:    model.ID,
		Dims:     out.Check.Dims,
		BaseURL:  hostIfMoved(status.Host),
	}
	return out, nil
}

// hostIfMoved records the local endpoint only when it is not the default.
//
// A config file that repeats a default turns that default into a pin, and the
// next person who moves OLLAMA_HOST is then quietly ignored by a line nobody
// remembers writing.
func hostIfMoved(host string) string {
	if host == "" || host == llm.DefaultOllamaBaseURL {
		return ""
	}
	return host
}

// askLocalModel runs the model menu, refusing an incompatible width before
// anything is downloaded.
//
// llm.SelectLocalEmbedModel is the refusal; this is the conversation around it.
// The body names the models that do *not* fit, because they are the ones people
// have heard of — leaving them out silently makes the list look arbitrary, and
// naming them with their width makes the omission explain itself.
func askLocalModel(opts Options) (llm.EmbedModelEntry, error) {
	p := opts.Prompt

	var choices []Choice
	var excluded []string
	for _, m := range llm.LocalEmbedModels() {
		if !m.FitsIndex() {
			excluded = append(excluded, fmt.Sprintf("%s at %d", m.ID, m.Dims))
			continue
		}
		choices = append(choices, Choice{
			ID: m.ID, Label: m.Label, Hint: m.Hint + " " + m.Size, Recommended: m.Recommended,
		})
	}
	choices = append(choices, Choice{
		ID: "other", Label: "Another model", Last: true,
		Hint: fmt.Sprintf("any id your runtime can pull; must emit %d dimensions", llm.EmbeddingDims),
	})

	id, err := p.Select(Question{
		ID:    "embedding.local.model",
		Title: "Which local model?",
		Body: fmt.Sprintf("Only %d-dimension models fit the index, so %s are not offered.",
			llm.EmbeddingDims, strings.Join(excluded, ", ")),
		Choices: choices,
		Default: llm.RecommendedLocalEmbed().ID,
	})
	if err != nil {
		return llm.EmbedModelEntry{}, err
	}
	if id != "other" {
		// Only fitting rows are offered, so this cannot refuse — and a test
		// asserts that rather than trusting it.
		return llm.SelectLocalEmbedModel(id)
	}

	// A typed id. A known-wrong width is refused here, before a download.
	for attempt := 0; attempt < 3; attempt++ {
		typed, err := p.Input(Input{
			ID: "embedding.local.other", Prompt: "Model id",
			Default: llm.RecommendedLocalEmbed().ID,
		})
		if err != nil {
			return llm.EmbedModelEntry{}, err
		}
		m, err := llm.SelectLocalEmbedModel(typed)
		if err != nil {
			p.Say("  %s", wrapIndent(err.Error(), 2, 76))
			continue
		}
		if m.Dims == 0 {
			p.Say("  %s", wrapIndent(fmt.Sprintf(
				"Relay does not know how wide %s is. That is allowed — the list is a menu, not a "+
					"cage — but the check happens for real in a moment, and a model of the wrong "+
					"width is refused then, before anything is indexed.", typed), 2, 76))
		}
		return m, nil
	}
	// Three refusals in a row: take the recommendation rather than looping.
	m := llm.RecommendedLocalEmbed()
	p.Say("  Falling back to %s.", m.ID)
	return m, nil
}

// provisionRuntime installs the local runtime, having asked first and shown the
// exact command.
//
// Same rules as the agent runtimes in runtimes.go, and for the same reasons:
// nothing installs without being asked, the question defaults to no, and Relay
// never invents an install command. A recommendation is not consent.
func provisionRuntime(ctx context.Context, opts Options, rt EmbedRuntime) (bool, error) {
	p := opts.Prompt

	cmd := rt.InstallCommand()
	if len(cmd) == 0 {
		p.Say("  %s", wrapIndent(fmt.Sprintf(
			"%s is not installed here and Relay has no install command it can run on this machine. "+
				"Install it from %s and run `relay embed` — we do not ship a guessed command, "+
				"because a wrong one is worse than none.", rt.Name(), rt.Docs()), 2, 76))
		return false, nil
	}

	yes, err := p.Confirm(Confirm{
		ID:      "embedding.local.install",
		Prompt:  fmt.Sprintf("Install %s?", rt.Name()),
		Body:    fmt.Sprintf("Runs: %s", strings.Join(cmd, " ")),
		Default: false,
	})
	if err != nil || !yes {
		return false, err
	}
	if err := rt.Install(ctx, p.Say); err != nil {
		p.Say("  %s did not install: %v", rt.Name(), err)
		return false, nil
	}
	p.Say("  %s installed.", rt.Name())
	return true, nil
}

// waitForService handles the second named failure mode: the binary is there and
// the service is not.
//
// Relay does not start it. Starting somebody else's daemon is exactly the guess
// the house rules forbid — the unit is named differently on every distribution,
// macOS runs it from an app bundle, and a wrong `systemctl start` is worse than
// a paragraph. So the state is named, the usual fixes are printed, and the
// check is offered again.
func waitForService(ctx context.Context, opts Options, rt EmbedRuntime, status LocalStatus) (LocalStatus, error) {
	p := opts.Prompt
	for attempt := 0; attempt < 2; attempt++ {
		body := fmt.Sprintf(
			"%s is installed here, but nothing is answering on %s. Those are two different states, "+
				"and only the second one can embed anything.\n\n"+
				"  macOS:  open the Ollama app\n"+
				"  Linux:  systemctl --user start ollama, or sudo systemctl start ollama\n"+
				"  either: ollama serve, in another terminal\n\n"+
				"Relay will not start it for you: the unit is named differently on every "+
				"distribution and a wrong guess is worse than this paragraph.",
			rt.Name(), status.Host)
		if status.Detail != "" {
			body += "\n\nWhat the check saw: " + status.Detail
		}
		again, err := p.Confirm(Confirm{
			ID: "embedding.local.start", Prompt: "Check again?", Body: body, Default: true,
		})
		if err != nil {
			return status, err
		}
		if !again {
			return status, nil
		}
		status = rt.Status(ctx)
		if status.Running {
			p.Say("  %s is answering on %s.", rt.Name(), status.Host)
			return status, nil
		}
	}
	return status, nil
}

// hasModel matches an id against a pulled library, defaulting the tag the way
// the runtime does. "nomic-embed-text" and "nomic-embed-text:latest" are the
// same model, and treating them as different re-downloads 274 MB somebody has.
func hasModel(have []string, want string) bool {
	if !strings.Contains(want, ":") {
		want += ":latest"
	}
	for _, h := range have {
		if !strings.Contains(h, ":") {
			h += ":latest"
		}
		if strings.EqualFold(h, want) {
			return true
		}
	}
	return false
}

// fallBackToNone turns a failed local or hosted setup into the supported
// lexical-only state rather than into a failed install.
//
// Same rule the voice step follows: an installer that aborts half way is worse
// than one that finishes and says what it could not do. Search still works, it
// is visibly worse, and one command fixes it.
func fallBackToNone(opts Options, out EmbeddingOutcome, why string) (EmbeddingOutcome, error) {
	out.Warnings = append(out.Warnings, why+". Search will be keyword-only until then.")
	opts.Prompt.Say("  %s", wrapIndent(why+".", 2, 76))
	opts.Prompt.Say("  Search still works; it is keyword-only, and it says so on every result.")
	out.Kind = EmbedNone
	out.Config = config.Embedding{Provider: config.EmbedProviderNone}
	return out, nil
}

// ----------------------------------------------------------------- hosted --

const hostedEmbeddingBody = `The same grouped vendor list as the orchestrator models, and usually the ` +
	`same key.

Two things are worth knowing before you pick. Not every vendor on that list has an embeddings endpoint ` +
	`at all — Anthropic does not, and the models list and the embeddings list are not the same list. ` +
	`And nothing hosted is natively the width this index needs, so Relay asks the provider for a ` +
	`shorter vector at request time; these are the models that can answer that.`

func chooseHostedEmbedding(ctx context.Context, opts Options) (EmbeddingOutcome, error) {
	p := opts.Prompt
	out := EmbeddingOutcome{Kind: EmbedHosted}

	var choices []Choice
	for _, m := range llm.HostedEmbedModels() {
		label := m.Label
		if v, ok := llm.Vendor(m.Vendor); ok {
			label += " — " + v.Label
		}
		choices = append(choices, Choice{
			ID: m.Vendor + "/" + m.ID, Label: label, Hint: m.Hint, Recommended: m.Recommended,
		})
	}
	// §2b's fourth borrowed rule: Custom Provider is always the last row, so
	// the list is never a cage. It costs almost nothing to support and it is
	// the difference between a menu and a whitelist.
	choices = append(choices, Choice{
		ID: customEmbedID, Label: "Custom provider", Last: true,
		Hint: "any OpenAI /v1/embeddings endpoint",
	})
	rec := hostedDefault()

	id, err := p.Select(Question{
		ID:      "embedding.hosted.model",
		Title:   "Which hosted embedding model?",
		Body:    hostedEmbeddingBody,
		Choices: choices,
		Default: rec.Vendor + "/" + rec.ID,
	})
	if err != nil {
		return out, err
	}

	vendorID, modelID, _ := strings.Cut(id, "/")
	var customBase string
	if id == customEmbedID {
		vendorID = customVendorID
		if customBase, err = p.Input(Input{
			ID: "embedding.hosted.base_url", Prompt: "Base URL",
			Body: "e.g. http://127.0.0.1:8080/v1",
		}); err != nil {
			return out, err
		}
		if modelID, err = p.Input(Input{
			ID: "embedding.hosted.custom_model", Prompt: "Embedding model id",
		}); err != nil {
			return out, err
		}
	}

	model, err := llm.SelectHostedEmbedModel(vendorID, modelID)
	if err != nil {
		// Unreachable with the catalog as it stands, and asserted in a test.
		return fallBackToNone(opts, out, err.Error())
	}
	vendor, ok := llm.Vendor(vendorID)
	if !ok {
		return out, fmt.Errorf("install: unknown vendor %q", vendorID)
	}
	// config.Embedding has no separate vendor field on purpose: every hosted
	// embedder is OpenAI-compatible, so the vendor id already determines the
	// wire shape and a second field would only be a second way to disagree with
	// it. Provider IS the vendor id here.
	out.Provider, out.Vendor, out.Model = vendor.ID, vendor.ID, model.ID

	cfg := llm.EmbedConfigFor(model)
	cfg.BaseURL = customBase
	cfg.HTTPClient = opts.HTTPClient
	cfg.Lookup = opts.Lookup()

	// The models step may already have taken a key for this vendor. Asking for
	// the same key twice is what makes an installer feel twice as long as it is.
	credential := ""
	if prior := opts.Config.Models.Small.Credential; prior != "" && opts.Config.Models.Small.Vendor == vendor.ID {
		yes, err := p.Confirm(Confirm{
			ID:      "embedding.hosted.reuse",
			Prompt:  fmt.Sprintf("Use the same %s credential as the orchestrator models?", vendor.Label),
			Default: true,
		})
		if err != nil {
			return out, err
		}
		if yes {
			credential = prior
		}
	}
	if credential == "" {
		ref, err := askCredential(ctx, opts, CredentialAsk{
			ID:        "embedding.hosted.cred",
			Service:   "embedding",
			Label:     vendor.Label,
			EnvHint:   envVarFor(vendor),
			Optional:  true,
			SkipLabel: "Skip — no embedder for now, and search stays keyword-only",
		})
		switch {
		case errors.Is(err, errCredentialSkipped):
			return fallBackToNone(opts, out, "no credential for "+vendor.Label)
		case err != nil:
			return out, err
		default:
			credential = ref.String()
		}
	}
	if ref, err := llm.ParseRef(credential); err == nil {
		cfg.Credential = ref
	}

	out.Check, out.Probed = probeEmbedding(ctx, opts, cfg)
	p.Say("  %s", out.Check.String())
	if !out.Check.OK() {
		return fallBackToNone(opts, out, embedFailure(out.Check))
	}

	out.Config = config.Embedding{
		Provider:   vendor.ID,
		Model:      model.ID,
		Dims:       out.Check.Dims,
		Credential: credential,
	}
	if vendor.Custom {
		// config.Validate refuses a custom provider with no base URL, which is
		// the one embedding misconfiguration a leaf package can catch on its
		// own — so it is written down rather than reconstructed later.
		out.Config.BaseURL = cfg.BaseURL
	}
	return out, checkNoInline(map[string]string{"embedding.credential": out.Config.Credential})
}

// customEmbedID is the menu id of the escape-hatch row, and customVendorID is
// the catalog's own id for it. They are separate constants because the row is
// not "vendor/model" shaped like the others.
const (
	customEmbedID  = "custom"
	customVendorID = "custom"
)

func hostedDefault() llm.EmbedModelEntry {
	models := llm.HostedEmbedModels()
	for _, m := range models {
		if m.Recommended {
			return m
		}
	}
	return models[0]
}

// ------------------------------------------------------------- the probe --

// probeEmbedding is ORCHESTRATOR.md §2's rule applied to an embedder: one real
// call, before the installer exits.
//
// The width assertion is what earns it. store.PutSummary, store.SearchVector
// and search.New all refuse a vector that is not exactly the index's width, and
// all three run *after* the backfill. Catching it here costs one call; catching
// it there costs an hour or two and a support ticket that opens with "search
// doesn't work".
func probeEmbedding(ctx context.Context, opts Options, cfg llm.EmbedConfig) (llm.EmbedCheck, bool) {
	if opts.ProbeEmbed != nil {
		return opts.ProbeEmbed(ctx, cfg), true
	}
	return llm.ProbeEmbedConfig(ctx, cfg), true
}

// embedFailure turns a failed probe into one sentence with the next action in
// it. llm.EmbedCheck.Advice already knows what to do about each reason code;
// this adds the command that does it, which is the part the installer owns.
func embedFailure(c llm.EmbedCheck) string {
	s := c.Model + ": " + string(c.Reason)
	if c.Detail != "" {
		s = c.Model + ": " + c.Detail
	}
	if a := c.Advice(); a != "" {
		s += ". " + a
	}
	return strings.TrimRight(s, ". ") + ". Run `relay embed` to try again"
}
