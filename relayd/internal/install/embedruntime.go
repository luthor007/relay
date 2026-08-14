package install

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/detect"
)

// The local embedding runtime, behind a seam.
//
// Everything that downloads, installs, pulls or calls goes through
// [EmbedRuntime], for the same reason detection goes through detect.Env: the
// only machine CI will ever have is one with none of this on it, and the
// registry the model comes from is not reachable from it either. So the whole
// step runs from fixtures, and the one thing that cannot be tested here — the
// real download — is the one thing behind the interface.
//
// **The runtime is a separate process, on purpose.** SYSTEM.md §8 requires
// CGO_ENABLED=0 and a single static binary that cross-compiles to darwin and
// linux on both architectures from one machine; that is what the $0 self-host
// tier depends on. Linking an inference runtime in would end that immediately.
// So Relay talks to the model over HTTP on loopback, exactly as it talks to a
// hosted provider, and the only difference is the address.

// Reporter is how a long-running step says what it is doing. It is
// Prompter.Say's signature, so the prompter is passed straight in.
type Reporter func(format string, args ...any)

// LocalStatus is what the local runtime says about itself.
//
// Installed and Running are separate fields because they are separate states,
// and the difference is the failure this whole file exists to make legible: a
// machine where the binary is present, the model is pulled, and every embedding
// call fails.
type LocalStatus struct {
	Installed bool
	Running   bool
	// Host is where the service is expected.
	Host    string
	Version string
	// Models is what is already pulled. nil means we could not ask, which is
	// not the same as an empty library.
	Models []string
	// Detail is why Running is false, verbatim.
	Detail string
}

// EmbedRuntime is the local model runtime. Ollama is the only implementation
// that ships; the interface exists so that stays a choice.
type EmbedRuntime interface {
	// Name is what the user calls it.
	Name() string
	// Docs is where to send someone when Relay has no command to offer.
	Docs() string

	// Status is one detection pass. It never returns an error: "not installed"
	// and "not running" are facts about the machine, not failures to ask.
	Status(ctx context.Context) LocalStatus

	// InstallPlan is exactly what Install will do, so it can be shown before it
	// is agreed to: somebody else's command run as-is, or a download Relay
	// performs itself. Not OK means Relay has no way to install this here and
	// says so — a wrong install command is worse than no suggestion.
	InstallPlan() InstallPlan
	Install(ctx context.Context, say Reporter) error

	// Pull fetches a model.
	Pull(ctx context.Context, model string, say Reporter) error
}

// Note what is NOT on that interface: embedding. internal/llm owns the call
// itself, for both the local runtime and every hosted provider, so there is one
// wire format, one batching path, one width check and one probe. This seam is
// only the part llm cannot do — putting the thing on the machine.

// runtime returns the configured local runtime, defaulting to Ollama wired to
// this Env.
func (o Options) runtime() EmbedRuntime {
	if o.EmbedRuntime != nil {
		return o.EmbedRuntime
	}
	return &ollamaRuntime{env: o.Env, client: o.HTTPClient, opts: &o}
}

// ------------------------------------------------------------------ ollama --

// Timeouts. An install and a model pull are both downloads over somebody's home
// connection, so these are generous — but they are bounded, because an
// installer that hangs forever is indistinguishable from one that crashed.
const (
	OllamaInstallTimeout = 15 * time.Minute
	OllamaPullTimeout    = 45 * time.Minute
)

// ollamaDocs is where a user goes when Relay has no command for their machine.
const ollamaDocs = "https://ollama.com/download"

type ollamaRuntime struct {
	env    detect.Env
	client *http.Client
	// opts carries the download seams — HTTP client, filesystem, launchctl —
	// for the path where Relay installs Ollama itself rather than running
	// somebody else's command.
	opts *Options
}

func (r *ollamaRuntime) Name() string { return "Ollama" }
func (r *ollamaRuntime) Docs() string { return ollamaDocs }

func (r *ollamaRuntime) Status(ctx context.Context) LocalStatus {
	env := r.env
	if env.HTTP == nil {
		env.HTTP = r.httpClient()
	}
	o := detect.DetectOllama(ctx, env)
	st := LocalStatus{
		Installed: o.Installed,
		Running:   o.Reachable,
		Host:      o.Host,
		Version:   o.Version,
		Detail:    o.ServiceNote,
	}
	if o.Models != nil {
		st.Models = o.ModelNames()
	}
	return st
}

