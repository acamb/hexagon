# M3.3 — Export workspace

## What was asked

> Dalla pagina delle sessioni deve essere possibile esportare il workspace
> montato sulla sessione tramite un bottone "Export /workspace" sulla card della
> sessione.
> L'export e' un file workspace.tar.gz che viene scaricato e non rimane su
> hexagon, se la cartella "/workspace" non esiste nella sessione viene segnalato
> all'utente subito prima di tentare il backup.

## Reading of the request

The smallest point of the milestone, and the one with the clearest constraint in
it: **the file does not stay on Hexagon.** That single clause decides the whole
design. Point 2 builds its archive in the background and leaves it under
`DataDir` because a `docker save` is gigabytes and takes minutes; this one says
the opposite, and it can, because a repository clone is not that.

So this streams. No job, no row, no janitor, nothing to clean up afterwards.

The phrase worth reading slowly is "il workspace montato sulla sessione" — the
workspace **mounted on** the session. That is exactly what it is: the container's
`/workspace` is not storage the container owns, it is a host directory bind
mounted into it. Which means the export has a choice of where to read from, and
the choice was settled with the user: **from the host.**

That is a divergence from the literal wording of the second sentence, and it is
recorded as one. "Se la cartella /workspace non esiste nella sessione" describes
a check inside the container; the check this implements is on the host directory
that *is* that folder. The two answer the same question for every session that
has one, and the host-side check goes on answering it after the container is
gone — which is when somebody most wants their work out. The milestone statement
is left as the user wrote it; this paragraph is the record of the reading.

## Current behaviour

`internal/session/manager.go` — a session's workspace is
`<WorkspaceRoot>/<session id>`, and the directory bind mounted into the container
is `RepoDir`, `<workspace>/repo`:

```go
binds := []string{
    session.RepoDir + ":" + dockerx.WorkspaceMount,   // /workspace
    homeDir + ":" + dockerx.AgentHome,                // /home/agent
}
```

Both paths are columns on the session row (`WorkspaceDir`, `RepoDir`), and
`repoDir` is already on the wire in `sessionResponse`. A session created without
a repository gets the same directory, made empty with `os.MkdirAll(…, 0o700)`,
so "has a workspace" and "has a repository" are not the same question.

`internal/session/lifecycle.go` — `removeWorkspace` is the precedent for touching
a path that came out of the database, and its comment says why it is careful:

```go
// removeWorkspace deletes a session's directory, after checking it really is
// one. The path comes from the database, and this is a recursive delete: if a
// row is ever corrupted, it must fail rather than take the rest of the disk.
```

It resolves both sides with `filepath.Abs` and refuses anything that is not
strictly inside `WorkspaceRoot`. The same guard shape is what this point needs,
for a read rather than a delete.

`internal/dockerx` has no `CopyFromContainer` and, after this point, still will
not. `internal/codeserver` shells out to `tar -xz`, which is the only archive
handling in the tree; nothing anywhere writes an archive into an HTTP response.

`web/src/views/SessionsView.vue` — the card's action row is Open / Stop or Start
/ Ports / Account / Delete, with a per-session `busy` id and a `confirming`
state, refreshed every three seconds.

## Design

### The archive is built from the host directory, not from the container

`CopyFromContainer` is the obvious call and it is the wrong one. It works only
while a container exists, and the moment somebody reaches for this button is
often the moment after one stopped existing: a session that failed, a session
marked `gone`, a session stopped last week whose image has since been deleted.
Reading the host path works in every one of those cases and needs no Docker round
trip at all — no daemon call, no exec, nothing to go wrong between two processes.

It also produces a better archive. `CopyFromContainer` yields an uncompressed tar
rooted at the copied path, which would then have to be re-framed and gzipped
anyway; walking the directory produces the paths the user expects directly.

The one thing the host path cannot see is a file the container wrote somewhere
other than `/workspace`, and that is correct: the button says `/workspace`.

### It streams, and nothing is staged

`GET /api/sessions/{id}/workspace` writes `application/gzip` with a
`Content-Disposition: attachment; filename="workspace.tar.gz"` and then walks the
tree straight into a `gzip.Writer` wrapping the response. The bytes never touch
the server's disk, which is what the statement asks for.

Two costs, both real and both accepted:

- **There is no `Content-Length`, so there is no progress bar** and no resume.
  The response is chunked and the browser shows an indeterminate download. Point
  2 pays for a progress bar with a staged file; this point is explicitly not
  allowed to.
- **A failure part-way through truncates the download.** The header has already
  gone out, so there is no status code left to change. What saves this from being
  a silent corruption is gzip: the trailer never gets written, so `tar -xzf`
  refuses the file with a checksum error rather than handing over most of a tree
  and no warning. The server logs the real error at `Error`; the user sees a file
  that will not open, which is the honest outcome.

