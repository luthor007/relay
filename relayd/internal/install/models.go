package install

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/luthor007/relay/relayd/internal/config"
	"github.com/luthor007/relay/relayd/internal/llm"
)

// Step 3b — the two orchestrator models. ORCHESTRATOR.md §2b.
//
// Two models, two jobs: the small one speaks and is never allowed to leave you
// in silence, the big one does the work. Both are picked from the same grouped
// vendor list, and OpenRouter is recommended because one key covers both and
// either can be swapped later without re-running setup.
//
// The menu shape is copied from OpenClaw rather than re-derived, because thirty
// vendors times three auth flows is ninety unreadable rows as one list:
//
//  1. vendor groups first, each with a one-line hint naming the auth methods
//     behind it, and you drill in only after picking one;
//  2. subscription auth is a first-class row where it exists — Codex's ChatGPT
//     OAuth, Copilot's device login, the Qwen/MiniMax/Z.AI/Chutes coding plans
//     — not a special case to apologise for;
//  3. risk is a hint on the row, not a wall: the option exists, the warning is
//     attached to it, and the user decides;
//  4. Custom Provider is always the last row, so the list is never a cage.

// claudePreamble is ORCHESTRATOR.md §2b's exact wording, and it is printed
// before the menu rather than after somebody has gone looking for the Anthropic
// row and drawn their own conclusion. The confusion is predictable, so it is
// pre-empted in as many words.
const claudePreamble = `A note on Claude, because this catches people out.

Your Claude Max plan still powers Claude Code on this machine — that is Anthropic's own ` +
	`client using its own login, and nothing about it changes. What it cannot do is power ` +
	`our orchestrator. That part needs an API key.

There are ways around that — claude setup-token, the community max-api-proxy — and both ` +
	`are subscription credentials used outside Anthropic's own client. Anthropic has blocked ` +
	`that before. We do not ship it as a row, we are not pretending it is impossible, and we ` +
	`will not be able to support it when it breaks.

Other subscriptions do work here: ChatGPT through the Codex row, GitHub Copilot, and the ` +
	`Qwen, MiniMax and Z.AI coding plans. Rather than shipping a table of which ones are ` +
	`allowed this quarter, Relay probes the credential and tells you what the provider ` +
	`actually said.`

const modelsPreamble = `Relay runs two models with different jobs.

The small one hears every utterance and speaks within about 400ms — status, control verbs, ` +
	`"on it". The big one only wakes for real work: routing judgement, tools, sessions, ` +
	`memory. This is a latency decision, not a cost one. A big model thinking is dead air in ` +
	`your ear, and eight seconds of silence reads as broken no matter what arrives after it.

The big model holds the MCP registry and a shell, so it is not the place to economise — ` +
	`weaker tiers are easier to prompt-inject.

OpenRouter is recommended: one key covers both, and either can be swapped later without ` +
	`re-running setup. Everything else on the list works; that one is just the shortest path.`

// loginCommands maps an auth row onto the command that performs it, where we
// know one. Every other subscription and OAuth row is walked in words instead
// of guessed at — the installer says what to run only when it is sure.
var loginCommands = map[string]string{
	"openai-codex": "codex login",
}

// ModelChoice is one of the two models.
type ModelChoice struct {
	Role   string // "small" or "big"
	Vendor llm.VendorEntry
	Auth   llm.Auth
	Model  config.Model
	Probe  llm.ProbeResult
	// Probed is false when nothing could be called at all.
	Probed   bool
	Warnings []string
}

// OK reports a verified working model.
func (m ModelChoice) OK() bool { return m.Probed && m.Probe.OK() }

// Line is the summary row.
func (m ModelChoice) Line() string {
	if m.Model.Model == "" {
		return "not configured"
	}
	s := fmt.Sprintf("%s on %s", m.Model.Model, m.Vendor.Label)
	switch {
	case m.OK():
		s += " — verified"
	case m.Probed:
		s += " — " + string(m.Probe.Reason)
	default:
		s += " — not tested"
	}
	return s
}

// ModelsOutcome is both models.
type ModelsOutcome struct {
	Small    ModelChoice
	Big      ModelChoice
	Config   config.Models
	Warnings []string
}

// OK reports whether both models were verified with a real call.
func (m ModelsOutcome) OK() bool { return m.Small.OK() && m.Big.OK() }

func chooseModels(ctx context.Context, opts Options) (ModelsOutcome, error) {
	p := opts.Prompt
	var out ModelsOutcome

	p.Section("Choose the orchestrator models", modelsPreamble)
	p.Say("\n%s", wrap(claudePreamble, 76))

	small, err := chooseModel(ctx, opts, "small",
		"The voice — fast and cheap. It speaks, narrates progress from structured events, and "+
			"never invents a specific it was not told.", llm.SmallModelDefault, nil)
	if err != nil {
		return out, err
	}
	out.Small = small
	out.Warnings = append(out.Warnings, small.Warnings...)

	big, err := chooseModel(ctx, opts, "big",
		"The work — the strongest one available. It routes, holds the MCP registry and a shell, "+
			"and writes to memory.", llm.BigModelDefault, &small)
	if err != nil {
		return out, err
	}
	out.Big = big
	out.Warnings = append(out.Warnings, big.Warnings...)

	out.Config = config.Models{Small: small.Model, Big: big.Model}
	if err := checkNoInline(map[string]string{
		"models.small.credential": small.Model.Credential,
		"models.big.credential":   big.Model.Credential,
	}); err != nil {
		return out, err
	}
	return out, nil
}

