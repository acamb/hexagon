# M1.6 — Bitbucket, and a model that expects other providers

## What was asked

> Deve essere possibile aggiungere un account bitbucket in modo da attingere
> anche da li la lista dei repository. Modelliamo in modo da prevedere anche
> altri provider in futuro. L'account di bitbucket non esclude quello di github
> e deve essere possibile selezionare uno dei due quando si sceglie un repo in
> fase di creazione. Nel caso in cui il repository sia bitbucket bisogna avere
> un'alternativa rispetto al token di github che viene passato nel container.

## Reading of the request

The point is not "add Bitbucket": it is "stop assuming there is one provider".
Bitbucket is the first thing to come through the seam, and the last sentence
names the place where the assumption is currently hard-coded — the GitHub token
handed to the container.

Three decisions were settled with the user before starting:

- Bitbucket is **only a source of repositories**. Signing in stays GitHub OAuth
  with the allowlist, and the Bitbucket account is linked from inside the app by
  someone already signed in. The alternative — Bitbucket as a second way to log
  in — would mean a second allowlist and an identity that no longer coincides
  with a GitHub account, which is a much larger change to the most sensitive
  part of the system for something nobody asked for.
- It authenticates with a **pasted API token**, not an OAuth consumer. See
  below: OAuth tokens on Bitbucket expire every two hours, so that road brings
  refresh machinery and two more configuration keys with it. The table keeps
  columns for a refresh token and an expiry so the day that changes is a
  migration, not a redesign.
- The picker keeps **one merged list** with a provider filter above it, because
  typing part of a name is how anyone with more than ten repositories finds one,
  and making them pick an account first would put a step in front of that for
  everybody, including the people with a single account.

## What research settled, and what it changes

**Atlassian removed Bitbucket app passwords on 28 July 2026**, a month before
this was written: git over HTTPS with one now fails with `410`. So the only
credential worth implementing is an Atlassian **API token**, and it is
asymmetric in a way that would otherwise have been a bug found late:

| Use | Username | Password |
|---|---|---|
| REST API (`api.bitbucket.org/2.0`) | the Atlassian **account email** | the API token |
| git over HTTPS | the literal `x-bitbucket-api-token-auth` | the API token |

The pair the API wants is not the pair git wants. That single fact shapes the
interface: a provider has to answer **two different questions** about the same
stored secret, so `Provider` exposes `ListRepos` (which uses the API pair)
*and* `GitCredentials` (which returns the git pair) rather than handing out one
"token" the rest of the system passes around. GitHub answers the second question
with `x-access-token` plus the OAuth token, which is exactly what
`internal/gitops` hard-codes today — so the existing behaviour becomes one
implementation of the new interface instead of a special case.

The token needs `read:workspace:bitbucket` and `read:repository:bitbucket`, and
`write:repository:bitbucket` if Claude Code is to push; `read:user:bitbucket`
turned out to be optional. Listing walks one workspace at a time, paginated by a
`next` URL **in the body** rather than GitHub's `Link` header — see the second
amendment, which is where the shape of the listing was actually settled.

## Design

### The credential moves out of `users`

Today `users.github_token_enc` does three jobs at once: it is the proof of who
signed in, the key to listing repositories, and the secret handed to a
container. Only the first belongs to identity. A new `provider_accounts` table
holds one row per (user, provider) with the sealed secret, the `account` shown
in the UI and the `identity` the API wants as its basic-auth user — the email
for Bitbucket, unused for GitHub.

The migration copies the existing GitHub token into that table and then
**drops the column**. Keeping it would leave two places holding the same
credential with the login path writing to the one nothing reads, which is the
drift the conventions exist to prevent. Sessions gain a `provider` column
defaulting to `github`, so every session that already exists keeps cloning and
starting exactly as it did.

### The container gets a credential helper, not a new image

