# Hexagon

A web UI for running Claude Code sessions in Docker containers. Pick a
repository from your GitHub account and a base image, and Hexagon clones the
repo, starts a container with the clone bind-mounted, runs `tmux` inside it, and
renders that tmux in the browser.

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

**Sessions** — pick a repository from your GitHub account and an image. Hexagon clones the
repository into `<workspace root>/<session id>/repo` on the host, starts a container with
that clone bind mounted on `/workspace`, and runs `tmux` inside it. The container runs as
you, so files it writes stay yours rather than root's.

**Claude Code in a session** — the host's `~/.claude/.credentials.json` is bind mounted
read-only, and each session gets its own `$HOME/.claude.json` seeded with
`hasCompletedOnboarding` and trust for `/workspace`, so `claude` opens straight into the
repository. Without that file Claude Code sees a machine it has never run on and starts
its first-run onboarding, which reads as being asked to sign in again. Because the
credentials are mounted read-only a session cannot refresh an expired OAuth token: when
that happens, sign in again on the host, or set `ANTHROPIC_API_KEY`.

**The terminal** — the session page attaches to the container's tmux session. Closing the
tab only detaches: Claude Code keeps working, and reopening the page finds the session
where you left it. Opening a second tab takes the terminal over from the first.

**Stopping and deleting** — stopping shuts the container down and keeps the clone; starting
brings it back with a fresh tmux, so the previous scrollback is gone. Deleting always
removes the container, and removes the clone on disk only if you tick the box.

## Configuration

Everything is read from the environment. Defaults suit a single user on a
developer machine.

| Variable | Default | |
|---|---|---|
| `HEXAGON_ADDR` | `127.0.0.1:8080` | Listen address |
| `HEXAGON_PUBLIC_URL` | `http://127.0.0.1:8080` | Origin the browser uses; OAuth callbacks and the origin check derive from it |
| `HEXAGON_DATA_DIR` | `~/.local/share/hexagon` | Database and secret key |
| `HEXAGON_WORKSPACE_ROOT` | `<data dir>/workspaces` | One directory per session, holding the repository clone |
| `HEXAGON_GITHUB_CLIENT_ID` | — | Required |
| `HEXAGON_GITHUB_CLIENT_SECRET` | — | Required |
| `HEXAGON_ALLOWED_USERS` | — | Required. Comma-separated GitHub logins allowed to sign in |
| `HEXAGON_GITHUB_API_URL` | `https://api.github.com` | Override for GitHub Enterprise, or a stub in development |
| `HEXAGON_SECRET_KEY` | `<data dir>/secret.key` | 32 bytes, base64. Generated on first run |
| `HEXAGON_DEV_USER` | — | Development bypass, see below |
| `HEXAGON_GITHUB_TOKEN` | — | Personal access token used by the bypass |
| `HEXAGON_DEBUG` | — | Set to anything for debug logging |
| `HEXAGON_CLAUDE_CREDENTIALS` | `~/.claude/.credentials.json` | Mounted read-only into session containers (from M4). Empty disables the mount |
| `ANTHROPIC_API_KEY` | — | Injected into session containers instead of the credentials mount (from M4) |
| `HEXAGON_GIT_USER_NAME` | — | Git identity for clones and container commits (from M3) |
| `HEXAGON_GIT_USER_EMAIL` | — | |
| `DOCKER_HOST` | SDK default | Docker Engine endpoint (from M2) |

The server refuses to start without a client id, a client secret and a non-empty
allowlist. That is deliberate: it drives the Docker socket and holds GitHub
credentials, so an unauthenticated instance is a root shell on this machine.

### Development bypass

To work on the UI without registering an OAuth App:

```sh
export HEXAGON_DEV_USER=your-github-login
export HEXAGON_GITHUB_TOKEN=ghp_...   # personal access token, scope: repo
make dev
```

Every request is then treated as coming from that user, with no login at all.
The identity is still resolved through GitHub, so the token is verified and must
belong to `HEXAGON_DEV_USER`. The bypass refuses to run on anything but a
loopback listen address.

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
there. Sessions last 30 days, so closing and reopening the browser keeps you
signed in.

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
internal/auth/      GitHub OAuth login, session cookies, token encryption
internal/github/    GitHub REST client
internal/dockerx/   Docker Engine API: images, and exec attach for the terminal
internal/gitops/    host-side git: cloning a repository into a session workspace
internal/session/   orchestrator: provisioning, lifecycle, reconciliation
internal/httpapi/   routes, middleware, handlers, the terminal WebSocket, SPA serving
web/                Vue 3 + Vite frontend, embedded into the binary at build time
deploy/images/      base image definitions for session containers (from M2)
```

## Security notes

- Hexagon binds to loopback by default and should stay there. Anyone who can
  reach the port and hold a session controls the Docker socket.
- Session cookies are `HttpOnly` and `SameSite=Lax`; only the SHA-256 of the
  cookie value is stored. GitHub tokens are sealed with AES-256-GCM under the
  server key.
- The `repo` scope grants full read/write over your repositories, and that token
  is handed to a container running an autonomous agent. That is the deliberate
  trade-off of the project.
