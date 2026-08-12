package acp

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

// A JSON Schema validator covering exactly the keywords acp-schema.json uses,
// and nothing else.
//
// Counting keywords across the vendored file gives $ref, type, required,
// properties, items, enum, const, oneOf, anyOf, minimum and maximum — plus
// $schema, description, title, default, format and the x-* annotations, which
// carry no constraints. That is the whole list below. Pulling in a full
// draft-2020-12 validator would be a new module dependency for one test, and
// go.mod has one owner.
//
// It exists so acp.trace.json's claim to be schema-derived is checked by
// `go test` rather than asserted in prose, and so a re-vendored schema with a
// new required field turns the fixture red instead of leaving the adapter
// quietly wrong.

type schemaDoc struct {
	raw  []byte
	doc  map[string]any
	defs map[string]any
}

func loadACPSchema(t *testing.T) *schemaDoc {
	t.Helper()
	b, err := os.ReadFile(repoFile(t, "docs/fixtures/adapters/acp-schema.json"))
	if err != nil {
		t.Fatalf("reading acp-schema.json: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parsing acp-schema.json: %v", err)
	}
	defs, _ := doc["$defs"].(map[string]any)
	if len(defs) == 0 {
		t.Fatal("acp-schema.json has no $defs")
	}
	return &schemaDoc{raw: b, doc: doc, defs: defs}
}

func (s *schemaDoc) def(t *testing.T, name string) map[string]any {
	t.Helper()
	d, ok := s.defs[name].(map[string]any)
	if !ok {
		t.Fatalf("$defs.%s is missing from the vendored schema", name)
	}
	return d
}

func (s *schemaDoc) validate(path string, schema any, value any) []string {
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

func (s *schemaDoc) validateObject(path string, sch map[string]any, value any) []string {
	var problems []string
	fail := func(f string, a ...any) { problems = append(problems, path+": "+fmt.Sprintf(f, a...)) }

	if ref, ok := sch["$ref"].(string); ok {
		name := strings.TrimPrefix(ref, "#/$defs/")
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
			}
		}
	case []any:
		if items, ok := sch["items"]; ok {
			for i, el := range v {
				problems = append(problems, s.validate(fmt.Sprintf("%s[%d]", path, i), items, el)...)
			}
		}
	case string:
		if p, ok := sch["pattern"].(string); ok {
			if re, err := regexp.Compile(p); err == nil && !re.MatchString(v) {
				fail("%q does not match %q", v, p)
			}
		}
	case float64:
		if n, ok := sch["minimum"].(float64); ok && v < n {
			fail("%v is below minimum %v", v, n)
		}
		if n, ok := sch["maximum"].(float64); ok && v > n {
			fail("%v is above maximum %v", v, n)
		}
	}

	if branches, ok := sch["oneOf"].([]any); ok && !s.anyBranch(path, branches, value) {
		fail("matches none of the %d oneOf branches", len(branches))
	}
	if branches, ok := sch["anyOf"].([]any); ok && !s.anyBranch(path, branches, value) {
		fail("matches none of the %d anyOf branches", len(branches))
	}
	return problems
}

