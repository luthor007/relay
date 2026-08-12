package apps

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// An app draws on the phone, for real
//
// Every other test in this file's neighbourhood exercises the vocabulary and
// the capability in isolation. This one runs Node, in the sandbox, on an app
// that calls `ctx.ui`, and asserts the view came out the other end — because
// the two halves that could silently not meet are the runner's shaper table
// (a method with no shaper is skipped, not an error) and the generic `ctx`
// builder, and neither is exercised by a unit test of the Go side.

// recordingScreen is a Screen that keeps what it was handed.
type recordingScreen struct {
	mu     sync.Mutex
	drawn  []Rendered
	asked  []Rendered
	answer bool
}

func (s *recordingScreen) Render(_ context.Context, r Rendered) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drawn = append(s.drawn, r)
	return nil
}

func (s *recordingScreen) Ask(_ context.Context, r Rendered) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, r)
	return s.answer, nil
}

func (s *recordingScreen) views() ([]Rendered, []Rendered) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Rendered(nil), s.drawn...), append([]Rendered(nil), s.asked...)
}

const drawingApp = `
export default {
  async onTrigger(ctx) {
    await ctx.ui.card("Standup", { body: "Two things.", fields: [{ label: "Blocked", value: "no" }] });
    await ctx.ui.list([{ title: "Ship the fix", detail: "4pm" }], { title: "Today" });
    const ok = await ctx.ui.ask("Send the summary to alice?", { detail: "It is four lines." });
    ctx.log("answered " + ok);
    await ctx.ui.render({
      vocabulary: 1,
      blocks: [{ kind: "card", title: ok ? "Sent" : "Kept it here" }],
    });
  },
};
`

func TestAnAppDrawsOnThePhone(t *testing.T) {
	screen := &recordingScreen{answer: true}
	tr := newTestRuntime(t, func(o *Options) { o.Screen = screen })

	// No permissions at all. Drawing costs no scope, and this is the test that
	// says so from outside: an app that asked for nothing can still put a card
	// in front of the person who installed it.
	src := writeApp(t, manifestWith("dev.test.draw", "",
		`{"type":"phrase","match":"draw something"}`, ""), drawingApp)
	inst := tr.install(t, src)

	inv, err := tr.Invoke(context.Background(), inst,
		TriggerFrame{Type: TriggerPhrase, Transcript: "draw something"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Outcome != OutcomeCompleted {
		t.Fatalf("outcome %s: %s (logs %q)", inv.Outcome, inv.Error, tr.logged())
	}

	drawn, asked := screen.views()
	if len(drawn) != 3 {
		t.Fatalf("drew %d views, want 3 (card, list, and the one after the answer)", len(drawn))
	}
	if len(asked) != 1 {
		t.Fatalf("asked %d questions, want 1", len(asked))
	}

	// The shorthands assemble a one-block view rather than a second host
	// method, so what arrives is a normal view that went through the normal
	// validator.
	if k := drawn[0].View.Blocks[0].Kind; k != BlockCard {
		t.Errorf("ui.card drew a %s", k)
	}
	if got := drawn[0].View.Blocks[0].Fields; len(got) != 1 || got[0].Value != "no" {
		t.Errorf("the card's fields did not survive: %+v", got)
	}
	if k := drawn[1].View.Blocks[0].Kind; k != BlockList {
		t.Errorf("ui.list drew a %s", k)
	}
	if q := asked[0].View.Blocks[0].Question; q != "Send the summary to alice?" {
		t.Errorf("the question did not survive: %q", q)
	}
	// The answer reached the app: its last card is the one for "yes".
	if title := drawn[2].View.Blocks[0].Title; title != "Sent" {
		t.Errorf("the app did not receive the answer; last card is %q", title)
	}
	if drawn[0].AppID != "dev.test.draw" {
		t.Errorf("the view arrived unattributed: %q", drawn[0].AppID)
	}
}

const speakingApp = `
export default {
  async onTrigger(ctx) {
    await ctx.ui.render({
      vocabulary: 1,
      blocks: [{ kind: "speak", text: "Tests are green." }],
    });
  },
};
`

func TestAnAppCannotSpeakThroughAViewWithoutTheSpeaker(t *testing.T) {
	// The one block that costs a permission, refused for an app that has every
	// other one. Without this the `ui` capability being scope-free would be a
	// way to reach someone's ear without having asked.
	screen := &recordingScreen{}
	tr := newTestRuntime(t, func(o *Options) { o.Screen = screen })
	src := writeApp(t, manifestWith("dev.test.speak",
		`{"scope":"memory.read","reason":"To read the meeting."}`,
		`{"type":"phrase","match":"say it"}`, ""), speakingApp)
	inst := tr.install(t, src)

	inv, err := tr.Invoke(context.Background(), inst,
		TriggerFrame{Type: TriggerPhrase, Transcript: "say it"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Outcome == OutcomeCompleted {
		t.Fatal("the app spoke through a view with no glasses.speaker")
	}
	if !strings.Contains(inv.Error, "glasses.speaker") {
		t.Errorf("the app was not told which permission it lacked: %q", inv.Error)
	}
	if drawn, _ := screen.views(); len(drawn) != 0 {
		t.Errorf("the refused view reached the screen anyway: %+v", drawn)
	}
}

const noPhoneApp = `
export default {
  async onTrigger(ctx) {
    try {
      await ctx.ui.card("Standup");
      ctx.log("drew it");
    } catch (err) {
      ctx.log("refused: " + err.code);
    }
  },
};
`

func TestAnAppIsToldWhenThereIsNoPhoneRatherThanBelievingItDrew(t *testing.T) {
	// A box with no screen wired at all. The capability is still minted — a
	// phone that connects a second later must not require a reinstall — and the
	// call answers unavailable, which is the code that means "the box", not
	// "your app".
	tr := newTestRuntime(t) // Options.Screen is nil
	src := writeApp(t, manifestWith("dev.test.nophone", "",
		`{"type":"phrase","match":"draw"}`, ""), noPhoneApp)
	inst := tr.install(t, src)

	inv, err := tr.Invoke(context.Background(), inst,
		TriggerFrame{Type: TriggerPhrase, Transcript: "draw"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Outcome != OutcomeCompleted {
		t.Fatalf("outcome %s: %s", inv.Outcome, inv.Error)
	}
	logs := strings.Join(tr.logged(), " | ")
	if strings.Contains(logs, "drew it") {
		t.Error("the app was told it drew a card on a box with no screen")
	}
	if !strings.Contains(logs, "refused: "+CodeUnavailable) {
		t.Errorf("the app should see %s, meaning the box and not its own code: %q",
			CodeUnavailable, logs)
	}
}
