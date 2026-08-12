package mcpbridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/apps"
	"github.com/luthor007/relay/relayd/internal/mcp"
)

// Catalogue is what is installed and runnable right now.
//
// It returns [apps.Installed] rather than an on-disk record because that is the
// value capability minting reads: the grant it carries is the intersection of
// what the manifest asked for and what the user consented to, which is what
// decides both what the app can do and — through [AccessFor] and
// [ConsequenceFor] — what its tool is.
//
// "Runnable" is the caller's word to keep true. An app whose container was
// never provisioned (internal/appstore's StateAwaitingRuntime) must not be in
// this list: a tool an agent can see is a tool it will call.
//
// It is called on every tools/list and on every call, so it must be cheap and
// must not block on a network — the same rule [mcp.Provider.Tools] states, for
// the same reason. An implementation backed by a directory should hold the
// parsed records in memory and re-read on change, not stat the disk per agent
// turn.
type Catalogue interface {
	Apps(ctx context.Context) []apps.Installed
}

// CatalogueFunc adapts a function to [Catalogue].
type CatalogueFunc func(ctx context.Context) []apps.Installed

// Apps implements [Catalogue].
func (f CatalogueFunc) Apps(ctx context.Context) []apps.Installed { return f(ctx) }

// Outcome is what one invocation produced, as the bridge needs it.
//
// Deliberately not the runtime's own result type: this package is downstream of
// internal/apps and naming its type here would make the sandbox's internals
// part of the MCP surface. It is also the honest shape — an agent that called
// an app wants what the app said and drew, not a process exit code.
type Outcome struct {
	// Spoken is every sentence the app sent to the speaker, in order.
	Spoken []string
	// Views is every view the app rendered, in order. Already delivered to the
	// phone by the time the outcome exists; they are here because the agent has
	// no screen and [ViewText] is how it reads what the user was shown.
	Views []View
	// Failed is the app's own error message when onTrigger threw. Distinct from
	// the error [Invoker.InvokeApp] returns, which is relayd's problem: one is
	// something the model can react to and the other is a protocol failure.
	Failed string
	// TimedOut says the supervisor stopped it rather than it finishing.
	TimedOut bool
	// Budget is the wall-clock ceiling it was given, for the sentence a timeout
	// produces. Zero when the caller did not say.
	Budget time.Duration
}

// Invoker runs one app.
//
// An interface because internal/apps owns the sandbox, the capability channel
// and the supervisor, and its `startFrame` is unexported by design — nothing
// outside that package can drive a [apps.Host]. Whatever the runtime's own
// entry point ends up being called, satisfying this is a handful of lines, and
// keeping the seam here means the MCP surface does not move when the sandbox
// does.
type Invoker interface {
	// InvokeApp wakes one app with one trigger and waits for it to finish.
	//
	// The returned error is relayd's failure — the process would not start, the
	// channel broke. An app that ran and threw comes back as an [Outcome] with
	// Failed set, because the agent can do something about the second and
	// nothing about the first.
	InvokeApp(ctx context.Context, app apps.Installed, trigger apps.TriggerFrame) (Outcome, error)
}

// InvokerFunc adapts a function to [Invoker].
type InvokerFunc func(ctx context.Context, app apps.Installed, trigger apps.TriggerFrame) (Outcome, error)

// InvokeApp implements [Invoker].
func (f InvokerFunc) InvokeApp(ctx context.Context, a apps.Installed, t apps.TriggerFrame) (Outcome, error) {
	return f(ctx, a, t)
}

// ProviderName is what the audit line calls this provider.
const ProviderName = "apps"

// Options configures a [Provider].
type Options struct {
	// Catalogue is what is installed. Nil contributes nothing.
	Catalogue Catalogue
	// Invoke runs an app. Nil contributes nothing — see [Provider.Tools].
	Invoke Invoker
	Log    *slog.Logger
}

// Provider puts every installed app on the shared MCP bus.
//
// It is an [mcp.Provider] and nothing more specialised, which is the point:
// SYSTEM.md §6.3 says memory and installed apps are tools on the same bus, not
// special cases, so an app tool goes through the same grant check, the same
// spoken confirmation and the same `tool_call` row a connector does.
type Provider struct {
	opts Options
	log  *slog.Logger
}

// New builds a provider. Register it with [mcp.Gateway.Register].
func New(o Options) *Provider {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Provider{opts: o, log: o.Log}
}

// ProviderName implements [mcp.Provider].
func (p *Provider) ProviderName() string { return ProviderName }

// Tools is every installed app that can actually be called, right now.
//
// Three ways an app is left out, and all three are the same rule — **never
// offer a tool that cannot run**:
//
//   - No [Invoker] wired. Nothing on this box can wake an app, so no app is a
//     tool. This is APP-PLATFORM.md §8's build order showing through: step 4
//     depends on step 2, and a bridge that listed tools before the runtime
//     existed would hand every agent a list of things that fail on contact.
//   - No `tool` trigger in the manifest. The author did not say the agent may
//     call it, and the manifest is what was reviewed and what the user was
//     shown.
//   - No description on that trigger, which internal/apps' parser already
//     refuses — checked again rather than assumed, because a tool with an empty
//     description is one the model will call by name and guesswork.
func (p *Provider) Tools(ctx context.Context) []mcp.Tool {
	if p.opts.Invoke == nil {
		// Silently empty *here* and loudly empty in [Provider.Degraded]: a box
		// with apps installed and no runtime is a state a user can be in for a
		// whole milestone, and a log line per tools/list would be noise. The
		// console asks Degraded for the sentence.
		return nil
	}
	if p.opts.Catalogue == nil {
		return nil
	}
	var out []mcp.Tool
	for _, a := range p.opts.Catalogue.Apps(ctx) {
		t, ok := a.Manifest.ToolTrigger()
		if !ok {
			continue
		}
		if strings.TrimSpace(t.Description) == "" {
			p.log.Warn("mcpbridge: an app declares a tool trigger with no description, so it is not offered",
				"app", a.Manifest.ID)
			continue
		}
		app := a
		out = append(out, mcp.Tool{
			Name:        ToolName(app.Manifest.ID),
			Title:       app.Manifest.Name,
			Description: describe(app, t),
			InputSchema: inputSchema(t),
			Connector:   Connector(app.Manifest.ID),
			Access:      AccessFor(app),
			Consequence: ConsequenceFor(app),
			Handler: func(ctx context.Context, c mcp.Call) (mcp.Result, error) {
				return p.run(ctx, app, c)
			},
		})
	}
	return out
}

