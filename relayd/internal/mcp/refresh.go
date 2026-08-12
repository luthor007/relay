package mcp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/registry"
)

// The tool-list refresh problem — SYSTEM.md §9 problem 6, ORCHESTRATOR.md §4b
// and MEMORY.md §7, all three of which say the same thing:
//
//	agents differ in how they discover and re-read tool lists. Some enumerate
//	once at session start. So granting a connector mid-session may not reach a
//	session already running, and the orchestrator has to either re-announce or
//	restart that session — and say which it did, rather than leaving the user
//	wondering why the thing they just connected is invisible.
//
// This file is that decision, made from what each runtime actually provides
// rather than from a guess:
//
//   - **A connection that can be pushed to gets pushed to.** MCP's own
//     `notifications/tools/list_changed` is the mechanism, and a client that
//     advertised nothing still gets it — the cost of an ignored notification is
//     nothing and the cost of skipping it is an invisible connector. The
//     gateway only claims this when the write actually succeeded.
//   - **Claude Code needs nothing.** ADAPTERS.md §2, verified against the
//     recorded trace: `system/init` carries the tool list and is re-emitted at
//     the head of *every* turn. The new tools arrive with the next thing the
//     user says, and the honest report is to say that rather than to restart a
//     session that did not need it.
//   - **Nothing is restarted behind the user's back on ACP.** ADAPTERS.md §8
//     leaves `loadSession` unprobed on all three ACP runtimes, and
//     registry.RestartPolicy already refuses to substitute a fresh session for
//     a resumed one by default for exactly this reason. A "restart" that loses
//     the conversation to make a tool visible is a worse outcome than the
//     invisible tool. So the planner asks whether the session can be resumed
//     with its history, restarts only if it can, and otherwise says plainly
//     that the user has to do it and why.

// RefreshAction is what the orchestrator did about one session.
type RefreshAction string

const (
	// RefreshNotified means a tools/list_changed notification was delivered on
	// that session's own MCP connection.
	RefreshNotified RefreshAction = "notified"

	// RefreshNextTurn means the runtime re-enumerates its tools on its own and
	// the change arrives without anyone doing anything.
	RefreshNextTurn RefreshAction = "next_turn"

	// RefreshRestarted means the session was restarted, with its history.
	RefreshRestarted RefreshAction = "restarted"

	// RefreshManual means neither worked and the user has to restart it. Detail
	// says why Relay did not do it for them.
	RefreshManual RefreshAction = "manual"

	// RefreshFailed means the attempt was made and did not work.
	RefreshFailed RefreshAction = "failed"
)

// SessionRefresh is one session's outcome.
type SessionRefresh struct {
	Session string        `json:"session"`
	Runtime string        `json:"runtime,omitempty"`
	Action  RefreshAction `json:"action"`
	Detail  string        `json:"detail,omitempty"`
}

// RefreshResult is the whole outcome, plus the sentence that says which was
// done. Note is the part the user hears; without it this is the failure the
// docs name three separate times.
type RefreshResult struct {
	Reason   string           `json:"reason,omitempty"`
	Sessions []SessionRefresh `json:"sessions"`
	Note     string           `json:"note"`
}

// NeedsUser reports whether any session is waiting on a human to restart it.
func (r RefreshResult) NeedsUser() bool {
	for _, s := range r.Sessions {
		if s.Action == RefreshManual || s.Action == RefreshFailed {
			return true
		}
	}
	return false
}

// SessionInfo is one live session, as the refresh planner needs it.
type SessionInfo struct {
	ID      string
	Runtime string
	// CanResume is whether restarting this session would keep its history.
	// False is the safe default and the correct one on ACP until
	// ADAPTERS.md §8's loadSession probe exists.
	CanResume bool
}

// SessionSource is the live session list plus the ability to restart one.
type SessionSource interface {
	LiveSessions() []SessionInfo
	// Restart brings a session back with its history intact. It must return
	// ErrRestartUnavailable rather than starting a fresh session when it cannot.
	Restart(ctx context.Context, id string) error
}

// ErrRestartUnavailable is a session that cannot be restarted without losing
// the conversation.
var ErrRestartUnavailable = errors.New("mcp: this session cannot be restarted without losing its history")

