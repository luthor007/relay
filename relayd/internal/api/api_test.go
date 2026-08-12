package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/adapter/fake"
	"github.com/luthor007/relay/relayd/internal/api"
	"github.com/luthor007/relay/relayd/internal/bus"
	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/logx"
	"github.com/luthor007/relay/relayd/internal/registry"
	"github.com/luthor007/relay/relayd/internal/store"
)

type rig struct {
	t    *testing.T
	Srv  *api.Server
	HTTP *httptest.Server
	Reg  *registry.Registry
	Ad   *fake.Adapter
	Gate *bus.SpeechGate
	DB   *store.DB
}

// rigOpt tunes the rig itself rather than the server.
type rigOpt func(*rigConfig)

type rigConfig struct{ hideDB bool }

// withoutDB builds a server with no main database, which is the state of a box
// where backfill has not run. Those screens must still render.
func withoutDB(c *rigConfig) { c.hideDB = true }

func newRig(t *testing.T, o api.Options, opts ...rigOpt) *rig {
	t.Helper()

	var rc rigConfig
	for _, fn := range opts {
		fn(&rc)
	}

	db := o.DB
	if db == nil {
		var err error
		db, err = store.Open(filepath.Join(t.TempDir(), "relay.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
	}
	o.DB = db
	if rc.hideDB {
		o.DB = nil
	}

	var n int
	var mu sync.Mutex
	reg, err := registry.New(registry.Options{
		DB: db, Log: logx.Discard(), FlushInterval: 20 * time.Millisecond,
		Restart: registry.RestartPolicy{Mode: registry.RestartNever},
		NewID: func() string {
			mu.Lock()
			defer mu.Unlock()
			n++
			return "id-" + strconv.Itoa(n)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		reg.Shutdown(ctx)
	})

	ad := fake.New(fake.Options{Runtime: adapter.ClaudeCode})
	reg.AddAdapter(ad)

	gate := bus.NewSpeechGate()
	o.Registry = reg
	o.Gate = gate
	if o.Token == "" {
		o.Token = "test-token"
	}
	if o.Log == nil {
		o.Log = logx.Discard()
	}
	s, err := api.New(o)
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s)
	t.Cleanup(hs.Close)

	return &rig{t: t, Srv: s, HTTP: hs, Reg: reg, Ad: ad, Gate: gate, DB: db}
}

func (r *rig) get(t *testing.T, path string) (*http.Response, []byte) {
	t.Helper()
	return r.do(t, "GET", path, "")
}

// do issues an authenticated request and reads the whole body.
func (r *rig) do(t *testing.T, method, path, body string) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, r.HTTP.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+r.Srv.Token())
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.HTTP.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

// decode reads a JSON response, failing the test with the body on a parse error
// so a 500 does not surface as "unexpected end of JSON input".
func decode[T any](t *testing.T, resp *http.Response, body []byte, want int) T {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, want, body)
	}
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return v
}

func (r *rig) start(t *testing.T, subject string) (*registry.Entry, *fake.Session) {
	t.Helper()
	e, err := r.Reg.Start(context.Background(), registry.StartOptions{
		Runtime: adapter.ClaudeCode, Subject: subject, Workspace: "/repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	ss := r.Ad.Sessions()
	return e, ss[len(ss)-1]
}

// ------------------------------------------------------------------ bind --

func TestBindDefaultsToLoopbackAndExposureIsDeliberate(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:8787", "localhost:8787", "[::1]:8787"} {
		if err := api.CheckBind(ok, false); err != nil {
			t.Fatalf("CheckBind(%q) = %v, want nil", ok, err)
		}
	}
	for _, exposed := range []string{"0.0.0.0:8787", "192.168.1.20:8787", ":8787"} {
		if err := api.CheckBind(exposed, false); !errors.Is(err, api.ErrExposed) {
			t.Fatalf("CheckBind(%q, false) = %v, want ErrExposed", exposed, err)
		}
		if err := api.CheckBind(exposed, true); err != nil {
			t.Fatalf("CheckBind(%q, true) = %v, want nil", exposed, err)
		}
	}
	if w := api.LANWarning("0.0.0.0:8787"); !strings.Contains(w, "vault") {
		t.Fatalf("the warning should say what is at risk: %q", w)
	}
}

// ------------------------------------------------------------------ auth --

func TestEverythingButHealthzNeedsTheToken(t *testing.T) {
	r := newRig(t, api.Options{})

	resp, err := r.HTTP.Client().Get(r.HTTP.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("/healthz = %d", resp.StatusCode)
	}

	for _, path := range []string{"/v1/sessions", "/v1/health", "/v1/runtimes", "/v1/events"} {
		resp, err := r.HTTP.Client().Get(r.HTTP.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without a token = %d, want 401", path, resp.StatusCode)
		}
	}

	// A browser cannot set a header on EventSource, so the query parameter has
	// to work too.
	resp, err = r.HTTP.Client().Get(r.HTTP.URL + "/v1/sessions?token=" + r.Srv.Token())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("token in the query = %d", resp.StatusCode)
	}
}