This is the part that could have been done badly. The reference image bakes in
an askpass that answers `x-access-token` and `$GITHUB_TOKEN`; teaching it about
Bitbucket would mean every user rebuilding every image before a Bitbucket
session could clone, and Hexagon cannot even tell whether the image someone
registered has the new script in it.

Instead the bootstrap — which Hexagon already runs inside every container, and
which already writes `safe.directory` — configures a global credential helper:

```sh
git config --global credential.helper '!f(){ echo "username=$HEXAGON_GIT_USERNAME"; echo "password=$HEXAGON_GIT_PASSWORD"; }; f'
```

It works with **any** image, needs no rebuild, takes precedence over the image's
`GIT_ASKPASS`, and keeps the rule the project already follows: the helper text
names environment variables, so no secret is written to a file. `GITHUB_TOKEN`
is still injected for GitHub sessions, because the reference image's askpass and
tools like `gh` expect it.

### Everything else follows the seam

`internal/provider` holds the neutral vocabulary — `Kind`, `Repo`,
`Credentials`, `Provider` — plus the repo cache generalised from
`internal/github/cache.go` to be keyed by user *and* provider, and the
aggregator that fans out over a user's connected accounts and merges the
listings. `internal/github` and the new `internal/bitbucket` implement the
interface, each decoding its own JSON through a private DTO. The HTTP layer
stops having a GitHub-shaped hole in it: `/api/github/repos` becomes
`/api/repos`, and `/api/accounts` connects, lists and forgets accounts.

Connecting verifies the credentials against the provider **before** storing
them, so a mistyped token fails at the form with the provider's own message
rather than silently producing an account that lists nothing.

## Impact

| Area | Change |
|---|---|
| Schema | `003`: `provider_accounts`, `sessions.provider`, `users.github_token_enc` copied then dropped |
| Go | new `internal/provider` and `internal/bitbucket`; `github` returns neutral repos; `auth.Credentials` replaces `GitHubToken`; `session` and `gitops` take a username as well as a secret |
| API | `GET /api/accounts`, `PUT/DELETE /api/accounts/{provider}`, `GET /api/repos` replacing `/api/github/repos`, `provider` on session create |
| Config | `HEXAGON_BITBUCKET_API_URL` / `bitbucket.apiUrl` |
| UI | new Accounts page; provider filter and tags in the session picker |

## Amendment: verifying against the right endpoint

Connecting a real account failed with "those credentials were rejected" on a
token whose scopes were right and which Bitbucket showed as having just been
used. Two mistakes, both mine:

- **Verification asked `GET /2.0/user`**, which carries a scope of its own. A
  token scoped for exactly what Hexagon does — list and clone repositories — is
  refused there. Verification now goes through
  `GET /2.0/repositories?role=member&pagelen=1`: the capability the session will
  actually need, so a token that passes the form works, and one that fails it
  would have failed later. The account endpoint is still asked, but only for a
  display name, and its refusal costs nothing: the account is then labelled with
  the email it was connected with.
- **The error threw away the evidence.** The handler replaced whatever Bitbucket
  said with "check the token and its scopes", a guess at the cause stated as a
  fact, and logged nothing at all for that path. The provider's own message now
  travels to the UI and to the log, and `internal/bitbucket` extracts it from
  the `{"error":{"message":…}}` document Bitbucket returns.

## Amendment 2: the listing endpoint no longer exists

The first fix did not survive contact either. Connecting then failed with

    410 Gone: CHANGE-2770 - Functionality has been deprecated

which is not about credentials at all: **Atlassian removed the cross-workspace
repository listing — `GET /2.0/repositories?role=member` — on 14 April 2026**,
and has said no equivalent is coming back. The endpoint this client was built on
had been gone for four months before it was written. Two searches had turned up
the app-password removal and the asymmetric credentials, and I stopped there
instead of checking the one call the whole feature rests on.

Repositories are now workspace-scoped, so a listing is two levels:

1. `GET /2.0/user/workspaces` for the workspaces the account belongs to;
2. `GET /2.0/repositories/{workspace}` for each of them, paginated as before.

