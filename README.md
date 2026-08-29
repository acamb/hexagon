# Hexagon

A web UI for running Claude Code sessions in Docker containers. Pick a base
image, and a repository from a connected account or none at all: Hexagon clones
the repo, starts a container with the workspace bind-mounted, runs `tmux` inside
it, and renders that tmux in the browser.

Go backend, Vue 3 frontend, one binary with the frontend embedded.

## Status

The implementation follows [start-plan.md](start-plan.md), milestone by
milestone.

| Milestone | |
|---|---|
| M0 — scaffolding, config, database, embedded SPA | done |
| M1 — GitHub sign-in, sessions, allowlist | done |
| M2 — base image management | done |
| M3 — repository listing and cloning | done |
| M4 — session containers | done |
| M5 — browser terminal | done |
| M6 — frontend and polish | done |

## Requirements

Go 1.26, Node 22, Docker (from M2 on), and a GitHub account.

## Getting started

### 1. Register a GitHub OAuth App

At <https://github.com/settings/developers> → **New OAuth App**:

| Field | Value |
|---|---|
| Application name | Hexagon (dev) |
| Homepage URL | `http://localhost:5173` |
| Authorization callback URL | `http://localhost:5173/api/auth/callback` |

Then **Generate a new client secret**. The callback URL has to match exactly:
GitHub compares it verbatim against the `redirect_uri` the server sends.

### 2. Configure and run

Either put the values in a configuration file:

```sh
mkdir -p ~/.config/hexagon
install -m 600 config.example.json ~/.config/hexagon/config.json
$EDITOR ~/.config/hexagon/config.json     # clientId, clientSecret, allowedUsers

make dev
```

or export them:

```sh
export HEXAGON_GITHUB_CLIENT_ID=Iv23li...
export HEXAGON_GITHUB_CLIENT_SECRET=...
export HEXAGON_ALLOWED_USERS=your-github-login

make dev
```

Open <http://localhost:5173> — **not** `127.0.0.1:5173`. `make dev` sets
`HEXAGON_PUBLIC_URL=http://localhost:5173`, and the server's origin check
compares hosts, so requests from `127.0.0.1` are rejected.

You should land on `/login`, sign in through GitHub (it asks for the `repo`
scope: Claude Code needs it to clone and push), and come back signed in.

`make dev` runs the Go server on `:8080` and the Vite dev server on `:5173`,
which proxies `/api` to the Go process. Ctrl-C stops both.

### Production-style run

```sh
make build && ./bin/hexagon
```

The binary serves the built frontend itself on `http://127.0.0.1:8080`. Set
`HEXAGON_PUBLIC_URL` to the same address and register a matching OAuth callback.

## Using it

**Images** — a session runs in a container built from a base image you register on the
Images page, either from a Dockerfile or by pulling a registry reference. An image must
provide `git`, `tmux` and `claude` on the `PATH`, and a long-running `CMD`; the one in
[deploy/images/base/Dockerfile](deploy/images/base/Dockerfile) is offered as the starting
point and is the reference for what a session needs.

**Editing a Dockerfile with Claude Code** — under the Dockerfile box, say what to change
("add the Go toolchain") and Hexagon runs `claude -p` on the host to rewrite it, showing
what changed with an undo. The call has every built-in tool removed (`--tools ""`), so it
is a text transformation with no shell, no file access and no fetch of its own, and it
ignores your own Claude Code configuration (`--safe-mode`). It authenticates with the Claude
login configured on the Accounts page, falling back to the host's own login when none is
stored. Without a `claude` binary on the server the control is not shown and the box is
edited by hand, as before.

**Accounts** — repositories come from the accounts you connect on the Accounts page. GitHub is
there already: it is how you signed in. Bitbucket is added with an Atlassian account email and an
API token, created under Atlassian account settings → Security → API tokens with
`read:workspace:bitbucket` and `read:repository:bitbucket`, plus `write:repository:bitbucket` if
Claude Code should push. Both reads are needed because Bitbucket lists the workspaces first and
their repositories one workspace at a time. `read:user:bitbucket` is optional and only decides
whether the account shows its username or its email. App passwords are not supported: Atlassian removed them in July 2026. The
credentials are checked against the call a listing starts from before they are stored, and sealed
with the same key as the GitHub token.

