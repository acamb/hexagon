# A GitHub personal access token for git

## What was asked

> Il token github che passo alle sessioni dopo un tot di ore scade e mi ritrovo a
> non poter fare push/pull nei container, vorrei un nuovo campo su accounts dentro
> al riquadro di github in cui impostare un Personal Access Token. Nella schermata
> di creazione delle sessioni se il Personal Access Token non e' impostato deve
> apparire un warning che avverte che se non lo imposto allo scadere del token
> OAuth non saro' in grado di fare push/pull a meno di ricreare il container con
> un link che porta alla pagina settings. Quando invece e' impostato il Personal
> Access Token va passato quello.

Three things: a field, a warning, and a rule about which of two secrets wins.

## Reading of the request

The report is a bug in the shape of a feature request. A session container is
given the OAuth token that came from signing in, and that token has a lifetime
measured in hours. When it dies, the session does not: it goes on running, with
an environment that authenticates nothing. Every `git push` from inside it —
including every one Claude Code makes on the user's behalf — is refused, and
there is no way to hand the container a new value.

So the field is not a preference. It is the only way to give a session a
credential that outlives it.

The last sentence, *va passato quello*, was read with the user before this was
written: the personal access token replaces the OAuth token **for everything git**
— the clone on the host as well as the container's environment — and for nothing
else. Listing repositories in the session picker goes on using the OAuth token.
That division is the reason a narrow PAT is safe to paste: its scopes decide what
a session can push to, never what the user can see in the picker. The rejected
alternative is at the end of the design.

The link in the warning goes to `/accounts`, where the field is. The request says
*pagina settings*, but `/settings` is the server configuration page — the port,
the client id and secret, the allowlist — and sending someone there to look for a
field that is not on it would be worse than taking the word literally.

## Current behaviour

The GitHub credential has one source and one shape. `SaveLogin` writes it at every
sign-in:

```go
_, err = s.Connect(ctx, user, provider.Account{
	Kind:      provider.GitHub,
	Account:   ghUser.Login,
	AvatarURL: ghUser.AvatarURL,
}, token)
```

and `Connect` seals it into `provider_accounts.secret_enc`, one row per user and
provider. `GitCredentialSource.GitCredentials` unseals it and asks the provider
what git wants for it; for GitHub that is `x-access-token` plus the token itself.
The answer is used twice: by `gitops.Clone` on the host, and by `containerSpec`,
which puts it in the container's environment as `HEXAGON_GIT_PASSWORD` — plus
`GITHUB_TOKEN`, which the reference image's askpass and `gh` read under that name.

Two facts decide how much this can be repaired and how much it cannot.

**A container keeps the environment it was created with.** This is the same wall
M1.7 ran into. Docker cannot change it, so the token in a live container is the
token it was built with, forever.

**`Start` does not rebuild.** `rebuildSpec` re-reads the sealed credentials, and
its comment says why — "a rebuilt container carries whatever the user's accounts
hold now" — but only `SetPorts` and `SetClaudeAccount` call it. Stopping and
starting a session restarts the container it already has. So the user's own
summary of the situation, *a meno di ricreare il container*, is exactly right.

That is worth stating plainly, because it bounds what this change fixes: it fixes
every session created from now on, and no session that already exists.

## Design

### One more secret, at the seam that already exists

`internal/provider` was built around the observation that one stored secret has to
answer two different questions — what the REST API wants, and what git wants over
HTTPS — because for Bitbucket the answers differ. This point is the same
observation one step further: for GitHub the two questions can now have two
different *secrets*, not just two different usernames.

So `provider.Credentials` gains a `GitSecret`, and `GitCredentials` prefers it:

```go
func (c *Client) GitCredentials(cred provider.Credentials) provider.GitAuth {
	return provider.GitAuth{Username: gitUsername, Secret: cred.SecretForGit()}
}
```

Everything downstream is untouched. `auth.GitCredentialSource` already returns a
`GitAuth` and nothing else; `session.Manager.Create`, `rebuildSpec`,
`containerSpec` and `gitops.Clone` never see a `Credentials`. One branch, at the
one place whose whole job is to answer "what does git want", and the clone and the
container both get the new answer because they already share that answer.

### A second column, not a replacement

`provider_accounts.git_secret_enc`, nullable, sealed by the same cipher.

