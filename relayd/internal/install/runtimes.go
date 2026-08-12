package install

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/detect"
)

// Step 2 of ORCHESTRATOR.md §2: offer to install the rest.
//
// Two things this step must not do. It must not install anything without being
// asked — software appearing on a machine because a script defaulted to yes is
// the behaviour that gets an installer distrusted, and it is trivially avoided
// by defaulting the question to no. And it must not invent an install command:
// where Relay does not know how to install a runtime on this machine, it says
// so and gets out of the way, because a wrong `npm install -g` is worse than no
// suggestion at all.

// InstallMethod is one way to install a runtime.
type InstallMethod struct {
	// Requires is the tool that must be on PATH for this method to apply.
	Requires string
	Command  []string
	// Verified records whether this exact command has been run against a real
	// machine by us. Nothing here has, yet — the runtimes are not installed in
	// CI and never will be. The installer prints the command and asks first,
	// which is what makes an unverified command safe to offer.
	Verified bool
}

// RuntimeInstaller is how one runtime gets onto a machine.
type RuntimeInstaller struct {
	Runtime adapter.Runtime
	Label   string
	Methods []InstallMethod
	// Docs is where to go when we have nothing to offer.
	Docs string
	// Why is the one line explaining what this runtime buys the user, so the
	// question is answerable by somebody who has not used it.
	Why string
}

// Installers is the install table.
//
// Two of the five have no entry, on purpose. Relay drives OpenClaw and Hermes
// over ACP and detects them by binary name, but no install command for either
// has been probed on a real machine, and the honest move is to say that rather
// than to guess a package name that may not exist. Closing those two rows needs
// a machine with them installed — the same list ADAPTERS.md §8 keeps.
func Installers() []RuntimeInstaller {
	return []RuntimeInstaller{
		{
			Runtime: adapter.ClaudeCode, Label: "Claude Code",
			Why: "Anthropic's own client. If you have a Claude subscription, this is where it works.",
			Methods: []InstallMethod{
				{Requires: "npm", Command: []string{"npm", "install", "-g", "@anthropic-ai/claude-code"}},
			},
			Docs: "https://docs.anthropic.com/en/docs/claude-code",
		},
		{
			Runtime: adapter.Codex, Label: "Codex",
			Why: "OpenAI's agent. Its ChatGPT OAuth is also the one subscription path the orchestrator can use.",
			Methods: []InstallMethod{
				{Requires: "npm", Command: []string{"npm", "install", "-g", "@openai/codex"}},
			},
			Docs: "https://developers.openai.com/codex/cli",
		},
		{
			Runtime: adapter.OpenCode, Label: "OpenCode",
			Why: "Speaks ACP, so Relay drives it with the same adapter as the other two.",
			Methods: []InstallMethod{
				{Requires: "npm", Command: []string{"npm", "install", "-g", "opencode-ai"}},
			},
			Docs: "https://opencode.ai",
		},
		{
			Runtime: adapter.OpenClaw, Label: "OpenClaw",
			Why:  "Speaks ACP through its gateway.",
			Docs: "",
		},
		{
			Runtime: adapter.Hermes, Label: "Hermes",
			Why:  "Speaks ACP. On the machine this design was measured on it held 70% of the history.",
			Docs: "",
		},
	}
}

func installerFor(rt adapter.Runtime) (RuntimeInstaller, bool) {
	for _, i := range Installers() {
		if i.Runtime == rt {
			return i, true
		}
	}
	return RuntimeInstaller{}, false
}

// RuntimeOutcome is what step 2 did.
type RuntimeOutcome struct {
	Offered   []adapter.Runtime
	Installed []adapter.Runtime
	Declined  []adapter.Runtime
	// Unknown are the runtimes Relay cannot install here, named rather than
	// hidden.
	Unknown  []adapter.Runtime
	Warnings []string
}

func offerRuntimes(ctx context.Context, opts Options, rep detect.Report) (RuntimeOutcome, error) {
	var out RuntimeOutcome
	p := opts.Prompt

	missing := rep.Missing()
	if len(missing) == 0 {
		p.Section("Anything else?", "Every runtime Relay drives is already on this machine.")
		return out, nil
	}

	p.Section("Anything else?", fmt.Sprintf(
		"%d of the five agent runtimes are not installed here. Relay works with whatever you "+
			"have — this is an offer, not a requirement, and nothing is installed unless you say so.",
		len(missing)))

	for _, f := range missing {
		inst, ok := installerFor(f.Runtime)
		if !ok {
			continue
		}
		out.Offered = append(out.Offered, f.Runtime)

		method, methodOK := pickMethod(opts.Env, inst)
		if !methodOK {
			out.Unknown = append(out.Unknown, f.Runtime)
			msg := fmt.Sprintf("%s: Relay has no install command it can run here.", inst.Label)
			if inst.Docs != "" {
				msg += " Install it from " + inst.Docs + " and re-run `relay detect`."
			} else {
				msg += " Install it however you normally would and re-run `relay detect` — " +
					"Relay will pick it up. We do not ship a guessed package name for this one."
			}
			p.Say("  %s", wrapIndent(msg, 2, 76))
			continue
		}

		yes, err := p.Confirm(Confirm{
			ID:     "install." + string(f.Runtime),
			Prompt: fmt.Sprintf("Install %s?", inst.Label),
			Body: fmt.Sprintf("%s\n  Relay would run: %s",
				inst.Why, strings.Join(method.Command, " ")),
			// Defaulting to no is deliberate: an unattended run must not put
			// software on a machine nobody asked it to.
			Default: false,
		})
		if err != nil {
			return out, err
		}
		if !yes {
			out.Declined = append(out.Declined, f.Runtime)
			continue
		}

		res, err := opts.Env.Exec.Run(ctx, detect.Cmd{
			Name: method.Command[0],
			Args: method.Command[1:],
			// npm on a cold cache is slow; ten minutes is generous and bounded.
			Timeout: 10 * time.Minute,
		})
		switch {
		case err != nil:
			w := fmt.Sprintf("%s did not install: %v", inst.Label, err)
			out.Warnings = append(out.Warnings, w)
			p.Say("  %s", w)
		case res.Code != 0:
			detail := res.Err()
			if detail == "" {
				detail = res.Out()
			}
			w := fmt.Sprintf("%s did not install (exit %d): %s", inst.Label, res.Code, firstLine(detail))
			out.Warnings = append(out.Warnings, w)
			p.Say("  %s", w)
		default:
			out.Installed = append(out.Installed, f.Runtime)
			p.Say("  %s installed. You will need to sign in to it separately — that is its own "+
				"login, not ours.", inst.Label)
		}
	}
	return out, nil
}

// pickMethod returns the first method whose prerequisite is on this machine.
func pickMethod(env detect.Env, inst RuntimeInstaller) (InstallMethod, bool) {
	for _, m := range inst.Methods {
		if len(m.Command) == 0 {
			continue
		}
		if m.Requires == "" {
			return m, true
		}
		if env.Exec == nil {
			continue
		}
		if _, err := env.Exec.LookPath(m.Requires); err == nil {
			return m, true
		}
	}
	return InstallMethod{}, false
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
