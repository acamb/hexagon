# M3.2 — Backup and restore of images

## What was asked

> Dalla pagina Images deve essere possibile fare il backup e restore di
> un'immagine, selezionando cosa salvare:
>     * Docker file / compose
>     * export dell'immagine
>
> Allo stesso modo durante il restore sara' possibile scegliere se prendere dal
> backup solo il dockerfile / compose o anche l'export dell'immagine.
> Se viene scelto solo dockerfile/compose, vengono caricati nella form di
> creazione dell'immagine per fare la build,altrimenti viene importata in
> hexagon l'immagine ed i file ed agganciati correttamente alle strutture del DB
> (se esiste gia' un'immagine con quel nome viene sovrascritta, previa conferma)
>
> Per il formato del backup fai un .tar.gz contenente una cartella spec/ con il
> dockerfile/compose e poi un image.tar.gz nella root del .tar.gz del backup.

## Reading of the request

An image in Hexagon is two things that live in two different places: a **spec**
— the Dockerfile, the compose file, the name, the source type — which is a few
kilobytes of text in SQLite, and a **built image**, which is a few gigabytes of
layers in the Docker daemon. The statement is precise about this and treats them
separately in both directions: you choose what to save, and you choose what to
take back out.

That is the whole shape of the feature. A spec-only backup is a portable recipe:
small, readable, and it rebuilds to whatever the base image and the package
mirrors say today. A full backup is the image as it actually is: large, exact,
and immune to a `RUN apt-get` line that stopped working last month. Neither is
the right one always, which is why the statement asks for the choice rather than
picking.

The restore side has a detail worth reading carefully. **Spec-only restore does
not create an image; it fills in the create form.** The user then presses Create
and the ordinary build runs. Full restore is the opposite: nothing is built,
the image is loaded into the daemon and wired into the database — "agganciati
correttamente alle strutture del DB" — so that a session can be created from it
at once.

Two decisions taken with the user before writing:

- **A backup is a job and a file, not a stream.** `docker save` of a development
  image is routinely multiple gigabytes. The archive is built in the background
  under `DataDir`, the page polls, and the download follows. The cost — a large
  file living on the server for a while — is paid deliberately, and there is a
  janitor below to make it temporary.
- **Restore is symmetric**: the upload is staged first, inspected, and only then
  imported.

## Current behaviour

`internal/store/images.go` — an image row holds `dockerfile`, `compose`,
`registry_ref` and `image_ref` as columns. **The spec is in the database and
nowhere on disk**; the only copy that ever reaches a filesystem is per session,
`<workspace>/compose/user.yaml`, written by `internal/session/compose.go`.
`UNIQUE (user_id, name)` is translated to `store.ErrConflict` and mapped to 409.

`internal/httpapi/handlers_images.go` — `imageTag(id)` derives
`hexagon/img-<first 12 hex of the uuid>:latest`, so **the tag is a function of
the row id**. `startImageBuild` is the precedent for a long job: a goroutine on
`context.Background()` with its own timeout, writing progress through a
`logSink` that `flushBuildLog` drains into `build_log`, with the handler
answering 202 and the page polling `GET /api/images/{id}/log`. The preflight
order is compose validation, then `CountBuildingImages` against
`MaxConcurrentBuilds` (429), then `s.docker.Ping` (503), then the insert.

Migration `012_session_image_digest.sql` added `sessions.image_digest`: the
content-addressable id pinned when a container is created, precisely because the
tag moves under a rebuild. It matters here for the same reason.

`internal/dockerx` — no `ImageSave`, no `ImageLoad`, no `ImageTag`. The SDK that
provides all three is already linked in. `decodeProgress` already turns Docker's
JSON progress stream into a log writer, and `ImageLoad` answers with exactly that
stream.

