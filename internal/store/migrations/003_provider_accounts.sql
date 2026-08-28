-- Where a user's repositories come from.
--
-- Until now this was one column on users: the GitHub OAuth token, doing duty as
-- the proof of who signed in, as the key to the repository listing, and as the
-- secret handed to a session container. Only the first of those belongs to
-- identity, so the credential moves out into its own table, one row per user
-- and provider, and the column goes with it. Two places holding the same secret
-- is how they drift.
CREATE TABLE provider_accounts (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider    TEXT NOT NULL CHECK (provider IN ('github', 'bitbucket')),
    -- account is the name shown in the UI. identity is what the provider's API
    -- wants as the user half of its credentials, which is not the same thing:
    -- Bitbucket wants the Atlassian account email, GitHub wants nothing at all.
    account     TEXT NOT NULL,
    identity    TEXT NOT NULL DEFAULT '',
    avatar_url  TEXT NOT NULL DEFAULT '',
    secret_enc  BLOB NOT NULL,
    -- Unused today: Hexagon only stores pasted tokens and the OAuth token it
    -- gets at login, neither of which is refreshed. An OAuth provider would
    -- need both, and the columns cost nothing now against a migration later.
    refresh_enc BLOB,
    expires_at  TEXT,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    UNIQUE (user_id, provider)
);

CREATE INDEX idx_provider_accounts_user ON provider_accounts(user_id);

-- Carry over the token every signed-in user already has, so nobody is asked to
-- sign in again for a change they did not make. The id is a v4 UUID built in
-- SQL, so these rows look like every other row the application writes.
INSERT INTO provider_accounts (id, user_id, provider, account, avatar_url, secret_enc, created_at, updated_at)
SELECT
    lower(
        hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-4' || substr(hex(randomblob(2)), 2) || '-' ||
        substr('89ab', abs(random()) % 4 + 1, 1) || substr(hex(randomblob(2)), 2) || '-' || hex(randomblob(6))
    ),
    id, 'github', github_login, avatar_url, github_token_enc, created_at, last_login_at
FROM users;

ALTER TABLE users DROP COLUMN github_token_enc;

-- Which account a session's repository came from, and therefore which
-- credential its clone and its container get. Everything that exists today came
-- from GitHub, which is exactly what the default says.
ALTER TABLE sessions ADD COLUMN provider TEXT NOT NULL DEFAULT 'github';
