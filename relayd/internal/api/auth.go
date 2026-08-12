package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/audit"
)

// One API for both deployments (DASHBOARD.md §5), so Relay Cloud is a proxy
// plus an auth layer rather than a second backend. That only stays true if the
// *authorization* decisions do not move with it.
//
// So the split here is deliberate and it is the whole point of this file:
//
//   - **Authentication is pluggable.** [Authenticator] turns a request into an
//     [Identity]. Self-hosted checks the token relayd printed on start; the
//     cloud host checks an account session. Two implementations, one interface.
//   - **Authorization is not.** [Server.guard] is the single chokepoint that
//     decides what an identity may do, and every route goes through it. There
//     is no second place to add a check and no way for the two deployments to
//     drift, which is the reason DASHBOARD.md §5 gives for having one API at
//     all.

// Scope is what a request is allowed to do. Three, because the console has
// three levels of consequence and collapsing them would mean a read-only
// dashboard token could rotate a Stripe key.
type Scope string

const (
	// ScopeRead is every listing and every stream.
	ScopeRead Scope = "read"
	// ScopeWrite drives sessions and edits facts: consequential, reversible,
	// and confined to this machine's own state.
	ScopeWrite Scope = "write"
	// ScopeVault is credential and connector mutation. DASHBOARD.md §4: the
	// console can write to the vault, and that makes it the highest-value target
	// in the system, above the glasses and above relayd's own API.
	ScopeVault Scope = "vault"
)

// AllScopes is what the printed token carries: on the self-hosted tier the
// person holding it is the person who ran the installer.
func AllScopes() []Scope { return []Scope{ScopeRead, ScopeWrite, ScopeVault} }

// Assurance levels, named for Supabase's `aal` claim because that is where they
// come from and a translation layer would only be a second vocabulary to keep
// in step.
const (
	// AssuranceSingleFactor is a password, and nothing else.
	AssuranceSingleFactor = "aal1"
	// AssuranceSecondFactor is a password plus a second factor.
	AssuranceSecondFactor = "aal2"
)

// Identity is who is making a request, after authentication.
type Identity struct {
	// Kind is token | account | proxy — the door, not the person. SYSTEM.md §5
	// has no user table: one box, one person.
	Kind string
	// Subject is the account on the cloud tier and "local" behind the token.
	Subject string
	Scopes  []Scope

	// AuthAt is when this identity last proved itself. For a cloud session that
	// is when the account last authenticated, and DASHBOARD.md §4 requires every
	// vault write to be re-authenticated regardless of session age.
	AuthAt time.Time

	// PerRequest means the credential was presented with *this* request, so the
	// identity is fresh by construction and the re-authentication window does
	// not apply. The printed token is the case: it is sent on every call, so
	// there is no session to age. A cookie or a bearer session token is not,
	// however recently it was issued — that is the distinction the field exists
	// to make explicit rather than inferring it from Kind.
	PerRequest bool

	// Cloud marks the hosted deployment, which is what turns on session expiry,
	// vault re-authentication and the billing route.
	Cloud bool

	// Assurance is how strongly this identity was proved: [AssuranceSecondFactor]
	// when a second factor was presented, [AssuranceSingleFactor] when only a
	// password was.
	//
	// CONTROL-PLANE.md §3 item 5 requires the strong one for vault writes, and it
	// is a separate fact from AuthAt on purpose: a password typed one second ago
	// is fresh and is still one factor. Empty on a per-request credential, where
	// the question does not arise — see the guard.
	Assurance string

	// From is the origin as the server saw it, and Agent the trimmed
	// user-agent. Both end up in the audit log's "from where".
	From  string
	Agent string
}

// Can reports whether this identity holds a scope.
func (i Identity) Can(s Scope) bool {
	for _, have := range i.Scopes {
		if have == s {
			return true
		}
	}
	return false
}

// Actor is this identity as the audit log records it.
func (i Identity) Actor() audit.Actor {
	kind := "console"
	if i.Cloud {
		kind = "cloud"
	}
	return audit.Actor{Kind: kind, ID: i.Subject, From: i.From, Agent: i.Agent}
}

// Authenticator turns a request into an [Identity]. It answers "who is this",
// never "may they do this" — that decision stays in [Server.guard] so both
// deployments make it in the same place.
type Authenticator interface {
	Authenticate(r *http.Request) (Identity, error)
}

// AuthenticatorFunc adapts a function to [Authenticator].
type AuthenticatorFunc func(r *http.Request) (Identity, error)

// Authenticate implements [Authenticator].
func (f AuthenticatorFunc) Authenticate(r *http.Request) (Identity, error) { return f(r) }

// ErrUnauthenticated is any request that did not prove who it is.
var ErrUnauthenticated = errors.New("api: no valid credential")

// tokenAuth is the self-hosted authenticator: the token relayd prints on start,
// same pattern as the pairing code.
type tokenAuth struct {
	token string
	now   func() time.Time
}

