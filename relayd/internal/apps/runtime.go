package apps

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/luthor007/relay/relayd/internal/logx"
)

//go:embed runner/host.mjs runner/sdk.js
var runnerFS embed.FS

// Runtime runs one app, once, per trigger.
//
// It composes the three layers that together are APP-PLATFORM.md §5:
//
//   - the [Sandbox], which is the kernel's part — namespaces, uid, rlimits;
//   - the runtime process's permission model, which is the filesystem and
//     process-creation part;
//   - the supervisor here, which is the clock and the kill.
//
// And it composes the one number none of them produce on their own: the
// [Enforcement] value, which says which of those parts actually hold on this
// machine. Every [Invocation] carries a copy, so a record of what an app did is
// also a record of what was containing it while it did it.

// Outcome is how one invocation ended.
type Outcome string

const (
	// OutcomeCompleted is onTrigger resolving.
	OutcomeCompleted Outcome = "completed"
	// OutcomeFailed is onTrigger throwing. The app's own message is on
	// [Invocation].Error.
	OutcomeFailed Outcome = "failed"
	// OutcomeTimeout is the wall clock running out. The process was killed.
	OutcomeTimeout Outcome = "timeout"
	// OutcomeCrashed is the process dying without saying anything — an OOM, a
	// SIGXCPU, a segfault in the runtime.
	OutcomeCrashed Outcome = "crashed"
	// OutcomeRefused is relayd declining to run it at all. See
	// [ErrCannotContain].
	OutcomeRefused Outcome = "refused"
)

// Invocation is the record of one run.
type Invocation struct {
	ID        string      `json:"id"`
	AppID     string      `json:"app"`
	Version   string      `json:"version"`
	Trigger   TriggerType `json:"trigger"`
	StartedAt time.Time   `json:"startedAt"`
	EndedAt   time.Time   `json:"endedAt"`
	Outcome   Outcome     `json:"outcome"`
	// Error is the app's own failure message, or relayd's reason for refusing.
	Error string `json:"error,omitempty"`
	// ExitCode is the runtime process's exit status, or -1 if it was signalled.
	ExitCode int `json:"exitCode"`

	Calls       int64 `json:"calls"`
	Refused     int64 `json:"refused"`
	FailedCalls int64 `json:"failedCalls"`
	// LogsDropped counts lines that could not be recorded. A log that silently
	// vanishes is worse than one that is absent, so the count is part of the
	// record.
	LogsDropped int64 `json:"logsDropped"`

	// Spoken is what the app said through the glasses, in order. It is on the
	// record because `onTrigger` returns nothing: when the agent called this app
	// as a tool, what it said out loud is the only answer there is.
	Spoken []string `json:"spoken,omitempty"`
	// Output is the app's own log for this run, capped. The full stream goes to
	// the [LogSink]; this is the tail a console or an agent can read without
	// going to look.
	Output []string `json:"output,omitempty"`

	// Enforcement is what was actually containing the app during this run,
	// including any control that degraded while it started.
	Enforcement Enforcement `json:"enforcement"`
	Sandbox     string      `json:"sandbox"`
	// AccessLogDurable says whether the memory reads this app made survive a
	// restart. A console that shows "reads are recorded" must be able to tell
	// when they are only recorded until the next reboot.
	AccessLogDurable bool `json:"accessLogDurable"`
}

// Duration is how long the run took.
func (i Invocation) Duration() time.Duration { return i.EndedAt.Sub(i.StartedAt) }

// ErrCannotContain is relayd refusing to run an app because this machine cannot
// enforce a boundary the app's own scopes make necessary.
//
// APP-PLATFORM.md §3: *an app with `memory.read` and unrestricted network access
// is an exfiltration tool.* That is a statement about a combination, so the
// refusal is about a combination too: an app that can read the user's life does
// not run where egress cannot be enforced, and an app that cannot read anything
// runs anywhere. The alternative — run it and note the weakness in a log — is how
// a sandbox becomes a thing that is true on the machines nobody uses.
var ErrCannotContain = errors.New("apps: this machine cannot contain this app")

// CannotContainError names what was missing and what made it necessary.
type CannotContainError struct {
	AppID string
	Scope Scope
	// Control is the boundary that is not enforced.
	Control string
	Because Guarantee
}

