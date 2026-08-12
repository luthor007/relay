package apps_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/apps"
)

// The vocabulary is defined twice and must not drift
//
// `apps/sdk/src/ui.ts` runs inside the sandbox and gives an author a good error
// while they are still writing. `internal/apps/ui.go` runs on relayd's side of
// the boundary and is the enforcement that counts. Neither can import the
// other, so nothing but this test stops them disagreeing — and a disagreement
// is not a compile error anywhere. It is an app that validated its own view,
// sent it, and had it refused by the host with a different number in the
// message.
//
// The test reads the TypeScript rather than a copy of it. A fixture would be a
// third place to update.

func sdkUI(t *testing.T) string {
	t.Helper()
	// internal/apps → relayd → repo root.
	path := filepath.Join("..", "..", "..", "apps", "sdk", "src", "ui.ts")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the SDK's ui.ts is the other half of this contract and could not be read: %v", err)
	}
	return string(b)
}

func TestTheVocabularyMirrorsTheSDK(t *testing.T) {
	src := sdkUI(t)

	t.Run("limits", func(t *testing.T) {
		// Field name here → key in the SDK's LIMITS object.
		want := map[string]int{
			"blocks":        apps.ViewCaps.Blocks,
			"cardTitle":     apps.ViewCaps.CardTitle,
			"cardBody":      apps.ViewCaps.CardBody,
			"cardFields":    apps.ViewCaps.CardFields,
			"fieldLabel":    apps.ViewCaps.FieldLabel,
			"fieldValue":    apps.ViewCaps.FieldValue,
			"listTitle":     apps.ViewCaps.ListTitle,
			"listItems":     apps.ViewCaps.ListItems,
			"itemTitle":     apps.ViewCaps.ItemTitle,
			"itemSubtitle":  apps.ViewCaps.ItemSubtitle,
			"itemDetail":    apps.ViewCaps.ItemDetail,
			"question":      apps.ViewCaps.Question,
			"buttonLabel":   apps.ViewCaps.ButtonLabel,
			"confirmDetail": apps.ViewCaps.ConfirmDetail,
			"speakText":     apps.ViewCaps.SpeakText,
		}

		block := between(t, src, "export const LIMITS = Object.freeze({", "});")
		found := map[string]bool{}
		for key, ours := range want {
			re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(key) + `:\s*([0-9]+)\s*,`)
			m := re.FindStringSubmatch(block)
			if m == nil {
				t.Errorf("LIMITS.%s is not in the SDK; this host caps it at %d", key, ours)
				continue
			}
			found[key] = true
			theirs, err := strconv.Atoi(m[1])
			if err != nil {
				t.Fatal(err)
			}
			if theirs != ours {
				t.Errorf("LIMITS.%s is %d in the SDK and %d here — an app would validate its own "+
					"view and have it refused by the host", key, theirs, ours)
			}
		}

		// The other direction: a cap added to the SDK that this host does not
		// enforce is a string an app believes is bounded and is not.
		for _, m := range regexp.MustCompile(`(?m)^\s*([a-zA-Z]+):\s*[0-9*\s]+,`).FindAllStringSubmatch(block, -1) {
			key := m[1]
			if key == "bytes" {
				continue // checked below, it is an expression not a literal
			}
			if !found[key] {
				t.Errorf("the SDK caps %s and this host does not — an app would be told its view "+
					"is fine by a validator that is not the one that decides", key)
			}
		}

		if !strings.Contains(block, "bytes: 16 * 1024") {
			t.Errorf("LIMITS.bytes is not 16 * 1024 in the SDK; this host refuses above %d",
				apps.ViewCaps.Bytes)
		}
	})

	t.Run("block kinds", func(t *testing.T) {
		m := regexp.MustCompile(`export const BLOCK_KINDS = \[(.*?)\] as const;`).FindStringSubmatch(src)
		if m == nil {
			t.Fatal("BLOCK_KINDS is not in the SDK in the shape this test reads")
		}
		var theirs []string
		for _, part := range strings.Split(m[1], ",") {
			part = strings.TrimSpace(strings.Trim(strings.TrimSpace(part), `"`))
			if part != "" {
				theirs = append(theirs, part)
			}
		}
		var ours []string
		for _, k := range apps.BlockKinds {
			ours = append(ours, string(k))
		}
		// Order matters as much as membership: both sides list them in the order
		// APP-PLATFORM.md §7 names them, and a reviewer comparing the two files
		// should not have to sort them first.
		if strings.Join(theirs, ",") != strings.Join(ours, ",") {
			t.Errorf("the vocabulary is [%s] in the SDK and [%s] here.\n"+
				"A kind this host does not know is a view it refuses whole; a kind the SDK does "+
				"not know is one no app can build",
				strings.Join(theirs, ", "), strings.Join(ours, ", "))
		}
	})

	t.Run("only speak costs a scope", func(t *testing.T) {
		block := between(t, src, "export const BLOCK_SCOPES", "});")
		theirs := map[string]string{}
		for _, m := range regexp.MustCompile(`(?m)^\s*([a-z]+):\s*"([^"]+)"`).FindAllStringSubmatch(block, -1) {
			theirs[m[1]] = m[2]
		}
		ours := map[string]string{}
		for kind, scope := range apps.BlockScopes {
			ours[string(kind)] = string(scope)
		}
		if fmt.Sprint(theirs) != fmt.Sprint(ours) {
			t.Errorf("BLOCK_SCOPES is %v in the SDK and %v here.\n"+
				"This is the map that decides whether a block reaches someone's ear without a "+
				"permission having been asked for", theirs, ours)
		}
	})

	t.Run("vocabulary version", func(t *testing.T) {
		m := regexp.MustCompile(`export const VOCABULARY_VERSION = ([0-9]+);`).FindStringSubmatch(src)
		if m == nil {
			t.Fatal("VOCABULARY_VERSION is not in the SDK")
		}
		if m[1] != strconv.Itoa(apps.VocabularyVersion) {
			t.Errorf("the SDK writes vocabulary %s and this host draws %d, so this host would "+
				"refuse every view every app sends", m[1], apps.VocabularyVersion)
		}
	})

	t.Run("the frame name", func(t *testing.T) {
		if !strings.Contains(src, `export const RENDER_FRAME = "ui.render"`) {
			t.Error(`RENDER_FRAME is not "ui.render" in the SDK; the frame this host sends would ` +
				`be one no phone is listening for`)
		}
	})
}

