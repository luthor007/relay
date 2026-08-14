package install

import (
	"context"
	"os"
	"path/filepath"

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
	// Free and best: the machine's own Claude Code login, used through Claude
	// Code itself. Nothing to ask.
	if claudeCLISignedIn(opts) {
		return busAuth{Choice: "anthropic-cli", Label: "the Claude Code login on this machine"}, nil
	}

	p := opts.Prompt
	var choices []Choice
	if claudeInstalled {
		choices = append(choices, Choice{
			ID: "claude", Label: "Sign in to Claude Code now, in another terminal",
			Hint:        "your Claude plan, used through Claude Code itself — Relay waits here",
			Recommended: true,
		})
	}

	// A key Relay already holds, for a vendor OpenClaw can be configured with.
	hand, key, ok := busHandoffCandidate(ctx, opts, models)
	if ok {
		choices = append(choices, Choice{
			ID: "key", Label: "Use the " + hand.Label + " key you already gave Relay",
			Hint: "copied into OpenClaw's own credential store, through the environment " +
				"rather than a command line",
			Recommended: !claudeInstalled,
		})
	}
	if len(choices) == 0 {
		// Nothing to offer is not a question. It is reported by the step that
		// follows, which says what to run and when it will be picked up.
		return busAuth{Choice: "skip"}, nil
	}
	choices = append(choices, Choice{ID: "skip", Label: "Not now", Last: true})

	def := choices[0].ID
	body := "The Gateway needs a model of its own to run agent sessions. Relay's own model " +
		"credential is not automatically its — that is a separate decision, and this is it."
	if models.Big.Auth.Ref == llm.RefCodex || models.Small.Auth.Ref == llm.RefCodex {
		// Named because it is the obvious question for somebody who just signed
		// in to ChatGPT two questions ago, and the answer is not "no".
		body += "\n\nA ChatGPT plan cannot be handed over from here: OpenClaw's Codex sign-in " +
			"only runs interactively. `openclaw onboard` does it, and `relay setup` adopts the " +
			"result."
	}
	pick, err := p.Select(Question{
		ID: "bus.auth", Title: "A model for the Gateway", Body: body,
		Choices: choices, Default: def,
	})
	if err != nil {
		// A scripted run that meets a question nobody decided an answer for
		// fails, here as everywhere else in this package.
		return busAuth{}, err
	}

	handoff := busAuth{
		Choice: hand.Choice, Env: []string{hand.Env + "=" + key},
		Label: "the " + hand.Label + " key",
	}
	switch pick {
	case "claude":
		signedIn, err := waitForClaudeLogin(opts)
		if err != nil {
			return busAuth{}, err
		}
		if signedIn {
			return busAuth{Choice: "anthropic-cli", Label: "the Claude Code login on this machine"}, nil
		}
		// Not signed in after all. Fall back to the key when there is one,
		// rather than making them run the whole step again for it.
		if ok {
			p.Say("  %s", wrapIndent("Using "+handoff.Label+" instead. `relay setup` binds "+
				"Claude Code the moment that login exists.", 2, 76))
			return handoff, nil
		}
	case "key":
		return handoff, nil
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

// claudeBinary is where Relay's own install put Claude Code, when it did.
func claudeBinary(opts Options) string {
	home := opts.Env.Home
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return ""
		}
	}
	path := filepath.Join(home, ".local", "bin", "claude")
	if _, err := os.Lstat(path); err != nil {
		return ""
	}
	return path
}

// waitForClaudeLogin sends the user to another terminal and waits for them.
//
// It is a question and not a poll: `claude` is an interactive login, Relay
// cannot drive it through a pipe, and a progress spinner over somebody else's
// browser flow would be a lie about what is being watched. What Relay can do is
// say the command, wait, and then check the one file that decides it.
func waitForClaudeLogin(opts Options) (bool, error) {
	p := opts.Prompt
	// The command has to work in the shell it is typed into.
	//
	// The first version of this said "run `claude`", and a real run met exactly
	// what it deserved: a new terminal, `zsh: command not found: claude`, and a
	// user stuck between two windows. Relay installs into ~/.local/bin, which a
	// fresh shell has no reason to know about, so the instruction names the
	// binary by the path Relay put it at.
	cmd := "claude"
	if abs := claudeBinary(opts); abs != "" {
		cmd = abs
	}
	for attempt := 0; attempt < 2; attempt++ {
		yes, err := p.Confirm(Confirm{
			ID:     "bus.claude.login",
			Prompt: "Signed in?",
			Body: "Open another terminal window and run:\n\n    " + cmd + "\n\n" +
				"Sign in there, then come back here and press return. Relay reads nothing but " +
				"the fact that a login exists — the Gateway then runs Claude work through " +
				"Claude Code itself.",
			Default: true,
		})
		if err != nil {
			return false, err
		}
		if !yes {
			return false, nil
		}
		if claudeCLISignedIn(opts) {
			return true, nil
		}
		p.Say("  %s", wrapIndent("No Claude Code login here yet. It writes ~/"+
			claudeCLICredentials+" when it finishes, and that file is not there. If that "+
			"terminal said `command not found`, it is the PATH question from a moment ago: "+
			"the command above is the full path, and it works either way.", 2, 76))
	}
	return false, nil
}
