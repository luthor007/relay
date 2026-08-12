package detect

import (
	"context"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/adapter"
)

// These are the tests that exist because the obvious implementation is wrong.
//
// MEMORY.md §4: never hardcode ~/.openclaw. A reader that assumes the default
// silently finds nothing and reports an empty history as success — and success
// is the one failure mode nobody investigates.

func openclawEnv(files map[string]string, dirs []string, exec *FakeExec, getenv func(string) string) Env {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	if exec == nil {
		exec = &FakeExec{}
	}
	return Env{
		FS:     &MemFS{Files: files, Dirs: append([]string{"/home/u"}, dirs...)},
		Exec:   exec,
		Getenv: getenv,
		Home:   "/home/u",
		GOOS:   "linux",
	}
}

// The headline case: the user runs `openclaw --profile work`, so the state
// lives in ~/.openclaw-work. ~/.openclaw exists too and is empty — a leftover.
// Assuming the default finds zero sessions and calls it a clean install.
func TestOpenClawAsksRatherThanAssuming(t *testing.T) {
	env := openclawEnv(
		map[string]string{
			"/home/u/.openclaw-work/openclaw.json":                      `{"agent":"main"}`,
			"/home/u/.openclaw-work/agents/main/sessions/sessions.json": `[{"id":"a"},{"id":"b"},{"id":"c"}]`,
		},
		[]string{"/home/u/.openclaw"}, // the default exists, and is empty
		&FakeExec{
			Paths: map[string]string{"openclaw": "/usr/local/bin/openclaw"},
			Responses: map[string]Result{
				Key("openclaw", "config", "file"): {Stdout: []byte("/home/u/.openclaw-work/openclaw.json\n")},
			},
		}, nil)

	st := ResolveOpenClawState(context.Background(), env, Options{})
	if st.Dir != "/home/u/.openclaw-work" {
		t.Fatalf("resolved %q; assuming ~/.openclaw here reports an empty history as success", st.Dir)
	}
	if st.Source != SourceAsked {
		t.Errorf("source = %q, want asked", st.Source)
	}
	if len(st.SessionStores) != 1 {
		t.Fatalf("session stores = %v", st.SessionStores)
	}

	rep := Detect(context.Background(), env, Options{Only: []adapter.Runtime{adapter.OpenClaw}})
	f := rep.Findings[0]
	if n, ok := f.SessionCount(); !ok || n != 3 {
		t.Errorf("sessions = %d ok=%v, want the 3 in the relocated store", n, ok)
	}
	if f.Status() != StatusInUse {
		t.Errorf("status = %s, want in_use", f.Status())
	}
}

func TestOpenClawEnvVarBeatsEverything(t *testing.T) {
	env := openclawEnv(
		map[string]string{
			"/srv/claw/agents/main/sessions/sessions.json": `{"sessions":[{"id":"a"}]}`,
			"/home/u/.openclaw-work/openclaw.json":         `{}`,
		},
		nil,
		&FakeExec{
			Paths: map[string]string{"openclaw": "/usr/local/bin/openclaw"},
			Responses: map[string]Result{
				Key("openclaw", "config", "file"): {Stdout: []byte("/home/u/.openclaw-work/openclaw.json\n")},
			},
		},
		func(k string) string {
			if k == "OPENCLAW_STATE_DIR" {
				return "/srv/claw"
			}
			return ""
		})

	st := ResolveOpenClawState(context.Background(), env, Options{})
	if st.Dir != "/srv/claw" || st.Source != SourceEnv {
		t.Fatalf("resolved %q from %q, want /srv/claw from env", st.Dir, st.Source)
	}
}

func TestOpenClawProfileFlagRelocates(t *testing.T) {
	env := openclawEnv(map[string]string{
		"/home/u/.openclaw-dev/agents/main/sessions/sessions.json": `[]`,
	}, nil, nil, nil)

	st := ResolveOpenClawState(context.Background(), env, Options{OpenClawDev: true})
	if st.Dir != "/home/u/.openclaw-dev" || st.Source != SourceProfile {
		t.Fatalf("--dev resolved to %q (%s)", st.Dir, st.Source)
	}

	st = ResolveOpenClawState(context.Background(), env, Options{OpenClawProfile: "work"})
	if st.Dir != "/home/u/.openclaw-work" {
		t.Fatalf("--profile work resolved to %q", st.Dir)
	}
}

