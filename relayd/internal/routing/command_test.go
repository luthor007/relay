package routing_test

import (
	"testing"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/routing"
)

// The escape hatch is the first of ORCHESTRATOR.md §4's three guardrails and
// the only one that is always correct. If this table regresses, the user's way
// out of a bad routing decision regresses with it.
func TestParseCommand(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		kind    routing.CommandKind
		ref     string
		runtime adapter.Runtime
		subject string
		rest    string
		approve bool
	}{
		// ---- the two phrases the doc names by example.
		{name: "new session", text: "New session.", kind: routing.CmdNewSession},
		{name: "talk to the refactor one", text: "Talk to the refactor one.",
			kind: routing.CmdSwitch, ref: "refactor"},
		{name: "undo that", text: "Undo that.", kind: routing.CmdUndo},

		// ---- new session, in the shapes people actually say.
		{name: "start a new session", text: "start a new session", kind: routing.CmdNewSession},
		{name: "fresh session", text: "Fresh session", kind: routing.CmdNewSession},
		{name: "new session on a repo", text: "new session on the api repo",
			kind: routing.CmdNewSession, subject: "api repo"},
		{name: "new session with an instruction", text: "new session, run the tests",
			kind: routing.CmdNewSession, rest: "run the tests"},
		{name: "new session names a runtime", text: "start a new codex session",
			kind: routing.CmdNewSession, runtime: adapter.Codex},
		{name: "new session names runtime and subject", text: "new claude code session on the api repo",
			kind: routing.CmdNewSession, runtime: adapter.ClaudeCode, subject: "api repo"},
		{name: "new session on a runtime is not a subject", text: "new session on codex",
			kind: routing.CmdNewSession, runtime: adapter.Codex},

		// ---- switching.
		{name: "switch to", text: "switch to payments", kind: routing.CmdSwitch, ref: "payments"},
		{name: "go back to", text: "go back to the payments refactor",
			kind: routing.CmdSwitch, ref: "payments refactor"},
		{name: "in the X session", text: "in the payments session",
			kind: routing.CmdSwitch, ref: "payments"},
		{name: "tell the X one to Y", text: "tell the refactor one to stop",
			kind: routing.CmdSwitch, ref: "refactor", rest: "stop"},
		{name: "switch with an instruction", text: "switch to payments, run the tests",
			kind: routing.CmdSwitch, ref: "payments", rest: "run the tests"},

		// ---- undo, including the form that names a destination. "Put that in
		// the api docs" is a move, not a change of subject: reading it as a
		// switch would leave the mis-routed turn where it landed.
		{name: "wrong session", text: "wrong session", kind: routing.CmdUndo},
		{name: "put that in", text: "put that in the api docs",
			kind: routing.CmdUndo, ref: "api docs"},
		{name: "move that to", text: "move that to the payments refactor",
			kind: routing.CmdUndo, ref: "payments refactor"},

		// ---- stop, status, list.
		{name: "stop", text: "Stop.", kind: routing.CmdStop},
		{name: "cancel that", text: "cancel that", kind: routing.CmdStop},
		{name: "what is it doing", text: "What is it doing?", kind: routing.CmdStatus},
		{name: "is it done", text: "is it done", kind: routing.CmdStatus},
		{name: "how long", text: "how long?", kind: routing.CmdStatus},
		{name: "whats running", text: "what's running", kind: routing.CmdList},

		// ---- answers to a blocked session.
		{name: "yes", text: "yes", kind: routing.CmdAnswer, approve: true},
		{name: "go ahead", text: "Go ahead.", kind: routing.CmdAnswer, approve: true},
		{name: "no", text: "no", kind: routing.CmdAnswer, approve: false},
		{name: "deny", text: "deny", kind: routing.CmdAnswer, approve: false},

		// ---- everything else is not a command, which is most utterances.
		{name: "ordinary work", text: "run the tests on the payments branch"},
		{name: "a question", text: "why is the build failing"},
		{name: "yes inside a sentence", text: "yes that is the file I meant to change"},
		{name: "empty", text: ""},
		{name: "new in a sentence", text: "add a new endpoint to the api"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := routing.ParseCommand(tc.text)
			if got.Kind != tc.kind {
				t.Fatalf("kind = %q, want %q (phrase %q)", got.Kind, tc.kind, got.Phrase)
			}
			if got.Ref != tc.ref {
				t.Errorf("ref = %q, want %q", got.Ref, tc.ref)
			}
			if got.Runtime != tc.runtime {
				t.Errorf("runtime = %q, want %q", got.Runtime, tc.runtime)
			}
			if got.Subject != tc.subject {
				t.Errorf("subject = %q, want %q", got.Subject, tc.subject)
			}
			if got.Rest != tc.rest {
				t.Errorf("rest = %q, want %q", got.Rest, tc.rest)
			}
			if tc.kind == routing.CmdAnswer && got.Approve != tc.approve {
				t.Errorf("approve = %v, want %v", got.Approve, tc.approve)
			}
		})
	}
}

func TestParseRuntime(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want adapter.Runtime
	}{
		{"codex", adapter.Codex},
		{"Claude Code", adapter.ClaudeCode},
		{"claude", adapter.ClaudeCode},
		{"open claw", adapter.OpenClaw},
		{"OpenCode", adapter.OpenCode},
		{"hermes", adapter.Hermes},
		{"gemini", ""},
		{"", ""},
	} {
		if got := routing.ParseRuntime(tc.in); got != tc.want {
			t.Errorf("ParseRuntime(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
