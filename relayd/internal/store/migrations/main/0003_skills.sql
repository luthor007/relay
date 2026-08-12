-- 0003_skills — the playbooks the orchestrator writes for itself.
--
-- A skill is instructions, never code and never an action. Relay orchestrates
-- and does not execute: a skill for "check the staging dashboard" does not open
-- a browser, it tells whichever agent has one how to, and Relay's contribution
-- was choosing that agent and handing over the text. Nothing in this table is
-- ever run by relayd, which is why there is no command column and no argv.
--
-- It is persisted rather than held in memory for the reason the feature exists:
-- a skill is supposed to get better during use, and an agent that forgets every
-- playbook when the daemon restarts is not learning, it is repeating itself.

CREATE TABLE skill (
    name  TEXT PRIMARY KEY,           -- the wire name; also the tool name suffix
    title TEXT NOT NULL DEFAULT '',   -- one line for the console

    -- The trigger. A skill without one is refused at the door: a playbook no
    -- model will reach for is worse than absent, because it still costs context.
    trigger_when TEXT NOT NULL,
    -- What the executing agent is told to do, in plain imperative sentences.
    steps        TEXT NOT NULL,
    -- Capabilities the agent must already have — "browser", "kubectl" — as a
    -- JSON array. Advisory: it is repeated in the instruction text so the agent
    -- can say it lacks one rather than improvise.
    needs        TEXT NOT NULL DEFAULT '[]',

    -- Where it came from. A skill Relay wrote for itself and one a human wrote
    -- deserve different scrutiny in review, and the console shows both.
    origin     TEXT NOT NULL DEFAULT 'orchestrator',
    created_at INTEGER NOT NULL,
    -- Bumped on every rewrite. A skill improves in place rather than forking,
    -- so this is the only record that it changed.
    updated_at INTEGER NOT NULL,
    -- Editable and deletable from the console, same as a fact: a wrong playbook
    -- the user can correct in one field beats one they can only delete.
    deleted_at INTEGER
) STRICT;

CREATE INDEX skill_live ON skill (deleted_at, updated_at DESC);
