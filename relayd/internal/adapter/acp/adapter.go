package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/event"
)

// ErrProtocolVersion is the handshake failing on version skew. ACP negotiates
// in exactly one round trip — we send the latest we speak, the agent answers
// with that or with the latest *it* speaks, and a client that cannot speak the
// answer must disconnect. So skew is a startup failure with a clear message,
// never a mysterious mid-session one.
var ErrProtocolVersion = errors.New("acp: the agent speaks no protocol version we support")

// ErrTransportUnsupported is an MCP server the agent cannot be handed: a URL
// server where mcpCapabilities.http is false. Refused loudly rather than
// dropped, because a silently missing MCP server looks like a broken tool.
var ErrTransportUnsupported = errors.New("acp: the agent did not advertise this MCP transport")

// Options configures the adapter. Runtime is the only required field.
type Options struct {
	// Runtime is OpenClaw, Hermes or OpenCode. It selects the launch command
	// and the per-runtime cost plan.
	Runtime adapter.Runtime

	// Binary and Args override the launch command from ConfigFor.
	Binary string
	Args   []string
	// Env is extra environment for the process, in "K=V" form.
	Env []string

	Log *slog.Logger

	// SessionKey is OpenClaw's --session argument. The key is baked into argv
	// at launch, so one `openclaw acp` process serves one Gateway session; dial
	// a second adapter for a second key. Ignored by Hermes and OpenCode.
	SessionKey string
	// RequireExisting adds OpenClaw's --require-existing, which turns a missing
	// session into a loud failure instead of a silently created one. Set it
	// when reattaching to a session the registry believes exists.
	RequireExisting bool

	// ProtocolVersions is what this client can speak, latest first. Default
	// {ProtocolVersion}.
	ProtocolVersions []int

	// Cost is the out-of-band metering hook. Nil means TurnCompleted.Usage
	// stays nil, which is the honest answer for all three runtimes today.
	Cost CostSource

	// AllowUnstableSetModel opts in to session/set_model. It and the `models`
	// field on the session responses are the only UNSTABLE members of the ACP
	// surface — "may be removed or changed at any point" — so SetModel refuses
	// unless this is set.
	AllowUnstableSetModel bool

	Clock func() time.Time
	// DrainGrace is how long Close waits for a consumer to take the last
	// events off the channel. Default 2s.
	DrainGrace time.Duration
	// CallTimeout bounds the short calls — initialize, session/new,
	// session/set_mode. It deliberately does not bound session/prompt, which is
	// a turn and may legitimately run for an hour. Default 60s.
	CallTimeout time.Duration
	// CancelGrace bounds the wait for a cancelled prompt to resolve. ACP
	// guarantees it resolves with "cancelled"; this is the guard against a
	// runtime that does not honour its own contract. Default 30s.
	CancelGrace time.Duration
	// CostTimeout bounds an out-of-band cost lookup so a slow store cannot
	// delay TurnCompleted. Default 5s.
	CostTimeout time.Duration

	// OnCommands receives available_commands_update. It is ACP's answer to
	// SYSTEM.md §9's tool-list-refresh problem, and it has no home in the nine
	// normalized events, so it is surfaced here rather than swallowed.
	OnCommands func(sessionID string, cmds []AvailableCommand)
	// OnModeChange receives current_mode_update. An agent may change its own
	// mode mid-session, which can change permission behaviour underneath a
	// session the registry believes it understands.
	OnModeChange func(sessionID, modeID string)
	// OnUserMessage receives user_message_chunk. It is replay or echo, never
	// spoken, and there is no normalized event for user text — so it is offered
	// here for the transcript index and dropped otherwise.
	OnUserMessage func(sessionID, text string, replay bool)
}

