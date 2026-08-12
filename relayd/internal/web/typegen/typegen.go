// Package typegen turns relayd's wire structs into the console's TypeScript
// types, so a field rename breaks a build rather than a screen.
//
// DASHBOARD.md §5 puts one API behind both deployments. That only holds if the
// client and the server agree on the shapes, and the cheapest way to keep two
// languages in agreement is to stop maintaining one of them by hand. So
// `console/src/api/types.ts` is generated from the Go structs themselves,
// checked in, and verified by [TestGeneratedTypesAreCurrent]: rename
// `SessionSummary.Blocked` and the Go test fails with a diff; regenerate and
// `tsc` fails in every screen that read `.blocked`. Neither failure waits for
// someone to open the sessions view.
//
// It reflects rather than parses because reflection sees what `encoding/json`
// sees. A struct that embeds another, a field with no tag, a `time.Time`, a
// `*float64` that is null rather than zero — all of those are decided by the
// marshaller, and a source parser would have to re-implement its rules to get
// them right. The one exception is `api.ssePingView`, which is unexported and
// therefore unreachable by reflection from outside its package; that one is
// read from source, and it is the only thing here that is.
//
// # Adding a type
//
// Put it in [Roots]. One line, in the order it should appear. Run
// `go generate ./internal/web/...` and commit the result. Nothing else in this
// file needs to change — the walker follows the field graph on its own.
//
// This package must never be imported by relayd itself. It exists so that a
// build machine can regenerate the console's types, and it imports
// internal/api, which is free to import internal/web in return.
package typegen

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/api"
	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/registry"
	"github.com/luthor007/relay/relayd/internal/store"
)

// OutputPath is where the generated file belongs, relative to the repo root.
const OutputPath = "console/src/api/types.ts"

// Root is one type the console is entitled to see, and the name it gets in
// TypeScript.
type Root struct {
	Value any
	Name  string
	// Doc is the comment written above the interface. The Go doc comment is not
	// reachable by reflection, and a type the screens read deserves a sentence.
	Doc string
}

