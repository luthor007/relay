# Agent adapters — how the orchestrator drives five runtimes

*Answers `SYSTEM.md` §9 hard problem #1. Every claim below was probed against
installed binaries on 2026-08-09, not inferred from documentation.*

| Runtime | Version probed |
|---|---|
| Claude Code | 2.1.226 |
| OpenClaw | 2026.3.13 (61d171a) |
| Codex | codex-cli 0.140.0 |
| Hermes Agent | v0.16.0 (2026.6.5) |
| OpenCode | 1.18.15 |

Two protocol contracts are vendored rather than probed, because they are
published as machine-readable schemas and a diff is worth more than a probe:

| Contract | Version vendored | Where | Verified |
|---|---|---|---|
| Codex app-server | codex-cli 0.140.0 | `docs/fixtures/adapters/{ClientRequest,ServerNotification,ServerRequest}.json` | 2026-08-09 |
| **Agent Client Protocol** | **`@zed-industries/agent-client-protocol` 0.4.5** — also npm `latest` when vendored | `docs/fixtures/adapters/acp-schema.json`, indexed by `acp-methods.md` | 2026-08-10 |

ACP's wire protocol version is a separate integer from the package version:
`PROTOCOL_VERSION = 1`, bumped only for breaking changes.

Worth knowing when reading "ACP is young" below: 0.4.5 is the newest release and
the registry has not published a new one since **2025-10-10** — checked live, not
against a cached index. Ten months without a release is not obviously good or bad,
but it does mean the contract we vendored is the contract the three runtimes have
been building against for a while, and that pinning costs us nothing today.

---

## 1. The headline: three protocols, not five, and no scraping

§9 assumed "five unstable output formats" and budgeted for recorded fixtures per
runtime against CLI text. **That was wrong, and wrong in our favour.** Every
installed runtime exposes a structured, bidirectional protocol over stdio:

| Runtime | Protocol | Why this one |
|---|---|---|
| Claude Code | `stream-json` over stdin/stdout | only structured option; `--input-format stream-json` is the one that accepts a live turn |
| Codex | **app-server** JSON-RPC | `codex exec --json` also exists but is one-shot; app-server is the only path with steering and approvals |
| OpenClaw | **ACP** via `openclaw acp` | bridges the Gateway; a standard protocol beats the Gateway's own WS RPC |
| Hermes | **ACP** via `hermes acp` | same protocol as OpenClaw, so one adapter covers all three |
| OpenCode | **ACP** via `opencode acp` | confirmed; also ships `opencode serve`, a headless HTTP server, if ACP proves limiting |

