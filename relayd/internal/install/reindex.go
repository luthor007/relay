package install

import (
	"context"
	"fmt"

	"github.com/luthor007/relay/relayd/internal/search"
)

// Re-embedding, after a model change. ORCHESTRATOR.md §2c.
//
// This exists because of the constraint that makes §2c a step in the first
// place. A vec0 column's width is fixed when the table is created, and the
// index records which model wrote its vectors in an embedding_model meta key
// that internal/search checks on every query. Together those mean a model
// change is not a config edit: the vectors already on disk cannot be compared
// with vectors from the new model, and search will correctly refuse to mix them
// and run keyword-only until the mismatch is resolved.
//
// So there are exactly two honest ways out of a mismatch — change the model
// back, or re-embed — and this is the second. It is a named command rather than
// an error message because somebody who wants a different embedder is not
// making a mistake.
//
// **What it costs, honestly.** Re-embedding is not re-summarising. The
// summaries are already on disk; the expensive part, the hour or two MEMORY.md
// §4 budgets, does not happen again. Embedding ~22,000 existing summaries is
// minutes locally and well under a dollar hosted. That asymmetry is worth
// stating plainly every time, because "reindex" sounds like the two-hour thing
// and it is not.

// Index is the slice of the summary index these commands need.
//
// It is an interface because internal/install has no business opening a
// database — the CLI does that — and because a test drives it from a fake. Both
// methods are internal/search's, unchanged: this package does not get its own
// opinion about what a mismatch is.
type Index interface {
	// Inspect reports what the index holds and what the configured embedder is.
	Inspect(ctx context.Context) (search.EmbeddingState, error)
	// Reset clears every vector and hands the index to model, in one
	// transaction, returning how many vectors went. The summaries and the
	// keyword index are untouched.
	Reset(ctx context.Context, model string) (int64, error)
}

// ReindexOutcome is what the command did.
type ReindexOutcome struct {
	State search.EmbeddingState
	// Needed is whether anything was out of step at all.
	Needed bool
	// Confirmed is whether the user agreed, and Cleared how many vectors went.
	Confirmed bool
	Cleared   int64
	// Message is the sentence the CLI prints.
	Message string
}

// RunReindex clears the index's vectors so relayd re-embeds from the summaries
// it already has.
//
// It does not do the embedding. That is relayd's job for the same reason
// backfill is: it is minutes of work that has to survive being interrupted, and
// nobody should watch a progress bar in a setup command. What this guarantees
// is that the next time relayd runs, the index is in a state where re-embedding
// is the obvious thing to do — no vectors, and the meta key already handed to
// the new model so the two can never disagree half way through.
func RunReindex(ctx context.Context, opts Options, force bool) (ReindexOutcome, error) {
	opts = opts.withDefaults()
	p := opts.Prompt
	want := opts.Config.Embedding

	out := ReindexOutcome{}
	if opts.Index == nil {
		out.Message = "There is no index on this machine yet, so there is nothing to re-embed. " +
			"Whatever `relay embed` last configured is what the first backfill will use."
		p.Section("Re-embed", out.Message)
		return out, nil
	}
	if !want.Configured() {
		out.Message = "No embedding model is configured, so there is nothing to re-embed into. " +
			"Run `relay embed` first — search is keyword-only until you do."
		p.Section("Re-embed", out.Message)
		return out, nil
	}

	state, err := opts.Index.Inspect(ctx)
	if err != nil {
		return out, err
	}
	// The index knows what wrote it; the config knows what is wanted now. The
	// Searcher the CLI builds may be nil, so Current is filled in from the
	// config rather than trusted to be there.
	if state.Current == "" {
		state.Current = want.Model
	}
	out.State = state

	switch {
	case state.Indexed == "" && state.Vectors == 0:
		out.Message = fmt.Sprintf(
			"Nothing is embedded yet — %d summaries, no vectors — so there is nothing to clear. "+
				"relayd will embed them with %s as it works through the backfill.",
			state.Summaries, want.Model)
		p.Section("Re-embed", out.Message)
		return out, nil
	case state.Indexed == want.Model && !force:
		out.Message = fmt.Sprintf(
			"The index was embedded with %s and that is still what is configured, so re-embedding "+
				"would produce the same vectors. Pass --force if you want it anyway.", state.Indexed)
		p.Section("Re-embed", out.Message)
		return out, nil
	}

	out.Needed = true
	body := fmt.Sprintf(
		"The index holds %d vectors written by %s, and %s is configured now. Vectors from two "+
			"models share a space only by coincidence, so search is refusing to mix them and is "+
			"running keyword-only until this is resolved.\n\n"+
			"Re-embedding drops those vectors and lets relayd rebuild them from the %d summaries "+
			"already on disk. It does not re-summarise anything — that was the hour or two, and "+
			"it is done. This part is minutes locally, and well under a dollar on a hosted "+
			"provider.\n\n"+
			"Search keeps working throughout. It is keyword-only until the rebuild finishes, and "+
			"it says so on every result.",
		state.Vectors, orNone(state.Indexed), want.Model, state.Summaries)

	yes, err := p.Confirm(Confirm{
		ID: "reindex.confirm", Prompt: "Drop the old vectors and re-embed?",
		Body: body, Default: true,
	})
	if err != nil {
		return out, err
	}
	out.Confirmed = yes
	if !yes {
		out.Message = "Left alone. Search stays keyword-only while the index and the configured " +
			"model disagree — run `relay embed` and pick " + orNone(state.Indexed) +
			" again to go back instead."
		p.Say("  %s", wrapIndent(out.Message, 2, 76))
		return out, nil
	}

	n, err := opts.Index.Reset(ctx, want.Model)
	if err != nil {
		return out, fmt.Errorf("install: reset the embedding index: %w", err)
	}
	out.Cleared = n
	out.Message = fmt.Sprintf(
		"Dropped %d vectors and handed the index to %s. relayd re-embeds the %d summaries in the "+
			"background the next time it runs; the keyword half never went away.",
		n, want.Model, state.Summaries)
	p.Say("  %s", wrapIndent(out.Message, 2, 76))
	return out, nil
}
