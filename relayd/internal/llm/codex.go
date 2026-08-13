package llm

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The ChatGPT subscription credential, which is not a key.
//
// Every other row in the catalog resolves to a string that stays a string. This
// one resolves to a JWT that expires in about an hour, is minted from a refresh
// token, and is only accepted by ChatGPT's own endpoint. Handing the installer's
// four reference kinds to it — env, file, exec, vault — asks the user to point
// at a secret that does not exist: `codex login` writes tokens, not a key.
//
// OpenClaw treats this as its own auth method rather than an API key with extra
// steps, and the three parts it splits into are copied here:
//
//  1. read what the CLI already left behind, from the Keychain first and then
//     auth.json, instead of asking a question the machine can answer;
//  2. mint a fresh access token from the refresh token at the moment of use,
//     because an hour-old one is the normal case, not the exceptional one;
//  3. send it to https://chatgpt.com/backend-api/codex, because api.openai.com
//     rejects a subscription bearer no matter how fresh it is.
//
// This file is (1) and (2). The wire is codexwire.go.

const (
	// CodexClientID is the Codex CLI's own OAuth client, which is the client
	// every one of these flows authenticates as. Relay is not registered with
	// OpenAI and does not need to be — "use the plan you already pay for" means
	// borrowing the client OpenAI ships, exactly as OpenClaw does.
	CodexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

	// CodexAuthBase is where tokens are minted and refreshed.
	CodexAuthBase = "https://auth.openai.com"

	// CodexBaseURL is the only endpoint that accepts a subscription bearer. It
	// speaks the Responses API, not chat completions — see codexwire.go.
	CodexBaseURL = "https://chatgpt.com/backend-api/codex"

	// codexKeychainService is the macOS Keychain item newer Codex builds write
	// instead of auth.json. The account name is derived from CODEX_HOME so two
	// homes on one machine do not collide.
	codexKeychainService = "Codex Auth"

	codexAuthFilename = "auth.json"

	// codexRefreshSkew mints a new token slightly before the old one dies. A
	// token that expires mid-flight reads as an auth failure to everything
	// downstream, and the whole point of this path is that the user is not
	// present to be asked about it.
	codexRefreshSkew = 5 * time.Minute

	// codexFallbackLifetime is how long a token is assumed to last when the JWT
	// carries no exp claim. Short, because being wrong in this direction costs
	// one extra refresh and being wrong in the other costs a failed call.
	codexFallbackLifetime = 45 * time.Minute
)

// CodexAuth is what the Codex CLI left behind, normalised.
type CodexAuth struct {
	// Mode is "chatgpt" for a subscription login and "apikey" when the CLI was
	// configured with a key instead. The distinction matters: an API-key Codex
	// home has no subscription to borrow, and pointing Relay at it as if it did
	// produces a 401 nobody can read.
	Mode string

	Access  string
	Refresh string
	IDToken string

	// AccountID is the ChatGPT-Account-Id header the endpoint requires. It comes
	// from the access token's own claims, not from the file, so a refreshed
	// token carries a matching one by construction.
	AccountID string
	// Email and Plan are for the installer to print. Neither is load-bearing.
	Email string
	Plan  string

	// APIKey is set only in "apikey" mode.
	APIKey string

	// Expires is when Access stops working. Zero means unknown.
	Expires time.Time

	// Source is where this was read from, for the installer to say so out loud.
	Source string
}

// Valid reports whether the access token is still usable.
func (a CodexAuth) Valid(now time.Time) bool {
	if a.Access == "" {
		return false
	}
	if a.Expires.IsZero() {
		return false
	}
	return now.Add(codexRefreshSkew).Before(a.Expires)
}

// Account is how the installer names this login in one line.
func (a CodexAuth) Account() string {
	switch {
	case a.Email != "" && a.Plan != "":
		return a.Email + " (" + a.Plan + ")"
	case a.Email != "":
		return a.Email
	case a.AccountID != "":
		return a.AccountID
	}
	return "a ChatGPT account"
}

// KeychainGet reads one secret from the OS keychain. It is a function rather
// than an interface so a test can supply one line.
type KeychainGet func(service, account string) (string, error)

