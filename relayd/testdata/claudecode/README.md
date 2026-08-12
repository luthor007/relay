# Claude Code adapter test data

`settings/` is a corpus for `ScanSettings` — the detector for ADAPTERS.md §2's
silent trap, where `permissions.defaultMode: "auto"` stops
`--permission-prompt-tool` from ever being called. Each file is a
`~/.claude/settings.json` in one of the states the detector has to tell apart:

| File | State |
|---|---|
| `auto.json` | the trap, exactly as it was found on the probe machine |
| `bypass.json` | a bypass mode, same effect |
| `accept-edits.json` | partial — file edits are auto-granted, commands still ask |
| `default.json` | explicitly asking |
| `unknown-mode.json` | a mode nobody here recognises, treated as unsafe rather than benign |
| `no-permissions.json` | a settings file with no permissions block at all |
| `broken.json` | invalid JSON — we cannot say whether the trap is set, and must say so |

None of these are credentials and none came from a real machine's home
directory.

The normalized-event golden for the vendored 49-event trace lives at
`../trace.golden.jsonl`. Regenerate it with:

    go test ./internal/adapter/claudecode/ -run TestTraceFixture -update

The trace itself is *not* copied here on purpose. It is read in place from
`docs/fixtures/adapters/claude-code.trace.json`, so there is exactly one copy
and a format change breaks CI instead of drifting quietly out of sync.
