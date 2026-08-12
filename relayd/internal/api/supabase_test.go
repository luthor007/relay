package api_test

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/api"
)

// The cloud tier's front door
//
// CONTROL-PLANE.md §3: the box verifies the Supabase token itself, so this file
// is the only thing standing between a stranger and a customer's recorded life.
// It is written against every failure that has historically got JWT verification
// wrong, because the point of hand-rolling this against stdlib crypto is that
// the failures are enumerable — and if they are not all here, the argument for
// not taking a dependency does not hold.

const (
	testIssuer  = "https://project-ref.supabase.co/auth/v1"
	testAccount = "11111111-2222-3333-4444-555555555555"
)

// signer mints tokens the way Supabase does.
type signer struct {
	key *ecdsa.PrivateKey
	kid string
}

func newSigner(t *testing.T) *signer {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &signer{key: k, kid: "test-key-1"}
}

func (s *signer) source() api.KeySource {
	return api.KeySourceFunc(func(kid string) (crypto.PublicKey, error) {
		if kid != s.kid {
			return nil, errors.New("no such key")
		}
		return &s.key.PublicKey, nil
	})
}

func seg(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

// token signs the given claims with ES256.
func (s *signer) token(t *testing.T, claims map[string]any) string {
	t.Helper()
	return s.tokenWithHeader(t, map[string]any{"alg": "ES256", "kid": s.kid, "typ": "JWT"}, claims)
}

func (s *signer) tokenWithHeader(t *testing.T, header, claims map[string]any) string {
	t.Helper()
	signed := seg(header) + "." + seg(claims)
	sum := sha256.Sum256([]byte(signed))
	r, sv, err := ecdsa.Sign(rand.Reader, s.key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	// r||s, fixed width — the JWS packing, not ASN.1.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	sv.FillBytes(sig[32:])
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func goodClaims(now time.Time) map[string]any {
	return map[string]any{
		"iss": testIssuer,
		"sub": testAccount,
		"aud": "authenticated",
		"exp": now.Add(time.Hour).Unix(),
		"iat": now.Unix(),
		"aal": "aal1",
		"amr": []map[string]any{{"method": "password", "timestamp": now.Add(-10 * time.Minute).Unix()}},
	}
}

func authWith(t *testing.T, keys api.KeySource, now time.Time) api.Authenticator {
	t.Helper()
	a, err := api.NewSupabaseAuthenticator(api.SupabaseOptions{
		Issuer:    testIssuer,
		AccountID: testAccount,
		Keys:      keys,
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func request(token string) *http.Request {
	r := httptest.NewRequest("GET", "/v1/sessions", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestAValidSupabaseSessionIsTheAccountThatOwnsTheBox(t *testing.T) {
	now := time.Now()
	s := newSigner(t)
	id, err := authWith(t, s.source(), now).Authenticate(request(s.token(t, goodClaims(now))))
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != testAccount {
		t.Errorf("subject = %q", id.Subject)
	}
	if !id.Cloud {
		t.Error("a Supabase session is not marked as the cloud tier, so session expiry and vault re-auth stay off")
	}
	if id.PerRequest {
		t.Error("a session token was marked per-request, which switches off the re-authentication window entirely")
	}
	for _, want := range api.AllScopes() {
		if !id.Can(want) {
			t.Errorf("the account cannot %v", want)
		}
	}
}

func TestTheAlgorithmComesFromTheKeyAndNeverFromTheToken(t *testing.T) {
	now := time.Now()
	s := newSigner(t)
	auth := authWith(t, s.source(), now)

	// `alg: none`, the oldest one. A verifier that reads the header decides
	// there is nothing to check.
	unsigned := seg(map[string]any{"alg": "none", "kid": s.kid}) + "." + seg(goodClaims(now)) + "."
	if _, err := auth.Authenticate(request(unsigned)); err == nil {
		t.Fatal("an unsigned token was accepted")
	}

	// Algorithm confusion: sign with HS256 using the *public* key as the secret.
	// Against a verifier that trusts the header, this is a forgery anybody can
	// produce from published JWKS material.
	pub, err := s.key.PublicKey.ECDH()
	if err != nil {
		t.Fatal(err)
	}
	header := seg(map[string]any{"alg": "HS256", "kid": s.kid})
	body := seg(goodClaims(now))
	mac := hmac.New(sha256.New, pub.Bytes())
	mac.Write([]byte(header + "." + body))
	forged := header + "." + body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if _, err := auth.Authenticate(request(forged)); err == nil {
		t.Fatal("an HS256 token signed with the public key was accepted")
	}
}

func TestATamperedPayloadIsRejected(t *testing.T) {
	now := time.Now()
	s := newSigner(t)
	valid := s.token(t, goodClaims(now))

	// Swap the body for one naming a different account, keeping the signature.
	other := goodClaims(now)
	other["sub"] = "99999999-9999-9999-9999-999999999999"
	head, _, sig := split3(t, valid)
	tampered := head + "." + seg(other) + "." + sig

	if _, err := authWith(t, s.source(), now).Authenticate(request(tampered)); err == nil {
		t.Fatal("a token whose payload was swapped was accepted")
	}
}

func TestATokenFromAnotherProjectIsRejected(t *testing.T) {
	// A valid signature over the wrong world. Without the issuer check, anybody
	// with any Supabase project could mint one — except they cannot sign with
	// our key, so the realistic version is a token from *our* project's other
	// environments. Either way the claim is required and compared.
	now := time.Now()
	s := newSigner(t)
	c := goodClaims(now)
	c["iss"] = "https://someone-else.supabase.co/auth/v1"
	if _, err := authWith(t, s.source(), now).Authenticate(request(s.token(t, c))); err == nil {
		t.Fatal("a token from another issuer was accepted")
	}
}

func TestATokenForAnotherAccountIsRejectedAndSaysSo(t *testing.T) {
	// The check that matters most: signature, issuer and expiry establish that
	// Supabase minted this for somebody. Only this one establishes that the
	// somebody owns *this* box.
	now := time.Now()
	s := newSigner(t)
	c := goodClaims(now)
	c["sub"] = "99999999-9999-9999-9999-999999999999"
	_, err := authWith(t, s.source(), now).Authenticate(request(s.token(t, c)))
	if !errors.Is(err, api.ErrWrongAccount) {
		t.Fatalf("a valid session for another account returned %v", err)
	}
}

func TestAnExpiredSessionIsRejected(t *testing.T) {
	now := time.Now()
	s := newSigner(t)
	c := goodClaims(now)
	c["exp"] = now.Add(-time.Hour).Unix()
	if _, err := authWith(t, s.source(), now).Authenticate(request(s.token(t, c))); err == nil {
		t.Fatal("an expired session was accepted")
	}

	// And a token with no expiry at all, which is a session that never ends.
	delete(c, "exp")
	if _, err := authWith(t, s.source(), now).Authenticate(request(s.token(t, c))); err == nil {
		t.Fatal("a token with no expiry was accepted")
	}
}

func TestAnUnknownKeyIsRejectedAndAColdCacheFailsClosed(t *testing.T) {
	now := time.Now()
	s := newSigner(t)

	// A kid we have never seen: during a rotation this is a refetch, and after
	// one it is a forgery attempt. Either way it is not a token to honour.
	other := newSigner(t)
	other.kid = "some-other-key"
	if _, err := authWith(t, s.source(), now).Authenticate(request(other.token(t, goodClaims(now)))); err == nil {
		t.Fatal("a token signed with an unknown key was accepted")
	}

	// A box that cannot reach the JWKS with a cold cache fails closed. The
	// failure a degraded mode must never have is "widen access".
	cold := api.KeySourceFunc(func(string) (crypto.PublicKey, error) { return nil, api.ErrNoKeys })
	_, err := authWith(t, cold, now).Authenticate(request(s.token(t, goodClaims(now))))
	if !errors.Is(err, api.ErrNoKeys) {
		t.Fatalf("a box with no keys returned %v; it must refuse, not fall back", err)
	}
}

func TestAuthAtIsWhenThePersonProvedThemselvesNotWhenTheTokenWasMinted(t *testing.T) {
	// Supabase refreshes access tokens roughly hourly without the user doing
	// anything. Reading `iat` would make DASHBOARD.md §4's "every vault write
	// re-authenticated regardless of session age" a check that always passes,
	// which is the same as not having it.
	now := time.Now()
	s := newSigner(t)
	c := goodClaims(now)
	c["iat"] = now.Unix() // freshly refreshed
	proved := now.Add(-6 * time.Hour)
	c["amr"] = []map[string]any{{"method": "password", "timestamp": proved.Unix()}}

	id, err := authWith(t, s.source(), now).Authenticate(request(s.token(t, c)))
	if err != nil {
		t.Fatal(err)
	}
	if got := id.AuthAt.Unix(); got != proved.Unix() {
		t.Errorf("AuthAt is %v, want the moment the password was presented (%v)",
			id.AuthAt, proved)
	}

	// A second factor moves it forward, which is what makes stepping up work.
	stepUp := now.Add(-time.Minute)
	c["amr"] = []map[string]any{
		{"method": "password", "timestamp": proved.Unix()},
		{"method": "totp", "timestamp": stepUp.Unix()},
	}
	id, err = authWith(t, s.source(), now).Authenticate(request(s.token(t, c)))
	if err != nil {
		t.Fatal(err)
	}
	if got := id.AuthAt.Unix(); got != stepUp.Unix() {
		t.Errorf("AuthAt is %v after a TOTP step-up, want %v", id.AuthAt, stepUp)
	}
}

func TestASessionTokenIsNotAcceptedFromTheQueryString(t *testing.T) {
	// The self-hosted authenticator accepts ?token= because a person pastes a
	// printed token into a browser once. This one must not: a Supabase session
	// is a live credential for somebody's recorded life, and query strings end
	// up in proxy logs and crash reports.
	now := time.Now()
	s := newSigner(t)
	r := httptest.NewRequest("GET", "/v1/sessions?token="+s.token(t, goodClaims(now)), nil)
	if _, err := authWith(t, s.source(), now).Authenticate(r); err == nil {
		t.Fatal("a session token in the query string was accepted")
	}
}

func TestABoxWithNoAccountBoundToItRefusesToBeBuilt(t *testing.T) {
	// Not defaulted to "any authenticated user". A box that accepted every
	// signed-in user would be open to the whole project and the failure would be
	// invisible until somebody looked.
	s := newSigner(t)
	if _, err := api.NewSupabaseAuthenticator(api.SupabaseOptions{
		Issuer: testIssuer, Keys: s.source(),
	}); err == nil {
		t.Fatal("an authenticator with no account id was built")
	}
	if _, err := api.NewSupabaseAuthenticator(api.SupabaseOptions{
		AccountID: testAccount, Keys: s.source(),
	}); err == nil {
		t.Fatal("an authenticator with no issuer was built")
	}
}

func TestAnRSAKeyIsVerifiedAndOnlyAsRS256(t *testing.T) {
	// Supabase projects exist on both key types, so both are supported — and
	// each is pinned to its own algorithm, which is the same rule that closes
	// the confusion attack above.
	now := time.Now()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keys := api.KeySourceFunc(func(string) (crypto.PublicKey, error) { return &key.PublicKey, nil })

	sign := func(alg string) string {
		header := seg(map[string]any{"alg": alg, "kid": "rsa-1"})
		body := seg(goodClaims(now))
		sum := sha256.Sum256([]byte(header + "." + body))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		return header + "." + body + "." + base64.RawURLEncoding.EncodeToString(sig)
	}

	if _, err := authWith(t, keys, now).Authenticate(request(sign("RS256"))); err != nil {
		t.Fatalf("a valid RS256 token was rejected: %v", err)
	}
	// The same bytes, claiming to be something else.
	if _, err := authWith(t, keys, now).Authenticate(request(sign("ES256"))); err == nil {
		t.Fatal("an RSA key verified a token whose header said ES256")
	}
}

func split3(t *testing.T, token string) (head, body, sig string) {
	t.Helper()
	var parts []string
	start := 0
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			parts = append(parts, token[start:i])
			start = i + 1
		}
	}
	parts = append(parts, token[start:])
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	return parts[0], parts[1], parts[2]
}

func TestTheAssuranceLevelSurvivesVerification(t *testing.T) {
	// The claim was parsed and thrown away for the whole life of this file,
	// which made CONTROL-PLANE.md §3 item 5 unenforceable no matter what the
	// guard did. It is carried straight from the token rather than derived from
	// `amr`: Supabase mints aal1 tokens that list a TOTP method, and its own
	// answer is the one that counts.
	now := time.Unix(1_700_000_000, 0)
	sg := newSigner(t)

	one := goodClaims(now)
	id, err := authWith(t, sg.source(), now).Authenticate(request(sg.token(t, one)))
	if err != nil {
		t.Fatal(err)
	}
	if id.Assurance != api.AssuranceSingleFactor {
		t.Errorf("a password session came out as %q", id.Assurance)
	}

	two := goodClaims(now)
	two["aal"] = "aal2"
	two["amr"] = []map[string]any{
		{"method": "password", "timestamp": now.Add(-10 * time.Minute).Unix()},
		{"method": "totp", "timestamp": now.Add(-time.Minute).Unix()},
	}
	id, err = authWith(t, sg.source(), now).Authenticate(request(sg.token(t, two)))
	if err != nil {
		t.Fatal(err)
	}
	if id.Assurance != api.AssuranceSecondFactor {
		t.Errorf("a two-factor session came out as %q", id.Assurance)
	}
	// And the freshness clock moved to the factor, not to the password.
	if want := now.Add(-time.Minute); !id.AuthAt.Equal(want) {
		t.Errorf("AuthAt is %v, want %v", id.AuthAt, want)
	}
}
