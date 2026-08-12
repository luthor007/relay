package routing_test

import (
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/routing"
)

// ORCHESTRATOR.md §3b's allowlist, row by row. The asymmetry is the point:
// escalating unnecessarily costs a few cents, self-answering wrongly costs
// trust, so every ambiguous row in this table is expected to escalate.
func TestClassify(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		class routing.Class
	}{
		// ---- status.
		{"what is it doing", "What is it doing?", routing.ClassStatus},
		{"is it done", "is it done", routing.ClassStatus},
		{"how long", "how long?", routing.ClassStatus},
		{"whats running", "what's running", routing.ClassControl},
		{"any progress", "any progress", routing.ClassStatus},

		// ---- control. The closed grammar, and the four the doc names.
		{"stop", "Stop.", routing.ClassControl},
		{"new session", "new session", routing.ClassControl},
		{"talk to the refactor one", "talk to the refactor one", routing.ClassControl},
		{"undo that", "undo that", routing.ClassControl},
		{"yes", "yes", routing.ClassControl},

		// ---- memory lookups: a read with no reasoning on top.
		{"what did i decide", "what did i decide about lunch", routing.ClassMemory},
		{"remind me what", "remind me what her name was", routing.ClassMemory},
		{"when did i last", "when did i last talk to sam", routing.ClassMemory},

		// ---- everything else. Anything touching a repo, a tool or a decision.
		{"work", "run the tests on the payments branch", routing.ClassEscalate},
		{"a refactor", "refactor the auth module", routing.ClassEscalate},
		{"a question with judgement in it", "should i use postgres or sqlite", routing.ClassEscalate},
		{"an explanation", "why is the build failing", routing.ClassEscalate},
		{"a tool call", "send that to sam by email", routing.ClassEscalate},
		{"nothing", "", routing.ClassEscalate},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := routing.Classify(tc.text)
			if v.Class != tc.class {
				t.Fatalf("class = %q, want %q (rule %q, because %q)", v.Class, tc.class, v.Rule, v.Because)
			}
			wantEscalate := tc.class == routing.ClassEscalate
			if v.Escalate != wantEscalate {
				t.Errorf("escalate = %v, want %v", v.Escalate, wantEscalate)
			}
		})
	}
}

// The acknowledgement row is not a classification of the utterance. It is a
// property of every utterance: the small model speaks first, always, which is
// what stops eight seconds of silence reading as broken.
func TestEveryUtteranceIsAcknowledged(t *testing.T) {
	for _, text := range []string{
		"stop", "what is it doing", "refactor the auth module",
		"what did i decide about lunch", "", "deploy to production",
	} {
		if v := routing.Classify(text); !v.Ack {
			t.Errorf("Classify(%q).Ack = false; the immediate \"on it\" is unconditional", text)
		}
	}
}

// The veto is the half of this file that keeps the allowlist honest. A status
// question that names a repo, a tool or a decision escalates even though the
// row matched — under-escalation is the failure mode that costs trust.
func TestVetoBeatsAMatchingAllowlistRow(t *testing.T) {
	tests := []struct {
		text string
		veto string
	}{
		{"how long has the refactor been running", "refactor"},
		{"how much longer until the deploy finishes", "deploy"},
		{"what did i decide about the payments repo", "repo"},
		{"what did we decide about the database migration", "database"},
		{"what did i say about the api key", "key"},
	}
	for _, tc := range tests {
		t.Run(tc.text, func(t *testing.T) {
			v := routing.Classify(tc.text)
			if !v.Escalate {
				t.Fatalf("class = %q, rule %q; this should have escalated", v.Class, v.Rule)
			}
			if v.Veto != tc.veto {
				t.Errorf("veto = %q, want %q", v.Veto, tc.veto)
			}
			if v.Rule == "" {
				t.Error("the matching row should still be named, so the log says which row was overridden")
			}
		})
	}
}

// Control is exempt from the veto, and it can be because the grammar is closed
// and anchored: "stop" cannot grow into "stop and deploy" without ceasing to
// match at all.
func TestControlIsNotVetoed(t *testing.T) {
	for _, text := range []string{"stop", "new session", "undo that", "talk to the refactor one"} {
		v := routing.Classify(text)
		if v.Class != routing.ClassControl || v.Escalate {
			t.Errorf("Classify(%q) = %q escalate=%v; control is the closed grammar and must not escalate",
				text, v.Class, v.Escalate)
		}
	}
	// And the longer sentence simply is not a control verb.
	if v := routing.Classify("stop and deploy to production"); !v.Escalate {
		t.Errorf("a control verb inside a longer instruction must escalate, got %q", v.Class)
	}
}

// The allowlist starts almost empty. This is a guard on scope creep: growing it
// is a product decision, and a row added without a test that argues for it
// should fail here first.
func TestAllowlistIsSmall(t *testing.T) {
	const max = 8
	if n := len(routing.Allowlist); n > max {
		t.Errorf("the allowlist has %d rows; ORCHESTRATOR.md §3b says it starts almost empty (max %d here)", n, max)
	}
	for _, row := range routing.Allowlist {
		if row.Class == routing.ClassEscalate {
			t.Errorf("row %q is on the allowlist with class escalate, which is a contradiction", row.Name)
		}
	}
}

// Escalates is the boolean form and must agree with the verdict.
func TestEscalatesAgreesWithClassify(t *testing.T) {
	for _, text := range []string{"stop", "is it done", "refactor the auth module", "deploy"} {
		if routing.Escalates(text) != routing.Classify(text).Escalate {
			t.Errorf("Escalates and Classify disagree on %q", text)
		}
	}
}

func TestVetoListNamesRepoToolAndDecision(t *testing.T) {
	// A cheap structural check that the three categories the doc names are all
	// actually represented, so a future edit cannot quietly drop one.
	want := map[string]string{"repo": "repo", "deploy": "tool", "should": "decision"}
	have := strings.Join(routing.Veto, " ")
	for w, kind := range want {
		if !strings.Contains(have, w) {
			t.Errorf("the veto list has no %s marker (%q missing)", kind, w)
		}
	}
}
