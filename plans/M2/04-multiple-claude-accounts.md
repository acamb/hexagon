# M2.4 — Multiple Claude accounts

## What was asked

> Deve essere possibile avere piu' account di claude configurati e se c'e' n'e'
> piu' di uno deve essere possibile sceglierlo in fase di creazione o allo start
> di una sessione dopo che e' stata messa in stop.

## Reading of the request

M1.10 gave Hexagon a Claude login that can be arranged from the browser, and it
gave it exactly one. The table it created has `user_id` as its primary key, and
the migration says why in as many words: "a second credential for the same person
would only raise the question of which one wins". This point is that question,
asked deliberately.

So the work is not a second field. It is **making the Claude login a listable
thing with a name**, and then letting a session say which one it runs as.

Three consequences follow from taking the statement literally, and each was
settled with the user before this was written.

**An account is either kind.** M1.10 established that there are two sorts of
credential and that they are not the same object: a string you can paste, and a
subscription, which is not a string at all but a file `claude auth login` writes.
Making only the pasted one plural would leave anybody with two subscriptions back
where they started — on the host, moving `~/.claude/.credentials.json` around by
hand — which is the thing M1.10 exists to remove. So both kinds are accounts.

**One account is the default.** "Choose it when there is more than one" says
nothing about what happens when nobody chooses, and there are three callers who
never will: every session that exists today, an API client that does not know the
field, and the **Ask Claude** button on the Images page, which authenticates as
the user and has no session to take the answer from. A default is one column, and
it is what stops those three from silently falling off the accounts the user
configured.

**Choosing at start is choosing while stopped.** The statement puts the second
choice "allo start", and mechanically that is what it has to mean: a container
carries the environment and the bind mounts it was created with, so the account
can only change by building another container, which is only safe while the
session is down. The control is therefore a button beside **Ports** on a stopped
session, and Start goes on being Start — the same shape, and the same reasoning,
as changing the published ports of a stopped session in M2.3.

## Current behaviour

`internal/store/migrations/006_claude_credentials.sql` — one row per user,
`user_id` as the primary key, `kind` checked against `('api_key', 'oauth_token')`,
the secret sealed. `internal/store/claude_credentials.go` is `Upsert`, one getter
and a delete: there is nothing to list because there is never more than one.

`internal/session/manager.go` — `containerSpec`, "the whole contract between
Hexagon and a session container", hands the credential over two ways:

```go
switch {
case claude.Secret != "":
	env = append(env, claude.Env()...)
case m.cfg.AnthropicAPIKey != "":
	env = append(env, "ANTHROPIC_API_KEY="+m.cfg.AnthropicAPIKey)
}
```

and, unconditionally, the mount:

```go
// Read-only: the container gets to use the credentials, not to change them.
if path := m.cfg.ClaudeCredentials; path != "" {
	if _, err := os.Stat(path); err == nil {
		binds = append(binds, path+":"+dockerx.AgentHome+"/.claude/.credentials.json:ro")
	}
}
```

Both are fixed when the container is created. `rebuildSpec` re-reads the
credentials rather than remembering them, and its comment already states the rule
this point extends: "a rebuilt container carries whatever the user's accounts hold
now, not what they held when the session was created".

`internal/session/claude_login.go` — the browser login runs `claude auth login` in
a throwaway container with `filepath.Dir(cfg.ClaudeCredentials)` bind mounted
read-write at `$HOME/.claude`. One directory, one machine, shared by every user of
the server: a second login overwrites the first, and there is no name on either.
The container is `hexagon-claude-login-<userID>`, deliberate so a leftover is
found rather than tracked.

`internal/httpapi/handlers_claude.go` — `GET /api/claude` reports the stored
credential, the file, and which of them is `effective`; `PUT`/`DELETE
/api/claude/credential` set and forget the pasted one; the login terminal and its
stop. `editorCredential` exists because the Images editor and sessions used to
disagree about which account they were, and it keeps them on the same order of
preference.