func (o *Options) defaults() {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if len(o.ProtocolVersions) == 0 {
		o.ProtocolVersions = []int{ProtocolVersion}
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.DrainGrace == 0 {
		o.DrainGrace = 2 * time.Second
	}
	if o.CallTimeout == 0 {
		o.CallTimeout = 60 * time.Second
	}
	if o.CancelGrace == 0 {
		o.CancelGrace = 30 * time.Second
	}
	if o.CostTimeout == 0 {
		o.CostTimeout = 5 * time.Second
	}
}

// Adapter owns one ACP agent process and every session on it.
type Adapter struct {
	opts Options
	cfg  RuntimeConfig
	log  *slog.Logger
	c    *conn
	cmd  *exec.Cmd

	caps adapter.Capabilities

	mu          sync.Mutex
	agentCaps   AgentCapabilities
	authMethods []AuthMethod
	negotiated  int
	sessions    map[string]*Session // by the runtime's session id
	closed      bool
	// extensions is every `_`-prefixed method seen. ACP's extension mechanism
	// is invisible to the schema, so this set is the only way we would find out
	// a runtime shipped its own steering — ADAPTERS.md §8 item 5.
	extensions map[string]int
	// refused counts the fs/* and terminal/* calls answered -32601.
	refused map[string]int
	// orphans holds traffic for a session id we have not finished registering.
	// An agent may push session/update the instant it answers session/new,
	// which is before our own bookkeeping is done; dropping those would lose
	// the first available_commands_update of every session.
	orphans map[string][]inbound
}

// maxOrphans bounds the buffer for a session id that never registers, so a
// runtime naming a session we never opened cannot grow memory without limit.
const maxOrphans = 512

// inbound is one agent→client message addressed to a session.
type inbound struct {
	method string
	id     json.RawMessage
	update json.RawMessage
	perm   *requestPermissionParams
}

var _ adapter.Adapter = (*Adapter)(nil)

// Dial launches the runtime's ACP mode and completes the handshake.
func Dial(ctx context.Context, opts Options) (*Adapter, error) {
	opts.defaults()
	cfg, ok := ConfigFor(opts.Runtime)
	if !ok {
		return nil, fmt.Errorf("acp: %q is not an ACP runtime; this adapter serves %v", opts.Runtime, Runtimes())
	}
	if opts.Binary != "" {
		cfg.Binary = opts.Binary
	}
	if len(opts.Args) > 0 {
		cfg.Args = opts.Args
	}
	bin, args := cfg.argv(opts.SessionKey, opts.RequireExisting)

	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), opts.Env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: stdout: %w", err)
	}
	// The agent's own diagnostics go to stderr and are not protocol. Relaying
	// them into the log is how "not authenticated" becomes a readable failure
	// rather than a silent one.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("acp: starting %s %s: %w", bin, strings.Join(args, " "), err)
	}
	go drainStderr(opts.Log, opts.Runtime, stderr)

	closer := func() error {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return cmd.Wait()
	}

	a, err := Attach(ctx, stdout, stdin, closer, opts)
	if err != nil {
		_ = closer()
		return nil, err
	}
	a.cmd = cmd
	return a, nil
}

// Attach drives an already-open pair of pipes. It is what makes the adapter
// testable with no runtime installed: the fixture replay in trace_test.go is an
// Attach against an in-memory agent.
func Attach(ctx context.Context, r io.Reader, w io.Writer, closer func() error, opts Options) (*Adapter, error) {
	opts.defaults()
	cfg, ok := ConfigFor(opts.Runtime)
	if !ok {
		return nil, fmt.Errorf("acp: %q is not an ACP runtime; this adapter serves %v", opts.Runtime, Runtimes())
	}

	a := &Adapter{
		opts:       opts,
		cfg:        cfg,
		log:        opts.Log.With("runtime", string(opts.Runtime), "protocol", "acp"),
		sessions:   map[string]*Session{},
		extensions: map[string]int{},
		refused:    map[string]int{},
		orphans:    map[string][]inbound{},
		caps:       adapter.Baseline(opts.Runtime),
	}
	a.c = newConn(r, w, closer, a, a.log)
	go a.c.run()

	if err := a.initialize(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

func drainStderr(log *slog.Logger, r adapter.Runtime, rd io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := rd.Read(buf)
		if n > 0 {
			line := strings.TrimRight(string(buf[:n]), "\n")
			if line != "" {
				log.Warn("acp: agent stderr", "runtime", string(r), "line", line)
			}
		}
		if err != nil {
			return
		}
	}
}

// initialize is the mandatory first call. Negotiation is one round trip and
// there is no second: if we cannot speak what came back, we disconnect.
func (a *Adapter) initialize(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, a.opts.CallTimeout)
	defer cancel()

	raw, err := a.c.call(cctx, methodInitialize, initializeParams{
		ProtocolVersion:    a.opts.ProtocolVersions[0],
		ClientCapabilities: relayClientCapabilities(),
	})
	if err != nil {
		return fmt.Errorf("acp: initialize: %w", err)
	}
	var res initializeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("acp: initialize result: %w", err)
	}
	if !slices.Contains(a.opts.ProtocolVersions, res.ProtocolVersion) {
		return fmt.Errorf("%w: agent answered %d, we speak %v", ErrProtocolVersion, res.ProtocolVersion, a.opts.ProtocolVersions)
	}

	a.mu.Lock()
	a.negotiated = res.ProtocolVersion
	a.agentCaps = res.AgentCapabilities
	a.authMethods = res.AuthMethods
	a.mu.Unlock()

	a.caps = narrowFromHandshake(a.caps, a.cfg, res.AgentCapabilities, a.opts.Cost)
	a.log.Info("acp: handshake complete",
		"protocolVersion", res.ProtocolVersion,
		"loadSession", res.AgentCapabilities.LoadSession,
		"promptImage", res.AgentCapabilities.PromptCapabilities.Image,
		"promptAudio", res.AgentCapabilities.PromptCapabilities.Audio,
		"promptEmbeddedContext", res.AgentCapabilities.PromptCapabilities.EmbeddedContext,
		"mcpHTTP", res.AgentCapabilities.MCPCapabilities.HTTP,
		"mcpSSE", res.AgentCapabilities.MCPCapabilities.SSE,
		"authMethods", len(res.AuthMethods))
	return nil
}

