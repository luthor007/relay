// Package backfill reads the history that already exists on the machine.
//
// MEMORY.md §1 measured a real Mac before any Relay code existed: **3.6 GB of
// history exists before we ship anything**. The first run of the installer is
// not a cold start, it is an archaeology job — and that is the best thing about
// this product's first five minutes, because it can know the user's stack
// before they have said a word to it.
//
// Three measurements shape every decision in this package, and all three are
// counter-intuitive enough to be worth restating:
//
//   - **The corpus is wildly lopsided.** Hermes was 2.5 GB and 27 sessions,
//     Claude Code 786 MB, Codex 295 MB, OpenCode 11 MB, OpenClaw nothing at
//     all. One runtime is 70% of it. Anything that assumes five equal peers is
//     wrong, which is why [Report] carries per-runtime totals rather than one
//     number and why Hermes and Claude Code were built first.
//   - **Two of five runtimes had no data at all.** "Installed but never used"
//     and "not installed" are the normal cases. A missing store is
//     [StoreAbsent] — success, zero sessions — never an error. The failure
//     worth fearing is the opposite one: reporting an empty history as success
//     when we simply failed to look in the right place, which is [StoreUnreadable].
//   - **Summarising it all is an hour or two.** So backfill is incremental and
//     resumable, keyed on (runtime, session_id, mtime) per MEMORY.md §4, and it
//     runs *after* the pairing code prints — nobody should watch a progress bar
//     before their glasses work.
//
// # What a reader may and may not do
//
// A reader parses a documented store format. It never scrapes an agent's
// terminal output, and it never invents a field: where a runtime does not
// record cost, [index.Session.CostUSD] stays nil rather than becoming zero.
// Everything a reader had to derive rather than read — a workspace decoded from
// a directory slug, a title taken from the first user message — is recorded in
// Notes and in [index.Session.TitleSource], because a derived value presented
// as a read one is the same class of lie as an adapter emitting an event it
// cannot observe.
//
// # Two phases, because 3.6 GB
//
// [Reader.Scan] enumerates sessions cheaply — a stat per file, or one small
// query — and [Reader.Read] parses one. The split is what makes resume real:
// the runner asks "has this changed?" from the Scan result alone and skips
// without opening the transcript at all.
package backfill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/detect"
	"github.com/luthor007/relay/relayd/internal/index"
)

// Session is the record a reader produces. Aliased from the index package so
// readers and the indexer cannot drift apart, and so backfill → index is the
// only direction the dependency runs.
type Session = index.Session

// DefaultMaxTextBytes caps how much message text one session contributes to
// detection and, later, summarisation.
//
// It is a real limit with a real cost, so it is visible rather than silent:
// when a reader stops here it sets TextTruncated and adds a note saying how
// much was not scanned. 32 MB is comfortably above any session seen in the §1
// measurement while keeping one session in memory at a time.
const DefaultMaxTextBytes = 32 << 20

// Env is the machine, as far as backfill is concerned.
//
// Unlike internal/detect this package reads real files: transcripts are
// gigabytes and streaming them through an in-memory FS seam would be a lie
// about what backfill does. What is injectable is where to look (Home, Getenv,
// Dir overrides) and how to ask a runtime a question (Exec), which is enough to
// drive every reader from fixtures.
type Env struct {
	// Home is the user's home directory. Every default store path hangs off it.
	Home string

	// Getenv reads the environment. Never os.Getenv directly: CLAUDE_CONFIG_DIR,
	// CODEX_HOME and OPENCLAW_STATE_DIR each move a store, and a test has to be
	// able to move them.
	Getenv func(string) string

	// Exec runs a runtime's own CLI, for the two stores that are only reachable
	// that way: `opencode export` and `openclaw config file`.
	Exec detect.Exec

	// GOOS is the platform, so a darwin-only path can be tested on linux.
	GOOS string

	// Now is injectable for deterministic tests.
	Now func() time.Time

	// OpenClawProfile and OpenClawDev relocate OpenClaw's state directory the
	// same way `--profile <name>` and `--dev` do.
	OpenClawProfile string
	OpenClawDev     bool

	// MaxTextBytes overrides DefaultMaxTextBytes.
	MaxTextBytes int64
}

// OSEnv returns an Env wired to the real machine.
func OSEnv() Env {
	home, _ := os.UserHomeDir()
	return Env{
		Home:   home,
		Getenv: os.Getenv,
		Exec:   detect.OS().Exec,
		GOOS:   runtime.GOOS,
		Now:    time.Now,
	}
}

func (e Env) getenv(k string) string {
	if e.Getenv == nil {
		return ""
	}
	return strings.TrimSpace(e.Getenv(k))
}

func (e Env) now() time.Time {
	if e.Now == nil {
		return time.Now()
	}
	return e.Now()
}

func (e Env) maxText() int64 {
	if e.MaxTextBytes > 0 {
		return e.MaxTextBytes
	}
	return DefaultMaxTextBytes
}

// expand resolves ~ and $HOME against this Env's home.
func (e Env) expand(p string) string {
	switch {
	case p == "":
		return ""
	case p == "~":
		return e.Home
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(e.Home, p[2:])
	case strings.HasPrefix(p, "$HOME/"):
		return filepath.Join(e.Home, p[6:])
	}
	return p
}

