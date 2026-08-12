-- A synthetic Hermes state.db, to the schema MEMORY.md §4 documents.
--
-- Column names come from §4's "free metadata" row (title, cwd, model,
-- started_at, message_count, tool_call_count, estimated_cost_usd,
-- actual_cost_usd) plus §9's per-session input_tokens / cache_read_tokens. The
-- rest of the real 2.5 GB schema has never been probed, so this fixture is
-- schema-verified and not corpus-verified — see MEMORY.md §12.7.
--
-- compression_locks is here on purpose: it is the lease Hermes coordinates its
-- own compaction through, and the reader must never touch it. A fixture without
-- it could not prove that.

CREATE TABLE sessions (
    id                 TEXT PRIMARY KEY,
    title              TEXT,
    cwd                TEXT,
    model              TEXT,
    started_at         INTEGER,
    updated_at         INTEGER,
    message_count      INTEGER,
    tool_call_count    INTEGER,
    estimated_cost_usd REAL,
    actual_cost_usd    REAL,
    input_tokens       INTEGER,
    output_tokens      INTEGER,
    cache_read_tokens  INTEGER
);

CREATE TABLE messages (
    id         INTEGER PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions (id),
    role       TEXT NOT NULL,
    content    TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE INDEX messages_by_session ON messages (session_id, created_at);

-- Hermes runs FTS5 over its own messages at 4,379-message scale, which is the
-- existence proof MEMORY.md §3 cites for FTS5 on this hardware.
CREATE VIRTUAL TABLE messages_fts USING fts5 (
    content,
    content = 'messages',
    content_rowid = 'id'
);

-- The lease. session_id, holder, expires_at — MEMORY.md §9 and §12.5.
CREATE TABLE compression_locks (
    session_id TEXT PRIMARY KEY,
    holder     TEXT NOT NULL,
    expires_at INTEGER NOT NULL
);