// narrowFromHandshake turns the documented baseline into what this agent
// actually said. Everything here is an observation; nothing is a guess.
func narrowFromHandshake(c adapter.Capabilities, cfg RuntimeConfig, ac AgentCapabilities, src CostSource) adapter.Capabilities {
	if ac.LoadSession {
		c = c.With(adapter.CapResume, adapter.SupportYes, "agentCapabilities.loadSession is true on this agent; session/load replays the conversation before it resolves")
	} else {
		c = c.With(adapter.CapResume, adapter.SupportNo, "agentCapabilities.loadSession is false on this agent; the registry must start a new session and say so")
	}
	c = withPromptCap(c, adapter.CapPromptImage, ac.PromptCapabilities.Image, "image")
	c = withPromptCap(c, adapter.CapPromptAudio, ac.PromptCapabilities.Audio, "audio")
	c = withPromptCap(c, adapter.CapPromptEmbeddedContext, ac.PromptCapabilities.EmbeddedContext, "embeddedContext")

	support, note := costNote(cfg, src)
	c = c.With(adapter.CapCostUSD, support, note)
	return c
}

func withPromptCap(c adapter.Capabilities, cap adapter.Capability, on bool, name string) adapter.Capabilities {
	if on {
		return c.With(cap, adapter.SupportYes, "agentCapabilities.promptCapabilities."+name+" is true")
	}
	return c.With(cap, adapter.SupportNo, "agentCapabilities.promptCapabilities."+name+" is false, so this content cannot enter a prompt on this runtime")
}

// Runtime is which of the three this drives.
func (a *Adapter) Runtime() adapter.Runtime { return a.opts.Runtime }

// Capabilities is what the handshake said, narrowed from the documented
// baseline.
func (a *Adapter) Capabilities() adapter.Capabilities { return a.caps }

// Config is the launch configuration in use, including where this runtime keeps
// its own sessions and where its cost would have to come from.
func (a *Adapter) Config() RuntimeConfig { return a.cfg }

// AgentCapabilities is the raw handshake answer.
func (a *Adapter) AgentCapabilities() AgentCapabilities {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.agentCaps
}

// AuthMethods is the handshake's authMethods[]. After a -32000 on session/new,
// recovery is Authenticate with one of these ids and then retrying — which
// belongs in the installer, because a device-code flow cannot be completed by
// voice.
func (a *Adapter) AuthMethods() []AuthMethod {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]AuthMethod(nil), a.authMethods...)
}

// ProtocolVersion is the negotiated wire version.
func (a *Adapter) ProtocolVersion() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.negotiated
}

// Extensions is every `_`-prefixed method the agent has used, with counts. The
// schema cannot see extensions, so this map is the whole of our evidence about
// whether a runtime ships one — including a private steering method, which
// would change ADAPTERS.md §4's central negative.
func (a *Adapter) Extensions() map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]int, len(a.extensions))
	for k, v := range a.extensions {
		out[k] = v
	}
	return out
}

// Refused counts the fs/* and terminal/* requests answered -32601 because Relay
// never advertised the matching client capability. A non-zero count means a
// runtime is calling out of contract, and §4's decision to decline those
// capabilities is worth re-reading.
func (a *Adapter) Refused() map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]int, len(a.refused))
	for k, v := range a.refused {
		out[k] = v
	}
	return out
}

