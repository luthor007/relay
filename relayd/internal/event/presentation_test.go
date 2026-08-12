package event_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
	"unicode"
)

// PRODUCT.md §6b is the only architectural rule v1 carries forward on behalf of
// a product that does not exist yet:
//
//	"Keep presentation out of the event model. Nothing in relayd decides *how*
//	an event surfaces — no 'speak this' baked into the core. Rendering is a
//	separate layer that today has one backend (voice) and tomorrow has two."
//
// It is cheap now and expensive to retrofit, and the difference it buys is
// whether v2's display is a new render target or a rewrite. Nothing exercises
// it at runtime, so these two tests are the only thing standing between the
// rule and a plausible-looking field on a struct someone adds in a hurry.
//
// They read this package's own source. That is unusual and it is deliberate:
// the property is about what the code *says*, not what it does, so there is
// nothing to observe by running it. A unit test cannot catch the absence of a
// rule any more than it can catch the absence of a call.
//
// Two things they deliberately do NOT do:
//
//   - They scan identifiers only, never doc comments. event.go and
//     needsinput.go discuss speech at length — "never spoken, on any runtime",
//     "Name is spoken verbatim" — and that prose is how a reader learns which
//     events reach an ear. Saying what a field is for is not deciding how it
//     surfaces.
//   - They stop at this package. internal/summarize names voice everywhere and
//     is supposed to: it is §6b's one backend, not a violation. The rule is
//     directional — the voice backend may name voice; nothing it is downstream
//     of may.

// modality is the vocabulary that would mean this package had grown an opinion
// about appearance: a channel to arrive on, or a way to look once it does.
//
// Kept to modality and appearance words on purpose. Generic content nouns are
// not here and must not be added — Title, Name, Target, Text, Prompt and
// Explanation are all carried observations, things a runtime told us, and
// event.ToolRef.Title already exists.
var modality = map[string]string{
	"speak":        "who says it out loud is the backend's business",
	"spoken":       "same",
	"speech":       "same",
	"say":          "same",
	"tts":          "a synthesiser is one backend of two",
	"voice":        "same",
	"audio":        "same",
	"sound":        "same",
	"chime":        "same",
	"vibrate":      "a haptic is a delivery channel",
	"haptic":       "same",
	"notify":       "whether it reaches a phone is a delivery decision",
	"notification": "same",
	"display":      "v2's backend must not be able to leak backwards into here",
	"screen":       "same",
	"hud":          "same",
	"glance":       "same",
	"render":       "the whole point: rendering is the layer above this one",
	"draw":         "same",
	"paint":        "same",
	"pixel":        "a coordinate is not an observation",
	"glyph":        "same",
	"font":         "same",
	"colour":       "appearance",
	"color":        "appearance",
	"style":        "appearance",
	"theme":        "appearance",
	"bold":         "appearance",
	"italic":       "appearance",
	"markdown":     "a text encoding is a rendering choice about observed text",
	"html":         "same",
	"css":          "same",
	"emoji":        "same",
	"icon":         "same",
	"badge":        "same",
	"banner":       "same",
	"toast":        "same",
	"layout":       "same",
	"silent":       "silence is something a backend does, not something observed",
}

// exempt names identifiers that read like presentation and are not. Every entry
// carries its reason, so the list cannot grow quietly — an exemption without an
// argument is a violation with a note attached.
var exempt = map[string]string{
	// Ping is semantic urgency, not modality: none / informational / blocking.
	// A v2 display backend wants exactly these three values and would reach
	// different conclusions from them than the voice backend does. It decides
	// how loudly something matters, never how it arrives.
	"Ping": "urgency, not a channel — a display backend wants the same three values",

	// OptionKind is a value the runtime handed us (ACP's PermissionOptionKind,
	// and the same four shapes on the other two). Its doc calls it "the agent's
	// UI hint" because that is what ACP calls it; Standing() derives a
	// permission semantic from it, not an appearance.
	"OptionKind": "carried verbatim from the runtime; Standing() is a permission rule",
}