func (s *schemaDoc) anyBranch(path string, branches []any, value any) bool {
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

// ---- the nine invariants from the end of acp-methods.md ----
//
// acp-methods.md says a Go test should carry them once the adapter lands, so
// that a version bump is a red build rather than a wrong adapter. These are
// those nine, plus the one thing the schema cannot carry — PROTOCOL_VERSION —
// asserted against the constant this package uses.

// 1. The method surface, exactly: 8 client→agent, 9 agent→client, 17 in total.
func TestInvariantMethodSurface(t *testing.T) {
	s := loadACPSchema(t)
	bySide := map[string][]string{}
	for _, v := range s.defs {
		d, ok := v.(map[string]any)
		if !ok {
			continue
		}
		m, _ := d["x-method"].(string)
		side, _ := d["x-side"].(string)
		if m == "" || side == "" {
			continue
		}
		if !contains(bySide[side], m) {
			bySide[side] = append(bySide[side], m)
		}
	}
	for _, side := range []string{"agent", "client"} {
		sort.Strings(bySide[side])
	}

	wantAgent := sortedCopy(AgentMethods())
	wantClient := sortedCopy(ClientMethods())
	if !reflect.DeepEqual(bySide["agent"], wantAgent) {
		t.Errorf("client→agent methods drifted:\n got %v\nwant %v", bySide["agent"], wantAgent)
	}
	if !reflect.DeepEqual(bySide["client"], wantClient) {
		t.Errorf("agent→client methods drifted:\n got %v\nwant %v", bySide["client"], wantClient)
	}
	if n := len(bySide["agent"]) + len(bySide["client"]); n != 17 {
		t.Errorf("the ACP surface is %d methods, want 17", n)
	}
}

// 2. No steer. This is the load-bearing negative behind ADAPTERS.md §4 and
// behind Session.Steer returning an *UnsupportedError.
func TestInvariantNoSteeringMethod(t *testing.T) {
	s := loadACPSchema(t)
	lower := strings.ToLower(string(s.raw))
	for _, word := range []string{"steer", "inject", "interrupt"} {
		if strings.Contains(lower, word) {
			t.Errorf("the vendored schema now contains %q — ADAPTERS.md §4's central negative has to be re-read, and Session.Steer with it", word)
		}
	}
}

// 3. Union shapes. A ninth branch in ClientRequest is the exact signal that a
// steering method has landed.
//
// Note the naming inversion the schema ships with: $defs.ClientRequest holds
// what the *client sends to the agent*. Trust x-side, not the def name.
func TestInvariantUnionShapes(t *testing.T) {
	s := loadACPSchema(t)
	for _, tc := range []struct {
		def  string
		want int
		why  string
	}{
		{"ClientRequest", 8, "7 client→agent requests plus the untitled ext branch"},
		{"ClientNotification", 2, "session/cancel plus ext"},
		{"AgentRequest", 9, "8 agent→client requests plus ext"},
		{"AgentNotification", 2, "session/update plus ext"},
	} {
		d := s.def(t, tc.def)
		branches, _ := d["anyOf"].([]any)
		if len(branches) != tc.want {
			t.Errorf("$defs.%s has %d anyOf branches, want %d (%s)", tc.def, len(branches), tc.want, tc.why)
		}
	}
}

// 4. SessionUpdate still has exactly the eight discriminants the normalizer
// switches on.
func TestInvariantEightSessionUpdateVariants(t *testing.T) {
	s := loadACPSchema(t)
	d := s.def(t, "SessionUpdate")
	branches, _ := d["oneOf"].([]any)
	var got []string
	for _, b := range branches {
		bm, _ := b.(map[string]any)
		props, _ := bm["properties"].(map[string]any)
		su, _ := props["sessionUpdate"].(map[string]any)
		c, _ := su["const"].(string)
		got = append(got, c)
	}
	if !reflect.DeepEqual(got, UpdateVariants()) {
		t.Errorf("session/update variants drifted:\n got %v\nwant %v", got, UpdateVariants())
	}
}

// 5. StopReason still has exactly the five values, in order. They are the turn
// boundary and they distinguish the cases the orchestrator must tell apart.
func TestInvariantFiveStopReasons(t *testing.T) {
	s := loadACPSchema(t)
	d := s.def(t, "StopReason")
	branches, _ := d["oneOf"].([]any)
	var got []string
	for _, b := range branches {
		bm, _ := b.(map[string]any)
		if c, ok := bm["const"].(string); ok {
			got = append(got, c)
		}
	}
	want := []string{"end_turn", "max_tokens", "max_turn_requests", "refusal", "cancelled"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("StopReason drifted:\n got %v\nwant %v", got, want)
	}
	// And the adapter maps every one of them onto an event.StopReason.
	for _, v := range want {
		if got := string(mapStopReason(discardLogger(), v)); got != v {
			t.Errorf("mapStopReason(%q) = %q", v, got)
		}
	}
}

// 6. The permission shape, including the two traps: toolCall is a
// ToolCallUpdate whose only required field is toolCallId, and the outcome union
// is exactly selected + cancelled.
func TestInvariantPermissionShape(t *testing.T) {
	s := loadACPSchema(t)

	req := s.def(t, "RequestPermissionRequest")
	if got := requiredOf(req); !reflect.DeepEqual(got, []string{"sessionId", "toolCall", "options"}) {
		t.Errorf("RequestPermissionRequest.required = %v", got)
	}
	props, _ := req["properties"].(map[string]any)
	tc, _ := props["toolCall"].(map[string]any)
	if ref, _ := tc["$ref"].(string); ref != "#/$defs/ToolCallUpdate" {
		t.Errorf("RequestPermissionRequest.toolCall $refs %q, want ToolCallUpdate", ref)
	}
	if got := requiredOf(s.def(t, "ToolCallUpdate")); !reflect.DeepEqual(got, []string{"toolCallId"}) {
		t.Errorf("ToolCallUpdate.required = %v, want only toolCallId — the adapter must not assume a title to read aloud", got)
	}

	outcome := s.def(t, "RequestPermissionOutcome")
	branches, _ := outcome["oneOf"].([]any)
	if len(branches) != 2 {
		t.Fatalf("RequestPermissionOutcome has %d branches, want 2", len(branches))
	}
	seen := map[string][]string{}
	for _, b := range branches {
		bm, _ := b.(map[string]any)
		p, _ := bm["properties"].(map[string]any)
		o, _ := p["outcome"].(map[string]any)
		name, _ := o["const"].(string)
		seen[name] = requiredOf(bm)
	}
	if !reflect.DeepEqual(seen[outcomeCancelled], []string{"outcome"}) {
		t.Errorf("cancelled outcome required = %v", seen[outcomeCancelled])
	}
	if !reflect.DeepEqual(seen[outcomeSelected], []string{"outcome", "optionId"}) {
		t.Errorf("selected outcome required = %v", seen[outcomeSelected])
	}
}

// 7. The two capability objects. A new capability means a new decision about
// what Relay advertises, so it should not slip in unnoticed.
func TestInvariantCapabilityShapes(t *testing.T) {
	s := loadACPSchema(t)
	if got := propertyNames(s.def(t, "ClientCapabilities")); !reflect.DeepEqual(got, []string{"fs", "terminal"}) {
		t.Errorf("ClientCapabilities properties = %v", got)
	}
	if got := propertyNames(s.def(t, "FileSystemCapability")); !reflect.DeepEqual(got, []string{"readTextFile", "writeTextFile"}) {
		t.Errorf("FileSystemCapability properties = %v", got)
	}
	if got := propertyNames(s.def(t, "AgentCapabilities")); !reflect.DeepEqual(got, []string{"loadSession", "mcpCapabilities", "promptCapabilities"}) {
		t.Errorf("AgentCapabilities properties = %v", got)
	}
	if got := propertyNames(s.def(t, "PromptCapabilities")); !reflect.DeepEqual(got, []string{"audio", "embeddedContext", "image"}) {
		t.Errorf("PromptCapabilities properties = %v", got)
	}
}

// 8. The UNSTABLE set is still exactly the five model definitions. Anything
// leaving it has been promoted; anything joining it has been demoted, and
// SetModel's opt-in gate is the thing that would need revisiting.
func TestInvariantUnstableSet(t *testing.T) {
	s := loadACPSchema(t)
	var got []string
	for name, v := range s.defs {
		d, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if desc, _ := d["description"].(string); strings.Contains(desc, "UNSTABLE") {
			got = append(got, name)
		}
	}
	sort.Strings(got)
	want := []string{"ModelId", "ModelInfo", "SessionModelState", "SetSessionModelRequest", "SetSessionModelResponse"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the UNSTABLE set drifted:\n got %v\nwant %v", got, want)
	}
}

// 9. Still no cost and no usage anywhere. This one is checked in the hope it
// will one day fire: per-turn metering in the protocol would rewrite
// ADAPTERS.md §5 and §8 in Relay's favour, and would let TurnCompleted.Usage
// stop being nil.
func TestInvariantNoCostOrUsageField(t *testing.T) {
	s := loadACPSchema(t)
	lower := strings.ToLower(string(s.raw))
	for _, word := range []string{"\"cost\"", "\"usage\""} {
		if strings.Contains(lower, word) {
			t.Errorf("the schema now mentions %s — ADAPTERS.md §5 and §8 item 3 need rewriting, and CostPlanFor with them", word)
		}
	}
	if n := strings.Count(lower, "token"); n != 2 {
		t.Errorf(`the word "token" occurs %d times, want 2 (both in the max_tokens stop reason)`, n)
	}
}

// The tenth thing, which the schema cannot carry: PROTOCOL_VERSION lives in the
// package's typescript/schema.ts, not in the JSON. acp-methods.md records it
// and this pins the constant to that record.
func TestProtocolVersionMatchesTheVendoredRecord(t *testing.T) {
	b, err := os.ReadFile(repoFile(t, "docs/fixtures/adapters/acp-methods.md"))
	if err != nil {
		t.Fatalf("reading acp-methods.md: %v", err)
	}
	if !strings.Contains(string(b), "| `PROTOCOL_VERSION` | **1** ") {
		t.Fatal("acp-methods.md no longer records PROTOCOL_VERSION as 1; the adapter's ProtocolVersion constant has to move with it")
	}
	if ProtocolVersion != 1 {
		t.Fatalf("ProtocolVersion = %d, want 1", ProtocolVersion)
	}
}

func requiredOf(d map[string]any) []string {
	req, _ := d["required"].([]any)
	out := make([]string, 0, len(req))
	for _, r := range req {
		s, _ := r.(string)
		out = append(out, s)
	}
	return out
}

func propertyNames(d map[string]any) []string {
	props, _ := d["properties"].(map[string]any)
	var out []string
	for k := range props {
		if k == "_meta" {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
