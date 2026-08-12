package bus_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
	"unicode"
)

// PRODUCT.md §6b: "Nothing in relayd decides *how* an event surfaces — no
// 'speak this' baked into the core. Rendering is a separate layer that today
// has one backend (voice) and tomorrow has two."
//
// internal/event never had that problem; it imports nothing and observes
// everything. This package did. bus.Ping carried `Speak bool`, `Notify bool`
// and `Silent bool` — literally the phrase §6b forbids, set by the ping policy,
// at the exact fan-out point (bus.Delivery) where a v2 display backend attaches.
// Adding a display meant adding a fourth boolean here and re-deciding the whole
// policy inside the policy layer, which is the retrofit §6b exists to avoid.
//
// So bus.Ping now carries the two facts the policy established — Gap and Quiet
// — and internal/api.speaks turns them into a decision on behalf of the one
// backend that exists. This test is what stops a fourth boolean appearing.

// deliveryChannel is narrower than the appearance vocabulary internal/event
// screens for, and narrower on purpose. bus is the ping *policy*: its job is to
// decide whether the user is disturbed at all and how urgently, which is a real
// decision it must keep making. What it must not decide is which device the
// disturbance comes out of, or what it sounds like when it does.
var deliveryChannel = map[string]string{
	"speak":        "the voice backend's verb; internal/api.speaks owns it",
	"spoken":       "same",
	"speech":       "same",
	"say":          "same",
	"tts":          "same",
	"voice":        "one backend of an eventual two",
	"audio":        "same",
	"sound":        "same",
	"chime":        "same",
	"silent":       "silence is what a backend does with Quiet, not a fact about a ping",
	"notify":       "whether it reaches a phone is the transport's decision",
	"notification": "same",
	"vibrate":      "a haptic is a delivery channel",
	"haptic":       "same",
	"display":      "v2's backend; adding a field for it here is the retrofit §6b forbids",
	"screen":       "same",
	"hud":          "same",
	"glance":       "same",
	"render":       "rendering is the layer above this one",
	"draw":         "same",
	"banner":       "same",
	"toast":        "same",
	"icon":         "same",
	"badge":        "same",
	"colour":       "appearance",
	"color":        "appearance",
	"markdown":     "a text encoding is a rendering choice",
	"html":         "same",
}

// TestThePingCarriesNoDeliveryChannel is the assertion the fix exists for. On
// the tree before it, this fails three times over — Speak, Notify, Silent.
//
// Ping's surviving fields are all either observations (ID, At, Sessions,
// Events, Ask, Line) or policy conclusions that any backend would want
// (Class, Repeat, Consequential, Gap, Quiet). Class is not an exception being
// tolerated: blocking-versus-informational is semantic urgency, and a display
// backend reads it just as eagerly as a speaker does — it simply reaches a
// different conclusion, which is the whole shape §6b is protecting.
func TestThePingCarriesNoDeliveryChannel(t *testing.T) {
	spec := findStruct(t, "Ping")

	for _, field := range spec.Fields.List {
		for _, name := range field.Names {
			flag(t, name.Name, "field")
		}
		// The type as well as the name. A field called Hint whose type is a
		// render.Style is the same violation with a duller name, and an
		// embedded type needs checking precisely because it has no name.
		flag(t, exprText(field.Type), "field type")
	}
}

func flag(t *testing.T, ident, what string) {
	t.Helper()
	for _, w := range words(ident) {
		if why, bad := deliveryChannel[w]; bad {
			t.Errorf("bus.Ping %s %q names a delivery channel (%q: %s).\n"+
				"PRODUCT.md §6b: the ping policy decides whether and how urgently "+
				"the user is disturbed; which device it comes out of belongs to the "+
				"render layer. Carry the fact your policy established and let "+
				"internal/api.speaks — and whatever v2's display backend is called — "+
				"draw its own conclusion from it.", what, ident, w, why)
		}
	}
}

// TestTheRenderBackendsAreDownstreamOfThePolicy is the structural half, and it
// is the one that survives someone renaming a field to dodge the vocabulary.
//
// internal/summarize is §6b's sanctioned single backend, not a violation: its
// doc says it "turns a turn's events into the two sentences someone hears",
// every constant in it is denominated in CharsPerSecond, and Clean() strips
// markdown because a TTS engine reads the asterisks aloud. What makes that
// legitimate rather than a leak is the direction of the arrow — summarize
// imports event, and neither event nor bus imports summarize. The voice backend
// may name voice; nothing it is downstream of may name it back.
//
// internal/api is deliberately not asserted on here. It is the render layer's
// transport, so it is *supposed* to know about speaking — api.Deliver builds
// the Speak frame and api.speaks decides when — and it already imports
// internal/voice for the installer's TTS-provider checks on the runtimes
// screen, which is configuration rather than rendering. Asserting anything
// about api's imports would be asserting the wrong rule.
func TestTheRenderBackendsAreDownstreamOfThePolicy(t *testing.T) {
	backends := []string{
		"github.com/luthor007/relay/relayd/internal/summarize",
		"github.com/luthor007/relay/relayd/internal/voice",
	}

	for _, dir := range []string{".", "../event"} {
		for name, file := range parsePackage(t, dir) {
			for _, spec := range file.Imports {
				path := strings.Trim(spec.Path.Value, `"`)
				for _, backend := range backends {
					if path == backend {
						t.Errorf("%s imports %q. A render backend must stay downstream "+
							"of the event model and the ping policy; one import the other "+
							"way and v2's display is a rewrite rather than a second "+
							"backend (PRODUCT.md §6b).", name, path)
					}
				}
			}
		}
	}
}

// ------------------------------------------------------------------ tools --

func findStruct(t *testing.T, name string) *ast.StructType {
	t.Helper()
	for _, file := range parsePackage(t, ".") {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name.Name != name {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					t.Fatalf("bus.%s is no longer a struct", name)
				}
				return st
			}
		}
	}
	t.Fatalf("bus.%s not found — the scan would pass vacuously", name)
	return nil
}

func parsePackage(t *testing.T, dir string) map[string]*ast.File {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
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

// words splits an identifier on camel-case boundaries. Substring matching was
// the obvious implementation and it is wrong: "draw" sits inside Withdraw.
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
