package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/luthor007/relay/relayd/internal/mcp"
)

// Prusa is the first connector, and it is the one ORCHESTRATOR.md §4b uses as
// its worked example:
//
//	> You have mentioned your Prusa four times this week. Want me to connect it?
//	> I could queue prints and tell you when they finish.
//
// It was chosen over Gmail for three reasons, all of which are about what it
// proves rather than what it does:
//
//   - **The read half is useful alone.** "Is it done yet" without walking to
//     the garage is the whole feature, and §4b's claim that most connectors
//     need only the read half is only credible if the first one demonstrates it.
//   - **The write half is unambiguously consequential.** Printing is one of the
//     four examples §4b names for "confirms at the glasses, every time" —
//     alongside sending mail, spending money and opening a door. It heats a bed
//     to 60 °C in a room the user is not in.
//   - **It is on the LAN.** No OAuth, no cloud, no third-party token, so the
//     first connector exercises grants, confirmation, envelopes and the refresh
//     path without also being an authentication project.
//
// **The HTTP shapes below are PrusaLink's documented local API and have not
// been verified against hardware** — there is no printer on the build machine
// and no fixture in the repository. That is why [Routes] exists: every path is
// a field, so a wrong guess is a config change rather than a code change, and
// the parsing is tolerant of missing fields rather than assuming a schema it
// has not seen. Nothing here scrapes anything; it is a documented JSON API.

// Doer is the HTTP client. It is an interface so the connector can be exercised
// against recorded responses with no network at all.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Routes are PrusaLink's paths. They are fields rather than constants because
// they are documented-but-unverified; see the type comment on [Prusa].
type Routes struct {
	Status string
	Job    string
	Files  string
	// Print starts a stored file. %s is the storage-qualified path.
	Print string
	// Stop cancels the running job. %s is the job id.
	Stop string
}

// DefaultRoutes are PrusaLink's v1 API paths.
func DefaultRoutes() Routes {
	return Routes{
		Status: "/api/v1/status",
		Job:    "/api/v1/job",
		Files:  "/api/v1/files/%s",
		Print:  "/api/v1/files/%s",
		Stop:   "/api/v1/job/%s",
	}
}

// PrusaName is the connector's grant key.
const PrusaName = "prusa"

// Prusa is one printer.
type Prusa struct {
	// Base is the printer's address on the LAN: "http://prusa.local".
	Base string

	// APIKey returns PrusaLink's key. It is a function so the secret lives in
	// the vault and is fetched per call rather than held in this struct for the
	// life of the process. It is never logged and never put in an envelope.
	APIKey func(ctx context.Context) (string, error)

	// HTTP is the client. Nil builds one with Timeout.
	HTTP Doer
	// Timeout bounds one request. Zero means DefaultPrusaTimeout.
	Timeout time.Duration

	// Routes defaults to DefaultRoutes.
	Routes Routes
	// Storage is which of the printer's storages to read: "usb" or "local".
	Storage string

	Now func() time.Time

	mu   sync.Mutex
	last prusaStatus
	seen bool

	client *http.Client
}

// DefaultPrusaTimeout bounds one call to the printer. A printer on the LAN that
// has not answered in five seconds is off, and waiting longer only makes the
// agent look stuck.
const DefaultPrusaTimeout = 5 * time.Second

var _ Poller = (*Prusa)(nil)

// Descriptor implements [Connector].
//
// Opens is the part ORCHESTRATOR.md §4b is strict about: "a reason that
// restates the permission is not a reason". So the read half does not say
// "reads your printer", it says what becomes possible.
func (p *Prusa) Descriptor() Descriptor {
	return Descriptor{
		Name:   PrusaName,
		Title:  "Prusa 3D printer",
		Vendor: "PrusaLink, on your network",
		Opens: map[mcp.Access]string{
			mcp.AccessRead: "I could tell you how far through a print is and when it will " +
				"finish, and say so when it does, without you going to look",
			mcp.AccessWrite: "I could start a print you have already sliced and stop one " +
				"that is going wrong. Starting or stopping a print heats or abandons a " +
				"physical machine in another room, so Relay asks out loud first, every time",
		},
		Mentions: []string{"prusa", "prusalink", "3d printer", "3d print", "mk4", "mk3s", "mk3"},
	}
}

