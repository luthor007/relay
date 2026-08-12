package codex

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A JSON Schema validator covering exactly the keywords the three vendored
// Codex schemas use, and nothing else.
//
// Pulling in a full validator would be a dependency for one test; counting the
// keywords across all three files gives $ref, type, required, properties,
// additionalProperties, items, enum, const, oneOf, anyOf, allOf, minimum,
// maximum, minItems, maxItems, minLength, maxLength and pattern — plus
// description/title/default/format, which are annotations. That is the whole
// list below.
//
// It exists so `codex.trace.json`'s claim to be schema-derived is checked by
// `go test` rather than asserted in prose, and so a re-vendored schema with a
// new required field turns the fixture red instead of leaving the adapter
// quietly wrong.

type schemaFile struct {
	name string
	doc  map[string]any
	defs map[string]any
}

func loadSchema(t *testing.T, name string) *schemaFile {
	t.Helper()
	b, err := os.ReadFile(repoFile(t, "docs/fixtures/adapters/"+name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	defs, _ := doc["definitions"].(map[string]any)
	if defs == nil {
		t.Fatalf("%s has no definitions", name)
	}
	return &schemaFile{name: name, doc: doc, defs: defs}
}

// paramsRef finds the definition a schema file pins to one method, which is
// also the answer to "does this method still exist upstream?".
func (s *schemaFile) paramsRef(method string) (string, bool) {
	variants, _ := s.doc["oneOf"].([]any)
	for _, v := range variants {
		vm, _ := v.(map[string]any)
		props, _ := vm["properties"].(map[string]any)
		mp, _ := props["method"].(map[string]any)
		names, _ := mp["enum"].([]any)
		for _, n := range names {
			if n == method {
				pp, _ := props["params"].(map[string]any)
				if ref, ok := pp["$ref"].(string); ok {
					return strings.TrimPrefix(ref, "#/definitions/"), true
				}
				return "", true // params: null
			}
		}
	}
	return "", false
}

func (s *schemaFile) validate(path string, schema any, value any) []string {
	switch sch := schema.(type) {
	case bool:
		if sch {
			return nil
		}
		return []string{path + ": schema is false"}
	case map[string]any:
		return s.validateObject(path, sch, value)
	}
	return []string{path + ": unusable schema"}
}

func (s *schemaFile) validateObject(path string, sch map[string]any, value any) []string {
	var problems []string
	fail := func(f string, a ...any) { problems = append(problems, path+": "+fmt.Sprintf(f, a...)) }

	if ref, ok := sch["$ref"].(string); ok {
		name := strings.TrimPrefix(ref, "#/definitions/")
		target, ok := s.defs[name]
		if !ok {
			return []string{path + ": unknown $ref " + ref}
		}
		return s.validate(path, target, value)
	}

	if t, ok := sch["type"]; ok && !typeMatches(t, value) {
		fail("type is %v, got %s", t, jsonType(value))
		return problems
	}
	if e, ok := sch["enum"].([]any); ok && !containsValue(e, value) {
		fail("value %v is not one of %v", value, e)
	}
	if c, ok := sch["const"]; ok && !reflect.DeepEqual(c, value) {
		fail("value %v is not the const %v", value, c)
	}

	switch v := value.(type) {
	case map[string]any:
		if req, ok := sch["required"].([]any); ok {
			for _, r := range req {
				key, _ := r.(string)
				if _, present := v[key]; !present {
					fail("missing required field %q", key)
				}
			}
		}
		props, _ := sch["properties"].(map[string]any)
		for key, val := range v {
			if ps, ok := props[key]; ok {
				problems = append(problems, s.validate(path+"."+key, ps, val)...)
				continue
			}
			switch ap := sch["additionalProperties"].(type) {
			case bool:
				if !ap {
					fail("unexpected field %q", key)
				}
			case map[string]any:
				problems = append(problems, s.validate(path+"."+key, ap, val)...)
			}
		}
	case []any:
		if items, ok := sch["items"]; ok {
			for i, el := range v {
				problems = append(problems, s.validate(fmt.Sprintf("%s[%d]", path, i), items, el)...)
			}
		}
		if n, ok := numberOf(sch["minItems"]); ok && float64(len(v)) < n {
			fail("has %d items, minItems is %v", len(v), n)
		}
		if n, ok := numberOf(sch["maxItems"]); ok && float64(len(v)) > n {
			fail("has %d items, maxItems is %v", len(v), n)
		}
	case string:
		if p, ok := sch["pattern"].(string); ok {
			re, err := regexp.Compile(p)
			if err == nil && !re.MatchString(v) {
				fail("%q does not match %q", v, p)
			}
		}
		if n, ok := numberOf(sch["minLength"]); ok && float64(len(v)) < n {
			fail("%q is shorter than minLength %v", v, n)
		}
		if n, ok := numberOf(sch["maxLength"]); ok && float64(len(v)) > n {
			fail("%q is longer than maxLength %v", v, n)
		}
	case float64:
		if n, ok := numberOf(sch["minimum"]); ok && v < n {
			fail("%v is below minimum %v", v, n)
		}
		if n, ok := numberOf(sch["maximum"]); ok && v > n {
			fail("%v is above maximum %v", v, n)
		}
	}

	if branches, ok := sch["oneOf"].([]any); ok {
		if !anyBranchMatches(s, path, branches, value) {
			fail("matches none of the %d oneOf branches", len(branches))
		}
	}
	if branches, ok := sch["anyOf"].([]any); ok {
		if !anyBranchMatches(s, path, branches, value) {
			fail("matches none of the %d anyOf branches", len(branches))
		}
	}
	if branches, ok := sch["allOf"].([]any); ok {
		for i, b := range branches {
			problems = append(problems, s.validate(fmt.Sprintf("%s/allOf[%d]", path, i), b, value)...)
		}
	}
	return problems
}

func anyBranchMatches(s *schemaFile, path string, branches []any, value any) bool {
	for i, b := range branches {
		if len(s.validate(fmt.Sprintf("%s/branch[%d]", path, i), b, value)) == 0 {
			return true
		}
	}
	return false
}

func typeMatches(t any, v any) bool {
	switch tt := t.(type) {
	case string:
		return oneTypeMatches(tt, v)
	case []any:
		for _, x := range tt {
			if s, ok := x.(string); ok && oneTypeMatches(s, v) {
				return true
			}
		}
		return false
	}
	return true
}

func oneTypeMatches(t string, v any) bool {
	switch t {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == math.Trunc(f)
	}
	return true
}

func jsonType(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		if x == math.Trunc(x) {
			return "integer"
		}
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

func containsValue(list []any, v any) bool {
	for _, x := range list {
		if reflect.DeepEqual(x, v) {
			return true
		}
	}
	return false
}

func numberOf(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

// ---- the tests that make the fixture load-bearing ----

// TestTraceValidatesAgainstTheVendoredSchemas is the claim the fixture makes
// about itself, checked. Every message that names a schema is validated against
// that definition; every method it names must still be a variant of the file it
// came from.
func TestTraceValidatesAgainstTheVendoredSchemas(t *testing.T) {
	recs := loadTrace(t)
	files := map[string]*schemaFile{
		"ClientRequest.json":      loadSchema(t, "ClientRequest.json"),
		"ServerNotification.json": loadSchema(t, "ServerNotification.json"),
		"ServerRequest.json":      loadSchema(t, "ServerRequest.json"),
	}

	checked := 0
	for _, r := range recs {
		if r.Dir == "meta" {
			continue
		}
		if r.Schema == nil {
			// Results have no schema anywhere in the vendored set — there is no
			// ServerResponse.json — and the fixture says so rather than
			// pretending otherwise.
			if r.Kind != "response" {
				t.Errorf("seq %d: only responses may carry schema:null, got kind %q", r.Seq, r.Kind)
			}
			continue
		}
		file, pointer, _ := strings.Cut(*r.Schema, "#")
		sf := files[file]
		if sf == nil {
			t.Fatalf("seq %d: unknown schema file %q", r.Seq, file)
		}
		name := (*r.Schema)[strings.LastIndex(*r.Schema, "/")+1:]

		ref, ok := sf.paramsRef(r.Method)
		if !ok {
			t.Errorf("seq %d: %s is no longer a variant of %s — upstream drifted", r.Seq, r.Method, file)
			continue
		}
		if ref != name {
			t.Errorf("seq %d: %s now takes %s, the fixture claims %s", r.Seq, r.Method, ref, name)
			continue
		}

		var msg struct {
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(r.Msg, &msg); err != nil {
			t.Fatalf("seq %d: %v", r.Seq, err)
		}
		var params any
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			t.Fatalf("seq %d: params: %v", r.Seq, err)
		}
		if problems := sf.validate(fmt.Sprintf("seq %d %s", r.Seq, r.Method), sf.defs[name], params); len(problems) > 0 {
			sort.Strings(problems)
			t.Errorf("seq %d %s:\n  %s", r.Seq, r.Method, strings.Join(problems, "\n  "))
		}
		_ = pointer
		checked++
	}
	if checked < 30 {
		t.Fatalf("only %d messages were schema-checked; the fixture is too thin to be worth having", checked)
	}
}

// TestTraceSaysItIsNotARecording. A synthetic fixture that is honestly labelled
// is useful; one that pretends to be a recording is worse than none, and the
// label is the only thing standing between the two.
func TestTraceSaysItIsNotARecording(t *testing.T) {
	recs := loadTrace(t)
	if len(recs) == 0 || recs[0].Dir != "meta" {
		t.Fatal("the trace must open with a meta record carrying its provenance")
	}
	var meta struct {
		Provenance string   `json:"provenance"`
		Warning    string   `json:"warning"`
		Generator  string   `json:"generator"`
		Unverified []string `json:"unverified"`
	}
	if err := json.Unmarshal(recs[0].raw, &meta); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(meta.Provenance, "NOT RECORDED") {
		t.Errorf("provenance = %q, must say plainly that this is not a recording", meta.Provenance)
	}
	if !strings.Contains(meta.Warning, "NOT a recorded") {
		t.Errorf("warning = %q", meta.Warning)
	}
	if meta.Generator == "" {
		t.Error("the meta record must name the generator that produced it")
	}
	if len(meta.Unverified) == 0 {
		t.Error("the meta record must list what is still unverified")
	}
}

// TestEveryMethodTheAdapterUsesStillExists. The adapter names methods as string
// constants; if a re-vendored schema drops one, this is where it shows up.
func TestEveryMethodTheAdapterUsesStillExists(t *testing.T) {
	client := loadSchema(t, "ClientRequest.json")
	notes := loadSchema(t, "ServerNotification.json")
	requests := loadSchema(t, "ServerRequest.json")

	for _, m := range []string{
		"initialize", "thread/start", "thread/resume", "thread/fork",
		"thread/unsubscribe", "thread/compact/start",
		"turn/start", "turn/steer", "turn/interrupt", "config/read",
	} {
		if _, ok := client.paramsRef(m); !ok {
			t.Errorf("%s is gone from ClientRequest.json", m)
		}
	}
	for _, m := range []string{
		"thread/started", "thread/settings/updated", "thread/status/changed",
		"thread/closed", "thread/deleted", "thread/tokenUsage/updated",
		"turn/started", "turn/completed", "turn/plan/updated",
		"item/started", "item/completed",
		"item/agentMessage/delta", "item/reasoning/textDelta",
		"item/reasoning/summaryTextDelta", "item/reasoning/summaryPartAdded",
		"item/commandExecution/outputDelta", "item/fileChange/patchUpdated",
		"item/mcpToolCall/progress", "serverRequest/resolved", "error",
		"item/autoApprovalReview/started", "item/autoApprovalReview/completed",
		"deprecationNotice", "configWarning", "warning", "guardianWarning",
	} {
		if _, ok := notes.paramsRef(m); !ok {
			t.Errorf("%s is gone from ServerNotification.json", m)
		}
	}
	all := []string{
		MethodCommandApproval, MethodFileChangeApproval, MethodPermissionsApproval,
		MethodToolUserInput, MethodElicitation,
		MethodDynamicToolCall, MethodAuthRefresh, MethodAttestation,
		MethodApplyPatchLegacy, MethodExecCommandLegacy,
	}
	for _, m := range all {
		if _, ok := requests.paramsRef(m); !ok {
			t.Errorf("%s is gone from ServerRequest.json", m)
		}
	}
	// Ten, not five. An adapter that handles only the approval subset hangs
	// Codex the first time one of the others arrives.
	variants, _ := requests.doc["oneOf"].([]any)
	if len(variants) != len(all) {
		t.Errorf("ServerRequest.json has %d variants, the adapter handles %d", len(variants), len(all))
	}
}