**Sessions** — pick an image, and a repository from any connected account or none at all.
With a repository Hexagon clones it into `<workspace root>/<session id>/repo` on the host;
without one that directory starts empty. Either way it is bind mounted on `/workspace` in a
container running `tmux`. The container runs as you, so files it writes stay yours rather
than root's.

**The provider token** — a session with a repository carries the credentials of the account
the repository came from, unless the box is unticked when it is created; a session without a
repository carries none unless an account is chosen for it. The clone on the host uses them
either way — it could not reach a private repository otherwise — so the choice is only about
what runs inside the container. It cannot be changed afterwards: a container keeps the
environment it was created with. Without the token nothing in the session can fetch or push.

**Starting Claude Code by itself** — a session either opens with `claude` already running in
its tmux or leaves you at a shell prompt. It is a checkbox when you create the session, on
by default, and a switch in the session page afterwards; because it is the command the tmux
session is created with, changing it on a running session takes effect the next time that
session starts. When Claude Code exits you get a shell rather than a terminal that closes.

**Configuring the Claude login** — the Claude card on the Accounts page is where a session's
Claude Code account is set, without a shell on the host. Paste an API key from the Console or
the token `claude setup-token` prints, checked against the CLI before it is stored; or press
**Log in** to open a real terminal running `claude auth login` in a container whose
`$HOME/.claude` is this machine's own, so signing in there writes the same file every
session mounts. A pasted
credential reaches sessions created after it was stored — it is an environment variable, fixed
when the container is created — and outranks the server's configured key; a browser login
reaches an existing session the next time it starts, since the mount is a path resolved at
start time. The card says which of the two a new session will actually use.

**Claude Code in a session** — the host's `~/.claude/.credentials.json` is bind mounted
read-only, and each session gets its own `$HOME/.claude.json` seeded with
`hasCompletedOnboarding` and trust for `/workspace`, so `claude` opens straight into the
repository. Without that file Claude Code sees a machine it has never run on and starts
its first-run onboarding, which reads as being asked to sign in again. Because the
credentials are mounted read-only a session cannot refresh an expired OAuth token: when
that happens, log in again from the Accounts page (or on the host), or set `ANTHROPIC_API_KEY`.

**The terminal** — the session page attaches to the container's tmux session. Closing the
tab only detaches: Claude Code keeps working, and reopening the page finds the session
where you left it. Opening a second tab takes the terminal over from the first. The
**tmux keys** button in the session header opens the shortcuts worth knowing, starting
with how to scroll back through the output.

**Stopping and deleting** — stopping shuts the container down and keeps the clone; starting
brings it back with a fresh tmux, so the previous scrollback is gone. Deleting always
removes the container, and removes the clone on disk only if you tick the box.

**VS Code in the browser** — ticking the box when a session is created gives its container a
published port and a **VS Code** button on the session page, opening `code-server` on
`/workspace` in a new tab. It is a creation-time choice because the mount and the port are
the container: there is no way to add them to one that already exists. The code-server
release itself is not part of any image; it is downloaded once into the data directory the
first time a session asks for it, and upgrading it is deleting that directory so the next
session downloads again. Honestly: code-server answers on the host's loopback interface with
no password of its own — Hexagon's session cookie is what stands in front of it — so any other
process on the machine can reach a running session's editor while it is up.

## Configuration

Every setting can come from a JSON configuration file or from the environment,
and has a default that suits a single user on a developer machine.

