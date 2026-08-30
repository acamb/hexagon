# M2.2 — Server settings

## What was asked

> Nuova pagina settings dove impostare le configurazioni dell'applicazione:
>  - porta
>  - client id / secret
>  - username github abilitati

## Reading of the request

The list is the minimum, and the user widened it while this was being written:
the page carries **every key the configuration file has**, with two of them shown
and not editable. So the point is not "a form for three settings" but "the
configuration file, edited from the browser".

The wizard of M2.1 exists to leave a state; this page exists to live in one. That
is the whole structural difference, and everything below follows from it: a save
happens on a server that is working, so it must not be able to stop it working.
Not now — the caller cannot sign themselves out, and the origin check cannot turn
against the page that made the call — and not at the next start, which is the
failure the wizard never had to think about because a server nobody could sign in
to had nothing left to lose.

The second reading is about honesty. Most of these settings are captured at
startup: the database handle, the Docker client, the session manager, the
code-server release, the rate limiter, the cookie `Secure` flag. Three of them
are not, because M2.1 put them behind `auth.Gate` on purpose. A page that
presented all twenty as if saving them were applying them would be lying about
seventeen. So the page's real job is to report three things per setting — what
the process is running on, what the file now says, and whether they differ — and
the API is shaped around that rather than around the form.

One divergence from the statement, decided with the user: **the port is
read-only**. It is displayed, because an operator looking at the settings page
wants to know which address the server answers on, and it is not writable. The
reasoning is in the design below, together with the second read-only setting,
which is the secret key.

## Current behaviour

`internal/config/update.go` — `Update(path, Patch)` is M2.1's writer. It edits
the **decoded JSON document** rather than the `file` struct, so a file keeps the
keys it had and gains no others; it refuses outright to overwrite a file that
does not load; and it renames a `0600` temporary file over the target so an
interrupted write cannot leave a configuration the server will not start from.
`Patch` has three pointer fields and models nil-vs-value — "not changing" is not
"clearing". `Writable(path)` answers by trying, because on a POSIX system nothing
else is conclusive.

`internal/config/config.go` — `Load` layers defaults, file, environment.
`checkTransport` is a property of the `addr`/`publicUrl` pair rather than of
either. `pickInt` refuses a negative limit and reads zero as "use the default".
`pickBool` makes an environment variable's mere presence win, so the file cannot
say false over it. `claudeCredentials` is the one setting where an explicit empty
string is itself a value, which is why the file holds it as a pointer. `loadFile`
demands `0600` and rejects unknown keys.

`internal/auth/gate.go` — `Gate.Set(oauth, allowlist)` replaces the pair
together, and `requireAuth` (`internal/httpapi/middleware.go:40`) reads the
allowlist through it on **every** request. That is why an allowlist change needs
no restart, and equally why removing yourself from it takes effect on your next
click.

`internal/httpapi/handlers_setup.go` — `setupStatus()` reports the settings the
server is actually running on and names the ones an environment variable is
supplying; `reconfigure()` re-`Load`s the file and hands the result to the gate.
Both are written for three settings and are the seed of what this point needs
for twenty.

Captured at startup, and therefore restart-scoped:
`internal/auth/session.go:69` (the cookie `Secure` flag, from `publicURL`),
`internal/httpapi/middleware.go:114` (HSTS, likewise), `Server.originMatches`
(reads `s.cfg.PublicURL`, and `s.cfg` is never mutated), `newIPLimiter` in
`router.go:99`, and every collaborator wired in `cmd/hexagon/main.go:buildDeps`.

There is no settings endpoint and no page: since M1.2 the configuration file has
been read at startup and never since, except by the wizard.

## Design

### Three classes of setting, and the page says which is which

**Hot** — `github.clientId`, `github.clientSecret`, `github.allowedUsers`.
Written, then applied through `reconfigure()` and `Gate.Set`. An allowlist change
admits or refuses on the very next request; nothing about admission is cached
anywhere.

