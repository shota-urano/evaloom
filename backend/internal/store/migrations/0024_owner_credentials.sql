CREATE TABLE owner_credentials (
    id TEXT PRIMARY KEY NOT NULL,
    password_hash TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- The owner is a singleton even when the login ID changes (spec 18 3.8).
CREATE UNIQUE INDEX owner_credentials_singleton ON owner_credentials ((1));

CREATE TABLE owner_sessions (
    id TEXT PRIMARY KEY NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    csrf_token_hash TEXT NOT NULL,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    via TEXT NOT NULL CHECK (via IN ('loopback', 'tailnet'))
);