| Variable | File key | Default | |
|---|---|---|---|
| `HEXAGON_CONFIG` | — | `~/.config/hexagon/config.json` | Where the configuration file is; `-config <path>` overrides it |
| `HEXAGON_ADDR` | `addr` | `127.0.0.1:8080` | Listen address |
| `HEXAGON_PUBLIC_URL` | `publicUrl` | `http://127.0.0.1:8080` | Origin the browser uses; OAuth callbacks and the origin check derive from it |
| `HEXAGON_INSECURE_HTTP` | `insecureHttp` | — | Set to anything to serve a non-loopback address without https, which the server otherwise refuses to do |
| `HEXAGON_DATA_DIR` | `dataDir` | `~/.local/share/hexagon` | Database and secret key |
| `HEXAGON_WORKSPACE_ROOT` | `workspaceRoot` | `<data dir>/workspaces` | One directory per session, holding its workspace |
| `HEXAGON_GITHUB_CLIENT_ID` | `github.clientId` | — | Required |
| `HEXAGON_GITHUB_CLIENT_SECRET` | `github.clientSecret` | — | Required |
| `HEXAGON_ALLOWED_USERS` | `github.allowedUsers` | — | Required. Who may sign in: comma-separated in the environment, a JSON array in the file. An entry that is a number is a GitHub account id, anything else a login — prefer ids, since a login is released when an account is renamed and can then be claimed by somebody else |
| `HEXAGON_GITHUB_API_URL` | `github.apiUrl` | `https://api.github.com` | Override for GitHub Enterprise, or a stub in development |
| `HEXAGON_BITBUCKET_API_URL` | `bitbucket.apiUrl` | `https://api.bitbucket.org/2.0` | Override, or a stub in development |
| `HEXAGON_SECRET_KEY` | `secretKey` | `<data dir>/secret.key` | 32 bytes, base64. Generated on first run |
| `HEXAGON_DEBUG` | `debug` | — | Set to anything for debug logging |
| `HEXAGON_CLAUDE_CREDENTIALS` | `claude.credentials` | `~/.claude/.credentials.json` | Mounted read-only into session containers (from M4). Empty disables the mount. Also the file a browser login from the Accounts page writes |
| `ANTHROPIC_API_KEY` | `claude.anthropicApiKey` | — | Injected into session containers instead of the credentials mount (from M4) |
| `HEXAGON_CLAUDE_BINARY` | `claude.binary` | `claude` on `PATH`, then `~/.local/bin/claude` | Runs `claude -p` to edit a Dockerfile from the Images page |
| `HEXAGON_CLAUDE_MODEL` | `claude.model` | — | Model for that call; empty leaves the choice to the CLI |
| `HEXAGON_GIT_USER_NAME` | `git.userName` | — | Git identity for clones and container commits (from M3) |
| `HEXAGON_GIT_USER_EMAIL` | `git.userEmail` | — | |
| `HEXAGON_VSCODE_DIR` | `vscode.dir` | `<data dir>/code-server` | Where the code-server release is kept, downloaded once for the machine |
| `HEXAGON_VSCODE_VERSION` | `vscode.version` | the version Hexagon is pinned to | Release fetched when `vscode.dir` is empty |
| `DOCKER_HOST` | `docker.host` | SDK default | Docker Engine endpoint (from M2) |
| `HEXAGON_MAX_SESSIONS_PER_USER` | `limits.maxSessionsPerUser` | `20` | Sessions one account may have at once; creating another answers 429 |
| `HEXAGON_MAX_CONCURRENT_BUILDS` | `limits.maxConcurrentBuilds` | `2` | Image builds one account may have in flight |
| `HEXAGON_PUBLIC_RATE_PER_MINUTE` | `limits.publicRatePerMinute` | `60` | Requests a minute, per client address, to the routes that answer without a session |

### The configuration file

[config.example.json](config.example.json) is a complete one; copy it to
`~/.config/hexagon/config.json`, or keep it anywhere and point `-config` at it.
The server looks for `-config`, then `HEXAGON_CONFIG`, then the default path. A
file named by either of the first two must exist — ignoring a path you asked for
would start the server configured by accident — while the default location is
optional, so no file at all means the environment and the defaults, as before.

Four things are worth knowing:

- **The environment wins over the file**, which wins over the defaults. That
  keeps `make dev` working over whatever file you have, and a one-off override a
  one-off override. An unset variable is not a value and overrides nothing.
- **The file must not be readable by anyone else.** It can hold the OAuth client
  secret, an API key and the key that seals stored GitHub tokens, so the server
  refuses to start on anything looser than `chmod 600`.
- **An unknown key is an error.** A misspelled `allowedUsers` that was quietly
  ignored would be an empty allowlist, which is to say an authentication bypass.
- **A leading `~` is expanded** in `dataDir`, `workspaceRoot`, `claude.credentials`
  and `vscode.dir`.

Values that are `""` or absent fall through to the layer below, with one
exception: `claude.credentials` set to `""` means *no credentials mount*, which
is how you tell a session to use `anthropicApiKey` instead.

