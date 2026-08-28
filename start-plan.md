# Hexagon — MVP: web-based Claude Code session manager

## Context

`/home/andrea/src/hexagon` is empty: this is a greenfield project. The goal is a web
application (Go backend + Vue 3 frontend) to start and manage isolated Claude Code
sessions running in Docker containers.

Desired flow: from the web UI I pick a repository from my GitHub account and a base
image; the application clones the repo to disk, creates a container from that base
image with the clone bind-mounted, starts `tmux` inside the container, and renders that
tmux in the browser so I can drive Claude Code. Base images are managed by the
application: you register either a Dockerfile to build or a registry reference to pull.

**The whole project — code, identifiers, comments, commit messages, UI copy and
documentation — is written in English.**

The MVP deliberately does **not** address: automatic container lifecycle (when to
stop/remove/GC), resource quotas, log persistence, PR/branch management from the UI.

Host toolchain verified: Go 1.26.4, Node 22.22.2, Docker 29.5.2, `claude` at
`~/.local/bin/claude`, credentials at `~/.claude/.credentials.json`. `tmux` is not
installed on the host and is not needed — it only runs inside containers.

### Decisions

| Topic | Decision |
|---|---|
| Repo source | **User's decision**: the app clones the repo into a per-session host directory using the GitHub token, configures git, records the path in the DB, and bind-mounts that directory into the container |
| Authentication | **Required.** Sign in with GitHub (OAuth App) + username allowlist. The OAuth token obtained at login is also the token used to list and clone repositories — one mechanism covers both needs |
| Claude Code auth | Read-only bind-mount of `~/.claude/.credentials.json`; optional override with an `ANTHROPIC_API_KEY` env var injected into the container. Both configurable |
| Persistence | SQLite via `modernc.org/sqlite` (pure Go, no cgo) |
| Terminal | `docker exec` with a TTY running `tmux new-session -A -s main`, streamed bidirectionally over a WebSocket into xterm.js |

---

## Architecture

```
Browser (Vue 3 + xterm.js)
   │  REST /api/*            WebSocket /api/sessions/{id}/terminal
   ▼
Go server (net/http, single binary with web/dist embedded)
   ├── auth/       GitHub OAuth login, cookie sessions, allowlist
   ├── store/      SQLite: users, user_sessions, images, sessions
   ├── github/     REST client: list the user's repositories
   ├── gitops/     host-side clone with an ephemeral credential helper
   ├── dockerx/    Docker Engine API: build, pull, create, start, exec, attach
   └── session/    orchestrator: session state machine
   ▼
Docker Engine (unix:///var/run/docker.sock)
   └── one container per session
         ├── bind: <workspace>/repo  → /workspace     (rw)
         ├── bind: <workspace>/home  → /home/agent    (rw, HOME)
         ├── bind: ~/.claude/.credentials.json → /home/agent/.claude/.credentials.json (ro)
         ├── cmd: sleep infinity
         └── tmux session "main" started via exec
```

### File layout

```
hexagon/
  go.mod
  start-plan.md                      # copy of this plan, as requested
  README.md
  Makefile
  cmd/hexagon/main.go                # wiring, embeds web/dist, graceful shutdown
  internal/config/config.go          # env-based config + defaults
  internal/store/store.go            # sql.DB, migrations
  internal/store/models.go           # User, UserSession, Image, Session + queries
  internal/store/migrations/001_init.sql
  internal/auth/oauth.go             # GitHub OAuth login/callback
  internal/auth/session.go           # cookie sessions, middleware, allowlist
  internal/auth/crypt.go             # AES-GCM sealing of stored GitHub tokens
  internal/github/client.go          # GET /user, GET /user/repos
  internal/gitops/clone.go           # clone + git config
  internal/dockerx/client.go         # Docker SDK wrapper (behind an interface)
  internal/dockerx/build.go          # ImageBuild from Dockerfile / ImagePull
  internal/dockerx/exec.go           # ExecCreate/Attach/Resize
  internal/session/manager.go        # create/start/stop/delete/reconcile
  internal/httpapi/router.go
  internal/httpapi/handlers_*.go     # auth, sessions, images, github
  internal/httpapi/terminal.go       # WS ↔ hijacked exec connection
  deploy/images/base/Dockerfile      # default base image
  web/                               # Vue 3 + Vite + TS
    src/App.vue, src/router.ts, src/api.ts
    src/views/LoginView.vue, SessionsView.vue, SessionView.vue, ImagesView.vue
    src/components/NewSessionDialog.vue, TerminalPane.vue
```

