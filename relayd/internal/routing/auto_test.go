package routing_test

import (
	"context"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/routing"
)

// The single most important property of the automatic router: it is off unless
// someone turns it on. ORCHESTRATOR.md §4 ships the manual path first, and a
// scorer that quietly became the default would be exactly the silent 80% router
// the doc argues against.
func TestAutomaticRoutingIsOffByDefault(t *testing.T) {
	ctx := context.Background()
	live := routing.StaticSessions{
		session("s1", "the payments refactor", "/repo/payments", adapter.ClaudeCode, time.Minute),
		session("s2", "the api docs", "/repo/api", adapter.Codex, 3*time.Hour),
	}
	r := router(t, routing.Options{Sessions: live})
	if r.Auto() {
		t.Fatal("Options.Auto defaults to true")
	}

	// An utterance that the scorer would happily route lands on an ask instead.
	d, _ := r.Route(ctx, routing.Request{Text: "the payments refactor needs a test"})
	if d.Kind != routing.KindAsk {
		t.Fatalf("got %s/%s; with the scorer off and no focus this asks", d.Kind, d.Session)
	}
	if d.Automatic {
		t.Error("a manual decision must not be marked automatic")
	}
}

func TestScorerUsesRecencyRepoAndSubject(t *testing.T) {
	payments := session("s1", "the payments refactor", "/repo/payments", adapter.ClaudeCode, time.Minute)
	payments.Files = []string{"/repo/payments/charge.go"}
	docs := session("s2", "the api docs", "/repo/api", adapter.Codex, 6*time.Hour)

	tests := []struct {
		name string
		req  routing.Request
		want string
	}{
		{"subject match", routing.Request{Text: "add a test to the payments refactor", At: now()}, "s1"},
		{"repo match", routing.Request{Text: "run it", Workspace: "/repo/api", At: now()}, "s2"},
		{"file match", routing.Request{Text: "look at charge.go again", At: now()}, "s1"},
		{"recency alone", routing.Request{Text: "carry on", At: now()}, "s1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := routing.Score(tc.req, []routing.SessionView{payments, docs}, routing.Scoring{})
			if got[0].Session.ID != tc.want {
				t.Fatalf("top = %s (%.3f) then %s (%.3f), want %s",
					got[0].Session.ID, got[0].Score, got[1].Session.ID, got[1].Score, tc.want)
			}
			if got[0].Why == "" {
				t.Error("a candidate has to carry why it scored, for the ask")
			}
		})
	}
}

func TestAutomaticContinuesAStrongMatch(t *testing.T) {
	ctx := context.Background()
	payments := session("s1", "the payments refactor", "/repo/payments", adapter.ClaudeCode, time.Minute)
	docs := session("s2", "the api docs", "/repo/api", adapter.Codex, 8*time.Hour)
	r := router(t, routing.Options{
		Sessions: routing.StaticSessions{payments, docs},
		Auto:     true,
	})

	d, _ := r.Route(ctx, routing.Request{
		Text: "the payments refactor still needs a test", Workspace: "/repo/payments", At: now(),
	})
	if d.Kind != routing.KindContinue || d.Session != "s1" {
		t.Fatalf("got %s/%s (%.3f), want continue/s1", d.Kind, d.Session, d.Confidence)
	}
	if !d.Automatic {
		t.Error("an automatic decision has to say it was automatic, so undo is offered")
	}
	if d.Reason != routing.ReasonAutomatic {
		t.Errorf("reason = %q", d.Reason)
	}
}

// A new topic is a new session. A wrong new costs a session; a wrong continue
// costs someone else's context, and only one of those is recoverable.
func TestAutomaticStartsANewSessionForAnUnrelatedTopic(t *testing.T) {
	ctx := context.Background()
	stale := session("s1", "the payments refactor", "/repo/payments", adapter.ClaudeCode, 30*24*time.Hour)
	rr := runtimeRouter(t, routing.Entitlements{routing.APIKeysOnly}, nil, used(adapter.Codex))
	r := router(t, routing.Options{
		Sessions: routing.StaticSessions{stale},
		Runtime:  rr,
		Auto:     true,
	})

	d, _ := r.Route(ctx, routing.Request{Text: "what is the weather doing to my solar panels", At: now()})
	if d.Kind != routing.KindNew {
		t.Fatalf("got %s/%s (%.3f), want a new session", d.Kind, d.Session, d.Confidence)
	}
	if d.Reason != routing.ReasonNewTopic {
		t.Errorf("reason = %q", d.Reason)
	}
}

// Two sessions that match about equally is a tie, and a tie with no tie-breaker
// is a question. Never a coin toss.
func TestAutomaticTieAsksWithoutATieBreaker(t *testing.T) {
	ctx := context.Background()
	a := session("s1", "payments refactor", "/repo/payments", adapter.ClaudeCode, time.Minute)
	b := session("s2", "payments migration", "/repo/payments", adapter.Codex, time.Minute)
	r := router(t, routing.Options{Sessions: routing.StaticSessions{a, b}, Auto: true})

	d, _ := r.Route(ctx, routing.Request{Text: "the payments work needs a test", Workspace: "/repo/payments", At: now()})
	if d.Kind != routing.KindAsk {
		t.Fatalf("got %s/%s, want an ask", d.Kind, d.Session)
	}
}

