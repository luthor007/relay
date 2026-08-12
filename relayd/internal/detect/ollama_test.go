package detect_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/detect"
)

// The local embedding runtime, detected from fixtures. No test here opens a
// socket: the service check goes through detect.Env.HTTP, which is a
// RoundTripper a test supplies.

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func tagsClient(t *testing.T, body string, seen *[]string) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if seen != nil {
			*seen = append(*seen, r.URL.String())
		}
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Request:    r,
		}, nil
	})}
}

func refusedClient() *http.Client {
	return &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp 127.0.0.1:11434: connect: connection refused")
	})}
}

func ollamaEnv(exec *detect.FakeExec, client *http.Client, getenv func(string) string) detect.Env {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	return detect.Env{
		FS: &detect.MemFS{}, Exec: exec, HTTP: client,
		Getenv: getenv, Home: "/home/u", GOOS: "linux",
	}
}

func TestOllamaRunning(t *testing.T) {
	ex := &detect.FakeExec{
		Paths: map[string]string{"ollama": "/usr/local/bin/ollama"},
		Responses: map[string]detect.Result{
			detect.Key("ollama", "--version"): {Stdout: []byte("ollama version is 0.5.7\n")},
		},
	}
	var seen []string
	env := ollamaEnv(ex, tagsClient(t,
		`{"models":[{"name":"nomic-embed-text:latest","size":274302450}]}`, &seen), nil)

	o := detect.DetectOllama(context.Background(), env)
	if o.Status() != detect.OllamaRunning {
		t.Fatalf("status = %q, want running (%+v)", o.Status(), o)
	}
	if o.Version != "ollama version is 0.5.7" {
		t.Errorf("version = %q", o.Version)
	}
	if o.Host != detect.OllamaDefaultHost || o.HostSource != "default" {
		t.Errorf("host = %q (%s)", o.Host, o.HostSource)
	}
	if len(seen) != 1 || !strings.HasSuffix(seen[0], "/api/tags") {
		t.Errorf("one GET /api/tags, got %v", seen)
	}

	// A config that says "nomic-embed-text" and a library that says
	// "nomic-embed-text:latest" are the same model.
	if !o.Has("nomic-embed-text") {
		t.Error("Has must default the tag to :latest, or Relay re-downloads what is already here")
	}
	if o.Has("mxbai-embed-large") {
		t.Error("Has matched a model that is not installed")
	}
}

// The failure mode ORCHESTRATOR.md §2c names explicitly: the binary is there,
// the service is not, and the error must be a sentence rather than a connection
// refused in the middle of an install.
func TestOllamaInstalledButNotRunning(t *testing.T) {
	ex := &detect.FakeExec{
		Paths: map[string]string{"ollama": "/usr/local/bin/ollama"},
		Responses: map[string]detect.Result{
			detect.Key("ollama", "--version"): {Stdout: []byte("ollama version is 0.5.7\n")},
		},
	}
	o := detect.DetectOllama(context.Background(), ollamaEnv(ex, refusedClient(), nil))

	if o.Status() != detect.OllamaNotRunning {
		t.Fatalf("status = %q, want installed_not_running", o.Status())
	}
	if o.Usable() {
		t.Error("an unreachable service is not usable")
	}
	if !strings.Contains(o.ServiceNote, "connection refused") {
		t.Errorf("the transport error must survive verbatim; got %q", o.ServiceNote)
	}
	if len(o.Notes) == 0 || !strings.Contains(strings.Join(o.Notes, " "), "not the same as the service being up") {
		t.Errorf("notes = %v", o.Notes)
	}
	// nil, not empty: nobody could ask what models are there.
	if o.Models != nil {
		t.Errorf("models = %v, want nil when the service did not answer", o.Models)
	}
}

// A container or another machine: something answers, no binary on this PATH.
// Usable, and worth naming rather than reporting as absent.
func TestOllamaServiceWithoutBinary(t *testing.T) {
	o := detect.DetectOllama(context.Background(),
		ollamaEnv(&detect.FakeExec{}, tagsClient(t, `{"models":[]}`, nil), nil))

	if o.Status() != detect.OllamaServiceOnly {
		t.Fatalf("status = %q, want service_only", o.Status())
	}
	if !o.Usable() {
		t.Error("a reachable service is usable whether or not the CLI is on PATH")
	}
	// Empty, not nil: we asked, and the library is empty.
	if o.Models == nil || len(o.Models) != 0 {
		t.Errorf("models = %#v, want an empty slice", o.Models)
	}
}

func TestOllamaAbsent(t *testing.T) {
	o := detect.DetectOllama(context.Background(),
		ollamaEnv(&detect.FakeExec{}, refusedClient(), nil))
	if o.Status() != detect.OllamaAbsent {
		t.Fatalf("status = %q, want absent", o.Status())
	}
	if o.Status().Line() != "not installed" {
		t.Errorf("line = %q", o.Status().Line())
	}
}

// OLLAMA_HOST arrives in every shape people set it in, and half a URL is not a
// reason to report the service as absent.
func TestOllamaHostNormalisation(t *testing.T) {
	for _, tc := range []struct{ set, want, source string }{
		{"", "http://127.0.0.1:11434", "default"},
		{"127.0.0.1:11434", "http://127.0.0.1:11434", "OLLAMA_HOST"},
		{"http://127.0.0.1:11434", "http://127.0.0.1:11434", "OLLAMA_HOST"},
		{"http://127.0.0.1:11434/", "http://127.0.0.1:11434", "OLLAMA_HOST"},
		{"box.lan", "http://box.lan:11434", "OLLAMA_HOST"},
		{"box.lan:1234", "http://box.lan:1234", "OLLAMA_HOST"},
		{"https://box.lan:443", "https://box.lan:443", "OLLAMA_HOST"},
		// A bind address is not a destination: dialling 0.0.0.0 works on Linux
		// and does not on macOS, which is a platform-dependent failure nobody
		// should have to debug.
		{"0.0.0.0:11434", "http://127.0.0.1:11434", "OLLAMA_HOST"},
		{":11434", "http://127.0.0.1:11434", "OLLAMA_HOST"},
	} {
		env := ollamaEnv(&detect.FakeExec{}, refusedClient(), func(k string) string {
			if k == "OLLAMA_HOST" {
				return tc.set
			}
			return ""
		})
		host, source := detect.OllamaHost(env)
		if host != tc.want || source != tc.source {
			t.Errorf("OLLAMA_HOST=%q → %q (%s), want %q (%s)", tc.set, host, source, tc.want, tc.source)
		}
	}
}

// A service that answers with something that is not a model list is not a
// reachable Ollama. Reporting it as one produces a green install and a broken
// embedder.
func TestOllamaWrongServiceOnThePort(t *testing.T) {
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200, Body: io.NopCloser(strings.NewReader("<html>hello</html>")),
			Header: http.Header{}, Request: r,
		}, nil
	})}
	o := detect.DetectOllama(context.Background(), ollamaEnv(&detect.FakeExec{}, client, nil))
	if o.Reachable {
		t.Fatal("an HTML page is not an Ollama service")
	}
	if !strings.Contains(o.ServiceNote, "not with a model list") {
		t.Errorf("note = %q", o.ServiceNote)
	}
}
