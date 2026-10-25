CREATE TABLE runners (
    id TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL,
    token_hash TEXT NOT NULL,
    worktree_root TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('online', 'offline')),
    last_heartbeat_at TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE projects (
    id TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL,
    local_repo_path TEXT NOT NULL UNIQUE,
    git_remote_url TEXT NOT NULL,
    runner_id TEXT NOT NULL REFERENCES runners(id),
    base_branch TEXT NOT NULL,
    default_role TEXT NOT NULL,
    review_role TEXT,
    context_location TEXT NOT NULL,
    test_command TEXT,
    test_report_format TEXT,
    test_report_path TEXT,
    setup_command TEXT,
    copy_allowlist TEXT NOT NULL,
    test_file_patterns TEXT NOT NULL,
    allowed_operations TEXT NOT NULL,
    approval_policy TEXT NOT NULL,
    timeouts TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE harnesses (
    id TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL,
    specialty TEXT NOT NULL,
    supported_tasks TEXT NOT NULL,
    adapter TEXT NOT NULL CHECK (adapter IN ('codex', 'claude_code')),
    repo_path TEXT NOT NULL,
    repo_url TEXT NOT NULL,
    supported_models TEXT NOT NULL,
    available_tools TEXT NOT NULL,
    tool_permissions TEXT NOT NULL,
    contract_version TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE harness_profiles (
    id TEXT PRIMARY KEY NOT NULL,
    harness_id TEXT NOT NULL REFERENCES harnesses(id),
    version TEXT NOT NULL,
    commit_sha TEXT NOT NULL,
    tag_name TEXT NOT NULL,
    definition TEXT NOT NULL,
    definition_hash TEXT NOT NULL,
    model_id TEXT NOT NULL,
    operation_status TEXT NOT NULL CHECK (operation_status = 'provisional'),
    registered_at TEXT NOT NULL,
    UNIQUE (harness_id, version)
);

CREATE TABLE project_harnesses (
    project_id TEXT NOT NULL REFERENCES projects(id),
    role TEXT NOT NULL,
    harness_id TEXT NOT NULL REFERENCES harnesses(id),
    profile_id TEXT NOT NULL REFERENCES harness_profiles(id),
    assigned_at TEXT NOT NULL,
    PRIMARY KEY (project_id, role)
);
