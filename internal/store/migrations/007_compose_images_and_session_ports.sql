-- Advanced images: a compose file describing the services that have to be there
-- beside the container the Dockerfile builds, and the two columns a session
-- needs to run one.

-- SQLite cannot alter a CHECK constraint, so admitting a third source_type
-- means rebuilding the table rather than adding a column to it. The migration
-- runner applies this with foreign_keys off and runs foreign_key_check before
-- committing (see Store.migrate), which is what lets the DROP below past
-- sessions.image_id without deleting anything.
CREATE TABLE images_new (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users(id),
    name         TEXT NOT NULL,
    source_type  TEXT NOT NULL CHECK (source_type IN ('dockerfile', 'registry', 'compose')),
    dockerfile   TEXT NOT NULL DEFAULT '',
    -- The services beside the agent, for a compose image. Hexagon renders its
    -- own service into a second file at session time; this one is the user's.
    compose      TEXT NOT NULL DEFAULT '',
    registry_ref TEXT NOT NULL DEFAULT '',
    image_ref    TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL CHECK (status IN ('pending', 'building', 'ready', 'failed')),
    build_log    TEXT NOT NULL DEFAULT '',
    error        TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    UNIQUE (user_id, name)
);

INSERT INTO images_new (id, user_id, name, source_type, dockerfile, registry_ref,
                        image_ref, status, build_log, error, created_at)
SELECT id, user_id, name, source_type, dockerfile, registry_ref,
       image_ref, status, build_log, error, created_at
FROM images;

DROP TABLE images;
ALTER TABLE images_new RENAME TO images;
CREATE INDEX idx_images_user ON images(user_id);

-- The container ports this session publishes, separated by commas. Set when the
-- session is created and never edited, for the reason vscode already carries: a
-- container keeps the port bindings it was created with.
ALTER TABLE sessions ADD COLUMN ports TEXT NOT NULL DEFAULT '';

-- Whether this session is a compose project rather than a single container.
-- Existing rows are 0: they were created from an image that had no compose file.
ALTER TABLE sessions ADD COLUMN compose INTEGER NOT NULL DEFAULT 0 CHECK (compose IN (0, 1));
