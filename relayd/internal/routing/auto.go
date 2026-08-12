package routing

import (
	"context"
	"math"
	"strings"
	"time"
)

// Scoring are the automatic router's weights.
//
// They are exported and defaulted rather than tuned constants because
// ORCHESTRATOR.md §6 puts automatic routing after real usage exists: the
// numbers below are a starting point that can be moved from a config file
// without a rebuild, and every one of them is a signal the doc names —
// recency, repo and file overlap, entity match against the session's subject.
type Scoring struct {
	// HalfLife is how long a session stays "recent". At exactly one half-life
	// of silence, recency contributes half its weight.
	HalfLife time.Duration

	Recency   float64
	Workspace float64
	Files     float64
	Entities  float64

	// Continue is the score a candidate must reach before the router will
	// continue an existing session on its own.
	Continue float64
	// Margin is how far ahead of the runner-up the winner has to be. Two
	// candidates inside this are a tie, and a tie is a question or a tie-break,
	// never a coin toss.
	Margin float64
	// New is the score below which the router starts a new session instead of
	// continuing a poor match. Between New and Continue it asks.
	New float64
}

func (s Scoring) withDefaults() Scoring {
	if s.HalfLife <= 0 {
		s.HalfLife = 20 * time.Minute
	}
	if s.Recency == 0 {
		s.Recency = 0.25
	}
	if s.Workspace == 0 {
		s.Workspace = 0.30
	}
	if s.Files == 0 {
		s.Files = 0.20
	}
	if s.Entities == 0 {
		// The heaviest weight, and deliberately heavier than recency: naming
		// what a session is about is a much stronger signal than having spoken
		// to it a minute ago, and letting recency win that contest is the
		// wrong-continue failure with a plausible-looking score attached.
		s.Entities = 0.40
	}
	if s.Continue == 0 {
		s.Continue = 0.55
	}
	if s.Margin == 0 {
		s.Margin = 0.15
	}
	if s.New == 0 {
		s.New = 0.20
	}
	return s
}

// TieBreaker is the LLM tie-break from ORCHESTRATOR.md §4, kept behind an
// interface so the scorer is testable without a model and so a deployment with
// no small model configured degrades to asking rather than to guessing.
type TieBreaker interface {
	// Break picks one of the candidates, or returns false to leave it
	// ambiguous. Returning false must be cheap and must be the answer whenever
	// the model is unsure — an LLM that always picks turns a tie into a silent
	// 50% guess, which is worse than the ask it replaced.
	Break(ctx context.Context, req Request, cands []Candidate) (Candidate, bool)
}

// TieBreakFunc adapts a function to [TieBreaker].
type TieBreakFunc func(ctx context.Context, req Request, cands []Candidate) (Candidate, bool)

func (f TieBreakFunc) Break(ctx context.Context, req Request, cands []Candidate) (Candidate, bool) {
	return f(ctx, req, cands)
}

// Score ranks the live sessions against an utterance.
//
// Four signals, all of them observable and none of them a model:
//
//   - recency, decayed on last activity;
//   - workspace match, exact or by directory name;
//   - file overlap, when the utterance names a file the session has touched;
//   - entity and subject match.
//
// The session the conversation is already in gets recency for free by virtue of
// having just been used, which is the continuity bias MEMORY.md §8 asks for
// without a special case for it.
func Score(req Request, live []SessionView, s Scoring) []Candidate {
	s = s.withDefaults()
	want := tokens(req.Text)
	at := req.At
	if at.IsZero() {
		at = time.Now()
	}

	out := make([]Candidate, 0, len(live))
	for _, v := range live {
		var total float64
		best, bestWhy := 0.0, "running"

		if rec := decay(at.Sub(v.LastActive), s.HalfLife); rec > 0 {
			total += s.Recency * rec
			if x := s.Recency * rec; x > best {
				best, bestWhy = x, "you were just in it"
			}
		}
		if w := workspaceMatch(req.Workspace, v.Workspace); w > 0 {
			total += s.Workspace * w
			if x := s.Workspace * w; x > best {
				best, bestWhy = x, "same repo"
			}
		}
		if f := agree(want, fileTerms(v)); f > 0 {
			total += s.Files * f
			if x := s.Files * f; x > best {
				best, bestWhy = x, "it touched that file"
			}
		}
		if e := agree(want, subjectTerms(v)); e > 0 {
			total += s.Entities * e
			if x := s.Entities * e; x > best {
				best, bestWhy = x, "it is about that"
			}
		}

		out = append(out, Candidate{Session: v, Score: round(total), Why: bestWhy})
	}
	sortCandidates(out)
	return out
}

