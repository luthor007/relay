package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/llm"
)

// ORCHESTRATOR.md §4's LLM tie-break, which until now was an interface, a set
// of test fakes, and nothing else.
//
// It runs in one narrow place and it is worth being precise about which,
// because the whole design of this package is that the model is the last
// resort rather than the mechanism. [Scorer] ranks candidates on recency, repo
// and file overlap, and entity match. When one candidate wins clearly, it wins
// and no model is called. When two are close — or the best is middling — the
// deterministic part has run out of signal, and there are exactly two honest
// moves left: ask the user, or ask a model whether one of them is obviously
// right. This is the second, and it is allowed to decline into the first.
//
// Three properties this has to have, and they are the reason it is not simply
// a prompt:
//
//  1. **It may decline.** The interface says so — "an LLM that always picks
//     turns a tie into a silent wrong-continue" — so the schema has an explicit
//     "unsure" and declining is the default reading of a bad answer. A model
//     that cannot tell is more useful saying so than guessing, because
//     [KindAsk] is a good outcome here and a wrong continue is not recoverable.
//  2. **It only ever chooses from the shortlist.** The reply names a candidate
//     index. An id hallucinated whole is not a session, and a free-text id
//     would make that failure look like a routing bug rather than a decline.
//  3. **It cannot hang the turn.** The user is mid-utterance with the
//     announcement still to come, so this gets [TieBreakTimeout] and a failure
//     is a decline, never an error the caller has to handle.
//
// What it is NOT is the thing that decides where work goes. That is the
// scorer, plus the user, plus [ParseCommand]. This is a nudge applied to a
// two-way coin flip.

// TieBreakTimeout caps the call. SYSTEM.md §7b budgets the whole voice turn in
// hundreds of milliseconds and the announcement cannot be spoken until routing
// has decided, so a slow tie-break is indistinguishable from a broken one.
const TieBreakTimeout = 2 * time.Second

// tieBreakSchema forces an answer this package can act on: an index into the
// shortlist, or a refusal.
var tieBreakSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"choice": map[string]any{
			"type": "integer",
			"description": "1-based index of the session this utterance continues, " +
				"or 0 when it is not clearly one of them",
		},
		"why": map[string]any{
			"type":        "string",
			"description": "at most eight words, naming the evidence",
		},
	},
	"required":             []string{"choice", "why"},
	"additionalProperties": false,
}

const tieBreakSystem = `You are resolving one ambiguous voice command for a developer who is ` +
	`talking to several running coding sessions at once.

You are given what they just said and a shortlist of sessions it might belong to. The shortlist ` +
	`is already close — a scorer could not separate them — so only answer when the utterance ` +
	`names something that clearly belongs to one of them: a repo, a file, a service, a task, or ` +
	`an obvious continuation of that session's subject.

Answer 0 whenever you are unsure. Being unsure is a useful answer here: the user is asked out ` +
	`loud, which costs them a second. Guessing wrong drops their question into an unrelated ` +
	`session, poisons its context, and cannot be undone by the session that received it. A wrong ` +
	`answer is far more expensive than no answer, so prefer 0 unless the evidence is in the words ` +
	`themselves.`

// LLMTieBreak returns the tie-break backed by a model.
//
// p is the big model. It is consulted at most once per routed utterance, only
// when Options.Auto is on and the scorer produced a tie.
func LLMTieBreak(p llm.Provider) TieBreaker {
	return TieBreakFunc(func(ctx context.Context, req Request, cands []Candidate) (Candidate, bool) {
		if p == nil || len(cands) < 2 || strings.TrimSpace(req.Text) == "" {
			return Candidate{}, false
		}

		ctx, cancel := context.WithTimeout(ctx, TieBreakTimeout)
		defer cancel()

		res, err := p.Complete(ctx, llm.Request{
			System:    tieBreakSystem,
			Messages:  []llm.Message{{Role: llm.RoleUser, Text: tieBreakPrompt(req, cands)}},
			MaxTokens: 120,
			// The decision is a field, not a sentence. Asking for prose and
			// then reading a session out of it is how "the model said the
			// refactor one" becomes a parser.
			Format: &llm.OutputFormat{Name: "tie_break", Schema: tieBreakSchema, Strict: true},
			// Low effort on purpose: this is a two-way choice with the evidence
			// already in front of it, and the budget is two seconds.
			Effort: "low",
		})
		if err != nil {
			// A decline, not an error. The caller's next move is to ask the
			// user, which is the right thing to do when the model is down.
			return Candidate{}, false
		}

		var out struct {
			Choice int    `json:"choice"`
			Why    string `json:"why"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(res.Text)), &out) != nil {
			return Candidate{}, false
		}
		// Out of range covers both the refusal (0) and a hallucinated index.
		if out.Choice < 1 || out.Choice > len(cands) {
			return Candidate{}, false
		}

		pick := cands[out.Choice-1]
		if why := strings.TrimSpace(out.Why); why != "" {
			pick.Why = why
		}
		return pick, true
	})
}

// tieBreakPrompt describes the shortlist. It sends what the session is *about*
// — subject, entities, recent files — and never its transcript: this runs on
// every ambiguous utterance, and shipping conversation history to a provider to
// settle a coin flip is a much larger disclosure than the decision is worth.
func tieBreakPrompt(req Request, cands []Candidate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "They said: %q\n\n", req.Text)
	if req.Workspace != "" {
		fmt.Fprintf(&b, "They are working in: %s\n\n", req.Workspace)
	}
	b.WriteString("Sessions it might continue:\n")
	for i, c := range cands {
		v := c.Session
		fmt.Fprintf(&b, "%d. %s\n", i+1, subjectOr(v))
		if v.Workspace != "" {
			fmt.Fprintf(&b, "   workspace: %s\n", v.Workspace)
		}
		if len(v.Entities) > 0 {
			fmt.Fprintf(&b, "   about: %s\n", strings.Join(v.Entities, ", "))
		}
		if len(v.Files) > 0 {
			fmt.Fprintf(&b, "   recent files: %s\n", strings.Join(topN(v.Files, 5), ", "))
		}
		if !v.LastActive.IsZero() {
			fmt.Fprintf(&b, "   last active: %s ago\n", ago(req.At, v.LastActive))
		}
	}
	return b.String()
}

func subjectOr(v SessionView) string {
	if s := strings.TrimSpace(v.Subject); s != "" {
		return s
	}
	return "(no subject; session " + v.ID + ")"
}

func topN(ss []string, n int) []string {
	if len(ss) <= n {
		return ss
	}
	return ss[:n]
}

// ago is deliberately coarse. "Four minutes" and "six minutes" do not decide
// anything a tie-break should be deciding, and a precise duration invites the
// model to treat recency as the signal — which is the scorer's job, already
// done, and the reason these two candidates tied in the first place.
func ago(now, then time.Time) string {
	d := now.Sub(then)
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}
