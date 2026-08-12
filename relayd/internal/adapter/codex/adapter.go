package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/event"
)

// Options configures the app-server adapter.
type Options struct {
	// Binary defaults to "codex" on PATH.
	Binary string
	// Args defaults to ["app-server"].
	Args []string
	// Env is extra environment for the process, in "K=V" form.
	Env []string

	Log *slog.Logger

	// ClientName and ClientVersion go into the mandatory `initialize`
	// handshake. Codex logs them, so they should say "relay".
	ClientName    string // default "relay"
	ClientVersion string // default "0"

	// DisableExperimentalAPI turns off `capabilities.experimentalApi`. Relay
	// asks for it by default because item/tool/requestUserInput — a needs-input
	// source — is gated behind it. The flag fails loudly per method rather than
	// silently, so asking costs nothing.
	DisableExperimentalAPI bool

	// OptOutNotifications is `capabilities.optOutNotificationMethods`: the
	// supported way to shed notification volume, better than filtering in the
	// adapter because it saves the serialisation.
	OptOutNotifications []string

	// ApprovalPolicy is `AskForApproval` and defaults to "on-request". It is
	// always sent explicitly: leaving it to the user's config.toml is how
	// "never" gets switched on under us.
	ApprovalPolicy string
	// Sandbox is `SandboxMode` and defaults to "workspace-write".
	Sandbox string

	// UnverifiedReplies registers a reply encoder for one of
	// [UnverifiedReplyMethods]. Without one, those requests are answered with a
	// JSON-RPC error rather than a guessed payload — see ADAPTERS.md §8 item 7.
	UnverifiedReplies map[string]ReplyEncoder

	// Clock defaults to time.Now.
	Clock func() time.Time
	// DrainGrace is how long Close waits for a consumer to take the last events
	// off the channel. Default 2s.
	DrainGrace time.Duration
	// StartTimeout bounds the wait for the `thread/started` notification that
	// names a new thread. Default 30s.
	StartTimeout time.Duration
}

