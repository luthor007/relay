package acp

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/adapter"
)

func TestMessageKindDemultiplexesOnFieldPresence(t *testing.T) {
	for _, tc := range []struct {
		line string
		want messageKind
	}{
		{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, kindRequest},
		{`{"jsonrpc":"2.0","method":"session/cancel","params":{}}`, kindNotification},
		{`{"jsonrpc":"2.0","id":1,"result":{}}`, kindResponse},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"nope"}}`, kindError},
		// A notification with an explicit null id is still a notification.
		{`{"jsonrpc":"2.0","id":null,"method":"session/update","params":{}}`, kindNotification},
		{`{"jsonrpc":"2.0"}`, kindUnknown},
	} {
		var m message
		if err := json.Unmarshal([]byte(tc.line), &m); err != nil {
			t.Fatalf("%s: %v", tc.line, err)
		}
		if got := m.kind(); got != tc.want {
			t.Errorf("%s -> %v, want %v", tc.line, got, tc.want)
		}
	}
}

func TestClientCapabilitiesAreAllFalse(t *testing.T) {
	b, err := json.Marshal(initializeParams{
		ProtocolVersion:    ProtocolVersion,
		ClientCapabilities: relayClientCapabilities(),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":false,"writeTextFile":false},"terminal":false}}`
	if string(b) != want {
		t.Errorf("initialize params =\n%s\nwant\n%s", b, want)
	}
}

func TestEncodeBlocks(t *testing.T) {
	blocks, err := encodeBlocks(adapter.Turn{
		Text: "hello",
		Blocks: []adapter.Block{
			{Kind: adapter.BlockResourceLink, Text: "notes", URI: "file:///tmp/notes.md", MIMEType: "text/markdown"},
			{Kind: adapter.BlockImage, Data: []byte{1, 2, 3}, MIMEType: "image/png"},
			{Kind: adapter.BlockAudio, Data: []byte{4, 5}, MIMEType: "audio/wav"},
			{Kind: adapter.BlockEmbeddedContext, URI: "file:///tmp/a.ts", Text: "const x = 1"},
		},
	})
	if err != nil {
		t.Fatalf("encodeBlocks: %v", err)
	}
	if len(blocks) != 5 {
		t.Fatalf("got %d blocks", len(blocks))
	}
	if blocks[0].Type != "text" || blocks[0].Text != "hello" {
		t.Errorf("text block = %+v", blocks[0])
	}
	if blocks[1].Type != "resource_link" || blocks[1].Name != "notes" || blocks[1].URI != "file:///tmp/notes.md" {
		t.Errorf("resource_link = %+v", blocks[1])
	}
	if blocks[2].Type != "image" || blocks[2].Data != "AQID" {
		t.Errorf("image = %+v", blocks[2])
	}
	if blocks[3].Type != "audio" || blocks[3].Data != "BAU=" {
		t.Errorf("audio = %+v", blocks[3])
	}
	if blocks[4].Type != "resource" {
		t.Fatalf("embedded = %+v", blocks[4])
	}
	var res textResourceContents
	if err := json.Unmarshal(blocks[4].Resource, &res); err != nil {
		t.Fatal(err)
	}
	if res.URI != "file:///tmp/a.ts" || res.Text != "const x = 1" {
		t.Errorf("embedded resource = %+v", res)
	}
}

func TestEncodeBlocksRefusesEmptyAndUnknown(t *testing.T) {
	if _, err := encodeBlocks(adapter.Turn{}); err == nil {
		t.Error("an empty prompt must be refused, not sent")
	}
	if _, err := encodeBlocks(adapter.Turn{Blocks: []adapter.Block{{Kind: "sculpture"}}}); err == nil {
		t.Error("an unknown block kind must be refused")
	}
	if _, err := encodeBlocks(adapter.Turn{Blocks: []adapter.Block{{Kind: adapter.BlockResourceLink}}}); err == nil {
		t.Error("a resource_link with no uri must be refused")
	}
}

