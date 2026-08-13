package install

import (
	"context"
	"fmt"
)

// Verify, then offer to repair — OpenClaw's onboarding loop.
//
// Relay already made the harder half of this promise: every credential is
// tested with one real call before the installer exits, and the reason code is
// printed rather than swallowed. What it did with the answer was tell you and
// carry on, so an install could finish, print "Done", and hand over a box with
// two dead models on it. The user finds that out when they speak to it, which
// is the worst possible place to discover a bad key — the exact failure the
// probe exists to prevent.
//
// OpenClaw's setup.inference-verification.ts loops instead: verify, and on
// failure offer "fix it now" or "continue anyway", re-entering the step that
// produced the credential and testing again. It exits when the thing works or
// when the user says they know and want to move on.
//
// Three rules this keeps, and all three matter:
//
//  1. **Continuing is always offered.** A user who knows their key arrives
//     tomorrow should not be trapped in the installer. Refusing to finish would
//     be a worse installer, not a safer one.
//  2. **An unattended run never loops.** `relay setup --yes` has nobody to ask,
//     and asking a default-taking prompter to choose between "fix" and
//     "continue" would either spin or quietly re-answer the same way forever.
//     One attempt, then the warning, exactly as OpenClaw's nonInteractive path.
//  3. **The loop is bounded even when it is interactive.** Three goes is enough
//     for a typo and few enough that a wrong answer given three times ends the
//     step rather than the afternoon.
const maxRepairAttempts = 3

// repair describes one thing worth verifying and re-asking about.
type repair[T any] struct {
	// ID prefixes the question, so a scripted run answers it by name.
	ID string
	// Title is the heading on the repair question.
	Title string
	// Choose runs the step. It is called again for each repair attempt.
	Choose func() (T, error)
	// OK reports whether the outcome verified.
	OK func(T) bool
	// Trouble is the one line saying what went wrong, quoted from the probe.
	Trouble func(T) string
	// Facts describes the failure for the model that reads it out loud. Nil
	// means no diagnosis is offered for this step. See diagnose.go.
	Facts func(T) DiagnoseFacts
	// FixLabel and ContinueLabel name the two rows.
	FixLabel      string
	ContinueLabel string
	// Give up is what is said when the attempts run out.
	GiveUp string
}

// verify runs a step and, when it did not verify, offers to run it again.
func verify[T any](ctx context.Context, opts Options, r repair[T]) (T, error) {
	p := opts.Prompt
	var out T

	for attempt := 1; ; attempt++ {
		var err error
		out, err = r.Choose()
		if err != nil {
			return out, err
		}
		if r.OK(out) {
			return out, nil
		}

		// An unattended run has nobody to ask. It has already printed the
		// reason code and will carry the warning into the summary, which is the
		// whole of what it can honestly do.
		if !p.Interactive() {
			return out, nil
		}
		if attempt >= maxRepairAttempts {
			p.Say("\n  %s", wrapIndent(r.GiveUp, 2, 76))
			return out, nil
		}

		body := r.Trouble(out)
		if body != "" {
			body += "\n\n"
		}
		// Asked once per failure rather than once per attempt: the second time
		// round the user has already read it, and a fresh paragraph of the same
		// advice reads as the installer not listening.
		if attempt == 1 && r.Facts != nil {
			sayDiagnosis(p, diagnose(ctx, opts, r.Facts(out)))
		}
		body += "Fixing it now is usually faster than finding out later: the credential is " +
			"tested again the moment you finish, and nothing is saved until it is."

		what, err := p.Select(Question{
			ID:    r.ID,
			Title: r.Title,
			Body:  body,
			Choices: []Choice{
				{ID: "fix", Label: r.FixLabel, Recommended: true},
				{ID: "continue", Label: r.ContinueLabel, Last: true},
			},
			Default: "fix",
		})
		if err != nil {
			return out, err
		}
		if what == "continue" {
			return out, nil
		}
	}
}

// modelTrouble quotes what the probe actually said. ORCHESTRATOR.md §2: report
// what the provider said rather than a guess at what it meant.
func modelTrouble(role string, c ModelChoice) string {
	switch {
	case c.Model.Model == "":
		return fmt.Sprintf("The %s model was never configured, so there is nothing to test.", role)
	case !c.Probed:
		return fmt.Sprintf("The %s model could not be tested from this machine.", role)
	}
	s := fmt.Sprintf("The %s model did not answer: %s said %s",
		role, c.Vendor.Label, c.Probe.Reason)
	if c.Probe.Detail != "" {
		s += " — " + c.Probe.Detail
	}
	if advice := reasonAdvice(c.Probe.Reason); advice != "" {
		s += "\n\n" + advice
	}
	return s
}
