package apps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// The sandbox, behind an interface, with the strongest implementation the
// platform actually offers — and a value that says which one that was.
//
// APP-PLATFORM.md §5 asks for a container per app. This box has no container
// runtime it can rely on: relayd is one static binary a self-hoster drops on a
// machine, and the machine may or may not have Docker, may or may not have
// cgroup v2 delegated, may or may not permit unprivileged user namespaces. A
// package that assumed any of those would be a package whose safety story is
// "it worked on the developer's laptop".
//
// So isolation is probed, not assumed. [NewSandbox] runs a real probe process at
// startup and reports back an [Enforcement] describing what it actually got. The
// rule the rest of the package follows from there is the one thing that must
// never bend: **a declared boundary is never described as an enforced one.**

// Control is how strongly one boundary holds.
type Control string

const (
	// ControlEnforced means something outside the app's reach stops it: a
	// kernel namespace, an rlimit, a supervisor holding a signal.
	ControlEnforced Control = "enforced"
	// ControlPartial means a real mechanism stops part of it, and the note says
	// which part. It is not a softer word for enforced — it is the honest state
	// of, say, a memory cap that binds the JS heap and not a native allocation.
	ControlPartial Control = "partial"
	// ControlDeclared means nothing stops it. The manifest says so, the code
	// says so, and a determined app does what it likes.
	ControlDeclared Control = "declared"
)

// Rank orders the three, weakest first, so [Enforcement.Weakest] can be a fold.
func (c Control) Rank() int {
	switch c {
	case ControlEnforced:
		return 2
	case ControlPartial:
		return 1
	default:
		return 0
	}
}

// Guarantee is one boundary: how strongly it holds, what holds it, and exactly
// how far that goes.
type Guarantee struct {
	Control Control `json:"control"`
	// By names the mechanism — "linux network namespace", "node --permission",
	// "prlimit RLIMIT_CPU", "supervisor". A guarantee with no mechanism is a
	// declaration, and [Declared] is the only way to build one.
	By string `json:"by,omitempty"`
	// Note is the sentence a console shows. It says what is *not* covered, not
	// only what is.
	Note string `json:"note,omitempty"`
}

// Enforced builds an enforced guarantee.
func Enforced(by, note string) Guarantee {
	return Guarantee{Control: ControlEnforced, By: by, Note: note}
}

// Partial builds a partially enforced guarantee. The note must say which part.
func Partial(by, note string) Guarantee {
	return Guarantee{Control: ControlPartial, By: by, Note: note}
}

// Declared builds a guarantee that nothing enforces.
func Declared(note string) Guarantee { return Guarantee{Control: ControlDeclared, Note: note} }

func (g Guarantee) String() string {
	s := string(g.Control)
	if g.By != "" {
		s += " by " + g.By
	}
	if g.Note != "" {
		s += " — " + g.Note
	}
	return s
}

// Enforcement is the whole containment story for one runtime, as data.
type Enforcement struct {
	// Filesystem: can the app read or write outside its own root and scratch?
	Filesystem Guarantee `json:"filesystem"`
	// Network: can the app reach a host the manifest did not declare?
	Network Guarantee `json:"network"`
	// Processes: can the app see, signal or spawn anything of the host's?
	Processes Guarantee `json:"processes"`
	// User: does it run as somebody other than relayd?
	User Guarantee `json:"user"`
	// CPU: is there a ceiling on processor time?
	CPU Guarantee `json:"cpu"`
	// Memory: is there a ceiling on what it can allocate?
	Memory Guarantee `json:"memory"`
	// WallClock: is an app that hangs actually killed?
	WallClock Guarantee `json:"wallClock"`
	// FileSize: is there a ceiling on what it can write to scratch?
	FileSize Guarantee `json:"fileSize"`
}

// NamedGuarantee is one row of [Enforcement.Controls].
type NamedGuarantee struct {
	Name string `json:"name"`
	Guarantee
}

// Controls lists every boundary in a stable order, for the console and for the
// invocation record.
//
// A boundary nobody filled in comes back as [ControlDeclared] rather than as an
// empty string. That is the only safe default: an unset field means no code
// claimed the boundary, and "no claim" and "declared" are the same fact.
func (e Enforcement) Controls() []NamedGuarantee {
	out := []NamedGuarantee{
		{"filesystem", e.Filesystem},
		{"network", e.Network},
		{"processes", e.Processes},
		{"user", e.User},
		{"cpu", e.CPU},
		{"memory", e.Memory},
		{"wall-clock", e.WallClock},
		{"file-size", e.FileSize},
	}
	for i := range out {
		switch out[i].Control {
		case ControlEnforced, ControlPartial, ControlDeclared:
		default:
			out[i].Control = ControlDeclared
			out[i].By = ""
		}
	}
	return out
}

// Weakest is the weakest control in the set. A console that shows one badge
// shows this one, because a sandbox is as strong as its worst boundary.
func (e Enforcement) Weakest() Control {
	worst := ControlEnforced
	for _, c := range e.Controls() {
		if c.Control.Rank() < worst.Rank() {
			worst = c.Control
		}
	}
	return worst
}

// Declares lists the boundaries that are declared rather than enforced, for the
// sentence a console has to be able to write honestly.
func (e Enforcement) Declares() []string {
	var out []string
	for _, c := range e.Controls() {
		if c.Control == ControlDeclared {
			out = append(out, c.Name)
		}
	}
	sort.Strings(out)
	return out
}

