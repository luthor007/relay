-- 0001_init — the registry and index tiers.
--
-- SYSTEM.md §5's seven entities, MEMORY.md §2's registry and index tiers, and
-- the two virtual tables §3 requires. The facts tier is 0002; the vault is a
-- separate database with a separate migration set and is never indexed.
--
-- Times are unix milliseconds. There is no user table: one box, one person
-- (SYSTEM.md §5), and pretending otherwise now costs a migration later.

-- ---------------------------------------------------------------- registry --

CREATE TABLE device (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL CHECK (kind IN ('glasses', 'phone')),
    name       TEXT NOT NULL DEFAULT '',
    paired_at  INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE TABLE session (
    id            TEXT PRIMARY KEY,      -- Relay's id
    runtime       TEXT NOT NULL,         -- claude-code | codex | openclaw | hermes | opencode
    native_id     TEXT NOT NULL DEFAULT '', -- the runtime's own id, when it differs
    agent         TEXT NOT NULL DEFAULT '', -- the model or agent name inside the runtime
    subject       TEXT NOT NULL DEFAULT '',
    workspace     TEXT NOT NULL DEFAULT '', -- absolute cwd
    git_branch    TEXT NOT NULL DEFAULT '',
    entities      TEXT NOT NULL DEFAULT '[]', -- JSON array
    created_at    INTEGER NOT NULL,
    last_active   INTEGER NOT NULL,
    state         TEXT NOT NULL CHECK (state IN ('running', 'awaiting', 'idle', 'closed')),

    -- Nullable on purpose: ADAPTERS.md §5's cost coverage is uneven and a zero
    -- would claim an observation the runtime never made. ACP leaves all four
    -- null, Codex leaves cost_usd null, Claude Code fills them in.
    cost_usd       REAL,
    tokens_total   INTEGER,
    tokens_input   INTEGER,
    context_window INTEGER
) STRICT;

CREATE INDEX session_by_activity ON session (last_active DESC);
CREATE INDEX session_by_state ON session (state, last_active DESC);
CREATE INDEX session_by_workspace ON session (workspace, last_active DESC);
CREATE UNIQUE INDEX session_by_native ON session (runtime, native_id) WHERE native_id <> '';

CREATE TABLE turn (
    id         TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES session (id) ON DELETE CASCADE,
    role       TEXT NOT NULL CHECK (role IN ('user', 'agent')),
    text       TEXT NOT NULL DEFAULT '',
    at         INTEGER NOT NULL,
    audio_ref  TEXT,

    stop_reason TEXT NOT NULL DEFAULT '',
    ok          INTEGER NOT NULL DEFAULT 1,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    cost_usd    REAL,
    tokens      INTEGER
) STRICT;

CREATE INDEX turn_by_session ON turn (session_id, at);

CREATE TABLE tool_call (
    id            TEXT PRIMARY KEY,
    session_id    TEXT NOT NULL REFERENCES session (id) ON DELETE CASCADE,
    turn_id       TEXT NOT NULL DEFAULT '',
    tool          TEXT NOT NULL,
    target        TEXT NOT NULL DEFAULT '',
    args_digest   TEXT NOT NULL DEFAULT '', -- a digest, never the arguments
    at            INTEGER NOT NULL,
    result_status TEXT NOT NULL DEFAULT ''  -- pending|in_progress|completed|failed
) STRICT;

CREATE INDEX tool_call_by_session ON tool_call (session_id, at);

-- ------------------------------------------------------------ capture tier --

CREATE TABLE episode (
    id           TEXT PRIMARY KEY,
    started_at   INTEGER NOT NULL,
    ended_at     INTEGER,
    kind         TEXT NOT NULL CHECK (kind IN ('meeting', 'focus', 'conversation', 'ambient')),
    transcript   TEXT NOT NULL DEFAULT '',
    participants TEXT NOT NULL DEFAULT '[]', -- JSON array
    location     TEXT
) STRICT;

CREATE INDEX episode_by_time ON episode (started_at DESC);

CREATE TABLE commitment (
    id         TEXT PRIMARY KEY,
    episode_id TEXT REFERENCES episode (id) ON DELETE SET NULL,
    text       TEXT NOT NULL,
    owed_to    TEXT,
    due_at     INTEGER,
    done_at    INTEGER,
    created_at INTEGER NOT NULL
) STRICT;

CREATE INDEX commitment_open ON commitment (done_at, due_at);

-- --------------------------------------------------------- grants and MCP --

CREATE TABLE grant (
    id           TEXT PRIMARY KEY,
    connector    TEXT NOT NULL,
    scopes       TEXT NOT NULL DEFAULT '[]', -- JSON array
    granted_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    revoked_at   INTEGER
) STRICT;

CREATE INDEX grant_by_connector ON grant (connector);

-- MEMORY.md §7: one registry reconciled from five. The originals are kept so
-- pointing every runtime at Relay is a reversible move.
CREATE TABLE mcp_server (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    command       TEXT NOT NULL DEFAULT '',
    args          TEXT NOT NULL DEFAULT '[]', -- JSON array
    url           TEXT NOT NULL DEFAULT '',
    dedupe_key    TEXT NOT NULL,             -- command + args, so three configs are one server
    seen_in       TEXT NOT NULL DEFAULT '[]', -- JSON array of runtimes
    adopted       INTEGER NOT NULL DEFAULT 0,
    original_json TEXT NOT NULL DEFAULT '',  -- rollback
    created_at    INTEGER NOT NULL,
    last_used_at  INTEGER
) STRICT;

CREATE UNIQUE INDEX mcp_server_by_key ON mcp_server (dedupe_key);

-- --------------------------------------------------------------- index tier --

-- One row per session ever seen, across all five runtimes. This holds a
-- POINTER into the original transcript and never a copy: MEMORY.md §3 keeps the
-- 3.6 GB on disk, in place, unmoved. Anything can be re-read in full on demand.
CREATE TABLE session_index (
    id           TEXT PRIMARY KEY,
    runtime      TEXT NOT NULL,
    session_id   TEXT NOT NULL,          -- the runtime's own id
    path         TEXT NOT NULL,          -- absolute path to the transcript on disk
    byte_offset  INTEGER NOT NULL DEFAULT 0,

    title        TEXT NOT NULL DEFAULT '',
    workspace    TEXT NOT NULL DEFAULT '',
    git_branch   TEXT NOT NULL DEFAULT '',
    model        TEXT NOT NULL DEFAULT '',
    started_at   INTEGER,
    ended_at     INTEGER,
    message_count   INTEGER NOT NULL DEFAULT 0,
    tool_call_count INTEGER NOT NULL DEFAULT 0,
    cost_usd     REAL,
    tokens_total INTEGER,

    -- Backfill is incremental and resumable, keyed on (runtime, session_id,
    -- mtime): 3.6 GB through a small model is an hour or two and must survive
    -- being interrupted.
    source_mtime INTEGER NOT NULL DEFAULT 0,
    source_size  INTEGER NOT NULL DEFAULT 0,
    indexed_at   INTEGER
) STRICT;

CREATE UNIQUE INDEX session_index_by_native ON session_index (runtime, session_id);
CREATE INDEX session_index_by_time ON session_index (started_at DESC);

-- Summaries are what gets embedded. Raw transcripts are not: 875k chunks of
-- diffs and stack traces buries the two sentences that said what the session
-- was for (MEMORY.md §3).
CREATE TABLE summary (
    id          INTEGER PRIMARY KEY,
    kind        TEXT NOT NULL CHECK (kind IN ('session', 'cluster')),

    -- The pointer, repeated here so a search hit can be opened without a join.
    runtime     TEXT NOT NULL,
    session_id  TEXT NOT NULL,
    path        TEXT NOT NULL,
    byte_offset INTEGER NOT NULL DEFAULT 0,
    byte_length INTEGER NOT NULL DEFAULT 0,

    text        TEXT NOT NULL,
    model       TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL
) STRICT;

CREATE INDEX summary_by_session ON summary (runtime, session_id);

-- FTS5 for the lexical half of MEMORY.md §3's hybrid retrieval. Exact
-- identifiers — a repo name, an error string, STRIPE_SECRET_KEY — are where
-- vector search is weakest and BM25 is strongest, and those are most of what
-- routing looks up.
CREATE VIRTUAL TABLE summary_fts USING fts5 (
    text,
    content = 'summary',
    content_rowid = 'id',
    tokenize = 'porter unicode61'
);

CREATE TRIGGER summary_ai AFTER INSERT ON summary BEGIN
    INSERT INTO summary_fts (rowid, text) VALUES (new.id, new.text);
END;

CREATE TRIGGER summary_ad AFTER DELETE ON summary BEGIN
    INSERT INTO summary_fts (summary_fts, rowid, text) VALUES ('delete', old.id, old.text);
END;

CREATE TRIGGER summary_au AFTER UPDATE ON summary BEGIN
    INSERT INTO summary_fts (summary_fts, rowid, text) VALUES ('delete', old.id, old.text);
    INSERT INTO summary_fts (rowid, text) VALUES (new.id, new.text);
END;

-- sqlite-vec for the dense half. 768 dimensions, brute force, no ANN index:
-- 44 ms at 22k on the pure-Go wasm build we ship, measured. Revisit at ~50k and
-- treat 100k as the ceiling. rowid ties a vector to its summary row.
CREATE VIRTUAL TABLE summary_vec USING vec0 (
    embedding float[768]
);

-- Secrets are detected BEFORE indexing, never after — an embedded key cannot be
-- unembedded. What lands in the index is this marker; the key itself goes to
-- the vault, in the other database.
CREATE TABLE secret_marker (
    id          TEXT PRIMARY KEY,
    runtime     TEXT NOT NULL,
    session_id  TEXT NOT NULL,
    path        TEXT NOT NULL,
    byte_offset INTEGER NOT NULL DEFAULT 0,
    detector    TEXT NOT NULL,          -- which rule matched
    service     TEXT NOT NULL DEFAULT '', -- "stripe", "twilio"
    vault_id    TEXT NOT NULL DEFAULT '', -- set once the proposal is accepted
    at          INTEGER NOT NULL
) STRICT;

CREATE INDEX secret_marker_by_session ON secret_marker (runtime, session_id);

-- ---------------------------------------------------------------- meta --

CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;

-- The embedding contract. Changing either of these is a migration, because
-- summary_vec's column width is fixed at create time.
INSERT INTO meta (key, value) VALUES ('embedding_dims', '768');
INSERT INTO meta (key, value) VALUES ('embedding_model', '');
