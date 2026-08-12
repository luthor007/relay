package connector_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/audit"
	"github.com/luthor007/relay/relayd/internal/connector"
	"github.com/luthor007/relay/relayd/internal/mcp"
)

func printerSet() *connector.Set {
	return connector.NewSet(&connector.Prusa{Base: "http://prusa.local"})
}

func newProposer(t *testing.T, granted mcp.Grants, now time.Time) *connector.Proposer {
	t.Helper()
	p := connector.NewProposer(printerSet(), granted)
	p.Now = func() time.Time { return now }
	return p
}

// ORCHESTRATOR.md §4b's worked example, in the shape the doc gives it:
//
//	> You have mentioned your Prusa four times this week. Want me to connect it?
//	> I could queue prints and tell you when they finish.
func TestFourMentionsInAWeekIsAProposal(t *testing.T) {
	now := at
	p := newProposer(t, nil, now)
	for i := range 4 {
		p.Observe(connector.Evidence{
			Episode: string(rune('a' + i)),
			At:      now.Add(-time.Duration(i+1) * 12 * time.Hour),
			Text:    "the Prusa is jamming again",
		})
	}

	got := p.Proposals(context.Background())
	if len(got) != 1 {
		t.Fatalf("want one proposal, got %d", len(got))
	}
	pr := got[0]
	if pr.Evidence != "You have mentioned your Prusa 3D printer four times this week." {
		t.Fatalf("evidence = %q", pr.Evidence)
	}
	if pr.Access != mcp.AccessRead || len(pr.Scopes) != 1 || pr.Scopes[0] != "prusa:read" {
		t.Fatalf("a proposal is the read half only: %+v", pr)
	}
	if !strings.Contains(pr.Line(), "Want me to connect it?") {
		t.Fatalf("line = %q", pr.Line())
	}
	if !strings.Contains(pr.Line(), "when it will finish") {
		t.Fatalf("the line must say what becomes possible, not restate the permission: %q", pr.Line())
	}
}

// "Ungrounded suggestions are just a settings screen that talks." There is no
// path from a connector list to a proposal.
func TestNoEvidenceIsNoProposal(t *testing.T) {
	p := newProposer(t, nil, at)
	if got := p.Proposals(context.Background()); len(got) != 0 {
		t.Fatalf("want nothing, got %+v", got)
	}
}

// Mentions are counted per conversation. Saying "Prusa" four times in one rant
// is one occasion, and treating it as four is how a suggestion engine becomes a
// nuisance.
func TestMentionsAreCountedPerEpisode(t *testing.T) {
	p := newProposer(t, nil, at)
	for range 6 {
		p.Observe(connector.Evidence{Episode: "same", At: at.Add(-time.Hour), Text: "prusa prusa prusa"})
	}
	if got := p.Proposals(context.Background()); len(got) != 0 {
		t.Fatalf("one conversation is one occasion: %+v", got)
	}
}

func TestEvidenceOutsideTheWindowExpires(t *testing.T) {
	p := newProposer(t, nil, at)
	for i := range 4 {
		p.Observe(connector.Evidence{
			Episode: string(rune('a' + i)),
			At:      at.Add(-30 * 24 * time.Hour),
			Text:    "prusa",
		})
	}
	if got := p.Proposals(context.Background()); len(got) != 0 {
		t.Fatalf("a mention from last month is not a reason now: %+v", got)
	}
}

// The whole point is that it arrives at the moment the capability would have
// been useful — not for something already connected.
func TestAlreadyGrantedIsNotProposed(t *testing.T) {
	g := &connector.Grants{
		Store: connector.NewMemoryStore(), Audit: audit.NewMemory(),
		NewID: func() string { return "g1" },
	}
	ctx := context.Background()
	if _, _, err := g.Grant(ctx, connector.GrantRequest{
		Connector: "prusa", Access: mcp.AccessRead, Decided: true, By: "console",
	}); err != nil {
		t.Fatal(err)
	}

	p := newProposer(t, g, at)
	for i := range 4 {
		p.Observe(connector.Evidence{Episode: string(rune('a' + i)), At: at, Text: "prusa"})
	}
	if got := p.Proposals(ctx); len(got) != 0 {
		t.Fatalf("want nothing for an already-granted connector, got %+v", got)
	}
}

