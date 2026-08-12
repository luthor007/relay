package main

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/audit"
	"github.com/luthor007/relay/relayd/internal/connector"
	"github.com/luthor007/relay/relayd/internal/mcp"
	"github.com/luthor007/relay/relayd/internal/orchestrator"
	"github.com/luthor007/relay/relayd/internal/registry"
	"github.com/luthor007/relay/relayd/internal/store"
)

// toolBus is the shared MCP registry, constructed.
//
// ORCHESTRATOR.md §4b's promise is "grant once, works in all five, revoke
// once", and until now the daemon held only the read half: install.Options.Gateway
// was never populated by any production caller, so reconcileMCP enumerated the
// five runtimes' servers and changed nothing. These are the parts that were
// missing on this side of that line — something to serve, and something to
// serve it on.
type toolBus struct {
	gateway *mcp.Gateway
	grants  *connector.Grants
	skills  *orchestrator.SkillBook
}

// newToolBus builds the bus, or explains why it could not.
//
// A nil return is a supported state, the same way a missing vault is: the
// daemon still runs, still routes, still pings. What it loses is the shared
// tool bus, and the log line says so rather than leaving a /mcp/ endpoint that
// answers 404 to five runtimes that were told it is real.
func newToolBus(ctx context.Context, db *store.DB, auditLog audit.Log, reg *registry.Registry, connectors *connector.Set, skillsDir string, log *slog.Logger) *toolBus {
	if db == nil || auditLog == nil {
		return nil
	}

	// The grant store is the only path from "no grant" to "grant", and
	// connector.Grants refuses a request that does not carry an explicit human
	// decision. The gateway has no reference to anything that could grant, so
	// rule 1 — nothing is auto-granted, not on install, not on suggestion, not
	// ever — is a property of the wiring rather than of anyone's manners.
	grants := connector.NewGrants(connector.NewSQLStore(db), auditLog)

	gw := mcp.NewGateway(mcp.Options{
		Name:    "relay",
		Version: version,
		Grants:  grants,
		Audit:   auditLog,
		// Sessions lets the gateway push tools/list_changed to the runtimes
		// that can take it, which is the difference between a mid-session grant
		// being visible and the user being told to restart their agent.
		Sessions: sessionSource(reg),
		Log:      log,
	})

	// The skill book is the first provider on the bus, and the reason the bus
	// is worth building today: a playbook the orchestrator writes once becomes
	// a tool in Claude Code, Codex and the three ACP runtimes, without touching
	// any of their configs again.
	// Persisted, because the point of the feature is that a playbook gets
	// better during use. A book that emptied on every restart would mean the
	// orchestrator rediscovering the same sequence every morning and telling
	// the user each time that it had learned something.
	skills, err := orchestrator.OpenSkillBook(ctx, orchestrator.SkillsIn(db))
	if err != nil {
		// Starting with an empty book on a machine that has skills would
		// silently withdraw tools from every runtime on the bus, which is worse
		// than having no bus: an agent whose tools disappear without
		// explanation is exactly MEMORY.md §7's failure.
		log.Warn("relayd: could not read the skill book; the tool bus is not being served",
			"error", err)
		return nil
	}
	// The second, wider distribution: a directory another agent can be pointed
	// at even when it is not on our bus. See ExportSkillMD.
	skills.ExportDir = skillsDir
	if res := gw.Register(ctx, skills); res.NeedsUser() {
		// A registration that some running session cannot pick up is not a
		// failure — it is a fact the user has to be told, because the
		// alternative is wondering why a tool the console says exists is not
		// in the agent in front of them.
		log.Info("relayd: some running sessions will not see the tool bus until restarted",
			"note", res.Note)
	}

	// The connectors themselves, on the same bus. Without this a grant recorded
	// by accepting a proposal would write a row in the `grant` table and change
	// nothing any of the five runtimes can see — a decision with no effect,
	// which is this codebase's own defect wearing a different hat.
	//
	// Registering is safe by construction and does not weaken rule 1: the
	// gateway filters tools/list by grant and checks the grant again before
	// every call, so a machine with a configured-but-ungranted printer still
	// lists zero tools. That is asserted in cmd/relayd's wiring tests on a
	// daemon that HAS a connector configured, which is where the guarantee
	// newly matters.
	if connectors != nil && len(connectors.All()) > 0 {
		if res := gw.Register(ctx, connectors); res.NeedsUser() {
			log.Info("relayd: some running sessions will not see the configured connectors until restarted",
				"note", res.Note)
		}
	}

	return &toolBus{gateway: gw, grants: grants, skills: skills}
}

// sessionSource adapts the registry to the gateway's view of live sessions.
func sessionSource(reg *registry.Registry) mcp.SessionSource {
	if reg == nil {
		return nil
	}
	return registrySessions{reg: reg}
}

// registrySessions answers the two questions the refresh planner asks: who is
// live, and can this one be brought back with its history intact.
type registrySessions struct{ reg *registry.Registry }

func (r registrySessions) LiveSessions() []mcp.SessionInfo {
	live := r.reg.Live()
	out := make([]mcp.SessionInfo, 0, len(live))
	for _, e := range live {
		row := e.Row()
		out = append(out, mcp.SessionInfo{
			ID:      row.ID,
			Runtime: row.Runtime,
			// Has() is SupportYes and nothing else, so an unprobed ACP runtime
			// reports false rather than optimistic. ADAPTERS.md §8: resume is
			// unverified on three of the five, and the cost of being wrong here
			// is a restart that silently drops the conversation.
			CanResume: e.Capabilities().Has(adapter.CapResume),
		})
	}
	return out
}

// Restart is deliberately unavailable.
//
// Bringing a session back with its history is exactly the resume path
// ADAPTERS.md §8 lists as unverified on three of the five runtimes. Returning
// the sentinel makes the planner tell the user to restart it themselves, which
// is a worse experience and a true one; starting a fresh session and calling it
// a restart would lose the conversation and report success.
func (r registrySessions) Restart(context.Context, string) error {
	return mcp.ErrRestartUnavailable
}

// capabilities adapts the grant store to the orchestrator's [Capabilities].
//
// Note which half of the split each side gets. The orchestrator can see what
// exists and can ask; recording a decision goes through connector.Grants, which
// refuses anything that is not one. The model is never the decider, and there
// is no argument it can pass that would make it one.
func (b *toolBus) capabilities() orchestrator.Capabilities {
	if b == nil {
		return nil
	}
	return &busCapabilities{bus: b}
}

func (b *toolBus) skillBook() orchestrator.Skills {
	if b == nil {
		return nil
	}
	return b.skills
}

// handler is the HTTP surface, or nil when there is no bus to serve.
func (b *toolBus) handler() http.Handler {
	if b == nil {
		return nil
	}
	return b.gateway.HTTPHandler()
}

// setEndpoint records where the bus is actually reachable, once the listener
// has an address. It is what `relay setup` reads to point the five runtimes at
// something that certainly exists.
func (b *toolBus) setEndpoint(addr string) {
	if b == nil || addr == "" {
		return
	}
	b.gateway.SetEndpoint(mcp.HTTPDescriptor("relay", "http://"+addr))
}
