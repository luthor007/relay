package install

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/detect"
)

// Ctrl-C, and what the installer did with it.
//
// From the first clean-machine run: somebody interrupted at the "Install Claude
// Code?" prompt, and the installer asked three more install questions, took
// three more yeses, failed all three instantly, and walked on to the voice menu.
// The context was cancelled the whole time and nothing looked.
//
//	Install Claude Code? [y/N] > ^Cy
//	  Claude Code did not install: context canceled
//	Install Codex? [y/N] > y
//	  Codex did not install: context canceled
//
// Two separate faults there and both are fixed below: the run continued, and it
// blamed npm for the user's own interrupt.

// A cancelled context ends the run rather than turning every remaining step
// into an instant failure.
func TestAnInterruptStopsTheRunRatherThanPoisoningIt(t *testing.T) {
	answers := baseAnswers()
	opts, script, _ := newOpts(t, answers, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Run(ctx, opts)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation to end the run", err)
	}
	// And it says so in words, rather than leaving the user to infer it from a
	// missing summary.
	if !strings.Contains(script.Output(), "Stopped.") {
		t.Errorf("the run ended silently:\n%s", script.Output())
	}
	// Nothing past the first check ran: no voice question, no models.
	for _, id := range script.Asked {
		if strings.HasPrefix(id, "voice") || strings.HasPrefix(id, "models") {
			t.Errorf("asked %q after the run was cancelled", id)
		}
	}
}

// The runtime loop asks one question per runtime, so the guard has to sit
// between rows: an interrupt at the first row must not be answered by asking the
// next three, each failing instantly.
func TestTheRuntimeLoopStopsBetweenRowsWhenCancelled(t *testing.T) {
	opts, script, _ := newOpts(t, baseAnswers(), nil)
	opts = opts.withDefaults()
	rep := detect.Detect(context.Background(), opts.Env, opts.Detect)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := offerRuntimes(ctx, opts, rep)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the loop to stop rather than ask", err)
	}
	for _, id := range script.Asked {
		if strings.HasPrefix(id, "install.") {
			t.Errorf("asked %q with the context already cancelled", id)
		}
	}
	// The give-away that the old code was wrong: it blamed the package manager
	// for the user's own Ctrl-C.
	if strings.Contains(script.Output(), "did not install: context canceled") {
		t.Error("reported an interrupt as a failed install")
	}
}
