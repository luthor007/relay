package claudecode

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/luthor007/relay/relayd/internal/adapter"
)

// Options configures the adapter. Everything with an external dependency is
// injectable, because none of those dependencies exist in a build container.
type Options struct {
	// Binary is the executable to run. Default "claude".
	Binary string

	// Launcher starts the process. Default ExecLauncher.
	Launcher Launcher

	Log *slog.Logger

	// Now is the clock the event envelopes are stamped from. Default time.Now.
	Now func() time.Time

	// Buffer is the per-session event channel depth. Default 256 — a turn's
	// text deltas arrive faster than a TTS consumer drains them.
	Buffer int

	// PermissionTimeout is how long a blocked approval waits. Default
	// DefaultPermissionTimeout, which is Claude Code's own 1e8 ms (~27.8 h).
	PermissionTimeout time.Duration

	// PermissionBaseURL overrides where the generated mcp.json points, e.g.
	// "http://127.0.0.1:9000". Empty starts a loopback listener of our own.
	PermissionBaseURL string

	// ConfigDir is where generated mcp.json files are written. Empty uses a
	// temporary directory that Close removes.
	ConfigDir string

	// Home is the home directory the settings scan looks in. Empty uses the
	// real one.
	Home string

	// AllowSilentMode lets a caller start a session in a permission mode that
	// silences approvals. It is off by default and the session still reports
	// CapNeedsInput as SupportNo — this exists so a user who genuinely wants an
	// unattended run can have one, not as a convenience, and nothing in Relay
	// should ever suggest turning it on.
	AllowSilentMode bool

	// UnverifiedControlInterrupt sends
	// {"type":"control_request","request":{"subtype":"interrupt"}} on Cancel.
	//
	// system/init advertises "interrupt_receipt_v1" and
	// "interrupt_cancel_queued_v1", so an interrupt is a protocol feature
	// rather than TUI behaviour — but what the *client* has to send to use it
	// is not in the vendored trace, so this line is a guess and is off by
	// default. Cancel's supported path is answering an open permission question
	// with interrupt: true, which is verified. ADAPTERS.md §8 item 9.
	UnverifiedControlInterrupt bool
}

// Adapter drives Claude Code. One adapter owns many sessions, one process each,
// and one loopback listener that all of them route permission calls through.
type Adapter struct {
	opts Options
	log  *slog.Logger
	caps adapter.Capabilities

	hub *permissionHub

	mu       sync.Mutex
	sessions map[string]*Session
	closed   bool

	listener net.Listener
	server   *http.Server
	baseURL  string
	tmpDir   string
}

var _ adapter.Adapter = (*Adapter)(nil)

