package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// The three framing facts from ADAPTERS.md §8 item 6, each as a test, because
// each of them was a guess until the vendor's transport source settled it.

func TestNDJSONFramingWritesOneObjectPerLineWithNoHeaders(t *testing.T) {
	var buf bytes.Buffer
	if err := (ndjson{}).write(&buf, []byte(`{"id":1,"method":"initialize"}`)); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if got != `{"id":1,"method":"initialize"}`+"\n" {
		t.Fatalf("framing = %q", got)
	}
	if strings.Contains(got, "Content-Length") {
		t.Fatal("NDJSON must not carry LSP-style headers")
	}
}

func TestOutboundMessagesCarryNoJSONRPCField(t *testing.T) {
	// "We do not do true JSON-RPC 2.0, as we neither send nor expect the
	// jsonrpc: 2.0 field." Sending one is wrong on the wire, not redundant.
	b, err := json.Marshal(&message{ID: json.RawMessage("1"), Method: "initialize"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("jsonrpc")) {
		t.Fatalf("outbound message carries a jsonrpc field: %s", b)
	}
}

func TestUntaggedDemultiplexOnFieldPresence(t *testing.T) {
	cases := []struct {
		line string
		want msgKind
	}{
		{`{"id":1,"method":"item/tool/call","params":{}}`, kindRequest},
		{`{"method":"turn/started","params":{}}`, kindNotification},
		{`{"id":1,"result":{}}`, kindResponse},
		{`{"id":"srv-1","result":null}`, kindResponse},
		{`{"id":1,"error":{"code":-32601,"message":"nope"}}`, kindError},
	}
	for _, c := range cases {
		var m message
		if err := json.Unmarshal([]byte(c.line), &m); err != nil {
			t.Fatalf("%s: %v", c.line, err)
		}
		if got := m.kind(); got != c.want {
			t.Errorf("%s: kind = %d, want %d", c.line, got, c.want)
		}
	}
}

func TestDecodeSkipsBlankLinesAndRejectsGarbage(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("\n\n{\"method\":\"warning\",\"params\":{}}\nnot json\n"))
	m, err := decode(ndjson{}, r)
	if err != nil {
		t.Fatalf("first message: %v", err)
	}
	if m.Method != "warning" {
		t.Fatalf("method = %q", m.Method)
	}
	if _, err := decode(ndjson{}, r); err == nil {
		t.Fatal("a non-JSON line must be an error, not a silent skip")
	}
}

func TestDecodeAcceptsAFinalLineWithoutANewline(t *testing.T) {
	r := bufio.NewReader(strings.NewReader(`{"method":"warning","params":{}}`))
	if _, err := decode(ndjson{}, r); err != nil {
		t.Fatalf("unterminated final line: %v", err)
	}
	if _, err := decode(ndjson{}, r); err != io.EOF {
		t.Fatalf("second read = %v, want EOF", err)
	}
}

func TestRequestIDKeepsItsJSONType(t *testing.T) {
	// RequestId = string | int64, and a reply id must be echoed with the type
	// it arrived as. Keeping the raw bytes is the only way to guarantee it.
	for _, raw := range []string{`7`, `"srv-1"`} {
		var m message
		if err := json.Unmarshal([]byte(`{"id":`+raw+`,"method":"attestation/generate","params":{}}`), &m); err != nil {
			t.Fatal(err)
		}
		out, err := json.Marshal(&message{ID: m.ID, Result: json.RawMessage(`null`)})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(out, []byte(`"id":`+raw)) {
			t.Fatalf("id %s was not echoed verbatim: %s", raw, out)
		}
	}
}

func TestRequestKeyNormalisesBothIDTypes(t *testing.T) {
	if requestKey(json.RawMessage(`"srv-1"`)) != requestKey(json.RawMessage(` "srv-1" `)) {
		t.Fatal("whitespace changed a string id's key")
	}
	if requestKey(json.RawMessage(`7`)) == requestKey(json.RawMessage(`"7"`)) {
		t.Fatal("a numeric id and a string id must not collide")
	}
}

func TestFramingRefusesEmbeddedNewline(t *testing.T) {
	var buf bytes.Buffer
	if err := (ndjson{}).write(&buf, []byte("{\"a\":\"b\\nc\"}\n{\"evil\":1}")); err == nil {
		t.Fatal("a payload containing a newline would split into two messages")
	}
}
