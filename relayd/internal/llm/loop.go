package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/luthor007/relay/relayd/internal/event"
)

// Loop is the orchestrator's own agentic loop: call the model, run what it
// asked for, feed the results back, repeat until it stops asking.
//
// It emits [event.Event] rather than a vocabulary of its own, which is the
// whole design. ADAPTERS.md §5 seals nine event kinds and every runtime adapter
// normalises onto them; a loop that emits the same nine is, as far as the bus
// is concerned, another adapter. The console renders it, the pinger decides
// whether it wakes anyone, the phone shows its approvals, and the small model
// narrates it — none of them needing a line of new code, and none of them able
// to tell whether the work happened in Claude Code or in here.
//
// The three things worth knowing before changing it:
//
//   - Steering is drained at the model boundary, never mid-tool. See
//     [Hooks.Boundary].
//   - A denied call still returns a result. Both wires reject a turn with an
//     unanswered tool_use, so "deny" means an error result, not a dropped one.
//   - Only tools marked [Tool.ParallelSafe] overlap. Everything else is
//     serialised in the order the model asked for it.
type Loop struct {
	// Provider is the model. ORCHESTRATOR.md §3b's big one — the small one
	// speaks and does not hold tools.
	Provider Provider

	// Tools is the bound tool set. A tool with no handler cannot exist here,
	// which is the point of the type.
	Tools Toolbox

	Hooks Hooks

	// MaxIterations bounds the run. Zero means [DefaultMaxIterations].
	MaxIterations int

	// CallTimeout bounds one model call, separately from the deadline on the
	// whole run. The two are different questions: a local model that takes
	// ninety seconds to answer is slow, not stuck, and collapsing them into one
	// number means either killing it or letting a genuinely hung run sit
	// forever. Zero leaves the provider's own timeout in charge.
	CallTimeout time.Duration

	// Meta stamps every event. Session and Runtime are the caller's; Seq is
	// assigned here, per-session monotonic, as ADAPTERS.md §5 requires.
	Meta event.Meta

	// Emit receives every event. Nil is allowed and means the run is silent,
	// which is only ever right in a test.
	Emit func(event.Event)

	Log *slog.Logger

	seq atomic.Uint64
}

// DefaultMaxIterations bounds a run that never stops asking for tools.
//
// Sixteen rather than a larger number because of what this loop is for: the
// orchestrator's tool set is small and deliberately not a coding agent's, so a
// run that has taken sixteen model turns to route an utterance has misunderstood
// the request rather than found a hard problem.
const DefaultMaxIterations = 16

// Binding is one tool and the function that runs it.
type Binding struct {
	Tool Tool
	Run  ToolFunc
}

// ToolFunc executes one call. Returning an error and returning a
// [ToolResult] with IsError set mean the same thing to the model; the error
// return exists so a handler does not have to build a result to say "no".
type ToolFunc func(ctx context.Context, call ToolCall) (ToolResult, error)

// Toolbox is the bound tool set. Declaring a tool and forgetting to implement
// it is the failure this type exists to make unrepresentable.
type Toolbox []Binding

// Decls returns the wire-facing declarations.
func (b Toolbox) Decls() []Tool {
	out := make([]Tool, 0, len(b))
	for _, x := range b {
		out = append(out, x.Tool)
	}
	return out
}

func (b Toolbox) find(name string) (Binding, bool) {
	for _, x := range b {
		if x.Tool.Name == name {
			return x, true
		}
	}
	return Binding{}, false
}

// Validate checks the declarations and that every one of them can actually run.
func (b Toolbox) Validate() error {
	for _, x := range b {
		if x.Run == nil {
			return fmt.Errorf("llm: tool %q is declared with no handler", x.Tool.Name)
		}
	}
	return ValidateTools(b.Decls())
}

// Verdict is what a policy decided about a tool call.
type Verdict uint8

const (
	// VerdictAsk is the zero value on purpose: a policy that forgets to decide
	// has not thereby allowed anything.
	VerdictAsk Verdict = iota
	VerdictAllow
	VerdictDeny
)

// Decision is one policy's answer about one call.
type Decision struct {
	Verdict Verdict
	// Reason is shown to the user when asking and sent to the model when
	// denying, so it is written for both: "this would start a second session
	// on the same repository".
	Reason string
	// Input replaces the call's input when allowing. Empty leaves it alone.
	Input json.RawMessage
}