### Dependencies

Go: `github.com/docker/docker/client`, `github.com/coder/websocket`,
`modernc.org/sqlite`, `github.com/google/uuid`. Routing with `net/http.ServeMux`
(method patterns, Go 1.22+) — no framework. OAuth implemented directly with `net/http`
(two calls: authorize redirect, token exchange); `golang.org/x/oauth2` is optional and
not worth the dependency here.

Frontend: `vue`, `vue-router`, `vite`, `@xterm/xterm`, `@xterm/addon-fit`.
No Pinia in the MVP: state lives in components plus an `api.ts` module.

---

## Authentication

**Requirement**: no API endpoint other than the auth handshake and the health check may
be reached without a valid session. This is not optional polish — the application drives
the Docker socket and holds GitHub credentials, so an unauthenticated endpoint is a root
shell on the host.

### Login flow (GitHub OAuth App)

1. `GET /api/auth/login` — generates a random `state` (32 bytes, base64url), stores it in
   a short-lived `HttpOnly` `SameSite=Lax` cookie, and redirects to
   `https://github.com/login/oauth/authorize?client_id=…&scope=repo&state=…&redirect_uri=…`.
2. `GET /api/auth/callback?code=…&state=…` — verifies `state` against the cookie
   (constant-time compare), exchanges `code` at `https://github.com/login/oauth/access_token`,
   then calls `GET https://api.github.com/user` with the resulting token.
3. **Allowlist check**: the GitHub login must appear in `HEXAGON_ALLOWED_USERS`
   (comma-separated). If the list is empty the server refuses to start — failing closed
   beats shipping an app that anyone with a GitHub account can log into.
4. Upsert the `users` row, storing the GitHub access token sealed with AES-GCM.
5. Create a `user_sessions` row: a random 32-byte token, of which only the SHA-256 hash is
   stored, with a 30-day expiry. Set cookie `hexagon_session`, `HttpOnly`, `SameSite=Lax`,
   `Path=/`, `Secure` when the request is HTTPS. Redirect to `/`.
6. `GET /api/auth/me` returns the current user; `POST /api/auth/logout` deletes the row
   and clears the cookie.

### Enforcement

- `RequireAuth` middleware wraps everything under `/api/` except `/api/auth/*` and
  `/api/health`. It resolves the cookie to a user, rejects expired sessions with `401`,
  and puts the `*User` into the request context.
- The **WebSocket terminal endpoint goes through the same middleware** — browsers send
  cookies on the WS handshake. In addition the handler validates the `Origin` header
  against the configured public URL: without that check any website could open a socket
  to a shell inside the user's containers.
- CSRF: `SameSite=Lax` blocks cross-site form POSTs; mutating handlers additionally
  require `Content-Type: application/json`, which cross-site HTML forms cannot set.
- Ownership: `sessions` and `images` carry a `user_id`; every handler scopes its queries
  to the authenticated user. Multi-user is not an MVP goal, but scoping the queries now
  costs one `WHERE` clause and avoids a migration later.
- Cloning and repo listing use the **logged-in user's** stored OAuth token, not a
  server-wide PAT.

### Dev escape hatch

If `HEXAGON_DEV_USER` is set (dev builds only), the server skips OAuth and treats every
request as coming from that user, whose GitHub token is read from `HEXAGON_GITHUB_TOKEN`.
It logs a loud warning on every startup and refuses to run when the listen address is not
loopback.

---

## Data model

