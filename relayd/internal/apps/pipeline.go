package apps

import (
	"context"
	"time"

	"github.com/luthor007/relay/relayd/internal/episode"
)

// Memory triggers, wired to the pipeline that produces them.
//
// APP-PLATFORM.md §4 names three pipeline events — `meeting.ended`,
// `commitment.detected`, `day.synced` — and the SDK adds `episode.created`. They
// come out of M4's episode pipeline, and this file is the only place that knows
// how to read that pipeline's output as those four names.
//
// It lives here rather than in `internal/episode` on purpose: the episode
// pipeline should not know that apps exist. It writes episodes; this reads what
// it wrote and decides which of four names that was. The direction of the
// dependency is the point — an app platform that the memory pipeline has to be
// changed for is an app platform whose triggers are a maintenance burden on the
// thing that matters more.
//
// # An event is only emitted when it was observed
//
// The rule that runs through the adapters applies to triggers as well: nothing
// here emits an event it cannot see. `commitment.detected` fires when the write
// actually stored commitments, not when extraction was attempted;
// `meeting.ended` fires on an episode the segmenter classified as a meeting, not
// on one that merely had two speakers. An app woken by `meeting.ended` that finds
// no meeting learns to stop trusting the trigger.

// EpisodeStored is the hook for `internal/episode`'s writer: one episode has
// been segmented, redacted, extracted and persisted.
//
// It fires up to three of the four events, in the order an app would expect to
// see them: the specific one first, then the general one, because an app that
// declared both wants to have handled `meeting.ended` before it sees
// `episode.created` for the same episode.
func (d *Dispatcher) EpisodeStored(ctx context.Context, e episode.Episode, res episode.WriteResult) []Invocation {
	id := res.EpisodeID
	if id == "" {
		id = e.ID
	}
	var out []Invocation
	if e.Kind == episode.KindMeeting {
		out = append(out, d.Event(ctx, EventMeetingEnded, id)...)
	}
	if res.Commitments > 0 {
		out = append(out, d.Event(ctx, EventCommitmentDetected, id)...)
	}
	out = append(out, d.Event(ctx, EventEpisodeCreated, id)...)
	return out
}

// DaySynced is the hook for the nightly bulk sync: a day's audio has landed,
// been transcribed and been written.
//
// It carries no episode id because a day is not an episode. An app woken by it
// searches for what it wants — which is the honest shape, since "the day" is a
// range and the app knows better than this package which part of it matters.
func (d *Dispatcher) DaySynced(ctx context.Context, _ time.Time) []Invocation {
	return d.Event(ctx, EventDaySynced, "")
}

// EpisodeSource adapts `internal/episode`'s types to the [Source] an app's
// memory capability reads.
//
// It is a conversion and not a store: the store lives in `internal/store` and
// the search in `internal/search`, and an app runtime that reached into either
// directly would be a second implementation of retrieval that drifts from the
// one the user's own agent uses. [Lookup] and [Searcher] are the seams.
type EpisodeSource struct {
	// Lookup returns one episode by id, and the most recent of a kind.
	Lookup Lookup
	// Retrieval is the search the box already has — `internal/search`'s hybrid
	// ranker, wired in by whoever builds this. Nil falls back to a keyword match
	// over the recent list, which is weaker and is named as such in
	// [StaticSource]: a fallback that presents itself as the real ranker teaches
	// an app author something false about relevance.
	Retrieval Searcher
	// Extractor is the deterministic commitment extractor. Nil uses
	// `internal/episode`'s rules, which is what the nightly job uses.
	Extractor func(episode.Episode, episode.Options) episode.Extraction
	Options   episode.Options
}

// Lookup is the read half of the episode store this package needs.
type Lookup interface {
	Episode(ctx context.Context, id string) (*Episode, error)
	Recent(ctx context.Context, kind string, within time.Duration) (*Episode, error)
	List(ctx context.Context, limit int) ([]Episode, error)
}

// Searcher is the box's retrieval, narrowed to what an app may ask of it.
type Searcher interface {
	SearchEpisodes(ctx context.Context, q Query) ([]Episode, error)
}

var _ Source = (*EpisodeSource)(nil)

func (s *EpisodeSource) Search(ctx context.Context, q Query) ([]Episode, error) {
	if s.Retrieval != nil {
		return s.Retrieval.SearchEpisodes(ctx, q)
	}
	if s.Lookup == nil {
		return nil, nil
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultMaxResults
	}
	all, err := s.Lookup.List(ctx, limit*4)
	if err != nil {
		return nil, err
	}
	fallback := &StaticSource{Episodes: all}
	return fallback.Search(ctx, q)
}

func (s *EpisodeSource) Recent(ctx context.Context, kind string, within time.Duration) (*Episode, error) {
	if s.Lookup == nil {
		return nil, nil
	}
	return s.Lookup.Recent(ctx, kind, within)
}

func (s *EpisodeSource) Get(ctx context.Context, id string) (*Episode, error) {
	if s.Lookup == nil {
		return nil, nil
	}
	return s.Lookup.Episode(ctx, id)
}

// Extract runs the same rule-based extractor the nightly job runs.
//
// Deterministic on purpose, and the reason is `internal/episode`'s: a commitment
// carries a person's name and a date, and a model that invents either produces a
// confident wrong reminder. An app that wants a model's opinion has `ctx.agent`
// and can ask for one under its own name.
func (s *EpisodeSource) Extract(_ context.Context, e Episode) ([]Commitment, error) {
	ep := episode.Episode{
		ID: e.ID, Kind: episode.Kind(e.Kind), StartedAt: e.StartedAt, EndedAt: e.EndedAt,
		Participants: e.Participants, Location: e.Location,
		Utterances: []episode.Utterance{{Text: e.Transcript, At: e.StartedAt}},
	}
	extract := s.Extractor
	if extract == nil {
		extract = func(e episode.Episode, o episode.Options) episode.Extraction { return episode.Extract(e, o) }
	}
	ex := extract(ep, s.Options)
	out := make([]Commitment, 0, len(ex.Commitments))
	for _, c := range ex.Commitments {
		cm := Commitment{Text: c.Text, To: c.OwedTo, SourceEpisodeID: e.ID}
		if !c.DueAt.IsZero() {
			due := c.DueAt
			cm.DueAt = &due
		}
		out = append(out, cm)
	}
	return out, nil
}
