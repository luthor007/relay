package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/luthor007/relay/relayd/internal/event"
)

// The ten server→client requests. Every one of them blocks Codex until the
// client replies, which is what makes voice answers real rather than
// aspirational — and also what makes an unhandled one hang the runtime. There
// are ten, not the five ADAPTERS.md §3 originally listed.
const (
	MethodCommandApproval     = "item/commandExecution/requestApproval"
	MethodFileChangeApproval  = "item/fileChange/requestApproval"
	MethodPermissionsApproval = "item/permissions/requestApproval"
	MethodToolUserInput       = "item/tool/requestUserInput"
	MethodElicitation         = "mcpServer/elicitation/request"

	MethodDynamicToolCall   = "item/tool/call"
	MethodAuthRefresh       = "account/chatgptAuthTokens/refresh"
	MethodAttestation       = "attestation/generate"
	MethodApplyPatchLegacy  = "applyPatchApproval"
	MethodExecCommandLegacy = "execCommandApproval"
)

// UnverifiedReplyMethods are the three approval requests whose *reply* shape is
// outside the vendored contract (ADAPTERS.md §8 item 7). `generate-json-schema`
// emits request and notification params only; there is no ServerResponse.json,
// and unlike command execution and permissions these three have no orphaned
// definition to infer from either.
//
// Until someone probes them on a real Codex, Relay answers them with a JSON-RPC
// error — Codex surfaces a failure instead of hanging, and nobody is asked to
// approve something on the strength of a guessed payload. Register a
// [ReplyEncoder] in [Options.UnverifiedReplies] to turn one on.
func UnverifiedReplyMethods() []string {
	return []string{MethodFileChangeApproval, MethodToolUserInput, MethodElicitation}
}

// ReplyEncoder builds the JSON-RPC result for one of those three. It exists so
// that closing ADAPTERS.md §8 item 7 is a five-line registration rather than a
// change to this package.
type ReplyEncoder func(params json.RawMessage, r event.Reply) (any, error)

// pendingRequest is one unanswered server→client request.
type pendingRequest struct {
	id     json.RawMessage
	method string
	turnID string
	// itemID and approvalID are the routing key the contract asks for:
	// "For zsh-exec-bridge subcommand approvals, multiple callbacks can belong
	// to one parent itemId", so itemId alone is not unique.
	itemID     string
	approvalID string
	question   *event.NeedsInput
}

// requestKey normalises a RequestId (`string | int64`) for use as a map key, so
// a `serverRequest/resolved` naming the same id matches however it was spelled.
func requestKey(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return "s:" + str
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return "n:" + strconv.FormatInt(n, 10)
	}
	return "?:" + s
}

// handleServerRequest is called on the connection's reader goroutine and must
// not block. A question that reaches a human is raised here and answered later.
func (s *Session) handleServerRequest(id json.RawMessage, method string, params json.RawMessage) {
	switch method {
	case MethodCommandApproval:
		s.raiseCommandApproval(id, method, params)
	case MethodPermissionsApproval:
		s.raisePermissionsApproval(id, method, params)
	case MethodFileChangeApproval:
		s.raiseGated(id, method, params, s.fileChangeSpec)
	case MethodToolUserInput:
		s.raiseGated(id, method, params, s.toolUserInputSpec)
	case MethodElicitation:
		s.raiseGated(id, method, params, s.elicitationSpec)
	default:
		s.a.refuse(id, method)
	}
}

// ---- the two shapes with evidence ----

type commandApprovalParams struct {
	ThreadID    string   `json:"threadId"`
	TurnID      string   `json:"turnId"`
	ItemID      string   `json:"itemId"`
	ApprovalID  *string  `json:"approvalId"`
	Command     *string  `json:"command"`
	Cwd         *string  `json:"cwd"`
	Reason      *string  `json:"reason"`
	StartedAtMs int64    `json:"startedAtMs"`
	Proposed    []string `json:"proposedExecpolicyAmendment"`
}

// commandDecisionOptions are `CommandExecutionApprovalDecision`, minus the
// three that persist a standing grant.
//
// ORCHESTRATOR.md §4b requires consequential actions to be confirmed every
// time, so `acceptForSession`, `acceptWithExecpolicyAmendment` and
// `applyNetworkPolicyAmendment` are never offered and never sent. `decline` and
// `cancel` are carried separately because the schema's own words distinguish
// them: decline means "the agent will continue the turn", cancel means "the
// turn will also be immediately interrupted" — the hard stop behind "no, stop"
// spoken at the glasses.
func commandDecisionOptions() []event.Option {
	return []event.Option{
		{ID: "accept", Name: "approve this command", Kind: event.OptionAllowOnce},
		{ID: "decline", Name: "deny it, and let the agent carry on", Kind: event.OptionRejectOnce},
		{ID: "cancel", Name: "deny it, and stop the turn", Kind: event.OptionRejectOnce},
	}
}

