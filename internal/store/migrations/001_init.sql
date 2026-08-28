-- Timestamps are TEXT, not DATETIME: the driver rewrites values it believes are
-- dates, so storing them as plain text keeps what we write and what we read
-- identical. store.timeLayout is UTC and fixed width, so the usual string
-- comparison in SQL still orders them correctly.

-- Users authenticated through the GitHub OAuth App. The GitHub access token
-- obtained at login is also what we use to list and clone repositories, so it
-- is stored sealed rather than in the clear.
CREATE TABLE users (
    id               TEXT PRIMARY KEY,
    github_login     TEXT NOT NULL UNIQUE,
    github_id        INTEGER NOT NULL UNIQUE,
    avatar_url       TEXT NOT NULL DEFAULT '',
    github_token_enc BLOB NOT NULL,
    created_at       TEXT NOT NULL,
    last_login_at    TEXT NOT NULL
);

-- Browser login sessions. Only the SHA-256 of the cookie value is stored, so a
-- database leak does not hand out live sessions.
CREATE TABLE user_sessions (
    token_hash BLOB PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);

CREATE INDEX idx_user_sessions_user ON user_sessions(user_id);

-- Base images available when starting a session, either built from a Dockerfile
-- or pulled from a registry.
CREATE TABLE images (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users(id),
    name         TEXT NOT NULL,
    source_type  TEXT NOT NULL CHECK (source_type IN ('dockerfile', 'registry')),
    dockerfile   TEXT NOT NULL DEFAULT '',
    registry_ref TEXT NOT NULL DEFAULT '',
    image_ref    TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL CHECK (status IN ('pending', 'building', 'ready', 'failed')),
    build_log    TEXT NOT NULL DEFAULT '',
    error        TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    UNIQUE (user_id, name)
);

CREATE INDEX idx_images_user ON images(user_id);

-- Claude Code sessions. status is the persisted view; the real container state
-- is re-read from Docker on every read and reconciled at startup.
CREATE TABLE sessions (
    id             TEXT PRIMARY KEY,
    user_id        TEXT NOT NULL REFERENCES users(id),
    title          TEXT NOT NULL,
    repo_full_name TEXT NOT NULL,
    repo_clone_url TEXT NOT NULL,
    branch         TEXT NOT NULL DEFAULT '',
    image_id       TEXT NOT NULL REFERENCES images(id),
    image_ref      TEXT NOT NULL,
    workspace_dir  TEXT NOT NULL,
    repo_dir       TEXT NOT NULL,
    container_id   TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL CHECK (status IN ('creating', 'cloning', 'starting', 'running', 'stopped', 'failed', 'gone')),
    error          TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
);

CREATE INDEX idx_sessions_user ON sessions(user_id);