// Authenticate answers a -32000 from session/new. methodID must be one of the
// ids from AuthMethods.
func (a *Adapter) Authenticate(ctx context.Context, methodID string) error {
	known := false
	for _, m := range a.AuthMethods() {
		if m.ID == methodID {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("acp: %q is not one of the auth methods this agent offered", methodID)
	}
	cctx, cancel := context.WithTimeout(ctx, a.opts.CallTimeout)
	defer cancel()
	if _, err := a.c.call(cctx, methodAuthenticate, authenticateParams{MethodID: methodID}); err != nil {
		return fmt.Errorf("acp: authenticate: %w", err)
	}
	return nil
}

// Start opens a new session with session/new.
func (a *Adapter) Start(ctx context.Context, opts adapter.SessionOptions) (adapter.Session, error) {
	if err := a.checkClosed(); err != nil {
		return nil, err
	}
	cwd, err := absWorkspace(opts.Workspace)
	if err != nil {
		return nil, err
	}
	servers, err := a.encodeMCPServers(opts.MCPServers)
	if err != nil {
		return nil, err
	}
	if a.cfg.SessionScopedProcess && a.opts.RequireExisting {
		a.log.Warn("acp: starting a new session on a process launched with " + a.cfg.RequireExistingFlag +
			"; a session key the runtime does not already have will fail loudly, which is the point of the flag")
	}

	cctx, cancel := context.WithTimeout(ctx, a.opts.CallTimeout)
	defer cancel()
	raw, err := a.c.call(cctx, methodSessionNew, newSessionParams{CWD: cwd, MCPServers: servers})
	if err != nil {
		return nil, a.sessionError("session/new", err)
	}
	var res newSessionResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("acp: session/new result: %w", err)
	}
	if res.SessionID == "" {
		return nil, errors.New("acp: session/new returned an empty sessionId")
	}
	return a.register(opts, res.SessionID, cwd, res.Modes, res.Models), nil
}

// Resume reattaches with session/load.
//
// The agent must have advertised loadSession; when it has not, this returns an
// *adapter.UnsupportedError for CapResume and the registry starts a new session
// and says so rather than failing silently. session/load replays the entire
// conversation back as session/update notifications before it resolves, so
// every event produced during it carries Meta.Replay and never pings.
func (a *Adapter) Resume(ctx context.Context, ref adapter.SessionRef, opts adapter.SessionOptions) (adapter.Session, error) {
	if err := a.checkClosed(); err != nil {
		return nil, err
	}
	if err := a.caps.Require(adapter.CapResume); err != nil {
		return nil, err
	}
	native := ref.Native
	if native == "" {
		native = ref.ID
	}
	if native == "" {
		return nil, fmt.Errorf("%w: resume needs the runtime's own session id", adapter.ErrSessionNotFound)
	}
	if a.cfg.SessionScopedProcess {
		key := a.opts.SessionKey
		if key == "" {
			key = a.cfg.DefaultSessionKey
		}
		if native != key {
			return nil, fmt.Errorf("%w: this %s process is bound to %s %q at launch; dial a second adapter with SessionKey %q",
				adapter.ErrSessionNotFound, a.opts.Runtime, a.cfg.SessionFlag, key, native)
		}
	}

	workspace := opts.Workspace
	if workspace == "" {
		workspace = ref.Workspace
	}
	cwd, err := absWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	servers, err := a.encodeMCPServers(opts.MCPServers)
	if err != nil {
		return nil, err
	}

	if opts.ID == "" {
		opts.ID = ref.ID
	}
	s := a.register(opts, native, cwd, nil, nil)
	s.setReplaying(true)

	cctx, cancel := context.WithTimeout(ctx, a.opts.CallTimeout)
	defer cancel()
	raw, err := a.c.call(cctx, methodSessionLoad, loadSessionParams{SessionID: native, CWD: cwd, MCPServers: servers})
	s.setReplaying(false)
	if err != nil {
		a.forget(native)
		s.q.closeAndDrain(a.opts.DrainGrace)
		return nil, a.sessionError("session/load", err)
	}
	var res loadSessionResult
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &res); err != nil {
			a.log.Warn("acp: session/load result did not decode", "err", err)
		}
	}
	s.setModes(res.Modes, res.Models)
	return s, nil
}