func (s *Session) raiseCommandApproval(id json.RawMessage, method string, raw json.RawMessage) {
	var p commandApprovalParams
	if err := json.Unmarshal(raw, &p); err != nil {
		s.log.Error("codex: undecodable approval params", "method", method, "err", err)
		_ = s.conn().respondError(id, codeInternalError, "relay could not decode the approval params")
		return
	}

	prompt := "Codex wants to run a command"
	if p.Command != nil && *p.Command != "" {
		prompt = "Codex wants to run: " + *p.Command
	}
	if p.Reason != nil && *p.Reason != "" {
		prompt += " — " + *p.Reason
	}

	tool := &event.ToolRef{ID: p.ItemID, Name: "command", Kind: "command_execution"}
	if p.Command != nil {
		tool.Title = *p.Command
		tool.RawInput = map[string]any{"command": *p.Command}
		if p.Cwd != nil {
			tool.RawInput["cwd"] = *p.Cwd
		}
	}

	s.raise(id, method, raw, p.TurnID, p.ItemID, deref(p.ApprovalID), event.InputSpec{
		Ask:     event.InputPermission,
		Prompt:  prompt,
		Options: commandDecisionOptions(),
		Tool:    tool,
	}, func(_ json.RawMessage, r event.Reply) (any, error) {
		return commandDecision(r), nil
	})
}

// commandDecision maps a normalized reply onto the schema's union. The result
// is the bare union value: a Rust `Result = CommandExecutionApprovalDecision`
// serialises as `"accept"`, not as an object wrapping it. That is inference
// from the orphaned definition rather than an observation — the caveat is in
// ADAPTERS.md §8 item 7 and it is the first thing to check on the Mac.
func commandDecision(r event.Reply) string {
	switch r.OptionID {
	case "accept", "decline", "cancel":
		return r.OptionID
	}
	switch {
	case r.Decision == event.DecisionAllow && !r.Interrupt:
		return "accept"
	case r.Decision == event.DecisionCancelled, r.Interrupt:
		return "cancel"
	default:
		return "decline"
	}
}

type permissionsApprovalParams struct {
	ThreadID    string          `json:"threadId"`
	TurnID      string          `json:"turnId"`
	ItemID      string          `json:"itemId"`
	Cwd         string          `json:"cwd"`
	Reason      *string         `json:"reason"`
	Permissions json.RawMessage `json:"permissions"`
	StartedAtMs int64           `json:"startedAtMs"`
}

func (s *Session) raisePermissionsApproval(id json.RawMessage, method string, raw json.RawMessage) {
	var p permissionsApprovalParams
	if err := json.Unmarshal(raw, &p); err != nil {
		s.log.Error("codex: undecodable approval params", "method", method, "err", err)
		_ = s.conn().respondError(id, codeInternalError, "relay could not decode the approval params")
		return
	}

	prompt := "Codex is asking for additional permissions"
	if p.Reason != nil && *p.Reason != "" {
		prompt += " — " + *p.Reason
	}
	tool := &event.ToolRef{ID: p.ItemID, Name: "permissions", Kind: "permissions"}
	if len(p.Permissions) > 0 {
		var v map[string]any
		if err := json.Unmarshal(p.Permissions, &v); err == nil {
			tool.RawInput = v
		}
	}

	granted := p.Permissions
	s.raise(id, method, raw, p.TurnID, p.ItemID, "", event.InputSpec{
		Ask:    event.InputPermission,
		Prompt: prompt,
		Options: []event.Option{
			{ID: "grant", Name: "grant exactly what was asked for", Kind: event.OptionAllowOnce},
			{ID: "refuse", Name: "grant nothing", Kind: event.OptionRejectOnce},
		},
		Tool: tool,
	}, func(_ json.RawMessage, r event.Reply) (any, error) {
		// The reply type is the orphaned `AdditionalPermissionProfile`, which
		// has the same two fields as the `RequestPermissionProfile` that came
		// in. Granting is echoing back what was asked for; refusing is an empty
		// profile, which grants nothing. Same evidence level, same caveat.
		if r.OptionID == "grant" || (r.OptionID == "" && r.Decision == event.DecisionAllow && !r.Interrupt) {
			if len(granted) == 0 {
				return map[string]any{}, nil
			}
			return granted, nil
		}
		return map[string]any{}, nil
	})
}

