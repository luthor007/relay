package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/luthor007/relay/relayd/internal/audit"
	"github.com/luthor007/relay/relayd/internal/vault"
)

// DASHBOARD.md §3.2 — the credential screen.
//
// The rule that shapes this whole file: **never display a secret after it is
// stored.** Last four characters and a re-validate button, because a UI that
// shows the key back to you is a UI that gets screenshotted into a support
// ticket. And on the self-hosted tier that is not just a display rule — it is
// the commercial claim in MEMORY.md §6 and CLOUD.md §1, which says keys never
// leave the machine *including through the UI*.
//
// So the rule is enforced twice, by types rather than by handlers:
//
//  1. [CredentialStore] is vault.Vault minus Reveal. The API cannot read a
//     plaintext secret because it does not hold anything that can. A future
//     endpoint in this package cannot leak one by forgetting, because there is
//     no method to forget to avoid.
//  2. [CredentialView] has no field a secret fits in, and it is built from
//     vault.Entry, which has none either. Every listing in this package goes
//     through it.

// CredentialStore is the vault as the console is allowed to see it: everything
// except the one method that returns a plaintext secret.
//
// vault.Vault satisfies this. The narrowing is the mechanism — an interface
// with no Reveal cannot be talked into revealing.
type CredentialStore interface {
	Put(ctx context.Context, in vault.Input) (vault.Entry, error)
	Get(ctx context.Context, id string) (vault.Entry, error)
	List(ctx context.Context) ([]vault.Entry, error)
	RecordValidation(ctx context.Context, id, reason string, at time.Time) error
	Revoke(ctx context.Context, id string) error
	Status() vault.Status
}

// The compiler checks the claim above rather than a comment making it.
var _ CredentialStore = (vault.Vault)(nil)

// CredentialView is one row of the credential screen.
//
// There is no secret here and there is nowhere to put one. The test in this
// package that builds a view from an entry whose secret is known, marshals it,
// and searches the JSON for that secret is what fails if that ever changes.
type CredentialView struct {
	ID      string `json:"id"`
	Service string `json:"service"`
	Label   string `json:"label,omitempty"`

	// LastFour is the display form, and the only form. It is empty for secrets
	// short enough that four characters would be most of them.
	LastFour string `json:"last_four"`

	// Backend is where the secret material actually is — the OS keychain, or
	// AES-GCM in the vault database. DASHBOARD.md §3.5 says it out loud rather
	// than implying a keychain that is not there.
	Backend string `json:"backend"`

	// Where it came from (MEMORY.md §6's three ways a key arrives).
	Source        string `json:"source"`
	SourceRuntime string `json:"source_runtime,omitempty"`
	SourceSession string `json:"source_session,omitempty"`
	SourcePath    string `json:"source_path,omitempty"`
	SourceAt      int64  `json:"source_at,omitempty"`
	// SharedSession marks a key found in a session that had another participant.
	// A key in your transcript may not be yours.
	SharedSession bool `json:"shared_session,omitempty"`

	CreatedAt int64 `json:"created_at"`
	// When it was last used, and by which runtime. DASHBOARD.md §3.4: access
	// nobody has touched in a month is the kind that gets forgotten and then
	// exploited.
	LastUsedAt int64  `json:"last_used_at,omitempty"`
	LastUsedBy string `json:"last_used_by,omitempty"`

	LastValidatedAt int64  `json:"last_validated_at,omitempty"`
	Validation      string `json:"validation,omitempty"`

	Revoked   bool  `json:"revoked"`
	RevokedAt int64 `json:"revoked_at,omitempty"`
}

// credentialView renders one entry. It takes a vault.Entry and nothing wider,
// so there is no path by which a secret reaches this function to be dropped.
func credentialView(e vault.Entry) CredentialView {
	return CredentialView{
		ID:              e.ID,
		Service:         e.Service,
		Label:           e.Label,
		LastFour:        e.LastFour,
		Backend:         string(e.Backend),
		Source:          string(e.Source.Kind),
		SourceRuntime:   e.Source.Runtime,
		SourceSession:   e.Source.Session,
		SourcePath:      e.Source.Path,
		SourceAt:        msOrZero(e.Source.At),
		SharedSession:   e.Source.SharedSession,
		CreatedAt:       msOrZero(e.CreatedAt),
		LastUsedAt:      msOrZero(e.LastUsedAt),
		LastUsedBy:      e.LastUsedBy,
		LastValidatedAt: msOrZero(e.LastValidatedAt),
		Validation:      e.LastValidationReason,
		Revoked:         e.Revoked(),
		RevokedAt:       msOrZero(e.RevokedAt),
	}
}

