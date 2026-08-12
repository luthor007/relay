package vault

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/index"
)

// MEMORY.md §6's proposal flow: nothing is captured silently.
//
// Detection does not store a credential. It stores a *question* — "I found what
// looks like a Twilio auth token in a session from March. Save it as your Twilio
// credential?" — in the same shape as ORCHESTRATOR.md §4b's connector flow, and
// the answer is a human's. Four rules hold this together, and each one is a
// method or a refusal below rather than a note:
//
//   - **Tier 1 only reaches this queue.** MEMORY.md §12.2 measured a 26%
//     false-positive rate on the shape heuristics — a Twilio auth token and an
//     MD5 digest are both exactly 32 lowercase hex characters and no rule
//     separates them — so a tier-2 hit is redacted before indexing and never
//     proposed. [Propose] refuses one. Redaction is cheap; a wrong proposal
//     costs the user's trust in every later one.
//   - **Validate before trusting.** [Proposals.Accept] makes one real call
//     through a [Validator] and refuses to store a credential the provider
//     rejected. Transcript-scraped keys are frequently stale, revoked, or
//     truncated by a line wrap, and a silently-wrong credential is worse than a
//     missing one. When there is no validator, or the provider does not answer,
//     the credential is stored *without* a validation timestamp, so the console
//     shows the gap instead of a green tick nobody earned.
//   - **A key in your transcript may not be yours.** [Proposal.Line] says so
//     whenever the session had another participant, because the confirmation is
//     the only thing standing between a colleague's pasted key and us storing
//     someone else's production credential.
//   - **Newest validated wins, and provenance is kept.** Accepting never
//     overwrites: the older credential stays with its own provenance and
//     [Vault.Current] picks between them.
//
// A candidate lives in the vault database as AES-GCM ciphertext from the moment
// it is proposed until the moment it is decided, and the ciphertext is destroyed
// either way. That is deliberately not a keychain entry: a proposal is not yet
// anybody's credential.

// Proposal errors.
var (
	// ErrNotProposable is a tier-2 shape match. It is redacted, never proposed.
	ErrNotProposable = errors.New("vault: a shape match is redacted, never proposed")
	// ErrDecided means the user has already answered this question.
	ErrDecided = errors.New("vault: that proposal was already decided")
	// ErrKnown means this exact secret is already in the vault. Asking again is
	// how a proposal queue becomes noise the user learns to dismiss unread.
	ErrKnown = errors.New("vault: that credential is already in the vault")
	// ErrInvalidCredential is a candidate the provider rejected. Storing it
	// anyway would be worse than storing nothing.
	ErrInvalidCredential = errors.New("vault: the provider rejected that credential")
)

// Decision is the answer to a proposal.
type Decision string

const (
	// Undecided is an open question, waiting on the console or the app.
	Undecided Decision = ""
	Accepted  Decision = "accepted"
	Dismissed Decision = "dismissed"
)

// Candidate is a detected credential before anybody has confirmed it.
//
// It carries the plaintext, which is the whole reason this type does not
// survive a function call: [Proposals.Propose] seals it and forgets it, and
// nothing here is logged without [logx.Secret].
type Candidate struct {
	// Service is the vendor a proposal will name — "twilio", "stripe". A
	// candidate that cannot name one is not proposed: "save it as your ???
	// credential" is not a question anybody can answer.
	Service string
	// Label is the detector's own words: "Twilio auth token".
	Label string
	// Detector is the rule id that matched, for the console's tier column.
	Detector string
	// Tier is which half of MEMORY.md §12.2's ruleset fired. Required for a
	// transcript candidate.
	Tier index.Tier

	Secret string
	Source Provenance
}

