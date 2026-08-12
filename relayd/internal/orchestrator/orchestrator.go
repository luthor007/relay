package orchestrator

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/routing"
	"github.com/luthor007/relay/relayd/internal/summarize"
)

// Options builds an [Orchestrator].
type Options struct {
	// Small speaks. Nil is supported and falls back to the deterministic
	// templates in internal/summarize — a blunter product, not a broken one.
	Small llm.Provider

	// Big holds the tools and does the work. Nil means nothing escalates,
	// which is what an unconfigured install looks like: the allowlist still
	// answers status and control, and everything else says so.
	Big llm.Provider

	// Deps is everything the big model can reach. Sessions and Memory are here
	// too for callers that only have those.
	Deps Deps

	// Emit publishes the big model's run onto the bus. The events are the same
	// nine every runtime adapter emits, so the console and the pinger need no
	// idea this was not a runtime.
	Emit func(event.Event)

	// Approve decides the consequential tools. Nil means every one of them is
	// asked, which is ORCHESTRATOR.md §4b's default and the safe direction to
	// be wrong in.
	Approve func(ctx context.Context, call llm.ToolCall) llm.Decision

	// Reflect is the closed learning loop. Zero disables it and the
	// orchestrator still works — it just stops getting better.
	Reflect ReflectOptions

	Log *slog.Logger
}

// Orchestrator is the two-model split.
type Orchestrator struct {
	small    llm.Provider
	big      llm.Provider
	tools    llm.Toolbox
	emit     func(event.Event)
	approve  func(ctx context.Context, call llm.ToolCall) llm.Decision
	narrator *summarize.Narrator
	reflect  *Reflection
	log      *slog.Logger
}

// New builds one.
func New(o Options) (*Orchestrator, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	box := ToolboxFor(o.Deps)
	if o.Big != nil && len(box) == 0 {
		return nil, errors.New("orchestrator: a big model with no tools cannot do anything the small one cannot")
	}
	if len(box) > 0 {
		if err := box.Validate(); err != nil {
			return nil, err
		}
	}
	return &Orchestrator{
		small:    o.Small,
		big:      o.Big,
		tools:    box,
		emit:     o.Emit,
		approve:  o.Approve,
		narrator: summarize.NewNarrator(summarize.NarratorOptions{Model: o.Small}),
		reflect: NewReflection(ReflectOptions{
			// The big model, deliberately: reflection is judgement about what is
			// worth remembering, and the small one would write playbooks nobody
			// wants.
			Model:        cmp.Or[llm.Provider](o.Reflect.Model, o.Big),
			Skills:       cmp.Or[Skills](o.Reflect.Skills, o.Deps.Skills),
			MinToolCalls: o.Reflect.MinToolCalls,
		}),
		log: o.Log,
	}, nil
}

// Utterance is one thing somebody said.
type Utterance struct {
	Text string
	// Session is the session it was routed to, which is a different question
	// from the one this package answers — internal/routing decides *which
	// session*, this decides *which model*.
	Session string
}

// Outcome is what happened to it.
type Outcome struct {
	// Class is the allowlist row that matched, or ClassEscalate.
	Class routing.Class
	// Escalated is whether the big model was woken. Roughly 30% of utterances,
	// by ORCHESTRATOR.md §3b's estimate, and that ratio is the pricing.
	Escalated bool
	// Ack is the immediate line, already spoken by the time the work starts.
	Ack summarize.Speech
	// Speech is the final line, empty when nothing escalated.
	Speech summarize.Speech
	// Result is the big model's run. Zero when nothing escalated.
	Result llm.Result
	// Rule names the allowlist row, for the audit trail.
	Rule string
	// Learned is the playbook this turn produced, if any. Nil is the common
	// case and the correct one: most work is ordinary.
	Learned *Skill
}

// Speak is the callback that puts a line in the user's ear. Handle calls it
// once for the acknowledgement and once for the outcome, in that order.
type Speak func(summarize.Speech)

