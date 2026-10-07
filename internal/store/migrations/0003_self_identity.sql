-- 0003_self_identity.sql records the authoring login on objects and events
-- so delivery can filter self-authored occurrences. Empty string means the
-- row predates this migration or the object has no author (synthetic target
-- events); such rows are always deliverable.
ALTER TABLE events  ADD COLUMN author TEXT NOT NULL DEFAULT '';
ALTER TABLE objects ADD COLUMN author TEXT NOT NULL DEFAULT '';