func TestTheEventModelCarriesNoPresentation(t *testing.T) {
	pkg := parsePackage(t, ".")

	for _, file := range pkg {
		ast.Inspect(file, func(n ast.Node) bool {
			switch d := n.(type) {
			case *ast.TypeSpec:
				check(t, d.Name.Name, "type")
			case *ast.ValueSpec:
				for _, name := range d.Names {
					check(t, name.Name, "const or var")
				}
			case *ast.FuncDecl:
				check(t, d.Name.Name, "func")
			case *ast.Field:
				for _, name := range d.Names {
					check(t, name.Name, "field")
				}
				// The type matters as much as the name: a field called Hint
				// whose type is a render.Style is the same violation wearing a
				// duller name.
				check(t, exprText(d.Type), "field type")
			}
			return true
		})
	}
}

func check(t *testing.T, ident, what string) {
	t.Helper()
	if _, ok := exempt[ident]; ok {
		return
	}
	for _, w := range words(ident) {
		if why, bad := modality[w]; bad {
			t.Errorf("internal/event %s %q names a presentation concern (%q: %s).\n"+
				"PRODUCT.md §6b: the event model records what happened; how it surfaces "+
				"belongs to the render layer, which today is the voice backend and in v2 "+
				"is also a display. If this really is an observation a runtime reported, "+
				"add it to the exempt map above with the reason.",
				what, ident, w, why)
		}
	}
}

// TestTheEventModelDependsOnNothingThatRenders is the structural half, and it
// is the one that cannot be argued with.
//
// The vocabulary test above is a tripwire and a blunt one; this asserts a
// property. internal/event imports nothing from this module at all, so it sits
// underneath every other package and no render backend can ever be upstream of
// it — not summarize, not api, not whatever v2's display layer is called. That
// is what makes "rendering is a separate layer" a fact about the build graph
// rather than a convention people remember.
//
// The stdlib allowlist is the tripwire half. Widening it is allowed and should
// be a deliberate edit with a reason in the commit, which is exactly the moment
// to notice that html/template has appeared in the event model.
func TestTheEventModelDependsOnNothingThatRenders(t *testing.T) {
	allowed := map[string]string{
		"context": "NeedsInput.Reply carries a context back into the runtime",
		"errors":  "the three sentinel errors on a question",
		"fmt":     "wrapping those errors with the option that was not offered",
		"sync":    "NeedsInput is answered from one goroutine and read from another",
		"time":    "Meta.At, Usage durations, NeedsInput.Deadline",
	}

	for name, file := range parsePackage(t, ".") {
		for _, spec := range file.Imports {
			path := strings.Trim(spec.Path.Value, `"`)

			if strings.HasPrefix(path, "github.com/luthor007/relay/") {
				t.Errorf("%s imports %q. internal/event is the bottom of the graph: "+
					"every render backend is downstream of it, and one import the "+
					"other way is how a rendering decision reaches the core.", name, path)
				continue
			}
			if _, ok := allowed[path]; !ok {
				t.Errorf("%s imports %q, which is not on the event model's allowlist. "+
					"Widening it is fine and is meant to be deliberate — but check "+
					"first that what you are reaching for is not a rendering concern "+
					"(PRODUCT.md §6b).", name, path)
			}
		}
	}
}

// ------------------------------------------------------------------ tools --

func parsePackage(t *testing.T, dir string) map[string]*ast.File {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		// Non-test files only. A test file may name anything it likes; the rule
		// is about what the package ships.
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", dir, err)
	}
	out := map[string]*ast.File{}
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			out[name] = file
		}
	}
	if len(out) == 0 {
		t.Fatalf("parsed no files in %s — the scan would pass vacuously", dir)
	}
	return out
}

func exprText(e ast.Expr) string {
	var b strings.Builder
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			b.WriteString(id.Name)
			b.WriteByte(' ')
		}
		return true
	})
	return b.String()
}

// words splits an identifier into lower-cased words on camel-case boundaries.
//
// Substring matching was the obvious implementation and it is wrong: "draw" is
// inside NeedsInput.Withdraw, which is how a question is resolved from the
// runtime's side and has nothing to do with a screen. Splitting on word
// boundaries costs fifteen lines and removes a whole class of false positive
// that would otherwise get the rule weakened rather than the list fixed.
func words(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = nil
		}
	}
	rs := []rune(s)
	for i, r := range rs {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
		case unicode.IsUpper(r):
			// A new word starts at any upper-case run boundary, so both
			// PlanUpdated and the acronym in TTSVoice split correctly.
			if i > 0 && (unicode.IsLower(rs[i-1]) ||
				(i+1 < len(rs) && unicode.IsUpper(rs[i-1]) && unicode.IsLower(rs[i+1]))) {
				flush()
			}
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return out
}
