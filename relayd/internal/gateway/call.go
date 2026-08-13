package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// Error is a call the gateway refused, carrying its own code and message.
//
// The message is theirs verbatim — "model not allowed: claude-cli/claude-opus-5"
// says more about what to do next than any sentence this package could
// construct from the code alone, which is the same reason llm.HTTPError keeps
// the provider's body.
type Error struct {
	Method string
	ErrorShape
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s: %s", e.Method, e.Code)
	}
	return fmt.Sprintf("%s: %s: %s", e.Method, e.Code, e.Message)
}

// Code returns the gateway's error code, or "" for anything that is not a
// refusal from the gateway — a dead socket, a cancelled context, bad JSON.
//
// Callers should branch on this rather than on the message: the codes are a
// closed set and the messages are prose that changes between releases.
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// answer is one delivered response, or the reason there will not be one.
type answer struct {
	frame *frame
	err   error
}

// Call sends one method and waits for its answer.
//
// params may be nil (sent as {}), any value that marshals to a JSON object, or
// a json.RawMessage already encoded. out may be nil when the answer is not
// worth reading.
//
// The wait is bounded by ctx and by the life of the socket: a connection that
// drops fails everything in flight with [ErrNotConnected] wrapping the read
// error, rather than leaving a caller on its own timeout wondering.
//
// No method is checked against hello-ok's features.methods first. That list is
// not a full enumeration, and a client that gated on it would refuse to make
// calls the gateway would have answered.
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	body, err := encodeParams(params)
	if err != nil {
		return fmt.Errorf("gateway: %s params: %w", method, err)
	}

	c.mu.Lock()
	if !c.connected || c.conn == nil {
		c.mu.Unlock()
		return ErrNotConnected
	}
	id := c.nextIDLocked()
	ch := make(chan answer, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.write(ctx, request{Type: frameReq, ID: id, Method: method, Params: body}); err != nil {
		return fmt.Errorf("gateway: %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case a := <-ch:
		if a.err != nil {
			return fmt.Errorf("gateway: %s: %w", method, a.err)
		}
		if a.frame.Error != nil {
			return &Error{Method: method, ErrorShape: *a.frame.Error}
		}
		if !a.frame.OK {
			return &Error{Method: method, ErrorShape: ErrorShape{
				Code:    CodeUnavailable,
				Message: "the gateway refused the call without saying why",
			}}
		}
		if out == nil || len(a.frame.Payload) == 0 {
			return nil
		}
		if err := json.Unmarshal(a.frame.Payload, out); err != nil {
			return fmt.Errorf("gateway: %s answered with something this client could not read: %w", method, err)
		}
		return nil
	}
}

// Sticky is [Call] for a subscription.
//
// A subscription lives in the gateway's memory, bound to the connection that
// asked for it, so a reconnect silently loses it: the socket comes back, the
// health screen says on, and no session change is ever reported again. Calls
// made through Sticky are recorded and made again after every handshake, which
// is the difference between a link that recovers and one that only looks like
// it did.
//
// Recorded on success only, and de-duplicated, so calling it twice with the
// same arguments does not replay it twice.
func (c *Client) Sticky(ctx context.Context, method string, params, out any) error {
	body, err := encodeParams(params)
	if err != nil {
		return fmt.Errorf("gateway: %s params: %w", method, err)
	}
	if err := c.Call(ctx, method, json.RawMessage(body), out); err != nil {
		return err
	}
	c.remember(method, body)
	return nil
}

// Forget drops a recorded subscription, so a reconnect stops re-making it.
func (c *Client) Forget(method string, params any) {
	body, err := encodeParams(params)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := c.sticky[:0]
	for _, s := range c.sticky {
		if s.method == method && string(s.params) == string(body) {
			continue
		}
		kept = append(kept, s)
	}
	c.sticky = kept
}

func (c *Client) remember(method string, params json.RawMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.sticky {
		if s.method == method && string(s.params) == string(params) {
			return
		}
	}
	c.sticky = append(c.sticky, stickyCall{method: method, params: params})
}

// dispatch routes one inbound frame.
func (c *Client) dispatch(data []byte) {
	f, err := decodeFrame(data)
	if err != nil {
		// A frame this client cannot parse is a gateway newer than it is.
		// Ignored rather than fatal, for the reason relaylink gives about
		// unknown control frames: a gateway release must not be a forced daemon
		// upgrade.
		c.log.Debug("gateway: ignoring an unparseable frame", "error", err)
		return
	}
	switch f.Type {
	case frameRes:
		c.deliver(f)
	case frameEvent:
		c.emit(f)
	default:
		c.log.Debug("gateway: ignoring an unrecognised frame", "type", f.Type)
	}
}

func (c *Client) deliver(f *frame) {
	c.mu.Lock()
	ch, ok := c.pending[f.ID]
	if ok {
		delete(c.pending, f.ID)
	}
	c.mu.Unlock()
	if !ok {
		// The second answer to a request already answered. The `agent` method
		// sends an acceptance and then a final frame with the same id — their
		// own source calls this out and says clients may take the first and
		// ignore the rest. Dropped at debug, because treating it as a protocol
		// violation would make every agent call log an error.
		c.log.Debug("gateway: a second answer to one request", "id", f.ID)
		return
	}
	select {
	case ch <- answer{frame: f}:
	default:
	}
}

func (c *Client) emit(f *frame) {
	if c.opts.OnEvent == nil {
		return
	}
	c.opts.OnEvent(Event{Name: f.Event, Payload: f.Payload, Seq: f.Seq})
}

// write sends one frame, one writer at a time.
func (c *Client) write(ctx context.Context, r request) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}

	c.wmu.Lock()
	defer c.wmu.Unlock()

	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return ErrNotConnected
	}
	return conn.Write(ctx, data)
}

// send is write for the handshake, which happens on a connection the client has
// not adopted yet — there is deliberately no c.conn to find until hello-ok has
// landed, so that no other goroutine can put a method on a socket the gateway
// has not accepted.
func (c *Client) send(ctx context.Context, conn Conn, id, method string, params any) error {
	body, err := encodeParams(params)
	if err != nil {
		return err
	}
	data, err := json.Marshal(request{Type: frameReq, ID: id, Method: method, Params: body})
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return conn.Write(ctx, data)
}

func (c *Client) nextID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nextIDLocked()
}

// nextIDLocked mints a request id.
//
// A counter, not a uuid: ids only have to be unique within one connection,
// which a counter guarantees more cheaply than randomness does, and a frame log
// where the ids read 1, 2, 3 is one a person can follow.
func (c *Client) nextIDLocked() string {
	c.n++
	return strconv.FormatUint(c.n, 10)
}

// encodeParams turns anything into a params object.
//
// nil becomes {} rather than being omitted, because several of the gateway's
// validators are closed objects that reject a missing params where they accept
// an empty one.
func encodeParams(params any) (json.RawMessage, error) {
	switch p := params.(type) {
	case nil:
		return json.RawMessage(`{}`), nil
	case json.RawMessage:
		if len(p) == 0 {
			return json.RawMessage(`{}`), nil
		}
		return p, nil
	}
	b, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 || string(b) == "null" {
		return json.RawMessage(`{}`), nil
	}
	return b, nil
}
