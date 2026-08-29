-- The Anthropic credential a user configured from the UI. It is handed to
-- session containers, and to the host's `claude -p`, as an environment
-- variable.
--
-- One row per user, so user_id is the primary key rather than a uuid with a
-- UNIQUE beside it: there is nothing to list and nothing to order, and a second
-- credential for the same person would only raise the question of which one
-- wins.
--
-- This is one half of "the Claude login". The other half is a subscription,
-- which is a file Claude Code writes at claude.credentials on the host in a
-- format Hexagon does not own; see plans/M1/10-claude-login-from-ui.md.
CREATE TABLE claude_credentials (
    user_id    TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    -- kind decides which environment variable carries the secret. The allowed
    -- values are claudex.KindAPIKey and claudex.KindOAuthToken: the meaning of
    -- the kind is which name the CLI reads, so it is that package's word.
    kind       TEXT NOT NULL CHECK (kind IN ('api_key', 'oauth_token')),
    secret_enc BLOB NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
