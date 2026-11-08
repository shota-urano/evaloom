-- docs/specs/02-data-model.md 3.4 (Q76, decided 2026-11-08): global settings,
-- one key per row. A key without a row uses the default its spec defines, so
-- no default rows are inserted here.
CREATE TABLE settings (
	key        TEXT PRIMARY KEY,
	value      TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