// ---- the three shapes without evidence ----

// specFn builds the question and the reply encoder for a gated request, or
// reports that this one cannot be raised.
type specFn func(raw json.RawMessage) (turnID, itemID string, spec event.InputSpec, err error)

func (s *Session) raiseGated(id json.RawMessage, method string, raw json.RawMessage, build specFn) {
	enc := s.encoderFor(method)
	if enc == nil {
		// Visible degradation, not a guess and not a hang.
		s.log.Warn("codex: refusing an approval whose reply shape is unverified",
			"method", method,
			"fix", "probe the reply on a real Codex, then register Options.UnverifiedReplies["+method+"]")
		s.noteUnverified(method)
		_ = s.conn().respondError(id, codeUnverified, fmt.Sprintf(
			"%s: relay has no verified reply shape for this request (ADAPTERS.md §8 item 7)", method))
		return
	}
	turnID, itemID, spec, err := build(raw)
	if err != nil {
		s.log.Error("codex: undecodable server request params", "method", method, "err", err)
		_ = s.conn().respondError(id, codeInternalError, "relay could not decode the request params")
		return
	}
	s.raise(id, method, raw, turnID, itemID, "", spec, enc)
}

func (s *Session) fileChangeSpec(raw json.RawMessage) (string, string, event.InputSpec, error) {
	var p struct {
		TurnID    string  `json:"turnId"`
		ItemID    string  `json:"itemId"`
		Reason    *string `json:"reason"`
		GrantRoot *string `json:"grantRoot"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", event.InputSpec{}, err
	}
	prompt := "Codex wants to change files"
	if p.Reason != nil && *p.Reason != "" {
		prompt += " — " + *p.Reason
	}
	if p.GrantRoot != nil && *p.GrantRoot != "" {
		// grantRoot is a standing grant for the rest of the session. Say so out
		// loud rather than folding it into a yes.
		prompt += " (and to keep write access under " + *p.GrantRoot + " for the rest of the session)"
	}
	// The params deliberately do not carry the diff; it is on the fileChange
	// item and on item/fileChange/patchUpdated, both of which the console
	// already has.
	return p.TurnID, p.ItemID, event.InputSpec{
		Ask:     event.InputPermission,
		Prompt:  prompt,
		Options: commandDecisionOptions(),
		Tool:    &event.ToolRef{ID: p.ItemID, Name: "file_change", Kind: "file_change"},
	}, nil
}

func (s *Session) toolUserInputSpec(raw json.RawMessage) (string, string, event.InputSpec, error) {
	var p struct {
		TurnID           string `json:"turnId"`
		ItemID           string `json:"itemId"`
		AutoResolutionMs *int64 `json:"autoResolutionMs"`
		Questions        []struct {
			ID       string `json:"id"`
			Header   string `json:"header"`
			Question string `json:"question"`
			IsOther  bool   `json:"isOther"`
			IsSecret bool   `json:"isSecret"`
			Options  []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", event.InputSpec{}, err
	}

	spec := event.InputSpec{Ask: event.InputToolValue}
	var secret bool
	var prompts []string
	for _, q := range p.Questions {
		if q.IsSecret {
			secret = true
		}
		line := q.Question
		if q.Header != "" {
			line = q.Header + ": " + q.Question
		}
		prompts = append(prompts, line)
		for _, o := range q.Options {
			spec.Options = append(spec.Options, event.Option{
				ID:   q.ID + "/" + o.Label,
				Name: o.Label,
				Kind: event.OptionOther,
			})
		}
	}
	spec.Prompt = strings.Join(prompts, " ")
	spec.Tool = &event.ToolRef{
		ID:   p.ItemID,
		Name: "request_user_input",
		Kind: "tool_value",
		// isSecret marks an *answer* that must go through the credential vault
		// and never into the index (MEMORY.md §6). The flag rides on the tool
		// reference because the sealed event model has nowhere else for it.
		RawInput: map[string]any{"secret": secret},
	}
	if secret {
		spec.Tool.Kind = "tool_value_secret"
	}
	if p.AutoResolutionMs != nil && *p.AutoResolutionMs > 0 {
		spec.Deadline = s.now().Add(msDuration(*p.AutoResolutionMs))
	}
	return p.TurnID, p.ItemID, spec, nil
}

func (s *Session) elicitationSpec(raw json.RawMessage) (string, string, event.InputSpec, error) {
	var p struct {
		ServerName string  `json:"serverName"`
		TurnID     *string `json:"turnId"`
		Mode       string  `json:"mode"`
		Message    string  `json:"message"`
		URL        string  `json:"url"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", event.InputSpec{}, err
	}
	prompt := p.ServerName + " is asking: " + p.Message
	if p.Mode == "url" && p.URL != "" {
		prompt += " (" + p.URL + ")"
	}
	// turnId is nullable here and only here: MCP models elicitation as a
	// standalone server-to-client request, so this question may belong to no
	// turn at all. An empty Meta.Turn is the honest answer.
	return deref(p.TurnID), "", event.InputSpec{
		Ask:    event.InputElicitation,
		Prompt: prompt,
		Tool:   &event.ToolRef{Name: p.ServerName, Kind: "elicitation:" + p.Mode},
	}, nil
}