So the adapter layer is **three adapters**, and OpenClaw, Hermes and OpenCode
share one.
Nobody parses terminal output. The §9 mitigation ("a format change breaks CI, not
a user") still applies, but to three JSON schemas rather than five text formats.

**Revise §9 accordingly** — this is no longer the highest-risk item. The riskiest
part is now that two of the three protocols are explicitly experimental: Codex
labels app-server `[experimental]`, and ACP is young. Version-pin, and keep a
`codex exec --json` one-shot fallback for the case where app-server changes
shape under us.

---

## 2. Claude Code — verified live

### Start

```bash
claude -p \
  --input-format stream-json --output-format stream-json \
  --verbose --include-partial-messages --replay-user-messages \
  --session-id <uuid> \
  --permission-prompt-tool <mcp-tool>
```

`--session-id` takes a UUID we generate, so the orchestrator names the session
rather than discovering its name afterwards. `--resume <id>` and `--fork-session`
reattach later.

### Inject a turn into a running process — **confirmed working**

Write one NDJSON line to the live process's stdin:

```json
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"..."}]}}
```

Probed with two turns through a single long-lived process: the second turn
answered with the first still in context, and no restart. **This is the mechanism
that makes "continue an existing session" real** for Claude Code — the process
stays up and we push turns into it.

With `--replay-user-messages` the input is echoed back on stdout carrying
`"isReplay": true`, which is a free acknowledgement that the turn was accepted.

### Events observed, in order

**Re-verified 2026-08-10 against `docs/fixtures/adapters/claude-code.trace.json`**
— a 49-event recording of claude-code 2.1.226 running two turns, the first of
which calls `Bash`. Every count and JSON path below was read out of that file,
and the fixture is the contract: if a future release changes any of it, the
adapter's tests go red before a user does.

| Event | Count | Use |
|---|---|---|
| `system/hook_started`, `system/hook_response` | 3 + 3 | **hook lifecycle — see below. Not previously documented, and an adapter that switches exhaustively on `system.subtype` will fall over them** |
| `system/init` | 2 | tool list and session config — **re-emitted at the head of every turn**, which answers §9 problem 6 (tool-list refresh) for this runtime |
| `system/status` `{"status":"requesting"}` | 3 | **a model request started — once per API request, not once per turn.** Two turns produced three, because the tool round trip needs a second request. Do not use it as a turn boundary |
| `user` with `"isReplay": true` | 2 | our injected turn was accepted |
| `user` with `tool_use_result` and **no** `isReplay` key | 1 | a tool result being fed back. Same `type`, entirely different meaning — discriminate on key *presence*, not on `isReplay == false` |
| `stream_event` → `event.type: message_start` | 3 | new assistant message; carries `message.id`, `message.model`, and the opening `usage` |
| `stream_event` → `content_block_start` | 4 | `content_block.type` is `text` (3) or `tool_use` (1); the `tool_use` block carries `id`, `name`, and an **empty** `input` |
| `stream_event` → `content_block_delta` / `text_delta` | 7 | **token stream — the input to streaming TTS.** Path: `event.delta.text` |
| `stream_event` → `content_block_delta` / `input_json_delta` | 4 | tool arguments, streamed as *string fragments* at `event.delta.partial_json`. They concatenate to the JSON object; the first fragment in the fixture is the empty string |
| `stream_event` → `content_block_stop` | 4 | one per block, carrying `event.index` |
| `stream_event` → `message_delta` | 3 | **the only place `stop_reason` is populated** — `tool_use` then `end_turn`. Also carries that request's own `usage` |
| `stream_event` → `message_stop` | 3 | request finished. Payload is exactly `{"type":"message_stop"}` — no fields |
| `assistant` | 4 | **one per completed content block, not one per message.** See below |
| `rate_limit_event` | 1 | `rate_limit_info` = `{status, resetsAt, rateLimitType, overageStatus, overageDisabledReason, isUsingOverage}` — quota state, worth surfacing before it bites |
| `result` `{"subtype":"success"}` | 2 | **turn boundary** |

Six distinct `stream_event` inner types appear (`message_start`,
`content_block_start`, `content_block_delta`, `content_block_stop`,
`message_delta`, `message_stop`) but only **two** delta types (`text_delta`,
`input_json_delta`). A session with extended thinking would add `thinking_delta`
and `signature_delta`; this fixture does not contain them, so the adapter must
treat unknown delta types as ignorable rather than fatal.

The sixteen rows above sum to exactly 49, so the table is the whole trace and not
a selection. Every event carries `session_id` (one value throughout) and `uuid`;
`stream_event`, `user` and `assistant` also carry `parent_tool_use_id`, **null on
all 49**. Sub-agent (`Task`) output is the obvious thing it exists to attribute,
but this fixture contains no sub-agent, so treat that as unobserved: the adapter
should carry the field through and must not claim sub-agent attribution until a
trace with a non-null value exists.

**`assistant` is per content block, not per message.** The two `assistant` events
in turn 1 share one `message.id` (`msg_011CdtJRqQ…`): the first carries a
one-element `content` array holding the `text` block, the second a one-element
array holding the `tool_use` block. Each is emitted *before* its matching
`content_block_stop`. `message.stop_reason` is `null` on all four. So an adapter
that treats `assistant` as "the finished message" will double-count assistant
turns and will never see a stop reason. Assemble the message from the stream
events, or accumulate `assistant` events by `message.id`.

### The `result` event, field by field

Present and confirmed on both `result` events:

| Field | Turn 1 | Turn 2 | Meaning |
|---|---|---|---|
| `num_turns` | 2 | 1 | model iterations **within this turn** — the tool round trip makes it 2. Not cumulative |
| `stop_reason` | `end_turn` | `end_turn` | |
| `terminal_reason` | `completed` | `completed` | |
| `duration_api_ms` | 2985 | 4450 | this turn only |
| `duration_ms` | 5481 | 5966 | wall clock, this turn only |
| `total_cost_usd` | 0.1795935 | 0.196937 | **cumulative for the session — see the trap below** |
| `usage` | | | `input_tokens`, `output_tokens`, `cache_creation_input_tokens`, `cache_read_input_tokens`, `cache_creation.{ephemeral_5m,ephemeral_1h}_input_tokens`, `server_tool_use`, `service_tier`, `speed`, `inference_geo`, `iterations` |
| `modelUsage` | | | keyed by the *decorated* model id (`claude-opus-5[1m]`) |
| `result` | `"DONE"` | `"SECOND-TURN"` | the final assistant text — this is §6's input |
| `is_error`, `api_error_status`, `permission_denials` | `false`, `null`, `[]` | same | |
| `ttft_ms`, `ttft_stream_ms`, `time_to_request_ms` | 2001 / 1053 / 61 | | latency budget, free |

**The cost trap, and it is nastier than it looks: the two usage blocks in the
same event have different scopes.**

| Block | Scope | Evidence in the fixture |
|---|---|---|
| `result.usage` | **this turn**, summed over the requests it took | turn 1 `cache_read` 51,997 = 18,502 + 33,495, its two requests; `output_tokens` 101 = 96 + 5 |
| `result.modelUsage[…]`, `total_cost_usd` | **the whole session, cumulative** | `outputTokens` 101 → 111 across the two results, against a second turn that emitted exactly 10; `cacheReadInputTokens` 51,997 → 85,604 |

So **per-turn cost is the delta between consecutive `result` events**, not the
value on the event. The previous claim that "per-turn cost metering is free" was
half right: the numbers are free, the subtraction is ours. `usage.iterations` is
no help — it holds only the last iteration, not the whole turn.

**And neither block is the live context size.** `result.usage` *sums* a turn's
requests, so turn 1 reports 51,997 cache-read tokens for a context that was
actually ~33,500 — a turn with eight tool calls would overstate it eightfold and
trigger compaction on a session that is barely full. The live context is the
**most recent request's** usage, from `message_start` (or `message_delta`):

```
context_used = event.message.usage.input_tokens
             + event.message.usage.cache_read_input_tokens
             + event.message.usage.cache_creation_input_tokens
```

which across the fixture's three requests reads 33,497 → 33,609 → 33,637 —
monotonic and plausible — against the `contextWindow` of 1,000,000 from
`modelUsage`. `MEMORY.md` §9 is corrected accordingly.

**And that has a consequence for the normalized event, which the adapter now
enforces.** `TurnCompleted.Usage` on Claude Code carries the turn's own totals —
they are correct for metering — but its `ContextWindow` is left **nil**, so
`Usage.ContextPressure()` cannot be computed from a finished turn at all.
Populating it would pair a summed numerator with a per-request denominator and
reproduce the eightfold overstatement above; nil means "not reported", which is
the honest answer for a figure this object cannot hold correctly. The live
context lives on the session instead, as `Session.Context()` →
`{Used, Window}`, which is exactly the recipe in the two paragraphs above:
numerator from the most recent `message_start`, denominator from `modelUsage`.
That is the value `MEMORY.md` §9's compact-at-70% must read.

`modelUsage["claude-opus-5[1m]"]` also carries **`contextWindow: 1000000`** and
`maxOutputTokens: 64000`, plus `canonicalModel: "claude-opus-5"` and
`provider: "firstParty"`. That `contextWindow` is the denominator `MEMORY.md` §9
says it needs and cannot get from Codex — for Claude Code it arrives free at the
end of every turn. Note the id is decorated: `model` in `system/init` is
`claude-opus-5[1m]`, and only `canonicalModel` is the real model name. Never key
a routing table on the decorated form.

### `system/init`, field by field

Both init events are byte-identical except for `uuid`, which confirms
"re-emitted every turn" is a genuine repeat rather than a delta. It carries
`claude_code_version` (`2.1.226`), `model`, `cwd`, `session_id`, `apiKeySource`,
`output_style`, `memory_paths`, `messaging_socket_path`, and counted arrays:
`tools` (153), `slash_commands` (157), `skills` (108), `agents` (10),
`plugins` (5). Two fields matter more than the rest:

- **`permissionMode`** — the fixture records `"auto"`, which is exactly the state
  that silently disables `--permission-prompt-tool` (below). **This is the check
  Relay needs, and it is free**: read `permissionMode` from `system/init` at the
  head of every turn and refuse to claim needs-input capability when it is `auto`
  or `bypassPermissions`. There is no need to go reading the user's
  `settings.json`.
- **`mcp_servers`** — `[{name, status}]`, `status: "connected"`. `MEMORY.md` §7's
  registry reconciliation gets Claude Code's side of the picture from here.
- `capabilities` is `["interrupt_receipt_v1", "interrupt_cancel_queued_v1",
  "msg_lifecycle_v1"]` — a negotiated feature list, and the first evidence that
  interrupt and queued-cancel are protocol features rather than TUI behaviour.
  What the client must *send* to use them is not in this trace. **So cancelling
  a turn is only half-implemented on this runtime**: the adapter cancels by
  answering an open permission prompt with `interrupt: true`, which is verified,
  and returns an `*UnsupportedError` for `CapCancel` otherwise rather than
  guessing at a stdin message. `CapCancel` is `SupportUnknown`, not
  `SupportNo` — the runtime plainly can, we just have not seen how to ask. §8
  item 9 names the probe.

### Hooks — undocumented until now, and they arrive first

Before anything else, the trace emits three `system/hook_started` and three
`system/hook_response` events. All six are `hook_name: "SessionStart:startup"`,
`hook_event: "SessionStart"`, and each carries a `hook_id` UUID.

Two things an adapter must get right:

1. **They precede `system/init`.** A reader that waits for `init` as the first
   event will sit on six unread events, and one that hard-fails on an unknown
   `system.subtype` will crash on the very first line of a real session.
2. **Responses are correlated by `hook_id`, not by order.** The fixture's starts
   run `158d…`, `c038…`, `9224…`; the responses come back `158d…`, `9224…`,
   `c038…`. Hooks run concurrently. Match on the id.

`hook_response` adds `exit_code`, `outcome` (`"success"`), `output`, `stdout` and
`stderr`. A non-zero `exit_code` or a non-`success` `outcome` is a user-visible
misconfiguration worth surfacing — a failing `SessionStart` hook is a common
reason a session behaves oddly from the first turn.

Only `SessionStart` hooks appear in this fixture. Claude Code has other hook
events (`PreToolUse`, `PostToolUse`, `Stop` and friends) which would presumably
arrive mid-stream with the same envelope, but **that is inference, not
observation** — the adapter must handle any `hook_*` subtype at any position and
must not claim to report hook activity it has not seen.

### Needs-input — verified end to end

`--permission-prompt-tool` routes permission decisions to an MCP tool we
implement. That call *is* the needs-input signal. Probed on 2.1.226: allow path,
deny path, and the silent-failure mode all reproduced.

```bash
claude -p "<prompt>" \
  --mcp-config /abs/path/mcp.json --strict-mcp-config \
  --setting-sources "" \
  --permission-prompt-tool mcp__perm__approve \
  --output-format stream-json --verbose
```

**The trap that makes it silently do nothing.** If the user's
`~/.claude/settings.json` sets `permissions.defaultMode` to `auto` — or the run
uses an auto or bypass permission mode — the prompt tool is **never called**. The
tool simply runs. No warning, no stderr, exit 0.

That is not hypothetical: the probe machine had `defaultMode: "auto"` set
globally and it leaked into every headless run, which is why the first attempt
produced no output at all. So `--setting-sources ""`, or an explicit non-auto
`--permission-mode`, is **mandatory** rather than tidy. Relay must also *check*
for this and say so, because the failure presents as "the glasses never ask me
anything" — which reads as a feature until something destructive runs unattended.

**The check is one field.** `system/init` reports `permissionMode` on every turn,
and the vendored fixture was recorded in exactly the broken state
(`"permissionMode": "auto"`) — so the trace doubles as the regression test for
the detector. Read it at the head of each turn; when it is `auto` or a bypass
mode, mark the session's needs-input capability **off** rather than reporting a
capability the adapter cannot observe.

Note which way the risk runs: our needs-input path requires permission checks to
be **on**. Nothing here should ever push a user toward a bypass mode.

**Request shape:**

```json
{"tool_name":"Bash",
 "input":{"command":"touch /tmp/x","description":"Create empty file"},
 "tool_use_id":"toolu_01SYENeww5TLrR6ZWAJ7T7LZ"}
```

plus `_meta["claudecode/toolUseId"]` and a `progressToken`.

**Response shape** — a single MCP text block whose `text` is a JSON *string*,
not an object:

| | Payload |
|---|---|
| Allow | `{"behavior":"allow","updatedInput":{…},"updatedPermissions":[…]}` |
| Deny | `{"behavior":"deny","message":"…","interrupt":false}` |

`updatedInput` is optional and falls back to the original; if supplied it is
schema-validated against the target tool, and a mismatch raises
`InputValidationError`.

**On deny the session continues** — the denial arrives as a normal tool result
with `is_error: true`, the model narrates it, and the run still exits 0 with
`result.subtype: "success"`. **`interrupt: true` is different**: it aborts the
turn outright. That is the hard-stop lever for "no, stop" spoken at the glasses.

**We can block for as long as we need.** The default MCP tool-call timeout is
`1e8` ms — about **27.8 hours** — capped near 24.8 days and overridable per
server. Progress notifications do *not* extend it. So holding the `approve` call
open while the user is pinged, walks away, and answers an hour later is well
within budget. That is what makes §7's voice-answerable blocking real rather than
aspirational.

**Smaller gotchas.** The flag is undocumented — it does not appear in
`claude --help` — and works only with `-p`. The permission tool is *hidden from
the model*, so it never shows in the init event's `tools` array; test by watching
for the call, not by looking for the tool. It must be an MCP tool, and MCP tools
that themselves need elicitation are rejected. Use `--strict-mcp-config`, or
every one of the user's own MCP servers loads on each spawn.

**What the adapter does with all of that** (`relayd/internal/adapter/claudecode`):

- The MCP server is **in-process**, mounted on a loopback listener at
  `/mcp/<session id>`, and the generated `--mcp-config` file names it as an
  `http` server alongside whatever `MEMORY.md` §7's registry granted the
  session — which is the whole set, because `--strict-mcp-config` is always
  passed. A `ServeStdio` transport ships too, for a host that would rather not
  hold an HTTP request open for a day. **Neither transport's 27-hour behaviour
  has been probed**; the MCP timeout allows it, an HTTP client's own timeout
  might not, and §8 item 10 names that probe.
- The question reaches the orchestrator as `NeedsInput` with exactly three
  options — Allow, Deny, Deny-and-stop — mapping to `allow`, `deny`, and
  `deny` + `interrupt: true`. **No `allow_always`, and `updatedPermissions` is
  never returned**, because `ORCHESTRATOR.md` §4b requires consequential actions
  to be confirmed every time and a standing grant is the opposite of that.
- If the orchestrator is unreachable the call answers **deny, without
  interrupt**. Failing closed is the safe direction: nothing destructive ran,
  and the session survives to be asked again.

---

## 3. Codex — app-server, the richest of the three

```bash
codex app-server
```

JSON-RPC over stdio. The protocol ships machine-readable schemas —
`codex app-server generate-json-schema --out <dir>` — and the three inventory
files are vendored under `docs/fixtures/adapters/` so a version bump diffs
visibly.

> **This section was written from memory and has now been checked against the
> vendored schemas (codex-cli 0.140.0).** Every method name below survived; the
> counts and the surrounding claims did not. The complete inventory — all 84
> client requests, 66 notifications and 10 server requests, with params types and
> which ones Relay uses — is `docs/fixtures/adapters/codex-methods.md`, and
> `check_codex_methods.py` next to it fails the build when upstream drifts.
> **The corrections are marked ⚠ below.** Write the adapter against the inventory,
> not against this summary.

| Need | Method | Params |
|---|---|---|
| Handshake ⚠ | `initialize` | `{clientInfo:{name,version}, capabilities?}` — **mandatory, and it was missing from this table** |
| Start | `thread/start`, or `thread/resume` / `thread/fork` | all fields optional on `start`; `threadId` required on the other two |
| Begin a turn | `turn/start` | `{threadId, input: UserInput[]}` |
| **Inject into a running turn** | **`turn/steer`** ⚠ | `{threadId, expectedTurnId, input}` — **`expectedTurnId` is a required precondition**; the request fails if that turn is no longer the active one |
| Cancel | `turn/interrupt` ⚠ | `{threadId, turnId}` — **needs the turn id, not just the thread** |
| Turn boundary | `turn/started` → `turn/completed` | both `{threadId, turn: Turn}` |
| Item boundary | `item/started` → `item/completed` | `{threadId, turnId, item: ThreadItem, startedAtMs/completedAtMs}` |
| Detach ⚠ | `thread/unsubscribe` | there is **no `thread/subscribe`** — subscription is implicit in start/resume/fork |

⚠ **`initialize` gates what arrives.** `InitializeCapabilities` has three fields:
`experimentalApi` (default **false**, "opt into receiving experimental API methods
and fields"), `optOutNotificationMethods` (suppress named notifications for this
connection), and `requestAttestation` (default false). Everything the schemas mark
EXPERIMENTAL — `item/tool/requestUserInput`, `item/plan/delta`, all of
`thread/realtime/*` — is presumed off unless we ask for it. Relay sets
`experimentalApi: true` and treats every experimental payload as optional.

**Progress deltas** are unusually good — finer-grained than Claude Code's. All
seven names below are confirmed verbatim in `ServerNotification.json`:

```
item/agentMessage/delta            assistant text, token by token
item/reasoning/textDelta           thinking (do not speak this)
item/reasoning/summaryTextDelta    thinking summary (speakable)
item/commandExecution/outputDelta  live stdout from a running command
item/mcpToolCall/progress          MCP tool progress ({message}, prose not percent)
turn/plan/updated                  the agent's own plan — best narration source
thread/tokenUsage/updated          tokens ⚠ — see below
```

⚠ Four more the summary omitted, each of which changes behaviour:

| | Why it matters |
|---|---|
| `error {threadId, turnId, error, willRetry}` | the turn-level failure event. **`willRetry` decides whether to ping** — a retryable error is not a user-facing failure |
| `serverRequest/resolved {requestId, threadId}` | a pending approval was answered *somewhere else*. Without this a Relay ping outlives its question and wakes the user to approve something already approved in a terminal |
| `thread/status/changed {status}` | `active` carries `activeFlags: [waitingOnApproval｜waitingOnUserInput]` — a second, independent needs-input signal, and the only one that survives a reconnect |
| `item/fileChange/patchUpdated {changes}` | file diffs. `item/fileChange/outputDelta` is deprecated and "the server no longer emits" it |

⚠ **`thread/tokenUsage/updated` is tokens, not money.** `TokenUsageBreakdown` is
`{inputTokens, cachedInputTokens, outputTokens, reasoningOutputTokens,
totalTokens}`; there is no dollar figure anywhere in the Codex contract, so USD
has to be computed from a price table. `turn/completed` carries **no cost at all**
— only `turn.status` (`completed｜interrupted｜failed｜inProgress`), `durationMs`
and, when failed, `turn.error`. And `modelContextWindow` is **nullable**, so
`MEMORY.md` §9's "compact on idle at ~70%" needs a fallback denominator.

`turn/plan/updated` deserves attention: it is the agent stating its own plan in
structured form — `{step, status: pending｜inProgress｜completed}[]` plus an
optional `explanation`. That is better narration material than anything we could
infer from a text stream, and §3b's grounding rule is satisfied by construction.

**Needs-input** is server→client *requests*, which is exactly the right shape —
the protocol blocks until we answer:

```
item/commandExecution/requestApproval    wants to run a command
item/fileChange/requestApproval          wants to write a file
item/permissions/requestApproval         wants a broader grant
item/tool/requestUserInput               a tool needs a value   [EXPERIMENTAL]
mcpServer/elicitation/request            an MCP server is asking
```

⚠ **There are ten server→client requests, not five, and every one blocks until
answered.** The other five are `item/tool/call` (Codex asking *us* to run a
dynamic tool), `account/chatgptAuthTokens/refresh` (Codex got a 401 and wants the
client to re-auth), `attestation/generate`, and the two deprecated legacy-API
approvals `applyPatchApproval` and `execCommandApproval`. Relay wants none of
them — but an adapter that only handles the five above **hangs Codex** the first
time one of the others arrives. Answer them with a JSON-RPC error; never drop one.

⚠ **Approvals reach us only if two settings say so, and this is Codex's exact
analogue of the `permissions.defaultMode: auto` trap in §2.** `approvalPolicy:
"never"` disables approvals; `approvalsReviewer` defaults to `user` but can be
`auto_review` (or its legacy alias `guardian_subagent`), which routes every
approval to a prompted subagent that decides on our behalf. Either one and the
five requests above simply never arrive. Pass both explicitly on
start/resume/fork, *check* the result with `config/read` and
`thread/settings/updated`, and watch for `item/autoApprovalReview/started` — that
notification is the visible symptom. As in §2, the failure presents as "the
glasses never ask me anything", which reads as a feature until something
destructive runs unattended.

⚠ **The approval reply vocabulary is `decline` vs `cancel`.** From
`CommandExecutionApprovalDecision`: `decline` means "the agent will continue the
turn"; `cancel` means "the turn will also be immediately interrupted". That is the
same distinction as Claude Code's `interrupt: false` / `true`, and it is what
makes "no, stop" spoken at the glasses a hard stop. The same union also offers
`acceptForSession` and two policy-amendment variants that persist a standing
grant — **Relay must never send those**, because `ORCHESTRATOR.md` §4b requires
consequential actions to be confirmed every time.

⚠ **`thread/realtime/*` is observable but not drivable.** The eight realtime
notifications are real, `outputAudio/delta` and `transcript/delta` among them —
but `ClientRequest.json` contains **no realtime methods at all**, only orphaned
param types (`ThreadRealtimeStartTransport`, `RealtimeVoice`,
`ThreadRealtimeAudioChunk`, …) that nothing references. The same is true of
`process/spawn` and `remoteControl/enable|disable`, both of which are named in
notification descriptions and absent from the request contract. So Codex has a
realtime voice path, and this contract does not tell us how to open it. Out of
scope now, and *more* out of reach than this section previously implied.

⚠ **The schemas carry no results and no framing.** `generate-json-schema` emits
request/notification *params* only — there is no `ServerResponse.json`. So the
thread id does **not** come from the `thread/start` result; take it from the
`thread/started` notification, whose `thread.id` is required. And nothing in any
of the three files says whether the transport is LSP-style `Content-Length`
headers or newline-delimited JSON. NDJSON is the strong prior; it is **not
verified**, it needs one command on a machine with Codex installed, and it is
listed in §8. Write the codec behind a `Framing` interface until then.
(Framing was settled after this paragraph was written — §8 item 6 — and the
`Framing` interface survived as twenty lines of optional insurance rather than
as a hedge.)

### Four things building `relayd/internal/adapter/codex` settled that this section left open

- **`item/reasoning/summaryPartAdded` is a boundary, and the event model has no
  field for it.** Two summary parts are two thoughts, and concatenating them
  produces one wrong sentence. The adapter emits the boundary as a paragraph
  break in the `Reasoning` stream — `"\n\n"`, only *between* parts, never
  leading. That is a rendering decision about something genuinely observed, not
  an invented event, and it is free at the TTS layer because `Reasoning` is
  never spoken. The `Summary` flag stays set, so the summariser can still tell
  the speakable stream from the raw one.

- **`thread/status/changed` must not become a `NeedsInput`.** It is a second,
  independent "this session is blocked" signal and the only one that survives a
  reconnect — but after a reconnect the JSON-RPC request it refers to is gone,
  so there is nothing to answer. A question nobody can answer is a hung session.
  The adapter records the flags as state (`Session.Blocked()`), logs when they
  say blocked while it holds no answerable request, and emits nothing.

- **`item/completed` is authoritative over the deltas, and only one of the two
  may be emitted.** Re-emitting the completed `agentMessage.text` after
  streaming it speaks the whole answer twice. The adapter tracks which items
  streamed and carries the completed payload only for the ones that did not —
  which is also what makes a runtime with streaming disabled still produce text.
  Same rule for `reasoning` content and for `commandExecution.aggregatedOutput`.

- **There is no `mcpServers` field on any thread method.** MCP servers reach
  Codex through the free-form `config` object; see §8 item 13.

---

## 4. OpenClaw, Hermes and OpenCode — one ACP adapter

```bash
openclaw acp --session agent:main:main   # bridges the OpenClaw Gateway
hermes acp                                # native ACP mode
opencode acp                              # native ACP mode
```

All three speak the Agent Client Protocol, so **one adapter serves three of the
five runtimes**. ACP's
model maps cleanly onto everything above: `session/new` and `session/load`,
`session/prompt` to send a turn, `session/update` notifications for streaming
progress, `session/request_permission` for needs-input, and `session/cancel`.

The protocol schema is **vendored** at `docs/fixtures/adapters/acp-schema.json`
(from `@zed-industries/agent-client-protocol@0.4.5`), with a method-by-method
index at `docs/fixtures/adapters/acp-methods.md`. Everything in this section is
checkable against that file rather than against memory; the nine assertions at
the end of `acp-methods.md` are what a version bump has to break.

Two runtime-specific notes:

- **OpenClaw's ACP is a bridge, not the source of truth.** It sits in front of
  the Gateway, and session keys look like `agent:main:main`. `--require-existing`
  makes a missing session fail loudly instead of silently creating one, which is
  what we want when routing to a session the registry believes exists.
- **Hermes keeps sessions in SQLite** (`hermes sessions list|export`), so the
  registry can be reconciled against the runtime's own store after a crash
  rather than rebuilt from memory.

### The handshake, and what it commits us to

Newline-delimited JSON-RPC 2.0 over the agent's stdin/stdout — one message per
line, no `Content-Length` framing, and **symmetric**: the agent issues requests
to us as freely as we issue them to it.

`initialize` is the mandatory first call and negotiation is exactly one round
trip. We send the latest wire version we support (`PROTOCOL_VERSION`, today `1`);
the agent answers with that version if it supports it, otherwise with **the
latest version it supports**, which may be lower or higher than ours. There is no
second round: if we cannot speak what came back, we disconnect. Version skew is
therefore a startup-time failure with a clear message, never a mysterious
mid-session one.

Three things come back with it and all three change what the adapter may do:

- **`agentCapabilities.loadSession`** — if false, `session/load` is unavailable
  and reattaching to an existing session is impossible on that runtime. The
  registry must fall back to starting a new session and saying so.
- **`promptCapabilities`** — `image`, `audio`, `embeddedContext`, each default
  `false`. Baseline is text and `resource_link` only. A photo from the glasses
  cannot go into a prompt on a runtime that did not advertise `image`.
- **`authMethods[]`** — if the runtime is not logged in, `session/new` fails with
  JSON-RPC **`-32000` "Authentication required"**; recovery is `authenticate`
  with one of these ids, then retry. That belongs in the installer
  (`ORCHESTRATOR.md` §2), not mid-conversation — a device-code flow cannot be
  completed by voice.

The mirror of that is `clientCapabilities`, which is what we let the *agent* ask
*us* for: `fs.readTextFile`, `fs.writeTextFile` and `terminal`. **Relay declares
all three false.** This is a decision, not a probe result, and the reason is §5's
grounding rule: advertise them and the agent does its file and shell work by
calling back into us over RPC, where nothing obliges it to also appear in
`session/update`. Declining them keeps every edit and every command visible as a
`tool_call` we can narrate. An `fs/*` or `terminal/*` call that arrives anyway
gets `-32601`, which is the honest degradation — visibly refused, not faked.

Session setup then needs an **absolute** `cwd` and the `mcpServers[]` array, which
is where `MEMORY.md` §7's shared registry is injected. Note that `session/load`
**replays the whole conversation back as `session/update` notifications** before
it resolves; the adapter has to know it is in replay and not ping the user about
a completed turn from two weeks ago.

### Steering — settled, and the answer is no

Verified against the vendored schema for
`@zed-industries/agent-client-protocol@0.4.5`. The complete method surface is
**17 methods**, and this is all of them:

```
client → agent   initialize  authenticate
                 session/new  session/load  session/prompt  session/cancel
                 session/set_mode  session/set_model †
agent → client   session/update (notification)  session/request_permission
                 fs/read_text_file  fs/write_text_file
                 terminal/create  terminal/output  terminal/wait_for_exit
                 terminal/kill  terminal/release
```

† `session/set_model`, and the `models` field on the session responses, are the
only members of the surface marked **UNSTABLE** upstream — "not part of the spec
yet, and may be removed or changed at any point". Relay does not call it.
Everything else above is stable.

`session/cancel` is a **notification**, not a request; so is `session/update`.
Everything else is a request/response pair.

**There is no steer or inject method.** `session/prompt` and `session/cancel` are
the only ways to affect a turn, so the assumption held: ACP cannot push an
utterance into a turn already running, and that now covers *three* of five
runtimes.

This is a load-bearing negative, so here is the evidence rather than the
assertion. The strings `steer`, `inject` and `interrupt` do not occur anywhere in
the published package — not in `schema/schema.json`, not in the TypeScript
sources, not in the generated `.d.ts`. The `ClientRequest` union in the vendored
schema has exactly eight branches, listed above. And `session/prompt` is a single
long-lived request that does not resolve until the whole turn is over, with no
second-prompt-while-running semantics defined anywhere — so even a runtime that
wanted to accept one has nothing to accept it *as*.

One caveat, and it is the thing to re-probe rather than the thing to worry about:
ACP has a first-class extension mechanism. Any method whose name begins with `_`
is dispatched to `extMethod` / `extNotification` and is invisible to the schema.
A runtime *could* ship `_openclaw/steer` tomorrow. So the honest claim is "no
steering in ACP 0.4.5, and none of the three runtimes is known to extend it" —
and the adapter should log unknown `_`-prefixed methods rather than dropping them
silently, because that log line is how we would find out.

But `session/cancel` makes the fallback better than plain queueing. Its contract
is explicit — on cancel the agent stops model requests, aborts tools, **flushes
pending `session/update` notifications**, then resolves the original prompt with
`StopReason::cancelled`. Nothing observed is lost. Two obligations come with it,
both mandatory: the client must keep accepting `session/update` *after* sending
the cancel, and it must resolve any outstanding `session/request_permission` with
the `cancelled` outcome or the agent's turn cannot unwind. So:

| The utterance is | Do |
|---|---|
| an addition — "also update the changelog" | **queue** until `end_turn`, and say so |
| a redirect — "no, stop, do X instead" | **cancel, then re-prompt** with the merged instruction |

The small model has to distinguish those two, and when unsure it queues and says
which it chose — same announce-and-undo rule as `ORCHESTRATOR.md` §4.

### `StopReason` — the turn boundary

`session/prompt` resolves with exactly one field, `stopReason`, and that single
value *is* the turn boundary. Five values, confirmed complete against the schema:

| Value | Meaning | What the user hears |
|---|---|---|
| `end_turn` | finished normally | `TurnCompleted{ok:true}` → ping |
| `max_tokens` | hit the token ceiling | failed; offer to continue |
| `max_turn_requests` | hit the cap on agent requests within one user turn | failed; offer to continue |
| `refusal` | the agent declined | failed — say so plainly |
| `cancelled` | we sent `session/cancel` | not a ping; feeds the re-prompt |

`refusal` has a consequence the others do not: **the user prompt and everything
after it are dropped from the next prompt**. The adapter must not treat a refused
turn as merely failed and retry on top of it — that context is gone, and the
re-prompt has to carry the instruction again.

### `session/request_permission` — the needs-input path for three runtimes

A **request**, not a notification, so the agent blocks until we answer, for as
long as we take. That is what makes §7's voice-answerable approval real on
OpenClaw, Hermes and OpenCode rather than aspirational.

Request carries `sessionId`, a `toolCall`, and `options[]`:

```json
{"sessionId":"sess_abc",
 "toolCall":{"toolCallId":"call_1","title":"Run npm test","kind":"execute",
             "status":"pending","rawInput":{"command":"npm test"}},
 "options":[{"optionId":"o1","name":"Allow","kind":"allow_once"},
            {"optionId":"o2","name":"Always allow","kind":"allow_always"},
            {"optionId":"o3","name":"Reject","kind":"reject_once"}]}
```

Response is one of exactly two shapes:

```json
{"outcome":{"outcome":"selected","optionId":"o1"}}
{"outcome":{"outcome":"cancelled"}}
```

Two traps in that shape. **`toolCall` is a `ToolCallUpdate`, not a `ToolCall`** —
only `toolCallId` is required, so `title`, `kind` and `rawInput` may all be
missing. There is no guarantee of a human-readable description to read aloud, and
when it is absent the correct behaviour is to say what we do know rather than
infer a description from `rawInput`. And **`options[]` is agent-supplied and
open-ended**: `PermissionOptionKind` (`allow_once`, `allow_always`, `reject_once`,
`reject_always`) is a UI hint, not a fixed menu. We speak the `name`s we were
given, match the spoken reply back to an `optionId`, and never offer an option
that was not in the array.

Note the asymmetry with Claude Code, which returns `allow`/`deny` plus an
`interrupt` flag that aborts the turn outright. ACP has no equivalent hard stop
inside the permission response — rejecting a tool is not cancelling a turn. "No,
stop" spoken at the glasses maps to `reject_*` **plus** a `session/cancel`.

---

## 5. The normalized event model

Three adapters, one internal stream. Everything above collapses to:

```
TurnStarted    { session, turn }
TextDelta      { session, turn, text }              → streaming TTS
Reasoning      { session, turn, text }              → never spoken
ToolStarted    { session, turn, tool, target }      → narration material
ToolOutput     { session, turn, chunk }
PlanUpdated    { session, turn, steps[] }           → best narration material
NeedsInput     { session, turn, kind, prompt, options[], reply(...) }   ⚑ PING
TurnCompleted  { session, turn, ok, stop_reason, cost, duration }       ⚑ PING
Error          { session, turn, message }           ⚑ PING
```

Adapter coverage, honestly:

| Event | Claude Code | Codex | ACP (OpenClaw, Hermes, OpenCode) |
|---|---|---|---|
| TextDelta | ✅ | ✅ | ✅ |
| Reasoning | ✅ thinking blocks | ✅ | ✅ `agent_thought_chunk` — protocol-native; whether each of the three *emits* it is unverified, see §8 |
| PlanUpdated | ✗ — **not emitted at all**, and that is a decision; see "Claude Code and PlanUpdated" below | ✅ native | ✅ `plan` session update |
| ToolStarted / Output | ✅ | ✅ | ✅ |
| NeedsInput | ✅ via permission-prompt MCP tool | ✅ native requests | ✅ `request_permission` |
| TurnCompleted | ✅ `result` | ✅ `turn/completed` | ✅ `session/prompt` resolving with a `stopReason` |
| Mid-turn steering | ✅ verified | ✅ `turn/steer`, but only with a matching `expectedTurnId` | ✗ **verified absent** — cancel + re-prompt |
| **Cancel a turn** | ⚠ **only while blocked on the permission prompt**, where a deny with `interrupt: true` aborts it. `system/init` advertises `interrupt_receipt_v1`, so the feature exists, but the client-side message is not in the vendored trace — §8 item 9 | ✅ `turn/interrupt` | ✅ `session/cancel` |
| Per-turn cost | ✅ `total_cost_usd`, **as a delta between consecutive `result` events** | ⚠ **tokens only** — `thread/tokenUsage/updated`; the contract carries no USD | ✗ **not in the protocol at all** — must come from each runtime's own store |

ACP's `session/update` has **eight** variants, discriminated by `sessionUpdate`:

| Variant | Normalized event |
|---|---|
| `agent_message_chunk` | `TextDelta` |
| `agent_thought_chunk` | `Reasoning` — never spoken |
| `user_message_chunk` | replay/echo — not spoken |
| `tool_call` | `ToolStarted` |
| `tool_call_update` | `ToolOutput`, or a status change |
| `plan` | `PlanUpdated` |
| `available_commands_update` | tool-list refresh — §9 problem 6 |
| `current_mode_update` | mode changed, possibly by the agent itself |

**That `plan` variant corrects an earlier claim here** — structured plans are
available on four of five runtimes, not only Codex, so plan-based narration is the
normal path rather than the exception. Claude Code is the lone runtime with no
plan at all, and the next section says what the adapter does about that.

`current_mode_update` was missing from the earlier list of seven. It matters
because an agent may change its own mode mid-session — "ask" to "code", say — and
that changes permission behaviour underneath a session the registry believes it
understands. Surface it; do not swallow it.

`ToolCallStatus` is `pending` → `in_progress` → `completed` | `failed`, and
`tool_call_update` may carry only `toolCallId` with every other field null, so the
adapter must merge updates onto the `tool_call` it already has rather than
expecting each one to be self-describing.

**Per-turn cost is worse than "partial" on ACP.** The word "token" appears exactly
twice in the whole 87-definition schema, both times in the `max_tokens` stop
reason. There is no token count, no cost field and no usage object anywhere in the
protocol. Metering for these three runtimes is necessarily out-of-band and
per-runtime, which is what §8 records.

**An adapter never invents an event it cannot observe.** Where a cell is ✗, the
capability is absent and the orchestrator must degrade visibly — no PlanUpdated
means narration falls back to tool activity, and the small model says less rather
than guessing more. This is §3b's grounding rule enforced at the adapter
boundary, which is the only place it can actually be enforced.

### Claude Code and PlanUpdated — the tension, resolved

This section used to say two things that cannot both be obeyed: the coverage
table said "✗ — synthesise from tool calls", and the paragraph above says an
adapter never invents an event it cannot observe. **The Claude Code adapter
resolves it by not emitting `PlanUpdated` at all**, and reports
`CapPlan = SupportNo` with the reason attached rather than `SupportSynthesized`.
Three reasons, in order of weight:

1. **A plan built from tool calls is not a plan.** It is a description of what
   already ran. `PlanUpdated`'s value on Codex and ACP is that the agent states
   its intent *before* acting, which is what makes it the best narration
   material there is; the Claude Code version would be a list of completed
   steps, which the orchestrator already has, in order, as `ToolStarted` and
   `ToolOutput`.
2. **It would be strictly redundant, and worse than redundant.** §3b tells the
   small model to trust a plan above looser signals. Handing it a "plan"
   assembled out of the same tool events it can already see does not add
   information — it re-labels inference as structure and then instructs the
   model to weight it more heavily. That is the exact failure the grounding rule
   exists to prevent, and a `Synthesized: true` flag does not fix it, because
   the flag is advisory while the event's presence is not.
3. **The only richer source is prose.** Anything better than tool activity would
   have to come from reading the assistant's text and guessing which sentences
   are steps. `SYSTEM.md`'s standing rule is that if you find yourself parsing
   prose you are on the wrong path.

So for this runtime the ✗ means what a ✗ is supposed to mean: **narration falls
back to tool activity and the small model says less rather than guessing more.**

`SupportSynthesized` and `PlanUpdated.Synthesized` stay in the code — they are
the right shape for a runtime that has a genuine partial signal — but no adapter
uses them today, and adding one is a change to this section first.

`relayd/internal/adapter/coverage.go` still carries `CapPlan: SupportSynthesized`
in `Baseline(ClaudeCode)` because that file is the *documented* default; the
adapter narrows it to `SupportNo` at construction, and
`TestNoPlanUpdated` asserts both the capability and that no plan event survives
a full replay of the vendored trace.

Two further refinements from implementing the grounding rule in
`relayd/internal/event` and `relayd/internal/adapter`, each of which the table
above left implicit:

- **"Not reported" is not zero.** Every cost and token field is a pointer, and
  nil means the runtime does not report it. ACP turns carry no usage object at
  all rather than a zeroed one, so the console shows a gap instead of claiming a
  free turn.
- **A replayed event is not news, and this is a ping decision.** `session/load`
  replays a whole conversation back as `session/update` notifications before it
  resolves, and Claude Code echoes injected turns with `isReplay`. Every event
  carries a `Replay` marker and all three ⚑PING events return "no ping" when it
  is set. Without that, reattaching to a session fires a completion ping for
  every turn in its history.

---

## 6. Summarising a turn for speech

The orchestrator watches the normalized stream and produces something a person
hears. Three rules make this work.

**Summarise events, not the transcript.** The raw output of a coding turn is
thousands of tokens of diffs and command output. The event stream is already
structured — which tools ran, against what, what the plan was, whether it
succeeded. Feeding the small model *that* is cheaper, faster, and much harder to
hallucinate from.

**Budget by seconds, not tokens.** Speech runs about 14 characters per second, so
a 400-character summary is nearly half a minute in someone's ear. Caps:

| Moment | Cap | Roughly |
|---|---|---|
| Immediate ack | ~40 chars | "on it — payments branch" |
| **Routing announcement** | **~90 chars** | "starting a new Codex session on the API repo" |
| Mid-task progress | ~90 chars | "tests are running, 40 in so far" |
| **Turn completed** | **~160 chars** | two short sentences |
| Needs input | ~120 chars + options | the question, then the choices |

The routing announcement is its own row, added while building `internal/routing`,
because it is an acknowledgement that has to carry an object. `ORCHESTRATOR.md`
§4 rule 2 and `MEMORY.md` §8 both make it the thing that turns a wrong routing
guess into a correction rather than a discovery — and both write it out by
example at 44 characters, over the ack budget. Clipping it removes "on the API
repo", which is exactly the clause the human is listening for. So it gets the
progress budget, and it is the one spoken line that is **never phrased by a
model**: it is the audit trail for a routing decision, and a paraphrased audit
trail is not one.

**Lead with the outcome.** People listening while walking retain the first
clause and little else. "Tests pass. Two files changed." beats "I've finished
working on the payments branch and I can report that…". The small model's prompt
should require outcome-first phrasing and forbid preamble.

If the turn failed, say what failed and stop. Do not read a stack trace aloud —
offer it: "Build broke on the auth module. Want the error?"

### Enforced in code, not asked for in a prompt — `relayd/internal/summarize`

All three rules above are things a model is *told*. Every one of them is also
checked, because a prompt is a request and none of these can be left to a
request. What the implementation added:

**The digest cannot hold tool output.** The type the narrator speaks from
carries a tool's name, target, status and *byte count* — never a byte of what it
printed. A type that cannot hold a diff cannot leak one into a prompt or into
someone's ear, which is a stronger guarantee than remembering to truncate. The
same type carries `Reasoning` only as a boolean, so "never spoken on any
runtime" is structural too.

**The caps are applied after the model answers**, on runes rather than bytes,
cutting at a sentence boundary if one fits and a word boundary otherwise. Never
mid-word: a clipped word is heard as a stutter and makes the line sound broken
rather than short.

**Preamble is rejected, not edited.** Output matching "I've finished…", "I'm
happy to report…", "Here's a summary…" is thrown away and a deterministic
template speaks instead. Rewriting the model's sentence to remove its preamble
means parsing prose, which is the standing rule's wrong path, and a half-edited
sentence is worse than a plain one.

**The offer survives the clip.** When a failed turn's line will not fit, the
outcome is shortened and "Want the error?" is kept — it is the half the listener
can act on. And a turn with no error to offer, such as one the user cancelled,
does not offer one: "Want the error?" answered with "there isn't one" is worse
than not asking.

**Invented specifics are rejected mechanically.** Every identifier-shaped token
in the model's line — a path, a package, a file, an error code — must appear in
the brief it was given, or the line is dropped. It is narrow on purpose: a line
that names nothing passes, because §3b's *vague and true beats precise and
invented* is a rule about what to prefer, not a licence to say nothing.

**With no observable events, the model is not asked at all.** Not asked and then
checked — not asked. That is the strongest available form of "given no event,
say still working, never invent a specific", and it is also free.

**The template is the floor, and it ships without a model.** Every word of it
comes from an event, it leads with the outcome, and a box with no small model
configured still speaks. That makes the model an improvement rather than a
dependency.

---

## 7. Pings — when the user hears from us unprompted

Two events reach the user without being asked for. They are not equally urgent
and should not behave the same way.

| | `NeedsInput` | `TurnCompleted` |
|---|---|---|
| Urgency | **blocking** — nothing proceeds | informational |
| Glasses | speak, may interrupt | speak only if idle |
| Phone | notification with actions | notification |
| If unheard | re-ping once at 2 min, then hold | never repeat |
| Batching | never | yes |

**Needs-input must be answerable by voice**, or the feature is decorative — the
whole premise is that the laptop is closed. So the ping carries its options and
the reply routes straight back through the adapter: Codex resolves the pending
JSON-RPC request, Claude Code returns from the permission-prompt tool, ACP
answers `request_permission`. The session unblocks without anyone opening a
terminal.

Three things this needs to get right:

**Do not interrupt the user mid-sentence.** Turn-taking is already
orchestrator-owned (`ORCHESTRATOR.md` §3). A completion ping waits for a gap. A
needs-input ping may interrupt, because the alternative is a session blocked
indefinitely — but it waits for the current *utterance* to end, not the current
conversation.

**Batch completions, never blocks.** Three sessions finishing inside a minute is
one ping — "payments and docs are done, the migration failed" — not three. Three
sessions *asking* is three pings, because each one needs a distinct answer.

**Confirm consequential actions every time.** `ORCHESTRATOR.md` §4b already
requires spoken confirmation for anything with effects outside the machine.
Needs-input is the mechanism that delivers it, and it must not be suppressible by
batching or quiet hours — an approval the user never heard is an approval they
did not give.

**Quiet hours apply to completions only.** A blocked session at 3 a.m. is still
blocked at 8 a.m.; holding the ping just means eight wasted hours. Hold the
*speech*, keep the phone notification silent-but-present.

---

## 8. What is still unverified

Named rather than buried, because each one could change a design decision.
**Every row carries a disposition**, so nothing here is merely listed:

| | Meaning |
|---|---|
| **RESOLVED** | closed, with the evidence named inline |
| **NEEDS THE MAC** | requires a runtime installed *and authenticated with the user's own subscription*. A cloud sandbox cannot do this; device-code flows on an invisible container do not work |
| **UPSTREAM CHURN** | not a gap in what we know — a standing risk in someone else's release cadence. Mitigated, not closable |

Last dispositioned 2026-08-10.

1. **OpenCode's `serve` HTTP API** as an alternative to its ACP mode.
   **NEEDS THE MAC.** ACP is preferred only because it shares an adapter, and
   that reason is good enough to ship on. Closing it needs `opencode serve`
   running against an authenticated OpenCode install so the two can be compared
   on the same session — specifically whether `serve` exposes token usage and
   per-turn cost that ACP 0.4.5 structurally cannot (item 3). Do not reopen the
   adapter choice without that comparison.

2. **Codex app-server stability.** Marked `[experimental]`.
   **UPSTREAM CHURN**, and permanently so. The vendored schemas make a change
   visible, `check_codex_methods.py` turns it into a red test, and the
   `codex exec --json` fallback makes it survivable. There is nothing to probe;
   the mitigation *is* the answer. Re-vendor the schemas on every codex upgrade.

3. **Per-turn cost for ACP runtimes.**
   **RESOLVED at the protocol level, NEEDS THE MAC for the fallback.** ACP 0.4.5
   has no token, cost or usage field at all — read out of the vendored schema, so
   this half is closed and closed negatively. What remains is only the
   per-runtime fallback: OpenCode has `opencode stats`, Hermes stores
   `estimated_cost_usd` and `actual_cost_usd` per session in SQLite, and no
   OpenClaw equivalent has been found — that last one is the open question, and
   it needs an OpenClaw install with real history on it. Design for
   **per-provider** metering, because per-runtime is not uniformly available.

4. **Which ACP capabilities each of the three runtimes actually advertises.**
   **NEEDS THE MAC.** The protocol is vendored but `agentCapabilities` is
   per-runtime and per-version. Specifically needed: one `initialize` call
   against each of `openclaw acp`, `hermes acp` and `opencode acp`, capturing
   `agentCapabilities.loadSession` (decides whether the registry can reattach to
   an existing session or must always start a new one) and
   `agentCapabilities.promptCapabilities.{image,audio,embeddedContext}` (decides
   whether a glasses photo can enter a prompt). Three commands, one answer each,
   and the answer belongs in §4. **Until then §4 must not claim reattach works.**

5. **Whether any of the three ships `_`-prefixed ACP extensions.**
   **NEEDS THE MAC.** The extension mechanism is invisible to the schema, so the
   vendored contract cannot rule out a runtime-specific steer — which matters
   because §4 concluded ACP has no mid-turn steering at all. The probe is the
   same session as item 4: log every method name seen that is not in
   `acp-methods.md`, over one real multi-turn session per runtime.

6. **Codex app-server's JSON-RPC framing.**
   **RESOLVED — 2026-08-10, from the vendor's own transport source rather than a
   run.** `codex-rs/app-server-transport/src/transport/stdio.rs` reads with
   `BufReader::lines()` / `next_line()` and writes with `json.push('\n')` then
   `write_all`. So: **newline-delimited JSON, one object per line, no headers.**
   Two corrections fall out of the same read of
   `codex-rs/app-server-protocol/src/rpc.rs`:
   - **There is no `jsonrpc` envelope field.** The crate says so in a comment —
     *"We do not do true JSON-RPC 2.0, as we neither send nor expect the
     `jsonrpc: 2.0` field."* Do not emit it and do not require it. Any probe
     snippet that sends `{"jsonrpc":"2.0",…}` is wrong-shaped, including the one
     in `codex-methods.md` §1.
   - **`JSONRPCMessage` is `#[serde(untagged)]`**, so the client demultiplexes on
     field presence, not on a discriminator: `id`+`method` → request,
     `method` alone → notification, `id`+`result` → response, `id`+`error` →
     error. That is the dispatch a Go decoder must implement.

   Caveat, and it is the reason this is not marked as good as a run: the source
   read is `openai/codex` at `main`, while the vendored schemas are codex-cli
   **0.140.0**. Framing is a structural property and unlikely to have moved, but
   the two were not read at the same revision. The one `Content-Length` hit in
   that repo is in `tests/suite/v2/remote_control.rs` — a *different* transport,
   not stdio. **Ship NDJSON.** The `Framing` interface is now optional insurance
   rather than a required hedge; keep it only if it costs nothing.

7. **Codex app-server's JSON-RPC *results*.**
   **PARTLY RESOLVED (envelope), NEEDS THE MAC (payloads).** The same read of
   `rpc.rs` settles the envelope: a response is exactly `{id, result}` and an
   error is exactly `{id, error:{code, message, data?}}`. What is still outside
   the vendored contract is the *content* of `result` per method —
   `generate-json-schema` emits request and notification params only, and there
   is no `ServerResponse.json`. Still needed, and only a live server gives it:
   the reply shapes for `item/fileChange/requestApproval`,
   `item/tool/requestUserInput` and `mcpServer/elicitation/request`. Two orphaned
   definitions (`CommandExecutionApprovalDecision`, `AdditionalPermissionProfile`)
   cover two of the five approval types; three are unknown. The adapter can still
   be written today by taking the thread id from the `thread/started`
   *notification* rather than the `thread/start` result — do that, and leave the
   three approval replies behind a feature flag that is off until probed.

8. **What `capabilities.experimentalApi` actually gates.**
   **MECHANISM RESOLVED, effect set NEEDS THE MAC.** From
   `codex-rs/app-server/tests/suite/v2/`, the flag is enforced as a **per-method
   gate at request time**: calling a gated method without it returns a JSON-RPC
   error whose message is literally `"<method> requires experimentalApi
   capability"` (`server/diagnostics`, `account/login/start.amazonBedrock` and
   others assert exactly that string). So it fails **loudly, not silently**,
   which is the property that matters: the adapter can discover the gated set
   empirically at runtime and degrade per method rather than guessing up front.
   What is still unknown is *which* methods are gated in codex-cli 0.140.0 —
   the set is per-version and the schema does not mark it. Ask for
   `experimentalApi: true`, and treat that error message as a capability
   negative rather than a failure.

9. **How a client cancels a Claude Code turn.**
   **NEEDS THE MAC.** `system/init` advertises `interrupt_receipt_v1` and
   `interrupt_cancel_queued_v1`, so interrupting is a protocol feature — but the
   49-event trace contains only the *runtime's* side of the conversation, and
   nothing in it shows what the client sends. The adapter therefore cancels the
   one way it has verified, by answering an open permission prompt with
   `interrupt: true`, and returns an `*UnsupportedError` for `CapCancel` when no
   question is open. That is a real gap: "no, stop" spoken at the glasses works
   only while a turn is blocked on an approval.
   The probe is one command on a machine with Claude Code installed: start a
   long turn with `--input-format stream-json`, write
   `{"type":"control_request","request_id":"<uuid>","request":{"subtype":"interrupt"}}`
   to stdin, and record what comes back. `Options.UnverifiedControlInterrupt`
   sends exactly that line and is **off by default**; flipping it on is the
   whole fix if the probe succeeds. Until then `CapCancel` stays
   `SupportUnknown` — the runtime plainly can, we have not seen how to ask.

10. **Whether a permission call really survives a 27-hour block over HTTP.**
    **NEEDS THE MAC.** The MCP tool-call timeout is `1e8` ms and progress
    notifications do not extend it, which is what makes voice-answerable
    approval real — but that is the *MCP* budget. The adapter serves the
    permission tool over loopback HTTP, and an HTTP client has timeouts of its
    own that the MCP spec says nothing about. The probe: point
    `--permission-prompt-tool` at the adapter's endpoint, trigger an approval,
    leave it unanswered for an hour, and see whether the call is still open. If
    it is not, `ServeStdio` is already written and the fix is a helper process
    rather than a redesign. Nothing about the request or response *shapes*
    depends on this — those were probed and are in §2.

11. **`docs/fixtures/adapters/codex.trace.json` is schema-derived, not recorded.**
    **NEEDS THE MAC.** `BUILD-PROMPT.md` asks for a recorded Codex trace "same
    shape, same directory" as the Claude Code one. No `codex` binary exists in
    the build container, so the file that is there now was **constructed from
    the vendored codex-cli 0.140.0 schemas** by
    `relayd/testdata/codex/gen_trace.py` and validated message-by-message
    against the definition each one names — in Python at generation time, and
    again in Go by `TestTraceValidatesAgainstTheVendoredSchemas`, so a
    re-vendored schema turns it red. Its first record says so in the file
    itself: `"provenance": "SCHEMA-DERIVED, NOT RECORDED"`, and
    `TestTraceSaysItIsNotARecording` fails if that label is ever removed.
    It is a good contract fixture and it is **not evidence of runtime
    behaviour**. Three things it cannot tell us, all of them listed in its own
    `unverified` block: every JSON-RPC *result* payload (there is no
    `ServerResponse.json` — item 7), the real reply to
    `item/commandExecution/requestApproval`, and the true interleaving of
    responses with the notifications around them. **Replace it with a real
    `codex app-server` recording**, keeping the same record shape, and this item
    closes. The same applies to `relayd/testdata/codex/exec-stream.ndjson`,
    which stands in for a `codex exec --json` capture nobody has taken.

12. **What `codex exec --json` actually emits.**
    **NEEDS THE MAC.** `generate-json-schema` describes app-server only, so the
    one-shot fallback of §1 has no contract at all. The `ExecAdapter` handles
    that the only honest way available: lines shaped like app-server
    notifications go through the same mapping table as the real adapter, and
    anything else is logged by its discriminator key and dropped. Its capability
    descriptor says `SupportUnknown` — not `SupportNo` — for plan, reasoning,
    tokens and context window, because nobody has looked. The probe is one
    command: `codex exec --json 'say hi' | head -40`. Those "unrecognised stream
    shape" log lines *are* the probe result if it is ever run in anger.

13. **Codex MCP server injection.**
    **NEEDS THE MAC.** None of `thread/start`, `thread/resume` or `thread/fork`
    has an `mcpServers` field; Codex takes MCP servers from config, and the
    `config` param is `additionalProperties: true` with no schema behind it. So
    `MEMORY.md` §7's shared registry is injected as
    `config.mcp_servers.<name>.{command,args,env|url}`, following Codex's own
    `config.toml` layout. That layout is read from the CLI's documented config
    file, **not** from anything in the vendored contract, so the adapter logs a
    warning every time it does it. One `thread/start` with one MCP server on a
    real Codex closes this.

14. **`docs/fixtures/adapters/acp.trace.json` is schema-derived, not recorded.**
    **NEEDS THE MAC**, and three times over — one recording per runtime.
    `BUILD-PROMPT.md` asks for a recorded ACP trace "same shape, same directory"
    as the Claude Code one. No `openclaw`, `hermes` or `opencode` binary exists
    in the build container, so the file that is there now was **constructed from
    the vendored `@zed-industries/agent-client-protocol@0.4.5` schema** by
    `relayd/testdata/acp/gen_trace.py` and validated message by message against
    the `$defs` entry each one names — in Python at generation time, and again in
    Go by `TestTraceValidatesAgainstTheVendoredSchema`, so a re-vendored schema
    turns it red. Its first record says so in the file itself:
    `"provenance": "SCHEMA-DERIVED, NOT RECORDED"`, and
    `TestTraceSaysItIsNotARecording` fails if that label is ever removed.
    It covers a whole session — handshake, `session/new`, all eight
    `session/update` variants including `plan`, a `session/request_permission`
    answered, a refused `fs/read_text_file`, a `session/cancel` with its
    mandatory `cancelled` outcome and flushed update, the redirect that follows,
    and a queued addition delivered afterwards — and
    `TestReplayTraceThroughTheAdapter` runs the whole thing through the real
    adapter once per runtime. It is a good contract fixture and it is **not
    evidence of runtime behaviour**. Four things it cannot tell us, all listed
    in its own `unverified` block: the `agentCapabilities` each runtime really
    advertises (item 4), the real shape of a session id on each, the true
    interleaving of notifications with the responses around them, and whether
    any of the three emits `agent_thought_chunk` at all. **Replace it with three
    real recordings**, keeping the same record shape, and this item closes.

15. **Whether OpenClaw's `--session` is really launch-scoped.**
    **NEEDS THE MAC.** `openclaw acp --session agent:main:main` takes the
    Gateway session key as an argument to the *process*, which the adapter reads
    as "one `openclaw acp` process serves one session": `Resume` against a
    different key returns `ErrSessionNotFound` with a message saying to dial a
    second adapter, rather than quietly talking to whatever session the bridge
    happens to be pointing at. That reading is inferred from the command line,
    not probed. If the bridge in fact multiplexes — if `session/new` on a
    process launched with one key can return a second, different `sessionId` —
    then the guard in `internal/adapter/acp` is one adapter per session where
    one would do, which is wasteful but not wrong. The probe is two
    `session/new` calls on one `openclaw acp` process.
