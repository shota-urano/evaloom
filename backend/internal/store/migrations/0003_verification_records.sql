CREATE TABLE artifacts (
    id TEXT PRIMARY KEY NOT NULL,
    execution_id TEXT NOT NULL REFERENCES executions(id),
    kind TEXT NOT NULL CHECK (kind IN (
        'harness_log', 'test_log', 'setup_log', 'diff', 'test_report', 'test_ids', 'harness_output'
    )),
    storage_path TEXT NOT NULL,
    sha256 TEXT NOT NULL,
    size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
    redacted INTEGER NOT NULL CHECK (redacted IN (0, 1)),
    created_at TEXT NOT NULL
);

CREATE INDEX artifacts_execution_id ON artifacts(execution_id);

CREATE TABLE test_runs (
    id TEXT PRIMARY KEY NOT NULL,
    execution_id TEXT NOT NULL REFERENCES executions(id),
    kind TEXT NOT NULL CHECK (kind IN ('setup', 'baseline', 'verify')),
    target_sha TEXT NOT NULL,
    command TEXT NOT NULL,
    exit_code INTEGER,
    total INTEGER CHECK (total >= 0),
    passed INTEGER CHECK (passed >= 0),
    failed INTEGER CHECK (failed >= 0),
    skipped INTEGER CHECK (skipped >= 0),
    test_ids_artifact_id TEXT REFERENCES artifacts(id),
    timed_out INTEGER NOT NULL CHECK (timed_out IN (0, 1)),
    started_at TEXT NOT NULL,
    finished_at TEXT NOT NULL
);

CREATE INDEX test_runs_execution_id ON test_runs(execution_id);

CREATE TABLE gate_results (
    id TEXT PRIMARY KEY NOT NULL,
    execution_id TEXT NOT NULL REFERENCES executions(id),
    gate INTEGER NOT NULL CHECK (gate IN (1, 2, 3)),
    result TEXT NOT NULL CHECK (result IN ('pass', 'blocked', 'failed')),
    base_sha TEXT NOT NULL,
    target_sha TEXT NOT NULL,
    findings TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE INDEX gate_results_execution_id ON gate_results(execution_id);

CREATE TABLE execution_blocks (
    id TEXT PRIMARY KEY NOT NULL,
    execution_id TEXT NOT NULL REFERENCES executions(id),
    cause TEXT NOT NULL CHECK (cause IN (
        'gate1_test_change', 'gate1_unverifiable', 'gate2_missing_tests',
        'gate2_unverifiable', 'gate3_mismatch', 'context_conflict', 'git_state'
    )),
    gate_result_id TEXT REFERENCES gate_results(id),
    head_sha TEXT NOT NULL,
    detail TEXT NOT NULL,
    decision TEXT CHECK (decision IN ('continue', 'abort')),
    decided_at TEXT,
    created_at TEXT NOT NULL
);

CREATE INDEX execution_blocks_execution_id ON execution_blocks(execution_id);

CREATE TABLE audit_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    actor TEXT NOT NULL CHECK (actor IN ('owner', 'system') OR actor GLOB 'runner:?*'),
    action TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id TEXT NOT NULL,
    detail TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE usage_records (
    id TEXT PRIMARY KEY NOT NULL,
    execution_id TEXT NOT NULL REFERENCES executions(id),
    input_tokens INTEGER CHECK (input_tokens >= 0),
    output_tokens INTEGER CHECK (output_tokens >= 0),
    wall_time_ms INTEGER NOT NULL CHECK (wall_time_ms >= 0),
    cost_amount REAL,
    created_at TEXT NOT NULL
);

CREATE INDEX usage_records_execution_id ON usage_records(execution_id);
