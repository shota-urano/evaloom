-- NULL means no heartbeat has established a boot identity yet (including
-- Runners registered before this migration).
ALTER TABLE runners ADD COLUMN boot_id TEXT;