// Handle routes one utterance to a model and runs it.
//
// The order is the design. The acknowledgement goes out first, from the small
// model, before anything decides how long the work will take — SYSTEM.md §7b
// measured eight seconds of silence as reading like a broken device no matter
// what arrives afterwards. Then the allowlist decides, and only what it refuses
// to answer alone reaches the big model.
func (o *Orchestrator) Handle(ctx context.Context, in Utterance, speak Speak) (Outcome, error) {
	ctx = WithSession(ctx, in.Session)
	verdict := routing.Classify(in.Text)
	out := Outcome{Class: verdict.Class, Escalated: verdict.Escalate, Rule: verdict.Rule}

	// §3b lists acknowledgement as its own allowlist row — "the immediate 'on
	// it', always" — so it is not conditional on anything below.
	out.Ack = o.narrator.Ack(ctx, summarize.AckHint{Verb: verbFor(verdict.Class)})
	if speak != nil {
		speak(out.Ack)
	}

	if !verdict.Escalate {
		// Answered alone. internal/routing owns what happens next; this
		// package's job was to decide that the big model is not needed, and
		// the caller's existing command path does the rest.
		return out, nil
	}
	if o.big == nil || len(o.tools) == 0 {
		out.Speech = summarize.Speech{
			Moment: summarize.MomentCompleted,
			Text:   "I can't take that on — no work model is configured yet.",
			Source: summarize.SourceTemplate,
		}
		if speak != nil {
			speak(out.Speech)
		}
		return out, nil
	}

	// The narration hears the run as events, exactly as it hears a runtime.
	// There is no path by which it could be handed the transcript instead, and
	// that is what makes drift structurally impossible rather than discouraged.
	narration := routing.NewNarration(routing.NarrationOptions{
		Session: in.Session,
		Model:   o.small,
	})

	// The events are kept so reflection can see which playbooks were followed.
	// Reading it off the events rather than the transcript makes it an
	// observation: a skill is reached through the shared bus, so the runtime
	// that used one emitted a ToolStarted naming it.
	var mu sync.Mutex
	var seen []event.Event

	loop := &llm.Loop{
		Provider: o.big,
		Tools:    o.tools,
		Meta:     event.Meta{Runtime: "relay", Session: in.Session},
		Emit: func(e event.Event) {
			narration.Observe(e)
			mu.Lock()
			seen = append(seen, e)
			mu.Unlock()
			if o.emit != nil {
				o.emit(e)
			}
		},
		Hooks: llm.Hooks{BeforeTool: o.beforeTool},
		Log:   o.log,
	}

	res, err := loop.Run(ctx, llm.Request{
		System:   SystemPrompt,
		Messages: []llm.Message{{Role: llm.RoleUser, Text: in.Text}},
		// The big model decides for itself whether it needs a tool. Forcing
		// one here would make "what do you think about X" impossible to answer.
		ToolChoice: &llm.ToolChoice{Mode: llm.ChoiceAuto},
	})
	out.Result = res

	out.Speech = narration.Completed(ctx)
	if speak != nil {
		speak(out.Speech)
	}

	// After the user has their answer, never before it. Reflection is a second
	// model call and the person is not waiting on it; a learning loop that
	// added latency to every turn would be turned off within a day.
	mu.Lock()
	used := SkillsUsed(seen)
	mu.Unlock()
	if trigger, skill, rerr := o.reflect.Consider(ctx, in, res, used); rerr != nil {
		// Never the caller's problem. The turn succeeded or failed on its own
		// merits and a reflection that did not happen is not a third outcome.
		o.log.Info("relayd: reflection did not complete", "trigger", trigger, "error", rerr)
	} else if skill != nil {
		out.Learned = skill
		o.log.Info("relayd: wrote a playbook", "skill", skill.Name, "trigger", trigger)
	}

	return out, err
}

// beforeTool is ORCHESTRATOR.md §4b's confirmation rule, as a policy rather
// than a habit.
//
// Note the direction of the default: a consequential tool with no approver
// configured is asked, and [llm.Loop] denies an unanswerable question. Both
// halves have to fail closed for that to hold, and this is the half that
// decides which tools are worth asking about at all.
func (o *Orchestrator) beforeTool(ctx context.Context, call llm.ToolCall) llm.Decision {
	if !Consequential(call.Name) {
		return llm.Decision{Verdict: llm.VerdictAllow}
	}
	if o.approve != nil {
		return o.approve(ctx, call)
	}
	return llm.Decision{Reason: askFor(call)}
}

func askFor(call llm.ToolCall) string {
	switch call.Name {
	case ToolStartSession:
		if p := call.Arg("prompt"); p != "" {
			return "Start a session for “" + p + "”?"
		}
		return "Start a new session?"
	case ToolStopSession:
		if s := call.Arg("session"); s != "" {
			return "Stop " + s + "? Anything unfinished is lost."
		}
		return "Stop that session?"
	default:
		return "Run " + call.Name + "?"
	}
}

// verbFor is the one word the acknowledgement leads with. It comes from the
// classification rather than the model, so it cannot be a guess about work that
// has not started.
func verbFor(c routing.Class) string {
	switch c {
	case routing.ClassStatus:
		return "checking"
	case routing.ClassControl:
		return ""
	case routing.ClassMemory:
		return "looking"
	default:
		return ""
	}
}

// SystemPrompt is the big model's instruction.
//
// Short on purpose. The judgement about when to reach for each tool lives in
// the tool descriptions, where it is next to the schema the model is reading
// anyway; a system prompt that repeats it is a second copy to keep in sync.
const SystemPrompt = `You are the work half of Relay, a voice orchestrator for coding agents.

You do not write code. You drive agent sessions that do: look at what is already
running before starting anything, prefer adding to a session over starting a
second one on the same work, and search memory before asking the user something
this machine already knows.

The runtimes are different products with different strengths, commands and
traps, and you are the only thing on this machine that can see across all of
them. Use describe_runtime before choosing one you have not used, and prompt it
the way its brief says to.

The user is speaking, often hands-free, and hears a short spoken summary rather
than your text. Be brief. Never read out an identifier, a path, an error message
or a stack trace.

If you cannot do something, say so in one sentence and stop.`