func (e *CannotContainError) Error() string {
	return fmt.Sprintf(
		"apps: %s holds %s, and %s isolation on this machine is %s (%s). "+
			"An app that can read your life and reach the network without a boundary between them is an "+
			"exfiltration tool, so it is not started",
		e.AppID, e.Scope, e.Control, e.Because.Control, e.Because.Note)
}

func (e *CannotContainError) Unwrap() error { return ErrCannotContain }

// Options configures a [Runtime].
type Options struct {
	// Node is the path to the JavaScript runtime. Looked up on PATH when empty.
	Node string
	// RuntimeDir is where the runner and the SDK stub are written. It must be a
	// directory relayd owns and the app cannot write to.
	RuntimeDir string
	// Sandbox is the containment layer. Built by [NewSandbox] when nil.
	Sandbox Sandbox
	// DisableSandbox turns namespace isolation off — for a machine where it is
	// known broken. It downgrades [Enforcement] honestly, which means the
	// default policy will refuse apps that read the user's life.
	DisableSandbox bool
	// AllowUnenforcedNetwork runs apps that read the user's life even where
	// egress cannot be enforced. Off by default, and turning it on is a decision
	// with a name.
	AllowUnenforcedNetwork bool

	Limits Limits

	// FetchClient is the HTTP client `ctx.fetch` runs on. Nil builds one with
	// the default timeout. It is an option because a self-hoster behind a
	// corporate proxy has one already, and because redirect handling is this
	// package's job either way — [Fetcher] overrides CheckRedirect on whatever
	// it is given, so an app's allowlist cannot be escaped by a 302.
	FetchClient *http.Client

	Source    Source
	Sink      Sink
	Device    Device
	Indicator Indicator
	// Screen is where a view goes — the phone, in the shipped daemon. Nil means
	// this box has no render surface at all, which is different from having one
	// with nothing connected: `ui` is still minted either way, because a phone
	// that connects a second later must not require the app to be reinstalled,
	// and the call answers CodeUnavailable while there is nobody there.
	Screen    Screen
	Agent     AgentSession
	Storage   Storage
	AccessLog AccessLog
	EgressLog EgressLog
	LogSink   LogSink
	// Redact is required. See [ErrNoRedactor].
	Redact Redactor

	// KeepScratch leaves an app's scratch in place between invocations. Off by
	// default — the scratch is emptied before every run — because a scratch that
	// survives is a place to accumulate state the user cannot see, and §5 calls
	// it scratch rather than storage for a reason. `ctx.storage` is where an app
	// keeps something on purpose.
	KeepScratch bool

	Log   *slog.Logger
	Now   func() time.Time
	NewID func() string
}

// Runtime is the app runtime.
type Runtime struct {
	opts    Options
	sandbox Sandbox
	node    string
	runner  string
	sdkDir  string
	enf     Enforcement
	log     *slog.Logger
	now     func() time.Time
	newID   func() string
}

