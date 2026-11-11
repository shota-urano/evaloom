-- Registration tables from docs/specs/02-data-model.md 3.2.
-- Copy only the old default-role assignment. Missing assignments and globally
-- duplicated labels fail the migration rather than inventing a Profile/route.
CREATE TABLE runners_new (
    id TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL,
    token_hash TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('online', 'offline')),
    last_heartbeat_at TEXT,
    boot_id TEXT,
    created_at TEXT NOT NULL
);
INSERT INTO runners_new SELECT id, name, token_hash, status,
    NULLIF(last_heartbeat_at, ''), boot_id, created_at FROM runners;
DROP TABLE runners;
ALTER TABLE runners_new RENAME TO runners;

CREATE TABLE harnesses_new (
    id TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL,
    repo_url TEXT NOT NULL,
    repo_path TEXT NOT NULL,
    default_profile_id TEXT REFERENCES harness_profiles(id),
    approval_waivers TEXT NOT NULL DEFAULT '[]',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY (id, default_profile_id) REFERENCES harness_profiles(harness_id, id)
);
INSERT INTO harnesses_new (id, name, repo_url, repo_path, created_at, updated_at)
    SELECT id, name, repo_url, repo_path, created_at, updated_at FROM harnesses;

CREATE TABLE harness_profiles_new (
    id TEXT PRIMARY KEY NOT NULL,
    harness_id TEXT NOT NULL REFERENCES harnesses(id),
    version TEXT NOT NULL,
    commit_sha TEXT NOT NULL,
    tag_name TEXT NOT NULL,
    contract_version TEXT NOT NULL,
    definition TEXT NOT NULL,
    definition_hash TEXT NOT NULL,
    -- Display/search projection allowed by 02, harness_profiles.
    model_id TEXT NOT NULL,
    operation_status TEXT NOT NULL CHECK (operation_status IN ('provisional', 'official')),
    registered_at TEXT NOT NULL,
    UNIQUE (harness_id, version),
    UNIQUE (harness_id, id)
);
INSERT INTO harness_profiles_new
    SELECT hp.id, hp.harness_id, hp.version, hp.commit_sha, hp.tag_name,
        h.contract_version, hp.definition, hp.definition_hash, hp.model_id,
        hp.operation_status, hp.registered_at
    FROM harness_profiles hp JOIN harnesses h ON h.id = hp.harness_id;
DROP TABLE harness_profiles;
DROP TABLE harnesses;
ALTER TABLE harnesses_new RENAME TO harnesses;
ALTER TABLE harness_profiles_new RENAME TO harness_profiles;

CREATE TABLE projects_new (
    id TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL,
    harness_id TEXT NOT NULL REFERENCES harnesses(id),
    profile_id TEXT NOT NULL REFERENCES harness_profiles(id),
    params TEXT NOT NULL DEFAULT '{}',
    approval_waivers TEXT NOT NULL DEFAULT '[]',
    runner_id TEXT NOT NULL REFERENCES runners(id),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY (harness_id, profile_id) REFERENCES harness_profiles(harness_id, id)
);
INSERT INTO projects_new (id, name, harness_id, profile_id, runner_id, created_at, updated_at)
    SELECT p.id, p.name, ph.harness_id, ph.profile_id, p.runner_id, p.created_at, p.updated_at
    FROM projects p LEFT JOIN project_harnesses ph
        ON ph.project_id = p.id AND ph.role = p.default_role;

CREATE TABLE routes (
    label TEXT PRIMARY KEY NOT NULL,
    project_id TEXT UNIQUE REFERENCES projects(id),
    harness_id TEXT UNIQUE REFERENCES harnesses(id),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK ((project_id IS NOT NULL) <> (harness_id IS NOT NULL))
);
INSERT INTO routes (label, project_id, created_at, updated_at)
    SELECT linear_routing_label, id, created_at, updated_at FROM projects
    WHERE linear_routing_label IS NOT NULL;

-- Preserve the old effective polling configuration when it is unambiguous.
-- Multiple old Teams cannot map to the one-Team setting: the settings PK
-- rejects them atomically instead of silently selecting a Team.
INSERT INTO settings (key, value, updated_at)
    SELECT 'linear.teamId', json_quote(linear_team_id), max(updated_at)
    FROM projects
    WHERE linear_team_id IS NOT NULL AND trim(linear_team_id) <> ''
        AND NOT EXISTS (SELECT 1 FROM settings WHERE key='linear.teamId')
    GROUP BY linear_team_id;
-- An absent old prefix meant repo:. Keep that effective value for old routes.
INSERT INTO settings (key, value, updated_at)
    SELECT 'linear.routingLabelPrefix', '"repo:"', max(updated_at) FROM projects
    WHERE linear_routing_label IS NOT NULL
    HAVING count(*) > 0 AND NOT EXISTS (
        SELECT 1 FROM settings WHERE key='linear.routingLabelPrefix'
    );

DROP TABLE project_harnesses;
DROP TABLE projects;
ALTER TABLE projects_new RENAME TO projects;
