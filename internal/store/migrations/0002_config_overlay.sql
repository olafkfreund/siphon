CREATE TABLE config_item (
  kind       TEXT NOT NULL,
  name       TEXT NOT NULL,
  yaml       TEXT NOT NULL,
  deleted    INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (kind, name)
);

CREATE TABLE config_revision (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  at         INTEGER NOT NULL,
  actor      TEXT NOT NULL,
  summary    TEXT NOT NULL,
  items_json TEXT NOT NULL,
  diff       TEXT NOT NULL
);
CREATE INDEX config_revision_at ON config_revision(at);
