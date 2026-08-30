# M2 — analyses

The milestone statement is [../milestones/M2.md](../milestones/M2.md). This
folder holds one analysis per point, written before that point is implemented,
as described in [AGENTS.md](../../AGENTS.md).

| # | Point | Analysis | State |
|---|---|---|---|
| 1 | First time wizard | [01-first-time-wizard.md](01-first-time-wizard.md) | done |
| 2 | Server settings | | todo |
| 3 | Support for complex builds | | todo |
| 4 | Multiple Claude accounts | | todo |

Points 1 and 2 are the same feature seen twice: the wizard configures a server
that cannot authenticate anybody yet, the settings page reconfigures one that
can. Point 1 is analysed first because it decides where the settings live and
how a running server picks up a change to them, which is the whole of point 2.
