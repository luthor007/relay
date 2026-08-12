package detect

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The three seams. Everything in this package reaches the machine through one
// of them, so the whole detection flow runs from fixtures in a test on a box
// with none of the five runtimes installed — which is the only box CI will ever
// have.

// FS is the read side of the filesystem.
type FS interface {
	Stat(name string) (fs.FileInfo, error)
	ReadFile(name string) ([]byte, error)
	ReadDir(name string) ([]fs.DirEntry, error)
}

// WriteFS adds the writes the installer needs: config files, service units and
// the rollback copies of every runtime config we touch.
type WriteFS interface {
	FS
	MkdirAll(dir string, perm fs.FileMode) error
	WriteFile(name string, data []byte, perm fs.FileMode) error
	Remove(name string) error
}

// Cmd is one command to run.
type Cmd struct {
	Name string
	Args []string
	Env  []string
	Dir  string
	// Stdin is fed to the command when non-empty.
	Stdin []byte
	// Timeout defaults to CommandTimeout.
	Timeout time.Duration
}

// Result is what a command said.
type Result struct {
	Stdout []byte
	Stderr []byte
	Code   int
}

// Out is stdout with surrounding whitespace removed.
func (r Result) Out() string { return strings.TrimSpace(string(r.Stdout)) }

// Err is stderr with surrounding whitespace removed.
func (r Result) Err() string { return strings.TrimSpace(string(r.Stderr)) }

// Exec is the process seam.
type Exec interface {
	// LookPath resolves a binary on PATH. A miss returns an error, and a miss
	// is the normal case — MEMORY.md §1 measured two of five runtimes absent on
	// a real machine.
	LookPath(file string) (string, error)
	Run(ctx context.Context, c Cmd) (Result, error)
}

// Process is one entry of the process table.
type Process struct {
	PID     int
	Command string
	Args    string
}

// ProcessTable lists what is running. ORCHESTRATOR.md §2 asks for three
// signals — binaries on PATH, config directories, and running processes — and
// this is the third.
//
// Reading `ps` is not the thing ADAPTERS.md forbids. The rule there is that no
// *agent event* may come from scraped terminal output; the process table is a
// machine-readable OS interface that happens to have a text rendering, and
// nothing in it is ever turned into an event.
type ProcessTable interface {
	List(ctx context.Context) ([]Process, error)
}

// Env is the machine, as far as this package is concerned.
type Env struct {
	FS    FS
	Exec  Exec
	Procs ProcessTable

	// HTTP is the fourth seam, and it exists for exactly one thing: the local
	// embedding runtime is a *service*, so the only honest test of whether it is
	// up is a request to it (see ollama.go). Nil means a default client, and a
	// test supplies a RoundTripper so this package still makes no network call.
	//
	// Nothing else here reaches the network, and nothing else should: detection
	// of the five agent runtimes is a filesystem and process-table job.
	HTTP *http.Client

	// Getenv reads the environment. Never os.Getenv directly: OPENCLAW_STATE_DIR
	// and CODEX_HOME both move a store, and a test has to be able to move them.
	Getenv func(string) string

	// Home is the user's home directory.
	Home string

	// GOOS is the platform, so a darwin-only path can be tested on linux.
	GOOS string
}

// CommandTimeout caps a probe command. `claude --version` on a cold npm shim
// can take a second; ten is generous and still bounded.
const CommandTimeout = 10 * time.Second

// OS returns an Env wired to the real machine.
func OS() Env {
	home, _ := os.UserHomeDir()
	return Env{
		FS:     osFS{},
		Exec:   osExec{},
		Procs:  psTable{},
		HTTP:   &http.Client{Timeout: ServiceTimeout},
		Getenv: os.Getenv,
		Home:   home,
		GOOS:   runtime.GOOS,
	}
}

// Expand resolves ~ and returns an absolute path under this Env's home.
func (e Env) Expand(p string) string {
	if p == "" {
		return ""
	}
	if p == "~" {
		return e.Home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(e.Home, p[2:])
	}
	if strings.HasPrefix(p, "$HOME/") {
		return filepath.Join(e.Home, p[6:])
	}
	return p
}

func (e Env) getenv(k string) string {
	if e.Getenv == nil {
		return ""
	}
	return strings.TrimSpace(e.Getenv(k))
}