// routeAuto is the automatic router. It returns false when it declines to
// decide, and the manual path takes over — which is the whole safety property:
// the scorer can only ever *add* an answer, never remove the ask.
func (r *Router) routeAuto(ctx context.Context, req Request, live []SessionView) (Decision, bool) {
	if len(live) == 0 {
		return Decision{}, false
	}
	cands := Score(req, live, r.scoring)
	s := r.scoring

	top := cands[0]
	runner := 0.0
	if len(cands) > 1 {
		runner = cands[1].Score
	}

	d := Decision{
		Text:       req.Text,
		Workspace:  req.Workspace,
		Automatic:  true,
		Candidates: cands,
		Confidence: top.Score,
	}

	switch {
	case top.Score >= s.Continue && top.Score-runner >= s.Margin:
		d.Kind = KindContinue
		d.Session = top.Session.ID
		d.Subject = top.Session.Name()
		d.Runtime = top.Session.Runtime
		d.Reason = ReasonAutomatic
		d.Because = top.Why
		return d, true

	case top.Score < s.New:
		// Nothing running looks related. A new session is the right answer and
		// it is also the recoverable one: a wrong new costs a session, a wrong
		// continue costs someone else's context.
		d.Kind = KindNew
		d.Reason = ReasonNewTopic
		d.Because = "nothing running looks related"
		d.RuntimeChoice = r.chooseRuntime(ctx, req, Command{}, live)
		if d.RuntimeChoice != nil {
			d.Runtime = d.RuntimeChoice.Runtime
			if !d.RuntimeChoice.Chosen() {
				// Nowhere to start it. Falling back to the manual path here
				// would continue the focus — the exact wrong continue the
				// scorer just declined to make — so it asks instead.
				d.Kind = KindAsk
				d.Reason = ReasonAmbiguous
				d.Question = d.RuntimeChoice.Ask
				d.Because = d.RuntimeChoice.Because
			}
		}
		return d, true
	}

	// A tie, or a middling best. This is where ORCHESTRATOR.md §4's LLM
	// tie-break goes, and where its absence turns into an ask.
	if r.tie != nil {
		shortlist := shortlist(cands, s)
		if pick, ok := r.tie.Break(ctx, req, shortlist); ok && pick.Session.ID != "" {
			d.Kind = KindContinue
			d.Session = pick.Session.ID
			d.Subject = pick.Session.Name()
			d.Runtime = pick.Session.Runtime
			d.Reason = ReasonTieBreak
			d.Because = "it was the closest of " + itoa(len(shortlist))
			return d, true
		}
	}

	d.Kind = KindAsk
	d.Reason = ReasonAmbiguous
	d.Question = AskLine(cands)
	d.Because = "two sessions match this about equally"
	return d, true
}

// shortlist is the candidates close enough to the top to be worth a model call.
func shortlist(cands []Candidate, s Scoring) []Candidate {
	if len(cands) == 0 {
		return nil
	}
	cut := cands[0].Score - s.Margin
	out := []Candidate{cands[0]}
	for _, c := range cands[1:] {
		if c.Score >= cut && c.Score > 0 {
			out = append(out, c)
		}
	}
	return out
}

// decay is exponential on a half-life. A session touched a minute ago is
// effectively current; one from yesterday contributes almost nothing, which is
// what stops the router resurrecting last week's context because it happened to
// mention the same repo.
func decay(since, halfLife time.Duration) float64 {
	if since < 0 {
		since = 0
	}
	if halfLife <= 0 {
		return 0
	}
	return math.Pow(0.5, since.Seconds()/halfLife.Seconds())
}

// workspaceMatch scores two directories. Exact is 1, one inside the other is
// 0.7, sharing a final path element is 0.5 — a monorepo checked out twice is
// still the same repo to the person talking about it.
func workspaceMatch(a, b string) float64 {
	a, b = strings.TrimRight(a, "/"), strings.TrimRight(b, "/")
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	if strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/") {
		return 0.7
	}
	if baseName(a) == baseName(b) {
		return 0.5
	}
	return 0
}

func fileTerms(v SessionView) []string {
	var out []string
	for _, f := range v.Files {
		out = append(out, tokens(baseName(f))...)
		out = append(out, tokens(f)...)
	}
	return out
}

func subjectTerms(v SessionView) []string {
	out := tokens(v.Subject)
	for _, e := range v.Entities {
		out = append(out, tokens(e)...)
	}
	if b := baseName(v.Workspace); b != "" {
		out = append(out, tokens(b)...)
	}
	return out
}

func round(f float64) float64 { return math.Round(f*1000) / 1000 }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