func (o *Options) defaults() {
	if o.Binary == "" {
		o.Binary = "codex"
	}
	if len(o.Args) == 0 {
		o.Args = []string{"app-server"}
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.ClientName == "" {
		o.ClientName = "relay"
	}
	if o.ClientVersion == "" {
		o.ClientVersion = "0"
	}
	if o.ApprovalPolicy == "" {
		o.ApprovalPolicy = "on-request"
	}
	if o.Sandbox == "" {
		o.Sandbox = "workspace-write"
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.DrainGrace == 0 {
		o.DrainGrace = 2 * time.Second
	}
	if o.StartTimeout == 0 {
		o.StartTimeout = 30 * time.Second
	}
}

// Adapter owns one `codex app-server` process and every thread on it.
type Adapter struct {
	opts Options
	log  *slog.Logger
	c    *conn
	cmd  *exec.Cmd

	// openMu serialises thread opens: `thread/started` is the only place a new
	// thread id appears, and binding it to the right Session needs exactly one
	// open in flight.
	openMu sync.Mutex

	mu         sync.Mutex
	byThread   map[string]*Session
	starting   *Session
	closed     bool
	initResult json.RawMessage
}

var _ adapter.Adapter = (*Adapter)(nil)

// Dial spawns `codex app-server` and completes the handshake.
func Dial(ctx context.Context, opts Options) (*Adapter, error) {
	opts.defaults()

	cmd := exec.Command(opts.Binary, opts.Args...)
	cmd.Env = append(os.Environ(), opts.Env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("codex: stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("codex: stdout: %w", err)
	}
	// app-server's own diagnostics go to stderr and are not protocol. Relaying
	// them into the log is how a "codex is not authenticated" turns into a
	// readable failure instead of a silent one.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("codex: stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("codex: starting %s: %w", opts.Binary, err)
	}
	go drainStderr(opts.Log, stderr)

	closer := func() error {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return cmd.Wait()
	}

	a, err := attach(ctx, stdout, stdin, closer, opts)
	if err != nil {
		_ = closer()
		return nil, err
	}
	a.cmd = cmd
	return a, nil
}

// Attach drives an app-server that is already running — the seam tests use, and
// the one a non-stdio transport would use if Codex ever grows one.
func Attach(ctx context.Context, r io.Reader, w io.Writer, closer func() error, opts Options) (*Adapter, error) {
	opts.defaults()
	return attach(ctx, r, w, closer, opts)
}

func attach(ctx context.Context, r io.Reader, w io.Writer, closer func() error, opts Options) (*Adapter, error) {
	a := &Adapter{
		opts:     opts,
		log:      opts.Log.With("runtime", "codex"),
		byThread: map[string]*Session{},
	}
	a.c = newConn(r, w, closer, a, a.log)
	go a.c.run()

	if err := a.initialize(ctx); err != nil {
		a.c.close()
		return nil, err
	}
	return a, nil
}

func drainStderr(log *slog.Logger, r io.Reader) {
	b := make([]byte, 4096)
	for {
		n, err := r.Read(b)
		if n > 0 {
			for _, line := range strings.Split(strings.TrimRight(string(b[:n]), "\n"), "\n") {
				if line != "" {
					log.Warn("codex: app-server stderr", "line", line)
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// initialize is mandatory and typed. It was missing from ADAPTERS.md §3's table
// entirely; without it nothing else works.
func (a *Adapter) initialize(ctx context.Context) error {
	caps := &initializeCapabilities{
		ExperimentalApi:           !a.opts.DisableExperimentalAPI,
		OptOutNotificationMethods: a.opts.OptOutNotifications,
		// Left false deliberately: setting it is what makes Codex send
		// attestation/generate, which Relay has no way to answer.
		RequestAttestation: false,
	}
	res, err := a.c.call(ctx, "initialize", initializeParams{
		ClientInfo:   clientInfo{Name: a.opts.ClientName, Version: a.opts.ClientVersion, Title: "Relay"},
		Capabilities: caps,
	})
	if err != nil {
		return fmt.Errorf("codex: initialize: %w", err)
	}
	a.mu.Lock()
	a.initResult = res
	a.mu.Unlock()
	// Nothing is read out of the result: `generate-json-schema` emits params
	// only and there is no ServerResponse.json to say what is in there.
	a.log.Info("codex: app-server handshake complete", "experimental_api", caps.ExperimentalApi)
	return nil
}

func (a *Adapter) Runtime() adapter.Runtime { return adapter.Codex }

func (a *Adapter) now() time.Time { return a.opts.Clock() }

// Capabilities is ADAPTERS.md §5's row for Codex, narrowed by what this
// adapter's configuration can actually deliver.
func (a *Adapter) Capabilities() adapter.Capabilities {
	c := adapter.Baseline(adapter.Codex)

	// One note, not several: With replaces, so writing CapNeedsInput twice
	// would leave the console showing only the last reason.
	caveats := []string{"server-to-client requests that block until answered"}
	if a.opts.DisableExperimentalAPI {
		caveats = append(caveats,
			"experimentalApi is off, so item/tool/requestUserInput never arrives; the four non-experimental requests still do")
	}
	missing := 0
	for _, m := range UnverifiedReplyMethods() {
		if a.opts.UnverifiedReplies[m] == nil {
			missing++
		}
	}
	if missing > 0 {
		caveats = append(caveats, fmt.Sprintf(
			"%d of the 5 approval requests have no verified reply shape and are refused rather than guessed (ADAPTERS.md §8 item 7)", missing))
	}
	if len(caveats) > 1 {
		c = c.With(adapter.CapNeedsInput, adapter.SupportYes, strings.Join(caveats, "; "))
	}
	// Prompt capabilities: Codex's UserInput has image and localImage variants,
	// but nobody has probed whether a running Codex accepts them from
	// app-server, so they stay Unknown rather than becoming a yes we cannot
	// stand behind. Audio and embedded context have no variant at all.
	c = c.With(adapter.CapPromptAudio, adapter.SupportNo, "UserInput has no audio variant").
		With(adapter.CapPromptEmbeddedContext, adapter.SupportNo, "UserInput has no embedded-context variant").
		With(adapter.CapPromptImage, adapter.SupportUnknown,
			"UserInput has image and localImage variants; whether app-server accepts them has not been probed")
	return c
}

// Start opens a new thread.
func (a *Adapter) Start(ctx context.Context, opts adapter.SessionOptions) (adapter.Session, error) {
	policy, reviewer, sandbox, err := a.approvalSettings(opts)
	if err != nil {
		return nil, err
	}
	return a.open(ctx, opts, "thread/start", threadStartParams{
		Cwd:               opts.Workspace,
		Model:             opts.Model,
		Sandbox:           sandbox,
		ApprovalPolicy:    policy,
		ApprovalsReviewer: reviewer,
		Config:            mcpConfig(opts.MCPServers, a.log),
	}, "")
}

// Resume reattaches to a thread. `thread/resume` rejoins a running thread and
// loads a stopped one; either way the id we key on comes back on
// `thread/started`.
func (a *Adapter) Resume(ctx context.Context, ref adapter.SessionRef, opts adapter.SessionOptions) (adapter.Session, error) {
	thread := ref.Native
	if thread == "" {
		thread = ref.ID
	}
	if thread == "" {
		return nil, fmt.Errorf("%w: resume needs a thread id", adapter.ErrSessionNotFound)
	}
	policy, reviewer, sandbox, err := a.approvalSettings(opts)
	if err != nil {
		return nil, err
	}
	if opts.ID == "" {
		opts.ID = ref.ID
	}
	if opts.Workspace == "" {
		opts.Workspace = ref.Workspace
	}
	return a.open(ctx, opts, "thread/resume", threadResumeParams{
		ThreadID:          thread,
		Cwd:               opts.Workspace,
		Model:             opts.Model,
		Sandbox:           sandbox,
		ApprovalPolicy:    policy,
		ApprovalsReviewer: reviewer,
		Config:            mcpConfig(opts.MCPServers, a.log),
	}, thread)
}

// Fork copies a thread's history into a new one. It is not on
// adapter.Adapter — only two of the five runtimes can do it — so a caller
// reaches for it after checking CapFork.
func (a *Adapter) Fork(ctx context.Context, ref adapter.SessionRef, opts adapter.SessionOptions) (adapter.Session, error) {
	thread := ref.Native
	if thread == "" {
		thread = ref.ID
	}
	if thread == "" {
		return nil, fmt.Errorf("%w: fork needs a thread id", adapter.ErrSessionNotFound)
	}
	policy, reviewer, sandbox, err := a.approvalSettings(opts)
	if err != nil {
		return nil, err
	}
	// The forked thread gets a new id, so nothing is pre-registered here.
	return a.open(ctx, opts, "thread/fork", threadForkParams{
		ThreadID:          thread,
		Cwd:               opts.Workspace,
		Model:             opts.Model,
		Sandbox:           sandbox,
		ApprovalPolicy:    policy,
		ApprovalsReviewer: reviewer,
		Config:            mcpConfig(opts.MCPServers, a.log),
	}, "")
}

// open runs the one sequence all three entry points share: register a session,
// make the call, and wait for the `thread/started` notification that names the
// thread. knownThread pre-registers the routing key when we already have one,
// so notifications that beat the notification still land.
func (a *Adapter) open(ctx context.Context, opts adapter.SessionOptions, method string, params any, knownThread string) (adapter.Session, error) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil, adapter.ErrSessionClosed
	}
	a.mu.Unlock()

	id := opts.ID
	if id == "" {
		id = uuid.NewString()
	}
	s := newSession(a, id, opts.Workspace)

	a.openMu.Lock()
	defer a.openMu.Unlock()

	if knownThread != "" {
		// Resume knows the id up front, so notifications that beat the
		// thread/started notification still route. Fork does not: the forked
		// thread gets a fresh id and nothing may be pre-registered.
		s.setThread(knownThread)
	}
	a.mu.Lock()
	a.starting = s
	if knownThread != "" {
		a.byThread[knownThread] = s
	}
	a.mu.Unlock()

	clearStarting := func() {
		a.mu.Lock()
		if a.starting == s {
			a.starting = nil
		}
		a.mu.Unlock()
	}

	if _, err := a.c.call(ctx, method, params); err != nil {
		clearStarting()
		a.forget(s)
		// Nobody ever received this session, so there is no consumer to drain to.
		s.q.closeAndDrain(0)
		if isAuthError(err) {
			return nil, fmt.Errorf("%w: %w", adapter.ErrAuthRequired, err)
		}
		return nil, fmt.Errorf("codex: %s: %w", method, err)
	}

	wait, cancel := context.WithTimeout(ctx, a.opts.StartTimeout)
	defer cancel()
	select {
	case <-s.ready:
		clearStarting()
		return s, nil
	case <-a.c.done():
		clearStarting()
		a.forget(s)
		// Nobody ever received this session, so there is no consumer to drain to.
		s.q.closeAndDrain(0)
		return nil, fmt.Errorf("codex: app-server went away during %s: %w", method, a.c.closeErr())
	case <-wait.Done():
		clearStarting()
		a.forget(s)
		// Nobody ever received this session, so there is no consumer to drain to.
		s.q.closeAndDrain(0)
		// The id is only ever announced on thread/started. Without it there is
		// nothing to key on, so this is a failure rather than a degraded start.
		return nil, fmt.Errorf("codex: %s returned but no thread/started arrived: %w", method, wait.Err())
	}
}

// approvalSettings turns SessionOptions into the two settings that decide
// whether approvals reach us at all, refusing the ones that switch them off.
func (a *Adapter) approvalSettings(opts adapter.SessionOptions) (policy, reviewer, sandbox string, err error) {
	policy = a.opts.ApprovalPolicy
	if opts.PermissionMode != "" {
		policy = opts.PermissionMode
	}
	switch policy {
	case "untrusted", "on-failure", "on-request":
	case "never":
		return "", "", "", fmt.Errorf(`codex: approvalPolicy "never" disables every approval request; SessionOptions.PermissionMode must not be an auto or bypass mode`)
	default:
		return "", "", "", fmt.Errorf("codex: unknown approvalPolicy %q (want untrusted, on-failure or on-request)", policy)
	}

	reviewer = "user"
	if v, ok := opts.Extra["approvalsReviewer"]; ok && v != "" {
		if v != "user" {
			return "", "", "", fmt.Errorf("codex: approvalsReviewer %q routes approvals to a subagent; Relay requires \"user\"", v)
		}
		reviewer = v
	}

	sandbox = a.opts.Sandbox
	if v, ok := opts.Extra["sandbox"]; ok && v != "" {
		sandbox = v
	}
	switch sandbox {
	case "read-only", "workspace-write", "danger-full-access":
	default:
		return "", "", "", fmt.Errorf("codex: unknown sandbox %q", sandbox)
	}
	return policy, reviewer, sandbox, nil
}

// mcpConfig folds MEMORY.md §7's shared registry into `thread/start`'s free-form
// `config` object.
//
// There is no mcpServers field on any of the thread methods; Codex takes MCP
// servers from config, and `config` is `additionalProperties: true` with no
// schema at all. The key names below follow Codex's own config.toml
// (`[mcp_servers.<name>]` with command/args/env, or url for HTTP servers) and
// are UNVERIFIED against a running app-server — hence the log line.
func mcpConfig(servers []adapter.MCPServer, log *slog.Logger) map[string]any {
	if len(servers) == 0 {
		return nil
	}
	out := map[string]any{}
	for _, s := range servers {
		entry := map[string]any{}
		switch {
		case s.URL != "":
			entry["url"] = s.URL
		case s.Command != "":
			entry["command"] = s.Command
			if len(s.Args) > 0 {
				entry["args"] = s.Args
			}
		default:
			continue
		}
		if len(s.Env) > 0 {
			env := map[string]any{}
			for _, kv := range s.Env {
				k, v, ok := strings.Cut(kv, "=")
				if ok {
					env[k] = v
				}
			}
			entry["env"] = env
		}
		out[s.Name] = entry
	}
	if len(out) == 0 {
		return nil
	}
	log.Warn("codex: injecting MCP servers through thread/start config.mcp_servers — key shape follows config.toml and is unverified against app-server",
		"count", len(out))
	return map[string]any{"mcp_servers": out}
}

func isAuthError(err error) bool {
	var re *rpcError
	if errors.As(err, &re) {
		return re.Code == -32000 && strings.Contains(strings.ToLower(re.Message), "auth")
	}
	return false
}

// Sessions lists every live session.
func (a *Adapter) Sessions() []*Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*Session, 0, len(a.byThread))
	for _, s := range a.byThread {
		out = append(out, s)
	}
	return out
}

func (a *Adapter) forget(s *Session) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, cur := range a.byThread {
		if cur == s {
			delete(a.byThread, id)
		}
	}
	if a.starting == s {
		a.starting = nil
	}
}

// Close ends every session and stops the process.
func (a *Adapter) Close(ctx context.Context) error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	sessions := make([]*Session, 0, len(a.byThread))
	for _, s := range a.byThread {
		sessions = append(sessions, s)
	}
	a.byThread = map[string]*Session{}
	a.mu.Unlock()

	for _, s := range sessions {
		s.finish("the adapter was closed", false)
		s.q.closeAndDrain(a.opts.DrainGrace)
	}
	a.c.close()
	return nil
}

// ---- inbound traffic ----

func (a *Adapter) onNotification(method string, params json.RawMessage) {
	tid := threadIDOf(method, params)
	if tid == "" {
		a.connectionNotification(method, params)
		return
	}
	s := a.route(method, tid, params)
	if s == nil {
		a.log.Debug("codex: notification for a thread Relay does not own", "method", method, "thread", tid)
		return
	}
	s.norm.handle(method, params)
}

func (a *Adapter) route(method, tid string, params json.RawMessage) *Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s, ok := a.byThread[tid]; ok {
		if method == "thread/started" && a.starting == s {
			a.starting = nil
		}
		return s
	}
	if method != "thread/started" || a.starting == nil {
		return nil
	}
	// A sub-agent thread starting mid-open would otherwise be mistaken for the
	// thread we asked for. parentThreadId is how Codex says "this is a
	// sub-agent", and it is observable.
	var p threadStartedNote
	if err := json.Unmarshal(params, &p); err == nil && p.Thread.ParentThreadID != nil && *p.Thread.ParentThreadID != "" {
		a.log.Info("codex: sub-agent thread started", "thread", tid, "parent", *p.Thread.ParentThreadID)
		return nil
	}
	s := a.starting
	a.starting = nil
	a.byThread[tid] = s
	return s
}

