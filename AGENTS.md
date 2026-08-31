# AGENTS.md

Conventions for working on Hexagon. Read this before touching the code; it
records the decisions that are already made, so they do not have to be
re-litigated in every change.

Hexagon is a web UI for running Claude Code sessions in Docker containers: a Go
backend, a Vue 3 frontend, shipped as one binary with the frontend embedded.
[README.md](README.md) explains what it does and how to run it;
[plans/milestones/start-plan.md](plans/milestones/start-plan.md) is the original
design and the reasoning behind the architecture.

## The one rule that overrides preference

**Everything in this repository is written in English**: code, identifiers,
comments, commit messages, UI copy, documentation, plans. Conversation with the
user may be in Italian; nothing that lands in a file is.

## Commands

```sh
make dev          # Go server on :8080 + Vite on :5173 (open http://localhost:5173)
make build        # frontend, then the binary into bin/hexagon
make test         # go test ./...
make fmt          # go fmt ./...
make vet          # go vet ./...
make install      # the installed layout into DESTDIR, with PREFIX
make deb          # one .deb for one architecture, built inside Debian
make release      # every release artifact into dist/, plus SHA256SUMS
```

Cutting a release is three steps, and the version is the `VERSION` file and nothing else —
bump it first, because the binary, the package and the asset names all come from it:

```sh
make test && make vet && make release
git tag -a v0.1.0 -m 'hexagon 0.1.0' && git push --tags
gh release create v0.1.0 --title 'hexagon 0.1.0' --notes '...' dist/*
```

`install.sh` finds a release by following the redirect from `/releases/latest` and builds
the asset name from the tag, so **renaming an asset breaks every future install of every
past version**. [plans/installer.md](plans/installer.md) is the reasoning.

Always go through the Makefile for tests. The server embeds `web/dist`, so
`go test ./...` fails on a tree that has never been built; `make test` creates
the placeholder first. Frontend type checking happens in `make build`
(`vue-tsc -b && vite build`) — run it after changing anything under `web/`.

There is no linter beyond `go vet` and the TypeScript compiler, and no
formatter beyond `go fmt`. Match the surrounding style by hand.

## Layout

```
cmd/hexagon/        entry point: config, database, wiring, graceful shutdown
embed.go            embeds web/dist and the reference Dockerfile
internal/config/    environment configuration
internal/store/     SQLite: schema migrations and queries
internal/auth/      GitHub OAuth login, session cookies, credential encryption
internal/provider/  the interface a source of repositories implements, and the merged listing
internal/github/    GitHub REST client
internal/bitbucket/ Bitbucket Cloud REST client
internal/claudex/   runs the Claude Code CLI for the image source editor — the
                    binary on the host, or a container when there is none
internal/composex/  runs the Docker Compose CLI for images that carry a compose file
internal/dockerx/   Docker Engine API behind an interface
internal/gitops/    host-side git: cloning a repository into a session workspace
internal/session/   orchestrator: provisioning, lifecycle, reconciliation
internal/httpapi/   routes, middleware, handlers, terminal WebSocket, SPA serving
web/                Vue 3 + Vite frontend
deploy/images/base/ the reference session image
packaging/          the systemd unit, the OpenRC init script, and the Debian package
plans/              milestone statements and their analyses (see below)
```

`install.sh` and `uninstall.sh` at the root are the second installation method, and the only
one that knows about more than one init system: they detect systemd or OpenRC and route
through four `service_*` functions, so a third one is a case in each rather than a rewrite.
The layout they produce is defined once, by `make install`: the package stages it, the
release tarball is that stage rolled up, and the script unpacks it.

Dependencies stay few and deliberate: `net/http.ServeMux` with method patterns
instead of a router, OAuth written by hand instead of `golang.org/x/oauth2`,
`modernc.org/sqlite` because it needs no cgo. Adding a dependency is a decision
to justify in the change, not a default.

## Comments

This is the most visible convention in the codebase, and the easiest to break.

Comments explain **why**, never **what**. A comment that restates the code is
noise; a comment that records the constraint, the failure it prevents, or the
alternative that was rejected is the reason the next reader does not undo the
decision. Compare:

```go
// Create the file ourselves so it never exists with wider permissions,
// even briefly: it holds sealed GitHub tokens and live session hashes.
f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
```

```go
// Hijack keeps WebSocket upgrades working. Without it this wrapper hides the
// underlying http.Hijacker and the terminal handshake fails with 501.
```

Write full sentences with normal punctuation. Every package has a doc comment
saying what it owns. Every exported identifier has one. Non-obvious constants
carry the reasoning for their value. Unexported helpers get a comment when the
name alone does not carry the intent.

