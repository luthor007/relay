package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/vault"
)

// The ChatGPT rows, which do not end in a credential question.
//
// What this replaces was a confirmation and then the wrong menu: run `codex
// login` somewhere else, say yes when you are done, now tell Relay which
// environment variable holds the key. There is no key. `codex login` writes
// tokens that expire hourly, and the four reference kinds all describe a string
// that stays put.
//
// OpenClaw's shape, copied:
//
//  1. look for a login that already exists before asking anything, because the
//     machine can answer this question and the user should not have to;
//  2. offer to sign in here when there is none — device pairing first, since
//     this product is installed on headless boxes and a device code is the flow
//     that does not need a browser, a port or a display;
//  3. never ask for a credential reference for a subscription, because the
//     credential is not a thing the user can point at.

// codexOutcome is what the ChatGPT rows produced.
type codexOutcome struct {
	Ref llm.CredentialRef
	// Account is the line the installer prints: who this is, and from where.
	Account string
	// Source is "cli", "device" or "browser".
	Source string
}

// chooseCodexCredential runs a ChatGPT sign-in row to a credential reference.
func chooseCodexCredential(ctx context.Context, opts Options, id string, auth llm.Auth) (codexOutcome, error) {
	p := opts.Prompt

	existing, found := detectCodexLogin(opts)

	choices := make([]Choice, 0, 4)
	def := "device"
	if found {
		choices = append(choices, Choice{
			ID:    "cli",
			Label: "Use the ChatGPT login already on this machine",
			Hint: fmt.Sprintf("%s, from %s — Relay reads it at the moment of use and never copies it",
				existing.Account(), shortSource(existing.Source, opts.Env.Home)),
			Recommended: true,
		})
		def = "cli"
	} else if auth.Kind == llm.AuthOAuth {
		def = "browser"
	}
	choices = append(choices,
		Choice{
			ID:    "device",
			Label: "Sign in now with a device code",
			Hint:  "works on a headless box",
		},
		Choice{
			ID:    "browser",
			Label: "Sign in now in a browser on this machine",
			Hint:  fmt.Sprintf("opens auth.openai.com and listens on port %d", llm.CodexCallbackPort),
		},
		Choice{ID: "skip", Label: "Skip for now", Last: true},
	)

	body := "Relay signs in to ChatGPT itself rather than sending you to another terminal. " +
		"Whichever way you choose, only the refresh token is kept, and the access token that " +
		"actually authenticates a call is minted fresh each time it is needed."
	if found && existing.Mode == "apikey" {
		p.Say("\n  %s", wrapIndent(
			"The Codex CLI on this machine is configured with an API key rather than a ChatGPT "+
				"login, so there is no subscription here to borrow. Sign in below, or go back and "+
				"choose the API key row.", 2, 76))
	}

	kind, err := p.Select(Question{
		ID: id + ".how", Title: "ChatGPT sign-in", Body: body,
		Choices: choices, Default: def,
	})
	if err != nil {
		return codexOutcome{}, err
	}

	switch kind {
	case "skip":
		return codexOutcome{}, errCredentialSkipped

	case "cli":
		// The reference spells out CODEX_HOME rather than defaulting to it, so a
		// relocated home survives being written to config.toml and read back by
		// a daemon with a different environment.
		home := llm.CodexHome(codexOptions(opts))
		return codexOutcome{
			Ref:     llm.CredentialRef{Kind: llm.RefCodex, Value: home},
			Account: existing.Account(),
			Source:  "cli",
		}, nil

	case "device":
		tokens, err := runCodexDevice(ctx, opts)
		if err != nil {
			return codexOutcome{}, err
		}
		return storeCodexLogin(ctx, opts, tokens, "device")

	case "browser":
		tokens, err := runCodexBrowser(ctx, opts, id)
		if err != nil {
			return codexOutcome{}, err
		}
		return storeCodexLogin(ctx, opts, tokens, "browser")
	}
	return codexOutcome{}, fmt.Errorf("install: unknown ChatGPT sign-in %q", kind)
}

