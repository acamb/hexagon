# M1 — analyses

The milestone statement is [../milestones/M1.md](../milestones/M1.md). This
folder holds one analysis per point, written before that point is implemented,
as described in [AGENTS.md](../../AGENTS.md).

| # | Point | Analysis | State |
|---|---|---|---|
| 1 | Agents.md | [01-agents-md.md](01-agents-md.md) | done |
| 2 | Configuration file | [02-configuration-file.md](02-configuration-file.md) | done |
| 3 | Switch to launch claude automatically | [03-auto-claude-switch.md](03-auto-claude-switch.md) | done |
| 4 | tmux cheatsheet | [04-tmux-cheatsheet.md](04-tmux-cheatsheet.md) | done |
| 5 | Dockerfile editor when creating an image | [05-dockerfile-editor.md](05-dockerfile-editor.md) | done |
| 6 | Additional repository providers | [06-additional-providers.md](06-additional-providers.md) | done |
| 7 | Switch to propagate the provider token | [07-token-propagation-switch.md](07-token-propagation-switch.md) | done |
| 8 | Sessions without a repository | [08-sessions-without-repo.md](08-sessions-without-repo.md) | done |
| 9 | VS Code integration | [09-vscode-integration.md](09-vscode-integration.md) | done |
| 10 | Claude login from the UI | [10-claude-login-from-ui.md](10-claude-login-from-ui.md) | done |

Points 6 to 8 overlap: how a repository is chosen, which provider it comes from,
and whether a session has a repository at all are the same part of the create
flow. Their analyses are worth reading together, and point 6 should be analysed
first because it decides the data model the other two build on.