// ---- raising and answering ----

// raise turns a blocking server request into a NeedsInput with a reply path
// that resolves that exact JSON-RPC request.
func (s *Session) raise(id json.RawMessage, method string, raw json.RawMessage, turnID, itemID, approvalID string, spec event.InputSpec, enc ReplyEncoder) {
	key := requestKey(id)
	p := &pendingRequest{
		id:         id,
		method:     method,
		turnID:     turnID,
		itemID:     itemID,
		approvalID: approvalID,
	}

	reply := func(ctx context.Context, r event.Reply) error {
		result, err := enc(raw, r)
		if err != nil {
			return fmt.Errorf("codex: encoding the reply to %s: %w", method, err)
		}
		if err := s.conn().respond(id, result); err != nil {
			return err
		}
		s.forget(key)
		// "no, stop" is not folded into the decision on every runtime. On a
		// command approval `cancel` already interrupts the turn; anywhere else
		// an interrupt has to be sent separately or the hard stop is only a
		// note the model narrates around.
		if r.Interrupt && method != MethodCommandApproval && turnID != "" {
			if err := s.interrupt(ctx, turnID); err != nil {
				s.log.Warn("codex: could not interrupt after a hard stop", "turn", turnID, "err", err)
			}
		}
		return nil
	}

	q := event.NewNeedsInput(s.src.meta(turnID), spec, reply)
	p.question = q

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = s.conn().respondError(id, codeInternalError, "relay session is closed")
		return
	}
	s.pending[key] = p
	s.mu.Unlock()

	s.emit(q)
}

func (s *Session) encoderFor(method string) ReplyEncoder {
	if s.a == nil {
		return nil
	}
	return s.a.opts.UnverifiedReplies[method]
}

func (s *Session) forget(key string) {
	s.mu.Lock()
	delete(s.pending, key)
	s.mu.Unlock()
}

// withdraw resolves a question from the runtime's side. `serverRequest/resolved`
// says an approval was answered somewhere else — in a terminal — and without
// honouring it a Relay ping outlives its question and wakes someone to approve
// what is already approved.
func (s *Session) withdraw(id json.RawMessage, reason string) {
	key := requestKey(id)
	s.mu.Lock()
	p := s.pending[key]
	delete(s.pending, key)
	s.mu.Unlock()
	if p == nil {
		return
	}
	p.question.Withdraw(reason)
}

// cancelPending answers every outstanding question for a turn so the turn can
// unwind, then withdraws the pings. turnID == "" means every question.
func (s *Session) cancelPending(turnID, reason string) {
	s.mu.Lock()
	var doomed []*pendingRequest
	for key, p := range s.pending {
		if turnID != "" && p.turnID != turnID && p.turnID != "" {
			continue
		}
		doomed = append(doomed, p)
		delete(s.pending, key)
	}
	s.mu.Unlock()

	for _, p := range doomed {
		switch p.method {
		case MethodCommandApproval, MethodFileChangeApproval:
			// `cancel` is the schema's own "denied, and interrupt the turn".
			if err := s.conn().respond(p.id, "cancel"); err != nil {
				s.log.Warn("codex: could not cancel a pending approval", "method", p.method, "err", err)
			}
		case MethodPermissionsApproval:
			if err := s.conn().respond(p.id, map[string]any{}); err != nil {
				s.log.Warn("codex: could not cancel a pending approval", "method", p.method, "err", err)
			}
		default:
			// No verified way to say "cancelled" in these shapes; an error at
			// least unblocks the server, which is the invariant that matters.
			if err := s.conn().respondError(p.id, codeUnverified, "relay cancelled this request"); err != nil {
				s.log.Warn("codex: could not cancel a pending request", "method", p.method, "err", err)
			}
		}
		p.question.Withdraw(reason)
	}
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
