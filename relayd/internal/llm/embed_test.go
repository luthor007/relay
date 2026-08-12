package llm_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/store"
)

// EmbeddingDims is duplicated in three packages so each stays a leaf. This is
// what makes the duplication safe: if store's width ever moves, the client that
// talks to the model finds out here rather than at the first write.
func TestEmbeddingDimsMirrorsTheStore(t *testing.T) {
	if llm.EmbeddingDims != store.EmbeddingDims {
		t.Fatalf("llm.EmbeddingDims is %d and store.EmbeddingDims is %d — a vec0 column's "+
			"width is fixed at create time, so these cannot disagree",
			llm.EmbeddingDims, store.EmbeddingDims)
	}
}

// ------------------------------------------------------------- test rigging --

// routeTransport answers by URL path, so a test can say "this endpoint 404s and
// that one works" — which is exactly the old-Ollama case. No socket is opened.
type routeTransport struct {
	routes map[string]stubResponse
	// dynamic, when set, answers from the request body as well as the path —
	// which is what a "reject this field, accept it without" endpoint needs.
	dynamic func(path, body string) stubResponse
	calls   []string
	bodies  []string
	auth    []string
}

type stubResponse struct {
	status int
	body   string
}

func (r *routeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var raw []byte
	if req.Body != nil {
		raw, _ = io.ReadAll(req.Body)
	}
	r.calls = append(r.calls, req.URL.Path)
	r.bodies = append(r.bodies, string(raw))
	r.auth = append(r.auth, req.Header.Get("Authorization"))

	resp, ok := r.routes[req.URL.Path]
	if r.dynamic != nil {
		resp, ok = r.dynamic(req.URL.Path, string(raw)), true
	}
	if !ok {
		resp = stubResponse{status: 404, body: `{"error":"404 page not found"}`}
	}
	return &http.Response{
		StatusCode: resp.status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(resp.body)),
		Request:    req,
	}, nil
}

func (r *routeTransport) count(path string) int {
	n := 0
	for _, c := range r.calls {
		if c == path {
			n++
		}
	}
	return n
}

func routed(routes map[string]stubResponse) (*routeTransport, *http.Client) {
	tr := &routeTransport{routes: routes}
	return tr, &http.Client{Transport: tr}
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "embed", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return string(b)
}

// ollamaBody builds an /api/embed response of n vectors, each dims wide. The
// width tests need 768 and 384 specifically and checking in two files of that
// size to compare two integers would be silly.
func ollamaBody(n, dims int, value float64) string {
	var vecs []string
	for i := 0; i < n; i++ {
		parts := make([]string, dims)
		for j := range parts {
			parts[j] = fmt.Sprintf("%g", value)
		}
		vecs = append(vecs, "["+strings.Join(parts, ",")+"]")
	}
	return `{"model":"m","embeddings":[` + strings.Join(vecs, ",") + `]}`
}

func unitLength(t *testing.T, v []float32) {
	t.Helper()
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	// summary_vec is an L2 index and L2 distance is monotonic in cosine only on
	// unit vectors, so this is what makes "cosine" in MEMORY.md §3 true.
	if math.Abs(sum-1) > 1e-5 {
		t.Fatalf("vector is not unit length: %v", sum)
	}
}

// ------------------------------------------------------------------ ollama --

