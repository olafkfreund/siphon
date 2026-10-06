CREATE TABLE jobs (
  id          INTEGER PRIMARY KEY,
  rule        TEXT NOT NULL,
  action_json TEXT NOT NULL,
  state       TEXT NOT NULL DEFAULT 'queued'
    CHECK (state IN ('queued','running','pending_approval','done','failed','cancelled')),
  attempt     INTEGER NOT NULL DEFAULT 0,
  run_after   INTEGER NOT NULL DEFAULT 0,  -- unix ms
  started_at  INTEGER,
  finished_at INTEGER,
  exit_code   INTEGER,
  output      TEXT NOT NULL DEFAULT '',
  parent_id   INTEGER REFERENCES jobs(id),
  depth       INTEGER NOT NULL DEFAULT 0,
  resume_step INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL
);
CREATE INDEX jobs_state_run_after ON jobs(state, run_after);

CREATE TABLE rule_state (
  rule          TEXT NOT NULL,
  key           TEXT NOT NULL,
  last_value    INTEGER NOT NULL DEFAULT 0,
  last_fired_at INTEGER,
  PRIMARY KEY (rule, key)
);

CREATE TABLE seen_event (
  scope   TEXT NOT NULL,
  id      TEXT NOT NULL,
  seen_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX seen_event_scope_id ON seen_event(scope, id);

CREATE TABLE approvals (
  id         INTEGER PRIMARY KEY,
  job_id     INTEGER NOT NULL REFERENCES jobs(id),
  token_hash BLOB NOT NULL,
  expires_at INTEGER NOT NULL,
  decision   TEXT CHECK (decision IN ('approved','denied')),
  decided_by TEXT,
  decided_at INTEGER
);

CREATE TABLE audit (
  id     INTEGER PRIMARY KEY,
  at     INTEGER NOT NULL,
  actor  TEXT NOT NULL,
  event  TEXT NOT NULL,
  job_id INTEGER,
  detail TEXT NOT NULL DEFAULT ''
);

CREATE TABLE rule_override (
  rule       TEXT PRIMARY KEY,
  enabled    INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE source_state (
  source       TEXT PRIMARY KEY,
  json         TEXT NOT NULL DEFAULT '',
  last_poll_at INTEGER,
  last_error   TEXT NOT NULL DEFAULT ''
);
