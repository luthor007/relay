package acp

import (
	"context"
	"fmt"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/event"
)

// RuntimeConfig is everything that differs between the three ACP runtimes.
// The differences are configuration of one adapter, not three adapters
// (ADAPTERS.md §4).
type RuntimeConfig struct {
	Runtime adapter.Runtime

	// Binary and Args are the launch command, before any session flags.
	Binary string
	Args   []string

	// SessionScopedProcess is true when the ACP process is bound to one
	// session by an argument at launch, so a second session needs a second
	// process. OpenClaw's bridge is: `openclaw acp --session agent:main:main`.
	SessionScopedProcess bool
	// SessionFlag is the flag that carries the key, "" when there is none.
	SessionFlag string
	// DefaultSessionKey is the shape of that key. OpenClaw's look like
	// "agent:main:main".
	DefaultSessionKey string
	// RequireExistingFlag makes a missing session fail loudly instead of being
	// silently created — what routing to a session the registry believes exists
	// needs.
	RequireExistingFlag string

	// BridgeOnly records that this runtime's ACP endpoint is a front for
	// something else, so its session list is not the source of truth.
	BridgeOnly bool
	// OwnStore is where the runtime keeps its own sessions, for reconciling the
	// registry after a crash. Empty when there is none we know of.
	OwnStore string

	// Cost is where per-turn money would have to come from. Never the protocol:
	// ACP 0.4.5 has no cost, token or usage field at all.
	Cost CostPlan
}

// CostPlan is ADAPTERS.md §8 item 3 as data. InProtocol is false for all three
// and always will be until the schema grows a usage object; what differs is
// whether the runtime has an out-of-band store and whether anyone has looked.
type CostPlan struct {
	Runtime adapter.Runtime
	// InProtocol is whether the wire protocol reports cost. Always false here.
	InProtocol bool
	// Source names the out-of-band store, "" when none is known to exist.
	Source string
	// Detail is what the console says next to a missing figure, so "no cost
	// data" reads as a fact rather than as a bug.
	Detail string
	// Verified is whether anybody has read that store on a real install.
	Verified bool
	// Support is the capability level this plan justifies.
	Support adapter.Support
}

// CostPlanFor is the per-runtime answer. It is deliberately not uniform:
// OpenCode has `opencode stats`, Hermes stores estimated_cost_usd and
// actual_cost_usd per session in SQLite, and no OpenClaw equivalent has been
// found. Reporting a figure for OpenClaw would be a lie; reporting a figure for
// the other two without having read their stores would be a guess.
func CostPlanFor(r adapter.Runtime) CostPlan {
	switch r {
	case adapter.OpenCode:
		return CostPlan{
			Runtime: r, Source: "opencode stats",
			Detail:   "cost is out-of-band: `opencode stats` exists but is install-level rather than per turn, and nobody has run it against a real install (ADAPTERS.md §8 item 3)",
			Verified: false, Support: adapter.SupportUnknown,
		}
	case adapter.Hermes:
		return CostPlan{
			Runtime: r, Source: "hermes SQLite: estimated_cost_usd / actual_cost_usd per session",
			Detail:   "cost is out-of-band: Hermes stores estimated_cost_usd and actual_cost_usd per session in its own SQLite, unread so far (ADAPTERS.md §8 item 3)",
			Verified: false, Support: adapter.SupportUnknown,
		}
	case adapter.OpenClaw:
		return CostPlan{
			Runtime: r, Source: "",
			Detail:   "no per-session cost store has been found in OpenClaw at all, and ACP carries none — this runtime reports no cost, and the console shows a gap rather than a zero (ADAPTERS.md §8 item 3)",
			Verified: false, Support: adapter.SupportNo,
		}
	}
	return CostPlan{Runtime: r, Support: adapter.SupportUnknown, Detail: "not an ACP runtime"}
}

// ConfigFor returns the launch configuration for one of the three ACP
// runtimes. ok is false for anything else.
func ConfigFor(r adapter.Runtime) (RuntimeConfig, bool) {
	switch r {
	case adapter.OpenClaw:
		return RuntimeConfig{
			Runtime:              r,
			Binary:               "openclaw",
			Args:                 []string{"acp"},
			SessionScopedProcess: true,
			SessionFlag:          "--session",
			DefaultSessionKey:    "agent:main:main",
			RequireExistingFlag:  "--require-existing",
			BridgeOnly:           true,
			OwnStore:             "the OpenClaw Gateway (agents/<agent>/sessions/sessions.json under a relocatable state dir)",
			Cost:                 CostPlanFor(r),
		}, true
	case adapter.Hermes:
		return RuntimeConfig{
			Runtime:  r,
			Binary:   "hermes",
			Args:     []string{"acp"},
			OwnStore: "hermes SQLite (`hermes sessions list|export`), so the registry can be reconciled against it after a crash",
			Cost:     CostPlanFor(r),
		}, true
	case adapter.OpenCode:
		return RuntimeConfig{
			Runtime: r,
			Binary:  "opencode",
			Args:    []string{"acp"},
			Cost:    CostPlanFor(r),
		}, true
	}
	return RuntimeConfig{}, false
}

// Runtimes lists the three runtimes this adapter serves.
func Runtimes() []adapter.Runtime {
	return []adapter.Runtime{adapter.OpenClaw, adapter.Hermes, adapter.OpenCode}
}

// argv builds the launch command for a dial.
func (rc RuntimeConfig) argv(sessionKey string, requireExisting bool) (string, []string) {
	args := append([]string(nil), rc.Args...)
	if rc.SessionScopedProcess && rc.SessionFlag != "" {
		key := sessionKey
		if key == "" {
			key = rc.DefaultSessionKey
		}
		if key != "" {
			args = append(args, rc.SessionFlag, key)
		}
	}
	if requireExisting && rc.RequireExistingFlag != "" {
		args = append(args, rc.RequireExistingFlag)
	}
	return rc.Binary, args
}

// TurnInfo is what a CostSource is told about a finished turn. There is nothing
// from the protocol in it because the protocol has nothing.
type TurnInfo struct {
	SessionID  string // Relay's id
	Native     string // the runtime's session id
	TurnID     string
	StopReason event.StopReason
}

// CostSource is the out-of-band metering hook. Nothing in this package
// implements one: reading Hermes's SQLite or shelling out to `opencode stats`
// needs a real install to verify against, and an adapter must never emit a
// figure it cannot observe. Wire one in and TurnCompleted starts carrying a
// Usage; leave it nil and Usage stays nil, which is what the console renders as
// a gap.
type CostSource interface {
	// TurnCost returns usage for a finished turn, or (nil, nil) when it cannot
	// say. Returning a zeroed Usage instead of nil would claim a free turn.
	TurnCost(ctx context.Context, info TurnInfo) (*event.Usage, error)
	// Describe is the one-line provenance the console shows beside a figure.
	Describe() string
}

// costNote is what the capability descriptor says about money on this runtime.
func costNote(rc RuntimeConfig, src CostSource) (adapter.Support, string) {
	if src != nil {
		return adapter.SupportYes, "out-of-band metering: " + src.Describe()
	}
	if rc.Cost.Source == "" {
		return rc.Cost.Support, rc.Cost.Detail
	}
	return rc.Cost.Support, fmt.Sprintf("%s — no CostSource is wired, so TurnCompleted.Usage stays nil", rc.Cost.Detail)
}
