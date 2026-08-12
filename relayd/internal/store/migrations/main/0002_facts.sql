-- 0002_facts — MEMORY.md §5's durable facts.
--
-- Separate migration from the index because the two tiers have different
-- lifetimes and because §5's five rules are all schema-visible: every fact
-- carries evidence, facts decay on last observation, contradictions supersede
-- rather than accumulate, everything is editable, and nothing here is a secret.

CREATE TABLE fact (
    id         TEXT PRIMARY KEY,
    subject    TEXT NOT NULL DEFAULT 'user',
    predicate  TEXT NOT NULL,          -- prefers | uses | deploys_on | writes
    object     TEXT NOT NULL,
    text       TEXT NOT NULL,          -- the sentence a human reads

    confidence REAL NOT NULL DEFAULT 0.5,
    first_seen INTEGER NOT NULL,
    -- Decay is on last observation, not creation: a long-held habit that still
    -- shows up stays strong.
    last_seen  INTEGER NOT NULL,

    -- Contradictions replace. The old fact stays as history with its date, so
    -- "you used to use Firebase" is still answerable.
    superseded_by TEXT REFERENCES fact (id) ON DELETE SET NULL,
    superseded_at INTEGER,

    -- Edited by hand in the console or the app. A wrong fact the user can
    -- correct in one field beats one they can only delete.
    edited_at  INTEGER,
    deleted_at INTEGER
) STRICT;

CREATE INDEX fact_live ON fact (deleted_at, superseded_at, last_seen DESC);
CREATE INDEX fact_by_predicate ON fact (predicate, object);

-- A fact that cannot point at where it came from is deleted, not kept at low
-- confidence. Evidence is a pointer into the transcript, same as the index.
CREATE TABLE fact_evidence (
    id          TEXT PRIMARY KEY,
    fact_id     TEXT NOT NULL REFERENCES fact (id) ON DELETE CASCADE,
    runtime     TEXT NOT NULL,
    session_id  TEXT NOT NULL,
    path        TEXT NOT NULL DEFAULT '',
    byte_offset INTEGER NOT NULL DEFAULT 0,
    quote       TEXT NOT NULL DEFAULT '',
    at          INTEGER NOT NULL
) STRICT;

CREATE INDEX fact_evidence_by_fact ON fact_evidence (fact_id, at DESC);
