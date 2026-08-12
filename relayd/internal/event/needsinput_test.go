package event_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/event"
)

func permission() event.InputSpec {
	return event.InputSpec{
		Ask:    event.InputPermission,
		Prompt: "Run npm test?",
		Tool: &event.ToolRef{
			ID: "call_1", Title: "Run npm test", Kind: "execute",
			RawInput: map[string]any{"command": "npm test"},
		},
		Options: []event.Option{
			{ID: "o1", Name: "Allow", Kind: event.OptionAllowOnce},
			{ID: "o2", Name: "Always allow", Kind: event.OptionAllowAlways},
			{ID: "o3", Name: "Reject", Kind: event.OptionRejectOnce},
		},
	}
}

// The reply mechanism has to resolve back through whichever adapter raised the
// question, without the question knowing which one that is: Codex resolves a
// pending JSON-RPC request, Claude Code returns from the permission-prompt MCP
// tool, ACP answers session/request_permission.
func TestReplyReachesTheAdapter(t *testing.T) {
	var got event.Reply
	n := event.NewNeedsInput(meta(), permission(), func(_ context.Context, r event.Reply) error {
		got = r
		return nil
	})

	if err := n.Reply(context.Background(), event.Reply{
		OptionID: "o1", Decision: event.DecisionAllow,
	}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if got.OptionID != "o1" || got.Decision != event.DecisionAllow {
		t.Fatalf("the adapter got %+v", got)
	}

	select {
	case <-n.Done():
	default:
		t.Fatal("Done did not close after a reply")
	}
	out, ok := n.Outcome()
	if !ok || out.OptionID != "o1" {
		t.Fatalf("Outcome: %+v %v", out, ok)
	}
	if !n.Answered() {
		t.Fatal("Answered is false after a reply")
	}
}

func TestReplyIsSingleShot(t *testing.T) {
	calls := 0
	n := event.NewNeedsInput(meta(), permission(), func(context.Context, event.Reply) error {
		calls++
		return nil
	})
	ctx := context.Background()

	if err := n.Reply(ctx, event.Reply{OptionID: "o1"}); err != nil {
		t.Fatal(err)
	}
	if err := n.Reply(ctx, event.Reply{OptionID: "o3"}); !errors.Is(err, event.ErrAnswered) {
		t.Fatalf("second reply returned %v, want ErrAnswered", err)
	}
	if calls != 1 {
		t.Fatalf("the adapter was called %d times", calls)
	}
}

// ADAPTERS.md §4: options are agent-supplied and open-ended, and we never send
// back one that was not in the array.
func TestReplyRejectsAnOptionThatWasNotOffered(t *testing.T) {
	n := event.NewNeedsInput(meta(), permission(), noReply)
	err := n.Reply(context.Background(), event.Reply{OptionID: "o9"})
	if !errors.Is(err, event.ErrUnknownOption) {
		t.Fatalf("got %v, want ErrUnknownOption", err)
	}
	if n.Answered() {
		t.Fatal("a rejected reply must leave the question open")
	}
}

// Codex's serverRequest/resolved: an approval was answered somewhere else, in a
// terminal. Without honouring it, a Relay ping outlives its question and wakes
// the user to approve something already approved.
func TestWithdrawRetractsTheQuestion(t *testing.T) {
	n := event.NewNeedsInput(meta(), permission(), func(context.Context, event.Reply) error {
		t.Fatal("the adapter must not be called after a withdrawal")
		return nil
	})

	n.Withdraw("approved in a terminal")

	select {
	case <-n.Done():
	default:
		t.Fatal("Done did not close on withdrawal")
	}
	err := n.Reply(context.Background(), event.Reply{OptionID: "o1"})
	if !errors.Is(err, event.ErrWithdrawn) {
		t.Fatalf("got %v, want ErrWithdrawn", err)
	}
	if _, ok := n.Outcome(); ok {
		t.Fatal("a withdrawn question has no outcome")
	}
	// Withdrawing twice is a no-op, not a panic on a closed channel.
	n.Withdraw("again")
}

// If the runtime refuses the answer, the question stays open so the caller can
// try a different option rather than being stuck with a dead session.
func TestFailedReplyLeavesTheQuestionOpen(t *testing.T) {
	boom := errors.New("rpc failed")
	attempts := 0
	n := event.NewNeedsInput(meta(), permission(), func(context.Context, event.Reply) error {
		attempts++
		if attempts == 1 {
			return boom
		}
		return nil
	})
	ctx := context.Background()

	if err := n.Reply(ctx, event.Reply{OptionID: "o1"}); !errors.Is(err, boom) {
		t.Fatalf("got %v, want the transport error", err)
	}
	if n.Answered() {
		t.Fatal("a failed reply must not close the question")
	}
	if err := n.Reply(ctx, event.Reply{OptionID: "o3"}); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestConcurrentRepliesResolveOnce(t *testing.T) {
	n := event.NewNeedsInput(meta(), permission(), func(context.Context, event.Reply) error { return nil })

	const n_ = 16
	var wg sync.WaitGroup
	errs := make(chan error, n_)
	for i := 0; i < n_; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- n.Reply(context.Background(), event.Reply{OptionID: "o1"})
		}()
	}
	wg.Wait()
	close(errs)

	ok := 0
	for err := range errs {
		if err == nil {
			ok++
		} else if !errors.Is(err, event.ErrAnswered) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d replies succeeded, want exactly 1", ok)
	}
}

