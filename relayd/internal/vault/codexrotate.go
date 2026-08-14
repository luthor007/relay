package vault

import (
	"context"
	"fmt"
	"time"
)

// Keeping a rotated ChatGPT refresh token.
//
// From a real machine, on the second `relay setup`:
//
//	gpt-5.6-luna on OpenAI is already configured here, and did not answer just
//	now: the ChatGPT login in Relay's vault could not be refreshed (http 401:
//	"Your refresh token has already been used to generate a new access token.
//	Please try signing in again." code: refresh_token_reused)
//
// OpenAI rotates: a refresh returns a new refresh token and invalidates the one
// that bought it. llm.resolveCodexVault knows this and calls llm.CodexPersist
// on every rotation — and nothing in this repository ever assigned that
// variable, in either process, despite its own comment saying the daemon did.
//
// So every rotation lived in one process's memory and died with it, the vault
// kept the token from sign-in forever, and the second process to resolve that
// credential presented a refresh token that had already been spent. Which is to
// say: a ChatGPT login worked exactly once, in the run that created it, and
// then never again — including for relayd, which is the process that has to use
// it every day.

// Rotatable is the half of [Vault] this needs: read the entry to keep its
// name, write the new secret under the same id. Narrower than Vault so the
// installer, which holds a Put-only handle, can wire this too — and the
// installer is where a ChatGPT login is created and first spent.
type Rotatable interface {
	Get(ctx context.Context, id string) (Entry, error)
	Put(ctx context.Context, in Input) (Entry, error)
}

// RotateCodex returns a function for llm.CodexPersist that writes a rotated
// refresh token back to this vault.
//
// The entry's own service, label and provenance are read first and kept: a
// rotation must not turn "ChatGPT (alexis@…)" into an anonymous row, and it is
// not a new origin. This schema's comment on provenance says what it is for —
// "newest validated wins and two Stripe keys means one is probably rotated" —
// which is about where a credential came from, and a rotated ChatGPT token came
// from the same sign-in. Only `At` moves, to when this value took over.
func RotateCodex(v Rotatable) func(ctx context.Context, id, refresh string) error {
	if v == nil {
		return nil
	}
	return func(ctx context.Context, id, refresh string) error {
		if id == "" || refresh == "" {
			return fmt.Errorf("vault: refusing to store an empty rotated token")
		}
		service, label := "openai", "ChatGPT login"
		source := Provenance{Kind: SourceTyped}
		if e, err := v.Get(ctx, id); err == nil {
			if e.Service != "" {
				service = e.Service
			}
			if e.Label != "" {
				label = e.Label
			}
			if e.Source.Kind != "" {
				source = e.Source
			}
		}
		source.At = time.Now()
		_, err := v.Put(ctx, Input{
			ID: id, Service: service, Label: label, Secret: refresh, Source: source,
		})
		return err
	}
}