**Nothing in this codebase uploads or downloads a file.** No `multipart`
anywhere, no `Content-Disposition`, no `http.ServeContent`. Every request body
goes through `decodeJSON` with an `http.MaxBytesReader`, and
`guardStateChanges` (`internal/httpapi/middleware.go`) refuses any mutating
`/api/` request whose content type is not `application/json`, with
`isVSCodeProxyPath` as the single carve-out. On the other side, `api.ts`'s
`request<T>` always parses the response as JSON.

## Design

### The format is the statement's, and the manifest is the one addition

```
hexagon-<name>-<timestamp>.tar.gz
├── spec/
│   ├── image.json
│   ├── Dockerfile        (when the image has one)
│   └── compose.yaml      (when the image has one)
└── image.tar.gz          (only when the image export was asked for)
```

`spec/image.json` is not in the statement and is the reason the rest works. A
restore has to know the image's name, its `sourceType`, its `registryRef` when it
has one, and the tag the saved layers carry — none of which can be recovered from
a Dockerfile's contents. Guessing the source type from which files are present
would work until the first `compose` image whose compose file is empty. It also
carries a format version, so a future change to the layout is a refusal with a
sentence rather than a confusing failure halfway through an import.

The outer archive is written with `gzip.BestSpeed`. Almost all of its bytes are
`image.tar.gz`, which is already compressed; spending the CPU to compress it
again gains nothing and would double the time on the one part that is slow. The
spec files still compress, which is all the outer gzip is there for.

`image.tar.gz` is `docker save`'s tar stream through a gzip writer. The statement
names it, and naming it means a person who untars the backup can `docker load <
image.tar.gz` by hand without Hexagon — which is the property that makes a backup
format worth having.

### One table for both directions

Migration **013** (012 is the highest today) adds `image_transfers`:

```sql
CREATE TABLE image_transfers (
    id            TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL REFERENCES users(id),
    direction     TEXT NOT NULL CHECK (direction IN ('backup', 'restore')),
    image_id      TEXT,
    name          TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL CHECK (status IN ('pending','running','ready','failed')),
    with_image    INTEGER NOT NULL DEFAULT 0,
    path          TEXT NOT NULL DEFAULT '',
    size          INTEGER NOT NULL DEFAULT 0,
    error         TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    expires_at    TEXT NOT NULL
);
```

A backup and a restore are the same object seen from two sides: a file under
`DataDir`, a job that is or is not finished with it, and a lifetime. Two tables
would mean two sets of store methods, two startup sweeps and two janitors to say
one thing. `image_id` is the image a backup came from, or the image a restore
produced, and it is nullable because a restore has none until it succeeds.

The files live under `<DataDir>/transfers/<id>/`, the directory `0700` and the
file `0600`, created that way rather than chmod'd afterwards — the same rule the
database file already follows.

### The job outlives the request, and nothing outlives the process silently

`startImageBuild` is the shape: a goroutine on a background context with its own
timeout, writing its progress where the page can poll it. Backup and restore
follow it.

Two pieces of hygiene that a build does not need and a transfer does:

- **Interrupted transfers fail at startup.** A row left `running` by a server
  that was restarted mid-save describes a job nobody is doing, and its file is
  incomplete. `store.FailInterruptedImageBuilds` is the precedent and the
  counterpart here deletes the file as well as failing the row.
- **A janitor removes expired transfers**, on a ticker and at startup. A backup
  nobody collected is a multi-gigabyte file that would otherwise be permanent,
  and the machine this runs on is somebody's laptop. The lifetime is a constant
  with the reasoning on it, not a setting: an operator who wants the file kept
  has already downloaded it.

### Restore is inspect-then-import, and inspection answers the spec-only case entirely

`POST /api/images/restore` takes the archive, stages it, reads `spec/` out of it
and answers with what it found: the manifest, the Dockerfile and compose text,
whether `image.tar.gz` is present and how big it is, and whether an image of that
name already exists for this user.

