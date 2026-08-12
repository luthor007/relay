package mcpbridge_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/apps"
	"github.com/luthor007/relay/relayd/internal/apps/mcpbridge"
)

func at() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

func meta() mcpbridge.FrameMeta {
	return mcpbridge.FrameMeta{ID: "f-1", At: at(), App: "dev.alexis.standup-notes"}
}

// SYSTEM.md §6.1's envelope, carrying a view. Nothing new in either direction.
func TestARenderFrameIsTheDocumentedEnvelope(t *testing.T) {
	v := mustParse(t, `{"vocabulary":1,"blocks":[{"kind":"card","title":"Standup"}]}`)
	f, err := mcpbridge.NewRenderFrame(v, meta())
	if err != nil {
		t.Fatal(err)
	}
	if f.V != 1 || f.Type != "ui.render" || f.ID != "f-1" || f.At != at().UnixMilli() {
		t.Fatalf("frame is %+v", f)
	}
	if f.Payload.App != "dev.alexis.standup-notes" {
		t.Fatalf("payload.app is %q", f.Payload.App)
	}
	if f.Payload.Expects != "" {
		t.Fatalf("a view that asks nothing must not expect an answer: %q", f.Payload.Expects)
	}

	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(b, &envelope); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"v", "id", "type", "at", "payload"} {
		if _, ok := envelope[key]; !ok {
			t.Fatalf("the envelope is missing %q: %s", key, b)
		}
	}
}

// A confirm's answer comes back on `consent.decision`, which §6.1 already
// defines, keyed by this frame's id. There is no second confirmation channel.
func TestAQuestionExpectsADecision(t *testing.T) {
	v := mustParse(t, `{"vocabulary":1,"blocks":[{"kind":"confirm","question":"File these as issues?"}]}`)
	f, err := mcpbridge.NewRenderFrame(v, meta())
	if err != nil {
		t.Fatal(err)
	}
	if f.Payload.Expects != mcpbridge.PayloadExpectsDecision {
		t.Fatalf("payload.expects is %q", f.Payload.Expects)
	}
}

func TestARenderFrameMustNameTheAppAndCarryAnID(t *testing.T) {
	v := mustParse(t, `{"vocabulary":1,"blocks":[{"kind":"card","title":"Standup"}]}`)
	if _, err := mcpbridge.NewRenderFrame(v, mcpbridge.FrameMeta{ID: "f-1", At: at()}); err == nil {
		t.Fatal("a frame with no app was built")
	}
	if _, err := mcpbridge.NewRenderFrame(v, mcpbridge.FrameMeta{At: at(), App: "a.b"}); err == nil {
		t.Fatal("a frame with no id was built; an answer would have nothing to name")
	}
}

func TestAFrameRoundTripsThroughJSON(t *testing.T) {
	v := mustParse(t, `{"vocabulary":1,"blocks":[
		{"kind":"card","title":"Standup","body":"Three decisions."},
		{"kind":"confirm","question":"Read them back?","cancelLabel":"Later"}
	]}`)
	f, err := mcpbridge.NewRenderFrame(v, meta())
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	back, err := mcpbridge.ParseRenderFrame(b)
	if err != nil {
		t.Fatalf("a frame this package built did not parse: %v", err)
	}
	again, err := json.Marshal(back)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(b) {
		t.Fatalf("round trip changed the frame:\n%s\n%s", b, again)
	}
}

// A phone that received a frame it cannot fully validate draws nothing and says
// why. It does not draw the half it understood.
func TestAHostRefusesAFrameItCannotFullyValidate(t *testing.T) {
	good := `{"v":1,"id":"f-1","type":"ui.render","at":1,"payload":{"app":"a.b","view":{"vocabulary":1,"blocks":[{"kind":"card","title":"T"}]}}}`
	if _, err := mcpbridge.ParseRenderFrame([]byte(good)); err != nil {
		t.Fatalf("a good frame was refused: %v", err)
	}
	for _, tc := range []struct{ name, frame, want string }{
		{"wrong envelope version", strings.Replace(good, `"v":1`, `"v":2`, 1), "envelope version"},
		{"wrong type", strings.Replace(good, `"ui.render"`, `"speak"`, 1), "not a ui.render frame"},
		{"no id", strings.Replace(good, `"id":"f-1"`, `"id":""`, 1), "needs an id"},
		{"no app", strings.Replace(good, `"app":"a.b",`, ``, 1), "payload.app"},
		{"bad view", strings.Replace(good, `"kind":"card","title":"T"`, `"kind":"iframe"`, 1), "the vocabulary is"},
		{"not an object", `"a frame"`, "must be a JSON object"},
	} {
		if _, err := mcpbridge.ParseRenderFrame([]byte(tc.frame)); err == nil {
			t.Fatalf("%s: accepted", tc.name)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: refused with %q, want %q", tc.name, err, tc.want)
		}
	}
}

