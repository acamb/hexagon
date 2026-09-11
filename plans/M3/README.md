# M3 — analyses

The milestone statement is [../milestones/M3.md](../milestones/M3.md). This
folder holds one analysis per point, written before that point is implemented,
as described in [AGENTS.md](../../AGENTS.md).

| # | Point | Analysis | State |
|---|---|---|---|
| 1 | Stats page | [01-stats-page.md](01-stats-page.md) | to do |
| 2 | Backup and restore of images | [02-image-backup-restore.md](02-image-backup-restore.md) | done |
| 3 | Export workspace | [03-export-workspace.md](03-export-workspace.md) | to do |

M3's statement numbers nothing: its three points are headings, where M1 and M2
used a numbered list. The numbers above are this index's, assigned in the order
the statement makes them, and the filenames follow from them.

Point 1 should be built first, and not only because it is the smallest. Its
`internal/hostinfo` is what lets point 2 refuse an upload that would fill the
disk, and filling the disk is the failure an image restore is most likely to
cause — SQLite is on the same filesystem. Building them the other way round means
either shipping the restore without that check or writing the check twice.

Points 2 and 3 are the first file transfers this codebase has ever done, and they
set conventions between them that are worth reading together: the first
`Content-Disposition`, the first response that is not `writeJSON`, the first
helpers in `api.ts` that are not `request<T>`, and — point 2 only — the first
carve-out in `guardStateChanges` since the VS Code proxy. They deliberately
diverge on one thing and agree on the rest. Point 2 stages its archive under
`DataDir` and answers a poll, because a `docker save` is gigabytes; point 3
streams and keeps nothing, because the statement says the file must not stay on
Hexagon and a repository clone does not need the machinery. Everything else —
GET for the download, a plain `<a download>` rather than a `Blob`, the user
scoping — is the same on both sides and should stay that way.

Point 3 diverges from the literal wording of its statement, at the user's
request: the archive is built from the host directory that is bind mounted at
`/workspace` rather than read out of the container, so the export still works
for a session that is stopped, failed or `gone`. The reasoning is in the
analysis, under "Reading of the request".

Point 1 also records one deliberate exception to a rule AGENTS.md calls
non-optional. The query that decides which images the prune must protect is not
scoped to a user, because Docker images are not: scoping it would let one user's
prune delete another's image.

Each analysis carries an `## Implementation steps` section, at the user's
request: the work split into units a person can review one at a time. M1 kept
these in separate `NN-slug-implementation.md` files and M2 dropped them
altogether; folding them into the analysis keeps AGENTS.md's one file per point
while giving the steps a place to live.