**Restart-scoped** — every other editable setting. Written, and in force at the
next start. `publicUrl` is the instructive one: it feeds the OAuth `redirect_uri`,
the origin check, the cookie `Secure` flag and HSTS, and hot-swapping only the
OAuth half would send the browser that just saved to an origin this process
rejects. The rest are worse, not better: the store is open, the Docker client is
connected, containers are running against a workspace root the manager was built
with.

**Read-only** — `addr` and `secretKey`. They are not fields of the request type
at all, so there is nothing to ignore: read-only here means the API cannot
express the change, not that it accepts one and drops it.

The rule behind that pair, stated once so it does not have to be argued again: a
wrong value in either cannot be corrected from this page afterwards.

- A wrong `addr` is a port nobody can reach, and the settings page is behind that
  port. Rebinding live is not the answer either — the terminal WebSocket and the
  VS Code proxy are hijacked connections, which no listener shutdown reclaims, so
  after a swap sessions would keep running on a port the operator believes is
  closed.
- A wrong `secretKey` makes every sealed GitHub and Bitbucket token
  undecryptable. It is the one setting whose change is silent data loss, and no
  amount of validation before the write can undo it afterwards.

Both stay a file edit and a restart, which is a shell on the machine — the same
place the recovery for either already was.

A pleasant consequence of `addr` being read-only: the pair `checkTransport`
guards cannot be broken from this page in the dangerous direction. The listener
cannot be moved onto the network from here at all, so the only way a save can
fail that check is by taking `publicUrl` off https on an instance whose address
is already public — and that is caught before the file is replaced.

### The response carries running and saved, per setting

`GET /api/settings` reports what the process is running on — `s.cfg`, plus the
gate for the three hot settings, which are the live truth after an apply — beside
what the configuration file would produce at the next start, obtained by a fresh
`config.Load(s.cfg.ConfigPath)`. `restartRequired` is simply "the two differ",
and the UI marks the individual settings that do.

Reloading rather than reading the file's raw values is what makes the comparison
mean anything: what matters is not what the file says but what the next start
will resolve, environment and defaults included. It buys two more things for
free. A file edited by hand behind the server's back shows up as a difference on
a plain `GET`, with no save involved. And a setting that has been saved but is
not in force yet is shown as exactly that, rather than as a value the page claims
is live.

One wrinkle: `Load` refuses a path that was named and is not there, which is
right for a start — ignoring a path someone asked for configures a server by
accident — and wrong for a question about a file that may not exist yet. So the
reporting side goes through `config.Resolve`, which is `Load` for a path that is
being reported on rather than asked for.

The cost, said out loud: resolving creates directories and reads the secret key
as a side effect, so a `dataDir` changed on disk by hand is created by a `GET`,
and a rejected save that moved it can leave the directory behind. It is
idempotent for every normal case — the directories are the ones this process is
already using — and it is the same trade M2.1 already makes and documents. What
the page has to say out loud instead is the consequence for the operator:
moving `dataDir` moves the database *and* the secret key, so the next start
finds neither the sessions nor the sealed tokens of this one.

### Validation happens before the file is replaced

This is the new engineering, and with the whole file editable it is what makes
the page safe to use. `Update` today refuses to overwrite a file that *currently*
loads badly; nothing checks the file it is about to write. That was enough for a
wizard writing two opaque strings and a list of logins onto a server that could
not be signed in to. It is not enough for a form that can set a listen address's
partner, a limit, a data directory and a secret key path.

So `Update` writes the candidate into the temporary file it already uses, calls
`Load` on that path, and renames it over the target **only** when `Load`
succeeds. Otherwise the temporary file is removed and the error comes back to the
caller, which answers `400` with the message verbatim.

