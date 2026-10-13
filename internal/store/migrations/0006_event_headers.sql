-- The last event's headers (filtered), so the rule tester can replay them.
ALTER TABLE source_state ADD COLUMN headers TEXT NOT NULL DEFAULT '{}';
