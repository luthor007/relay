package apps

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// The host tests drive the capability channel directly, with no app process.
//
// That is the point of them: the runner is a file inside the app's own root and
// the app is untrusted code, so "the runner would never send that frame" is not
// a boundary. These tests send the frames the runner would never send.

// channel is a pair of pipes plus a running Host, driven by hand.
type channel struct {
	t    *testing.T
	host *Host
	in   *io.PipeWriter // frames from the "app"
	out  *io.PipeReader // frames to the "app"
	dec  *json.Decoder
	done chan error
}

func newChannel(t *testing.T, o HostOptions) *channel {
	t.Helper()
	if o.Redact == nil {
		o.Redact = Detector()
	}
	h, err := NewHost(o)
	if err != nil {
		t.Fatal(err)
	}
	appReader, hostWriter := io.Pipe()
	hostReader, appWriter := io.Pipe()
	c := &channel{t: t, host: h, in: appWriter, out: appReader,
		dec: json.NewDecoder(appReader), done: make(chan error, 1)}
	go func() {
		c.done <- h.Serve(context.Background(), hostReader, hostWriter, startFrame{T: frameStart})
	}()
	// Consume the start frame the host always sends first.
	var start map[string]any
	if err := c.dec.Decode(&start); err != nil {
		t.Fatal(err)
	}
	if start["t"] != frameStart {
		t.Fatalf("first frame is %v, want start", start["t"])
	}
	return c
}

func (c *channel) call(method Method, args any) resultFrame {
	c.t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		c.t.Fatal(err)
	}
	if err := json.NewEncoder(c.in).Encode(callFrame{
		T: frameCall, ID: 1, Method: method, Args: raw,
	}); err != nil {
		c.t.Fatal(err)
	}
	var out resultFrame
	if err := c.dec.Decode(&out); err != nil {
		c.t.Fatal(err)
	}
	return out
}

func (c *channel) finish() {
	_ = json.NewEncoder(c.in).Encode(callFrame{T: frameDone})
	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		c.t.Error("Serve did not return after the done frame")
	}
	c.in.Close()
	c.out.Close()
}

func installedWith(granted ...Scope) Installed {
	return Installed{
		Manifest: Manifest{ID: "dev.test.host", Name: "Host Test", Version: "1.0.0"},
		Granted:  granted,
	}
}

func TestAMethodTheGrantDidNotMintDoesNotExist(t *testing.T) {
	c := newChannel(t, HostOptions{Installed: installedWith(ScopeMemoryRead)})
	defer c.finish()

	res := c.call(MethodGlassesCapture, map[string]any{})
	if res.Error == nil {
		t.Fatal("an ungranted method must not run")
	}
	if res.Error.Code != CodeNoCapability {
		t.Errorf("code = %q, want %q — an app that can tell 'refused' from 'absent' has a probe of "+
			"what the user declined", res.Error.Code, CodeNoCapability)
	}
	if strings.Contains(strings.ToLower(res.Error.Message), "denied") {
		t.Errorf("the message must not read as a refusal: %q", res.Error.Message)
	}

	// And a method that is not part of the SDK at all is told so, differently.
	res = c.call("glasses.setIndicator", map[string]any{})
	if res.Error == nil || res.Error.Code != CodeNoCapability {
		t.Fatalf("an invented method must be refused: %+v", res.Error)
	}
	if !strings.Contains(res.Error.Message, "not part of the SDK") {
		t.Errorf("message = %q", res.Error.Message)
	}
}