// CredentialList is the credential screen.
type CredentialList struct {
	Credentials []CredentialView `json:"credentials"`
	// Vault says where the secrets live and whether the keychain was available,
	// so the console can show honest degradation rather than implying protection
	// that is not there.
	Vault VaultStatus `json:"vault"`
	// Available is false when no vault is wired. The screen still renders — an
	// empty list with a reason beats a 404.
	Available bool   `json:"available"`
	Note      string `json:"note,omitempty"`
	At        int64  `json:"at"`
}

// VaultStatus mirrors vault.Status over the wire.
type VaultStatus struct {
	Backend   string `json:"backend,omitempty"`
	KeySource string `json:"key_source,omitempty"`
	Degraded  bool   `json:"degraded"`
	Reason    string `json:"reason,omitempty"`
}

func vaultStatus(v CredentialStore) VaultStatus {
	if v == nil {
		return VaultStatus{}
	}
	s := v.Status()
	return VaultStatus{
		Backend:   string(s.Backend),
		KeySource: string(s.KeySource),
		Degraded:  s.Degraded,
		Reason:    s.Reason,
	}
}

const noVaultNote = "no credential vault is open on this machine yet"

func (s *Server) handleCredentials(w http.ResponseWriter, r *http.Request) {
	out := CredentialList{
		Credentials: []CredentialView{},
		Vault:       vaultStatus(s.credentials),
		At:          s.now().UnixMilli(),
	}
	if s.credentials == nil {
		out.Note = noVaultNote
		writeJSON(w, http.StatusOK, out)
		return
	}
	out.Available = true

	entries, err := s.credentials.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeFailed, err.Error())
		return
	}
	for _, e := range entries {
		out.Credentials = append(out.Credentials, credentialView(e))
	}
	sortViews(out.Credentials)
	writeJSON(w, http.StatusOK, out)
}

// ------------------------------------------------------------------- add --

type addCredentialRequest struct {
	Service string `json:"service"`
	Label   string `json:"label,omitempty"`
	// Secret is inbound only. It is never echoed, never logged, and never
	// reachable again through this API.
	Secret string `json:"secret"`

	// Source describes where it came from. Typed is the clean path and the
	// default: MEMORY.md §6's first of three ways a key arrives.
	Source        string `json:"source,omitempty"`
	SourceRuntime string `json:"source_runtime,omitempty"`
	SourceSession string `json:"source_session,omitempty"`
	SourcePath    string `json:"source_path,omitempty"`
	SharedSession bool   `json:"shared_session,omitempty"`

	// Validate runs MEMORY.md §6's one real call before the response returns.
	Validate bool `json:"validate,omitempty"`
}

func (s *Server) handleAddCredential(w http.ResponseWriter, r *http.Request) {
	var req addCredentialRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadPayload, err.Error())
		return
	}
	if s.credentials == nil {
		writeErr(w, http.StatusServiceUnavailable, CodeUnavailable, noVaultNote)
		return
	}
	if req.Service == "" || req.Secret == "" {
		writeErr(w, http.StatusBadRequest, CodeBadPayload, "a credential needs a service and a secret")
		return
	}

	kind := vault.SourceKind(req.Source)
	switch kind {
	case vault.SourceTyped, vault.SourceConfig, vault.SourceTranscript:
	case "":
		kind = vault.SourceTyped
	default:
		writeErr(w, http.StatusBadRequest, CodeBadPayload,
			"source must be typed, config or transcript")
		return
	}

	id, _ := IdentityFrom(r.Context())
	var entry vault.Entry
	err := s.audited(r.Context(), auditRequest{
		kind: ConsoleCredential, action: audit.ActionCredentialAdd,
		identity: id, service: req.Service,
		detail: map[string]string{"source": string(kind)},
	}, func() (map[string]string, error) {
		var err error
		entry, err = s.credentials.Put(r.Context(), vault.Input{
			Service: req.Service,
			Label:   req.Label,
			Secret:  req.Secret,
			Source: vault.Provenance{
				Kind:          kind,
				Runtime:       req.SourceRuntime,
				Session:       req.SourceSession,
				Path:          req.SourcePath,
				At:            s.now(),
				SharedSession: req.SharedSession,
			},
		})
		if err != nil {
			return nil, err
		}
		// The id lands in the audit entry so a later revoke can be traced back to
		// the moment the key arrived. Last four, never more.
		return map[string]string{"credential": entry.ID, "last_four": entry.LastFour}, nil
	})
	if err != nil {
		s.writeVaultErr(w, err)
		return
	}

	out := map[string]any{"credential": credentialView(entry)}
	if req.Validate {
		out["validation"] = s.validate(r.Context(), id, entry)
	}
	writeJSON(w, http.StatusCreated, out)
}

