package install

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/detect"
	"github.com/luthor007/relay/relayd/internal/llm"
)

// The ChatGPT rows, which are the ones that used to ask a question with no
// correct answer: run `codex login` in another terminal, then tell Relay which
// environment variable holds the key. A ChatGPT plan has no key. What `codex
// login` leaves behind is a token that expires in about an hour, and none of
// env, file, exec or vault describes a thing like that.

// codexJWT builds an unsigned token carrying the claims Relay reads. Nothing
// verifies it — OpenAI rejects a bad one on the call that matters, and this
// package only ever decodes the claims.
func codexJWT(t *testing.T, email, plan, account string, expires time.Time) string {
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

// fakeCodexLogin is a machine with a ChatGPT login already on it.
func fakeCodexLogin(o llm.CodexOptions) (llm.CodexAuth, error) {
	return llm.CodexAuth{
		Mode:      "chatgpt",
		Access:    "header.payload.sig",
		Refresh:   "refresh-token",
		AccountID: "acct-123",
		Email:     "someone@example.com",
		Plan:      "pro",
		Expires:   time.Now().Add(time.Hour),
		Source:    home + "/.codex/auth.json",
	}, nil
}

// withCodexLogin puts a real auth.json on the fixture machine.
//
// Detection and resolution are two different seams and both have to be fed:
// ReadCodexAuth answers "is there a login here" during the step, and the
// credential written from it is resolved again by the probe — through the
// installer's filesystem, so that a test never reads the developer's own
// ChatGPT tokens.
func withCodexLogin(t *testing.T) func(*Options) {
	access := codexJWT(t, "someone@example.com", "pro", "acct-123", time.Now().Add(time.Hour))
	return func(o *Options) {
		o.ReadCodexAuth = fakeCodexLogin
		fs, ok := o.Env.FS.(*detect.MemFS)
		if !ok {
			t.Fatalf("fixture filesystem is %T, not a MemFS", o.Env.FS)
		}
		fs.Files[home+"/.codex/auth.json"] = fmt.Sprintf(
			`{"auth_mode":"chatgpt","tokens":{"access_token":%q,"refresh_token":"r"}}`, access)
	}
}

func codexAnswers() map[string]string {
	a := baseAnswers()
	a["models.small.vendor"] = "openai"
	a["models.small.auth"] = "openai-codex"
	a["models.small.model"] = ""
	a["models.big.cred.kind"] = "env"
	a["models.big.cred.env"] = "OPENROUTER_API_KEY"
	delete(a, "models.big.reuse")
	return a
}

// A login this machine already has is read, not asked about. The installer can
// answer this question itself, so it does.
func TestExistingChatGPTLoginIsFoundNotAskedFor(t *testing.T) {
	answers := codexAnswers()
	answers["models.small.chatgpt.how"] = "cli"

	opts, script, _ := newOpts(t, answers, withCodexLogin(t))
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}

	got := res.Models.Small.Model.Credential
	want := "codex:" + llm.CodexHome(codexOptions(opts))
	if got != want {
		t.Errorf("credential = %q, want %q", got, want)
	}
	// The installer must not write a config the daemon then refuses. config
	// keeps its own copy of the known reference prefixes — it is a leaf package
	// and imports nothing — so a new kind has to be added in both places or the
	// run ends in a file that will not load.
	if err := res.Config.Validate(); err != nil {
		t.Errorf("the installer wrote a config the daemon will refuse: %v", err)
	}
	// The account is named back, so a machine with two logins is not a coin
	// toss the user finds out about later.
	if out := script.Output(); !strings.Contains(out, "someone@example.com") {
		t.Errorf("the login found should be named:\n%s", out)
	}
	for _, id := range script.Asked {
		if strings.HasPrefix(id, "models.small.cred") {
			t.Errorf("asked %q — a subscription has no credential reference", id)
		}
		if id == "models.small.login" {
			t.Error(`asked "Signed in?" — the machine can answer that itself`)
		}
	}
}

// With no login on the machine, Relay performs one. Device pairing is the row
// that works on the box this product is actually installed on: no browser, no
// display, no loopback port.
func TestDevicePairingStoresOnlyTheRefreshToken(t *testing.T) {
	answers := codexAnswers()
	answers["models.small.chatgpt.how"] = "device"

	access := codexJWT(t, "someone@example.com", "plus", "acct-9", time.Now().Add(time.Hour))
	var shown llm.CodexDevicePrompt
	v := &fakeVault{}
	opts, script, _ := newOpts(t, answers, func(o *Options) {
		o.Vault = v
		o.ReadCodexAuth = func(llm.CodexOptions) (llm.CodexAuth, error) {
			return llm.CodexAuth{}, fmt.Errorf("no login here")
		}
		o.CodexDeviceLogin = func(ctx context.Context, show func(llm.CodexDevicePrompt) error) (llm.CodexTokens, error) {
			shown = llm.CodexDevicePrompt{
				URL: "https://auth.openai.com/codex/device", Code: "ABCD-EFGH",
				Expires: time.Now().Add(15 * time.Minute),
			}
			if err := show(shown); err != nil {
				return llm.CodexTokens{}, err
			}
			return llm.CodexTokens{
				Access: access, Refresh: "refresh-xyz", Expires: time.Now().Add(time.Hour),
			}, nil
		}
	})
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}

	out := script.Output()
	if !strings.Contains(out, shown.Code) || !strings.Contains(out, shown.URL) {
		t.Errorf("the code and the URL both have to be printed:\n%s", out)
	}

	cred := res.Models.Small.Model.Credential
	if !strings.HasPrefix(cred, "codex:vault:") {
		t.Fatalf("credential = %q, want a codex reference into the vault", cred)
	}
	id := strings.TrimPrefix(cred, "codex:vault:")

	// Only the long-lived half is kept. An access token is worth an hour, so
	// storing one would mean the vault's copy is stale nearly always.
	if got := v.stored[id]; got != "refresh-xyz" {
		t.Errorf("vault holds %q, want the refresh token", got)
	}
	for _, secret := range v.stored {
		if secret == access {
			t.Error("an access token must not be stored; it is minted at use")
		}
	}
}

