package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/luthor007/relay/relayd/internal/event"
)

// The two outcome shapes. There are exactly two, and `cancelled` is mandatory
// rather than optional: a client that sends session/cancel while a permission
// request is outstanding must resolve it this way or the agent's turn cannot
// unwind.
const (
	outcomeSelected  = "selected"
	outcomeCancelled = "cancelled"
)

// pendingPermission is one session/request_permission we have not answered.
//
// The agent blocks on our answer for as long as we take — there is no deadline
// in the protocol — which is what makes ADAPTERS.md §7's voice-answerable
// approval real on these three runtimes rather than aspirational.
type pendingPermission struct {
	s      *Session
	id     json.RawMessage
	turnID string
	opts   []PermissionOption
	ni     *event.NeedsInput

	mu   sync.Mutex
	done bool
}

func (a *Adapter) handlePermission(id json.RawMessage, params json.RawMessage) {
	var p requestPermissionParams
	if err := json.Unmarshal(params, &p); err != nil {
		a.log.Warn("acp: session/request_permission did not decode", "err", err)
		a.refuse(id, methodRequestPermission, "params did not decode")
		return
	}
	a.route(p.SessionID, inbound{
		method: methodRequestPermission,
		id:     append(json.RawMessage(nil), id...),
		perm:   &p,
	})
}

func (s *Session) raisePermission(id json.RawMessage, p requestPermissionParams) {
	s.mu.Lock()
	turnID := ""
	if s.active != nil {
		turnID = s.active.id
	}
	s.mu.Unlock()

	pp := &pendingPermission{
		s:      s,
		id:     append(json.RawMessage(nil), id...),
		turnID: turnID,
		opts:   p.Options,
	}

	spec := event.InputSpec{
		Ask:     event.InputPermission,
		Prompt:  permissionPrompt(s, p.ToolCall),
		Options: mapOptions(p.Options),
		Tool:    toolRef(p.ToolCall),
		// Zero: ACP puts no deadline on a permission request. The agent waits
		// as long as we take, which is the whole budget for "ping, walk away,
		// answer an hour later".
	}
	pp.ni = event.NewNeedsInput(s.meta(turnID), spec, pp.reply)

	s.mu.Lock()
	s.perms[string(pp.id)] = pp
	s.mu.Unlock()

	s.q.push(pp.ni)
}

// permissionPrompt says what we know and nothing more.
//
// `toolCall` is a ToolCallUpdate, so only toolCallId is guaranteed; title, kind
// and rawInput may all be missing. When the title is absent the correct
// behaviour is to say so rather than to infer a description out of rawInput,
// which is how a voice assistant ends up reading a command it invented.
func permissionPrompt(s *Session, tc toolCallUpdate) string {
	if tc.Title != nil && *tc.Title != "" {
		return *tc.Title
	}
	kind := ""
	if tc.Kind != nil {
		kind = *tc.Kind
	}
	if kind != "" {
		return fmt.Sprintf("%s is asking to run a %s tool call (%s) and did not say what it does",
			s.runtime, kind, tc.ToolCallID)
	}
	return fmt.Sprintf("%s is asking permission for tool call %s and did not say what it does",
		s.runtime, tc.ToolCallID)
}

func toolRef(tc toolCallUpdate) *event.ToolRef {
	ref := &event.ToolRef{ID: tc.ToolCallID, RawInput: rawObject(tc.RawInput)}
	if tc.Title != nil {
		ref.Title = *tc.Title
	}
	if tc.Kind != nil {
		ref.Kind = *tc.Kind
	}
	return ref
}

// mapOptions carries the agent's own option names through verbatim. The array
// is agent-supplied and open-ended and PermissionOptionKind is a UI hint, not a
// fixed menu, so an unrecognised kind becomes OptionOther rather than being
// dropped — dropping it would remove an answer the agent is willing to accept.
func mapOptions(in []PermissionOption) []event.Option {
	out := make([]event.Option, 0, len(in))
	for _, o := range in {
		out = append(out, event.Option{ID: o.OptionID, Name: o.Name, Kind: optionKind(o.Kind)})
	}
	return out
}

func optionKind(k string) event.OptionKind {
	switch event.OptionKind(k) {
	case event.OptionAllowOnce, event.OptionAllowAlways, event.OptionRejectOnce, event.OptionRejectAlways:
		return event.OptionKind(k)
	}
	return event.OptionOther
}

// reply is the path back into the runtime. NeedsInput has already checked that
// a named OptionID is one the agent offered.
func (p *pendingPermission) reply(ctx context.Context, r event.Reply) error {
	outcome, err := p.choose(r)
	if err != nil {
		return err
	}
	if err := p.respond(outcome); err != nil {
		return err
	}

	if r.Interrupt {
		// ACP has no hard stop inside the permission response — rejecting a
		// tool is not cancelling a turn. "No, stop" spoken at the glasses is
		// therefore reject *plus* session/cancel, and this is the only place
		// that pairing happens.
		if err := p.s.Cancel(ctx, ""); err != nil {
			p.s.log.Warn("acp: answered the permission request but could not cancel the turn", "err", err)
			return err
		}
	}
	return nil
}