It is a second column rather than an overwrite of `secret_enc` because the two
secrets have different lifecycles: `secret_enc` is rewritten by every sign-in, and
a PAT that lived there would be destroyed by the next login — which is exactly the
moment the user is least likely to notice. `UpsertProviderAccount` therefore
leaves `git_secret_enc` out of its `DO UPDATE SET`, and carries a comment saying
so, because that omission looks like a bug to anyone who has not read this.

The column is provider-neutral, and both providers honour it, because
`provider_accounts` is a provider-neutral table and a column named for GitHub in
it would be a lie. Only the GitHub box on the Accounts page offers the field,
though: GitHub's sign-in token is the only credential that expires under the user.
The one Bitbucket is connected with *is* a long-lived pasted token already, so
offering to replace it with a second long-lived pasted token would be a question
with no meaning. If that ever changes it is a `v-if`, not a migration.

### Verified, and verified to be the same account

Connecting a Bitbucket account checks the credentials against the provider before
storing them, "so a mistyped token fails at the form with the provider's own
message rather than silently producing an account that lists nothing". The same
reasoning applies with more force here, because a PAT that does not work fails
much later and much further away — inside a container, in a push nobody is
watching.

There is a second check this one needs and connecting does not. A PAT identifies
an account of its own, and it does not have to be the account the user signed in
as. Storing one that belongs to someone else would produce a session that pushes
commits attributed to a different GitHub user, which is a mess to unpick and
impossible to guess at from the symptom. So the handler compares the login behind
the token with the connected one and refuses a mismatch, naming both.

### Its own route

`PUT` and `DELETE /api/accounts/{provider}/git-token`, rather than an extra field
on `PUT /api/accounts/{provider}`. That endpoint refuses GitHub outright — "the
GitHub account comes from signing in" — and that rule is still true: this is not a
way to connect a GitHub account, it is a second secret attached to one that is
already connected. Relaxing the 409 to let one field through would have made the
rule conditional and the handler two handlers in a trench coat.

The account listing reports `gitTokenSet`, a boolean. The value never comes back
out, in the shape the settings page already uses for the client secret.

The repository cache is deliberately **not** invalidated: the listing runs on the
OAuth token, so nothing it holds went stale.

### Rejected: the PAT replacing the OAuth token everywhere

The tidier-looking option — one credential, used for the API and for git — was
rejected with the user. It makes the PAT's scopes decide what appears in the
session picker, so a token scoped to push to two repositories empties the list of
everything else, and the failure looks like "my repositories are gone" rather than
"my token is narrow". Keeping the listing on the OAuth token means a PAT can be as
narrow as its owner likes.

### Rejected: delivering credentials through the bootstrap

M1.7 recorded this option and the reason it was not taken, and it comes up again
here because it would make a stale token fixable in place: the bootstrap runs at
every start, the tmux server inherits its environment, so a restart could carry a
new secret. It is still the wrong trade for this point — it moves where the secret
lives for every session, and it does nothing for a session whose tmux server is
already running. A long-lived credential removes the need for it rather than
arguing with it. If sessions ever have to gain or lose credentials while they
exist, that is its own point, with the container recreation made explicit.

## Impact

| Area | Change |
|---|---|
| Schema | `010_provider_git_secret.sql`: `provider_accounts.git_secret_enc BLOB` |
| Store | `ProviderAccount.GitSecretEnc`; `SetProviderGitSecret`; `UpsertProviderAccount` preserves it |
| Provider | `Credentials.GitSecret` and `Credentials.SecretForGit()`; both providers prefer it |
| Auth | `Service.credentials` unseals it; `SetGitSecret` / `ClearGitSecret` |
| API | `PUT`/`DELETE /api/accounts/{provider}/git-token`; `gitTokenSet` on the account response |
| Session | none — the credential arrives already chosen |
| Config | none |
| UI | A personal access token control in the GitHub box; a warning in the new-session dialog |

## Verification

Unit tests where the rule lives: `GitCredentials` prefers the git secret and falls
back to the account's own, in both providers. In `internal/store`, the round trip,
the clearing, and the one that matters — a fresh `UpsertProviderAccount`, which is
what signing in again does, leaves the git secret alone.

In `internal/httpapi`, over the real router: the token is verified before it is
stored; one belonging to another login is refused; `gitTokenSet` comes back from
`GET /api/accounts`; and, in `sessions_test.go`, a session created while a PAT is
set clones with it and starts a container carrying it as `HEXAGON_GIT_PASSWORD`
and `GITHUB_TOKEN` — with the same test without a PAT proving the ordinary path
did not move.

By hand, the thing no test covers: a session created with a PAT, pushed from the
day after.
