package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// message is Codex's `JSONRPCMessage`, which is `#[serde(untagged)]` in the
// vendor's `rpc.rs`. There is deliberately no `jsonrpc` field: the crate's own
// comment says "we neither send nor expect" it, so emitting one would be wrong
// on the wire rather than merely redundant.
//
// ID is a [json.RawMessage] rather than a string or an int because
// `RequestId = string | int64` and a reply id must be echoed with the same JSON
// type it arrived as. Keeping the bytes is the only way to guarantee that.
type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

// rpcError is Codex's `JSONRPCError.error`, exactly `{code, message, data?}`.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return "codex: <nil rpc error>"
	}
	return fmt.Sprintf("codex: rpc error %d: %s", e.Code, e.Message)
}

// JSON-RPC error codes. -32601 is the standard "method not found" and is the
// honest answer to the five server requests Relay wants nothing to do with:
// we genuinely do not implement them. -32001 is in the implementation-defined
// range and means "we understand the question and cannot form a reply we have
// any evidence is correct" — see approvals.go.
const (
	codeMethodNotFound = -32601
	codeInternalError  = -32603
	codeUnverified     = -32001
)

type msgKind int

const (
	kindInvalid msgKind = iota
	kindRequest
	kindNotification
	kindResponse
	kindError
)

func (m *message) kind() msgKind {
	hasID := len(m.ID) > 0 && !bytes.Equal(m.ID, jsonNull)
	switch {
	case hasID && m.Method != "":
		return kindRequest
	case m.Method != "":
		return kindNotification
	case hasID && m.Error != nil:
		return kindError
	case hasID:
		return kindResponse
	}
	return kindInvalid
}

var jsonNull = []byte("null")

// framing is how a payload becomes bytes on the wire.
//
// ADAPTERS.md §8 item 6 resolved the transport as NDJSON and downgraded this
// interface from "required hedge" to "keep it only if it costs nothing". It
// costs about twenty lines, and it is the seam a `Content-Length` codec drops
// into if a future app-server ever grows one — which the same section notes
// already exists for a *different* Codex transport (`remote_control.rs`).
type framing interface {
	// write frames one already-marshalled payload.
	write(w io.Writer, payload []byte) error
	// read returns the next payload, without its framing.
	read(r *bufio.Reader) ([]byte, error)
}

// ndjson is one JSON object per line. This is what `codex app-server` speaks.
type ndjson struct{}

func (ndjson) write(w io.Writer, payload []byte) error {
	if bytes.ContainsRune(payload, '\n') {
		// A newline inside the payload would split one message into two on the
		// far side. encoding/json never emits a bare newline, so this is a
		// corrupted-caller check rather than an expected path.
		return errors.New("codex: refusing to write a payload containing a newline")
	}
	buf := make([]byte, 0, len(payload)+1)
	buf = append(buf, payload...)
	buf = append(buf, '\n')
	_, err := w.Write(buf)
	return err
}

func (ndjson) read(r *bufio.Reader) ([]byte, error) {
	for {
		line, err := r.ReadBytes('\n')
		line = bytes.TrimRight(line, "\r\n")
		if len(line) > 0 {
			// A final line without a trailing newline is still a message; the
			// EOF comes back on the next call.
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			return line, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// decode reads one message. Lines that are not JSON objects are reported as
// errors rather than skipped: silently dropping wire traffic is how a protocol
// drift becomes a mystery instead of a red test.
func decode(fr framing, r *bufio.Reader) (*message, error) {
	payload, err := fr.read(r)
	if err != nil {
		return nil, err
	}
	var m message
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, fmt.Errorf("codex: undecodable message %q: %w", truncate(payload, 200), err)
	}
	if m.kind() == kindInvalid {
		return nil, fmt.Errorf("codex: message is neither request, notification, response nor error: %q", truncate(payload, 200))
	}
	return &m, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