// reEnumeratesEveryTurn reports whether a runtime re-reads its own tool list at
// the head of every turn, so a mid-session change needs nothing from us.
//
// Claude Code is the only yes, and it is verified: ADAPTERS.md §2's recorded
// trace contains two byte-identical `system/init` events, one per turn, each
// carrying the whole tool list.
func reEnumeratesEveryTurn(rt string) bool {
	return rt == string(adapter.ClaudeCode)
}

// refreshDetail is why a runtime that cannot be pushed to is where it is.
func refreshDetail(rt string) string {
	switch adapter.Runtime(rt) {
	case adapter.ClaudeCode:
		return "Claude Code re-sends its whole tool list at the head of every turn, " +
			"so the change is there the next time you say anything"
	case adapter.Codex:
		return "Codex's app-server has no tool-list refresh message Relay can send, " +
			"and this session's MCP connection cannot be pushed to"
	case adapter.OpenClaw, adapter.Hermes, adapter.OpenCode:
		return "ACP tells Relay when its command list changes but gives Relay no way " +
			"to ask for a refresh, and this session's MCP connection cannot be pushed to"
	default:
		return "Relay has no way to tell this session that its tool list changed"
	}
}

// Refresh tells every session it can reach that the tool list moved, and
// reports what it did for the ones it could not.
func (g *Gateway) Refresh(ctx context.Context, reason string) RefreshResult {
	out := RefreshResult{Reason: reason}

	// Connections belonging to sessions that have ended are dead weight, and a
	// refresh is the natural moment to notice.
	if g.opts.Sessions != nil {
		alive := map[string]bool{}
		for _, s := range g.opts.Sessions.LiveSessions() {
			alive[s.ID] = true
		}
		g.Sweep(func(id string) bool { return alive[id] })
	}

	// 1. Push down every connection that has a channel back. A session may have
	//    more than one; one delivered notification is enough for that session.
	notified := map[string]bool{}
	for _, conn := range g.Connections() {
		if !conn.CanNotify() {
			continue
		}
		sess := conn.Session()
		if err := conn.ToolsChanged(); err != nil {
			if sess != "" {
				out.Sessions = append(out.Sessions, SessionRefresh{
					Session: sess, Runtime: conn.Runtime(), Action: RefreshFailed,
					Detail: "the notification could not be delivered: " + err.Error(),
				})
			}
			continue
		}
		if sess == "" {
			// A connection on the shared endpoint that never named a session.
			// It was told; there is no session row to attribute it to, and
			// inventing one would be worse than the gap.
			continue
		}
		if notified[sess] {
			continue
		}
		notified[sess] = true
		out.Sessions = append(out.Sessions, SessionRefresh{
			Session: sess, Runtime: conn.Runtime(), Action: RefreshNotified,
			Detail: "told directly over its own MCP connection",
		})
	}

	// 2. Everything else, by what the runtime itself provides.
	if g.opts.Sessions != nil {
		for _, s := range g.opts.Sessions.LiveSessions() {
			if notified[s.ID] || covered(out.Sessions, s.ID) {
				continue
			}
			out.Sessions = append(out.Sessions, g.refreshOne(ctx, s))
		}
	}

	sort.SliceStable(out.Sessions, func(i, j int) bool {
		if out.Sessions[i].Runtime != out.Sessions[j].Runtime {
			return out.Sessions[i].Runtime < out.Sessions[j].Runtime
		}
		return out.Sessions[i].Session < out.Sessions[j].Session
	})
	out.Note = refreshNote(out.Sessions)
	return out
}

func covered(list []SessionRefresh, id string) bool {
	for _, s := range list {
		if s.Session == id {
			return true
		}
	}
	return false
}

