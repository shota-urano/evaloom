-- Adapter names are validated against the runtime Registry, not a fixed SQL list.
-- Replace only this column, preserving IDs and every referencing table.
ALTER TABLE harnesses ADD COLUMN registered_adapter TEXT NOT NULL DEFAULT '';
UPDATE harnesses SET registered_adapter = adapter;
ALTER TABLE harnesses DROP COLUMN adapter;
ALTER TABLE harnesses RENAME COLUMN registered_adapter TO adapter;
