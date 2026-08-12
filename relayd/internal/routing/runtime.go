package routing

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
)

// History is how much this machine has ever used a runtime.
//
// Three values rather than a count, because the interesting distinction is not
// "how much" but "has anyone ever" — and because MEMORY.md §1 measured a real
// machine where two of five runtimes were installed and had never been run.
type History uint8

const (
	// HistoryUnknown means nobody looked. It is the default and it is treated
	// as "do not route here", identically to HistoryNone: a runtime we have not
	// checked is precisely the one the never-route rule is about.
	HistoryUnknown History = iota
	// HistoryNone means installed, never run.
	HistoryNone
	// HistorySome means there are sessions in its own store.
	HistorySome
)

func (h History) String() string {
	switch h {
	case HistoryNone:
		return "none"
	case HistorySome:
		return "some"
	}
	return "unknown"
}

// RuntimeProfile is one runtime, as the router sees it.
type RuntimeProfile struct {
	Runtime adapter.Runtime

	// Installed is a binary on PATH or a driveable adapter.
	Installed bool
	// Attached is true when relayd has an adapter registered and could start a
	// session right now. An installed runtime with no adapter is a runtime we
	// can name but not drive.
	Attached bool

	History History
	// Sessions is how many sessions its own store holds, nil when nobody
	// counted. Same rule as detect.Finding: nil is "not counted" and 0 is
	// "counted and empty", and rendering the first as the second is a lie.
	Sessions *int
	LastUsed time.Time

	// Capabilities is the runtime's baseline descriptor, narrowed by whatever
	// the adapter learned at handshake time.
	Capabilities adapter.Capabilities

	// Tools are the MCP servers and connectors this runtime currently has,
	// which is MEMORY.md §8's capability step. Empty means unknown rather than
	// none: the shared registry (MEMORY.md §7) reconciles this, and a router
	// that reads an unreconciled list as "has nothing" rejects every runtime.
	Tools []string

	// Busy is a turn in flight right now.
	Busy bool
	// Sessions the daemon is driving on this runtime, live.
	LiveSessions int
}

// Usable reports whether a session could be started here at all.
func (p RuntimeProfile) Usable() bool { return p.Installed && p.Attached }

// Used reports whether this machine has ever run it.
func (p RuntimeProfile) Used() bool { return p.History == HistorySome }

// Steerable reports whether a turn in flight can be redirected. Three of the
// five cannot (ADAPTERS.md §4) and the answer comes from the descriptor, never
// from a table of names.
func (p RuntimeProfile) Steerable() bool { return p.Capabilities.Has(adapter.CapSteer) }

// HasTool reports whether this runtime carries a named MCP server. An empty
// tool list answers false and the caller has to treat that as unknown.
func (p RuntimeProfile) HasTool(name string) bool {
	for _, t := range p.Tools {
		if strings.EqualFold(t, name) {
			return true
		}
	}
	return false
}

// Preferences is the learned half of MEMORY.md §8 step 2 — "always uses Codex
// for Rust", read out of the facts tier (MEMORY.md §5).
type Preferences interface {
	// Preferred returns a runtime the user is known to prefer for this work,
	// the evidence for saying so, and whether there is one at all.
	Preferred(ctx context.Context, req RuntimeRequest) (adapter.Runtime, string, bool)
}

// NoPreferences is the default: nothing learned, nothing claimed.
type NoPreferences struct{}

func (NoPreferences) Preferred(context.Context, RuntimeRequest) (adapter.Runtime, string, bool) {
	return "", "", false
}

// StaticPreference is one learned preference, for tests and for a config file.
type StaticPreference struct {
	Runtime  adapter.Runtime
	Evidence string
	// When, if set, gates the preference on the request. Nil always applies.
	When func(RuntimeRequest) bool
}

func (s StaticPreference) Preferred(_ context.Context, req RuntimeRequest) (adapter.Runtime, string, bool) {
	if s.Runtime == "" {
		return "", "", false
	}
	if s.When != nil && !s.When(req) {
		return "", "", false
	}
	return s.Runtime, s.Evidence, true
}

// RuntimeRequest is question 2: where should this work go?
type RuntimeRequest struct {
	// Text is the utterance, for the family sniff and for logging.
	Text      string
	Workspace string

	// Runtime is an explicit choice the user made out loud. It outranks
	// everything except a live session already doing this work, and if it is
	// not usable the router says so rather than quietly substituting another.
	Runtime adapter.Runtime

	// Family is which model family the work wants. Empty means unspecified,
	// which is the normal case.
	Family ModelFamily

	// Continuity is a live session already on this repo and topic. It is step 1
	// of the priority order and the cheapest correct answer available.
	Continuity *SessionView

	// Tools are MCP servers or connectors this work needs (step 4).
	Tools []string
	// NeedsSteering means the work will want mid-turn redirection.
	NeedsSteering bool
}

