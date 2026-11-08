-- docs/specs/02-data-model.md projects, 03-registry.md 3.3.1 (Q73, decided
-- 2026-11-07): destinations (`host:port`) the setup Test Sandbox may reach.
-- Existing Projects get an empty array, so their setup stays fully offline.
ALTER TABLE projects ADD COLUMN setup_allowed_hosts TEXT NOT NULL DEFAULT '[]';