// CodexOptions locates a Codex login. The zero value reads the real machine.
type CodexOptions struct {
	// Home is CODEX_HOME. Empty takes $CODEX_HOME, then ~/.codex.
	Home string
	// ReadFile reads auth.json. Nil uses the real filesystem. The installer
	// passes its own seam through, because a test that reads the developer's
	// actual ChatGPT login is a test that passes on one machine.
	ReadFile func(string) ([]byte, error)
	// Getenv reads CODEX_HOME. Nil uses the process environment.
	Getenv func(string) string
	// HomeDir expands "~". Empty asks the OS.
	HomeDir string
	// Keychain reads the macOS Keychain. Nil uses the real one on darwin and
	// skips the Keychain everywhere else.
	Keychain KeychainGet
	// Now defaults to time.Now.
	Now func() time.Time
	// HTTPClient is used for refreshes. Nil takes the package default.
	HTTPClient *http.Client
	// Lookup resolves the vault form, "codex:vault:<id>". Nil is fine for the
	// CLI form, which reads the filesystem and the Keychain instead.
	Lookup SecretLookup
}

func (o CodexOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// expandHome resolves a leading "~" against the configured home, or the OS's.
func (o CodexOptions) expandHome(p string) (string, error) {
	if !strings.HasPrefix(p, "~") {
		return p, nil
	}
	home := o.HomeDir
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return "", err
		}
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}

func (o CodexOptions) readFile(name string) ([]byte, error) {
	if o.ReadFile != nil {
		return o.ReadFile(name)
	}
	return os.ReadFile(name)
}

func (o CodexOptions) client() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	return codexHTTPClient
}

// codexHTTPClient is the default transport for token refreshes. Package-level
// because [CredentialRef.Resolve] has no config to carry one, and overridable in
// a test for the same reason.
var codexHTTPClient = &http.Client{Timeout: 30 * time.Second}

// CodexHome resolves CODEX_HOME the way the CLI itself does.
//
// It deliberately does not follow Relay's own relocatable home: an isolated
// RELAY_HOME must not hide a Codex login that belongs to the OS user, which is
// the same reasoning OpenClaw records against this function.
func CodexHome(o CodexOptions) string {
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	home := o.Home
	if strings.TrimSpace(home) == "" {
		home = getenv("CODEX_HOME")
	}
	if strings.TrimSpace(home) == "" {
		home = "~/.codex"
	}
	expanded, err := o.expandHome(home)
	if err != nil {
		return home
	}
	// Resolve symlinks so the Keychain account derived from this path matches
	// the one the CLI computed.
	if real, err := filepath.EvalSymlinks(expanded); err == nil {
		return real
	}
	return expanded
}

// codexKeychainAccount is sha256(CODEX_HOME) truncated, prefixed "cli|" —
// Codex's own scheme, reproduced so the item can be found at all.
func codexKeychainAccount(home string) string {
	sum := sha256.Sum256([]byte(home))
	return "cli|" + hex.EncodeToString(sum[:])[:16]
}

// securityKeychainGet shells out to macOS `security`. go-keyring is already a
// dependency for Relay's own vault, but this item was written by another
// program with its own service and account naming, and `security` is what Codex
// and OpenClaw both use to read it back.
func securityKeychainGet(service, account string) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", os.ErrNotExist
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "security",
		"find-generic-password", "-s", service, "-a", account, "-w").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// codexAuthFile is auth.json's shape. Only the fields that decide something are
// named; Codex writes more and is free to keep doing so.
type codexAuthFile struct {
	AuthMode     string `json:"auth_mode"`
	OpenAIAPIKey string `json:"OPENAI_API_KEY"`
	LastRefresh  string `json:"last_refresh"`
	Tokens       struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
}

