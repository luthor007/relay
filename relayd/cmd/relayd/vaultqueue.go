package main

import (
	"context"

	"github.com/luthor007/relay/relayd/internal/api"
	"github.com/luthor007/relay/relayd/internal/vault"
)

// proposalQueue joins MEMORY.md §6's confirmation queue to the console screen
// that answers it.
//
// The two halves were both written, both tested, and could not be connected:
// vault.Proposals.List returns []vault.Proposal and api.ProposalStore.List
// returns []api.Proposal, so the vault's queue does not satisfy the API's
// interface and no adapter existed in any package. Accept and Dismiss line up
// exactly; only the listing needed translating. Until this file, api.New was
// called without Proposals, /v1/credentials/proposals fell back to the index's
// raw secret markers, and POST .../accept answered 501 after recording a
// refusal in the audit log — which the console has a branch for, which is why
// nobody noticed that no proposal had ever been accepted.
//
// It lives in cmd/relayd rather than in internal/api on purpose: internal/api
// deliberately holds only the narrowed view of the vault (no Reveal), and the
// composition root is where two packages that do not know about each other are
// allowed to meet.
type proposalQueue struct{ q vault.Proposals }

// The compiler checks the claim rather than a comment making it.
var _ api.ProposalStore = proposalQueue{}

// proposals returns the console's view of this vault's queue, or nil.
//
// Nil is meaningful and must be preserved: api.Server falls back to listing the
// index's secret markers when there is no queue, and that fallback carries a
// note saying these are detections rather than offers. A non-nil adapter around
// a nil vault would replace an honest fallback with a permanently empty list.
func proposals(v vault.Vault) api.ProposalStore {
	if v == nil {
		return nil
	}
	return proposalQueue{q: v.Proposals()}
}

func (p proposalQueue) List(ctx context.Context) ([]api.Proposal, error) {
	list, err := p.q.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]api.Proposal, 0, len(list))
	for _, v := range list {
		// FoundAt is when the key was *seen*, which is what the proposal line
		// says ("in a session from March"). A candidate whose source recorded no
		// time falls back to when the question was asked — never to zero, which
		// the console renders as a date it was never told.
		found := v.Source.At
		if found.IsZero() {
			found = v.CreatedAt
		}
		out = append(out, api.Proposal{
			ID:       v.ID,
			Service:  v.Service,
			Detector: v.Detector,
			Runtime:  v.Source.Runtime,
			Session:  v.Source.Session,
			Path:     v.Source.Path,
			// ByteOffset stays zero. The queue holds the candidate itself —
			// sealed, and recovered by Accept — so nothing has to re-read the
			// transcript at an offset, and the queue never observed one. An
			// invented offset would be a field the console could act on and we
			// could not stand behind.
			LastFour:      v.LastFour,
			SharedSession: v.Source.SharedSession,
			FoundAt:       found.UnixMilli(),
		})
	}
	return out, nil
}

func (p proposalQueue) Accept(ctx context.Context, id, label string) (vault.Entry, error) {
	return p.q.Accept(ctx, id, label)
}

func (p proposalQueue) Dismiss(ctx context.Context, id, reason string) error {
	return p.q.Dismiss(ctx, id, reason)
}
