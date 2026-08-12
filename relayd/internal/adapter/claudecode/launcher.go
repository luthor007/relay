package claudecode

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

// The process, behind an interface.
//
// No agent runtimes exist in the build container — there is no claude binary —
// so every test in this package drives a Process that replays the vendored
// 49-event fixture and records what was written to stdin. That is the whole
// reason the interface exists: the decoder, the normalizer, the turn
// correlation and the permission path are all exercised for real, and only the
// fork/exec is stubbed.

// LaunchSpec is one process to start.
type LaunchSpec struct {
	Binary string
	Args   []string
	Dir    string
	// Env is the complete environment, in K=V form. Empty inherits ours.
	Env []string
}

// Process is a running runtime.
type Process interface {
	// Stdin is where turns are injected.
	Stdin() io.WriteCloser
	// Stdout carries the NDJSON event stream.
	Stdout() io.Reader
	// Stderr is diagnostics only. Nothing in Relay parses it: ADAPTERS.md §1,
	// if you find yourself reading prose you are on the wrong path.
	Stderr() io.Reader
	// Wait blocks until the process exits and returns its exit error.
	Wait() error
	// Kill ends it. It must make Stdout return EOF.
	Kill() error
}

// Launcher starts processes.
type Launcher interface {
	Launch(ctx context.Context, spec LaunchSpec) (Process, error)
}

// ExecLauncher runs a real binary.
type ExecLauncher struct{}

var _ Launcher = ExecLauncher{}

func (ExecLauncher) Launch(ctx context.Context, spec LaunchSpec) (Process, error) {
	bin := spec.Binary
	if bin == "" {
		bin = DefaultBinary
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("claudecode: %s is not installed or not on PATH: %w", bin, err)
	}
	cmd := exec.Command(path, spec.Args...)
	cmd.Dir = spec.Dir
	if len(spec.Env) > 0 {
		cmd.Env = spec.Env
	} else {
		cmd.Env = os.Environ()
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("claudecode: could not start %s: %w", path, err)
	}
	return &execProcess{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr}, nil
}

type execProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	once sync.Once
}

func (p *execProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *execProcess) Stdout() io.Reader     { return p.stdout }
func (p *execProcess) Stderr() io.Reader     { return p.stderr }
func (p *execProcess) Wait() error           { return p.cmd.Wait() }

func (p *execProcess) Kill() error {
	var err error
	p.once.Do(func() {
		_ = p.stdin.Close()
		if p.cmd.Process != nil {
			err = p.cmd.Process.Kill()
		}
	})
	return err
}

// ringBuffer keeps the last n bytes of stderr, so a process that dies can say
// why without an unbounded buffer in a long-lived daemon.
type ringBuffer struct {
	mu  sync.Mutex
	buf []byte
	n   int
}

func newRingBuffer(n int) *ringBuffer { return &ringBuffer{n: n} }

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.n {
		r.buf = append([]byte(nil), r.buf[len(r.buf)-r.n:]...)
	}
	return len(p), nil
}

func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}