`internal/session/lifecycle.go` — `SetPorts` and `rebuildContainer` are the
precedent for everything the second half of this point needs: refuse unless the
session is stopped, write the row before building, rebuild the container (or let
compose recreate the project), and leave the session `gone` if the rebuild loses
it. `handleUpdateSessionPorts` is the route shape, and `handleUpdateSession` says
in its comment why a change like this is not a `PATCH` field.

`internal/store/images.go` — `CountSessionsUsingImage`, and the 409 the delete
handler answers with, are the precedent for a resource a session still depends on.

## Design

### An account is a row, whichever kind it is

`009_claude_accounts.sql`:

```sql
CREATE TABLE claude_accounts (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('api_key', 'oauth_token', 'login')),
    secret_enc BLOB,
    is_default INTEGER NOT NULL DEFAULT 0 CHECK (is_default IN (0, 1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (user_id, name)
);
CREATE INDEX idx_claude_accounts_user ON claude_accounts(user_id);
CREATE UNIQUE INDEX idx_claude_accounts_default ON claude_accounts(user_id) WHERE is_default = 1;
```

Four things in that are decisions rather than syntax.

**`kind` gains a third value, and it is Hexagon's word rather than the CLI's.**
`api_key` and `oauth_token` stay `claudex.KindAPIKey` and `claudex.KindOAuthToken`
for the reason `006` records: the meaning of the kind is which variable the CLI
reads, so it is that package's word. `login` names no variable at all — it says
the credential is a file in a directory Hexagon keeps — so the constant for it
belongs to `store`, and the migration comment says which is which.

**`secret_enc` is nullable**, and it is the only place the two kinds differ in the
table. A login account is a row with a name and a directory and no secret at all,
which is also the honest description of one that has been created but never signed
in on.

**The default is a partial unique index, not a convention.** `is_default` with
`CREATE UNIQUE INDEX ... WHERE is_default = 1` makes "at most one default per
user" a fact the database enforces; setting a new one is a two-statement
transaction that clears the old. A `default_claude_account_id` column on `users`
would say the same thing and put a property of the accounts list somewhere else to
read it from.

**The existing credential is carried over as a default account**, with the same
sealed bytes and a v4 UUID built in SQL exactly as `003_provider_accounts.sql`
builds one, and then `claude_credentials` is dropped. Nobody is asked to paste a
token again for a change they did not make, and there is one table holding Claude
accounts rather than two holding half of one each.

And on `sessions`:

```sql
ALTER TABLE sessions ADD COLUMN claude_account_id TEXT NOT NULL DEFAULT '';
```

`NOT NULL DEFAULT ''` with no foreign key, in the shape of `sessions.provider`.
Empty means "whatever the server resolves", which is precisely what every session
that exists today already gets, so nothing is backfilled and no existing row
changes meaning. A foreign key with `ON DELETE SET NULL` was rejected for the
opposite reason: it would quietly turn a session that names its account into one
that names none, while its container goes on carrying that account's credential.
The row would stop describing the container, which is the one thing it is for.

Instead, **an account a session still uses cannot be deleted**, answered 409 and
counted the way `CountSessionsUsingImage` counts. It is the same rule images
already carry and for the same reason: a stopped session can be started, and
rebuilt, from what its row names.

### A login account owns a directory; a pasted one owns a sealed blob

| Kind | Where the credential is | How it reaches a session container |
|---|---|---|
| `api_key`, `oauth_token` | `secret_enc`, sealed with the server key | `claude.Env()` in the container's environment |
| `login` | `<DataDir>/claude-accounts/<accountID>/.credentials.json`, written by Claude Code itself | that file bind mounted read-only at `$HOME/.claude/.credentials.json` |

This is M1.10's asymmetry left exactly as it was — Hexagon still never parses,
copies or invents a `.credentials.json`, because the format belongs to Anthropic
and has a refresh token and an expiry in it — with the single change that makes it
plural: the login container binds **the account's** directory instead of
`filepath.Dir(cfg.ClaudeCredentials)`.

So `StartClaudeLogin` takes the account rather than the user: the container is
`hexagon-claude-login-<accountID>`, its throwaway `$HOME` is
`<ClaudeLoginDir>/<accountID>`, and the directory it writes into is the account's.
Two accounts can therefore be signed in at once without either overwriting the
other, which is the whole point. `ErrClaudeLoginUnavailable` stops applying to an
account login: it meant "no credentials path is configured", and an account's
directory is one Hexagon makes.