// New builds the adapter. It starts nothing: the listener comes up lazily on
// the first session that needs it.
func New(o Options) *Adapter {
	if o.Binary == "" {
		o.Binary = DefaultBinary
	}
	if o.Launcher == nil {
		o.Launcher = ExecLauncher{}
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Buffer <= 0 {
		o.Buffer = 256
	}
	if o.PermissionTimeout <= 0 {
		o.PermissionTimeout = DefaultPermissionTimeout
	}
	return &Adapter{
		opts:     o,
		log:      o.Log,
		caps:     adapterCapabilities(),
		hub:      newPermissionHub(o.Log),
		sessions: map[string]*Session{},
	}
}

// adapterCapabilities is Baseline narrowed by what this adapter actually
// implements. Two rows differ from adapter.Baseline(ClaudeCode) and both are
// deliberate; ADAPTERS.md §5 carries the reasoning.
func adapterCapabilities() adapter.Capabilities {
	return adapter.Baseline(adapter.ClaudeCode).
		// PlanUpdated: not emitted at all. §5 marks the cell ✗ and offers
		// synthesis from tool calls as the fallback; the same section forbids
		// emitting an event the adapter cannot observe. A plan built from tool
		// activity is retrospective, is strictly redundant with the
		// ToolStarted/ToolOutput stream the orchestrator already receives, and
		// would launder inference into a structure the small model is told to
		// trust above the events it was made of. So: no plan, and narration
		// falls back to tool activity, which is what §5 prescribes for a ✗ cell.
		With(adapter.CapPlan, adapter.SupportNo,
			"stream-json has no plan event, and a plan inferred from tool calls would be a description of what already ran rather than a plan; narration falls back to ToolStarted/ToolOutput").
		// Cancel: only observed while a permission question is open, where
		// answering with interrupt:true aborts the turn. system/init advertises
		// interrupt_receipt_v1 but the client-side message is not in the
		// vendored trace, so the general case is unprobed rather than absent.
		With(adapter.CapCancel, adapter.SupportUnknown,
			"verified only while a turn is blocked on the permission prompt, where a deny with interrupt:true aborts it; the stdin message for a general interrupt is not in the vendored trace (ADAPTERS.md §8 item 9)").
		// contextWindow is reported, but on the session rather than on
		// TurnCompleted.Usage: result.usage sums a turn's requests, so dividing
		// it by contextWindow overstates pressure by the number of tool round
		// trips (ADAPTERS.md §2).
		With(adapter.CapContextWindow, adapter.SupportYes,
			"modelUsage.contextWindow, paired with the most recent request's usage — read it from Session.Context(), not from TurnCompleted.Usage")
}

func (a *Adapter) Runtime() adapter.Runtime           { return adapter.ClaudeCode }
func (a *Adapter) Capabilities() adapter.Capabilities { return a.caps }

// Start opens a new session. SessionOptions.ID is used as --session-id when it
// is a UUID, because ADAPTERS.md §2 is explicit that we name the session rather
// than discovering its name afterwards.
func (a *Adapter) Start(ctx context.Context, opts adapter.SessionOptions) (adapter.Session, error) {
	return a.open(ctx, opts, adapter.SessionRef{}, false)
}

// Resume reattaches. ref.Native is the runtime's own session id; when it is
// empty, ref.ID is used, because for this runtime they are the same UUID unless
// the caller renamed the session.
//
// Set opts.Extra["fork"] = "true" to branch instead of continuing.
func (a *Adapter) Resume(ctx context.Context, ref adapter.SessionRef, opts adapter.SessionOptions) (adapter.Session, error) {
	if ref.Native == "" && ref.ID == "" {
		return nil, fmt.Errorf("%w: resume needs a session id", adapter.ErrSessionNotFound)
	}
	fork := opts.Extra["fork"] == "true"
	return a.open(ctx, opts, ref, fork)
}

func (a *Adapter) open(ctx context.Context, opts adapter.SessionOptions, ref adapter.SessionRef, fork bool) (adapter.Session, error) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil, adapter.ErrSessionClosed
	}
	a.mu.Unlock()

	if opts.Workspace == "" {
		return nil, errors.New("claudecode: SessionOptions.Workspace is required and must be absolute")
	}
	if !filepath.IsAbs(opts.Workspace) {
		return nil, fmt.Errorf("claudecode: workspace %q is not absolute", opts.Workspace)
	}

	resuming := ref.Native != "" || ref.ID != ""
	native := ref.Native
	if resuming && native == "" {
		native = ref.ID
	}

	// Relay's own id, and the UUID the runtime is told to use. They are the
	// same when the caller handed us a UUID, which is the normal path.
	id := opts.ID
	if id == "" {
		id = ref.ID
	}
	if id == "" {
		id = uuid.NewString()
	}
	newUUID := ""
	if !resuming || fork {
		if _, err := uuid.Parse(id); err == nil {
			newUUID = id
		} else {
			newUUID = uuid.NewString()
		}
	}
	if native == "" {
		native = newUUID
	}

	// The permission mode. Ours, explicit, and never an auto or bypass one
	// unless the caller has said in as many words that it wants an unattended
	// run.
	mode := opts.PermissionMode
	if mode == "" {
		mode = DefaultPermissionMode
	}
	class := ClassifyMode(mode)
	if !class.SafeMode() && !a.opts.AllowSilentMode {
		h, _ := modeHazard(HazardRunPermissionMode, "--permission-mode", mode)
		return nil, fmt.Errorf("%w: %s. %s", ErrUnsafePermissionMode, h.Detail, h.Remedy)
	}

	base, err := a.ensureListener()
	if err != nil {
		return nil, err
	}
	endpoint := endpointFor(base, id)

	cfg, err := buildMCPConfig(endpoint, opts.MCPServers)
	if err != nil {
		return nil, err
	}
	cfgPath, err := a.writeMCPConfig(id, cfg)
	if err != nil {
		return nil, err
	}

	resume := ""
	if resuming {
		resume = native
	}
	args, err := buildArgs(argSpec{
		SessionID:      newUUID,
		Resume:         resume,
		Fork:           fork,
		Model:          opts.Model,
		PermissionMode: mode,
		MCPConfigPath:  cfgPath,
		PermissionTool: PermissionToolName(),
		SettingSources: "",
	})
	if err != nil {
		return nil, err
	}

	// Pre-flight: what the user's own settings say. With --setting-sources ""
	// these should not reach our run, but the fixture was recorded in the
	// broken state despite being headless, so this is reported rather than
	// assumed away. The authority is the per-turn system/init check below.
	scan := ScanSettings(ScanOptions{Home: a.opts.Home, Workspace: opts.Workspace})

	s := newSession(sessionConfig{
		adapter:   a,
		id:        id,
		native:    native,
		workspace: opts.Workspace,
		mode:      mode,
		modeClass: class,
		scan:      scan,
		resuming:  resuming,
	})

	a.hub.register(id, s)

	proc, err := a.opts.Launcher.Launch(ctx, LaunchSpec{
		Binary: a.opts.Binary,
		Args:   args,
		Dir:    opts.Workspace,
		Env:    opts.Env,
	})
	if err != nil {
		a.hub.unregister(id)
		return nil, err
	}
	s.attach(proc)

	a.mu.Lock()
	a.sessions[id] = s
	a.mu.Unlock()

	a.log.Info("claudecode: session started",
		"session", id, "native", native, "resume", resuming, "fork", fork,
		"permission_mode", mode, "mcp_servers", mcpServerNames(cfg))
	for _, h := range scan.Hazards {
		a.log.Warn("claudecode: approval hazard in the user's settings",
			"session", id, "kind", string(h.Kind), "source", h.Source, "value", h.Value, "detail", h.Detail)
	}
	return s, nil
}