// New builds the runtime, probing the machine rather than assuming it.
func New(ctx context.Context, o Options) (*Runtime, error) {
	if o.Redact == nil {
		return nil, ErrNoRedactor
	}
	if o.AccessLog == nil {
		return nil, errors.New("apps: the runtime needs an access log — an unlogged memory read is the thing §5 forbids")
	}
	if o.EgressLog == nil {
		o.EgressLog = &MemoryEgressLog{}
	}
	if o.Log == nil {
		o.Log = logx.Discard()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.NewID == nil {
		o.NewID = uuid.NewString
	}
	if o.RuntimeDir == "" {
		return nil, errors.New("apps: the runtime needs a directory to write the runner into")
	}

	node := o.Node
	if node == "" {
		p, err := exec.LookPath("node")
		if err != nil {
			return nil, fmt.Errorf("apps: no JavaScript runtime found: %w", err)
		}
		node = p
	}
	if !filepath.IsAbs(node) {
		abs, err := filepath.Abs(node)
		if err != nil {
			return nil, err
		}
		node = abs
	}

	runner, sdkDir, err := writeRunner(o.RuntimeDir)
	if err != nil {
		return nil, err
	}

	sb := o.Sandbox
	if sb == nil {
		sb, err = NewSandbox(ctx, SandboxOptions{Probe: node, Disable: o.DisableSandbox})
		if err != nil {
			return nil, err
		}
	}

	r := &Runtime{
		opts: o, sandbox: sb, node: node, runner: runner, sdkDir: sdkDir,
		log: o.Log, now: o.Now, newID: o.NewID,
	}
	r.enf = r.compose(sb.Enforcement(), limitExpectation(o.Limits))
	return r, nil
}

// writeRunner materialises the runner and the SDK stub. They are written
// read-only into a directory relayd owns: the app process can read them (it has
// to — the runner is what it executes) and cannot write them, which matters
// because the runner is the one piece of relayd's code inside the sandbox.
func writeRunner(dir string) (runner, sdkDir string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("apps: runtime directory: %w", err)
	}
	runner = filepath.Join(dir, "host.mjs")
	sdkDir = filepath.Join(dir, "sdk")
	if err := os.MkdirAll(sdkDir, 0o755); err != nil {
		return "", "", err
	}
	files := map[string]string{
		"runner/host.mjs": runner,
		"runner/sdk.js":   filepath.Join(sdkDir, "index.js"),
	}
	for src, dst := range files {
		b, err := runnerFS.ReadFile(src)
		if err != nil {
			return "", "", err
		}
		_ = os.Remove(dst)
		if err := os.WriteFile(dst, b, 0o444); err != nil {
			return "", "", fmt.Errorf("apps: write %s: %w", dst, err)
		}
	}
	pkg := `{"name":"@relay/sdk","version":"0.1.0","type":"module","main":"index.js","exports":{".":"./index.js"}}`
	dst := filepath.Join(sdkDir, "package.json")
	_ = os.Remove(dst)
	if err := os.WriteFile(dst, []byte(pkg), 0o444); err != nil {
		return "", "", err
	}
	return runner, sdkDir, nil
}

// SDKDir is where the runtime keeps the `@relay/sdk` stub, so [Install] can put
// a copy inside an app's root where Node's resolver will find it.
func (r *Runtime) SDKDir() string { return r.sdkDir }

// Enforcement is what this machine actually enforces, measured at startup.
func (r *Runtime) Enforcement() Enforcement { return r.enf }

// SandboxName identifies the containment implementation in use.
func (r *Runtime) SandboxName() string { return r.sandbox.Name() }

// compose fills in the boundaries the sandbox layer does not hold.
func (r *Runtime) compose(e Enforcement, lim *limitReport) Enforcement {
	e.Filesystem = Enforced("node --permission",
		"the app process is denied fs, child_process, native addons and worker threads outright; "+
			"its own root is readable and its scratch is writable, and nothing else is. This is enforced "+
			"inside the runtime process, not by the kernel — the namespaces behind it are what stands "+
			"between a runtime escape and the rest of the box")
	e.WallClock = Enforced("supervisor",
		"SIGTERM to the whole process group at the deadline, SIGKILL after the grace period")
	if lim != nil {
		e.CPU, e.Memory, e.FileSize = lim.CPU, lim.Memory, lim.FileSize
	}
	return e
}

// CanRun reports whether this machine can contain this app.
//
// Separate from [Runtime.Invoke] so an install flow can refuse before it copies
// anything, and so a console can say "this app will not run here, and why"
// instead of showing an app that fails at every trigger.
func (r *Runtime) CanRun(inst Installed) error {
	if r.enf.Network.Control == ControlEnforced || r.opts.AllowUnenforcedNetwork {
		return nil
	}
	for _, s := range inst.Granted {
		if s.ReadsYourLife() {
			return &CannotContainError{
				AppID: inst.Manifest.ID, Scope: s, Control: "network", Because: r.enf.Network,
			}
		}
	}
	return nil
}