// dirExists reports whether a path exists and is a directory.
func (e Env) dirExists(p string) bool {
	if p == "" || e.FS == nil {
		return false
	}
	fi, err := e.FS.Stat(p)
	return err == nil && fi.IsDir()
}

// fileExists reports whether a path exists and is a regular file.
func (e Env) fileExists(p string) bool {
	if p == "" || e.FS == nil {
		return false
	}
	fi, err := e.FS.Stat(p)
	return err == nil && !fi.IsDir()
}

// ---------------------------------------------------------------- OS backing

type osFS struct{}

func (osFS) Stat(name string) (fs.FileInfo, error)      { return os.Stat(name) }
func (osFS) ReadFile(name string) ([]byte, error)       { return os.ReadFile(name) }
func (osFS) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }

// OSWriteFS is the real filesystem, write side included.
type OSWriteFS struct{ osFS }

func (OSWriteFS) MkdirAll(dir string, perm fs.FileMode) error { return os.MkdirAll(dir, perm) }
func (OSWriteFS) WriteFile(name string, data []byte, perm fs.FileMode) error {
	return os.WriteFile(name, data, perm)
}
func (OSWriteFS) Remove(name string) error { return os.Remove(name) }

type osExec struct{}

func (osExec) LookPath(file string) (string, error) { return exec.LookPath(file) }

func (osExec) Run(ctx context.Context, c Cmd) (Result, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = CommandTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	if len(c.Env) > 0 {
		cmd.Env = append(os.Environ(), c.Env...)
	}
	if len(c.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb

	err := cmd.Run()
	res := Result{Stdout: out.Bytes(), Stderr: errb.Bytes()}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.Code = ee.ExitCode()
		return res, nil // a non-zero exit is an answer, not a failure to ask
	}
	if err != nil {
		return res, err
	}
	return res, nil
}

// psTable reads the process table with ps, which exists on both platforms we
// register a boot service for.
type psTable struct{}

func (psTable) List(ctx context.Context) ([]Process, error) {
	res, err := osExec{}.Run(ctx, Cmd{Name: "ps", Args: []string{"-Ao", "pid=,comm=,args="}})
	if err != nil {
		return nil, err
	}
	return ParsePS(string(res.Stdout)), nil
}

// ParsePS turns `ps -Ao pid=,comm=,args=` output into processes. Exported
// because it is the one piece of psTable worth testing without a machine.
func ParsePS(out string) []Process {
	var procs []Process
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pidStr, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			continue
		}
		rest = strings.TrimSpace(rest)
		comm, args, ok := strings.Cut(rest, " ")
		if !ok {
			args = rest
		}
		procs = append(procs, Process{
			PID:     pid,
			Command: filepath.Base(comm),
			Args:    strings.TrimSpace(args),
		})
	}
	return procs
}

// ---------------------------------------------------------------- fixtures

// MemFS is an in-memory filesystem keyed by absolute path. Directories are
// implied by the files under them, plus anything named in Dirs — which is how a
// fixture expresses "installed but never run": the state directory exists and
// holds nothing.
type MemFS struct {
	Files map[string]string
	Dirs  []string
	// Fail maps a path onto the error Stat and ReadFile return for it, so a
	// permission error is testable.
	Fail map[string]error
}

func (m *MemFS) file(name string) (string, bool) {
	if m.Files == nil {
		return "", false
	}
	v, ok := m.Files[path.Clean(name)]
	return v, ok
}

func (m *MemFS) isDir(name string) bool {
	name = path.Clean(name)
	for _, d := range m.Dirs {
		if path.Clean(d) == name {
			return true
		}
	}
	prefix := name + "/"
	for k := range m.Files {
		if strings.HasPrefix(path.Clean(k), prefix) {
			return true
		}
	}
	for _, d := range m.Dirs {
		if strings.HasPrefix(path.Clean(d), prefix) {
			return true
		}
	}
	return false
}

func (m *MemFS) Stat(name string) (fs.FileInfo, error) {
	name = path.Clean(name)
	if err, ok := m.Fail[name]; ok {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: err}
	}
	if v, ok := m.file(name); ok {
		return memInfo{name: path.Base(name), size: int64(len(v))}, nil
	}
	if m.isDir(name) {
		return memInfo{name: path.Base(name), dir: true}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
}

