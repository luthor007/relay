package routing_test

import (
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/routing"
)

// The announcements the docs write out by example, and the shapes around them.
func TestAnnounce(t *testing.T) {
	tests := []struct {
		name     string
		decision routing.Decision
		want     string
	}{
		{
			// ORCHESTRATOR.md §4 rule 2, verbatim.
			name: "adding that to the payments refactor",
			decision: routing.Decision{
				Kind: routing.KindContinue, Subject: "the payments refactor", Reason: routing.ReasonFocus,
			},
			want: "Adding that to the payments refactor.",
		},
		{
			// MEMORY.md §8's guardrail example.
			name: "starting a new codex session on the api repo",
			decision: routing.Decision{
				Kind: routing.KindNew, Runtime: adapter.Codex, Subject: "the api repo",
				Reason: routing.ReasonNothingLive,
			},
			want: "Starting a new Codex session for the api repo.",
		},
		{
			name: "a new session named by its workspace",
			decision: routing.Decision{
				Kind: routing.KindNew, Runtime: adapter.ClaudeCode, Workspace: "/home/me/src/api",
				Reason: routing.ReasonNothingLive,
			},
			want: "Starting a new Claude Code session on api.",
		},
		{
			name: "switching on request",
			decision: routing.Decision{
				Kind: routing.KindContinue, Subject: "the api docs", Reason: routing.ReasonExplicit,
			},
			want: "Switching to the api docs.",
		},
		{
			name: "the only one running says so",
			decision: routing.Decision{
				Kind: routing.KindContinue, Subject: "the payments refactor", Reason: routing.ReasonOnlyLive,
			},
			want: "Adding that to the payments refactor, the only one running.",
		},
		{
			name: "an ask speaks its question",
			decision: routing.Decision{
				Kind: routing.KindAsk, Question: "Which one — payments, or the api docs?",
			},
			want: "Which one — payments, or the api docs?",
		},
		{
			name: "stopping something named",
			decision: routing.Decision{
				Kind: routing.KindControl, Subject: "the payments refactor",
				Command: &routing.Command{Kind: routing.CmdStop},
			},
			want: "Stopping the payments refactor.",
		},
		{
			name: "undo",
			decision: routing.Decision{
				Kind: routing.KindControl, Command: &routing.Command{Kind: routing.CmdUndo},
			},
			want: "Taking that back.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := routing.Announce(tc.decision); got != tc.want {
				t.Errorf("Announce = %q, want %q", got, tc.want)
			}
		})
	}
}

// A session with no subject is named by runtime and id, not by an invented
// topic — the same rule the pinger uses.
func TestAnnounceNeverInventsASubject(t *testing.T) {
	d := routing.Decision{Kind: routing.KindNew, Runtime: adapter.Hermes, Reason: routing.ReasonNothingLive}
	got := routing.Announce(d)
	if got != "Starting a new Hermes session." {
		t.Errorf("Announce = %q", got)
	}

	v := routing.SessionView{ID: "3f2a91c0-dead", Runtime: adapter.Codex}
	if name := v.Name(); !strings.HasPrefix(name, "Codex session 3f2a91") {
		t.Errorf("Name = %q; a subject-less session is named by id", name)
	}
}

// The announcement is deterministic. It is the audit trail for the routing
// decision, and an audit trail that reads differently each time is not one.
func TestAnnounceIsDeterministic(t *testing.T) {
	d := routing.Decision{Kind: routing.KindContinue, Subject: "the payments refactor", Reason: routing.ReasonFocus}
	first := routing.Announce(d)
	for i := 0; i < 50; i++ {
		if got := routing.Announce(d); got != first {
			t.Fatalf("run %d said %q, first said %q", i, got, first)
		}
	}
}

func TestAskLineNamesTheCandidates(t *testing.T) {
	cs := []routing.Candidate{
		{Session: routing.SessionView{ID: "a", Subject: "payments"}},
		{Session: routing.SessionView{ID: "b", Subject: "the api docs"}},
		{Session: routing.SessionView{ID: "c", Subject: "the migration"}},
	}
	line := routing.AskLine(cs)
	if !strings.Contains(line, "payments") || !strings.Contains(line, "api docs") {
		t.Errorf("AskLine = %q; the first two have to be named out loud", line)
	}
	if strings.Contains(line, "migration") {
		t.Errorf("AskLine = %q; two names is the useful maximum in someone's ear", line)
	}
	if got := routing.AskLine(nil); !strings.Contains(got, "new one") {
		t.Errorf("AskLine(nil) = %q; with nothing running the offer is a new session", got)
	}
}