func TestGeneratedTokensDiffer(t *testing.T) {
	a, err := api.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := api.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two generated tokens are identical")
	}
	if len(a) < 40 {
		t.Fatalf("token is too short to be a credential: %q", a)
	}
	// A server with no configured token generates one, the same way the pairing
	// code is generated rather than defaulted.
	s := newRig(t, api.Options{Token: " "}) // whitespace is not empty, but is not a token either
	if s.Srv.Token() == "" {
		t.Fatal("server has no token")
	}
}

// ------------------------------------------------------------------ REST --

func TestSessionsListPutsBlockedSessionsFirst(t *testing.T) {
	r := newRig(t, api.Options{})
	_, _ = r.start(t, "docs")
	e2, fs2 := r.start(t, "payments")
	fs2.Ask("turn-1", event.InputSpec{Ask: event.InputPermission, Prompt: "may I?"})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && e2.Row().State != store.SessionAwaiting {
		time.Sleep(2 * time.Millisecond)
	}

	_, body := r.get(t, "/v1/sessions")
	var list api.SessionList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	if len(list.Sessions) != 2 {
		t.Fatalf("sessions = %d", len(list.Sessions))
	}
	// DASHBOARD.md §3.1: unmissable, at the top.
	if !list.Sessions[0].Blocked || list.Sessions[0].ID != e2.ID() {
		t.Fatalf("blocked session is not first: %+v", list.Sessions)
	}
	if list.Sessions[0].Questions != 1 {
		t.Fatalf("questions = %d", list.Sessions[0].Questions)
	}
}

