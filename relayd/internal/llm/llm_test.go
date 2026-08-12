package llm_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/llm"
)

// stubTransport is the injection point. No test in this package opens a socket.
type stubTransport struct {
	status  int
	body    string
	ctype   string
	seen    []*http.Request
	seenRaw []string
}

func (s *stubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var raw []byte
	if r.Body != nil {
		raw, _ = io.ReadAll(r.Body)
	}
	s.seen = append(s.seen, r)
	s.seenRaw = append(s.seenRaw, string(raw))

	ct := s.ctype
	if ct == "" {
		ct = "application/json"
	}
	return &http.Response{
		StatusCode: s.status,
		Header:     http.Header{"Content-Type": []string{ct}},
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Request:    r,
	}, nil
}

func client(t *stubTransport) *http.Client { return &http.Client{Transport: t} }

func TestOpenAICompleteAndHeaders(t *testing.T) {
	tr := &stubTransport{status: 200, body: `{
		"model":"openai/gpt-5.6-luna",
		"choices":[{"message":{"role":"assistant","content":"tests pass"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`}

	p, err := llm.New(llm.Config{
		Vendor: "openrouter", Model: "openai/gpt-5.6-luna",
		Credential: llm.CredentialRef{Kind: llm.RefInline, Value: "sk-test-abcd"},
		HTTPClient: client(tr),
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.API() != llm.APIOpenAI {
		t.Fatalf("vendor default API is %s", p.API())
	}

	res, err := p.Complete(context.Background(), llm.Request{
		System:    "outcome first, no preamble",
		Messages:  []llm.Message{{Role: llm.RoleUser, Text: "summarise"}},
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if res.Text != "tests pass" || res.Usage.TotalTokens != 15 || res.FinishReason != "stop" {
		t.Fatalf("response: %+v", res)
	}

	req := tr.seen[0]
	if got := req.URL.String(); got != "https://openrouter.ai/api/v1/chat/completions" {
		t.Fatalf("URL is %s — the catalog base URL should have been used", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-test-abcd" {
		t.Fatalf("Authorization is %q", got)
	}
	if !strings.Contains(tr.seenRaw[0], `"role":"system"`) {
		t.Fatalf("the system prompt did not become a message: %s", tr.seenRaw[0])
	}
}

func TestAnthropicCompleteAndHeaders(t *testing.T) {
	tr := &stubTransport{status: 200, body: `{
		"model":"claude-x","content":[{"type":"text","text":"done"},{"type":"thinking","text":"hidden"}],
		"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`}

	p, err := llm.New(llm.Config{
		Vendor: "anthropic", Model: "claude-x",
		Credential: llm.CredentialRef{Kind: llm.RefInline, Value: "anthropic-test-key-1234"},
		HTTPClient: client(tr),
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.API() != llm.APIAnthropic {
		t.Fatalf("API is %s, want anthropic", p.API())
	}

	res, err := p.Complete(context.Background(), llm.Request{
		System:   "be brief",
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "done" {
		t.Fatalf("non-text content blocks must not be concatenated in: %q", res.Text)
	}
	if res.Usage.TotalTokens != 7 {
		t.Fatalf("usage: %+v", res.Usage)
	}

	req := tr.seen[0]
	if req.Header.Get("x-api-key") != "anthropic-test-key-1234" {
		t.Fatalf("x-api-key is %q", req.Header.Get("x-api-key"))
	}
	if req.Header.Get("anthropic-version") != llm.AnthropicVersion {
		t.Fatalf("anthropic-version is %q", req.Header.Get("anthropic-version"))
	}
	if !strings.Contains(tr.seenRaw[0], `"system":"be brief"`) {
		t.Fatalf("system belongs in its own field here: %s", tr.seenRaw[0])
	}
	if !strings.Contains(tr.seenRaw[0], `"max_tokens"`) {
		t.Fatal("the Messages API requires max_tokens")
	}
}

// Streaming is SYSTEM.md §7b's largest available latency win: time-to-first-
// audio becomes the model's first token rather than its last.
func TestOpenAIStream(t *testing.T) {
	tr := &stubTransport{status: 200, ctype: "text/event-stream", body: strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Tests "}}]}`,
		`data: {"choices":[{"delta":{"content":"pass."}}]}`,
		`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13}}`,
		`data: [DONE]`,
		"",
	}, "\n\n")}

	p, _ := llm.New(llm.Config{
		Vendor: "openrouter", Model: "m",
		Credential: llm.CredentialRef{Kind: llm.RefInline, Value: "k"},
		HTTPClient: client(tr),
	})
	s, err := p.Stream(context.Background(), llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Text: "go"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var text strings.Builder
	var usage *llm.Usage
	var done bool
	for {
		d, err := s.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		text.WriteString(d.Text)
		if d.Usage != nil {
			usage = d.Usage
		}
		if d.Done {
			done = true
			break
		}
	}
	if text.String() != "Tests pass." {
		t.Fatalf("streamed %q", text.String())
	}
	if !done {
		t.Fatal("the stream never reported Done")
	}
	if usage == nil || usage.TotalTokens != 13 {
		t.Fatalf("usage: %+v", usage)
	}
	if !strings.Contains(tr.seenRaw[0], `"stream":true`) {
		t.Fatal("stream flag was not set on the request")
	}
}

func TestAnthropicStream(t *testing.T) {
	tr := &stubTransport{status: 200, ctype: "text/event-stream", body: strings.Join([]string{
		"event: message_start\ndata: {\"type\":\"message_start\"}",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Two \"}}",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"files changed.\"}}",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":4,\"output_tokens\":6}}",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}",
		"",
	}, "\n\n")}

	p, _ := llm.New(llm.Config{
		Vendor: "anthropic", Model: "m",
		Credential: llm.CredentialRef{Kind: llm.RefInline, Value: "k"},
		HTTPClient: client(tr),
	})
	s, err := p.Stream(context.Background(), llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Text: "go"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var text strings.Builder
	for {
		d, err := s.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		text.WriteString(d.Text)
		if d.Done {
			break
		}
	}
	if text.String() != "Two files changed." {
		t.Fatalf("streamed %q", text.String())
	}
}

// ORCHESTRATOR.md §2: every credential is probed with one real call before the
// installer exits, and reports a stable reason code.
func TestProbeReasonCodes(t *testing.T) {
	inline := llm.CredentialRef{Kind: llm.RefInline, Value: "sk-test"}

	cases := []struct {
		name string
		cfg  llm.Config
		want llm.Reason
	}{
		{
			name: "ok",
			cfg: llm.Config{Vendor: "openrouter", Model: "m", Credential: inline,
				HTTPClient: client(&stubTransport{status: 200, body: `{"choices":[{"message":{"content":"pong"}}]}`})},
			want: llm.ReasonOK,
		},
		{
			name: "nothing configured at all",
			cfg: llm.Config{Vendor: "openrouter", Model: "m",
				HTTPClient: client(&stubTransport{status: 200, body: `{}`})},
			want: llm.ReasonMissingCredential,
		},
		{
			name: "env var is unset",
			cfg: llm.Config{Vendor: "openrouter", Model: "m",
				Credential: llm.CredentialRef{Kind: llm.RefEnv, Value: "RELAY_TEST_DEFINITELY_UNSET"},
				HTTPClient: client(&stubTransport{status: 200, body: `{}`})},
			want: llm.ReasonUnresolvedRef,
		},
		{
			name: "provider rejected the key",
			cfg: llm.Config{Vendor: "openrouter", Model: "m", Credential: inline,
				HTTPClient: client(&stubTransport{status: 401, body: `{"error":{"message":"invalid api key"}}`})},
			want: llm.ReasonExpired,
		},
		{
			name: "credit exhausted",
			cfg: llm.Config{Vendor: "openrouter", Model: "m", Credential: inline,
				HTTPClient: client(&stubTransport{status: 402, body: `{"error":"insufficient credits"}`})},
			want: llm.ReasonExpired,
		},
		{
			// A wrong model id is not an expired key, and telling the user to
			// rotate a working credential is worse than saying nothing.
			name: "model does not exist",
			cfg: llm.Config{Vendor: "openrouter", Model: "nope", Credential: inline,
				HTTPClient: client(&stubTransport{status: 404, body: `{"error":"no such model"}`})},
			want: llm.ReasonUnavailable,
		},
		{
			name: "rate limited",
			cfg: llm.Config{Vendor: "openrouter", Model: "m", Credential: inline,
				HTTPClient: client(&stubTransport{status: 429, body: `slow down`})},
			want: llm.ReasonUnavailable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := llm.New(tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			res := p.Probe(context.Background())
			if res.Reason != tc.want {
				t.Fatalf("reason %q, want %q (detail: %s)", res.Reason, tc.want, res.Detail)
			}
			if res.OK() != (tc.want == llm.ReasonOK) {
				t.Fatalf("OK() disagrees with the reason code")
			}
			// The provider's own words are what the installer prints.
			if tc.want != llm.ReasonOK && res.Detail == "" {
				t.Fatal("a failed probe must carry a detail")
			}
			// A probe must never leak the secret it tried.
			if strings.Contains(res.Ref, "sk-test") {
				t.Fatalf("the probe result carries the secret: %q", res.Ref)
			}
		})
	}
}

func TestProbeDoesNotLeakInlineSecrets(t *testing.T) {
	ref := llm.CredentialRef{Kind: llm.RefInline, Value: "sk-super-secret"}
	if s := ref.String(); strings.Contains(s, "secret") {
		t.Fatalf("CredentialRef.String() leaks: %q", s)
	}
	if s := (llm.CredentialRef{Kind: llm.RefEnv, Value: "OPENROUTER_API_KEY"}).String(); s != "env:OPENROUTER_API_KEY" {
		t.Fatalf("a reference should print as itself: %q", s)
	}
}

func TestCredentialRefResolution(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	t.Setenv("RELAY_TEST_KEY", "  from-env  ")
	got, err := (llm.CredentialRef{Kind: llm.RefEnv, Value: "RELAY_TEST_KEY"}).Resolve(ctx, nil)
	if err != nil || got != "from-env" {
		t.Fatalf("env: %q %v", got, err)
	}

	keyFile := filepath.Join(dir, "k")
	if err := os.WriteFile(keyFile, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = (llm.CredentialRef{Kind: llm.RefFile, Value: keyFile}).Resolve(ctx, nil)
	if err != nil || got != "from-file" {
		t.Fatalf("file: %q %v", got, err)
	}

	got, err = (llm.CredentialRef{Kind: llm.RefExec, Value: "printf from-exec"}).Resolve(ctx, nil)
	if err != nil || got != "from-exec" {
		t.Fatalf("exec: %q %v", got, err)
	}

	got, err = (llm.CredentialRef{Kind: llm.RefVault, Value: "cred-1"}).Resolve(ctx,
		func(context.Context, string) (string, error) { return "from-vault", nil })
	if err != nil || got != "from-vault" {
		t.Fatalf("vault: %q %v", got, err)
	}

	// Failures, each mapping onto a reason code.
	for name, ref := range map[string]llm.CredentialRef{
		"unset env":     {Kind: llm.RefEnv, Value: "RELAY_TEST_UNSET_XYZ"},
		"missing file":  {Kind: llm.RefFile, Value: filepath.Join(dir, "nope")},
		"failing exec":  {Kind: llm.RefExec, Value: "exit 3"},
		"silent exec":   {Kind: llm.RefExec, Value: "true"},
		"unwired vault": {Kind: llm.RefVault, Value: "cred-1"},
	} {
		if _, err := ref.Resolve(ctx, nil); !errors.Is(err, llm.ErrUnresolvedRef) {
			t.Fatalf("%s: got %v, want ErrUnresolvedRef", name, err)
		}
	}
	if _, err := (llm.CredentialRef{}).Resolve(ctx, nil); !errors.Is(err, llm.ErrMissingCredential) {
		t.Fatal("an empty reference is a missing credential, not an unresolved one")
	}
}

func TestParseRef(t *testing.T) {
	for in, want := range map[string]llm.CredentialRef{
		"env:OPENROUTER_API_KEY":  {Kind: llm.RefEnv, Value: "OPENROUTER_API_KEY"},
		"file:~/.config/relay/k":  {Kind: llm.RefFile, Value: "~/.config/relay/k"},
		"exec:op read op://x/y/z": {Kind: llm.RefExec, Value: "op read op://x/y/z"},
		"vault:abc":               {Kind: llm.RefVault, Value: "abc"},
		"OPENROUTER_API_KEY":      {Kind: llm.RefEnv, Value: "OPENROUTER_API_KEY"},
	} {
		got, err := llm.ParseRef(in)
		if err != nil || got != want {
			t.Fatalf("ParseRef(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	if _, err := llm.ParseRef(""); !errors.Is(err, llm.ErrMissingCredential) {
		t.Fatal("an empty string is a missing credential")
	}
	if _, err := llm.ParseRef("env:"); !errors.Is(err, llm.ErrUnresolvedRef) {
		t.Fatal("a prefix with no value should not resolve")
	}
}

// ORCHESTRATOR.md §2b: two models, both from the same grouped vendor list.
func TestPairProbesBoth(t *testing.T) {
	ok := func() *http.Client {
		return client(&stubTransport{status: 200, body: `{"choices":[{"message":{"content":"pong"}}]}`})
	}
	bad := client(&stubTransport{status: 401, body: `{"error":"nope"}`})

	pair, err := llm.NewPair(
		llm.Config{Vendor: "openrouter", Model: llm.SmallModelDefault,
			Credential: llm.CredentialRef{Kind: llm.RefInline, Value: "k"}, HTTPClient: ok()},
		llm.Config{Vendor: "openrouter", Model: llm.BigModelDefault,
			Credential: llm.CredentialRef{Kind: llm.RefInline, Value: "k"}, HTTPClient: bad},
	)
	if err != nil {
		t.Fatal(err)
	}
	res := pair.Probe(context.Background())
	if res["small"].Reason != llm.ReasonOK {
		t.Fatalf("small: %+v", res["small"])
	}
	if res["big"].Reason != llm.ReasonExpired {
		t.Fatalf("big: %+v", res["big"])
	}
	if pair.Small.Model() != llm.SmallModelDefault || pair.Big.Model() != llm.BigModelDefault {
		t.Fatal("the pair lost its models")
	}
}

func TestNewRejectsIncompleteConfigs(t *testing.T) {
	if _, err := llm.New(llm.Config{Vendor: "openrouter"}); !errors.Is(err, llm.ErrNoModel) {
		t.Fatalf("got %v, want ErrNoModel", err)
	}
	if _, err := llm.New(llm.Config{Vendor: "custom", Model: "m"}); err == nil {
		t.Fatal("a custom provider with no base URL must be refused")
	}
	// A custom provider WITH a base URL is fine — the list is never a cage.
	if _, err := llm.New(llm.Config{
		Vendor: "custom", Model: "m", BaseURL: "http://localhost:1234/v1",
		Credential: llm.CredentialRef{Kind: llm.RefInline, Value: "k"},
	}); err != nil {
		t.Fatalf("custom provider: %v", err)
	}
}

// ORCHESTRATOR.md §2b's four borrowed shapes, as data the installer renders.
func TestVendorCatalogShape(t *testing.T) {
	vendors := llm.Vendors()
	if len(vendors) < 5 {
		t.Fatalf("only %d vendors", len(vendors))
	}

	// OpenRouter is the recommendation, and there is exactly one.
	recommended := 0
	for _, v := range vendors {
		if v.Recommended {
			recommended++
			if v.ID != llm.RecommendedVendor {
				t.Fatalf("%s is recommended, want %s", v.ID, llm.RecommendedVendor)
			}
		}
	}
	if recommended != 1 {
		t.Fatalf("%d recommended vendors, want exactly 1", recommended)
	}

	// Custom Provider is always the last row.
	last := vendors[len(vendors)-1]
	if !last.Custom || last.ID != "custom" {
		t.Fatalf("the last row is %s, want the custom provider", last.ID)
	}
	for _, v := range vendors[:len(vendors)-1] {
		if v.Custom {
			t.Fatalf("%s is marked custom but is not last", v.ID)
		}
	}

	// Two levels: every group has a hint naming the auth methods behind it,
	// and at least one method to drill into.
	for _, v := range vendors {
		if v.Hint == "" {
			t.Fatalf("%s has no hint", v.ID)
		}
		if len(v.Auths) == 0 {
			t.Fatalf("%s has no auth methods", v.ID)
		}
		for _, a := range v.Auths {
			if a.ID == "" || a.Label == "" || a.Kind == "" {
				t.Fatalf("%s has an incomplete auth row: %+v", v.ID, a)
			}
		}
	}

	// Subscription auth is a first-class row where it exists.
	var subs int
	for _, v := range vendors {
		for _, a := range v.Auths {
			if a.Kind == llm.AuthSubscription {
				subs++
			}
		}
	}
	if subs == 0 {
		t.Fatal("no subscription rows; ChatGPT via Codex and the coding plans are usable")
	}

	// Risk is a hint on the row, not a wall: the option exists and carries its
	// warning.
	risky := 0
	for _, v := range vendors {
		for _, a := range v.Auths {
			if a.Risk != "" {
				risky++
			}
		}
	}
	if risky == 0 {
		t.Fatal("no risk hints; the unofficial Gemini CLI flow carries one")
	}

	// Claude has no supported subscription path, and the note says so rather
	// than the row being quietly omitted.
	anthropic, ok := llm.Vendor("anthropic")
	if !ok {
		t.Fatal("anthropic missing from the catalog")
	}
	for _, a := range anthropic.Auths {
		if a.Kind == llm.AuthSubscription {
			t.Fatal("Anthropic must not ship a subscription row; there is no supported path")
		}
	}
	if !strings.Contains(anthropic.Note, "Claude Code") {
		t.Fatalf("the Anthropic note must pre-empt the Claude Max confusion: %q", anthropic.Note)
	}

	if _, ok := llm.Vendor("nonesuch"); ok {
		t.Fatal("Vendor found something that is not in the catalog")
	}
}

func TestReasonsAreStable(t *testing.T) {
	want := map[llm.Reason]bool{
		"ok": true, "missing_credential": true, "expired": true,
		"unresolved_ref": true, "unavailable": true,
	}
	for _, r := range llm.Reasons() {
		if !want[r] {
			t.Fatalf("unexpected reason code %q — these are a contract", r)
		}
		delete(want, r)
	}
	if len(want) != 0 {
		t.Fatalf("missing reason codes: %v", want)
	}
}
