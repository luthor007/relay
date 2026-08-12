package api

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// The cloud tier's authenticator: a Supabase session, verified on the box.
//
// CONTROL-PLANE.md §3 settles who checks the token, and it is this side. The
// console is a browser, Supabase signs the user in, and the console then talks
// to *the box* through the rendezvous relay. Terminating the session in a
// control plane and forwarding an identity header would put our infrastructure
// in the data path of every console request, which is exactly what CLOUD.md §4's
// "a breach of our infrastructure does not expose anyone's recorded life"
// forbids. The relay already cannot read the traffic; a proxy that could would
// undo it.
//
// # The two rules that make hand-written JWT verification safe
//
// There is no JWT dependency in this module, so this is written against stdlib
// crypto. That is defensible for this shape and would not be for a novel
// protocol — compare `pairing.ts`, which refuses to implement a PAKE and injects
// one. Verifying a JWT is base64url, JSON, a canonical signing input and a
// stdlib signature check. What has historically gone wrong is never the
// cryptography; it is two structural mistakes, and both are closed here by
// construction rather than by a check somebody could delete:
//
//  1. **The algorithm comes from the key, never from the token.** [verify]
//     switches on the type of the key it fetched and ignores `alg` entirely
//     except to reject a token whose header disagrees. `alg: none` and the
//     RS256→HS256 confusion attack — sign with the RSA *public* key as an HMAC
//     secret, because the verifier trusted the header — are both unreachable:
//     there is no branch in this file that treats a public key as a secret.
//  2. **No claim is optional.** Issuer, audience, subject and expiry are all
//     required and all compared. A verifier that skips a missing claim accepts a
//     token from another Supabase project, or from this project with no account
//     bound to it.
//
// # Why the subject check is the one that matters most
//
// Signature, issuer and expiry establish that Supabase minted this token for
// somebody. Only [SupabaseOptions.AccountID] establishes that the somebody owns
// *this* box. Without it, every authenticated user of our Supabase project could
// open every customer's machine, and the whole system reduces to "knowing a
// hostname". It is written into the box's config at provisioning and is not
// discoverable from the token.

// KeySource returns the public key a token was signed with.
//
// An interface because the production implementation fetches and caches
// Supabase's JWKS over the network, and every test in this file needs to mint
// tokens against a key it holds. It takes the `kid` because a project rotates
// keys and both are live during a rotation.
type KeySource interface {
	Key(kid string) (crypto.PublicKey, error)
}

// KeySourceFunc adapts a function to [KeySource].
type KeySourceFunc func(kid string) (crypto.PublicKey, error)

// Key implements [KeySource].
func (f KeySourceFunc) Key(kid string) (crypto.PublicKey, error) { return f(kid) }

// SupabaseOptions configures the cloud authenticator.
type SupabaseOptions struct {
	// Issuer is the project's token issuer, `https://<ref>.supabase.co/auth/v1`.
	// Required: a valid signature over the wrong project is still the wrong
	// project.
	Issuer string
	// Audience is Supabase's, which is "authenticated" for a signed-in user.
	// Empty defaults to that.
	Audience string
	// AccountID is the Supabase user id this box belongs to. Required. See the
	// file comment.
	AccountID string
	// Keys resolves signing keys.
	Keys KeySource
	// Leeway forgives clock skew on expiry. Small on purpose: DASHBOARD.md §4
	// says cloud sessions expire and means it.
	Leeway time.Duration
	Now    func() time.Time
}

// DefaultLeeway is the clock skew allowed on `exp`.
const DefaultLeeway = 30 * time.Second