func TestSessionDetailShowsWhatTheRuntimeCannotDo(t *testing.T) {
	r := newRig(t, api.Options{})
	caps := adapter.Baseline(adapter.Hermes)
	acp := fake.New(fake.Options{Runtime: adapter.Hermes, Caps: &caps})
	r.Reg.AddAdapter(acp)

	e, err := r.Reg.Start(context.Background(), registry.StartOptions{
		Runtime: adapter.Hermes, Subject: "docs", Workspace: "/repo",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, body := r.get(t, "/v1/sessions/"+e.ID())
	var d api.SessionDetail
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	if !d.Live {
		t.Fatal("live session not marked live")
	}
	var sawSteer, sawCost bool
	for _, m := range d.Missing {
		if m == "steer" {
			sawSteer = true
		}
		if m == "cost_usd" {
			sawCost = true
		}
	}
	if !sawSteer || !sawCost {
		t.Fatalf("ACP cannot steer and reports no cost; missing = %v", d.Missing)
	}
	// Nil, never zero: the console shows a gap rather than claiming a free turn.
	if d.Session.CostUSD != nil {
		t.Fatalf("cost = %v, want null", *d.Session.CostUSD)
	}
}

func TestUnknownSessionIs404(t *testing.T) {
	r := newRig(t, api.Options{})
	resp, body := r.get(t, "/v1/sessions/nope")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	var e api.ErrorPayload
	if err := json.Unmarshal(body, &e); err != nil || e.Code != "no_such_session" {
		t.Fatalf("payload = %s", body)
	}
}

func TestHealthReportsRuntimesAndIncidents(t *testing.T) {
	r := newRig(t, api.Options{Version: "test", Listen: "127.0.0.1:8787"})
	_, _ = r.start(t, "payments")

	_, body := r.get(t, "/v1/health")
	var h api.Health
	if err := json.Unmarshal(body, &h); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	if !h.OK || h.Version != "test" || h.Listen != "127.0.0.1:8787" {
		t.Fatalf("health = %+v", h)
	}
	if h.Sessions.Total != 1 || h.Sessions.Live != 1 {
		t.Fatalf("sessions = %+v", h.Sessions)
	}
	if len(h.Runtimes) != 5 {
		t.Fatalf("health must list all five runtimes, got %d", len(h.Runtimes))
	}
	var cc, oc api.RuntimeState
	for _, rt := range h.Runtimes {
		switch rt.Runtime {
		case "claude-code":
			cc = rt
		case "opencode":
			oc = rt
		}
	}
	if !cc.Adapter {
		t.Fatal("claude-code has an adapter registered and health says it does not")
	}
	if oc.Adapter {
		t.Fatal("opencode has no adapter and health says it does")
	}
	if oc.Protocol != "acp" {
		t.Fatalf("opencode protocol = %q", oc.Protocol)
	}
}

func TestAnswerThroughREST(t *testing.T) {
	r := newRig(t, api.Options{})
	e, fs := r.start(t, "payments")
	ask := fs.Ask("turn-1", event.InputSpec{
		Ask:     event.InputPermission,
		Prompt:  "send the email?",
		Options: []event.Option{{ID: "yes", Name: "Send it", Kind: event.OptionAllowOnce}},
	})
	waitFor(t, func() bool { return len(e.Questions()) == 1 })

	body := strings.NewReader(`{"option":"yes","decision":"allow"}`)
	req, _ := http.NewRequest("POST", r.HTTP.URL+"/v1/sessions/"+e.ID()+"/answer", body)
	req.Header.Set("Authorization", "Bearer "+r.Srv.Token())
	resp, err := r.HTTP.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !ask.Answered() {
		t.Fatal("the reply never reached the runtime")
	}
	got, ok := ask.Outcome()
	if !ok || got.OptionID != "yes" {
		t.Fatalf("outcome = %+v", got)
	}
}

// ------------------------------------------------------------------- SSE --

func TestSSEStreamsTheListThenChanges(t *testing.T) {
	r := newRig(t, api.Options{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", r.HTTP.URL+"/v1/events?token="+r.Srv.Token(), nil)
	resp, err := r.HTTP.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}

	events := make(chan string, 16)
	go func() {
		buf := make([]byte, 4096)
		var acc strings.Builder
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				acc.WriteString(string(buf[:n]))
				for {
					s := acc.String()
					i := strings.Index(s, "\n\n")
					if i < 0 {
						break
					}
					select {
					case events <- s[:i]:
					default:
					}
					acc.Reset()
					acc.WriteString(s[i+2:])
				}
			}
			if err != nil {
				return
			}
		}
	}()

	first := waitEvent(t, events, "event: sessions")
	if !strings.Contains(first, `"sessions"`) {
		t.Fatalf("opening frame = %q", first)
	}

	_, _ = r.start(t, "payments")
	got := waitEvent(t, events, "event: session")
	if !strings.Contains(got, "payments") {
		t.Fatalf("session change = %q", got)
	}
}

func waitEvent(t *testing.T, ch <-chan string, prefix string) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-ch:
			if strings.HasPrefix(e, prefix) {
				return e
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q", prefix)
		}
	}
}

// -------------------------------------------------------------- WebSocket --

type wsClient struct {
	t  *testing.T
	c  *websocket.Conn
	in chan api.Envelope
}

func dial(t *testing.T, r *rig) *wsClient {
	t.Helper()
	url := "ws" + strings.TrimPrefix(r.HTTP.URL, "http") + "/v1/ws?token=" + r.Srv.Token()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	cl := &wsClient{t: t, c: c, in: make(chan api.Envelope, 64)}
	go func() {
		for {
			_, b, err := c.Read(context.Background())
			if err != nil {
				close(cl.in)
				return
			}
			var e api.Envelope
			if json.Unmarshal(b, &e) == nil {
				cl.in <- e
			}
		}
	}()
	t.Cleanup(func() { _ = c.CloseNow() })
	return cl
}