### The warning the statement asks for is its own endpoint

`GET /api/sessions/{id}/workspace/info` answers
`{"exists": true, "files": 1243, "bytes": 48123904}`, and the card calls it when
the button is pressed. Only if `exists` is true does the browser navigate to the
download; otherwise a `Notice` says the session has no workspace directory, and
nothing is attempted. "Segnalato all'utente subito prima di tentare il backup" is
precisely that sequence — the check happens on the click, not on page load, so
what the user is told is true at the moment they asked.

The alternative is a field on `sessionResponse`, and it is rejected because
`SessionsView` polls every three seconds: it would `stat` and walk a directory
for every session on every poll, forever, to answer a question asked twice a
month.

`bytes` is the sum of the regular files' sizes — what the tree occupies before
compression, not after — and the UI says so rather than promising a download
size it cannot know without doing the work twice.

### Symlinks are archived, not followed

A symlink inside the workspace is the way this endpoint turns into "read any file
the server process can read". `node_modules/.bin` is full of them and so is any
repository that has been built once.

So the walk does not follow them: a symlink is written into the tar as a symlink
member, with its target as recorded. That is both the safe answer and the correct
one — the extracted tree then has the same links the original had, and a link
that pointed outside the workspace still points outside it, which is the
original's own business and not the archive's.

Everything that is not a regular file, a directory or a symlink — sockets, fifos,
devices — is skipped, and the count of skipped entries goes in the log. A git
repository has none of them; a workspace where somebody ran a database might.

### The path still has to be checked

`WorkspaceDir` and `RepoDir` come out of the database, and this is a read of a
whole directory tree. The same guard `removeWorkspace` applies is applied here:
both paths resolved with `filepath.Abs`, and anything that is not strictly inside
`WorkspaceRoot` refused with an error naming both. A corrupted or hand-edited row
must fail rather than tar the disk.

Cheap, and the reason it is not optional is that the consequence of skipping it
is the entire filesystem in a `.tar.gz` the user then downloads.

### A running session is tarred live

The container is writing to the same directory while the walk reads it. A file
created after the walk passed its directory is missing; a file being written when
the walk reaches it lands half-written. That is what tarring a live tree means,
and it is what `tar` on any machine does.

The alternative is stopping the container first, which turns "export my work"
into "end my session", and that is a worse answer than a sentence under the
button. The sentence is there.

### Which sessions get the button

Any session whose `RepoDir` exists — which includes `stopped`, `failed` and
`gone`, deliberately. The button is not offered while the status is `creating` or
`cloning`: the clone is in flight, the tree is half a repository, and an archive
of it would be worse than none.

A session whose workspace was purged on delete no longer exists as a row, so the
question does not arise.

### Alternatives rejected

- **`CopyFromContainer`.** Above: only works while a container does, and yields
  a tar that has to be re-framed anyway.
- **Staging the archive like point 2.** The statement forbids it in so many
  words, and a clone does not need it.
- **`exec tar -czf -` inside the container.** It needs a running container, it
  needs `tar` in the image, and it puts the archive's correctness in the hands of
  whatever userland the user's Dockerfile installed.
- **Shelling out to the host's `tar`.** `archive/tar` and `compress/gzip` are in
  the standard library and already do this; `gitops` and `composex` shell out
  because there is no library alternative, which is not the case here.
- **Including `/home/agent`.** Shell history, the Claude Code state and the
  credentials mount live there. The button says `/workspace`, and a credential
  file in an export nobody expected to contain one is not a feature.
- **A field on `sessionResponse` instead of the probe.** Above: a directory walk
  per session per poll.

## API

Two endpoints, both in the `protected` map, both scoped to the authenticated
user through the existing `sessionOr404`:

- `GET /api/sessions/{id}/workspace/info` →
  `{"exists": true, "files": 1243, "bytes": 48123904}`. `exists` is false with
  the other two absent when there is no directory. 409 while the session is
  `creating` or `cloning`.
- `GET /api/sessions/{id}/workspace` → the archive, or 404 as JSON when the
  directory is gone between the probe and the download.

Both are GET, which matters twice: `guardStateChanges` returns early for safe
methods, so the download needs no `Origin` header and no JSON content type, and
the `SameSite=Lax` cookie rides a top-level navigation. That is what lets the
browser side be an `<a download>` rather than a `fetch` into a `Blob` — the same
decision point 2 records for the same reason, and the two should stay the same
shape.

`internal/session` gains a `workspace.go` with the guard, an info function and a
writer:

```go
func (m *Manager) WorkspaceInfo(session *store.Session) (WorkspaceInfo, error)
func (m *Manager) WriteWorkspaceArchive(ctx context.Context, session *store.Session, w io.Writer) error
```

