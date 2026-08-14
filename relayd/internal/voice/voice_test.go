package voice

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/llm"
)

// The two rows whose copy is the product decision, not decoration.

// SYSTEM.md §7c: "mute out of the box" is the worst possible first hour for a
// voice product, and the keyless fallback is what replaced the starter
// allowance entirely. A buyer who skips this step must still have a device that
// talks.
func TestKeylessFallbackExistsAndNeedsNoCredential(t *testing.T) {
	fb := Fallback()
	if fb.NeedsCredential {
		t.Fatal("the fallback needs a credential, which defeats the entire point of it")
	}
	if fb.Cost == "" || !strings.Contains(fb.Cost, "free") {
		t.Errorf("fallback cost = %q, want it free", fb.Cost)
	}
	if !strings.Contains(fb.Hint+fb.Note, "still talks") && !strings.Contains(fb.Hint+fb.Note, "Mute") {
		t.Errorf("the fallback row has to say why it exists, got %q / %q", fb.Hint, fb.Note)
	}

	// Exactly one keyless row, and it is the default fallback.
	n := 0
	for _, o := range Catalog() {
		if o.Keyless {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d keyless rows; there is one, and it is the safety net", n)
	}
	if DefaultPlan().Fallback != fb.ID {
		t.Errorf("default plan falls back to %q", DefaultPlan().Fallback)
	}
}

// ORCHESTRATOR.md §2a: phone-native is the FASTEST option and the worst
// sounding one. "Slower and worse" is wrong in a way a user notices in the
// first minute.
func TestPhoneNativeIsDescribedAsFastestAndWorst(t *testing.T) {
	p, ok := Get("phone")
	if !ok {
		t.Fatal("no phone-native row")
	}
	if p.Synthesis != SynthDevice {
		t.Errorf("synthesis = %q; it happens on the handset, which is why it is fastest", p.Synthesis)
	}
	if !strings.Contains(strings.ToLower(p.Latency), "fastest") {
		t.Errorf("latency = %q, want it named as the fastest option", p.Latency)
	}
	if strings.Contains(strings.ToLower(p.Latency), "slow") {
		t.Errorf("latency = %q; calling the phone slow is the specific error to avoid", p.Latency)
	}
	want := "free and instant, but it sounds like a robot in your ear all day"
	if p.Hint != want {
		t.Errorf("hint = %q, want ORCHESTRATOR.md §2a's exact line %q", p.Hint, want)
	}
	if p.Probeable {
		t.Error("this machine cannot test synthesis that happens on a handset")
	}
}

func TestSimbaIsTheRecommendation(t *testing.T) {
	r := Recommended()
	if r.ID != "speechify" {
		t.Fatalf("recommended = %q, want simba", r.ID)
	}
	if !strings.Contains(r.Cost, "$10") {
		t.Errorf("cost = %q, want $10 / M chars", r.Cost)
	}
	if !r.Streams {
		t.Error("Simba streams; that is the perceived-latency argument for it")
	}
	n := 0
	for _, o := range Catalog() {
		if o.Recommended {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d recommended rows, want exactly one", n)
	}
}

func TestCatalogCoversEveryRowTheDocLists(t *testing.T) {
	want := []string{"speechify", "elevenlabs", "cartesia", "deepgram", "openai", "openrouter", "edge", "phone", "local"}
	for _, id := range want {
		if _, ok := Get(id); !ok {
			t.Errorf("ORCHESTRATOR.md §2a lists %q and the catalog does not have it", id)
		}
	}
}

func TestPlanValidateRefusesAMuteConfiguration(t *testing.T) {
	if err := DefaultPlan().Validate(); err != nil {
		t.Fatalf("the default plan must be valid: %v", err)
	}

	var mute *ErrWouldBeMute
	if err := (Plan{Primary: "speechify"}).Validate(); !errors.As(err, &mute) {
		t.Errorf("a plan with no fallback must be refused, got %v", err)
	}
	if err := (Plan{Primary: "speechify", Fallback: "elevenlabs"}).Validate(); !errors.As(err, &mute) {
		t.Errorf("a fallback that needs a key is not a fallback, got %v", err)
	}
	if err := (Plan{Primary: "nope", Fallback: "edge"}).Validate(); err == nil {
		t.Error("an unknown primary should be rejected")
	}
}

// --------------------------------------------------------------- probing

type rt func(*http.Request) (*http.Response, error)

func (f rt) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func respond(status int, body string, req *http.Request) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
		Request:    req,
	}
}

func TestProbeSynthesisesAWord(t *testing.T) {
	var gotURL, gotAuth, gotBody string
	client := &http.Client{Transport: rt(func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return respond(200, "\xff\xfbID3 fake mp3 bytes", r), nil
	})}

	c := Probe(context.Background(), Config{
		Option:     "speechify",
		Credential: llm.CredentialRef{Kind: llm.RefInline, Value: "sk-test-key"},
		HTTPClient: client,
	})
	if !c.OK() {
		t.Fatalf("probe = %s", c)
	}
	if c.Bytes == 0 {
		t.Error("a working voice returns audio, and bytes are the proof")
	}
	// The endpoint that is documented and confirmed to work, not the one that
	// was aimed at a host which never resolved.
	if gotURL != "https://api.speechify.ai/v1/audio/stream" {
		t.Errorf("url = %q", gotURL)
	}
	// Speechify names the voice `voice_id`; `voice` is silently ignored, which
	// is how you ship a request that works and a voice nobody chose.
	if !strings.Contains(gotBody, `"voice_id"`) {
		t.Errorf("body = %q, want voice_id", gotBody)
	}
	if gotAuth != "Bearer sk-test-key" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if !strings.Contains(gotBody, ProbeWord) {
		t.Errorf("body = %q, want the probe word", gotBody)
	}
	// The reference is recorded; the secret never is.
	if strings.Contains(c.Ref, "sk-test-key") || strings.Contains(c.String(), "sk-test-key") {
		t.Fatalf("the secret leaked into the result: %q / %q", c.Ref, c.String())
	}
}