func (a *Adapter) connectionNotification(method string, params json.RawMessage) {
	switch method {
	case "deprecationNotice":
		a.log.Warn("codex: deprecation notice from app-server — re-vendor the schemas", "params", string(params))
	case "configWarning", "warning":
		a.log.Warn("codex: warning", "method", method, "params", string(params))
	default:
		a.log.Debug("codex: connection-level notification", "method", method)
	}
}

func (a *Adapter) onServerRequest(id json.RawMessage, method string, params json.RawMessage) {
	switch method {
	case MethodDynamicToolCall, MethodAuthRefresh, MethodAttestation, MethodApplyPatchLegacy, MethodExecCommandLegacy:
		a.refuse(id, method)
		return
	}
	tid := threadIDOf(method, params)
	a.mu.Lock()
	s := a.byThread[tid]
	a.mu.Unlock()
	if s == nil {
		a.log.Warn("codex: server request for a thread Relay does not own", "method", method, "thread", tid)
		// Still answered: an unanswered request blocks app-server for good.
		_ = a.c.respondError(id, codeInternalError, "relay has no session for thread "+tid)
		return
	}
	s.handleServerRequest(id, method, params)
}

// refuse answers one of the five server requests Relay wants nothing to do
// with. -32601 is honest — we genuinely do not implement these — and dropping
// one instead hangs Codex on a question it will wait on forever.
func (a *Adapter) refuse(id json.RawMessage, method string) {
	var why string
	switch method {
	case MethodDynamicToolCall:
		why = "relay registers no dynamic tools"
	case MethodAuthRefresh:
		why = "relay does not own ChatGPT auth tokens; surface the auth failure instead of waiting on us"
	case MethodAttestation:
		why = "relay leaves capabilities.requestAttestation false"
	case MethodApplyPatchLegacy, MethodExecCommandLegacy:
		why = "relay drives turns with turn/start, so the legacy approval APIs are unreachable"
	default:
		why = "unknown server request"
	}
	a.log.Warn("codex: refusing a server request", "method", method, "reason", why)
	if err := a.c.respondError(id, codeMethodNotFound, method+": "+why); err != nil {
		a.log.Error("codex: could not refuse a server request", "method", method, "err", err)
	}
}

