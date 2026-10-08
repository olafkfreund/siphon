CREATE TABLE rule_error (
  rule  TEXT PRIMARY KEY,
  at    INTEGER NOT NULL,
  error TEXT NOT NULL
);
ALTER TABLE source_state ADD COLUMN event_at INTEGER;
ALTER TABLE source_state ADD COLUMN last_reject_at INTEGER;
ALTER TABLE source_state ADD COLUMN last_reject TEXT NOT NULL DEFAULT '';