// FromFinding turns an internal/index detection into a candidate.
//
// It returns false for anything that must not be proposed: a tier-2 shape
// match, or a vendor-less rule like a bare JWT. Those are still redacted before
// indexing — that is internal/index's job and it has already happened — and the
// searchable artefact is the marker saying one appeared. Being unable to name
// the vendor is a good reason not to ask.
func FromFinding(f index.Finding, src Provenance) (Candidate, bool) {
	if !f.Tier.ProposeToVault() || strings.TrimSpace(f.Service) == "" || f.Value == "" {
		return Candidate{}, false
	}
	return Candidate{
		Service:  f.Service,
		Label:    f.Label,
		Detector: f.RuleID,
		Tier:     f.Tier,
		Secret:   f.Value,
		Source:   src,
	}, true
}

// Proposal is one stored, undecided question. It never carries the candidate.
type Proposal struct {
	ID       string
	Service  string
	Label    string
	Detector string
	// LastFour is as much of the candidate as anything ever shows, and it is
	// empty for secrets short enough that four characters would be most of them.
	LastFour string

	Source    Provenance
	CreatedAt time.Time

	Decision   Decision
	DecidedAt  time.Time
	Reason     string
	Credential string
}

// Open reports whether this proposal is still a question.
func (p Proposal) Open() bool { return p.Decision == Undecided }

// Line is the whole prompt, in the shape MEMORY.md §6 gives it.
//
// The shared-session sentence is not optional decoration. Colleagues paste keys
// into pairing sessions, and this is the only place the user is told that the
// thing they are about to save may not be theirs.
func (p Proposal) Line() string {
	what := p.Label
	if what == "" {
		what = p.Service + " credential"
	}
	where := ""
	switch {
	case p.Source.Kind == SourceConfig && p.Source.Path != "":
		where = " in " + p.Source.Path
	case !p.Source.At.IsZero():
		where = " in a session from " + p.Source.At.Format("January 2006")
	case p.Source.Session != "":
		where = " in an earlier session"
	}

	s := "I found what looks like a " + what + where + ". Save it as your " +
		strings.TrimSpace(p.Service) + " credential?"
	if p.Source.SharedSession {
		s += " That session had another participant, so this key may not be yours to keep."
	}
	return s
}

// Proposals is the confirmation queue.
type Proposals interface {
	// Propose stores a question. It is idempotent on the candidate itself: the
	// same key found in forty sessions is one proposal, not forty, and a key
	// already in the vault produces [ErrKnown] rather than a second ask.
	Propose(ctx context.Context, c Candidate) (Proposal, error)

	// List returns the open questions, newest first.
	List(ctx context.Context) ([]Proposal, error)
	// History returns the decided ones, so the console can show what was
	// dismissed and why rather than losing it.
	History(ctx context.Context) ([]Proposal, error)
	Get(ctx context.Context, id string) (Proposal, error)

	// Accept validates the candidate with one real call and, if the provider
	// does not reject it, stores it. label may be empty.
	Accept(ctx context.Context, id, label string) (Entry, error)
	// Dismiss answers no, and keeps why.
	Dismiss(ctx context.Context, id, reason string) error
}

// ---------------------------------------------------------------- validation --

// Validation is what one real call found out. Reason is llm.Reason as a string
// — ok, missing_credential, expired, unresolved_ref, unavailable — kept as a
// string so the vault does not depend on the model client.
type Validation struct {
	Reason  string
	Detail  string
	At      time.Time
	Service string
}

// OK reports whether the provider accepted the credential.
func (v Validation) OK() bool { return v.Reason == "ok" }

// Refused reports whether the provider said this credential is wrong, as
// opposed to not answering. Only a refusal blocks an accept: a provider outage
// must not stop somebody saving a key they have in front of them.
func (v Validation) Refused() bool {
	switch v.Reason {
	case "expired", "missing_credential":
		return true
	}
	return false
}

// Validator makes MEMORY.md §6's one real call. It takes the plaintext because
// that is what a real call needs, and it is the only interface in this package
// that does.
type Validator interface {
	Validate(ctx context.Context, service, secret string) (Validation, error)
}

