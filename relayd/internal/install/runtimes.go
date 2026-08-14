package install

import (
	"context"
	"errors"
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
			Why: "Where your Claude subscription works, if you have one.",
			Methods: []InstallMethod{
				{Requires: "npm", Command: []string{"npm", "install", "-g", "@anthropic-ai/claude-code"}},
			},
			Docs: "https://docs.anthropic.com/en/docs/claude-code",
		},
		{
			Runtime: adapter.Codex, Label: "Codex",
			Why: "Where your ChatGPT subscription works, if you have one.",
			Methods: []InstallMethod{
				{Requires: "npm", Command: []string{"npm", "install", "-g", "@openai/codex"}},
			},
			Docs: "https://developers.openai.com/codex/cli",
		},
		{
			Runtime: adapter.OpenCode, Label: "OpenCode",
			Why: "Speaks ACP.",
			Methods: []InstallMethod{
				{Requires: "npm", Command: []string{"npm", "install", "-g", "opencode-ai"}},
			},
			Docs: "https://opencode.ai",
		},
		{
			Runtime: adapter.OpenClaw, Label: "OpenClaw",
			Why: "Speaks ACP; drives the other runtimes.",
			// Probed on a real machine on 2026-08-12, which is what this table
			// requires before it will print a command: the global package was
			// removed with `npm uninstall -g openclaw` and the registry lists
			// the same name and binary. Hermes is still open for want of the
			// same evidence.
			Methods: []InstallMethod{
				{Requires: "npm", Command: []string{"npm", "install", "-g", "openclaw"}},
			},
			Docs: "https://docs.openclaw.ai",
		},
		{
			Runtime: adapter.Hermes, Label: "Hermes",
			Why:  "Speaks ACP.",
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

	// "5 of the five" is what the old line printed on a clean machine, which
	// reads as a typo because it is one.
	count := fmt.Sprintf("%d of the five", len(missing))
	if len(missing) == len(Installers()) {
		count = "All five"
	}
	p.Section("Anything else?", fmt.Sprintf(
		"%s agent runtimes are missing. Nothing is installed unless you say so.", count))

	for _, f := range missing {
		// Between rows, because the loop asks one question per runtime and an
		// interrupt at the first of them used to be answered by asking the next
		// three — each one failing instantly with "context canceled".
		if err := ctx.Err(); err != nil {
			return out, err
		}
		inst, ok := installerFor(f.Runtime)
		if !ok {
			continue
		}
		out.Offered = append(out.Offered, f.Runtime)

		method, methodOK := pickMethod(opts.Env, inst)
		// Every row here installs with npm, and a machine with no Node has no
		// npm — which used to mean a clean Mac mini finished setup with no
		// agent runtimes at all and no explanation beyond "no install command".
		// Offered once: declining puts every row back on the unknown list.
		if !methodOK && needsNode(inst) {
			ok, err := ensureNode(ctx, opts, "The agent runtimes")
			if err != nil {
				return out, err
			}
			if ok {
				method, methodOK = pickMethod(opts.Env, inst)
			}
		}
		if !methodOK {
			out.Unknown = append(out.Unknown, f.Runtime)
			msg := fmt.Sprintf("%s: Relay has no install command it can run here.", inst.Label)
			if inst.Docs != "" {
				msg += " Install it from " + inst.Docs + " and re-run `relay detect`."
			} else {
				msg += " Install it yourself and re-run `relay detect`."
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
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			// Not a failed install — an interrupted one. Saying "Claude Code
			// did not install: context canceled" blames npm for the user's
			// own Ctrl-C.
			return out, err
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
// needsNode reports whether every way of installing this runtime goes through
// npm, which is the only case worth offering a language runtime for.
func needsNode(inst RuntimeInstaller) bool {
	if len(inst.Methods) == 0 {
		return false
	}
	for _, m := range inst.Methods {
		if m.Requires != "npm" {
			return false
		}
	}
	return true
}

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