// Tools implements [Connector].
func (p *Prusa) Tools() []mcp.Tool {
	noArgs := map[string]any{"type": "object", "properties": map[string]any{}}
	return []mcp.Tool{
		{
			Name:        mcp.ToolName(PrusaName, "status"),
			Title:       "Printer status",
			Description: "What the Prusa is doing right now: state, temperatures, and the running job's progress and time remaining.",
			InputSchema: noArgs,
			Connector:   PrusaName,
			Access:      mcp.AccessRead,
			Handler:     p.status,
		},
		{
			Name:        mcp.ToolName(PrusaName, "files"),
			Title:       "Printable files",
			Description: "The G-code files stored on the printer, which are the only things it can be asked to print.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"storage": map[string]any{
						"type":        "string",
						"description": "Which storage to list: usb or local. Defaults to the configured one.",
					},
				},
			},
			Connector: PrusaName,
			Access:    mcp.AccessRead,
			Handler:   p.files,
		},
		{
			Name:        mcp.ToolName(PrusaName, "print"),
			Title:       "Start a print",
			Description: "Start printing a file already stored on the printer.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "The file's path on the printer, as prusa_files reported it.",
					},
					"storage": map[string]any{"type": "string"},
				},
				"required": []string{"path"},
			},
			Connector: PrusaName,
			Access:    mcp.AccessWrite,
			// ORCHESTRATOR.md §4b names printing explicitly. This string is what
			// the user hears, so it says what physically happens.
			Consequence: "starts a print on your Prusa, which heats the bed and moves the head",
			Handler:     p.print,
		},
		{
			Name:        mcp.ToolName(PrusaName, "stop"),
			Title:       "Stop the print",
			Description: "Cancel the job the printer is running.",
			InputSchema: noArgs,
			Connector:   PrusaName,
			Access:      mcp.AccessWrite,
			Consequence: "stops the print that is running, and the part already printed is scrap",
			Handler:     p.stop,
		},
	}
}

// ------------------------------------------------------------------ tools --

func (p *Prusa) status(ctx context.Context, _ mcp.Call) (mcp.Result, error) {
	st, err := p.fetchStatus(ctx)
	if err != nil {
		return mcp.Result{}, err
	}
	env := st.envelope(PrusaName, "printer.status", p.now())
	return mcp.Result{Text: st.line(), Structured: env, Target: st.jobFile()}, nil
}

func (p *Prusa) files(ctx context.Context, c mcp.Call) (mcp.Result, error) {
	storage := p.storageFrom(c.Arguments)
	var doc prusaFiles
	if err := p.get(ctx, fmt.Sprintf(p.routes().Files, url.PathEscape(storage)), &doc); err != nil {
		return mcp.Result{}, err
	}
	names := doc.names()
	if len(names) == 0 {
		return mcp.Result{Text: "The printer has no printable files on " + storage + "."}, nil
	}
	env := Envelope{
		Connector: PrusaName,
		Kind:      "files.listed",
		At:        p.now(),
		Summary:   fmt.Sprintf("%d printable file(s) on %s.", len(names), storage),
		Entities:  names,
		Payload:   map[string]any{"storage": storage, "files": toAny(names)},
	}
	return mcp.Result{
		Text:       fmt.Sprintf("%d printable file(s) on %s: %s.", len(names), storage, strings.Join(names, ", ")),
		Structured: env,
		Target:     storage,
	}, nil
}

func (p *Prusa) print(ctx context.Context, c mcp.Call) (mcp.Result, error) {
	path, _ := c.Arguments["path"].(string)
	path = strings.TrimSpace(path)
	if path == "" {
		return mcp.Result{Text: "Which file? prusa_files lists what the printer has.", IsError: true}, nil
	}
	storage := p.storageFrom(c.Arguments)
	route := fmt.Sprintf(p.routes().Print, url.PathEscape(storage)+"/"+escapePath(path))
	if err := p.do(ctx, http.MethodPost, route, map[string]string{"Print-After-Upload": "?1"}, nil); err != nil {
		return mcp.Result{}, err
	}
	env := Envelope{
		Connector: PrusaName,
		Kind:      "job.started",
		At:        p.now(),
		Summary:   "Started printing " + path + ".",
		Entities:  []string{path},
		Payload:   map[string]any{"storage": storage, "path": path},
	}
	return mcp.Result{Text: "Started printing " + path + ".", Structured: env, Target: path}, nil
}

func (p *Prusa) stop(ctx context.Context, _ mcp.Call) (mcp.Result, error) {
	st, err := p.fetchStatus(ctx)
	if err != nil {
		return mcp.Result{}, err
	}
	if st.Job == nil || st.Job.ID == 0 {
		return mcp.Result{Text: "The printer is not running a job, so there is nothing to stop."}, nil
	}
	route := fmt.Sprintf(p.routes().Stop, fmt.Sprint(st.Job.ID))
	if err := p.do(ctx, http.MethodDelete, route, nil, nil); err != nil {
		return mcp.Result{}, err
	}
	file := st.jobFile()
	env := Envelope{
		Connector: PrusaName,
		Kind:      "job.stopped",
		At:        p.now(),
		Summary:   "Stopped the print" + suffixFile(file) + ".",
		Entities:  entitiesFor(file),
		Payload:   map[string]any{"job": st.Job.ID, "file": file},
	}
	return mcp.Result{Text: env.Summary, Structured: env, Target: file}, nil
}