// Roots is every shape that crosses the wire to the console, in the order they
// are emitted.
//
// This list is deliberately short. It holds what `relayd/internal/api` actually
// serves today and nothing it might serve later: a generated type for an
// endpoint that does not exist is a guess wearing the costume of a contract,
// and the screen written against it fails at runtime rather than at build time,
// which is the whole thing this package exists to prevent.
func Roots() []Root {
	return []Root{
		{Value: api.Health{}, Name: "Health",
			Doc: "GET /v1/health — DASHBOARD.md §3.5's runtimes-and-health screen."},
		{Value: api.SessionList{}, Name: "SessionList",
			Doc: "GET /v1/sessions, and the SSE `sessions` opening frame."},
		{Value: api.SessionDetail{}, Name: "SessionDetail",
			Doc: "GET /v1/sessions/{id}."},
		{Value: api.ConfirmRequest{}, Name: "ConfirmRequest",
			Doc: "A session blocked on a human, carried inside an SSE `ping`."},
		{Value: api.ConfirmResolved{}, Name: "ConfirmResolved",
			Doc: "Retracts a ConfirmRequest whose question is already answered."},
		{Value: registry.Change{}, Name: "SessionChange",
			Doc: "SSE `session` — one row moved.\n\n" +
				"Note the shape: registry.Change embeds store.Session, which carries no\n" +
				"json tags, so this frame's session fields are Go names (`ID`, `LastActive`)\n" +
				"and its timestamps are RFC3339 strings — not the lower_snake_case,\n" +
				"unix-millis SessionSummary that GET /v1/sessions returns. Rendering an SSE\n" +
				"row therefore means re-fetching or mapping, never assigning."},
		{Value: registry.Incident{}, Name: "Incident",
			Doc: "SSE `incident`, and the tail of GET /v1/health."},
		{Value: api.TranscriptChunk{}, Name: "TranscriptChunk",
			Doc: "GET /v1/sessions/{id}/transcript — one window into the runtime's own\n" +
				"file. MEMORY.md §3 keeps the transcript in place and stores a pointer, so\n" +
				"this is read on demand and never held."},
		{Value: api.CredentialList{}, Name: "CredentialList",
			Doc: "GET /v1/credentials — DASHBOARD.md §3.2.\n\n" +
				"CredentialView carries `last_four` and has no field a secret could live\n" +
				"in, mirroring vault.Entry. Render it through `secretField`, which has the\n" +
				"same property, and never widen either."},
		{Value: api.Validation{}, Name: "Validation",
			Doc: "POST /v1/credentials/{id}/validate — MEMORY.md §6's one real call."},
		{Value: api.Proposal{}, Name: "Proposal",
			Doc: "GET /v1/credentials/proposals — \"I found what looks like a Twilio token\n" +
				"in a session from March\". Nothing is captured silently; this is the queue\n" +
				"that has to be accepted or dismissed."},
		{Value: api.FactList{}, Name: "FactList",
			Doc: "GET /v1/facts — DASHBOARD.md §3.3 and MEMORY.md §5.\n\n" +
				"`superseded` arrives only when asked for, because §3.3 puts it behind a\n" +
				"toggle. `counts.no_evidence` above zero is a bug in the extractor rather\n" +
				"than something to render quietly: §5 deletes an unevidenced fact."},
		{Value: api.ConnectorList{}, Name: "ConnectorList",
			Doc: "GET /v1/connectors — DASHBOARD.md §3.4 and ORCHESTRATOR.md §4b."},
		{Value: api.ConnectorProposalList{}, Name: "ConnectorProposalList",
			Doc: "GET /v1/connectors/proposals — ORCHESTRATOR.md §4b.\n\n" +
				"`available` false means nothing on this machine can propose anything, which\n" +
				"is a different statement from an empty list and is rendered differently.\n" +
				"`ConnectorProposal.access` is always \"read\": §4b makes the write half a\n" +
				"second decision, so there is no field here for it to arrive in and the\n" +
				"accept endpoint takes no half as an argument."},
		{Value: api.ConnectorGrantResult{}, Name: "ConnectorGrantResult",
			Doc: "POST /v1/connectors/proposals/{connector}/accept — the read half, granted.\n\n" +
				"`sessions` and `note` are §4b's catch: some runtimes enumerate their tools\n" +
				"once per session, so a grant made now may not reach an agent already\n" +
				"running. Render the note, or the user wonders why what they just connected\n" +
				"is invisible."},
		{Value: api.RevokeResult{}, Name: "RevokeResult",
			Doc: "POST /v1/connectors/{id}/revoke — one revoke, all five runtimes, and a\n" +
				"per-runtime result so a partial failure is visible rather than averaged."},
		{Value: api.AuditList{}, Name: "AuditList",
			Doc: "GET /v1/audit — DASHBOARD.md §4.\n\n" +
				"`intact` is the hash chain verifying. A false there is worth more than\n" +
				"every entry in the list and must not be rendered as a footnote."},
		{Value: api.BillingLink{}, Name: "BillingLink",
			Doc: "GET /v1/billing/portal — cloud only. DASHBOARD.md §3.6: link to Stripe's\n" +
				"customer portal, do not rebuild it."},
		{Value: api.ConsoleEvent{}, Name: "ConsoleEvent",
			Doc: "SSE `credential` | `connector` | `fact` | `audit` | `probe`.\n\n" +
				"Deliberately thin: it carries an id and never a credential, so the console\n" +
				"re-reads the list — the same listing every other reader gets, and therefore\n" +
				"the same one that cannot contain a secret."},
		{Value: api.ErrorPayload{}, Name: "ErrorPayload",
			Doc: "Every non-2xx body from the API."},
	}
}

