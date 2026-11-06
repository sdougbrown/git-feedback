ALTER TABLE targets ADD COLUMN generation TEXT NOT NULL DEFAULT '';
UPDATE targets SET generation = lower(hex(randomblob(16))) WHERE generation = '';
