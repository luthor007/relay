package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/api"
)

// A mini-app's view has to actually reach a phone
//
// The frame type `ui.render` was declared in wire.go and used by nothing for
// the whole life of this package — the same defect this file's neighbours were
// written to catch. These tests assert through the real server and a real
// WebSocket, so deleting the delivery in deliverPing fails them.

// dialReady dials and waits until the server counts the phone as connected.
//
// [dial] returns when the handshake completes on the client, which is before
// handleWS has subscribed and counted it — so a Draw issued immediately after
// can legitimately answer [api.ErrNoScreen]. That is the correct production
// behaviour and a race in a test, so the test waits for the opening
// `session.list`, which the server sends only after both.
func dialReady(t *testing.T, r *rig) *wsClient {
	t.Helper()
	c := dial(t, r)
	c.await(t, api.TypeSessionList)
	return c
}

func drawn(t *testing.T, e api.Envelope) api.UIRender {
	t.Helper()
	var got api.UIRender
	if err := json.Unmarshal(e.Payload, &got); err != nil {
		t.Fatalf("the ui.render payload does not decode: %v", err)
	}
	return got
}

func TestADrawnViewReachesTheConnectedPhone(t *testing.T) {
	r := newRig(t, api.Options{})
	c := dialReady(t, r)

	view := json.RawMessage(`{"vocabulary":1,"blocks":[{"kind":"card","title":"Standup"}]}`)
	if err := r.Srv.Draw(context.Background(), api.UIRender{
		App: "dev.alexis.standup", AppName: "Standup", View: view,
	}); err != nil {
		t.Fatal(err)
	}

	got := drawn(t, c.await(t, api.TypeUIRender))
	if got.App != "dev.alexis.standup" || got.AppName != "Standup" {
		t.Errorf("the view arrived unattributed: %+v", got)
	}
	// The transport carries the view without opinions about it: internal/apps
	// owns the vocabulary and has already validated this, and a second parse
	// here would be a second definition of it in one binary.
	if !strings.Contains(string(got.View), `"kind":"card"`) {
		t.Errorf("the view did not survive the wire: %s", got.View)
	}
	if got.ActionID != "" {
		t.Errorf("a view that asks nothing carried an action id: %q", got.ActionID)
	}
}

func TestDrawingWithNoPhoneConnectedIsAnErrorNotASilentSuccess(t *testing.T) {
	r := newRig(t, api.Options{})
	// Nobody dialled.
	err := r.Srv.Draw(context.Background(), api.UIRender{
		App:  "dev.alexis.standup",
		View: json.RawMessage(`{"vocabulary":1,"blocks":[{"kind":"card","title":"Standup"}]}`),
	})
	// The app is entitled to know its card went nowhere. The alternative is an
	// app that reports having shown you something it did not.
	if !errors.Is(err, api.ErrNoScreen) {
		t.Fatalf("drawing into an empty room returned %v", err)
	}
}

func TestAViewsQuestionIsAnsweredByTheOrdinaryConsentFrame(t *testing.T) {
	r := newRig(t, api.Options{})
	c := dialReady(t, r)

	type answer struct {
		ok  bool
		err error
	}
	done := make(chan answer, 1)
	go func() {
		ok, err := r.Srv.DrawAndAsk(context.Background(), api.UIRender{
			App: "dev.alexis.standup",
			View: json.RawMessage(
				`{"vocabulary":1,"blocks":[{"kind":"confirm","question":"Send it?"}]}`),
		}, time.Now().Add(30*time.Second))
		done <- answer{ok, err}
	}()

	got := drawn(t, c.await(t, api.TypeUIRender))
	if got.ActionID == "" {
		t.Fatal("a question arrived with no id, so nothing could have answered it")
	}
	if got.Deadline == 0 {
		t.Error("a question with no deadline leaves a button that outlives the app waiting on it")
	}

	// The whole point: the phone answers with the same frame it uses for a
	// runtime's approval, and ws.go needed no case for mini-apps at all.
	c.send(t, "d-1", api.TypeConsentDecision, api.ConsentDecision{
		ActionID: got.ActionID,
		Approved: true,
	})

	select {
	case a := <-done:
		if a.err != nil {
			t.Fatalf("the ask failed: %v", a.err)
		}
		if !a.ok {
			t.Error("the app was told no after the user said yes")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the answer never reached the app")
	}
}

func TestAQuestionNobodyAnswersIsRetractedRatherThanLeftOnScreen(t *testing.T) {
	r := newRig(t, api.Options{})
	c := dialReady(t, r)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.Srv.DrawAndAsk(ctx, api.UIRender{
			App: "dev.alexis.standup",
			View: json.RawMessage(
				`{"vocabulary":1,"blocks":[{"kind":"confirm","question":"Send it?"}]}`),
		}, time.Now().Add(30*time.Second))
		done <- err
	}()

	got := drawn(t, c.await(t, api.TypeUIRender))
	cancel() // the app stopped waiting

	// A button that no longer does anything is worse than no button: the user
	// presses it believing they answered.
	resolved := c.await(t, api.TypeConfirmResolved)
	var payload api.ConfirmResolved
	if err := json.Unmarshal(resolved.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ActionID != got.ActionID {
		t.Errorf("retracted %q, but the question was %q", payload.ActionID, got.ActionID)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Error("the app was told the question resolved when it had given up on it")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DrawAndAsk did not return after its context was cancelled")
	}
}

func TestADrawnViewIsNotBatchedWithThePingPolicy(t *testing.T) {
	// A view is a reply to something the user just did. The ping policy holds
	// completions for a gap in the conversation and silences them in quiet
	// hours; applying either to a card would be the turn-taking rules used on
	// something they were not written about.
	r := newRig(t, api.Options{})
	c := dialReady(t, r)

	if err := r.Srv.Draw(context.Background(), api.UIRender{
		App:  "dev.alexis.standup",
		View: json.RawMessage(`{"vocabulary":1,"blocks":[{"kind":"card","title":"Standup"}]}`),
	}); err != nil {
		t.Fatal(err)
	}

	// It arrives on its own: no speak, no notify, no confirm alongside it.
	c.awaitWithout(t, api.TypeUIRender, api.TypeSpeak, api.TypeNotify, api.TypeConfirmRequest)
}
