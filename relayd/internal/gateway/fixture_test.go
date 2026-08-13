package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// The tests in this package open no socket. They play the probe's capture of a
// real gateway (docs/fixtures/openclaw/, openclaw 2026.7.1-2) back through the
// client over an in-memory pipe, which is the only way to be sure the shapes
// here match the wire rather than matching each other.

const fixtureDir = "../../../docs/fixtures/openclaw"

// capture is one line of a capture file. dir is from the recording client's
// point of view: "in" is gateway to client, "out" is client to gateway, "meta"
// is the probe's own annotation and not a frame at all.
type capture struct {
	Who   string          `json:"who"`
	Dir   string          `json:"dir"`
	Frame json.RawMessage `json:"frame"`
}

// loadCapture reads one capture file, or skips the test when the fixtures are
// not in the tree.
//
// Skipped rather than failed on purpose: the capture is evidence, not source,
// and a checkout without it should still be able to run the tests that do not
// need it. A checkout WITH it gets the stronger ones.
func loadCapture(t *testing.T, name string) []capture {
	t.Helper()
	path := filepath.Join(fixtureDir, name)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		t.Skipf("no capture at %s; the gateway fixtures are not in this checkout", path)
	}
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var out []capture
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var c capture
		if err := json.Unmarshal(line, &c); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		out = append(out, c)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return out
}

// serverFrames keeps the frames the gateway sent, for one socket of a capture.
// who is "" for the single-socket files.
func serverFrames(recs []capture, who string) []json.RawMessage {
	var out []json.RawMessage
	for _, r := range recs {
		if r.Dir != "in" || r.Who != who {
			continue
		}
		out = append(out, r.Frame)
	}
	return out
}

var errPipeClosed = errors.New("pipe closed")

// pipeConn is a [Conn] with no network under it.
type pipeConn struct {
	in     chan json.RawMessage
	out    chan []byte
	closed chan struct{}
	once   sync.Once
}

func newPipeConn() *pipeConn {
	return &pipeConn{
		in:     make(chan json.RawMessage, 64),
		out:    make(chan []byte, 64),
		closed: make(chan struct{}),
	}
}

func (p *pipeConn) Read(ctx context.Context) ([]byte, error) {
	// Drain first, so a server that pushed its last frame and closed does not
	// lose it to a coin flip inside select.
	select {
	case b := <-p.in:
		return b, nil
	default:
	}
	select {
	case b := <-p.in:
		return b, nil
	case <-p.closed:
		return nil, errPipeClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *pipeConn) Write(ctx context.Context, b []byte) error {
	select {
	case p.out <- b:
		return nil
	case <-p.closed:
		return errPipeClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *pipeConn) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}

// clientReq is one frame the client sent.
type clientReq struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// replayServer plays a captured gateway back.
//
// Events are pushed as they come. A captured response waits for the client's
// next request and is re-stamped with that request's id, because the ids in the
// capture are the probe's uuids and this client mints its own — which is the
// only edit made to a captured frame anywhere in these tests.
type replayServer struct {
	frames []json.RawMessage
	conn   *pipeConn
	ids    chan string

	mu   sync.Mutex
	reqs []clientReq
}

func newReplayServer(frames []json.RawMessage) *replayServer {
	return &replayServer{frames: frames, conn: newPipeConn(), ids: make(chan string, 64)}
}

// dial hands the client this server's socket. A second dial gets a closed one,
// so a test that expects one connection notices when it gets two.
func (s *replayServer) dial(ctx context.Context, _ string) (Conn, error) {
	return s.conn, nil
}

func (s *replayServer) serve(ctx context.Context) {
	go s.readLoop(ctx)
	for _, f := range s.frames {
		var head struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(f, &head)
		if head.Type == frameRes {
			select {
			case id := <-s.ids:
				f = withID(f, id)
			case <-ctx.Done():
				return
			}
		}
		select {
		case s.conn.in <- f:
		case <-ctx.Done():
			return
		}
	}
	<-ctx.Done()
}

func (s *replayServer) readLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.conn.closed:
			return
		case b := <-s.conn.out:
			var r clientReq
			if err := json.Unmarshal(b, &r); err != nil {
				continue
			}
			s.mu.Lock()
			s.reqs = append(s.reqs, r)
			s.mu.Unlock()
			select {
			case s.ids <- r.ID:
			case <-ctx.Done():
				return
			}
		}
	}
}

// requests is every frame the client has sent so far.
func (s *replayServer) requests() []clientReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]clientReq, len(s.reqs))
	copy(out, s.reqs)
	return out
}

// methods is requests(), named.
func (s *replayServer) methods() []string {
	var out []string
	for _, r := range s.requests() {
		out = append(out, r.Method)
	}
	return out
}

// answerServer answers each client request with whatever handle returns, which
// is how a test says something the capture cannot — two answers to one request,
// or an answer that never comes.
type answerServer struct {
	conn   *pipeConn
	handle func(clientReq) []json.RawMessage

	mu   sync.Mutex
	reqs []clientReq
}

func newAnswerServer(handle func(clientReq) []json.RawMessage) *answerServer {
	return &answerServer{conn: newPipeConn(), handle: handle}
}

func (s *answerServer) dial(context.Context, string) (Conn, error) { return s.conn, nil }

func (s *answerServer) serve(ctx context.Context) {
	select {
	case s.conn.in <- json.RawMessage(challengeFrame):
	case <-ctx.Done():
		return
	}
	for {
		var b []byte
		select {
		case <-ctx.Done():
			return
		case <-s.conn.closed:
			return
		case b = <-s.conn.out:
		}
		var r clientReq
		if err := json.Unmarshal(b, &r); err != nil {
			continue
		}
		s.mu.Lock()
		s.reqs = append(s.reqs, r)
		s.mu.Unlock()
		for _, f := range s.handle(r) {
			select {
			case s.conn.in <- withID(f, r.ID):
			case <-ctx.Done():
				return
			}
		}
	}
}

func (s *answerServer) requests() []clientReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]clientReq, len(s.reqs))
	copy(out, s.reqs)
	return out
}

func withID(frame json.RawMessage, id string) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(frame, &m); err != nil {
		return frame
	}
	m["id"], _ = json.Marshal(id)
	out, err := json.Marshal(m)
	if err != nil {
		return frame
	}
	return out
}