## Go

- Package names are short and lowercase; `dockerx` and `gitops` are wrappers
  around an external surface, and stay that way — no business logic in them.
- Wrap errors with context and `%w`: `fmt.Errorf("read %s: %w", path, err)`. The
  message names the operation that failed, lowercase, no trailing punctuation.
- Sentinel errors are exported from the package that owns the concept
  (`store.ErrNotFound`, `store.ErrConflict`, `session.ErrImageNotFound`) and
  compared with `errors.Is`. The HTTP layer maps them to status codes.
- Every external collaborator sits behind an interface defined by the consumer
  (`dockerx.API`, `session.Cloner`, `session.TokenSource`, `httpapi.RepoLister`)
  so handlers and the orchestrator are testable without a daemon or a network.
- Dependencies are passed in explicitly — `httpapi.Deps` into `httpapi.New`,
  `session.Config` into `session.NewManager`. No globals, no package-level state,
  no init-time magic.
- Logging is `log/slog` with key/value pairs: `log.Error("list sessions", "err", err)`.
  `Debug` for per-request noise, `Info` for lifecycle, `Warn` for a degraded but
  usable state, `Error` for a failure the user will notice.
- Every call that can block takes a `context.Context` and honours it. Background
  work started from a handler gets its own bounded context, not the request's.
- Fail closed. Directories are `0700`, secrets `0600`, the listen address is
  loopback, and the server refuses to start without an allowlist.

## HTTP API

- Routes live in one place, `internal/httpapi/router.go`. Public endpoints are
  registered directly; everything else goes into the `protected` map and is
  wrapped by `requireAuth`. A new endpoint that is not in that map is a bug.
- Handlers are methods on `*Server`, named `handleVerbNoun`, one per file group
  (`handlers_sessions.go`, `handlers_images.go`, ...).
- Responses always go through `writeJSON` / `writeError`. Errors are
  `{"error": "..."}`; the frontend depends on that shape.
- The wire format is its own type: a `sessionResponse` struct with `json` tags
  in camelCase, built by a `newXResponse(*store.X)` constructor. Never serialize
  a `store` model directly — that is how a sealed token ends up in a response.
- Request bodies are decoded through `http.MaxBytesReader` with a named
  `maxXRequestBody` constant.
- Every query is scoped to the authenticated user (`s.user(r).ID`). Multi-user is
  not a goal, but the `WHERE user_id = ?` costs nothing and is not optional.
- Work that takes longer than a request returns `202 Accepted` with the row in a
  provisioning status; the UI polls. There is no SSE, and the only WebSockets
  are the session terminal, the Claude login terminal, and the VS Code proxy —
  the latter forwards a whole other application's socket rather than speaking
  it.

## Store

- One `Store` type owning the `*sql.DB`. Queries are methods on it. Prefer adding
  a method over reaching for `Store.DB()`.
- Migrations are `internal/store/migrations/NNN_description.sql`, versions
  contiguous from 1, applied in order inside a transaction, and verified by a
  test. **An applied migration is never edited** — add the next one.
- Timestamps are `TEXT` in `store.timeLayout` (UTC, fixed width) so the driver
  does not rewrite them and SQL string ordering stays chronological. Use
  `formatTime` / `parseTime`, never a raw layout.
- Status columns are `TEXT` with a `CHECK` constraint, and the allowed values are
  also exported as Go constants (`store.SessionStatusRunning`). Adding a status
  means touching the migration, the constants, and `web/src/status.ts`.
- Lookups that match nothing return `ErrNotFound`; unique violations are
  translated into `ErrConflict` by `isUniqueViolation`.
- Column lists that appear more than once become a `const xColumns` string, with a
  matching `scanX` helper.

## Configuration

Every setting can be given in the JSON configuration file or as an environment
variable prefixed `HEXAGON_`, and has a default that suits one user on a
developer machine. The layers resolve as defaults, then file, then environment.
Adding a setting means, in this order:

1. A field on `config.Config`, grouped with its neighbours, with the variable
   name and the file key in a trailing comment.
2. A field on `config.file` with a camelCase `json` tag, in the matching group.
3. A line in `Load` resolving it with `pick(...)`, or a helper of its own when
   an empty value means something.
4. A row in the README configuration table, and a key in `config.example.json`.

Settings that can be written back from the running server also need a field on
`config.Patch` and a row in its `fields()` table. `secretKey` is deliberately not
one of them: a wrong value cannot be corrected from the page that wrote it.

Required settings are validated where they are used (the OAuth constructor
refuses an empty allowlist), not silently defaulted. Unknown keys in the file are
an error, so a setting that is not in `config.file` cannot be configured at all.