// Merge combines two policies' answers, and the asymmetry is the point.
//
// A deny is terminal: no later policy can clear it, and a policy with no
// opinion cannot overturn one that had. This is OpenClaw's rule for
// before_tool_call, where {block: true} stops lower-priority handlers and
// {block: false} is explicitly a no-op rather than an unblock — and it is the
// only ordering that is safe to get wrong, because the failure mode of the
// other one is a guard silently cancelling another guard.
func (d Decision) Merge(next Decision) Decision {
	switch {
	case d.Verdict == VerdictDeny:
		return d
	case next.Verdict == VerdictDeny:
		return next
	case d.Verdict == VerdictAsk:
		return next
	case next.Verdict == VerdictAsk:
		return d
	default:
		if len(next.Input) > 0 {
			return next
		}
		return d
	}
}

// Hooks are the interception points. Each may be nil.
type Hooks struct {
	// BeforeTool runs after the model asks and before anything happens.
	// Returning [VerdictAsk] escalates to a human through [event.NeedsInput],
	// which blocks this call — and only this call — until it is answered.
	BeforeTool func(ctx context.Context, call ToolCall) Decision

	// AfterTool sees the result before the model does. Use it to redact, to
	// annotate, or to shorten; it runs before the size cap, so a hook that
	// summarises a large result is worth more than the cap is.
	AfterTool func(ctx context.Context, call ToolCall, res ToolResult) ToolResult

	// Boundary is drained at the model boundary and its messages are appended
	// before the next call.
	//
	// This is the answer to talking over a working agent, and the position in
	// the loop is the whole of it: after the tool batch has run and its results
	// are paired with the request that produced them, before the next model
	// call. Draining mid-tool would split a batch from its results; waiting for
	// the run to end would mean the words arrive after the thing they were
	// meant to change. OpenClaw drains at exactly this point for the same
	// reason.
	Boundary func(ctx context.Context) []Message

	// NeedsInputSpec adapts a call into the question a human is asked. Nil
	// means a plain permission question naming the tool.
	NeedsInputSpec func(call ToolCall, d Decision) event.InputSpec
}

// Result is one completed run.
type Result struct {
	// Text is the model's final answer, after the last tool call.
	Text string
	// Messages is the full history including the tool round trips, so a caller
	// can continue the conversation rather than rebuild it.
	Messages []Message
	Stop     event.StopReason
	Usage    Usage
	Duration time.Duration
	// Turns is how many model calls it took.
	Turns int
	// ToolCalls is how many tools ran, denials included.
	ToolCalls int
}

// ErrMaxIterations means the run hit [Loop.MaxIterations] with the model still
// asking for tools. The partial [Result] is returned alongside it.
var ErrMaxIterations = errors.New("llm: the run hit its iteration limit with tools still pending")

// Run drives the loop to completion.
func (l *Loop) Run(ctx context.Context, req Request) (Result, error) {
	if l.Provider == nil {
		return Result{}, errors.New("llm: the loop has no provider")
	}
	if err := l.Tools.Validate(); err != nil {
		return Result{}, err
	}

	max := l.MaxIterations
	if max <= 0 {
		max = DefaultMaxIterations
	}
	req.Tools = l.Tools.Decls()

	started := time.Now()
	res := Result{Messages: req.Messages}
	l.emit(func(m event.Meta) event.Event { return event.TurnStarted{Meta: m} })

	for res.Turns < max {
		res.Turns++

		turn := req
		turn.Messages = res.Messages
		resp, err := l.call(ctx, turn)
		if err != nil {
			res.Stop = event.StopError
			res.Duration = time.Since(started)
			l.emitError(err)
			return res, err
		}

		res.Usage.InputTokens += resp.Usage.InputTokens
		res.Usage.OutputTokens += resp.Usage.OutputTokens
		res.Usage.TotalTokens += resp.Usage.TotalTokens
		res.Text = resp.Text

		if resp.Text != "" {
			text := resp.Text
			l.emit(func(m event.Meta) event.Event {
				return event.TextDelta{Meta: m, Text: text}
			})
		}

		res.Messages = append(res.Messages, Message{
			Role:      RoleAssistant,
			Text:      resp.Text,
			ToolCalls: resp.ToolCalls,
		})

		if len(resp.ToolCalls) == 0 {
			res.Stop = stopFrom(resp.FinishReason)
			res.Duration = time.Since(started)
			l.emitCompleted(res)
			return res, nil
		}

		results := l.runBatch(ctx, resp.ToolCalls)
		res.ToolCalls += len(results)
		res.Messages = append(res.Messages, Message{Role: RoleUser, ToolResults: results})

		// The boundary. Everything above this line belongs to the turn that
		// just ended; everything below belongs to the next one.
		if l.Hooks.Boundary != nil {
			if extra := l.Hooks.Boundary(ctx); len(extra) > 0 {
				res.Messages = append(res.Messages, extra...)
			}
		}

		if err := ctx.Err(); err != nil {
			res.Stop = event.StopCancelled
			res.Duration = time.Since(started)
			l.emitCompleted(res)
			return res, err
		}
	}

	res.Stop = event.StopMaxTurnRequests
	res.Duration = time.Since(started)
	l.emitCompleted(res)
	return res, ErrMaxIterations
}

