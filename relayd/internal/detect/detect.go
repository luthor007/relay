// Package detect answers the installer's first question: what is already on
// this machine?
//
// ORCHESTRATOR.md §2 step 1 asks for three signals per runtime — a binary on
// PATH, a config or state directory, and a running process. MEMORY.md §1 is the
// reality to design against, measured on a real machine before any Relay code
// existed:
//
//	Hermes       2.5 GB   27 sessions
//	Claude Code  786 MB   one JSONL per session
//	Codex        295 MB   session_index.jsonl + rollouts
//	OpenCode      11 MB   zero sessions — installed, never run
//	OpenClaw          —   ~/.openclaw does not exist — installed, never run
//
// Three consequences, and every one of them is a rule here:
//
//   - **A missing store is not an error.** Two of five had no data at all.
//     "Installed but never used" and "not installed" are the normal cases, and
//     a detector that reports either as a failure has mis-modelled the world.
//   - **The corpus is lopsided.** One runtime was 70% of it. [Report.Dominant]
//     exists so the installer can say so instead of implying five equal peers.
//   - **Never hardcode ~/.openclaw.** OPENCLAW_STATE_DIR, --profile <name> and
//     --dev all relocate it, the session store path is itself configurable in
//     the gateway config, and the directory does not exist until the gateway
//     has run once. Resolve it by asking. A reader that assumes the default
//     silently finds nothing and reports an empty history as success, which is
//     the worst failure available here because it looks like a clean install.
//
// Everything reaches the machine through the seams in env.go, so the whole flow
// runs from fixtures on a box with none of the five installed.
package detect

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
)

// Status is what we concluded about one runtime. The four values exist because
// MEMORY.md §1 found three of them on one machine on one afternoon.
type Status string

const (
	// StatusAbsent: no binary, no store. Nothing to do but offer to install it.
	StatusAbsent Status = "absent"
	// StatusNeverRun: the binary is there and the store is missing or empty.
	// Normal, not an error — this was two of five runtimes on the test machine.
	StatusNeverRun Status = "never_run"
	// StatusInUse: binary and history both present. This is where the value is.
	StatusInUse Status = "in_use"
	// StatusHistoryOnly: a store with sessions in it and no binary on PATH. The
	// binary was removed, or lives somewhere PATH does not reach. Worth saying
	// out loud, because the history is still ours to index.
	StatusHistoryOnly Status = "history_only"
)

func (s Status) String() string { return string(s) }

// Line is the one-clause rendering the installer prints.
func (s Status) Line() string {
	switch s {
	case StatusAbsent:
		return "not installed"
	case StatusNeverRun:
		return "installed, never run"
	case StatusInUse:
		return "installed, in use"
	case StatusHistoryOnly:
		return "history on disk, binary not on PATH"
	}
	return string(s)
}

// StateSource records how we found a state directory, which matters more than
// the path itself. A path we were told is a fact; a path we assumed is a guess,
// and the OpenClaw trap is exactly what happens when the two are conflated.
type StateSource string

const (
	SourceNone StateSource = ""
	// SourceEnv: an environment variable named it.
	SourceEnv StateSource = "env"
	// SourceAsked: the runtime told us, e.g. `openclaw config file`.
	SourceAsked StateSource = "asked"
	// SourceConfig: read out of the runtime's own config file.
	SourceConfig StateSource = "config"
	// SourceProfile: derived from a --profile or --dev the caller passed in.
	SourceProfile StateSource = "profile"
	// SourceDefault: the documented default. A guess until something confirms
	// it, and reported as one.
	SourceDefault StateSource = "default"
)

// Trusted reports whether the path came from something authoritative rather
// than from an assumption.
func (s StateSource) Trusted() bool {
	switch s {
	case SourceEnv, SourceAsked, SourceConfig, SourceProfile:
		return true
	}
	return false
}

// Finding is everything detection concluded about one runtime.
//
// Two fields are pointers on purpose, matching the house rule that anything we
// might not be able to observe is nil rather than zero: a nil Sessions means
// nobody counted, and 0 means counted and empty. Rendering "0 sessions" for a
// store we never opened is the same class of lie as an adapter emitting an
// event it did not see.
type Finding struct {
	Runtime adapter.Runtime
	Label   string

	// BinaryName is what we looked for on PATH; BinaryPath is where it was.
	BinaryName string
	BinaryPath string
	Installed  bool

	// Version is the runtime's own report. VersionNote says why it is empty.
	Version     string
	VersionNote string

	// StateDir is where this runtime keeps its sessions, and Source says how we
	// know. StateDirDetail names the env var, config key or command.
	StateDir       string
	StateDirSource StateSource
	StateDirDetail string
	StateDirExists bool

	// Candidates are the paths we looked at and rejected, so a user whose store
	// is somewhere else can see that we looked.
	Candidates []string

	// Sessions is a count where counting is cheap and honest. nil means not
	// counted; SessionsNote says why.
	Sessions     *int
	SessionsNote string

	// StoreBytes is the size of the store on disk, nil when unmeasured. This is
	// MEMORY.md §1's own unit, and it is what makes the lopsided corpus visible
	// before anything is parsed.
	StoreBytes *int64

	// Running is every process on the machine that looks like this runtime.
	Running []Process

	// Notes carry anything the installer should say out loud.
	Notes []string
}

