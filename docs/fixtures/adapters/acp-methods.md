# ACP method inventory — every method, its params, and whether Relay uses it

Companion to `acp-schema.json`, which is the vendored contract. This file is the
human-readable index; the schema is the thing CI diffs.

| | |
|---|---|
| Package | `@zed-industries/agent-client-protocol` |
| Version vendored | **0.4.5** — also the current `latest` on npm at time of vendoring |
| Vendored file | `docs/fixtures/adapters/acp-schema.json` — byte-identical to `schema/schema.json` in the tarball |
| Source | `npm pack @zed-industries/agent-client-protocol@0.4.5` → `package/schema/schema.json` |
| tarball sha256 | `aeeb1391a657cb3ee5ad8c999aa35f10d324c6a31cdc30ec874b645fe78a4ce8` |
| schema sha256 | `b34aee888aa2a0b81e46907eef9ac5e4e531ffda87f586182a8dae243152106d` |
| Schema dialect | JSON Schema draft 2020-12, 87 `$defs` |
| Licence | Apache-2.0 (Zed Industries) — schema only, no runtime code vendored |
| `PROTOCOL_VERSION` | **1** — a `uint16`, bumped only for breaking changes |
| Verified | 2026-08-10, against the published tarball, in a container with no runtimes installed |

The schema is generated from the Rust crate with `--features unstable`, so it
contains items the spec has *not* settled. Those are called out below.

**Why a JSON schema and not the `.d.ts`.** The package ships both. The schema is
self-describing for our purpose: every request and response definition carries
non-standard `x-method` and `x-side` annotations giving the exact wire method
name and which side handles it, so the whole method surface is recoverable from
the schema alone. The one thing it does *not* carry is the `PROTOCOL_VERSION`
constant, which lives only in `typescript/schema.ts`; it is recorded in the table
above. `dist/schema.d.ts` is 880 KB of generated types and is not worth vendoring
for one integer.

**This file is checked by machine.** `check_acp_methods.py` in this directory
re-reads the schema, parses the two method tables below, and fails if they
disagree — plus the structural invariants listed at the end, of which the
important one is that no steering method has appeared.

```bash
python3 docs/fixtures/adapters/check_acp_methods.py          # verify
python3 docs/fixtures/adapters/check_acp_methods.py --print  # fresh rows to paste
```

Three of the five runtimes ride on this one contract, so a silent drift here is a
silent drift in most of the product.

---

## Transport

Newline-delimited JSON-RPC 2.0 over the agent process's stdin/stdout. One
message per line, `\n`-terminated, no Content-Length framing. Both directions
carry requests, responses and notifications — it is symmetric, so the agent
calls *us* as much as we call it.

Error codes are stock JSON-RPC (`-32700` parse, `-32600` invalid request,
`-32601` method not found, `-32602` invalid params, `-32603` internal) plus two
ACP additions: **`-32000` "Authentication required"** and `-32002`
"Resource not found".

Any method whose name starts with `_` is an **extension** (`ext_method` /
`ext_notification` in the library). Unknown non-`_` methods MUST be answered
with `-32601`. The `_` prefix is the only door through which a runtime could
add steering later — see the note under `session/prompt`.

---

## Disposition legend

Last column of both method tables. Same vocabulary as `codex-methods.md`, so the
two inventories read the same way.

| | Meaning |
|---|---|
| **use** | the Relay ACP adapter calls it or handles it today |
| **must-answer** | agent → client request the adapter must reply to even though Relay does not want the feature — an unanswered JSON-RPC request stalls the agent |
| **later** | real value for a named later milestone; deliberately not wired yet |
| **ignore** | out of scope for an orchestrator. For a *request* that still means replying — with `-32601` — never leaving it hanging |

---

## Client → agent (8 methods)

Wire names exactly as in `AGENT_METHODS`. "Required" is the schema's `required`
array, in schema order; optional fields are covered in the prose below.

| Method | Kind | Required params | Response — required | Relay |
|---|---|---|---|---|
| `initialize` | request | `protocolVersion` | `protocolVersion` | use |
| `authenticate` | request | `methodId` | *(none)* | use |
| `session/new` | request | `cwd`, `mcpServers` | `sessionId` | use |
| `session/load` | request | `mcpServers`, `cwd`, `sessionId` | *(none)* | use |
| `session/prompt` | request | `sessionId`, `prompt` | `stopReason` | use |
| `session/cancel` | notification | `sessionId` | *(none)* | use |
| `session/set_mode` | request | `sessionId`, `modeId` | *(none)* | later |
| `session/set_model` | request | `sessionId`, `modelId` | *(none)* | ignore |

