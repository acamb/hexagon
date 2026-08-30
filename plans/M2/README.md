# M2 — analyses

The milestone statement is [../milestones/M2.md](../milestones/M2.md). This
folder holds one analysis per point, written before that point is implemented,
as described in [AGENTS.md](../../AGENTS.md).

| # | Point | Analysis | State |
|---|---|---|---|
| 1 | First time wizard | [01-first-time-wizard.md](01-first-time-wizard.md) | done |
| 2 | Server settings | [02-server-settings.md](02-server-settings.md) | done |
| 3 | Support for complex builds | | todo |
| 4 | Multiple Claude accounts | | todo |

Points 1 and 2 are the same feature seen twice: the wizard configures a server
that cannot authenticate anybody yet, the settings page reconfigures one that
can. Point 1 is analysed first because it decides where the settings live and
how a running server picks up a change to them, which is the whole of point 2.

Point 2 covers the whole configuration file rather than the three settings its
statement lists, at the user's request. Two keys are shown and not editable —
the listen address and the secret key — for the reason given in the analysis: a
wrong value in either cannot be corrected from the page afterwards.
