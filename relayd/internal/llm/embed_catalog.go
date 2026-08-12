package llm

import "fmt"

// The embedding-model catalog: data the installer renders, not a branch it
// takes. Adding a model is a row here.
//
// **Every row carries its width, and the width is the whole reason this list
// exists.** summary_vec is 768 wide and a vec0 column's width is fixed when the
// table is created, so a 384-dimension model is not a slower choice or a worse
// choice — it is a model that cannot be used against this index at all. The
// list therefore includes the models people will ask for and reach for, marked
// with the width that disqualifies them, so the refusal can name the number
// instead of the option quietly not being there. Rows that do not fit are
// refused by [SelectLocalEmbedModel] *before* a download rather than after one.
//
// **The widths here are from model cards, and the probe is the authority.**
// ollama.com, registry.ollama.ai and huggingface.co are all unreachable from
// the build environment (an organisation egress policy, not a transient
// failure), so no row in this file was confirmed by pulling the model. That is
// exactly why ORCHESTRATOR.md §2's rule applies: nothing is trusted until one
// real call has been made, and [Embedder.Probe] asserts the width itself before
// the backfill writes a single vector. A wrong number here costs a clear error
// at setup, never a corrupt index.

// EmbedModelEntry is one row of the embedding-model menu.
type EmbedModelEntry struct {
	// ID is what gets pulled or sent as the model id.
	ID    string
	Label string
	// Vendor is the catalog vendor for a hosted row, empty for a local one.
	Vendor string
	// Dims is the model's native output width.
	Dims int
	// Truncatable marks a Matryoshka model: one trained so a prefix of the
	// vector is itself a usable embedding, which is what lets a 1536- or
	// 3072-wide hosted model answer at 768 via the `dimensions` request field.
	//
	// Without this, hosted embedding does not work against this index at all —
	// there is no widely-served hosted model whose native width is 768.
	Truncatable bool
	// Size is the download, for local rows. Informational: it is what the
	// installer prints while it waits.
	Size string
	// Recommended marks the default. Exactly one local row carries it.
	Recommended bool
	// Hint is the sentence under the row.
	Hint string
}

// FitsIndex reports whether this model can write vectors summary_vec will
// accept: either it is natively the right width, or it is Matryoshka and wider,
// so it can be asked for a 768-dimension prefix.
func (e EmbedModelEntry) FitsIndex() bool {
	if e.Dims == EmbeddingDims {
		return true
	}
	return e.Truncatable && e.Dims > EmbeddingDims
}

// LocalEmbedModels is the Ollama menu.
//
// nomic-embed-text is the default because it is 768 natively, which is
// store.EmbeddingDims and MEMORY.md §3's sizing exactly, and because it is
// small enough that the install step is minutes rather than a download people
// abandon.
func LocalEmbedModels() []EmbedModelEntry {
	return []EmbedModelEntry{
		{
			ID: DefaultLocalEmbedModel, Label: "Nomic Embed Text", Dims: 768,
			Size: "274 MB", Recommended: true,
			Hint: "768 dimensions natively, which is exactly what the index is. The default.",
		},
		{
			ID: "snowflake-arctic-embed:137m", Label: "Snowflake Arctic Embed (137M)", Dims: 768,
			Size: "274 MB",
			Hint: "Also 768. Retrieval-tuned; a reasonable second opinion if the default disappoints.",
		},
		{
			ID: "granite-embedding:278m", Label: "IBM Granite Embedding (278M)", Dims: 768,
			Size: "563 MB",
			Hint: "768, and multilingual — worth it if your sessions are not all in English.",
		},
		{
			ID: "embeddinggemma", Label: "EmbeddingGemma (300M)", Dims: 768,
			Size: "622 MB",
			Hint: "768. Newer and larger than the default; slower to embed, better on paraphrase.",
		},
		// The rows below cannot be used, and they are here rather than omitted
		// because these are the models people have heard of. Leaving them out
		// makes the list look arbitrary; leaving them in with their width makes
		// the refusal explain itself.
		{
			ID: "mxbai-embed-large", Label: "mxbai Embed Large", Dims: 1024,
			Size: "670 MB",
			Hint: "1024 dimensions — too wide for this index, and not truncatable.",
		},
		{
			ID: "bge-m3", Label: "BGE-M3", Dims: 1024,
			Size: "1.2 GB",
			Hint: "1024 dimensions — too wide for this index.",
		},
		{
			ID: "all-minilm", Label: "all-MiniLM", Dims: 384,
			Size: "46 MB",
			Hint: "384 dimensions — too narrow. Small and fast, and it still cannot be used here.",
		},
	}
}