The writer takes a `context.Context` and honours it, so a browser that cancels
the download stops the walk instead of reading a tree into a socket nobody is
listening to.

Nothing is added to `dockerx.API`, which is worth saying out loud: this is a
session feature that needs no daemon.

## Configuration

None. `WorkspaceRoot` is already a setting and is the only path involved.

## UI

- An **Export /workspace** button in the card's action row in
  `SessionsView.vue`, using the existing per-session `busy` id while the probe is
  in flight. The same button on `SessionView.vue`, where somebody looking at a
  session will expect it.
- On click: the probe, then either the navigation or a `Notice` saying this
  session has no workspace directory. The notice stays until dismissed — the
  three-second refresh must not clear it, which is the rule AGENTS.md states and
  the reason it states it.
- Under the button, in the session view rather than on the card: what the archive
  contains and what it does not — `/workspace` only, not the home directory, not
  a compose project's volumes — and that a running session is archived while it
  is being written to.
- `api.ts` gains `api.sessions.workspaceInfo(id)` through the ordinary
  `request<T>`, and `api.sessions.workspaceUrl(id)` returning a path string, in
  the shape of `api.claude.loginTerminal`. The download itself is never a
  `fetch`.

## Impact

| Area | Change |
|---|---|
| Schema, Config, dockerx | none |
| session | `workspace.go`: the path guard, `WorkspaceInfo`, `WriteWorkspaceArchive` |
| API | `GET /api/sessions/{id}/workspace/info` and `GET /api/sessions/{id}/workspace` |
| UI | the card and session-view button, the probe, the notice, two `api.ts` helpers |
| Docs | README: what the export contains, and that it is taken live |

## Implementation steps

Four steps, and the first one is most of the work.

**1 — `internal/session/workspace.go`.** The guard, `WorkspaceInfo`,
`WriteWorkspaceArchive`, and the tests below. No route yet, so it can be reviewed
purely as "does this walk do the right thing". Reviewable question: is there any
tree shape that makes it read outside `WorkspaceRoot`.

**2 — The two handlers and their routes.** `handleWorkspaceInfo` and
`handleExportWorkspace` in `handlers_sessions.go`, the `protected` entries, the
info DTO, and the status refusals. Reviewable question: is the error path before
the first byte a JSON status code, and after it a logged truncation.

**3 — The UI.** The button in both places, the probe-then-navigate sequence, the
notice, the explanatory copy, the two `api.ts` helpers. Reviewable question: does
the user find out there is no workspace *before* a download starts.

**4 — README.** A paragraph on the export: what is in it, what is not, and that
it is a live read.

## Verification

`internal/session` — over `t.TempDir()`: a tree with nested directories,
an empty directory, a file with no read permission and a symlink round-trips
through `WriteWorkspaceArchive` and `tar`'s own reader with the paths, the modes
and the symlink target intact; the symlink is a symlink member and its target was
never opened; a fifo is skipped rather than failing the walk; a session whose
`RepoDir` is outside `WorkspaceRoot` is refused with an error naming both paths,
and one whose `RepoDir` is `WorkspaceRoot` itself is refused too — that is the
`removeWorkspace` boundary case; a cancelled context stops the walk; and
`WorkspaceInfo` reports `exists` false for a missing directory and a byte count
matching the sum of the regular files for one that is there.

`internal/httpapi`, in the `testEnv` style over the real router — both endpoints
are 401 without a session and 404 for another user's session; the info endpoint
is 409 while the session is `cloning`; the download carries
`Content-Disposition: attachment; filename="workspace.tar.gz"` and a body that
untars to the fixture tree; a session with no workspace directory answers 404 as
JSON before any archive bytes are written; and nothing is left in the data
directory afterwards, which is the assertion that says "non rimane su hexagon"
holds.

By hand: `make dev`, create a session, write a file into `/workspace` from its
terminal, press the button, and confirm the file downloads and `tar tzf` shows
the repository. Then stop the session and export again — same result, which is
the whole argument for reading the host path. Then `rm -rf` the session's
`repo` directory on the host and press the button: the message arrives and no
download starts.

## The security invariants this touches

Both routes are in the `protected` map and both go through `sessionOr404`, so a
session belonging to another user is a 404 like every other session endpoint.

The one this point comes closest to is not in the AGENTS.md list, and it is worth
naming anyway: **an endpoint that reads a path out of the database and streams
whatever is under it is a file-disclosure bug waiting for a bad row.** Two things
keep it from being one — the `WorkspaceRoot` containment check, which is the same
refusal `removeWorkspace` already makes, and not following symlinks, which is the
same problem arriving by a different door. Neither is optional and both are
tested above.

Nothing else moves: no new Docker capability, no new content type admitted
through `guardStateChanges`, no configuration, and no data at rest. The export is
a read of files the server process could already read, handed to the user who
owns them.
