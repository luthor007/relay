package api

import (
	"context"
	"fmt"

	"github.com/luthor007/relay/relayd/internal/audit"
)

// ConsoleEvent is a change to something the console holds a list of.
//
// DASHBOARD.md §5 wants optimistic UI on the credential flows: the row appears
// the instant you press the button and reconciles against what actually landed.
// That needs a channel that says what landed, and §7.1 already settled the
// channel — SSE, one direction, no subscription to get wrong.
//
// The payload is deliberately thin. A credential event carries an id and never
// a credential; the console re-reads the list, which is the same listing every
// other reader gets and therefore the same one that cannot contain a secret.
type ConsoleEvent struct {
	// Kind is the SSE event name: credential | connector | fact | audit | probe.
	Kind string `json:"kind"`
	// Action is what happened, in the audit log's vocabulary where there is one.
	Action string `json:"action,omitempty"`
	ID     string `json:"id,omitempty"`
	// Outcome distinguishes "this landed" from "this was refused", so an
	// optimistic row can be rolled back rather than left looking applied.
	Outcome string `json:"outcome,omitempty"`
	Reason  string `json:"reason,omitempty"`
	At      int64  `json:"at"`
}

// Console event kinds.
const (
	ConsoleCredential = "credential"
	ConsoleConnector  = "connector"
	ConsoleFact       = "fact"
	ConsoleAudit      = "audit"
	ConsoleProbe      = "probe"
)

// publish fans a console event out to every open SSE stream.
func (s *Server) publish(e ConsoleEvent) {
	if e.At == 0 {
		e.At = s.now().UnixMilli()
	}
	s.console.Publish(e)
}

// audited runs a credential or connector mutation with the audit log wrapped
// around it, and refuses the mutation when the log cannot record the attempt.
//
// This is the one place the ordering lives, because getting it wrong is the
// whole failure this log exists to prevent: the attempt is written first, the
// work runs second, the outcome is written third. A caller that crashes in the
// middle leaves an attempt with no outcome, which is the evidence you want.
func (s *Server) audited(ctx context.Context, e auditRequest, work func() (map[string]string, error)) error {
	a, err := beginAudit(ctx, s, e)
	if err != nil {
		// The work has not run and will not. Wrapping in ErrNoLog is what turns
		// this into a 503 that says why, rather than a 500 that looks like a bug
		// in the vault.
		return fmt.Errorf("%w: %v", audit.ErrNoLog, err)
	}
	detail, err := work()
	if err != nil {
		_ = a.Fail(ctx, err)
		s.publish(ConsoleEvent{
			Kind: e.kind, Action: string(e.action), ID: e.target,
			Outcome: "failed", Reason: err.Error(),
		})
		return err
	}
	if err := a.OK(ctx, detail); err != nil {
		// The mutation happened and the record of it did not. That is worth
		// shouting about: it is the exact gap somebody would engineer.
		s.log.Error("api: a vault mutation succeeded and its audit entry did not",
			"action", e.action, "target", e.target, "error", err)
	}
	s.publish(ConsoleEvent{
		Kind: e.kind, Action: string(e.action), ID: e.target, Outcome: "ok",
	})
	return nil
}