// Degraded is the sentence a console shows when this provider is contributing
// nothing, and empty when it is working.
//
// It exists because "no apps are tools" has three causes and only one of them
// is "you have not installed any". A screen that shows an empty list for all
// three is the silent degradation this codebase refuses everywhere else.
func (p *Provider) Degraded() string {
	switch {
	case p.opts.Invoke == nil:
		return "this box has no app runtime, so no installed app is exposed to your agent as a tool"
	case p.opts.Catalogue == nil:
		return "the installed-app list is not wired, so no app is exposed to your agent as a tool"
	default:
		return ""
	}
}

// ErrGone is an app that vanished between tools/list and the call.
var ErrGone = errors.New("mcpbridge: that app is no longer installed")

func (p *Provider) run(ctx context.Context, app apps.Installed, c mcp.Call) (mcp.Result, error) {
	// Re-read the catalogue rather than trusting the closure. An app removed
	// between the list and the call must not run, and the window is real: an
	// agent holds a tool list for a whole turn.
	live, ok := p.current(ctx, app.Manifest.ID)
	if !ok {
		return mcp.Result{}, fmt.Errorf("%w: %s", ErrGone, app.Manifest.ID)
	}

	out, err := p.opts.Invoke.InvokeApp(ctx, live, apps.TriggerFrame{
		Type:      apps.TriggerTool,
		Arguments: c.Arguments,
	})
	if err != nil {
		// relayd's failure, not the app's. A protocol error, so the gateway
		// records it as failed and the agent sees an error rather than a result
		// it might quote.
		return mcp.Result{}, fmt.Errorf("mcpbridge: %s could not be run: %w", name(live), err)
	}
	return result(live, out), nil
}

func (p *Provider) current(ctx context.Context, id string) (apps.Installed, bool) {
	for _, a := range p.opts.Catalogue.Apps(ctx) {
		if a.Manifest.ID == id {
			return a, true
		}
	}
	return apps.Installed{}, false
}

// result turns what an app did into what the agent reads.
//
// The projection is the whole of §5's "app UI renders in the phone app" seen
// from the other end: the same view the phone drew natively becomes text,
// because the caller here has no screen. The app wrote neither rendering.
func result(app apps.Installed, out Outcome) mcp.Result {
	structured := map[string]any{"app": app.Manifest.ID}
	if len(out.Spoken) > 0 {
		structured["spoken"] = out.Spoken
	}
	if len(out.Views) > 0 {
		structured["views"] = out.Views
	}

	switch {
	case out.TimedOut:
		structured["status"] = "timed_out"
		return mcp.Result{
			IsError:    true,
			Text:       name(app) + " ran out of its " + budget(out.Budget) + " and was stopped before it finished.",
			Structured: structured,
			Target:     app.Manifest.ID,
		}
	case out.Failed != "":
		structured["status"] = "failed"
		return mcp.Result{
			IsError:    true,
			Text:       name(app) + " could not finish: " + strings.TrimSpace(out.Failed),
			Structured: structured,
			Target:     app.Manifest.ID,
		}
	}

	structured["status"] = "completed"
	text := transcript(out)
	if text == "" {
		// Not an empty string. An app that ran, did its work and said nothing —
		// filed a note, updated its own storage — has a real outcome, and an
		// empty tool result invites the model to decide the call failed.
		text = name(app) + " ran and had nothing to say."
	}
	return mcp.Result{Text: text, Structured: structured, Target: app.Manifest.ID}
}

// transcript is everything the app put in front of the user, in order: the
// views as text, then anything it said that the views did not already carry.
//
// A `speak` block inside a view is delivered through the speaker, so the same
// sentence arrives twice — once as a view block and once in Spoken. Reading it
// to the agent twice would have the model believe the app repeated itself.
//
// The de-duplication runs only *between* the two sources, never inside one: a
// list with two identical rows is an app saying something twice on purpose, and
// collapsing it would misreport what the user was shown.
func transcript(out Outcome) string {
	var lines []string
	drawn := map[string]bool{}
	for _, v := range out.Views {
		for _, line := range split(ViewText(v)) {
			drawn[line] = true
			lines = append(lines, line)
		}
	}
	said := map[string]bool{}
	for _, s := range out.Spoken {
		for _, line := range split(s) {
			if drawn[line] || said[line] {
				continue
			}
			said[line] = true
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func split(s string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

func name(a apps.Installed) string {
	if n := strings.TrimSpace(a.Manifest.Name); n != "" {
		return n
	}
	return a.Manifest.ID
}

func budget(d time.Duration) string {
	if d <= 0 {
		return "time budget"
	}
	return d.Round(time.Second).String() + " budget"
}
