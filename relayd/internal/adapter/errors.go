package adapter

import (
	"errors"
	"fmt"
)

var (
	// ErrUnsupported is what every *UnsupportedError matches with errors.Is.
	// Callers degrade against it; nothing panics on a missing capability.
	ErrUnsupported = errors.New("adapter: capability not supported by this runtime")

	// ErrSessionClosed is returned by any method on a session that has ended.
	ErrSessionClosed = errors.New("adapter: session is closed")

	// ErrTurnNotActive is Codex's turn/steer precondition failing: the turn
	// named by expectedTurnId is no longer the active one. The caller either
	// starts a new turn or, on a runtime without steering, cancels and
	// re-prompts.
	ErrTurnNotActive = errors.New("adapter: that turn is no longer active")

	// ErrAuthRequired is ACP's JSON-RPC -32000 "Authentication required" on
	// session/new. Recovery is the authenticate method with one of the ids from
	// the handshake, and it belongs in the installer rather than mid-conversation
	// — a device-code flow cannot be completed by voice.
	ErrAuthRequired = errors.New("adapter: runtime is not authenticated")

	// ErrSessionNotFound is a resume against a session the runtime no longer
	// has. OpenClaw's --require-existing turns a missing session into this
	// rather than silently creating a new one.
	ErrSessionNotFound = errors.New("adapter: no such session on this runtime")
)

// UnsupportedError says which runtime cannot do what, and why as far as we
// know. The Note comes from the capability descriptor, so a user-facing message
// can explain a gap instead of just reporting one.
type UnsupportedError struct {
	Runtime    Runtime
	Capability Capability
	Support    Support
	Note       string
}

func (e *UnsupportedError) Error() string {
	msg := fmt.Sprintf("adapter: %s does not support %s (%s)", e.Runtime, e.Capability, e.Support)
	if e.Note != "" {
		msg += ": " + e.Note
	}
	return msg
}

func (e *UnsupportedError) Is(target error) bool { return target == ErrUnsupported }

// Unsupported builds an *UnsupportedError without a descriptor to hand.
func Unsupported(r Runtime, cap Capability, note string) error {
	return &UnsupportedError{Runtime: r, Capability: cap, Support: SupportNo, Note: note}
}
