-- image_transfers is a backup of an image leaving Hexagon, or a restore
-- coming back in: one table because both are the same object seen from two
-- sides — a file under DataDir, a job that is or is not finished with it, and
-- a lifetime after which the janitor removes it.
--
-- image_id is the image a backup was taken from, or the image a restore
-- produced. It is nullable: a restore has none until the import succeeds, and
-- a spec-only restore never gets one at all — it only ever fills in the create
-- form.
CREATE TABLE image_transfers (
    id            TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL REFERENCES users(id),
    direction     TEXT NOT NULL CHECK (direction IN ('backup', 'restore')),
    image_id      TEXT,
    name          TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL CHECK (status IN ('pending','running','ready','failed')),
    with_image    INTEGER NOT NULL DEFAULT 0,
    path          TEXT NOT NULL DEFAULT '',
    size          INTEGER NOT NULL DEFAULT 0,
    error         TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    expires_at    TEXT NOT NULL
);

CREATE INDEX idx_image_transfers_user ON image_transfers(user_id);