The server refuses to start without a client id, a client secret and a non-empty
allowlist. That is deliberate: it drives the Docker socket and holds GitHub
credentials, so an unauthenticated instance is a root shell on this machine.
There is no way to skip the login: the development bypass that used to exist
(`dev.user` / `HEXAGON_DEV_USER`, `dev.githubToken` / `HEXAGON_GITHUB_TOKEN`)
has been removed, and a configuration file that still names them will not start,
because an unknown key is an error. Working on the UI means registering an OAuth
App, which is [step 1 of Getting started](#1-register-a-github-oauth-app) and
takes a minute.

## Testing it by hand

Once signed in, these are the interesting things to try:

```sh
# A GitHub account outside the allowlist: change the value, restart, sign in.
export HEXAGON_ALLOWED_USERS=somebody-else        # → /login?error=not_allowed

# The stored GitHub token is encrypted at rest: this prints hex, never a gho_ token.
sqlite3 ~/.local/share/hexagon/hexagon.db 'select github_login, hex(github_token_enc) from users'

# Start over.
rm ~/.local/share/hexagon/hexagon.db*
```

Sign out from the header and you land back on `/login`; reloading `/` keeps you
there. Sessions last 7 days and are extended whenever a browser uses one that is
more than halfway through its life, so an instance in daily use never signs you
out on the clock.

Your account id is the `id` in `curl https://api.github.com/users/<login>`. A
login on the list still works, and is refused only if this instance has already
seen a different account under it.

Removing a login from `allowedUsers` and restarting ends that account's sessions:
admission is re-checked on every request, and the sessions of anyone no longer on
the list are deleted at startup.

Calling the API by hand takes one more header: a mutating request has to show
that it came from our own pages, and `curl` says nothing about where it is from.

```sh
curl -X POST -H "Content-Type: application/json" -H "Origin: $HEXAGON_PUBLIC_URL" \
  -b "hexagon_session=..." http://127.0.0.1:8080/api/auth/logout
```

For a session, the things worth checking are that a file Claude writes in the
container belongs to you on the host, and that closing the tab does not
interrupt it:

```sh
# after starting a session, from the host
ls -l <workspace root>/<session id>/repo
docker ps --filter label=hexagon.managed=true
```

```sh
make test    # go test ./...
make vet
```

## Layout

```
cmd/hexagon/        entry point: config, database, HTTP server
internal/config/    environment configuration
internal/store/     SQLite: schema migrations and queries
internal/auth/      GitHub OAuth login, session cookies, credential encryption
internal/provider/  what a source of repositories is, and merging the connected ones
internal/github/    GitHub REST client
internal/bitbucket/ Bitbucket Cloud REST client
internal/claudex/   runs `claude -p` for the Dockerfile editor
internal/dockerx/   Docker Engine API: images, and exec attach for the terminal
internal/gitops/    host-side git: cloning a repository into a session workspace
internal/session/   orchestrator: provisioning, lifecycle, reconciliation
internal/httpapi/   routes, middleware, handlers, the terminal WebSocket, SPA serving
web/                Vue 3 + Vite frontend, embedded into the binary at build time
deploy/images/      base image definitions for session containers (from M2)
```

## Security notes

- Hexagon binds to loopback by default and should stay there. Anyone who can
  reach the port and hold a session controls the Docker socket. To publish it,
  put a TLS reverse proxy in front and leave the bind where it is: the server
  refuses to start on a listen address the network can reach unless
  `publicUrl` is https, or `insecureHttp` says the plaintext is deliberate.
- The routes that answer without a session — the health check and the login
  handshake — are rate limited per client address, and the health check tells an
  unauthenticated caller only that the server is up. Behind the proxy the
  address comes from `X-Forwarded-For`, which is believed only because the peer
  is loopback; exposed directly, the header is ignored.
- A mutating API call is refused unless it shows a same-origin signal — a
  matching `Origin`, or `Sec-Fetch-Site: same-origin` — and a JSON content type.
  `SameSite=Lax` is a browser default, not something this server enforces.
- Session cookies are `HttpOnly` and `SameSite=Lax`; only the SHA-256 of the
  cookie value is stored. Over https the cookie is `Secure` and named
  `__Host-hexagon_session`, a prefix the browser enforces so no sibling
  subdomain can overwrite it over plaintext. GitHub tokens are sealed with AES-256-GCM under the
  server key.
- The `repo` scope grants full read/write over your repositories, and that token
  is handed to a container running an autonomous agent. That is the deliberate
  trade-off of the project, and the reason it can be declined per session. The
  same applies to a connected Bitbucket token: a session gets one account's
  credentials at most, chosen when it is created, and never another's.
