// Package vault holds credentials. MEMORY.md §6 calls this the most dangerous
// thing in the design, so the shape is defensive on purpose.
//
// Two rules are enforced by the types rather than by discipline:
//
//   - [Entry] has no secret field. Every listing, every console row and every
//     log line goes through Entry, and the only way to a plaintext secret is
//     [Vault.Reveal], which is named to be obvious in a code review.
//     DASHBOARD.md §3.2: never display a secret after it is stored — last four
//     characters and a re-validate button, because a UI that shows the key back
//     to you is a UI that gets screenshotted into a support ticket.
//   - The vault is a separate database from the index, and it is never
//     indexed. That separation is asserted in internal/store's tests.
//
// Provenance is carried on every entry because newest-validated wins and two
// Stripe keys means one is probably rotated. So is [Provenance.SharedSession]:
// a key in your transcript may not be yours, and the proposal has to say so.
//
// M3 and M4 extend this — rotation, the proposal queue, the cloud KMS backend.
// This is the shape they extend.
package vault

import (
	"context"
	"errors"
	"time"
)

// Errors.
var (
	ErrNotFound = errors.New("vault: no such credential")
	ErrRevoked  = errors.New("vault: credential was revoked")
	// ErrLocked means the vault's encryption key is unavailable — the keychain
	// is locked, or the key file is gone. It is recoverable, unlike a lost key.
	ErrLocked = errors.New("vault: cannot unlock the store")
)

// SourceKind is how a credential arrived, in MEMORY.md §6's order of how much
// we should like them.
type SourceKind string

const (
	// SourceTyped is the clean path: typed into the app or the console.
	SourceTyped SourceKind = "typed"
	// SourceConfig is discovered in an existing runtime config, enumerable at
	// install with the user watching.
	SourceConfig SourceKind = "config"
	// SourceTranscript is extracted from a session transcript. The convenience
	// path, and the one that needs the guardrails.
	SourceTranscript SourceKind = "transcript"
)

// Provenance is where a credential came from. A credential without it cannot
// be reasoned about later — which of two Stripe keys is current, and whether
// this one was even the user's to keep.
type Provenance struct {
	Kind    SourceKind
	Runtime string
	Session string
	Path    string
	At      time.Time
	// SharedSession marks a session that had another participant. Colleagues
	// paste keys into pairing sessions, and the confirmation step is the only
	// thing standing between that and storing someone else's production
	// credential.
	SharedSession bool
}

// Entry is everything about a credential except the credential.
//
// Nothing in this struct is secret. If a field is ever added that could be,
// the test in this package that asserts a round-tripped Entry does not contain
// its own secret is what fails.
type Entry struct {
	ID      string
	Service string
	Label   string

	// LastFour is the display form, and the only form. It is empty for secrets
	// short enough that four characters would be most of them.
	LastFour string

	Backend Backend
	Source  Provenance

	CreatedAt  time.Time
	LastUsedAt time.Time
	LastUsedBy string

	LastValidatedAt time.Time
	// LastValidationReason is llm.Reason as a string — ok, missing_credential,
	// expired, unresolved_ref, unavailable — kept as a string so the vault does
	// not depend on the model client.
	LastValidationReason string

	RevokedAt time.Time
}

// Revoked reports whether this credential has been withdrawn.
func (e Entry) Revoked() bool { return !e.RevokedAt.IsZero() }

// Input is a new credential.
type Input struct {
	// ID is optional; one is generated when empty.
	ID      string
	Service string
	Label   string
	Secret  string
	Source  Provenance
}

// Vault stores credentials.
type Vault interface {
	// Put stores a secret and returns its entry. The returned Entry never
	// carries the secret back.
	Put(ctx context.Context, in Input) (Entry, error)

	// Reveal returns the plaintext secret. This is the privileged path: use it
	// to make a call, never to display. Everything that only needs to show a
	// credential uses Entry.LastFour.
	Reveal(ctx context.Context, id string) (string, error)

	// Get returns one entry without its secret.
	Get(ctx context.Context, id string) (Entry, error)

	// List returns every entry without any secrets. Revoked ones are included
	// so the console can show that they were revoked rather than lose them.
	List(ctx context.Context) ([]Entry, error)

	// Current is the credential a runtime should use for a service. MEMORY.md
	// §6: newest validated wins, and provenance is kept — two Stripe keys means
	// one is probably rotated, and the vault says which is which rather than
	// guessing. It returns ErrNotFound when nothing live is held.
	Current(ctx context.Context, service string) (Entry, error)

	// Proposals is the confirmation queue. Nothing is captured silently:
	// detection produces a question, and this is where it waits for an answer.
	Proposals() Proposals

	// Touch records that a credential was used, and by which runtime.
	// DASHBOARD.md §3.4: access nobody has touched in a month is the kind that
	// gets forgotten and then exploited.
	Touch(ctx context.Context, id, usedBy string) error

	// RecordValidation stores the result of a probe. MEMORY.md §6: validate
	// before trusting, with one real call — transcript-scraped keys are
	// frequently stale, revoked, or truncated by a line wrap, and a silently
	// wrong credential is worse than a missing one.
	RecordValidation(ctx context.Context, id, reason string, at time.Time) error

	// Revoke marks a credential withdrawn and destroys the secret material.
	Revoke(ctx context.Context, id string) error

	// Status describes where the secrets actually live, so DASHBOARD.md §3.5
	// can say it out loud rather than implying a keychain that is not there.
	Status() Status

	Close() error
}

// Backend is where secret material is kept.
type Backend string

const (
	// BackendKeychain puts the secret itself in the OS keychain.
	BackendKeychain Backend = "keychain"
	// BackendFile puts AES-256-GCM ciphertext in the vault database, with the
	// key in the keychain where there is one.
	BackendFile Backend = "file"
)

// KeySource is where the vault's AES-256-GCM key came from.
//
// It is set whichever credential backend won, because the proposal queue seals
// undecided candidates into the vault database in both cases — an unconfirmed
// key is not yet anybody's credential and does not get a keychain entry of its
// own. With a working keychain the key is in the keychain and this reads
// "keychain"; [KeySourceFile] is the degraded case and the one the console
// draws a warning for.
type KeySource string

const (
	KeySourceNone     KeySource = ""
	KeySourceKeychain KeySource = "keychain"
	// KeySourceFile is the degraded mode: a 0600 key file beside the database,
	// because this machine has no usable keychain. MEMORY.md §6 describes the
	// keychain path; this is what honest degradation looks like when there is
	// no keychain to degrade to, and Status reports it so the console can show
	// it rather than implying protection that is not there.
	KeySourceFile KeySource = "file"
)

// Status is what the console shows about the vault itself.
type Status struct {
	Backend   Backend
	KeySource KeySource
	// Degraded is true when the OS keychain was unavailable.
	Degraded bool
	// Reason is the keychain's own error, when there was one.
	Reason string
}

// LastFour is the display form of a secret, and the only one that leaves this
// package other than through Reveal.
//
// Short secrets get an empty string rather than most of themselves: four
// characters of a six-character token is not a hint, it is the token.
func LastFour(secret string) string {
	r := []rune(secret)
	if len(r) < 12 {
		return ""
	}
	return string(r[len(r)-4:])
}