func (c *wsClient) send(t *testing.T, id, typ string, payload any) {
	t.Helper()
	e, err := api.Frame(id, typ, time.Now(), payload)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(e)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func (c *wsClient) sendRaw(t *testing.T, raw string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.c.Write(ctx, websocket.MessageText, []byte(raw)); err != nil {
		t.Fatal(err)
	}
}

func (c *wsClient) await(t *testing.T, typ string) api.Envelope {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e, ok := <-c.in:
			if !ok {
				t.Fatalf("socket closed while waiting for %s", typ)
			}
			if e.Type == typ {
				return e
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a %s frame", typ)
		}
	}
}

// awaitWithout waits for a frame of type typ and fails if any of the forbidden
// types arrives before it.
//
// await on its own cannot express "and it did not speak". It skips every frame
// it is not looking for, and Deliver publishes the speak frame *before* the
// notify frame — so a test that awaits notify and then checks the channel is
// empty has already thrown away the frame it is trying to prove is absent.
// That is not hypothetical: it is how TestQuietHoursPingNotifiesWithoutSpeaking
// passed against a speaks() rewritten to return true unconditionally.
func (c *wsClient) awaitWithout(t *testing.T, typ string, forbidden ...string) api.Envelope {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e, ok := <-c.in:
			if !ok {
				t.Fatalf("socket closed while waiting for %s", typ)
			}
			for _, bad := range forbidden {
				if e.Type == bad {
					t.Fatalf("got a %s frame while waiting for %s", bad, typ)
				}
			}
			if e.Type == typ {
				return e
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a %s frame", typ)
		}
	}
}

func TestWebSocketOpensWithTheSessionList(t *testing.T) {
	r := newRig(t, api.Options{})
	_, _ = r.start(t, "payments")

	c := dial(t, r)
	e := c.await(t, api.TypeSessionList)
	if e.V != api.Version {
		t.Fatalf("envelope version = %d", e.V)
	}
	if e.At == 0 || e.ID == "" {
		t.Fatalf("envelope = %+v", e)
	}
	var list api.SessionList
	if err := json.Unmarshal(e.Payload, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].Subject != "payments" {
		t.Fatalf("list = %+v", list.Sessions)
	}
}

func TestWebSocketRejectsTheWrongEnvelopeVersion(t *testing.T) {
	r := newRig(t, api.Options{})
	c := dial(t, r)
	c.await(t, api.TypeSessionList)

	c.sendRaw(t, `{"v":2,"id":"x","type":"utterance","at":1,"payload":{}}`)
	e := c.await(t, api.TypeError)
	var p api.ErrorPayload
	_ = json.Unmarshal(e.Payload, &p)
	if p.Code != api.CodeBadVersion {
		t.Fatalf("code = %q", p.Code)
	}
}

func TestWebSocketSessionCommands(t *testing.T) {
	r := newRig(t, api.Options{})
	e1, fs := r.start(t, "payments")
	c := dial(t, r)
	c.await(t, api.TypeSessionList)

	c.send(t, "req-1", api.TypeSessionCommand, api.SessionCommand{Command: "list"})
	c.await(t, api.TypeSessionList)

	c.send(t, "req-2", api.TypeSessionCommand, api.SessionCommand{
		Command: "send", Session: e1.ID(), Text: "run the tests",
	})
	ack := c.await(t, api.TypeAck)
	var a api.Ack
	_ = json.Unmarshal(ack.Payload, &a)
	if a.Re != "req-2" || !a.OK {
		t.Fatalf("ack = %+v", a)
	}
	waitFor(t, func() bool { return len(fs.Sent()) == 1 })

	c.send(t, "req-3", api.TypeSessionCommand, api.SessionCommand{
		Command: "send", Session: "nope", Text: "hi",
	})
	errE := c.await(t, api.TypeError)
	var p api.ErrorPayload
	_ = json.Unmarshal(errE.Payload, &p)
	if p.Code != api.CodeNoSuchSession || p.Re != "req-3" {
		t.Fatalf("error = %+v", p)
	}
}