// unions are named string types whose values are worth having in the type
// system: a status pill with a `string` prop accepts a typo.
//
// The values are written as conversions of the package constants, so renaming
// one stops this file compiling. Adding a constant is not caught, which is
// stated here rather than implied — the fallback in the pill component is what
// covers that case.
func unions() map[reflect.Type][]string {
	return map[reflect.Type][]string{
		reflect.TypeOf(store.SessionRunning): {
			string(store.SessionRunning), string(store.SessionAwaiting),
			string(store.SessionIdle), string(store.SessionClosed),
		},
		reflect.TypeOf(registry.ChangeAdded): {
			string(registry.ChangeAdded), string(registry.ChangeUpdated), string(registry.ChangeClosed),
		},
		reflect.TypeOf(registry.IncidentStartFailed): {
			string(registry.IncidentStartFailed), string(registry.IncidentSessionExited),
			string(registry.IncidentSessionFailed), string(registry.IncidentRestarted),
			string(registry.IncidentRestartedFresh), string(registry.IncidentRestartFailed),
			string(registry.IncidentOrphanDetached),
		},
		reflect.TypeOf(event.OptionAllowOnce): {
			string(event.OptionAllowOnce), string(event.OptionAllowAlways),
			string(event.OptionRejectOnce), string(event.OptionRejectAlways),
			string(event.OptionOther),
		},
	}
}

// sourceTypes are structs reflection cannot reach because they are unexported.
// Each is read from the Go source instead.
//
// All three are unexported for a good reason — a request body and one SSE view
// are api's own business — and all three are shapes the console must produce or
// parse exactly. Reading them from source keeps the rename-breaks-the-build
// property without asking another package to export something for our benefit.
// If this list grows past a handful, the right fix is a wire type in api, not a
// longer list here.
type sourceType struct {
	Dir  string // relative to the repo root
	Go   string // the Go type name
	Name string // the TypeScript name
	Doc  string
}

func sourceTypes() []sourceType {
	const dir = "relayd/internal/api"
	return []sourceType{
		{
			Dir: dir, Go: "ssePingView", Name: "SSEPing",
			Doc: "SSE `ping` — the user is about to hear from us.\n\n" +
				"Read from the Go source rather than reflected: api.ssePingView is\n" +
				"unexported. Renaming a field still fails the contract test, because that\n" +
				"test re-reads the same source.",
		},
		{
			Dir: dir, Go: "sendTurnRequest", Name: "SendTurnRequest",
			Doc: "POST /v1/sessions/{id}/turns.",
		},
		{
			Dir: dir, Go: "addCredentialRequest", Name: "AddCredentialRequest",
			Doc: "POST /v1/credentials.\n\n" +
				"`secret` is inbound only — it is never echoed, never logged and never\n" +
				"reachable again through this API. It is the one plaintext field in this\n" +
				"file and the only place the console may hold one: read it out of the form,\n" +
				"post it, and let the input go.",
		},
		{
			Dir: dir, Go: "rotateRequest", Name: "RotateCredentialRequest",
			Doc: "POST /v1/credentials/{id}/rotate.\n\n" +
				"Rotation keeps the id rather than adding a row, so a config that says\n" +
				"vault:<id> does not need editing because somebody rotated the key.",
		},
		{
			Dir: dir, Go: "proposalRequest", Name: "ProposalDecisionRequest",
			Doc: "POST /v1/credentials/proposals/{id}/accept | /dismiss.",
		},
		{
			Dir: dir, Go: "editFactRequest", Name: "EditFactRequest",
			Doc: "PATCH /v1/facts/{id} — MEMORY.md §5's editable, not just deletable.\n\n" +
				"Every field is optional so that \"set the text to empty\" is distinguishable\n" +
				"from \"leave the text alone\". Send only what changed.",
		},
		{
			Dir: dir, Go: "answerRequest", Name: "AnswerRequest",
			Doc: "POST /v1/sessions/{id}/answer — answering a blocked session.\n\n" +
				"`decision` is allow | deny | cancelled; empty means allow. `option` is\n" +
				"authoritative when the question carried options. ORCHESTRATOR.md §4b: an\n" +
				"option marked `standing` grants something beyond the action in front of\n" +
				"the user, so a console must never pre-select one.",
		},
	}
}