In one step that covers `checkTransport`, `pickInt`'s bounds, the secret key
decode, the permission rule and unknown keys, with no second copy of any of those
rules to drift from the originals — the file is validated by the very function
that will read it at the next start, which is the same argument M2.1 made for
reloading after the write, moved to before it where it can still refuse. The
temporary file is loaded with this process's environment, which is right: the
next start has the same environment.

Two checks remain outside it, because `Load` cannot make them. `publicUrl` has to
parse and carry a scheme and a host — `Load` accepts any string, and an unusable
one surfaces much later as an OAuth redirect that never matches — so it is
checked beside `auth.CallbackURL`, where the derivation already lives. And the
caller has to survive the new allowlist, which is the next section.

### The allowlist is the admin list

Multi-user is not a goal, and no role is being added: everyone the allowlist
admits can change these settings. The analysis says that plainly rather than
leaving it to be discovered from the router.

What follows from it is the guard. A save whose new allowlist does not admit the
caller is **refused**, checked by building the allowlist and asking
`Allowed(ctx, login, githubID)` before anything is written. `requireAuth`
re-checks per request, so the lockout would be immediate, and `/api/setup` is
closed for good once somebody has signed in, so there is no wizard to fall back
to. Every other kind of wrong value has the environment as an escape hatch; this
one has only a shell.

The refusal names the fix, and it is deliberately not a warning-and-proceed: an
operator removing their own entry has almost certainly mistyped the list rather
than decided to lock themselves out of the machine.

### Secrets never come back

`github.clientSecret` and `claude.anthropicApiKey` are reported as
`clientSecretSet` and `anthropicApiKeySet` booleans, never as values. This is the
same rule the rest of the API already follows — a sealed token has never left
`internal/auth`, and a `store` model is never serialized directly — applied to
the two secrets that live in the configuration file instead of the database.

An empty field on save means "leave the stored one alone", which is exactly what
`config.Patch`'s nil field already expresses, so no new mechanism appears for it.
The form's placeholder says so, because a password box that looks empty and a
password box that means empty are otherwise the same box.

### A patch of twenty settings, without twenty assignments

`Patch` grows to one pointer per editable key, grouped like `config.file` so the
two can be read side by side, plus one flag of its own for the third state of
`claude.credentials` described below. `Update` applies them onto the decoded document
through a small table of path-and-value pairs rather than twenty hand-written
assignments, which is what keeps the property M2.1 bought — a file that sets
three things goes on setting three things instead of growing every key Hexagon
knows — from being lost the moment the patch got big.

One rule for empty values, and one exception. **A setting left empty is removed
from the document rather than written as an empty value**, so the file goes on
saying only what has actually been chosen: an empty string, an absent key and a
false boolean already resolve identically in `Load`, so writing them would add
noise and no meaning. The exception is `claude.credentials`, where an explicit
`""` means "mount nothing" and has to be written to say so. The form expresses
that as a three-way choice — the default path, no file, or a specific path —
rather than as an empty text box whose meaning nobody could guess.

### A bug this point has to fix

`reconfigure()` builds `auth.NewOAuth` with the **reloaded** `cfg.PublicURL`.
That is harmless for the wizard, which runs before anybody has an origin to be
checked against, and wrong the moment `publicUrl` becomes editable: the OAuth
`redirect_uri` would change under a process whose origin check, cookie `Secure`
flag and HSTS header still hold the old value, so a sign-in would be redirected
to an origin the same process rejects.

It takes `PublicURL` from `s.cfg`, the running value, and `publicUrl` is then
restart-scoped everywhere with no exceptions to remember.

### Alternatives rejected

- **Settings in the database.** Decided against in M2.1, and the reasoning
  stands: two homes for the same secret, a precedence rule to explain, and a page
  that has to say which of the two the server actually used.
- **An admin role, or an `is_admin` column.** A second answer to a question the
  allowlist already answers, and a way to end up with a machine nobody can
  configure.
