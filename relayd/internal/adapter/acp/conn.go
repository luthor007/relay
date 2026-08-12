package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
)

// ErrConnClosed is returned by a call on a connection whose reader has stopped,
// either because the agent process exited or because Close was called.
var ErrConnClosed = errors.New("acp: connection is closed")

// peer receives everything the agent sends us. The Adapter implements it;
// splitting it out keeps the JSON-RPC plumbing free of ACP semantics.
//
// Both methods run on the single reader goroutine and must not block. A
// request that becomes a question for a human is *raised* here and *answered*
// later, from whichever goroutine calls Reply.
type peer interface {
	onNotification(method string, params json.RawMessage)
	onRequest(id json.RawMessage, method string, params json.RawMessage)
	// onClosed fires exactly once, with the reason the reader stopped. err is
	// nil for an orderly shutdown.
	onClosed(err error)
}

// conn is one newline-delimited JSON-RPC 2.0 connection to an ACP agent.
//
// ACP is symmetric: the agent issues requests to us as freely as we issue them
// to it, so this type serves both directions over one pair of pipes. A single
// goroutine reads, which is what keeps event sequence numbers in wire order.
type conn struct {
	br  *bufio.Reader
	w   io.Writer
	log *slog.Logger

	closer func() error

	wmu sync.Mutex // serialises writes; JSON-RPC allows interleaved calls

	mu      sync.Mutex
	nextID  int64
	pending map[string]chan *message
	err     error

	closed    chan struct{}
	closeOnce sync.Once

	handler peer
}

func newConn(r io.Reader, w io.Writer, closer func() error, h peer, log *slog.Logger) *conn {
	if log == nil {
		log = slog.Default()
	}
	return &conn{
		// A tool_call_update can carry a whole file diff; ReadBytes grows past
		// this, so the size is a throughput knob rather than a cap.
		br:      bufio.NewReaderSize(r, 1<<16),
		w:       w,
		log:     log,
		closer:  closer,
		pending: map[string]chan *message{},
		closed:  make(chan struct{}),
		handler: h,
	}
}

// run reads until the transport ends. It is the only reader.
func (c *conn) run() {
	var readErr error
	for {
		line, err := c.br.ReadBytes('\n')
		if len(line) > 0 {
			if m, ok := c.parse(line); ok {
				c.dispatch(m)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
				readErr = err
			}
			break
		}
	}
	c.finish(readErr)
}

// parse turns one line into a message. A line that is not JSON at all is
// logged and skipped rather than killing the connection: ACP agents are young,
// and one malformed line should not lose a session. It is counted by the log,
// which is the only honest signal we have that a runtime is misframing.
func (c *conn) parse(line []byte) (*message, bool) {
	trimmed := trimSpace(line)
	if len(trimmed) == 0 {
		return nil, false
	}
	var m message
	if err := json.Unmarshal(trimmed, &m); err != nil {
		c.log.Warn("acp: unparseable line from the agent", "err", err, "bytes", len(trimmed))
		return nil, false
	}
	return &m, true
}

func trimSpace(b []byte) []byte {
	i := 0
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n') {
		i++
	}
	j := len(b)
	for j > i && (b[j-1] == ' ' || b[j-1] == '\t' || b[j-1] == '\r' || b[j-1] == '\n') {
		j--
	}
	return b[i:j]
}

func (c *conn) dispatch(m *message) {
	switch m.kind() {
	case kindNotification:
		c.handler.onNotification(m.Method, m.Params)
	case kindRequest:
		c.handler.onRequest(m.ID, m.Method, m.Params)
	case kindResponse, kindError:
		c.mu.Lock()
		ch, ok := c.pending[string(m.ID)]
		if ok {
			delete(c.pending, string(m.ID))
		}
		c.mu.Unlock()
		if !ok {
			c.log.Warn("acp: response for an unknown request id", "id", string(m.ID))
			return
		}
		ch <- m
	default:
		c.log.Warn("acp: message with no method, result or error", "raw", string(m.ID))
	}
}

func (c *conn) finish(err error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.err = err
		pending := c.pending
		c.pending = map[string]chan *message{}
		c.mu.Unlock()

		close(c.closed)
		for _, ch := range pending {
			close(ch)
		}
		c.handler.onClosed(err)
	})
}

// call issues a request and waits for its response.
//
// It is safe to call from any goroutine except the reader's: a call made from
// inside onNotification or onRequest would wait for a reply the reader can no
// longer deliver.
func (c *conn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	body, err := marshalParams(params)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if c.err != nil || c.isClosed() {
		c.mu.Unlock()
		return nil, c.closedErr()
	}
	c.nextID++
	id := strconv.FormatInt(c.nextID, 10)
	ch := make(chan *message, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	unregister := func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}

	if err := c.write(&message{JSONRPC: "2.0", ID: json.RawMessage(id), Method: method, Params: body}); err != nil {
		unregister()
		return nil, err
	}

	select {
	case <-ctx.Done():
		unregister()
		return nil, ctx.Err()
	case m, ok := <-ch:
		if !ok {
			return nil, c.closedErr()
		}
		if m.Error != nil {
			return nil, m.Error
		}
		return m.Result, nil
	}
}

// notify sends a notification. session/cancel is one, and it has no response of
// its own — the acknowledgement is the original session/prompt resolving with
// stopReason "cancelled".
func (c *conn) notify(method string, params any) error {
	body, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.write(&message{JSONRPC: "2.0", Method: method, Params: body})
}

func (c *conn) respond(id json.RawMessage, result any) error {
	body, err := marshalParams(result)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		body = json.RawMessage("{}")
	}
	return c.write(&message{JSONRPC: "2.0", ID: id, Result: body})
}

func (c *conn) respondError(id json.RawMessage, code int, msg string) error {
	return c.write(&message{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: msg}})
}

func (c *conn) write(m *message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("acp: encoding %s: %w", m.Method, err)
	}
	b = append(b, '\n')

	c.wmu.Lock()
	defer c.wmu.Unlock()
	select {
	case <-c.closed:
		return c.closedErr()
	default:
	}
	if _, err := c.w.Write(b); err != nil {
		return fmt.Errorf("acp: writing %s: %w", m.Method, err)
	}
	return nil
}

func (c *conn) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func (c *conn) closedErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return fmt.Errorf("%w: %v", ErrConnClosed, c.err)
	}
	return ErrConnClosed
}

func (c *conn) close() error {
	var err error
	if c.closer != nil {
		err = c.closer()
	}
	c.finish(nil)
	return err
}

func marshalParams(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		return raw, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("acp: encoding params: %w", err)
	}
	return b, nil
}
