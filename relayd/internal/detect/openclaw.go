package detect

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// OpenClaw's state directory is the one place in this package where the
// obvious implementation is silently wrong.
//
// MEMORY.md §4: never hardcode ~/.openclaw. OPENCLAW_STATE_DIR, --profile
// <name> (→ ~/.openclaw-<name>) and --dev (→ ~/.openclaw-dev) all relocate it,
// the session store path is itself configurable in the gateway config, and the
// directory does not exist at all until the gateway has run once. A reader that
// assumes the default finds nothing and reports an empty history as success —
// which is worse than an error, because it looks like a clean install and the
// user never learns their 27 sessions were skipped.
//
// So: resolve it by asking. `openclaw config file` prints the config path, and
// the state directory is that file's parent. Everything else is a fallback, and
// every fallback says so in StateSource.

// OpenClawState is the result of resolving where OpenClaw keeps its things.
type OpenClawState struct {
	Dir    string
	Source StateSource
	Detail string
	Exists bool

	// ConfigPath is the gateway config, when we found one.
	ConfigPath string

	// SessionStores are the sessions.json files under the resolved directory.
	SessionStores []string

	// Candidates is every path considered, in the order considered.
	Candidates []string

	// Notes carry anything the installer should say out loud — in particular a
	// sibling ~/.openclaw-<profile> that exists when the resolved directory
	// does not.
	Notes []string
}

// sessionStoreKeys are the config keys that could point somewhere other than
// the state directory. None has been probed against a real gateway config, so a
// hit is reported with the key it came from rather than trusted silently.
var sessionStoreKeys = []string{
	"sessionStorePath", "session_store_path",
	"sessionsDir", "sessions_dir",
	"stateDir", "state_dir",
	"storagePath", "storage_path",
}

// ResolveOpenClawState finds OpenClaw's state directory the way MEMORY.md §4
// says to: ask first, assume last, and always report which one happened.
func ResolveOpenClawState(ctx context.Context, env Env, opts Options) OpenClawState {
	var st OpenClawState

	add := func(p string) {
		if p == "" {
			return
		}
		for _, c := range st.Candidates {
			if c == p {
				return
			}
		}
		st.Candidates = append(st.Candidates, p)
	}

	// 1. The environment variable wins outright. It is the documented override
	//    and it is unambiguous.
	if v := env.getenv("OPENCLAW_STATE_DIR"); v != "" {
		st.Dir = env.Expand(v)
		st.Source = SourceEnv
		st.Detail = "OPENCLAW_STATE_DIR"
		add(st.Dir)
	}

	// 2. A profile the caller told us about. --profile <name> relocates to
	//    ~/.openclaw-<name> and --dev to ~/.openclaw-dev.
	if st.Dir == "" {
		switch {
		case opts.OpenClawProfile != "":
			st.Dir = joinPath(env.Home, ".openclaw-"+opts.OpenClawProfile)
			st.Source = SourceProfile
			st.Detail = "--profile " + opts.OpenClawProfile
		case opts.OpenClawDev:
			st.Dir = joinPath(env.Home, ".openclaw-dev")
			st.Source = SourceProfile
			st.Detail = "--dev"
		}
		add(st.Dir)
	}

	// 3. Ask the binary. This is the step that makes the whole thing correct,
	//    and it is the step an implementation written from memory skips.
	if cfg, ok := askOpenClawConfigPath(ctx, env); ok {
		st.ConfigPath = cfg
		parent := parentDir(cfg)
		add(parent)
		if st.Dir == "" {
			st.Dir = parent
			st.Source = SourceAsked
			st.Detail = "`openclaw config file` printed " + cfg
		}
	}

	// 4. The config file may itself relocate the session store. Look for one
	//    even when nothing above resolved a directory: an unasked binary plus a
	//    config under the default path is still better evidence than the path.
	if st.ConfigPath == "" {
		probe := st.Dir
		if probe == "" {
			probe = joinPath(env.Home, ".openclaw")
		}
		for _, name := range []string{"openclaw.json", "config.json", "openclaw.yaml", "openclaw.yml", "config.yaml", "config.yml"} {
			p := joinPath(probe, name)
			if env.fileExists(p) {
				st.ConfigPath = p
				break
			}
		}
	}
	if st.ConfigPath != "" {
		if override, key, ok := openClawStoreOverride(env, st.ConfigPath); ok {
			resolved := env.Expand(override)
			if !strings.HasPrefix(resolved, "/") {
				resolved = joinPath(parentDir(st.ConfigPath), resolved)
			}
			add(resolved)
			st.Dir = resolved
			st.Source = SourceConfig
			st.Detail = fmt.Sprintf("%s in %s", key, st.ConfigPath)
			st.Notes = append(st.Notes, fmt.Sprintf(
				"OpenClaw's session store is relocated by %q in %s. The default would have been wrong here.",
				key, st.ConfigPath))
		}
	}

	// 5. Only now, the default — and it is labelled a guess.
	if st.Dir == "" {
		st.Dir = joinPath(env.Home, ".openclaw")
		st.Source = SourceDefault
		st.Detail = "~/.openclaw, assumed, because `openclaw config file` did not answer"
		add(st.Dir)
	}

	st.Exists = env.dirExists(st.Dir)

	// Whatever we picked, look for siblings. A ~/.openclaw-work sitting next to
	// an absent ~/.openclaw is the profile case in the flesh, and saying so is
	// the difference between "no history" and "your history is over there".
	siblings := openClawSiblings(env, st.Dir)
	for _, s := range siblings {
		add(s)
	}
	if !st.Exists && len(siblings) > 0 {
		st.Notes = append(st.Notes, fmt.Sprintf(
			"%s does not exist, but %s does. If you run OpenClaw with --profile or --dev, point the installer at it with OPENCLAW_STATE_DIR — otherwise its history will be skipped and the empty result will look like success.",
			st.Dir, strings.Join(siblings, ", ")))
	}
	if !st.Source.Trusted() && st.Exists {
		st.Notes = append(st.Notes, fmt.Sprintf(
			"%s was assumed, not confirmed: %s. Set OPENCLAW_STATE_DIR if OpenClaw runs under a profile.",
			st.Dir, st.Detail))
	}

	if st.Exists {
		st.SessionStores = findOpenClawSessionStores(env, st.Dir)
	}
	return st
}