// ReadCodexAuth reads the Codex CLI's credential: Keychain first, then
// auth.json. Both are checked because which one holds it depends on the Codex
// build, and a machine mid-upgrade has both.
func ReadCodexAuth(o CodexOptions) (CodexAuth, error) {
	home := CodexHome(o)

	get := o.Keychain
	// A supplied filesystem means this is not the real machine, so the real
	// machine's Keychain is not consulted either. Half a seam is worse than
	// none: it is the half that passes locally and fails in CI, or the reverse.
	if get == nil && o.ReadFile == nil && runtime.GOOS == "darwin" {
		get = securityKeychainGet
	}
	if get != nil {
		if raw, err := get(codexKeychainService, codexKeychainAccount(home)); err == nil && raw != "" {
			var f codexAuthFile
			if json.Unmarshal([]byte(raw), &f) == nil {
				if auth, ok := codexAuthFrom(f, "the macOS Keychain"); ok {
					return auth, nil
				}
			}
		}
	}

	path := filepath.Join(home, codexAuthFilename)
	b, err := o.readFile(path)
	if err != nil {
		return CodexAuth{}, fmt.Errorf("%w: no Codex login in %s: %v", ErrUnresolvedRef, home, err)
	}
	var f codexAuthFile
	if err := json.Unmarshal(b, &f); err != nil {
		return CodexAuth{}, fmt.Errorf("%w: %s is not readable JSON: %v", ErrUnresolvedRef, path, err)
	}
	auth, ok := codexAuthFrom(f, path)
	if !ok {
		return CodexAuth{}, fmt.Errorf(
			"%w: %s holds no ChatGPT tokens — run `codex login` and choose the ChatGPT option",
			ErrUnresolvedRef, path)
	}
	return auth, nil
}

// codexAuthFrom normalises one parsed record. auth_mode decides, and its
// absence falls back to whether an API key is present — Codex's own rule, and
// the reason an api-key home is reported as such rather than as a broken login.
func codexAuthFrom(f codexAuthFile, source string) (CodexAuth, bool) {
	mode := strings.ToLower(strings.TrimSpace(f.AuthMode))
	switch mode {
	case "apikey", "api_key":
		if f.OpenAIAPIKey == "" {
			return CodexAuth{}, false
		}
		return CodexAuth{Mode: "apikey", APIKey: f.OpenAIAPIKey, Source: source}, true
	case "chatgpt", "chatgptauthtokens":
		// fall through to the token path
	case "":
		if f.OpenAIAPIKey != "" {
			return CodexAuth{Mode: "apikey", APIKey: f.OpenAIAPIKey, Source: source}, true
		}
	default:
		return CodexAuth{}, false
	}

	if f.Tokens.AccessToken == "" || f.Tokens.RefreshToken == "" {
		return CodexAuth{}, false
	}
	auth := CodexAuth{
		Mode:      "chatgpt",
		Access:    f.Tokens.AccessToken,
		Refresh:   f.Tokens.RefreshToken,
		IDToken:   f.Tokens.IDToken,
		AccountID: f.Tokens.AccountID,
		Source:    source,
	}
	claims := decodeCodexClaims(f.Tokens.AccessToken)
	if claims.accountID != "" {
		auth.AccountID = claims.accountID
	}
	auth.Email = claims.email
	auth.Plan = claims.plan
	switch {
	case !claims.expires.IsZero():
		auth.Expires = claims.expires
	case f.LastRefresh != "":
		if t, err := time.Parse(time.RFC3339, f.LastRefresh); err == nil {
			auth.Expires = t.Add(codexFallbackLifetime)
		}
	}
	return auth, true
}

// codexClaims is the part of the access token Relay reads. The token is not
// verified here — it is not ours to verify, and OpenAI rejects a bad one on the
// call that matters.
type codexClaims struct {
	expires   time.Time
	accountID string
	email     string
	plan      string
}

