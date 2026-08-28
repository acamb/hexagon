# M1.8 — Sessions without a repository

## What was asked

> Deve essere possibile creare una sessione senza per forza agganciare un
> repository. Anche se viene scelto di non passare un repository deve essere
> possibile scegliere se passare il token di github/bitbucket/altro.

Two sentences, and the second one is the interesting half: dropping the
repository must not drop the credentials with it.

## Reading of the request

Until now a session was a repository. Everything else — which account it came
from, which credential the container got, what the session was called — was
derived from that one choice. The point separates it into two choices that
happen to have been made together:

- **what is in `/workspace`**: a clone, or an empty directory;
- **which account the session carries**, if any.

The second sentence says so explicitly: a session with no repository may still
want a token, because an agent with a token can clone something itself, open a
pull request, or use `gh`. So the provider stops meaning "where the repository
came from" and starts meaning "the account this session is attached to" — which
for a session with a repository is still the same account, because it is the one
the clone came out of.

That is the whole change. Everything below follows from not adding a second way
to say the same thing.

## Current behaviour

`handleCreateSession` refuses a request without a repository:

```go
if strings.TrimSpace(req.RepoFullName) == "" || strings.TrimSpace(req.ImageID) == "" {
    writeError(w, http.StatusBadRequest, "a repository and an image are required")
```

and everything after that assumes one: `resolveRepo` looks the name up in the
caller's accounts to get a clone URL that came from the provider rather than
from the browser, the title falls back to the repository's full name, `Create`
reads the credentials of the repository's provider, and `provisionSteps` clones
before it creates the container. The clone is what fills `RepoDir`, the
directory bind mounted at `/workspace`.

Point 7 left one thing pointing the wrong way for this: the bootstrap installs
the credential helper when `session.PropagateToken && session.RepoCloneURL != ""`.
The second half was a fair approximation while every session with credentials
had a repository. It is exactly the assumption this point removes.

## Design

### No new column

A session without a repository is one with `repo_full_name`, `repo_clone_url`
and `branch` empty, and a session with no credentials is one with `provider`
empty. Both columns already exist, both are `TEXT NOT NULL` with no `CHECK`
constraint on them, and empty already means "nothing here" everywhere else in
this schema. There is no migration in this point.

Rejected: **a `has_repo` flag**, which would be a second source of truth for
something `repo_clone_url` already answers, and the two would eventually
disagree.

The invariant that comes out of it is worth stating, because the handler
enforces it: `provider` is non-empty exactly when the session is attached to an
account, and `propagate_token` is the answer to "does the container get that
account's credentials". A session with no repository and no token has neither,
so the provider is stored empty rather than recording an account the session
does not use.

### One directory, whether or not anything was cloned

`RepoDir` stays `<workspace>/repo` and stays the thing mounted at `/workspace`.
When there is no repository, `provisionSteps` creates it empty (0700, like every
other directory Hexagon makes) instead of cloning into it, and skips the
`cloning` status.

`git config --global --add safe.directory /workspace` still runs. It costs
nothing on an empty directory and it is what makes a repository cloned inside
the session by hand — the obvious thing to do in a session that started without
one — usable rather than refused for mismatched ownership.

Rejected: **renaming the column to `work_dir`**. It is the honest name now that
the directory is not always a clone, but the rename is a migration, a store
field, a wire field the SPA reads, and every test that spells out the path, in
exchange for a label. The column means "the directory mounted at `/workspace`",
and it is named after the case that usually fills it.

### The provider is the token choice

With no repository, the `provider` field of the create request is no longer
about the repository: it names the account whose credentials the session gets,
and empty means none. So:

- `provider` empty, no repository → no account, `propagateToken` stored as
  false whatever the request said, because there is nothing to propagate;
- `provider` set, no repository → it must be a provider this build knows and an
  account the caller has actually connected, or the request is a 400. Nothing
  else in the create path would have checked, and the alternative is a session
  that fails to provision with a message about reading a stored credential;
- `propagateToken` keeps its default from point 7 — absent means yes. Naming an
  account for a session with no repository has no other purpose, so defaulting
  it to no would make the field say one thing and do another.

With a repository nothing changes at all: the provider is still resolved from
the repository listing, which is what keeps the clone URL trustworthy.

Rejected: **letting a session take its token from an account other than the one
its repository came from**. Nobody asked for it, it doubles the states the
create request can be in, and the clone would still need the repository's own
account — so the session would carry two credentials and have to explain which
is which.

### The bootstrap condition, corrected

`session.PropagateToken && session.RepoCloneURL != ""` becomes
`session.PropagateToken && session.Provider != ""`. The bootstrap does not have
the credentials in hand — it runs again on every start — so it decides from the
session row whether the environment it is running in has them, and after this
point the row's answer to that is the provider, not the clone URL.

### The title

`title` falls back to the repository's full name, which a session without one
does not have. It falls back to the image name instead: it is the only other
thing that says anything about the session, and a fallback that cannot be empty
is worth more than one that is precise. The user can still type a title, and for
this kind of session they probably will.

### What the UI has to say

The dialog gets a choice before everything else — a repository, or none — rather
than an empty state that has to be discovered. Choosing none hides the
repository list, the branch and the point 7 checkbox, and shows one select in
their place: **No token**, or one of the connected accounts.

That select defaults to **No token**, which is the opposite of point 7's default
and deliberately so. A session with a repository is a session that is expected to
push, so it gets the token unless told otherwise; a session with no repository
has no operation that needs a credential until its user thinks of one, and the
choice is right there in front of them when they do.

The session page and the session list then have to stop assuming a repository:
no name, no branch tag, no provider tag, and the copy about the clone on disk
becomes copy about the workspace.

## Impact

| Area | Change |
|---|---|
| Schema | none |
| Store | none |
| Session | `CreateRequest` may carry an empty repository and an empty provider; `Create` reads credentials only for a provider; `provisionSteps` creates `RepoDir` instead of cloning when there is no clone URL, and the title falls back to the image name; `bootstrap` keys the credential helper off the provider |
| API | `repoFullName` becomes optional on `createSessionRequest`; with no repository, `provider` selects the account and is validated against the caller's connected accounts; `propagateToken` is forced off when there is no account |
| Config | none |
| UI | The new-session dialog gains the repository/no-repository choice and the account select; the session page and the session list stop assuming a repository |

## Verification

In `internal/httpapi/sessions_test.go`:

- a session created with only an image reaches `running`, with nothing cloned,
  an empty `/workspace` directory on disk, a container with no credentials in
  its environment and a bootstrap with no credential helper;
- the same session with `provider: "bitbucket"` carries that account's
  credentials — the second sentence of the point, which is the part that would
  be easy to get wrong — and still clones nothing;
- naming an account that is not connected, or one this build does not know, is a
  400 and creates no session;
- the title of a repository-less session falls back to the image name;
- the existing tests for sessions with a repository keep passing unchanged,
  which is what says the two paths did not get tangled.

By hand: a session with no repository and no token, in which `/workspace` is
empty and `git clone` of a private repository is refused; the same with the
GitHub account chosen, in which the clone succeeds.