// ValidatorFunc adapts a function to [Validator].
type ValidatorFunc func(ctx context.Context, service, secret string) (Validation, error)

// Validate calls f.
func (f ValidatorFunc) Validate(ctx context.Context, service, secret string) (Validation, error) {
	return f(ctx, service, secret)
}

// ------------------------------------------------------------ implementation --

// Proposals returns this vault's confirmation queue.
func (v *vault) Proposals() Proposals { return (*proposals)(v) }

type proposals vault

func (p *proposals) v() *vault { return (*vault)(p) }

func (p *proposals) Propose(ctx context.Context, c Candidate) (Proposal, error) {
	v := p.v()
	if strings.TrimSpace(c.Service) == "" {
		return Proposal{}, errors.New("vault: a proposal has to name a service; \"save it as your ??? credential\" is not a question")
	}
	if c.Secret == "" {
		return Proposal{}, errors.New("vault: refusing to propose an empty secret")
	}
	if c.Source.Kind == "" {
		return Proposal{}, errors.New("vault: a proposal needs provenance; MEMORY.md §6 keeps which session and what date")
	}
	if c.Source.Kind == SourceTranscript && c.Tier == 0 {
		return Proposal{}, fmt.Errorf("%w: a transcript candidate must say which tier found it", ErrNotProposable)
	}
	if c.Tier != 0 && !c.Tier.ProposeToVault() {
		return Proposal{}, fmt.Errorf("%w: %s is %s and one in four of those is a checksum",
			ErrNotProposable, c.Detector, c.Tier)
	}

	fp := v.fingerprint(c.Service, c.Secret)
	if fp == "" {
		return Proposal{}, fmt.Errorf("%w: this vault has no key to fingerprint with", ErrLocked)
	}
	if held, err := v.db.Meta(ctx, fingerprintKey(fp)); err != nil {
		return Proposal{}, err
	} else if held != "" {
		return Proposal{}, fmt.Errorf("%w: %s", ErrKnown, held)
	}

	// The id *is* the fingerprint, so the same key found in a hundred sessions
	// is one question. A dismissal therefore sticks across sessions too, which
	// is the difference between a queue and a nag.
	id := fp
	existing, err := p.Get(ctx, id)
	switch {
	case err == nil && !existing.Open():
		return existing, fmt.Errorf("%w: %s", ErrDecided, existing.Decision)
	case err == nil:
		return existing, nil
	case !errors.Is(err, ErrNotFound):
		return Proposal{}, err
	}

	ciphertext, nonce, err := v.seal(c.Secret)
	if err != nil {
		return Proposal{}, err
	}
	now := v.clock()
	_, err = v.db.SQL().ExecContext(ctx, `
		INSERT INTO credential_proposal (id, service, detector, last_four, ciphertext, nonce,
			source_kind, source_runtime, source_session, source_path, source_at,
			shared_session, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, c.Service, detectorWithTier(c.Detector, c.Tier), LastFour(c.Secret), ciphertext, nonce,
		string(c.Source.Kind), c.Source.Runtime, c.Source.Session, c.Source.Path,
		nullTime(c.Source.At), boolInt(c.Source.SharedSession), now.UnixMilli())
	if err != nil {
		return Proposal{}, err
	}
	if c.Label != "" {
		if err := v.db.SetMeta(ctx, labelKey(id), c.Label); err != nil {
			return Proposal{}, err
		}
	}
	return p.Get(ctx, id)
}

func (p *proposals) List(ctx context.Context) ([]Proposal, error) {
	return p.query(ctx, `WHERE decided_at IS NULL`)
}

func (p *proposals) History(ctx context.Context) ([]Proposal, error) {
	return p.query(ctx, `WHERE decided_at IS NOT NULL`)
}

func (p *proposals) query(ctx context.Context, where string) ([]Proposal, error) {
	rows, err := p.v().db.SQL().QueryContext(ctx,
		`SELECT `+proposalCols+` FROM credential_proposal `+where+` ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Proposal
	for rows.Next() {
		v, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := p.hydrate(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (p *proposals) Get(ctx context.Context, id string) (Proposal, error) {
	row := p.v().db.SQL().QueryRowContext(ctx,
		`SELECT `+proposalCols+` FROM credential_proposal WHERE id = ?`, id)
	v, err := scanProposal(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Proposal{}, fmt.Errorf("%w: proposal %s", ErrNotFound, id)
	}
	if err != nil {
		return Proposal{}, err
	}
	return v, p.hydrate(ctx, &v)
}

// hydrate fills the three fields the schema has no column for. They live in the
// vault's own meta table rather than in a migration this package does not own;
// the alternative was dropping the dismissal reason, and "why did we say no to
// this key" is exactly what stops it being re-proposed forever.
func (p *proposals) hydrate(ctx context.Context, v *Proposal) error {
	db := p.v().db
	label, err := db.Meta(ctx, labelKey(v.ID))
	if err != nil {
		return err
	}
	v.Label = label
	reason, err := db.Meta(ctx, reasonKey(v.ID))
	if err != nil {
		return err
	}
	v.Reason = reason
	cred, err := db.Meta(ctx, credentialKey(v.ID))
	if err != nil {
		return err
	}
	v.Credential = cred
	return nil
}

func (p *proposals) Accept(ctx context.Context, id, label string) (Entry, error) {
	v := p.v()
	prop, secret, err := p.reveal(ctx, id)
	if err != nil {
		return Entry{}, err
	}

	// Validate before trusting: one real call. A provider that says the key is
	// wrong stops this here, with the proposal left open so the console can
	// show why rather than losing the question.
	var val Validation
	validated := false
	if v.validator != nil {
		val, err = v.validator.Validate(ctx, prop.Service, secret)
		if err != nil {
			val = Validation{Reason: "unavailable", Detail: err.Error()}
		}
		validated = true
		if val.Refused() {
			detail := val.Detail
			if detail == "" {
				detail = val.Reason
			}
			return Entry{}, fmt.Errorf("%w: %s (%s)", ErrInvalidCredential, prop.Service, detail)
		}
	}

	if label == "" {
		label = prop.Label
	}
	entry, err := v.Put(ctx, Input{
		Service: prop.Service,
		Label:   label,
		Secret:  secret,
		Source:  prop.Source,
	})
	if err != nil {
		return Entry{}, err
	}

	// Only a probe that happened is recorded. A validator that was not
	// configured leaves last_validated_at NULL, and one that could not reach
	// the provider leaves it NULL with a reason — the console shows "never
	// validated" and "tried, no answer" as the different things they are.
	if validated {
		at := val.At
		if at.IsZero() {
			at = v.clock()
		}
		if val.OK() {
			if err := v.RecordValidation(ctx, entry.ID, val.Reason, at); err != nil {
				return Entry{}, err
			}
		} else if _, err := v.db.SQL().ExecContext(ctx,
			`UPDATE credential SET last_validation_reason = ? WHERE id = ?`,
			val.Reason, entry.ID); err != nil {
			return Entry{}, err
		}
	}

	if err := p.decide(ctx, id, Accepted, "", entry.ID); err != nil {
		return Entry{}, err
	}
	return v.Get(ctx, entry.ID)
}

func (p *proposals) Dismiss(ctx context.Context, id, reason string) error {
	prop, err := p.Get(ctx, id)
	if err != nil {
		return err
	}
	if !prop.Open() {
		return fmt.Errorf("%w: %s", ErrDecided, prop.Decision)
	}
	return p.decide(ctx, id, Dismissed, reason, "")
}

// decide answers the question and destroys the candidate either way. An
// accepted secret is in the credential table by now and a dismissed one was
// never ours; keeping ciphertext for either is a liability with no reader.
func (p *proposals) decide(ctx context.Context, id string, d Decision, reason, credential string) error {
	v := p.v()
	res, err := v.db.SQL().ExecContext(ctx, `
		UPDATE credential_proposal
		SET decision = ?, decided_at = ?, ciphertext = NULL, nonce = NULL
		WHERE id = ? AND decided_at IS NULL`,
		string(d), v.clock().UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: proposal %s", ErrNotFound, id)
	}
	if reason != "" {
		if err := v.db.SetMeta(ctx, reasonKey(id), reason); err != nil {
			return err
		}
	}
	if credential != "" {
		if err := v.db.SetMeta(ctx, credentialKey(id), credential); err != nil {
			return err
		}
	}
	return nil
}

// reveal recovers a candidate. It is the one path in this file that produces
// plaintext, and it is unexported for the same reason [Vault.Reveal] is named
// the way it is.
func (p *proposals) reveal(ctx context.Context, id string) (Proposal, string, error) {
	v := p.v()
	var ciphertext, nonce []byte
	var decided *int64
	err := v.db.SQL().QueryRowContext(ctx,
		`SELECT ciphertext, nonce, decided_at FROM credential_proposal WHERE id = ?`, id).
		Scan(&ciphertext, &nonce, &decided)
	if errors.Is(err, sql.ErrNoRows) {
		return Proposal{}, "", fmt.Errorf("%w: proposal %s", ErrNotFound, id)
	}
	if err != nil {
		return Proposal{}, "", err
	}
	prop, err := p.Get(ctx, id)
	if err != nil {
		return Proposal{}, "", err
	}
	if decided != nil {
		return prop, "", fmt.Errorf("%w: %s", ErrDecided, prop.Decision)
	}
	secret, err := v.open(ciphertext, nonce)
	if err != nil {
		return prop, "", err
	}
	return prop, secret, nil
}

// detectorWithTier writes the detector column in the one format the console
// parses.
//
// internal/index already writes `stripe_secret (tier1)` into secret_marker, and
// console/src/screens/credentials.ts reads the tier back out of exactly that
// shape. This package used to store the bare rule id, which nothing had ever
// noticed because no proposal had ever reached a screen: the first real one
// would have rendered as "unknown" tier and demanded the typed danger
// confirmation reserved for tier-2 shape matches — for a tier-1 hit, which by
// [ErrNotProposable] is the only thing that can be in this queue. Two writers,
// one format, and the reader parses the other one's.
//
// It is done here rather than in [FromFinding] so a hand-built candidate — the
// installer's config discovery, say — gets the same string.
func detectorWithTier(detector string, tier index.Tier) string {
	if detector == "" || tier == 0 {
		return detector
	}
	return detector + " (" + tier.String() + ")"
}

func labelKey(id string) string      { return "proposal-label/" + id }
func reasonKey(id string) string     { return "proposal-reason/" + id }
func credentialKey(id string) string { return "proposal-credential/" + id }

const proposalCols = `id, service, detector, last_four, source_kind, source_runtime,
	source_session, source_path, source_at, shared_session, created_at, decided_at, decision`

func scanProposal(sc interface{ Scan(...any) error }) (Proposal, error) {
	var p Proposal
	var sourceKind, decision string
	var sourceAt, decidedAt *int64
	var shared, created int64
	err := sc.Scan(&p.ID, &p.Service, &p.Detector, &p.LastFour, &sourceKind,
		&p.Source.Runtime, &p.Source.Session, &p.Source.Path, &sourceAt, &shared,
		&created, &decidedAt, &decision)
	if err != nil {
		return Proposal{}, err
	}
	p.Source.Kind = SourceKind(sourceKind)
	p.Source.At = fromPtr(sourceAt)
	p.Source.SharedSession = shared != 0
	p.CreatedAt = time.UnixMilli(created).UTC()
	p.DecidedAt = fromPtr(decidedAt)
	p.Decision = Decision(decision)
	return p, nil
}