func decodeCodexClaims(token string) codexClaims {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return codexClaims{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return codexClaims{}
	}
	var payload struct {
		Exp  json.Number `json:"exp"`
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
			Plan      string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
		Profile struct {
			Email string `json:"email"`
		} `json:"https://api.openai.com/profile"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return codexClaims{}
	}
	out := codexClaims{
		accountID: strings.TrimSpace(payload.Auth.AccountID),
		email:     strings.TrimSpace(payload.Profile.Email),
		plan:      strings.TrimSpace(payload.Auth.Plan),
	}
	if secs, err := strconv.ParseInt(payload.Exp.String(), 10, 64); err == nil && secs > 0 {
		out.expires = time.Unix(secs, 0)
	}
	return out
}

// CodexTokens is one refresh's worth of credential.
type CodexTokens struct {
	Access  string
	Refresh string
	IDToken string
	Expires time.Time
}

// RefreshCodexToken mints a new access token from a refresh token.
func RefreshCodexToken(ctx context.Context, client *http.Client, refresh string) (CodexTokens, error) {
	if strings.TrimSpace(refresh) == "" {
		return CodexTokens{}, fmt.Errorf("%w: no refresh token", ErrMissingCredential)
	}
	if client == nil {
		client = codexHTTPClient
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {CodexClientID},
		"scope":         {"openid profile email"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		CodexAuthBase+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return CodexTokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	codexUserAgent(req.Header)

	resp, err := client.Do(req)
	if err != nil {
		return CodexTokens{}, fmt.Errorf("%w: refreshing the ChatGPT token: %v", ErrUnresolvedRef, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := readLimited(resp.Body)
		return CodexTokens{}, &HTTPError{Status: resp.StatusCode, Body: body}
	}

	var out struct {
		AccessToken  string      `json:"access_token"`
		RefreshToken string      `json:"refresh_token"`
		IDToken      string      `json:"id_token"`
		ExpiresIn    json.Number `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return CodexTokens{}, fmt.Errorf("%w: refresh reply was not JSON: %v", ErrUnresolvedRef, err)
	}
	if out.AccessToken == "" {
		return CodexTokens{}, fmt.Errorf("%w: refresh returned no access token", ErrUnresolvedRef)
	}

	tokens := CodexTokens{
		Access:  out.AccessToken,
		Refresh: out.RefreshToken,
		IDToken: out.IDToken,
	}
	if tokens.Refresh == "" {
		// OpenAI does not always rotate. Keeping the old one is correct, and
		// dropping it would silently turn a working login into a one-hour one.
		tokens.Refresh = refresh
	}
	if claims := decodeCodexClaims(tokens.Access); !claims.expires.IsZero() {
		tokens.Expires = claims.expires
	} else if secs, err := strconv.ParseInt(out.ExpiresIn.String(), 10, 64); err == nil && secs > 0 {
		tokens.Expires = time.Now().Add(time.Duration(secs) * time.Second)
	} else {
		tokens.Expires = time.Now().Add(codexFallbackLifetime)
	}
	return tokens, nil
}

// codexUserAgent identifies the client the way the CLI does. The device-code
// endpoints are picky about the originator and answer 404 without it.
func codexUserAgent(h http.Header) {
	h.Set("originator", "relay")
	h.Set("User-Agent", "relay")
}

// readLimited reads an error body, capped. A provider that answers an auth
// failure with a megabyte of HTML should not become a megabyte of log.
func readLimited(r io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, 8<<10))
	return string(b), err
}

// codexCache holds tokens refreshed in this process.
//
// Relay does not write back to auth.json. The Codex CLI owns that file and
// rewrites it on its own schedule; a second writer racing it can leave the user
// logged out of both. OpenClaw draws the same line — it imports the CLI's
// credential once and then refreshes into its own store — so a refresh here
// lives in memory for as long as relayd does, and the CLI stays the file's only
// author.
var codexCache = struct {
	sync.Mutex
	byHome map[string]CodexTokens
}{byHome: map[string]CodexTokens{}}

// CodexPersist is called when a refresh rotates the refresh token on a login
// Relay owns, so the new one outlives this process. The daemon wires it to the
// vault; nil means rotation is kept in memory only, which costs one extra
// sign-in after a restart in the worst case and nothing in the common one.
var CodexPersist func(ctx context.Context, id, refresh string) error

// codexVaultPrefix marks the second form of a codex reference:
// "codex:vault:<id>" is a login Relay performed itself, with the refresh token
// in its own vault. "codex:<path>" borrows the CLI's.
const codexVaultPrefix = "vault:"

// codexVaultCache holds tokens minted from a vault-held refresh token.
var codexVaultCache = struct {
	sync.Mutex
	byID map[string]CodexTokens
}{byID: map[string]CodexTokens{}}

