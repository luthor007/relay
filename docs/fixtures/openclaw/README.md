# OpenClaw Gateway wire fixtures

Captured live on 2026-08-13 against **openclaw 2026.7.1-2** (the installed
version, not the 2026.8.1 source clone) running as
`openclaw --profile relay-probe gateway run --port 19311`, with
**Claude Code 2.1.231** as the `claude-cli` agent runtime.

The capture client is a plain WebSocket client — no openclaw process in the
loop — so these frames are what relayd will actually see on the wire.

Every file is JSONL, one record per line:

```json
{ "t": 1786650451690, "who": "A-observer", "dir": "in|out|meta", "frame": { ... } }
```

`dir` is from the client's point of view: `in` = gateway → client, `out` =
client → gateway, `meta` = probe annotation (not a wire frame). `who` is only
present in the two-socket captures.

Gateway auth tokens are replaced with `<REDACTED_GATEWAY_TOKEN>`; the exec
approval socket token with `<REDACTED_EXEC_APPROVAL_SOCKET_TOKEN>`.

| File | What it pins down |
| --- | --- |
| `01-handshake.jsonl` | `connect.challenge` → `connect` → `hello-ok`, including the full `features.methods` and `features.events` lists and the negotiated scope set. |
| `02-models-list.jsonl` | `models.list` with `view: "all"` — note the five `claude-cli` provider models and that `claude-opus-5` is not among them. |
| `03-session-create-and-turn-claude-cli.jsonl` | The happy path: `sessions.subscribe`, `sessions.create`, `sessions.messages.subscribe`, `sessions.send`, then the whole `agent` / `chat` / `session.message` / `sessions.changed` event sequence for one Claude Code turn. |
| `04-exec-approval-external-client.jsonl` | Two sockets. B raises `exec.approval.request`; A (a different connection) receives `exec.approval.requested`, calls `exec.approval.resolve` with `allow-once`, and both see `exec.approval.resolved`. B's blocked RPC returns the decision. |
| `05-exec-approval-suppressed-and-scoped-out.jsonl` | The two negative cases for the same flow: `suppressDelivery: true` skips the broadcast entirely, and a client without `operator.admin` and without a paired device id never sees another connection's approval. |
| `06-exec-approval-claude-cli-hard-denied.jsonl` | Agent exec policy set to `security=allowlist, ask=always`, then a Claude Code session tries `touch`. No approval frame is ever emitted; the tool result is `OpenClaw exec policy denied Claude native tool use`. |
| `07-sessions-create-model-rejected.jsonl` | `sessions.create` with no params (succeeds) and with `model: "claude-cli/claude-opus-5"` (`INVALID_REQUEST: model not allowed`). |
| `08-session-patch-execask-then-turn.jsonl` | `sessions.patch` with `execAsk: "always"` and the turn that follows, including `agent` `stream: "tool"` start/result frames. |
| `09-spawnedcwd-ignored-by-claude-cli.jsonl` | `sessions.patch` with `spawnedCwd`; the run's `pwd` still reports the agent workspace. |
| `10-claude-code-native-transcript.jsonl` | One Gateway-spawned Claude Code session's own transcript, exactly as Claude Code wrote it under `~/.claude/projects/`. `attachment` bodies are elided; every other record is verbatim. |
| `11-slash-command-acp-doctor.jsonl` | `chat.send` carrying a slash command (`/acp doctor`) plus the `chat.history` read-back. The gateway answers these itself — `provider: "openclaw"`, `model: "gateway-injected"` — so no model credential is consumed. Also records that the `acpx` runtime backend is not installed. |
| `transcript-locations.json` | Both on-disk transcript paths per session and the field that links them (`claudeCliSessionId`). |

## Reproducing

```sh
export NVM_DIR="$HOME/.nvm"; . "$NVM_DIR/nvm.sh"; nvm use 24
openclaw --profile relay-probe onboard --non-interactive --accept-risk \
  --flow quickstart --skip-channels --skip-skills --skip-ui --skip-search \
  --skip-daemon --gateway-port 19311 --auth-choice anthropic-cli
openclaw --profile relay-probe gateway run --port 19311
curl -s http://127.0.0.1:19311/health   # {"ok":true,"status":"live"}
```

The token to put in `connect.params.auth.token` is
`gateway.auth.token` in `~/.openclaw-relay-probe/openclaw.json`.
