# M1.7 — Switch to propagate the provider token

## What was asked

> In fase di creazione della sessione deve essere possibile decidere se passare
> o meno il token.

A per-session choice, made when the session is created: does the container get
the credentials of the account the repository came from, or not?

## Reading of the request

The token is handed over for two different reasons, and only one of them is
what this switch is about.

Hexagon **clones on the host**. That clone is Hexagon's own act, on behalf of a
user who picked the repository out of their own listing, and it needs the
credential to reach a private repository at all. A switch that turned it off
would not be a security control, it would be "create a session that fails".

Hexagon then **puts the credential inside the container**, where it belongs to
whatever runs in that container — Claude Code, and anything Claude Code decides
to run. That is the thing worth being able to refuse: a session for reading a
repository, or for letting an agent loose in it, without also handing it the
ability to push to every other repository the token can reach. So the switch
governs the container, never the clone.

The point says *in fase di creazione*, and that is also the only place it can
honestly go — see below.

## Current behaviour

`containerSpec` puts the credentials in the container's environment whenever
there are any:

```go
env = append(env, gitUserEnv+"="+credentials.Username, gitSecretEnv+"="+credentials.Secret)
if session.Provider == string(provider.GitHub) {
    env = append(env, "GITHUB_TOKEN="+credentials.Secret)
}
```

and the bootstrap installs a credential helper that reads those two variables,
so git inside the container authenticates without being asked. There is no way
to say no: every session that has a repository has the token.

Two facts about the surrounding code decide how clean the switch can be.

**The clone leaves nothing behind.** `internal/gitops` passes the secret through
the environment to a helper that lives for the duration of the command, and
never writes it into the remote URL — the comment on `tokenEnvVar` says so, and
that is exactly why the switch works. The workspace bind mounted at
`/workspace` contains a `.git/config` with a plain `https://` remote and no
credential in it. Without the environment variables, a container genuinely has
no way to authenticate.

**The environment is fixed when the container is created.** Docker has no way to
change the environment of a container that already exists, and the credentials
are part of `containerSpec`. That is what makes this different from
`autoClaude`, which the session page can flip because the bootstrap re-reads it
on every start.

## Design

### A creation-time flag, and honestly so

`sessions.propagate_token`, set at creation, read by `containerSpec` and by the
bootstrap. There is no `PATCH` for it, because the only truthful way to change
it is to destroy the container and build another one, and that is a lifecycle
operation nobody asked for. The session page shows the choice as a fact about
the session — next to the provider it came from — rather than as a control that
would quietly do nothing.

Rejected: **delivering the credentials through the bootstrap exec instead of the
container environment**, which would make the flag flippable at every start
(the tmux server inherits the exec's environment, so the shells would see them).
It is a real option and it would even keep the token out of `docker inspect`,
but it moves where the secret lives for every session in order to make one flag
editable, and it breaks down anyway when the tmux server is already running. If
the day comes that sessions need to gain or lose credentials while they exist,
that is the design to revisit — as its own point, with the recreation of the
container made explicit.

### Default on

Absent from the request body it is on, and `createSessionRequest` takes a
`*bool` so a client that has never heard of the field keeps today's behaviour.
Hexagon is for running an agent that commits and pushes; the switch is for the
session where that is not wanted. The migration defaults existing rows to on for
the same reason — and, more to the point, because their containers already have
the environment, so any other value would be a lie about what is inside them.

### What "off" actually costs

Nothing else changes: the clone happens, the branch is checked out, the session
runs. Inside the container, `git fetch` and `git push` fail to authenticate,
`gh` is not signed in, and Claude Code cannot open a pull request. The
credential helper is not installed either — with no variables to read it would
answer with an empty username and password, which turns a clear "no credentials"
into a confusing rejection.

One rough edge is worth recording rather than engineering around: the reference
image sets `GIT_ASKPASS` to a script that echoes `$GITHUB_TOKEN`, and
`GIT_ASKPASS` takes precedence over anything git config can say. In a session
without the token, git therefore tries an empty password and is refused instead
of asking the user to type something. Anyone who wants to authenticate by hand
in such a session can prefix a command with `GIT_ASKPASS= `. Changing the image
would mean every user rebuilding theirs, which is the trade the credential
helper was introduced to avoid.

### The bootstrap condition

`bootstrap` runs at provisioning and at every start, and does not have the
credentials in hand — it decides from the session row alone whether to install
the helper. That approximation becomes `session.PropagateToken &&
session.RepoCloneURL != ""`: a session with no repository never had credentials
in its environment, and one created with the switch off never will.

## Impact

| Area | Change |
|---|---|
| Schema | `004_session_propagate_token.sql`: `sessions.propagate_token INTEGER NOT NULL DEFAULT 1 CHECK (propagate_token IN (0, 1))` |
| Store | `Session.PropagateToken`, in `sessionColumns`, `CreateSession` and `scanSession` |
| Session | `CreateRequest.PropagateToken`; `containerSpec` gates the credential environment on it; `bootstrap` gates the helper on it |
| API | `propagateToken` on `createSessionRequest` as a `*bool`, and on `sessionResponse`. No `PATCH`: the container carries the environment it was created with |
| Config | none |
| UI | A second checkbox in the new-session dialog, and a tag on the session page for a session that runs without the token |

## Verification

In `internal/httpapi/sessions_test.go`, which already reads back the environment
the fake Docker was asked for:

- the default still creates a container carrying `HEXAGON_GIT_USERNAME`,
  `HEXAGON_GIT_PASSWORD` and `GITHUB_TOKEN`, and a bootstrap that installs the
  helper — the test that proves the switch did not change the normal path;
- `propagateToken: false` creates a container with none of the three, and a
  bootstrap with no `credential.helper` in it, while the clone still receives
  the credentials — the two halves of the reading above, asserted together;
- the flag survives a round trip through `GET /api/sessions/{id}`.

In `internal/store`, the migration test gains a row proving existing sessions
come out with the token propagated, matching the containers they already have.

By hand: a session created with the box unticked, in which `git push` is refused
and `env | grep -i token` comes back empty.