// Invoke runs one app once.
//
// The sequence is the contract, and each step is there because the one before it
// would otherwise be a promise: refuse what cannot be contained, mint only what
// was granted, start contained, cap before the app loads, supervise the clock,
// record what happened.
func (r *Runtime) Invoke(ctx context.Context, inst Installed, trigger TriggerFrame) (Invocation, error) {
	// Canonicalise before anything reads a path out of the layout.
	//
	// Node's permission model compares the path an app passes to fs against the
	// grants **literally** — it does not resolve either side first — so a grant
	// and a use that name the same file through different symlinks do not match,
	// and the app dies with ERR_ACCESS_DENIED. Measured on Node 25.8.0: granting
	// /tmp/x and writing /tmp/x/f is refused naming the file, while granting and
	// writing /private/tmp/x/f succeeds. Doing it here rather than in nodeArgs
	// keeps the grants, the environment the app is handed and the OS sandbox's
	// own path list derived from one value that is already real.
	// Entry is the path the runner imports, and it is checked the same way.
	inst.Layout, inst.Entry = inst.Layout.resolved(), resolveLinks(inst.Entry)

	inv := Invocation{
		ID:               r.newID(),
		AppID:            inst.Manifest.ID,
		Version:          inst.Manifest.Version,
		Trigger:          trigger.Type,
		StartedAt:        r.now(),
		ExitCode:         -1,
		Enforcement:      r.enf,
		Sandbox:          r.sandbox.Name(),
		AccessLogDurable: Durable(r.opts.AccessLog),
	}

	if err := r.CanRun(inst); err != nil {
		inv.Outcome, inv.Error, inv.EndedAt = OutcomeRefused, err.Error(), r.now()
		return inv, err
	}

	limits := r.opts.Limits.withDefaults()
	limits.WallClock = inst.Timeout(limits.WallClock)

	obs := &observer{}
	host, err := r.newHost(inst, inv.ID, obs)
	if err != nil {
		inv.Outcome, inv.Error, inv.EndedAt = OutcomeRefused, err.Error(), r.now()
		return inv, err
	}

	if !r.opts.KeepScratch {
		if err := resetDir(inst.Layout.Scratch); err != nil {
			inv.Outcome, inv.Error, inv.EndedAt = OutcomeRefused, err.Error(), r.now()
			return inv, err
		}
	}

	// Two pipes rather than one socket: the strongest sandbox has no network.
	hostR, childW, err := os.Pipe()
	if err != nil {
		return inv, err
	}
	childR, hostW, err := os.Pipe()
	if err != nil {
		hostR.Close()
		childW.Close()
		return inv, err
	}
	defer hostR.Close()
	defer hostW.Close()

	// stdout and stderr are the app's *log*, never a protocol. Nothing in this
	// package parses them for meaning: the capability channel is on fds 3 and 4
	// and it is structured, which is the same rule the five agent adapters
	// follow for the same reason.
	stdout := &lineWriter{emit: func(s string) { host.emitLog(ctx, "stdout", "info", s, nil) }}
	stderr := &lineWriter{emit: func(s string) { host.emitLog(ctx, "stderr", "warn", s, nil) }}

	spec := Spec{
		Path:     r.node,
		Args:     r.nodeArgs(inst, limits),
		Env:      r.env(inst),
		Dir:      inst.Layout.Scratch,
		ReadOnly: []string{inst.Layout.Root, filepath.Dir(r.runner)},
		Writable: []string{inst.Layout.Scratch},
		Files:    []*os.File{childR, childW},
		Stdout:   stdout,
		Stderr:   stderr,
		Limits:   limits,
	}

	proc, err := r.sandbox.Start(ctx, spec)
	childR.Close()
	childW.Close()
	if err != nil {
		inv.Outcome, inv.Error, inv.EndedAt = OutcomeCrashed, err.Error(), r.now()
		return inv, err
	}
	if proc.limiter != nil {
		inv.Enforcement = r.compose(r.sandbox.Enforcement(), proc.limiter)
	}

	runCtx, cancel := context.WithTimeout(ctx, limits.WallClock)
	defer cancel()

	// The supervisor. An app that hangs is killed, not left holding the box —
	// and the kill goes to the process group, so nothing it started outlives it.
	var timedOut bool
	var supervisor sync.WaitGroup
	supervisor.Add(1)
	go func() {
		defer supervisor.Done()
		select {
		case <-proc.Done():
		case <-runCtx.Done():
			if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
				timedOut = true
			}
			proc.Terminate(limits.Grace)
		}
	}()

	start := startFrame{
		T: frameStart,
		App: appIdentity{
			ID: inst.Manifest.ID, Name: inst.Manifest.Name, Version: inst.Manifest.Version,
		},
		Trigger:      trigger,
		Capabilities: Capabilities(inst.Granted),
		Granted:      inst.Granted,
		Declined:     inst.Declined(),
		DeadlineMs:   limits.WallClock.Milliseconds(),
		Entry:        inst.Entry,
	}

	serveErr := host.Serve(runCtx, hostR, hostW, start)
	// Closing the host's end tells the runner nothing more is coming, which is
	// how an app that finished without exiting gets to exit.
	hostW.Close()

	waitErr := proc.Wait()
	cancel()
	supervisor.Wait()
	stdout.flush()
	stderr.flush()

	inv.EndedAt = r.now()
	inv.ExitCode = proc.ExitCode()
	inv.Calls, inv.Refused, inv.FailedCalls, inv.LogsDropped = host.Counts()
	inv.Spoken, inv.Output = obs.snapshot()

	switch {
	case timedOut:
		inv.Outcome = OutcomeTimeout
		inv.Error = fmt.Sprintf("this app was still running after %s, so it was stopped", limits.WallClock)
	case host.AppError() != nil:
		inv.Outcome = OutcomeFailed
		inv.Error = host.AppError().Error()
	case serveErr != nil && !errors.Is(serveErr, context.Canceled):
		inv.Outcome = OutcomeCrashed
		inv.Error = serveErr.Error()
	case !host.Finished():
		inv.Outcome = OutcomeCrashed
		inv.Error = "the app process exited without finishing"
		if waitErr != nil {
			inv.Error += ": " + waitErr.Error()
		}
	default:
		inv.Outcome = OutcomeCompleted
	}

	if inv.Outcome != OutcomeCompleted {
		r.log.Info("apps: invocation did not complete",
			"app", inv.AppID, "outcome", inv.Outcome, "err", inv.Error, "exit", inv.ExitCode)
	}
	return inv, nil
}

