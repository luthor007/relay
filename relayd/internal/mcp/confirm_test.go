package mcp_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/bus"
	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/mcp"
)

// awaitAsk pulls the next NeedsInput off a subscription.
func awaitAsk(t *testing.T, sub *bus.Subscription) *event.NeedsInput {
	t.Helper()
	select {
	case ev := <-sub.C():
		ask, ok := ev.(*event.NeedsInput)
		if !ok {
			t.Fatalf("want a NeedsInput on the bus, got %T", ev)
		}
		return ask
	case <-time.After(2 * time.Second):
		t.Fatal("no confirmation reached the bus")
		return nil
	}
}

// ADAPTERS.md §7: the confirmation is a blocking ping — it may interrupt, it
// never batches, and quiet hours do not apply. All three of those follow from
// the event being a NeedsInput whose Ping is PingBlocking, which is why this
// path reuses the existing mechanism rather than inventing a second one.
func TestConfirmationIsABlockingNeedsInput(t *testing.T) {
	b := bus.New(bus.Options{})
	defer b.Close()
	sub := b.Subscribe("test", bus.Filter{})
	defer sub.Close()

	c := confirmerFor(b)
	done := make(chan error, 1)
	go func() {
		done <- c.Confirm(context.Background(), mcp.Confirmation{
			Connector: "prusa", Tool: "prusa_print",
			Consequence: "starts a print on your Prusa",
			Target:      "benchy.gcode", Session: "s1", Runtime: "claude-code",
		})
	}()

	ask := awaitAsk(t, sub)
	if ask.Ping() != event.PingBlocking {
		t.Fatalf("a consequential confirmation must ping blocking, got %s", ask.Ping())
	}
	if ask.Ask != event.InputPermission {
		t.Fatalf("want InputPermission so bus.Pinger marks it Consequential, got %s", ask.Ask)
	}
	if !strings.Contains(ask.Prompt, "starts a print on your Prusa") ||
		!strings.Contains(ask.Prompt, "benchy.gcode") {
		t.Fatalf("the spoken prompt must say what happens and to what: %q", ask.Prompt)
	}
	if ask.Envelope().Session != "s1" || ask.Envelope().Runtime != "claude-code" {
		t.Fatalf("the question must carry who asked: %+v", ask.Envelope())
	}
	if ask.Envelope().Replay {
		t.Fatal("a confirmation is never a replay; a replayed event never pings")
	}

	if err := ask.Reply(context.Background(), event.Reply{OptionID: mcp.OptionConfirmAllow}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("an allowed confirmation must succeed: %v", err)
	}
}

// A standing option is exactly what rule 3 forbids: the value of the
// confirmation is that it cannot be switched off, and "always allow" is the
// switch.
func TestConfirmationNeverOffersAStandingOption(t *testing.T) {
	for _, o := range mcp.ConfirmOptions() {
		if o.Kind.Standing() {
			t.Fatalf("option %q is standing, which would let one yes cover every future print", o.ID)
		}
	}
	if len(mcp.ConfirmOptions()) != 2 {
		t.Fatalf("there are two answers to a confirmation, got %d", len(mcp.ConfirmOptions()))
	}
}

func TestDecliningIsARefusal(t *testing.T) {
	b := bus.New(bus.Options{})
	defer b.Close()
	sub := b.Subscribe("test", bus.Filter{})
	defer sub.Close()

	c := confirmerFor(b)
	done := make(chan error, 1)
	go func() {
		done <- c.Confirm(context.Background(), mcp.Confirmation{Tool: "t", Consequence: "spends money"})
	}()

	ask := awaitAsk(t, sub)
	if err := ask.Reply(context.Background(), event.Reply{OptionID: mcp.OptionConfirmDeny}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, mcp.ErrDenied) {
		t.Fatalf("want ErrDenied, got %v", err)
	}
}

// A question withdrawn from the runtime's side — answered in a terminal, the
// session died — is not a yes. Anything that is not an explicit yes is a no.
func TestWithdrawnConfirmationIsNotConsent(t *testing.T) {
	b := bus.New(bus.Options{})
	defer b.Close()
	sub := b.Subscribe("test", bus.Filter{})
	defer sub.Close()

	c := confirmerFor(b)
	done := make(chan error, 1)
	go func() {
		done <- c.Confirm(context.Background(), mcp.Confirmation{Tool: "t", Consequence: "opens a door"})
	}()

	awaitAsk(t, sub).Withdraw("answered in a terminal")
	if err := <-done; !errors.Is(err, mcp.ErrUnanswered) {
		t.Fatalf("want ErrUnanswered, got %v", err)
	}
}

func TestUnansweredConfirmationExpires(t *testing.T) {
	b := bus.New(bus.Options{})
	defer b.Close()
	sub := b.Subscribe("test", bus.Filter{})
	defer sub.Close()

	c := &mcp.BusConfirmer{Bus: b, Wait: 20 * time.Millisecond}
	done := make(chan error, 1)
	go func() {
		done <- c.Confirm(context.Background(), mcp.Confirmation{Tool: "t", Consequence: "sends mail as you"})
	}()

	ask := awaitAsk(t, sub)
	select {
	case err := <-done:
		if !errors.Is(err, mcp.ErrUnanswered) {
			t.Fatalf("want ErrUnanswered, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the confirmation never expired")
	}
	if !ask.Answered() {
		t.Fatal("an expired question must be closed, or its ping outlives it")
	}
	// A late yes must not resurrect the action.
	if err := ask.Reply(context.Background(), event.Reply{OptionID: mcp.OptionConfirmAllow}); !errors.Is(err, event.ErrWithdrawn) {
		t.Fatalf("a late answer must be refused, got %v", err)
	}
}

func TestConfirmerWithNoBusRefuses(t *testing.T) {
	var c *mcp.BusConfirmer
	if err := c.Confirm(context.Background(), mcp.Confirmation{Tool: "t", Consequence: "prints"}); !errors.Is(err, mcp.ErrNoConfirmer) {
		t.Fatalf("want ErrNoConfirmer, got %v", err)
	}
	empty := &mcp.BusConfirmer{}
	if err := empty.Confirm(context.Background(), mcp.Confirmation{Tool: "t", Consequence: "prints"}); !errors.Is(err, mcp.ErrNoConfirmer) {
		t.Fatalf("want ErrNoConfirmer, got %v", err)
	}
}

func TestPromptSaysSomethingEvenWithNothingToSay(t *testing.T) {
	c := mcp.Confirmation{Tool: "mystery_tool"}
	if !strings.Contains(c.Prompt(), "has effects outside this machine") {
		t.Fatalf("a consequence with no words still has to be spoken honestly: %q", c.Prompt())
	}
}

// confirmerFor is a tiny constructor so the tests above read without repeating
// the struct literal.
func confirmerFor(b *bus.Bus) *mcp.BusConfirmer {
	return &mcp.BusConfirmer{Bus: b, Wait: 5 * time.Second}
}
