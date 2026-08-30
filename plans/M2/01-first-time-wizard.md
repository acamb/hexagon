# M2.1 — First time wizard

## What was asked

> L'applicazione viene avviata senza github client id / secret e genera una
> password temporanea stampata nei log.
> Una pagina di first time wizard chiede quella password ed una volta inserita
> e' possibile inserire client id / secret / username abilitati che vengono
> salvati nel db.

## Reading of the request

Underneath the page and the password there is one structural change: **the
server must be able to start unconfigured**. Today it cannot, and that is not an
oversight — `buildDeps` refuses to build an API nobody can be authorised
against. So this point does not add a form; it adds a second startup state,
narrower than the running one, whose only purpose is to leave itself.

Everything else follows from that. The password exists because in this state
there is no identity to check and the log is the only channel the operator and
the server already share. The page exists because that state has to be reachable
from a browser. The form is the smallest set of settings that gets GitHub
sign-in working: client id, client secret, allowlist.

One deliberate divergence from the statement, decided with the user: the
settings are written to **the configuration file**, not the database. The client
id and secret already have a home, and it is one that is `chmod 600`, is what an
operator backs up or copies to another machine, and is validated on load by code
that already exists. A second home would mean two places holding the same
secret, a precedence rule to explain in the README, and a settings page that has
to say which of the two the server actually used. The milestone statement is
left as the user wrote it; this paragraph is the record of the change.

## Current behaviour

`cmd/hexagon/main.go` — `buildDeps` builds the allowlist and then the OAuth
client, and either error aborts `run`, which prints it and exits 1. Its own doc
comment states the intent: "The GitHub OAuth login is the only mode there is,
and its constructor refuses an empty allowlist, so the server never starts with
an unauthenticated API." A fresh clone therefore cannot be started at all before
an OAuth app has been registered by hand and three variables exported.

`internal/auth/oauth.go` — `NewOAuth` requires `ClientID` and `ClientSecret`,
and derives `redirectURI` from `PublicURL`; the callback URL is never
configured. `internal/auth/allowlist.go` — `NewAllowlist` refuses an empty list,
"without an allowlist any GitHub account on the planet could sign in and get a
shell on this machine". Neither value can change after construction: both are
captured in `httpapi.Deps` and read for the life of the process.

