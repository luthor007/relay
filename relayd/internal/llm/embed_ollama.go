package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
)

// ollamaEmbedder talks to a local Ollama over HTTP.
//
// This is the recommended provider on the self-hosted tier, and the reason is
// privacy rather than price (see embed.go): summaries of somebody's entire
// working history are the most sensitive thing this system holds, and on this
// path they never leave the machine. It is also the option the installer can
// provision itself — the user is not sent to a README to install a model by
// hand — which is why it is a step and not a setting.
//
// It runs as a **separate process**, not a library. SYSTEM.md §8 requires
// CGO_ENABLED=0 cross-compilation to darwin and linux on arm64 and amd64 from
// one machine, and linking an inference runtime into the binary would end that
// the same way a native sqlite-vec would (MEMORY.md §3). So it is HTTP, like
// every other provider here, and the transport is injectable.
//
// # Two endpoints, and why both
//
// /api/embed is the current one: it takes an array and returns an array, which
// is the whole point of a batched interface — 22,000 summaries is 344 requests
// at the default batch size instead of 22,000.
//
// /api/embeddings is the older one, single-input only, `prompt` instead of
// `input` and `embedding` instead of `embeddings`. It is implemented because
// the alternative is that a box running an Ollama from before /api/embed
// existed gets a bare 404 from setup with nothing actionable in it. The
// fallback is narrow on purpose: it triggers only on a 404 from /api/embed —
// which is unambiguous, an old daemon has no other reason to 404 that path —
// and it is sticky, so the cost is one wasted request per process rather than
// one per batch. Everything after that is the same batching, the same width
// check and the same normalisation.
type ollamaEmbedder struct {
	core embedCore

	mu sync.Mutex
	// legacy records that this daemon predates /api/embed, so the rest of the
	// backfill goes straight to the old endpoint.
	legacy bool
}

var _ Embedder = (*ollamaEmbedder)(nil)

func (e *ollamaEmbedder) Model() string { return e.core.cfg.Model }
func (e *ollamaEmbedder) Dims() int     { return e.core.dims() }

type ollamaEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type ollamaEmbedResponse struct {
	Model      string      `json:"model"`
	Embeddings [][]float32 `json:"embeddings"`
}

type ollamaLegacyRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type ollamaLegacyResponse struct {
	Embedding []float32 `json:"embedding"`
}

func (e *ollamaEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return e.core.embed(ctx, texts, e.call)
}

func (e *ollamaEmbedder) Probe(ctx context.Context) EmbedCheck {
	return e.core.probe(ctx, e.call)
}

func (e *ollamaEmbedder) call(ctx context.Context, texts []string) ([][]float32, error) {
	if e.useLegacy() {
		return e.callLegacy(ctx, texts)
	}

	var out ollamaEmbedResponse
	err := e.core.postJSON(ctx, "/api/embed",
		ollamaEmbedRequest{Model: e.core.cfg.Model, Input: texts}, &out)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && he.Status == http.StatusNotFound {
			// An Ollama that predates /api/embed. Remember it and retry once on
			// the old endpoint; every later batch goes straight there.
			e.setLegacy()
			return e.callLegacy(ctx, texts)
		}
		return nil, err
	}
	if len(out.Embeddings) == 0 {
		return nil, fmt.Errorf("llm: ollama returned no embeddings for %s", e.core.cfg.Model)
	}
	return out.Embeddings, nil
}

// callLegacy walks the batch one text at a time, because that is all the old
// endpoint can do. It is slower by exactly the number of texts, which is the
// price of talking to an old daemon and is stated rather than hidden.
func (e *ollamaEmbedder) callLegacy(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for _, t := range texts {
		var one ollamaLegacyResponse
		if err := e.core.postJSON(ctx, "/api/embeddings",
			ollamaLegacyRequest{Model: e.core.cfg.Model, Prompt: t}, &one); err != nil {
			return nil, err
		}
		if len(one.Embedding) == 0 {
			return nil, fmt.Errorf("llm: ollama returned an empty embedding for %s", e.core.cfg.Model)
		}
		out = append(out, one.Embedding)
	}
	return out, nil
}

func (e *ollamaEmbedder) useLegacy() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.legacy
}

func (e *ollamaEmbedder) setLegacy() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.legacy = true
}