var timeType = reflect.TypeOf(time.Time{})

// Generate renders the TypeScript file.
func Generate() ([]byte, error) {
	g := &gen{
		names:  map[reflect.Type]string{},
		taken:  map[string]reflect.Type{},
		unions: unions(),
	}

	for _, r := range Roots() {
		t := reflect.TypeOf(r.Value)
		if t.Kind() != reflect.Struct {
			return nil, fmt.Errorf("typegen: root %s is %s, not a struct", r.Name, t.Kind())
		}
		if err := g.collect(t, r.Name, r.Doc); err != nil {
			return nil, err
		}
	}

	root, err := repoRoot()
	if err != nil {
		return nil, err
	}
	for _, st := range sourceTypes() {
		if err := g.collectSource(filepath.Join(root, st.Dir), st); err != nil {
			return nil, err
		}
	}

	return g.render()
}

// ------------------------------------------------------------------- walk --

type decl struct {
	name   string
	doc    string
	body   string
	isType bool // a type alias (union) rather than an interface
}

type gen struct {
	names  map[reflect.Type]string
	taken  map[string]reflect.Type
	unions map[reflect.Type][]string
	decls  []decl
}

// collect walks a struct and everything it reaches, emitting each named type
// once. name may be empty, in which case the Go name is used.
func (g *gen) collect(t reflect.Type, name, doc string) error {
	if _, done := g.names[t]; done {
		return nil
	}
	if name == "" {
		name = t.Name()
	}
	if name == "" {
		return fmt.Errorf("typegen: anonymous struct in the field graph: %s", t)
	}
	if prev, clash := g.taken[name]; clash && prev != t {
		return fmt.Errorf("typegen: %s and %s both want the TypeScript name %s; give one an explicit Root name", prev, t, name)
	}
	g.names[t] = name
	g.taken[name] = t

	// Reserve the slot before recursing so a self-referential struct terminates.
	idx := len(g.decls)
	g.decls = append(g.decls, decl{name: name, doc: doc})

	var b strings.Builder
	if err := g.fields(&b, t, ""); err != nil {
		return err
	}
	g.decls[idx].body = b.String()
	return nil
}

func (g *gen) fields(b *strings.Builder, t reflect.Type, prefix string) error {
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		jsonName, opts, _ := strings.Cut(tag, ",")
		omitempty := hasOpt(opts, "omitempty")

		// An embedded struct with no name of its own is flattened by
		// encoding/json, so it is flattened here too.
		if f.Anonymous && jsonName == "" && f.Type.Kind() == reflect.Struct && f.Type != timeType {
			if err := g.fields(b, f.Type, prefix); err != nil {
				return err
			}
			continue
		}
		if jsonName == "" {
			jsonName = f.Name
		}

		ts, nullable, err := g.tsType(f.Type)
		if err != nil {
			return fmt.Errorf("%s.%s: %w", t.Name(), f.Name, err)
		}

		q := ""
		switch {
		case omitempty:
			// encoding/json drops the key entirely, so it is optional and never
			// null.
			q = "?"
		case nullable:
			ts += " | null"
		}
		fmt.Fprintf(b, "%s  %s%s: %s;\n", prefix, quoteKey(jsonName), q, ts)
	}
	return nil
}