func TestGrantedButUnavailableIsADifferentAnswer(t *testing.T) {
	// The scope was granted and this box has no glasses. "You were granted the
	// camera and there is none" and "you were never granted the camera" are
	// different facts, and an app should be able to say which it hit.
	//
	// This test used to assert CodeFailed, which is the answer that makes those
	// two facts indistinguishable from a crash — the assertion contradicted the
	// sentence above it. The cause was in toWireError: the `unavailable` helper
	// wraps ErrUnavailable and that error was not in the switch, so every one of
	// its callers answered `failed`. An author whose app got `failed` on a box
	// with no glasses goes looking for a bug in their own code.
	c := newChannel(t, HostOptions{Installed: installedWith(ScopeGlassesCamera)})
	defer c.finish()

	res := c.call(MethodGlassesCapture, map[string]any{})
	if res.Error == nil || res.Error.Code != CodeUnavailable {
		t.Fatalf("expected unavailable about the box, got %+v", res.Error)
	}
	if !strings.Contains(res.Error.Message, "no glasses are paired") {
		t.Errorf("the message must be about the box: %q", res.Error.Message)
	}
}

func TestTheHostRefusesAnUngrantedMethodEvenFromATamperedRunner(t *testing.T) {
	// The whole reason the check exists twice. Capabilities() decided the runner
	// would not build ctx.memory; this asserts the table refuses anyway.
	inst := installedWith()
	// Six: the four self-directed ones, plus ui.render and ui.ask, which cost
	// no scope because a view reaches nothing of the user's.
	if got := len(Capabilities(inst.Granted)); got != 6 {
		t.Fatalf("an app with no scopes was minted %d capabilities, want 6", got)
	}
	c := newChannel(t, HostOptions{Installed: inst, Memory: mustMemory(t)})
	defer c.finish()

	res := c.call(MethodMemorySearch, map[string]any{"query": "everything"})
	if res.Error == nil || res.Error.Code != CodeNoCapability {
		t.Fatalf("the dispatch table must refuse it: %+v", res)
	}
	if res.Result != nil {
		t.Error("nothing may come back")
	}
}

