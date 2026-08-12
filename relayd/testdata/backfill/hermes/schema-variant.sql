-- The same store under different names, because Hermes's full schema has never
-- been probed and a reader with a rigid SELECT breaks on the first version
-- drift. Everything the reader needs is here under a synonym: conversations
-- rather than sessions, name rather than title, working_directory rather than
-- cwd, created_at rather than started_at, and timestamps as ISO text rather
-- than unix milliseconds.

CREATE TABLE conversations (
    session_id      TEXT PRIMARY KEY,
    name            TEXT,
    working_dir     TEXT,
    model_name      TEXT,
    created_at      TEXT,
    last_message_at TEXT,
    num_messages    INTEGER,
    estimated_cost  REAL
);

CREATE TABLE messages (
    id              INTEGER PRIMARY KEY,
    conversation_id TEXT NOT NULL,
    sender          TEXT NOT NULL,
    body            TEXT NOT NULL,
    ts              TEXT NOT NULL
);

CREATE TABLE compression_locks (
    session_id TEXT PRIMARY KEY,
    holder     TEXT NOT NULL,
    expires_at INTEGER NOT NULL
);

INSERT INTO conversations VALUES
  ('cv-1', 'ship the installer', '/home/user/src/relay', 'claude-sonnet-4',
   '2026-08-01T10:00:00Z', '2026-08-01T11:30:00Z', 2, 0.42);

INSERT INTO messages VALUES
  (1, 'cv-1', 'user', 'does the installer detect a runtime that is installed but never run', '2026-08-01T10:00:00Z'),
  (2, 'cv-1', 'assistant', 'Yes — never_run, which is not an error.', '2026-08-01T10:01:00Z');