That response is the whole of the spec-only path. The text is already in the
browser, so "only the Dockerfile/compose" fills the create form with it and the
user presses Create exactly as they do for an image typed by hand — same
validation, same build, same 202. No second endpoint, no special-case create, no
image row that exists in a half-restored state.

Only the full path posts back: `POST /api/images/restore/{id}/import` with the
name and the overwrite flag, answering 202 while the load runs.

Staging once rather than uploading twice is not only about bandwidth. The second
upload could differ from the first, and then the spec the user was shown would
not be the spec that got imported.

### The upload is a raw body, not multipart

`POST /api/images/restore` sends `Content-Type: application/gzip` and the archive
as the body; the two options it needs travel as query parameters. Multipart would
buy the ability to carry fields beside the file, which is exactly what query
parameters already do, at the price of a parser and of a second content type the
middleware has to admit.

`guardStateChanges` has to admit something either way, and the narrower the
better. The carve-out is one path and one content type, written in the shape of
`isVSCodeProxyPath`, and the same-origin check is untouched — it is the content
type rule that bends, not the origin rule.

The body limit is its own constant, `maxRestoreUpload`, sitting beside
`maxImageRequestBody` and several orders of magnitude larger. Before accepting
the upload the handler checks the free space on the filesystem holding `DataDir`
against the declared `Content-Length`, through the `hostinfo` package point 1
introduces — **which is the reason to implement point 1 first.** Filling the disk
under SQLite takes the whole application down, and it is the failure this feature
is most likely to cause.

### Overwrite updates the row in place

`sessions.image_id` references `images(id)`, and `DeleteImage` is refused while a
session still uses an image. So a restore that overwrote by deleting and
re-creating would be refused precisely when it is most wanted — restoring over
the image a running session was created from.

Instead the existing row is updated: same id, so every session that points at it
goes on pointing at it, and same `imageTag(id)`, so the tag the new layers get is
the tag the row already claims. The confirmation the statement asks for is an
explicit `overwrite` flag in the request; without it the server answers 409 with
the name, and the browser turns that into the dialog.

What happens to sessions already created from the old image is worth stating,
because it is the question a careful reader will have: **nothing, until they are
rebuilt.** Their container is running on the digest migration 012 pinned, not on
the tag, so a restore does not pull the floor out from under a live session. The
next container built for them uses the restored image, and that is the point at
which the change lands — the same moment a rebuild already lands a Dockerfile
edit.

### The loaded image is re-tagged, from the manifest

`docker save` writes the tags the image had on the machine it came from. On this
machine `hexagon/img-<other id>:latest` belongs to no row and would sit there
forever looking like something Hexagon owns — and would be pruned by point 1's
image prune, correctly, as unregistered.

So after the load the image is tagged as `imageTag(id)` for the row this restore
targets, and the imported tag is removed. The manifest is what says which loaded
reference to tag: parsing "Loaded image: …" out of Docker's progress stream would
work and would be a text format nobody promised to keep. Removing the imported
tag is guarded against the case where it is already the tag being applied — the
same image restored into the same instance — because removing it there would
delete what was just loaded.

Finally `InspectImage` resolves the digest and the row is finished `ready` with
its `image_ref`, exactly as a build ends.

### A restore reuses `building` rather than adding a status

`images.status` carries a `CHECK`, and SQLite cannot alter one: a `restoring`
value means rebuilding the whole table the way migration `007` had to, plus a new
constant, plus a new label wherever a status is rendered.

What that would buy is one correct word on screen for the length of one import.
`building` already means what the page needs it to mean — not usable yet, wait,
watch the log — and the log pane carries the load progress, which is what the
user is actually looking at. So `building` is reused, and this paragraph is the
record that it is a deliberate trade rather than an oversight.

### Reading a tar somebody else wrote

The archive is user-supplied input, and extraction is where that bites:

- Member names are matched against the three that are allowed. Anything else is
  ignored, and an archive containing only unknown names is a refusal naming what
  was expected.