func TestProbeClassifiesFailures(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   llm.Reason
	}{
		{"bad key", 401, `{"error":{"message":"Invalid API key"}}`, llm.ReasonExpired},
		{"unpaid", 402, `{"message":"Balance exhausted"}`, llm.ReasonExpired},
		{"forbidden", 403, `{"message":"no"}`, llm.ReasonExpired},
		// The one failure mode a probe exists to prevent: a wrong model id must
		// not send the user to rotate a key that is fine.
		{"wrong model", 404, `{"error":{"message":"Unknown model simba-9"}}`, llm.ReasonUnavailable},
		{"rate limited", 429, `{"message":"slow down"}`, llm.ReasonUnavailable},
		{"outage", 503, ``, llm.ReasonUnavailable},
		{"200 with no audio", 200, ``, llm.ReasonUnavailable},
		{"200 with a json error", 200, `{"error":"quota"}`, llm.ReasonUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: rt(func(r *http.Request) (*http.Response, error) {
				return respond(tc.status, tc.body, r), nil
			})}
			c := Probe(context.Background(), Config{
				Option:     "speechify",
				Credential: llm.CredentialRef{Kind: llm.RefInline, Value: "k"},
				HTTPClient: client,
			})
			if c.Reason != tc.want {
				t.Fatalf("reason = %q, want %q (detail %q)", c.Reason, tc.want, c.Detail)
			}
			if tc.body != "" && c.Detail == "" {
				t.Error("the provider's own message has to survive to the user")
			}
		})
	}
}

func TestProbeReportsMissingAndUnresolvedCredentials(t *testing.T) {
	c := Probe(context.Background(), Config{Option: "speechify"})
	if c.Reason != llm.ReasonMissingCredential {
		t.Errorf("reason = %q, want missing_credential", c.Reason)
	}

	c = Probe(context.Background(), Config{
		Option:     "speechify",
		Credential: llm.CredentialRef{Kind: llm.RefEnv, Value: "RELAY_TEST_DEFINITELY_UNSET"},
	})
	if c.Reason != llm.ReasonUnresolvedRef {
		t.Errorf("reason = %q, want unresolved_ref", c.Reason)
	}
}

// A row this machine cannot exercise is reported as untested, never as ok.
// Claiming a verification that did not happen is the same mistake as an adapter
// emitting an event it did not observe.
func TestUntestableRowIsReportedAsUntested(t *testing.T) {
	c := Probe(context.Background(), Config{Option: "phone"})
	if c.Probed {
		t.Fatal("phone-native synthesis cannot be probed from this machine")
	}
	if c.OK() {
		t.Fatal("an untested row must never report ok")
	}
	if c.Verdict() != "not tested here" {
		t.Errorf("verdict = %q", c.Verdict())
	}
	if !strings.Contains(c.Detail, "handset") {
		t.Errorf("detail should say why: %q", c.Detail)
	}
}

func TestKeylessProbeNeedsNoCredentialAndSaysWhatItProved(t *testing.T) {
	client := &http.Client{Transport: rt(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" {
			t.Error("the keyless row must not send an Authorization header")
		}
		return respond(200, `[{"Name":"en-US-AriaNeural"}]`, r), nil
	})}
	c := Probe(context.Background(), Config{Option: "edge", HTTPClient: client})
	if !c.OK() {
		t.Fatalf("keyless probe = %s", c)
	}
	if !strings.Contains(c.Detail, "not exercised here") {
		t.Errorf("the keyless probe must not overclaim: %q", c.Detail)
	}
}

func TestLocalRowNeedsAnEndpointBeforeItCanBeProbed(t *testing.T) {
	c := Probe(context.Background(), Config{Option: "local"})
	if c.Probed {
		t.Fatal("there is nothing to call yet")
	}
	if !strings.Contains(c.Detail, "local endpoint") {
		t.Errorf("detail = %q", c.Detail)
	}
}

func TestProbePlanTestsTheFallbackToo(t *testing.T) {
	client := &http.Client{Transport: rt(func(r *http.Request) (*http.Response, error) {
		return respond(200, "audio", r), nil
	})}
	checks := ProbePlan(context.Background(),
		Config{Option: "speechify", Credential: llm.CredentialRef{Kind: llm.RefInline, Value: "k"}, HTTPClient: client},
		Config{Option: "edge", HTTPClient: client},
	)
	if len(checks) != 2 {
		t.Fatalf("got %d checks; a fallback nobody tested is a promise, not a safety net", len(checks))
	}
}

func TestEveryProviderBuildsARequest(t *testing.T) {
	for _, o := range Catalog() {
		if !o.Probeable || o.API == APILocal {
			continue
		}
		cfg := Config{Option: o.ID, Credential: llm.CredentialRef{Kind: llm.RefInline, Value: "k"}}
		req, err := buildRequest(context.Background(), o, cfg, o.BaseURL, "k")
		if err != nil {
			t.Errorf("%s: %v", o.ID, err)
			continue
		}
		if req.URL == nil || req.URL.Host == "" {
			t.Errorf("%s: no host in %v", o.ID, req.URL)
		}
	}
}
