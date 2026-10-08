-- The last Test of a Services connection: whether it worked, and a short,
-- secret-free detail. One row per connection.
CREATE TABLE connection_check (
  connection TEXT PRIMARY KEY,
  at         INTEGER NOT NULL,
  ok         INTEGER NOT NULL,
  detail     TEXT NOT NULL DEFAULT ''
);
