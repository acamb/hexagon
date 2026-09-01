# M2 — analyses

The milestone statement is [../milestones/M2.md](../milestones/M2.md). This
folder holds one analysis per point, written before that point is implemented,
as described in [AGENTS.md](../../AGENTS.md).

| # | Point | Analysis | State |
|---|---|---|---|
| 1 | First time wizard | [01-first-time-wizard.md](01-first-time-wizard.md) | done |
| 2 | Server settings | [02-server-settings.md](02-server-settings.md) | done |
| 3 | Support for complex builds | [03-complex-builds.md](03-complex-builds.md) | done |
| 4 | Multiple Claude accounts | [04-multiple-claude-accounts.md](04-multiple-claude-accounts.md) | todo |

Points 1 and 2 are the same feature seen twice: the wizard configures a server
that cannot authenticate anybody yet, the settings page reconfigures one that
can. Point 1 is analysed first because it decides where the settings live and
how a running server picks up a change to them, which is the whole of point 2.

Point 2 covers the whole configuration file rather than the three settings its
statement lists, at the user's request. Two keys are shown and not editable —
the listen address and the secret key — for the reason given in the analysis: a
wrong value in either cannot be corrected from the page afterwards.

Point 3 is two features on one page: a second shape of image, where a compose
file describes the services beside the one the Dockerfile builds, and published
ports, which belong to the session rather than to the image because a port
binding is a property of a container. Its analysis has since been extended twice
at the user's request, and both additions are at the end of it: the interface a
port binds, and changing the ports of a session that is stopped.

Point 4 undoes a decision point 10 of M1 made deliberately: one Claude login per
user, because a second one would only raise the question of which one wins. Its
analysis answers that question — a name, a default, and a column on the session —
and it makes both kinds of credential plural, the pasted one as a row and the
subscription as a directory of its own, so that having two subscriptions does not
send anybody back to the host shell.