// Spec is one process to run under the sandbox.
type Spec struct {
	// Path is the executable, absolute.
	Path string
	Args []string
	Env  []string
	// Dir is the working directory. The scratch, so a stray relative write lands
	// somewhere the app is allowed to write.
	Dir string
	// ReadOnly and Writable are the paths this process is meant to be able to
	// see. The sandbox uses them where it can confine paths itself; where it
	// cannot, they are passed to the runtime process (Node's permission model)
	// and the guarantee is reported accordingly.
	ReadOnly []string
	Writable []string
	// Files become fds 3, 4, … in the child. The capability channel travels this
	// way rather than over a socket precisely because the strongest sandbox has
	// no network at all.
	Files          []*os.File
	Stdout, Stderr io.Writer
	Limits         Limits
}

// Process is a started, contained process.
type Process struct {
	cmd     *exec.Cmd
	pid     int
	limiter *limitReport

	waitOnce sync.Once
	waitErr  error
	done     chan struct{}
	killed   atomic.Bool
}

// Pid is the process id in relayd's namespace.
func (p *Process) Pid() int { return p.pid }

// Wait blocks until the process exits. Safe to call from several goroutines.
func (p *Process) Wait() error {
	p.waitOnce.Do(func() {
		p.waitErr = p.cmd.Wait()
		close(p.done)
	})
	<-p.done
	return p.waitErr
}

// Done is closed when the process has exited.
func (p *Process) Done() <-chan struct{} { return p.done }

// Killed reports whether this package killed it, as opposed to it exiting.
func (p *Process) Killed() bool { return p.killed.Load() }

// ExitCode is the exit status, or -1 if it was signalled or has not exited.
func (p *Process) ExitCode() int {
	if p.cmd.ProcessState == nil {
		return -1
	}
	return p.cmd.ProcessState.ExitCode()
}

// Terminate asks the whole process group to stop, then kills it after grace.
//
// The group and not the process: an app that spawned something (which the
// runtime's permission model already refuses, but the sandbox does not depend on
// that) must not leave a child behind holding the box. That is the whole content
// of §5's "an app that hangs is killed, not left holding the box" — a kill that
// misses a child is a hang with extra steps.
func (p *Process) Terminate(grace time.Duration) {
	p.killed.Store(true)
	_ = signalGroup(p.pid, sigTerm)
	if grace <= 0 {
		grace = DefaultGrace
	}
	select {
	case <-p.done:
		return
	case <-time.After(grace):
	}
	_ = signalGroup(p.pid, sigKill)
}

// Kill stops the group now.
func (p *Process) Kill() {
	p.killed.Store(true)
	_ = signalGroup(p.pid, sigKill)
}

// Sandbox starts contained processes and says what it contains.
type Sandbox interface {
	// Name identifies the implementation: "linux-namespaces", "process-only".
	Name() string
	// Enforcement is what this sandbox actually enforces, measured at
	// construction rather than assumed from GOOS.
	Enforcement() Enforcement
	// Start launches one process. Resource limits are applied before it
	// returns, so a caller that hands the process its work afterwards knows the
	// caps were in place first.
	Start(ctx context.Context, s Spec) (*Process, error)
}

// SandboxOptions configures [NewSandbox].
type SandboxOptions struct {
	// Probe is the executable used to measure what the kernel allows. It should
	// be the same binary the sandbox will actually run — measuring with
	// /bin/true and running node is measuring the wrong thing.
	Probe string
	// ProbeArgs make the probe exit immediately. Defaults to --version.
	ProbeArgs []string
	// Disable turns namespace isolation off. For a machine where it is known
	// broken; it downgrades [Enforcement] honestly rather than pretending.
	Disable bool
}

// NewSandbox builds the strongest sandbox this machine offers, measuring rather
// than assuming.
func NewSandbox(ctx context.Context, o SandboxOptions) (Sandbox, error) {
	if o.Probe == "" {
		return nil, errors.New("apps: the sandbox needs the runtime binary to probe with")
	}
	if len(o.ProbeArgs) == 0 {
		o.ProbeArgs = []string{"--version"}
	}
	iso := isolationNone()
	if !o.Disable {
		iso = probeIsolation(ctx, o.Probe, o.ProbeArgs)
	}
	return &sandbox{iso: iso}, nil
}

type sandbox struct{ iso isolation }

func (s *sandbox) Name() string { return s.iso.name }

func (s *sandbox) Enforcement() Enforcement {
	e := s.iso.guarantees()
	// Two boundaries this layer never claims. The filesystem is confined by the
	// runtime process (see [Runtime]), and the wall clock by the supervisor;
	// both are filled in by whoever holds them, and until then they are
	// declared, which is the truth about a bare sandbox.
	e.Filesystem = Declared("the sandbox layer does not confine paths on this platform")
	e.WallClock = Declared("nothing is watching the clock at this layer")
	return e
}

func (s *sandbox) Start(ctx context.Context, spec Spec) (*Process, error) {
	if spec.Path == "" {
		return nil, errors.New("apps: nothing to run")
	}
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Env = spec.Env
	cmd.Dir = spec.Dir
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr
	cmd.ExtraFiles = spec.Files
	s.iso.apply(cmd)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("apps: start %s: %w", spec.Path, err)
	}
	p := &Process{cmd: cmd, pid: cmd.Process.Pid, done: make(chan struct{})}
	// Limits before the caller is told the process exists, so nothing has been
	// asked of it yet. The window between exec and here belongs to the runtime's
	// own startup, never to app code: the runner blocks for a start frame it
	// cannot receive until this function has returned.
	p.limiter = applyLimits(p.pid, spec.Limits)
	return p, nil
}

// limitReport is what applying the caps actually achieved.
type limitReport struct {
	CPU      Guarantee
	Memory   Guarantee
	FileSize Guarantee
}