func between(t *testing.T, src, start, end string) string {
	t.Helper()
	i := strings.Index(src, start)
	if i < 0 {
		t.Fatalf("the SDK does not contain %q, so this test cannot check it", start)
	}
	rest := src[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("no %q after %q in the SDK", end, start)
	}
	return rest[:j]
}

// --------------------------------------------------------------- the parser --

func card() apps.Block {
	return apps.Block{Kind: apps.BlockCard, Title: "Standup", Body: "Three things."}
}

func view(blocks ...apps.Block) apps.View {
	return apps.View{Vocabulary: apps.VocabularyVersion, Blocks: blocks}
}

func TestAViewFromTheFutureIsRefusedWhole(t *testing.T) {
	_, err := apps.ParseView(apps.View{Vocabulary: 2, Blocks: []apps.Block{card()}})
	if err == nil {
		t.Fatal("a vocabulary this host does not draw was accepted")
	}
	// The reason is the point: drawing the recognised parts of a future view is
	// how a confirmation reaches a screen with the wrong buttons.
	if !strings.Contains(err.Error(), "vocabulary 2") {
		t.Errorf("the error does not name the version it refused: %v", err)
	}
}

func TestEveryFieldThatDoesNotBelongToItsKindIsCleared(t *testing.T) {
	// A list carrying a Question. Nothing rejects this at the type level —
	// Block is one struct for four kinds — so the parser is what makes it safe
	// to hand a Block to a renderer.
	dirty := apps.Block{
		Kind:     apps.BlockList,
		Title:    "Today",
		Items:    []apps.ListItem{{Title: "Ship it"}},
		Question: "Send the email?",
		Text:     "hello",
	}
	got, err := apps.ParseView(view(dirty))
	if err != nil {
		t.Fatal(err)
	}
	b := got.Blocks[0]
	if b.Question != "" || b.Text != "" {
		t.Errorf("a list survived parsing with question=%q text=%q — the phone would receive "+
			"fields its renderer has no reason to expect", b.Question, b.Text)
	}
	if b.Title != "Today" || len(b.Items) != 1 {
		t.Errorf("parsing cleared something it should have kept: %+v", b)
	}
}

func TestAFieldOnTheWrongKindIsAnErrorAndNotIgnored(t *testing.T) {
	// Off the wire this is the check Go's decoder cannot do for us: an unknown
	// key is discarded silently, and the app believes it will be drawn.
	raw := []byte(`{"vocabulary":1,"blocks":[{"kind":"speak","text":"hi","body":"drawn?"}]}`)
	_, err := apps.ParseViewJSON(raw)
	if err == nil {
		t.Fatal("a speak block with a body was accepted; the app would think the body renders")
	}
	if !strings.Contains(err.Error(), "body") {
		t.Errorf("the error does not name the field: %v", err)
	}
}

func TestAViewAsksAtMostOneQuestionAndSpeaksAtMostOnce(t *testing.T) {
	q := apps.Block{Kind: apps.BlockConfirm, Question: "Send it?"}
	if _, err := apps.ParseView(view(q, q)); err == nil {
		t.Error("two confirmations were accepted, and the answer would be keyed to one frame")
	}
	s := apps.Block{Kind: apps.BlockSpeak, Text: "Done."}
	if _, err := apps.ParseView(view(s, s)); err == nil {
		t.Error("two spoken blocks were accepted, and they would talk over each other")
	}
}