// Errors a caller may want to distinguish. Everything else collapses to
// [ErrUnauthenticated] on purpose: a verifier that explains precisely which
// check a token failed is a probe of what a valid one would look like.
var (
	// ErrWrongAccount is a token that is valid and belongs to somebody else.
	// Distinct because it is the one failure that is not a bug in the client and
	// not an attack in progress — it is what a user sees if they are signed into
	// the wrong account, and the console can say so.
	ErrWrongAccount = errors.New("api: this session belongs to a different account")
	// ErrNoKeys is a box that cannot reach Supabase's JWKS with a cold cache. It
	// fails closed: a degraded mode that widens access is not a degraded mode.
	ErrNoKeys = errors.New("api: cannot verify sessions right now")
)

type supabaseAuth struct {
	opts SupabaseOptions
}

// NewSupabaseAuthenticator builds the cloud tier's [Authenticator].
func NewSupabaseAuthenticator(o SupabaseOptions) (Authenticator, error) {
	if strings.TrimSpace(o.Issuer) == "" {
		return nil, errors.New("api: supabase issuer is required")
	}
	if strings.TrimSpace(o.AccountID) == "" {
		// Refused at construction rather than defaulted to "any". A box with no
		// account bound to it that still accepted tokens would be open to every
		// user of the project, and the failure would be invisible.
		return nil, errors.New("api: no account is bound to this box, so every signed-in user would reach it")
	}
	if o.Keys == nil {
		return nil, errors.New("api: no key source")
	}
	if o.Audience == "" {
		o.Audience = "authenticated"
	}
	if o.Leeway <= 0 {
		o.Leeway = DefaultLeeway
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &supabaseAuth{opts: o}, nil
}

func (a *supabaseAuth) Authenticate(r *http.Request) (Identity, error) {
	// Header only. The self-hosted authenticator also accepts `?token=` because
	// the user pastes a printed token into a browser once; a session token in a
	// query string ends up in proxy logs and crash reports, and this one is a
	// live credential for somebody's recorded life.
	raw := bearer(r)
	if raw == "" {
		return Identity{}, ErrUnauthenticated
	}

	claims, err := a.verify(raw)
	if err != nil {
		return Identity{}, err
	}

	return Identity{
		Kind:    "account",
		Subject: claims.Subject,
		// Every scope. The cloud tier has one account per box — SYSTEM.md §5 has
		// no user table, one box, one person — so there is no narrower set to
		// grant. What guards the vault is freshness, below, not a scope the
		// account does not hold.
		Scopes: AllScopes(),
		// When the person last actually proved who they are, which is not when
		// this token was issued: Supabase refreshes access tokens roughly hourly
		// without the user doing anything, so `iat` would make
		// DASHBOARD.md §4's "every vault write re-authenticated regardless of
		// session age" a check that always passes. `amr` carries a timestamp per
		// authentication method, and the most recent of those is the real answer.
		AuthAt: claims.authenticatedAt(),
		// A session token, not a per-request credential: it ages, which is the
		// whole point of the re-authentication window.
		PerRequest: false,
		// §3 item 5. Read straight from the claim rather than derived: `amr` can
		// list a TOTP method on a token Supabase still minted at aal1, and the
		// assurance level is the answer Supabase itself reached.
		Assurance: claims.AAL,
		Cloud:     true,
		From:      r.RemoteAddr,
		Agent:     r.UserAgent(),
	}, nil
}

// claims is the subset of a Supabase token this cares about.
type claims struct {
	Issuer   string   `json:"iss"`
	Subject  string   `json:"sub"`
	Audience audience `json:"aud"`
	Expiry   int64    `json:"exp"`
	IssuedAt int64    `json:"iat"`
	// AAL is Supabase's assurance level: "aal1" for a password, "aal2" once a
	// second factor has been presented.
	AAL string `json:"aal"`
	// AMR is the per-method record, with when each was satisfied.
	AMR []struct {
		Method    string `json:"method"`
		Timestamp int64  `json:"timestamp"`
	} `json:"amr"`
}

// authenticatedAt is the most recent moment the person proved who they are.
func (c claims) authenticatedAt() time.Time {
	var newest int64
	for _, m := range c.AMR {
		if m.Timestamp > newest {
			newest = m.Timestamp
		}
	}
	if newest == 0 {
		// No `amr` means an older Supabase or a token shape we do not know. Fall
		// back to issuance, which is the *most generous* reading — so this is
		// stated rather than silent: on such a token the vault window is weaker
		// than §4 intends.
		newest = c.IssuedAt
	}
	if newest == 0 {
		return time.Time{}
	}
	return time.Unix(newest, 0)
}

// audience is a claim that is a string in some tokens and an array in others.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = []string{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("aud is neither a string nor an array")
	}
	*a = many
	return nil
}

