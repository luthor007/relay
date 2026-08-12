// Package mcp is Relay's MCP gateway: one registry, every runtime pointed at
// it.
//
// SYSTEM.md §6.3 in one sentence — "one MCP gateway, every runtime pointed at
// it; grant once, works everywhere, revoke once, audit in one place; memory and
// installed apps are tools on the same bus, not special cases." ORCHESTRATOR.md
// §4b is the argument for it: a connector granted for Claude Code that Codex
// cannot see teaches the user that "connected" means nothing, and five parallel
// integrations is the alternative.
//
// This package is the write half's missing half. `internal/detect` already
// reads all five runtimes' MCP configuration and `internal/install` already
// knows how to rewrite it, back the originals up and roll them back — but
// MEMORY.md §7 records that the installer deliberately stops short, because
// pointing seven working servers at a gateway that does not exist would leave
// the machine with none. [Gateway] is the thing that has to exist first, and
// [Descriptor] is the value that turns install's switch on.
//
// Four rules are enforced here rather than trusted to callers, because every
// one of them is the kind that decays into a convention and then into a CVE:
//
//   - **Nothing is auto-granted.** [Gateway.Call] asks [Grants] before every
//     call and there is no bypass, no "trusted tool" list and no first-run
//     exemption. A provider with no grant contributes no tools to tools/list at
//     all, so an agent cannot even see what it may not use.
//   - **Read and write are separate grants.** [Access] is part of a tool's
//     identity, and the grant lookup is per half. Reading a calendar is not
//     sending invitations.
//   - **Consequences outside the machine confirm at the glasses, every time.**
//     A tool with a non-empty [Tool.Consequence] goes through [Confirmer]
//     before it runs, and [BusConfirmer] delivers it as an event.NeedsInput —
//     the blocking path that ADAPTERS.md §7 says batching and quiet hours must
//     not touch. A gateway with no confirmer wired **refuses** those calls: an
//     approval nobody could be asked for is not an approval.
//   - **Never claim a refresh that did not happen.** [Gateway.Refresh] answers
//     SYSTEM.md §9 problem 6 with what each runtime actually provides — a real
//     `notifications/tools/list_changed` where the transport can push one,
//     Claude Code's per-turn `system/init` where it cannot, and a named,
//     explained "you have to restart this yourself" where neither works. It
//     never restarts an ACP session behind the user's back, because
//     ADAPTERS.md §8 leaves `loadSession` unprobed on all three and a "restart"
//     there is a fresh session wearing the same name.
//
// The audit trail is the reason the gateway is one place rather than five.
// Grant and revoke mutations go through internal/audit (M3's package, imported
// and not modified); per-call attribution goes into the `tool_call` table that
// DASHBOARD.md §3.4's "last used, and for what" column reads. Where a call
// cannot be attributed to a session row, [Gateway] counts it as unrecorded and
// says so rather than implying a trail it does not have.
package mcp
