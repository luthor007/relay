package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/luthor007/relay/relayd/internal/bus"
	"github.com/luthor007/relay/relayd/internal/event"
)

// ORCHESTRATOR.md §4b rule 3, and ADAPTERS.md §7's last paragraph on it:
//
//	Actions with consequences outside the machine confirm at the glasses. Sending
//	mail, spending money, printing, opening a door. One spoken confirmation,
//	every time — not a setting that can be turned off, because the value of the
//	confirmation is that it cannot be. […] it must not be suppressible by
//	batching or quiet hours — an approval the user never heard is an approval
//	they did not give.
//
// The mechanism is the NeedsInput path that already exists, and using it rather
// than inventing a second one is the whole point: bus.Pinger already treats a
// blocking question as never-batched, never-quiet-holdable and re-pinged once
// at two minutes, and it already sets Ping.Consequential for an InputPermission
// ask. A separate confirmation channel would have to re-earn all of that and
// would get one of them wrong.

// DefaultConfirmWait bounds how long a consequential action waits for an
// answer. It is generous on purpose — ADAPTERS.md §2 records Claude Code's own
// MCP tool timeout at about 27.8 hours, which is what makes "ping, walk away,
// answer an hour later" real — but it is not infinite, because a print job that
// is still waiting for approval next week should fail rather than fire.
const DefaultConfirmWait = time.Hour

// Errors from the confirmation path.
var (
	// ErrNoConfirmer is a consequential tool on a gateway with nobody to ask.
	// It is a refusal, not a warning: an approval nobody could be asked for is
	// not an approval.
	ErrNoConfirmer = errors.New("mcp: this action needs a spoken confirmation and no confirmation path is wired")

	// ErrDenied is the user saying no.
	ErrDenied = errors.New("mcp: the confirmation was declined")

	// ErrUnanswered is nobody answering in time.
	ErrUnanswered = errors.New("mcp: nobody answered the confirmation")
)

// Confirmation is one consequential action, described in the words the user
// will hear.
type Confirmation struct {
	Connector string
	Tool      string
	// Consequence is Tool.Consequence — what happens outside the machine.
	Consequence string
	// Target is what it acts on, when the tool could name one.
	Target string
	// Session and Runtime are who is asking.
	Session string
	Runtime string
}

// Prompt is the sentence spoken at the glasses. It is built only from the tool's
// own declared consequence and target — nothing here is generated, because
// ORCHESTRATOR.md §3b's grounding rule applies hardest to the sentence somebody
// says yes to.
func (c Confirmation) Prompt() string {
	verb := strings.TrimSpace(c.Consequence)
	if verb == "" {
		verb = "has effects outside this machine"
	}
	s := strings.TrimSpace(c.Connector)
	if s == "" {
		s = c.Tool
	}
	line := s + " " + verb
	if t := strings.TrimSpace(c.Target); t != "" {
		line += ": " + t
	}
	return line + ". Go ahead?"
}

// Confirmer asks the user and waits.
type Confirmer interface {
	Confirm(ctx context.Context, c Confirmation) error
}

// ConfirmerFunc adapts a function to [Confirmer].
type ConfirmerFunc func(ctx context.Context, c Confirmation) error

func (f ConfirmerFunc) Confirm(ctx context.Context, c Confirmation) error { return f(ctx, c) }

// Option ids offered on a confirmation. There is deliberately no allow_always:
// OptionKind.Standing() is what the orchestrator checks before selecting an
// option on the user's behalf, and a standing grant is exactly what rule 3
// forbids. The Claude Code permission server refuses to return
// updatedPermissions for the same reason.
const (
	OptionConfirmAllow = "allow_once"
	OptionConfirmDeny  = "reject_once"
)

// ConfirmOptions are the two answers, and only these two.
func ConfirmOptions() []event.Option {
	return []event.Option{
		{ID: OptionConfirmAllow, Name: "Go ahead", Kind: event.OptionAllowOnce},
		{ID: OptionConfirmDeny, Name: "No", Kind: event.OptionRejectOnce},
	}
}

// BusConfirmer delivers a confirmation as an event.NeedsInput on the shared
// bus, which is what puts it on the glasses through bus.Pinger's blocking path.
type BusConfirmer struct {
	// Bus is where the question is published. Required.
	Bus *bus.Bus

	// Wait bounds the answer. Zero means DefaultConfirmWait.
	Wait time.Duration

	Now   func() time.Time
	NewID func() string
}

// NewBusConfirmer builds a confirmer on a bus.
func NewBusConfirmer(b *bus.Bus) *BusConfirmer { return &BusConfirmer{Bus: b} }

func (c *BusConfirmer) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *BusConfirmer) newID() string {
	if c.NewID != nil {
		return c.NewID()
	}
	return uuid.NewString()
}

// Confirm publishes the question and blocks until it is answered, declined,
// withdrawn or the wait expires. Anything other than an explicit yes is a no.
func (c *BusConfirmer) Confirm(ctx context.Context, conf Confirmation) error {
	if c == nil || c.Bus == nil {
		return ErrNoConfirmer
	}
	wait := c.Wait
	if wait <= 0 {
		wait = DefaultConfirmWait
	}
	at := c.now()

	// The reply function is the seam back into whatever is being approved. Here
	// the gateway *is* the runtime being unblocked, so there is nothing to send
	// on a wire — but the constructor requires one, and that requirement is the
	// reason a question can never be raised with no way to answer it.
	ask := event.NewNeedsInput(
		event.Meta{
			Runtime: conf.Runtime,
			Session: conf.Session,
			At:      at,
		},
		event.InputSpec{
			Ask:     event.InputPermission,
			Prompt:  conf.Prompt(),
			Options: ConfirmOptions(),
			Tool: &event.ToolRef{
				Name:  conf.Tool,
				Title: conf.Connector,
				Kind:  string(AccessWrite),
			},
			Deadline: at.Add(wait),
		},
		func(context.Context, event.Reply) error { return nil },
	)

	c.Bus.Publish(ask)

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-ask.Done():
	case <-ctx.Done():
		ask.Withdraw("the tool call was abandoned before anyone answered")
		return fmt.Errorf("%w: %w", ErrUnanswered, ctx.Err())
	case <-timer.C:
		ask.Withdraw("nobody answered in time")
		return fmt.Errorf("%w within %s", ErrUnanswered, wait)
	}

	reply, ok := ask.Outcome()
	if !ok {
		// Withdrawn: answered somewhere else, or the session went away. Either
		// way nobody said yes here.
		return ErrUnanswered
	}
	switch {
	case reply.OptionID == OptionConfirmAllow:
		return nil
	case reply.OptionID == OptionConfirmDeny:
		return ErrDenied
	case reply.Decision == event.DecisionAllow:
		return nil
	default:
		return ErrDenied
	}
}
