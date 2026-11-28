-- Spec 20, section 7. Program clients hold one bearer key (hash only) and one
-- routing target. Tasks gain source 'program' and the creating client, and
-- audit_logs accepts the actor 'program:<client ID>'. Existing rows keep their
-- values: no existing task is source 'program'.
CREATE TABLE program_clients (
    id TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    project_id TEXT REFERENCES projects(id),
    harness_id TEXT REFERENCES harnesses(id),
    created_at TEXT NOT NULL,
    revoked_at TEXT,
    CHECK ((project_id IS NULL) <> (harness_id IS NULL))
);

CREATE TABLE tasks_new (
    id TEXT PRIMARY KEY NOT NULL,
    source TEXT NOT NULL DEFAULT 'manual' CHECK (source IN ('manual', 'linear', 'program')),
    project_id TEXT REFERENCES projects(id),
    harness_id TEXT REFERENCES harnesses(id),
    title TEXT NOT NULL,
    body TEXT NOT NULL,
    acceptance_criteria TEXT NOT NULL,
    collaboration_mode TEXT NOT NULL CHECK (collaboration_mode = 'solo'),
    status TEXT NOT NULL CHECK (status IN (
        'received', 'routing', 'queued', 'running', 'waiting_approval', 'blocked',
        'completed', 'failed', 'cancelled', 'timed_out'
    )),
    intake_block_cause TEXT CHECK (intake_block_cause IN (
        'missing_acceptance_criteria', 'config_missing_profile'
    )),
    idempotency_key TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    program_client_id TEXT REFERENCES program_clients(id),
    CHECK (project_id IS NULL OR harness_id IS NULL),
    CHECK ((source = 'program') = (program_client_id IS NOT NULL))
);

INSERT INTO tasks_new (
    id, source, project_id, harness_id, title, body, acceptance_criteria,
    collaboration_mode, status, intake_block_cause, idempotency_key, created_at, updated_at
) SELECT id, source, project_id, harness_id, title, body, acceptance_criteria,
    collaboration_mode, status, intake_block_cause, idempotency_key, created_at, updated_at
FROM tasks;

DROP TABLE tasks;
ALTER TABLE tasks_new RENAME TO tasks;
CREATE INDEX tasks_program_client_id ON tasks(program_client_id, created_at);

CREATE TABLE audit_logs_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    actor TEXT NOT NULL CHECK (actor IN ('owner', 'system') OR actor GLOB 'runner:?*' OR actor GLOB 'program:?*'),
    action TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id TEXT NOT NULL,
    detail TEXT NOT NULL,
    created_at TEXT NOT NULL
);

INSERT INTO audit_logs_new (id, actor, action, target_type, target_id, detail, created_at)
SELECT id, actor, action, target_type, target_id, detail, created_at FROM audit_logs;
-- Keep the high-water mark even when the highest audit rows were deleted, and
-- also when no rows remain (then the new table has no sequence row yet).
INSERT INTO sqlite_sequence (name, seq)
SELECT 'audit_logs_new', 0 WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = 'audit_logs_new');
UPDATE sqlite_sequence SET seq = max(seq, coalesce(
    (SELECT seq FROM sqlite_sequence WHERE name = 'audit_logs'), 0
)) WHERE name = 'audit_logs_new';

DROP TABLE audit_logs;
ALTER TABLE audit_logs_new RENAME TO audit_logs;
