package install

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The verify/repair loop.
//
// The installer already tested every credential with one real call. What it did
// with a failure was print it and carry on, so a run could end with "Done" over
// a box with two dead models on it. These tests are about the half that was
// missing: the offer to fix it now, and the three ways that offer has to stop.

// probeCount is a step that fails a stated number of times and then works.
type probeCount struct {
	fails int
	runs  int
}

func (p *probeCount) step() (bool, error) {
	p.runs++
	return p.runs > p.fails, nil
}

func repairOf(p *probeCount) repair[bool] {
	return repair[bool]{
		ID:            "thing.repair",
		Title:         "Not working yet",
		Choose:        p.step,
		OK:            func(ok bool) bool { return ok },
		Trouble:       func(bool) string { return "it did not answer" },
		FixLabel:      "Choose again",
		ContinueLabel: "Leave it for now",
		GiveUp:        "Leaving it as it is.",
	}
}

// The point of the whole thing: choosing "fix" runs the step again, and the
// loop ends when the credential works rather than when the user gives up.
func TestRepairRerunsTheStepUntilItVerifies(t *testing.T) {
	p := &probeCount{fails: 1}
	script := NewScript(map[string]string{"thing.repair": "fix"})

	ok, err := verify(context.Background(), Options{Prompt: script}, repairOf(p))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("the second attempt worked and the loop reported failure")
	}
	if p.runs != 2 {
		t.Errorf("ran the step %d times, want one failure and one repair", p.runs)
	}
	if n := strings.Count(strings.Join(script.Asked, "\n"), "thing.repair"); n != 1 {
		t.Errorf("asked to repair %d times, want once", n)
	}
}

// Continuing is always offered, and it is honoured immediately. A user whose
// key arrives tomorrow must not be trapped in the installer — refusing to
// finish would be a worse installer, not a safer one.
func TestRepairAlwaysOffersToContinueAndStopsWhenTaken(t *testing.T) {
	p := &probeCount{fails: 99}
	script := NewScript(map[string]string{"thing.repair": "continue"})

	ok, err := verify(context.Background(), Options{Prompt: script}, repairOf(p))
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("the step never worked and the loop said it did")
	}
	if p.runs != 1 {
		t.Errorf("ran the step %d times after being told to continue, want once", p.runs)
	}
	if !strings.Contains(script.Output(), "Leave it for now") {
		t.Error("the continue row was never shown, so the loop is a trap")
	}
	if !strings.Contains(script.Output(), "it did not answer") {
		t.Error("the repair question has to quote what actually went wrong")
	}
}

// A wrong answer given three times ends the step, not the afternoon.
func TestRepairIsBounded(t *testing.T) {
	p := &probeCount{fails: 99}
	script := NewScript(map[string]string{"thing.repair": "fix"})

	if _, err := verify(context.Background(), Options{Prompt: script}, repairOf(p)); err != nil {
		t.Fatal(err)
	}
	if p.runs != maxRepairAttempts {
		t.Errorf("ran the step %d times, want the cap of %d", p.runs, maxRepairAttempts)
	}
	if !strings.Contains(script.Output(), "Leaving it as it is.") {
		t.Error("giving up has to say so, and say what to run later")
	}
}

// `relay setup --yes` has nobody to ask. Asking a prompter that takes every
// default to choose between "fix" and "continue" is either a spin or the same
// wrong answer three times, and both are worse than the warning.
func TestAnUnattendedRunNeverLoops(t *testing.T) {
	p := &probeCount{fails: 99}
	auto := &Auto{}

	if _, err := verify(context.Background(), Options{Prompt: auto}, repairOf(p)); err != nil {
		t.Fatal(err)
	}
	if p.runs != 1 {
		t.Errorf("an unattended run ran the step %d times, want once", p.runs)
	}
	for _, line := range auto.Log {
		if strings.Contains(line, "Not working yet") {
			t.Error("an unattended run asked a question it cannot answer")
		}
	}
}

// An error from the step is a broken installer, not a broken credential, and
// re-running it would bury the reason under three more copies of itself.
func TestRepairDoesNotRetryARealError(t *testing.T) {
	boom := errors.New("the filesystem is gone")
	runs := 0
	script := NewScript(map[string]string{"thing.repair": "fix"})

	_, err := verify(context.Background(), Options{Prompt: script}, repair[bool]{
		ID:      "thing.repair",
		Choose:  func() (bool, error) { runs++; return false, boom },
		OK:      func(ok bool) bool { return ok },
		Trouble: func(bool) string { return "" },
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the step's own error", err)
	}
	if runs != 1 {
		t.Errorf("ran the step %d times, want no retry of a hard failure", runs)
	}
}
