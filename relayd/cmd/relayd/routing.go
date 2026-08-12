package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/api"
	"github.com/luthor007/relay/relayd/internal/config"
	"github.com/luthor007/relay/relayd/internal/connector"
	"github.com/luthor007/relay/relayd/internal/detect"
	"github.com/luthor007/relay/relayd/internal/orchestrator"
	"github.com/luthor007/relay/relayd/internal/registry"
	"github.com/luthor007/relay/relayd/internal/routing"
	"github.com/luthor007/relay/relayd/internal/summarize"
)

// utteranceRouter is the seam between the socket and the router.
//
// The API's job is to carry the utterance; deciding what it means belongs to
// internal/routing. This is the piece that was missing between them —
// routing.Router was written, tested, and constructed by nothing, so no spoken
// sentence could reach it, and api.New answered every utterance by naming the
// milestone that would.
type utteranceRouter struct {
	router *routing.Router
	// orch is ORCHESTRATOR.md §3b's second half: the router decides which
	// session, this decides which model. Nil is supported — an install with no
	// keys still answers the allowlist.
	orch *orchestrator.Orchestrator
	// proposer is ORCHESTRATOR.md §4b's evidence feed. Nil is the normal state
	// on a machine with no connectors configured, and it must stay cheap: this
	// runs on every final utterance.
	proposer *connector.Proposer
	// apps offers the sentence to APP-PLATFORM.md §4's phrase triggers. Nil is
	// the normal state on a box with no Node.
	apps *appPlatform
	log  *slog.Logger
	now  func() time.Time

	// The router announces through the server and the server is built with the
	// router, so one of the two has to be set afterwards. SetPinger already
	// establishes that shape in this package.
	mu    sync.RWMutex
	speak func(text, sessionID string)
}

func (u *utteranceRouter) setSpeaker(f func(text, sessionID string)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.speak = f
}

func (u *utteranceRouter) say(text, sessionID string) {
	u.mu.RLock()
	f := u.speak
	u.mu.RUnlock()
	if f != nil && text != "" {
		f(text, sessionID)
	}
}

// Utterance routes one recognised sentence and speaks the decision.
//
// ORCHESTRATOR.md §4's second guarantee: say what it chose, in one clause,
// *before* acting. SYSTEM.md §7b explains why that ordering is the point rather
// than a nicety — the announcement is also the acknowledgement. The agent takes
// 1–10 s while everything else totals under 1.2 s, so the alternative to
// speaking now is a silence long enough to read as broken, and a wrong route
// caught in the same breath is one the human can still stop.
func (u *utteranceRouter) Utterance(ctx context.Context, in api.Utterance) error {
	// Partials exist so the prompt is ready the moment they stop talking
	// (SYSTEM.md §7b). Routing one would announce a decision about half a
	// sentence.
	if !in.Final {
		return nil
	}

	decision, err := u.router.Route(ctx, routing.Request{
		Text:       in.Text,
		At:         u.now(),
		Confidence: in.Confidence,
		Source:     in.Source,
	})
	if err != nil {
		return err
	}

	u.say(routing.Announce(decision), decision.Session)

	// ORCHESTRATOR.md §4b's evidence, from the one source that exists today.
	// §4b hangs the feature on the capture pipeline's extraction step, which is
	// M2 and hardware-blocked; what is drivable now is the sentence the user
	// just said, and "you have mentioned your Prusa four times this week" is
	// closer to a mention than to an agent-turn summary anyway.
	//
	// The proposer keeps no text — it matches, counts the occasion and discards
	// the sentence — so nothing spoken is stored or repeated back.
	u.observe(decision, in.Text)

	// And the installed apps, after routing rather than before it. A phrase
	// match is an *addition* to what the orchestrator decided, never a
	// substitute: an app must not be able to take an utterance away from the
	// agent session the user was talking to by claiming a common word.
	u.apps.phrase(ctx, in.Text)

	u.log.Info("relayd: routed an utterance",
		"kind", decision.Kind,
		"session", decision.Session,
		"runtime", decision.Runtime,
		"automatic", decision.Automatic)

	// The announcement above is already the acknowledgement — §4's rule and
	// §3b's "immediate on it" are the same sentence, spoken once. So the
	// orchestrator's own ack is dropped here and only the outcome is spoken;
	// two acknowledgements for one utterance is worse than none.
	if u.orch != nil && routing.Escalates(in.Text) {
		out, err := u.orch.Handle(ctx,
			orchestrator.Utterance{Text: in.Text, Session: decision.Session},
			func(s summarize.Speech) {
				if s.Moment == summarize.MomentAck {
					return
				}
				u.say(s.Text, decision.Session)
			})
		if err != nil {
			// The user has already heard the outcome line; this is for the
			// operator, and a failed run must not fail the utterance.
			u.log.Warn("relayd: the work model did not finish",
				"session", decision.Session, "turns", out.Result.Turns, "error", err)
		}
	}

	return nil
}

