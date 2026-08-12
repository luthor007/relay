package orchestrator

import (
	"context"
	"fmt"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/registry"
	"github.com/luthor007/relay/relayd/internal/search"
)

// FromRegistry adapts the session registry to [Sessions].
//
// Like internal/routing's bridge it reads only what relayd is actually driving.
// A row in the table that no adapter is attached to is history rather than a
// destination: sending to it would mean resuming first, and resume is
// per-runtime and unverified on three of the five (ADAPTERS.md §8). Offering
// the big model a session it cannot reach is a tool call that fails after the
// user has been told it worked.
func FromRegistry(reg *registry.Registry) Sessions {
	return registrySessions{reg: reg}
}

type registrySessions struct{ reg *registry.Registry }

func (r registrySessions) List(context.Context) ([]Session, error) {
	live := r.reg.Live()
	out := make([]Session, 0, len(live))
	for _, e := range live {
		row := e.Row()
		out = append(out, Session{
			ID:        row.ID,
			Runtime:   row.Runtime,
			Subject:   row.Subject,
			Workspace: row.Workspace,
			State:     string(row.State),
			Since:     row.LastActive,
		})
	}
	return out, nil
}

func (r registrySessions) Start(ctx context.Context, runtime, workspace, prompt string) (Session, error) {
	// Subject is the prompt in the user's own words, because it is what the
	// completion ping says out loud. An empty one produces "session 3f2a…",
	// which is worse than a slightly long sentence.
	entry, err := r.reg.Start(ctx, registry.StartOptions{
		Runtime:   adapter.Runtime(runtime),
		Subject:   prompt,
		Workspace: workspace,
	})
	if err != nil {
		return Session{}, err
	}
	row := entry.Row()
	if _, err := r.reg.Send(ctx, row.ID, adapter.Turn{Text: prompt}); err != nil {
		// The session exists but never got its instruction. Saying so beats
		// reporting a start that did nothing.
		return Session{ID: row.ID, Runtime: row.Runtime},
			fmt.Errorf("started %s but could not send the first turn: %w", row.ID, err)
	}
	return Session{
		ID:        row.ID,
		Runtime:   row.Runtime,
		Subject:   row.Subject,
		Workspace: row.Workspace,
		State:     string(row.State),
		Since:     row.LastActive,
	}, nil
}

func (r registrySessions) Send(ctx context.Context, id, text string) error {
	_, err := r.reg.Send(ctx, id, adapter.Turn{Text: text})
	return err
}

func (r registrySessions) Stop(ctx context.Context, id string) error {
	return r.reg.Close(ctx, id)
}

// FromSearcher adapts the hybrid index to [Memory].
func FromSearcher(s *search.Searcher) Memory {
	return searcherMemory{s: s}
}

type searcherMemory struct{ s *search.Searcher }

func (m searcherMemory) Search(ctx context.Context, query string, limit int) ([]Hit, error) {
	res, err := m.s.Search(ctx, search.Query{Text: query, Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]Hit, 0, len(res.Hits))
	for _, h := range res.Hits {
		out = append(out, Hit{
			SessionID: h.Summary.SessionID,
			Title:     h.Title,
			Snippet:   h.Summary.Text,
			When:      h.Summary.CreatedAt,
		})
	}
	return out, nil
}
