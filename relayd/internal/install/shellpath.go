package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/detect"
)

// Making what was installed typeable.
//
// From a real machine, after a run that installed Codex:
//
//	$ codex
//	zsh: command not found: codex
//
// Nothing was broken. Everything Relay installs lands in ~/.local/bin, Relay
// puts that directory on its own PATH so its detection and its doctor can see
// it, and a fresh terminal has never heard of it. install.sh prints one warning
// about this at the very top, before anything is installed, and then the
// installer spends the next ten minutes telling the user to run `relay embed`,
// `relay doctor` and `claude` — none of which they can type.
//
// So it is offered, once, at the end, when it is actually true: the exact line,
// the exact file, and a no that leaves the machine exactly as it was.

// ShellPathOutcome is what this step did.
type ShellPathOutcome struct {
	// Dir is the directory in question.
	Dir string
	// Profile is the file that was, or would have been, appended to.
	Profile string
	// Added is true when the line was written.
	Added bool
	// AlreadyThere is true when the shell could already find it, which is the
	// case this step exists to shut up about.
	AlreadyThere bool
	Warnings     []string
}

// pathMarker keeps a second run from appending a second copy, and tells anyone
// reading their profile who put the line there.
const pathMarker = "# added by relay setup"

// offerShellPath asks to put ~/.local/bin on the user's PATH for good.
func offerShellPath(ctx context.Context, opts Options) (ShellPathOutcome, error) {
	p := opts.Prompt
	home := opts.Env.Home
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return ShellPathOutcome{}, nil
		}
	}
	out := ShellPathOutcome{Dir: filepath.Join(home, ".local", "bin")}

	profile, kind := shellProfile(opts, home)

	// The question is about the user's own shell, and it has to be asked of
	// that shell.
	//
	// This used to read os.Getenv("PATH"), which is this process's — and
	// restorePath adds ~/.local/bin to it three hundred lines earlier, so the
	// answer was always "already there" and the question never fired on any
	// real machine. It fired in tests, whose Getenv is a map. So: ask a login
	// shell what its PATH is, and fall back to reading the file that would have
	// set it.
	if shellSeesDir(ctx, opts, profile, out.Dir) {
		out.AlreadyThere = true
		return out, nil
	}
	if profile == "" {
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"%s is not on your PATH, so `relay` and the agent runtimes it installed cannot be "+
				"typed. Add it the way your shell does.", out.Dir))
		p.Say("  %s", wrapIndent(out.Warnings[0], 2, 76))
		return out, nil
	}
	out.Profile = profile

	line := exportLine(out.Dir, kind)
	if existing, err := opts.FS.ReadFile(profile); err == nil && strings.Contains(string(existing), out.Dir) {
		// Already written, by us or by them, and not yet in this shell — which
		// is a new terminal away, not a second line in the file.
		out.AlreadyThere = true
		p.Say("  %s", wrapIndent(fmt.Sprintf(
			"%s is in %s already. A new terminal picks it up.",
			out.Dir, shortSource(profile, home)), 2, 76))
		return out, nil
	}

	yes, err := p.Confirm(Confirm{
		ID:     "path.profile",
		Prompt: "Put it on your PATH?",
		Body: fmt.Sprintf("Relay, and everything it installed, lives in %s, which your shell "+
			"does not look in. This appends one line to %s:\n\n    %s\n",
			out.Dir, shortSource(profile, home), line),
		// The only question in this installer that defaults to yes. It installs
		// nothing, sends nothing, and the alternative is a machine full of
		// commands the user was told to run and cannot.
		Default: true,
	})
	if err != nil {
		return out, err
	}
	if !yes {
		p.Say("  %s", wrapIndent("Left alone. For this terminal: "+line, 2, 76))
		return out, nil
	}

	body := ""
	if b, err := opts.FS.ReadFile(profile); err == nil {
		body = string(b)
		if body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
	}
	body += "\n" + pathMarker + "\n" + line + "\n"
	if err := opts.FS.MkdirAll(filepath.Dir(profile), 0o755); err != nil {
		return out, err
	}
	if err := opts.FS.WriteFile(profile, []byte(body), 0o644); err != nil {
		w := fmt.Sprintf("could not write %s: %v. Add this yourself: %s", profile, err, line)
		out.Warnings = append(out.Warnings, w)
		p.Say("  %s", wrapIndent(w, 2, 76))
		return out, nil
	}
	out.Added = true
	p.Say("  Added to %s. New terminals have it; for this one:", shortSource(profile, home))
	p.Say("    %s", line)
	return out, nil
}

// shellProfile picks the file to append to, and the syntax to write in it.
//
// $SHELL is what the user's terminal starts, which is the shell that will not
// find the binary. A shell Relay does not know how to write for gets a sentence
// instead of a guess.
func shellProfile(opts Options, home string) (path, kind string) {
	sh := filepath.Base(opts.Env.Getenv("SHELL"))
	switch sh {
	case "zsh":
		if z := opts.Env.Getenv("ZDOTDIR"); z != "" {
			return filepath.Join(z, ".zshrc"), "posix"
		}
		return filepath.Join(home, ".zshrc"), "posix"
	case "bash":
		// macOS starts login shells in Terminal, which read .bash_profile;
		// everywhere else .bashrc is the one that runs.
		if opts.Env.GOOS == "darwin" {
			return filepath.Join(home, ".bash_profile"), "posix"
		}
		return filepath.Join(home, ".bashrc"), "posix"
	case "fish":
		return filepath.Join(home, ".config", "fish", "config.fish"), "fish"
	}
	return "", ""
}

func exportLine(dir, kind string) string {
	if kind == "fish" {
		return fmt.Sprintf("fish_add_path %s", dir)
	}
	return fmt.Sprintf("export PATH=%q:$PATH", dir)
}

// shellSeesDir reports whether a terminal the user opens next would find dir.
//
// A login+interactive shell is what a terminal window starts, and it is the
// only thing that can answer this: the directory can arrive from a profile, an
// /etc/paths entry, a version manager's hook, or something Relay has never
// heard of. Asking is cheap and exact; inferring is neither.
func shellSeesDir(ctx context.Context, opts Options, profile, dir string) bool {
	if sh := opts.Env.Getenv("SHELL"); sh != "" && opts.Env.Exec != nil {
		// -i because a terminal window is interactive and that is where zsh
		// reads .zshrc; -l because it is a login shell on macOS. Bounded,
		// because somebody's rc file is somebody else's program.
		res, err := opts.Env.Exec.Run(ctx, detect.Cmd{
			Name: sh, Args: []string{"-lic", "printf %s \"$PATH\""},
			Timeout: 10 * time.Second,
		})
		if err == nil && res.Code == 0 {
			if out := res.Out(); out != "" {
				return pathHas(out, dir)
			}
		}
	}
	// The shell would not answer. The file it would have read is the next best
	// evidence, and a line already in it means a new terminal has this.
	if profile != "" {
		if b, err := opts.FS.ReadFile(profile); err == nil && strings.Contains(string(b), dir) {
			return true
		}
	}
	return false
}
