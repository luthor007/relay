package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// Embedding models — ORCHESTRATOR.md §2c, and the third choice step alongside
// the voice (§2a) and the two orchestrator models (§2b).
//
// Four things shape everything in this file, and all four are consequences of
// decisions made elsewhere rather than preferences expressed here:
//
//  1. **The width is fixed before the first write.** summary_vec is a vec0
//     table and a vec0 column's width is set when the table is created, so
//     store.EmbeddingDims is 768 for the life of an index. An embedder that
//     returns 384 cannot write and cannot query. That makes the choice a step
//     taken *before* the backfill, not a setting adjusted afterwards, and it
//     makes [EmbedModelEntry.FitsIndex] a refusal at selection time rather than
//     an error after a download.
//
//  2. **Local is recommended on the self-hosted tier, which inverts §2a/§2b.**
//     Not on cost: embedding ~22,000 summaries (MEMORY.md §3) is 4–5 million
//     tokens, well under a dollar once, for the whole 3.6 GB corpus. The
//     argument is the same one MEMORY.md §6 and CLOUD.md §1 already make about
//     the vault — self-hosting means it never leaves your hardware — applied to
//     the more sensitive asset. Summaries of somebody's entire working history
//     are a sharper thing to hand a third party than the keys are. Embedding is
//     also minutes next to the hour or two MEMORY.md §4 budgets for
//     summarisation, so choosing local costs almost nothing in install time.
//     On Relay Cloud the box is ours, the local model is preinstalled, and the
//     step is informational.
//
//  3. **Nothing is trusted until one real call has been made** (ORCHESTRATOR.md
//     §2). For an embedder that is [Embedder.Probe]: embed a known string,
//     assert the width, assert the vector is not degenerate. A 384-dimension
//     model fails there, named, with both numbers — not at the first search
//     after a two-hour backfill.
//
//  4. **The local model is a separate process we talk to over HTTP.** SYSTEM.md
//     §8 requires CGO_ENABLED=0 cross-compilation to darwin and linux on arm64
//     and amd64 from one machine, which rules out linking an inference runtime
//     into the binary. Ollama is a subprocess with an HTTP API, exactly like
//     every other provider here, and the transport is injectable so no test in
//     this package opens a socket.

// EmbeddingDims mirrors store.EmbeddingDims. It is duplicated rather than
// imported so internal/llm stays a leaf package — importing store would drag
// the SQLite wasm stack into every binary that only wants to call a model — in
// the same way internal/config duplicates the credential-reference prefixes.
// The two constants are pinned together by a test that imports both.
const EmbeddingDims = 768

// EmbedProvider is where the vectors come from.
type EmbedProvider string

const (
	// EmbedOllama is the local runtime: a separate process on this machine,
	// reached over HTTP, holding a model the installer provisioned. Recommended
	// on the self-hosted tier.
	EmbedOllama EmbedProvider = "ollama"
	// EmbedOpenAI is any OpenAI-compatible /v1/embeddings endpoint, which covers
	// OpenRouter and most of ORCHESTRATOR.md §2b's vendor list. Which vendor is
	// carried separately, in [EmbedConfig.Vendor].
	EmbedOpenAI EmbedProvider = "openai"
	// EmbedNone is no embedder at all. It is a supported state, not a failure:
	// internal/search degrades to lexical-only and says so in Results.Degraded.
	// A box whose embedder is down gets worse search, not no search.
	EmbedNone EmbedProvider = "none"
)

// EmbedProviders lists every wire shape.
//
// These are not the values config.toml's `provider` key takes — there one
// string carries both the shape and, for a hosted embedder, the vendor id. See
// [EmbedProviderFor], which is where the two meet.
func EmbedProviders() []EmbedProvider {
	return []EmbedProvider{EmbedOllama, EmbedOpenAI, EmbedNone}
}

// DefaultOllamaBaseURL is where a stock Ollama listens. It binds loopback by
// default, which is the point: the text never leaves the machine.
const DefaultOllamaBaseURL = "http://127.0.0.1:11434"

// DefaultLocalEmbedModel is the recommended local model. 768 dimensions, which
// is store.EmbeddingDims and MEMORY.md §3's sizing exactly.
const DefaultLocalEmbedModel = "nomic-embed-text"

// DefaultEmbedBatch is how many texts go in one request. Every provider bills
// and rate-limits per call, and a summariser has many short texts rather than
// one long one, so batching is the difference between one request and 22,000.
const DefaultEmbedBatch = 64

// DefaultEmbedTimeout caps one batch. A local model on a cold start can take a
// while to load into memory before it answers at all.
const DefaultEmbedTimeout = 2 * time.Minute