// resolveCodexVault mints an access token from a refresh token Relay stored.
//
// Only the refresh token is kept. An access token is worth about an hour and
// storing one would mean the vault's copy is stale nearly always — so the
// long-lived half is what gets persisted and the short-lived half is made on
// demand, which is also why this path needs no write on the happy path.
func resolveCodexVault(ctx context.Context, lookup SecretLookup, id string, client *http.Client, now time.Time) (CodexAuth, error) {
	if lookup == nil {
		return CodexAuth{}, fmt.Errorf(
			"%w: codex:vault:%s, but no vault is wired up", ErrUnresolvedRef, id)
	}

	codexVaultCache.Lock()
	cached, ok := codexVaultCache.byID[id]
	codexVaultCache.Unlock()
	if ok && now.Add(codexRefreshSkew).Before(cached.Expires) {
		return codexAuthFromTokens(cached, "Relay's vault"), nil
	}

	seed, err := lookup(ctx, id)
	if err != nil {
		return CodexAuth{}, fmt.Errorf("%w: codex:vault:%s: %v", ErrUnresolvedRef, id, err)
	}
	refresh := strings.TrimSpace(seed)
	if ok && cached.Refresh != "" {
		// Prefer the rotated one this process was given over the stored seed.
		refresh = cached.Refresh
	}

	tokens, err := RefreshCodexToken(ctx, client, refresh)
	if err != nil {
		return CodexAuth{}, fmt.Errorf(
			"%w: the ChatGPT login in Relay's vault could not be refreshed (%v) — sign in again",
			ErrUnresolvedRef, err)
	}
	codexVaultCache.Lock()
	codexVaultCache.byID[id] = tokens
	codexVaultCache.Unlock()
	if tokens.Refresh != refresh && CodexPersist != nil {
		// Best effort. A rotation that cannot be saved still works for as long
		// as this process runs, and failing the call over it would be worse.
		_ = CodexPersist(ctx, id, tokens.Refresh)
	}
	return codexAuthFromTokens(tokens, "Relay's vault"), nil
}

// CodexAccountOf names the account a freshly minted set of tokens belongs to,
// for the installer to print. Empty when the token carries no identity claims.
func CodexAccountOf(t CodexTokens) string {
	claims := decodeCodexClaims(t.Access)
	if claims.accountID == "" && claims.email == "" {
		return ""
	}
	return CodexAuth{AccountID: claims.accountID, Email: claims.email, Plan: claims.plan}.Account()
}

func codexAuthFromTokens(t CodexTokens, source string) CodexAuth {
	claims := decodeCodexClaims(t.Access)
	return CodexAuth{
		Mode:      "chatgpt",
		Access:    t.Access,
		Refresh:   t.Refresh,
		IDToken:   t.IDToken,
		AccountID: claims.accountID,
		Email:     claims.email,
		Plan:      claims.plan,
		Expires:   t.Expires,
		Source:    source,
	}
}

// ResolveCodex returns a usable access token and the account it belongs to,
// refreshing first when the stored one has expired.
func ResolveCodex(ctx context.Context, o CodexOptions) (CodexAuth, error) {
	now := o.now()
	if id, ok := strings.CutPrefix(strings.TrimSpace(o.Home), codexVaultPrefix); ok {
		return resolveCodexVault(ctx, o.Lookup, id, o.client(), now)
	}
	home := CodexHome(o)

	read := o
	read.Home = home
	auth, err := ReadCodexAuth(read)
	if err != nil {
		return CodexAuth{}, err
	}
	if auth.Mode == "apikey" {
		// An api-key Codex home is a working credential, just not a
		// subscription one. Say which, because the fix differs.
		return auth, nil
	}

	codexCache.Lock()
	cached, ok := codexCache.byHome[home]
	codexCache.Unlock()
	if ok && cached.Refresh == auth.Refresh && now.Add(codexRefreshSkew).Before(cached.Expires) {
		auth.Access = cached.Access
		auth.Expires = cached.Expires
		if claims := decodeCodexClaims(cached.Access); claims.accountID != "" {
			auth.AccountID = claims.accountID
		}
		return auth, nil
	}
	if auth.Valid(now) {
		return auth, nil
	}

	tokens, err := RefreshCodexToken(ctx, o.client(), auth.Refresh)
	if err != nil {
		return CodexAuth{}, fmt.Errorf(
			"%w: the ChatGPT login in %s expired and could not be refreshed (%v) — run `codex login`",
			ErrUnresolvedRef, auth.Source, err)
	}
	codexCache.Lock()
	codexCache.byHome[home] = tokens
	codexCache.Unlock()

	auth.Access = tokens.Access
	auth.Expires = tokens.Expires
	if tokens.IDToken != "" {
		auth.IDToken = tokens.IDToken
	}
	if claims := decodeCodexClaims(tokens.Access); claims.accountID != "" {
		auth.AccountID = claims.accountID
	}
	return auth, nil
}
