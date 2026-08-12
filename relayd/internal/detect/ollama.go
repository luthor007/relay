package detect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Ollama detection — the local embedding runtime ORCHESTRATOR.md §2c
// provisions.
//
// It is detected with the same three signals as the five agent runtimes, plus
// one they do not need. A binary on PATH and a version are the usual two. The
// third is different: an agent runtime is a process we launch, so "is it
// running" is a question about the machine's process table, whereas Ollama is a
// *service* we talk to over HTTP, so the only honest test of it is a request.
//
// That distinction is the whole reason this file exists rather than another row
// in runtimes.go. **Installed and running are two different states here**, and
// conflating them produces the exact failure ORCHESTRATOR.md §2c calls out: a
// box where `ollama` is on PATH, the model is pulled, and every embedding call
// fails with a connection refused that surfaces as "search got worse" weeks
// later. The service check is one GET, it costs a few milliseconds, and it
// turns that into a sentence at setup.
//
// Nothing here downloads anything. Provisioning lives behind an interface in
// internal/install; this only reports.

// OllamaDefaultHost is where Ollama listens unless OLLAMA_HOST moves it.
const OllamaDefaultHost = "http://127.0.0.1:11434"

// OllamaBinary is the binary name on PATH.
const OllamaBinary = "ollama"

// ServiceTimeout caps the local service check. This is a request to loopback in
// the normal case; three seconds is generous and, more importantly, bounded —
// the installer is already the slow part of setup.
const ServiceTimeout = 3 * time.Second

// OllamaStatus is what we concluded about the local runtime.
type OllamaStatus string

const (
	// OllamaAbsent: no binary, nothing answering. Nothing to do but offer to
	// install it.
	OllamaAbsent OllamaStatus = "absent"
	// OllamaNotRunning: the binary is installed and nothing answers on the
	// host. This is a first-class state, not an error, because it is the one
	// that otherwise surfaces as a connection-refused stack trace in the middle
	// of an install.
	OllamaNotRunning OllamaStatus = "installed_not_running"
	// OllamaRunning: something answered.
	OllamaRunning OllamaStatus = "running"
	// OllamaServiceOnly: something answers on the host but there is no binary on
	// PATH — a container, a remote box, a Homebrew service whose shim is not on
	// this shell's PATH. Perfectly usable, and worth naming rather than
	// reporting as absent.
	OllamaServiceOnly OllamaStatus = "service_only"
)

// Line is the one-clause rendering the installer prints.
func (s OllamaStatus) Line() string {
	switch s {
	case OllamaAbsent:
		return "not installed"
	case OllamaNotRunning:
		return "installed, service not running"
	case OllamaRunning:
		return "installed, service answering"
	case OllamaServiceOnly:
		return "service answering, binary not on PATH"
	}
	return string(s)
}

// OllamaModel is one model the local runtime already holds.
type OllamaModel struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// Ollama is everything detection concluded about the local embedding runtime.
type Ollama struct {
	Installed  bool
	BinaryPath string

	Version     string
	VersionNote string

	// Host is the base URL the service is expected on, and HostSource says how
	// we know: "OLLAMA_HOST" or "default". A user who has moved it and a user
	// who has not are looking at different machines, and a report that hides
	// which is which sends them to the wrong place.
	Host       string
	HostSource string

	// Reachable is the result of one real request. ServiceNote carries the
	// transport error verbatim when it is false — "connection refused" is the
	// answer to the support question, so it is not paraphrased away.
	Reachable   bool
	ServiceNote string

	// Models is what the runtime already has, when it answered. nil means we
	// could not ask, which is not the same as an empty library.
	Models []OllamaModel

	Notes []string
}

// Status classifies the finding.
func (o Ollama) Status() OllamaStatus {
	switch {
	case o.Installed && o.Reachable:
		return OllamaRunning
	case o.Installed:
		return OllamaNotRunning
	case o.Reachable:
		return OllamaServiceOnly
	default:
		return OllamaAbsent
	}
}

// Usable reports whether an embedding call could be made right now.
func (o Ollama) Usable() bool { return o.Reachable }

// Has reports whether a model is already pulled.
//
// Ollama names models as name[:tag] and defaults the tag to "latest", so a
// config that says "nomic-embed-text" and a library that says
// "nomic-embed-text:latest" are the same model. Treating them as different is
// how you re-download 274 MB somebody already has.
func (o Ollama) Has(model string) bool {
	want := strings.TrimSpace(model)
	if want == "" {
		return false
	}
	if !strings.Contains(want, ":") {
		want += ":latest"
	}
	for _, m := range o.Models {
		have := m.Name
		if !strings.Contains(have, ":") {
			have += ":latest"
		}
		if strings.EqualFold(have, want) {
			return true
		}
	}
	return false
}

// ModelNames is the library as plain strings.
func (o Ollama) ModelNames() []string {
	out := make([]string, 0, len(o.Models))
	for _, m := range o.Models {
		out = append(out, m.Name)
	}
	return out
}

