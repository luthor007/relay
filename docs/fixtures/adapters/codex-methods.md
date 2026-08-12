# Codex app-server — complete method inventory

**Source of truth.** Extracted mechanically from the three schema files vendored
beside this one, which were produced by
`codex app-server generate-json-schema --out docs/fixtures/adapters/` against
**codex-cli 0.140.0**:

| File | Direction | Variants | Definitions |
|---|---|---|---|
| `ClientRequest.json` | client → server, expects a result | 84 | 158 |
| `ServerNotification.json` | server → client, no result | 66 | 176 |
| `ServerRequest.json` | server → client, **expects a result** | 10 | 54 |

Nothing in this document is remembered or inferred from the CLI's help text. Where
a "purpose" cell is in "quotes" it is the schema's own `description`, verbatim.
Unquoted purposes are derived from the method name plus its params — the schema
carries no description for those, and that is itself a fact worth knowing.

**This file is checked by machine.** `check_codex_methods.py` in this directory
re-reads the three schemas, parses the three tables below, and fails if they
disagree — a method added, removed, or re-typed upstream turns into a red test
rather than a silently wrong adapter.

```bash
python3 docs/fixtures/adapters/check_codex_methods.py          # verify
python3 docs/fixtures/adapters/check_codex_methods.py --print  # fresh rows to paste
```

Disposition legend, in the last column of every table:

| | Meaning |
|---|---|
| **use** | the Relay Codex adapter calls it or handles it today |
| **must-answer** | server → client request the adapter must reply to even though Relay does not want the feature — an unanswered JSON-RPC request stalls the server |
| **later** | real value for a named later milestone; deliberately not wired yet |
| **ignore** | out of scope for an orchestrator; parsed and dropped |

---

## 1. Framing — NOT specified by these schemas

The three files describe **payloads only**. They contain no transport section, and
a full-text search finds zero occurrences of `Content-Length`, `jsonrpc`,
`newline`, `ndjson`, `delimited`, or `framing` in any of them. They do not even
carry the `"jsonrpc": "2.0"` envelope field — each `oneOf` variant declares only
`id` / `method` / `params`.

So the schemas **imply nothing about framing**. The answer came from the
implementation instead.

> **RESOLVED 2026-08-10 — NDJSON, and no `jsonrpc` envelope.** Read out of
> `openai/codex` at `main`, not inferred:
>
> - `codex-rs/app-server-transport/src/transport/stdio.rs` reads with
>   `BufReader::lines()` / `next_line()` and writes with `json.push('\n')`
>   followed by `write_all`. **One JSON object per line, no headers.**
> - `codex-rs/app-server-protocol/src/rpc.rs` says in a comment: *"We do not do
>   true JSON-RPC 2.0, as we neither send nor expect the `jsonrpc: 2.0` field."*
>   So **do not send it and do not expect it back.**
> - `JSONRPCMessage` is `#[serde(untagged)]`. Demultiplex on field presence:
>   `id`+`method` → request, `method` alone → notification, `id`+`result` →
>   response, `id`+`error` → error.
> - `JSONRPCResponse` is exactly `{id, result}`; `JSONRPCError` is exactly
>   `{id, error:{code, message, data?}}`.
>
> **Caveat:** this is a source read at `main`, while the schemas beside it are
> codex-cli **0.140.0** — the two were not taken at the same revision, and a
> source read is not an executed probe. The one `Content-Length` hit in that
> repository is in `codex-rs/app-server/tests/suite/v2/remote_control.rs`, a
> *different* transport, not stdio.

The probe below is still worth running on the author's Mac to confirm 0.140.0
behaves the same, but note the corrected request shape — **no `jsonrpc` field**:

```bash
printf '{"id":1,"method":"initialize","params":{"clientInfo":{"name":"relay","version":"0"}}}\n' \
  | codex app-server | head -c 400 | xxd | head
```

If the first byte is `{` it is NDJSON, as the source says. The `Framing`
interface is now optional insurance rather than a required hedge.

The `id` type is pinned: `RequestId = string | int64`. Reply ids must be echoed
with the same JSON type they arrived as.

## 2. Results are NOT in these schemas either

The vendored set describes *inbound* shapes only. There is no `ServerResponse.json`
and no `ClientNotification.json` here — whether `generate-json-schema` emits such
files at all and they simply were not vendored, or whether it never emits them, is
**UNVERIFIED** and worth one `ls` of the output directory on the author's Mac.
Either way the adapter has to be written without them today.
Searching all three files for definitions named `*Response` or `*Result` finds
only `ResponseItem` (a Responses API history item), `FuzzyFileSearchResult` and
`McpToolCallResult` — none of which is a JSON-RPC result envelope.

Consequences the adapter author must plan around:

- **`thread/start` → threadId is unspecified here.** Do not read the thread id
  from the result. Read it from the `thread/started` notification, whose params
  are `{ thread: Thread }` and whose `Thread.id` is required. That is observable,
  which is the rule.
- **`thread/list` pagination shape is unspecified.** `ThreadListParams` has a
  `cursor` in and the description says "opaque pagination cursor returned by a
  previous call", so a cursor comes back, but its field name is not in the
  contract.
