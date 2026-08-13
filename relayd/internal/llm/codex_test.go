package llm_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/llm"
)

// The ChatGPT subscription credential.
//
// It is the one entry in the catalog that is not a string on disk: `codex
// login` writes a token that expires in about an hour, and the four original
// reference kinds all describe something that stays put. These tests pin the
// three things that follow from that — where the login is read from, that a
// stale one is refreshed rather than reported as broken, and that the call goes
// to the only endpoint a subscription bearer is accepted at.

func codexToken(t *testing.T, email, plan, account string, expires time.Time) string {
	t.Helper()
	payload := map[string]any{
		"exp": expires.Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": account,
			"chatgpt_plan_type":  plan,
		},
		"https://api.openai.com/profile": map[string]any{"email": email},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(b) + ".sig"
}

// codexHome writes an auth.json the way the CLI does.
func codexHome(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCodexAuthIsReadFromTheCLIsOwnFile(t *testing.T) {
	access := codexToken(t, "someone@example.com", "pro", "acct-7", time.Now().Add(time.Hour))
	dir := codexHome(t, fmt.Sprintf(
		`{"auth_mode":"chatgpt","tokens":{"access_token":%q,"refresh_token":"r","account_id":"from-file"}}`,
		access))

	auth, err := llm.ReadCodexAuth(llm.CodexOptions{Home: dir, ReadFile: os.ReadFile})
	if err != nil {
		t.Fatal(err)
	}
	if auth.Mode != "chatgpt" {
		t.Errorf("mode = %q", auth.Mode)
	}
	// The account id comes from the token's own claims, not the file's copy of
	// it: a refreshed token then carries a matching one by construction, and
	// the two cannot drift apart.
	if auth.AccountID != "acct-7" {
		t.Errorf("account = %q, want the claim from the token", auth.AccountID)
	}
	if auth.Email != "someone@example.com" || auth.Plan != "pro" {
		t.Errorf("identity = %q/%q", auth.Email, auth.Plan)
	}
	if !auth.Valid(time.Now()) {
		t.Error("a token good for an hour should be valid")
	}
}

// An api-key Codex home is a working credential, just not a subscription. The
// distinction is reported rather than smoothed over, because the fix differs.
func TestCodexAPIKeyHomeIsNotASubscription(t *testing.T) {
	dir := codexHome(t, `{"OPENAI_API_KEY":"sk-abc"}`)
	auth, err := llm.ReadCodexAuth(llm.CodexOptions{Home: dir, ReadFile: os.ReadFile})
	if err != nil {
		t.Fatal(err)
	}
	if auth.Mode != "apikey" || auth.APIKey != "sk-abc" {
		t.Errorf("auth = %+v, want the api key it actually holds", auth)
	}
}

func TestCodexHomeWithNoLoginDoesNotResolve(t *testing.T) {
	ref, err := llm.ParseRef("codex:" + t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ref.Resolve(context.Background(), nil); !errors.Is(err, llm.ErrUnresolvedRef) {
		t.Errorf("err = %v, want an unresolved reference", err)
	}
}

// An expired access token is the normal case, not a failure: the whole point of
// this path is that nobody is present to be asked about it.
func TestExpiredCodexTokenIsRefreshedAtUse(t *testing.T) {
	stale := codexToken(t, "a@b.c", "plus", "acct-1", time.Now().Add(-time.Hour))
	fresh := codexToken(t, "a@b.c", "plus", "acct-1", time.Now().Add(time.Hour))
	dir := codexHome(t, fmt.Sprintf(
		`{"auth_mode":"chatgpt","tokens":{"access_token":%q,"refresh_token":"old-refresh"}}`, stale))

	tr := &stubTransport{status: 200, body: fmt.Sprintf(
		`{"access_token":%q,"refresh_token":"new-refresh","expires_in":3600}`, fresh)}

	auth, err := llm.ResolveCodex(context.Background(), llm.CodexOptions{
		Home: dir, ReadFile: os.ReadFile, HTTPClient: client(tr),
	})
	if err != nil {
		t.Fatal(err)
	}
	if auth.Access != fresh {
		t.Error("resolving an expired login must mint a new access token")
	}
	if len(tr.seen) != 1 {
		t.Fatalf("made %d calls, want one refresh", len(tr.seen))
	}
	if got := tr.seen[0].URL.String(); got != "https://auth.openai.com/oauth/token" {
		t.Errorf("refreshed at %s", got)
	}
	if !strings.Contains(tr.seenRaw[0], "old-refresh") ||
		!strings.Contains(tr.seenRaw[0], llm.CodexClientID) {
		t.Errorf("refresh body = %s", tr.seenRaw[0])
	}
}

// Relay never writes to ~/.codex. The CLI owns that file and rewrites it on its
// own schedule; a second writer racing it can log the user out of both.
func TestRefreshDoesNotWriteBackToTheCodexCLI(t *testing.T) {
	stale := codexToken(t, "a@b.c", "plus", "acct-1", time.Now().Add(-time.Hour))
	fresh := codexToken(t, "a@b.c", "plus", "acct-1", time.Now().Add(time.Hour))
	contents := fmt.Sprintf(
		`{"auth_mode":"chatgpt","tokens":{"access_token":%q,"refresh_token":"old-refresh"}}`, stale)
	dir := codexHome(t, contents)

	tr := &stubTransport{status: 200, body: fmt.Sprintf(
		`{"access_token":%q,"refresh_token":"new-refresh","expires_in":3600}`, fresh)}
	if _, err := llm.ResolveCodex(context.Background(), llm.CodexOptions{
		Home: dir, ReadFile: os.ReadFile, HTTPClient: client(tr),
	}); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != contents {
		t.Errorf("auth.json was rewritten:\n%s", after)
	}
}

// A subscription bearer is refused by api.openai.com. The plan buys the Codex
// endpoint, which speaks Responses — and it wants the account named in a header.
func TestCodexProviderCallsTheSubscriptionEndpoint(t *testing.T) {
	access := codexToken(t, "a@b.c", "plus", "acct-42", time.Now().Add(time.Hour))
	tr := &stubTransport{
		status: 200,
		ctype:  "text/event-stream",
		body: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5.6-codex\"," +
			"\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n",
	}

	p, err := llm.New(llm.Config{
		Vendor: "openai", API: llm.APICodex, BaseURL: llm.CodexBaseURL,
		Model: llm.CodexModelDefault, HTTPClient: client(tr),
		Credential: llm.CredentialRef{Kind: llm.RefInline, Value: access},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "ping"}}, MaxTokens: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "ok" {
		t.Errorf("text = %q", res.Text)
	}
	if res.Usage.OutputTokens != 1 {
		t.Errorf("usage = %+v", res.Usage)
	}

	req := tr.seen[0]
	if got := req.URL.String(); got != llm.CodexBaseURL+"/responses" {
		t.Errorf("called %s, want the Responses endpoint", got)
	}
	if got := req.Header.Get("ChatGPT-Account-Id"); got != "acct-42" {
		t.Errorf("account header = %q, want the one in the bearer", got)
	}

	var sent map[string]any
	if err := json.Unmarshal([]byte(tr.seenRaw[0]), &sent); err != nil {
		t.Fatal(err)
	}
	// The endpoint refuses a non-streaming request, and floors output tokens at
	// 16 — the probe asks for one, which would be a 400 that reads like a bad
	// credential and is not one.
	if sent["stream"] != true {
		t.Error("the Codex endpoint only answers streaming requests")
	}
	if got := sent["max_output_tokens"]; got != float64(16) {
		t.Errorf("max_output_tokens = %v, want the endpoint's floor", got)
	}
	if _, ok := sent["messages"]; ok {
		t.Error("Responses takes input items, not messages")
	}
}

// A probe against a subscription that has lapsed has to read as a credential
// problem, not as a provider outage: the two send the user somewhere different.
func TestCodexProbeReportsAnExpiredSubscription(t *testing.T) {
	access := codexToken(t, "a@b.c", "plus", "acct-42", time.Now().Add(time.Hour))
	tr := &stubTransport{status: 401, body: `{"error":{"message":"no active plan"}}`}
	p, err := llm.New(llm.Config{
		Vendor: "openai", API: llm.APICodex, BaseURL: llm.CodexBaseURL,
		Model: llm.CodexModelDefault, HTTPClient: client(tr),
		Credential: llm.CredentialRef{Kind: llm.RefInline, Value: access},
	})
	if err != nil {
		t.Fatal(err)
	}
	res := p.Probe(context.Background())
	if res.Reason != llm.ReasonExpired {
		t.Errorf("reason = %q, want %q", res.Reason, llm.ReasonExpired)
	}
	if !strings.Contains(res.Detail, "no active plan") {
		t.Errorf("detail = %q, want the provider's own words", res.Detail)
	}
}

// The catalog's ChatGPT rows carry their own endpoint, wire and model, because
// the vendor's are all three wrong for a subscription.
func TestChatGPTRowsCarryTheirOwnEndpoint(t *testing.T) {
	v, ok := llm.Vendor("openai")
	if !ok {
		t.Fatal("no openai vendor")
	}
	var subscription int
	for _, a := range v.Auths {
		if a.Ref != llm.RefCodex {
			continue
		}
		subscription++
		if a.BaseURL != llm.CodexBaseURL || a.API != llm.APICodex {
			t.Errorf("%s points at %s/%s", a.ID, a.BaseURL, a.API)
		}
		if a.Model == "" {
			t.Errorf("%s has no model default; the subscription serves its own catalog", a.ID)
		}
	}
	// Browser sign-in and device pairing both. The second is the one that works
	// on the headless box this product is installed on.
	if subscription != 2 {
		t.Errorf("found %d ChatGPT rows, want the browser and device-code pair", subscription)
	}
}