// Embedding errors.
var (
	// ErrEmbedDims means a model's width is not the index's width. It is a
	// schema error, not a data error: a vec0 column cannot be widened.
	ErrEmbedDims = errors.New("llm: embedding width does not match the index")
	// ErrNoEmbedModel means no model was named.
	ErrNoEmbedModel = errors.New("llm: no embedding model configured")
	// ErrNoEmbedder means embedding is switched off. Callers check for this and
	// pass a nil Embedder to search, which is a supported state.
	ErrNoEmbedder = errors.New("llm: embedding is switched off; search runs lexical-only")
)

// Embedder turns text into vectors.
//
// The first three methods are deliberately the same set as search.Embedder, so
// every provider built here satisfies that interface without either package
// importing the other. That interface is the contract; this one only adds
// [Embedder.Probe], because probing is an installer concern and search has no
// business knowing about credentials.
type Embedder interface {
	// Model names the embedding model. It is written to the index's
	// embedding_model meta key on first write and checked on every search.
	Model() string
	// Dims is the vector width, which must equal EmbeddingDims to be usable
	// against a real index.
	Dims() int
	// Embed returns one L2-normalised vector per input, in order. Normalising
	// here rather than hoping the provider did it is what makes "cosine" in
	// MEMORY.md §3 true of summary_vec, which is an L2 index: L2 distance is
	// monotonic in cosine only on unit vectors.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Probe makes one real call and reports a stable reason code. Like
	// [Provider.Probe] it never returns an error — a failed probe is a result
	// the installer prints, not a reason to abort setup half way through.
	Probe(ctx context.Context) EmbedCheck
}

// EmbedConfig describes one embedding model on one provider.
type EmbedConfig struct {
	// Provider defaults to EmbedOllama, or to EmbedOpenAI when a Vendor is set.
	Provider EmbedProvider
	// Vendor is a catalog id from Vendors(), used for its base URL. Hosted only.
	Vendor string
	// BaseURL overrides the provider's default. Required for a custom vendor.
	BaseURL string
	Model   string
	// Dims defaults to EmbeddingDims.
	//
	// It is settable so tests can drive narrow fixtures, not so a box can run a
	// 384-dimension model: config.Validate refuses anything but EmbeddingDims,
	// search.New refuses to build a Searcher on a mismatched embedder, and
	// [SelectLocalEmbedModel] refuses one before it is ever downloaded.
	//
	// On the hosted path it is also what gets asked for through the
	// `dimensions` request field, because nothing hosted is natively this
	// narrow. See embed_openai.go.
	Dims int
	// Credential is a reference, not a secret — and it is legitimately empty for
	// the local runtime, which has no credential at all. That is a normal state
	// here rather than an error state.
	Credential CredentialRef
	// Headers are extra headers, e.g. OpenRouter's HTTP-Referer.
	Headers map[string]string
	// HTTPClient is the injection point. Tests supply a RoundTripper and this
	// package makes no network calls.
	HTTPClient *http.Client
	// Lookup resolves a "vault:<id>" reference.
	Lookup SecretLookup
	// Timeout caps one batch; DefaultEmbedTimeout when zero.
	Timeout time.Duration
	// BatchSize caps texts per request; DefaultEmbedBatch when zero.
	BatchSize int
}

// NewEmbedder builds an embedder from a config.
func NewEmbedder(cfg EmbedConfig) (Embedder, error) {
	if cfg.Provider == "" {
		if cfg.Vendor != "" {
			cfg.Provider = EmbedOpenAI
		} else {
			cfg.Provider = EmbedOllama
		}
	}
	if cfg.Provider == EmbedNone {
		return nil, ErrNoEmbedder
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, ErrNoEmbedModel
	}
	if cfg.Dims < 0 {
		return nil, fmt.Errorf("%w: %d dimensions is not a width", ErrEmbedDims, cfg.Dims)
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}

	switch cfg.Provider {
	case EmbedOllama:
		if cfg.BaseURL == "" {
			cfg.BaseURL = DefaultOllamaBaseURL
		}
		return &ollamaEmbedder{core: embedCore{cfg: cfg}}, nil

	case EmbedOpenAI:
		if cfg.BaseURL == "" {
			v, ok := Vendor(cfg.Vendor)
			if !ok {
				return nil, fmt.Errorf("llm: no base URL, and vendor %q is not in the catalog", cfg.Vendor)
			}
			if v.API == APIAnthropic {
				// A real fact worth stating rather than discovering as a 404:
				// Anthropic exposes no embeddings endpoint. The models list and
				// the embedding list are not the same list.
				return nil, fmt.Errorf("llm: %s has no embeddings endpoint — "+
					"choose an OpenAI-compatible vendor, or the local model, which needs no vendor at all", v.Label)
			}
			cfg.BaseURL = v.BaseURL
		}
		if cfg.BaseURL == "" {
			return nil, fmt.Errorf("llm: no base URL for vendor %q", cfg.Vendor)
		}
		return &openaiEmbedder{core: embedCore{cfg: cfg, needsCredential: true}}, nil
	}
	return nil, fmt.Errorf("llm: unknown embedding provider %q", cfg.Provider)
}

