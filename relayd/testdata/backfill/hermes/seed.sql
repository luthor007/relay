-- Three sessions, chosen to exercise the three things that differ in practice:
-- a fully-populated row, a row with only an estimated cost and no title, and a
-- row Hermes never finished writing.

INSERT INTO sessions (id, title, cwd, model, started_at, updated_at, message_count,
                      tool_call_count, estimated_cost_usd, actual_cost_usd,
                      input_tokens, output_tokens, cache_read_tokens)
VALUES
  ('hs-0001', 'refactor the BLE codec around CRC-16/MODBUS', '/home/user/src/relay',
   'claude-opus-4', 1770553200000, 1770556800000, 42, 11, 1.9400, 2.1100, 91000, 12400, 410000),

  ('hs-0002', NULL, '/home/user/src/osmo', 'gpt-5-codex',
   1770639600000, 1770641400000, 8, 2, 0.3100, NULL, 12000, 900, 4000),

  ('hs-0003', 'quick question about wazero pins', '/home/user/src/relay', NULL,
   1770726000000, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL);

INSERT INTO messages (id, session_id, role, content, created_at) VALUES
  (1, 'hs-0001', 'user',      'the checksum is wrong on every frame the glasses send', 1770553200000),
  (2, 'hs-0001', 'assistant', 'The vendor spec says ARC. The disassembly says MODBUS with init 0xFFFF — the spec is wrong.', 1770553260000),
  (3, 'hs-0001', 'user',      'fix it and add a test', 1770553320000),
  (4, 'hs-0002', 'user',      'why does opencode report zero sessions', 1770639600000),
  (5, 'hs-0002', 'assistant', 'It is installed and has never been run. That is the normal case, not an error.', 1770639660000),
  (6, 'hs-0003', 'user',      'which wazero version is safe', 1770726000000);

INSERT INTO messages_fts (rowid, content) SELECT id, content FROM messages;

-- A held lease, so a reader that reached for it would find something to fight
-- over. Nothing in backfill may read this row.
INSERT INTO compression_locks (session_id, holder, expires_at)
VALUES ('hs-0001', 'hermes-cli/17422', 1770556800000);
