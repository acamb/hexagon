-- Whether a session starts Claude Code inside tmux or leaves a bare shell.
-- Existing sessions default to on, like new ones: running Claude Code is what
-- a session is for, and the switch is there for the times it is not.
ALTER TABLE sessions ADD COLUMN auto_claude INTEGER NOT NULL DEFAULT 1
    CHECK (auto_claude IN (0, 1));