// ------------------------------------------------------- validate, rotate --

// CredentialValidator makes MEMORY.md §6's one real call.
//
// It takes an entry rather than a secret, and resolves the secret itself out of
// the vault it owns, so that even the validation path does not hand plaintext
// through this package.
type CredentialValidator interface {
	Validate(ctx context.Context, e vault.Entry) (Validation, error)
}

// ValidatorFunc adapts a function to [CredentialValidator].
type ValidatorFunc func(ctx context.Context, e vault.Entry) (Validation, error)

// Validate implements [CredentialValidator].
func (f ValidatorFunc) Validate(ctx context.Context, e vault.Entry) (Validation, error) {
	return f(ctx, e)
}

// Validation is what one real call found out. Reason reuses llm.Reason's
// vocabulary — ok, missing_credential, expired, unresolved_ref, unavailable —
// so the console, the installer and the health screen speak one language.
type Validation struct {
	// Probed is false when nothing was tested. Reporting an untested credential
	// as ok is the same mistake as an adapter emitting an event it did not see.
	Probed bool   `json:"probed"`
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
	At     int64  `json:"at"`
}

const noValidatorNote = "no validator is wired, so this credential has not been tested from here"

func (s *Server) validate(ctx context.Context, id Identity, e vault.Entry) Validation {
	if s.validator == nil {
		return Validation{Probed: false, Detail: noValidatorNote, At: s.now().UnixMilli()}
	}
	v, err := s.validator.Validate(ctx, e)
	if err != nil {
		v = Validation{Probed: true, Reason: "unavailable", Detail: err.Error()}
	}
	if v.At == 0 {
		v.At = s.now().UnixMilli()
	}
	if v.Probed && s.credentials != nil {
		if err := s.credentials.RecordValidation(ctx, e.ID, v.Reason, time.UnixMilli(v.At)); err != nil {
			s.log.Warn("api: record validation", "credential", e.ID, "error", err)
		}
	}
	_ = id
	return v
}

func (s *Server) handleValidateCredential(w http.ResponseWriter, r *http.Request) {
	if s.credentials == nil {
		writeErr(w, http.StatusServiceUnavailable, CodeUnavailable, noVaultNote)
		return
	}
	credID := r.PathValue("id")
	id, _ := IdentityFrom(r.Context())

	var out Validation
	var entry vault.Entry
	err := s.audited(r.Context(), auditRequest{
		kind: ConsoleCredential, action: audit.ActionCredentialValidate,
		identity: id, target: credID,
	}, func() (map[string]string, error) {
		var err error
		entry, err = s.credentials.Get(r.Context(), credID)
		if err != nil {
			return nil, err
		}
		out = s.validate(r.Context(), id, entry)
		return map[string]string{"reason": out.Reason, "probed": boolWord(out.Probed)}, nil
	})
	if err != nil {
		s.writeVaultErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"credential": credentialView(mustGet(r.Context(), s.credentials, credID, entry)),
		"validation": out,
	})
}

type rotateRequest struct {
	Secret string `json:"secret"`
	Label  string `json:"label,omitempty"`
}