The cost M1.10 recorded goes down rather than up, and that is worth saying out
loud: today the login container gets read-write access to the host user's entire
`~/.claude`; an account's gets one directory that holds one account's credential
and nothing else.

**The machine-wide login stays.** `claude.credentials` goes on meaning what its
README row says, the existing login terminal keeps its route, and a session that
resolves to no account keeps mounting that file. Adopting it into an account row
at first start was rejected: it is a path the operator configured and can point
anywhere, and rewriting the database on the strength of a configuration key is the
kind of magic that is impossible to undo from the page that did it.

### A session gets exactly one Claude credential

Resolved when the session is created, and again every time its spec is rebuilt:

```
1. the account named in the request      -> its environment variable, or its file
2. else the user's default account       -> the same
3. else nothing                          -> cfg.AnthropicAPIKey, and the cfg.ClaudeCredentials mount
```

Step 3 is today's behaviour, unchanged and untouched, so a machine configured
entirely from a file goes on working exactly as it does — the same promise M1.10
made when it added the layer above the configuration.

What does change is that **with an account in play the legacy mount is not added**.
Today it goes in unconditionally and the environment silently wins inside the
container; a session that has an account carries an environment credential or a
file and never both. That retires the paragraph M1.10 had to write for the UI
about Claude Code's precedence shadowing a login the user had just performed:
there is nothing to shadow, because there is only ever one.

### Interfaces

`session.CredentialSource.ClaudeCredential(ctx, userID)` becomes
`ClaudeSecret(ctx, account *store.ClaudeAccount) (claudex.Credential, error)`.
The manager loads the account row itself — it already holds `m.store` — and
decides between the environment and the mount, because the directory layout is the
manager's to own; `internal/auth` is asked only to open the cipher, and goes on
being the one package that ever does.

`claudex.Credential` gains `File string` beside `Secret`, and `KindLogin`; `Env()`
returns nothing for a login. That is what lets the **Images page** authenticate as
a default account that happens to be a subscription: `claudex/container.go`
already mounts a credentials file read-only and copies it into the container's
home — it does exactly that for `cfg.ClaudeCredentials` today, with a comment
explaining that the copy is because the CLI rewrites the file when it refreshes —
so it takes `cred.File` when there is one and falls back to what it has now.

The host `claudex.Runner` is the honest gap, and the analysis names it rather than
letting it be discovered: with no secret it goes on inheriting the server user's
own environment and login, exactly as today. A file cannot be handed to a process
that is not in a container without deciding what its `HOME` is, and that is a
decision this point does not need to make.

### Changing the account of a stopped session

`Manager.SetClaudeAccount(ctx, session, accountID)` in `lifecycle.go`, built as
`SetPorts` is built, because it is the same operation on a different field:

- refused unless the session is `stopped`, reconciled against Docker first so a
  container somebody started from outside Hexagon is not rebuilt underneath them;
- a no-op when the account is already the one asked for, so nothing is thrown away
  for a request that changes nothing;
- the row written **before** the container is built, for the reason `SetPorts`
  gives: if the rebuild then fails the session claims an account it does not carry,
  and the other order would leave a session showing one account while its container
  authenticates as another. Only one of those is safe to be wrong about;
- `rebuildContainer`, which already branches to `rebuildProject` for a compose
  session and already leaves the session `gone` when the old container is removed
  and the new one cannot be created.

Two consequences the dialog has to say, both already written for ports: the
container is rebuilt from its image, so anything installed inside it by hand is
gone while the workspace and the home directory are not; and the rebuilt container
carries today's credentials, so an account that has been signed out of since fails
the rebuild instead of quietly producing a container that cannot authenticate.

## Alternatives rejected

- **Sealing the credentials file into the database.** M1.10 rejected it once, and
  nothing about the reasons has changed: a format Hexagon would have to keep up
  with, a refresh with two owners, and a per-session copy that goes stale in
  silence. Per-account directories are the plural form of the answer it gave
  instead.