`initialize` also takes `clientCapabilities`, and answers with
`agentCapabilities` and `authMethods[]`. `session/new` and `session/load` answer
with optional `modes` and `models`. `session/prompt`'s `prompt` is an array of
`ContentBlock`. `session/set_model` is `ignore` because it is the one **UNSTABLE**
member of the surface, not because model choice does not matter.

### `initialize`

Called once, before anything else. Negotiation is one round trip:

1. Client sends the **latest** version it supports (`PROTOCOL_VERSION`, today `1`).
2. Agent replies with that same version if it supports it, otherwise **the latest
   version the agent supports** — which may be lower *or* higher than ours.
3. If the client cannot speak the returned version it **must disconnect**. There
   is no second round of negotiation.

`clientCapabilities` is what the agent is allowed to ask *us* for:

```json
{"protocolVersion":1,
 "clientCapabilities":{"fs":{"readTextFile":false,"writeTextFile":false},
                       "terminal":false}}
```

All three default to `false` when omitted. `agentCapabilities` comes back with
`loadSession`, `promptCapabilities` (`image`, `audio`, `embeddedContext`) and
`mcpCapabilities` (`http`, `sse`) — all defaulting to `false`, and stdio MCP
transport is mandatory for every agent so it has no capability flag.

**Relay declares `fs: false` and `terminal: false`.** That is a decision, not a
probed fact, and the reason is the grounding rule: if we advertise those
capabilities the agent performs file and shell work by calling back into us over
RPC, and none of it is required to appear in `session/update`. Declining them
keeps every file edit and command visible as a `tool_call` we can narrate.
Revisit only if a runtime degrades badly without them.

### `authenticate`

`session/new` may fail with `-32000` if the runtime is not logged in. The
recovery is to pick an `id` from the `authMethods[]` array returned by
`initialize`, call `authenticate`, then retry `session/new`. Relay surfaces this
in the installer rather than mid-conversation — a device-code flow cannot be
completed by voice.

### `session/new` / `session/load`

`cwd` **must be an absolute path**. `mcpServers[]` is where `MEMORY.md` §7's
shared MCP registry is injected; three transports exist — `stdio` (mandatory),
`http` and `sse` (capability-gated).

`session/load` **replays the entire conversation history back as
`session/update` notifications** before it resolves. The adapter must know it is
in replay and not ping the user for a two-week-old completed turn.

`modes` / `models` in the response are `SessionModeState` / `SessionModelState`.
Modes are stable; **models are UNSTABLE**.

### `session/prompt`

The request does not resolve until the whole turn is over — every model call,
every tool, every permission round trip happens inside it, and the response is
one field, `stopReason`. That single long-lived request *is* the turn boundary.

**There is no way to add to a turn in flight.** No `steer`, no `inject`, no
`queue`; `session/prompt` and `session/cancel` are the only two levers, and the
protocol has no second-prompt-while-running semantics. Verified by exhaustive
search of the vendored schema and the shipped TypeScript: the strings `steer`,
`inject` and `interrupt` do not occur anywhere in the package. See
`ADAPTERS.md` §4 for what Relay does instead.

`prompt[]` is an array of `ContentBlock`. Baseline support is `text` and
`resource_link` only; `image`, `audio` and `resource` (embedded context) each
require the matching `promptCapabilities` flag.

### `session/cancel`

A **notification**, so it has no response of its own — the acknowledgement is
the original `session/prompt` resolving with `stopReason: "cancelled"`. The
agent's contract on receipt is explicit: stop model requests, abort in-flight
tools, **flush pending `session/update` notifications**, then resolve. The
client must keep accepting updates after sending the cancel, and must answer any
outstanding `session/request_permission` with `{"outcome":{"outcome":"cancelled"}}`.

---

## Agent → client (9 methods)

Wire names exactly as in `CLIENT_METHODS`.

| Method | Kind | Required params | Response — required | Relay |
|---|---|---|---|---|
| `session/update` | notification | `sessionId`, `update` | *(none)* | use |
| `session/request_permission` | request | `sessionId`, `toolCall`, `options` | `outcome` | use |
| `fs/read_text_file` | request | `sessionId`, `path` | `content` | ignore |
| `fs/write_text_file` | request | `sessionId`, `path`, `content` | *(none)* | ignore |
| `terminal/create` | request | `sessionId`, `command` | `terminalId` | ignore |
| `terminal/output` | request | `sessionId`, `terminalId` | `output`, `truncated` | ignore |
| `terminal/wait_for_exit` | request | `sessionId`, `terminalId` | *(none)* | ignore |
| `terminal/kill` | request | `sessionId`, `terminalId` | *(none)* | ignore |
| `terminal/release` | request | `sessionId`, `terminalId` | *(none)* | ignore |

