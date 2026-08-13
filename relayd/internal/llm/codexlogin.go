package llm

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Signing in, when there is no Codex CLI to borrow from.
//
// The installer's old wording sent the user to another terminal to run `codex
// login`, then asked them to point at the credential it left behind. On a
// headless box that is two problems: there may be no codex binary at all, and
// there is certainly no browser. OpenClaw solves both by owning the flow, and
// this file is that, in Go:
//
//   - [CodexDeviceLogin] is the headless answer. A code on the terminal, a URL
//     the user opens on whatever machine they are actually sitting at, and a
//     poll. Nothing local needs a browser, a port or a display.
//   - [CodexBrowserLogin] is the desktop answer: PKCE, a loopback listener on
//     the port OpenAI's client is registered for, and a paste fallback that
//     races the callback so a browser that never opens is not a dead end.
//
// Both authenticate as the Codex client, because that is what a ChatGPT plan is
// spendable through. Neither writes to ~/.codex: the CLI owns that file, and a
// second writer racing it can log the user out of both.

const (
	// CodexCallbackPort is fixed by OpenAI's registration of the Codex client.
	// A different port is not a preference, it is a rejected redirect_uri —
	// which is also why the installer says the number out loud before it binds
	// it, on a box where something else may already have it.
	CodexCallbackPort = 1455
	codexCallbackPath = "/auth/callback"
	codexScope        = "openid profile email offline_access"

	// codexDeviceTimeout matches the CLI's own patience.
	codexDeviceTimeout     = 15 * time.Minute
	codexDevicePollDefault = 5 * time.Second
	codexDevicePollMin     = time.Second

	// codexPasteAfter is how long the browser gets before the paste prompt is
	// offered as well. It is offered *alongside* the listener, not instead of
	// it — whichever finishes first wins.
	codexPasteAfter = 15 * time.Second
)

// CodexDevicePrompt is what the user has to be told to complete a device login.
type CodexDevicePrompt struct {
	// URL is opened on any machine with a browser — not necessarily this one.
	URL string
	// Code is typed into that page.
	Code string
	// Expires is when the code stops working.
	Expires time.Time
}

// CodexDeviceLogin runs the device-pairing flow. show is called once, as soon
// as there is something for the user to do.
func CodexDeviceLogin(ctx context.Context, client *http.Client, show func(CodexDevicePrompt) error) (CodexTokens, error) {
	if client == nil {
		client = codexHTTPClient
	}
	ctx, cancel := context.WithTimeout(ctx, codexDeviceTimeout)
	defer cancel()

	var start struct {
		DeviceAuthID string      `json:"device_auth_id"`
		UserCode     string      `json:"user_code"`
		Interval     json.Number `json:"interval"`
	}
	if err := codexPostJSON(ctx, client,
		CodexAuthBase+"/api/accounts/deviceauth/usercode",
		map[string]string{"client_id": CodexClientID}, &start); err != nil {
		var he *HTTPError
		if errors.As(err, &he) && he.Status == http.StatusNotFound {
			return CodexTokens{}, errors.New(
				"OpenAI has not enabled device pairing for this account — use the browser sign-in instead")
		}
		return CodexTokens{}, err
	}
	if start.DeviceAuthID == "" || start.UserCode == "" {
		return CodexTokens{}, errors.New("llm: OpenAI returned no device code")
	}

	if show != nil {
		if err := show(CodexDevicePrompt{
			URL:     CodexAuthBase + "/codex/device",
			Code:    start.UserCode,
			Expires: time.Now().Add(codexDeviceTimeout),
		}); err != nil {
			return CodexTokens{}, err
		}
	}

	interval := codexDevicePollDefault
	if secs, err := start.Interval.Int64(); err == nil && secs > 0 {
		interval = time.Duration(secs) * time.Second
	}
	if interval < codexDevicePollMin {
		interval = codexDevicePollMin
	}

	for {
		var poll struct {
			AuthorizationCode string `json:"authorization_code"`
			CodeVerifier      string `json:"code_verifier"`
		}
		err := codexPostJSON(ctx, client,
			CodexAuthBase+"/api/accounts/deviceauth/token",
			map[string]string{
				"device_auth_id": start.DeviceAuthID,
				"user_code":      start.UserCode,
			}, &poll)

		switch {
		case err == nil && poll.AuthorizationCode != "" && poll.CodeVerifier != "":
			return codexExchange(ctx, client, poll.AuthorizationCode, poll.CodeVerifier,
				CodexAuthBase+"/deviceauth/callback")
		case err == nil:
			return CodexTokens{}, errors.New("llm: device authorization returned no exchange code")
		}

		// 403 and 404 are "not yet" on this endpoint, not failures. Anything
		// else is real and stops the loop rather than spinning for 15 minutes.
		var he *HTTPError
		if !errors.As(err, &he) || (he.Status != http.StatusForbidden && he.Status != http.StatusNotFound) {
			if ctx.Err() != nil {
				return CodexTokens{}, fmt.Errorf("llm: device authorization timed out after %s", codexDeviceTimeout)
			}
			return CodexTokens{}, err
		}

		select {
		case <-ctx.Done():
			return CodexTokens{}, fmt.Errorf("llm: device authorization timed out after %s", codexDeviceTimeout)
		case <-time.After(interval):
		}
	}
}

