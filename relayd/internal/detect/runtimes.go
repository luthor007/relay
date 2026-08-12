package detect

import (
	"context"

	"github.com/luthor007/relay/relayd/internal/adapter"
)

// One detector per runtime, because the five differ in exactly the ways that
// matter. A single table-driven probe would have to pretend they are the same
// shape, and the OpenClaw case is the proof that they are not.

// detectClaudeCode finds Claude Code.
//
// Store: ~/.claude/projects/<slug>/<uuid>.jsonl, one JSONL per session, which
// makes the count a file count and not a parse. CLAUDE_CONFIG_DIR relocates the
// whole config directory.
func detectClaudeCode(ctx context.Context, env Env, _ Options) Finding {
	f := Finding{Runtime: adapter.ClaudeCode, Label: "Claude Code"}
	lookBinary(env, &f, "claude")
	askVersion(ctx, env, &f, "--version")

	if v := env.getenv("CLAUDE_CONFIG_DIR"); v != "" {
		f.StateDir = env.Expand(v)
		f.StateDirSource = SourceEnv
		f.StateDirDetail = "CLAUDE_CONFIG_DIR"
	} else {
		f.StateDir = joinPath(env.Home, ".claude")
		f.StateDirSource = SourceDefault
		f.StateDirDetail = "~/.claude, the documented default; CLAUDE_CONFIG_DIR moves it"
	}
	f.Candidates = []string{f.StateDir}
	f.StateDirExists = env.dirExists(f.StateDir)

	projects := joinPath(f.StateDir, "projects")
	if env.dirExists(projects) {
		bytes, n, _ := walkSize(env, projects, ".jsonl")
		f.Sessions = intp(n)
		f.StoreBytes = int64p(bytes)
		f.SessionsNote = "one JSONL per session under projects/<slug>/"
	} else if f.StateDirExists {
		f.Sessions = intp(0)
		f.SessionsNote = "the config directory exists but has no projects/ — installed, never run in a repo"
	} else {
		f.SessionsNote = "no config directory yet"
	}

	if f.Installed && f.Status() == StatusNeverRun {
		f.note("Claude Code is here but has no transcripts yet. Nothing to import, and nothing wrong.")
	}
	return f
}

// detectCodex finds Codex. CODEX_HOME relocates ~/.codex, and the rollouts are
// indexed by session_index.jsonl, so the count is a line count of the index
// rather than a walk of 295 MB.
func detectCodex(ctx context.Context, env Env, _ Options) Finding {
	f := Finding{Runtime: adapter.Codex, Label: "Codex"}
	lookBinary(env, &f, "codex")
	askVersion(ctx, env, &f, "--version")

	if v := env.getenv("CODEX_HOME"); v != "" {
		f.StateDir = env.Expand(v)
		f.StateDirSource = SourceEnv
		f.StateDirDetail = "CODEX_HOME"
	} else {
		f.StateDir = joinPath(env.Home, ".codex")
		f.StateDirSource = SourceDefault
		f.StateDirDetail = "~/.codex, the documented default; CODEX_HOME moves it"
	}
	f.Candidates = []string{f.StateDir}
	f.StateDirExists = env.dirExists(f.StateDir)

	index := joinPath(f.StateDir, "session_index.jsonl")
	if n, ok := countLines(env, index); ok {
		f.Sessions = intp(n)
		f.SessionsNote = "session_index.jsonl, one line per rollout"
	} else if f.StateDirExists {
		f.SessionsNote = "no session_index.jsonl; rollouts may still exist under sessions/"
	} else {
		f.SessionsNote = "no state directory yet"
	}

	if sessions := joinPath(f.StateDir, "sessions"); env.dirExists(sessions) {
		bytes, rollouts, _ := walkSize(env, sessions, ".jsonl")
		f.StoreBytes = int64p(bytes)
		if f.Sessions == nil {
			f.Sessions = intp(rollouts)
			f.SessionsNote = "counted rollout files under sessions/, because there is no index"
		}
	} else if f.StateDirExists && f.Sessions == nil {
		f.Sessions = intp(0)
	}
	return f
}

