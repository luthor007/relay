package install

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/detect"
)

// The step that answers "zsh: command not found: codex" about a codex this
// installer put on the machine twenty minutes earlier.

func pathOpts(t *testing.T, answers map[string]string, shell, path string) (Options, *Script, *detect.MemFS) {
	t.Helper()
	home := t.TempDir()
	fs := &detect.MemFS{}
	script := NewScript(answers)
	env := map[string]string{"SHELL": shell, "PATH": path}
	return Options{
		Prompt: script, FS: fs,
		Env: detect.Env{
			Home: home, GOOS: "darwin", FS: fs, Exec: &detect.FakeExec{},
			Getenv: func(k string) string { return env[k] },
		},
	}.withDefaults(), script, fs
}

func TestThePathLineIsOfferedAndWritten(t *testing.T) {
	opts, script, fs := pathOpts(t, map[string]string{"path.profile": "yes"}, "/bin/zsh", "/usr/bin:/bin")

	out, err := offerShellPath(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Added {
		t.Fatalf("out = %+v, want the line written", out)
	}
	rc := fs.Files[filepath.Join(opts.Env.Home, ".zshrc")]
	want := `export PATH="` + filepath.Join(opts.Env.Home, ".local", "bin") + `":$PATH`
	if !strings.Contains(rc, want) {
		t.Errorf("~/.zshrc does not contain %q:\n%s", want, rc)
	}
	// Whoever reads that file later should be able to tell where it came from.
	if !strings.Contains(rc, pathMarker) {
		t.Errorf("the line is unattributed:\n%s", rc)
	}
	// And the user can use this terminal too, not just the next one.
	if !strings.Contains(script.Output(), want) {
		t.Errorf("the line was not printed for the shell already open:\n%s", script.Output())
	}
}

// A shell that can already find it is not asked anything. This step is not
// worth a question on a machine where the answer is already yes.
func TestNothingIsAskedWhenTheShellAlreadyLooksThere(t *testing.T) {
	home := t.TempDir()
	fs := &detect.MemFS{}
	script := NewScript(map[string]string{})
	env := map[string]string{
		"SHELL": "/bin/zsh",
		"PATH":  "/usr/bin:" + filepath.Join(home, ".local", "bin") + ":/bin",
	}
	opts := Options{
		Prompt: script, FS: fs,
		Env: detect.Env{Home: home, GOOS: "darwin", FS: fs, Exec: &detect.FakeExec{},
			Getenv: func(k string) string { return env[k] }},
	}.withDefaults()

	out, err := offerShellPath(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !out.AlreadyThere || out.Added {
		t.Errorf("out = %+v, want it to notice and say nothing", out)
	}
	if len(script.Asked) != 0 {
		t.Errorf("asked %v about a PATH that already works", script.Asked)
	}
}

// No means no, and the machine is left exactly as it was.
func TestDecliningWritesNothing(t *testing.T) {
	opts, script, fs := pathOpts(t, map[string]string{"path.profile": "no"}, "/bin/zsh", "/usr/bin")

	out, err := offerShellPath(opts)
	if err != nil {
		t.Fatal(err)
	}
	if out.Added {
		t.Error("the line was written after a no")
	}
	if len(fs.Files) != 0 {
		t.Errorf("files written: %v", fs.Files)
	}
	// The line is still printed, because the user still has to be able to use
	// what was just installed.
	if !strings.Contains(script.Output(), "export PATH=") {
		t.Errorf("no way forward was offered:\n%s", script.Output())
	}
}

// Twice is once. A second setup run must not append a second copy.
func TestASecondRunDoesNotAppendASecondLine(t *testing.T) {
	opts, _, fs := pathOpts(t, map[string]string{"path.profile": "yes"}, "/bin/zsh", "/usr/bin")

	if _, err := offerShellPath(opts); err != nil {
		t.Fatal(err)
	}
	first := fs.Files[filepath.Join(opts.Env.Home, ".zshrc")]

	// The line is in the file and still not in this process's PATH, which is
	// exactly the state of a rerun in the same terminal.
	out, err := offerShellPath(opts)
	if err != nil {
		t.Fatal(err)
	}
	if out.Added {
		t.Error("appended a second copy")
	}
	if got := fs.Files[filepath.Join(opts.Env.Home, ".zshrc")]; got != first {
		t.Errorf("the file changed on the second run:\n%s", got)
	}
}

// fish keeps its own syntax, and an export line in config.fish is a syntax
// error rather than a PATH.
func TestFishGetsFishSyntax(t *testing.T) {
	opts, _, fs := pathOpts(t, map[string]string{"path.profile": "yes"}, "/opt/homebrew/bin/fish", "/usr/bin")

	if _, err := offerShellPath(opts); err != nil {
		t.Fatal(err)
	}
	conf := fs.Files[filepath.Join(opts.Env.Home, ".config", "fish", "config.fish")]
	if !strings.Contains(conf, "fish_add_path ") {
		t.Errorf("config.fish did not get fish syntax:\n%s", conf)
	}
	if strings.Contains(conf, "export PATH=") {
		t.Errorf("config.fish got posix syntax, which it cannot read:\n%s", conf)
	}
}

// A shell Relay does not know is told the truth rather than handed a guess at
// which file it reads.
func TestAnUnknownShellIsToldRatherThanGuessedAt(t *testing.T) {
	opts, script, fs := pathOpts(t, map[string]string{}, "/usr/bin/exotic", "/usr/bin")

	out, err := offerShellPath(opts)
	if err != nil {
		t.Fatal(err)
	}
	if out.Added || len(fs.Files) != 0 {
		t.Errorf("wrote something for a shell it does not know: %+v %v", out, fs.Files)
	}
	if len(out.Warnings) == 0 {
		t.Error("said nothing about a PATH that does not contain what was installed")
	}
	if len(script.Asked) != 0 {
		t.Errorf("asked %v a question it could not act on", script.Asked)
	}
}

// And the whole run reaches it. A step that works in isolation and is never
// called is the shape of this exact bug: `linkGlobals` put codex in
// ~/.local/bin, and the user still could not type it.
func TestAFullRunOffersThePathLine(t *testing.T) {
	answers := baseAnswers()
	answers["path.profile"] = "yes"

	opts, script, fs := newOpts(t, answers, func(o *Options) {
		env := map[string]string{"SHELL": "/bin/zsh", "PATH": "/usr/bin:/bin"}
		inner := o.Env.Getenv
		o.Env.Getenv = func(k string) string {
			if v, ok := env[k]; ok {
				return v
			}
			return inner(k)
		}
	})
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	if !res.ShellPath.Added {
		t.Fatalf("ShellPath = %+v, want the line written during a full run", res.ShellPath)
	}
	if rc := fs.Files[home+"/.zshrc"]; !strings.Contains(rc, "/.local/bin") {
		t.Errorf("~/.zshrc = %q", rc)
	}
}
