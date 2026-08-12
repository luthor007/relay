package connector_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/connector"
	"github.com/luthor007/relay/relayd/internal/mcp"
)

// The printer is exercised against recorded-shape responses, not a socket:
// there is no printer on a build machine, and a connector that can only be
// tested against hardware is a connector nobody tests.

type stubHTTP struct {
	// answers is keyed "METHOD /path".
	answers map[string]stubAnswer
	seen    []string
	headers []http.Header
	err     error
}

type stubAnswer struct {
	code int
	body string
}

func (s *stubHTTP) Do(req *http.Request) (*http.Response, error) {
	key := req.Method + " " + req.URL.Path
	s.seen = append(s.seen, key)
	s.headers = append(s.headers, req.Header.Clone())
	if s.err != nil {
		return nil, s.err
	}
	a, ok := s.answers[key]
	if !ok {
		a = stubAnswer{code: http.StatusNotFound, body: "{}"}
	}
	return &http.Response{
		StatusCode: a.code,
		Body:       io.NopCloser(strings.NewReader(a.body)),
		Header:     http.Header{},
	}, nil
}

const statusPrinting = `{
  "printer": {"state":"PRINTING","temp_nozzle":215.0,"target_nozzle":215.0,"temp_bed":60.0,"target_bed":60.0},
  "job": {"id": 42, "state":"PRINTING", "progress": 41.6, "time_remaining": 3060,
          "file": {"name":"benchy.gcode","display_name":"benchy.gcode","path":"/usb/benchy.gcode"}}
}`

const statusIdle = `{"printer":{"state":"IDLE","temp_nozzle":24.0,"temp_bed":23.0}}`

const statusAttention = `{"printer":{"state":"ATTENTION","temp_nozzle":210.0,"temp_bed":60.0}}`

func newPrusa(h *stubHTTP) *connector.Prusa {
	return &connector.Prusa{
		Base:    "http://prusa.local",
		HTTP:    h,
		Storage: "usb",
		APIKey:  func(context.Context) (string, error) { return "synthetic-test-key", nil },
		Now:     func() time.Time { return at },
	}
}

