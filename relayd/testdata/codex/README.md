# `testdata/codex`

Fixtures for `internal/adapter/codex`. **Both of the data files here are
synthetic**, and that is the first thing to know about them.

| | |
|---|---|
| `gen_trace.py` | builds `docs/fixtures/adapters/codex.trace.json` from the vendored codex-cli 0.140.0 schemas and validates every message against the definition it claims to conform to |
| `exec-stream.ndjson` | stands in for a `codex exec --json` capture, for the one-shot fallback adapter |

## Why they are synthetic

No `codex` binary exists in the build container — no `~/.codex` either — so
nothing here could be recorded. `BUILD-PROMPT.md` asks for a Codex trace "same
shape, same directory" as `claude-code.trace.json`, which *is* a real recording;
this one is derived from the contract instead. A synthetic fixture that is
honestly labelled is useful. One that pretends to be a recording is worse than
none, so:

- the trace's first record is a `meta` record saying
  `"provenance": "SCHEMA-DERIVED, NOT RECORDED"`, and
  `TestTraceSaysItIsNotARecording` fails if anyone removes it;
- `exec-stream.ndjson` opens with a `_comment` line saying the same thing;
- `ADAPTERS.md` §8 items 11 and 12 track replacing both with real captures on
  a machine that has Codex installed and authenticated.

## Regenerating the trace

```bash
python3 relayd/testdata/codex/gen_trace.py            # rewrite it
python3 relayd/testdata/codex/gen_trace.py --check    # verify the file on disk
```

`--check` fails if the file has drifted from what the generator produces, so a
hand-edit to the fixture is a red test rather than a silent fork.

The Python path uses `jsonschema` when it is installed. The Go path does not
need it: `schema_test.go` carries a validator covering exactly the keywords
these three schemas use, and
`TestTraceValidatesAgainstTheVendoredSchemas` re-checks every message on every
`go test`. That is what turns a re-vendored schema — a new required field, a
renamed method — into a failing build instead of an adapter that is quietly
wrong.

## What the trace does *not* establish

Listed in the file's own `unverified` block, and worth repeating:

- **every JSON-RPC `result` payload.** `generate-json-schema` emits request and
  notification params only; there is no `ServerResponse.json`. Those records
  carry `"schema": null` and are the adapter's best reading of the contract.
- **the real reply to `item/commandExecution/requestApproval`.** The bare
  `"accept"` is inferred from an orphaned definition (`ADAPTERS.md` §8 item 7).
- **the true interleaving** of a response with the notifications around it. The
  ordering in the file is one the adapter tolerates, not one anybody observed.