// RuntimeReason is which step of MEMORY.md §8's priority order decided it.
type RuntimeReason string

const (
	RuntimeContinuity  RuntimeReason = "continuity"
	RuntimeExplicit    RuntimeReason = "explicit preference"
	RuntimeLearned     RuntimeReason = "a learned preference"
	RuntimeEntitlement RuntimeReason = "entitlement"
	RuntimeCapability  RuntimeReason = "capability"
	RuntimeLoad        RuntimeReason = "load"
	// RuntimeOnlyOne is the common single-runtime machine, where there is no
	// decision to make and saying "load balancing" would be theatre.
	RuntimeOnlyOne RuntimeReason = "it is the only one you use"
	// RuntimeNone is no answer: the router asks instead of picking.
	RuntimeNone RuntimeReason = "no runtime could be chosen"
)

// RuntimeChoice is question 2's answer.
type RuntimeChoice struct {
	Runtime adapter.Runtime
	Reason  RuntimeReason
	Because string
	// Entitlement names the row that decided it, when one did.
	Entitlement Entitlement
	// Announcement is the clause spoken before the session is started.
	Announcement string

	// Ask is set when nothing could be chosen. It is a question, not an error:
	// the never-route rule below means "ask" is a correct outcome and picking
	// anyway is not.
	Ask string
	// Rejected is every runtime considered and why it lost, in stable order.
	// This is what the console shows when a user asks why their subscription
	// was not used.
	Rejected []RuntimeRejection
}

// Chosen reports whether a runtime was picked.
func (c RuntimeChoice) Chosen() bool { return c.Runtime != "" }

// RuntimeRejection is one runtime that was not chosen, and why.
type RuntimeRejection struct {
	Runtime adapter.Runtime
	Why     string
}

// RuntimeOptions configures a [RuntimeRouter].
type RuntimeOptions struct {
	// Profiles is the machine's runtimes. Required.
	Profiles func(ctx context.Context) ([]RuntimeProfile, error)

	// Entitlements is what the user pays for. Empty is honest and common: the
	// entitlement step is then skipped with a note rather than guessed.
	Entitlements Entitlements

	// Preferences is the facts tier. Nil means none.
	Preferences Preferences

	Now func() time.Time
}

// RuntimeRouter answers MEMORY.md §8.
type RuntimeRouter struct {
	profiles func(ctx context.Context) ([]RuntimeProfile, error)
	ents     Entitlements
	prefs    Preferences
	now      func() time.Time
}