func (r *Runtime) newHost(inst Installed, invocation string, obs *observer) (*Host, error) {
	o := HostOptions{
		Installed: inst, Invocation: invocation,
		Redact: r.opts.Redact, Log: tee(r.opts.LogSink, obs), Now: r.now,
	}

	if inst.Has(ScopeMemoryRead) || inst.Has(ScopeMemoryWrite) {
		if r.opts.Source != nil {
			m, err := NewMemory(MemoryOptions{
				Source: r.opts.Source, Sink: r.opts.Sink, Log: r.opts.AccessLog,
				Redact: r.opts.Redact, AppID: inst.Manifest.ID, Invocation: invocation, Now: r.now,
			})
			if err != nil {
				return nil, err
			}
			o.Memory = m
		}
	}
	if inst.Has(ScopeGlassesSpeaker) || inst.Has(ScopeGlassesCamera) || inst.Has(ScopeGlassesAudio) {
		if r.opts.Device != nil && r.opts.Indicator != nil {
			g, err := NewGlasses(GlassesOptions{
				Device: observedDevice{Device: r.opts.Device, obs: obs}, Indicator: r.opts.Indicator,
				AppID: inst.Manifest.ID, AppName: inst.Manifest.Name, Redact: r.opts.Redact,
			})
			if err != nil {
				return nil, err
			}
			o.Glasses = g
		}
	}
	if inst.Has(ScopeAgentSession) && r.opts.Agent != nil {
		a, err := NewAgent(r.opts.Agent, inst.Manifest.ID, r.opts.Redact)
		if err != nil {
			return nil, err
		}
		o.Agent = a
	}
	if inst.Has(ScopeNetFetch) {
		f, err := NewFetcher(FetchOptions{
			Guard: NewGuard(inst.AllowedHosts()), Log: r.opts.EgressLog, Client: r.opts.FetchClient,
			AppID: inst.Manifest.ID, Invocation: invocation, Now: r.now,
		})
		if err != nil {
			return nil, err
		}
		o.Fetch = f
	}
	// `ui` costs no scope, so unlike every branch above there is nothing to test
	// the grant against — the capability is minted for every app. What it may
	// draw is still narrowed by what was granted: a `speak` block is checked
	// against inst.Granted per block, inside the capability.
	ui, err := NewUI(UIOptions{
		Screen: r.opts.Screen, AppID: inst.Manifest.ID, AppName: inst.Manifest.Name,
		Granted: inst.Granted, Redact: r.opts.Redact,
	})
	if err != nil {
		return nil, err
	}
	o.UI = ui

	o.Storage = r.opts.Storage
	if o.Storage == nil && inst.Layout.Data != "" {
		s, err := NewFileStorage(inst.Layout.Data)
		if err != nil {
			return nil, err
		}
		o.Storage = s
	}
	return NewHost(o)
}