// InstallPlan is the vendor's own install, and nothing else: their documented
// command where there is one that runs on this machine, and their own published
// build where there is not.
//
// The commands have not been run by us on a real machine — the registry is not
// reachable from the machine this was written on — so they are shown in full and
// agreed to before they run, which is the same treatment the five agent runtimes
// get in runtimes.go. The download path was measured: the archive, its checksum
// file, and the unpacked binary all answered on 2026-08-14.
func (r *ollamaRuntime) InstallPlan() InstallPlan {
	switch r.env.GOOS {
	case "darwin":
		if r.has("brew") {
			cmd := []string{"brew", "install", "ollama"}
			return InstallPlan{Cmd: cmd, OK: true, Body: "Runs: " + strings.Join(cmd, " ")}
		}
		// No Homebrew, which is the state of a Mac nobody has set up yet. The
		// alternative used to be a sentence pointing at a .dmg somebody drags
		// into /Applications. Relay downloads the tarball beside it instead.
		if ollamaArchive(r.env.GOOS) == "" {
			return InstallPlan{}
		}
		return InstallPlan{
			OK: true,
			Body: fmt.Sprintf("Downloads Ollama %s (%d MB) from ollama's own release, checks it "+
				"against their published checksums, and unpacks it into ~/.local — no sudo, and "+
				"nothing touches your shell profile. It is started now and at login.",
				OllamaPin, OllamaDownloadMB),
		}
	case "linux":
		if r.has("curl") {
			cmd := []string{"sh", "-c", "curl -fsSL https://ollama.com/install.sh | sh"}
			return InstallPlan{Cmd: cmd, OK: true, Body: "Runs: " + strings.Join(cmd, " ")}
		}
	}
	return InstallPlan{}
}

// download is the no-package-manager path: fetch the vendor's own build,
// verify it, and leave a server running that comes back after a reboot.
func (r *ollamaRuntime) download(ctx context.Context, say Reporter) error {
	if r.opts == nil {
		return fmt.Errorf("install: no download seam wired for Ollama")
	}
	opts := *r.opts
	archive := ollamaArchive(r.env.GOOS)
	dir, err := installOllama(ctx, opts, archive)
	if err != nil {
		return err
	}
	say("  Ollama %s in %s.", OllamaPin, shortSource(dir, r.env.Home))
	if err := serveOllama(ctx, opts, dir); err != nil {
		// The binary is on the machine either way, and `ollama serve` in a
		// terminal is a working answer, so this is reported rather than fatal.
		say("  %s", wrapIndent("Installed, but it could not be started at login: "+
			err.Error()+" Run `ollama serve` and it works for this session.", 2, 76))
		return nil
	}
	if !waitForOllama(ctx, r, 30*time.Second) {
		say("  %s", wrapIndent("Installed and registered, but the server has not answered yet. "+
			"It may still be starting; `relay embed` picks up where this left off.", 2, 76))
	}
	return nil
}

func (r *ollamaRuntime) Install(ctx context.Context, say Reporter) error {
	plan := r.InstallPlan()
	if !plan.OK {
		return fmt.Errorf("install: no way to install Ollama on %s; see %s", r.env.GOOS, ollamaDocs)
	}
	if len(plan.Cmd) == 0 {
		return r.download(ctx, say)
	}
	cmd := plan.Cmd
	if r.env.Exec == nil {
		return fmt.Errorf("install: no way to run commands on this machine")
	}
	say("  Running: %s", strings.Join(cmd, " "))
	start := time.Now()
	res, err := r.env.Exec.Run(ctx, detect.Cmd{
		Name: cmd[0], Args: cmd[1:], Timeout: OllamaInstallTimeout,
	})
	if err != nil {
		return err
	}
	if res.Code != 0 {
		detail := res.Err()
		if detail == "" {
			detail = res.Out()
		}
		return fmt.Errorf("exit %d: %s", res.Code, lastLine(detail))
	}
	say("  Done in %s.", time.Since(start).Round(time.Second))
	return nil
}