func (t tokenAuth) Authenticate(r *http.Request) (Identity, error) {
	got := bearer(r)
	if got == "" {
		got = r.URL.Query().Get("token")
	}
	// Constant time, always, and on both the empty and the wrong case: a compare
	// that returns early on the first differing byte leaks the token one byte at
	// a time to anything that can measure a loopback round trip.
	if subtle.ConstantTimeCompare([]byte(got), []byte(t.token)) != 1 {
		return Identity{}, ErrUnauthenticated
	}
	return Identity{
		Kind:    "token",
		Subject: "local",
		Scopes:  AllScopes(),
		// The credential is presented on every request, so there is no session
		// to age and nothing to re-authenticate against.
		AuthAt:     t.now(),
		PerRequest: true,
	}, nil
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	v, ok := strings.CutPrefix(h, "Bearer ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}

// ---------------------------------------------------------------- context --

type identityKey struct{}

// IdentityFrom returns the authenticated identity for a request.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	v, ok := ctx.Value(identityKey{}).(Identity)
	return v, ok
}

// WithIdentity attaches an identity, for a cloud host that authenticates in its
// own middleware and hands the request on.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// ------------------------------------------------------------------ guard --

// guard is the one authorization chokepoint. Every authenticated route goes
// through it; nothing decides access anywhere else.
func (s *Server) guard(scope Scope, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := s.authn.Authenticate(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="relayd"`)
			writeErr(w, http.StatusUnauthorized, CodeUnauthorized,
				"relayd prints a token on start; pass it as a bearer token")
			return
		}
		id.From = s.origin(r)
		id.Agent = trimAgent(r.UserAgent())

		if !id.Can(scope) {
			s.denied(r.Context(), id, scope, "this credential does not carry the "+string(scope)+" scope")
			writeErr(w, http.StatusForbidden, CodeForbidden,
				"this credential may read but not "+string(scope))
			return
		}

		// DASHBOARD.md §4, cloud: every vault write is re-authenticated
		// regardless of session age. Self-hosted identities are always fresh
		// because the token is presented on every request, so this costs the free
		// tier nothing and cannot be forgotten on the paid one.
		if scope == ScopeVault && s.vaultReauth > 0 && !id.PerRequest {
			// Two conditions, and they are not the same one twice.
			//
			// Freshness answers "was this person here recently". Assurance
			// answers "how well do we know it is them". A password typed one
			// second ago passes the first and is still one factor, which is
			// exactly the case CONTROL-PLANE.md §3 item 5 exists to refuse — the
			// vault is the highest-value target in the system, and a stolen
			// password should not reach it.
			//
			// Both refusals carry the same code, deliberately. The box cannot
			// tell them apart usefully for the user: whether the answer is "type
			// your code" or "you have no second factor yet, add one" depends on
			// what is enrolled, which lives in the account service and not in
			// this token. The console asks, because the console can.
			if id.Assurance == AssuranceSingleFactor {
				s.denied(r.Context(), id, scope, "the session was authenticated with one factor")
				w.Header().Set("WWW-Authenticate", `Bearer realm="relayd", error="reauthenticate"`)
				writeErr(w, http.StatusUnauthorized, CodeReauthenticate,
					"a vault write needs a second factor, not just a password")
				return
			}
			if id.AuthAt.IsZero() || s.now().Sub(id.AuthAt) > s.vaultReauth {
				s.denied(r.Context(), id, scope, "the session is older than the vault re-authentication window")
				w.Header().Set("WWW-Authenticate", `Bearer realm="relayd", error="reauthenticate"`)
				writeErr(w, http.StatusUnauthorized, CodeReauthenticate,
					"a vault write needs a fresh authentication, whatever the session's age")
				return
			}
		}

		next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), id)))
	})
}

// denied records a refused vault or connector mutation. A log that only holds
// what was allowed cannot answer "did anything try", which is the question this
// log exists for.
func (s *Server) denied(ctx context.Context, id Identity, scope Scope, reason string) {
	if scope != ScopeVault || s.audit == nil {
		return
	}
	a, err := audit.Begin(ctx, s.audit, audit.Entry{
		Actor: id.Actor(), Action: audit.ActionCredentialRead,
		Detail: map[string]string{"scope": string(scope)},
	})
	if err != nil {
		s.log.Error("api: could not record a denied vault request", "error", err)
		return
	}
	_ = a.Deny(ctx, reason)
}

// origin is the request's source address as this server saw it.
//
// X-Forwarded-For is read only when the deployment says a proxy is in front,
// because on a loopback bind anyone who can reach the port can also set that
// header, and an audit log that records an attacker's chosen address is worse
// than one that records none.
func (s *Server) origin(r *http.Request) string {
	if s.trustForwarded {
		if h := r.Header.Get("X-Forwarded-For"); h != "" {
			if first, _, ok := strings.Cut(h, ","); ok {
				return strings.TrimSpace(first)
			}
			return strings.TrimSpace(h)
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func trimAgent(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}

// ------------------------------------------------------------------ bind --

// BindNotice is what relayd prints on start, in both cases. It exists here
// rather than in the daemon so the sentence and the check that produces it stay
// in one file.
func BindNotice(listen string, lan bool, token string) string {
	if lan {
		return fmt.Sprintf("%s\nConsole: http://%s/?token=%s", LANWarning(listen), listen, token)
	}
	return fmt.Sprintf("Console: http://%s/?token=%s\n"+
		"This address is reachable from this machine only. Pass --lan to change that, on purpose.",
		listen, token)
}