// ensureListener brings up the loopback HTTP listener the permission tool is
// pointed at. DASHBOARD.md's rule applies here too: loopback, never 0.0.0.0.
func (a *Adapter) ensureListener() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.baseURL != "" {
		return a.baseURL, nil
	}
	if a.opts.PermissionBaseURL != "" {
		a.baseURL = a.opts.PermissionBaseURL
		return a.baseURL, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("claudecode: could not open the permission listener: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle(pathPrefix, a.hub)
	srv := &http.Server{
		Handler: mux,
		// No read or write timeout: a permission call is held open until
		// someone answers, and ADAPTERS.md §2 budgets ~27.8 hours for that.
		ReadHeaderTimeout: 10 * time.Second,
	}
	a.listener = ln
	a.server = srv
	a.baseURL = "http://" + ln.Addr().String()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.log.Error("claudecode: permission listener stopped", "err", err)
		}
	}()
	return a.baseURL, nil
}

// Handler exposes the permission hub so a host that already runs an HTTP server
// can mount it instead of letting the adapter open its own listener. Mount it
// at "/mcp/" and set Options.PermissionBaseURL to the origin.
func (a *Adapter) Handler() http.Handler { return a.hub }

func (a *Adapter) writeMCPConfig(id string, cfg []byte) (string, error) {
	dir := a.opts.ConfigDir
	if dir == "" {
		a.mu.Lock()
		if a.tmpDir == "" {
			d, err := os.MkdirTemp("", "relay-claudecode-")
			if err != nil {
				a.mu.Unlock()
				return "", err
			}
			a.tmpDir = d
		}
		dir = a.tmpDir
		a.mu.Unlock()
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "mcp-"+id+".json")
	// 0600: the file names a loopback endpoint that can approve anything this
	// session asks for, and it may carry the user's own MCP server env.
	if err := os.WriteFile(path, cfg, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// Close ends every session and shuts the listener.
func (a *Adapter) Close(ctx context.Context) error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	sessions := make([]*Session, 0, len(a.sessions))
	for _, s := range a.sessions {
		sessions = append(sessions, s)
	}
	srv, tmp := a.server, a.tmpDir
	a.mu.Unlock()

	var firstErr error
	for _, s := range sessions {
		if err := s.Close(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if srv != nil {
		if err := srv.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if tmp != "" {
		if err := os.RemoveAll(tmp); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (a *Adapter) forget(id string) {
	a.hub.unregister(id)
	a.mu.Lock()
	delete(a.sessions, id)
	a.mu.Unlock()
}
