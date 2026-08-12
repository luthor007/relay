# Backfill fixtures

**Synthetic. Schema-verified, not corpus-verified.** Every file here was written
against the store formats documented in `docs/MEMORY.md` §4 — not copied from,
derived from, or checked against a real store.

That distinction is the whole point of this README. The real corpus is 3.6 GB of
`~/.claude/projects`, `~/.hermes` and `~/.codex` on the author's Mac: personal
transcripts that must never enter this repo, and which no container has ever
seen. So the readers in `internal/backfill` are tested against these fixtures,
which proves they parse the documented schema correctly and proves nothing at
all about whether the documented schema matches what those five runtimes
actually write today. `MEMORY.md` §12.7 says the same thing in the place a
future session will look for it.

| Directory | Stands in for | Documented in |
|---|---|---|
| `claudecode/projects/<slug>/<uuid>.jsonl` | `~/.claude/projects` — one JSONL per session | §4: `cwd`, `gitBranch`, `timestamp`, `version`, `aiTitle` |
| `hermes/*.sql` | `~/.hermes/state.db` — SQLite + FTS5 | §4: `title`, `cwd`, `model`, `started_at`, `message_count`, `tool_call_count`, `estimated_cost_usd`, `actual_cost_usd`; §9: `input_tokens`, `cache_read_tokens`, `compression_locks` |
| `codex/sessions/YYYY/MM/DD/rollout-*.jsonl` + `session_index.jsonl` | `~/.codex` | §4's probe of 133 real rollouts: four line types, each `{type, timestamp, payload}` |
| `opencode/` | `opencode export <id> --sanitize` output, and the on-disk storage fallback | §4 documents the export command only |
| `openclaw/agents/<agent>/sessions/sessions.json` | a **relocatable** state dir | §4: never hardcode `~/.openclaw` |

## What each fixture is shaped to exercise

Not one happy path per runtime. Each fixture carries the specific failure the
corresponding reader has to survive:

- **Claude Code** — one session with `aiTitle` (the runtime titled it), one with
  only a `summary` record, and one with no title, no `cwd` and a line that is
  not JSON. The third exists so the lossy slug decode and the skipped line are
  both *disclosed* rather than silent.
- **Hermes** — `schema.sql` + `seed.sql` is the documented schema;
  `schema-variant.sql` is the same store under different column and table names
  with ISO-text timestamps, because the full schema has never been probed and a
  reader with a rigid `SELECT` breaks on the first version drift.
  `compression_locks` is populated with a held lease so a reader that reached
  for it would have something to fight over.
- **Codex** — one rollout with all four line types including `token_count` and
  `ghost_snapshot`, and one with an unrecognised line type and no token count, so
  format drift is reported instead of swallowed. `session_index.jsonl` has a
  deliberately malformed line: it is a hint file, so a bad line must change
  nothing.
- **OpenCode** — an export payload with messages in both `content` and `parts`
  shapes, plus a `storage/session/` file, because the two enumeration paths are
  independent and only the export command is documented.
- **OpenClaw** — two agents, one store keyed by session id and one a plain
  array, and only one session has a transcript beside it. The session without
  one keeps a real pointer: the byte range of its entry inside `sessions.json`.

## No credentials live here

Fixtures in this directory carry **no credential-shaped strings**. The one test
that needs a real one reads it out of `testdata/secrets/corpus.jsonl` at run
time and writes it into a temporary directory.

That is deliberate: `testdata/secrets/` is the single sanctioned home for
synthetic credentials and is on `scripts/build-public-repo.sh`'s exclude list,
because that script greps the assembled tree for credential shapes and cannot
tell a synthetic key from a real one. Adding a `sk_live_…`-shaped string to
*this* directory would make the public repo refuse to publish.

Paths in these fixtures use `/home/user/…` for the same reason: the publish
script also refuses a tree containing somebody's real home directory.