// askOpenClawConfigPath runs `openclaw config file`, which prints the path of
// the config OpenClaw is actually using — profile, dev mode and all.
func askOpenClawConfigPath(ctx context.Context, env Env) (string, bool) {
	if env.Exec == nil {
		return "", false
	}
	if _, err := env.Exec.LookPath("openclaw"); err != nil {
		return "", false
	}
	res, err := env.Exec.Run(ctx, Cmd{Name: "openclaw", Args: []string{"config", "file"}})
	if err != nil || res.Code != 0 {
		return "", false
	}
	out := firstLine(res.Out())
	if out == "" || !strings.Contains(out, "/") {
		return "", false
	}
	return env.Expand(out), true
}

// openClawStoreOverride looks for a configured session store path. The config
// is JSON or YAML depending on version, so both are tried, and the search is by
// key name at any depth because the schema has not been probed.
func openClawStoreOverride(env Env, cfgPath string) (value, key string, ok bool) {
	b, err := env.FS.ReadFile(cfgPath)
	if err != nil {
		return "", "", false
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		if err := yaml.Unmarshal(b, &doc); err != nil {
			return "", "", false
		}
	}
	return findStringKey(doc, sessionStoreKeys, "")
}

// findStringKey walks a decoded document for the first of wanted keys holding a
// non-empty string, and returns the dotted path it was found at.
func findStringKey(doc any, wanted []string, prefix string) (value, key string, ok bool) {
	switch v := doc.(type) {
	case map[string]any:
		// Deterministic order, so two runs of the installer agree.
		names := make([]string, 0, len(v))
		for k := range v {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, name := range names {
			full := name
			if prefix != "" {
				full = prefix + "." + name
			}
			if s, isStr := v[name].(string); isStr && strings.TrimSpace(s) != "" {
				for _, w := range wanted {
					if strings.EqualFold(name, w) {
						return strings.TrimSpace(s), full, true
					}
				}
			}
		}
		for _, name := range names {
			full := name
			if prefix != "" {
				full = prefix + "." + name
			}
			if val, k, found := findStringKey(v[name], wanted, full); found {
				return val, k, true
			}
		}
	case []any:
		for i, item := range v {
			if val, k, found := findStringKey(item, wanted, fmt.Sprintf("%s[%d]", prefix, i)); found {
				return val, k, true
			}
		}
	}
	return "", "", false
}

// openClawSiblings lists ~/.openclaw-* directories other than the resolved one.
func openClawSiblings(env Env, resolved string) []string {
	entries, err := env.FS.ReadDir(env.Home)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasPrefix(n, ".openclaw") {
			continue
		}
		p := joinPath(env.Home, n)
		if p == resolved {
			continue
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// findOpenClawSessionStores finds every agents/<agent>/sessions/sessions.json
// under the state directory. The path shape is probed (ADAPTERS.md §4); the
// number of agents is not fixed.
func findOpenClawSessionStores(env Env, dir string) []string {
	agents := joinPath(dir, "agents")
	entries, err := env.FS.ReadDir(agents)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := joinPath(agents, e.Name(), "sessions", "sessions.json")
		if env.fileExists(p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// countOpenClawSessions counts entries across every sessions.json. The file is
// either a JSON array or an object keyed by session id depending on version, so
// both shapes count.
func countOpenClawSessions(env Env, stores []string) (int, bool) {
	if len(stores) == 0 {
		return 0, false
	}
	total, counted := 0, false
	for _, p := range stores {
		b, err := env.FS.ReadFile(p)
		if err != nil {
			continue
		}
		var doc any
		if err := json.Unmarshal(b, &doc); err != nil {
			continue
		}
		switch v := doc.(type) {
		case []any:
			total += len(v)
			counted = true
		case map[string]any:
			// {"sessions": [...]} or {"<id>": {...}}.
			if inner, ok := v["sessions"]; ok {
				switch s := inner.(type) {
				case []any:
					total += len(s)
					counted = true
					continue
				case map[string]any:
					total += len(s)
					counted = true
					continue
				}
			}
			total += len(v)
			counted = true
		}
	}
	return total, counted
}

func parentDir(p string) string {
	p = strings.TrimRight(p, "/")
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "/"
	}
	return p[:i]
}
