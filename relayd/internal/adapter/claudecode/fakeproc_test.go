package claudecode

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
)

// scriptedProcess is a Claude Code that only exists in this test binary.
//
// No agent runtime is installed in the build container, so the only honest way
// to exercise the decoder, the turn correlation and the permission path is to
// replay the vendored 49-event trace through them. Emission is *gated on
// stdin*: a chunk of the recording goes out only after a turn has been written,
// which is exactly the real ordering — the runtime says nothing until it is
// asked something — and it makes the whole test deterministic without a sleep.
type scriptedProcess struct {
	pr *io.PipeReader
	pw *io.PipeWriter

	mu     sync.Mutex
	input  []string
	chunks [][]byte
	next   int

	feed     chan []byte
	feedOnce sync.Once
	exited   chan struct{}
	stderr   io.Reader
	waitErr  error
}

type scriptOptions struct {
	// Chunks are emitted one per line written to stdin.
	Chunks [][]byte
	// Prelude goes out immediately, before any input.
	Prelude []byte
	// Stderr is what the process wrote to stderr.
	Stderr string
	// WaitErr is what Wait reports, so an exit-code path is testable.
	WaitErr error
	// KeepOpen leaves the process running after the last chunk instead of
	// closing stdout.
	KeepOpen bool
}

func newScriptedProcess(o scriptOptions) *scriptedProcess {
	pr, pw := io.Pipe()
	p := &scriptedProcess{
		pr:      pr,
		pw:      pw,
		chunks:  o.Chunks,
		feed:    make(chan []byte, 64),
		exited:  make(chan struct{}),
		stderr:  strings.NewReader(o.Stderr),
		waitErr: o.WaitErr,
	}
	keepOpen := o.KeepOpen
	go func() {
		for b := range p.feed {
			if _, err := p.pw.Write(b); err != nil {
				break
			}
		}
		_ = p.pw.Close()
		close(p.exited)
	}()
	if len(o.Prelude) > 0 {
		p.feed <- o.Prelude
	}
	if len(o.Chunks) == 0 && !keepOpen {
		p.closeFeed()
	}
	return p
}

func (p *scriptedProcess) closeFeed() { p.feedOnce.Do(func() { close(p.feed) }) }

func (p *scriptedProcess) Stdin() io.WriteCloser { return scriptStdin{p} }
func (p *scriptedProcess) Stdout() io.Reader     { return p.pr }
func (p *scriptedProcess) Stderr() io.Reader     { return p.stderr }

func (p *scriptedProcess) Wait() error {
	<-p.exited
	return p.waitErr
}

func (p *scriptedProcess) Kill() error {
	p.closeFeed()
	return nil
}

// Input returns every line written to stdin, newline stripped.
func (p *scriptedProcess) Input() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.input...)
}

type scriptStdin struct{ p *scriptedProcess }

func (w scriptStdin) Close() error { return nil }

func (w scriptStdin) Write(b []byte) (int, error) {
	p := w.p
	for _, line := range bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		p.mu.Lock()
		p.input = append(p.input, string(line))
		var chunk []byte
		last := false
		if p.next < len(p.chunks) {
			chunk = p.chunks[p.next]
			p.next++
			last = p.next == len(p.chunks)
		}
		p.mu.Unlock()

		if chunk != nil {
			p.feed <- chunk
			if last {
				p.closeFeed()
			}
		}
	}
	return len(b), nil
}

// scriptLauncher hands out one prepared process.
type scriptLauncher struct {
	proc *scriptedProcess
	err  error

	mu   sync.Mutex
	spec LaunchSpec
}

func (l *scriptLauncher) Launch(_ context.Context, spec LaunchSpec) (Process, error) {
	l.mu.Lock()
	l.spec = spec
	l.mu.Unlock()
	if l.err != nil {
		return nil, l.err
	}
	return l.proc, nil
}

func (l *scriptLauncher) Spec() LaunchSpec {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.spec
}