// tsType maps a Go type onto TypeScript. nullable reports whether a nil value
// of this type marshals to JSON null, which is the difference between a field
// a screen may read straight and one it has to check.
func (g *gen) tsType(t reflect.Type) (ts string, nullable bool, err error) {
	if t == timeType {
		// encoding/json writes RFC 3339. The API's own wire types use unix
		// millis instead; both appear, so both are typed honestly.
		return "string", false, nil
	}
	if vals, ok := g.unions[t]; ok {
		return g.union(t, vals), false, nil
	}

	switch t.Kind() {
	case reflect.Bool:
		return "boolean", false, nil
	case reflect.String:
		return "string", false, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "number", false, nil

	case reflect.Pointer:
		inner, _, err := g.tsType(t.Elem())
		return inner, true, err

	case reflect.Interface:
		if t.NumMethod() == 0 {
			return "unknown", false, nil
		}
		return "", false, fmt.Errorf("typegen: cannot express interface %s", t)

	case reflect.Slice, reflect.Array:
		// []byte marshals to a base64 string, not to an array of numbers.
		if t.Elem().Kind() == reflect.Uint8 {
			return "string", t.Kind() == reflect.Slice, nil
		}
		inner, innerNull, err := g.tsType(t.Elem())
		if err != nil {
			return "", false, err
		}
		if innerNull {
			inner = "(" + inner + " | null)"
		}
		return inner + "[]", t.Kind() == reflect.Slice, nil

	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return "", false, fmt.Errorf("typegen: map key %s is not a string", t.Key())
		}
		inner, innerNull, err := g.tsType(t.Elem())
		if err != nil {
			return "", false, err
		}
		if innerNull {
			inner += " | null"
		}
		return "Record<string, " + inner + ">", true, nil

	case reflect.Struct:
		if err := g.collect(t, "", ""); err != nil {
			return "", false, err
		}
		return g.names[t], false, nil
	}
	return "", false, fmt.Errorf("typegen: no TypeScript for %s (%s)", t, t.Kind())
}

func (g *gen) union(t reflect.Type, vals []string) string {
	name := t.Name()
	if _, done := g.names[t]; done {
		return name
	}
	g.names[t] = name
	g.taken[name] = t
	quoted := make([]string, len(vals))
	for i, v := range vals {
		quoted[i] = `"` + v + `"`
	}
	g.decls = append(g.decls, decl{
		name:   name,
		isType: true,
		body:   strings.Join(quoted, " | "),
	})
	return name
}

// ----------------------------------------------------------------- source --

// collectSource reads one unexported struct out of the Go source.
func (g *gen) collectSource(dir string, st sourceType) error {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("typegen: parse %s: %w", dir, err)
	}

	var found *ast.StructType
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				spec, ok := n.(*ast.TypeSpec)
				if !ok || spec.Name.Name != st.Go {
					return true
				}
				if s, ok := spec.Type.(*ast.StructType); ok {
					found = s
				}
				return false
			})
		}
	}
	if found == nil {
		return fmt.Errorf("typegen: %s not found in %s — was it renamed?", st.Go, dir)
	}

	var b strings.Builder
	for _, f := range found.Fields.List {
		tag := ""
		if f.Tag != nil {
			tag = reflect.StructTag(strings.Trim(f.Tag.Value, "`")).Get("json")
		}
		if tag == "-" {
			continue
		}
		jsonName, opts, _ := strings.Cut(tag, ",")
		omitempty := hasOpt(opts, "omitempty")

		ts, nullable, err := g.sourceExpr(f.Type)
		if err != nil {
			return fmt.Errorf("typegen: %s.%v: %w", st.Go, f.Names, err)
		}
		for _, n := range f.Names {
			if !n.IsExported() && jsonName == "" {
				return fmt.Errorf("typegen: %s.%s is unexported and untagged, so it does not marshal", st.Go, n.Name)
			}
			key := jsonName
			if key == "" {
				key = n.Name
			}
			q := ""
			out := ts
			switch {
			case omitempty:
				q = "?"
			case nullable:
				out += " | null"
			}
			fmt.Fprintf(&b, "  %s%s: %s;\n", quoteKey(key), q, out)
		}
	}

	g.decls = append(g.decls, decl{name: st.Name, doc: st.Doc, body: b.String()})
	return nil
}