func (a *Adapter) register(opts adapter.SessionOptions, native, cwd string, modes *SessionModeState, models *SessionModelState) *Session {
	id := opts.ID
	if id == "" {
		id = uuid.NewString()
	}
	s := newSession(a, id, native, cwd)
	s.setModes(modes, models)

	a.mu.Lock()
	a.sessions[native] = s
	early := a.orphans[native]
	delete(a.orphans, native)
	a.mu.Unlock()

	// Anything that arrived while session/new was still in flight goes first,
	// in wire order, and only then does the session start taking traffic
	// directly. Without this the first available_commands_update of every
	// session is lost to a race with our own bookkeeping.
	s.bootstrap(early)
	return s
}

// route hands one addressed message to its session, buffering it when the
// session id is not registered yet.
func (a *Adapter) route(sessionID string, in inbound) {
	a.mu.Lock()
	s, ok := a.sessions[sessionID]
	if !ok {
		n := len(a.orphans[sessionID])
		if n >= maxOrphans {
			a.mu.Unlock()
			a.log.Warn("acp: dropping traffic for a session id we do not have and that is not opening",
				"sessionId", sessionID, "method", in.method, "buffered", n)
			if in.id != nil {
				a.refuse(in.id, in.method, "no such session on this connection")
			}
			return
		}
		a.orphans[sessionID] = append(a.orphans[sessionID], in)
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()
	s.deliver(in)
}

func (a *Adapter) forget(native string) {
	a.mu.Lock()
	delete(a.sessions, native)
	a.mu.Unlock()
}

// dropOrphans answers anything still buffered for a session that never opened.
// An unanswered JSON-RPC request stalls the agent, so a request dies with a
// refusal rather than with silence.
func (a *Adapter) dropOrphans() {
	a.mu.Lock()
	all := a.orphans
	a.orphans = map[string][]inbound{}
	a.mu.Unlock()
	for id, list := range all {
		for _, in := range list {
			if in.id == nil {
				continue
			}
			a.log.Warn("acp: refusing a buffered request for a session that never opened",
				"sessionId", id, "method", in.method)
			// Synchronously: this runs on the way to closing the transport, and
			// a refusal posted to a goroutine would race the pipes shutting.
			_ = a.c.respondError(in.id, codeMethodNotFound,
				fmt.Sprintf("%s is not available: no such session on this connection", in.method))
		}
	}
}

// Sessions lists the open sessions on this connection, by Relay id.
func (a *Adapter) Sessions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.sessions))
	for _, s := range a.sessions {
		out = append(out, s.ID())
	}
	sort.Strings(out)
	return out
}

// Close shuts down every session and then the runtime.
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
	a.sessions = map[string]*Session{}
	a.mu.Unlock()

	a.dropOrphans()
	for _, s := range sessions {
		s.finish(nil)
	}
	return a.c.close()
}

func (a *Adapter) checkClosed() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return adapter.ErrSessionClosed
	}
	return nil
}

// sessionError maps ACP's two custom JSON-RPC codes onto the errors the
// orchestrator already knows how to degrade against.
func (a *Adapter) sessionError(method string, err error) error {
	var rpc *RPCError
	if errors.As(err, &rpc) {
		switch rpc.Code {
		case CodeAuthRequired:
			ids := make([]string, 0, len(a.AuthMethods()))
			for _, m := range a.AuthMethods() {
				ids = append(ids, m.ID)
			}
			return fmt.Errorf("%w: %s answered -32000 %q; authenticate with one of %v and retry (the installer's job, not a voice flow)",
				adapter.ErrAuthRequired, method, rpc.Message, ids)
		case CodeResourceNotFound:
			return fmt.Errorf("%w: %s answered -32002 %q", adapter.ErrSessionNotFound, method, rpc.Message)
		}
	}
	return fmt.Errorf("acp: %s: %w", method, err)
}

func absWorkspace(p string) (string, error) {
	if p == "" {
		return "", errors.New("acp: session/new needs a workspace; cwd must be an absolute path")
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("acp: cwd must be an absolute path, got %q", p)
	}
	return filepath.Clean(p), nil
}

