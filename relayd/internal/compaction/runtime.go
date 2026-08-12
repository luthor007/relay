package compaction

import (
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/luthor007/relay/relayd/internal/adapter"
)

// MechanismKind is how a runtime is asked to compact.
type MechanismKind string

const (
	// MechanismCall is a protocol method. Codex's thread/compact/start takes
	// {threadId} and nothing else — no threshold, no mode — which is why the
	// policy lives entirely on our side.
	MechanismCall MechanismKind = "call"
	// MechanismCommand is a slash command sent as an ordinary user turn.
	MechanismCommand MechanismKind = "command"
	// MechanismHTTP is an endpoint outside the agent protocol.
	MechanismHTTP MechanismKind = "http"
)

// Mechanism is how one runtime compacts on demand, and — the field that
// actually changes behaviour — whether we can see it happen.
//
// ADAPTERS.md §5's rule is that an adapter never emits an event it cannot
// observe. The same rule applies to us: where Observable is false we must not
// report "compacted" as an observation. The most we know is that we asked and
// the request did not error, and the only corroboration available is the
// pressure reading dropping on the next turn — which on ACP does not exist at
// all, because the protocol has no token field anywhere in it.
type Mechanism struct {
	Runtime adapter.Runtime
	Kind    MechanismKind
	// Method is the protocol method, the slash command, or the endpoint.
	Method string
	// Observable is whether a completed compaction produces something we can
	// see.
	Observable bool
	// RequiresLease is Hermes, and only Hermes.
	RequiresLease bool
	Note          string
}

// mechanisms is MEMORY.md §9's "trigger on-demand" column, plus what each one
// gives back.
var mechanisms = map[adapter.Runtime]Mechanism{
	adapter.ClaudeCode: {
		Runtime: adapter.ClaudeCode, Kind: MechanismCommand, Method: "/compact",
		Observable: false,
		Note:       "works as a user message in --print/stream-json; stream-json carries no compaction event, so the only corroboration is the next Session.Context() reading",
	},
	adapter.Codex: {
		Runtime: adapter.Codex, Kind: MechanismCall, Method: "thread/compact/start",
		Observable: true,
		Note:       "an item/completed whose item type is contextCompaction; thread/compacted is deprecated",
	},
	adapter.OpenCode: {
		Runtime: adapter.OpenCode, Kind: MechanismHTTP, Method: "POST /session/{id}/summarize",
		Observable: false,
		Note:       "outside ACP entirely, on opencode serve; ACP has no usage object to corroborate it with",
	},
	adapter.Hermes: {
		Runtime: adapter.Hermes, Kind: MechanismCommand, Method: "/compress",
		Observable: false, RequiresLease: true,
		Note: "compression_locks (session_id, holder, expires_at) is a lease with dedicated upstream concurrency tests — take it, do not race it",
	},
	adapter.OpenClaw: {
		Runtime: adapter.OpenClaw, Kind: MechanismCommand, Method: "/compact",
		Observable: false,
		Note:       "ACP, so no usage object; OpenClaw also recovers on overflow independently of the threshold",
	},
}

// MechanismFor returns how to compact a runtime, and false for one we have no
// documented way to drive. A false here is not a dead end: the handoff is ours
// to do regardless of what the runtime supports.
func MechanismFor(rt adapter.Runtime) (Mechanism, bool) {
	m, ok := mechanisms[rt]
	return m, ok
}

// ---------------------------------------------------------------- settings --

// HermesThreshold is what Hermes's compression.threshold is raised to.
//
// Its default is 0.50 — early enough to fight the 70% idle pass constantly,
// which is the one change MEMORY.md §9 says is actually required. 0.90 leaves
// it as a safety net well above our pass and well below its own overflow paths.
const HermesThreshold = 0.90

// CodexHeadroomDivisor and CodexMinHeadroom bound how close
// model_auto_compact_token_limit may get to model_context_window.
//
// Codex's own shipped relationship is 258k under a 272k wall — a gap of about
// 5% — and the divisor reproduces that rather than inventing a tighter one. The
// floor exists so a small window cannot produce a one-token gap.
const (
	CodexHeadroomDivisor = 20
	CodexMinHeadroom     = 4096
)