// codexOptions routes Codex discovery through the installer's own seams. The
// detection seam is not optional politeness: without it a test reads the
// developer's real ChatGPT login and prints their email into its output.
func codexOptions(opts Options) llm.CodexOptions {
	o := llm.CodexOptions{HTTPClient: opts.HTTPClient}
	if opts.Env.FS != nil {
		o.ReadFile = opts.Env.FS.ReadFile
	}
	if opts.Env.Getenv != nil {
		o.Getenv = opts.Env.Getenv
	}
	o.HomeDir = opts.Env.Home
	return o
}

// detectCodexLogin reports an existing ChatGPT login, if there is one.
func detectCodexLogin(opts Options) (llm.CodexAuth, bool) {
	read := opts.ReadCodexAuth
	if read == nil {
		read = llm.ReadCodexAuth
	}
	auth, err := read(codexOptions(opts))
	if err != nil {
		return llm.CodexAuth{}, false
	}
	return auth, auth.Mode == "chatgpt"
}

// shortSource shortens a path under the home directory back to ~, which is how
// the user thinks of it and how they typed it.
func shortSource(s, home string) string {
	if s == "" {
		return "the Codex CLI"
	}
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return s
		}
	}
	if rest, ok := strings.CutPrefix(s, home); ok {
		return "~" + rest
	}
	return s
}

func runCodexDevice(ctx context.Context, opts Options) (llm.CodexTokens, error) {
	p := opts.Prompt
	login := opts.CodexDeviceLogin
	if login == nil {
		login = func(ctx context.Context, show func(llm.CodexDevicePrompt) error) (llm.CodexTokens, error) {
			return llm.CodexDeviceLogin(ctx, opts.HTTPClient, show)
		}
	}
	return login(ctx, func(prompt llm.CodexDevicePrompt) error {
		p.Say("\n  Open %s on any machine with a browser, and enter:\n", prompt.URL)
		p.Say("      %s\n", prompt.Code)
		p.Say("  Waiting for you to finish. The code is good for %s.",
			time.Until(prompt.Expires).Round(time.Minute))
		return nil
	})
}

func runCodexBrowser(ctx context.Context, opts Options, id string) (llm.CodexTokens, error) {
	p := opts.Prompt
	login := opts.CodexBrowserLogin
	if login == nil {
		login = func(ctx context.Context, open func(string) error, paste func() (string, error)) (llm.CodexTokens, error) {
			return llm.CodexBrowserLogin(ctx, opts.HTTPClient, open, paste)
		}
	}
	return login(ctx,
		func(url string) error {
			p.Say("\n  Open this in a browser:\n")
			p.Say("      %s\n", url)
			return nil
		},
		func() (string, error) {
			// Offered alongside the listener rather than instead of it: if the
			// browser callback lands first this prompt never gets an answer,
			// and if it does not, this is the whole flow.
			return p.Input(Input{
				ID:       id + ".paste",
				Prompt:   "Or paste the code, or the whole redirect URL",
				Body:     "The address bar after sign-in holds it.",
				Optional: true,
			})
		})
}

// storeCodexLogin keeps the refresh token and returns a reference to it.
func storeCodexLogin(ctx context.Context, opts Options, tokens llm.CodexTokens, source string) (codexOutcome, error) {
	if opts.Vault == nil {
		return codexOutcome{}, errors.New(
			"install: signed in, but Relay's vault is not available in this run, so there is " +
				"nowhere to keep the token — rerun setup with the vault open")
	}
	entry, err := opts.Vault.Put(ctx, vault.Input{
		Service: "models",
		Label:   "ChatGPT (Codex)",
		Secret:  tokens.Refresh,
		Source:  vault.Provenance{Kind: vault.SourceTyped, At: opts.Now()},
	})
	if err != nil {
		return codexOutcome{}, fmt.Errorf("install: store the ChatGPT token: %w", err)
	}

	account := "a ChatGPT account"
	if a := llm.CodexAccountOf(tokens); a != "" {
		account = a
	}
	return codexOutcome{
		Ref:     llm.CredentialRef{Kind: llm.RefCodex, Value: "vault:" + entry.ID},
		Account: account,
		Source:  source,
	}, nil
}