// ------------------------------------------------------------------- poll --

// Poll implements [Poller]. It reports transitions, not the current state: a
// connector that emitted "still printing" every thirty seconds would be noise,
// and ADAPTERS.md §7's batching rules exist because noise is what makes people
// turn notifications off.
//
// An unreachable printer is an error, not an empty result. "Nothing happened"
// and "we could not tell" are different answers and only one of them is safe to
// act on.
func (p *Prusa) Poll(ctx context.Context) ([]Envelope, error) {
	st, err := p.fetchStatus(ctx)
	if err != nil {
		return nil, err
	}
	now := p.now()

	p.mu.Lock()
	prev, had := p.last, p.seen
	p.last, p.seen = st, true
	p.mu.Unlock()

	if !had {
		// The first poll establishes the baseline. Announcing whatever was
		// already happening would be reporting history as news, which is the
		// same mistake event.Meta.Replay exists to prevent.
		return nil, nil
	}

	var out []Envelope
	prevJob, nowJob := prev.jobID(), st.jobID()

	// The two conditions are separate rather than a switch: a printer polled
	// across a job boundary can have finished one and started the next between
	// two reads, and "tell you when they finish" has to survive that. Finished
	// comes first because that is the order it happened in.
	if prevJob != 0 && nowJob != prevJob {
		out = append(out, Envelope{
			Connector: PrusaName, Kind: "job.finished", At: now,
			Summary:  "The printer finished " + orUnnamed(prev.jobFile()) + ".",
			Entities: entitiesFor(prev.jobFile()),
			Payload:  map[string]any{"job": prevJob, "file": prev.jobFile()},
		})
	}
	if nowJob != 0 && nowJob != prevJob {
		out = append(out, Envelope{
			Connector: PrusaName, Kind: "job.started", At: now,
			Summary:  "The printer started " + orUnnamed(st.jobFile()) + ".",
			Entities: entitiesFor(st.jobFile()),
			Payload:  map[string]any{"job": nowJob, "file": st.jobFile()},
		})
	}

	if st.attention() && !prev.attention() {
		out = append(out, Envelope{
			Connector: PrusaName, Kind: "printer.attention", At: now,
			Summary:  "The printer wants attention: " + strings.ToLower(st.state()) + ".",
			Entities: entitiesFor(st.jobFile()),
			Payload:  map[string]any{"state": st.state()},
		})
	}
	return out, nil
}

// ------------------------------------------------------------------ wire --

type prusaStatus struct {
	Printer *struct {
		State        string   `json:"state"`
		TempNozzle   *float64 `json:"temp_nozzle"`
		TargetNozzle *float64 `json:"target_nozzle"`
		TempBed      *float64 `json:"temp_bed"`
		TargetBed    *float64 `json:"target_bed"`
	} `json:"printer"`
	Job *prusaJob `json:"job"`
}

type prusaJob struct {
	ID            int64    `json:"id"`
	State         string   `json:"state"`
	Progress      *float64 `json:"progress"`
	TimeRemaining *int64   `json:"time_remaining"`
	File          *struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		Path        string `json:"path"`
	} `json:"file"`
}

func (s prusaStatus) state() string {
	if s.Printer == nil || s.Printer.State == "" {
		return "unknown"
	}
	return s.Printer.State
}

func (s prusaStatus) attention() bool {
	switch strings.ToUpper(s.state()) {
	case "ATTENTION", "ERROR", "PAUSED":
		return true
	}
	return false
}

func (s prusaStatus) jobID() int64 {
	if s.Job == nil {
		return 0
	}
	return s.Job.ID
}

