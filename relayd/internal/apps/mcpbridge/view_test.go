package mcpbridge_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/apps"
	"github.com/luthor007/relay/relayd/internal/apps/mcpbridge"
)

func parse(t *testing.T, s string) (mcpbridge.View, error) {
	t.Helper()
	return mcpbridge.ParseView([]byte(s))
}

func mustParse(t *testing.T, s string) mcpbridge.View {
	t.Helper()
	v, err := parse(t, s)
	if err != nil {
		t.Fatalf("parsing a view that should be valid: %v", err)
	}
	return v
}

func rejects(t *testing.T, s, want string) {
	t.Helper()
	_, err := parse(t, s)
	if err == nil {
		t.Fatalf("this view was accepted and should not have been: %s", s)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("the refusal says %q and should mention %q", err.Error(), want)
	}
}

// The vocabulary is four kinds and staying four is the point — APP-PLATFORM.md
// §7's three promises (identical on both platforms, cannot phone home, reviewed
// as a manifest rather than a binary) are all properties of its size.
func TestTheVocabularyIsFourKinds(t *testing.T) {
	got := mcpbridge.BlockKinds()
	want := []mcpbridge.BlockKind{"card", "list", "confirm", "speak"}
	if len(got) != len(want) {
		t.Fatalf("the vocabulary has %d kinds and should have %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kind %d is %q, want %q", i, got[i], want[i])
		}
	}
}

func TestOnlySpeechCostsAPermission(t *testing.T) {
	for _, k := range mcpbridge.BlockKinds() {
		scope, costs := mcpbridge.ScopeFor(k)
		if k == mcpbridge.KindSpeak {
			if !costs || scope != apps.ScopeGlassesSpeaker {
				t.Fatalf("speaking should cost glasses.speaker, got %q/%v", scope, costs)
			}
			continue
		}
		if costs {
			t.Fatalf("%s costs %q; drawing on the phone of the person who installed the app "+
				"reaches nothing of theirs and should be minted like storage", k, scope)
		}
	}
}

func TestParseViewAcceptsAllFourKinds(t *testing.T) {
	v := mustParse(t, `{"vocabulary":1,"blocks":[
		{"kind":"card","title":"Standup","body":"Three decisions.","fields":[{"label":"Length","value":"12 min"}]},
		{"kind":"list","title":"Commitments","items":[{"title":"Ship it","subtitle":"Alexis","detail":"Friday"}]},
		{"kind":"confirm","question":"Read them back?","confirmLabel":"Go on"},
		{"kind":"speak","text":"Saved."}
	]}`)
	if len(v.Blocks) != 4 {
		t.Fatalf("got %d blocks, want 4", len(v.Blocks))
	}
	if v.Vocabulary != mcpbridge.VocabularyVersion {
		t.Fatalf("the version was not stamped: %d", v.Vocabulary)
	}
	if got := mcpbridge.Kinds(v); strings.Join(got, ",") != "card,confirm,list,speak" {
		t.Fatalf("Kinds gave %v", got)
	}
}

// A view from a version this host does not know is refused whole. Drawing the
// blocks it recognises is how a confirmation reaches a screen with a question
// and no buttons.
func TestAnUnknownVocabularyIsRefusedWhole(t *testing.T) {
	rejects(t, `{"vocabulary":2,"blocks":[{"kind":"speak","text":"hi"}]}`, "newer Relay")
	rejects(t, `{"blocks":[{"kind":"speak","text":"hi"}]}`, "which vocabulary")
}