```sql
CREATE TABLE users (
  id                TEXT PRIMARY KEY,
  github_login      TEXT NOT NULL UNIQUE,
  github_id         INTEGER NOT NULL UNIQUE,
  avatar_url        TEXT,
  github_token_enc  BLOB NOT NULL,        -- AES-GCM sealed OAuth token
  created_at        DATETIME NOT NULL,
  last_login_at     DATETIME NOT NULL
);

CREATE TABLE user_sessions (
  token_hash   BLOB PRIMARY KEY,          -- SHA-256 of the cookie value
  user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at   DATETIME NOT NULL,
  expires_at   DATETIME NOT NULL
);

CREATE TABLE images (
  id            TEXT PRIMARY KEY,
  user_id       TEXT NOT NULL REFERENCES users(id),
  name          TEXT NOT NULL,            -- display name; UNIQUE(user_id, name)
  source_type   TEXT NOT NULL,            -- 'dockerfile' | 'registry'
  dockerfile    TEXT,
  registry_ref  TEXT,
  image_ref     TEXT,                     -- resulting tag, e.g. hexagon/img-<id>:latest
  status        TEXT NOT NULL,            -- pending|building|ready|failed
  build_log     TEXT,
  error         TEXT,
  created_at    DATETIME NOT NULL,
  UNIQUE(user_id, name)
);

CREATE TABLE sessions (
  id             TEXT PRIMARY KEY,
  user_id        TEXT NOT NULL REFERENCES users(id),
  title          TEXT NOT NULL,
  repo_full_name TEXT NOT NULL,           -- owner/repo
  repo_clone_url TEXT NOT NULL,
  branch         TEXT,
  image_id       TEXT NOT NULL REFERENCES images(id),
  image_ref      TEXT NOT NULL,           -- snapshot: the image may change later
  workspace_dir  TEXT NOT NULL,           -- <workspaceRoot>/<id> on the host
  repo_dir       TEXT NOT NULL,           -- <workspace_dir>/repo  ← the tracked path
  container_id   TEXT,
  status         TEXT NOT NULL,           -- creating|cloning|starting|running|stopped|failed|gone
  error          TEXT,
  created_at     DATETIME NOT NULL,
  updated_at     DATETIME NOT NULL
);
```

`sessions.status` is the persisted view; the *actual* container state is always re-read
via `ContainerInspect` on GET and reconciled.

---

## Implementation milestones

### M0 — Scaffolding — **done**

1. ~~Write `start-plan.md` at the project root.~~ Done.
2. `go mod init github.com/andrea/hexagon`; scaffold the frontend with
   `npm create vite@latest web -- --template vue-ts`.
3. `internal/config`: env-based config with defaults —
   `HEXAGON_ADDR` (`127.0.0.1:8080`), `HEXAGON_PUBLIC_URL` (`http://127.0.0.1:8080`),
   `HEXAGON_DATA_DIR` (`~/.local/share/hexagon`),
   `HEXAGON_WORKSPACE_ROOT` (`<data_dir>/workspaces`),
   `HEXAGON_GITHUB_CLIENT_ID`, `HEXAGON_GITHUB_CLIENT_SECRET`, `HEXAGON_ALLOWED_USERS`,
   `HEXAGON_SECRET_KEY` (32 bytes base64; generated into `<data_dir>/secret.key` with mode
   `0600` if absent), `HEXAGON_CLAUDE_CREDENTIALS` (`~/.claude/.credentials.json`, empty
   disables the mount), `ANTHROPIC_API_KEY`, `HEXAGON_GIT_USER_NAME`,
   `HEXAGON_GIT_USER_EMAIL`, `DOCKER_HOST`, `HEXAGON_DEV_USER`.
   Default bind is loopback: the app controls the Docker socket, so exposing it on a
   network is equivalent to handing out root on the host.
4. `internal/store`: open SQLite (`journal_mode=WAL`, `busy_timeout=5000`), apply
   migrations from an `embed.FS` at startup, create the DB file with mode `0600`.
5. `cmd/hexagon`: `//go:embed all:web/dist` served as an SPA fallback on `/`, API under
   `/api/`, `GET /api/health`. In development the Vite dev server proxies `/api` to `:8080`.

### M1 — Authentication — **done**