// Status classifies the finding.
func (f Finding) Status() Status {
	sessions := 0
	if f.Sessions != nil {
		sessions = *f.Sessions
	}
	hasHistory := f.StateDirExists && (sessions > 0 || (f.Sessions == nil && f.StoreBytes != nil && *f.StoreBytes > 0))
	switch {
	case f.Installed && hasHistory:
		return StatusInUse
	case f.Installed:
		return StatusNeverRun
	case hasHistory:
		return StatusHistoryOnly
	default:
		return StatusAbsent
	}
}

// SessionCount returns the count and whether it is known.
func (f Finding) SessionCount() (int, bool) {
	if f.Sessions == nil {
		return 0, false
	}
	return *f.Sessions, true
}

// Bytes returns the store size and whether it is known.
func (f Finding) Bytes() (int64, bool) {
	if f.StoreBytes == nil {
		return 0, false
	}
	return *f.StoreBytes, true
}

func (f *Finding) note(format string, args ...any) {
	f.Notes = append(f.Notes, fmt.Sprintf(format, args...))
}

// Report is one whole pass over the machine.
type Report struct {
	At       time.Time
	GOOS     string
	Findings []Finding
}

// Get returns the finding for a runtime.
func (r Report) Get(rt adapter.Runtime) (Finding, bool) {
	for _, f := range r.Findings {
		if f.Runtime == rt {
			return f, true
		}
	}
	return Finding{}, false
}

// Installed lists the runtimes with a binary on PATH.
func (r Report) Installed() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Installed {
			out = append(out, f)
		}
	}
	return out
}

// Missing lists the runtimes with no binary and no history — the ones step 2 of
// the installer offers to install.
func (r Report) Missing() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Status() == StatusAbsent {
			out = append(out, f)
		}
	}
	return out
}

// WithHistory lists the runtimes that have something for backfill to read,
// installed or not.
func (r Report) WithHistory() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		switch f.Status() {
		case StatusInUse, StatusHistoryOnly:
			out = append(out, f)
		}
	}
	return out
}

// Dominant returns the runtime holding the largest share of the corpus, and
// that share. MEMORY.md §1: one runtime was 70% of 3.6 GB, and any design that
// assumes an even distribution across five runtimes will be wrong. ok is false
// when nothing measurable was found.
func (r Report) Dominant() (adapter.Runtime, float64, bool) {
	var total int64
	var best Finding
	var bestBytes int64
	for _, f := range r.Findings {
		b, ok := f.Bytes()
		if !ok {
			continue
		}
		total += b
		if b > bestBytes {
			bestBytes, best = b, f
		}
	}
	if total == 0 {
		return "", 0, false
	}
	return best.Runtime, float64(bestBytes) / float64(total), true
}

// TotalBytes is the size of the whole corpus we can see.
func (r Report) TotalBytes() int64 {
	var total int64
	for _, f := range r.Findings {
		if b, ok := f.Bytes(); ok {
			total += b
		}
	}
	return total
}

// Summary is the sentence the installer prints above the table.
func (r Report) Summary() string {
	var used, present, absent int
	for _, f := range r.Findings {
		switch f.Status() {
		case StatusInUse, StatusHistoryOnly:
			used++
		case StatusNeverRun:
			present++
		default:
			absent++
		}
	}
	parts := []string{fmt.Sprintf("%d of %d agent runtimes have history here", used, len(r.Findings))}
	if present > 0 {
		parts = append(parts, fmt.Sprintf("%d installed but never run", present))
	}
	if absent > 0 {
		parts = append(parts, fmt.Sprintf("%d not installed", absent))
	}
	s := strings.Join(parts, ", ") + "."
	if rt, share, ok := r.Dominant(); ok && share > 0.5 {
		s += fmt.Sprintf(" %s is %.0f%% of it.", rt, share*100)
	}
	return s
}

// Options tunes a detection pass.
type Options struct {
	// Only restricts the pass to these runtimes. Empty means all five.
	Only []adapter.Runtime

	// OpenClawProfile is the --profile <name> the user runs OpenClaw with, if
	// any: it relocates the state dir to ~/.openclaw-<name>.
	OpenClawProfile string
	// OpenClawDev is `openclaw --dev`, which relocates it to ~/.openclaw-dev.
	OpenClawDev bool

	// SkipProcesses skips the process table, which is the slowest signal.
	SkipProcesses bool
	// SkipSizes skips walking the stores for their size. The walk is stat-only
	// and fast, but it is the one part that scales with 3.6 GB of history.
	SkipSizes bool
}

