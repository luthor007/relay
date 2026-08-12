package main

import (
	"context"
	"log/slog"

	"github.com/luthor007/relay/relayd/internal/bus"
	"github.com/luthor007/relay/relayd/internal/config"
	"github.com/luthor007/relay/relayd/internal/connector"
	"github.com/luthor007/relay/relayd/internal/facts"
	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/search"
	"github.com/luthor007/relay/relayd/internal/store"
	"github.com/luthor007/relay/relayd/internal/summarize"
)

// Subsystem names on the health screen.
const (
	SubsystemSummaries = "summaries"
	SubsystemFacts     = "fact_extraction"
	SubsystemEmbedder  = "embedder"
)

// startMemory wires MEMORY.md §4's live ingestion path and returns a stop
// function.
//
// Every TurnCompleted writes a turn summary, updates the session row, and
// re-runs fact extraction for that session. All three pieces existed and none
// of them had a caller: internal/summarize.Live was constructed by nothing, so
// nothing was ever summarised on the live path, and facts.Bridge — the adapter
// whose whole purpose is joining the fact tier to the summariser — was written
// and never used. The `remember` tool was the manual half of a memory with no
// automatic half at all.
//
// Two model choices worth stating, because they cost money either way:
//
//   - Summaries use the **small** model. MEMORY.md §10 measures the corpus at
//     3.6 GB and puts it "through a small model" for an hour or two; the live
//     path is the same job at one turn a time.
//   - Facts use the **big** one. It runs at most once every two minutes per
//     session against summaries rather than transcripts, so the volume is tiny
//     — and a wrong durable fact does not fail loudly, it sits in memory
//     decaying for months and quietly steers answers.
//
// Both degrade rather than fail: with no model the summariser writes
// deterministic metadata rows, which MEMORY.md §11 step 2b calls most of the
// value, and fact extraction reports itself off.
func startMemory(ctx context.Context, deps memoryDeps) func() {
	report, log := deps.report, deps.log
	if deps.db == nil || deps.bus == nil {
		report(SubsystemSummaries, "no database or event bus")
		return func() {}
	}

	embedder := buildEmbedder(deps.cfg, deps.lookups, report, log)

	sum, err := summarize.New(summarize.Options{
		DB:    deps.db,
		Model: deps.small,
		// Nil writes the lexical half of the index only, which is honest
		// degradation: search reports which half did not run rather than
		// handing back quietly worse results.
		Embedder: embedder,
		// Required, and required for a reason: detection happens before
		// indexing, never after, and an embedded key cannot be unembedded.
		Redactor: summarize.Detector(),
		Log:      log,
	})
	if err != nil {
		log.Warn("relayd: no live summaries; sessions will only be indexed by backfill",
			"error", err)
		report(SubsystemSummaries, err.Error())
		return func() {}
	}
	report(SubsystemSummaries, statusOf(true, ""))

	live, err := summarize.NewLive(summarize.LiveOptions{
		Summarizer: sum,
		Facts:      factExtraction(deps, report),
		Log:        log,
	})
	if err != nil {
		log.Warn("relayd: live ingestion did not start", "error", err)
		report(SubsystemSummaries, err.Error())
		return func() {}
	}

	// Every event, not just the pings: Live folds text, tools and reasoning
	// into a digest and writes on TurnCompleted, so a filtered subscription
	// would summarise a turn from its last event alone.
	sub := deps.bus.Subscribe("memory", bus.Filter{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer sub.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-sub.C():
				if !ok {
					return
				}
				out, err := live.Handle(ctx, ev)
				if err != nil {
					// One turn that failed to summarise is one gap in the
					// index, not a reason to stop consuming — and the next
					// turn's summary is still worth writing.
					log.Warn("relayd: could not summarise a turn",
						"session", ev.Envelope().Session, "error", err)
				}
				// ORCHESTRATOR.md §4b's second evidence feed. The summary, not
				// the transcript: it is already redacted and it is what the
				// session was about, which is closer to a "mention" than any
				// single event is.
				//
				// UNOBSERVED IN THIS CONTAINER, and said so rather than
				// implied. Producing a summary needs an agent runtime emitting
				// event.KindTurnCompleted, and none is installed on the build
				// machine — startAdapters finds nothing and the banner prints
				// "runtimes none found". This join is real code on a user's
				// machine and it runs zero times here, so the row's evidence
				// rests on the utterance feed in routing.go instead.
				if deps.proposer != nil && out != nil && out.Written() {
					deps.proposer.Observe(connector.Evidence{
						Episode: ev.Envelope().Session,
						At:      ev.Envelope().At,
						Text:    out.Text,
					})
				}
			}
		}
	}()
	return func() { <-done }
}