// HostedEmbedModels is the hosted menu, keyed to the same grouped vendor list
// ORCHESTRATOR.md §2b already uses. Every one of these speaks OpenAI-compatible
// /v1/embeddings, which is what makes OpenRouter and most of the rest one code
// path.
//
// Note what the widths say: nothing hosted is natively 768. The hosted option
// exists only because these models are Matryoshka and can be asked for a 768-
// dimension prefix at request time.
func HostedEmbedModels() []EmbedModelEntry {
	return []EmbedModelEntry{
		{
			ID: "openai/text-embedding-3-small", Label: "OpenAI text-embedding-3-small",
			Vendor: "openrouter", Dims: 1536, Truncatable: true, Recommended: true,
			Hint: "Cheap and good. Asked for 768 dimensions at request time.",
		},
		{
			ID: "openai/text-embedding-3-large", Label: "OpenAI text-embedding-3-large",
			Vendor: "openrouter", Dims: 3072, Truncatable: true,
			Hint: "Stronger, several times the price. Also truncated to 768.",
		},
		{
			ID: "text-embedding-3-small", Label: "OpenAI text-embedding-3-small",
			Vendor: "openai", Dims: 1536, Truncatable: true,
			Hint: "The same model billed directly by OpenAI rather than through OpenRouter.",
		},
		{
			ID: "text-embedding-3-large", Label: "OpenAI text-embedding-3-large",
			Vendor: "openai", Dims: 3072, Truncatable: true,
			Hint: "Direct, and truncated to 768.",
		},
		{
			ID: "gemini-embedding-001", Label: "Gemini Embedding",
			Vendor: "google", Dims: 3072, Truncatable: true,
			Hint: "Google's, through their OpenAI-compatible endpoint.",
		},
	}
}

// LocalEmbedModel looks a local row up by id.
func LocalEmbedModel(id string) (EmbedModelEntry, bool) {
	for _, m := range LocalEmbedModels() {
		if m.ID == id {
			return m, true
		}
	}
	return EmbedModelEntry{}, false
}

// HostedEmbedModel looks a hosted row up by vendor and id.
func HostedEmbedModel(vendor, id string) (EmbedModelEntry, bool) {
	for _, m := range HostedEmbedModels() {
		if m.Vendor == vendor && m.ID == id {
			return m, true
		}
	}
	return EmbedModelEntry{}, false
}

// RecommendedLocalEmbed is the default local model.
func RecommendedLocalEmbed() EmbedModelEntry {
	for _, m := range LocalEmbedModels() {
		if m.Recommended {
			return m
		}
	}
	panic("llm: no recommended local embedding model — the installer needs a default")
}

// SelectLocalEmbedModel resolves a local row and refuses one that cannot write
// to this index.
//
// The refusal happens here, at selection, and not after a download: the point
// of the width being data on the row is that a user picks all-minilm, is told
// why in one sentence, and picks again — rather than waiting for 46 MB, waiting
// for a backfill, and finding out at the first search.
func SelectLocalEmbedModel(id string) (EmbedModelEntry, error) {
	m, ok := LocalEmbedModel(id)
	if !ok {
		// Not in the list is not a refusal. The catalog is a menu, not a cage,
		// and the probe is what actually decides — it will assert the width
		// against one real call before anything is written.
		return EmbedModelEntry{ID: id, Label: id, Dims: 0}, nil
	}
	return m, checkFits(m)
}

// SelectHostedEmbedModel resolves a hosted row and refuses one that cannot
// write to this index.
func SelectHostedEmbedModel(vendor, id string) (EmbedModelEntry, error) {
	m, ok := HostedEmbedModel(vendor, id)
	if !ok {
		return EmbedModelEntry{ID: id, Label: id, Vendor: vendor, Dims: 0}, nil
	}
	return m, checkFits(m)
}

func checkFits(m EmbedModelEntry) error {
	if m.FitsIndex() {
		return nil
	}
	why := "too narrow"
	if m.Dims > EmbeddingDims {
		why = "too wide, and it is not one of the models that can be asked for a shorter vector"
	}
	return fmt.Errorf(
		"%w: %s produces %d-dimension vectors and Relay's index is %d wide, which is %s. "+
			"A vec0 column's width is fixed when the table is created, so this is not something "+
			"setup can adjust afterwards — pick a %d-dimension model (%s is the default), or "+
			"re-embed into a new index",
		ErrEmbedDims, m.Label, m.Dims, EmbeddingDims, why, EmbeddingDims, DefaultLocalEmbedModel)
}

// EmbedConfigFor builds the client config for a catalog row: which endpoint
// shape it speaks, which vendor's base URL it uses, and the width to ask for.
//
// It exists so the installer does not have to know that
// text-embedding-3-small's native width is 1536; the row knows, and the hosted
// client asks for 768 either way.
func EmbedConfigFor(m EmbedModelEntry) EmbedConfig {
	cfg := EmbedConfig{Model: m.ID, Dims: EmbeddingDims}
	if m.Vendor == "" {
		cfg.Provider = EmbedOllama
		return cfg
	}
	cfg.Provider = EmbedOpenAI
	cfg.Vendor = m.Vendor
	return cfg
}

// EmbedProviderFor reads config.toml's `provider` value: "ollama" is the local
// runtime, anything else is a §2b vendor id speaking OpenAI-compatible
// /v1/embeddings, and empty or "none" is no embedder at all.
//
// One value carries both facts because for a hosted embedder the vendor *is*
// the wire shape — every one of them is OpenAI-compatible — so a second field
// would only be a second way to say the same thing and a second way to
// disagree with it.
func EmbedProviderFor(provider string) (EmbedProvider, string) {
	switch provider {
	case "", string(EmbedNone):
		return EmbedNone, ""
	case string(EmbedOllama):
		return EmbedOllama, ""
	default:
		return EmbedOpenAI, provider
	}
}
