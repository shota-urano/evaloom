-- Spec 02, section 3.5. Legacy artifacts have no stage/name and legacy usage
-- has no stage. Reject those rows atomically rather than inventing history.
CREATE TABLE artifacts_new (
    id TEXT PRIMARY KEY NOT NULL,
    execution_id TEXT REFERENCES executions(id),
    route_proposal_id TEXT REFERENCES route_proposals(id),
    stage TEXT NOT NULL CHECK (stage IN ('prepare', 'work', 'check', 'publish', 'runner', 'proposal')),
    kind TEXT NOT NULL CHECK (kind IN (
        'log', 'out_file', 'request_json', 'result_json', 'work_summary', 'question', 'proposal_result'
    )),
    name TEXT NOT NULL,
    storage_path TEXT NOT NULL,
    sha256 TEXT NOT NULL,
    size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
    redacted INTEGER NOT NULL CHECK (redacted IN (0, 1)),
    created_at TEXT NOT NULL,
    CHECK ((execution_id IS NOT NULL) <> (route_proposal_id IS NOT NULL))
);

INSERT INTO artifacts_new (
    id, execution_id, stage, kind, name, storage_path, sha256, size_bytes, redacted, created_at
) SELECT id, execution_id, NULL, kind, NULL, storage_path, sha256, size_bytes, redacted, created_at
FROM artifacts;

CREATE TABLE usage_records_new (
    id TEXT PRIMARY KEY NOT NULL,
    execution_id TEXT NOT NULL REFERENCES executions(id),
    stage TEXT NOT NULL CHECK (stage IN ('prepare', 'work', 'check', 'publish')),
    input_tokens INTEGER CHECK (input_tokens >= 0),
    output_tokens INTEGER CHECK (output_tokens >= 0),
    wall_time_ms INTEGER NOT NULL CHECK (wall_time_ms >= 0),
    cost_amount REAL,
    created_at TEXT NOT NULL
);

INSERT INTO usage_records_new (
    id, execution_id, stage, input_tokens, output_tokens, wall_time_ms, cost_amount, created_at
) SELECT id, execution_id, NULL, input_tokens, output_tokens, wall_time_ms, cost_amount, created_at
FROM usage_records;

DROP TABLE artifacts;
ALTER TABLE artifacts_new RENAME TO artifacts;
DROP TABLE usage_records;
ALTER TABLE usage_records_new RENAME TO usage_records;
CREATE INDEX artifacts_execution_id ON artifacts(execution_id);
CREATE INDEX artifacts_route_proposal_id ON artifacts(route_proposal_id);
CREATE INDEX usage_records_execution_id ON usage_records(execution_id);

CREATE TABLE harness_records (
    id TEXT PRIMARY KEY NOT NULL,
    harness_id TEXT NOT NULL REFERENCES harnesses(id),
    artifact_id TEXT NOT NULL REFERENCES artifacts(id),
    execution_id TEXT NOT NULL REFERENCES executions(id),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    source_identifier TEXT,
    file_name TEXT NOT NULL,
    created_at TEXT NOT NULL,
    deleted_at TEXT
);
CREATE INDEX harness_records_available ON harness_records(harness_id, created_at DESC, id DESC)
    WHERE deleted_at IS NULL;

-- audit_logs already matches section 3.5. Keep its rows and AUTOINCREMENT.