func mustMemory(t *testing.T) *MemoryCap {
	t.Helper()
	m, err := NewMemory(MemoryOptions{
		Source: &StaticSource{Episodes: []Episode{{ID: "ep-1", Kind: "meeting"}}},
		Log:    &MemoryAccessLog{}, Redact: Detector(), AppID: "dev.test.host",
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAppLogsAreRedactedBeforeTheySink(t *testing.T) {
	sink := &MemoryLogSink{}
	c := newChannel(t, HostOptions{Installed: installedWith(), Log: sink})
	defer c.finish()

	const key = "AKIA" + "IOSFODNN7EXAMPLE"
	res := c.call(MethodLog, map[string]any{
		"message": "the key is " + key,
		"data":    map[string]any{"token": key, "count": 3},
	})
	if res.Error != nil {
		t.Fatalf("log should not fail: %+v", res.Error)
	}
	lines := sink.All()
	if len(lines) != 1 {
		t.Fatalf("lines = %+v", lines)
	}
	if strings.Contains(lines[0].Message, key) {
		t.Errorf("a credential reached the log: %q", lines[0].Message)
	}
	if s, _ := lines[0].Data["token"].(string); strings.Contains(s, key) {
		t.Errorf("a credential reached the log's structured half: %q", s)
	}
	if lines[0].Data["count"] != float64(3) {
		t.Errorf("non-string data should pass through: %v", lines[0].Data["count"])
	}
}

func TestALogThatCannotBeRecordedIsCounted(t *testing.T) {
	failing := LogSinkFunc(func(context.Context, LogLine) error { return errors.New("disk full") })
	c := newChannel(t, HostOptions{Installed: installedWith(), Log: failing})
	c.call(MethodLog, map[string]any{"message": "hello"})
	c.finish()

	if _, _, _, dropped := c.host.Counts(); dropped != 1 {
		t.Errorf("dropped = %d, want 1 — a log that silently vanishes is worse than one that is absent", dropped)
	}
}

func TestStorageIsNamespacedToTheApp(t *testing.T) {
	store := &MemoryStorage{}
	c := newChannel(t, HostOptions{Installed: installedWith(), Storage: store})
	defer c.finish()

	if res := c.call(MethodStorageSet, map[string]any{"key": "runs", "value": 7}); res.Error != nil {
		t.Fatalf("set: %+v", res.Error)
	}
	res := c.call(MethodStorageGet, map[string]any{"key": "runs"})
	if res.Error != nil {
		t.Fatalf("get: %+v", res.Error)
	}
	if res.Result != float64(7) {
		t.Errorf("result = %v", res.Result)
	}
	// Another app's id must not reach the same key.
	v, _ := store.Get(context.Background(), "dev.test.other", "runs")
	if v != nil {
		t.Errorf("another app can see this app's storage: %s", v)
	}
	// And a key that is really a path is refused.
	if res := c.call(MethodStorageSet, map[string]any{"key": "../../etc/passwd", "value": 1}); res.Error == nil {
		t.Error("a storage key that is a path must be refused")
	}
}

func TestConcurrentCallsAreServedAndBounded(t *testing.T) {
	store := &MemoryStorage{}
	inst := installedWith()
	h, err := NewHost(HostOptions{Installed: inst, Storage: store, Redact: Detector(), MaxConcurrent: 2})
	if err != nil {
		t.Fatal(err)
	}
	appReader, hostWriter := io.Pipe()
	hostReader, appWriter := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- h.Serve(context.Background(), hostReader, hostWriter, startFrame{T: frameStart}) }()

	dec := json.NewDecoder(appReader)
	var start map[string]any
	if err := dec.Decode(&start); err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(appWriter)
	const n = 8
	// Written from a goroutine while the replies are being read. An io.Pipe is
	// unbuffered, so filling it before reading anything back would deadlock the
	// *test*, not the host — the real channel is an os.Pipe with a kernel
	// buffer, and an app that stops reading its own replies is stalling itself
	// until the wall-clock supervisor kills it.
	go func() {
		for i := 0; i < n; i++ {
			raw, _ := json.Marshal(map[string]any{"key": "k", "value": i})
			if err := enc.Encode(callFrame{
				T: frameCall, ID: int64(i + 1), Method: MethodStorageSet, Args: raw,
			}); err != nil {
				return
			}
		}
	}()
	seen := map[int64]bool{}
	for i := 0; i < n; i++ {
		var f resultFrame
		if err := dec.Decode(&f); err != nil {
			t.Fatal(err)
		}
		seen[f.ID] = true
	}
	if len(seen) != n {
		t.Errorf("answered %d of %d calls", len(seen), n)
	}
	_ = enc.Encode(callFrame{T: frameDone})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("Serve did not return")
	}
	appWriter.Close()
	appReader.Close()
}

func TestAnUnreadableFrameEndsTheChannel(t *testing.T) {
	h, err := NewHost(HostOptions{Installed: installedWith(), Redact: Detector()})
	if err != nil {
		t.Fatal(err)
	}
	hostReader, appWriter := io.Pipe()
	go func() {
		_, _ = appWriter.Write([]byte("this is not JSON\n"))
		appWriter.Close()
	}()
	err = h.Serve(context.Background(), hostReader, io.Discard, startFrame{T: frameStart})
	if err == nil || !strings.Contains(err.Error(), "unreadable frame") {
		t.Fatalf("a frame the host cannot parse has to end the channel: %v", err)
	}
	if h.Finished() {
		t.Error("that is not the app finishing")
	}
}

func TestAnAppErrorSurvivesTheChannel(t *testing.T) {
	h, err := NewHost(HostOptions{Installed: installedWith(), Redact: Detector()})
	if err != nil {
		t.Fatal(err)
	}
	hostReader, appWriter := io.Pipe()
	go func() {
		_ = json.NewEncoder(appWriter).Encode(callFrame{
			T: frameFailed, Error: &appError{Name: "TypeError", Message: "cannot read x of undefined"},
		})
		appWriter.Close()
	}()
	if err := h.Serve(context.Background(), hostReader, io.Discard, startFrame{T: frameStart}); err != nil {
		t.Fatalf("an app failing is not a transport error: %v", err)
	}
	if !h.Finished() {
		t.Error("the app did say something, so it finished")
	}
	if got := h.AppError(); got == nil || !strings.Contains(got.Error(), "TypeError") {
		t.Errorf("the app's own error must survive: %v", got)
	}
}