// Verified absent on ACP: the caller has to be told it cannot steer, not that
// something failed, or it will retry instead of cancelling and re-prompting.
func TestSteeringOnACPDegradesVisibly(t *testing.T) {
	r := newRig(t, api.Options{})
	caps := adapter.Baseline(adapter.OpenClaw)
	acp := fake.New(fake.Options{Runtime: adapter.OpenClaw, Caps: &caps})
	r.Reg.AddAdapter(acp)
	e, err := r.Reg.Start(context.Background(), registry.StartOptions{
		Runtime: adapter.OpenClaw, Workspace: "/repo",
	})
	if err != nil {
		t.Fatal(err)
	}

	c := dial(t, r)
	c.await(t, api.TypeSessionList)
	c.send(t, "req-1", api.TypeSessionCommand, api.SessionCommand{
		Command: "steer", Session: e.ID(), Turn: "t1", Text: "actually, do the other thing",
	})
	frame := c.await(t, api.TypeError)
	var p api.ErrorPayload
	_ = json.Unmarshal(frame.Payload, &p)
	if p.Code != api.CodeUnsupported {
		t.Fatalf("code = %q, message = %q", p.Code, p.Message)
	}
	if !strings.Contains(p.Message, "steer") {
		t.Fatalf("the message should name the capability: %q", p.Message)
	}
}

func TestCaptureFramesSayWhichMilestone(t *testing.T) {
	r := newRig(t, api.Options{})
	c := dial(t, r)
	c.await(t, api.TypeSessionList)

	for _, typ := range []string{api.TypeAudioChunk, api.TypePhoto, api.TypeSyncOffer} {
		c.send(t, "req-"+typ, typ, map[string]any{})
		e := c.await(t, api.TypeError)
		var p api.ErrorPayload
		_ = json.Unmarshal(e.Payload, &p)
		if p.Code != api.CodeNotImplemented {
			t.Fatalf("%s: code = %q", typ, p.Code)
		}
		if !strings.Contains(p.Milestone, "M4") {
			t.Fatalf("%s: milestone = %q, should name M4", typ, p.Milestone)
		}
	}
}

func TestUtteranceWithoutARouterNamesTheMilestone(t *testing.T) {
	r := newRig(t, api.Options{})
	c := dial(t, r)
	c.await(t, api.TypeSessionList)

	c.send(t, "u-1", api.TypeUtterance, api.Utterance{Text: "run the tests", Final: true})
	e := c.await(t, api.TypeError)
	var p api.ErrorPayload
	_ = json.Unmarshal(e.Payload, &p)
	if p.Code != api.CodeNotImplemented || !strings.Contains(p.Milestone, "routing") {
		t.Fatalf("payload = %+v", p)
	}
}

func TestUtteranceReachesTheRouterAndDrivesTheGate(t *testing.T) {
	got := make(chan api.Utterance, 4)
	r := newRig(t, api.Options{
		Utterances: api.UtteranceFunc(func(_ context.Context, u api.Utterance) error {
			got <- u
			return nil
		}),
	})
	c := dial(t, r)
	c.await(t, api.TypeSessionList)

	c.send(t, "u-1", api.TypeUtterance, api.Utterance{Text: "run the", Final: false})
	<-got
	waitFor(t, func() bool { return r.Gate.Busy() })

	c.send(t, "u-2", api.TypeUtterance, api.Utterance{Text: "run the tests", Final: true})
	<-got
	waitFor(t, func() bool { return !r.Gate.Busy() })
	c.await(t, api.TypeAck)
}

// ---------------------------------------------------------- ping delivery --