`fs/read_text_file` also takes optional `line` (1-based) and `limit`;
`terminal/create` also takes `args`, `cwd`, `env` and `outputByteLimit`;
`terminal/output` may include `exitStatus`, and `terminal/wait_for_exit` returns
`exitCode` and `signal`, both nullable.

An agent must not call `fs/*` or `terminal/*` unless the matching client
capability was advertised, and Relay advertises neither — which is why all seven
are `ignore` rather than `must-answer`. The adapter answers `-32601` if one
arrives anyway: visibly refused, not silently faked. If a runtime turns out to
degrade badly without them, that is a capability decision to revisit in §4, not a
reason to fake a success response.

### `session/update` — eight variants

Discriminated by the `sessionUpdate` field.

| Variant | Payload | Normalized event |
|---|---|---|
| `user_message_chunk` | `content`: ContentBlock | replay/echo — not spoken |
| `agent_message_chunk` | `content`: ContentBlock | `TextDelta` → streaming TTS |
| `agent_thought_chunk` | `content`: ContentBlock | `Reasoning` — **never spoken** |
| `tool_call` | **`toolCallId`**, **`title`**, `kind`, `status`, `content[]`, `locations[]`, `rawInput`, `rawOutput` | `ToolStarted` |
| `tool_call_update` | **`toolCallId`**, everything else nullable | `ToolOutput` / status change |
| `plan` | **`entries[]`** of `{content, priority, status}` | `PlanUpdated` — best narration source |
| `available_commands_update` | **`availableCommands[]`** of `{name, description, input?}` | tool-list refresh (`SYSTEM.md` §9 problem 6) |
| `current_mode_update` | **`currentModeId`** | mode changed, possibly by the agent itself |

`ToolCallStatus` = `pending`, `in_progress`, `completed`, `failed`.
`ToolKind` = `read`, `edit`, `delete`, `move`, `search`, `execute`, `think`,
`fetch`, `switch_mode`, `other`.
`PlanEntryStatus` = `pending`, `in_progress`, `completed`;
`PlanEntryPriority` = `high`, `medium`, `low`.
`ToolCallContent` is one of `content` (a ContentBlock), `diff`
(`path` + `newText` + optional `oldText`), or `terminal` (`terminalId`).

**There is no token or cost field anywhere in ACP.** The only occurrence of the
word "token" in the whole schema is the `max_tokens` stop reason. Per-turn
metering for these three runtimes has to come from outside the protocol.

### `session/request_permission` — the needs-input path

This is a **request**, so the agent blocks on our answer for as long as we take.
That is what makes voice-answerable approval real for OpenClaw, Hermes and
OpenCode.

Request:

```json
{"sessionId":"sess_abc",
 "toolCall":{"toolCallId":"call_1","title":"Run npm test","kind":"execute",
             "status":"pending","rawInput":{"command":"npm test"}},
 "options":[{"optionId":"o1","name":"Allow","kind":"allow_once"},
            {"optionId":"o2","name":"Always allow","kind":"allow_always"},
            {"optionId":"o3","name":"Reject","kind":"reject_once"}]}
```

`toolCall` is a **`ToolCallUpdate`**, so only `toolCallId` is guaranteed —
`title`, `kind` and `rawInput` are all optional and may be absent. The adapter
must not assume it has a human-readable description to read aloud; when `title`
is missing, say so rather than inventing one.

`options[]` is agent-supplied and open-ended. `PermissionOptionKind` is a *hint*
for UI treatment — `allow_once`, `allow_always`, `reject_once`, `reject_always` —
not a fixed menu. Relay speaks `name`, matches the spoken reply back to an
`optionId`, and never invents an option that was not offered.

Response, exactly two shapes:

```json
{"outcome":{"outcome":"selected","optionId":"o1"}}
{"outcome":{"outcome":"cancelled"}}
```

`cancelled` is mandatory, not optional: if we send `session/cancel` while a
permission request is outstanding, we must resolve it with `cancelled` or the
agent's turn cannot unwind.

---

## `StopReason` — the turn boundary