// memoryDeps is what the live path needs. A struct because the alternative is
// seven positional arguments, three of which are optional.
type memoryDeps struct {
	db    *store.DB
	bus   *bus.Bus
	cfg   config.Config
	small llm.Provider
	big   llm.Provider
	// lookups resolves an `embedding.credential = "vault:<id>"`. The zero value
	// resolves nothing, which is the honest state on a daemon with no vault.
	lookups credentialLookups
	// proposer is ORCHESTRATOR.md §4b's connector proposer, fed from the
	// summaries this loop writes. Nil on a machine with no connectors.
	proposer *connector.Proposer
	report   func(string, string)
	log      *slog.Logger
}

// factExtraction builds the durable-fact half, or reports why it is off.
//
// MEMORY.md §5's five rules are enforced in internal/facts rather than here:
// every fact carries evidence, decay runs on last observation, contradictions
// supersede, everything is editable, nothing is a secret. This only decides
// whether the tier gets fed.
func factExtraction(deps memoryDeps, report func(string, string)) summarize.FactExtractor {
	if deps.big == nil {
		// Explicit rather than a nil check downstream, so "facts are off"
		// appears in the wiring and on the health screen instead of being
		// inferred from an empty table.
		report(SubsystemFacts, "no work model configured")
		return summarize.NoFacts{}
	}

	store, err := facts.Open(deps.db, facts.Options{Redactor: facts.Detector()})
	if err != nil {
		report(SubsystemFacts, err.Error())
		return summarize.NoFacts{}
	}

	updater, err := facts.NewUpdater(facts.UpdaterOptions{
		Extractor: &facts.LLM{
			DB:     deps.db,
			Model:  deps.big,
			Redact: facts.Detector(),
			Log:    deps.log,
		},
		Store: store,
		Log:   deps.log,
	})
	if err != nil {
		report(SubsystemFacts, err.Error())
		return summarize.NoFacts{}
	}

	report(SubsystemFacts, "on")
	return facts.Bridge{Updater: updater}
}

// buildEmbedder builds the vector half of the index, or reports why there is
// none.
//
// A missing embedder is a supported configuration and not a broken one:
// MEMORY.md §11 step 2b ships "a plain indexed list of every session with its
// title, repo and date" before anything vectorised, and calls that most of the
// value. What must not happen is a *mismatched* one — search refuses an
// embedder whose width is not the index's, because a silently-swapped model
// produces a vector half that returns confident nonsense.
func buildEmbedder(cfg config.Config, lookups credentialLookups, report func(string, string), log *slog.Logger) search.Embedder {
	e := cfg.Embedding
	if e.Provider == "" || e.Provider == config.EmbedProviderNone || e.Model == "" {
		report(SubsystemEmbedder, "not configured; search is lexical only")
		return nil
	}

	ref, err := llm.ParseRef(e.Credential)
	if err != nil {
		report(SubsystemEmbedder, "credential is not a usable reference")
		log.Warn("relayd: embedding credential is not a reference",
			"error", err,
			"detail", "credentials are references — env:, file:, exec: or vault: — never pasted secrets")
		return nil
	}

	emb, err := llm.NewEmbedder(llm.EmbedConfig{
		Provider:   llm.EmbedProvider(e.Provider),
		Vendor:     e.Provider,
		BaseURL:    e.BaseURL,
		Model:      e.Model,
		Dims:       e.Dims,
		Credential: ref,
		// Without this a "vault:" embedding credential resolves to nothing at
		// first use, and the failure surfaces as an embedding call that will not
		// authenticate rather than as anything naming the vault.
		Lookup: lookups.resolver(usedBy(SubsystemEmbedder)),
	})
	if err != nil {
		report(SubsystemEmbedder, err.Error())
		log.Warn("relayd: no embedder; search will be lexical only", "error", err)
		return nil
	}
	report(SubsystemEmbedder, "on")
	return emb
}