// detectEnv adapts this Env for internal/detect, which owns the one piece of
// path resolution too subtle to duplicate: OpenClaw's relocatable state dir.
func (e Env) detectEnv() detect.Env {
	d := detect.OS()
	d.Home = e.Home
	if e.Getenv != nil {
		d.Getenv = e.Getenv
	}
	if e.Exec != nil {
		d.Exec = e.Exec
	}
	if e.GOOS != "" {
		d.GOOS = e.GOOS
	}
	return d
}

// StoreStatus is what we found where a store should be. The distinction
// between the last two is the one that matters: MEMORY.md §1 measured two
// runtimes with no history at all, so an empty result is usually the truth —
// but a reader that could not read a store it did find must say so, because
// "no sessions" and "we could not look" lead to opposite decisions and only
// one of them is recoverable.
type StoreStatus string

const (
	// StoreAbsent: no store directory, no database, nothing. Success, zero
	// sessions. OpenClaw's directory did not exist at all on the test machine.
	StoreAbsent StoreStatus = "absent"
	// StoreEmpty: the store is there and holds no sessions. OpenCode: 11 MB,
	// installed, never run.
	StoreEmpty StoreStatus = "empty"
	// StoreOK: sessions found.
	StoreOK StoreStatus = "ok"
	// StoreUnreadable: the store exists and we could not read it. Never
	// reported as an empty history.
	StoreUnreadable StoreStatus = "unreadable"
)

// Ref locates one session without having parsed it.
type Ref struct {
	Runtime   adapter.Runtime
	SessionID string

	// Path is the file the session lives in. ByteOffset locates it inside that
	// file when the file holds more than one session; ByteLength is 0 when the
	// session runs to the end.
	Path       string
	ByteOffset int64
	ByteLength int64

	// MTime and Size are the resume key, with (runtime, session_id).
	//
	// For a one-session-per-file store they are the file's mtime and size. For
	// a store that holds many sessions in one file or one database they are the
	// session's own last activity and its message count, because the file's
	// mtime moves for every session at once and would re-index the whole store
	// on every run. MTimeFrom says which of those happened.
	MTime     time.Time
	Size      int64
	MTimeFrom string

	// Title and StartedAt are hints from a store's own index, when it has one.
	// They are not authoritative — Read fills the real values.
	Title     string
	StartedAt time.Time
}

// ScanResult is one reader's enumeration pass.
type ScanResult struct {
	Runtime adapter.Runtime
	Status  StoreStatus

	// Refs is every session found, oldest first where the store gives us an
	// order to work with.
	Refs []Ref

	// Roots are the directories and files we looked in — printed when the
	// status is StoreAbsent or StoreUnreadable, so "nothing found" always comes
	// with "and here is where we looked".
	Roots []string

	// Notes carry anything the installer should say out loud, including every
	// case where a path was assumed rather than confirmed.
	Notes []string

	// Err is set only for StoreUnreadable.
	Err error
}

func (s *ScanResult) note(format string, args ...any) {
	s.Notes = append(s.Notes, fmt.Sprintf(format, args...))
}

// Reader reads one runtime's session store.
type Reader interface {
	// Runtime is which of the five this reads.
	Runtime() adapter.Runtime

	// Scan enumerates sessions without parsing them. A missing store is not an
	// error: it returns StoreAbsent and no refs.
	Scan(ctx context.Context) (ScanResult, error)

	// Read parses one session. The returned Session carries transient text for
	// detection and summarisation; nothing persists it.
	Read(ctx context.Context, ref Ref) (Session, error)
}

// Readers builds one reader per runtime, in the order MEMORY.md §11 implies:
// Hermes and Claude Code first, because they were 85% of the measured corpus.
func Readers(env Env) []Reader {
	return []Reader{
		NewHermes(env),
		NewClaudeCode(env),
		NewCodex(env),
		NewOpenCode(env),
		NewOpenClaw(env),
	}
}

// ---------------------------------------------------------------- helpers --

// textBuilder accumulates message text up to a limit, and remembers that it
// stopped.
type textBuilder struct {
	b         strings.Builder
	limit     int64
	n         int64
	skipped   int64
	truncated bool
}

func newTextBuilder(limit int64) *textBuilder { return &textBuilder{limit: limit} }

func (t *textBuilder) add(role, s string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return
	}
	line := s
	if role != "" {
		line = role + ": " + s
	}
	if t.n+int64(len(line)) > t.limit {
		t.truncated = true
		t.skipped += int64(len(line))
		return
	}
	if t.b.Len() > 0 {
		t.b.WriteByte('\n')
		t.n++
	}
	t.b.WriteString(line)
	t.n += int64(len(line))
}

func (t *textBuilder) String() string { return t.b.String() }

// titleFrom derives a one-line title from a message body. It is never
// presented as a runtime-generated title — see index.TitleFirstMessage.
func titleFrom(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	const max = 80
	if len(s) <= max {
		return s
	}
	cut := strings.LastIndex(strings.ToValidUTF8(s[:max], ""), " ")
	if cut < max/2 {
		cut = max
	}
	return strings.TrimSpace(strings.ToValidUTF8(s[:cut], "")) + "…"
}

func dirExists(p string) bool {
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func float64p(v float64) *float64 { return &v }
func int64p(v int64) *int64       { return &v }