func TestBlockingPingBecomesAConfirmRequestAnsweredByConsent(t *testing.T) {
	r := newRig(t, api.Options{})
	e, fs := r.start(t, "payments")

	c := dial(t, r)
	c.await(t, api.TypeSessionList)

	ask := fs.Ask("turn-1", event.InputSpec{
		Ask:    event.InputPermission,
		Prompt: "send the release email?",
		Options: []event.Option{
			{ID: "once", Name: "Send it", Kind: event.OptionAllowOnce},
			{ID: "always", Name: "Always allow email", Kind: event.OptionAllowAlways},
		},
	})
	waitFor(t, func() bool { return len(e.Questions()) == 1 })

	if err := r.Srv.Deliver(context.Background(), bus.Ping{
		ID: "ping-1", Class: bus.ClassBlocking, At: time.Now(),
		Sessions: []string{e.ID()}, Ask: ask, Consequential: true,
		// No delivery flags: bus.Ping has none any more, and this ping carries
		// the least favourable facts a blocking ping can carry — no gap was
		// found and quiet hours are the zero value. It must speak anyway,
		// because ADAPTERS.md §7 exempts blocking pings from both.
		Gap: false, Quiet: false, Line: "payments: send the release email?",
	}); err != nil {
		t.Fatal(err)
	}

	frame := c.await(t, api.TypeConfirmRequest)
	var req api.ConfirmRequest
	if err := json.Unmarshal(frame.Payload, &req); err != nil {
		t.Fatal(err)
	}
	if req.ActionID != "ping-1" || !req.Consequential {
		t.Fatalf("confirm = %+v", req)
	}
	if len(req.Options) != 2 {
		t.Fatalf("options = %+v", req.Options)
	}
	// ORCHESTRATOR.md §4b: a standing grant is marked so nothing picks it for
	// the user.
	if req.Options[0].Standing || !req.Options[1].Standing {
		t.Fatalf("standing flags = %+v", req.Options)
	}
	// A blocked session may interrupt.
	sp := c.await(t, api.TypeSpeak)
	var speak api.Speak
	_ = json.Unmarshal(sp.Payload, &speak)
	if !speak.Interrupt {
		t.Fatal("a blocking ping must be able to interrupt")
	}

	c.send(t, "c-1", api.TypeConsentDecision, api.ConsentDecision{
		ActionID: "ping-1", Approved: true, Option: "once",
	})
	c.await(t, api.TypeAck)
	if !ask.Answered() {
		t.Fatal("the consent decision never reached the runtime")
	}
	out, _ := ask.Outcome()
	if out.OptionID != "once" || out.Decision != event.DecisionAllow {
		t.Fatalf("outcome = %+v", out)
	}
}

// The render layer, not the ping policy, is what holds the speech during quiet
// hours — PRODUCT.md §6b. So the ping arrives with the facts only (the gap was
// found, quiet hours apply) and this test asserts the decision the voice
// backend reaches from them. Setting Gap true is what makes it sharp: nothing
// but the quiet-hours rule can be the reason the speech is missing.
func TestQuietHoursPingNotifiesWithoutSpeaking(t *testing.T) {
	r := newRig(t, api.Options{})
	c := dial(t, r)
	c.await(t, api.TypeSessionList)

	if err := r.Srv.Deliver(context.Background(), bus.Ping{
		ID: "ping-2", Class: bus.ClassInformational, At: time.Now(),
		Sessions: []string{"s1"}, Gap: true, Quiet: true,
		Line: "payments is done",
	}); err != nil {
		t.Fatal(err)
	}

	n := c.awaitWithout(t, api.TypeNotify, api.TypeSpeak)
	var notify api.Notify
	_ = json.Unmarshal(n.Payload, &notify)
	if !notify.Silent || notify.Body != "payments is done" {
		t.Fatalf("notify = %+v", notify)
	}

	select {
	case e := <-c.in:
		if e.Type == api.TypeSpeak {
			t.Fatal("spoke during quiet hours")
		}
	case <-time.After(200 * time.Millisecond):
	}
}

