// Package codex drives `codex app-server`, the richest of the three protocols
// Relay speaks (ADAPTERS.md §3).
//
// # What the wire actually looks like
//
// JSON-RPC over stdio, with two corrections that cost a day if you guess:
//
//   - **NDJSON.** One JSON object per line, no `Content-Length` headers. Settled
//     from the vendor's own transport source rather than from a probe —
//     `codex-rs/app-server-transport/src/transport/stdio.rs` reads with
//     `BufReader::lines()` and writes with `json.push('\n')` (ADAPTERS.md §8
//     item 6).
//   - **No `jsonrpc` envelope field.** `codex-rs/app-server-protocol/src/rpc.rs`
//     says so in a comment: "We do not do true JSON-RPC 2.0, as we neither send
//     nor expect the `jsonrpc: 2.0` field." So [message] does not carry one, and
//     the decoder demultiplexes on field *presence* because `JSONRPCMessage` is
//     `#[serde(untagged)]`: id+method is a request, method alone a notification,
//     id+result a response, id+error an error.
//
// The three vendored schemas — `docs/fixtures/adapters/{ClientRequest,
// ServerNotification,ServerRequest}.json` from codex-cli 0.140.0 — describe
// *params only*. There is no `ServerResponse.json`, so nothing here reads an
// identifier out of a result. In particular the thread id comes from the
// `thread/started` notification, whose `thread.id` is required, and not from the
// `thread/start` result, whose shape is not in the contract at all.
// `docs/fixtures/adapters/codex-methods.md` is the complete inventory and is
// more current than ADAPTERS.md §3's prose; where they disagree, the inventory
// wins.
//
// # Two entry points, because app-server is experimental
//
// [Adapter] is the real one: one `codex app-server` process, many threads, full
// steering and approvals. [ExecAdapter] is the survivability plan ADAPTERS.md §1
// asks for — `codex exec --json`, one shot, no steering, no approvals — for the
// day app-server changes shape under us. It reports what it cannot do rather
// than pretending, so the orchestrator degrades visibly instead of silently.
//
// # Out of scope, and why
//
// `thread/realtime/*` — eight notifications including `outputAudio/delta` and
// `transcript/delta` — is **observable but not drivable**. `ClientRequest.json`
// contains no realtime method at all, only orphaned param types that nothing
// references, so this contract does not say how to open such a session. The
// same is true of `process/spawn` and `remoteControl/enable|disable`. Codex has
// a realtime voice path; Relay cannot reach it from here, and inventing a
// request shape to try would be the exact failure this package is built to
// avoid. Those notifications fall through to the "unhandled notification" log
// line, which is where the evidence would show up if it ever changes.
//
// # The rule this package exists to keep
//
// An adapter never emits an event it cannot observe. Three places that bites:
//
//   - `thread/status/changed` carries `activeFlags: [waitingOnApproval]`, which
//     says a session is blocked. It is *not* turned into a [event.NeedsInput],
//     because after a reconnect the original JSON-RPC request is gone and there
//     is no way to answer it. It is recorded (see [Session.Blocked]) and logged.
//   - Codex reports tokens and never money. `Usage.CostUSD` stays nil; USD has
//     to be computed upstream from a price table.
//   - Three of the five approval reply *shapes* are outside the vendored
//     contract (ADAPTERS.md §8 item 7). Until they are probed on a real Codex
//     they are answered with a JSON-RPC error — visibly, loudly — rather than
//     with a guess. [Options.UnverifiedReplies] is the flag that turns each one
//     on once someone has actually looked.
package codex
