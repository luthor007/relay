package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
)

// ErrConnClosed is returned by a call on a connection whose reader has stopped,
// either because the process exited or because Close was called.
var ErrConnClosed = errors.New("codex: app-server connection is closed")

// peer is what a conn hands its inbound traffic to. The Adapter implements it;
// splitting it out keeps the JSON-RPC plumbing free of any Codex semantics.
type peer interface {
	onNotification(method string, params json.RawMessage)
	onServerRequest(id json.RawMessage, method string, params json.RawMessage)
	// onClosed fires exactly once, with the reason the reader stopped. err is
	// nil for an orderly shutdown.
	onClosed(err error)
}

// conn is one JSON-RPC connection to a `codex app-server` process.
//
// A single goroutine reads, so every inbound message is dispatched in wire
// order and event sequence numbers cannot interleave. Handlers must therefore
// not block: a server request that becomes a question for a human is *raised*
// here and *answered* later, from whichever goroutine calls Reply.
type conn struct {
	fr  framing
	br  *bufio.Reader
	w   io.Writer
	log *slog.Logger

	// closer releases the underlying transport (the process's pipes).
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
		fr: ndjson{},
		// A command's aggregatedOutput can be large; bufio.Reader.ReadBytes
		// grows past this, so the size is a throughput knob rather than a cap.
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
		m, err := decode(c.fr, c.br)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
				readErr = err
			}
			break
		}
		c.dispatch(m)
	}
	c.finish(readErr)
}

func (c *conn) dispatch(m *message) {
	switch m.kind() {
	case kindNotification:
		c.handler.onNotification(m.Method, m.Params)
	case kindRequest:
		c.handler.onServerRequest(m.ID, m.Method, m.Params)
	case kindResponse, kindError:
		c.mu.Lock()
		ch, ok := c.pending[string(m.ID)]
		if ok {
			delete(c.pending, string(m.ID))
		}
		c.mu.Unlock()
		if !ok {
			// A reply to a call we already gave up on, or one we never made.
			// Worth a line: it is the shape a duplicated id would take.
			c.log.Warn("codex: response for an unknown request id", "id", string(m.ID))
			return
		}
		ch <- m
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
		for id, ch := range pending {
			_ = id
			ch <- &message{Error: &rpcError{Code: codeInternalError, Message: "connection closed"}}
		}
		if c.closer != nil {
			_ = c.closer()
		}
		c.handler.onClosed(err)
	})
}

// call sends a request and waits for its response.
func (c *conn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	select {
	case <-c.closed:
		return nil, c.closeErr()
	default:
	}

	c.mu.Lock()
	c.nextID++
	id := c.nextID
	rawID := json.RawMessage(fmt.Sprintf("%d", id))
	ch := make(chan *message, 1)
	c.pending[string(rawID)] = ch
	c.mu.Unlock()

	unregister := func() {
		c.mu.Lock()
		delete(c.pending, string(rawID))
		c.mu.Unlock()
	}

	m := message{ID: rawID, Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			unregister()
			return nil, fmt.Errorf("codex: marshalling %s params: %w", method, err)
		}
		m.Params = raw
	}
	if err := c.send(&m); err != nil {
		unregister()
		return nil, err
	}

	select {
	case <-ctx.Done():
		unregister()
		return nil, ctx.Err()
	case <-c.closed:
		unregister()
		return nil, c.closeErr()
	case reply := <-ch:
		if reply.Error != nil {
			return nil, reply.Error
		}
		return reply.Result, nil
	}
}

// respond answers a server→client request. result may be nil, which is sent as
// JSON null rather than omitted — a response with no `result` key at all is not
// a shape the untagged decoder on the far side can classify.
func (c *conn) respond(id json.RawMessage, result any) error {
	raw := json.RawMessage(jsonNull)
	if result != nil {
		b, err := json.Marshal(result)
		if err != nil {
			return fmt.Errorf("codex: marshalling response: %w", err)
		}
		raw = b
	}
	return c.send(&message{ID: id, Result: raw})
}

// respondError answers a server→client request with a failure. Every server
// request blocks Codex until it is answered, so this is what the adapter sends
// for the five it wants nothing to do with — dropping one hangs the runtime.
func (c *conn) respondError(id json.RawMessage, code int, msg string) error {
	return c.send(&message{ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func (c *conn) send(m *message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("codex: marshalling message: %w", err)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	select {
	case <-c.closed:
		return c.closeErr()
	default:
	}
	if err := c.fr.write(c.w, b); err != nil {
		return fmt.Errorf("codex: writing %s: %w", m.Method, err)
	}
	return nil
}

func (c *conn) closeErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return fmt.Errorf("%w: %w", ErrConnClosed, c.err)
	}
	return ErrConnClosed
}

// close shuts the transport down. run() notices and calls finish().
func (c *conn) close() {
	c.finish(nil)
}

func (c *conn) done() <-chan struct{} { return c.closed }
