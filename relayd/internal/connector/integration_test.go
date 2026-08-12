package connector_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/audit"
	"github.com/luthor007/relay/relayd/internal/bus"
	"github.com/luthor007/relay/relayd/internal/connector"
	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/mcp"
)

// The whole of SYSTEM.md §10 step 6, end to end: evidence becomes a proposal,
// a decision becomes a grant, the grant becomes a tool on the shared bus, the
// consequential half stops at the glasses, and one revoke takes it away from
// every runtime at once — with the running sessions told which happened to them.

type liveSessions struct {
	live     []mcp.SessionInfo
	restarts []string
}

func (l *liveSessions) LiveSessions() []mcp.SessionInfo { return l.live }
func (l *liveSessions) Restart(_ context.Context, id string) error {
	l.restarts = append(l.restarts, id)
	return nil
}

// answerer says yes to every confirmation and records what it was asked, which
// is what a person at the glasses does.
func answerer(t *testing.T, b *bus.Bus, said *[]string, decision string) func() {
	t.Helper()
	sub := b.Subscribe("glasses", bus.Filter{Kinds: []event.Kind{event.KindNeedsInput}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range sub.C() {
			ask, ok := ev.(*event.NeedsInput)
			if !ok {
				continue
			}
			*said = append(*said, ask.Prompt)
			_ = ask.Reply(context.Background(), event.Reply{OptionID: decision})
		}
	}()
	return func() { sub.Close(); <-done }
}

func TestGrantOnceWorksEverywhereAndRevokeOnceRemovesIt(t *testing.T) {
	ctx := context.Background()

	printer := newPrusa(&stubHTTP{answers: map[string]stubAnswer{
		"GET /api/v1/status":                  {200, statusPrinting},
		"POST /api/v1/files/usb/benchy.gcode": {204, ""},
	}})
	set := connector.NewSet(printer)

	log := audit.NewMemory()
	grants := &connector.Grants{
		Store: connector.NewMemoryStore(), Audit: log,
		Now: func() time.Time { return at }, NewID: func() string { return "g-1" },
	}

	b := bus.New(bus.Options{})
	defer b.Close()
	var spoken []string
	stop := answerer(t, b, &spoken, mcp.OptionConfirmAllow)
	defer stop()

	sessions := &liveSessions{live: []mcp.SessionInfo{
		{ID: "cc", Runtime: "claude-code"},
		{ID: "hermes", Runtime: "hermes"},
	}}
	recorder := &mcp.MemoryRecorder{}
	g := mcp.NewGateway(mcp.Options{
		Name:     "relay",
		Grants:   grants,
		Confirm:  &mcp.BusConfirmer{Bus: b, Wait: 5 * time.Second},
		Record:   recorder,
		Audit:    log,
		Sessions: sessions,
	})
	grants.Refresher = g
	g.Register(ctx, set)

	// 1. Nothing is granted, so nothing is visible and nothing is callable.
	if got := g.Tools(ctx); len(got) != 0 {
		t.Fatalf("an install grants nothing: %v", got)
	}
	if _, err := g.Call(ctx, mcp.Call{Tool: "prusa_status"}); !errors.Is(err, mcp.ErrNotGranted) {
		t.Fatalf("want ErrNotGranted, got %v", err)
	}

	// 2. Evidence produces a proposal, and the proposal grants nothing.
	prop := connector.NewProposer(set, grants)
	prop.Now = func() time.Time { return at }
	for i := range 4 {
		prop.ObserveEpisode(connector.Episode{
			ID: string(rune('a' + i)), At: at.Add(-time.Duration(i) * 20 * time.Hour),
			Text: "the prusa is still going",
		})
	}
	proposals := prop.Proposals(ctx)
	if len(proposals) != 1 || proposals[0].Access != mcp.AccessRead {
		t.Fatalf("want one read-half proposal, got %+v", proposals)
	}
	if got := g.Tools(ctx); len(got) != 0 {
		t.Fatal("a proposal is a proposal, not a grant")
	}

	// 3. The person says yes to the read half.
	if _, refresh, err := grants.Grant(ctx, connector.GrantRequest{
		Connector: proposals[0].Connector, Access: proposals[0].Access,
		Decided: true, By: "glasses", Opens: proposals[0].Opens, From: "proposal-1",
	}); err != nil {
		t.Fatal(err)
	} else if refresh.Note == "" {
		t.Fatal("a grant has to say what it meant for running sessions")
	}

	visible := map[string]bool{}
	for _, tool := range g.Tools(ctx) {
		visible[tool.Name] = true
	}
	if !visible["prusa_status"] || !visible["prusa_files"] {
		t.Fatalf("the read half should be visible: %v", visible)
	}
	if visible["prusa_print"] || visible["prusa_stop"] {
		t.Fatalf("the write half is a second decision: %v", visible)
	}
	if _, err := g.Call(ctx, mcp.Call{Tool: "prusa_status", Session: "cc"}); err != nil {
		t.Fatalf("the granted read call failed: %v", err)
	}
	if len(spoken) != 0 {
		t.Fatalf("a read must not stop to ask: %v", spoken)
	}

	// 4. The write half costs its own decision, and then confirms every time.
	if _, err := g.Call(ctx, mcp.Call{Tool: "prusa_print", Arguments: map[string]any{"path": "benchy.gcode"}}); !errors.Is(err, mcp.ErrNotGranted) {
		t.Fatalf("want ErrNotGranted for the write half, got %v", err)
	}
	if _, _, err := grants.Grant(ctx, connector.GrantRequest{
		Connector: "prusa", Access: mcp.AccessWrite, Decided: true, By: "console",
	}); err != nil {
		t.Fatal(err)
	}

	for i := range 2 {
		res, err := g.Call(ctx, mcp.Call{
			Tool: "prusa_print", Session: "cc", Runtime: "claude-code",
			Arguments: map[string]any{"path": "benchy.gcode"},
		})
		if err != nil {
			t.Fatalf("print %d: %v", i, err)
		}
		if !strings.Contains(res.Text, "benchy.gcode") {
			t.Fatalf("res = %+v", res)
		}
	}
	if len(spoken) != 2 {
		t.Fatalf("every consequential call confirms: %v", spoken)
	}
	for _, s := range spoken {
		if !strings.Contains(s, "starts a print") || !strings.Contains(s, "benchy.gcode") {
			t.Fatalf("the spoken prompt has to say what and to what: %q", s)
		}
	}

	// 5. One revoke, and it is gone for all five runtimes at once.
	res, err := grants.Revoke(ctx, "prusa")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Runtimes) != 5 {
		t.Fatalf("runtimes = %+v", res.Runtimes)
	}
	if got := g.Tools(ctx); len(got) != 0 {
		t.Fatalf("a revoked connector must vanish from the bus: %v", got)
	}
	if _, err := g.Call(ctx, mcp.Call{Tool: "prusa_status"}); !errors.Is(err, mcp.ErrNotGranted) {
		t.Fatalf("want ErrNotGranted after revoke, got %v", err)
	}

	// 6. And the sessions that were running are told which happened to them.
	if !strings.Contains(res.Note, "next turn") {
		t.Fatalf("Claude Code's own refresh has to be named: %q", res.Note)
	}
	if !strings.Contains(res.Note, "Restart") {
		t.Fatalf("the ACP session's manual restart has to be named: %q", res.Note)
	}
	if len(sessions.restarts) != 0 {
		t.Fatalf("nothing may be restarted behind the user's back: %v", sessions.restarts)
	}

	// 7. Every call went through one place, refusals included.
	calls := recorder.Calls()
	if len(calls) < 6 {
		t.Fatalf("the trail is short: %+v", calls)
	}
	var completed, denied int
	for _, c := range calls {
		switch c.Status {
		case "completed":
			completed++
		case "denied":
			denied++
		}
		if c.Connector != "prusa" {
			t.Fatalf("every row is attributed to the connector: %+v", c)
		}
	}
	if completed != 3 || denied < 3 {
		t.Fatalf("completed=%d denied=%d: %+v", completed, denied, calls)
	}

	// 8. The audit chain covers the grants and the revoke, and it holds.
	entries, err := log.List(ctx, audit.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if err := audit.Verify(entries); err != nil {
		t.Fatalf("the chain must hold: %v", err)
	}
	var grantsSeen, revokes int
	for _, e := range entries {
		switch e.Action {
		case audit.ActionConnectorGrant:
			grantsSeen++
		case audit.ActionConnectorRevoke:
			revokes++
		}
	}
	if grantsSeen != 4 || revokes != 2 {
		t.Fatalf("grant/revoke entries = %d/%d", grantsSeen, revokes)
	}
}

// The user saying no at the glasses stops the print, and the printer never
// hears about it.
func TestSayingNoAtTheGlassesStopsTheAction(t *testing.T) {
	ctx := context.Background()
	h := &stubHTTP{answers: map[string]stubAnswer{
		"POST /api/v1/files/usb/benchy.gcode": {204, ""},
	}}
	set := connector.NewSet(newPrusa(h))

	grants := &connector.Grants{
		Store: connector.NewMemoryStore(), Audit: audit.NewMemory(),
		NewID: func() string { return "g-1" },
	}
	if _, _, err := grants.Grant(ctx, connector.GrantRequest{
		Connector: "prusa", Access: mcp.AccessWrite, Decided: true, By: "console",
	}); err != nil {
		t.Fatal(err)
	}

	b := bus.New(bus.Options{})
	defer b.Close()
	var spoken []string
	stop := answerer(t, b, &spoken, mcp.OptionConfirmDeny)
	defer stop()

	g := mcp.NewGateway(mcp.Options{
		Grants:  grants,
		Confirm: &mcp.BusConfirmer{Bus: b, Wait: 5 * time.Second},
	})
	g.Register(ctx, set)

	_, err := g.Call(ctx, mcp.Call{Tool: "prusa_print", Arguments: map[string]any{"path": "benchy.gcode"}})
	if !errors.Is(err, mcp.ErrDenied) {
		t.Fatalf("want ErrDenied, got %v", err)
	}
	for _, seen := range h.seen {
		if strings.HasPrefix(seen, http.MethodPost) {
			t.Fatalf("the printer was told to print anyway: %v", h.seen)
		}
	}
	if len(spoken) != 1 {
		t.Fatalf("the user should have been asked exactly once: %v", spoken)
	}
}
