package search_test

import (
	"context"
	"math"
	"testing"

	"github.com/luthor007/relay/relayd/internal/search"
	"github.com/luthor007/relay/relayd/internal/store"
)

func TestHashEmbedderIsDeterministicAndUnitLength(t *testing.T) {
	ctx := context.Background()
	e := search.NewHashEmbedder(store.EmbeddingDims)
	if e.Dims() != store.EmbeddingDims {
		t.Fatalf("dims %d", e.Dims())
	}

	a, err := e.Embed(ctx, []string{"payments branch, stripe webhook retry"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	b, _ := e.Embed(ctx, []string{"payments branch, stripe webhook retry"})
	for i := range a[0] {
		if a[0][i] != b[0][i] {
			t.Fatalf("not deterministic at %d", i)
		}
	}

	var sum float64
	for _, x := range a[0] {
		sum += float64(x) * float64(x)
	}
	// vec0 indexes by L2 distance. On unit vectors L2 is monotonic in cosine,
	// which is what makes "cosine" in MEMORY.md §3 true of what we query.
	if math.Abs(sum-1) > 1e-5 {
		t.Fatalf("not unit length: %v", sum)
	}
}

func TestHashEmbedderSeparatesUnrelatedText(t *testing.T) {
	ctx := context.Background()
	e := search.NewHashEmbedder(store.EmbeddingDims)
	v, err := e.Embed(ctx, []string{
		"stripe webhook signature verification failing on the payments branch",
		"stripe webhook signature verification on the payments branch, retried",
		"BLE CRC-16 MODBUS codec on the glasses firmware",
	})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	near := search.Cosine(v[0], v[1])
	far := search.Cosine(v[0], v[2])
	if near <= far {
		t.Fatalf("token overlap did not dominate: near=%v far=%v", near, far)
	}
}

func TestEmbedEmptyTextIsZeroNotNaN(t *testing.T) {
	// A summary can legitimately come back empty — a turn with no observable
	// events. Normalising it must not poison the index with NaNs.
	e := search.NewHashEmbedder(store.EmbeddingDims)
	v, err := e.Embed(context.Background(), []string{""})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	for i, x := range v[0] {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			t.Fatalf("component %d is %v", i, x)
		}
	}
}

func TestCosine(t *testing.T) {
	if got := search.Cosine([]float32{1, 0}, []float32{1, 0}); math.Abs(got-1) > 1e-9 {
		t.Fatalf("identical: %v", got)
	}
	if got := search.Cosine([]float32{1, 0}, []float32{0, 1}); math.Abs(got) > 1e-9 {
		t.Fatalf("orthogonal: %v", got)
	}
	if got := search.Cosine([]float32{1, 0}, []float32{1, 0, 0}); got != 0 {
		t.Fatalf("mismatched widths must be 0, got %v", got)
	}
	if got := search.Cosine([]float32{0, 0}, []float32{1, 0}); got != 0 {
		t.Fatalf("zero vector must be 0, got %v", got)
	}
}

func TestTokenizeKeepsIdentifiersBothWays(t *testing.T) {
	got := map[string]bool{}
	for _, tok := range search.Tokenize("Set STRIPE_SECRET_KEY on api.stripe.com") {
		got[tok] = true
	}
	for _, want := range []string{"stripe_secret_key", "stripe", "secret", "key", "api.stripe.com", "api", "com"} {
		if !got[want] {
			t.Fatalf("missing %q in %v", want, got)
		}
	}
}

func TestStaticEmbedderFallsBack(t *testing.T) {
	e := &search.StaticEmbedder{
		Vectors: map[string][]float32{"known": unit(1, 0)},
		Next:    search.NewHashEmbedder(store.EmbeddingDims),
	}
	v, err := e.Embed(context.Background(), []string{"known", "unknown"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(v) != 2 || len(v[1]) != store.EmbeddingDims {
		t.Fatalf("fallback did not fill in: %d", len(v))
	}
	if v[0][0] != 1 {
		t.Fatalf("stated vector not used: %v", v[0][:3])
	}
}
