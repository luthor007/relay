# `relayd`

The orchestrator. Go, one static binary, no runtime to install — that is the
pricing story, not a preference (`SYSTEM.md` §8).

The design lives in `docs/`. Where this file and the docs disagree, the docs
win, and the fix belongs in both.

```bash
go build ./...
go test ./...
```

Both must be green before anything is committed.

---

## What is here

| Package | Owns |
|---|---|
| `internal/event` | the normalized event model — `ADAPTERS.md` §5's nine events, and the reply path back into a runtime |
| `internal/adapter` | the one interface all three adapters implement, plus the capability descriptor |
| `internal/adapter/fake` | an in-memory adapter, so everything above this layer is testable without a runtime installed |
| `internal/store` | SQLite: registry, index and facts tiers in one file, the vault in another |
| `internal/llm` | the two orchestrator models, provider-abstracted, credentials as references |
| `internal/vault` | credentials: OS keychain, encrypted-file fallback, last-four only |
| `internal/config` | config file location, defaults, validation |
| `internal/logx` | structured logging, with a redaction helper |
| `internal/deps` | blank imports pinning modules nothing imports *yet* — see "go.mod" below |

### The seams, and why they are where they are

**`event.Event` is a sealed interface of nine types, not a struct with a kind
field.** The nine come from `ADAPTERS.md` §5 and adding a tenth is a change to
that document first. Two rules are enforced by the types rather than by
convention:

- Anything a runtime might not be able to report is a pointer.
  `TurnCompleted.Usage` is nil on ACP because ACP 0.4.5 has no token, cost or
  usage field anywhere in its schema — nil, never zero, so the console shows a
  gap instead of a lie. Same for `Usage.CostUSD` on Codex and
  `Usage.ContextWindow` when `modelContextWindow` comes back null.
- `Meta.Replay` marks an event we are re-reading rather than watching happen.
  ACP's `session/load` replays a whole conversation as `session/update`
  notifications before it resolves. Every `Ping()` returns `PingNone` for a
  replayed event, so nobody gets woken at 3am about a turn from two weeks ago.

**`NeedsInput` is a pointer type carrying its own reply function.** Three
runtimes resolve a blocked question three different ways — Codex resolves a
pending JSON-RPC request, Claude Code returns from the permission-prompt MCP
tool, ACP answers `session/request_permission` — and the orchestrator must not
know which. It answers in terms of an option id plus an `Interrupt` flag, and
the adapter maps that onto its own vocabulary. `Withdraw` exists because Codex's
`serverRequest/resolved` says an approval was answered somewhere else, and a
ping that outlives its question wakes someone to approve what is already
approved.

**`adapter.Capabilities` is data, not behaviour.** `ADAPTERS.md` §5's coverage
table is uneven and the orchestrator has to read the unevenness before it asks
for something. `Baseline(runtime)` is that table as code; adapters narrow it at
handshake time with `With`. Four support levels, and the difference between the
last two is the whole point:

| | Meaning |
|---|---|
| `SupportYes` | observed, in the protocol, safe to depend on |
| `SupportNo` | the runtime cannot; degrade visibly |
| `SupportSynthesized` | Relay can infer it — a Claude Code plan built from tool activity. `Observed()` is false, and any event produced this way carries `Synthesized: true` |
| `SupportUnknown` | nobody has looked. ACP's `loadSession` is here, and `ADAPTERS.md` §4 must not claim reattach works until it is probed on a real runtime |

A capability that is absent produces an `*UnsupportedError` matching
`adapter.ErrUnsupported`. Never a panic. `Steer` on an ACP session returns one,
and the caller cancels and re-prompts.

**Two databases.** `store.Open` is the main file — registry, index, facts.
`store.OpenVault` is a second file with a separate migration set, no FTS5 table
and no vec0 table. `TestVaultIsNeverIndexed` asserts that, because `MEMORY.md`
§2 keeps the tiers apart precisely so a credential cannot appear in a search
result, and §6 says the ordering is not negotiable.

**The index stores a pointer, never a copy.** `summary` and `session_index` both
carry `(runtime, session_id, path, byte_offset)` into the original transcript.
The 3.6 GB stays on disk, in place, unmoved.

---

## The stack, and the trap that costs a day

Pure Go via wazero, no cgo, which is what lets one machine cross-compile
darwin/linux × arm64/amd64 — verified, all four targets, `CGO_ENABLED=0`.

```go
import (
    sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/ncruces" // v0.1.6
    _ "github.com/ncruces/go-sqlite3/driver"                      // v0.21.0, PINNED
)
```

Three pins are load-bearing, with their failure modes in `MEMORY.md` §3:

| Module | Pin | If you get it wrong |
|---|---|---|
| `asg017/sqlite-vec-go-bindings` | v0.1.6 | this is the wasm build that has sqlite-vec compiled in |
| `ncruces/go-sqlite3` | **v0.21.0** | v0.35 removed `sqlite3.Binary` (compile error); v0.22–v0.30 fail at module instantiation with a wasm-threads error |
| `tetratelabs/wazero` | **≥ v1.9.0** | on v1.8.2 *every* `vec0` table panics with an out-of-bounds memory access, on ten 4-dimensional vectors. Nothing imports it directly, and `go get` resolves v1.8.2 for you |