Returned by `session/prompt`. Five values, all five confirmed present:

| Value | Meaning | Relay |
|---|---|---|
| `end_turn` | finished normally | `TurnCompleted{ok:true}` → ping |
| `max_tokens` | hit the token ceiling | `TurnCompleted{ok:false}` — offer to continue |
| `max_turn_requests` | hit the agent-request ceiling for one user turn | `TurnCompleted{ok:false}` |
| `refusal` | agent declined; **the user prompt and everything after it are dropped from the next prompt** | `TurnCompleted{ok:false}` — say so plainly |
| `cancelled` | we sent `session/cancel` | not a ping; feeds the re-prompt |

---

## The invariants `check_acp_methods.py` enforces

These are the assertions that turn a version bump into a red build instead of a
wrong adapter. All nine pass against the vendored file as of 2026-08-10, and each
was confirmed to *fail* when the corresponding shape is mutated.

1. **Method surface, exactly.** Collect every `$defs.*["x-method"]` grouped by
   `["x-side"]`. `x-side: "agent"` must be exactly the 8 client→agent names,
   `x-side: "client"` exactly the 9 agent→client names, 17 in total.
2. **No steer.** The substrings `steer`, `inject` and `interrupt` do not occur
   anywhere in the file, and `session/prompt` + `session/cancel` remain the only
   turn-affecting client→agent methods.
3. **Union shapes.** `ClientRequest.anyOf` has 8 branches — 7 `$ref`s plus one
   untitled `ExtMethodRequest`; `ClientNotification.anyOf` has 2 —
   `CancelNotification` plus `ExtNotification`; `AgentRequest.anyOf` has 9 — 8
   `$ref`s plus ext; `AgentNotification.anyOf` has 2. A ninth branch appearing in
   `ClientRequest` is the exact signal that a steering method has landed.
   (Note the naming inversion in the schema: `$defs.ClientRequest` holds what the
   *client sends to the agent*, and the top-level `anyOf` titles it `AgentRequest`.
   Trust `x-side`, not the def name.)
4. `SessionUpdate.oneOf` still has exactly the eight `sessionUpdate` discriminants.
5. `StopReason.oneOf` still has exactly the five values, in order.
6. `RequestPermissionOutcome` still has exactly `selected` (requiring `optionId`)
   and `cancelled`; `RequestPermissionRequest.required` is still
   `["sessionId","toolCall","options"]` and its `toolCall` still `$ref`s
   `ToolCallUpdate`, whose only required field is `toolCallId`.
7. `ClientCapabilities` is still `{fs:{readTextFile,writeTextFile}, terminal}` and
   `AgentCapabilities` still `{loadSession, promptCapabilities, mcpCapabilities}` —
   a new capability means a new decision about what Relay advertises.
8. The set of `UNSTABLE`-marked definitions is still exactly `ModelId`,
   `ModelInfo`, `SessionModelState`, `SetSessionModelRequest`,
   `SetSessionModelResponse`. Anything leaving that set has been promoted;
   anything joining it has been demoted.
9. No `cost` or `usage` field has appeared. This one is checked in the hope it
   *will* fire: if ACP ever adds per-turn metering, `ADAPTERS.md` §5 and §8 both
   need rewriting in Relay's favour.

Two things the checker cannot cover, because they are not in the schema:
`PROTOCOL_VERSION` (still `1`; lives in `typescript/schema.ts` and must be
re-read on any bump), and `_`-prefixed extension methods, which are invisible to
the contract by design. Both are named in `ADAPTERS.md` §8.

**The nine now run twice.** `relayd/internal/adapter/acp/schema_test.go` asserts
all nine against this same file on every `go test ./...`, one test each
(`TestInvariantMethodSurface` … `TestInvariantNoCostOrUsageField`), plus a tenth
the schema cannot carry: `TestProtocolVersionMatchesTheVendoredRecord` pins the
adapter's `ProtocolVersion` constant to the `1` recorded in the table at the top
of this file, so a bump has to move both. `check_acp_methods.py` stays as the
runnable version for anyone bumping the vendored schema by hand, and it is the
one that regenerates the two method tables above.

Alongside them, `relayd/testdata/acp/gen_trace.py` builds
`docs/fixtures/adapters/acp.trace.json` from this schema and validates every
message against the `$defs` entry it names. That file is **schema-derived, not
recorded** — no ACP runtime exists in the build container — and `ADAPTERS.md` §8
item 14 tracks replacing it with one real recording per runtime.