## Frontend

- Vue 3 with `<script setup lang="ts">`, one component per file, PascalCase names.
  Views in `src/views`, reusable pieces in `src/components`.
- `src/api.ts` is the only place that calls `fetch`. Its interfaces mirror the Go
  response structs field for field; when a DTO changes, both sides change in the
  same commit.
- No Pinia and no store library. State is component-local `ref`s, plus small
  modules for the few cross-cutting values (`session.ts` for the current user,
  `status.ts` for status copy).
- Server state is the only authority: the router guard asks `/api/auth/me`, and
  provisioning views poll (`setInterval` on mount, cleared on unmount) rather
  than guessing.
- What a request answered goes in a `Notice`, error or success, from a local
  `message(e)` helper that understands `ApiError`. A notice stays until the user
  dismisses it: a view's polling must never clear one, because a refresh that did
  took the message off the screen a second after it arrived. Clearing when the
  user starts the action again is right; clearing because a background request
  succeeded is not. Text that reports what something *is* — a failed image's
  reason, a session that has gone — is state and stays in the view.
- Styling is plain CSS. Shared tokens and element defaults in `src/style.css`
  (with the dark-mode block), everything else in the component's `<style scoped>`.
  No CSS framework.
- UI copy is sentence case and says what is happening, not what the state is
  called — see the labels in `status.ts`.

## Tests

- Go tests only; there is no frontend test setup, and adding one is a decision,
  not a drive-by.
- Names are sentences: `TestCreateSessionProvisionsAContainer`,
  `TestOpenCreatesDatabaseWithRestrictivePermissions`. The name states the
  behaviour, not the function under test.
- Failure messages read `got = x, want y`, with the reason when it is not obvious:
  `t.Errorf("branch = %q, want the repository's default branch", ...)`.
  `t.Fatalf` when the test cannot continue, `t.Errorf` when it can.
- Integration-style tests build a `testEnv` (see `internal/httpapi`) with fakes
  for Docker, GitHub and the cloner, and drive the real router over `httptest`.
  That is the preferred level: it covers wiring, auth and status codes at once.
- Use `t.TempDir()` for anything on disk. Tests never touch the real Docker
  daemon, the real GitHub, or the user's data directory.
- Asynchronous work is awaited by polling to a deadline (`waitForSessionStatus`),
  never by a bare sleep.

## Security invariants

These are not preferences. A change that breaks one is wrong even if it passes.

- No endpoint outside `/api/health`, the auth handshake and `/api/setup` is
  reachable without a valid session — the terminal WebSocket included.
  `/api/setup` is the first-time wizard, and it is the exception that proves the
  rule: it answers only while no user has ever signed in, and only to a password
  generated for this process and printed in its log. See
  [plans/M2/01-first-time-wizard.md](plans/M2/01-first-time-wizard.md).
- GitHub tokens are stored sealed with AES-256-GCM and never leave
  `internal/auth`. Only the SHA-256 of a session cookie is stored.
- Mutating API calls require a same-origin request and a JSON content type.
- The default listen address stays loopback: whoever reaches the port controls
  the Docker socket.
- Containers run as the host user, and never as root. A user-supplied compose file is
  checked against a list of refusals — privileged, added capabilities, host namespaces,
  host bind mounts, fixed host ports, build, and root — before anything is created from
  it. See [plans/M2/03-complex-builds.md](plans/M2/03-complex-builds.md).

## Plans and milestones

Two directories, two different kinds of document:

- `plans/milestones/<milestone>.md` — the statement of intent, written by the
  user: a numbered list of what the milestone must deliver. Treat it as the
  requirement, and do not rewrite it.
- `plans/<milestone>/` — the analyses this repository produces for that
  milestone, one Markdown file per point.

**Every milestone point gets an analysis in its milestone folder before it is
implemented.** The folder is created with the milestone
(`plans/milestones/M1.md` → `plans/M1/`), and files are named
`NN-slug.md` after the point's number and subject: point 3 of M1, "Switch per
lanciare claude in automatico", becomes `plans/M1/03-auto-claude-switch.md`.
`plans/<milestone>/README.md` indexes the points and tracks their state.

An analysis is written in English and covers, as briefly as the point allows:
what is being asked and how it is being read; the current behaviour it changes;
the design, including the alternatives rejected and why; the concrete impact on
schema, API, configuration and UI; and how it will be verified. It is where the
reasoning goes, so the code can carry only the reasoning a reader needs in place.

## Commits

Short English subject lines describing the change (`session creation`,
`UI for session management`). No conventional-commit prefixes, no body unless
there is something to explain. Commit and push only when asked.