func TestControlCharactersAreRefusedRatherThanStripped(t *testing.T) {
	b := card()
	b.Title = "Stand\x1b[31mup"
	_, err := apps.ParseView(view(b))
	if err == nil {
		t.Fatal("an escape sequence in a title was accepted")
	}
	// Stripping would hide from the app that it sent something it did not mean
	// to, which is how a terminal escape ends up being someone else's bug.
	if !strings.Contains(err.Error(), "control character") {
		t.Errorf("wrong reason: %v", err)
	}

	// A newline is fine in the two fields that are paragraphs and nowhere else.
	body := card()
	body.Body = "one\ntwo"
	if _, err := apps.ParseView(view(body)); err != nil {
		t.Errorf("a newline in a card body is allowed and was refused: %v", err)
	}
	title := card()
	title.Title = "one\ntwo"
	if _, err := apps.ParseView(view(title)); err == nil {
		t.Error("a newline in a title was accepted; a title is one line")
	}
}

func TestLengthIsCountedTheWayTheSDKCountsIt(t *testing.T) {
	// The SDK measures with String.length, which is UTF-16 code units. Counting
	// runes here would accept a view the SDK refused and counting bytes would
	// refuse one it accepted — either way an emoji moves the boundary and the
	// two validators disagree about the same string.
	b := card()
	b.Title = strings.Repeat("🙂", apps.ViewCaps.CardTitle/2)
	if _, err := apps.ParseView(view(b)); err != nil {
		t.Errorf("a title of exactly the limit in UTF-16 units was refused: %v", err)
	}
	b.Title = strings.Repeat("🙂", apps.ViewCaps.CardTitle/2+1)
	if _, err := apps.ParseView(view(b)); err == nil {
		t.Error("a title one code unit over the limit was accepted")
	}
}

func TestAnEmptyListIsRefused(t *testing.T) {
	_, err := apps.ParseView(view(apps.Block{Kind: apps.BlockList, Title: "Today"}))
	if err == nil {
		t.Fatal("an empty list was accepted; it draws as a heading with nothing under it")
	}
}

func TestAViewOverTheByteCapIsRefusedNotTruncated(t *testing.T) {
	// Eight cards, each with the largest body and the most fields it is allowed.
	// Every string is inside its own limit, so this is the cap that catches a
	// view that is legal block by block and too big as a whole — which is the
	// only way an app reaches it without noticing.
	var blocks []apps.Block
	for i := 0; i < apps.ViewCaps.Blocks; i++ {
		b := card()
		b.Body = strings.Repeat("x", apps.ViewCaps.CardBody)
		for j := 0; j < apps.ViewCaps.CardFields; j++ {
			b.Fields = append(b.Fields, apps.Field{
				Label: strings.Repeat("l", apps.ViewCaps.FieldLabel),
				Value: strings.Repeat("v", apps.ViewCaps.FieldValue),
			})
		}
		blocks = append(blocks, b)
	}
	_, err := apps.ParseView(view(blocks...))
	if err == nil {
		t.Fatal("a view past the byte cap was accepted")
	}
	if !strings.Contains(err.Error(), "bytes") {
		t.Errorf("wrong reason: %v", err)
	}
}

func TestSpeakingCostsThePermissionAndDrawingDoesNot(t *testing.T) {
	drawing := view(card())
	if err := apps.CheckScopes(drawing, nil); err != nil {
		t.Errorf("a card needed a scope: %v.\nDrawing on the phone of the person who installed "+
			"the app reaches nothing of theirs", err)
	}

	speaking := view(apps.Block{Kind: apps.BlockSpeak, Text: "Tests are green."})
	err := apps.CheckScopes(speaking, []apps.Scope{apps.ScopeMemoryRead})
	if err == nil {
		t.Fatal("an app without glasses.speaker spoke through a view")
	}
	if !strings.Contains(err.Error(), string(apps.ScopeGlassesSpeaker)) {
		t.Errorf("the error does not name the permission: %v", err)
	}
	if err := apps.CheckScopes(speaking, []apps.Scope{apps.ScopeGlassesSpeaker}); err != nil {
		t.Errorf("a granted app was refused: %v", err)
	}
}

func TestTheTextProjectionIsForSomethingWithNoScreen(t *testing.T) {
	v, err := apps.ParseView(view(
		apps.Block{Kind: apps.BlockCard, Title: "Standup", Fields: []apps.Field{{Label: "Blocked", Value: "no"}}},
		apps.Block{Kind: apps.BlockList, Title: "Today", Items: []apps.ListItem{{Title: "Ship", Detail: "4pm"}}},
	))
	if err != nil {
		t.Fatal(err)
	}
	got := v.Text()
	for _, want := range []string{"Standup", "Blocked: no", "Today", "Ship", "4pm"} {
		if !strings.Contains(got, want) {
			t.Errorf("the projection an agent reads is missing %q:\n%s", want, got)
		}
	}
}

func TestAParsedViewSerialisesWithoutTheFieldsItCleared(t *testing.T) {
	v, err := apps.ParseView(view(apps.Block{
		Kind: apps.BlockSpeak, Text: "Done.", Title: "leftover", Question: "leftover",
	}))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "leftover") {
		t.Errorf("a cleared field reached the wire: %s", b)
	}
}
