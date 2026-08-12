package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/llm"
)

// Reflection is the closed learning loop: after a run, decide whether what just
// happened is worth writing down.
//
// Hermes creates a skill autonomously after a complex task and lets skills
// improve during use. We had the storage and the distribution for that and no
// trigger — `author_skill` worked and nothing ever decided to call it, which is
// the difference between a system that can learn and one that has learned once.
//
// Two triggers, and the asymmetry between them is the design:
//
//   - **Success worth repeating.** A run that took real work and finished is a
//     candidate for a new playbook. The bar is deliberately high (see
//     [ReflectOptions.MinToolCalls]) because a skill for something trivial is
//     worse than no skill: it costs context on every future turn and will never
//     be the best answer to anything.
//   - **Failure with a playbook in it.** A run that used a skill and still went
//     wrong is the strongest signal there is that the skill is wrong, and it is
//     the one Hermes means by "improve during use". This bar is low — one
//     failure is enough — because the playbook already exists, so improving it
//     costs nothing new in context.
//
// Reflection never speaks and never blocks the answer. It runs after the user
// has been told what happened, on a model call the user is not waiting on.
type Reflection struct {
	opts ReflectOptions
}

// ReflectOptions configures reflection.
type ReflectOptions struct {
	// Model is the big one. Reflection is judgement about what is worth
	// remembering, which is the same faculty the escalation path exists to
	// reach; the small model would be cheaper and would write playbooks nobody
	// wants.
	Model llm.Provider

	// Skills is where a new or improved playbook goes.
	Skills Skills

	// MinToolCalls is how much work a run has to have done before success is
	// worth writing down. Zero means [DefaultMinToolCalls].
	//
	// Tool calls rather than turns, because a run that asked three questions
	// and got three answers did something; a run that took four turns to
	// produce one sentence did not.
	MinToolCalls int
}

// DefaultMinToolCalls is the bar for "this was complex enough to be worth a
// playbook".
//
// Three, because two is the shape of nearly every successful run — look at what
// is running, then act on it — and a skill for that is a skill for "do your
// job". The number is a product decision and should move with real usage rather
// than with taste.
const DefaultMinToolCalls = 3

// NewReflection builds one. A nil model or nil skills store disables it, which
// is the correct behaviour on an install with no key: the orchestrator still
// works, it just stops learning.
func NewReflection(o ReflectOptions) *Reflection {
	if o.Model == nil || o.Skills == nil {
		return nil
	}
	if o.MinToolCalls <= 0 {
		o.MinToolCalls = DefaultMinToolCalls
	}
	return &Reflection{opts: o}
}

// Trigger is why reflection ran, or why it did not.
type Trigger string

const (
	// TriggerNone means the run was not worth reflecting on.
	TriggerNone Trigger = ""
	// TriggerComplexSuccess is a run that did real work and finished.
	TriggerComplexSuccess Trigger = "complex_success"
	// TriggerSkillFailed is a run that used a playbook and went wrong anyway.
	TriggerSkillFailed Trigger = "skill_failed"
)

// Consider decides whether a completed run is worth learning from, and does it.
//
// It returns the trigger that fired and the skill written, if any. An error
// here is never the caller's problem: the user already has their answer, and a
// reflection that failed is a reflection that did not happen.
func (r *Reflection) Consider(ctx context.Context, in Utterance, res llm.Result, used []string) (Trigger, *Skill, error) {
	if r == nil {
		return TriggerNone, nil, nil
	}

	trigger := r.classify(res, used)
	if trigger == TriggerNone {
		return TriggerNone, nil, nil
	}

	skill, err := r.propose(ctx, in, res, used, trigger)
	if err != nil || skill == nil {
		return trigger, nil, err
	}
	if err := r.opts.Skills.Author(ctx, *skill); err != nil {
		return trigger, nil, err
	}
	return trigger, skill, nil
}

func (r *Reflection) classify(res llm.Result, used []string) Trigger {
	switch {
	case len(used) > 0 && !res.Stop.OK():
		// A playbook was followed and the turn still failed. One is enough.
		return TriggerSkillFailed
	case res.Stop.OK() && res.ToolCalls >= r.opts.MinToolCalls:
		return TriggerComplexSuccess
	default:
		return TriggerNone
	}
}

// reflectSchema is the shape a proposal comes back in. Structured output rather
// than prose, because "did the model decide to write a skill" has to be a field
// and not something inferred from whether it used the word "skill".
var reflectSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"worth_writing": map[string]any{
			"type":        "boolean",
			"description": "False if this was ordinary work that no playbook would improve.",
		},
		"name":  map[string]any{"type": "string"},
		"title": map[string]any{"type": "string"},
		"when":  map[string]any{"type": "string"},
		"steps": map[string]any{"type": "string"},
		"needs": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	},
	"required":             []any{"worth_writing"},
	"additionalProperties": false,
}

