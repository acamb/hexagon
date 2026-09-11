# M3.4 — Restore loaded backups

## What was asked

> dalla pagina images vedo la lista dei backup, vorrei che fosse possibile fare
> il restore dell'immagine direttamente da li (attualmente c'e' solo il pulsante
> di delete)

## Reading of the request

A backup made by Hexagon is already a restore archive sitting on the server: the
same format, read by the same `backup.Inspect`, in the same `TransfersDir`. Today
the only way to restore it is to download it and upload it again — gigabytes
through the browser and back, to end up in a file the server already had. The
point is to cut that loop and hand the file on disk straight to the
inspect-then-choose flow point 2 already built, unchanged: the same panel, the
same two choices, the same overwrite confirmation.

Two decisions taken with the user before writing:

- **Every row that holds an archive gets the button, not only backups.** A
  restore upload that was never imported — the panel was cancelled, or the import
  failed on Docker's side — is staged and good for 24 hours, and today there is
  nothing to do with it but delete it. Point 2 already keeps a failed import's
  transfer "so the same import can be retried without uploading again"; this is
  the button that makes that sentence true.
- **The name is editable.** The panel gains a name field, prefilled from the
  manifest. Without it, a backup restored into the instance it came from always
  lands on the overwrite confirmation, since its source image is usually still
  there; with it, the same backup can also come back as a copy beside the
  original — `base-before-upgrade` next to `base`. The upload flow gets the field
  too: it is the same panel.

## Current behaviour

`internal/httpapi/handlers_restore.go`:

- `handleRestoreUpload` is the only producer of a `restoreInspectionResponse`,
  and it produces one only from a fresh upload. There is no way to ask what is
  in a transfer that is already staged.
- `handleImportTransfer` refuses a transfer whose `Direction` is not `restore`
  with 400 `not a restore`, even though a ready backup's archive would import
  exactly as an uploaded one does.
- On success, `startImportImage` calls `FinishTransfer(..., img.ID, ...)`, which
  sets the transfer's `image_id` to the image the import produced. For a restore
  row that is the column's meaning. For a backup row, `image_id` is the image the
  backup came *from*, and overwriting it would silently change what the row says.
- The inspection does not carry `registryRef`. The backup manifest has it and
  `backup.Inspection` exposes it — the import uses it — but the response drops
  it, so "Load the files into the form" on a registry image leaves the one field
  that matters empty.

`web/src/views/ImagesView.vue`:

- The restore panel opens only from `onRestoreFileChosen`.
- `importRestoredImage` always imports under `insp.name`, with `overwrite` taken
  from `insp.nameExists` — an answer about the manifest's name, computed at
  upload time.
- The transfers list offers Download on a ready backup and Delete on everything.

## Design

### Inspecting a staged transfer is a GET

`GET /api/transfers/{id}/inspection` reads the spec out of any transfer that is
`ready` and has a `Path`, in either direction, and answers with the same
`restoreInspectionResponse` the upload does. 409 while the transfer is not
ready — a backup still being written has no archive to read — and 404 for a row
that is not the caller's, through `transferOr404` like every other transfer
route.

It is a GET because it changes nothing, and that matters for the spec-only path:
"Load the files into the form" must not create anything, and after this change it
still creates nothing — no row, no file. Being a GET also means it needs nothing
from `guardStateChanges`, and the carve-out point 2 added stays exactly one path.

The pattern is a literal segment after the wildcard, the same shape as
`GET /api/transfers/{id}/file`, so it registers beside it without the ServeMux
conflict that moved these routes to `/api/transfers` in the first place.

The response is built by one helper shared with `handleRestoreUpload` — the
`ImageByName` lookup that fills `nameExists` included — so the two ways into the
panel cannot drift apart.

### The inspection carries `registryRef`

`registryRef,omitempty` joins the response and the form reads it. This fixes a
gap in point 2 rather than adding anything new, and it belongs here because the
list makes it common: a registry image has no Dockerfile to lose, so it is the
image most likely to be backed up spec-only, and a spec-only registry backup is
nothing *but* the reference.

### Import accepts a backup row

The direction refusal in `handleImportTransfer` goes. What is left says
everything that matters: the transfer is ready, it carries an image, the name is
valid, an existing name needs `overwrite`, builds in flight are under the limit,
Docker answers. None of those depends on whether the file arrived by upload or by
`docker save`.

What does depend on direction is the link afterwards. `startImportImage` links
`image_id` only for a `restore` row. A backup row keeps pointing at the image it
was taken from, whatever its archive was later restored into — and it can be
restored more than once, under different names, which is one more reason the
column cannot follow the latest import.

