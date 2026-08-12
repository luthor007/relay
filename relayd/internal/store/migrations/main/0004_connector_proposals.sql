-- 0004_connector_proposals — the evidence behind ORCHESTRATOR.md §4b's
-- suggestion, and the answer the user already gave.
--
-- Two tables, because they fail differently. Evidence accrues and expires;
-- a dismissal is a decision and expires only on the cooldown.
--
-- **Neither table stores what was said.** connector.Proposer discards
-- Evidence.Text the moment it has matched — it keeps a connector, an episode
-- and a timestamp, and the sentence the user is shown is generated from the
-- COUNTS by evidenceLine(), never quoted back. Adding an `evidence TEXT` column
-- here so the console could "show why" would put unredacted user speech into
-- relay.db, which is MEMORY.md §6's "detect secrets before indexing, never
-- after" broken at the schema level rather than in a code path. There is
-- nowhere in this file for a transcript to land, and that is the point.

CREATE TABLE connector_sighting (
    -- The connector's grant key: "prusa". Not a foreign key to anything: a
    -- connector that is unconfigured today may be configured tomorrow, and
    -- evidence gathered in between is still evidence.
    connector TEXT NOT NULL,
    -- The conversation it came from. Mentions are counted PER EPISODE, which is
    -- what makes "four times this week" mean four occasions rather than four
    -- sentences in one rant. Empty is allowed and means unattributed.
    episode   TEXT NOT NULL DEFAULT '',
    at        INTEGER NOT NULL          -- unix milliseconds
) STRICT;

-- The read is always "this connector, inside the window", so the index is on
-- both and the sweep that drops expired evidence uses the same one.
CREATE INDEX connector_sighting_at ON connector_sighting (connector, at DESC);

CREATE TABLE connector_dismissal (
    -- One row per connector: a second "not now" replaces the first rather than
    -- accumulating, because the cooldown runs from the most recent answer.
    connector TEXT PRIMARY KEY,
    at        INTEGER NOT NULL,         -- unix milliseconds
    -- Why, when the user said. Free text they typed about their own decision,
    -- and never a quote of anything Relay observed.
    reason    TEXT NOT NULL DEFAULT ''
) STRICT;