func TestAutomaticTieBreakIsConsultedAndMayDecline(t *testing.T) {
	ctx := context.Background()
	a := session("s1", "payments refactor", "/repo/payments", adapter.ClaudeCode, time.Minute)
	b := session("s2", "payments migration", "/repo/payments", adapter.Codex, time.Minute)

	var sawCandidates int
	r := router(t, routing.Options{
		Sessions: routing.StaticSessions{a, b},
		Auto:     true,
		TieBreak: routing.TieBreakFunc(func(_ context.Context, _ routing.Request, cs []routing.Candidate) (routing.Candidate, bool) {
			sawCandidates = len(cs)
			return cs[1], true
		}),
	})
	d, _ := r.Route(ctx, routing.Request{Text: "the payments work needs a test", Workspace: "/repo/payments", At: now()})
	if d.Kind != routing.KindContinue || d.Session != "s2" {
		t.Fatalf("got %s/%s, want the tie-break's pick", d.Kind, d.Session)
	}
	if d.Reason != routing.ReasonTieBreak {
		t.Errorf("reason = %q; the console has to be able to show that a model decided", d.Reason)
	}
	if sawCandidates < 2 {
		t.Errorf("the tie-break saw %d candidates", sawCandidates)
	}

	// A tie-breaker that is unsure must be able to say so, and that turns back
	// into an ask rather than into a guess.
	r2 := router(t, routing.Options{
		Sessions: routing.StaticSessions{a, b},
		Auto:     true,
		TieBreak: routing.TieBreakFunc(func(context.Context, routing.Request, []routing.Candidate) (routing.Candidate, bool) {
			return routing.Candidate{}, false
		}),
	})
	d2, _ := r2.Route(ctx, routing.Request{Text: "the payments work needs a test", Workspace: "/repo/payments", At: now()})
	if d2.Kind != routing.KindAsk {
		t.Fatalf("got %s; an unsure tie-break must fall back to asking", d2.Kind)
	}
}

// Even with the scorer on, the escape hatch outranks it.
func TestExplicitCommandsBeatTheScorer(t *testing.T) {
	ctx := context.Background()
	a := session("s1", "payments refactor", "/repo/payments", adapter.ClaudeCode, time.Minute)
	b := session("s2", "the api docs", "/repo/api", adapter.Codex, 5*time.Hour)
	r := router(t, routing.Options{Sessions: routing.StaticSessions{a, b}, Auto: true})

	d, _ := r.Route(ctx, routing.Request{Text: "talk to the api one", At: now()})
	if d.Session != "s2" || d.Reason != routing.ReasonExplicit {
		t.Fatalf("got %s via %q, want s2 via the escape hatch", d.Session, d.Reason)
	}
}

func TestScoringWeightsAreConfigurable(t *testing.T) {
	// A caller that decides recency should dominate can say so without a
	// rebuild, which is the point of exporting the weights at all.
	fresh := session("s1", "unrelated", "/repo/other", adapter.ClaudeCode, time.Minute)
	stale := session("s2", "the payments refactor", "/repo/payments", adapter.Codex, 4*time.Hour)
	req := routing.Request{Text: "the payments refactor needs a test", At: now()}

	byDefault := routing.Score(req, []routing.SessionView{fresh, stale}, routing.Scoring{})
	if byDefault[0].Session.ID != "s2" {
		t.Fatalf("default weights put %s first; subject match should win here", byDefault[0].Session.ID)
	}

	recencyHeavy := routing.Score(req, []routing.SessionView{fresh, stale}, routing.Scoring{
		Recency: 1, Entities: 0.01, Workspace: 0.01, Files: 0.01, HalfLife: time.Hour,
	})
	if recencyHeavy[0].Session.ID != "s1" {
		t.Fatalf("recency-heavy weights put %s first", recencyHeavy[0].Session.ID)
	}
}

// A new topic with nowhere to start it is a question, not a fallback into the
// focus. Falling back would produce exactly the wrong continue the scorer had
// just declined to make.
func TestAutomaticNewTopicWithNoRuntimeAsks(t *testing.T) {
	ctx := context.Background()
	stale := session("s1", "the payments refactor", "/repo/payments", adapter.ClaudeCode, 30*24*time.Hour)
	// Everything installed here has never been run, so nothing is routable.
	rr := runtimeRouter(t, routing.Entitlements{routing.APIKeysOnly}, nil,
		neverRun(adapter.Codex), neverRun(adapter.OpenCode))
	r := router(t, routing.Options{
		Sessions: routing.StaticSessions{stale},
		Runtime:  rr,
		Auto:     true,
	})
	r.SetFocus("s1")

	d, _ := r.Route(ctx, routing.Request{Text: "what is the weather doing to my solar panels", At: now()})
	if d.Kind != routing.KindAsk {
		t.Fatalf("got %s/%s, want an ask", d.Kind, d.Session)
	}
	if d.Question == "" {
		t.Error("the ask has to carry a question")
	}
}