// call makes one model call under the per-call timeout.
func (l *Loop) call(ctx context.Context, req Request) (Response, error) {
	if l.CallTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, l.CallTimeout)
		defer cancel()
	}
	return l.Provider.Complete(ctx, req)
}

// runBatch decides every call in the batch, then runs the ones that survived.
//
// Deciding first is deliberate. Approving call one, running it, and then
// denying call two leaves half a batch executed and a human who was asked
// about the half that did not matter; deciding the batch as a batch means the
// person sees the whole of what was requested before any of it happens.
func (l *Loop) runBatch(ctx context.Context, calls []ToolCall) []ToolResult {
	decisions := make([]Decision, len(calls))
	for i, c := range calls {
		decisions[i] = l.decide(ctx, c)
	}

	results := make([]ToolResult, len(calls))
	var wg sync.WaitGroup
	for i := range calls {
		call, d := calls[i], decisions[i]

		if d.Verdict != VerdictAllow {
			reason := d.Reason
			if reason == "" {
				reason = "the user did not approve this"
			}
			results[i] = ToolResult{CallID: call.ID, Content: reason, IsError: true}
			l.emitToolDenied(call, reason)
			continue
		}
		if len(d.Input) > 0 {
			call.Input = d.Input
		}

		bind, ok := l.Tools.find(call.Name)
		if !ok {
			// A name we never declared. Say so plainly rather than failing the
			// run: the model can recover from "no such tool" and cannot
			// recover from a dead loop.
			results[i] = ToolResult{
				CallID:  call.ID,
				Content: fmt.Sprintf("no tool named %q is available", call.Name),
				IsError: true,
			}
			continue
		}

		if bind.Tool.ParallelSafe {
			wg.Add(1)
			go func(i int, call ToolCall, bind Binding) {
				defer wg.Done()
				results[i] = l.invoke(ctx, bind, call)
			}(i, call, bind)
			continue
		}

		// An unsafe call is a barrier, not merely something that does not run
		// concurrently with its own kind. Letting a read that was already in
		// flight overlap a write is the same race as running two writes, just
		// harder to see: "parallel-safe" is a claim about having no effects,
		// not a claim about tolerating someone else's.
		wg.Wait()
		results[i] = l.invoke(ctx, bind, call)
	}
	wg.Wait()
	return results
}

// invoke runs one tool and normalises everything that can come back out of it.
func (l *Loop) invoke(ctx context.Context, bind Binding, call ToolCall) ToolResult {
	l.emit(func(m event.Meta) event.Event {
		return event.ToolStarted{
			Meta: m, ID: call.ID, Tool: call.Name,
			Target:   target(call),
			RawInput: rawInput(call),
		}
	})

	res, err := bind.Run(ctx, call)
	if err != nil {
		res = ToolResult{Content: err.Error(), IsError: true}
	}
	res.CallID = call.ID

	if l.Hooks.AfterTool != nil {
		res = l.Hooks.AfterTool(ctx, call, res)
		res.CallID = call.ID
	}

	// The cap runs last so a hook that shortens a result is not undone by it,
	// and so nothing reaches the model uncapped.
	if trimmed, cut := bind.Tool.Truncate(res.Content); cut {
		res.Content = trimmed
		if l.Log != nil {
			l.Log.Debug("llm: truncated a tool result", "tool", call.Name)
		}
	}

	status := event.ToolCompleted
	if res.IsError {
		status = event.ToolFailed
	}
	chunk := res.Content
	l.emit(func(m event.Meta) event.Event {
		return event.ToolOutput{Meta: m, ID: call.ID, Chunk: chunk, Status: status}
	})
	return res
}

// decide runs the policy and, if it did not resolve, asks a human.
func (l *Loop) decide(ctx context.Context, call ToolCall) Decision {
	d := Decision{Verdict: VerdictAllow}
	if l.Hooks.BeforeTool != nil {
		d = Decision{}.Merge(l.Hooks.BeforeTool(ctx, call))
	}
	if d.Verdict != VerdictAsk {
		return d
	}
	return l.ask(ctx, call, d)
}

