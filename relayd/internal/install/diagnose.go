package install

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/summarize"
)

// Reading the failure out loud, with a model.
//
// A reason code and an HTTP body are accurate and not always useful. "expired —
// http 401: No auth credentials found" tells someone who has seen it before
// exactly what happened and tells everybody else nothing, and the repair loop is
// most valuable to the second group. So the facts already on screen are handed
// to a small model and its read is printed under them, above the fix/continue
// rows.
//
// Four rules, and the first is the one this repo has already been bitten by:
//
//  1. **Nothing goes to the provider that has not been through the detector.**
//     internal/summarize's Redactor is required rather than optional for
//     exactly this reason — "a key posted to a model provider has already left
//     the machine" — and a diagnosis is a second consumer of the same shape of
//     text that narration was. The credential itself is never in the facts at
//     all: a reference is a name, and the name is what gets sent.
//  2. **It is off unless a credential for it exists.** No key, no call, no
//     behaviour change, nothing sent anywhere. That is the opt-in: it is not a
//     question in the flow, it is the absence of a key.
//  3. **It cannot fail the install.** Any error, timeout or empty answer is
//     silence — the repair question is printed exactly as it would have been.
//     A diagnostic that breaks setup is worse than no diagnostic.
//  4. **It says where the read came from.** A sentence written by a model, in
//     an installer that otherwise only says what it measured, has to be labelled
//     as what it is.

// DiagnoseModel is the model asked to read a failure.
const DiagnoseModel = "deepseek/deepseek-v4-pro-0813"

// DiagnoseEnv is the environment variable holding the key for it, falling back
// to the OpenRouter key the user may already have set for the models
// themselves.
const (
	DiagnoseEnv = "RELAY_DIAGNOSE_KEY"
	diagnoseAlt = "OPENROUTER_API_KEY"
)

// diagnoseTimeout is short on purpose. The user is sitting in front of an
// installer waiting to answer a question, and a diagnosis that arrives after
// they have answered it is worse than none.
const diagnoseTimeout = 12 * time.Second

// DiagnoseFacts is what the model is told. Every field is already on the user's
// screen — this adds no new disclosure, it just leaves the machine.
type DiagnoseFacts struct {
	// What failed, in the installer's own words: "the small model", "Simba 3.2".
	What string
	// Vendor and Model name the provider and what was asked of it.
	Vendor string
	Model  string
	// Endpoint is the host only. A path can carry a token; a host cannot.
	Endpoint string
	// Reason is the stable reason code.
	Reason string
	// Detail is the provider's own message.
	Detail string
	// Ref is the credential REFERENCE — "env:OPENROUTER_API_KEY". A name, never
	// a value. Nothing in this package resolves it to write it here.
	Ref string
}

const diagnoseSystem = `You are reading one failed credential check from the installer of a ` +
	`voice assistant, for a user who is sitting in front of it and has to decide whether to fix ` +
	`it now or move on.

Write at most three short sentences, plain prose, no lists, no headings, no code blocks.

Say the most likely cause, and then the single most useful next action. Be concrete about the ` +
	`difference between "the reference points at nothing", "the key resolved and the provider ` +
	`rejected it", and "the provider is having a problem" — the user's next move is different in ` +
	`each case and that distinction is the whole reason you are being asked.

You are given a credential REFERENCE, which is the name of an environment variable, a file path ` +
	`or a vault id. You are never given the credential. Do not ask for it, do not guess it, and ` +
	`do not suggest pasting one anywhere. If you are not sure, say what you would check first ` +
	`rather than inventing a cause.`

// diagnose asks the model to read a failure, and returns "" for every reason it
// might not be able to.
func diagnose(ctx context.Context, opts Options, f DiagnoseFacts) string {
	if opts.Diagnose != nil {
		return opts.Diagnose(ctx, f)
	}

	ref := diagnoseCredential(opts)
	if ref.IsZero() {
		return ""
	}

	// The detector is the same measured ruleset the summariser indexes through.
	// It runs over the provider's message because that is the field this package
	// did not write and cannot vouch for: an endpoint that echoes a request back
	// has put a bearer token in an error body before.
	redactor := opts.Redact
	if redactor == nil {
		redactor = summarize.Detector()
	}
	detail, _ := redactor.Redact(f.Detail)

	p, err := llm.New(llm.Config{
		Vendor: llm.RecommendedVendor, API: llm.APIOpenAI, Model: DiagnoseModel,
		Credential: ref, HTTPClient: opts.HTTPClient, Timeout: diagnoseTimeout,
	})
	if err != nil {
		return ""
	}

	ctx, cancel := context.WithTimeout(ctx, diagnoseTimeout)
	defer cancel()

	res, err := p.Complete(ctx, llm.Request{
		System:    diagnoseSystem,
		Messages:  []llm.Message{{Role: llm.RoleUser, Text: diagnosePrompt(f, detail)}},
		MaxTokens: 220,
		Effort:    "low",
	})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(res.Text)
}

func diagnosePrompt(f DiagnoseFacts, detail string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "What failed: %s\n", f.What)
	if f.Vendor != "" {
		fmt.Fprintf(&b, "Provider: %s\n", f.Vendor)
	}
	if f.Model != "" {
		fmt.Fprintf(&b, "Model asked for: %s\n", f.Model)
	}
	if f.Endpoint != "" {
		fmt.Fprintf(&b, "Endpoint host: %s\n", f.Endpoint)
	}
	fmt.Fprintf(&b, "Reason code: %s\n", f.Reason)
	if f.Ref != "" {
		fmt.Fprintf(&b, "Credential reference (a name, not a secret): %s\n", f.Ref)
	}
	if detail != "" {
		fmt.Fprintf(&b, "What the provider said: %s\n", detail)
	}
	return b.String()
}

// diagnoseCredential finds a key for the diagnosis model without asking for
// one. Its own variable first, so this can be pointed somewhere else than the
// models are, then the OpenRouter key the user has probably already set.
func diagnoseCredential(opts Options) llm.CredentialRef {
	getenv := os.Getenv
	if opts.Env.Getenv != nil {
		getenv = opts.Env.Getenv
	}
	for _, name := range []string{DiagnoseEnv, diagnoseAlt} {
		if strings.TrimSpace(getenv(name)) != "" {
			return llm.CredentialRef{Kind: llm.RefEnv, Value: name}
		}
	}
	return llm.CredentialRef{}
}

// hostOf reduces a base URL to its host. A path can carry a token in it; a host
// cannot, and the host is the whole of what the model needs.
func hostOf(base string) string {
	if base == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

// sayDiagnosis prints the model's read, labelled as a model's read.
func sayDiagnosis(p Prompter, text string) {
	if text == "" {
		return
	}
	p.Say("\n  %s asked to read that:", DiagnoseModel)
	p.Say("  %s", wrapIndent(text, 2, 76))
}