func TestDismissalIsRespected(t *testing.T) {
	p := newProposer(t, nil, at)
	for i := range 4 {
		p.Observe(connector.Evidence{Episode: string(rune('a' + i)), At: at, Text: "prusa"})
	}
	if len(p.Proposals(context.Background())) != 1 {
		t.Fatal("setup")
	}
	p.Dismiss("prusa")
	if got := p.Proposals(context.Background()); len(got) != 0 {
		t.Fatalf("a dismissed proposal is not repeated next week: %+v", got)
	}
}

// The write half is never proposed. §4b: the write half should cost a second
// decision, and a write half offered alongside the read one is the same click.
func TestTheWriteHalfIsNeverProposed(t *testing.T) {
	p := newProposer(t, nil, at)
	for i := range 5 {
		p.Observe(connector.Evidence{Episode: string(rune('a' + i)), At: at, Text: "start a print on the prusa"})
	}
	for _, pr := range p.Proposals(context.Background()) {
		if pr.Access != mcp.AccessRead {
			t.Fatalf("proposal offered %s: %+v", pr.Access, pr)
		}
		for _, s := range pr.Scopes {
			if strings.HasSuffix(s, ":write") {
				t.Fatalf("a write scope was proposed: %v", pr.Scopes)
			}
		}
	}
}

// A proposal is a proposal. This type holds a read-only view of the grants and
// has no way to change them, which is rule 1 held by the type rather than by
// review.
func TestAProposerCannotGrant(t *testing.T) {
	g := &connector.Grants{
		Store: connector.NewMemoryStore(), Audit: audit.NewMemory(),
		NewID: func() string { return "g1" },
	}
	p := newProposer(t, g, at)
	for i := range 4 {
		p.Observe(connector.Evidence{Episode: string(rune('a' + i)), At: at, Text: "prusa"})
	}
	if len(p.Proposals(context.Background())) != 1 {
		t.Fatal("setup")
	}
	if ok, _ := g.Allowed(context.Background(), "prusa", mcp.AccessRead); ok {
		t.Fatal("making a proposal granted something")
	}
	// Its only reference to the grant layer is mcp.Grants, whose sole method is
	// a read. This assignment is the compile-time half of the assertion.
	var _ mcp.Grants = g
}

// A false match is a claim about what the user said, which is worse than no
// proposal at all.
func TestMentionsMatchWordsNotSubstrings(t *testing.T) {
	p := newProposer(t, nil, at)
	for i := range 5 {
		p.Observe(connector.Evidence{
			Episode: string(rune('a' + i)), At: at,
			Text: "the prusalinker library and mk3sX are unrelated things",
		})
	}
	if got := p.Proposals(context.Background()); len(got) != 0 {
		t.Fatalf("substring matches must not count: %+v", got)
	}
}

func TestEntitiesCountAsEvidence(t *testing.T) {
	p := newProposer(t, nil, at)
	for i := range 3 {
		p.Observe(connector.Evidence{
			Episode: string(rune('a' + i)), At: at, Entities: []string{"Prusa"},
		})
	}
	if got := p.Proposals(context.Background()); len(got) != 1 {
		t.Fatalf("extracted entities are the evidence §4b names: %+v", got)
	}
}

func TestObserveEpisodeIsTheCapturePipelineDoor(t *testing.T) {
	p := newProposer(t, nil, at)
	for i := range 3 {
		matched := p.ObserveEpisode(connector.Episode{
			ID: string(rune('a' + i)), At: at, Text: "waiting on the 3D printer",
		})
		if len(matched) != 1 || matched[0] != "prusa" {
			t.Fatalf("matched = %v", matched)
		}
	}
	if got := p.Proposals(context.Background()); len(got) != 1 {
		t.Fatalf("proposals = %+v", got)
	}
}
