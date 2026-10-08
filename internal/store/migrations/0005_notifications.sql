-- The notification outbox: one row per (channel, event, key), so a message is
-- recorded once and then delivered, retried or given up on.
CREATE TABLE notifications (
  id         INTEGER PRIMARY KEY,
  channel    TEXT NOT NULL,
  event      TEXT NOT NULL,
  key        TEXT NOT NULL,
  job_id     INTEGER,
  source     TEXT NOT NULL DEFAULT '',
  title      TEXT NOT NULL,
  body       TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  attempts   INTEGER NOT NULL DEFAULT 0,
  next_at    INTEGER NOT NULL,
  sent_at    INTEGER,
  state      TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','sent','failed','suppressed')),
  error      TEXT NOT NULL DEFAULT '',
  UNIQUE(channel, event, key)
);
CREATE INDEX notifications_state_next ON notifications(state, next_at);
CREATE INDEX notifications_job ON notifications(job_id);

-- A channel's baseline: nothing older than `since` (ms) is ever sent to it.
CREATE TABLE notify_channel (
  name  TEXT PRIMARY KEY,
  since INTEGER NOT NULL
);

-- When the source's current run of failed polls began (NULL while healthy).
ALTER TABLE source_state ADD COLUMN failing_since INTEGER;