Restoring a backup over its own source image is the case to think through, and
it already works: the manifest's `ImageRef` is the row's own `imageTag(id)`,
`TagImage` applies a tag to itself, and the guard in `importImage` that skips
removing the foreign tag when it equals the target is exactly what stops the
restore from deleting what it just loaded. Point 2 wrote that guard for "the same
image restored into the same instance"; this is how that case now happens.

### Lifetime is unchanged

A restore does not extend a transfer's `expires_at`. The janitor removing the
file, or the user deleting the row, while an import is running is harmless:
`backup.OpenImage` opened the file before the load began, and on the platforms
this runs on an open file outlives its directory entry. A restore attempted on a
row that has since expired is a 404, like any deleted row, and the list's next
poll removes the button.

### The name is chosen in the panel, and so is the overwrite

The panel gains a name input, prefilled from the manifest, and both choices use
it: "Load the files into the form" puts it in the form's name field, "Import the
image" sends it.

Whether to show the overwrite step is decided against the name as typed, by
looking it up in the `images` list the page already polls — `insp.nameExists`
answers only for the manifest's name and only at the moment of the upload. The
server's 409 without `overwrite` stays the authority: a name taken between the
check and the request surfaces as a notice, and pressing Import again goes
through the confirmation. The confirmation names the image it replaces, which is
now not necessarily the one the manifest names.

### Alternatives rejected

- **Copying or hard-linking the backup into a new restore row.** It would keep
  import single-direction, but it creates a row for every restore attempt —
  including the spec-only ones, which by point 2's own design should create
  nothing — and a second copy of the file with its own lifetime, all to preserve
  a refusal that protects nothing.
- **Restoring through the Download link.** Download, then upload, is the loop
  this point exists to remove.
- **An import endpoint for backups alongside the restore one.** Same checks,
  same job, same response; the only difference is one `if` about linking, which
  is where it lives.

## API

- `GET /api/transfers/{id}/inspection` — new, in the `protected` map. Answers
  `restoreInspectionResponse`; 409 while not ready, 404 for another user's
  transfer.
- `restoreInspectionResponse` gains `registryRef`.
- `POST /api/images/restore/{id}/import` — unchanged in shape; now also accepts
  a ready backup transfer.

## Impact

| Area | Change |
|---|---|
| Schema, store, dockerx | none |
| httpapi | the inspection route; the shared inspection helper; `registryRef` in the inspection; the import accepting backups and linking `image_id` only for restores |
| Config | none |
| UI | a Restore button on ready rows in the transfers list; the panel opened from a row and scrolled into view; the name field; the overwrite step decided against the chosen name; `registryRef` loaded into the form |
| api.ts | `api.transfers.inspect(id)`; `registryRef?` on `RestoreInspection` |
| Docs | README, "Backing up and restoring an image": restoring from the list, and choosing the name |

## Implementation steps

Three steps. The first two are small server changes that can each be reviewed
against one question; the third is most of the diff.

**1 — The inspection route.** The shared helper, `registryRef`, the GET route and
its tests. Reviewable question: is there any way to read a transfer that is not
`ready`, or is not the caller's.

**2 — Import from a backup.** The refusal removed, the link restricted to restore
rows, and the tests. Reviewable question: after a backup is restored under a new
name, does the backup row still name the image it was taken from.

**3 — The UI, then the README.** The Restore button, opening the panel from a
row, the name field, the overwrite step against the chosen name, the registry
reference in the form; then the README paragraph.

## Verification

`internal/httpapi`, in the `testEnv` style of `backup_restore_test.go`:

- the inspection of a ready backup answers its spec, `hasImage` and
  `registryRef`;
- the inspection is 409 while a backup is running, 404 for another user's
  transfer, and 401 without a session — the last by extending
  `TestBackupEndpointsRequireASession`;
- importing a full backup over the image it was taken from keeps the id and the
  local tag, ends `ready`, and does not remove the tag it just applied;
- importing a backup under a new name creates a second image, and the backup
  row's `imageId` still names the source;
- importing a spec-only backup is 400;
- an upload that was never imported can be inspected and imported later.

`make test`, `make vet`, and `make build` for the type check.

By hand, with `make dev`: back up an image with the export on, press Restore on
its row, import over the original through the confirmation, and create a session
from it; restore the same backup again as `base-copy` and see both images listed;
press Restore on a spec-only backup of a registry image and see "Load the files
into the form" fill the reference; upload an archive, cancel the panel, and
restore it from its row.

## The security invariants this touches

None bend. The new route is in the `protected` map and scoped to the caller
through `transferOr404`; it is a GET, so the content-type rule does not apply to
it and the carve-out point 2 made stays one path. Importing a backup made here is
strictly less exposure than importing an upload: the layers came out of this
daemon in the first place.
