package apps_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/apps"
)

// fakeScreen stands in for a paired phone.
type fakeScreen struct {
	mu      sync.Mutex
	drawn   []apps.Rendered
	asked   []apps.Rendered
	answer  bool
	hold    time.Duration
	failing error
}

func (s *fakeScreen) Render(_ context.Context, r apps.Rendered) error {
	if s.failing != nil {
		return s.failing
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drawn = append(s.drawn, r)
	return nil
}

func (s *fakeScreen) Ask(ctx context.Context, r apps.Rendered) (bool, error) {
	if s.failing != nil {
		return false, s.failing
	}
	s.mu.Lock()
	s.asked = append(s.asked, r)
	hold, answer := s.hold, s.answer
	s.mu.Unlock()
	if hold > 0 {
		select {
		case <-time.After(hold):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return answer, nil
}

func (s *fakeScreen) views() []apps.Rendered {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]apps.Rendered(nil), s.drawn...)
}

func newUI(t *testing.T, screen apps.Screen, granted ...apps.Scope) *apps.UICap {
	t.Helper()
	u, err := apps.NewUI(apps.UIOptions{
		Screen:  screen,
		AppID:   "dev.alexis.standup",
		AppName: "Standup",
		Granted: granted,
		Redact:  apps.Detector(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestAUICapabilityWithoutADetectorIsRefused(t *testing.T) {
	// The same structural refusal the memory capability makes. A view is text an
	// app assembled, and there is no path here that draws text without having
	// looked for credentials in it first.
	_, err := apps.NewUI(apps.UIOptions{Screen: &fakeScreen{}})
	if !errors.Is(err, apps.ErrNoRedactor) {
		t.Fatalf("a UI capability was built with no detector: %v", err)
	}
}

func TestACredentialOnACardIsRedactedBeforeItIsDrawn(t *testing.T) {
	screen := &fakeScreen{}
	ui := newUI(t, screen)

	// The failure this prevents: an app reads a transcript, finds a key in it,
	// and puts it on a card "so you can see what I found".
	secret := "sk_live_" + strings.Repeat("a", 32)
	err := ui.Render(context.Background(), apps.View{
		Vocabulary: apps.VocabularyVersion,
		Blocks: []apps.Block{{
			Kind:   apps.BlockCard,
			Title:  "Found a key",
			Body:   "In yesterday's session: " + secret,
			Fields: []apps.Field{{Label: "key", Value: secret}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	drawn := screen.views()
	if len(drawn) != 1 {
		t.Fatalf("drew %d views, want 1", len(drawn))
	}
	got := drawn[0].View.Text()
	if strings.Contains(got, secret) {
		t.Fatalf("the credential reached the phone:\n%s", got)
	}
	if !strings.Contains(got, "Found a key") {
		t.Errorf("redaction took the whole card, not the secret:\n%s", got)
	}
}

func TestTheAppThatDrewItTravelsWithTheView(t *testing.T) {
	screen := &fakeScreen{}
	ui := newUI(t, screen)
	if err := ui.Render(context.Background(), apps.View{
		Vocabulary: apps.VocabularyVersion,
		Blocks:     []apps.Block{{Kind: apps.BlockCard, Title: "Standup"}},
	}); err != nil {
		t.Fatal(err)
	}
	got := screen.views()[0]
	// "Which of my apps is asking me this" is the first question a card raises,
	// and the phone cannot answer it from the view alone.
	if got.AppID != "dev.alexis.standup" || got.AppName != "Standup" {
		t.Errorf("the view arrived unattributed: %+v", got)
	}
}

func TestRenderRefusesAQuestionNobodyIsWaitingFor(t *testing.T) {
	screen := &fakeScreen{}
	ui := newUI(t, screen)
	err := ui.Render(context.Background(), apps.View{
		Vocabulary: apps.VocabularyVersion,
		Blocks:     []apps.Block{{Kind: apps.BlockConfirm, Question: "Send the email?"}},
	})
	if err == nil {
		t.Fatal("a confirmation was drawn by render(); the user would press a button and " +
			"nothing would happen")
	}
	if !strings.Contains(err.Error(), "ask()") {
		t.Errorf("the error does not point at the method that does wait: %v", err)
	}
	if len(screen.views()) != 0 {
		t.Error("the refused view was drawn anyway")
	}
}

func TestSilenceIsANoAndTheAppCannotTellTheDifference(t *testing.T) {
	// The screen never answers. An app must treat that as a no — a third
	// outcome it could branch on is how "confirm before you send" becomes
	// "send".
	screen := &fakeScreen{hold: time.Hour, answer: true}
	u, err := apps.NewUI(apps.UIOptions{
		Screen: screen, AppID: "a", Redact: apps.Detector(), AskTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ok, err := u.Ask(context.Background(), apps.View{
		Vocabulary: apps.VocabularyVersion,
		Blocks:     []apps.Block{{Kind: apps.BlockConfirm, Question: "Send it?"}},
	})
	if err != nil {
		t.Fatalf("nobody answering is not an error the app has to handle: %v", err)
	}
	if ok {
		t.Fatal("an unanswered question came back true")
	}
}

func TestAskNeedsAQuestion(t *testing.T) {
	ui := newUI(t, &fakeScreen{})
	_, err := ui.Ask(context.Background(), apps.View{
		Vocabulary: apps.VocabularyVersion,
		Blocks:     []apps.Block{{Kind: apps.BlockCard, Title: "Standup"}},
	})
	if err == nil {
		t.Fatal("ask() accepted a view with nothing to answer; it would wait for a button " +
			"that was never drawn")
	}
}

func TestABoxWithNoPhoneSaysSoRatherThanSwallowingTheView(t *testing.T) {
	ui := newUI(t, nil)
	err := ui.Render(context.Background(), apps.View{
		Vocabulary: apps.VocabularyVersion,
		Blocks:     []apps.Block{{Kind: apps.BlockCard, Title: "Standup"}},
	})
	// The SDK's own words: a render() that resolves having sent a frame into
	// nothing is worse than one that fails, because the app reports success.
	if !errors.Is(err, apps.ErrNoPhone) {
		t.Fatalf("drawing with no phone paired returned %v", err)
	}
}

func TestDrawingIsMintedWithoutTheSpeakerAndSpeakingIsNot(t *testing.T) {
	// The property that makes `ui` safe to mint without a scope: an app that has
	// it can draw and cannot speak.
	granted := []apps.Scope{}
	methods := apps.Methods(granted)
	var canRender, canSay bool
	for _, m := range methods {
		switch m {
		case apps.MethodUIRender, apps.MethodUIAsk:
			canRender = true
		case apps.MethodGlassesSay:
			canSay = true
		}
	}
	if !canRender {
		t.Error("an app with no scopes at all cannot draw; ui is supposed to be scope-free")
	}
	if canSay {
		t.Error("an app with no scopes was minted glasses.say")
	}

	ui := newUI(t, &fakeScreen{}) // no scopes granted
	err := ui.Render(context.Background(), apps.View{
		Vocabulary: apps.VocabularyVersion,
		Blocks:     []apps.Block{{Kind: apps.BlockSpeak, Text: "Tests are green."}},
	})
	if err == nil {
		t.Fatal("an app with no glasses.speaker spoke through a view, which would be the " +
			"speaker minted by the back door")
	}
	if !strings.Contains(err.Error(), "glasses.speaker") {
		t.Errorf("the error does not name the permission: %v", err)
	}
}

func TestUIIsTheOnlyScopeFreeCapabilityThatLeavesTheBox(t *testing.T) {
	// A guard on the argument in capability.go rather than on the code: if
	// somebody adds another scope-free row later, this fails and they have to
	// write down why it reaches nothing of the user's.
	scopeFree := map[apps.Method]bool{}
	for _, m := range apps.Methods(nil) {
		scopeFree[m] = true
	}
	want := map[apps.Method]bool{
		apps.MethodUIRender:      true,
		apps.MethodUIAsk:         true,
		apps.MethodStorageGet:    true,
		apps.MethodStorageSet:    true,
		apps.MethodStorageDelete: true,
		apps.MethodLog:           true,
	}
	for m := range scopeFree {
		if !want[m] {
			t.Errorf("%s is mintable with no scopes and is not one of the capabilities that "+
				"argument was made for. Either it reaches nothing of the user's — write that "+
				"down in capability.go — or it needs a scope", m)
		}
	}
	for m := range want {
		if !scopeFree[m] {
			t.Errorf("%s used to be scope-free and now is not", m)
		}
	}
}