func (a audience) has(want string) bool {
	for _, got := range a {
		if got == want {
			return true
		}
	}
	return false
}

// verify checks the signature and every claim.
func (a *supabaseAuth) verify(token string) (claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims{}, ErrUnauthenticated
	}

	headerBytes, err := b64(parts[0])
	if err != nil {
		return claims{}, ErrUnauthenticated
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return claims{}, ErrUnauthenticated
	}

	key, err := a.opts.Keys.Key(header.Kid)
	if err != nil {
		if errors.Is(err, ErrNoKeys) {
			return claims{}, ErrNoKeys
		}
		return claims{}, ErrUnauthenticated
	}

	sig, err := b64(parts[2])
	if err != nil {
		return claims{}, ErrUnauthenticated
	}
	signed := []byte(parts[0] + "." + parts[1])

	// The algorithm is decided by the key, and the header is only ever consulted
	// to *disagree*. This is the whole defence against alg confusion: there is no
	// path where a public key is used as an HMAC secret, because no branch here
	// does HMAC at all.
	if err := verifySignature(key, header.Alg, signed, sig); err != nil {
		return claims{}, ErrUnauthenticated
	}

	payload, err := b64(parts[1])
	if err != nil {
		return claims{}, ErrUnauthenticated
	}
	var c claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return claims{}, ErrUnauthenticated
	}

	now := a.opts.Now()
	switch {
	case c.Issuer != a.opts.Issuer:
		return claims{}, ErrUnauthenticated
	case !c.Audience.has(a.opts.Audience):
		return claims{}, ErrUnauthenticated
	case c.Expiry == 0 || now.After(time.Unix(c.Expiry, 0).Add(a.opts.Leeway)):
		return claims{}, ErrUnauthenticated
	case c.Subject == "":
		return claims{}, ErrUnauthenticated
	case c.Subject != a.opts.AccountID:
		// The one failure worth naming: a real session for the wrong box.
		return claims{}, ErrWrongAccount
	}
	return c, nil
}

// verifySignature checks sig over signed, with the algorithm the key implies.
func verifySignature(key crypto.PublicKey, headerAlg string, signed, sig []byte) error {
	sum := sha256.Sum256(signed)
	switch k := key.(type) {
	case *ecdsa.PublicKey:
		if headerAlg != "ES256" {
			return fmt.Errorf("api: key is ES256 and the token says %q", headerAlg)
		}
		// JWS packs ECDSA as r||s, fixed width, not ASN.1 — a verifier that
		// hands this to VerifyASN1 rejects every valid token, which is a bug
		// that presents as "nobody can sign in".
		if len(sig) != 64 {
			return errors.New("api: ES256 signature is not 64 bytes")
		}
		r := new(big.Int).SetBytes(sig[:32])
		s := new(big.Int).SetBytes(sig[32:])
		if !ecdsa.Verify(k, sum[:], r, s) {
			return errors.New("api: bad signature")
		}
		return nil

	case *rsa.PublicKey:
		if headerAlg != "RS256" {
			return fmt.Errorf("api: key is RS256 and the token says %q", headerAlg)
		}
		return rsa.VerifyPKCS1v15(k, crypto.SHA256, sum[:], sig)
	}
	// Deliberately no default that tries something. An unknown key type is a
	// verifier that does not know what it is doing, and the safe answer is no.
	return errors.New("api: unsupported key type")
}

func b64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
