-- When an on: each id was last seen in a poll result, so retention can keep ids still returned.
ALTER TABLE rule_state ADD COLUMN last_seen_at INTEGER;
