-- Whether this session was created with the VS Code integration: its container
-- publishes code-server and has the release bind mounted. Existing rows are 0
-- because their containers have neither, and a container keeps the mounts and
-- the port bindings it was created with.
ALTER TABLE sessions ADD COLUMN vscode INTEGER NOT NULL DEFAULT 0 CHECK (vscode IN (0, 1));