func TestUnknownKindsAndFieldsAreRefused(t *testing.T) {
	// There is nowhere in this vocabulary to put a URL, and that is what "cannot
	// phone home with your data" is made of.
	rejects(t, `{"vocabulary":1,"blocks":[{"kind":"webview","url":"https://evil"}]}`,
		"the vocabulary is card, list, confirm, speak")
	rejects(t, `{"vocabulary":1,"blocks":[{"kind":"card","title":"x","style":"red"}]}`,
		`unknown field "style"`)
	rejects(t, `{"vocabulary":1,"blocks":[{"kind":"list","items":[{"title":"a","imageUrl":"https://x"}]}]}`,
		`unknown field "imageUrl"`)
	rejects(t, `{"vocabulary":1,"blocks":[{"kind":"card","title":"x","fields":[{"label":"l","value":"v","href":"https://x"}]}]}`,
		`unknown field "href"`)
	rejects(t, `{"vocabulary":1,"blocks":[{"kind":"speak","text":"hi"}],"theme":"dark"}`,
		`"theme" is not part of the format`)
}

func TestTheCapsHold(t *testing.T) {
	long := strings.Repeat("x", mcpbridge.Limits.CardTitle+1)
	rejects(t, `{"vocabulary":1,"blocks":[{"kind":"card","title":"`+long+`"}]}`, "the limit is 120")

	var many []string
	for i := 0; i <= mcpbridge.Limits.Blocks; i++ {
		many = append(many, `{"kind":"card","title":"a card"}`)
	}
	rejects(t, `{"vocabulary":1,"blocks":[`+strings.Join(many, ",")+`]}`, "the limit is 8")

	var rows []string
	for i := 0; i <= mcpbridge.Limits.ListItems; i++ {
		rows = append(rows, `{"title":"row"}`)
	}
	rejects(t, `{"vocabulary":1,"blocks":[{"kind":"list","items":[`+strings.Join(rows, ",")+`]}]}`,
		"the limit is 50")
}

func TestTheSerialisedSizeIsCapped(t *testing.T) {
	body := strings.Repeat("x", mcpbridge.Limits.CardBody)
	value := strings.Repeat("v", mcpbridge.Limits.FieldValue)
	var blocks []string
	for i := 0; i < mcpbridge.Limits.Blocks; i++ {
		blocks = append(blocks, `{"kind":"card","title":"a card","body":"`+body+
			`","fields":[{"label":"note","value":"`+value+`"}]}`)
	}
	rejects(t, `{"vocabulary":1,"blocks":[`+strings.Join(blocks, ",")+`]}`, "the limit is 16384")
}

func TestAViewAsksAtMostOneQuestionAndSpeaksAtMostOnce(t *testing.T) {
	rejects(t, `{"vocabulary":1,"blocks":[{"kind":"confirm","question":"a?"},{"kind":"confirm","question":"b?"}]}`,
		"at most one question")
	rejects(t, `{"vocabulary":1,"blocks":[{"kind":"speak","text":"a"},{"kind":"speak","text":"b"}]}`,
		"talk over each other")
}

func TestEmptyAndBlankAreRefused(t *testing.T) {
	rejects(t, `{"vocabulary":1,"blocks":[]}`, "renders nothing")
	rejects(t, `{"vocabulary":1,"blocks":[{"kind":"list","title":"Commitments","items":[]}]}`, "empty list")
	rejects(t, `{"vocabulary":1,"blocks":[{"kind":"card","title":"   "}]}`, "is empty")
	rejects(t, `{"vocabulary":1,"blocks":[{"kind":"card"}]}`, "is required")
}

// Refused rather than stripped: a card is text a phone draws, not a terminal,
// and quietly removing an escape sequence hides from the app that it sent one.
func TestControlCharactersAreRefusedNotStripped(t *testing.T) {
	rejects(t, `{"vocabulary":1,"blocks":[{"kind":"card","title":"done\u001b[31m"}]}`, "control character")
	rejects(t, `{"vocabulary":1,"blocks":[{"kind":"list","items":[{"title":"a\u0007"}]}]}`, "control character")
	// A newline in a title is a control character; in a body it is a paragraph.
	rejects(t, `{"vocabulary":1,"blocks":[{"kind":"card","title":"one\ntwo"}]}`, "control character")
	v := mustParse(t, `{"vocabulary":1,"blocks":[{"kind":"card","title":"Notes","body":"one\ntwo"}]}`)
	if v.Blocks[0].Body != "one\ntwo" {
		t.Fatalf("the body was altered: %q", v.Blocks[0].Body)
	}
}