// choose maps a runtime-agnostic Reply onto one of the two outcome shapes.
//
// Two rules it will not break. It never sends an option the agent did not
// offer, and it never selects a *standing* option on the user's behalf —
// ORCHESTRATOR.md §4b requires consequential choices to be made by a human
// every time, and "always allow" is exactly that.
func (p *pendingPermission) choose(r event.Reply) (permissionOutcome, error) {
	if r.OptionID != "" {
		return permissionOutcome{Outcome: outcomeSelected, OptionID: r.OptionID}, nil
	}
	switch r.Decision {
	case event.DecisionCancelled:
		return permissionOutcome{Outcome: outcomeCancelled}, nil
	case event.DecisionAllow:
		if o, ok := p.pick(event.OptionAllowOnce); ok {
			return permissionOutcome{Outcome: outcomeSelected, OptionID: o}, nil
		}
		return permissionOutcome{}, fmt.Errorf("%w: the agent offered no allow-once option, only %v — a standing grant has to be chosen by a human",
			event.ErrUnknownOption, p.optionNames())
	case event.DecisionDeny:
		if o, ok := p.pick(event.OptionRejectOnce); ok {
			return permissionOutcome{Outcome: outcomeSelected, OptionID: o}, nil
		}
		if r.Interrupt {
			// Nothing to reject with, but the user said stop. The cancelled
			// outcome plus session/cancel is the protocol's own way to unwind,
			// and it invents no option.
			return permissionOutcome{Outcome: outcomeCancelled}, nil
		}
		return permissionOutcome{}, fmt.Errorf("%w: the agent offered no reject-once option, only %v",
			event.ErrUnknownOption, p.optionNames())
	}
	return permissionOutcome{}, fmt.Errorf("acp: a permission reply needs an option id or a decision, got neither")
}

func (p *pendingPermission) pick(kind event.OptionKind) (string, bool) {
	for _, o := range p.opts {
		if optionKind(o.Kind) == kind {
			return o.OptionID, true
		}
	}
	return "", false
}

func (p *pendingPermission) optionNames() []string {
	out := make([]string, 0, len(p.opts))
	for _, o := range p.opts {
		out = append(out, fmt.Sprintf("%s(%s)", o.Name, o.Kind))
	}
	return out
}

// respond writes the answer exactly once. A failed write leaves the question
// open so the caller can try again rather than losing the agent's turn.
//
// The question leaves the outstanding set *before* the write, not after. The
// window between the two is long enough for the whole rest of the turn to
// arrive — the agent unblocks the moment our line lands — and a question still
// in the set when the turn resolves gets withdrawn out from under the answer
// that already went out.
func (p *pendingPermission) respond(o permissionOutcome) error {
	p.mu.Lock()
	if p.done {
		p.mu.Unlock()
		return event.ErrAnswered
	}
	p.done = true
	p.mu.Unlock()
	p.s.forgetPermission(p)

	if err := p.s.a.c.respond(p.id, requestPermissionResult{Outcome: o}); err != nil {
		p.mu.Lock()
		p.done = false
		p.mu.Unlock()
		p.s.rememberPermission(p)
		return fmt.Errorf("acp: answering session/request_permission: %w", err)
	}
	return nil
}

func (s *Session) forgetPermission(p *pendingPermission) {
	s.mu.Lock()
	delete(s.perms, string(p.id))
	s.mu.Unlock()
}

func (s *Session) rememberPermission(p *pendingPermission) {
	s.mu.Lock()
	if !s.closed {
		s.perms[string(p.id)] = p
	}
	s.mu.Unlock()
}

// cancelPermissions resolves every outstanding question with the cancelled
// outcome. Mandatory on session/cancel: without it the agent's turn cannot
// unwind, and the prompt never resolves.
func (s *Session) cancelPermissions() {
	s.resolveOutstanding("", "the turn was cancelled")
}

// withdrawPermissions is the same thing for a turn that ended some other way.
// Answering is still the right move — an unanswered request stalls the agent —
// and the NeedsInput is withdrawn so a ping that outlived its question can be
// retracted instead of waking someone to approve what nobody is waiting for.
func (s *Session) withdrawPermissions(turnID, reason string) {
	s.resolveOutstanding(turnID, reason)
}

func (s *Session) resolveOutstanding(turnID, reason string) {
	s.mu.Lock()
	var hit []*pendingPermission
	for k, p := range s.perms {
		if turnID != "" && p.turnID != turnID {
			continue
		}
		hit = append(hit, p)
		delete(s.perms, k)
	}
	s.mu.Unlock()

	for _, p := range hit {
		err := p.respond(permissionOutcome{Outcome: outcomeCancelled})
		if errors.Is(err, event.ErrAnswered) {
			// Somebody answered it while we were reaching for it. Withdrawing
			// now would report a question as unanswered when its answer is
			// already on the wire.
			continue
		}
		if err != nil {
			s.log.Warn("acp: could not send the cancelled outcome for an outstanding permission request", "err", err)
		}
		p.ni.Withdraw(reason)
	}
}

// OutstandingPermissions is how many questions this session is blocked on.
func (s *Session) OutstandingPermissions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.perms)
}