// Pull fetches a model.
//
// Progress is reported per phase rather than per byte: detect.Exec buffers a
// command's output and hands it back at the end, so there is no stream to
// forward. That is a deliberate limit of the process seam — it is what lets
// every other command in this package be driven from a fixture — and a
// progress bar is not worth giving that up for. What the user gets is the model
// name, the size, and the elapsed time, which is what they need to decide
// whether to wait.
func (r *ollamaRuntime) Pull(ctx context.Context, model string, say Reporter) error {
	if r.env.Exec == nil {
		return fmt.Errorf("install: no way to run commands on this machine")
	}
	start := time.Now()
	res, err := r.env.Exec.Run(ctx, detect.Cmd{
		Name: detect.OllamaBinary, Args: []string{"pull", model}, Timeout: OllamaPullTimeout,
	})
	if err != nil {
		return err
	}
	if res.Code != 0 {
		detail := res.Err()
		if detail == "" {
			detail = res.Out()
		}
		return fmt.Errorf("ollama pull %s exited %d: %s", model, res.Code, lastLine(detail))
	}
	say("  Pulled in %s.", time.Since(start).Round(time.Second))
	return nil
}

func (r *ollamaRuntime) httpClient() *http.Client {
	if r.client != nil {
		return r.client
	}
	if r.env.HTTP != nil {
		return r.env.HTTP
	}
	return &http.Client{Timeout: detect.ServiceTimeout}
}

func (r *ollamaRuntime) has(bin string) bool {
	if r.env.Exec == nil {
		return false
	}
	_, err := r.env.Exec.LookPath(bin)
	return err == nil
}

// ---------------------------------------------------------------- fixtures --

// FakeEmbedRuntime is a scripted local runtime.
//
// It is here rather than in a _test.go file for the same reason detect.MemFS
// and detect.FakeExec are: the CLI's tests and the installer's tests both need
// it, and the real one cannot run in either place — the download hosts are
// unreachable from CI by policy, so the fixture is the only way this path is
// ever exercised.
type FakeEmbedRuntime struct {
	NameStr string
	DocsStr string

	// State is what Status reports. Install, Pull and the service coming up all
	// mutate it, so a test drives the whole sequence by asserting on it.
	State LocalStatus

	// InstallCmd is what InstallPlan carries; nil means Relay has none for
	// this machine, which is a case worth testing.
	InstallCmd []string
	// CanDownload makes a runtime with no command still installable, which is
	// the Ollama-on-a-stock-Mac case.
	CanDownload bool
	// InstallErr and PullErr fail the corresponding call.
	InstallErr error
	PullErr    error

	// StartsOnStatusCall makes the service come up on the Nth Status call,
	// which is how "the user went and started it" is expressed.
	StartsOnStatusCall int

	// Calls records every method, in order.
	Calls []string
}

func (f *FakeEmbedRuntime) Name() string {
	if f.NameStr != "" {
		return f.NameStr
	}
	return "Ollama"
}

func (f *FakeEmbedRuntime) Docs() string {
	if f.DocsStr != "" {
		return f.DocsStr
	}
	return ollamaDocs
}

func (f *FakeEmbedRuntime) statusCalls() int {
	n := 0
	for _, c := range f.Calls {
		if c == "status" {
			n++
		}
	}
	return n
}

func (f *FakeEmbedRuntime) Status(context.Context) LocalStatus {
	f.Calls = append(f.Calls, "status")
	if f.StartsOnStatusCall > 0 && f.statusCalls() >= f.StartsOnStatusCall {
		f.State.Running = true
		f.State.Detail = ""
	}
	return f.State
}

func (f *FakeEmbedRuntime) InstallPlan() InstallPlan {
	if len(f.InstallCmd) == 0 {
		return InstallPlan{OK: f.CanDownload, Body: "Downloads it."}
	}
	return InstallPlan{Cmd: f.InstallCmd, OK: true, Body: "Runs: " + strings.Join(f.InstallCmd, " ")}
}

func (f *FakeEmbedRuntime) Install(_ context.Context, say Reporter) error {
	f.Calls = append(f.Calls, "install")
	if f.InstallErr != nil {
		return f.InstallErr
	}
	say("  (fixture) installed %s", f.Name())
	f.State.Installed = true
	f.State.Running = true
	return nil
}

func (f *FakeEmbedRuntime) Pull(_ context.Context, model string, say Reporter) error {
	f.Calls = append(f.Calls, "pull:"+model)
	if f.PullErr != nil {
		return f.PullErr
	}
	say("  (fixture) pulled %s", model)
	f.State.Models = append(f.State.Models, model)
	return nil
}