// NewRuntimeRouter builds one.
func NewRuntimeRouter(o RuntimeOptions) (*RuntimeRouter, error) {
	if o.Profiles == nil {
		return nil, fmt.Errorf("routing: a runtime router needs a profile source")
	}
	if o.Preferences == nil {
		o.Preferences = NoPreferences{}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &RuntimeRouter{profiles: o.Profiles, ents: SortEntitlements(o.Entitlements), prefs: o.Preferences, now: o.Now}, nil
}

// StaticProfiles is a fixed machine, for tests and for a caller that already
// has a detection report.
func StaticProfiles(ps ...RuntimeProfile) func(context.Context) ([]RuntimeProfile, error) {
	return func(context.Context) ([]RuntimeProfile, error) {
		return append([]RuntimeProfile(nil), ps...), nil
	}
}

// Entitlements returns the configured set.
func (r *RuntimeRouter) Entitlements() Entitlements { return r.ents }

// Choose runs MEMORY.md §8's priority order:
//
//  1. Continuity — a live session already on this repo and topic beats a fresh
//     session anywhere. Cheapest and most often correct.
//  2. Explicit preference — stated out loud, or learned from the facts tier.
//  3. Entitlement — the table above.
//  4. Capability — does that runtime have the MCP server this needs?
//  5. Load — is it mid-turn, and can it be steered?
//
// And one rule that outranks all five: **never route to a runtime with no
// history and no explicit preference.** It is applied as a filter before
// entitlement, so a Copilot subscription does not send someone's first voice
// command to a runtime they have never opened.
func (r *RuntimeRouter) Choose(ctx context.Context, req RuntimeRequest) (RuntimeChoice, error) {
	all, err := r.profiles(ctx)
	if err != nil {
		return RuntimeChoice{}, fmt.Errorf("routing: read runtime profiles: %w", err)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Runtime < all[j].Runtime })

	byRuntime := make(map[adapter.Runtime]RuntimeProfile, len(all))
	for _, p := range all {
		byRuntime[p.Runtime] = p
	}
	var rejected []RuntimeRejection
	reject := func(rt adapter.Runtime, why string) {
		rejected = append(rejected, RuntimeRejection{Runtime: rt, Why: why})
	}

	// ---- 1. Continuity.
	if s := req.Continuity; s != nil && s.Runtime != "" {
		p, ok := byRuntime[s.Runtime]
		switch {
		case !ok || !p.Usable():
			reject(s.Runtime, "a session is already on this work but the runtime is not driveable right now")
		default:
			return r.pick(p, RuntimeContinuity,
				"there is already a session on this repo and topic", "", rejected), nil
		}
	}

	// ---- 2. Explicit preference, stated.
	if req.Runtime != "" {
		p, ok := byRuntime[req.Runtime]
		switch {
		case !ok:
			return RuntimeChoice{
				Reason:   RuntimeNone,
				Because:  fmt.Sprintf("%s is not installed on this machine", RuntimeLabel(req.Runtime)),
				Ask:      fmt.Sprintf("%s is not installed here. Use something else?", RuntimeLabel(req.Runtime)),
				Rejected: append(rejected, RuntimeRejection{Runtime: req.Runtime, Why: "not installed"}),
			}, nil
		case !p.Usable():
			return RuntimeChoice{
				Reason:   RuntimeNone,
				Because:  fmt.Sprintf("%s is installed but relayd cannot drive it", RuntimeLabel(req.Runtime)),
				Ask:      fmt.Sprintf("%s is installed but I cannot drive it. Use something else?", RuntimeLabel(req.Runtime)),
				Rejected: append(rejected, RuntimeRejection{Runtime: req.Runtime, Why: "no adapter attached"}),
			}, nil
		default:
			// An explicit choice is the one case where a runtime with no
			// history is allowed. The user asked for it by name, so it is not a
			// first impression we chose for them.
			return r.pick(p, RuntimeExplicit, "you asked for it by name", "", rejected), nil
		}
	}

	// ---- 2b. Explicit preference, learned from the facts tier.
	//
	// This also runs before the never-route filter, and deliberately: the rule
	// is "no history *and* no explicit preference", and MEMORY.md §8 step 2
	// counts a learned preference as an explicit one. "Always uses Codex for
	// Rust" is the user's own habit, evidenced, not load balancing.
	if rt, evidence, ok := r.prefs.Preferred(ctx, req); ok && rt != "" {
		p, found := byRuntime[rt]
		if found && p.Usable() {
			because := "you usually use it for this"
			if evidence != "" {
				because = evidence
			}
			return r.pick(p, RuntimeLearned, because, "", rejected), nil
		}
		reject(rt, "learned preference, but the runtime is not driveable here")
	}

	// ---- the rule that outranks the rest.
	var pool []RuntimeProfile
	for _, p := range all {
		switch {
		case !p.Installed:
			reject(p.Runtime, "not installed")
		case !p.Attached:
			reject(p.Runtime, "installed, but relayd has no adapter for it")
		case !p.Used():
			reject(p.Runtime, "installed but never used — "+neverUsedWhy(p.History))
		default:
			pool = append(pool, p)
		}
	}
	if len(pool) == 0 {
		return r.nothing(all, rejected), nil
	}

	// ---- 3. Entitlement.
	if !r.ents.Empty() {
		sanctions := Sanctioned(r.ents, req.Family)
		var anyRow *EntitlementRow
		for _, s := range sanctions {
			if s.Any {
				row := s.Row
				anyRow = &row
				continue
			}
			p, ok := profileOf(pool, s.Runtime)
			if !ok {
				reject(s.Runtime, string(s.Row.Entitlement)+" points here, but "+
					reasonNotInPool(byRuntime, s.Runtime))
				continue
			}
			switch s.Support {
			case adapter.SupportYes:
				c := r.pick(p, RuntimeEntitlement, s.Row.Because, s.Row.Entitlement, rejected)
				return c, nil
			case adapter.SupportUnknown:
				// Honest degradation: we do not know that this runtime lists
				// that provider, so we do not spend the user's subscription on
				// a guess. It stays a candidate for capability and load.
				reject(s.Runtime, "may front that plan, but nobody has checked: "+s.Note)
			default:
				reject(s.Runtime, "does not front that plan: "+s.Note)
			}
		}
		if anyRow == nil && len(sanctions) > 0 {
			// Entitlements exist but none resolved to a usable runtime. Fall
			// through to capability and load rather than failing — the work
			// still has to go somewhere, it just costs money and the console
			// can say so.
			reject("", "no entitlement resolved to a runtime with history")
		}
	}

	// ---- 4. Capability.
	capable := pool
	for _, need := range req.Tools {
		var next []RuntimeProfile
		for _, p := range capable {
			if p.HasTool(need) {
				next = append(next, p)
			}
		}
		if len(next) == 0 {
			// Nobody advertises it. That is a fact about the shared registry
			// rather than a reason to refuse the work, so the filter is dropped
			// and the gap is recorded.
			reject("", "no runtime advertises "+need+"; the registry may not be reconciled yet")
			continue
		}
		for _, p := range capable {
			if !p.HasTool(need) {
				reject(p.Runtime, "does not have "+need)
			}
		}
		capable = next
	}
	if req.NeedsSteering {
		var next []RuntimeProfile
		for _, p := range capable {
			if p.Steerable() {
				next = append(next, p)
			} else {
				reject(p.Runtime, "cannot be steered mid-turn (ADAPTERS.md §4)")
			}
		}
		if len(next) > 0 {
			capable = next
		}
	}
	if len(capable) == 0 {
		// Unreachable as written — every filter above puts the list back when
		// it would have emptied it — but a router that panics on an empty slice
		// is worse than one that asks, and this is the line that decides which.
		return r.nothing(all, rejected), nil
	}
	if len(capable) == 1 {
		reason := RuntimeCapability
		if len(req.Tools) == 0 && !req.NeedsSteering {
			reason = RuntimeOnlyOne
		}
		return r.pick(capable[0], reason, "it is the one that fits", "", rejected), nil
	}

	// ---- 5. Load.
	best := byLoad(capable)
	return r.pick(best, RuntimeLoad, loadWhy(best), "", rejected), nil
}