// ADAPTERS.md §7: "past the gap timeout the speech is dropped and the
// notification is not." That used to be a boolean the ping policy computed; it
// is now a rule this layer applies to a fact, and it has to survive the move.
//
// The pair of this and TestACompletionSpeaksOnceTheGapArrives is what stops
// speaks() from degenerating: one of them fails if it ever returns a constant.
func TestACompletionThatFoundNoGapNotifiesWithoutSpeaking(t *testing.T) {
	r := newRig(t, api.Options{})
	c := dial(t, r)
	c.await(t, api.TypeSessionList)

	if err := r.Srv.Deliver(context.Background(), bus.Ping{
		ID: "ping-3", Class: bus.ClassInformational, At: time.Now(),
		Sessions: []string{"s1"}, Gap: false, Quiet: false,
		Line: "payments is done",
	}); err != nil {
		t.Fatal(err)
	}

	n := c.awaitWithout(t, api.TypeNotify, api.TypeSpeak)
	var notify api.Notify
	_ = json.Unmarshal(n.Payload, &notify)
	// Not quiet hours, so the notification still makes a sound. Only the speech
	// was dropped.
	if notify.Silent || notify.Body != "payments is done" {
		t.Fatalf("notify = %+v", notify)
	}

	select {
	case e := <-c.in:
		if e.Type == api.TypeSpeak {
			t.Fatal("spoke over a conversation that never left a gap")
		}
	case <-time.After(200 * time.Millisecond):
	}
}

func TestACompletionSpeaksOnceTheGapArrives(t *testing.T) {
	r := newRig(t, api.Options{})
	c := dial(t, r)
	c.await(t, api.TypeSessionList)

	if err := r.Srv.Deliver(context.Background(), bus.Ping{
		ID: "ping-4", Class: bus.ClassInformational, At: time.Now(),
		Sessions: []string{"s1"}, Gap: true, Quiet: false,
		Line: "payments is done",
	}); err != nil {
		t.Fatal(err)
	}

	sp := c.await(t, api.TypeSpeak)
	var speak api.Speak
	_ = json.Unmarshal(sp.Payload, &speak)
	if speak.Text != "payments is done" {
		t.Fatalf("speak = %+v", speak)
	}
	// A completion never interrupts, however clear the gap was. Only a blocked
	// session may (ADAPTERS.md §7).
	if speak.Interrupt {
		t.Fatal("a completion claimed the right to interrupt")
	}
	if speak.Session != "s1" {
		t.Fatalf("a single-session ping must name its session: %+v", speak)
	}
}

func TestRetractionReachesThePhone(t *testing.T) {
	r := newRig(t, api.Options{})
	c := dial(t, r)
	c.await(t, api.TypeSessionList)

	if err := r.Srv.Retract(context.Background(), "ping-9", "answered in a terminal"); err != nil {
		t.Fatal(err)
	}
	frame := c.await(t, api.TypeConfirmResolved)
	var res api.ConfirmResolved
	_ = json.Unmarshal(frame.Payload, &res)
	if res.ActionID != "ping-9" || res.Reason == "" {
		t.Fatalf("resolved = %+v", res)
	}
}

func TestRegistryChangesPushANewSessionList(t *testing.T) {
	r := newRig(t, api.Options{})
	c := dial(t, r)
	c.await(t, api.TypeSessionList)

	_, _ = r.start(t, "docs")

	frame := c.await(t, api.TypeSessionList)
	var list api.SessionList
	_ = json.Unmarshal(frame.Payload, &list)
	if len(list.Sessions) != 1 || list.Sessions[0].Subject != "docs" {
		t.Fatalf("pushed list = %+v", list.Sessions)
	}
}

// ---------------------------------------------------------------- codec --

func TestEnvelopeRoundTrip(t *testing.T) {
	at := time.UnixMilli(1754800000000)
	e, err := api.Frame("abc", api.TypeSpeak, at, api.Speak{Text: "on it"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(e)
	back, err := api.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if back.V != 1 || back.ID != "abc" || back.Type != api.TypeSpeak || back.At != at.UnixMilli() {
		t.Fatalf("round trip = %+v", back)
	}
	sp, err := api.Bind[api.Speak](back)
	if err != nil || sp.Text != "on it" {
		t.Fatalf("payload = %+v (%v)", sp, err)
	}

	if _, err := api.Decode([]byte(`{"v":1,"id":"x","at":1}`)); err == nil {
		t.Fatal("a frame with no type parsed")
	}
	if _, err := api.Decode([]byte(`not json`)); err == nil {
		t.Fatal("garbage parsed")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}
