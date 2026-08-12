package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
)

// openaiEmbedder speaks POST /v1/embeddings, which covers OpenRouter — the
// recommended vendor — OpenAI directly, Google's compatibility endpoint, and
// every custom provider that advertises itself as OpenAI-compatible. One wire
// format for the whole hosted list, the same way openaiProvider covers the
// whole completion list.
//
// # The dimensions parameter, which is not a nicety
//
// **Nothing hosted is natively 768.** The models worth using are 1024, 1536 or
// 3072 wide and this index is 768, so without asking for a narrower vector the
// hosted option cannot write to the index at all. OpenAI's v3 embeddings accept
// a `dimensions` field for exactly this — the models are trained so a prefix of
// the vector is itself a usable embedding — so 768 is something to ask for
// rather than hope for.
//
// The request is sent with `dimensions` and, if the provider rejects the *body*
// (400 or 422), retried once without it and the omission is remembered. That
// covers an OpenAI-compatible endpoint that does not implement the field
// without costing a wasted request per batch across a 22,000-summary backfill.
// A provider that instead *ignores* the field answers at its native width, and
// the probe reports wrong_width with both numbers — which is the honest outcome
// and the reason the probe measures rather than trusting a model card.
//
// An auth failure is never retried. The second 401 tells nobody anything.
type openaiEmbedder struct {
	core embedCore

	mu sync.Mutex
	// noDimensions records that this endpoint refused the dimensions field.
	noDimensions bool
}

var _ Embedder = (*openaiEmbedder)(nil)

func (e *openaiEmbedder) Model() string { return e.core.cfg.Model }
func (e *openaiEmbedder) Dims() int     { return e.core.dims() }

type openaiEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
	// EncodingFormat is explicit because the default is base64 on some
	// deployments, and a silently base64 body decodes to an empty vector rather
	// than to an error.
	EncodingFormat string `json:"encoding_format"`
	// Dimensions is omitted when zero: not every provider accepts it, and an
	// unknown field is a 400 on the strict ones.
	Dimensions int `json:"dimensions,omitempty"`
}

type openaiEmbedResponse struct {
	Model string `json:"model"`
	Data  []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Usage *struct {
		PromptTokens int64 `json:"prompt_tokens"`
		TotalTokens  int64 `json:"total_tokens"`
	} `json:"usage"`
}

func (e *openaiEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return e.core.embed(ctx, texts, e.call)
}

func (e *openaiEmbedder) Probe(ctx context.Context) EmbedCheck {
	return e.core.probe(ctx, e.call)
}

func (e *openaiEmbedder) call(ctx context.Context, texts []string) ([][]float32, error) {
	dims := e.core.dims()
	if e.dimensionsRefused() {
		dims = 0
	}

	vecs, err := e.callWith(ctx, texts, dims)
	if err == nil || dims == 0 {
		return vecs, err
	}
	if !rejectedTheBody(err) {
		return nil, err
	}
	e.refuseDimensions()
	return e.callWith(ctx, texts, 0)
}

// rejectedTheBody reports whether a provider refused the request itself rather
// than the credential, the model or the rate.
//
// It is narrow on purpose. A 404 is a wrong model id or a wrong path and
// dropping a field will not fix it; a 429 is a rate limit and retrying
// immediately makes it worse; 401/402/403 are the credential's. 400 and 422 are
// the two codes that actually mean "I do not understand this body", which is
// what an endpoint without `dimensions` says.
func rejectedTheBody(err error) bool {
	var he *HTTPError
	if !errors.As(err, &he) {
		return false
	}
	return he.Status == http.StatusBadRequest || he.Status == http.StatusUnprocessableEntity
}

func (e *openaiEmbedder) callWith(ctx context.Context, texts []string, dims int) ([][]float32, error) {
	var out openaiEmbedResponse
	err := e.core.postJSON(ctx, "/embeddings", openaiEmbedRequest{
		Model:          e.core.cfg.Model,
		Input:          texts,
		EncodingFormat: "float",
		Dimensions:     dims,
	}, &out)
	if err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, fmt.Errorf("llm: %s returned no embeddings", e.core.cfg.Model)
	}

	// The spec says the data array is in input order and every element also
	// carries its index. Sorting by the index costs nothing and means a provider
	// that reorders under load pairs vectors with the wrong summaries never
	// rather than occasionally — a failure that would be invisible in every test
	// and wrong in every search.
	sort.SliceStable(out.Data, func(i, j int) bool { return out.Data[i].Index < out.Data[j].Index })

	vecs := make([][]float32, len(out.Data))
	for i, d := range out.Data {
		vecs[i] = d.Embedding
	}
	return vecs, nil
}

func (e *openaiEmbedder) dimensionsRefused() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.noDimensions
}

func (e *openaiEmbedder) refuseDimensions() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.noDimensions = true
}