**Never import `github.com/ncruces/go-sqlite3/embed`.** Its `init()` overwrites
`sqlite3.Binary` with a vanilla SQLite and sqlite-vec silently disappears
(`no such function: vec_version`).

`TestStackWorks` in `internal/store` is what goes red if any of this drifts. It
inserts into `vec0` and reads the row back, because `select vec_version()`
succeeds on the broken wazero versions and proves nothing. It logs the versions
it actually got: SQLite 3.47.0, sqlite-vec v0.1.6.

---

## go.mod — one owner

**Only the Foundation agent edits `go.mod` or `go.sum`.** Concurrent writes
corrupt the module graph, and several agents work on this tree in parallel. Do
not run `go get`, `go mod tidy`, or `go mod edit`.

Every dependency the milestone needs is already there. `internal/deps` holds
blank imports for the three that no code imports yet — `coder/websocket`,
`yaml.v3`, `x/term` — so `go.sum` stays complete and a stray `tidy` cannot drop
them. Delete a line from that file when a real package starts importing it.

If something is genuinely missing, say so in your return value instead of adding
it.

Two version constraints worth knowing before you propose an upgrade: the module
targets **Go 1.24** and the toolchain here is 1.24.7, so `golang.org/x/term` is
pinned at v0.36.0 and `golang.org/x/sys` at v0.38.0 — the next releases of both
require Go ≥ 1.25 and silently rewrite the `go` directive.

---

## File ownership — five agents, no collisions

Foundation owns everything listed under "What is here" above. Nobody else edits
those files; extend them by adding new files in your own tree, or say what you
need changed.

| Agent | Owns, exclusively |
|---|---|
| Claude Code adapter | `internal/adapter/claudecode/**` |
| Codex adapter | `internal/adapter/codex/**` |
| ACP adapter | `internal/adapter/acp/**` |
| Core | `internal/registry/**`, `internal/bus/**`, `internal/api/**`, `cmd/relayd/**` |
| Installer | `internal/detect/**`, `internal/install/**`, `internal/voice/**`, `cmd/relay/**` |
| Backfill | `internal/backfill/**`, `internal/index/**` |
| Memory | `internal/summarize/**`, `internal/search/**` |
| Routing | `internal/routing/**` |

Shared, read-only to everyone but Foundation:

- `internal/adapter/coverage.go` — `ADAPTERS.md` §5's table as data. When a
  probe on the author's machine closes one of the `SupportUnknown` rows, that is
  one line here and one line in `ADAPTERS.md`, in the same commit.
- `internal/store/migrations/**` — the schema. A new table is a **new numbered
  migration file**, never an edit to an existing one; migrations are
  forward-only and `ErrSchemaAhead` refuses a database written by a newer build.
  Tell Foundation rather than adding one mid-phase if two agents need the same
  number.
- `testdata/secrets/**` — the secret-detection corpus and ruleset, from the
  probe phase. `scripts/build-public-repo.sh` excludes it.

Where two agents need the same thing, it goes in Foundation's tree and both
import it. Nothing gets copy-pasted between adapter packages.

### Notes for each of the three adapters

All three implement `adapter.Adapter` and produce `adapter.Session`. Start from
`adapter.Baseline(runtime)` and narrow it with what the handshake actually says.

- **Claude Code** — `--session-id` takes a UUID we generate, so use
  `SessionOptions.ID` directly. Watch `system/init`'s `permissionMode` on every
  turn: when it is `auto` or a bypass mode the permission-prompt tool is never
  called, so set `CapNeedsInput` to `SupportNo` with a note rather than
  reporting a capability you cannot observe. Discriminate the two `user` event
  shapes on key *presence* (`isReplay` vs `tool_use_result`), not on
  `isReplay == false`. The vendored 49-event fixture was recorded in the broken
  state, so it doubles as the regression test for the detector.
- **Codex** — NDJSON, one object per line, no `Content-Length` headers and **no
  `jsonrpc` envelope field**. `JSONRPCMessage` is untagged, so demultiplex on
  field presence. Take the thread id from the `thread/started` *notification*,
  not the `thread/start` result. Ten server→client requests block until
  answered, not five: answer the ones we do not want with a JSON-RPC error
  rather than dropping them, or Codex hangs.
- **ACP** — one adapter, three runtimes. Declare all three client capabilities
  **false**; an `fs/*` or `terminal/*` call that arrives anyway gets `-32601`,
  which is honest degradation. `session/load` replays the conversation, so set
  `Meta.Replay` while it does. On cancel, keep reading events and resolve every
  outstanding `NeedsInput` with `DecisionCancelled` or the turn cannot unwind.
  Log unknown `_`-prefixed methods rather than dropping them — that log line is
  how we would find out a runtime shipped its own steering extension.

---

## Conventions

- Times on the wire and in SQLite are unix milliseconds. In Go they are
  `time.Time`, and the zero time means "never", not 1970.
- Nothing in `internal/store` writes a secret. Detection happens before
  indexing; what lands in the index is a `secret_marker`.
- Anything that could be a credential is logged through `logx.Secret`.
- `internal/llm` makes no network call in a test. Inject an `*http.Client` with
  a `RoundTripper`.