func (o Options) wants(rt adapter.Runtime) bool {
	if len(o.Only) == 0 {
		return true
	}
	for _, r := range o.Only {
		if r == rt {
			return true
		}
	}
	return false
}

// detector is one runtime's probe.
type detector func(ctx context.Context, env Env, opts Options) Finding

func detectors() []struct {
	rt adapter.Runtime
	fn detector
} {
	return []struct {
		rt adapter.Runtime
		fn detector
	}{
		{adapter.ClaudeCode, detectClaudeCode},
		{adapter.Codex, detectCodex},
		{adapter.Hermes, detectHermes},
		{adapter.OpenCode, detectOpenCode},
		{adapter.OpenClaw, detectOpenClaw},
	}
}

// Detect runs one pass over the machine.
//
// It never returns an error. Every failure it can have — a binary that is not
// there, a directory it cannot read, a runtime that will not say its version —
// is a fact about the machine and belongs in the report, not in an error the
// installer would have to decide how to survive.
func Detect(ctx context.Context, env Env, opts Options) Report {
	rep := Report{At: time.Now(), GOOS: env.GOOS}

	var procs []Process
	if !opts.SkipProcesses && env.Procs != nil {
		if p, err := env.Procs.List(ctx); err == nil {
			procs = p
		}
	}

	for _, d := range detectors() {
		if !opts.wants(d.rt) {
			continue
		}
		f := d.fn(ctx, env, opts)
		f.Running = matchProcesses(procs, f.BinaryName)
		rep.Findings = append(rep.Findings, f)
	}
	return rep
}

// matchProcesses finds running processes for a binary name. It matches the
// command name exactly and the argv only on a word boundary, because "codex"
// must not match a shell editing codex.md.
func matchProcesses(procs []Process, binary string) []Process {
	if binary == "" {
		return nil
	}
	var out []Process
	for _, p := range procs {
		if p.Command == binary {
			out = append(out, p)
			continue
		}
		for _, field := range strings.Fields(p.Args) {
			if field == binary || strings.HasSuffix(field, "/"+binary) {
				out = append(out, p)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// ---------------------------------------------------------------- helpers

func intp(v int) *int       { return &v }
func int64p(v int64) *int64 { return &v }

// lookBinary resolves a binary and records the result on the finding.
func lookBinary(env Env, f *Finding, name string) {
	f.BinaryName = name
	if env.Exec == nil {
		return
	}
	if p, err := env.Exec.LookPath(name); err == nil {
		f.Installed = true
		f.BinaryPath = p
	}
}

// askVersion asks a runtime its version. A runtime that will not answer is
// recorded as unknown rather than guessed at.
func askVersion(ctx context.Context, env Env, f *Finding, args ...string) {
	if !f.Installed || env.Exec == nil {
		return
	}
	res, err := env.Exec.Run(ctx, Cmd{Name: f.BinaryName, Args: args})
	if err != nil {
		f.VersionNote = "could not run " + f.BinaryName + " " + strings.Join(args, " ") + ": " + err.Error()
		return
	}
	if res.Code != 0 {
		f.VersionNote = fmt.Sprintf("%s %s exited %d", f.BinaryName, strings.Join(args, " "), res.Code)
		return
	}
	v := firstLine(res.Out())
	if v == "" {
		f.VersionNote = f.BinaryName + " reported no version"
		return
	}
	f.Version = v
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// maxWalk bounds a store walk. 3.6 GB of history is tens of thousands of files
// and a stat each is fast, but an unbounded walk over a symlink loop is not.
const maxWalk = 250_000

// walkSize sums the size of every regular file under dir, and counts files
// matching suffix. It is stat-only: nothing is opened and nothing is parsed.
func walkSize(env Env, dir, suffix string) (bytes int64, matches int, ok bool) {
	if !env.dirExists(dir) {
		return 0, 0, false
	}
	seen := 0
	var walk func(string, int)
	walk = func(d string, depth int) {
		if depth > 12 || seen > maxWalk {
			return
		}
		entries, err := env.FS.ReadDir(d)
		if err != nil {
			return
		}
		for _, e := range entries {
			seen++
			if seen > maxWalk {
				return
			}
			p := joinPath(d, e.Name())
			if e.IsDir() {
				walk(p, depth+1)
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			bytes += info.Size()
			if suffix != "" && strings.HasSuffix(e.Name(), suffix) {
				matches++
			}
		}
	}
	walk(dir, 0)
	return bytes, matches, true
}

// joinPath joins with forward slashes. Detection paths are unix paths on both
// platforms we support, and using path rather than filepath keeps MemFS and the
// real filesystem in agreement.
func joinPath(parts ...string) string {
	out := strings.Join(parts, "/")
	for strings.Contains(out, "//") {
		out = strings.ReplaceAll(out, "//", "/")
	}
	return out
}

// countLines counts non-empty lines in a file, for the JSONL indexes.
func countLines(env Env, p string) (int, bool) {
	b, err := env.FS.ReadFile(p)
	if err != nil {
		return 0, false
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n, true
}
