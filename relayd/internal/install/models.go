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

// modelsPreamble is what is left of ORCHESTRATOR.md §2b's wording after asking
// what a person needs in order to answer the next question.
//
// The two paragraphs that went: why the split is a latency decision and not a
// cost one, and why the big model is not the place to economise. Both are true,
// both are the reasoning behind a default the user is not being asked to
// relitigate, and both are still in §2b where they belong. What survives is the
// one sentence that changes an answer — the small one speaks, the big one
// works, one key does both.
const modelsPreamble = `Two models: a fast one that speaks, and a strong one that does the work.

OpenRouter covers both with one key, and either can be swapped later.`

// loginCommands maps an auth row onto the command that performs it, where we
// know one. Every other subscription and OAuth row is walked in words instead
// of guessed at — the installer says what to run only when it is sure.
//
// It is empty, and that is the point of the change on 2026-08-12: the one entry
// it held was `codex login`, and sending the user to another terminal to run it
// was the wrong half of the answer. Relay performs that login itself now (see
// codex.go), and a row that Relay can perform never reaches this map. The map
// stays because the rows it is for — Copilot's device login, Qwen's OAuth —
// are still walked in words, and the day we are sure of one of those commands
// this is where it goes.
var loginCommands = map[string]string{}

// ModelChoice is one of the two models.
type ModelChoice struct {
	Role   string // "small" or "big"
	Vendor llm.VendorEntry
	Auth   llm.Auth
	Model  config.Model
	Probe  llm.ProbeResult
	// Probed is false when nothing could be called at all.
	Probed bool
	// Account is who a subscription sign-in signed in as, when there was one.
	Account  string
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

	// The Claude note is not printed here any more. It answered a question —
	// "why can I not use my Max plan?" — that only exists once somebody has
	// gone looking for the Anthropic row, and it charged every user four
	// paragraphs for it. It is now that row's own note, printed when the row is
	// chosen. See llm.Vendors.
	p.Section("Choose the models", modelsPreamble)

	var small, big ModelChoice
	// Going back from the big model's first question means going back to the
	// small one, which is a whole model earlier and the only place "back" can
	// mean from there.
	for {
		var err error
		small, err = verifyModel(ctx, opts, "small",
			"The voice — fast and cheap. It speaks, narrates progress from structured events, and "+
				"never invents a specific it was not told.", llm.SmallModelDefault, nil)
		if err != nil {
			return out, err
		}

		big, err = verifyModel(ctx, opts, "big",
			"The work — the strongest one available. It routes, holds the MCP registry and a shell, "+
				"and writes to memory.", llm.BigModelDefault, &small)
		if errors.Is(err, ErrBack) {
			continue
		}
		if err != nil {
			return out, err
		}
		break
	}
	out.Small = small
	out.Warnings = append(out.Warnings, small.Warnings...)
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

// verifyModel picks a model and, when the one real call did not answer, offers
// to pick again rather than carrying a dead credential to the summary. See
// repair.go.
func verifyModel(ctx context.Context, opts Options, role, why, defaultModel string, prior *ModelChoice) (ModelChoice, error) {
	// A rerun starts from what the last run left, tested rather than trusted.
	// For a ChatGPT sign-in this is the difference between one keystroke and
	// another device code.
	if kept, ok, err := keptModel(ctx, opts, role); err != nil {
		return kept, err
	} else if ok {
		return kept, nil
	}
	return verify(ctx, opts, repair[ModelChoice]{
		ID:      "models." + role + ".repair",
		Title:   strings.ToUpper(role[:1]) + role[1:] + " model — not working yet",
		Choose:  func() (ModelChoice, error) { return chooseModel(ctx, opts, role, why, defaultModel, prior) },
		OK:      func(c ModelChoice) bool { return c.OK() },
		Trouble: func(c ModelChoice) string { return modelTrouble(role, c) },
		Facts: func(c ModelChoice) DiagnoseFacts {
			base := c.Model.BaseURL
			if base == "" {
				base = c.Vendor.BaseURL
			}
			return DiagnoseFacts{
				What:     "the " + role + " orchestrator model",
				Vendor:   c.Vendor.Label,
				Model:    c.Model.Model,
				Endpoint: hostOf(base),
				Reason:   string(c.Probe.Reason),
				Detail:   c.Probe.Detail,
				Ref:      c.Model.Credential,
			}
		},
		FixLabel:      "Choose again — vendor, sign-in, model id, credential",
		ContinueLabel: "Leave it for now, and finish the install",
		GiveUp: "Leaving the " + role + " model as it is. `relay models` re-runs this step on its " +
			"own, and `relay doctor` re-tests every credential with one real call, so this is " +
			"not the last chance to fix it.",
	})
}

// The questions one model role asks, in order. A wrong turn three questions ago
// used to cost the whole step — there was no way back from "I picked ChatGPT
// and meant OpenRouter" except finishing the model and running `relay models`
// again. Each stage below can hand control back to the one before it, skipping
// the ones that do not apply to the vendor in hand.
const (
	stageVendor = iota
	stageAuth
	stageCustom
	stageLogin
	stageModelID
	stageCredential
	stageDone
)

// chooseModel runs the menu for one role. prior is the model already
// configured, so the second role can reuse one key when the vendor matches —
// which is the whole reason OpenRouter is the recommendation.
func chooseModel(ctx context.Context, opts Options, role, why, defaultModel string, prior *ModelChoice) (ModelChoice, error) {
	p := opts.Prompt
	choice := ModelChoice{Role: role}

	var (
		vendor   llm.VendorEntry
		auth     llm.Auth
		cfgModel config.Model
	)

	// applies reports whether a stage asks anything for this vendor and auth
	// method. Going back has to skip the questions that were never asked, or
	// "back" from the model id on a one-auth vendor would land on a menu that
	// never appeared.
	applies := func(stage int) bool {
		switch stage {
		case stageAuth:
			return len(vendor.Auths) > 1
		case stageCustom:
			return vendor.Custom
		case stageLogin:
			return auth.Kind != llm.AuthAPIKey && auth.Ref == ""
		}
		return true
	}
	back := func(from int) int {
		for s := from - 1; s > stageVendor; s-- {
			if applies(s) {
				return s
			}
		}
		return stageVendor
	}

	for stage := stageVendor; stage != stageDone; {
		switch stage {
		case stageVendor:
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
				// The first question of the first model has nothing behind it
				// in this step. The second model does: the one before it.
				Back: prior != nil,
			})
			if err != nil {
				return choice, err
			}
			v, ok := llm.Vendor(vendorID)
			if !ok {
				return choice, fmt.Errorf("install: unknown vendor %q", vendorID)
			}
			vendor, choice.Vendor = v, v
			if vendor.Note != "" {
				p.Say("\n  %s", wrapIndent(vendor.Note, 2, 76))
			}
			if len(vendor.Auths) == 0 {
				return choice, fmt.Errorf("install: vendor %q has no auth methods", vendorID)
			}
			auth = vendor.Auths[0]
			stage++

		case stageAuth:
			if !applies(stage) {
				stage++
				continue
			}
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
				Back:    true,
			})
			if errors.Is(err, ErrBack) {
				stage = back(stage)
				continue
			}
			if err != nil {
				return choice, err
			}
			for _, a := range vendor.Auths {
				if a.ID == authID {
					auth = a
				}
			}
			stage++

		case stageCustom:
			// An auth method may reach a different endpoint than its vendor's.
			// The ChatGPT rows do: a subscription is only spendable at
			// chatgpt.com, which speaks a different wire than api.openai.com,
			// and probing the credential against the vendor's own base URL
			// would report a working login as a bad one.
			cfgModel = config.Model{Vendor: vendor.ID, API: string(vendor.API)}
			if auth.BaseURL != "" {
				cfgModel.BaseURL = auth.BaseURL
			}
			if auth.API != "" {
				cfgModel.API = string(auth.API)
			}
			choice.Auth = auth
			if !applies(stage) {
				stage++
				continue
			}
			base, err := p.Input(Input{
				ID: "models." + role + ".base_url", Prompt: "Base URL",
				Body: "Anything OpenAI-compatible or Anthropic-compatible.",
				Back: true,
			})
			if errors.Is(err, ErrBack) {
				stage = back(stage)
				continue
			}
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
				Back:    true,
			})
			if errors.Is(err, ErrBack) {
				continue // back to the base URL, one question up in this stage
			}
			if err != nil {
				return choice, err
			}
			cfgModel.API = shape
			stage++

		case stageLogin:
			if !applies(stage) {
				stage++
				continue
			}
			// A subscription, OAuth or device-code row that Relay can perform,
			// it performs — see codex.go. What it will not do is walk the user
			// through somebody else's login and then ask which environment
			// variable holds the key, because for a subscription there is no
			// key to hold.
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
			_, err := p.Confirm(Confirm{
				ID: "models." + role + ".login", Prompt: "Signed in?", Body: body,
				Default: true, Back: true,
			})
			if errors.Is(err, ErrBack) {
				stage = back(stage)
				continue
			}
			if err != nil {
				return choice, err
			}
			stage++

		case stageModelID:
			// An auth method that serves its own catalog names its own default:
			// the subscription endpoint does not answer to platform model ids,
			// so inheriting the vendor's would be a 404 dressed up as a bad
			// login.
			modelDefault := defaultModelFor(vendor, defaultModel)
			if auth.Model != "" {
				modelDefault = auth.Model
			}
			// The budget alternative is named on the question that decides the
			// bill, and only for the role where the bill is decided. It is one
			// line, and it is not a menu: the default is a real recommendation,
			// and this is the one fact a user needs to overrule it on purpose
			// rather than by accident.
			var modelBody string
			if role == "big" && modelDefault == llm.BigModelDefault {
				modelBody = "Cheaper: " + llm.BudgetModelDefault + " — about 6× less, and not far behind."
			}
			modelID, err := p.Input(Input{
				ID: "models." + role + ".model", Prompt: "Model id",
				Body: modelBody, Default: modelDefault, Back: true,
			})
			if errors.Is(err, ErrBack) {
				stage = back(stage)
				continue
			}
			if err != nil {
				return choice, err
			}
			cfgModel.Model = modelID
			stage++

		case stageCredential:
			// Re-entered after a back, so it starts from nothing rather than
			// carrying an answer the user has since walked away from.
			cfgModel.Credential = ""
			choice.Warnings = nil

			// One key covers both models when the vendor and the sign-in match,
			// which is exactly the OpenRouter argument, so offer it rather than
			// asking twice. Matching on the auth method and not just the vendor:
			// a ChatGPT login and an OpenAI API key are the same vendor and not
			// the same credential, and reusing one as the other is a 401.
			if prior != nil && prior.Auth.ID == auth.ID && prior.Model.Credential != "" &&
				auth.Ref != llm.RefCodex {
				yes, err := p.Confirm(Confirm{
					ID:      "models." + role + ".reuse",
					Prompt:  fmt.Sprintf("Use the same %s credential as the %s model?", vendor.Label, prior.Role),
					Default: true, Back: true,
				})
				if errors.Is(err, ErrBack) {
					stage = back(stage)
					continue
				}
				if err != nil {
					return choice, err
				}
				if yes {
					cfgModel.Credential = prior.Model.Credential
					stage++
					continue
				}
			}

			// A subscription row does not end in a credential question. It ends
			// in a sign-in, or in reading one that already exists — on this
			// machine, or from earlier in this run.
			if auth.Ref == llm.RefCodex {
				out, err := chooseCodexCredential(ctx, opts, "models."+role+".chatgpt", auth, true)
				switch {
				case errors.Is(err, ErrBack):
					stage = back(stage)
					continue
				case errors.Is(err, errCredentialSkipped):
					choice.Warnings = append(choice.Warnings,
						fmt.Sprintf("the %s model has no ChatGPT login, so the orchestrator cannot use it yet", role))
				case err != nil:
					return choice, err
				default:
					cfgModel.Credential = out.Ref.String()
					opts.rememberCodex(out)
					choice.Account = out.Account
					if out.Source == "run" {
						p.Say("  Using the ChatGPT login from earlier in this run: %s.", out.Account)
					} else {
						p.Say("  Signed in as %s.", out.Account)
					}
				}
				stage++
				continue
			}

			ref, err := askCredential(ctx, opts, CredentialAsk{
				ID:      "models." + role + ".cred",
				Service: "models",
				Label:   vendor.Label,
				EnvHint: envVarFor(vendor),
				Back:    true,
			})
			switch {
			case errors.Is(err, ErrBack):
				stage = back(stage)
				continue
			case errors.Is(err, errCredentialSkipped):
				choice.Warnings = append(choice.Warnings,
					fmt.Sprintf("the %s model has no credential, so the orchestrator cannot use it yet", role))
			case err != nil:
				return choice, err
			default:
				cfgModel.Credential = ref.String()
			}
			stage++
		}
	}
	choice.Auth = auth
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
			Codex: codexOptions(opts),
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
