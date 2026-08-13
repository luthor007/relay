package install

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/vault"
)

// Credentials are stored as references, not values — ORCHESTRATOR.md §2, taken
// from OpenClaw. It matters more here than there: a Relay box holds a speech
// key, two model keys and five agent logins at once, and config.toml is the
// file people cat into support tickets.
//
// Four reference kinds are offered. A fifth, `inline:`, exists in internal/llm
// so a custom provider can be configured in a test, and this package refuses to
// write one into a config file — see [checkNoInline], which is wired into the
// save path rather than left as a convention.

// CredentialAsk describes one credential the installer needs.
type CredentialAsk struct {
	// ID prefixes every question this asks, so a scripted run can answer them.
	ID string
	// Service is the vault service name, e.g. "voice" or "models".
	Service string
	// Label is what the user calls it, e.g. "Simba 3.2".
	Label string
	// EnvHint is the environment variable a user most likely already has.
	EnvHint string
	// Optional allows the whole credential to be skipped.
	Optional bool
	// SkipLabel is the wording of the skip row.
	SkipLabel string
}

// maxCredentialAttempts caps the preflight retry loop. Three goes is enough for
// a typo and few enough that an unattended run cannot spin.
const maxCredentialAttempts = 3

var errCredentialSkipped = errors.New("install: credential skipped")

// askCredential asks for a reference and preflights it before it is saved.
//
// The preflight is not the probe. This only checks that the reference leads to
// a secret at all — an unset variable, a missing file, a password-manager
// command that fails. The probe that follows is the real call, and both have to
// happen: a reference that resolves to a stale key looks perfect here.
func askCredential(ctx context.Context, opts Options, ask CredentialAsk) (llm.CredentialRef, error) {
	p := opts.Prompt

	choices := []Choice{
		{
			ID: "env", Label: "An environment variable",
			Hint: "env:" + envHint(ask), Recommended: true,
		},
		{
			ID: "file", Label: "A file on this machine",
			Hint: "file:~/.config/relay/" + strings.ToLower(ask.Service) + ".key",
		},
		{
			ID: "exec", Label: "A command that prints it",
			Hint: "exec:op read op://Private/" + ask.Label + "/credential",
		},
		{
			ID: "vault", Label: "Type it now, and Relay keeps it",
			Hint: "kept in Relay's encrypted vault",
		},
	}
	if ask.Optional {
		label := ask.SkipLabel
		if label == "" {
			label = "Skip for now"
		}
		choices = append(choices, Choice{ID: "skip", Label: label, Last: true})
	}

	var previous llm.CredentialRef
	for attempt := 0; attempt < maxCredentialAttempts; attempt++ {
		kind, err := p.Select(Question{
			ID:      ask.ID + ".kind",
			Title:   "Credential for " + ask.Label,
			Body:    "Relay stores a reference, not the key.",
			Choices: choices,
			Default: "env",
		})
		if err != nil {
			return llm.CredentialRef{}, err
		}
		if kind == "skip" {
			return llm.CredentialRef{}, errCredentialSkipped
		}

		ref, err := readRef(ctx, opts, ask, kind)
		if err != nil {
			return llm.CredentialRef{}, err
		}
		if ref.IsZero() {
			return llm.CredentialRef{}, errCredentialSkipped
		}

		// Preflight, before saving.
		if _, err := ref.Resolve(ctx, opts.Lookup()); err != nil {
			p.Say("  That reference does not resolve yet: %s", err.Error())
			// An unattended run answers every question the same way, so asking
			// again would print the same failure three times and change
			// nothing. One identical answer is enough to know that.
			if ref == previous || attempt == maxCredentialAttempts-1 {
				return ref, nil
			}
			previous = ref
			again, cerr := p.Confirm(Confirm{
				ID:      ask.ID + ".retry",
				Prompt:  "Try a different reference?",
				Default: true,
			})
			if cerr != nil {
				return ref, cerr
			}
			if !again {
				return ref, nil
			}
			continue
		}
		p.Say("  %s resolves.", ref.String())
		return ref, nil
	}
	return llm.CredentialRef{}, errCredentialSkipped
}