// The session store path is itself configurable in the gateway config, so even
// the right state directory is not the end of the resolution.
func TestOpenClawConfigCanRelocateTheSessionStore(t *testing.T) {
	env := openclawEnv(map[string]string{
		"/home/u/.openclaw/openclaw.json":               `{"storage":{"sessionStorePath":"/data/claw"}}`,
		"/data/claw/agents/main/sessions/sessions.json": `[{"id":"a"},{"id":"b"}]`,
	}, nil, &FakeExec{
		Paths: map[string]string{"openclaw": "/usr/local/bin/openclaw"},
		Responses: map[string]Result{
			Key("openclaw", "config", "file"): {Stdout: []byte("/home/u/.openclaw/openclaw.json\n")},
		},
	}, nil)

	st := ResolveOpenClawState(context.Background(), env, Options{})
	if st.Dir != "/data/claw" {
		t.Fatalf("resolved %q, want the configured store path", st.Dir)
	}
	if st.Source != SourceConfig {
		t.Errorf("source = %q, want config", st.Source)
	}
	if len(st.Notes) == 0 || !strings.Contains(strings.Join(st.Notes, " "), "sessionStorePath") {
		t.Errorf("the override has to be said out loud: %v", st.Notes)
	}
}

// YAML config, same answer. The format differs by version and neither has been
// probed, so both are read.
func TestOpenClawYAMLConfig(t *testing.T) {
	env := openclawEnv(map[string]string{
		"/home/u/.openclaw/openclaw.yaml":                   "storage:\n  sessionsDir: /data/yamlclaw\n",
		"/data/yamlclaw/agents/main/sessions/sessions.json": `[{"id":"a"}]`,
	}, nil, nil, nil)

	st := ResolveOpenClawState(context.Background(), env, Options{})
	if st.Dir != "/data/yamlclaw" {
		t.Fatalf("resolved %q from YAML config", st.Dir)
	}
}

// The directory does not exist until the gateway has run once. That is not an
// error, and it is not the same as "no history" — a sibling profile directory
// next to it is the single most likely explanation, so say so.
func TestOpenClawMissingDirectoryPointsAtSiblings(t *testing.T) {
	env := openclawEnv(map[string]string{
		"/home/u/.openclaw-dev/agents/main/sessions/sessions.json": `[{"id":"a"}]`,
	}, nil, nil, nil)

	st := ResolveOpenClawState(context.Background(), env, Options{})
	if st.Exists {
		t.Fatal("the default directory must not be reported as existing")
	}
	joined := strings.Join(st.Notes, " ")
	if !strings.Contains(joined, ".openclaw-dev") {
		t.Errorf("want a note naming the sibling directory, got %v", st.Notes)
	}
	if !strings.Contains(joined, "OPENCLAW_STATE_DIR") {
		t.Errorf("want the note to say how to fix it, got %v", st.Notes)
	}
}

// A default that happens to be right is still a guess, and the report says so.
func TestOpenClawDefaultIsLabelledAsAGuess(t *testing.T) {
	env := openclawEnv(map[string]string{
		"/home/u/.openclaw/agents/main/sessions/sessions.json": `[{"id":"a"}]`,
	}, nil, nil, nil)

	st := ResolveOpenClawState(context.Background(), env, Options{})
	if st.Dir != "/home/u/.openclaw" {
		t.Fatalf("resolved %q", st.Dir)
	}
	if st.Source.Trusted() {
		t.Error("the default must never be reported as a trusted source")
	}
	if !strings.Contains(strings.Join(st.Notes, " "), "assumed") {
		t.Errorf("want the guess named as one: %v", st.Notes)
	}
}

func TestOpenClawSessionCountShapes(t *testing.T) {
	cases := map[string]struct {
		body string
		want int
	}{
		"array":         {`[{"id":"a"},{"id":"b"}]`, 2},
		"keyed object":  {`{"a":{"id":"a"},"b":{"id":"b"},"c":{}}`, 3},
		"sessions list": {`{"sessions":[{"id":"a"}]}`, 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := openclawEnv(map[string]string{
				"/home/u/.openclaw/agents/main/sessions/sessions.json": tc.body,
			}, nil, nil, nil)
			st := ResolveOpenClawState(context.Background(), env, Options{})
			n, ok := countOpenClawSessions(env, st.SessionStores)
			if !ok || n != tc.want {
				t.Errorf("count = %d ok=%v, want %d", n, ok, tc.want)
			}
		})
	}
}
