package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/config"
	"github.com/luthor007/relay/relayd/internal/store"
)

// The width is duplicated so config stays a leaf package. This is what keeps
// the duplication honest: a vec0 column's width is fixed when the table is
// created, so these two cannot be allowed to drift.
func TestEmbeddingDimsMirrorsTheStore(t *testing.T) {
	if config.EmbeddingDims != store.EmbeddingDims {
		t.Fatalf("config says %d and the index is %d wide",
			config.EmbeddingDims, store.EmbeddingDims)
	}
}

// ORCHESTRATOR.md §2c: local is the recommendation on the self-hosted tier, and
// it inverts §2a and §2b because the argument is privacy rather than quality —
// summaries of the user's entire working history never leave the machine.
func TestDefaultEmbeddingIsLocalAndKeyless(t *testing.T) {
	e := config.Default().Embedding
	if e.Provider != config.EmbedProviderOllama {
		t.Fatalf("the default provider is %q, want the local runtime", e.Provider)
	}
	if e.Model != config.DefaultEmbedModel {
		t.Fatalf("the default model is %q", e.Model)
	}
	if e.Dims != store.EmbeddingDims {
		t.Fatalf("the default width is %d, and the index is %d", e.Dims, store.EmbeddingDims)
	}
	// The local runtime has no credential, and that is a normal state rather
	// than a half-finished configuration.
	if e.Credential != "" {
		t.Fatalf("the default carries a credential: %q", e.Credential)
	}
	if !e.Configured() || !e.Local() {
		t.Fatalf("the default should be a working local embedder: %+v", e)
	}
	if err := config.Default().Validate(); err != nil {
		t.Fatalf("the default configuration must validate: %v", err)
	}
}

// A config written before this step existed has no [embedding] section at all,
// and must come up on the recommendation rather than with no embedder.
func TestAbsentEmbeddingSectionTakesTheDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("listen = \"127.0.0.1:8787\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Embedding.Provider != config.EmbedProviderOllama || c.Embedding.Model != config.DefaultEmbedModel {
		t.Fatalf("embedding: %+v", c.Embedding)
	}
	if c.Embedding.Dims != store.EmbeddingDims {
		t.Fatalf("dims %d", c.Embedding.Dims)
	}
}

// "no embedder" is a supported state, not a broken one: internal/search
// degrades to lexical-only and says so on every query. A box whose embedder is
// down should get worse search, not no search.
//
// Both spellings mean it — an explicit "none" and an empty provider — because
// the installer writes an empty one when the user takes the third row, and a
// person editing the file by hand writes the word.
func TestEmbeddingCanBeSwitchedOff(t *testing.T) {
	for _, provider := range []string{config.EmbedProviderNone, ""} {
		c := config.Default()
		c.Embedding = config.Embedding{Provider: provider}
		if err := c.Validate(); err != nil {
			t.Fatalf("provider %q: switching embedding off must be allowed: %v", provider, err)
		}
		if c.Embedding.Configured() {
			t.Fatalf("provider %q must not build an embedder", provider)
		}

		// And it survives a round trip rather than being defaulted back on. An
		// installer that wrote "off" must not find relayd reaching for a service
		// on this machine that nobody asked it to reach for.
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := config.Save(path, c); err != nil {
			t.Fatal(err)
		}
		got, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if got.Embedding.Configured() {
			t.Fatalf("a deliberate %q was quietly re-enabled as %+v", provider, got.Embedding)
		}
	}
}

// The choice has to be refused where it is made. A width that does not match
// the index cannot be fixed later: the vec0 column is already that wide.
func TestEmbeddingWidthIsRefusedWithBothNumbers(t *testing.T) {
	c := config.Default()
	c.Embedding.Model = "all-minilm"
	c.Embedding.Dims = 384

	err := c.Validate()
	if err == nil {
		t.Fatal("a 384-dimension model must be refused, not discovered at the first search")
	}
	for _, want := range []string{"384", "768", "vec0", "embedding.dims"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal must say %q: %v", want, err)
		}
	}
}

func TestEmbeddingProviderNeedsAModel(t *testing.T) {
	c := config.Default()
	c.Embedding.Model = ""
	err := c.Validate()
	if err == nil {
		t.Fatal("a provider with no model is a half-configuration")
	}
	if !strings.Contains(err.Error(), "embedding.provider") {
		t.Fatalf("the error should name the field: %v", err)
	}

	// A vendor id this package has never heard of is NOT refused here: config
	// is a leaf and importing the catalog to check a string would invert that.
	// An unknown vendor fails at construction, which is still before any call.
	c = config.Default()
	c.Embedding = config.Embedding{
		Provider: "some-new-vendor", Model: "embed-1", Dims: store.EmbeddingDims,
		Credential: "env:SOME_NEW_VENDOR_API_KEY",
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("the vendor list is not a cage: %v", err)
	}
}

func TestHostedEmbeddingCustomVendorNeedsABaseURL(t *testing.T) {
	c := config.Default()
	c.Embedding = config.Embedding{
		Provider: "custom",
		Model:    "text-embedding-3-small", Dims: store.EmbeddingDims,
		Credential: "env:OPENAI_API_KEY",
	}
	if err := c.Validate(); err == nil {
		t.Fatal("a custom embedding endpoint with no base_url must be caught at load")
	}
	c.Embedding.BaseURL = "http://localhost:8080/v1"
	if err := c.Validate(); err != nil {
		t.Fatalf("with a base URL: %v", err)
	}
}

// config.toml is the file people paste into support tickets.
func TestEmbeddingCredentialIsAReferenceOnly(t *testing.T) {
	c := config.Default()
	c.Embedding = config.Embedding{
		Provider: "openrouter",
		Model:    "openai/text-embedding-3-small", Dims: store.EmbeddingDims,
		Credential: "sk-or-v1:abcdef0123456789",
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("a pasted secret must be refused")
	}
	if !strings.Contains(err.Error(), "embedding.credential") {
		t.Fatalf("the error should name the field: %v", err)
	}

	for _, ref := range []string{
		"env:OPENROUTER_API_KEY",
		"file:~/.config/relay/openrouter.key",
		"exec:op read op://Private/OpenRouter/credential",
		"vault:embed-1",
		"OPENROUTER_API_KEY",
		"", // the local runtime, which has none
	} {
		c.Embedding.Credential = ref
		if err := c.Validate(); err != nil {
			t.Fatalf("reference %q: %v", ref, err)
		}
	}
}

func TestEmbeddingRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	want := config.Default()
	want.Embedding = config.Embedding{
		Provider: "openrouter",
		Model:    "openai/text-embedding-3-small", Dims: store.EmbeddingDims,
		BaseURL:    "https://openrouter.ai/api/v1",
		Credential: "env:OPENROUTER_API_KEY",
	}
	if err := config.Save(path, want); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "[embedding]") {
		t.Fatalf("the section is not in the file:\n%s", raw)
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Embedding != want.Embedding {
		t.Fatalf("round trip: %+v", got.Embedding)
	}
}