- Absolute paths, `..` anywhere in a path, symlinks and hard links, and any type
  that is not a regular file are refused outright. There is no case where this
  format needs one, and every classic tar extraction bug lives in the cases where
  one is allowed.
- The spec members are capped at `maxImageRequestBody`, the same limit a typed
  Dockerfile has, and the member count is capped — a decompression bomb is a
  small file that expands forever, and the only defence is a limit that is
  checked as it is read rather than afterwards.
- `image.tar.gz` is never extracted by Hexagon. It is handed to `ImageLoad` as a
  stream, which is Docker's problem to validate and Docker's sandbox to be wrong
  inside of.

`docker load` of an image somebody else built is the larger surface, and it is
worth saying plainly rather than leaving implied: it is the same class of thing
as the Dockerfile an allowlisted user can already write. What it is not is a way
out of the container, which is the boundary AGENTS.md actually names.

### Downloading is this codebase's first file download

`GET /api/images/backups/{id}/file` answers with `application/gzip`, a
`Content-Disposition: attachment` naming the archive, and
`http.ServeContent` over the staged file — which gets `Content-Length` and range
requests for free, so an interrupted download can resume.

The browser side is a plain `<a download href="…">`. GET is exempt from
`guardStateChanges`, the session cookie is `SameSite=Lax` and `Path=/` so a
top-level navigation carries it, and the CSP allows the navigation. The
alternative — `fetch` into a `Blob` and an object URL — is rejected because it
buffers the entire archive in the tab's memory, which for this feature is the
whole problem. The cost is that an expired session renders a JSON 401 into a
saved file instead of a message; the page polled the API a second earlier, so it
is a narrow window, and it is the trade this takes.

### Alternatives rejected

- **Streaming the backup instead of staging it.** No progress, no resume, no
  size known in advance, and a failure at minute nine of a `docker save` leaves
  a truncated file the user has to notice for themselves.
- **Two tables, `image_backups` and `image_restores`.** Above: two of everything
  to say one thing.
- **Multipart upload.** Above: a parser and a second admitted content type for
  what query parameters already do.
- **A `restoring` status.** Above: a table rebuild for one word.
- **Delete-and-recreate on overwrite.** Above: refused by the foreign key
  exactly when it is needed.
- **Putting the spec files loose at the root of the archive.** The statement
  asks for `spec/`, and it is right to: it is what keeps `image.tar.gz`
  unambiguous and leaves room for a fourth file later.
- **Storing the backup as a blob in SQLite.** A gigabyte in a `BLOB` column is a
  way to make every backup of the database a copy of every backup of every image.

## API

Four endpoints, all in the `protected` map:

- `POST /api/images/{id}/backup` — body `{"withImage": true}`, answers 202 with
  the transfer row. Refuses with 409 when the image is not `ready` and the export
  was asked for: there is nothing to save yet.
- `GET /api/images/backups` and `GET /api/images/backups/{id}` — the transfer
  rows for the user, which is what the page polls.
- `GET /api/images/backups/{id}/file` — the archive, as above. 409 while it is
  not `ready`.
- `DELETE /api/images/backups/{id}` — the row and the file.
- `POST /api/images/restore` — the upload, answering the inspection:

```json
{
  "id": "…", "name": "node-22", "sourceType": "dockerfile",
  "dockerfile": "FROM node:22\n…", "compose": "",
  "hasImage": true, "imageSize": 1402653184,
  "nameExists": true, "existingImageId": "…"
}
```

- `POST /api/images/restore/{id}/import` — body `{"name": "…", "overwrite":
  false}`, answers 202 with the image row, or 409 when the name exists and
  `overwrite` is false.

`dockerx.API` gains:

```go
SaveImage(ctx context.Context, ref string, w io.Writer) error
LoadImage(ctx context.Context, r io.Reader, logs io.Writer) error
TagImage(ctx context.Context, source, target string) error
```