- **Editing `addr` or `secretKey`.** Above.
- **Hot-reloading `publicUrl`, or rebuilding `buildDeps` on save.** Four captured
  values for `publicUrl` alone; a live Docker client, an open database and
  running containers for the rest; and the browser that saved is still on the old
  origin either way.
- **A free-text editor for the raw JSON.** It would need exactly the same
  validation, and it would lose the per-setting provenance — running, saved, from
  the environment — that is the reason this page is worth more than `$EDITOR`.

## API

Two routes, both in the `protected` map of `internal/httpapi/router.go`. Handlers
in a new `internal/httpapi/handlers_settings.go`, the body through
`http.MaxBytesReader` with a `maxSettingsRequestBody` constant, responses through
`writeJSON` and `writeError`. Being under `/api/`, the save is already covered by
`guardStateChanges`: a same-origin signal plus a JSON content type.

`GET /api/settings`

```json
{
  "configPath": "/home/andrea/.config/hexagon/config.json",
  "writable": true,
  "restartRequired": false,
  "fromEnvironment": ["publicUrl", "debug"],
  "clientSecretSet": true,
  "anthropicApiKeySet": false,
  "running": {
    "addr": "127.0.0.1:8080",
    "publicUrl": "http://localhost:5173",
    "insecureHttp": false,
    "dataDir": "/home/andrea/.local/share/hexagon",
    "workspaceRoot": "/home/andrea/.local/share/hexagon/workspaces",
    "secretKeySource": "/home/andrea/.local/share/hexagon/secret.key",
    "debug": false,
    "github": { "clientId": "Iv23li...", "allowedUsers": ["1234567"], "apiUrl": "" },
    "bitbucket": { "apiUrl": "" },
    "claude": { "credentials": "/home/andrea/.claude/.credentials.json", "binary": "", "model": "" },
    "git": { "userName": "", "userEmail": "" },
    "vscode": { "dir": "", "version": "4.x.y" },
    "docker": { "host": "" },
    "limits": { "maxSessionsPerUser": 20, "maxConcurrentBuilds": 2, "publicRatePerMinute": 60 },
    "callbackUrl": "http://localhost:5173/api/auth/callback"
  },
  "saved": { "…": "the same shape, as the next start would resolve it" }
}
```

`addr` and `secretKeySource` are there to be displayed and have no counterpart in
the request type. `secretKeySource` names where the key comes from — the
environment, the file, or the generated `secret.key` — and never the key.
`callbackUrl` is derived, and appears in both halves because it is what has to be
registered on GitHub: the one in `running` is what the handshake sends today, the
one in `saved` is what it will send after a restart.

`PUT /api/settings` takes the editable subset, every field optional, and answers
`200` with the body above read back after the apply — off the reloaded
configuration rather than off the form, which is where an environment variable
shadowing what was just saved becomes visible. `400` for a value `Load` rejects,
for a `publicUrl` that does not parse, and for an allowlist that excludes the
caller; `500` naming the path when the file cannot be written.

`PUT` rather than `PATCH`: the form submits the whole set it manages, and the
fields it may omit have a defined meaning when absent.

## Shared with the wizard

Both routes write settings and then reconfigure, so the shared part moves onto
`*Server` in `handlers_settings.go`:

- `settingsSnapshot()` — the running and saved pair, `fromEnvironment`,
  `restartRequired`, `writable`.
- `applySettings(ctx, patch)` — validate, `config.Update`, `reconfigure`.

`handleSetup` becomes that plus the password check, which is the accurate
description of what the wizard is: a settings save for a server nobody can sign
in to yet. `setupStatusResponse` keeps its wire shape — `SetupView.vue` depends
on it, and the wizard deliberately says less than this page does.

`fromEnvironment` grows from three variables to the whole table, and two details
of it are worth a comment where it is written: `ANTHROPIC_API_KEY` and
`DOCKER_HOST` carry no `HEXAGON_` prefix, and for the two booleans the variable's
presence alone wins, so a file saying false cannot turn one off again.

