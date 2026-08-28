# M1.3 — Switch to launch Claude Code automatically

## What was asked

> Alla creazione di una sessione e dentro una sessione deve essere possibile
> impostare se lanciare claude in automatico dentro tmux o solo la shell.

A per-session choice — start `claude` inside tmux, or leave a bare shell —
settable both in the create dialog and from the session page afterwards.

## Current behaviour

The bootstrap that runs after the container starts creates the tmux session with
no command:

```sh
tmux has-session -t main 2>/dev/null || tmux new-session -d -s main -c /workspace
```

so tmux runs the default shell, and `claude` is something the user types. The
browser then attaches with `tmux new-session -A -D -s main`, which finds that
session and joins it. Because `bootstrap` runs on provisioning *and* on every
`Start`, a stopped session that is started again gets a brand new tmux — the
scrollback is gone either way, which is already documented.

That is the hook this point hangs on: **whatever the tmux session is created
with is decided at bootstrap**, and bootstrap happens once per container start.

## Design

### The flag is a property of the session

A new `auto_claude` column on `sessions`, read by the bootstrap. Not a global
setting in the configuration file: the whole point is that one session drives
Claude Code while another is a shell for poking at the same repository.

New sessions default to on. Hexagon exists to run Claude Code sessions, so the
default is the thing it is for; the switch is there for the session where you
want to look around first. Existing rows take the same default in the migration:
their next start launches Claude Code, which is the behaviour the product
promises, and turning it off is one click.

### What "on" runs

```sh
tmux new-session -d -s main -c /workspace 'claude; exec "${SHELL:-sh}"'
```

The command is given to `tmux new-session` rather than typed into a shell with
`send-keys`. `send-keys` would race the shell's startup and leave the command in
the scrollback as if the user had typed it; making it the session's command is
what tmux is for.

**The shell fallback is not decoration.** If the command a tmux session was
created with exits, the window closes, and with the last window the session
itself is gone: a `claude` that is missing from the image, or that exits because
its credentials expired, would take the terminal with it and leave the user
staring at a socket that closed for no visible reason. `exec "${SHELL:-sh}"`
turns that into "Claude Code exited, here is a prompt". `sh` rather than `bash`
as the fallback because the base image is the reference, not a guarantee.

### When a change takes effect

At the next container start. There is no way around it — the tmux session that
exists was created with, or without, a command — so the UI says so instead of
pretending: the session page labels the switch with when it applies, and stopping
and starting the session is what makes it happen.

Rejected: **launching Claude Code into the running tmux when the switch is
flipped on.** It is a different feature ("run something in my session now"), it
needs its own endpoint injecting a command into someone's terminal, and the user
who wants it has a terminal right there in which to type `claude`.

### API

`PATCH /api/sessions/{id}` with `{"autoClaude": bool}`, returning the updated
session. A new verb rather than another `POST /api/sessions/{id}/<action>`: the
existing ones are lifecycle transitions that do something to a container, while
this changes a stored field, and PATCH leaves room for the settings the later
points of M1 will add to the same row.

On create, `autoClaude` is `*bool`: absent means the default, which is on, so an
API client that has never heard of the field gets the behaviour Hexagon is for
rather than a bare shell.

### One cleanup that comes with it

The tmux session name is currently the string `"main"` in two places: the
bootstrap script in `internal/session`, and `tmuxSessionName` in
`internal/httpapi`. This change edits the first one, so the name becomes
`session.TmuxSession` and the terminal handler refers to it. If those two ever
drift the terminal silently attaches to an empty session instead of the one that
was set up.

## Impact

| Area | Change |
|---|---|
| Schema | `002_session_auto_claude.sql`: `sessions.auto_claude INTEGER NOT NULL DEFAULT 1` |
| Store | `Session.AutoClaude`, in `sessionColumns`, `CreateSession` and `scanSession`; new `SetSessionAutoClaude` |
| Session | `CreateRequest.AutoClaude`; `bootstrap` takes the session; `bootstrapScript(autoClaude)`; exported `TmuxSession` |
| API | `sessionResponse.autoClaude`; `createSessionRequest.autoClaude`; `PATCH /api/sessions/{id}` |
| UI | A checkbox in the new-session dialog, and a switch in the session page header with a note about when it applies |
| Config | none |

## Verification

`internal/httpapi/sessions_test.go`, which already inspects the commands the fake
Docker was asked to run:

- a session created with the default bootstraps a tmux that runs `claude`;
- a session created with `autoClaude: false` bootstraps one that does not;
- `PATCH` flips the flag, the response and a later `GET` both show it, and the
  *next* `start` bootstraps accordingly — the test that proves the switch is
  wired to the thing it claims to control;
- `PATCH` on someone else's session is a 404, like every other session endpoint.

By hand: create a session with the box ticked and confirm Claude Code is already
running when the terminal opens; type `/exit` and confirm a shell prompt appears
instead of the terminal dying.