and `fakeDocker` in `internal/httpapi/fakedocker_test.go` gains the three.

A new `internal/backup` owns the archive format and nothing else: writing one
from a spec and an image stream, reading the spec out of one, and the path
guards. It has no knowledge of the store, of Docker or of HTTP, which is what
makes the refusals above testable as a table.

## Configuration

None. The staging directory is derived from `DataDir` in the shape of
`DatabasePath` and `ClaudeAccountsDir` — there is nothing here for an operator to
choose, and a second directory setting would be a second thing to get wrong. The
transfer lifetime and the upload limit are constants carrying their reasoning,
for the same reason `imageBuildTimeout` is one.

## UI

- **Backup**, on the image row beside Edit / Log / Delete: a dialog with two
  checkboxes — the Dockerfile and compose, and the image export — the second
  disabled with a reason when the image is not `ready`, and a line saying roughly
  how large the second will make the file. Confirming starts the job.
- **The transfers list**, on the Images page below the images: what is being
  prepared, what is ready with its size and a Download link, and what failed with
  why. It polls in the shape the build log already polls, at one second while a
  transfer is running and five otherwise.
- **Restore**, a button at the top of the page: a file input, then the inspection
  summary — the name, the source type, whether the archive carries an image — and
  two choices, "Load the files into the form" and "Import the image". The second
  is absent when the archive has no `image.tar.gz`, rather than present and
  failing.
- **The overwrite confirmation** is its own step, and it names what it is
  replacing and says what it does not do: existing sessions keep their current
  container until it is rebuilt.
- `api.ts` gains the first two helpers that are not `request<T>`: one that posts
  a `File` body and parses JSON back, and one that returns a URL string for the
  download, in the shape of `api.claude.loginTerminal`.
- Notices stay until dismissed; the transfer polling must not clear them.

## Impact

| Area | Change |
|---|---|
| Schema | `013`: `image_transfers`, with the CHECKs on `direction` and `status` |
| Store | `CreateTransfer`, `TransfersByUser`, `TransferByID`, `FinishTransfer`, `DeleteTransfer`, `FailInterruptedTransfers`, `ExpiredTransfers` |
| dockerx | `SaveImage`, `LoadImage`, `TagImage` |
| backup | new package: the archive format, the manifest, and the extraction refusals |
| httpapi | four backup routes and two restore routes; the `guardStateChanges` carve-out; `maxRestoreUpload`; the first `Content-Disposition` |
| cmd | the janitor ticker and the startup sweep, beside the existing interrupted-build sweep |
| Config | none |
| UI | the backup dialog, the transfers list, the restore flow, and the first non-JSON helpers in `api.ts` |
| Docs | README: the archive layout, what each half of a restore does, and how long a backup is kept |

## Implementation steps

Eight steps. Steps 1 to 3 are pure additions with no route behind them, which
makes them quick to review; step 6 is the one to read slowly.

**1 — Migration `013` and the store methods.** The table, the constants, the
queries, and the migration test the repository already has. Reviewable question:
are the CHECK values and the Go constants the same set.

**2 — `dockerx`: `SaveImage`, `LoadImage`, `TagImage`.** Three SDK calls, the
progress stream routed through the existing `decodeProgress`, and the fakes.
Reviewable question: does `LoadImage` surface a Docker-side error rather than
swallowing it in the progress stream.

**3 — `internal/backup`.** Writing an archive, reading the spec out of one, the
manifest type and version, and every refusal in "Reading a tar somebody else
wrote", with a table test per refusal. Reviewable question: is there any member
name or type that reaches the filesystem.

**4 — The backup job and its endpoints.** `POST /api/images/{id}/backup`, the
goroutine, the three read routes and the download with `ServeContent`.
Reviewable question: does a failed job leave a file behind.

**5 — The janitor and the startup sweep.** The ticker in `cmd/hexagon`, the
expiry query, and failing interrupted rows. Small, and easier to review apart
from step 4 than inside it.