// handleRotateCredential replaces the secret behind an existing id.
//
// Rotation keeps the id rather than adding a row, on purpose: MEMORY.md §6's
// "newest validated wins, and provenance is kept" is about telling two Stripe
// keys apart, and a config that says vault:<id> must not need editing because
// somebody rotated the key it points at.
func (s *Server) handleRotateCredential(w http.ResponseWriter, r *http.Request) {
	var req rotateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadPayload, err.Error())
		return
	}
	if s.credentials == nil {
		writeErr(w, http.StatusServiceUnavailable, CodeUnavailable, noVaultNote)
		return
	}
	if req.Secret == "" {
		writeErr(w, http.StatusBadRequest, CodeBadPayload, "rotation needs the new secret")
		return
	}

	credID := r.PathValue("id")
	id, _ := IdentityFrom(r.Context())

	var entry vault.Entry
	err := s.audited(r.Context(), auditRequest{
		kind: ConsoleCredential, action: audit.ActionCredentialRotate,
		identity: id, target: credID,
	}, func() (map[string]string, error) {
		prev, err := s.credentials.Get(r.Context(), credID)
		if err != nil {
			return nil, err
		}
		label := req.Label
		if label == "" {
			label = prev.Label
		}
		entry, err = s.credentials.Put(r.Context(), vault.Input{
			ID:      prev.ID,
			Service: prev.Service,
			Label:   label,
			Secret:  req.Secret,
			Source: vault.Provenance{
				// A rotation is a typed key however the old one arrived. Carrying
				// the old provenance forward would claim this one came out of a
				// March transcript, which is exactly the lie provenance exists to
				// prevent.
				Kind: vault.SourceTyped,
				At:   s.now(),
			},
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{
			"service": entry.Service, "was": prev.LastFour, "now": entry.LastFour,
		}, nil
	})
	if err != nil {
		s.writeVaultErr(w, err)
		return
	}
	out := map[string]any{"credential": credentialView(entry)}
	out["validation"] = s.validate(r.Context(), id, entry)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRevokeCredential(w http.ResponseWriter, r *http.Request) {
	if s.credentials == nil {
		writeErr(w, http.StatusServiceUnavailable, CodeUnavailable, noVaultNote)
		return
	}
	credID := r.PathValue("id")
	id, _ := IdentityFrom(r.Context())

	err := s.audited(r.Context(), auditRequest{
		kind: ConsoleCredential, action: audit.ActionCredentialRevoke,
		identity: id, target: credID,
	}, func() (map[string]string, error) {
		prev, err := s.credentials.Get(r.Context(), credID)
		if err != nil {
			return nil, err
		}
		if err := s.credentials.Revoke(r.Context(), credID); err != nil {
			return nil, err
		}
		return map[string]string{"service": prev.Service, "last_four": prev.LastFour}, nil
	})
	if err != nil {
		s.writeVaultErr(w, err)
		return
	}
	// The row stays and says it was revoked, and when. A vanished credential is
	// a question nobody can answer later.
	e, err := s.credentials.Get(r.Context(), credID)
	if err != nil {
		s.writeVaultErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"credential": credentialView(e)})
}

// --------------------------------------------------------------- proposals --

// Proposal is MEMORY.md §6's "I found what looks like a Twilio auth token in a
// session from March. Save it as your Twilio credential?"
//
// It lives on the credential screen because that flow needs somewhere to be
// accepted or dismissed that is not a voice prompt at 2 a.m. (DASHBOARD.md
// §3.2).
type Proposal struct {
	ID      string `json:"id"`
	Service string `json:"service"`
	// Detector names the rule and its tier. MEMORY.md §12.2 measured a 26%
	// false-positive rate on tier 2, so the console shows which tier found it
	// rather than presenting every hit as equally likely.
	Detector string `json:"detector,omitempty"`

	Runtime    string `json:"runtime,omitempty"`
	Session    string `json:"session,omitempty"`
	Path       string `json:"path,omitempty"`
	ByteOffset int64  `json:"byte_offset,omitempty"`

	// LastFour is as much of the candidate as anything ever shows.
	LastFour string `json:"last_four,omitempty"`
	// SharedSession says the session had another participant, so the key may not
	// be the user's to keep. The proposal has to say so.
	SharedSession bool  `json:"shared_session,omitempty"`
	FoundAt       int64 `json:"found_at,omitempty"`
}

// ProposalStore is the queue behind the proposal list. Accepting one has to
// re-read the transcript at its byte offset to recover the candidate, which is
// the index's job and not this package's.
type ProposalStore interface {
	List(ctx context.Context) ([]Proposal, error)
	Accept(ctx context.Context, id, label string) (vault.Entry, error)
	Dismiss(ctx context.Context, id, reason string) error
}

func (s *Server) handleProposals(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"proposals": []Proposal{}, "at": s.now().UnixMilli()}

	if s.proposals != nil {
		list, err := s.proposals.List(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, CodeFailed, err.Error())
			return
		}
		if list == nil {
			list = []Proposal{}
		}
		out["proposals"] = list
		writeJSON(w, http.StatusOK, out)
		return
	}

	// No queue wired: fall back to the index's own secret markers, which are the
	// raw material a proposal is made of. It is a real list rather than an empty
	// one, and the console can say "found, not yet offered".
	list, note, err := s.markerProposals(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeFailed, err.Error())
		return
	}
	out["proposals"] = list
	if note != "" {
		out["note"] = note
	}
	writeJSON(w, http.StatusOK, out)
}

