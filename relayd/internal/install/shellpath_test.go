package install

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/detect"
)

// The step that answers "zsh: command not found: codex" about a codex this
// installer put on the machine twenty minutes earlier.

// pathOpts builds a machine whose login shell reports shellPATH — which is the
// only PATH that decides this question, and emphatically not this process's.
func pathOpts(t *testing.T, answers map[string]string, shell, shellPATH string) (Options, *Script, *detect.MemFS) {
	t.Helper()
	home := t.TempDir()
	fs := &detect.MemFS{}
	script := NewScript(answers)
	ex := &detect.FakeExec{
		Paths:     map[string]string{filepath.Base(shell): shell},
		Responses: map[string]detect.Result{},
	}
	if shellPATH != "" {
		ex.Responses[detect.Key(shell, "-lic", `printf %s "$PATH"`)] = detect.Result{
			Stdout: []byte(shellPATH),
		}
	}
	return Options{
		Prompt: script, FS: fs,
		Env: detect.Env{
			Home: home, GOOS: "darwin", FS: fs, Exec: ex,
			Getenv: func(k string) string {
				if k == "SHELL" {
					return shell
				}
				return ""
			},
		},
	}.withDefaults(), script, fs
}

func TestThePathLineIsOfferedAndWritten(t *testing.T) {
	opts, script, fs := pathOpts(t, map[string]string{"path.profile": "yes"}, "/bin/zsh", "/usr/bin:/bin")

	out, err := offerShellPath(context.Background(), opts)
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
	opts, script, _ := pathOpts(t, map[string]string{}, "/bin/zsh", "")
	// The shell's own report is what counts, and it names the directory.
	dir := filepath.Join(opts.Env.Home, ".local", "bin")
	opts.Env.Exec.(*detect.FakeExec).Responses[detect.Key("/bin/zsh", "-lic", `printf %s "$PATH"`)] =
		detect.Result{Stdout: []byte("/usr/bin:" + dir + ":/bin")}

	out, err := offerShellPath(context.Background(), opts)
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

// The bug that shipped in v0.2.9 and fired on nobody.
//
// restorePath puts ~/.local/bin on the INSTALLER's PATH three hundred lines
// before this step, so a step that reads os.Getenv("PATH") always concludes
// there is nothing to do — on every real machine, while passing every test,
// because a fixture's Getenv is a map. The user's shell is a different program
// and has to be asked.
func TestTheInstallersOwnPathDoesNotAnswerForTheUsers(t *testing.T) {
	opts, script, fs := pathOpts(t, map[string]string{"path.profile": "yes"}, "/bin/zsh", "/usr/bin:/bin")
	dir := filepath.Join(opts.Env.Home, ".local", "bin")

	// Exactly what restorePath does, to this process, before this step runs.
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	out, err := offerShellPath(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if out.AlreadyThere {
		t.Fatal("the installer's own PATH was taken as the user's")
	}
	if !out.Added {
		t.Fatalf("out = %+v, want the question asked and answered", out)
	}
	if !strings.Contains(fs.Files[filepath.Join(opts.Env.Home, ".zshrc")], dir) {
		t.Error("nothing was written for a shell that cannot see the directory")
	}
	if len(script.Asked) == 0 {
		t.Error("the user was never asked")
	}
}

// No means no, and the machine is left exactly as it was.
func TestDecliningWritesNothing(t *testing.T) {
	opts, script, fs := pathOpts(t, map[string]string{"path.profile": "no"}, "/bin/zsh", "/usr/bin")

	out, err := offerShellPath(context.Background(), opts)
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

	if _, err := offerShellPath(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	first := fs.Files[filepath.Join(opts.Env.Home, ".zshrc")]

	// The line is in the file and still not in this process's PATH, which is
	// exactly the state of a rerun in the same terminal.
	out, err := offerShellPath(context.Background(), opts)
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

	if _, err := offerShellPath(context.Background(), opts); err != nil {
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

	out, err := offerShellPath(context.Background(), opts)
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

// Order matters, and this is why.
//
// From a real run: the Gateway step said "open another terminal window and run
// `claude`", the user did, and that terminal said `command not found` — because
// the line that would have taught it was four steps further down the install.
// The offer has to come before the first thing that sends somebody elsewhere to
// type something.
func TestThePathIsOfferedBeforeAnythingSendsYouToAnotherTerminal(t *testing.T) {
	answers := baseAnswers()
	answers["path.profile"] = "yes"
	answers["bus.ack"] = "yes"
	answers["bus.install"] = "yes"
	answers["bus.auth"] = "skip"

	opts, script, _ := newOpts(t, answers, func(o *Options) {
		busEnv(t, "v24.19.0", false)(o)
		env := map[string]string{"SHELL": "/bin/zsh"}
		inner := o.Env.Getenv
		o.Env.Getenv = func(k string) string {
			if v, ok := env[k]; ok {
				return v
			}
			return inner(k)
		}
	})
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}

	var path, login int = -1, -1
	for i, id := range script.Asked {
		switch id {
		case "path.profile":
			if path < 0 {
				path = i
			}
		case "bus.ack":
			if login < 0 {
				login = i
			}
		}
	}
	if path < 0 {
		t.Fatalf("the PATH question was never asked:\n%v", script.Asked)
	}
	if login < 0 {
		t.Skip("this fixture never reached the bus step")
	}
	if path > login {
		t.Errorf("asked about the bus (%d) before making its commands typeable (%d):\n%v",
			login, path, script.Asked)
	}
}