- **Copying the chosen account's file into the session home at each start**, so
  that switching accounts would not need a new container. It works for a login
  account and cannot work for a pasted one, so "switch account" would mean two
  different things with two different costs depending on which kind you picked —
  and a token the container refreshed would end up in the session's home rather
  than in the account, where the next session would not find it.
- **Adopting `cfg.ClaudeCredentials` into an account row.** Above: magic performed
  on a configured path.
- **`ON DELETE SET NULL` on `sessions.claude_account_id`.** Above: the row would
  stop describing its own container.
- **No default account.** Above: the Images editor and any create request without
  the field would fall past every account the user configured and onto the server
  configuration, which is the one thing this point exists to stop being the answer.
- **A page of its own for Claude accounts.** The Accounts page is already where
  credentials are configured and already has the shape — a row per thing, an inline
  form that opens, a hint saying exactly which token to make. This is a second list
  on it, not a second page.

## API

- `GET /api/claude` keeps `file`, `canLogin`, `canVerify`, `inContainer` and
  `defaultImage`; `credential` goes, and `effective` becomes
  `"account" | "apiKey" | "file" | "none"` — the default account first, then the
  two configuration fallbacks, then nothing.
- `GET /api/claude/accounts` lists them: id, name, kind, default, timestamps, and
  for a login account `login: {present, updatedAt}`, because a row can exist before
  anybody has signed in on it and the card has to be able to say so. **No endpoint
  has a secret field**, which is the point of the response type existing.
- `POST /api/claude/accounts` takes `{name, kind, secret}`. A secret kind is
  checked against the CLI before it is stored, exactly as
  `handleSetClaudeCredential` does now and for the same reason — a mistyped token
  stored anyway shows up later as a session that cannot sign in, with no clue why —
  and a login kind takes no secret at all. The first account a user creates is the
  default.
- `PATCH /api/claude/accounts/{id}` renames, replaces the secret, or makes the
  account the default. `DELETE` removes it, 409 while a session still uses it, and
  takes its directory with it.
- `GET /api/claude/accounts/{id}/login/terminal` and `DELETE .../login` are the
  browser login for one account. `GET /api/claude/login/terminal` and `DELETE
  /api/claude/login` stay as they are: the machine-wide login is still there.
- `PUT`/`DELETE /api/claude/credential` are removed. The collection replaces them,
  which is the same churn `POST /api/images/dockerfile` paid to become
  `/api/images/source`, and paid once.
- `PUT /api/sessions/{id}/claude-account` takes `{claudeAccountId}` and is built in
  the shape of `/ports`: 409 while the session runs, 400 for an account that is not
  this user's, and the lifecycle errors otherwise.
- `sessionResponse` gains `claudeAccountId` and `createSessionRequest` gains the
  same. An id rather than a name, like `imageId`: the pages that show a name are
  the pages that already load the list.
- Every route goes in the `protected` map. Bodies stay under
  `maxClaudeRequestBody` and `maxSessionRequestBody`.

## Configuration

One derived field and no new setting: `config.Config.ClaudeAccountsDir` =
`<DataDir>/claude-accounts`, in the shape of `DatabasePath`. There is no file key,
no environment variable, no `Patch` row and no README table row, because there is
nothing here for an operator to choose — it is where Hexagon keeps its own data.
`0700`, created on demand, the way `ClaudeLoginDir` already is.

`claude.credentials` keeps its key, its default and its README row, with the
sentence about what it is for made narrower: it is the login of the machine, used
by a session that resolves to no account, and it is still what the machine-wide
browser login writes.

## UI

- `AccountsView.vue` — the Claude card becomes a list. Each row: the name, the
  kind, a default badge, and for a login account whether anybody has signed in on
  it yet. Per row: make default, rename, replace the secret, log in again, delete.
  Above it a form that adds one — a name, and the three kinds. Below it, the
  machine-wide login, said plainly to be what a session with no account falls back
  to.
- Creating a login account is a row before it is a credential, and the card has to
  show that state rather than pretend: the account exists, nobody has signed in,
  press **Log in**. The dialog is the one that is already there, with
  `TerminalPane` pointed at the account's own terminal URL.
- `NewSessionDialog.vue` — an account select, shown only when there is more than
  one to choose between; with one, or none, the resolution answers and the form
  says nothing.