var (
	// ErrUnknownWindow refuses to write a limit against a wall we do not know.
	// Writing a number that cannot be bounded is precisely how the limit ends up
	// at or above the window.
	ErrUnknownWindow = errors.New("compaction: refusing to write an auto-compact limit without a known model_context_window")

	// ErrWouldDisable is the refusal that keeps four of five runtimes' safety
	// nets switched on.
	ErrWouldDisable = errors.New("compaction: refusing to disable a runtime's own auto-compaction")

	// ErrWouldExceedWindow is the single most dangerous write in the survey.
	ErrWouldExceedWindow = errors.New("compaction: model_auto_compact_token_limit must stay below model_context_window")

	// ErrWouldFightIdlePass refuses a threshold low enough to compact
	// underneath us.
	ErrWouldFightIdlePass = errors.New("compaction: refusing a compression threshold at or below the idle pass")
)

// Limit is a clamped auto-compact limit.
type Limit struct {
	Value   int64
	Clamped bool
	Reason  string
}

// CodexAutoCompactLimit returns a value for model_auto_compact_token_limit that
// is always strictly below window.
//
// This is the one configuration in MEMORY.md §9's whole survey that provably
// converts a graceful pause into a lost thread. ContextWindowExceeded is a
// *distinct terminal error* — "Codex ran out of room in the model's context
// window. Start a new thread…" — and it is separate from the compaction
// trigger, so a limit at or above the window does not compact late, it removes
// compaction and leaves the thread to die.
//
// requested <= 0 means "no explicit request", and the answer is the ceiling.
// Nothing here can return a value greater than or equal to window, for any
// input, and the package's tests fuzz that claim rather than sampling it.
func CodexAutoCompactLimit(requested, window int64) (Limit, error) {
	if window <= 0 {
		return Limit{}, ErrUnknownWindow
	}
	headroom := window / CodexHeadroomDivisor
	if headroom < CodexMinHeadroom {
		headroom = CodexMinHeadroom
	}
	ceiling := window - headroom
	if ceiling <= 0 {
		// A window smaller than the floor headroom. Halving is still strictly
		// below the wall, which is the only invariant that matters, and a window
		// this small is a misconfiguration worth saying out loud.
		ceiling = window / 2
		if ceiling <= 0 {
			return Limit{}, fmt.Errorf("%w: a %d-token window has no room for a compaction trigger", ErrWouldExceedWindow, window)
		}
	}

	if requested <= 0 {
		return Limit{
			Value:  ceiling,
			Reason: fmt.Sprintf("no limit requested; %d leaves %d tokens under the %d-token wall", ceiling, window-ceiling, window),
		}, nil
	}
	if requested > ceiling {
		return Limit{
			Value:   ceiling,
			Clamped: true,
			Reason: fmt.Sprintf("clamped %d to %d: at or near %d, ContextWindowExceeded replaces compaction and the thread is lost",
				requested, ceiling, window),
		}, nil
	}
	return Limit{Value: requested, Reason: fmt.Sprintf("%d is %d tokens under the wall", requested, window-requested)}, nil
}

// CheckCodexLimit is the assertion a config writer makes before writing. It is
// the same rule as [CodexAutoCompactLimit] stated as a predicate, so a writer
// that assembled a value some other way still cannot get it wrong.
func CheckCodexLimit(value, window int64) error {
	if window <= 0 {
		return ErrUnknownWindow
	}
	if value <= 0 {
		return fmt.Errorf("compaction: model_auto_compact_token_limit must be positive, got %d", value)
	}
	if value >= window {
		return fmt.Errorf("%w: %d >= %d", ErrWouldExceedWindow, value, window)
	}
	return nil
}

// Setting is one config key this package is willing to have written, with the
// reason attached. It is data: nothing here opens a file. The installer or the
// registry applies it, and calls [Refuse] first.
type Setting struct {
	Runtime adapter.Runtime
	Key     string
	Int     *int64
	Float   *float64
	Bool    *bool
	Reason  string
	Clamped bool
}

// String renders the setting the way a config diff would show it.
func (s Setting) String() string {
	switch {
	case s.Int != nil:
		return s.Key + " = " + strconv.FormatInt(*s.Int, 10)
	case s.Float != nil:
		return s.Key + " = " + strconv.FormatFloat(*s.Float, 'g', -1, 64)
	case s.Bool != nil:
		return s.Key + " = " + strconv.FormatBool(*s.Bool)
	}
	return s.Key + " = <unset>"
}