// ------------------------------------------------------- the shared middle --

// batchCall sends one batch and returns raw, un-normalised vectors in input
// order. Everything above it — blank text, batching, width checks,
// normalisation — is shared, so a new provider is one wire format and nothing
// else.
type batchCall func(ctx context.Context, texts []string) ([][]float32, error)

type embedCore struct {
	cfg EmbedConfig
	// needsCredential is false for the local runtime, which has none.
	needsCredential bool
}

func (c *embedCore) dims() int {
	if c.cfg.Dims > 0 {
		return c.cfg.Dims
	}
	return EmbeddingDims
}

func (c *embedCore) batchSize() int {
	if c.cfg.BatchSize > 0 {
		return c.cfg.BatchSize
	}
	return DefaultEmbedBatch
}

func (c *embedCore) timeout() time.Duration {
	if c.cfg.Timeout > 0 {
		return c.cfg.Timeout
	}
	return DefaultEmbedTimeout
}

// embed is every provider's Embed: blank inputs are answered locally, the rest
// are batched, width-checked and normalised.
func (c *embedCore) embed(ctx context.Context, texts []string, call batchCall) ([][]float32, error) {
	out := make([][]float32, len(texts))
	if len(texts) == 0 {
		return out, nil
	}

	// A summary can legitimately be empty — a turn with no observable events.
	// Sending "" is a 400 on most providers and a wasted call on the rest, so it
	// is answered here with the zero vector, which is what search.HashEmbedder
	// produces for the same input and which search.Normalize deliberately leaves
	// alone rather than turning into NaNs.
	live := make([]string, 0, len(texts))
	at := make([]int, 0, len(texts))
	for i, t := range texts {
		if strings.TrimSpace(t) == "" {
			out[i] = make([]float32, c.dims())
			continue
		}
		live = append(live, t)
		at = append(at, i)
	}
	if len(live) == 0 {
		return out, nil
	}

	size := c.batchSize()
	for start := 0; start < len(live); start += size {
		end := min(start+size, len(live))
		batch := live[start:end]

		vecs, err := c.callWithTimeout(ctx, batch, call)
		if err != nil {
			return nil, err
		}
		if len(vecs) != len(batch) {
			return nil, fmt.Errorf("llm: %s returned %d vectors for %d inputs",
				c.cfg.Model, len(vecs), len(batch))
		}
		for j, v := range vecs {
			if len(v) != c.dims() {
				return nil, fmt.Errorf("%w: %s returned %d dimensions, the index is %d wide",
					ErrEmbedDims, c.cfg.Model, len(v), c.dims())
			}
			normalizeVec(v)
			out[at[start+j]] = v
		}
	}
	return out, nil
}

func (c *embedCore) callWithTimeout(ctx context.Context, batch []string, call batchCall) ([][]float32, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	return call(ctx, batch)
}

// postJSON sends a JSON body, reads the whole response and closes it.
//
// It does not reuse [post] because that one resolves a credential
// unconditionally, and the local runtime has none. "No credential" is a normal
// configuration here, not a missing one.
func (c *embedCore) postJSON(ctx context.Context, path string, payload any, out any) error {
	var key string
	if !c.cfg.Credential.IsZero() {
		var err error
		key, err = c.cfg.Credential.Resolve(ctx, c.cfg.Lookup)
		if err != nil {
			return err
		}
	} else if c.needsCredential {
		return ErrMissingCredential
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	url := strings.TrimSuffix(c.cfg.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range c.cfg.Headers {
		req.Header.Set(k, v)
	}
	if key != "" {
		bearer(req.Header, key)
	}

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return &HTTPError{Status: resp.StatusCode, Body: string(b)}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ------------------------------------------------------------ vector maths --

// normalizeVec scales v to unit length in place, leaving a zero vector alone —
// there is no direction to preserve and dividing by zero would poison the whole
// index with NaNs.
//
// It duplicates search.Normalize for the same reason EmbeddingDims duplicates
// store.EmbeddingDims: internal/llm is a leaf.
func normalizeVec(v []float32) {
	n := l2(v)
	if n == 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return
	}
	inv := float32(1 / n)
	for i := range v {
		v[i] *= inv
	}
}

// l2 is the Euclidean norm, in float64 so a 3072-wide vector of small
// components does not lose the sum to rounding.
func l2(v []float32) float64 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	return math.Sqrt(sum)
}

// finite reports whether every component is a real number. A provider that
// answers with NaNs writes a vector that makes every distance NaN, which
// silently destroys ranking rather than failing.
func finite(v []float32) bool {
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return false
		}
	}
	return true
}