// utteranceEpisode is how long one unattributed occasion lasts.
//
// connector.DefaultMinEpisodes counts *separate conversations*, and its comment
// is explicit that saying "Prusa" four times inside one rant is one occasion and
// treating it as four is how a suggestion engine becomes a nuisance. Routing
// leaves Decision.Session empty for KindNew and KindAsk, and an empty episode
// makes the proposer key each sighting by its own nanosecond timestamp — so the
// naive wiring turns exactly that rant into four episodes. Ten minutes is longer
// than a burst of related sentences and far shorter than the gap between two
// times someone sits down to work.
const utteranceEpisode = 10 * time.Minute

// observe feeds one spoken sentence to the proposer.
func (u *utteranceRouter) observe(decision routing.Decision, text string) {
	if u.proposer == nil || text == "" {
		return
	}
	at := u.now()
	episode := decision.Session
	if episode == "" {
		episode = "utterance/" + at.UTC().Truncate(utteranceEpisode).Format(time.RFC3339)
	}
	u.proposer.Observe(connector.Evidence{Episode: episode, At: at, Text: text})
}

// newUtteranceRouter builds the router and its seam, or explains why it could not.
//
// A nil handler is a supported state rather than a failure: api.New already
// answers an utterance it cannot route by naming the milestone that will, which
// is a better shape than a daemon refusing to start because one subsystem is
// unavailable.
func newUtteranceRouter(
	ctx context.Context,
	reg *registry.Registry,
	orch *orchestrator.Orchestrator,
	ents routing.Entitlements,
	proposer *connector.Proposer,
	log *slog.Logger,
) *utteranceRouter {
	sessions := routing.FromRegistry(reg)

	// MEMORY.md §8's second question, constructed at last. Without this,
	// Options.Runtime is nil, Router.chooseRuntime returns nil unless the user
	// named a runtime out loud, and Decision.Runtime is only ever a copy of an
	// existing session's — so the entitlement table could not have fired even
	// with a set recorded.
	var runtimeRouter *routing.RuntimeRouter
	rr, err := routing.NewRuntimeRouter(routing.RuntimeOptions{
		Profiles:     newRuntimeProfiles(ctx, reg, sessions, log),
		Entitlements: ents,
		// Preferences stays nil (NoPreferences). MEMORY.md §8 step 2's learned
		// half reads "always uses Codex for Rust" out of the facts tier, and
		// nothing yet writes a fact of that shape — wiring a reader onto an
		// empty producer is the defect this branch keeps finding, so it waits.
	})
	if err != nil {
		// Degrade to the shipped behaviour rather than failing the daemon: the
		// session router still works, new sessions simply carry no runtime.
		log.Warn("relayd: no runtime router; new sessions will not name a runtime",
			"error", err)
	} else {
		runtimeRouter = rr
	}

	router, err := routing.New(routing.Options{
		Sessions: sessions,
		Runtime:  runtimeRouter,
		// Auto stays off. ORCHESTRATOR.md §4 ships the manual path plus the
		// announcement first, deliberately: a router that is right 80% of the time
		// and silent about it is worse than one that asks, and there is no real
		// usage to tune the scorer against yet.
		Log: log,
	})
	if err != nil {
		log.Warn("relayd: no router; spoken utterances will say so rather than guess",
			"error", err)
		return nil
	}
	return &utteranceRouter{router: router, orch: orch, proposer: proposer, log: log, now: time.Now}
}