Implement `internal/auth` and the endpoints described in the Authentication section:
`GET /api/auth/login`, `GET /api/auth/callback`, `GET /api/auth/me`,
`POST /api/auth/logout`, plus the `RequireAuth` middleware and the `Origin` check helper
used by the terminal handler.

`internal/auth/crypt.go`: `Seal/Open` over AES-256-GCM with a random nonce prefix, key
from `HEXAGON_SECRET_KEY`. Used for `users.github_token_enc`.

`LoginView.vue` with a single "Sign in with GitHub" button; the router guard calls
`/api/auth/me` on boot and redirects to `/login` on `401`; `api.ts` redirects to `/login`
whenever any response is `401`.

This milestone comes before anything that touches GitHub or Docker, so no endpoint is
ever reachable unauthenticated, not even transiently during development.

### M2 — Image management

`internal/dockerx`:
- `Build(ctx, dockerfile, tag string, logs io.Writer) error`: builds an in-memory tar with
  a single `Dockerfile` entry, calls `ImageBuild`, decodes the JSON stream line by line
  (`{"stream":…}`, `{"errorDetail":…}`) into `logs`.
- `Pull(ctx, ref string, logs io.Writer) error`: `ImagePull` plus the same decoding.

`internal/httpapi/handlers_images.go`: `GET /api/images`, `POST /api/images`,
`DELETE /api/images/{id}`, `GET /api/images/{id}/log`.
`POST` inserts the row as `building` and spawns a goroutine that builds or pulls,
accumulates the log and flips `status` to `ready`/`failed`. Generated tag:
`hexagon/img-<short-id>:latest` for Dockerfiles; for registry sources `image_ref` is the
pulled reference. The UI polls the log endpoint every second while `status=building` — no
SSE in the MVP.

`deploy/images/base/Dockerfile` — the reference base image, pre-filled in the UI textarea:

```dockerfile
FROM node:22-bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      git tmux ca-certificates curl less ripgrep procps openssh-client \
    && rm -rf /var/lib/apt/lists/*
RUN npm install -g @anthropic-ai/claude-code
# askpass reading GITHUB_TOKEN from the environment: no token ever hits the disk
RUN printf '#!/bin/sh\ncase "$1" in *Username*) echo x-access-token ;; *) echo "$GITHUB_TOKEN" ;; esac\n' \
      > /usr/local/bin/git-askpass && chmod +x /usr/local/bin/git-askpass
ENV GIT_ASKPASS=/usr/local/bin/git-askpass
WORKDIR /workspace
CMD ["sleep", "infinity"]
```

**Valid base image requirements**, documented in the UI: `git`, `tmux` and `claude` must
be on the `PATH`, and the image must run as a non-root-agnostic user (see the UID note in
M4). Optional post-build check: `docker run --rm <ref> sh -c 'command -v git tmux claude'`.

### M3 — GitHub and cloning

`internal/github/client.go`: `ListRepos(ctx, token)` →
`GET https://api.github.com/user/repos?per_page=100&sort=updated&affiliation=owner,collaborator,organization_member`,
`Authorization: Bearer <token>`, pagination via the `Link` header. Kept fields:
`full_name`, `clone_url`, `default_branch`, `private`, `updated_at`, `description`.
In-memory cache keyed by user with a 60s TTL. Exposed as `GET /api/github/repos`.

`internal/gitops/clone.go`:

```go
func Clone(ctx context.Context, cloneURL, branch, dest, token string) error
```

Runs `git clone` through `os/exec` with an ephemeral credential helper passed on the
command line, so **the token never lands in `.git/config` or on disk**:

```
git -c credential.helper= \
    -c credential.helper='!f(){ echo username=x-access-token; echo "password=$HEXAGON_GH_TOKEN"; }; f' \
    clone --branch <branch> <cloneURL> <dest>
```