func (r *Reflection) propose(ctx context.Context, in Utterance, res llm.Result, used []string, t Trigger) (*Skill, error) {
	prompt := reflectPrompt(in, res, used, t)

	// No tools. Reflection decides what to write down; it does not get to start
	// sessions or grant anything while it is thinking about the last turn.
	out, err := r.opts.Model.Complete(ctx, llm.Request{
		System:     ReflectSystemPrompt,
		Messages:   []llm.Message{{Role: llm.RoleUser, Text: prompt}},
		MaxTokens:  900,
		ToolChoice: &llm.ToolChoice{Mode: llm.ChoiceNone},
		Format: &llm.OutputFormat{
			Name:   "skill_proposal",
			Schema: reflectSchema,
			Strict: true,
		},
	})
	if err != nil {
		return nil, err
	}

	var p struct {
		WorthWriting bool     `json:"worth_writing"`
		Name         string   `json:"name"`
		Title        string   `json:"title"`
		When         string   `json:"when"`
		Steps        string   `json:"steps"`
		Needs        []string `json:"needs"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.Text)), &p); err != nil {
		return nil, fmt.Errorf("orchestrator: the reflection did not parse: %w", err)
	}
	if !p.WorthWriting {
		// The common answer, and it must stay cheap to say. A reflection that
		// felt obliged to produce a skill every time would fill the bus with
		// playbooks for nothing.
		return nil, nil
	}

	skill := Skill{
		Name: p.Name, Title: p.Title, When: p.When, Steps: p.Steps, Needs: p.Needs,
		Origin: string(t),
	}
	if err := skill.Validate(); err != nil {
		return nil, err
	}
	return &skill, nil
}

// ReflectSystemPrompt is the instruction for the reflection turn.
const ReflectSystemPrompt = `You are deciding whether the turn that just finished is worth writing down as a reusable playbook for the agents on this machine.

Write one only if a future request of the same shape would go better for having it. Ordinary work does not need a playbook, and a bad one is worse than none: it costs context on every future turn and will never be the best answer to anything. Saying worth_writing: false is the common and correct answer.

If you do write one:
- "when" is the trigger condition, in the user's language, not yours.
- "steps" is instructions for an agent to carry out — imperative sentences, no code. The agent reading it has a browser, a shell and an editor; you do not. Never write steps that assume the reader is you.
- "needs" lists capabilities the agent must already have, so it can say it lacks one rather than improvise.
- "name" is lowercase with underscores.

If you are improving an existing playbook, keep its name so the improvement replaces it rather than forking it, and fix the part that went wrong rather than rewriting what worked.`

func reflectPrompt(in Utterance, res llm.Result, used []string, t Trigger) string {
	var b strings.Builder

	switch t {
	case TriggerSkillFailed:
		fmt.Fprintf(&b, "A playbook was followed and the turn still went wrong. "+
			"Playbook(s) used: %s.\n", strings.Join(used, ", "))
		fmt.Fprintf(&b, "Improve it: keep the name, fix what failed.\n\n")
	default:
		fmt.Fprintf(&b, "A turn finished successfully after %d tool calls.\n\n", res.ToolCalls)
	}

	fmt.Fprintf(&b, "What the user asked for:\n%s\n\n", in.Text)
	fmt.Fprintf(&b, "How it ended: %s\n", res.Stop)
	if res.Text != "" {
		fmt.Fprintf(&b, "\nWhat the agent reported:\n%s\n", res.Text)
	}

	// The tool calls, in order, are the actual sequence worth capturing —
	// which is why they come from the message history rather than from a
	// summary the model would have to be trusted about.
	steps := toolSequence(res)
	if len(steps) > 0 {
		fmt.Fprintf(&b, "\nWhat it actually did, in order:\n")
		for i, s := range steps {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, s)
		}
	}
	return b.String()
}

// toolSequence reads the calls back out of the history.
func toolSequence(res llm.Result) []string {
	var out []string
	for _, m := range res.Messages {
		for _, c := range m.ToolCalls {
			if target := toolTarget(c); target != "" {
				out = append(out, c.Name+" "+target)
				continue
			}
			out = append(out, c.Name)
		}
	}
	return out
}

func toolTarget(c llm.ToolCall) string {
	for _, k := range []string{"query", "prompt", "name", "session", "workspace"} {
		if v := c.Arg(k); v != "" {
			if len(v) > 80 {
				v = v[:80]
			}
			return "(" + v + ")"
		}
	}
	return ""
}

// SkillsUsed reports which playbooks a run followed.
//
// It reads the events rather than the transcript: a skill is reached through
// the shared bus, so the runtime that used one emitted a ToolStarted naming it,
// and that is an observation rather than an inference.
func SkillsUsed(events []event.Event) []string {
	var out []string
	seen := map[string]bool{}
	for _, e := range events {
		ts, ok := e.(event.ToolStarted)
		if !ok {
			continue
		}
		name, ok := strings.CutPrefix(ts.Tool, SkillConnector+"_")
		if !ok || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}
