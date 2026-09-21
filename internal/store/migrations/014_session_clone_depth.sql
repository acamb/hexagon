-- How deep the session's clone was made: 0 is the whole history, which is what
-- every session that exists today has. There is nothing to backfill, because
-- the clone has already happened and a row cannot change what is on disk.
--
-- Nothing reads this after provisioning. It is stored so a workspace whose
-- history stops abruptly can be explained rather than guessed at.
ALTER TABLE sessions ADD COLUMN clone_depth INTEGER NOT NULL DEFAULT 0
    CHECK (clone_depth >= 0);