func TestOllamaEmbedsBatchedAndNormalises(t *testing.T) {
	tr, c := routed(map[string]stubResponse{
		"/api/embed": {status: 200, body: fixture(t, "ollama_embed.json")},
	})
	e, err := llm.NewEmbedder(llm.EmbedConfig{
		Provider: llm.EmbedOllama, Model: llm.DefaultLocalEmbedModel, Dims: 8, HTTPClient: c,
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.Model() != llm.DefaultLocalEmbedModel || e.Dims() != 8 {
		t.Fatalf("%s/%d", e.Model(), e.Dims())
	}

	vecs, err := e.Embed(context.Background(), []string{"the payments branch", "the glasses firmware"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(vecs) != 2 {
		t.Fatalf("got %d vectors", len(vecs))
	}
	for _, v := range vecs {
		unitLength(t, v)
	}
	// Order is the contract: vector i belongs to text i, and a swap here writes
	// every summary against the wrong vector.
	if vecs[0][0] <= 0 || vecs[1][0] >= 0 {
		t.Fatalf("the two vectors came back in the wrong order: %v %v", vecs[0][:2], vecs[1][:2])
	}

	// One request for two texts. 22,000 summaries is the reason this matters.
	if tr.count("/api/embed") != 1 {
		t.Fatalf("%d requests for one batch", tr.count("/api/embed"))
	}
	if !strings.Contains(tr.bodies[0], `"input":["the payments branch","the glasses firmware"]`) {
		t.Fatalf("request body: %s", tr.bodies[0])
	}
}

// The local runtime has no credential, and the config shape has to allow that
// rather than treat it as an error state.
func TestOllamaNeedsNoCredential(t *testing.T) {
	tr, c := routed(map[string]stubResponse{
		"/api/embed": {status: 200, body: fixture(t, "ollama_embed.json")},
	})
	e, err := llm.NewEmbedder(llm.EmbedConfig{
		Provider: llm.EmbedOllama, Model: llm.DefaultLocalEmbedModel, Dims: 8, HTTPClient: c,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Embed(context.Background(), []string{"one", "two"}); err != nil {
		t.Fatalf("a keyless provider must not need a key: %v", err)
	}
	if tr.auth[0] != "" {
		t.Fatalf("an Authorization header was sent to the local runtime: %q", tr.auth[0])
	}

	// And the probe passes with no credential at all, rather than reporting a
	// missing one. "No credential" is the local runtime's normal state.
	tr2, c2 := routed(map[string]stubResponse{
		"/api/embed": {status: 200, body: ollamaBody(1, 8, 0.5)},
	})
	p, err := llm.NewEmbedder(llm.EmbedConfig{
		Provider: llm.EmbedOllama, Model: llm.DefaultLocalEmbedModel, Dims: 8, HTTPClient: c2,
	})
	if err != nil {
		t.Fatal(err)
	}
	chk := p.Probe(context.Background())
	if !chk.OK() {
		t.Fatalf("probe: %s", chk)
	}
	if chk.Ref != "" {
		t.Fatalf("a keyless provider should report no reference: %q", chk.Ref)
	}
	if tr2.auth[0] != "" {
		t.Fatalf("the probe sent an Authorization header: %q", tr2.auth[0])
	}
}

// An Ollama that predates /api/embed answers 404 there. That must not be a bare
// 404 out of setup, and it must cost one wasted request per process rather than
// one per batch.
func TestOllamaFallsBackToTheLegacyEndpointAndRemembers(t *testing.T) {
	tr, c := routed(map[string]stubResponse{
		"/api/embed":      {status: 404, body: fixture(t, "ollama_embed_404.json")},
		"/api/embeddings": {status: 200, body: fixture(t, "ollama_embeddings_legacy.json")},
	})
	e, err := llm.NewEmbedder(llm.EmbedConfig{
		Provider: llm.EmbedOllama, Model: llm.DefaultLocalEmbedModel, Dims: 8, HTTPClient: c,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	vecs, err := e.Embed(ctx, []string{"one", "two"})
	if err != nil {
		t.Fatalf("the old endpoint should have carried this: %v", err)
	}
	if len(vecs) != 2 {
		t.Fatalf("got %d vectors", len(vecs))
	}
	unitLength(t, vecs[0])
	// The old endpoint is single-input, so a batch of two is two requests.
	if got := tr.count("/api/embeddings"); got != 2 {
		t.Fatalf("%d legacy requests for two texts", got)
	}
	if !strings.Contains(tr.bodies[1], `"prompt":"one"`) {
		t.Fatalf("the legacy request uses prompt, not input: %s", tr.bodies[1])
	}

	if _, err := e.Embed(ctx, []string{"three"}); err != nil {
		t.Fatal(err)
	}
	// Sticky: the second call does not re-probe the endpoint that 404s.
	if got := tr.count("/api/embed"); got != 1 {
		t.Fatalf("/api/embed was tried %d times; the fallback must be remembered", got)
	}
}

// A 404 that is not about the endpoint — the model was never pulled — must not
// be read as an old daemon and must reach the user as what it is.
func TestOllamaMissingModelIsReported(t *testing.T) {
	_, c := routed(map[string]stubResponse{
		"/api/embed":      {status: 404, body: fixture(t, "ollama_model_missing.json")},
		"/api/embeddings": {status: 404, body: fixture(t, "ollama_model_missing.json")},
	})
	e, err := llm.NewEmbedder(llm.EmbedConfig{
		Provider: llm.EmbedOllama, Model: llm.DefaultLocalEmbedModel, Dims: 8, HTTPClient: c,
	})
	if err != nil {
		t.Fatal(err)
	}
	chk := e.Probe(context.Background())
	if chk.OK() {
		t.Fatal("a model that has not been pulled is not a working embedder")
	}
	if chk.Reason != llm.ReasonUnavailable {
		t.Fatalf("reason %q", chk.Reason)
	}
	if !strings.Contains(chk.Detail, "not found") {
		t.Fatalf("the provider's own words are what the installer prints: %q", chk.Detail)
	}
	if !strings.Contains(chk.Advice(), "pulled") {
		t.Fatalf("advice for a local runtime should point at the runtime: %q", chk.Advice())
	}
}

// The real width, end to end, from a recorded-shape body.
func TestOllamaFullWidthFixture(t *testing.T) {
	_, c := routed(map[string]stubResponse{
		"/api/embed": {status: 200, body: fixture(t, "ollama_nomic_768.json")},
	})
	e, err := llm.NewEmbedder(llm.EmbedConfig{
		Provider: llm.EmbedOllama, Model: llm.DefaultLocalEmbedModel, HTTPClient: c,
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.Dims() != store.EmbeddingDims {
		t.Fatalf("default width is %d", e.Dims())
	}
	vecs, err := e.Embed(context.Background(), []string{"the payments branch"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(vecs[0]) != store.EmbeddingDims {
		t.Fatalf("width %d", len(vecs[0]))
	}
	unitLength(t, vecs[0])

	chk := e.Probe(context.Background())
	if !chk.OK() {
		t.Fatalf("probe: %s", chk)
	}
	if chk.Dims != store.EmbeddingDims || chk.WantDims != store.EmbeddingDims {
		t.Fatalf("probe widths: %d/%d", chk.Dims, chk.WantDims)
	}
	// This model does not pre-normalise, which is precisely why the client does.
	if chk.PreNormalised {
		t.Fatalf("the fixture is not unit length; norm was %v", chk.Norm)
	}
	if chk.Norm <= 0 {
		t.Fatalf("norm %v", chk.Norm)
	}
}

// ------------------------------------------------------------------ hosted --

func TestOpenAIEmbedSortsByIndexAndAsksForTheWidth(t *testing.T) {
	tr, c := routed(map[string]stubResponse{
		"/api/v1/embeddings": {status: 200, body: fixture(t, "openai_embeddings.json")},
	})
	e, err := llm.NewEmbedder(llm.EmbedConfig{
		Provider: llm.EmbedOpenAI, Vendor: "openrouter",
		Model: "openai/text-embedding-3-small", Dims: 8,
		Credential: llm.CredentialRef{Kind: llm.RefInline, Value: "sk-test-abcd"},
		HTTPClient: c,
	})
	if err != nil {
		t.Fatal(err)
	}

	vecs, err := e.Embed(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	// The fixture returns index 1 before index 0. Pairing a vector with the
	// wrong summary would be invisible in every test and wrong in every search.
	if vecs[0][0] <= 0 {
		t.Fatalf("the response was not re-ordered by index: %v", vecs[0][:2])
	}
	for _, v := range vecs {
		unitLength(t, v)
	}

	if tr.auth[0] != "Bearer sk-test-abcd" {
		t.Fatalf("Authorization is %q", tr.auth[0])
	}
	// Nothing hosted is natively 768, so the dimensions field is what makes the
	// hosted option able to write to this index at all.
	if !strings.Contains(tr.bodies[0], `"dimensions":8`) {
		t.Fatalf("dimensions was not requested: %s", tr.bodies[0])
	}
	if !strings.Contains(tr.bodies[0], `"encoding_format":"float"`) {
		t.Fatalf("encoding_format must be explicit: %s", tr.bodies[0])
	}
}

// An OpenAI-compatible endpoint that does not implement `dimensions` rejects
// the body. Dropping the field and retrying is what keeps a perfectly good
// model id from being reported as an unavailable provider — and remembering the
// refusal is what keeps it from costing a wasted request on every one of 22,000
// summaries.
func TestOpenAIDropsDimensionsWhenRefusedAndRemembers(t *testing.T) {
	calls := 0
	tr := &routeTransport{routes: map[string]stubResponse{}}
	tr.dynamic = func(path, body string) stubResponse {
		calls++
		if strings.Contains(body, "dimensions") {
			return stubResponse{status: 400, body: `{"error":{"message":"unknown field: dimensions"}}`}
		}
		return stubResponse{status: 200, body: fixture(t, "openai_embeddings.json")}
	}
	e, err := llm.NewEmbedder(llm.EmbedConfig{
		Provider: llm.EmbedOpenAI, BaseURL: "https://example.invalid/v1",
		Model: "some-open-model", Dims: 8,
		Credential: llm.CredentialRef{Kind: llm.RefInline, Value: "k"},
		HTTPClient: &http.Client{Transport: tr},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := e.Embed(ctx, []string{"a", "b"}); err != nil {
		t.Fatalf("the retry should have carried this: %v", err)
	}
	if calls != 2 {
		t.Fatalf("%d calls; want one refused and one retried", calls)
	}
	if !strings.Contains(tr.bodies[0], `"dimensions":8`) || strings.Contains(tr.bodies[1], "dimensions") {
		t.Fatalf("bodies: %s / %s", tr.bodies[0], tr.bodies[1])
	}

	if _, err := e.Embed(ctx, []string{"c", "d"}); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("%d calls; the refusal must be remembered rather than re-tried per batch", calls)
	}
}

// An auth failure is never retried without the field: the second 401 tells
// nobody anything, and a rate limit gets worse when you immediately ask again.
func TestOpenAIDoesNotRetryOnAuthOrRateLimit(t *testing.T) {
	for name, resp := range map[string]stubResponse{
		"unauthorized":  {status: 401, body: `{"error":{"message":"invalid api key"}}`},
		"rate limited":  {status: 429, body: `slow down`},
		"no such model": {status: 404, body: `{"error":{"message":"no such model"}}`},
	} {
		t.Run(name, func(t *testing.T) {
			tr, c := routed(map[string]stubResponse{"/v1/embeddings": resp})
			e, _ := llm.NewEmbedder(llm.EmbedConfig{
				Provider: llm.EmbedOpenAI, BaseURL: "https://example.invalid/v1",
				Model: "m", Dims: 8,
				Credential: llm.CredentialRef{Kind: llm.RefInline, Value: "k"},
				HTTPClient: c,
			})
			if _, err := e.Embed(context.Background(), []string{"a"}); err == nil {
				t.Fatal("want an error")
			}
			if got := tr.count("/v1/embeddings"); got != 1 {
				t.Fatalf("%d calls; this is not a body the provider refused", got)
			}
		})
	}
}

// -------------------------------------------------------- batching, blanks --

func TestEmbedBatchesAtBatchSize(t *testing.T) {
	tr, c := routed(map[string]stubResponse{
		"/api/embed": {status: 200, body: ollamaBody(2, 8, 0.5)},
	})
	e, err := llm.NewEmbedder(llm.EmbedConfig{
		Provider: llm.EmbedOllama, Model: "m", Dims: 8, BatchSize: 2, HTTPClient: c,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Four texts at a batch size of two is two requests, and a provider that
	// returns the wrong count for the last batch is an error rather than a
	// silently short result.
	if _, err := e.Embed(context.Background(), []string{"a", "b", "c", "d"}); err != nil {
		t.Fatal(err)
	}
	if got := tr.count("/api/embed"); got != 2 {
		t.Fatalf("%d requests for four texts at batch size 2", got)
	}
}

func TestEmbedRefusesAShortAnswer(t *testing.T) {
	_, c := routed(map[string]stubResponse{
		"/api/embed": {status: 200, body: ollamaBody(1, 8, 0.5)},
	})
	e, _ := llm.NewEmbedder(llm.EmbedConfig{
		Provider: llm.EmbedOllama, Model: "m", Dims: 8, HTTPClient: c,
	})
	_, err := e.Embed(context.Background(), []string{"a", "b"})
	if err == nil || !strings.Contains(err.Error(), "2 inputs") {
		t.Fatalf("a provider that drops an input must not be papered over: %v", err)
	}
}

// A summary can legitimately be empty — a turn with no observable events.
// Sending "" is a 400 on most providers and a wasted call on the rest.
func TestBlankTextNeverReachesTheProvider(t *testing.T) {
	tr, c := routed(map[string]stubResponse{
		"/api/embed": {status: 200, body: ollamaBody(1, 8, 0.5)},
	})
	e, _ := llm.NewEmbedder(llm.EmbedConfig{
		Provider: llm.EmbedOllama, Model: "m", Dims: 8, HTTPClient: c,
	})
	vecs, err := e.Embed(context.Background(), []string{"", "real", "   "})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(vecs) != 3 {
		t.Fatalf("got %d vectors", len(vecs))
	}
	if !strings.Contains(tr.bodies[0], `"input":["real"]`) {
		t.Fatalf("a blank text was sent: %s", tr.bodies[0])
	}
	for _, i := range []int{0, 2} {
		if len(vecs[i]) != 8 {
			t.Fatalf("blank vector %d is %d wide", i, len(vecs[i]))
		}
		for _, x := range vecs[i] {
			if x != 0 || math.IsNaN(float64(x)) {
				t.Fatalf("a blank text must be zeros, not NaNs: %v", vecs[i])
			}
		}
	}
	if len(vecs[1]) != 8 {
		t.Fatalf("the real vector is %d wide", len(vecs[1]))
	}
}

func TestEmbedNoTextsMakesNoCall(t *testing.T) {
	tr, c := routed(map[string]stubResponse{})
	e, _ := llm.NewEmbedder(llm.EmbedConfig{
		Provider: llm.EmbedOllama, Model: "m", Dims: 8, HTTPClient: c,
	})
	v, err := e.Embed(context.Background(), nil)
	if err != nil || len(v) != 0 {
		t.Fatalf("%v %v", v, err)
	}
	if len(tr.calls) != 0 {
		t.Fatalf("an empty batch made %d requests", len(tr.calls))
	}
}

// The width check is not only in the probe. A model that changes its answer
// mid-backfill must not write a wrong-width vector.
func TestEmbedRefusesAWrongWidthOnEveryCall(t *testing.T) {
	_, c := routed(map[string]stubResponse{
		"/api/embed": {status: 200, body: ollamaBody(1, 384, 0.5)},
	})
	e, _ := llm.NewEmbedder(llm.EmbedConfig{
		Provider: llm.EmbedOllama, Model: "all-minilm", HTTPClient: c,
	})
	_, err := e.Embed(context.Background(), []string{"anything"})
	if !errors.Is(err, llm.ErrEmbedDims) {
		t.Fatalf("got %v, want ErrEmbedDims", err)
	}
	for _, want := range []string{"all-minilm", "384", "768"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error must name %q: %v", want, err)
		}
	}
}

// -------------------------------------------------------------- the probe --

func TestEmbedProbeReasonCodes(t *testing.T) {
	inline := llm.CredentialRef{Kind: llm.RefInline, Value: "sk-test"}
	okBody := ollamaBody(1, 768, 0.03)

	cases := []struct {
		name string
		cfg  llm.EmbedConfig
		want llm.Reason
	}{
		{
			name: "ok",
			cfg: llm.EmbedConfig{Provider: llm.EmbedOllama, Model: "nomic-embed-text",
				HTTPClient: mustClient(map[string]stubResponse{"/api/embed": {200, okBody}})},
			want: llm.ReasonOK,
		},
		{
			name: "hosted with nothing configured at all",
			cfg: llm.EmbedConfig{Provider: llm.EmbedOpenAI, Vendor: "openrouter", Model: "e",
				HTTPClient: mustClient(map[string]stubResponse{"/api/v1/embeddings": {200, okBody}})},
			want: llm.ReasonMissingCredential,
		},
		{
			name: "env var is unset",
			cfg: llm.EmbedConfig{Provider: llm.EmbedOpenAI, Vendor: "openrouter", Model: "e",
				Credential: llm.CredentialRef{Kind: llm.RefEnv, Value: "RELAY_TEST_DEFINITELY_UNSET"},
				HTTPClient: mustClient(map[string]stubResponse{"/api/v1/embeddings": {200, okBody}})},
			want: llm.ReasonUnresolvedRef,
		},
		{
			name: "provider rejected the key",
			cfg: llm.EmbedConfig{Provider: llm.EmbedOpenAI, Vendor: "openrouter", Model: "e",
				Credential: inline,
				HTTPClient: mustClient(map[string]stubResponse{
					"/api/v1/embeddings": {401, fixture(t, "openai_unauthorized.json")}})},
			want: llm.ReasonExpired,
		},
		{
			// A chat model on the embeddings endpoint is not an expired key, and
			// telling somebody to rotate a working credential is the one failure
			// a probe exists to prevent. It survives the dimensions retry too:
			// dropping the field does not turn a chat model into an embedder.
			name: "a chat model was named",
			cfg: llm.EmbedConfig{Provider: llm.EmbedOpenAI, Vendor: "openrouter", Model: "gpt-5.6-luna",
				Credential: inline,
				HTTPClient: mustClient(map[string]stubResponse{
					"/api/v1/embeddings": {400, fixture(t, "openai_chat_model_refused.json")}})},
			want: llm.ReasonUnavailable,
		},
		{
			name: "the wrong width",
			cfg: llm.EmbedConfig{Provider: llm.EmbedOllama, Model: "all-minilm",
				HTTPClient: mustClient(map[string]stubResponse{"/api/embed": {200, ollamaBody(1, 384, 0.5)}})},
			want: llm.ReasonWrongWidth,
		},
		{
			name: "the right width and no information",
			cfg: llm.EmbedConfig{Provider: llm.EmbedOllama, Model: "nomic-embed-text",
				HTTPClient: mustClient(map[string]stubResponse{"/api/embed": {200, ollamaBody(1, 768, 0)}})},
			want: llm.ReasonDegenerate,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chk := llm.ProbeEmbedConfig(context.Background(), tc.cfg)
			if chk.Reason != tc.want {
				t.Fatalf("reason %q, want %q (detail: %s)", chk.Reason, tc.want, chk.Detail)
			}
			if chk.OK() != (tc.want == llm.ReasonOK) {
				t.Fatal("OK() disagrees with the reason code")
			}
			if tc.want != llm.ReasonOK {
				if chk.Detail == "" {
					t.Fatal("a failed probe must carry a detail")
				}
				if chk.Advice() == "" {
					t.Fatal("a failed probe must say what to do about it")
				}
			}
			if strings.Contains(chk.Ref, "sk-test") || strings.Contains(chk.String(), "sk-test") {
				t.Fatalf("the probe leaked the secret: %q / %q", chk.Ref, chk.String())
			}
		})
	}
}

// The whole reason this is a probe and not a runtime check: a 384-dimension
// model must fail at setup, named, with both numbers — not at the first search
// after a two-hour backfill.
func TestProbeNamesTheModelAndBothNumbers(t *testing.T) {
	chk := llm.ProbeEmbedConfig(context.Background(), llm.EmbedConfig{
		Provider: llm.EmbedOllama, Model: "all-minilm",
		HTTPClient: mustClient(map[string]stubResponse{"/api/embed": {200, ollamaBody(1, 384, 0.5)}}),
	})
	if chk.Reason != llm.ReasonWrongWidth {
		t.Fatalf("reason %q", chk.Reason)
	}
	if chk.Dims != 384 || chk.WantDims != 768 {
		t.Fatalf("widths %d/%d", chk.Dims, chk.WantDims)
	}
	for _, want := range []string{"all-minilm", "384", "768", "vec0"} {
		if !strings.Contains(chk.Detail, want) {
			t.Fatalf("the detail must name %q: %q", want, chk.Detail)
		}
	}
	if !strings.Contains(chk.Advice(), llm.DefaultLocalEmbedModel) {
		t.Fatalf("the advice should name a model that works: %q", chk.Advice())
	}
}

// A model that has not finished loading returns the right number of zeros. That
// is worse than an outage, because an outage is visible: zeros are equidistant
// from every query, so the dense half would return the same arbitrary documents
// forever.
func TestProbeCatchesADegenerateVector(t *testing.T) {
	chk := llm.ProbeEmbedConfig(context.Background(), llm.EmbedConfig{
		Provider: llm.EmbedOllama, Model: "nomic-embed-text",
		HTTPClient: mustClient(map[string]stubResponse{"/api/embed": {200, ollamaBody(1, 768, 0)}}),
	})
	if chk.Reason != llm.ReasonDegenerate {
		t.Fatalf("reason %q", chk.Reason)
	}
	if !strings.Contains(chk.Detail, "zero") {
		t.Fatalf("detail %q", chk.Detail)
	}
	if chk.Dims != 768 {
		t.Fatalf("the width was right and that is the point: %d", chk.Dims)
	}
}

func TestEmbedReasonsIncludeTheSharedFive(t *testing.T) {
	have := map[llm.Reason]bool{}
	for _, r := range llm.EmbedReasons() {
		if have[r] {
			t.Fatalf("duplicate reason code %q", r)
		}
		have[r] = true
	}
	for _, r := range llm.Reasons() {
		if !have[r] {
			t.Fatalf("embedding probes must speak the same vocabulary: %q is missing", r)
		}
	}
	for _, r := range []llm.Reason{llm.ReasonWrongWidth, llm.ReasonDegenerate} {
		if !have[r] {
			t.Fatalf("%q is missing", r)
		}
	}
}

// ----------------------------------------------------------- the catalogue --

func TestLocalEmbedCatalogIsDataAndRefusesTheWrongWidth(t *testing.T) {
	models := llm.LocalEmbedModels()
	if len(models) < 3 {
		t.Fatalf("only %d local models; the list has to offer an alternative", len(models))
	}

	recommended := 0
	for _, m := range models {
		if m.ID == "" || m.Label == "" || m.Dims == 0 {
			t.Fatalf("incomplete row: %+v", m)
		}
		if m.Recommended {
			recommended++
			if m.ID != llm.DefaultLocalEmbedModel {
				t.Fatalf("%s is recommended, want %s", m.ID, llm.DefaultLocalEmbedModel)
			}
			if m.Dims != store.EmbeddingDims {
				t.Fatalf("the default must be %d dimensions, not %d", store.EmbeddingDims, m.Dims)
			}
		}
	}
	if recommended != 1 {
		t.Fatalf("%d recommended local models, want exactly 1", recommended)
	}

	// At least one alternative that actually fits, so the recommendation is a
	// recommendation and not the only row.
	fits := 0
	for _, m := range models {
		if m.FitsIndex() {
			fits++
		}
	}
	if fits < 2 {
		t.Fatalf("only %d usable local models", fits)
	}

	// And the refusal is at selection time, before a download, naming both
	// numbers and the reason.
	_, err := llm.SelectLocalEmbedModel("all-minilm")
	if !errors.Is(err, llm.ErrEmbedDims) {
		t.Fatalf("got %v, want ErrEmbedDims", err)
	}
	for _, want := range []string{"384", "768", "vec0", llm.DefaultLocalEmbedModel} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal must say %q: %v", want, err)
		}
	}
	if _, err := llm.SelectLocalEmbedModel("mxbai-embed-large"); !errors.Is(err, llm.ErrEmbedDims) {
		t.Fatalf("1024 dimensions must be refused too: %v", err)
	}
	if _, err := llm.SelectLocalEmbedModel(llm.DefaultLocalEmbedModel); err != nil {
		t.Fatalf("the default must be selectable: %v", err)
	}

	// The list is a menu, not a cage: a model nobody has listed is allowed
	// through to the probe, which is what actually decides.
	m, err := llm.SelectLocalEmbedModel("something-nobody-listed")
	if err != nil || m.ID != "something-nobody-listed" {
		t.Fatalf("%+v %v", m, err)
	}

	if llm.RecommendedLocalEmbed().ID != llm.DefaultLocalEmbedModel {
		t.Fatal("RecommendedLocalEmbed disagrees with the default")
	}
	if _, ok := llm.LocalEmbedModel("nonesuch"); ok {
		t.Fatal("LocalEmbedModel found something that is not in the list")
	}
}

// Nothing hosted is natively 768, so every usable hosted row has to be one that
// can be asked for a narrower vector. If that ever stops being true the hosted
// option stops working, and it should stop here.
func TestHostedEmbedCatalogNeedsTruncation(t *testing.T) {
	for _, m := range llm.HostedEmbedModels() {
		if m.Vendor == "" {
			t.Fatalf("hosted row %s has no vendor", m.ID)
		}
		if _, ok := llm.Vendor(m.Vendor); !ok {
			t.Fatalf("%s names vendor %q, which is not in the §2b catalog", m.ID, m.Vendor)
		}
		if !m.FitsIndex() {
			t.Fatalf("%s is %d wide and cannot reach %d", m.ID, m.Dims, store.EmbeddingDims)
		}
		cfg := llm.EmbedConfigFor(m)
		if cfg.Provider != llm.EmbedOpenAI || cfg.Vendor != m.Vendor || cfg.Dims != store.EmbeddingDims {
			t.Fatalf("EmbedConfigFor(%s): %+v", m.ID, cfg)
		}
	}

	local := llm.EmbedConfigFor(llm.RecommendedLocalEmbed())
	if local.Provider != llm.EmbedOllama || local.Vendor != "" {
		t.Fatalf("the local default: %+v", local)
	}
}

// config.toml carries one `provider` value, and this is where it is read: the
// local runtime, a §2b vendor id, or nothing at all.
func TestEmbedProviderFor(t *testing.T) {
	for in, want := range map[string]struct {
		shape  llm.EmbedProvider
		vendor string
	}{
		"ollama":     {llm.EmbedOllama, ""},
		"openrouter": {llm.EmbedOpenAI, "openrouter"},
		"openai":     {llm.EmbedOpenAI, "openai"},
		"none":       {llm.EmbedNone, ""},
		"":           {llm.EmbedNone, ""},
	} {
		shape, vendor := llm.EmbedProviderFor(in)
		if shape != want.shape || vendor != want.vendor {
			t.Fatalf("EmbedProviderFor(%q) = %q/%q, want %q/%q", in, shape, vendor, want.shape, want.vendor)
		}
	}
}

// ---------------------------------------------------------- constructing it --

func TestNewEmbedderRejections(t *testing.T) {
	if _, err := llm.NewEmbedder(llm.EmbedConfig{Provider: llm.EmbedOllama}); !errors.Is(err, llm.ErrNoEmbedModel) {
		t.Fatalf("got %v, want ErrNoEmbedModel", err)
	}
	// "none" is a supported state, and it is reported as one rather than as a
	// broken config: search runs lexical-only and says so.
	if _, err := llm.NewEmbedder(llm.EmbedConfig{Provider: llm.EmbedNone, Model: "x"}); !errors.Is(err, llm.ErrNoEmbedder) {
		t.Fatalf("got %v, want ErrNoEmbedder", err)
	}
	// Anthropic has no embeddings endpoint. Saying so beats a 404 later.
	_, err := llm.NewEmbedder(llm.EmbedConfig{Provider: llm.EmbedOpenAI, Vendor: "anthropic", Model: "x"})
	if err == nil || !strings.Contains(err.Error(), "embeddings endpoint") {
		t.Fatalf("got %v", err)
	}
	if _, err := llm.NewEmbedder(llm.EmbedConfig{Provider: llm.EmbedOpenAI, Vendor: "nonesuch", Model: "x"}); err == nil {
		t.Fatal("an unknown vendor with no base URL must be refused")
	}
	// A custom endpoint with a base URL is fine; the list is never a cage.
	if _, err := llm.NewEmbedder(llm.EmbedConfig{
		Provider: llm.EmbedOpenAI, BaseURL: "http://localhost:1234/v1", Model: "x",
		Credential: llm.CredentialRef{Kind: llm.RefInline, Value: "k"},
	}); err != nil {
		t.Fatalf("custom endpoint: %v", err)
	}
	// No provider and no vendor is the local runtime, which is the default the
	// self-hosted tier is meant to land on.
	e, err := llm.NewEmbedder(llm.EmbedConfig{Model: llm.DefaultLocalEmbedModel})
	if err != nil {
		t.Fatal(err)
	}
	if e.Dims() != store.EmbeddingDims {
		t.Fatalf("default width %d", e.Dims())
	}
}

func mustClient(routes map[string]stubResponse) *http.Client {
	_, c := routed(routes)
	return c
}
