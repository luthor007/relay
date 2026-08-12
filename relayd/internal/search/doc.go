// Package search is MEMORY.md §3's retrieval: reciprocal-rank fusion over BM25
// (FTS5) and cosine (sqlite-vec), never pure vector.
//
// # Why hybrid, and not the obvious thing
//
// The obvious thing is a vector index and a cosine query. It is wrong here for a
// specific reason rather than a stylistic one: most of what routing looks up is
// an exact identifier — a repo name, an error string, STRIPE_SECRET_KEY — and
// exact identifiers are where dense retrieval is weakest and BM25 is strongest.
// A paraphrase that never contains the token can out-score the one document that
// does. So both halves run, each produces a ranked list, and
// [Fuse] combines them by rank rather than by score.
//
// Rank, not score, is the whole trick. BM25 in SQLite is a negative number where
// lower is better; vec0 reports L2 distance where lower is also better but on a
// completely different scale. There is no principled normalisation between them,
// and every attempt to invent one drifts as the corpus changes. Reciprocal-rank
// fusion needs only the position within each list, which is comparable by
// construction.
//
// # What it is sized for
//
// One embedding call per query, one kNN, one hydration query. That shape is
// deliberate: the pure-Go wasm sqlite-vec build we ship measures 45 ms median at
// the 22,000-vector design target and 190 ms at 100,000 (MEMORY.md §3, measured
// 2026-08-10, roughly 10× the native figure the design was originally sized
// against). 45 ms is comfortably inside SYSTEM.md §7b's sub-second budget for a
// memory lookup, so brute force stands and no ANN index is needed — but there is
// no room for a per-candidate round trip, so there are none here.
//
// # Degrading visibly
//
// [Results.Degraded] carries a reason whenever half the retrieval did not run:
// no embedder configured, the embedding provider failed, the index was written
// by a different embedding model. The caller gets lexical-only results and is
// told, rather than getting a quietly worse answer. That is the same rule the
// adapters follow at the runtime boundary, applied here.
package search