func TestBlockTextIsOnlyTextBlocks(t *testing.T) {
	if got := blockText(contentBlock{Type: "text", Text: "spoken"}); got != "spoken" {
		t.Errorf("blockText = %q", got)
	}
	for _, typ := range []string{"image", "audio", "resource", "resource_link"} {
		if got := blockText(contentBlock{Type: typ, Text: "should not be read"}); got != "" {
			t.Errorf("blockText on %s = %q; only text blocks are speakable", typ, got)
		}
	}
}

func TestSplitEnv(t *testing.T) {
	for _, tc := range []struct{ in, k, v string }{
		{"A=1", "A", "1"},
		{"A=", "A", ""},
		{"A", "A", ""},
		{"A=B=C", "A", "B=C"},
	} {
		k, v := splitEnv(tc.in)
		if k != tc.k || v != tc.v {
			t.Errorf("splitEnv(%q) = %q,%q want %q,%q", tc.in, k, v, tc.k, tc.v)
		}
	}
}

// TestMethodListsMatchTheSurface keeps the constants in this package aligned
// with what the adapter actually handles.
func TestMethodListsMatchTheSurface(t *testing.T) {
	if len(AgentMethods()) != 8 || len(ClientMethods()) != 9 {
		t.Fatalf("the ACP surface is 8 + 9 methods, got %d + %d", len(AgentMethods()), len(ClientMethods()))
	}
	if len(RefusedClientMethods()) != 7 {
		t.Fatalf("seven of the nine agent→client methods are refused, got %d", len(RefusedClientMethods()))
	}
	for _, m := range RefusedClientMethods() {
		if !contains(ClientMethods(), m) {
			t.Errorf("%s is refused but is not an agent→client method", m)
		}
		if !strings.HasPrefix(m, "fs/") && !strings.HasPrefix(m, "terminal/") {
			t.Errorf("%s is refused but is neither fs/* nor terminal/*", m)
		}
	}
	if len(UpdateVariants()) != 8 {
		t.Fatalf("session/update has eight variants, got %d", len(UpdateVariants()))
	}
}

// TestErrorCodes pins the five stock JSON-RPC codes plus ACP's two additions.
// -32000 and -32002 are the two with documented recoveries, and they are the
// reason this list is spelled out rather than inlined.
func TestErrorCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		{"parse", codeParseError, -32700},
		{"invalid request", codeInvalidRequest, -32600},
		{"method not found", codeMethodNotFound, -32601},
		{"invalid params", codeInvalidParams, -32602},
		{"internal", codeInternalError, -32603},
		{"auth required", CodeAuthRequired, -32000},
		{"resource not found", CodeResourceNotFound, -32002},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

func TestRPCErrorReadsClearly(t *testing.T) {
	e := &RPCError{Code: CodeAuthRequired, Message: "Authentication required"}
	if !strings.Contains(e.Error(), "-32000") {
		t.Errorf("RPCError.Error = %q", e.Error())
	}
	withData := &RPCError{Code: codeInvalidParams, Message: "bad", Data: json.RawMessage(`{"field":"cwd"}`)}
	if !strings.Contains(withData.Error(), `"cwd"`) {
		t.Errorf("RPCError.Error dropped the data: %q", withData.Error())
	}
}

func TestToolCallUpdateDistinguishesAbsentFromCleared(t *testing.T) {
	var absent, cleared toolCallUpdate
	if err := json.Unmarshal([]byte(`{"toolCallId":"c1","status":"completed"}`), &absent); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"toolCallId":"c1","content":[]}`), &cleared); err != nil {
		t.Fatal(err)
	}
	if absent.Content != nil {
		t.Error("an update that says nothing about content must decode as absent")
	}
	if cleared.Content == nil || len(*cleared.Content) != 0 {
		t.Error("an explicit empty array means replace-with-nothing, which is not the same as silence")
	}
	if absent.Title != nil || absent.Kind != nil {
		t.Error("title and kind must be absent rather than empty strings")
	}
	if !reflect.DeepEqual(*absent.Status, "completed") {
		t.Errorf("status = %v", absent.Status)
	}
}