// nodeArgs is where the filesystem boundary is actually drawn.
//
// `--permission` denies `fs`, `child_process`, native addons and worker threads
// outright; the `--allow-fs-read` entries re-open the app's own root, the
// directory the runner lives in, and the scratch — an app has to be able to read
// back what it just wrote — and `--allow-fs-write` re-opens the scratch alone.
//
// There is no entry for the agent's workspace, for another app's directory, or
// for the app's own `data` directory: `ctx.storage` is served by the host over
// the capability channel precisely so that it does not need one, which is what
// makes "private to this app" a statement about paths that do not exist rather
// than about paths the app is asked not to use.
func (r *Runtime) nodeArgs(inst Installed, l Limits) []string {
	runner := resolveLinks(r.runner)
	return []string{
		"--permission",
		"--allow-fs-read=" + filepath.Dir(runner),
		"--allow-fs-read=" + inst.Layout.Root,
		"--allow-fs-read=" + inst.Layout.Scratch,
		"--allow-fs-write=" + inst.Layout.Scratch,
		fmt.Sprintf("--max-old-space-size=%d", l.HeapMB()),
		"--no-warnings",
		"--disable-proto=throw",
		runner,
	}
}

// resolveLinks canonicalises a path for Node's permission model.
//
// Node resolves a path to its real one before testing it against the grants,
// and it does that for the main module too — before any of our code runs. So a
// grant naming a path that passes through a symlink never matches the path Node
// actually checks, and the app dies with ERR_ACCESS_DENIED naming a directory
// nobody wrote down.
//
// This is macOS, specifically: /var is a symlink to /private/var, so every
// path under t.TempDir() and under the default data directory goes through one.
// On Linux the paths are already real, which is why every one of these tests
// passed in the container this package was written in and none of them ran here.
//
// A path that does not exist yet resolves as far as it can and keeps the rest:
// scratch is created per invocation and may legitimately be absent at the moment
// the arguments are built, and a grant is still needed for it.
func resolveLinks(p string) string {
	if p == "" {
		return p
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	dir, base := filepath.Split(p)
	parent := filepath.Clean(dir)
	if base == "" || parent == p || parent == "." || parent == string(filepath.Separator) {
		return p
	}
	return filepath.Join(resolveLinks(parent), base)
}

// env is built from nothing rather than inherited.
//
// relayd's own environment holds the user's API keys, their model configuration
// and the path to their database. Passing it on and trusting the app not to read
// `process.env` would make every one of those a capability nobody granted — so
// the app gets four variables, all of them about itself.
func (r *Runtime) env(inst Installed) []string {
	return []string{
		"HOME=" + inst.Layout.Scratch,
		"TMPDIR=" + inst.Layout.Scratch,
		"NODE_ENV=production",
		"RELAY_APP_ID=" + inst.Manifest.ID,
	}
}

func resetDir(dir string) error {
	if dir == "" {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("apps: clear scratch: %w", err)
	}
	return os.MkdirAll(dir, 0o700)
}

// observer collects the two things an invocation record needs that the sinks
// alone cannot give it: what the app said out loud, and the tail of its log.
//
// Both are capped. An app that logs a megabyte a second must not turn every
// [Invocation] into a megabyte; the [LogSink] is where the whole stream goes,
// and this is the part a console or an agent reads inline.
type observer struct {
	mu     sync.Mutex
	spoken []string
	output []string
	bytes  int
}

// Caps on what one invocation record carries inline.
const (
	MaxRecordedLines = 32
	MaxRecordedBytes = 4096
)

func (o *observer) AppLogged(_ context.Context, l LogLine) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.output) >= MaxRecordedLines || o.bytes >= MaxRecordedBytes {
		return nil
	}
	o.output = append(o.output, l.Message)
	o.bytes += len(l.Message)
	return nil
}