func (r *RuntimeRouter) pick(p RuntimeProfile, reason RuntimeReason, because string, ent Entitlement, rejected []RuntimeRejection) RuntimeChoice {
	c := RuntimeChoice{
		Runtime:     p.Runtime,
		Reason:      reason,
		Because:     because,
		Entitlement: ent,
		Rejected:    rejected,
	}
	c.Announcement = RuntimeLabel(p.Runtime)
	return c
}

// nothing is the honest outcome when the never-route rule leaves no candidate.
func (r *RuntimeRouter) nothing(all []RuntimeProfile, rejected []RuntimeRejection) RuntimeChoice {
	var installed []string
	for _, p := range all {
		if p.Usable() {
			installed = append(installed, RuntimeLabel(p.Runtime))
		}
	}
	c := RuntimeChoice{Reason: RuntimeNone, Rejected: rejected}
	switch len(installed) {
	case 0:
		c.Because = "no agent runtime is installed and driveable on this machine"
		c.Ask = "I have no agent installed to run that. Want me to walk through setup?"
	case 1:
		c.Because = installed[0] + " is installed but has never been used here"
		c.Ask = "You have not used " + installed[0] + " yet. Start there?"
	default:
		c.Because = "nothing installed here has been used yet"
		c.Ask = "You have not used " + installed[0] + " or " + installed[1] + " yet. Which should I use?"
	}
	return c
}

// neverUsedWhy renders the two never-used cases differently, because they are
// different: one store was checked and was empty, the other was never checked.
// Reporting the second as the first is the same class of lie as an adapter
// emitting an event it did not observe.
func neverUsedWhy(h History) string {
	if h == HistoryUnknown {
		return "nobody has looked at its store"
	}
	return "its store is empty"
}

func profileOf(ps []RuntimeProfile, rt adapter.Runtime) (RuntimeProfile, bool) {
	for _, p := range ps {
		if p.Runtime == rt {
			return p, true
		}
	}
	return RuntimeProfile{}, false
}

func reasonNotInPool(by map[adapter.Runtime]RuntimeProfile, rt adapter.Runtime) string {
	p, ok := by[rt]
	switch {
	case !ok || !p.Installed:
		return "it is not installed"
	case !p.Attached:
		return "relayd has no adapter for it"
	case !p.Used():
		return "you have never used it"
	}
	return "it is not a candidate"
}

// byLoad picks the least-loaded runtime, preferring one that is idle, then one
// that can be steered if it is busy, then the least recently used.
func byLoad(ps []RuntimeProfile) RuntimeProfile {
	best := ps[0]
	score := loadScore(best)
	for _, p := range ps[1:] {
		if s := loadScore(p); s > score {
			best, score = p, s
		}
	}
	return best
}

func loadScore(p RuntimeProfile) float64 {
	s := 1.0
	if p.Busy {
		// Busy is a real cost: on three of the five runtimes the turn cannot be
		// steered, so a second request has to wait for the first to finish.
		s -= 0.6
		if p.Steerable() {
			s += 0.3
		}
	}
	s -= 0.05 * float64(p.LiveSessions)
	return s
}

func loadWhy(p RuntimeProfile) string {
	switch {
	case !p.Busy:
		return "it is idle"
	case p.Steerable():
		return "it is busy but can take another instruction"
	default:
		return "it is the least loaded of the ones you use"
	}
}