with `HEXAGON_GH_TOKEN` in the process environment. After cloning, set `user.name` and
`user.email` in the repo from config (falling back to the host's global values). No local
`credential.helper` is persisted — inside the container `GIT_ASKPASS` plus the
`GITHUB_TOKEN` env var handle authentication for pushes.

### M4 — Session creation

`internal/session/manager.go` — `Create(ctx, user, req) (*Session, error)`: synchronous
through the DB insert, then asynchronous with `status` updates.

1. Generate `id` (short uuid v4). `workspace_dir = <workspaceRoot>/<id>`; create
   `<workspace_dir>/repo` and `<workspace_dir>/home/.claude`.
2. `status=cloning` → `gitops.Clone(...)` into `<workspace_dir>/repo`, using the
   authenticated user's decrypted GitHub token.
3. `status=creating` → `ContainerCreate`:
   - `Image`: the selected image's `image_ref`
   - `Cmd`: `["sleep","infinity"]`
   - `User`: `"<uid>:<gid>"` of the user running the server (`os.Getuid/Getgid`) — required
     so files Claude writes into the bind mount stay owned by the host user
   - `Env`: `HOME=/home/agent`, `TERM=xterm-256color`, `GITHUB_TOKEN=<user token>`,
     `GIT_AUTHOR_NAME/GIT_AUTHOR_EMAIL/GIT_COMMITTER_*`, plus `ANTHROPIC_API_KEY` if configured
   - `WorkingDir`: `/workspace`
   - `Labels`: `hexagon.session.id=<id>`, `hexagon.managed=true` — the key to reconciliation
   - `HostConfig.Binds`: `<workspace>/repo:/workspace`, `<workspace>/home:/home/agent`,
     and if configured `<claudeCredentials>:/home/agent/.claude/.credentials.json:ro`
   - `HostConfig.RestartPolicy`: `unless-stopped`
4. `status=starting` → `ContainerStart`, then a non-interactive bootstrap exec:
   ```sh
   git config --global --add safe.directory /workspace
   tmux has-session -t main 2>/dev/null || tmux new-session -d -s main -c /workspace
   ```
5. `status=running`, persist `container_id`.

On failure at any step: `status=failed` with `error` populated, the container removed if
it was already created, and the workspace left on disk for inspection.

`Reconcile(ctx)` at server startup: `ContainerList` filtered on `hexagon.managed=true`,
cross-referenced with the `sessions` table — sessions whose container is gone become
`gone`; orphan containers are logged, never removed automatically.

Endpoints: `POST /api/sessions`, `GET /api/sessions`, `GET /api/sessions/{id}`,
`POST /api/sessions/{id}/start`, `POST /api/sessions/{id}/stop`,
`DELETE /api/sessions/{id}?purge=true|false` (with `purge`, the workspace directory is
removed as well). All scoped to the authenticated user.

### M5 — Terminal

Backend `internal/httpapi/terminal.go` — `GET /api/sessions/{id}/terminal` (WS upgrade,
behind `RequireAuth`, with the `Origin` check):

1. `ContainerExecCreate` with `Tty: true`, `AttachStdin/Stdout/Stderr: true`,
   `Cmd: ["tmux","new-session","-A","-s","main","-c","/workspace"]`,
   `Env: ["TERM=xterm-256color"]`. `new-session -A` attaches if the session exists and
   creates it otherwise: idempotent and resilient.
2. `ContainerExecAttach` → a `HijackedResponse` carrying a raw `net.Conn` (with a TTY there
   is no stdcopy multiplexing, so the stream is already clean).
3. Two goroutines: `Conn → WebSocket` (**binary** frames) and `WebSocket → Conn` (binary
   frames are keystrokes; **text** frames are control JSON,
   `{"type":"resize","cols":N,"rows":M}` → `ContainerExecResize`). Closing the WebSocket
   ends the exec but **leaves tmux running**, so Claude Code keeps working and reloading
   the page re-attaches to the same session.
4. Ping/pong every 30s to survive long idle periods behind proxies.

Frontend `components/TerminalPane.vue`: `@xterm/xterm` with `FitAddon`, a `ResizeObserver`
that sends the `resize` message (100ms debounce), and automatic reconnection with backoff
on close.

Accepted MVP limitation: tmux sizes to the smallest attached client, so two browsers with
different window sizes see the smaller geometry. That is standard tmux behaviour; no
workaround in the MVP.

### M6 — Frontend and polish

- `SessionsView.vue`: card grid (repo, branch, image, status dot, created-at) with
  Open / Stop / Start / Delete actions. Polls `GET /api/sessions` every 3s.
- `NewSessionDialog.vue`: repo select with text filter over the GitHub list, branch field
  (defaults to the repo's `default_branch`), image select (only `ready` ones); submit
  redirects to the session view, which shows the intermediate states.
- `SessionView.vue`: header with repo/branch/status plus a full-height terminal.
- `ImagesView.vue`: image list with status, creation form (Dockerfile/Registry radio,
  textarea pre-filled from `deploy/images/base/Dockerfile`), build-log viewer.
- `README.md`: how to register the GitHub OAuth App (callback
  `http://127.0.0.1:8080/api/auth/callback`, scope `repo`), the environment variables, and
  `make dev`.
- `Makefile`: `dev` (Vite + `go run` in parallel), `build` (vite build → `go build`), `test`.

---

## Verification

**Automated tests** (targeted, not exhaustive):
- `internal/auth`: `state` mismatch is rejected; a user outside the allowlist is rejected;
  an expired `user_sessions` row yields `401`; `RequireAuth` returns `401` with no cookie;
  `Seal`/`Open` round-trip.
- `internal/gitops`: clone a local repo created with `git init --bare` in `t.TempDir()`,
  assert `dest/.git` exists and that the token appears nowhere under `.git/`.
- `internal/store`: migrations plus CRUD round-trip against a DB in `t.TempDir()`.
- `internal/httpapi`: session handlers against a fake Docker implementation — define
  `dockerx.API` as an interface precisely so this is possible.

**Manual end-to-end run** — this is the MVP acceptance criterion:
1. Register the OAuth App, export the config vars, `make dev`, open `http://127.0.0.1:8080`.
2. Unauthenticated: `curl -i localhost:8080/api/sessions` returns `401`. The browser lands
   on the login page.
3. Sign in with GitHub → redirected back and authenticated; a GitHub account not in
   `HEXAGON_ALLOWED_USERS` is refused.
4. Images page → new image from the default Dockerfile → the build log streams and the
   status reaches `ready`. Cross-check with `docker images | grep hexagon/`.
5. New session → the repo appears in the list → pick an image and create it.
6. Host checks: `ls <workspaceRoot>/<id>/repo` shows the clone;
   `docker ps --filter label=hexagon.managed=true` shows the container.
7. In the web terminal: `tmux ls` shows `main`, `pwd` is `/workspace`, `git status` is
   clean, `claude --version` responds.
8. Run `claude` and have it make a trivial edit; verify **from the host** that the file
   changed and is owned by the host user, not root.
9. Close the tab while Claude is working, then reopen the session: the terminal re-attaches
   to the same state. This is the key test of the tmux architecture.
10. Resize the browser window: the tmux/Claude layout adapts.
11. Stop → the container stops; Start → it comes back and the terminal re-attaches (tmux is
    recreated by the bootstrap; the previous scrollback is gone — expected).
12. Delete with `purge=true` → container and workspace are gone; without purge the
    workspace remains.
13. Restart the server with live sessions → the list stays consistent (reconciliation works).
14. `POST /api/auth/logout` → subsequent API calls return `401` and the terminal WebSocket
    is refused.

---

## Out of scope (explicit)

Automatic container stop/GC, resource quotas, real multi-user administration (roles,
invitations), persistent log streaming, PR/branch management from the UI, notifications.
To be revisited once the MVP runs.

## Known risks

- **Attack surface**: whoever reaches the HTTP port and holds a valid session controls the
  Docker socket. Authentication plus the loopback bind are the mitigations; both are
  documented in the README.
- **Shared Claude credentials**: every session mounts the same
  `~/.claude/.credentials.json`. If token refresh across containers conflicts, switch to
  `ANTHROPIC_API_KEY` (already supported by the config).
- **UID mismatch**: solved by running the container with the host UID/GID; a base image
  that requires root will break — documented under the image requirements.
- **OAuth token scope**: the `repo` scope grants full read/write over the user's
  repositories, and that token is handed to a container running an autonomous agent. It is
  the intended trade-off for an MVP whose whole point is letting Claude work on the repo.