func (a *Adapter) encodeMCPServers(list []adapter.MCPServer) ([]mcpServer, error) {
	// The field is required, so an empty list must serialise as [] and not null.
	out := make([]mcpServer, 0, len(list))
	caps := a.AgentCapabilities().MCPCapabilities
	for _, m := range list {
		if m.URL != "" {
			// adapter.MCPServer carries no transport discriminator, so a URL
			// server is offered as http — the only URL transport we can name
			// without guessing between two incompatible wire formats.
			if !caps.HTTP {
				return nil, fmt.Errorf("%w: MCP server %q is a URL server but mcpCapabilities.http is false (sse=%v); stdio is the only transport every agent must support",
					ErrTransportUnsupported, m.Name, caps.SSE)
			}
			out = append(out, mcpServer{Type: "http", Name: m.Name, URL: m.URL, Headers: []httpHeader{}})
			continue
		}
		if m.Command == "" {
			return nil, fmt.Errorf("acp: MCP server %q has neither a command nor a url", m.Name)
		}
		env := make([]envVariable, 0, len(m.Env))
		for _, kv := range m.Env {
			k, v := splitEnv(kv)
			env = append(env, envVariable{Name: k, Value: v})
		}
		args := m.Args
		if args == nil {
			args = []string{}
		}
		out = append(out, mcpServer{Name: m.Name, Command: m.Command, Args: args, Env: env})
	}
	return out, nil
}

// ---------- peer: everything the agent sends us ----------

func (a *Adapter) onNotification(method string, params json.RawMessage) {
	if strings.HasPrefix(method, extensionMethodPrefix) {
		a.noteExtension(method)
		a.log.Warn("acp: unknown extension notification — the schema cannot see these, and one of them shipping a steer would change ADAPTERS.md §4",
			"method", method)
		return
	}
	switch method {
	case methodSessionUpdate:
		var n sessionNotification
		if err := json.Unmarshal(params, &n); err != nil {
			a.log.Warn("acp: session/update did not decode", "err", err)
			return
		}
		a.route(n.SessionID, inbound{method: methodSessionUpdate, update: n.Update})
	default:
		a.log.Warn("acp: unknown notification", "method", method)
	}
}

func (a *Adapter) onRequest(id json.RawMessage, method string, params json.RawMessage) {
	if strings.HasPrefix(method, extensionMethodPrefix) {
		a.noteExtension(method)
		a.log.Warn("acp: unknown extension request, answered -32601", "method", method)
		a.refuse(id, method, "unknown extension method")
		return
	}
	switch method {
	case methodRequestPermission:
		a.handlePermission(id, params)
	case methodFSReadTextFile, methodFSWriteTextFile,
		methodTerminalCreate, methodTerminalOutput, methodTerminalWaitExit,
		methodTerminalKill, methodTerminalRelease:
		// Relay declared fs and terminal false. A call anyway is out of
		// contract, and the honest answer is a refusal — never a faked success,
		// because a faked read is a lie the agent then reasons from.
		a.mu.Lock()
		a.refused[method]++
		n := a.refused[method]
		a.mu.Unlock()
		a.log.Warn("acp: agent called a method we never advertised; answering -32601",
			"method", method, "count", n, "declared", clientCapabilityRequired)
		a.refuse(id, method, fmt.Sprintf("%s: relay declares fs.readTextFile, fs.writeTextFile and terminal all false", clientCapabilityRequired))
	default:
		a.log.Warn("acp: unknown request, answered -32601", "method", method)
		a.refuse(id, method, "unknown method")
	}
}

// refuse answers off the reader goroutine. Writing from the dispatch path would
// couple our refusal to the agent draining its own input.
func (a *Adapter) refuse(id json.RawMessage, method, why string) {
	idCopy := append(json.RawMessage(nil), id...)
	go func() {
		if err := a.c.respondError(idCopy, codeMethodNotFound, fmt.Sprintf("%s is not available: %s", method, why)); err != nil {
			a.log.Warn("acp: could not answer a refused request", "method", method, "err", err)
		}
	}()
}

func (a *Adapter) noteExtension(method string) {
	a.mu.Lock()
	a.extensions[method]++
	a.mu.Unlock()
}

func (a *Adapter) onClosed(err error) {
	a.mu.Lock()
	sessions := make([]*Session, 0, len(a.sessions))
	for _, s := range a.sessions {
		sessions = append(sessions, s)
	}
	a.sessions = map[string]*Session{}
	a.closed = true
	a.mu.Unlock()

	if err != nil {
		a.log.Error("acp: connection to the agent ended", "err", err)
	}
	for _, s := range sessions {
		s.finish(err)
	}
}

// emitFatal is how a dead connection reaches a consumer that is only reading
// events.
func (s *Session) emitFatal(err error) {
	s.q.push(event.Error{
		Meta:    s.meta(""),
		Code:    "connection_closed",
		Message: err.Error(),
		Fatal:   true,
	})
}
