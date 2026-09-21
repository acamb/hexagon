# Shallow clone of a session's repository

## What was asked

> voglio aggiungere la possibilita' di fare dei shallow clone dei repository
> quandi si crea una sessione: nella modal di creazione, quando viene selezionata
> una sessione, aggiungi una checkbox "shallow clone" e se spuntata appare un
> campo "depth" in cui l'utente specifica la depth del clone (valore di default
> 1). Lato server questo parametro fa si che venga fatto il shallow clone con la
> depth specificata.

A checkbox, a field that appears with it, and a `--depth` on the clone.

## Reading of the request

Cloning is the slow half of provisioning, and the only part whose cost is set by
the repository rather than by Hexagon: `provisionTimeout` is thirty minutes, and
its comment says why. Most of what a full clone fetches is history that a session
never reads — the agent works on a working tree and pushes commits on top of it.

So this is not a preference about git. It is the one control that makes creating
a session on a large repository bearable, and it belongs where the repository is
chosen.

The request says *quando viene selezionata una sessione*; a session is what is
being created, so what is meant is the repository. The checkbox is therefore
shown only when the dialog is creating a session **on a repository** — a session
started on an empty workspace never clones, and offering it a clone depth would
be a control that does nothing.

## Current behaviour

`gitops.Options` has no depth, and `gitops.Clone` builds one shape of command:

```go
args := []string{
	"-c", "credential.helper=",
	"-c", "credential.helper=" + credentialHelper,
	"clone",
}
if opts.Branch != "" {
	args = append(args, "--branch", opts.Branch)
}
args = append(args, "--", opts.CloneURL, opts.Dest)
```

Optional flags go between `clone` and the `--` separator. There is exactly one
today, and a depth is the second.

The one fact that decides the shape of everything upstream is in
`provisionSteps`:

```go
err := m.cloner.Clone(ctx, gitops.Options{
	CloneURL:  session.RepoCloneURL,
	Branch:    session.Branch,
	Dest:      session.RepoDir,
	...
})
```

**The clone reads from the stored row, not from the create request.** `Create`
returns as soon as the row exists and hands `provision` a `*store.Session` and
nothing else. A field that lives only on `CreateRequest` cannot reach the clone,
so a clone option is a column — there is no lighter option available.

## Design

### One number, not a flag and a number

`cloneDepth`, an integer, on the request, the row and the response. `0` — which
is also "absent" — is a full clone.

The dialog has two controls because the request asks for two, but they describe
one value, and carrying both across four layers would make `shallow = true,
depth = 0` representable at every one of them. It is not a state anything could
act on, so it should not exist below the form that produces it. The checkbox
decides whether a depth is sent; the depth is what is stored.

For the same reason the request field is a plain `int` rather than the `*bool`
the neighbouring flags use. They are pointers because their default is `true` and
their zero value is `false`, so absent and off have to be told apart. Here the
default *is* the zero value, and a pointer would buy a nil check and nothing
else.

### git's own branch behaviour is kept

`git clone --depth N` implies `--single-branch`: only the selected branch is
fetched, and the others do not exist in the clone. That was decided with the
user and kept, because it is what makes a shallow clone small — the alternative,
`--no-single-branch`, fetches every branch tip and gives back much of what was
being avoided on a repository with many branches.

What it costs is real: inside the session, `git checkout other-branch` fails
until the user widens the refspec. So the dialog says it, in the explanation
under the checkbox, rather than leaving it to be discovered from a container.
The `Depth` field's comment in `gitops` records the same thing, because that is
where a reader will be when they wonder.

### Refused before anything is provisioned

A negative depth is a 400 from `handleCreateSession`, next to the missing-image
check and for the same reason its comment gives: a session is a container, a
clone on disk and a workspace directory, and refusing after any of that exists
leaves the mess behind. The column's `CHECK` is a backstop against a future
caller, not the check.

There is no upper bound. A depth larger than the history is not an error to git —
it clones everything and stops — and a limit here would be a number invented in
Hexagon that git would not have objected to.

### Create only

No setter, no `PATCH`, nothing in the session page. This follows `vscode` and
`propagate_token` rather than `auto_claude`: the clone happens once, during
provisioning, and a field that could be changed afterwards would be a control
that silently does nothing. The value is stored and returned all the same, so a
workspace with a truncated history can be explained rather than guessed at.

Rejected: **`git fetch --unshallow` from the session page.** It is a different
feature — "deepen the clone I have" — it is a long-running operation that would
need its own status, and the user who wants it has a terminal in which to type
it.

## Impact

| Area | Change |
|---|---|
| Schema | `014_session_clone_depth.sql`: `sessions.clone_depth INTEGER NOT NULL DEFAULT 0 CHECK (clone_depth >= 0)` |
| Store | `Session.CloneDepth`, in `sessionColumns`, `CreateSession` and `scanSession`; no setter |
| Gitops | `Options.Depth`, and the `--depth` argument |
| Session | `CreateRequest.CloneDepth`, stored on the row, read back in `provisionSteps` |
| API | `createSessionRequest.cloneDepth`, refused when negative; `sessionResponse.cloneDepth` |
| Config | none |
| UI | A shallow clone checkbox in the new-session dialog, and a depth field that appears with it |

## Verification

`internal/gitops`, where the flag is: a clone with `Depth: 1` leaves one commit
and a `.git/shallow`, and one with `Depth: 0` still leaves the whole history.
The test clones through a `file://` URL rather than a path, because git ignores
`--depth` on a local clone and says so — without it the test would pass whatever
the code did.

`internal/httpapi/sessions_test.go`, over the real router, reading what reached
the fake cloner's `gitops.Options`: a request with a depth clones with it; a
request without one clones with `0`, which is the test that proves the ordinary
path did not move; a negative depth is a 400 and provisions nothing; and the
value comes back on the response and on a later `GET`.

`internal/store`, the round trip through `CreateSession` and `SessionByID` —
which is also what catches a `sessionColumns` and a `scanSession` that have
drifted out of order.

By hand: a session on a large repository with the box ticked, which should be
visibly faster to provision, and a `git log` inside it that stops where it was
told to.