// ask raises a blocking question and waits for it.
//
// This is the shape Claude Code uses and the reason it uses it: question-asking
// is promoted to something the harness can render as a modal and block the loop
// on, rather than text the model hopes someone reads. Here it is an
// [event.NeedsInput], which means the question reaches the console and the
// phone through the path that already exists for runtime approvals — ADAPTERS.md
// §7's blocking ping, which quiet hours do not suppress, because a session
// blocked at 3am is still blocked at 8am.
func (l *Loop) ask(ctx context.Context, call ToolCall, d Decision) Decision {
	if l.Emit == nil {
		// Nobody is listening, so nobody can answer. Denying is the only
		// honest outcome: an unanswerable question must not become consent.
		return Decision{Verdict: VerdictDeny, Reason: "there was no way to ask for approval"}
	}

	spec := event.InputSpec{
		Ask:    event.InputPermission,
		Prompt: d.Reason,
		Tool: &event.ToolRef{
			ID: call.ID, Name: call.Name, Title: target(call), RawInput: rawInput(call),
		},
		Options: []event.Option{
			{ID: "allow", Name: "Allow", Kind: event.OptionAllowOnce},
			{ID: "deny", Name: "Deny", Kind: event.OptionRejectOnce},
		},
	}
	if spec.Prompt == "" {
		spec.Prompt = "Run " + call.Name + "?"
	}
	if l.Hooks.NeedsInputSpec != nil {
		spec = l.Hooks.NeedsInputSpec(call, d)
	}

	// We are the runtime here, so there is nothing to notify: the reply
	// resolves the question and the loop reads the outcome.
	q := event.NewNeedsInput(l.meta(), spec, func(context.Context, event.Reply) error { return nil })
	l.Emit(q)

	select {
	case <-q.Done():
	case <-ctx.Done():
		q.Withdraw("the run was cancelled")
		return Decision{Verdict: VerdictDeny, Reason: "the run was cancelled before this was approved"}
	}

	reply, ok := q.Outcome()
	if !ok {
		return Decision{Verdict: VerdictDeny, Reason: "the question was withdrawn"}
	}
	if reply.Decision == event.DecisionAllow || reply.OptionID == "allow" {
		out := Decision{Verdict: VerdictAllow}
		if len(reply.UpdatedInput) > 0 {
			if raw, err := json.Marshal(reply.UpdatedInput); err == nil {
				out.Input = raw
			}
		}
		return out
	}
	reason := reply.Message
	if reason == "" {
		reason = "the user declined this"
	}
	return Decision{Verdict: VerdictDeny, Reason: reason}
}

// target is the human-readable thing a call acts on.
//
// ADAPTERS.md §5 is explicit that an empty Target is better than an inferred
// one, so this reads the conventional names and stops — it does not pick the
// first string field and hope.
func target(call ToolCall) string {
	for _, k := range []string{"path", "query", "session", "session_id", "command", "name"} {
		if v := call.Arg(k); v != "" {
			return v
		}
	}
	return ""
}

func rawInput(call ToolCall) map[string]any {
	if len(call.Input) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(call.Input, &m); err != nil {
		return nil
	}
	return m
}

// stopFrom maps a provider finish reason onto ADAPTERS.md §5's five.
func stopFrom(reason string) event.StopReason {
	switch reason {
	case "max_tokens", "length":
		return event.StopMaxTokens
	case "refusal", "content_filter":
		return event.StopRefusal
	case "":
		// Absent rather than unknown: a provider that reported nothing has not
		// told us the turn failed.
		return event.StopEndTurn
	default:
		return event.StopEndTurn
	}
}

func (l *Loop) meta() event.Meta {
	m := l.Meta
	m.Seq = l.seq.Add(1)
	if m.At.IsZero() {
		m.At = time.Now()
	}
	return m
}

func (l *Loop) emit(build func(event.Meta) event.Event) {
	if l.Emit == nil {
		return
	}
	l.Emit(build(l.meta()))
}

func (l *Loop) emitError(err error) {
	l.emit(func(m event.Meta) event.Event {
		return event.Error{Meta: m, Message: err.Error()}
	})
}

func (l *Loop) emitToolDenied(call ToolCall, reason string) {
	l.emit(func(m event.Meta) event.Event {
		return event.ToolOutput{
			Meta: m, ID: call.ID, Chunk: reason, Status: event.ToolFailed,
		}
	})
}

func (l *Loop) emitCompleted(res Result) {
	usage := &event.Usage{
		InputTokens:  event.I64(res.Usage.InputTokens),
		OutputTokens: event.I64(res.Usage.OutputTokens),
		TotalTokens:  event.I64(res.Usage.TotalTokens),
	}
	stop, dur := res.Stop, res.Duration
	l.emit(func(m event.Meta) event.Event {
		return event.TurnCompleted{
			Meta: m, OK: stop.OK(), StopReason: stop, Duration: dur, Usage: usage,
		}
	})
}
