-- Claude Code as a listable account rather than a single credential per user:
-- see plans/M2/04-multiple-claude-accounts.md for why a name and a default were
-- the two things that turned out to be missing.
CREATE TABLE claude_accounts (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    -- api_key and oauth_token are claudex.KindAPIKey and claudex.KindOAuthToken:
    -- the meaning of a kind is which environment variable the CLI reads, so
    -- those two are that package's word. login names no variable at all — it
    -- says the credential is a directory Hexagon keeps — which is why its
    -- constant is store.ClaudeAccountKindLogin instead.
    kind       TEXT NOT NULL CHECK (kind IN ('api_key', 'oauth_token', 'login')),
    -- Nullable, and the only place the three kinds differ in this table: a
    -- login account is a row with a name and a directory and no secret at all,
    -- which is also the honest description of one that has never been signed
    -- into.
    secret_enc BLOB,
    is_default INTEGER NOT NULL DEFAULT 0 CHECK (is_default IN (0, 1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (user_id, name)
);
CREATE INDEX idx_claude_accounts_user ON claude_accounts(user_id);
-- "At most one default per user" as a fact the database enforces, rather than
-- a column on users that would put a property of this list somewhere else to
-- read it from.
CREATE UNIQUE INDEX idx_claude_accounts_default ON claude_accounts(user_id) WHERE is_default = 1;

-- Carry over the credential every user already configured, as their default
-- account: nobody is asked to paste a token again for a change they did not
-- make. The id is a v4 UUID built in SQL, exactly as 003_provider_accounts.sql
-- builds one.
INSERT INTO claude_accounts (id, user_id, name, kind, secret_enc, is_default, created_at, updated_at)
SELECT
    lower(
        hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-4' || substr(hex(randomblob(2)), 2) || '-' ||
        substr('89ab', abs(random()) % 4 + 1, 1) || substr(hex(randomblob(2)), 2) || '-' || hex(randomblob(6))
    ),
    user_id, 'default', kind, secret_enc, 1, created_at, updated_at
FROM claude_credentials;

DROP TABLE claude_credentials;

-- Which account a session's container authenticates Claude Code with. Empty
-- means "whatever the server resolves" — the user's default account, or the
-- server's own configuration — which is precisely what every session that
-- exists today already gets, so nothing is backfilled and no existing row
-- changes meaning. No foreign key: ON DELETE SET NULL would quietly turn a
-- session that names its account into one that names none, while its
-- container goes on carrying that account's credential, so an account a
-- session still uses is refused deletion instead — see
-- CountSessionsUsingClaudeAccount.
ALTER TABLE sessions ADD COLUMN claude_account_id TEXT NOT NULL DEFAULT '';
