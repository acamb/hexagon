-- Whether a session's container gets the credentials of the account its
-- repository came from. The clone on the host always uses them -- it could not
-- reach a private repository otherwise -- so this is only about what runs
-- inside the container.
--
-- Existing sessions default to on because that is what their containers
-- already carry: the environment is fixed when a container is created, and any
-- other value here would be a lie about what is inside them.
ALTER TABLE sessions ADD COLUMN propagate_token INTEGER NOT NULL DEFAULT 1
    CHECK (propagate_token IN (0, 1));