// The subscription is only spendable at chatgpt.com, which speaks the Responses
// API. Probing it against api.openai.com reports a working login as a bad one.
func TestChatGPTRowSwitchesEndpointAndWire(t *testing.T) {
	answers := codexAnswers()
	answers["models.small.chatgpt.how"] = "cli"

	var seen []string
	opts, script, _ := newOpts(t, answers, func(o *Options) {
		withCodexLogin(t)(o)
		o.HTTPClient = happyProvider(t, &seen)
	})
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}

	m := res.Models.Small.Model
	if m.BaseURL != llm.CodexBaseURL {
		t.Errorf("base URL = %q, want %q", m.BaseURL, llm.CodexBaseURL)
	}
	if m.API != string(llm.APICodex) {
		t.Errorf("api = %q, want %q", m.API, llm.APICodex)
	}
	if m.Model != llm.CodexModelDefault {
		t.Errorf("model = %q, want the subscription catalog's default %q", m.Model, llm.CodexModelDefault)
	}
	for _, u := range seen {
		if strings.Contains(u, "api.openai.com") {
			t.Errorf("probed %s — a subscription bearer is refused there", u)
		}
	}
}

// Two models, one ChatGPT account, one sign-in.
//
// Signing in is the slowest thing in the install — a code typed on a phone, a
// browser round trip — and the second model is normally the same account as the
// first. Asking twice is asking the user to redo the slow part to arrive where
// they already were.
func TestTheSecondModelDoesNotSignInAgain(t *testing.T) {
	answers := codexAnswers()
	answers["models.small.chatgpt.how"] = "device"
	answers["models.big.vendor"] = "openai"
	answers["models.big.auth"] = "openai-codex"
	answers["models.big.model"] = ""
	// Empty means "press return", so this asserts the default is the login the
	// run already has rather than another device flow.
	answers["models.big.chatgpt.how"] = ""
	delete(answers, "models.big.cred.kind")
	delete(answers, "models.big.cred.env")

	access := codexJWT(t, "someone@example.com", "plus", "acct-9", time.Now().Add(time.Hour))
	var logins int
	opts, script, _ := newOpts(t, answers, func(o *Options) {
		o.Vault = &fakeVault{}
		o.ReadCodexAuth = func(llm.CodexOptions) (llm.CodexAuth, error) {
			return llm.CodexAuth{}, fmt.Errorf("no login here")
		}
		o.CodexDeviceLogin = func(ctx context.Context, show func(llm.CodexDevicePrompt) error) (llm.CodexTokens, error) {
			logins++
			if err := show(llm.CodexDevicePrompt{
				URL: "https://auth.openai.com/codex/device", Code: "ABCD-EFGH",
				Expires: time.Now().Add(15 * time.Minute),
			}); err != nil {
				return llm.CodexTokens{}, err
			}
			return llm.CodexTokens{Access: access, Refresh: "refresh-xyz", Expires: time.Now().Add(time.Hour)}, nil
		}
	})
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}

	if logins != 1 {
		t.Errorf("signed in %d times for one account", logins)
	}
	small := res.Models.Small.Model.Credential
	big := res.Models.Big.Model.Credential
	if small == "" || big != small {
		t.Errorf("big model credential = %q, small = %q — want the same login", big, small)
	}
	// And it says which login it used, rather than silently reusing one.
	if out := script.Output(); !strings.Contains(out, "from earlier in this run") {
		t.Errorf("the reused login is not named:\n%s", out)
	}
}

// A ChatGPT login and an OpenAI API key are the same vendor and not the same
// credential. Offering to reuse one as the other produces a 401 three questions
// later, in a place that reads like a bad key.
func TestALoginIsNotOfferedAsAnAPIKey(t *testing.T) {
	answers := codexAnswers() // small: ChatGPT. big: OpenRouter key.
	answers["models.small.chatgpt.how"] = "cli"

	opts, script, _ := newOpts(t, answers, withCodexLogin(t))
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	for _, id := range script.Asked {
		if id == "models.big.reuse" {
			t.Error("offered a ChatGPT login as the credential for a different sign-in")
		}
	}
}