// A view built in Go must go through the same validator as a view off the
// channel, or relayd can emit something the SDK would have refused.
func TestValidateChecksAViewBuiltInGo(t *testing.T) {
	_, err := mcpbridge.Validate(mcpbridge.View{
		Vocabulary: mcpbridge.VocabularyVersion,
		Blocks:     []mcpbridge.Block{{Kind: mcpbridge.KindCard, Title: strings.Repeat("x", 200)}},
	})
	if err == nil {
		t.Fatal("a too-long title built in Go was accepted")
	}
	good, err := mcpbridge.Validate(mcpbridge.View{
		Vocabulary: mcpbridge.VocabularyVersion,
		Blocks:     []mcpbridge.Block{{Kind: mcpbridge.KindSpeak, Text: "Saved."}},
	})
	if err != nil || len(good.Blocks) != 1 {
		t.Fatalf("a valid view was refused: %v", err)
	}
}

// A Go-built view and an SDK-built view must serialise identically, or the
// phone sees two dialects of one format.
func TestBlocksSerialiseOnlyTheFieldsTheirKindHas(t *testing.T) {
	v := mcpbridge.View{Vocabulary: 1, Blocks: []mcpbridge.Block{
		// Title is set on a speak block: it has no title, so it must not appear.
		{Kind: mcpbridge.KindSpeak, Text: "Saved.", Title: "leaked"},
	}}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "leaked") {
		t.Fatalf("a field the kind does not have was serialised: %s", b)
	}
	if got := string(b); got != `{"vocabulary":1,"blocks":[{"kind":"speak","text":"Saved."}]}` {
		t.Fatalf("serialised as %s", got)
	}
}

func TestCheckScopesRefusesSpeechFromAnAppThatWasDeclined(t *testing.T) {
	v := mustParse(t, `{"vocabulary":1,"blocks":[{"kind":"speak","text":"Saved."}]}`)
	if err := mcpbridge.CheckScopes(v, []apps.Scope{apps.ScopeMemoryRead}); err == nil {
		t.Fatal("an app without glasses.speaker was allowed to speak through a view")
	}
	if err := mcpbridge.CheckScopes(v, []apps.Scope{apps.ScopeGlassesSpeaker}); err != nil {
		t.Fatalf("an app with glasses.speaker was refused: %v", err)
	}
	card := mustParse(t, `{"vocabulary":1,"blocks":[{"kind":"card","title":"Standup"}]}`)
	if err := mcpbridge.CheckScopes(card, nil); err != nil {
		t.Fatalf("a card should cost nothing: %v", err)
	}
}

// The projection an agent reads. Byte-identical to the SDK's viewText — the
// same string is asserted in apps/sdk/test/ui.test.ts, so the two renderings of
// one view cannot drift into two summaries.
func TestViewTextMatchesTheSDK(t *testing.T) {
	v := mustParse(t, `{"vocabulary":1,"blocks":[
		{"kind":"card","title":"Standup","body":"Three decisions.","fields":[{"label":"Length","value":"12 min"}]},
		{"kind":"list","title":"Commitments","items":[{"title":"Ship it","subtitle":"Alexis","detail":"Friday"}]},
		{"kind":"confirm","question":"Read them back?"},
		{"kind":"speak","text":"Saved."}
	]}`)
	want := strings.Join([]string{
		"Standup",
		"Three decisions.",
		"Length: 12 min",
		"Commitments",
		"- Ship it — Alexis — Friday",
		"Read them back?",
		"[Yes / No]",
		"Saved.",
	}, "\n")
	if got := mcpbridge.ViewText(v); got != want {
		t.Fatalf("ViewText gave:\n%s\nwant:\n%s", got, want)
	}
	if got := mcpbridge.SpokenText(v); got != "Saved." {
		t.Fatalf("SpokenText gave %q", got)
	}
	if !mcpbridge.ExpectsDecision(v) {
		t.Fatal("a view with a confirm expects a decision")
	}
}