// CodexBrowserLogin runs the PKCE browser flow.
//
// open is called with the authorization URL — printing it is a valid
// implementation, and on a remote box it is the only one. paste, when non-nil,
// is called after [codexPasteAfter] and should block until the user supplies
// the pasted code or redirect URL; it races the loopback callback.
func CodexBrowserLogin(ctx context.Context, client *http.Client, open func(string) error, paste func() (string, error)) (CodexTokens, error) {
	if client == nil {
		client = codexHTTPClient
	}
	verifier, challenge, err := codexPKCE()
	if err != nil {
		return CodexTokens{}, err
	}
	state, err := codexRandomHex(16)
	if err != nil {
		return CodexTokens{}, err
	}

	redirect := fmt.Sprintf("http://localhost:%d%s", CodexCallbackPort, codexCallbackPath)
	q := url.Values{
		"response_type":              {"code"},
		"client_id":                  {CodexClientID},
		"redirect_uri":               {redirect},
		"scope":                      {codexScope},
		"code_challenge":             {challenge},
		"code_challenge_method":      {"S256"},
		"state":                      {state},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
		"originator":                 {"relay"},
	}
	authURL := CodexAuthBase + "/oauth/authorize?" + q.Encode()

	codes := make(chan string, 2)
	fail := make(chan error, 2)

	ln, lnErr := net.Listen("tcp", fmt.Sprintf("localhost:%d", CodexCallbackPort))
	if lnErr == nil {
		srv := &http.Server{Handler: codexCallbackHandler(state, codes)}
		go func() { _ = srv.Serve(ln) }()
		defer func() {
			shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdown)
		}()
	}

	if open != nil {
		if err := open(authURL); err != nil {
			return CodexTokens{}, err
		}
	}

	if paste != nil {
		go func() {
			// The paste prompt is offered late rather than immediately, so the
			// common case — a browser that opens and works — never shows the
			// user a question they did not need to answer.
			if lnErr == nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(codexPasteAfter):
				}
			}
			raw, err := paste()
			if err != nil {
				fail <- err
				return
			}
			code, err := codexParseRedirect(raw, state)
			if err != nil {
				fail <- err
				return
			}
			codes <- code
		}()
	} else if lnErr != nil {
		return CodexTokens{}, fmt.Errorf(
			"llm: nothing is listening on port %d and there is no way to paste the code: %v",
			CodexCallbackPort, lnErr)
	}

	select {
	case <-ctx.Done():
		return CodexTokens{}, ctx.Err()
	case err := <-fail:
		return CodexTokens{}, err
	case code := <-codes:
		return codexExchange(ctx, client, code, verifier, redirect)
	}
}

// codexCallbackHandler answers the browser and hands the code back.
func codexCallbackHandler(state string, codes chan<- string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Force the connection closed: a keep-alive socket from the callback
		// browser can otherwise hold the listener open after the installer has
		// moved on.
		w.Header().Set("Connection", "close")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")

		if r.URL.Path != codexCallbackPath {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(codexPage("That is not the callback address.")))
			return
		}
		if r.URL.Query().Get("state") != state {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(codexPage("The sign-in state did not match. Start again.")))
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(codexPage("OpenAI sent no authorization code.")))
			return
		}
		_, _ = w.Write([]byte(codexPage("Signed in. You can close this window and go back to the terminal.")))
		select {
		case codes <- code:
		default:
		}
	})
	return mux
}

func codexPage(message string) string {
	return "<!doctype html><meta charset=utf-8><title>Relay</title>" +
		"<body style=\"font:16px/1.5 system-ui;margin:4rem auto;max-width:32rem;padding:0 1rem\">" +
		"<p>" + message + "</p>"
}

// codexParseRedirect accepts either a bare code or the whole redirect URL,
// because "paste the code" and "paste the URL" are the same instruction to
// somebody looking at a browser address bar.
func codexParseRedirect(raw, state string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("llm: nothing pasted")
	}
	if !strings.Contains(raw, "?") && !strings.Contains(raw, "://") {
		return raw, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("llm: that is not a code or a URL: %v", err)
	}
	q := u.Query()
	if got := q.Get("state"); got != "" && got != state {
		return "", errors.New("llm: that redirect belongs to a different sign-in attempt")
	}
	code := q.Get("code")
	if code == "" {
		return "", errors.New("llm: that URL carries no authorization code")
	}
	return code, nil
}

// codexExchange turns an authorization code into tokens.
func codexExchange(ctx context.Context, client *http.Client, code, verifier, redirect string) (CodexTokens, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"client_id":     {CodexClientID},
		"code_verifier": {verifier},
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
		return CodexTokens{}, err
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
		return CodexTokens{}, err
	}
	if out.AccessToken == "" || out.RefreshToken == "" {
		return CodexTokens{}, errors.New("llm: the token exchange returned no usable tokens")
	}
	tokens := CodexTokens{Access: out.AccessToken, Refresh: out.RefreshToken, IDToken: out.IDToken}
	if claims := decodeCodexClaims(tokens.Access); !claims.expires.IsZero() {
		tokens.Expires = claims.expires
	} else if secs, err := out.ExpiresIn.Int64(); err == nil && secs > 0 {
		tokens.Expires = time.Now().Add(time.Duration(secs) * time.Second)
	} else {
		tokens.Expires = time.Now().Add(codexFallbackLifetime)
	}
	return tokens, nil
}

func codexPostJSON(ctx context.Context, client *http.Client, endpoint string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	codexUserAgent(req.Header)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		text, _ := readLimited(resp.Body)
		return &HTTPError{Status: resp.StatusCode, Body: text}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func codexPKCE() (verifier, challenge string, err error) {
	buf := make([]byte, 64)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func codexRandomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