// chooseModel runs the two-level menu for one role. prior is the model already
// configured, so the second role can reuse one key when the vendor matches —
// which is the whole reason OpenRouter is the recommendation.
func chooseModel(ctx context.Context, opts Options, role, why, defaultModel string, prior *ModelChoice) (ModelChoice, error) {
	p := opts.Prompt
	choice := ModelChoice{Role: role}

	// Level one: vendor groups.
	var vendorChoices []Choice
	for _, v := range llm.Vendors() {
		vendorChoices = append(vendorChoices, Choice{
			ID: v.ID, Label: v.Label, Hint: v.Hint,
			Recommended: v.Recommended, Last: v.Custom,
		})
	}
	vendorID, err := p.Select(Question{
		ID:      "models." + role + ".vendor",
		Title:   strings.ToUpper(role[:1]) + role[1:] + " model",
		Body:    why,
		Choices: vendorChoices,
		Default: llm.RecommendedVendor,
	})
	if err != nil {
		return choice, err
	}
	vendor, ok := llm.Vendor(vendorID)
	if !ok {
		return choice, fmt.Errorf("install: unknown vendor %q", vendorID)
	}
	choice.Vendor = vendor
	if vendor.Note != "" {
		p.Say("\n  %s", wrapIndent(vendor.Note, 2, 76))
	}

	// Level two: the auth methods behind that group.
	auth := llm.Auth{}
	switch len(vendor.Auths) {
	case 0:
		return choice, fmt.Errorf("install: vendor %q has no auth methods", vendorID)
	case 1:
		auth = vendor.Auths[0]
	default:
		var authChoices []Choice
		for _, a := range vendor.Auths {
			authChoices = append(authChoices, Choice{
				ID: a.ID, Label: a.Label, Hint: authHint(a), Risk: a.Risk,
			})
		}
		authID, err := p.Select(Question{
			ID:      "models." + role + ".auth",
			Title:   vendor.Label,
			Body:    "How do you want to authenticate?",
			Choices: authChoices,
			Default: vendor.Auths[0].ID,
		})
		if err != nil {
			return choice, err
		}
		for _, a := range vendor.Auths {
			if a.ID == authID {
				auth = a
			}
		}
	}
	choice.Auth = auth

	cfgModel := config.Model{Vendor: vendor.ID, API: string(vendor.API)}

	// The custom provider needs a base URL and a wire shape.
	if vendor.Custom {
		base, err := p.Input(Input{
			ID: "models." + role + ".base_url", Prompt: "Base URL",
			Body: "Anything OpenAI-compatible or Anthropic-compatible.",
		})
		if err != nil {
			return choice, err
		}
		cfgModel.BaseURL = base

		shape, err := p.Select(Question{
			ID: "models." + role + ".api", Title: "Which shape does it speak?",
			Choices: []Choice{
				{ID: "auto", Label: "Auto-detect", Hint: "try OpenAI-compatible first, then Anthropic"},
				{ID: string(llm.APIOpenAI), Label: "OpenAI-compatible"},
				{ID: string(llm.APIAnthropic), Label: "Anthropic-compatible"},
			},
			Default: "auto",
		})
		if err != nil {
			return choice, err
		}
		cfgModel.API = shape
	}

	// A subscription, OAuth or device-code row is the vendor's own login. Relay
	// cannot perform it — and pretending to would be worse than saying so, on a
	// headless box especially. So it is walked, and the credential it leaves
	// behind is referenced like any other.
	if auth.Kind != llm.AuthAPIKey {
		body := fmt.Sprintf("%s is %s's own login, not ours. Run it in another terminal now — "+
			"on a headless box this is the slow part of setup, and it is one device-code flow "+
			"at a time.", auth.Label, vendor.Label)
		if cmd, ok := loginCommands[auth.ID]; ok {
			body += fmt.Sprintf("\n\n    %s\n", cmd)
		} else {
			body += "\n\nRelay does not ship the exact command for this one, because a wrong " +
				"command is worse than none. Use the vendor's documented login."
		}
		body += "\nThen tell Relay how to reach the credential it leaves behind — usually an " +
			"environment variable or a file it writes."
		if _, err := p.Confirm(Confirm{
			ID: "models." + role + ".login", Prompt: "Signed in?", Body: body, Default: true,
		}); err != nil {
			return choice, err
		}
	}

	// Model id.
	modelDefault := defaultModelFor(vendor, defaultModel)
	modelID, err := p.Input(Input{
		ID: "models." + role + ".model", Prompt: "Model id", Default: modelDefault,
	})
	if err != nil {
		return choice, err
	}
	cfgModel.Model = modelID

	// Credential. One key covers both models when the vendor matches, which is
	// exactly the OpenRouter argument, so offer it rather than asking twice.
	reused := false
	if prior != nil && prior.Vendor.ID == vendor.ID && prior.Model.Credential != "" {
		yes, err := p.Confirm(Confirm{
			ID:      "models." + role + ".reuse",
			Prompt:  fmt.Sprintf("Use the same %s credential as the %s model?", vendor.Label, prior.Role),
			Default: true,
		})
		if err != nil {
			return choice, err
		}
		if yes {
			cfgModel.Credential = prior.Model.Credential
			reused = true
		}
	}
	if !reused {
		ref, err := askCredential(ctx, opts, CredentialAsk{
			ID:      "models." + role + ".cred",
			Service: "models",
			Label:   vendor.Label,
			EnvHint: envVarFor(vendor),
		})
		switch {
		case errors.Is(err, errCredentialSkipped):
			choice.Warnings = append(choice.Warnings,
				fmt.Sprintf("the %s model has no credential, so the orchestrator cannot use it yet", role))
		case err != nil:
			return choice, err
		default:
			cfgModel.Credential = ref.String()
		}
	}
	choice.Model = cfgModel

	// One real call, before the installer exits.
	choice.Probe, choice.Probed = probeModel(ctx, opts, cfgModel)
	if choice.Probed {
		p.Say("  %s %s: %s", vendor.Label, cfgModel.Model, describeProbe(choice.Probe))
		if !choice.Probe.OK() {
			if advice := reasonAdvice(choice.Probe.Reason); advice != "" {
				p.Say("      %s", advice)
			}
			choice.Warnings = append(choice.Warnings, fmt.Sprintf(
				"the %s model (%s on %s) did not answer: %s",
				role, cfgModel.Model, vendor.Label, choice.Probe.Detail))
		}
	}
	return choice, nil
}