Verification uses the first call, which is where a listing starts.

**A wrong turn in between, worth recording.** Probing the API unauthenticated —
a path that exists answers 401, one that does not answers 404 — I found
`/2.0/workspaces` and `/2.0/user/permissions/workspaces` both gone, and
concluded that no endpoint listed a user's workspaces at all, which led to a
design where the user typed their workspace names into the connect form. That
conclusion was drawn from a list of paths I had guessed at, and the list was
missing the one that works: `/2.0/user/workspaces`, which is in the reference
and answers 401. The probe was a good instrument used to prove a negative it
could not prove. The workspace-naming design, the column it needed and the form
field were all reverted.

## Verification

Unit tests per package, including a stub Bitbucket API covering `next`
pagination and the `https` clone link, and a migration test proving an existing
token lands in `provider_accounts` and existing sessions come out as `github`.
The one that matters most in `internal/httpapi`: a session created from a
Bitbucket repository clones as `x-bitbucket-api-token-auth` and starts a
container carrying the Bitbucket credentials and no `GITHUB_TOKEN`.

By hand, the two things no test can prove: the migration against a copy of the
real database, and a real Bitbucket session that can `git fetch` and push from
inside its own terminal.

## Amendment 3: a listing that succeeded and returned nothing

Reported after the feature shipped: the session picker showed only GitHub
repositories, with the Bitbucket account connected and no error anywhere. That
combination is diagnostic. A provider that fails lands in `Listing.Failed` and
the picker prints "Bitbucket could not be reached", so nothing had failed:
`ListRepos` returned an empty slice and a nil error.

**The cause was the shape of one document.** `GET /2.0/user/workspaces` does not
answer with workspaces, it answers with *memberships*:

```json
{"values":[{"type":"workspace_access","administrator":false,
            "workspace":{"type":"workspace_base","slug":"…"}}]}
```

The client read `slug` at the top level of each value, one level above where it
is. Every slug came out empty, every empty slug was skipped, the workspace list
was therefore empty, and — because repositories are only ever asked for inside a
workspace — no repository call was made at all. Nothing in that chain is an
error. It is the same class of mistake as the two amendments above, and it
survived a full test suite because the stub server was written from the same
wrong assumption as the code: both had the flat shape, so they agreed with each
other and not with Bitbucket. The fixtures now carry the payload the real API
returned, and the parser reads the nested slug with the flat one as a fallback.

Three further changes came out of the same trail, each of them a reason the bug
was invisible rather than a cause of it:

- **`Verify` accepted a token that sees no workspace.** It only checked that the
  call did not fail, so an account that can never list a repository connected
  happily and the failure moved to the picker, which had nothing to say about
  it. Connecting now refuses it and names the scope, `read:workspace:bitbucket`,
  that is the likely reason. This is what turned the silence into the message
  that led to the diagnosis.
- **One unreadable workspace hid all the others.** `ListRepos` returned on the
  first workspace error. Failures are now collected and the walk continues; the
  error is returned only when no workspace could be read at all.
- **The picker built its provider tabs from the repositories that came back**, so
  a provider contributing nothing was indistinguishable from one nobody had
  connected. The tabs now come from the connected accounts, an empty one says
  the provider returned no repositories, and the 100-row cap applies per
  provider rather than to a merged list sorted by date, where a long GitHub
  history could push an entire account past the end.

The per-workspace listing also dropped its `role=member` filter. That was not
the bug — the account that reported this reads its repositories either way — but
the filter asks for *explicit* per-repository membership on a URL that is already
scoped to one workspace, so it can only ever hide repositories the account
reaches through a workspace-level or group grant. There is nothing it usefully
excludes.

Two smaller things: `Lister.List` wrote `failed[kind]` for an unknown provider
outside the mutex while goroutines for earlier kinds wrote the same map under it
— a concurrent map write, now locked — and `handleListRepos` logs the count per
provider at `Debug`, so the next listing that comes back mysteriously short can
be answered from the log instead of from a bisect.