// ORCHESTRATOR.md §4b requires consequential actions to be confirmed every
// time, so a standing grant is something a human picks and never something the
// orchestrator picks for them. The option is still carried — we speak the names
// we were given — but it is identifiable.
func TestStandingOptionsAreIdentifiable(t *testing.T) {
	n := event.NewNeedsInput(meta(), permission(), noReply)

	var standing, once int
	for _, o := range n.Options {
		if o.Kind.Standing() {
			standing++
		} else {
			once++
		}
	}
	if standing != 1 || once != 2 {
		t.Fatalf("standing=%d once=%d, want 1 and 2", standing, once)
	}
	if !event.OptionRejectAlways.Standing() {
		t.Fatal("reject_always is a standing decision too")
	}
	if event.OptionOther.Standing() {
		t.Fatal("an unclassified option must not be assumed standing")
	}
}

// ACP's toolCall is a ToolCallUpdate where only toolCallId is required: there
// may be no human-readable description to read aloud, and the correct
// behaviour is to say what we know rather than infer one from rawInput.
func TestToolRefMayBeEmpty(t *testing.T) {
	n := event.NewNeedsInput(meta(), event.InputSpec{
		Ask:     event.InputPermission,
		Options: []event.Option{{ID: "o1", Name: "Allow", Kind: event.OptionAllowOnce}},
		Tool:    &event.ToolRef{ID: "call_1"},
	}, noReply)

	if n.Tool.Title != "" || n.Tool.Kind != "" || n.Tool.RawInput != nil {
		t.Fatal("a bare ToolCallUpdate must not be filled in with invented fields")
	}
	if _, ok := n.Option("o1"); !ok {
		t.Fatal("Option lookup failed")
	}
	if _, ok := n.Option("nope"); ok {
		t.Fatal("Option found something that was never offered")
	}
}

func TestDeadlineIsCarried(t *testing.T) {
	// Claude Code's default MCP tool-call timeout is 1e8 ms — about 27.8 hours
	// — which is what makes "ping, walk away, answer an hour later" real.
	deadline := time.Unix(1770000000, 0).Add(27*time.Hour + 48*time.Minute)
	spec := permission()
	spec.Deadline = deadline
	n := event.NewNeedsInput(meta(), spec, noReply)
	if !n.Deadline.Equal(deadline) {
		t.Fatalf("deadline is %v", n.Deadline)
	}
}

func TestNewNeedsInputRequiresAReplyPath(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a question nobody can answer is a hung session; it must not be constructible")
		}
	}()
	event.NewNeedsInput(meta(), permission(), nil)
}