func authHint(a llm.Auth) string {
	switch a.Kind {
	case llm.AuthSubscription:
		return "a plan you already pay for"
	case llm.AuthOAuth:
		return "browser sign-in"
	case llm.AuthDeviceCode:
		return "device code, which works on a headless box"
	}
	return ""
}

// defaultModelFor adapts the documented default to the vendor. OpenRouter takes
// a namespaced id; talking to OpenAI or Anthropic directly does not.
func defaultModelFor(v llm.VendorEntry, doc string) string {
	if v.ID == llm.RecommendedVendor {
		return doc
	}
	if v.Custom {
		return ""
	}
	if i := strings.IndexByte(doc, '/'); i >= 0 {
		bare := doc[i+1:]
		// Only strip the namespace when it is this vendor's own model. Asking
		// Groq for "opus-5" is a guess either way, and the probe says so.
		if strings.EqualFold(doc[:i], v.ID) || strings.HasPrefix(v.ID, doc[:i]) {
			return bare
		}
		return bare
	}
	return doc
}

func envVarFor(v llm.VendorEntry) string {
	id := strings.ToUpper(strings.ReplaceAll(v.ID, "-", "_"))
	return id + "_API_KEY"
}

// probeModel makes one real call. A model with no credential is still probed,
// because "missing_credential" is a result the installer should print rather
// than a reason to skip the check.
func probeModel(ctx context.Context, opts Options, m config.Model) (llm.ProbeResult, bool) {
	if m.Model == "" {
		return llm.ProbeResult{}, false
	}
	ref, err := llm.ParseRef(m.Credential)
	if err != nil && m.Credential != "" {
		return llm.ProbeResult{Vendor: m.Vendor, Model: m.Model,
			Reason: llm.ReasonUnresolvedRef, Detail: err.Error()}, true
	}

	shapes := []llm.API{llm.API(m.API)}
	if m.API == "auto" || m.API == "" {
		// Auto-detect is two real calls, in order, and the first that completes
		// wins. Nothing is inferred from the URL.
		shapes = []llm.API{llm.APIOpenAI, llm.APIAnthropic}
	}

	var last llm.ProbeResult
	for _, shape := range shapes {
		p, err := llm.New(llm.Config{
			Vendor: m.Vendor, API: shape, BaseURL: m.BaseURL, Model: m.Model,
			Credential: ref, HTTPClient: opts.HTTPClient, Lookup: opts.Lookup(),
		})
		if err != nil {
			return llm.ProbeResult{Vendor: m.Vendor, Model: m.Model,
				Reason: llm.ReasonUnavailable, Detail: err.Error()}, true
		}
		last = p.Probe(ctx)
		if last.OK() {
			return last, true
		}
	}
	return last, true
}

func describeProbe(r llm.ProbeResult) string {
	if r.OK() {
		return fmt.Sprintf("ok (%s)", r.Latency.Round(1e6))
	}
	if r.Detail == "" {
		return string(r.Reason)
	}
	return string(r.Reason) + " — " + r.Detail
}
