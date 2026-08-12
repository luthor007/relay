package main

import (
	"context"
	"log/slog"

	"github.com/luthor007/relay/relayd/internal/config"
	"github.com/luthor007/relay/relayd/internal/facts"
	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/orchestrator"
	"github.com/luthor007/relay/relayd/internal/store"
	"github.com/luthor007/relay/relayd/internal/vault"
)

// buildModels builds ORCHESTRATOR.md §3b's two models from the config.
//
// Neither is required to start. A machine with no keys still runs the daemon,
// still lists sessions, still pings, and still answers the allowlist — it just
// says so when asked for something that needs a model. Refusing to start
// because one subsystem is unconfigured is the shape this file already rejects
// for the vault and the audit log, and the reasoning is the same: a daemon that
// runs and explains beats one that will not come up.
func buildModels(cfg config.Config, lookups credentialLookups, log *slog.Logger) (small, big llm.Provider) {
	build := func(name string, m config.Model) llm.Provider {
		if m.Model == "" {
			return nil
		}
		ref, err := llm.ParseRef(m.Credential)
		if err != nil {
			log.Warn("relayd: model credential is not a usable reference",
				"model", name, "error", err,
				"detail", "credentials are references — env:, file:, exec: or vault: — never pasted secrets")
			return nil
		}
		p, err := llm.New(llm.Config{
			Vendor:     m.Vendor,
			API:        llm.API(m.API),
			BaseURL:    m.BaseURL,
			Model:      m.Model,
			Credential: ref,
			Lookup:     lookups.resolver(usedBy(name)),
		})
		if err != nil {
			log.Warn("relayd: could not build a model", "model", name, "error", err)
			return nil
		}
		return p
	}
	return build("small", cfg.Models.Small), build("big", cfg.Models.Big)
}

// usedBy names the consumer that resolved a credential, for the vault's
// last_used_by column.
func usedBy(name string) string { return "relayd/" + name }

// credentialLookups hands out "vault:<id>" resolvers, one per consumer.
//
// A resolver is a function rather than the vault itself so internal/llm never
// imports internal/vault: the model client's whole credential story is that it
// holds references and asks somebody else to resolve them. A nil vault resolves
// nothing, which turns a "vault:" reference into a clear error at first use
// rather than a silent unauthenticated call.
//
// It is a factory rather than one shared function because of the second half of
// the job. DASHBOARD.md §3.4 wants to show access nobody has touched in a
// month, and [vault.Vault.Touch] is what makes that answerable — but a
// credential used by three consumers is only worth recording if the row says
// which one. Nothing in this tree called Touch at all before this, so
// last_used_at and last_used_by were permanently empty and §3.4 could never be
// evaluated on a real machine.
type credentialLookups struct {
	v   vault.Vault
	log *slog.Logger
}

// credentialLookup takes the privileged handle. It is named after
// [vault.Vault.Reveal] for the same reason that method is: so a code review
// sees who holds the thing that returns plaintext.
func credentialLookup(v vault.Vault, log *slog.Logger) credentialLookups {
	return credentialLookups{v: v, log: log}
}

// resolver returns one consumer's lookup, or nil when there is no vault.
func (c credentialLookups) resolver(by string) llm.SecretLookup {
	if c.v == nil {
		return nil
	}
	return func(ctx context.Context, id string) (string, error) {
		secret, err := c.v.Reveal(ctx, id)
		if err != nil {
			return "", err
		}
		// A failed Touch is logged and swallowed. The credential resolved; the
		// call it resolved for must not fail because a bookkeeping UPDATE did.
		// A model call that dies over an access-time column is a worse outcome
		// than a stale column.
		if err := c.v.Touch(ctx, id, by); err != nil && c.log != nil {
			c.log.Warn("relayd: could not record that a credential was used",
				"credential", id, "used_by", by, "error", err)
		}
		return secret, nil
	}
}

// notebook is the write half of memory, or nil.
//
// Nil is a supported state and it removes the remember tool rather than
// breaking it: an install whose fact tier will not open should still route
// sessions and answer questions, and a model told it can remember something it
// cannot is worse than one that knows it cannot.
func notebook(db *store.DB, log *slog.Logger) orchestrator.Notebook {
	if db == nil {
		return nil
	}
	// The redactor is required, and refusing to open without one is the fact
	// tier enforcing MEMORY.md §5's last rule — nothing here is a secret —
	// rather than trusting every caller to remember it.
	f, err := facts.Open(db, facts.Options{Redactor: facts.Detector()})
	if err != nil {
		log.Warn("relayd: no fact tier; the orchestrator will not remember anything new",
			"error", err)
		return nil
	}
	return orchestrator.NotebookIn(f)
}

// statusOf renders a subsystem's state for the health screen: "on", or the
// reason it is not.
func statusOf(on bool, why string) string {
	if on {
		return "on"
	}
	return why
}