## UI

`web/src/views/SettingsView.vue`, in the shape of `AccountsView.vue`:
`AppHeader`, an `h1`, one card per group of the configuration file — Server,
Storage, GitHub, Bitbucket, Claude, Git, VS Code, Docker, Limits — a local
`message(e)` helper over `ApiError`, and scoped CSS over the tokens already in
`style.css`.

Every field carries its label, its value, and a hint naming the environment
variable that overrides it. Three kinds of annotation sit on top of that, and
each of them is a state the operator would otherwise have to infer:

- **From the environment** — the wizard's existing wording, reused: set by
  `HEXAGON_…`, which wins over what is saved here.
- **Waiting for a restart** — the saved value differs from the running one, with
  both shown.
- **Read-only** — `addr` and the secret key render disabled, with one line saying
  why rather than leaving it to look like a bug.

`dataDir` carries a warning of its own: changing it points the next start at a
different database, which is to say at a different set of sessions and images.

The footer names the configuration file and warns when it cannot be written,
before the form rather than after it, as the wizard does. After a save, a notice
lists what is in force now and what is waiting for a restart.

`/settings` joins the routes in `web/src/router.ts`, not public; a **Settings**
link joins `AppHeader.vue`; and `api.ts` gains a `settings` group whose
interfaces mirror the Go structs field for field.

## Impact

| Area | Change |
|---|---|
| Schema | none — the settings live in the configuration file |
| Store | none |
| auth | none beyond what `Gate` already offers |
| Config | `Patch` covers every editable key, applied through a path table; `Update` validates the candidate by `Load`ing it before the rename; an empty value removes its key, `claude.credentials` excepted; a `publicUrl` shape check |
| API | protected `GET /api/settings` and `PUT /api/settings`; `reconfigure` takes `PublicURL` from the running configuration |
| UI | `SettingsView.vue`, the route, the nav entry, `api.settings` |
| Docs | README gains a Settings paragraph under "Using it", and a note in the configuration section on which keys the page writes and which two it only shows |

## Verification

`internal/config` — `Update` refuses a patch whose result does not load (the
`addr`/`publicUrl` pair, a negative limit, a malformed `publicUrl`) and leaves
the existing file byte-identical when it does; no temporary file survives a
rejected save; a setting cleared through the patch loses its key while its
neighbours keep theirs; `claude.credentials` set to `""` is written and survives
a round trip; keys this package never looks at still come back unchanged.

`internal/httpapi`, in the `testEnv` style over the real router — `GET
/api/settings` answers 401 without a session; no response ever carries the client
secret or the API key; a save with an empty `clientSecret` keeps the stored one;
a changed allowlist admits or refuses on the very next request, with no restart;
a save that drops the caller from the allowlist is refused **and** the previous
allowlist still admits them; a changed `publicUrl` sets `restartRequired` and
does not change the callback URL the running OAuth sends; a restart-scoped
setting is reported as saved-but-not-running; and a file edited on disk shows
`restartRequired` on a plain `GET`.

By hand: change the allowlist and watch a second browser get signed out on its
next request; change `publicUrl`, confirm sign-in still works on the old origin,
restart, confirm it works on the new one; confirm the file is still `0600`
afterwards.

## The security invariants this does not change

Nothing in AGENTS.md is amended, and that is the point worth making: the
exception M2.1 carved out for `/api/setup` is not widened. `/api/settings` is in
the `protected` map like every other endpoint; the allowlist remains the only
admission decision, and it is now also the answer to who may configure the
machine; the client secret, the API key and the secret key stay server-side; the
file keeps `0600` and the write stays atomic.

The honest summary is that validating before the rename makes a save from this
page strictly less able to break the instance than the hand edit it replaces —
with the exception of the two settings a hand edit is still the only way to
change, which is where they were before.
