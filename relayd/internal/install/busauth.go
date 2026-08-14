package install

import (
	"context"

	"github.com/luthor007/relay/relayd/internal/config"
	"github.com/luthor007/relay/relayd/internal/llm"
)

// Giving the Gateway a model.
//
// OpenClaw's onboarding takes one `--auth-choice`, and which values it will
// accept without a human in the loop is not a matter of opinion. Measured
// against openclaw 2026.7.1-2, non-interactive, on 2026-08-14:
//
//	anthropic-cli       works when ~/.claude/.credentials.json exists, and
//	                    writes agentRuntime: claude-cli bindings — no auth
//	                    profile, so sessions run through Anthropic's own client
//	                    rather than through a copied token
//	<vendor>-api-key    works, and takes the key from the environment, so it
//	                    never has to go on a command line
//	codex               "requires interactive mode. The Codex provider plugin
//	                    does not implement non-interactive setup."
//	openai-device-code  the same refusal
//	skip                configures a Gateway with no model
//
// So a ChatGPT subscription cannot be handed over from here at all, which is
// worth saying out loud rather than discovering as a failed onboard.

// busAuth is how this run will give the Gateway a model.
type busAuth struct {
	// Choice is the --auth-choice value.
	Choice string
	// Env carries the key to the child process. Never argv: that is
	// world-readable on Linux, and `ps` is not a place for somebody's key.
	Env []string
	// Label is what the user chose, in their words, for the line that reports it.
	Label string
}

// busKeyHandoff is a vendor Relay can configure the Gateway with, and the
// variable that carries the key.
//
// Every row here was run: the profile it writes is in the test. Anthropic is
// deliberately absent — `--auth-choice anthropic-api-key` exits 0 and writes no
// profile at all, with the key on the flag or in the environment, so offering it
// would be offering something that silently does nothing.
var busKeyHandoff = map[string]struct{ Choice, Env, Label string }{
	"openrouter": {"openrouter-api-key", "OPENROUTER_API_KEY", "OpenRouter"},
	"openai":     {"openai-api-key", "OPENAI_API_KEY", "OpenAI"},
	"xai":        {"xai-api-key", "XAI_API_KEY", "xAI"},
	"deepseek":   {"deepseek-api-key", "DEEPSEEK_API_KEY", "DeepSeek"},
	"groq":       {"groq-api-key", "GROQ_API_KEY", "Groq"},
	"google":     {"gemini-api-key", "GEMINI_API_KEY", "Google"},
	"zai":        {"zai-api-key", "ZAI_API_KEY", "Z.AI"},
	"moonshot":   {"moonshot-api-key", "MOONSHOT_API_KEY", "Moonshot"},
}

// chooseBusAuth decides what the Gateway is onboarded with, asking only when
// there is a choice worth putting to somebody.
func chooseBusAuth(ctx context.Context, opts Options, claudeInstalled bool, models ModelsOutcome) (busAuth, error) {
	p := opts.Prompt
	state := claudeCLIState(ctx, opts)

	// Free and best: a login OpenClaw's own non-interactive setup can read.
	// Nothing to ask.
	if state == claudeBindable {
		return busAuth{Choice: "anthropic-cli", Label: "the Claude Code login on this machine"}, nil
	}

	// A key Relay already holds, for a vendor OpenClaw can be configured with.
	hand, key, ok := busHandoffCandidate(ctx, opts, models)
	if !ok {
		// Nothing to offer is not a question. busOnboard says what to run.
		return busAuth{Choice: "skip"}, nil
	}

	body := "The Gateway needs a model of its own to run agent sessions. Relay's own model " +
		"credential is not automatically its — that is a separate decision, and this is it."
	switch {
	case state == claudeKeychainOnly:
		// The case that cost a real user two trips to another terminal: signed
		// in, and told they were not.
		body += "\n\nClaude Code is signed in here, and cannot be used for this: it keeps that " +
			"login in the macOS Keychain, and OpenClaw's unattended setup reads only " +
			"~/" + claudeCLICredentials + ". `openclaw onboard` asks interactively and does " +
			"read the Keychain."
	case claudeInstalled:
		body += "\n\nClaude Code is installed here and not signed in. Signing in with `claude` " +
			"and running `relay setup` again binds it, on a machine where it writes " +
			"~/" + claudeCLICredentials + "."
	}
	if models.Big.Auth.Ref == llm.RefCodex || models.Small.Auth.Ref == llm.RefCodex {
		// The obvious question for somebody who signed in to ChatGPT two
		// questions ago, and the answer is not "no".
		body += "\n\nA ChatGPT plan cannot be handed over from here either: OpenClaw's Codex " +
			"sign-in only runs interactively."
	}

	pick, err := p.Select(Question{
		ID: "bus.auth", Title: "A model for the Gateway", Body: body,
		Choices: []Choice{
			{
				ID: "key", Label: "Use the " + hand.Label + " key you already gave Relay",
				Hint: "copied into OpenClaw's own credential store, through the environment " +
					"rather than a command line",
				Recommended: true,
			},
			{ID: "skip", Label: "Not now", Last: true},
		},
		Default: "key",
	})
	if err != nil {
		// A scripted run that meets a question nobody decided an answer for
		// fails, here as everywhere else in this package.
		return busAuth{}, err
	}
	if pick == "key" {
		return busAuth{
			Choice: hand.Choice, Env: []string{hand.Env + "=" + key},
			Label: "the " + hand.Label + " key",
		}, nil
	}
	return busAuth{Choice: "skip"}, nil
}

// busHandoffCandidate finds a model credential that OpenClaw can be given.
//
// It resolves the reference, because a reference that does not resolve is not a
// credential this machine has, and offering it would configure the Gateway with
// an empty string.
func busHandoffCandidate(ctx context.Context, opts Options, models ModelsOutcome) (struct{ Choice, Env, Label string }, string, bool) {
	for _, m := range []config.Model{models.Big.Model, models.Small.Model} {
		hand, ok := busKeyHandoff[m.Vendor]
		if !ok || m.Credential == "" {
			continue
		}
		ref, err := llm.ParseRef(m.Credential)
		if err != nil || ref.Kind == llm.RefCodex {
			// A ChatGPT login is not a key, and there is nothing to hand over.
			continue
		}
		secret, err := ref.Resolve(ctx, opts.Lookup())
		if err != nil || secret == "" {
			continue
		}
		return hand, secret, true
	}
	return struct{ Choice, Env, Label string }{}, "", false
}
