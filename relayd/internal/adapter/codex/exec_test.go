package codex

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/logx"
)

// fixtureRunner plays a recorded NDJSON stream instead of spawning a binary
// that does not exist in this container.
func fixtureRunner(t *testing.T, path string) execRunner {
	t.Helper()
	return func(ctx context.Context, dir string, argv, env []string, stdout io.Writer) error {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range strings.SplitAfter(string(b), "\n") {
			if line == "" {
				continue
			}
			if _, err := io.WriteString(stdout, line); err != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
		return nil
	}
}

// TestExecFallbackSaysWhatItCannotDo. The whole value of the fallback is that
// it degrades visibly: ADAPTERS.md §1 keeps it for the day app-server changes
// shape, and an orchestrator that cannot see what it lost will steer into the
// void and think it worked.
func TestExecFallbackSaysWhatItCannotDo(t *testing.T) {
	a := NewExec(ExecOptions{Log: logx.Discard()})
	c := a.Capabilities()

	if c.Has(adapter.CapSteer) {
		t.Error("codex exec --json is one-shot; steering is structurally impossible")
	}
	if c.Get(adapter.CapSteer) != adapter.SupportNo {
		t.Errorf("steer = %s, want a definite no", c.Get(adapter.CapSteer))
	}
	if c.Get(adapter.CapNeedsInput) != adapter.SupportNo {
		t.Error("there is no server-to-client request path on the exec transport")
	}
	// Unknown, not No: nobody has looked, and the difference is the whole point
	// of having four support levels.
	for _, capName := range []adapter.Capability{adapter.CapPlan, adapter.CapTokens, adapter.CapContextWindow, adapter.CapResume} {
		if got := c.Get(capName); got != adapter.SupportUnknown {
			t.Errorf("%s = %s on the exec path, want unknown", capName, got)
		}
	}
	if c.Get(adapter.CapCostUSD) != adapter.SupportNo {
		t.Error("no dollar figure anywhere in the Codex contract, on either path")
	}
}

func TestExecFallbackRefusesToSteer(t *testing.T) {
	a := NewExec(ExecOptions{Log: logx.Discard()})
	s, err := a.Start(context.Background(), adapter.SessionOptions{ID: "x", Workspace: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Steer(context.Background(), "t", adapter.Turn{Text: "no"})
	if !errors.Is(err, adapter.ErrUnsupported) {
		t.Fatalf("steer = %v, want an *UnsupportedError so the caller can cancel and re-prompt", err)
	}
	var ue *adapter.UnsupportedError
	if !errors.As(err, &ue) || ue.Capability != adapter.CapSteer {
		t.Fatalf("error does not name the capability: %v", err)
	}
	if ue.Note == "" {
		t.Error("the console has to be able to explain the gap, not just report it")
	}
}

// TestExecFallbackMapsWhatItRecognisesAndLogsTheRest.
func TestExecFallbackRunsATurn(t *testing.T) {
	a := NewExec(ExecOptions{
		Log:        logx.Discard(),
		Clock:      fixedClock(),
		DrainGrace: time.Second,
		runner:     fixtureRunner(t, repoFile(t, "relayd/testdata/codex/exec-stream.ndjson")),
	})
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	s, err := a.Start(context.Background(), adapter.SessionOptions{ID: "exec-1", Workspace: "/w"})
	if err != nil {
		t.Fatal(err)
	}

	turn, err := s.Send(context.Background(), adapter.Turn{Text: "what is this file"})
	if err != nil {
		t.Fatal(err)
	}
	if turn == "" {
		t.Fatal("a turn needs an id even when the runtime does not name one")
	}

	evs := collect(t, s.Events(), 5, 5*time.Second)
	got := make([]string, 0, len(evs))
	for _, e := range evs {
		got = append(got, describe(e))
		if e.Envelope().Turn != turn {
			t.Errorf("%s is attributed to turn %q, want %q", describe(e), e.Envelope().Turn, turn)
		}
	}
	want := []string{
		"turn_started " + turn,
		"plan [in_progress read the file] [pending summarise it]",
		`text "The file "`,
		`text "is a migration."`,
		"turn_completed " + turn + " end_turn tokens=120",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events:\n %s", len(got), strings.Join(got, "\n "))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: got %s, want %s", i, got[i], want[i])
		}
	}

	// The lines the adapter did not recognise produced no events at all. That
	// is the rule: never emit what you cannot observe.
	last := evs[len(evs)-1].(event.TurnCompleted)
	if last.Usage == nil || last.Usage.TotalTokens == nil || *last.Usage.TotalTokens != 120 {
		t.Errorf("usage = %+v; the one app-server-shaped usage line should have landed", last.Usage)
	}
	if last.Usage.CostUSD != nil {
		t.Error("still no money on this path either")
	}

	// One shot. A second turn needs `codex exec resume`, which is unprobed.
	if _, err := s.Send(context.Background(), adapter.Turn{Text: "and again"}); !errors.Is(err, adapter.ErrUnsupported) {
		t.Errorf("second turn = %v, want ErrUnsupported", err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestExecFallbackResumeIsUnprobedRatherThanBroken(t *testing.T) {
	a := NewExec(ExecOptions{Log: logx.Discard()})
	_, err := a.Resume(context.Background(), adapter.SessionRef{Runtime: adapter.Codex, ID: "x"}, adapter.SessionOptions{})
	var ue *adapter.UnsupportedError
	if !errors.As(err, &ue) {
		t.Fatalf("resume = %v, want an *UnsupportedError", err)
	}
	if ue.Support != adapter.SupportUnknown {
		t.Errorf("support = %s; nobody has looked, which is not the same as no", ue.Support)
	}
}

func TestExecFallbackCancelStopsTheProcess(t *testing.T) {
	release := make(chan struct{})
	a := NewExec(ExecOptions{
		Log:        logx.Discard(),
		Clock:      fixedClock(),
		DrainGrace: time.Second,
		runner: func(ctx context.Context, dir string, argv, env []string, stdout io.Writer) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-release:
				return nil
			}
		},
	})
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	s, err := a.Start(context.Background(), adapter.SessionOptions{ID: "exec-2", Workspace: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan event.Event, 8)
	go func() {
		for e := range s.Events() {
			events <- e
		}
	}()

	turn, err := s.Send(context.Background(), adapter.Turn{Text: "long job"})
	if err != nil {
		t.Fatal(err)
	}
	if e := <-events; describe(e) != "turn_started "+turn {
		t.Fatalf("first event = %s", describe(e))
	}
	if err := s.Cancel(context.Background(), turn); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	e := <-events
	tc, ok := e.(event.TurnCompleted)
	if !ok {
		t.Fatalf("after cancel: %s", describe(e))
	}
	if tc.StopReason != event.StopCancelled {
		t.Errorf("stop = %q, want cancelled", tc.StopReason)
	}
	if !tc.StopReason.Retryable() {
		t.Error("a cancelled turn can be picked up again")
	}
	close(release)
}

func TestExecArgvIsExactlyWhatWillRun(t *testing.T) {
	a := NewExec(ExecOptions{Binary: "/usr/local/bin/codex", Log: logx.Discard()})
	got := strings.Join(a.execArgv("hello"), " ")
	if got != "/usr/local/bin/codex exec --json hello" {
		t.Fatalf("argv = %q", got)
	}
}