// ------------------------------------------------------- MEMORY.md §8's facts

// Health-screen names for MEMORY.md §8's two halves.
const (
	// SubsystemRuntimeRouting is whether question 2 can be answered at all.
	SubsystemRuntimeRouting = "runtime_routing"
	// SubsystemEntitlements is what the user declared they pay for.
	SubsystemEntitlements = "entitlements"
)

// entitlements reads the declared set out of the config.
//
// config.Load already refuses an id this build does not know, naming it and
// printing the valid list, so an unrecognised entry cannot normally reach here.
// It is filtered again anyway — against routing's own table rather than
// config's copy of the list — because the two live in different packages by
// necessity (routing -> llm -> config makes the import a cycle) and a set that
// silently contained a row the router has no case for would decide nothing and
// say nothing.
func entitlements(cfg config.Config, log *slog.Logger) routing.Entitlements {
	var out routing.Entitlements
	for _, id := range cfg.Routing.Entitlements {
		e := routing.Entitlement(id)
		if _, ok := routing.Row(e); !ok {
			log.Warn("relayd: ignoring an entitlement with no row in the routing table",
				"entitlement", id)
			continue
		}
		out = append(out, e)
	}
	return routing.SortEntitlements(out)
}

// runtimeRoutingStatus reports whether a new session can be given a runtime.
func runtimeRoutingStatus(u *utteranceRouter) string {
	if u == nil || u.router == nil {
		return "no router, so no utterance is routed at all"
	}
	if !u.router.RoutesRuntimes() {
		return "no runtime router, so a new session is announced without one"
	}
	return "on"
}

// entitlementStatus is what the router will actually consult.
//
// An empty set is reported as off *with the reason*, not as a fault: MEMORY.md
// §8 says the set starts empty and step 3 is then skipped with a note rather
// than guessed at. "off" here means nobody has told us, which is the truth on
// every machine installed before `relay entitlements` existed.
func entitlementStatus(u *utteranceRouter) string {
	if u == nil || u.router == nil || !u.router.RoutesRuntimes() {
		return "no runtime router, so entitlements cannot decide anything"
	}
	ents := u.router.Entitlements()
	if ents.Empty() {
		return "none recorded; routing falls back to capability and load"
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, string(e))
	}
	return "on: " + strings.Join(names, ", ")
}

// ---------------------------------------------- MEMORY.md §8's machine picture

// profileTTL is how long a detection report is reused.
//
// Detection is not free: it runs one `<binary> --version` per installed runtime
// (detect.CommandTimeout caps each at 10s) and stat-walks every runtime's store,
// which MEMORY.md §1 measured at ~3.6 GB on a real machine. Ten minutes is far
// shorter than the rate at which a runtime is installed or uninstalled and far
// longer than a conversation, so no utterance ever pays for it.
const profileTTL = 10 * time.Minute

// firstScanWait is how long an utterance will wait for the very first scan.
//
// ORCHESTRATOR.md §3b's whole subject is that silence reads as broken, and the
// budget for everything before the agent starts is about 1.2 s. So the first
// utterance after a cold start does not block on a detection pass that could
// take tens of seconds — it gives up, the router falls back to naming no
// runtime, and the daemon behaves exactly as it did before this join. Slower
// and correct beats a fabricated profile; silent and correct does not.
const firstScanWait = 3 * time.Second

// runtimeProfiles answers "what is on this machine, and what is it doing".
//
// It is the source routing.RuntimeRouter never had in production:
// routing.FromDetect and routing.LiveLoad were both written, both tested, and
// had no non-test caller, because cmd/relayd never imported internal/detect at
// all — only cmd/relay, the installer, did.
type runtimeProfiles struct {
	reg      *registry.Registry
	sessions routing.Sessions
	log      *slog.Logger

	// daemon is the daemon's own lifetime, used for background rescans. A
	// refresh must not be cancelled because the utterance that triggered it
	// finished first.
	daemon context.Context

	ready chan struct{}
	once  sync.Once

	mu       sync.Mutex
	rep      detect.Report
	at       time.Time
	scanning bool
}