func (m *MemFS) ReadFile(name string) ([]byte, error) {
	name = path.Clean(name)
	if err, ok := m.Fail[name]; ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	if v, ok := m.file(name); ok {
		return []byte(v), nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

func (m *MemFS) ReadDir(name string) ([]fs.DirEntry, error) {
	name = path.Clean(name)
	if err, ok := m.Fail[name]; ok {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: err}
	}
	if !m.isDir(name) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	prefix := name + "/"
	if name == "/" {
		prefix = "/"
	}
	seen := map[string]memInfo{}
	consider := func(p string, size int64, isFile bool) {
		p = path.Clean(p)
		if !strings.HasPrefix(p, prefix) {
			return
		}
		rest := strings.TrimPrefix(p, prefix)
		if rest == "" {
			return
		}
		base, _, nested := strings.Cut(rest, "/")
		if nested {
			seen[base] = memInfo{name: base, dir: true}
			return
		}
		if isFile {
			seen[base] = memInfo{name: base, size: size}
		} else if _, ok := seen[base]; !ok {
			seen[base] = memInfo{name: base, dir: true}
		}
	}
	for k, v := range m.Files {
		consider(k, int64(len(v)), true)
	}
	for _, d := range m.Dirs {
		consider(d, 0, false)
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]fs.DirEntry, 0, len(names))
	for _, n := range names {
		out = append(out, seen[n])
	}
	return out, nil
}

// MkdirAll records a directory.
func (m *MemFS) MkdirAll(dir string, _ fs.FileMode) error {
	m.Dirs = append(m.Dirs, path.Clean(dir))
	return nil
}

// WriteFile stores a file.
func (m *MemFS) WriteFile(name string, data []byte, _ fs.FileMode) error {
	if m.Files == nil {
		m.Files = map[string]string{}
	}
	m.Files[path.Clean(name)] = string(data)
	return nil
}

// Remove deletes a file.
func (m *MemFS) Remove(name string) error {
	delete(m.Files, path.Clean(name))
	return nil
}

type memInfo struct {
	name string
	size int64
	dir  bool
}

func (i memInfo) Name() string { return i.name }
func (i memInfo) Size() int64  { return i.size }
func (i memInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}
func (i memInfo) ModTime() time.Time         { return time.Time{} }
func (i memInfo) IsDir() bool                { return i.dir }
func (i memInfo) Sys() any                   { return nil }
func (i memInfo) Type() fs.FileMode          { return i.Mode().Type() }
func (i memInfo) Info() (fs.FileInfo, error) { return i, nil }

// FakeExec answers from a script. A command with no scripted answer is a
// command that is not installed, which is the case worth defaulting to.
type FakeExec struct {
	// Paths maps a binary name onto its resolved path. Absent means not on PATH.
	Paths map[string]string
	// Responses is keyed by the whole command line, "codex --version".
	Responses map[string]Result
	// Errors is keyed the same way, for a command that could not be run at all.
	Errors map[string]error
	// Calls records every command in order.
	Calls []Cmd
}

// Key is the Responses/Errors key for a command.
func Key(name string, args ...string) string {
	if len(args) == 0 {
		return name
	}
	return name + " " + strings.Join(args, " ")
}

func (f *FakeExec) LookPath(file string) (string, error) {
	if p, ok := f.Paths[file]; ok {
		return p, nil
	}
	return "", exec.ErrNotFound
}

func (f *FakeExec) Run(_ context.Context, c Cmd) (Result, error) {
	f.Calls = append(f.Calls, c)
	k := Key(c.Name, c.Args...)
	if err, ok := f.Errors[k]; ok {
		return Result{}, err
	}
	if r, ok := f.Responses[k]; ok {
		return r, nil
	}
	// Unscripted: exit 127, the shell's "no such command". Never a zero exit
	// with empty output, which a caller would read as "it worked and said
	// nothing".
	return Result{Code: 127, Stderr: []byte("command not found: " + c.Name)}, nil
}

// FakeProcs is a canned process table.
type FakeProcs []Process

func (f FakeProcs) List(context.Context) ([]Process, error) { return []Process(f), nil }