- `SessionsView.vue` — an **Account** button beside **Ports** on a stopped session,
  opening a dialog in the shape of `SessionPortsDialog.vue` and carrying the same
  warning about the container being rebuilt.
- `SessionView.vue` — which account this session runs as, beside what it already
  reports about itself.
- `api.ts` mirrors the Go structs field for field, as always.

## Impact

| Area | Change |
|---|---|
| Schema | `009`: `claude_accounts` with the partial unique default index, the rows carried over from `claude_credentials`, that table dropped, `sessions.claude_account_id` |
| Store | `ClaudeAccount` and its kind constants; list, get, create, update, set-default, delete, and a count of the sessions using one; `Session.ClaudeAccountID` and its setter |
| auth | `ClaudeSecret` in place of `ClaudeCredential`; the cipher still never leaves the package |
| claudex | `KindLogin`, `Credential.File`, `Env()` empty for a login, and the container editor preferring `cred.File` |
| session | `CredentialSource.ClaudeSecret`; `containerSpec` resolves one credential and adds the legacy mount only when there is no account; `StartClaudeLogin` per account; `SetClaudeAccount` beside `SetPorts` |
| Config | derived `ClaudeAccountsDir`, and a narrower sentence for `claude.credentials` |
| API | the `/api/claude/accounts` collection, per-account login, `PUT /api/sessions/{id}/claude-account`, `claudeAccountId` on the session; `/api/claude/credential` removed |
| UI | the accounts list on the Accounts page, the select in the create dialog, the Account button and its dialog, the account on the session page |
| Docs | README: more than one Claude account, which one a session uses, and what changing it costs |

## Verification

`internal/store` — a migration test in the shape of the one `007` already has:
an existing credential comes out as a default account of the same kind with the
same sealed bytes; the partial index refuses a second default for one user;
`UNIQUE(user_id, name)` becomes `ErrConflict`; and sessions that existed before
come out with an empty `claude_account_id`, which is the assertion that says no
existing session changed meaning.

`internal/session` — `containerSpec` asserted field by field, as
`TestSessionContainerSpec` already asserts it, because the spec is the whole
contract: a pasted account sets its variable and adds **no** credentials mount; a
login account mounts `<accountsDir>/<id>/.credentials.json` read-only and sets no
Anthropic variable at all; and a session with no account is exactly what it is
today, which is the test that says the new path did not disturb the old one.
`SetClaudeAccount` rebuilds only on a real change, refuses a running session, and
leaves the session `gone` when the rebuild loses its container.
`StartClaudeLogin` binds the account's directory and nothing else.

`internal/httpapi`, over the real router with `testEnv` — a stored secret never
comes back from any endpoint; an account belonging to another user is a 404;
deleting one a session still uses is a 409 and deletes nothing; a create request
naming no account gets the default, and one naming an account gets that account's
variable in the spec the fake Docker was handed; the per-account login container
carries no Anthropic environment; and without a cookie every one of them is a 401.

By hand, which is the only thing that proves the mechanism: two accounts, one
pasted and one signed in through the browser, a session on each, and `claude`
inside them reporting two different identities. Then stop one, change its account,
start it, and confirm the identity changed with it — that last claim is the one
most likely to be wrong, because it is the one that depends on the container
really having been rebuilt.

## The security invariants this touches

- **Secrets stay sealed and stay in `internal/auth`.** More rows is not more
  places: `ClaudeSecret` opens the cipher, the account response type has no secret
  field, and the store holds bytes it cannot read.
- **Every query is scoped to the authenticated user.** An account id arriving from
  the browser is resolved with `WHERE id = ? AND user_id = ?`, on the accounts
  endpoints and on the session ones alike.
- **The account directories hold live credentials**, so they are `0700` under
  `DataDir`, created by Hexagon rather than by Docker as root, and never served
  over HTTP.
- **The login container's reach shrinks.** It gets one account's directory
  read-write instead of the host user's whole `~/.claude`. That is the one place
  this point makes the boundary tighter rather than leaving it where it was, and it
  is worth recording as a reason to prefer accounts over the machine-wide login.