func (g *Gateway) refreshOne(ctx context.Context, s SessionInfo) SessionRefresh {
	r := SessionRefresh{Session: s.ID, Runtime: s.Runtime}

	if reEnumeratesEveryTurn(s.Runtime) {
		r.Action = RefreshNextTurn
		r.Detail = refreshDetail(s.Runtime)
		return r
	}

	if !s.CanResume {
		r.Action = RefreshManual
		r.Detail = refreshDetail(s.Runtime) +
			". Relay did not restart it: this runtime cannot reattach to a session, " +
			"so a restart would hand you a fresh one that has forgotten the conversation"
		return r
	}
	if g.opts.Sessions == nil {
		r.Action = RefreshManual
		r.Detail = refreshDetail(s.Runtime) + ", and nothing is wired to restart it"
		return r
	}
	if err := g.opts.Sessions.Restart(ctx, s.ID); err != nil {
		if errors.Is(err, ErrRestartUnavailable) {
			r.Action = RefreshManual
			r.Detail = refreshDetail(s.Runtime) + ". " + err.Error() +
				", so restarting it is yours to do"
			return r
		}
		r.Action = RefreshFailed
		r.Detail = "the restart failed: " + err.Error()
		return r
	}
	r.Action = RefreshRestarted
	r.Detail = "restarted with its history, because this runtime enumerates its tools once per session"
	return r
}

// refreshNote is the sentence the user hears. It names every outcome that
// happened, in plain words, and it never rolls a manual restart into a count of
// things that were handled.
func refreshNote(list []SessionRefresh) string {
	if len(list) == 0 {
		return "No agent sessions are running, so the next one to start gets the new list."
	}
	by := map[RefreshAction][]SessionRefresh{}
	for _, s := range list {
		by[s.Action] = append(by[s.Action], s)
	}

	var parts []string
	if n := len(by[RefreshNotified]); n > 0 {
		parts = append(parts, fmt.Sprintf("%s told directly and %s it now",
			plural(n, "session"), was(n, "has", "have")))
	}
	if n := len(by[RefreshNextTurn]); n > 0 {
		parts = append(parts, fmt.Sprintf("%s %s it at the start of %s next turn",
			plural(n, "session"), was(n, "picks up", "pick up"), was(n, "its", "their")))
	}
	if n := len(by[RefreshRestarted]); n > 0 {
		parts = append(parts, fmt.Sprintf("%s restarted, with %s history",
			plural(n, "session"), was(n, "its", "their")))
	}
	if rows := by[RefreshManual]; len(rows) > 0 {
		parts = append(parts, fmt.Sprintf("%s cannot be told and %s not restarted, because %s. "+
			"Restart %s when you are ready and the new tools will be there",
			plural(len(rows), "session"), was(len(rows), "was", "were"),
			rows[0].Detail, was(len(rows), "it", "them")))
	}
	if rows := by[RefreshFailed]; len(rows) > 0 {
		parts = append(parts, fmt.Sprintf("%s could not be reached at all (%s)",
			plural(len(rows), "session"), rows[0].Detail))
	}
	return capitalize(strings.Join(parts, "; ")) + "."
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func was(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ------------------------------------------------------- registry binding --

// RegistrySessions adapts the session registry to [SessionSource].
//
// Restart deliberately fails. The registry restarts a session only through its
// own supervisor, on a session that ended by itself, and under a policy whose
// default explicitly refuses to substitute a fresh session for one that cannot
// be resumed (registry.RestartPolicy.FreshOnResumeUnsupported). There is no
// public "restart this healthy session" and adding one from here would be
// asserting a capability ADAPTERS.md §8 says nobody has probed. So the planner
// falls through to RefreshManual and tells the user, which is the behaviour the
// docs ask for.
type RegistrySessions struct {
	Registry *registry.Registry
}

// LiveSessions lists the sessions the registry is driving.
func (r RegistrySessions) LiveSessions() []SessionInfo {
	if r.Registry == nil {
		return nil
	}
	live := r.Registry.Live()
	out := make([]SessionInfo, 0, len(live))
	for _, e := range live {
		out = append(out, SessionInfo{
			ID:      e.ID(),
			Runtime: string(e.Runtime()),
			// Has, not Get: SupportSynthesized and SupportUnknown are both "do
			// not rely on this", and relying on it here costs a conversation.
			CanResume: e.Capabilities().Has(adapter.CapResume),
		})
	}
	return out
}

// Restart always reports that it cannot. See the type comment.
func (r RegistrySessions) Restart(context.Context, string) error {
	return fmt.Errorf("%w: the session registry has no restart that preserves history for a "+
		"session that is still running", ErrRestartUnavailable)
}
