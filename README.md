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
| M2 — base image management | not started |
| M3 — repository listing and cloning | not started |
| M4 — session containers | not started |
| M5 — browser terminal | not started |
| M6 — frontend and polish | not started |

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
internal/httpapi/   routes, middleware, handlers, SPA serving
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