// PlanInput is what is known about one runtime's current configuration.
type PlanInput struct {
	Runtime adapter.Runtime
	// ContextWindow is model_context_window, or 0 when it is unknown. Codex's
	// plan refuses to touch anything without it.
	ContextWindow int64
	// CurrentCodexLimit is the model_auto_compact_token_limit already in the
	// file, or 0 for unset.
	CurrentCodexLimit int64
	// CurrentHermesThreshold is compression.threshold already in the file, or 0
	// for unset — Hermes's own default is 0.50.
	CurrentHermesThreshold float64
	// IdleAt is the pass these settings must not fight. Zero takes
	// [DefaultPolicy]'s.
	IdleAt float64
}

// RuntimePlan is the whole answer for one runtime: what to write, what we
// refused to write, and what stays on.
type RuntimePlan struct {
	Runtime  adapter.Runtime
	Settings []Setting
	// Refusals are the writes considered and declined, each with its reason.
	// They are part of the output rather than a log line because "we left your
	// OpenCode reserve alone because nobody has measured it" is a thing the
	// installer should be able to print.
	Refusals []string
	// KeepEnabled lists the keys that must stay switched on, with why. Nothing
	// in this package can produce a setting that turns one of them off.
	KeepEnabled []string
}

// Plan is the compaction configuration for one runtime.
//
// Four of the five need nothing: they already fire at ~95–100% of the window,
// and if we compact at 70% on idle their trigger simply never runs — so leaving
// it enabled costs nothing and buys a safety net for the pass that did not
// happen, which is a machine asleep, a relayd restart, or an hour of talking.
func Plan(in PlanInput) RuntimePlan {
	idleAt := in.IdleAt
	if idleAt <= 0 {
		idleAt = DefaultPolicy().IdleAt
	}

	p := RuntimePlan{Runtime: in.Runtime, KeepEnabled: keepEnabled(in.Runtime)}

	switch in.Runtime {
	case adapter.Codex:
		lim, err := CodexAutoCompactLimit(in.CurrentCodexLimit, in.ContextWindow)
		if err != nil {
			p.Refusals = append(p.Refusals,
				"model_auto_compact_token_limit: "+err.Error()+" — leaving Codex's own default in place")
			break
		}
		if !lim.Clamped && in.CurrentCodexLimit > 0 {
			// It is already safe. Rewriting a value to itself is a config diff
			// nobody asked for.
			p.Refusals = append(p.Refusals, "model_auto_compact_token_limit: already safe at "+strconv.FormatInt(in.CurrentCodexLimit, 10))
			break
		}
		v := lim.Value
		p.Settings = append(p.Settings, Setting{
			Runtime: adapter.Codex,
			Key:     "model_auto_compact_token_limit",
			Int:     &v,
			Reason:  lim.Reason,
			Clamped: lim.Clamped,
		})

	case adapter.Hermes:
		if in.CurrentHermesThreshold >= HermesThreshold {
			p.Refusals = append(p.Refusals, "compression.threshold: already at "+strconv.FormatFloat(in.CurrentHermesThreshold, 'g', -1, 64))
			break
		}
		t := HermesThreshold
		on := true
		p.Settings = append(p.Settings,
			Setting{
				Runtime: adapter.Hermes, Key: "compression.threshold", Float: &t,
				Reason: "Hermes defaults to compressing at 0.50, early enough to fight the " + percent(idleAt) + " idle pass constantly",
			},
			Setting{
				Runtime: adapter.Hermes, Key: "compression.enabled", Bool: &on,
				Reason: "left on: all three overflow paths are guarded by it and return compaction_disabled: true, and its own config comment says to set it false if you want errors on overflow",
			},
		)

	case adapter.OpenCode:
		p.Refusals = append(p.Refusals,
			"compaction.reserved: MEMORY.md §9 calls lowering this optional and nobody has measured whether the default is too eager — left alone")

	case adapter.OpenClaw:
		p.Refusals = append(p.Refusals,
			"agents.defaults.compaction.reserveTokens: same as OpenCode's reserve — optional, unmeasured, left alone")

	case adapter.ClaudeCode:
		p.Refusals = append(p.Refusals,
			"autoCompactWindow: the standing advice is raise the window, never disable it, and the fixture shows no autocompact setting to read — left alone (MEMORY.md §12.4)")
	}
	return p
}