// Otherwise the phone waits for an answer nobody will ever give it.
func TestExpectsOnAViewThatAsksNothingIsRefused(t *testing.T) {
	frame := `{"v":1,"id":"f-1","type":"ui.render","at":1,"payload":{"app":"a.b","expects":"decision",` +
		`"view":{"vocabulary":1,"blocks":[{"kind":"card","title":"T"}]}}}`
	if _, err := mcpbridge.ParseRenderFrame([]byte(frame)); err == nil ||
		!strings.Contains(err.Error(), "answer nobody will give") {
		t.Fatalf("got %v", err)
	}
}

// --- the Renderer: the server side of ctx.ui --------------------------------

type sink struct{ frames []mcpbridge.RenderFrame }

func (s *sink) Render(_ context.Context, f mcpbridge.RenderFrame) error {
	s.frames = append(s.frames, f)
	return nil
}

func renderer(t *testing.T, manifest string, granted []apps.Scope, s mcpbridge.ViewSink) *mcpbridge.Renderer {
	t.Helper()
	return &mcpbridge.Renderer{
		App:        installGranting(t, manifest, granted),
		Invocation: "inv-1",
		Sink:       s,
		Now:        at,
		NewID:      func() string { return "f-1" },
	}
}

func TestTheRendererStampsTheAppSoAnAppCannotForgeIt(t *testing.T) {
	s := &sink{}
	r := renderer(t, standupManifest, nil, s)
	f, err := r.Render(context.Background(), mcpbridge.View{
		Vocabulary: 1,
		Blocks:     []mcpbridge.Block{{Kind: mcpbridge.KindCard, Title: "Standup"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A card that could claim to be from another app is a phishing surface on a
	// device whose whole pitch is that you can trust what it shows you.
	if f.Payload.App != "dev.alexis.standup-notes" {
		t.Fatalf("payload.app is %q", f.Payload.App)
	}
	if f.Payload.Invocation != "inv-1" {
		t.Fatalf("a rendered card should tie to a log line, got %q", f.Payload.Invocation)
	}
	if len(s.frames) != 1 {
		t.Fatalf("%d frames reached the phone", len(s.frames))
	}
}

// Never emit what you cannot observe, on the output side: an app that lost
// glasses.speaker at the install sheet does not get to speak through a view.
func TestTheRendererRefusesABlockTheGrantDidNotPayFor(t *testing.T) {
	s := &sink{}
	r := renderer(t, standupManifest, []apps.Scope{apps.ScopeMemoryRead}, s)
	_, err := r.Render(context.Background(), mcpbridge.View{
		Vocabulary: 1,
		Blocks:     []mcpbridge.Block{{Kind: mcpbridge.KindSpeak, Text: "Saved."}},
	})
	if err == nil || !strings.Contains(err.Error(), "glasses.speaker") {
		t.Fatalf("got %v", err)
	}
	if len(s.frames) != 0 {
		t.Fatal("a refused view was delivered anyway")
	}
}

func TestTheRendererValidatesBeforeItDelivers(t *testing.T) {
	s := &sink{}
	r := renderer(t, standupManifest, nil, s)
	_, err := r.Render(context.Background(), mcpbridge.View{Vocabulary: 1})
	if err == nil {
		t.Fatal("an empty view was delivered")
	}
	if len(s.frames) != 0 {
		t.Fatal("an invalid view reached the phone")
	}
}

// A box with no phone paired has nowhere to draw. An app told its card was
// drawn when nothing drew it goes on to say "as you can see above" into a void.
func TestRenderingWithNowhereToDrawIsARefusal(t *testing.T) {
	r := renderer(t, standupManifest, nil, nil)
	_, err := r.Render(context.Background(), mcpbridge.View{
		Vocabulary: 1,
		Blocks:     []mcpbridge.Block{{Kind: mcpbridge.KindCard, Title: "Standup"}},
	})
	if !errors.Is(err, mcpbridge.ErrNoSurface) {
		t.Fatalf("got %v", err)
	}
}

// A sink that could not deliver is a failure the app is told about, not a
// resolved promise.
func TestASinkThatFailedIsReportedToTheApp(t *testing.T) {
	boom := errors.New("the phone is not connected")
	r := renderer(t, standupManifest, nil,
		mcpbridge.SinkFunc(func(context.Context, mcpbridge.RenderFrame) error { return boom }))
	_, err := r.Render(context.Background(), mcpbridge.View{
		Vocabulary: 1,
		Blocks:     []mcpbridge.Block{{Kind: mcpbridge.KindCard, Title: "Standup"}},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}