type proposalRequest struct {
	Label  string `json:"label,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func (s *Server) handleAcceptProposal(w http.ResponseWriter, r *http.Request) {
	var req proposalRequest
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req)

	id, _ := IdentityFrom(r.Context())
	propID := r.PathValue("id")

	if s.proposals == nil {
		// The attempt is still recorded. "Somebody tried to accept a proposal on
		// a box with no proposal queue" is exactly the kind of thing the log is
		// for, and dropping it because the feature is unbuilt would be a hole.
		s.recordRefusal(r.Context(), id, audit.ActionProposalAccept, propID,
			"no proposal queue is wired on this machine")
		writeErr(w, http.StatusNotImplemented, CodeNotImplemented,
			"accepting a proposal has to re-read the transcript at its byte offset; "+
				"that is the index's job and it is not wired here yet")
		return
	}

	var entry vault.Entry
	err := s.audited(r.Context(), auditRequest{
		kind: ConsoleCredential, action: audit.ActionProposalAccept,
		identity: id, target: propID,
	}, func() (map[string]string, error) {
		var err error
		entry, err = s.proposals.Accept(r.Context(), propID, req.Label)
		if err != nil {
			return nil, err
		}
		return map[string]string{"credential": entry.ID, "service": entry.Service}, nil
	})
	if err != nil {
		s.writeVaultErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"credential": credentialView(entry)})
}

func (s *Server) handleDismissProposal(w http.ResponseWriter, r *http.Request) {
	var req proposalRequest
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req)

	id, _ := IdentityFrom(r.Context())
	propID := r.PathValue("id")

	if s.proposals == nil {
		s.recordRefusal(r.Context(), id, audit.ActionProposalDismiss, propID,
			"no proposal queue is wired on this machine")
		writeErr(w, http.StatusNotImplemented, CodeNotImplemented,
			"there is no proposal queue on this machine yet")
		return
	}
	err := s.audited(r.Context(), auditRequest{
		kind: ConsoleCredential, action: audit.ActionProposalDismiss,
		identity: id, target: propID,
	}, func() (map[string]string, error) {
		if err := s.proposals.Dismiss(r.Context(), propID, req.Reason); err != nil {
			return nil, err
		}
		return map[string]string{"reason": req.Reason}, nil
	})
	if err != nil {
		s.writeVaultErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- helpers --

// maxBodyBytes caps a console request body. Every payload here is a handful of
// short strings; a megabyte is three orders of magnitude of headroom and stops
// an unbounded read.
const maxBodyBytes = 1 << 20

type auditRequest struct {
	kind     string
	action   audit.Action
	identity Identity
	target   string
	service  string
	detail   map[string]string
}

func beginAudit(ctx context.Context, s *Server, e auditRequest) (*audit.Attempt, error) {
	return audit.Begin(ctx, s.audit, audit.Entry{
		Actor:   e.identity.Actor(),
		Action:  e.action,
		Target:  e.target,
		Service: e.service,
		Detail:  e.detail,
	})
}

// recordRefusal writes an attempt and its denial for a mutation the server
// would not perform. The attempt is the point.
func (s *Server) recordRefusal(ctx context.Context, id Identity, action audit.Action, target, reason string) {
	a, err := audit.Begin(ctx, s.audit, audit.Entry{
		Actor: id.Actor(), Action: action, Target: target,
	})
	if err != nil {
		s.log.Error("api: could not record a refused mutation", "action", action, "error", err)
		return
	}
	_ = a.Deny(ctx, reason)
}

// writeVaultErr maps a vault or audit failure onto a status the console can act
// on.
func (s *Server) writeVaultErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, vault.ErrNotFound):
		writeErr(w, http.StatusNotFound, CodeNotFound, err.Error())
	case errors.Is(err, vault.ErrRevoked):
		writeErr(w, http.StatusConflict, CodeConflict, err.Error())
	case errors.Is(err, vault.ErrLocked):
		writeErr(w, http.StatusServiceUnavailable, CodeUnavailable, err.Error())
	case errors.Is(err, audit.ErrNoLog):
		// A vault write with nowhere to record it is not a vault write we make.
		writeErr(w, http.StatusServiceUnavailable, CodeUnavailable,
			"this mutation was refused because it could not be written to the audit log")
	default:
		writeErr(w, http.StatusInternalServerError, CodeFailed, err.Error())
	}
}

func mustGet(ctx context.Context, v CredentialStore, id string, fallback vault.Entry) vault.Entry {
	if v == nil {
		return fallback
	}
	e, err := v.Get(ctx, id)
	if err != nil {
		return fallback
	}
	return e
}

func msOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func boolWord(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// sortViews keeps the list stable: live credentials by service, revoked last.
func sortViews(v []CredentialView) {
	sort.SliceStable(v, func(i, j int) bool {
		if v[i].Revoked != v[j].Revoked {
			return !v[i].Revoked
		}
		if v[i].Service != v[j].Service {
			return v[i].Service < v[j].Service
		}
		return v[i].CreatedAt > v[j].CreatedAt
	})
}