- **Approval decision payloads are unspecified**, with two exceptions. Two
  definitions in `ServerRequest.json` are declared but never `$ref`'d by any
  variant — the generator emitted them because they belong to the same Rust
  module, and they are almost certainly the reply types:

  | Orphan definition | Almost certainly the result of |
  |---|---|
  | `CommandExecutionApprovalDecision` | `item/commandExecution/requestApproval` |
  | `AdditionalPermissionProfile` | `item/permissions/requestApproval` |

  `CommandExecutionApprovalDecision` is a tagged union of six shapes and is worth
  reproducing, because it is the only place the contract states what "yes" and
  "no" mean:

  | Value | Meaning (schema's own words) |
  |---|---|
  | `"accept"` | "User approved the command." |
  | `"acceptForSession"` | "…and future prompts in the same session-scoped approval cache should run without prompting." |
  | `{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":[…]}}` | approve and persist a rule so similar commands stop prompting |
  | `{"applyNetworkPolicyAmendment":{"network_policy_amendment":…}}` | "User chose a persistent network policy rule (allow/deny) for this host." |
  | `"decline"` | "User denied the command. **The agent will continue the turn.**" |
  | `"cancel"` | "User denied the command. **The turn will also be immediately interrupted.**" |

  `decline` versus `cancel` is exactly the Claude Code `interrupt:false` /
  `interrupt:true` distinction from `ADAPTERS.md` §2, and it is what makes "no,
  stop" spoken at the glasses a hard stop rather than a note the model narrates
  around. **Relay must never send `acceptForSession` or either amendment
  variant** — both persist a standing grant, and `ORCHESTRATOR.md` §4b requires
  consequential actions to be confirmed *every time*.

  The reply shapes for `item/fileChange/requestApproval`,
  `item/tool/requestUserInput` and `mcpServer/elicitation/request` are **not in
  the contract at all**, not even as orphans. Probe them on the Mac.

## 3. Handshake, gating and subscription — the parts that change what arrives

Three facts from `InitializeParams` change which of the tables below are live at
all, and none of them appears in `ADAPTERS.md` §3's summary.

**`initialize` is mandatory and typed.** `InitializeParams` requires
`clientInfo: { name, version, title? }`; `capabilities` is optional
(`InitializeCapabilities`) with three fields:

| Capability | Default | Effect |
|---|---|---|
| `experimentalApi` | `false` | "Opt into receiving experimental API methods and fields." |
| `optOutNotificationMethods` | `null` | "Exact notification method names that should be suppressed for this connection (for example `thread/started`)." |
| `requestAttestation` | `false` | "Opt into `attestation/generate` requests for upstream `x-oai-attestation`." |

So everything the schemas label EXPERIMENTAL — `item/tool/requestUserInput`,
`item/plan/delta`, the whole `thread/realtime/*` family, `app/list` — is
**presumed off unless Relay sets `experimentalApi: true`**. The schema states the
flag's purpose but does not enumerate what it gates, so the exact set is
UNVERIFIED. Relay should set it `true` (we want `item/tool/requestUserInput`,
which is a needs-input source) and treat every experimental payload as optional.

`optOutNotificationMethods` is the supported way to shed notification volume — a
better answer than filtering in the adapter, since it saves the serialisation.

**There is no `thread/subscribe`.** `thread/unsubscribe` exists;
its inverse does not. Subscription is implicit in `thread/start` /
`thread/resume` / `thread/fork`. An adapter that resumes a thread to inspect it
starts receiving its whole notification stream, and must `thread/unsubscribe` to
stop.

**Approvals reach us only if two settings say so**, and this is Codex's exact
analogue of the Claude Code `permissions.defaultMode: auto` trap in
`ADAPTERS.md` §2:

| Setting | Values | Kills needs-input when |
|---|---|---|
| `approvalPolicy` (`AskForApproval`) | `untrusted`, `on-failure`, `on-request`, `never`, or `{granular:{…}}` | `"never"` |
| `approvalsReviewer` (`ApprovalsReviewer`) | `user`, `auto_review`, `guardian_subagent` (legacy alias) | anything but `"user"` |

`auto_review` "uses a carefully prompted subagent to … apply a risk-based decision
framework before approving or denying the request" — meaning approvals are
answered by a model and the five `requestApproval` requests never arrive. Relay
must pass both explicitly on `thread/start` / `thread/resume` / `thread/fork`
and must *check* what it got, because the failure presents as "the glasses never
ask me anything", which reads as a feature until something destructive runs
unattended. The `item/autoApprovalReview/*` notifications are the visible symptom.

---

## 4. Client → server requests (`ClientRequest.json`, 84)

| Method | Params type — required fields | Purpose | Relay |
|---|---|---|---|
| `initialize` | `InitializeParams` — `clientInfo` | Handshake. Declares client identity and the three capability flags in §3. | use |
| `thread/start` | `ThreadStartParams` — *(none required)* | "NEW APIs". Opens a new thread. All 15 fields optional: `cwd`, `model`, `modelProvider`, `sandbox`, `approvalPolicy`, `approvalsReviewer`, `personality`, `ephemeral`, `config`, `baseInstructions`, `developerInstructions`, `serviceName`, `serviceTier`, `sessionStartSource`, `threadSource`. | use |
| `thread/resume` | `ThreadResumeParams` — `threadId` | Reattach to an existing thread; "If thread_id identifies a running thread, app-server rejoins that thread". Same override set as `thread/start` minus `ephemeral`/`serviceName`. | use |
| `thread/fork` | `ThreadForkParams` — `threadId` | Copy a thread into a new one. Result thread carries `forkedFromId`. | use |
| `thread/archive` | `ThreadArchiveParams` — `threadId` | Archive a thread (hides it from the default `thread/list`). | ignore |
| `thread/delete` | `ThreadDeleteParams` — `threadId` | Delete a thread. | ignore |
| `thread/unsubscribe` | `ThreadUnsubscribeParams` — `threadId` | Stop receiving this thread's notifications without ending it. The only half of the subscription pair that exists. | use |
| `thread/name/set` | `ThreadSetNameParams` — `name`, `threadId` | Set the user-facing thread title. | later |
| `thread/goal/set` | `ThreadGoalSetParams` — `threadId` | Set an objective / token budget on a thread. | ignore |
| `thread/goal/get` | `ThreadGoalGetParams` — `threadId` | Read the thread goal. | ignore |
| `thread/goal/clear` | `ThreadGoalClearParams` — `threadId` | Clear the thread goal. | ignore |
| `thread/metadata/update` | `ThreadMetadataUpdateParams` — `threadId` | Update stored `gitInfo` for a thread. | ignore |
| `thread/unarchive` | `ThreadUnarchiveParams` — `threadId` | Un-archive a thread. | ignore |
| `thread/compact/start` | `ThreadCompactStartParams` — `threadId` | Compact the thread on demand. **Params are `{threadId}` and nothing else** — no threshold, no mode. | use |
| `thread/shellCommand` | `ThreadShellCommandParams` — `command`, `threadId` | Run a shell command in the thread's context. | ignore |
| `thread/approveGuardianDeniedAction` | `ThreadApproveGuardianDeniedActionParams` — `event`, `threadId` | Override an action the auto-review subagent denied. | later |
| `thread/rollback` | `ThreadRollbackParams` — `numTurns`, `threadId` | Drop N turns from the end of a thread. "This only modifies the thread's history and does not revert local file changes … Clients are responsible for reverting these changes." | later |
| `thread/list` | `ThreadListParams` — *(none required)* | Enumerate threads, filtered by `cwd`, `archived`, `modelProviders`, `searchTerm`, `sourceKinds`; paged by `cursor`/`limit`. The session-registry entry point. | use |
| `thread/loaded/list` | `ThreadLoadedListParams` — *(none required)* | Enumerate threads this server currently holds in memory, as opposed to on disk. | use |
| `thread/read` | `ThreadReadParams` — `threadId` | Read one thread; `includeTurns: true` "include turns and their items from rollout history". Backfill without touching the rollout files. | use |
| `thread/inject_items` | `ThreadInjectItemsParams` — `items`, `threadId` | "Append raw Responses API items to the thread history without starting a user turn." Items are untyped (`true`). | ignore |
| `skills/list` | `SkillsListParams` — *(none required)* | List local skills for the given cwds. | ignore |
| `skills/extraRoots/set` | `SkillsExtraRootsSetParams` — `extraRoots` | Add skill search roots. | ignore |
| `hooks/list` | `HooksListParams` — *(none required)* | List configured hooks. | ignore |
| `marketplace/add` | `MarketplaceAddParams` — `source` | Register a plugin marketplace. | ignore |
| `marketplace/remove` | `MarketplaceRemoveParams` — `marketplaceName` | Remove a marketplace. | ignore |
| `marketplace/upgrade` | `MarketplaceUpgradeParams` — *(none required)* | Upgrade a marketplace checkout. | ignore |
| `plugin/list` | `PluginListParams` — *(none required)* | List available plugins. | ignore |
| `plugin/installed` | `PluginInstalledParams` — *(none required)* | List installed plugins. | ignore |
| `plugin/read` | `PluginReadParams` — `pluginName` | Read one plugin's manifest. | ignore |
| `plugin/skill/read` | `PluginSkillReadParams` — `remoteMarketplaceName`, `remotePluginId`, `skillName` | Read a skill from a remote plugin. | ignore |
| `plugin/share/save` | `PluginShareSaveParams` — `pluginPath` | Publish a plugin. | ignore |
| `plugin/share/updateTargets` | `PluginShareUpdateTargetsParams` — `discoverability`, `remotePluginId`, `shareTargets` | Change who a shared plugin is visible to. | ignore |
| `plugin/share/list` | `PluginShareListParams` — *(none required)* | List shared plugins. | ignore |
| `plugin/share/checkout` | `PluginShareCheckoutParams` — `remotePluginId` | Check out a shared plugin. | ignore |
| `plugin/share/delete` | `PluginShareDeleteParams` — `remotePluginId` | Delete a shared plugin. | ignore |
| `app/list` | `AppsListParams` — *(none required)* | "EXPERIMENTAL - list available apps/connectors." | ignore |
| `fs/readFile` | `FsReadFileParams` — `path` | "Read a file from the host filesystem." | ignore |
| `fs/writeFile` | `FsWriteFileParams` — `dataBase64`, `path` | "Write a file on the host filesystem." | ignore |
| `fs/createDirectory` | `FsCreateDirectoryParams` — `path` | "Create a directory on the host filesystem." | ignore |
| `fs/getMetadata` | `FsGetMetadataParams` — `path` | "Request metadata for an absolute path." | ignore |
| `fs/readDirectory` | `FsReadDirectoryParams` — `path` | "List direct child names for a directory." | ignore |
| `fs/remove` | `FsRemoveParams` — `path` | "Remove a file or directory tree from the host filesystem." | ignore |
| `fs/copy` | `FsCopyParams` — `destinationPath`, `sourcePath` | "Copy a file or directory tree on the host filesystem." | ignore |
| `fs/watch` | `FsWatchParams` — `path`, `watchId` | "Start filesystem watch notifications for an absolute path." | ignore |
| `fs/unwatch` | `FsUnwatchParams` — `watchId` | "Stop filesystem watch notifications for a prior `fs/watch`." | ignore |
| `skills/config/write` | `SkillsConfigWriteParams` — `enabled` | Enable or disable a skill. | ignore |
| `plugin/install` | `PluginInstallParams` — `pluginName` | Install a plugin. | ignore |
| `plugin/uninstall` | `PluginUninstallParams` — `pluginId` | Uninstall a plugin. | ignore |
| `turn/start` | `TurnStartParams` — `input`, `threadId` | Begin a turn. `input` is `UserInput[]` (§6). Everything else overrides thread settings "for this turn and subsequent turns" — `model`, `effort`, `summary`, `cwd`, `sandboxPolicy`, `approvalPolicy`, `approvalsReviewer`, `serviceTier`, `personality`, `outputSchema`, `clientUserMessageId`. | use |
| `turn/steer` | `TurnSteerParams` — `expectedTurnId`, `input`, `threadId` | Inject input into the **running** turn. `expectedTurnId` is a "Required active turn id precondition. The request fails when it does not match the currently active turn." | use |
| `turn/interrupt` | `TurnInterruptParams` — `threadId`, `turnId` | Cancel a turn. Needs the turn id, not just the thread. | use |
| `review/start` | `ReviewStartParams` — `target`, `threadId` | Start Codex's code-review mode on a target. | ignore |
| `model/list` | `ModelListParams` — *(none required)* | Enumerate models available to this Codex install. | later |
| `modelProvider/capabilities/read` | `ModelProviderCapabilitiesReadParams` — *(none required)* | Read the active provider's capability set. | ignore |
| `experimentalFeature/list` | `ExperimentalFeatureListParams` — *(none required)* | Enumerate experimental feature flags. | ignore |
| `permissionProfile/list` | `PermissionProfileListParams` — *(none required)* | Enumerate named permission profiles. | later |
| `experimentalFeature/enablement/set` | `ExperimentalFeatureEnablementSetParams` — `enablement` | Toggle an experimental feature. | ignore |
| `mcpServer/oauth/login` | `McpServerOauthLoginParams` — `name` | Start an OAuth login for a configured MCP server. | later |
| `config/mcpServer/reload` | *(params: `null`)* | Reload MCP server config. One of five methods whose params are literally `null`. | later |
| `mcpServerStatus/list` | `ListMcpServerStatusParams` — *(none required)* | Enumerate configured MCP servers and their startup status. Relevant to the shared MCP registry (`MEMORY.md` §7). | later |
| `mcpServer/resource/read` | `McpResourceReadParams` — `server`, `uri` | Read an MCP resource through Codex. | ignore |
| `mcpServer/tool/call` | `McpServerToolCallParams` — `server`, `threadId`, `tool` | Call an MCP tool through Codex, outside a turn. | ignore |
| `windowsSandbox/setupStart` | `WindowsSandboxSetupStartParams` — `mode` | Begin Windows sandbox setup. | ignore |
| `windowsSandbox/readiness` | *(params: `null`)* | Query Windows sandbox readiness. | ignore |
| `account/login/start` | `LoginAccountParams` — *(union)* | Begin an account login flow. | ignore |
| `account/login/cancel` | `CancelLoginAccountParams` — `loginId` | Cancel a login in progress. | ignore |
| `account/logout` | *(params: `null`)* | Log out. | ignore |
| `account/rateLimits/read` | *(params: `null`)* | Snapshot of rate-limit state. Pairs with the `account/rateLimits/updated` rolling notification. | later |
| `account/usage/read` | *(params: `null`)* | Snapshot of account usage. | later |
| `account/sendAddCreditsNudgeEmail` | `SendAddCreditsNudgeEmailParams` — `creditType` | Ask the backend to email a credits nudge. | ignore |
| `feedback/upload` | `FeedbackUploadParams` — `classification` | Upload feedback and optionally logs. | ignore |
| `command/exec` | `CommandExecParams` — `command` | "Execute a standalone command (argv vector) under the server's sandbox." Not the agent's tool calls — a client-driven exec. | ignore |
| `command/exec/write` | `CommandExecWriteParams` — `processId` | "Write stdin bytes to a running `command/exec` session or close stdin." | ignore |
| `command/exec/terminate` | `CommandExecTerminateParams` — `processId` | "Terminate a running `command/exec` session by client-supplied `processId`." | ignore |
| `command/exec/resize` | `CommandExecResizeParams` — `processId`, `size` | "Resize a running PTY-backed `command/exec` session by client-supplied `processId`." | ignore |
| `config/read` | `ConfigReadParams` — *(none required)* | Read effective config, optionally per layer. **The trap detector**: this is how Relay checks that `approvalPolicy` is not `never` and `approvalsReviewer` is `user` before trusting needs-input. | use |
| `externalAgentConfig/detect` | `ExternalAgentConfigDetectParams` — *(none required)* | Detect other agents' config on this machine, for import. | ignore |
| `externalAgentConfig/import` | `ExternalAgentConfigImportParams` — `migrationItems` | Import detected config. | ignore |
| `config/value/write` | `ConfigValueWriteParams` — `keyPath`, `mergeStrategy`, `value` | Write one config value, with optimistic concurrency via `expectedVersion`. | ignore |
| `config/batchWrite` | `ConfigBatchWriteParams` — `edits` | Write several config values atomically. | ignore |
| `configRequirements/read` | *(params: `null`)* | Read config requirements. | ignore |
| `account/read` | `GetAccountParams` — *(none required)* | Read the active account, optionally refreshing the token. | later |
| `fuzzyFileSearch` | `FuzzyFileSearchParams` — `query`, `roots` | Fuzzy file search, streamed back via `fuzzyFileSearch/session*`. | ignore |

## 5. Server → client notifications (`ServerNotification.json`, 66)

Every notification below carries `threadId` unless noted; the ones inside a turn
also carry `turnId`, and the ones about an item also carry `itemId`. That triple
is the correlation key the adapter keys its state machine on.

| Method | Params type — required fields | Purpose | Relay |
|---|---|---|---|
| `error` | `ErrorNotification` — `error`, `threadId`, `turnId`, `willRetry` | "NEW NOTIFICATIONS". A turn-level error. `error` is `TurnError { message, additionalDetails?, codexErrorInfo? }`. **`willRetry` decides whether to ping** — a retryable error is not a user-facing failure. | use |
| `thread/started` | `ThreadStartedNotification` — `thread` | Carries a whole `Thread`. **This, not the `thread/start` result, is where the adapter learns the thread id.** | use |
| `thread/status/changed` | `ThreadStatusChangedNotification` — `status`, `threadId` | `ThreadStatus` is `notLoaded` \| `idle` \| `systemError` \| `active{activeFlags}`, and `ThreadActiveFlag` is `waitingOnApproval` \| `waitingOnUserInput`. **A second, independent needs-input signal**, and the one that survives a reconnect when the original request is gone. | use |
| `thread/archived` | `ThreadArchivedNotification` — `threadId` | A thread was archived. | ignore |
| `thread/deleted` | `ThreadDeletedNotification` — `threadId` | A thread was deleted — drop it from the registry. | use |
| `thread/unarchived` | `ThreadUnarchivedNotification` — `threadId` | A thread was un-archived. | ignore |
| `thread/closed` | `ThreadClosedNotification` — `threadId` | The thread is no longer loaded — mark the registry entry detached. | use |
| `skills/changed` | `SkillsChangedNotification` — *(no fields at all)* | "Treat this as an invalidation signal and re-run `skills/list`". | ignore |
| `thread/name/updated` | `ThreadNameUpdatedNotification` — `threadId` | Thread title changed; `threadName` is nullable. | use |
| `thread/goal/updated` | `ThreadGoalUpdatedNotification` — `goal`, `threadId` | Thread goal changed. | ignore |
| `thread/goal/cleared` | `ThreadGoalClearedNotification` — `threadId` | Thread goal cleared. | ignore |
| `thread/settings/updated` | `ThreadSettingsUpdatedNotification` — `threadId`, `threadSettings` | Full `ThreadSettings`: `approvalPolicy`, `approvalsReviewer`, `collaborationMode`, `cwd`, `model`, `modelProvider`, `sandboxPolicy`, plus optional `effort`, `summary`, `personality`, `serviceTier`, `activePermissionProfile`. **The live check that the approvals trap in §3 has not been switched on under us.** | use |
| `thread/tokenUsage/updated` | `ThreadTokenUsageUpdatedNotification` — `threadId`, `tokenUsage`, `turnId` | `ThreadTokenUsage { last, total: TokenUsageBreakdown, modelContextWindow?: int64\|null }`. `TokenUsageBreakdown` is `{inputTokens, cachedInputTokens, outputTokens, reasoningOutputTokens, totalTokens}`. **Tokens only — no dollar figure anywhere in the contract.** `modelContextWindow` is nullable, so the ~70% idle-compaction trigger of `MEMORY.md` §9 needs a fallback denominator. | use |
| `turn/started` | `TurnStartedNotification` — `threadId`, `turn` | Turn boundary open. `turn` is a whole `Turn` (§6). | use |
| `hook/started` | `HookStartedNotification` — `run`, `threadId` | A configured hook began. | ignore |
| `turn/completed` | `TurnCompletedNotification` — `threadId`, `turn` | Turn boundary close. **Carries no cost.** `turn.status` ∈ `completed` \| `interrupted` \| `failed` \| `inProgress`; `turn.durationMs`, `turn.completedAt`, and `turn.error` when failed. | use |
| `hook/completed` | `HookCompletedNotification` — `run`, `threadId` | A configured hook finished. | ignore |
| `turn/diff/updated` | `TurnDiffUpdatedNotification` — `diff`, `threadId`, `turnId` | "the latest aggregated diff across all file changes in the turn" as one unified-diff string. Excellent turn-summary input. | later |
| `turn/plan/updated` | `TurnPlanUpdatedNotification` — `plan`, `threadId`, `turnId` | `plan` is `TurnPlanStep[]` = `{step: string, status: pending\|inProgress\|completed}`, plus an optional `explanation`. The structured plan `ADAPTERS.md` §5 calls the best narration material. | use |
| `item/started` | `ItemStartedNotification` — `item`, `startedAtMs`, `threadId`, `turnId` | An item began. `item` is the 17-variant `ThreadItem` union (§6). | use |
| `item/autoApprovalReview/started` | `ItemGuardianApprovalReviewStartedNotification` — `action`, `review`, `reviewId`, `startedAtMs`, `threadId`, `turnId` | "[UNSTABLE] … This shape is expected to change soon." **The visible symptom that approvals are being answered by a subagent instead of by the user** (§3). | use |
| `item/autoApprovalReview/completed` | `ItemGuardianApprovalReviewCompletedNotification` — `action`, `completedAtMs`, `decisionSource`, `review`, `reviewId`, `startedAtMs`, `threadId`, `turnId` | "[UNSTABLE]". Carries `decisionSource` — who actually decided. | use |
| `item/completed` | `ItemCompletedNotification` — `completedAtMs`, `item`, `threadId`, `turnId` | An item finished. The completed `ThreadItem` is authoritative over any deltas. | use |
| `item/agentMessage/delta` | `AgentMessageDeltaNotification` — `delta`, `itemId`, `threadId`, `turnId` | Assistant text, token by token. → `TextDelta`, the streaming-TTS input. | use |
| `item/plan/delta` | `PlanDeltaNotification` — `delta`, `itemId`, `threadId`, `turnId` | "EXPERIMENTAL - proposed plan streaming deltas … Clients should not assume concatenated deltas match the completed plan item content." Distinct from `turn/plan/updated`; this streams a `plan` **item**. | later |
| `command/exec/outputDelta` | `CommandExecOutputDeltaNotification` — `capReached`, `deltaBase64`, `processId`, `stream` | Output of a *client-driven* `command/exec`. **`deltaBase64`, not `delta`** — do not confuse with `item/commandExecution/outputDelta`. No `threadId`. | ignore |
| `process/outputDelta` | `ProcessOutputDeltaNotification` — `capReached`, `deltaBase64`, `processHandle`, `stream` | "Stream base64-encoded stdout/stderr chunks for a running `process/spawn` session." **`process/spawn` is not in `ClientRequest.json`** (§7). | ignore |
| `process/exited` | `ProcessExitedNotification` — `exitCode`, `processHandle`, `stderr`, `stderrCapReached`, `stdout`, `stdoutCapReached` | "Final exit notification for a `process/spawn` session." | ignore |
| `item/commandExecution/outputDelta` | `CommandExecutionOutputDeltaNotification` — `delta`, `itemId`, `threadId`, `turnId` | Live stdout/stderr from a command **the agent** is running. `delta` is a plain string. → `ToolOutput`. | use |
| `item/commandExecution/terminalInteraction` | `TerminalInteractionNotification` — `itemId`, `processId`, `stdin`, `threadId`, `turnId` | The agent wrote to a PTY's stdin. | ignore |
| `item/fileChange/outputDelta` | `FileChangeOutputDeltaNotification` — `delta`, `itemId`, `threadId`, `turnId` | "Deprecated legacy apply_patch output stream notification. **The server no longer emits this notification.**" | ignore |
| `item/fileChange/patchUpdated` | `FileChangePatchUpdatedNotification` — `changes`, `itemId`, `threadId`, `turnId` | `changes` is `FileUpdateChange[]` = `{path, kind, diff}`. The live replacement for the deprecated row above. → `ToolOutput`. | use |
| `serverRequest/resolved` | `ServerRequestResolvedNotification` — `requestId`, `threadId` | **A pending server → client request was answered somewhere else.** Without handling this, a Relay ping outlives the question it asked — the user is woken to approve something already approved in a terminal. | use |
| `item/mcpToolCall/progress` | `McpToolCallProgressNotification` — `itemId`, `message`, `threadId`, `turnId` | MCP tool progress. `message` is prose from the server, not a percentage. → `ToolOutput`. | use |
| `mcpServer/oauthLogin/completed` | `McpServerOauthLoginCompletedNotification` — `name`, `success` | An MCP OAuth login finished. | later |
| `mcpServer/startupStatus/updated` | `McpServerStatusUpdatedNotification` — `name`, `status` | An MCP server's startup status changed. | later |
| `account/updated` | `AccountUpdatedNotification` — *(none required)* | `authMode` / `planType` changed. | ignore |
| `account/rateLimits/updated` | `AccountRateLimitsUpdatedNotification` — `rateLimits` | "Sparse rolling rate-limit update. Clients should merge available values into the most recent `account/rateLimits/read` response". Quota state worth surfacing before it bites. | later |
| `app/list/updated` | `AppListUpdatedNotification` — `data` | "EXPERIMENTAL - notification emitted when the app list changes." | ignore |
| `remoteControl/status/changed` | `RemoteControlStatusChangedNotification` — `installationId`, `serverName`, `status` | "Current remote-control connection status and remote identity exposed to clients." **No `remoteControl/*` request exists in `ClientRequest.json`** (§7). | ignore |
| `externalAgentConfig/import/completed` | `ExternalAgentConfigImportCompletedNotification` — *(no fields)* | Config import finished. | ignore |
| `fs/changed` | `FsChangedNotification` — `changedPaths`, `watchId` | "Filesystem watch notification emitted for `fs/watch` subscribers." | ignore |
| `item/reasoning/summaryTextDelta` | `ReasoningSummaryTextDeltaNotification` — `delta`, `itemId`, `summaryIndex`, `threadId`, `turnId` | Thinking **summary**, streamed. Speakable. `summaryIndex` separates parts. | use |
| `item/reasoning/summaryPartAdded` | `ReasoningSummaryPartAddedNotification` — `itemId`, `summaryIndex`, `threadId`, `turnId` | A new summary part opened — the boundary between two summary blocks. Needed to avoid concatenating two thoughts into one sentence. | use |
| `item/reasoning/textDelta` | `ReasoningTextDeltaNotification` — `contentIndex`, `delta`, `itemId`, `threadId`, `turnId` | Raw thinking, streamed. → `Reasoning`, **never spoken**. | use |
| `thread/compacted` | `ContextCompactedNotification` — `threadId`, `turnId` | "Deprecated: Use `ContextCompaction` item type instead" — i.e. an `item/completed` whose item `type` is `contextCompaction`. | ignore |
| `model/rerouted` | `ModelReroutedNotification` — `fromModel`, `reason`, `threadId`, `toModel`, `turnId` | Codex silently switched models mid-turn. Worth narrating, because the user asked for one model and is getting another. | later |
| `model/verification` | `ModelVerificationNotification` — `threadId`, `turnId`, `verifications` | Model verification results. | ignore |
| `turn/moderationMetadata` | `TurnModerationMetadataNotification` — `metadata`, `threadId`, `turnId` | Untyped (`metadata: true`) moderation metadata. | ignore |
| `warning` | `WarningNotification` — `message` | Generic warning; `threadId` optional. | use |
| `guardianWarning` | `GuardianWarningNotification` — `message`, `threadId` | Warning from the auto-review guardian. | use |
| `deprecationNotice` | `DeprecationNoticeNotification` — `summary` | Upstream telling us we are using something on its way out. **Log it loudly — this is the early warning for the version-bump problem these fixtures exist to solve.** | use |
| `configWarning` | `ConfigWarningNotification` — `summary` | A problem in the user's `config.toml`, with `path` and `range`. | use |
| `fuzzyFileSearch/sessionUpdated` | `FuzzyFileSearchSessionUpdatedNotification` — `files`, `query`, `sessionId` | Streamed fuzzy-search results. | ignore |
| `fuzzyFileSearch/sessionCompleted` | `FuzzyFileSearchSessionCompletedNotification` — `sessionId` | Fuzzy search finished. | ignore |
| `thread/realtime/started` | `ThreadRealtimeStartedNotification` — `threadId`, `version` | "EXPERIMENTAL - emitted when thread realtime startup is accepted." | later |
| `thread/realtime/itemAdded` | `ThreadRealtimeItemAddedNotification` — `item`, `threadId` | "EXPERIMENTAL - raw non-audio thread realtime item emitted by the backend." | later |
| `thread/realtime/transcript/delta` | `ThreadRealtimeTranscriptDeltaNotification` — `delta`, `role`, `threadId` | "EXPERIMENTAL - flat transcript delta emitted whenever realtime transcript text changes." | later |
| `thread/realtime/transcript/done` | `ThreadRealtimeTranscriptDoneNotification` — `role`, `text`, `threadId` | "EXPERIMENTAL - final transcript text emitted when realtime completes a transcript part." | later |
| `thread/realtime/outputAudio/delta` | `ThreadRealtimeOutputAudioDeltaNotification` — `audio`, `threadId` | "EXPERIMENTAL - streamed output audio emitted by thread realtime." | later |
| `thread/realtime/sdp` | `ThreadRealtimeSdpNotification` — `sdp`, `threadId` | "EXPERIMENTAL - emitted with the remote SDP for a WebRTC realtime session." | later |
| `thread/realtime/error` | `ThreadRealtimeErrorNotification` — `message`, `threadId` | "EXPERIMENTAL - emitted when thread realtime encounters an error." | later |
| `thread/realtime/closed` | `ThreadRealtimeClosedNotification` — `threadId` | "EXPERIMENTAL - emitted when thread realtime transport closes." | later |
| `windows/worldWritableWarning` | `WindowsWorldWritableWarningNotification` — `extraCount`, `failedScan`, `samplePaths` | "Notifies the user of world-writable directories on Windows, which cannot be protected by the sandbox." | ignore |
| `windowsSandbox/setupCompleted` | `WindowsSandboxSetupCompletedNotification` — `mode`, `success` | Windows sandbox setup finished. | ignore |
| `account/login/completed` | `AccountLoginCompletedNotification` — `success` | A login flow finished. | ignore |

## 6. Server → client requests (`ServerRequest.json`, 10)

**Every row here blocks the server until the client replies.** There are ten, not
five. An adapter that handles only the approval subset will hang Codex the first
time one of the other five arrives.

| Method | Params type — required fields | Purpose | Relay |
|---|---|---|---|
| `item/commandExecution/requestApproval` | `CommandExecutionRequestApprovalParams` — `itemId`, `startedAtMs`, `threadId`, `turnId` | "Sent when approval is requested for a specific command execution. This request is used for Turns started via turn/start." Optional `command`, `cwd`, `commandActions` (parsed for friendly display), `reason`, `networkApprovalContext`, `proposedExecpolicyAmendment`, `proposedNetworkPolicyAmendments`, and `approvalId` — null for ordinary shell approvals, a UUID for zsh-exec-bridge subcommands where several callbacks share one `itemId`. **Route on `(itemId, approvalId)`, not `itemId` alone.** → `NeedsInput` ⚑PING | use |
| `item/fileChange/requestApproval` | `FileChangeRequestApprovalParams` — `itemId`, `startedAtMs`, `threadId`, `turnId` | "Sent when approval is requested for a specific file change." Optional `reason` and `grantRoot` ("[UNSTABLE] … allow writes under this root for the remainder of the session"). **The params do not include the diff** — get it from the `fileChange` item or `item/fileChange/patchUpdated`. → `NeedsInput` ⚑PING | use |
| `item/tool/requestUserInput` | `ToolRequestUserInputParams` — `itemId`, `questions`, `threadId`, `turnId` | "EXPERIMENTAL - Request input from the user for a tool call." `questions` is `ToolRequestUserInputQuestion[]` = `{id, header, question, options?: {label, description}[], isOther, isSecret}`, plus an optional `autoResolutionMs` deadline. **`isSecret` marks answers that must go through the credential vault and never into the index** (`MEMORY.md` §6). Gated behind `experimentalApi`. → `NeedsInput` ⚑PING | use |
| `mcpServer/elicitation/request` | `McpServerElicitationRequestParams` — `serverName`, `threadId` | "Request input for an MCP server elicitation." A union of two modes: `form` (`message` + `requestedSchema`) and `url` (`message` + `url` + `elicitationId`). **`turnId` is nullable** — "MCP models elicitation as a standalone server-to-client request", so this one may arrive with no turn to attribute it to. → `NeedsInput` ⚑PING | use |
| `item/permissions/requestApproval` | `PermissionsRequestApprovalParams` — `cwd`, `itemId`, `permissions`, `startedAtMs`, `threadId`, `turnId` | "Request approval for additional permissions from the user." `permissions` is `RequestPermissionProfile { fileSystem?, network? }`. → `NeedsInput` ⚑PING | use |
| `item/tool/call` | `DynamicToolCallParams` — `arguments`, `callId`, `threadId`, `tool`, `turnId` | "Execute a dynamic tool call on the client." Codex asking **Relay** to run a tool. Relay registers no dynamic tools, so this should never arrive — but it must be answered with a JSON-RPC error rather than dropped. | must-answer |
| `account/chatgptAuthTokens/refresh` | `ChatgptAuthTokensRefreshParams` — `reason` | Codex got a 401 and is asking the client to refresh ChatGPT auth. Relay does not own those tokens; reply with an error so Codex surfaces the auth failure instead of hanging. Relevant to `ORCHESTRATOR.md` §2b, where ChatGPT-via-Codex is the sanctioned subscription path. | must-answer |
| `attestation/generate` | `AttestationGenerateParams` — *(no fields)* | "Generate a fresh upstream attestation result on demand." Only sent when the client set `capabilities.requestAttestation: true`, which Relay leaves `false`. | must-answer |
| `applyPatchApproval` | `ApplyPatchApprovalParams` — `callId`, `conversationId`, `fileChanges` | "DEPRECATED APIs below. Request to approve a patch. This request is used for Turns started via the legacy APIs (i.e. SendUserTurn, SendUserMessage)." Relay uses `turn/start`, so this is dead — answer defensively. | must-answer |
| `execCommandApproval` | `ExecCommandApprovalParams` — `callId`, `command`, `conversationId`, `cwd`, `parsedCmd` | "Request to exec a command. This request is used for Turns started via the legacy APIs." Same as above. | must-answer |

## 7. Shapes the adapter has to model

### `ThreadItem` — 17 variants, discriminated on `type`

Everything that happens inside a turn is one of these, delivered by `item/started`
and `item/completed` and embedded in `Turn.items`.

| `type` | Payload beyond `id` | Relay mapping |
|---|---|---|
| `userMessage` | `content: UserInput[]`, `clientId?` | echo — our own turn accepted |
| `hookPrompt` | `fragments: HookPromptFragment[]` | — |
| `agentMessage` | `text`, `phase?`, `memoryCitation?` | final assistant text (authoritative over deltas) |
| `plan` | `text` | EXPERIMENTAL; prose plan, distinct from `turn/plan/updated`'s structured steps |
| `reasoning` | `content: string[]`, `summary: string[]` | `Reasoning` — never spoken |
| `commandExecution` | `command`, `cwd`, `commandActions`, `status`, `exitCode?`, `durationMs?`, `aggregatedOutput?`, `processId?`, `source` | `ToolStarted` / `ToolOutput` |
| `fileChange` | `changes: FileUpdateChange[]`, `status` | `ToolStarted` / `ToolOutput` |
| `mcpToolCall` | `server`, `tool`, `arguments`, `status`, `result?`, `error?`, `durationMs?`, `pluginId?`, `mcpAppResourceUri?` | `ToolStarted` / `ToolOutput` |
| `dynamicToolCall` | `tool`, `arguments`, `status`, `success?`, `contentItems?`, `namespace?` | pairs with the `item/tool/call` server request |
| `collabAgentToolCall` | `senderThreadId`, `receiverThreadIds`, `agentsStates`, `tool`, `status`, `prompt?`, `model?` | sub-agent fan-out — a thread can spawn threads |
| `subAgentActivity` | `agentPath`, `agentThreadId`, `kind` | as above |
| `webSearch` | `query`, `action?` | `ToolStarted` |
| `imageView` | `path` | — |
| `imageGeneration` | `result`, `status`, `revisedPrompt?`, `savedPath?` | — |
| `enteredReviewMode` / `exitedReviewMode` | `review` | — |
| `contextCompaction` | *(id only)* | **the live compaction signal** — the `thread/compacted` notification is deprecated |

Statuses are three separate enums, all with the same first three values:
`CommandExecutionStatus` and `PatchApplyStatus` are
`inProgress｜completed｜failed｜declined`; `McpToolCallStatus` has no `declined`.

### `UserInput` — 5 variants, the payload of `turn/start` and `turn/steer`

`text` (`{text, text_elements?}`), `image` (`{url, detail?}`), `localImage`
(`{path, detail?}`), `skill` (`{name, path}`), `mention` (`{name, path}`).
A spoken turn is a single `{"type":"text","text":"…"}`.

### `Thread` and `Turn`

`Thread` requires `id`, `sessionId`, `cwd`, `createdAt`, `updatedAt`, `preview`,
`status`, `turns`, `source`, `modelProvider`, `cliVersion`, `ephemeral`; and
optionally `name`, `path` ("[UNSTABLE]"), `gitInfo`, `forkedFromId`,
`parentThreadId`, `agentRole`, `agentNickname`, `threadSource`. Two of these
matter to the registry beyond the obvious: `sessionId` is "shared by threads that
belong to the same session tree", and `parentThreadId` "will only be set if this
thread is a subagent" — so Codex threads form a forest, not a list.

`Thread.turns` is documented as "Only populated on `thread/resume`,
`thread/rollback`, `thread/fork`, and `thread/read` (when `includeTurns` is true)
responses. For all other responses and notifications returning a Thread, the turns
field will be an empty list." **So `thread/started` gives an empty `turns` — do
not treat that as "a thread with no history".**

`Turn` requires `id`, `status`, `items`, and optionally `startedAt`, `completedAt`,
`durationMs`, `error`, `itemsView`. `itemsView` is `notLoaded｜summary｜full` and
tells you whether `items` is the whole story — a backfill reader must check it.

### `TurnError` / `CodexErrorInfo`

`TurnError { message, additionalDetails?, codexErrorInfo? }`. `CodexErrorInfo` is
a union of the string codes `contextWindowExceeded`, `usageLimitExceeded`,
`serverOverloaded`, `cyberPolicy`, `internalServerError`, `unauthorized`,
`badRequest`, `threadRollbackFailed`, `sandboxError`, `other`, plus two object
variants carrying an upstream HTTP status: `httpConnectionFailed` and
`responseStreamConnectionFailed`.

`contextWindowExceeded` is the terminal case `MEMORY.md` §9 singles out — it is a
distinct code here, so the adapter can recognise it without matching prose.

## 8. Methods referenced by the schemas but absent from them

Eighteen definitions in `ClientRequest.json` are declared and never `$ref`'d by
any request variant, and two notification families describe requests that do not
exist in the contract. Most plausibly these are gated behind
`capabilities.experimentalApi` and the generator emits their types regardless —
but **the schemas do not say that, so it is UNVERIFIED**.

| Missing request | Evidence it exists |
|---|---|
| `thread/realtime/*` (start, audio, sdp…) | 8 `thread/realtime/*` notifications, plus orphan `ThreadRealtimeStartTransport`, `ThreadRealtimeAudioChunk`, `RealtimeVoice`, `RealtimeOutputModality`, `RealtimeConversationVersion`, `RealtimeConversationArchitecture`, `ConversationTextRole` |
| `process/spawn` | `process/outputDelta` and `process/exited` both say "for a running `process/spawn` session" |
| `remoteControl/enable`, `remoteControl/disable` | `remoteControl/status/changed` notification; orphan `RemoteControlEnableParams`, `RemoteControlDisableParams` |
| a paged `thread/resume` variant | orphan `ThreadResumeInitialTurnsPageParams` |
| a per-turn environment override | orphan `TurnEnvironmentParams` |
| dynamic tool registration | orphan `DynamicToolSpec` (the `item/tool/call` server request has to be enabled somehow) |

Remaining orphans, for completeness: `AdditionalContextEntry`,
`CollaborationMode`, `ProcessTerminalSize`, `ResponseItem`, `SelectedCapabilityRoot`,
`ThreadMemoryMode`.

**Practical consequence for Relay:** the realtime voice path is *observable but
not drivable* from this contract. `ADAPTERS.md` §3 previously implied we could
reach for it later; we cannot, not without a schema we do not have.

## 9. The subset the Relay adapter actually implements

Tallied from the disposition columns above — and re-tallied by
`check_codex_methods.py`, so these numbers cannot rot:

| Direction | Total | use | must-answer | later | ignore |
|---|---|---|---|---|---|
| Client → server requests | 84 | 13 | 0 | 11 | 60 |
| Server → client notifications | 66 | 27 | 0 | 14 | 25 |
| Server → client requests | 10 | 5 | 5 | 0 | 0 |

All ten server → client requests are handled: five as real needs-input, five as
defensive replies that keep Codex from hanging on a question we do not want.

```
handshake     initialize {clientInfo, capabilities:{experimentalApi:true}}
open          thread/start | thread/resume | thread/fork   → thread/started gives the id
guard         config/read + thread/settings/updated        → approvalPolicy != never
                                                             approvalsReviewer == user
turn          turn/start {threadId, input:[{type:"text"}]}  → turn/started
steer         turn/steer {threadId, expectedTurnId, input}
cancel        turn/interrupt {threadId, turnId}
stream        item/agentMessage/delta                       → TextDelta
              item/reasoning/textDelta                      → Reasoning (never spoken)
              item/reasoning/summaryTextDelta + summaryPartAdded
              item/started / item/completed                 → ToolStarted / ToolOutput
              item/commandExecution/outputDelta             → ToolOutput
              item/fileChange/patchUpdated                  → ToolOutput
              item/mcpToolCall/progress                     → ToolOutput
              turn/plan/updated                             → PlanUpdated
block         item/{commandExecution,fileChange,permissions}/requestApproval
              item/tool/requestUserInput
              mcpServer/elicitation/request                 → NeedsInput ⚑PING
              serverRequest/resolved                        → withdraw the ping
              thread/status/changed activeFlags             → blocked, after reconnect
close         turn/completed                                → TurnCompleted ⚑PING
              error (only when willRetry == false)          → Error ⚑PING
meter         thread/tokenUsage/updated                     → tokens (no USD)
registry      thread/list, thread/loaded/list, thread/read
              thread/started, thread/closed, thread/deleted, thread/name/updated
context       thread/compact/start {threadId}
              item/completed type=contextCompaction
detach        thread/unsubscribe {threadId}
```

## 10. Version history of this file

| Date | Codex version | Change |
|---|---|---|
| 2026-08-10 | codex-cli 0.140.0 | First extraction. Corrects `ADAPTERS.md` §3 in eleven places; see that section's notes. |