// Summary is the line the installer prints.
func (o Ollama) Summary() string {
	s := "Ollama: " + o.Status().Line()
	if o.Version != "" {
		s += " (" + o.Version + ")"
	}
	if o.Host != "" && o.HostSource != "default" {
		s += ", at " + o.Host + " per $" + o.HostSource
	}
	if o.Status() == OllamaNotRunning && o.ServiceNote != "" {
		s += " — " + o.ServiceNote
	}
	return s
}

// OllamaHost resolves the base URL of the local service.
//
// OLLAMA_HOST is accepted in every shape people actually set it in — "host",
// "host:port", "http://host:port" — because a user who set it to "127.0.0.1"
// and a user who set it to "http://127.0.0.1:11434" both believe they have
// configured the same thing, and half a URL is not a reason to report the
// service as absent.
func OllamaHost(env Env) (host, source string) {
	raw := env.getenv("OLLAMA_HOST")
	if raw == "" {
		return OllamaDefaultHost, "default"
	}
	return normalizeOllamaHost(raw), "OLLAMA_HOST"
}

func normalizeOllamaHost(raw string) string {
	h := strings.TrimSpace(raw)
	h = strings.TrimSuffix(h, "/")
	scheme := "http://"
	switch {
	case strings.HasPrefix(h, "http://"):
		h, scheme = strings.TrimPrefix(h, "http://"), "http://"
	case strings.HasPrefix(h, "https://"):
		h, scheme = strings.TrimPrefix(h, "https://"), "https://"
	}
	// A bare host with no port means the default port, not port 80.
	hostOnly, _, hasPort := strings.Cut(h, ":")
	if !hasPort {
		h += ":11434"
	} else if hostOnly == "" {
		// ":11434" — a port with no host is loopback.
		h = "127.0.0.1" + h
	}
	// 0.0.0.0 is a bind address, not a destination. Dialling it works on Linux
	// and does not on macOS, so rewrite it to loopback rather than shipping a
	// platform-dependent failure.
	if strings.HasPrefix(h, "0.0.0.0:") {
		h = "127.0.0.1" + strings.TrimPrefix(h, "0.0.0.0")
	}
	return scheme + h
}

// DetectOllama runs one pass over the local embedding runtime.
//
// Like [Detect] it never returns an error: a missing binary, a service that is
// not running and a machine with no HTTP client wired up are all facts about
// the machine and belong in the report.
func DetectOllama(ctx context.Context, env Env) Ollama {
	o := Ollama{}
	o.Host, o.HostSource = OllamaHost(env)

	if env.Exec != nil {
		if p, err := env.Exec.LookPath(OllamaBinary); err == nil {
			o.Installed, o.BinaryPath = true, p
		}
	}
	if o.Installed && env.Exec != nil {
		res, err := env.Exec.Run(ctx, Cmd{Name: OllamaBinary, Args: []string{"--version"}})
		switch {
		case err != nil:
			o.VersionNote = "could not run ollama --version: " + err.Error()
		case res.Code != 0:
			o.VersionNote = fmt.Sprintf("ollama --version exited %d", res.Code)
		default:
			o.Version = firstLine(res.Out())
		}
	}

	models, err := listOllamaModels(ctx, env, o.Host)
	if err != nil {
		o.ServiceNote = err.Error()
	} else {
		o.Reachable = true
		o.Models = models
	}

	switch o.Status() {
	case OllamaNotRunning:
		o.Notes = append(o.Notes,
			"Ollama is installed but nothing is answering on "+o.Host+". The binary being "+
				"present is not the same as the service being up, and an embedder that cannot "+
				"be reached is a search that quietly gets worse.")
	case OllamaServiceOnly:
		o.Notes = append(o.Notes,
			"Something is answering on "+o.Host+" without an ollama binary on this PATH — a "+
				"container or another machine. That works: Relay talks to it over HTTP.")
	}
	return o
}

// ollamaTags is the shape of GET /api/tags.
type ollamaTags struct {
	Models []OllamaModel `json:"models"`
}

// listOllamaModels is the one real request this package makes. It doubles as
// the reachability check, because a service that lists its models is a service
// that will answer an embedding call — and asking twice for two facts one
// request already carries is waste in the middle of an install.
func listOllamaModels(ctx context.Context, env Env, host string) ([]OllamaModel, error) {
	client := env.HTTP
	if client == nil {
		client = &http.Client{}
	}
	ctx, cancel := context.WithTimeout(ctx, ServiceTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, host+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("no answer on %s: %w", host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s answered %s", host, resp.Status)
	}
	var tags ollamaTags
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return nil, fmt.Errorf("%s answered, but not with a model list: %w", host, err)
	}
	if tags.Models == nil {
		tags.Models = []OllamaModel{}
	}
	return tags.Models, nil
}
