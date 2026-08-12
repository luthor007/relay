# `testdata/acp`

Fixtures for `internal/adapter/acp` — the one adapter that serves OpenClaw,
Hermes and OpenCode.

| | |
|---|---|
| `gen_trace.py` | builds `docs/fixtures/adapters/acp.trace.json` from the vendored `@zed-industries/agent-client-protocol@0.4.5` schema and validates every message against the `$defs` entry it claims to conform to |

There is no second data file here on purpose: the trace lives with the other
vendored contracts in `docs/fixtures/adapters/`, next to the schema it was
derived from, and the Go tests read it from there.

## Why the trace is synthetic

No `openclaw`, `hermes` or `opencode` binary exists in the build container — no
`~/.openclaw` or `~/.hermes` either — so nothing here could be recorded.
`BUILD-PROMPT.md` asks for an ACP trace "same shape, same directory" as
`claude-code.trace.json`, which *is* a real recording; this one is derived from
the contract instead.

A synthetic fixture that is honestly labelled is useful. One that pretends to be
a recording is worse than none, so:

- the trace's first record is a `meta` record saying
  `"provenance": "SCHEMA-DERIVED, NOT RECORDED"`, with a six-item `unverified`
  block, and `TestTraceSaysItIsNotARecording` fails if either is removed;
- `ADAPTERS.md` §8 item 14 tracks replacing it with **three** real recordings,
  one per runtime, on a machine that has them installed and authenticated.

## Regenerating

```bash
python3 relayd/testdata/acp/gen_trace.py            # rewrite it
python3 relayd/testdata/acp/gen_trace.py --check    # verify the file on disk
```

`--check` fails if the file has drifted from what the generator produces, so a
hand-edit to the fixture is a red test rather than a silent fork.

The Python path uses `jsonschema` when it is installed. The Go path does not need
it: `schema_test.go` carries a validator covering exactly the keywords this
schema uses — `$ref`, `type`, `required`, `properties`, `items`, `enum`, `const`,
`oneOf`, `anyOf`, `minimum`, `maximum`, and nothing else — and
`TestTraceValidatesAgainstTheVendoredSchema` re-checks every message on every
`go test`. That is what turns a re-vendored schema into a failing build instead
of an adapter that is quietly wrong.

## What the trace covers

One complete session, 37 records:

- `initialize` and one-round-trip version negotiation, with all three client
  capabilities declared false;
- `session/new` with an absolute `cwd` and the shared MCP registry over stdio;
- **all eight** `session/update` variants, including `plan` and
  `available_commands_update`;
- a `session/request_permission` answered with `selected`, and a second one
  carrying only a `toolCallId` — no title to read aloud;
- an out-of-contract `fs/read_text_file` answered `-32601`;
- a `session/cancel`: the notification, the mandatory `cancelled` outcome for
  the outstanding permission request, the flushed `session/update` that follows
  it, and `stopReason: "cancelled"`;
- the redirect re-prompt after the cancel, and the queued addition delivered
  after *that* turn ends — both halves of `ADAPTERS.md` §4's table.

`TestReplayTraceThroughTheAdapter` drives the whole file through the real
adapter, once per runtime, and asserts the normalized events that come out —
which is why the fixture is a test of the adapter rather than of the JSON.

## What the trace does *not* establish

Listed in the file's own `unverified` block, and worth repeating:

- **the `agentCapabilities` each runtime advertises.** `loadSession` and
  `promptCapabilities` are per-runtime and per-version; the values here are the
  schema's defaults with `loadSession` flipped on so the reattach path is
  exercised at all (`ADAPTERS.md` §8 item 4).
- **the real shape of a session id.** OpenClaw's bridge uses Gateway keys like
  `agent:main:main`; Hermes and OpenCode mint their own. The opaque id in the
  trace is deliberately neither.
- **the true interleaving** of notifications with the responses around them. The
  ordering is one the adapter tolerates, not one anybody observed — which is why
  the replay matches consecutive client→agent messages as a set rather than in
  order.
- **whether any of the three emits `agent_thought_chunk`.** It is protocol-native
  and it is in this trace, which is exactly why the adapter starts
  `CapReasoning` at `SupportUnknown` and narrows it only when it sees one.
- **per-turn cost.** There is none to record: the word `token` occurs twice in
  the whole schema, both times in the `max_tokens` stop reason.
