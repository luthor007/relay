package apps

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Shared fixtures. Every app in this file is written to disk and run by a real
// Node process — there is no double for the runtime, because a sandbox that is
// only exercised through a mock is a sandbox nobody has tested.

func requireNode(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node on PATH; the app runtime needs one")
	}
	return p
}

// writeApp materialises an app package and returns its directory.
func writeApp(t *testing.T, manifest string, source string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "relay.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// testRuntime is a runtime wired to doubles, with the real sandbox.
type testRuntime struct {
	*Runtime
	Access *MemoryAccessLog
	Egress *MemoryEgressLog
	Logs   *MemoryLogSink
	Device *FakeDevice
	Ind    *RecordingIndicator
	Source *StaticSource
	Sink   *MemorySink
	Agent  *fakeAgent
	Dir    string
}

type fakeAgent struct {
	Answer   string
	Chunks   []string
	Prompts  []string
	StreamOK bool
}

func (a *fakeAgent) Ask(_ context.Context, _, prompt, _ string) (string, error) {
	a.Prompts = append(a.Prompts, prompt)
	return a.Answer, nil
}

func (a *fakeAgent) Stream(_ context.Context, _, prompt string, emit func(string) error) error {
	a.Prompts = append(a.Prompts, prompt)
	if !a.StreamOK {
		return ErrNoStreaming
	}
	for _, c := range a.Chunks {
		if err := emit(c); err != nil {
			return err
		}
	}
	return nil
}

// appsTempDir is t.TempDir plus a thaw before Go's cleanup runs.
//
// Installing an app freezes its tree to 0400/0500, and RemoveAll on a frozen
// tree fails for anyone who is not root — so t.TempDir's own cleanup reported
// "permission denied" on every macOS run while passing in a container running
// as uid 0. Cleanup registered here runs before the one t.TempDir registered,
// because testing runs cleanups last-in-first-out.
func appsTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { thawTree(dir) })
	return dir
}

func newTestRuntime(t *testing.T, mutate ...func(*Options)) *testRuntime {
	t.Helper()
	node := requireNode(t)
	dir := appsTempDir(t)

	tr := &testRuntime{
		Access: &MemoryAccessLog{},
		Egress: &MemoryEgressLog{},
		Logs:   &MemoryLogSink{},
		Device: &FakeDevice{},
		Ind:    &RecordingIndicator{},
		Sink:   &MemorySink{},
		Agent:  &fakeAgent{Answer: "a summary"},
		Dir:    dir,
	}
	start := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	tr.Source = &StaticSource{
		Episodes: []Episode{{
			ID: "ep-1", Kind: "meeting", StartedAt: start, EndedAt: start.Add(30 * time.Minute),
			Transcript: "alice: I'll send the BOM by Friday.", Participants: []string{"alice", "you"},
		}},
		Now: func() time.Time { return start.Add(time.Hour) },
		Extractor: func(e Episode) []Commitment {
			return []Commitment{{Text: "send the BOM", To: "alice", SourceEpisodeID: e.ID}}
		},
	}

	o := Options{
		Node:       node,
		RuntimeDir: filepath.Join(dir, "runtime"),
		Redact:     Detector(),
		Source:     tr.Source,
		Sink:       tr.Sink,
		Device:     tr.Device,
		Indicator:  tr.Ind,
		Agent:      tr.Agent,
		AccessLog:  tr.Access,
		EgressLog:  tr.Egress,
		LogSink:    tr.Logs,
		Limits:     DefaultLimits(),
	}
	for _, fn := range mutate {
		fn(&o)
	}
	rt, err := New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	tr.Runtime = rt
	return tr
}

// install stages an app package and installs it with the given consent.
func (tr *testRuntime) install(t *testing.T, source string, granted ...Scope) Installed {
	t.Helper()
	m, err := ReadManifest(source)
	if err != nil {
		t.Fatal(err)
	}
	if len(granted) == 0 {
		granted = m.Scopes()
	}
	inst, err := tr.Install(m, Consent{Granted: granted, By: "test"}, source, InstallOptions{
		Dir: filepath.Join(tr.Dir, "apps"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

// logged returns the app's own log lines, joined.
func (tr *testRuntime) logged() []string {
	var out []string
	for _, l := range tr.Logs.All() {
		out = append(out, l.Message)
	}
	return out
}

// probeJSON finds the first log line that parses as JSON and unmarshals it.
func probeJSON(t *testing.T, lines []string, v any) {
	t.Helper()
	for _, l := range lines {
		if err := json.Unmarshal([]byte(l), v); err == nil {
			return
		}
	}
	t.Fatalf("no JSON probe line in %q", lines)
}

const minimalManifest = `{
  "id": "dev.test.minimal",
  "name": "Minimal",
  "version": "1.0.0",
  "description": "The smallest app that does anything at all.",
  "author": { "name": "Test", "url": "https://example.com" },
  "permissions": [],
  "triggers": [{ "type": "phrase", "match": "run the minimal app" }]
}`

// rewriteTo sends every request to one local address while leaving the URL's
// host — and therefore the allowlist decision — alone. It is how the egress
// tests exercise the real [Guard] and the real [Fetcher] without a packet
// leaving the machine.
type rewritingTransport struct{ addr string }

func rewriteTo(addr string) http.RoundTripper { return &rewritingTransport{addr: addr} }

func (t *rewritingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = t.addr
	return http.DefaultTransport.RoundTrip(clone)
}