func envHint(ask CredentialAsk) string {
	if ask.EnvHint != "" {
		return ask.EnvHint
	}
	return strings.ToUpper(strings.ReplaceAll(ask.Service, ".", "_")) + "_API_KEY"
}

func readRef(ctx context.Context, opts Options, ask CredentialAsk, kind string) (llm.CredentialRef, error) {
	p := opts.Prompt
	switch kind {
	case "env":
		v, err := p.Input(Input{
			ID: ask.ID + ".env", Prompt: "Variable name", Default: envHint(ask),
		})
		if err != nil {
			return llm.CredentialRef{}, err
		}
		return llm.CredentialRef{Kind: llm.RefEnv, Value: strings.TrimPrefix(v, "$")}, nil

	case "file":
		v, err := p.Input(Input{ID: ask.ID + ".file", Prompt: "Path"})
		if err != nil {
			return llm.CredentialRef{}, err
		}
		return llm.CredentialRef{Kind: llm.RefFile, Value: v}, nil

	case "exec":
		v, err := p.Input(Input{
			ID: ask.ID + ".exec", Prompt: "Command",
			Body: "Its stdout is the secret. " + llm.ExecTimeout.String() + " to answer.",
		})
		if err != nil {
			return llm.CredentialRef{}, err
		}
		return llm.CredentialRef{Kind: llm.RefExec, Value: v}, nil

	case "vault":
		if opts.Vault == nil {
			// Refusing beats writing a secret into config.toml. Say why, and go
			// back to the menu rather than dead-ending.
			p.Say("  Relay's vault is not available in this run, so there is nowhere safe to put " +
				"a typed key. Use an environment variable, a file or a command instead — a key " +
				"pasted into config.toml ends up in a backup, a screenshot and a support ticket.")
			return llm.CredentialRef{}, nil
		}
		secret, err := p.Input(Input{
			ID: ask.ID + ".secret", Prompt: ask.Label + " key", Secret: true,
			Body: "Goes to the vault. The config file gets a reference.",
		})
		if err != nil {
			return llm.CredentialRef{}, err
		}
		secret = strings.TrimSpace(secret)
		if secret == "" {
			return llm.CredentialRef{}, nil
		}
		entry, err := opts.Vault.Put(ctx, vault.Input{
			Service: ask.Service,
			Label:   ask.Label,
			Secret:  secret,
			Source:  vault.Provenance{Kind: vault.SourceTyped, At: opts.Now()},
		})
		if err != nil {
			return llm.CredentialRef{}, fmt.Errorf("install: store credential: %w", err)
		}
		return llm.CredentialRef{Kind: llm.RefVault, Value: entry.ID}, nil
	}
	return llm.CredentialRef{}, fmt.Errorf("install: unknown credential kind %q", kind)
}

// Lookup returns the vault resolver for llm and voice references, or nil.
func (o Options) Lookup() llm.SecretLookup {
	if o.Vault == nil {
		return nil
	}
	rv, ok := o.Vault.(interface {
		Reveal(ctx context.Context, id string) (string, error)
	})
	if !ok {
		return nil
	}
	return rv.Reveal
}

// checkNoInline refuses to write an inline secret into a config file.
//
// ORCHESTRATOR.md §2 allows `inline:` in internal/llm so a custom provider can
// be configured in a test, and refuses it in a config file. This is that
// refusal, enforced on the save path rather than left to discipline.
func checkNoInline(refs map[string]string) error {
	for field, ref := range refs {
		if strings.HasPrefix(ref, string(llm.RefInline)+":") {
			return fmt.Errorf(
				"install: %s is an inline secret; config.toml holds references only "+
					"(env:, file:, exec: or vault:)", field)
		}
	}
	return nil
}