func (g *gen) sourceExpr(e ast.Expr) (string, bool, error) {
	switch v := e.(type) {
	case *ast.Ident:
		switch v.Name {
		case "string":
			return "string", false, nil
		case "bool":
			return "boolean", false, nil
		case "int", "int8", "int16", "int32", "int64",
			"uint", "uint8", "uint16", "uint32", "uint64", "float32", "float64":
			return "number", false, nil
		}
		// A named type in the same package: it has to be one we already emitted,
		// otherwise the console would reference something that does not exist.
		for t, name := range g.names {
			if t.Name() == v.Name {
				return name, false, nil
			}
		}
		return "", false, fmt.Errorf("unknown named type %s — add it to Roots so it is emitted first", v.Name)
	case *ast.StarExpr:
		inner, _, err := g.sourceExpr(v.X)
		return inner, true, err
	case *ast.ArrayType:
		inner, _, err := g.sourceExpr(v.Elt)
		if err != nil {
			return "", false, err
		}
		return inner + "[]", true, nil
	case *ast.SelectorExpr:
		if pkg, ok := v.X.(*ast.Ident); ok && pkg.Name == "time" && v.Sel.Name == "Time" {
			return "string", false, nil
		}
	}
	return "", false, fmt.Errorf("cannot express %T", e)
}

// ----------------------------------------------------------------- render --

func (g *gen) render() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(header)
	for _, d := range g.decls {
		b.WriteString("\n")
		if d.doc != "" {
			for _, line := range strings.Split(d.doc, "\n") {
				if line == "" {
					b.WriteString("//\n")
					continue
				}
				b.WriteString("// " + line + "\n")
			}
		}
		if d.isType {
			fmt.Fprintf(&b, "export type %s = %s;\n", d.name, d.body)
			continue
		}
		fmt.Fprintf(&b, "export interface %s {\n%s}\n", d.name, d.body)
	}
	return b.Bytes(), nil
}

const header = `// Code generated by relayd/internal/web/typegen. DO NOT EDIT.
//
// Every shape the console reads from relayd, reflected out of the Go structs in
// relayd/internal/api so the two cannot drift. Editing this file by hand is
// undone by the next generate and caught by the Go test that compares them.
//
// Regenerate:  cd relayd && go generate ./internal/web/...
//
// Timestamps: the API's own wire types carry unix milliseconds as numbers.
// Types reached through registry or store carry RFC 3339 strings, because those
// structs marshal time.Time directly. The difference is real and is typed here
// rather than smoothed over.
`

// ------------------------------------------------------------------ misc --

func hasOpt(opts, want string) bool {
	for _, o := range strings.Split(opts, ",") {
		if o == want {
			return true
		}
	}
	return false
}

// quoteKey quotes a JSON key that is not a bare TypeScript identifier.
func quoteKey(k string) string {
	ok := k != ""
	for i, r := range k {
		isAlpha := r == '_' || r == '$' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		isDigit := r >= '0' && r <= '9'
		if !isAlpha && !(isDigit && i > 0) {
			ok = false
			break
		}
	}
	if ok {
		return k
	}
	return `"` + k + `"`
}

// repoRoot finds the checkout from this file's own compiled-in path, so the
// generator works from any working directory and the test does not have to
// guess how it was invoked.
func repoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("typegen: cannot locate this package on disk")
	}
	// .../relayd/internal/web/typegen/typegen.go → up four to the checkout.
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "..")), nil
}

// OutputFile is the absolute path of the generated file.
func OutputFile() (string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, filepath.FromSlash(OutputPath)), nil
}

// SortedNames is the emitted type names, for tests and for error messages.
func (g *gen) SortedNames() []string {
	out := make([]string, 0, len(g.decls))
	for _, d := range g.decls {
		out = append(out, d.name)
	}
	sort.Strings(out)
	return out
}