`internal/config/config.go` — `Load(path)` layers defaults, then the file, then
the environment. `loadFile` refuses a named-but-missing file, a file readable by
group or others, and any unknown key (`DisallowUnknownFields`, "a dropped
allowedUsers is an authentication bypass"). `Config.ConfigFile` records the file
the settings came from and is empty when none was found. There is no writer:
configuration has only ever been read.

`internal/httpapi/router.go` — exactly three public routes, each wrapped in
`s.limitPublic`; every other route goes into the `protected` map and through
`requireAuth`. `web/src/router.ts` — `/login` is the only route with
`meta: { public: true }`, and the guard resolves the user through
`session.ts:load()`, which returns null on 401 and rethrows anything else.

## Design

### Setup mode is "nobody has ever signed in"

Not "the settings are missing". The two differ exactly where it matters: an
operator who saved a wrong client secret through the wizard has settings that
look complete and a server nobody can sign in to. Under the narrower rule the
wizard would have closed behind them and the only way back would be editing
files on the server. Under this one it is still open, with a fresh password in
the log, and the mistake is a retype.

The condition is one query, `store.AnyUser`, evaluated per request rather than
captured at startup, so the window closes the instant the first sign-in succeeds
without anything having to notice and flip a flag. From that moment the setup
routes answer `409` for the life of the installation, and reconfiguring is the
settings page of M2.2, behind a real session.

What this costs, plainly: a server configured correctly by hand that nobody has
used yet is also in setup mode. It exposes two public routes guarded by a
128-bit password, and prints that password at every start until someone signs
in. That is the price of having a recovery path that does not distinguish
between a bad secret typed into the wizard and the same bad secret typed into
the file — the code cannot tell them apart, and the operator locked out by
either one needs the same door.

### A temporary password, in memory, for one process

Fifteen bytes from `crypto/rand` — 120 bits, and a clean six groups once
encoded — put through `encoding/base32` and hyphenated in groups of four, so it
can be read off a terminal and retyped without ambiguity about case. It is
compared with `subtle.ConstantTimeCompare` after hyphens are stripped and the
input upper-cased. Only its SHA-256 is kept, in the `auth.Setup` value — the
same treatment as a session cookie, for the same reason.

It never reaches the disk and dies with the process. That is what makes
"printed in the logs" honest: the password in *this* run's log is the only one
that works, and a restart is also a revocation. It is logged once at startup at
`Warn` — a degraded but usable state — next to the URL of the wizard, because an
operator reading a log needs both halves.

**Rejected:** writing it beside `secret.key`. It would be one more secret at
rest, with no expiry and no owner, to answer a question the log already answers.

### The password authenticates every setup request

It travels in the JSON body of each call. No cookie, no TTL, no second kind of
session to reason about beside the one `internal/auth` already owns — and it
matches what the rest of the API settled on when authorisation stopped being
cached in the cookie and became a check on every request. The wizard holds the
password in a `ref` for the length of the form, which is the same exposure a
setup cookie would have, without the disk.

**Rejected:** a short-lived `hexagon_setup` cookie mirroring `auth.Service`. It
would buy a shorter request body and cost a cookie name, an expiry, a clearing
path and a second answer to "who is this". There are two requests here, and the
second is the last.

### The wizard writes the configuration file

The target is the path the configuration was resolved from — the one named on
the command line, then `HEXAGON_CONFIG`, then the default location — whether or
not a file is there, which is why `Load` now reports it as `ConfigPath` beside
the `ConfigFile` it actually read. It is created with its directory at `0700`
and the file at `0600`, the permissions `loadFile` will demand of it at the next
start.

One rule is deliberately not relaxed: a file named with `-config` or
`HEXAGON_CONFIG` that does not exist is still a startup error, so the wizard
cannot be used to create one at a path someone typed. The reasoning from M1.2
stands — ignoring a path that was asked for starts a server configured by
accident — and the case the wizard is for, a machine with no configuration file
at all, resolves to the default location and is covered. It is written to a
temporary file in the same directory and renamed over the target, so a crash
half way through cannot leave the server with a file that no longer loads.

The edit is made on the decoded JSON document — a plain `map[string]any`, read
back with `UseNumber` so no value this package never looks at is reshaped by
being read and written — and not on the `config.file` struct. Encoding the
struct would write every key Hexagon knows at its zero value, turning a file
that sets three things into one that sets forty; a file an operator hand-writes
and hand-edits should come back looking like the file they wrote.

Nothing is lost that way, and that is a dividend of an earlier decision:
because `loadFile` rejects unknown keys, a file that loads cannot hold a key
this package does not understand, so there is nothing outside the document
worth preserving. The struct still does the checking — `Update` refuses
outright to overwrite a file that does not load, so a form never writes over a
configuration this server cannot read. Two visible consequences, worth saying
out loud: keys come back in alphabetical order, so a hand-written file is
reordered, and JSON has no comments, so there are none to lose.

### Precedence is unchanged: defaults < file < environment

The wizard adds no layer; it writes into the one that already exists. What falls
out of that has to be shown rather than hidden: with `HEXAGON_GITHUB_CLIENT_ID`
exported, the environment goes on winning over what the wizard just wrote. So
the wizard reports the settings that are *in effect* after the reload, and names
any that a variable is shadowing. The alternative — a value that was accepted,
stored and ignored — is the failure mode this whole point exists to remove.

It also means the environment stays the escape hatch for a server whose file is
wrong and whose door has closed.

### Hot reload behind a gate

`auth.Gate` (new, `internal/auth/gate.go`) holds the `*OAuth` and `*Allowlist`
behind an `RWMutex`, both nil while unconfigured. `httpapi.Deps` carries the
gate instead of the two values, and `requireAuth` and the auth handlers read
through it.

Saving writes the file and then calls `config.Load` **again** on that path,
rebuilding the gate from the result. Re-loading rather than using the values in
hand is the point: it proves the file the wizard produced is loadable by the
very function that will read it at the next start, permissions and unknown keys
included, instead of leaving a server that works now and refuses to boot later.
Only the OAuth settings are taken from that reload — a listen address or a data
directory changed on disk in the meantime still needs a restart, and the
response says so.

Two call sites have to tolerate a nil gate: `requireAuth`, which answers 401 so
the SPA redirects to `/login` exactly as it does for an expired session, and
`pruneRevokedSessions` at startup, which is skipped with an `Info` because there
is no allowlist to prune against.

**Rejected:** telling the operator to restart. It is less code today and the
same code tomorrow, since the settings page of M2.2 needs the gate anyway; and a
binary under systemd on a machine reached through this very web UI is not always
a restart away.

### The callback URL is shown, not asked

It is derived from `PublicURL` and is the one value the operator has to get
right on GitHub's side, where a mismatch fails with a message about the
`redirect_uri` and no clue about what Hexagon sends. The derivation currently
inlined in `NewOAuth` is extracted into `auth.CallbackURL(publicURL string)` and
used by both the constructor and the setup endpoint, so what the wizard shows
cannot drift from what the handshake sends.

### The credentials are not verified before they are stored

M1.6 set the opposite precedent for Bitbucket: check the credentials against the
call a listing starts from before sealing them. It does not transfer. An OAuth
app's client id and secret cannot be proved without a user consent round trip —
there is no endpoint that says yes to the pair alone. The first sign-in is the
test, which is precisely why the wizard stays open until one succeeds.

### One save endpoint, not a verify step

The two steps of the wizard are one request. A `POST /api/setup/verify` was
considered and dropped: it would reveal exactly the bit the save already
reveals — whether the password is right — so the security argument does not
decide it, and one route fewer does. The first step validates locally that the
field is not empty; a wrong password comes back as `401` from the save, and the
wizard returns to step one with its message and every field of step two still
filled in.

### Writability is reported before the form, not after it

`GET /api/setup` returns the target path and whether it can be written, so a
read-only mount or a root-owned config file is a sentence at the top of the
wizard rather than an error after the form is complete.

## API

Two public routes in `internal/httpapi/router.go`, beside the existing three and
wrapped in `s.limitPublic`. Handlers in a new
`internal/httpapi/handlers_setup.go`, the body through `http.MaxBytesReader`
with a `maxSetupRequestBody` constant, responses through `writeJSON` and
`writeError`. Being under `/api/`, the save is already covered by
`guardStateChanges`: same-origin signal plus a JSON content type.

`GET /api/setup`

```json
{
  "required": true,
  "configPath": "/home/andrea/.config/hexagon/config.json",
  "writable": true,
  "callbackUrl": "http://localhost:5173/api/auth/callback",
  "clientId": "Iv23li...",
  "allowedUsers": ["1234567"],
  "fromEnvironment": ["clientId"]
}
```

It never returns the client secret, and once a user exists it returns
`{"required": false}` and nothing else: the path of a configuration file is a
fact about the machine, and this route is open to anyone who can reach the port.

`POST /api/setup` takes `{"password", "clientId", "clientSecret",
"allowedUsers"}` and answers `200` with the same body the `GET` returns, read
back off the reloaded configuration rather than off the form: that is where an
environment variable shadowing what was just saved becomes visible. `401` for a
wrong password, `400` for an empty field, `409` once setup is no longer
required, and `500` naming the path when the file cannot be written.

## UI

`web/src/views/SetupView.vue`, one component with two steps, in the shape of
`LoginView.vue`: a bare `<main>`, no `AppHeader`, scoped CSS over the tokens
already in `style.css`. Step one asks for the password and says where to find
it. Step two takes the client id, the secret and the allowed users one per line,
repeats the callback URL to register on GitHub, and warns when a value is
shadowed by the environment or the file cannot be written. On success it points
at `/login`.

`/setup` joins `/login` as a public route; the guard redirects away from it when
setup is not required. `LoginView.vue` asks `GET /api/setup` on mount and
redirects to the wizard when it is, and gains a `not_configured` entry in its
`messages` map for the case where `/api/auth/login` is reached with no OAuth
client — which it now answers by redirecting to `/login?error=not_configured`
rather than by failing. `api.ts` gains a `setup` group whose interfaces mirror
the Go structs field for field.

## Impact

| Area | Change |
|---|---|
| Schema | none — the settings live in the configuration file |
| Store | `AnyUser(ctx) (bool, error)` |
| auth | new `Gate` and `Setup`; `CallbackURL` extracted out of `NewOAuth`; `OAuth.ClientID` and `Allowlist.Entries` for showing the running settings back |
| Config | `Update(path, Patch)`, `Writable(path)`, `Config.ConfigPath`, `loadFile` split from `resolvePath` |
| API | public `GET /api/setup` and `POST /api/setup`; `Deps` carries the gate |
| cmd | `buildDeps` no longer fails without OAuth settings; the password is logged when setup is open; the revoked-session sweep is skipped when unconfigured |
| UI | `SetupView.vue`, the public route, the login-page redirect, `api.setup` |
| Docs | README gains a "First run" section and its GitHub rows stop saying "Required"; the first security invariant in AGENTS.md is amended |

## Verification

`internal/config` — `Update` preserves every other setting through a round
trip; adds no key it was not asked for; creates the directory at `0700` and the
file at `0600` when neither exists; its output loads back through `Load`; an
explicitly empty `claude.credentials` survives; a file with an unknown key is
refused rather than overwritten; and the environment still wins over what was
written. `Writable` answers for a missing file under a writable directory, and
refuses one under a directory the process cannot write.

`internal/auth` — an empty `Gate` reports itself unconfigured and hands out
nothing, and `Set` makes it configured; `Entries` gives back the allowlist as it
was configured; the password matches whatever the hyphens, spaces and case, and
nothing else — including the password another process generated.

`internal/httpapi`, in the `testEnv` style over the real router — setup is
required with no users and not required with one; a wrong password is refused;
a right one writes the file and `GET /api/auth/login` starts redirecting to
GitHub in the same process, with no restart; the setup routes answer `409` once
a user exists; and every protected route still answers 401 while setup is open,
which is the invariant this point comes closest to breaking.

`cmd/hexagon` — `buildDeps` succeeds with no OAuth settings at all and leaves
the gate empty; `openSetup` opens the wizard when nobody has signed in and
leaves it shut when somebody has.

By hand: `rm -rf ~/.config/hexagon ~/.local/share/hexagon && make dev`, take the
password out of the log, complete the wizard, sign in, then check that the file
is `0600` and that the next start prints no password.

## The security invariant this changes

AGENTS.md says no endpoint outside `/api/health` and the auth handshake is
reachable without a valid session. The setup routes are the exception, and it
is amended in the same commit to say so and to name what stands in their place:
they are open only while no user has ever signed in, and they require a
password that exists only in this process's memory and in its log.