func (a *Adapter) onClosed(err error) {
	a.mu.Lock()
	sessions := make([]*Session, 0, len(a.byThread))
	for _, s := range a.byThread {
		sessions = append(sessions, s)
	}
	a.byThread = map[string]*Session{}
	starting := a.starting
	a.starting = nil
	a.mu.Unlock()

	reason := "the app-server connection closed"
	if err != nil {
		reason = "the app-server connection failed: " + err.Error()
		a.log.Error("codex: app-server connection failed", "err", err)
	}
	for _, s := range sessions {
		s.finish(reason, err != nil)
	}
	if starting != nil {
		starting.finish(reason, err != nil)
	}
}

// threadIDOf pulls the routing key out of a payload. Most methods carry
// threadId; thread/started carries the whole Thread, and the two deprecated
// legacy approvals call it conversationId.
func threadIDOf(method string, params json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	switch method {
	case "thread/started":
		var p threadStartedNote
		if err := json.Unmarshal(params, &p); err == nil {
			return p.Thread.ID
		}
		return ""
	case MethodApplyPatchLegacy, MethodExecCommandLegacy:
		var p struct {
			ConversationID string `json:"conversationId"`
		}
		if err := json.Unmarshal(params, &p); err == nil {
			return p.ConversationID
		}
		return ""
	}
	var p threadIDOnly
	if err := json.Unmarshal(params, &p); err == nil {
		return p.ThreadID
	}
	return ""
}

// compile-time proof that the event package's sealed union is what this adapter
// produces, and nothing else.
var _ = []event.Kind{
	event.KindTurnStarted, event.KindTextDelta, event.KindReasoning,
	event.KindToolStarted, event.KindToolOutput, event.KindPlanUpdated,
	event.KindNeedsInput, event.KindTurnCompleted, event.KindError,
}
