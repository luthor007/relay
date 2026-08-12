package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/facts"
)

// factNotebook is [Notebook] over MEMORY.md §5's durable facts.
//
// It is the "adapting to its master" half, and the reason it is a thin adapter
// rather than a store of its own is that §5's five rules are already enforced
// in internal/facts: every fact carries evidence, facts decay on last
// observation, contradictions supersede rather than accumulate, everything is
// editable, and nothing here is a secret. A second write path would be a second
// place for one of those five to stop being true.
type factNotebook struct {
	store *facts.Store
	// runtime names us as the source of the evidence. "relay" rather than one
	// of the five, because this is something the user said to the orchestrator
	// and not something read out of an agent transcript.
	runtime string
	now     func() time.Time
}

// NotebookIn returns the write half of memory for a fact store.
func NotebookIn(s *facts.Store) Notebook {
	if s == nil {
		return nil
	}
	return &factNotebook{store: s, runtime: "relay", now: time.Now}
}

// Remember records one thing worth knowing next time.
//
// The subtle part is the evidence. facts.Observation refuses an observation
// with none — a fact that cannot point at where it came from is deleted rather
// than kept at low confidence — and "the user told me" is a real provenance,
// so the evidence is the utterance itself with the session it was said in. A
// month later the console can show the sentence next to the fact, which is the
// difference between a fact the user can check and one they can only trust.
func (n *factNotebook) Remember(ctx context.Context, f Fact) error {
	text := strings.TrimSpace(f.Text)
	if text == "" {
		return fmt.Errorf("orchestrator: a fact needs text")
	}
	at := f.At
	if at.IsZero() {
		at = n.now()
	}
	subject := strings.TrimSpace(f.Subject)
	if subject == "" {
		subject = facts.DefaultSubject
	}
	session := strings.TrimSpace(f.Session)
	if session == "" {
		// Said to Relay outside any agent session — through the console, or in
		// a turn that never got one. The provenance is still real and the
		// evidence rule is absolute, so it is named rather than left empty:
		// runtime "relay", session "spoken" reads correctly on the facts screen
		// and does not pretend to point at a transcript that can be reopened.
		session = "spoken"
	}

	res, err := n.store.Reconcile(ctx, []facts.Observation{{
		Subject:   subject,
		Predicate: predicateFor(text),
		Object:    objectFor(text),
		Text:      text,
		// Said directly rather than inferred from a transcript, so this is the
		// high-confidence path — but not 1.0. The user can be wrong, can change
		// their mind, and §5's decay applies to this the same as to anything
		// else: a preference stated once and never seen again should fade.
		Confidence: 0.9,
		Evidence: []facts.Evidence{{
			Runtime:   n.runtime,
			SessionID: session,
			Quote:     text,
			At:        at,
		}},
	}})
	if err != nil {
		return err
	}
	// Reconcile skips rather than errors, so a silent success here would report
	// "remembered" for something that was dropped. The two skip lists mean
	// different things and only one of them is a failure.
	if len(res.Rejected) > 0 {
		// Refused by the tier: no evidence, an unknown predicate, an empty
		// sentence — or a credential in the text, which is the one worth
		// noticing. MEMORY.md §5's last rule is that nothing here is a secret,
		// and the fact store enforces it rather than trusting the caller.
		return fmt.Errorf("orchestrator: %s", res.Rejected[0].Reason)
	}
	if len(res.Suppressed) > 0 {
		// Matched a fact the user deleted. Not an error — the user's deletion
		// is the more recent decision and it stands — but the model has to be
		// told, or it will report success and say so out loud.
		return &ErrSuppressed{Reason: res.Suppressed[0].Reason}
	}
	return nil
}

// ErrSuppressed means the fact matched something the user had removed.
//
// It is a distinct type because the right response differs: a rejection is
// something to fix and retry, a suppression is something to accept and not
// mention again.
type ErrSuppressed struct{ Reason string }

func (e *ErrSuppressed) Error() string {
	if e.Reason == "" {
		return "you removed this before, so it was not re-added"
	}
	return "you removed this before, so it was not re-added: " + e.Reason
}

// predicateFor picks one of §5's four predicates from the sentence.
//
// It is deliberately a small keyword pass and not a model call. The predicate
// is a filing decision — which shelf — and getting it wrong costs a fact that is
// harder to search, while a model call here would put a network round trip and
// a failure mode inside a tool whose whole job is to be cheap enough to use
// often. [Uses] is the default because it is the multi-valued one: filing two
// unrelated things under it is untidy, filing them under a single-valued
// predicate would make the second supersede the first.
func predicateFor(text string) facts.Predicate {
	t := strings.ToLower(text)
	switch {
	case containsAny(t, "deploy", "hosted on", "runs on", "ships to"):
		return facts.DeploysOn
	case containsAny(t, "prefer", "likes", "always ", "never ", "rather than", "instead of"):
		return facts.Prefers
	case containsAny(t, "writes ", "written in", "codebase is", "language"):
		return facts.Writes
	default:
		return facts.Uses
	}
}

// objectFor is the identity half of a fact: two observations about the same
// object are the same fact, which is what makes supersession work.
//
// The sentence itself is the object when nothing better presents itself. That
// is coarse — "deploys on Vercel" and "deploys on Fly" become two facts rather
// than one superseding the other — and the alternative is worse: a wrong
// merge silently rewrites something the user said. §5 is explicit that
// supersession happens only when something *names* the old fact, so being
// conservative here is the rule rather than a shortcut.
func objectFor(text string) string {
	t := strings.TrimSpace(text)
	if len(t) > 120 {
		t = t[:120]
	}
	return t
}

func containsAny(s string, subs ...string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}