// detectHermes finds Hermes.
//
// Hermes was 2.5 GB and 70% of the corpus on the test machine, so it is the one
// runtime whose absence would change the install materially. Its store is a
// SQLite database, and counting sessions means opening it — which backfill
// does, deliberately, and the installer does not. Reporting a session count
// here would mean either opening a database mid-install or guessing, and the
// house rule says report what you observed: the file, and its size.
func detectHermes(ctx context.Context, env Env, _ Options) Finding {
	f := Finding{Runtime: adapter.Hermes, Label: "Hermes"}
	lookBinary(env, &f, "hermes")
	askVersion(ctx, env, &f, "--version")

	if v := env.getenv("HERMES_STATE_DIR"); v != "" {
		f.StateDir = env.Expand(v)
		f.StateDirSource = SourceEnv
		f.StateDirDetail = "HERMES_STATE_DIR"
	} else {
		f.StateDir = joinPath(env.Home, ".hermes")
		f.StateDirSource = SourceDefault
		f.StateDirDetail = "~/.hermes, where MEMORY.md §4 found state.db; no relocation env var has been probed"
	}
	f.Candidates = []string{f.StateDir}
	f.StateDirExists = env.dirExists(f.StateDir)

	db := joinPath(f.StateDir, "state.db")
	if env.fileExists(db) {
		bytes, _, _ := walkSize(env, f.StateDir, "")
		f.StoreBytes = int64p(bytes)
		f.SessionsNote = "sessions live in state.db (SQLite + FTS5); counting them means opening it, which backfill does and the installer does not"
	} else if f.StateDirExists {
		f.Sessions = intp(0)
		f.SessionsNote = "state directory exists but has no state.db"
	} else {
		f.SessionsNote = "no state directory yet"
	}
	return f
}

// detectOpenCode finds OpenCode. It was the 11 MB / zero sessions row on the
// test machine: installed, never run.
func detectOpenCode(ctx context.Context, env Env, _ Options) Finding {
	f := Finding{Runtime: adapter.OpenCode, Label: "OpenCode"}
	lookBinary(env, &f, "opencode")
	askVersion(ctx, env, &f, "--version")

	// No relocation variable has been probed, so every candidate below is a
	// guess and is labelled as one. The first that exists wins.
	candidates := []string{}
	if v := env.getenv("OPENCODE_DATA"); v != "" {
		candidates = append(candidates, env.Expand(v))
	}
	if v := env.getenv("XDG_DATA_HOME"); v != "" {
		candidates = append(candidates, joinPath(env.Expand(v), "opencode"))
	}
	candidates = append(candidates,
		joinPath(env.Home, ".local", "share", "opencode"),
		joinPath(env.Home, ".opencode"),
	)
	f.Candidates = candidates

	for i, c := range candidates {
		if env.dirExists(c) {
			f.StateDir = c
			f.StateDirExists = true
			if i == 0 && env.getenv("OPENCODE_DATA") != "" {
				f.StateDirSource, f.StateDirDetail = SourceEnv, "OPENCODE_DATA"
			} else {
				f.StateDirSource, f.StateDirDetail = SourceDefault, "matched a documented default; OpenCode's data directory has not been probed"
			}
			break
		}
	}
	if f.StateDir == "" {
		f.StateDir = candidates[len(candidates)-1]
		f.StateDirSource = SourceDefault
		f.StateDirDetail = "none of the candidate directories exist"
	}

	if f.StateDirExists {
		bytes, _, _ := walkSize(env, f.StateDir, "")
		f.StoreBytes = int64p(bytes)
		f.SessionsNote = "sessions are read with `opencode export <id>`, which backfill drives; --sanitize redacts secrets and file data"
	} else {
		f.SessionsNote = "no data directory yet"
	}
	if f.Installed && !f.StateDirExists {
		f.note("OpenCode is installed and has never been run. That was one of the two such runtimes MEMORY.md §1 measured, and it is not a problem.")
	}
	return f
}

// detectOpenClaw finds OpenClaw, which is the one that punishes assumptions.
// The work is in openclaw.go.
func detectOpenClaw(ctx context.Context, env Env, opts Options) Finding {
	f := Finding{Runtime: adapter.OpenClaw, Label: "OpenClaw"}
	lookBinary(env, &f, "openclaw")
	askVersion(ctx, env, &f, "--version")

	res := ResolveOpenClawState(ctx, env, opts)
	f.StateDir = res.Dir
	f.StateDirSource = res.Source
	f.StateDirDetail = res.Detail
	f.StateDirExists = res.Exists
	f.Candidates = res.Candidates
	f.Notes = append(f.Notes, res.Notes...)

	if !res.Exists {
		f.SessionsNote = "the state directory does not exist yet; OpenClaw creates it the first time the gateway runs"
		if f.Installed {
			f.note("Installed, never run. This is exactly the case MEMORY.md §1 measured, and an empty history here is the truth rather than a failed read.")
		}
		return f
	}

	bytes, _, _ := walkSize(env, res.Dir, "")
	f.StoreBytes = int64p(bytes)

	n, counted := countOpenClawSessions(env, res.SessionStores)
	if counted {
		f.Sessions = intp(n)
		f.SessionsNote = "counted across agents/<agent>/sessions/sessions.json"
	} else {
		f.SessionsNote = "no agents/<agent>/sessions/sessions.json found under the state directory"
	}
	return f
}
