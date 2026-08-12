// Package acp drives OpenClaw, Hermes and OpenCode over the Agent Client
// Protocol — one adapter, three of the five runtimes (ADAPTERS.md §4).
//
// The contract is vendored, not remembered:
// `docs/fixtures/adapters/acp-schema.json` is
// `@zed-industries/agent-client-protocol@0.4.5`'s own schema, indexed method by
// method in `acp-methods.md`. Everything this package does is checkable against
// that file, and `schema_test.go` re-checks the nine invariants from the end of
// `acp-methods.md` on every `go test`, so a re-vendored schema turns a wrong
// adapter into a red build.
//
// # Three runtimes, one protocol, three launch commands
//
//	openclaw acp --session agent:main:main --require-existing
//	hermes acp
//	opencode acp
//
// The differences are configuration, not code ([ConfigFor]):
//
//   - OpenClaw's ACP is a bridge in front of the Gateway, not the source of
//     truth. Its session keys look like `agent:main:main` and the key is an
//     argument to the *process*, so one `openclaw acp` process serves one
//     session. `--require-existing` makes a missing session fail loudly instead
//     of being silently created, which is what routing to a session the
//     registry believes exists needs.
//   - Hermes keeps its sessions in SQLite, so the registry can be reconciled
//     against the runtime's own store after a crash rather than rebuilt from
//     memory.
//   - OpenCode has `opencode stats`; per-turn cost is out-of-band for all three
//     and structurally absent for OpenClaw ([CostPlanFor]).
//
// # Steering is absent, and the fallback is better than queueing
//
// There is no steer, inject or interrupt method in ACP 0.4.5 — `session/prompt`
// and `session/cancel` are the only two levers. [Session.Steer] therefore
// returns an *adapter.UnsupportedError and the caller uses [Session.Deliver]:
//
//   - an addition ("also update the changelog") → [ModeQueue], delivered as its
//     own turn once the running one ends;
//   - a redirect ("no, stop, do X instead") → [ModeRedirect], which cancels and
//     re-prompts.
//
// [Delivery] says which one happened, so the small model can announce it. The
// cancel path is safe because ACP's contract on `session/cancel` is explicit:
// the agent stops model requests, aborts tools, *flushes pending session/update
// notifications*, and only then resolves the original prompt with
// `cancelled`. Nothing observed is lost, and queued additions survive a
// redirect — they sit behind it and go out when the redirect turn ends.
//
// # What this adapter refuses to do
//
// Relay declares `fs.readTextFile`, `fs.writeTextFile` and `terminal` all
// false. That is a decision (§4), not a probe result: advertise them and the
// agent does its file and shell work by calling back into us over RPC, where
// nothing obliges it to also appear in `session/update`. Declining them keeps
// every edit and every command visible as a `tool_call` we can narrate. An
// `fs/*` or `terminal/*` request that arrives anyway is answered `-32601` —
// visibly refused, never faked. Unknown `_`-prefixed extension methods are
// logged and counted ([Session.Extensions], [Adapter.Extensions]) rather than
// dropped, because that log line is how we would find out a runtime shipped its
// own steering extension.
//
// # No cost, no tokens
//
// ACP 0.4.5 has no token, cost or usage field anywhere. [event.TurnCompleted]
// carries a nil Usage unless an out-of-band [CostSource] is wired in, so the
// console shows a gap instead of claiming a free turn.
package acp
