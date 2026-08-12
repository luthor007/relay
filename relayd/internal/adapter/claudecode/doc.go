// Package claudecode drives Claude Code over its stream-json protocol.
//
// ADAPTERS.md §2 is the spec, and every claim in it was probed live against
// Claude Code 2.1.226. `docs/fixtures/adapters/claude-code.trace.json` is a real
// recorded 49-event session; TestTraceFixture replays that file through this
// package and compares the normalized events against a golden file, so a format
// change breaks CI rather than someone's morning.
//
// Nothing here parses terminal output. The whole protocol is NDJSON on stdout,
// NDJSON on stdin, and one MCP tool call for approvals.
//
// # The shape of the runtime
//
// One process per session. We name the session — `--session-id` takes a UUID we
// generate — rather than discovering its name afterwards, and `--resume` (with
// `--fork-session` to branch) reattaches later.
//
// A turn is injected by writing one line to the live process's stdin:
//
//	{"type":"user","message":{"role":"user","content":[{"type":"text","text":"…"}]}}
//
// That is what makes "continue an existing session" real for this runtime, and
// it is the same wire message for a new turn and for a mid-turn steer — which is
// why CapSteer is SupportYes here and SupportNo on ACP. With
// `--replay-user-messages` the line is echoed back carrying `"isReplay": true`,
// and that echo is the free acknowledgement that the turn was accepted. This
// adapter treats it as the observable start of the turn, because it is the only
// event in the protocol that means "your turn is now the running one":
// `system/status{"requesting"}` fires once per *API request* and produced three
// events for two turns in the fixture.
//
// # Three things this adapter deliberately does not do
//
// **It never emits PlanUpdated.** ADAPTERS.md §5 marks the cell ✗ for Claude
// Code and suggests synthesising a plan from tool activity; the same section
// says an adapter must never emit an event it cannot observe. Those are in
// tension and this package resolves it by not emitting the event at all —
// [Adapter.Capabilities] reports CapPlan as SupportNo with the reason attached,
// and narration falls back to the tool stream, which is what §5 prescribes for a
// ✗ cell. The reasoning is written into ADAPTERS.md §5 under "Claude Code and
// PlanUpdated"; the short version is that a plan inferred from tool calls is
// retrospective (it describes what already ran), is strictly redundant with the
// ToolStarted/ToolOutput events the orchestrator already has, and would launder
// inference into a structure the small model is instructed to trust more than
// the events it was made of. Anything richer would require reading the model's
// prose, which is the one thing SYSTEM.md forbids outright.
//
// **It never sends `updatedPermissions`.** The permission-prompt response
// supports a standing grant; ORCHESTRATOR.md §4b requires consequential actions
// to be confirmed every time, so the field is never populated and no
// allow_always option is ever offered.
//
// **It never nudges anyone toward a bypass mode.** Our needs-input path requires
// permission checks to be *on*: with `permissions.defaultMode: "auto"` — or any
// auto/bypass `--permission-mode` — the permission-prompt tool is never called,
// the tool just runs, and the failure presents to a user as "the glasses never
// ask me anything", which reads as a feature until something destructive runs
// unattended. This package therefore (a) always passes `--setting-sources ""`
// and an explicit non-auto `--permission-mode`, (b) ships [ScanSettings], a
// first-class detector over the user's settings files, and (c) re-reads
// `permissionMode` from `system/init` at the head of every turn and drops
// CapNeedsInput to SupportNo the moment the runtime says the checks are off.
// (c) is the authoritative one: it is what the runtime actually did, not what
// we asked for.
//
// # Reading the code
//
//	wire.go        the stream-json envelope, as Go types
//	normalize.go   the state machine: wire events → ADAPTERS.md §5's nine events
//	args.go        the command line, and the permission-mode classification
//	hazard.go      the settings.json detector for the silent trap
//	permission.go  the MCP server that --permission-prompt-tool calls
//	launcher.go    process abstraction, so tests drive the fixture not a binary
//	adapter.go     Adapter: spawn, resume, capabilities
//	session.go     Session: send, steer, cancel, and the observation accessors
package claudecode