func toolNamed(t *testing.T, p *connector.Prusa, name string) mcp.Tool {
	t.Helper()
	for _, tool := range p.Tools() {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("no tool named %q", name)
	return mcp.Tool{}
}

// ORCHESTRATOR.md §4b: the read half is useful alone, and it must not need the
// write half to be granted.
func TestPrusaReadHalfIsSelfContained(t *testing.T) {
	p := newPrusa(&stubHTTP{answers: map[string]stubAnswer{
		"GET /api/v1/status": {200, statusPrinting},
	}})

	tool := toolNamed(t, p, "prusa_status")
	if tool.Access != mcp.AccessRead || tool.Consequential() {
		t.Fatalf("status is a read with no consequence outside the machine: %+v", tool)
	}
	res, err := tool.Handler(context.Background(), mcp.Call{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"printing", "benchy.gcode", "42% done", "51 minutes left"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("status line is missing %q: %q", want, res.Text)
		}
	}
	env, ok := res.Structured.(connector.Envelope)
	if !ok {
		t.Fatalf("the structured half must be the normalized envelope, got %T", res.Structured)
	}
	if env.Connector != "prusa" || env.Kind != "printer.status" || env.At != at {
		t.Fatalf("envelope = %+v", env)
	}
}

// Every clause comes from a field that was present. A printer that reported no
// temperature produces a shorter sentence, not a guessed one.
func TestPrusaSaysLessRatherThanGuessing(t *testing.T) {
	p := newPrusa(&stubHTTP{answers: map[string]stubAnswer{
		"GET /api/v1/status": {200, `{"printer":{"state":"IDLE"}}`},
	}})
	res, err := toolNamed(t, p, "prusa_status").Handler(context.Background(), mcp.Call{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "The printer is idle." {
		t.Fatalf("want a short honest sentence, got %q", res.Text)
	}
}

// §4b names printing as one of the four things that confirm at the glasses.
func TestPrusaWriteHalfIsConsequential(t *testing.T) {
	p := newPrusa(&stubHTTP{})
	for _, name := range []string{"prusa_print", "prusa_stop"} {
		tool := toolNamed(t, p, name)
		if tool.Access != mcp.AccessWrite {
			t.Fatalf("%s must be a write", name)
		}
		if !tool.Consequential() {
			t.Fatalf("%s heats or abandons a machine in another room and must confirm", name)
		}
	}
}

func TestPrusaPrintSendsTheKeyAndTheHeader(t *testing.T) {
	h := &stubHTTP{answers: map[string]stubAnswer{
		"POST /api/v1/files/usb/benchy.gcode": {204, ""},
	}}
	p := newPrusa(h)

	res, err := toolNamed(t, p, "prusa_print").Handler(context.Background(),
		mcp.Call{Arguments: map[string]any{"path": "benchy.gcode"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Target != "benchy.gcode" {
		t.Fatalf("the target is what the console shows under last-used-for: %q", res.Target)
	}
	if len(h.headers) != 1 {
		t.Fatalf("want one request, got %v", h.seen)
	}
	if got := h.headers[0].Get("X-Api-Key"); got != "synthetic-test-key" {
		t.Fatalf("X-Api-Key = %q", got)
	}
	if got := h.headers[0].Get("Print-After-Upload"); got != "?1" {
		t.Fatalf("Print-After-Upload = %q", got)
	}
	env := res.Structured.(connector.Envelope)
	if env.Kind != "job.started" {
		t.Fatalf("envelope kind = %q", env.Kind)
	}
}

func TestPrusaPrintWithoutAFileIsAnAnswerNotACrash(t *testing.T) {
	p := newPrusa(&stubHTTP{})
	res, err := toolNamed(t, p, "prusa_print").Handler(context.Background(), mcp.Call{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "prusa_files") {
		t.Fatalf("the model should be told how to fix it: %+v", res)
	}
}

func TestPrusaStopNeedsAJob(t *testing.T) {
	p := newPrusa(&stubHTTP{answers: map[string]stubAnswer{
		"GET /api/v1/status": {200, statusIdle},
	}})
	res, err := toolNamed(t, p, "prusa_stop").Handler(context.Background(), mcp.Call{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "nothing to stop") {
		t.Fatalf("res = %+v", res)
	}
}

func TestPrusaFilesLists(t *testing.T) {
	p := newPrusa(&stubHTTP{answers: map[string]stubAnswer{
		"GET /api/v1/files/usb": {200, `{"children":[
			{"name":"parts","type":"FOLDER","children":[
				{"name":"bracket.bgcode","path":"/usb/parts/bracket.bgcode","type":"PRINT_FILE"}]},
			{"name":"benchy.gcode","path":"/usb/benchy.gcode","type":"PRINT_FILE"}]}`},
	}})
	res, err := toolNamed(t, p, "prusa_files").Handler(context.Background(), mcp.Call{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "bracket.bgcode") || !strings.Contains(res.Text, "benchy.gcode") {
		t.Fatalf("res = %q", res.Text)
	}
}

// "I could queue prints and tell you when they finish" is the promise, and the
// envelope is how it is kept.
func TestPollReportsTransitionsNotState(t *testing.T) {
	h := &stubHTTP{answers: map[string]stubAnswer{"GET /api/v1/status": {200, statusPrinting}}}
	p := newPrusa(h)
	ctx := context.Background()

	// The first poll establishes a baseline. Announcing what was already
	// happening would be reporting history as news.
	first, err := p.Poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 0 {
		t.Fatalf("the first poll must be silent: %+v", first)
	}
	// Still printing: nothing new.
	if evs, err := p.Poll(ctx); err != nil || len(evs) != 0 {
		t.Fatalf("an unchanged printer is not news: %+v %v", evs, err)
	}

	h.answers["GET /api/v1/status"] = stubAnswer{200, statusIdle}
	evs, err := p.Poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Kind != "job.finished" {
		t.Fatalf("want one job.finished, got %+v", evs)
	}
	if !strings.Contains(evs[0].Summary, "benchy.gcode") {
		t.Fatalf("summary = %q", evs[0].Summary)
	}

	h.answers["GET /api/v1/status"] = stubAnswer{200, statusPrinting}
	evs, _ = p.Poll(ctx)
	if len(evs) != 1 || evs[0].Kind != "job.started" {
		t.Fatalf("want job.started, got %+v", evs)
	}

	h.answers["GET /api/v1/status"] = stubAnswer{200, statusAttention}
	evs, _ = p.Poll(ctx)
	found := false
	for _, e := range evs {
		if e.Kind == "printer.attention" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a printer that needs a human must say so: %+v", evs)
	}
}

// "Nothing happened" and "we could not tell" are different answers and only one
// is safe to act on.
func TestUnreachablePrinterIsAnErrorNotSilence(t *testing.T) {
	h := &stubHTTP{err: errors.New("no route to host")}
	p := newPrusa(h)
	if _, err := p.Poll(context.Background()); err == nil {
		t.Fatal("an unreachable printer must not look like a quiet one")
	}

	noBase := &connector.Prusa{HTTP: h}
	if _, err := noBase.Poll(context.Background()); !errors.Is(err, connector.ErrNoPrinter) {
		t.Fatalf("want ErrNoPrinter, got %v", err)
	}
}

func TestRefusedKeyIsNamed(t *testing.T) {
	p := newPrusa(&stubHTTP{answers: map[string]stubAnswer{
		"GET /api/v1/status": {401, `{"message":"invalid api key"}`},
	}})
	_, err := toolNamed(t, p, "prusa_status").Handler(context.Background(), mcp.Call{})
	if err == nil || !strings.Contains(err.Error(), "refused Relay's API key") {
		t.Fatalf("err = %v", err)
	}
}

// The API key never appears in an envelope, a summary or an error.
func TestTheKeyNeverLeavesTheRequest(t *testing.T) {
	const key = "synthetic-test-key"
	p := newPrusa(&stubHTTP{answers: map[string]stubAnswer{"GET /api/v1/status": {200, statusPrinting}}})
	res, err := toolNamed(t, p, "prusa_status").Handler(context.Background(), mcp.Call{})
	if err != nil {
		t.Fatal(err)
	}
	env := res.Structured.(connector.Envelope)
	if strings.Contains(res.Text, key) || strings.Contains(env.Line(), key) {
		t.Fatal("the key leaked into what the agent reads")
	}
}

// Every tool a connector offers must belong to a half the connector said it
// opens, or the user was never told what granting it would mean.
func TestSetDropsToolsForUndeclaredHalves(t *testing.T) {
	set := connector.NewSet(&halfDeclaringConnector{})
	got := set.Tools(context.Background())
	if len(got) != 1 || got[0].Name != "half_read" {
		t.Fatalf("want only the declared half, got %v", got)
	}
}

type halfDeclaringConnector struct{}

func (halfDeclaringConnector) Descriptor() connector.Descriptor {
	return connector.Descriptor{
		Name:  "half",
		Opens: map[mcp.Access]string{mcp.AccessRead: "see things"},
	}
}

func (halfDeclaringConnector) Tools() []mcp.Tool {
	run := func(context.Context, mcp.Call) (mcp.Result, error) { return mcp.Result{}, nil }
	return []mcp.Tool{
		{Name: "half_read", Connector: "half", Access: mcp.AccessRead, Handler: run},
		{Name: "half_write", Connector: "half", Access: mcp.AccessWrite, Handler: run},
		{Name: "someone_elses", Connector: "gmail", Access: mcp.AccessRead, Handler: run},
	}
}

// A printer polled across a job boundary can have finished one print and
// started the next between two reads. Losing the "finished" there would break
// the only promise the connector makes unprompted.
func TestPollSeesAJobBoundaryBetweenTwoReads(t *testing.T) {
	const secondJob = `{"printer":{"state":"PRINTING"},
	  "job":{"id":43,"progress":1.0,"file":{"name":"bracket.gcode"}}}`

	h := &stubHTTP{answers: map[string]stubAnswer{"GET /api/v1/status": {200, statusPrinting}}}
	p := newPrusa(h)
	ctx := context.Background()
	if _, err := p.Poll(ctx); err != nil {
		t.Fatal(err)
	}

	h.answers["GET /api/v1/status"] = stubAnswer{200, secondJob}
	evs, err := p.Poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Kind != "job.finished" || evs[1].Kind != "job.started" {
		t.Fatalf("want finished then started, got %+v", evs)
	}
	if !strings.Contains(evs[0].Summary, "benchy.gcode") ||
		!strings.Contains(evs[1].Summary, "bracket.gcode") {
		t.Fatalf("the envelopes name the wrong files: %+v", evs)
	}
}
