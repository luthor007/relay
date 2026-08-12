-- 0001_init — the credential vault, MEMORY.md §6.
--
-- This is a SEPARATE database file with a separate migration set, and it is
-- never indexed. There is no FTS5 table here and no vec0 table here, and a test
-- asserts that, because §2's tiers are separate precisely so that a credential
-- can never appear in a search result. An embedded secret cannot be unembedded.
--
-- The secret material itself is either in the OS keychain (backend='keychain',
-- ciphertext NULL) or AES-256-GCM ciphertext in this file whose key is in the
-- keychain (backend='file'). Nothing here ever holds plaintext.

CREATE TABLE credential (
    id      TEXT PRIMARY KEY,
    service TEXT NOT NULL,             -- "stripe", "twilio", "openrouter"
    label   TEXT NOT NULL DEFAULT '',

    -- DASHBOARD.md §3.2: the console never displays a secret after it is
    -- stored. Last four characters, and a re-validate button. Empty when the
    -- secret is short enough that four characters would be most of it.
    last_four TEXT NOT NULL DEFAULT '',

    backend    TEXT NOT NULL CHECK (backend IN ('keychain', 'file')),
    ciphertext BLOB,                   -- backend='file' only
    nonce      BLOB,                   -- backend='file' only

    -- ORCHESTRATOR.md §2: credentials are stored as references — env var, file
    -- path, or exec — rather than pasted inline, which is OpenClaw's shape.
    -- A vault-held secret is ref_kind='managed'.
    ref_kind  TEXT NOT NULL DEFAULT 'managed',
    ref_value TEXT NOT NULL DEFAULT '',

    -- Provenance, because newest validated wins and two Stripe keys means one
    -- is probably rotated.
    source_kind    TEXT NOT NULL CHECK (source_kind IN ('typed', 'config', 'transcript')),
    source_runtime TEXT NOT NULL DEFAULT '',
    source_session TEXT NOT NULL DEFAULT '',
    source_path    TEXT NOT NULL DEFAULT '',
    source_at      INTEGER,

    -- A key in your transcript may not be yours. Colleagues paste keys into
    -- pairing sessions, and the proposal has to say so.
    shared_session INTEGER NOT NULL DEFAULT 0,

    created_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    last_used_by TEXT NOT NULL DEFAULT '',   -- which runtime used it

    -- Validate before trusting: one real call, same probe as the installer's
    -- model keys. Reason codes are ORCHESTRATOR.md §2's.
    last_validated_at     INTEGER,
    last_validation_reason TEXT NOT NULL DEFAULT '',

    revoked_at INTEGER
) STRICT;

CREATE INDEX credential_by_service ON credential (service, created_at DESC);
CREATE INDEX credential_live ON credential (revoked_at, last_used_at DESC);

-- Nothing is captured silently. Detection produces a proposal, and the console
-- is where it gets accepted or dismissed — not a voice prompt at 2 a.m.
CREATE TABLE credential_proposal (
    id       TEXT PRIMARY KEY,
    service  TEXT NOT NULL,
    detector TEXT NOT NULL,
    last_four TEXT NOT NULL DEFAULT '',

    ciphertext BLOB,
    nonce      BLOB,

    source_kind    TEXT NOT NULL CHECK (source_kind IN ('typed', 'config', 'transcript')),
    source_runtime TEXT NOT NULL DEFAULT '',
    source_session TEXT NOT NULL DEFAULT '',
    source_path    TEXT NOT NULL DEFAULT '',
    source_at      INTEGER,
    shared_session INTEGER NOT NULL DEFAULT 0,

    created_at   INTEGER NOT NULL,
    decided_at   INTEGER,
    decision     TEXT NOT NULL DEFAULT ''   -- accepted | dismissed
) STRICT;

CREATE INDEX credential_proposal_open ON credential_proposal (decided_at, created_at DESC);

CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;