func newRuntimeProfiles(
	ctx context.Context,
	reg *registry.Registry,
	sessions routing.Sessions,
	log *slog.Logger,
) func(context.Context) ([]routing.RuntimeProfile, error) {
	p := &runtimeProfiles{
		reg: reg, sessions: sessions, log: log,
		daemon: ctx,
		ready:  make(chan struct{}),
	}
	go p.scan()
	return p.Profiles
}

// scan runs one detection pass.
//
// SkipProcesses is true and SkipSizes is false, and neither is arbitrary.
// routing.FromDetect reads f.Running only to set LastUsed, which no scoring
// step consults, so the process table is a `ps` for nothing. The sizes are the
// opposite: routing.historyOf reads Sessions and StoreBytes, and with sizes
// skipped every runtime comes back HistoryUnknown — which the never-route rule
// turns into "ask", forever, on every new session. It looks like the cheap
// optimisation and it is the one that breaks the feature.
func (p *runtimeProfiles) scan() {
	defer p.once.Do(func() { close(p.ready) })

	// One known limitation, recorded rather than papered over: detect.Options
	// has no field for a state directory somebody already told us about — only
	// OpenClawProfile and OpenClawDev — so a relocated OpenClaw store is
	// resolved by the documented default, which detect marks as a guess. That
	// runtime then comes back HistoryUnknown and the never-route rule excludes
	// it. Excluding a runtime we cannot honestly measure is the safe direction;
	// passing the guess off as a fact is what detect.StateSource exists to
	// prevent.
	rep := detect.Detect(p.daemon, detect.OS(), detect.Options{SkipProcesses: true})

	p.mu.Lock()
	p.rep, p.at, p.scanning = rep, time.Now(), false
	p.mu.Unlock()
}

func (p *runtimeProfiles) Profiles(ctx context.Context) ([]routing.RuntimeProfile, error) {
	select {
	case <-p.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(firstScanWait):
		return nil, fmt.Errorf("relayd: the first runtime scan has not finished yet")
	}

	p.mu.Lock()
	rep, at := p.rep, p.at
	stale := !p.scanning && time.Since(at) > profileTTL
	if stale {
		p.scanning = true
	}
	p.mu.Unlock()
	if stale {
		go p.scan()
	}

	// Attached is read now rather than at scan time, so a runtime whose adapter
	// came up after boot counts immediately. Installed-but-not-attached is a
	// runtime we can name and cannot drive, which the router rejects with that
	// reason rather than silently.
	attached := map[adapter.Runtime]bool{}
	for _, rt := range p.reg.Runtimes() {
		attached[rt] = true
	}

	profiles := routing.FromDetect(rep, attached)

	live, err := p.sessions.Live(ctx)
	if err != nil {
		// The machine picture is still true; only the load half is missing.
		// Reporting no runtimes at all because the session list hiccuped would
		// turn a degraded answer into no answer.
		p.log.Warn("relayd: routing load is stale; the session list did not answer", "error", err)
		return profiles, nil
	}
	return routing.LiveLoad(profiles, live), nil
}

// utteranceHandler converts a possibly-nil *utteranceRouter into the interface.
//
// A typed nil in an interface is not nil, and api.New checks Utterances against
// nil to decide whether it can route at all. Without this, a daemon that failed
// to build a router would report that it could route and then dereference a nil
// pointer on the first thing anybody said.
func utteranceHandler(u *utteranceRouter) api.UtteranceHandler {
	if u == nil {
		return nil
	}
	return u
}

// SetApps attaches the app platform.
//
// Set after construction because the platform needs the API server — apps draw
// on the phone through it — and the server needs a router. Somebody has to be
// second, and a nil platform is a supported state that answers no phrase.
func (u *utteranceRouter) SetApps(p *appPlatform) {
	if u == nil {
		return
	}
	u.apps = p
}