// keepEnabled is the list of switches this package will not turn off, with the
// evidence for each. Two of them fail explicitly in the runtime's own source.
func keepEnabled(rt adapter.Runtime) []string {
	switch rt {
	case adapter.OpenCode:
		return []string{"compaction.auto: with it false a context overflow sets finish: \"error\" and idles the session"}
	case adapter.Hermes:
		return []string{"compression.enabled: all three overflow paths return compaction_disabled: true without it"}
	case adapter.ClaudeCode:
		return []string{"autoCompactEnabled: three layers of graceful degradation depend on it, and whether prompt_too_long recovery survives disabling is unproven (MEMORY.md §12.4)"}
	case adapter.Codex:
		return []string{"model_auto_compact_token_limit: below the window it is a safety net; at or above it, ContextWindowExceeded ends the thread"}
	case adapter.OpenClaw:
		return []string{"agents.defaults.compaction: OpenClaw recovers on overflow independently of the threshold"}
	}
	return nil
}

// disableKeys are the config keys whose falsehood removes a runtime's own
// safety net, per runtime.
var disableKeys = map[adapter.Runtime]map[string]string{
	adapter.OpenCode: {
		"compaction.auto": "a context overflow then sets finish: \"error\" and idles the session instead of compacting and continuing",
	},
	adapter.Hermes: {
		"compression.enabled":  "all three overflow paths are guarded by it and return compaction_disabled: true",
		"compression_enabled":  "all three overflow paths are guarded by it and return compaction_disabled: true",
		"compression.disabled": "same switch, inverted",
	},
	adapter.ClaudeCode: {
		"autoCompactEnabled": "the reactive prompt_too_long recovery is only traced to the window-resolution path, so disabling it may take the recovery with it (MEMORY.md §12.4)",
	},
	adapter.OpenClaw: {
		"agents.defaults.compaction.enabled": "OpenClaw's own overflow recovery is the safety net for a pass we did not get to run",
	},
}

// Refuse is the chokepoint. A config writer calls it before touching any of the
// five runtimes' files, and a nil return is the only permission to write.
//
// It exists because MEMORY.md §9's two load-bearing rules are both of the form
// "never write X", and a rule of that shape enforced by a paragraph is enforced
// until the next person reads a different paragraph.
//
// It checks compression thresholds against [DefaultPolicy]'s idle pass rather
// than against a policy handed in, deliberately: a config file outlives the
// process that wrote it, and a guard that moved with a runtime setting would
// stop being a guard.
func Refuse(rt adapter.Runtime, key string, value any, window int64) error {
	if keys, ok := disableKeys[rt]; ok {
		if why, ok := keys[key]; ok {
			if b, isBool := value.(bool); isBool && !b {
				return fmt.Errorf("%w: %s.%s — %s", ErrWouldDisable, rt, key, why)
			}
			if s, isStr := value.(string); isStr && (s == "false" || s == "off" || s == "no") {
				return fmt.Errorf("%w: %s.%s — %s", ErrWouldDisable, rt, key, why)
			}
		}
	}

	switch {
	case rt == adapter.Codex && key == "model_auto_compact_token_limit":
		n, ok := asInt(value)
		if !ok {
			return fmt.Errorf("compaction: %s.%s must be an integer, got %T", rt, key, value)
		}
		return CheckCodexLimit(n, window)

	case rt == adapter.Hermes && (key == "compression.threshold" || key == "compression_threshold"):
		f, ok := asFloat(value)
		if !ok {
			return fmt.Errorf("compaction: %s.%s must be a number, got %T", rt, key, value)
		}
		if f <= 0 || f > 1 {
			return fmt.Errorf("compaction: %s.%s must be a fraction in (0,1], got %v", rt, key, f)
		}
		if f <= DefaultPolicy().IdleAt {
			return fmt.Errorf("%w: %v would compress underneath the %s idle pass", ErrWouldFightIdlePass, f, percent(DefaultPolicy().IdleAt))
		}
	}
	return nil
}

func asInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), true
	}
	return 0, false
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float32:
		return float64(n), true
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// PlanAll is every runtime's plan, in a stable order, for an installer that
// wants to print one screen.
func PlanAll(in map[adapter.Runtime]PlanInput) []RuntimePlan {
	out := make([]RuntimePlan, 0, len(in))
	for rt, p := range in {
		p.Runtime = rt
		out = append(out, Plan(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Runtime < out[j].Runtime })
	return out
}