func (o *observer) said(text string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.spoken) >= MaxRecordedLines {
		return
	}
	o.spoken = append(o.spoken, text)
}

func (o *observer) snapshot() (spoken, output []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.spoken...), append([]string(nil), o.output...)
}

// observedDevice records what was actually spoken — after the device accepted
// it, never before. A record of what the user heard must not include the
// sentence that failed on the way to the speaker.
type observedDevice struct {
	Device
	obs *observer
}

func (d observedDevice) Say(ctx context.Context, text string) error {
	if err := d.Device.Say(ctx, text); err != nil {
		return err
	}
	d.obs.said(text)
	return nil
}

// tee sends every line to both sinks. The real sink's error is the one that
// counts: a line the observer dropped because the record is full is not a lost
// line, and counting it as one would make [Invocation.LogsDropped] wrong.
func tee(real, obs LogSink) LogSink {
	if real == nil {
		return obs
	}
	return LogSinkFunc(func(ctx context.Context, l LogLine) error {
		_ = obs.AppLogged(ctx, l)
		return real.AppLogged(ctx, l)
	})
}

// lineWriter turns a byte stream into whole lines. It is used only for the app's
// stdout and stderr, which are treated as log output and never parsed for
// meaning.
type lineWriter struct {
	mu   sync.Mutex
	buf  []byte
	emit func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := indexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:i]), "\r")
		w.buf = w.buf[i+1:]
		if strings.TrimSpace(line) != "" {
			w.emit(line)
		}
	}
	if len(w.buf) > MaxFrameBytes {
		w.buf = w.buf[:0]
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s := strings.TrimSpace(string(w.buf)); s != "" {
		w.emit(s)
	}
	w.buf = nil
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// InstallSDK copies the `@relay/sdk` stub into an app's root, where Node's
// resolver finds it. It is called by [Runtime.Install] and exported so a CLI can
// do the same thing against an already-installed app.
func InstallSDK(sdkDir, appRoot string) error {
	dst := filepath.Join(appRoot, "node_modules", "@relay", "sdk")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	for _, name := range []string{"index.js", "package.json"} {
		b, err := os.ReadFile(filepath.Join(sdkDir, name))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, name), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// Install provisions an app and gives it the SDK stub, then freezes the root.
//
// It is [Install] plus the one thing only the runtime knows: where the SDK stub
// lives. Callers that do not have a runtime can use the package-level [Install]
// and ship their own `node_modules`.
func (r *Runtime) Install(m Manifest, c Consent, source string, o InstallOptions) (Installed, error) {
	tmp, err := os.MkdirTemp("", "relay-app-*")
	if err != nil {
		return Installed{}, err
	}
	defer os.RemoveAll(tmp)

	staged := filepath.Join(tmp, "pkg")
	if err := copyTree(source, staged); err != nil {
		return Installed{}, fmt.Errorf("apps: stage app package: %w", err)
	}
	if _, err := os.Stat(filepath.Join(staged, "node_modules", "@relay", "sdk")); errors.Is(err, os.ErrNotExist) {
		if err := InstallSDK(r.sdkDir, staged); err != nil {
			return Installed{}, fmt.Errorf("apps: install the SDK stub: %w", err)
		}
	}
	if o.Now == nil {
		o.Now = r.now
	}
	return Install(m, c, staged, o)
}

// InstallFromDir reads relay.json out of a package directory and installs it.
func (r *Runtime) InstallFromDir(source string, c Consent, o InstallOptions) (Installed, error) {
	b, err := os.ReadFile(filepath.Join(source, "relay.json"))
	if err != nil {
		return Installed{}, fmt.Errorf("apps: read relay.json: %w", err)
	}
	m, err := ParseManifest(b)
	if err != nil {
		return Installed{}, err
	}
	return r.Install(m, c, source, o)
}

// ReadManifest parses a package's relay.json.
func ReadManifest(dir string) (Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "relay.json"))
	if err != nil {
		return Manifest{}, fmt.Errorf("apps: read relay.json: %w", err)
	}
	return ParseManifest(b)
}