func (s prusaStatus) jobFile() string {
	if s.Job == nil || s.Job.File == nil {
		return ""
	}
	for _, v := range []string{s.Job.File.DisplayName, s.Job.File.Name, s.Job.File.Path} {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// line is what the agent reads. Every clause comes from a field that was
// present; a printer that did not report a temperature produces a shorter
// sentence rather than a guessed one.
func (s prusaStatus) line() string {
	parts := []string{"The printer is " + strings.ToLower(s.state())}
	if f := s.jobFile(); f != "" {
		parts[0] += ", printing " + f
	}
	if s.Job != nil && s.Job.Progress != nil {
		parts = append(parts, fmt.Sprintf("%.0f%% done", *s.Job.Progress))
	}
	if s.Job != nil && s.Job.TimeRemaining != nil && *s.Job.TimeRemaining > 0 {
		parts = append(parts, "about "+humanSeconds(*s.Job.TimeRemaining)+" left")
	}
	if s.Printer != nil && s.Printer.TempNozzle != nil && s.Printer.TempBed != nil {
		parts = append(parts, fmt.Sprintf("nozzle %.0f°C, bed %.0f°C", *s.Printer.TempNozzle, *s.Printer.TempBed))
	}
	return strings.Join(parts, ", ") + "."
}

func (s prusaStatus) envelope(connector, kind string, at time.Time) Envelope {
	payload := map[string]any{"state": s.state()}
	if s.Job != nil {
		payload["job"] = s.Job.ID
		if s.Job.Progress != nil {
			payload["progress"] = *s.Job.Progress
		}
		if s.Job.TimeRemaining != nil {
			payload["time_remaining_s"] = *s.Job.TimeRemaining
		}
		if f := s.jobFile(); f != "" {
			payload["file"] = f
		}
	}
	return Envelope{
		Connector: connector,
		Kind:      kind,
		At:        at,
		Summary:   s.line(),
		Entities:  entitiesFor(s.jobFile()),
		Payload:   payload,
	}
}

type prusaFiles struct {
	Children []prusaFileNode `json:"children"`
	Files    []prusaFileNode `json:"files"`
}

type prusaFileNode struct {
	Name        string          `json:"name"`
	DisplayName string          `json:"display_name"`
	Path        string          `json:"path"`
	Type        string          `json:"type"`
	Children    []prusaFileNode `json:"children"`
}

func (f prusaFiles) names() []string {
	var out []string
	var walk func(list []prusaFileNode)
	walk = func(list []prusaFileNode) {
		for _, n := range list {
			if len(n.Children) > 0 {
				walk(n.Children)
				continue
			}
			name := n.Path
			if name == "" {
				name = n.Name
			}
			if name == "" {
				name = n.DisplayName
			}
			if name == "" {
				continue
			}
			if n.Type != "" && !strings.EqualFold(n.Type, "PRINT_FILE") && !strings.EqualFold(n.Type, "FILE") {
				continue
			}
			out = append(out, name)
		}
	}
	walk(f.Children)
	walk(f.Files)
	return out
}

// ------------------------------------------------------------------ HTTP --

func (p *Prusa) routes() Routes {
	if p.Routes.Status == "" {
		return DefaultRoutes()
	}
	return p.Routes
}

func (p *Prusa) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Prusa) storageFrom(args map[string]any) string {
	if v, ok := args["storage"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if p.Storage != "" {
		return p.Storage
	}
	return "usb"
}

func (p *Prusa) doer() Doer {
	if p.HTTP != nil {
		return p.HTTP
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client == nil {
		timeout := p.Timeout
		if timeout <= 0 {
			timeout = DefaultPrusaTimeout
		}
		p.client = &http.Client{Timeout: timeout}
	}
	return p.client
}

// ErrNoPrinter is a connector with no address configured.
var ErrNoPrinter = errors.New("connector: no printer address configured")

func (p *Prusa) fetchStatus(ctx context.Context) (prusaStatus, error) {
	var st prusaStatus
	err := p.get(ctx, p.routes().Status, &st)
	return st, err
}

func (p *Prusa) get(ctx context.Context, path string, out any) error {
	return p.do(ctx, http.MethodGet, path, nil, out)
}

func (p *Prusa) do(ctx context.Context, method, path string, headers map[string]string, out any) error {
	base := strings.TrimRight(strings.TrimSpace(p.Base), "/")
	if base == "" {
		return ErrNoPrinter
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if p.APIKey != nil {
		key, err := p.APIKey(ctx)
		if err != nil {
			return fmt.Errorf("connector: prusa key: %w", err)
		}
		if key != "" {
			req.Header.Set("X-Api-Key", key)
		}
	}

	resp, err := p.doer().Do(req)
	if err != nil {
		return fmt.Errorf("connector: prusa %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("connector: the printer refused Relay's API key (%d)", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("connector: the printer answered %d to %s %s", resp.StatusCode, method, path)
	}
	if out == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("connector: the printer's answer to %s was not the JSON this connector understands: %w", path, err)
	}
	return nil
}

// ---------------------------------------------------------------- helpers --

func escapePath(p string) string {
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

func entitiesFor(file string) []string {
	if strings.TrimSpace(file) == "" {
		return nil
	}
	return []string{file}
}

func orUnnamed(file string) string {
	if strings.TrimSpace(file) == "" {
		return "a print"
	}
	return file
}

func suffixFile(file string) string {
	if strings.TrimSpace(file) == "" {
		return ""
	}
	return " of " + file
}

func toAny(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}

func humanSeconds(s int64) string {
	switch {
	case s < 90:
		return fmt.Sprintf("%d seconds", s)
	case s < 90*60:
		return fmt.Sprintf("%d minutes", (s+30)/60)
	default:
		h := s / 3600
		m := (s % 3600) / 60
		if m == 0 {
			return fmt.Sprintf("%d hours", h)
		}
		return fmt.Sprintf("%dh%02d", h, m)
	}
}