**6 — The upload, the inspection and the middleware carve-out.** This is the
security-relevant step: the free-space check, `maxRestoreUpload`, the narrowed
content-type exception, and the inspection response. Reviewable question: can any
request that is not exactly this one path get past the JSON content-type rule.

**7 — The import job.** The load, the re-tag, the overwrite-in-place, and the
409 without the flag. Reviewable question: what happens to a session created from
the image being overwritten.

**8 — The UI, then the README.** The backup dialog, the transfers list, the
restore flow, the two new `api.ts` helpers; then the README section describing
the format, so that a person holding a `.tar.gz` and no Hexagon knows what is in
it.

## Verification

`internal/backup` — a table over archives: a spec-only archive round-trips; one
with an image reports its size without reading it into memory; a member named
`../../etc/passwd` is refused; an absolute path is refused; a symlink member is
refused; an unknown member is ignored but an archive of nothing but unknown
members is an error naming what was expected; a spec member over the limit is
refused as it is read rather than after; a manifest with a future format version
is refused with a sentence; and an archive with no manifest is refused rather
than guessed at.

`internal/dockerx` — the arguments, as always: `SaveImage` asks for exactly one
ref, `TagImage` passes source and target in that order, and `LoadImage` turns a
Docker-side error in the progress stream into an error rather than a successful
empty load.

`internal/httpapi`, in the `testEnv` style over the real router — a backup of a
`ready` image reaches `ready` with a file on disk, and one asked for an image
that is still building is refused; the download carries
`Content-Disposition` and the archive's bytes, and is 409 while the job runs;
another user's transfer is 404, both for the row and for the file; the upload is
refused without the carve-out's content type and accepted with it, and a
mutating request to any other path with that content type is still 415; an
upload larger than `maxRestoreUpload` is refused without being written; an
import against an existing name is 409 without `overwrite` and succeeds with it,
keeping the image id; the imported image ends `ready` with `imageTag(id)` as its
ref and the foreign tag gone; an import whose load fails leaves the row `failed`
with the reason and does not overwrite the previous `image_ref`; and a session
created from an overwritten image still reports the digest it was created with.

`internal/store` — `FailInterruptedTransfers` moves `running` rows to `failed`
and leaves finished ones alone; `ExpiredTransfers` selects on `expires_at` using
`formatTime`, which is the test that says the fixed-width UTC layout is still
doing its job.

By hand, and this is the check that matters most: back up a compose image with
the export on, download the file, `tar tzf` it and confirm the layout is the one
this document draws; `tar xzf` it and `docker load < image.tar.gz` on a machine
with no Hexagon at all. Then restore the same archive into a second Hexagon,
spec-only first — the form fills, the build runs — and full second, and create a
session from the result. Finally restore over an existing name while a session
from that image is running, and confirm the session keeps working and the next
rebuild picks up the new image.

## The security invariants this touches

The new routes are in the `protected` map, every query is scoped to the
authenticated user — a transfer row and its file both — and the staged files are
`0600` inside a `0700` directory, created that way rather than corrected
afterwards.

**One invariant genuinely bends, and it is the content-type rule.** AGENTS.md
says mutating API calls require a same-origin request and a JSON content type,
and an upload cannot be JSON without base64 and a third more bytes. The
same-origin half is untouched: the carve-out admits one path and one content
type, and every other check `guardStateChanges` makes still runs. That is the
same shape as the exception the VS Code proxy already holds, and it is written
down here so the next reader knows it was a decision.

**The other widening is `docker load` itself.** An image built somewhere else
runs in a session container here. That is the same trust an allowlisted user
already has when they write a Dockerfile, and the boundary AGENTS.md actually
defends — containers run as the host user, never as root, and a compose file is
checked before anything is created from it — is untouched by this: a restored
image runs under exactly the same `ContainerSpec` as a built one, and a restored
compose file goes through `composex` validation on its way in, not on its way
out.
