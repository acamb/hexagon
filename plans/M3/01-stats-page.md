# M3.1 — Stats page

## What was asked

> Una nuova voce "Stats" nel menu' che fa vedere una pagina di statistiche della
> macchina:
>     * Cpu / Ram / Disco
>     * Lista immagini docker con dimensione (con un badge su quelle attualmente
>       usate)
>     * Bottone per fare il prune delle immagini e/o container non usati

## Reading of the request

Three things on one page, and only the third is dangerous.

**The machine.** Hexagon runs Claude Code sessions in containers on somebody's
machine, and the question that page answers is the one an operator asks before
creating the next session: is there room. CPU, memory and disk of the host the
server process runs on.

**The images.** Docker's images, not Hexagon's rows. The Images page already
lists what Hexagon knows about; what nobody can see today is the forty gigabytes
of layers behind them, or the images left over from an experiment three weeks
ago. So this list comes from the daemon, and Hexagon's own rows are what decides
which entries get a badge.

**The prune.** A button that deletes things. Everything else on the page is
read-only and can be got wrong without consequence; this one, got wrong, removes
the image a stopped session needs to start again. Most of the design below is
about that.

Two decisions taken with the user before writing:

- **The page shows the host *and* the daemon's own accounting.** `system df`
  knows what images, containers, volumes and build cache cost, and no amount of
  `statfs` on a directory will tell you which of those four it was. Both halves
  are reported, side by side.
- **Two prune buttons, not one.** Containers and images fail differently and a
  single "clean up" button would have to be as cautious as its most dangerous
  half. Each carries its own confirmation and reports what it actually freed.

## Current behaviour

There is no metrics code in this repository at all. `handleHealth`
(`internal/httpapi/router.go`) is the closest thing: for an authenticated caller
it answers `{status, uptime, docker}`, computed from `Server.started` and a
`store.Ping()`. Nothing reads `/proc`, nothing calls `Statfs`, nothing calls
`runtime.NumCPU`.

`internal/dockerx` wraps the Engine API through the official SDK, and its `API`
interface has no image listing, no disk usage and no prune of any kind. The only
deletions it can perform are `RemoveImage` (`image.RemoveOptions{PruneChildren:
true}`) and `RemoveContainer`. `InspectImage` returns a content-addressable id
and nothing else — no tags, no size.

`internal/store/images.go` holds the rows the Images page lists. `Image.ImageRef`
is the local ref a session runs from: the tag `imageTag(id)` derives for a built
image, or the normalized ref of a pulled one. There is no size column and there
should not be: a size is the daemon's to know, and it changes without Hexagon
doing anything.

`internal/dockerx/containers.go` labels every container Hexagon creates with
`hexagon.managed=true`, plus `hexagon.session.id` and, for a compose service,
`hexagon.role`. `ListManagedContainers` filters on the first of those.

`web/src/components/AppHeader.vue` holds four `RouterLink`s addressed by route
name — `sessions`, `images`, `accounts`, `settings` — with a dropdown below the
640px breakpoint. `web/src/router.ts` registers the matching routes.

## Design

### A new package, `internal/hostinfo`, and no new dependency

CPU from `/proc/stat`, memory from `/proc/meminfo`, filesystems from
`syscall.Statfs`. The package owns reading those and nothing else: it does no
formatting, keeps no policy, and returns numbers.

Memory is reported from `MemAvailable`, not `MemFree`. On any machine that has
been up for an hour `MemFree` is close to zero and means nothing — the page cache
is holding the rest and will give it back on demand. `MemAvailable` is the
kernel's own estimate of what a new process could actually get, which is the
number a person means when they ask how much memory is left.

The filesystems reported are the ones Hexagon can fill: the one holding
`cfg.DataDir` and the one holding `cfg.WorkspaceRoot`. They are usually the same
device, and the page says so once rather than drawing the same bar twice.

### The CPU percentage needs two samples, so the package keeps one

`/proc/stat` counts jiffies since boot. A single read yields the average since
the machine started, which is not what anybody wants from a page called Stats.
So `hostinfo` keeps the previous sample and reports the delta against it.

The consequence is deliberate and worth stating: **the first read after startup
reports no percentage at all.** A number computed against boot would be wrong in
a way nobody would notice, and the page polls every few seconds, so the missing
value is on screen once. Load average is read from `/proc/loadavg` and reported
beside it, because it costs one file read, needs no history and answers the same
question when the percentage is not there yet.

### `syscall.Statfs`, not `golang.org/x/sys/unix`

`golang.org/x/sys` is already in `go.sum`, pulled in indirectly by the Docker
SDK, so promoting it to a direct require would download nothing new. It is still
a dependency to justify under AGENTS.md, and the justification would have to be
that the standard library cannot do this — which on the one operating system
Hexagon ships for is not true. `syscall.Statfs` is in the standard library, it is
stable on Linux, and it is what this uses.

### Non-Linux builds degrade, they do not fail

`/proc` and `Statfs` are Linux, and Hexagon ships as a `.deb` with a systemd unit
and an OpenRC script, so Linux is the target. But `make test` on a developer's
Mac has to keep working. `hostinfo` is therefore split — the real implementation
behind `//go:build linux`, a fallback beside it that reports unavailable — and
the API answers with the host half absent rather than with an error. The page
draws the Docker half and says the host figures are not available on this
platform.

### Pruning containers is one call; pruning images is not

**Containers.** Every container Hexagon creates carries `hexagon.managed=true`,
and Docker's container prune accepts a `label!=` filter. So the exclusion is
expressible exactly, in one daemon call:
`ContainersPrune` with `label!=hexagon.managed=true` removes every stopped
container that is not Hexagon's and cannot touch one that is. Nothing has to be
enumerated, nothing has to be matched by hand, and a container created between
the listing and the delete cannot slip through a race that does not exist.

**Images.** The same trick does not work, and the reason is not an oversight in
the Engine API: an image pulled from a registry is not Hexagon's to label. A
`registry` image is `node:22` exactly as Docker Hub published it, and tagging it
to mark it as ours would be a lie about provenance in `docker images` for the
sake of a filter. So the protected set is computed here — every `image_ref` in
the `images` table, and the id each resolves to — and the unused remainder is
removed one at a time through the `RemoveImage` that already exists.

"Unused" means no container, running or stopped, is based on it. That comes from
the image listing, which is asked for container counts.

**The selection lives in the handler, not in `dockerx`.** The package doc says
`dockerx` wraps the Engine API and carries no business logic, and "which images
is Hexagon not allowed to delete" is nothing but business logic. `dockerx` gains
a listing and a disk-usage call; what to do with them stays above it.

### The prune that is not offered

Docker's image prune with `dangling=false` deletes every image no container is
based on. That is the obvious implementation of the button the statement asks
for, and it is the trap this design exists to avoid: most of Hexagon's images
have no container most of the time, because sessions get stopped and deleted
while their image stays registered for the next one. A user who pressed it would
lose the image behind every stopped session, and the next start would be a
rebuild — from a Dockerfile whose `apt-get` lines may no longer resolve, or a
pull of a tag that has moved.

The default prune, dangling only, is safe but answers a smaller question than
the one asked. Computing the protected set is what lets the button mean "unused"
and still be safe.

### The protected set is deliberately not scoped to one user

AGENTS.md says every query is scoped to the authenticated user and that the
`WHERE user_id = ?` is not optional. This is the exception, and it is an
exception in the safe direction: the prune must protect **every** user's images,
because an image is a daemon-wide object and one user pressing the button would
otherwise delete another's. A new `store.AllImageRefs` carries that reasoning in
its comment so the next reader does not "fix" it.

Multi-user is not a goal here, and it does not have to be for this to matter: a
second row in `images` is enough.

### Two badges, because there are two kinds of "used"

The statement asks for a badge on the images currently in use. An image can be
in use in two different senses, and telling them apart is what makes the prune
button's behaviour legible:

- **in use** — a container exists on it. This is the statement's badge.
- **registered** — a row in `images` points at it. The image is idle right now
  and the prune will still leave it alone.

Without the second badge the page shows a list of untouched images and no
explanation of why they were untouched, which reads as a broken button.

### Build cache is shown and not reclaimed

`system df` accounts for images, containers, volumes and build cache, and on a
machine that has built a few images the cache is often the largest of the four.
Leaving it off the page would misstate where the disk went. Offering to prune it
is outside the statement, and it costs the next build every layer it would have
reused, so it is reported and not actioned.

Volumes are reported for the same reason and pruned for none: a compose project's
named volume is a user's database, and `Delete` with `purge` already removes the
ones Hexagon created.

### The two halves may be different machines

`cfg.DockerHost` can point at a daemon somewhere else. When it does, the host
gauges describe the machine running the server and the Docker figures describe
the machine running the containers, and adding them up is meaningless. The
response reports the daemon's name and address, and the page says plainly that
the two halves are not the same computer rather than quietly drawing one picture.

### Alternatives rejected

- **`gopsutil`.** Cross-platform and complete, and everything above needs three
  files and one syscall. A dependency is a decision to justify, and this one has
  no argument behind it.
- **A background sampler goroutine.** A ticker keeping CPU history would give a
  percentage on the first request and a sparkline later. It also runs forever on
  every server, for a page most operators open twice a month. The sample is taken
  when the page asks.
- **Stats on the health endpoint.** `/api/health` is answered before
  authentication for the liveness case, and CPU, memory and a list of image names
  are not things to hand out unauthenticated.
- **Per-container CPU and memory.** `docker stats` is a stream per container and
  the statement asks about the machine. A session's own usage is a different
  page, and not this milestone's.
- **Pruning through `ImagesPrune` with a label filter.** Above: it would require
  labelling images Hexagon did not build.

## API

Three endpoints, all in the `protected` map:

- `GET /api/stats` →

```json
{
  "host": {
    "available": true,
    "cpu": {"cores": 8, "usedPercent": 12.4, "load": [0.4, 0.6, 0.8]},
    "memory": {"total": 33526104064, "available": 21290328064},
    "filesystems": [{"path": "/home/u/.local/share/hexagon", "total": 0, "free": 0}]
  },
  "docker": {
    "host": "unix:///var/run/docker.sock",
    "sameMachine": true,
    "usage": {"images": 0, "containers": 0, "volumes": 0, "buildCache": 0},
    "images": [
      {"id": "sha256:…", "tags": ["hexagon/img-ab12cd34ef56:latest"],
       "size": 1402653184, "created": "2026-08-03T10:12:00Z",
       "containers": 1, "inUse": true, "registered": true, "dangling": false}
    ]
  }
}
```

  `usedPercent` is absent on the first read, for the reason given above.
  `host.available` is false on a platform without `/proc`, and the rest of
  `host` is then absent.

- `POST /api/stats/prune/images` → `{"removed": 7, "reclaimed": 4294967296}`
- `POST /api/stats/prune/containers` → `{"removed": 2, "reclaimed": 1048576}`

Both prunes take no body and both are idempotent in the only sense that matters:
pressing twice removes nothing the second time. Both answer 503 when the daemon
is unreachable, in the shape the create path already uses.

`dockerx.API` gains three methods:

```go
ListImages(ctx context.Context) ([]ImageSummary, error)
DiskUsage(ctx context.Context) (DiskUsage, error)
PruneContainers(ctx context.Context) (Pruned, error)
```

`ImageSummary` carries id, tags, size, creation time and container count;
`inUse`, `registered` and `dangling` are decided in the handler, which is where
the `images` table is in scope. Adding to the interface means adding to
`fakeDocker` in `internal/httpapi/fakedocker_test.go`, which the whole httpapi
suite builds on.

`store` gains `AllImageRefs(ctx) ([]string, error)`.

## Configuration

None. Every path this page reports on is already configured — `dataDir`,
`workspaceRoot`, `docker.host` — and a polling interval is a property of the view,
not of the server.

## UI

- A fifth entry in `AppHeader.vue`, `Stats`, after `Images`, and a `/stats`
  route in `router.ts` pointing at a new `StatsView.vue`.
- Three gauges across the top — CPU, memory, disk — each a bar with the absolute
  figures under it, because a percentage alone does not say whether the eight per
  cent left is eight gigabytes or eighty megabytes. The CPU gauge shows the load
  average while the percentage is not available yet.
- The Docker accounting as four figures: images, containers, volumes, build
  cache, with a line saying which of them the buttons below can reclaim.
- The image table: tag (or the short id when there is none), size, age,
  containers, badges. Sorted largest first — the page exists to answer "what is
  eating the disk".
- Two buttons, each behind a confirmation that names what will go: how many
  images or containers, and how many bytes that is. The answer goes into a
  `Notice`, which stays until it is dismissed — the polling refresh must not
  clear it.
- Polling with `setInterval` on mount, cleared on unmount, per the convention in
  AGENTS.md. Five seconds: these numbers do not move fast enough to be worth
  more, and the image listing is not free on a machine with hundreds of them.
- Copy says what is happening, not what the state is called: "Nothing to remove"
  rather than an empty result, and a plain sentence when the daemon and the host
  are different machines.

## Impact

| Area | Change |
|---|---|
| Schema | none |
| hostinfo | new package: CPU, memory and filesystem readings, Linux plus a fallback |
| dockerx | `ListImages`, `DiskUsage`, `PruneContainers`, and their types |
| Store | `AllImageRefs`, deliberately not scoped to one user |
| API | `GET /api/stats`, `POST /api/stats/prune/images`, `POST /api/stats/prune/containers` |
| Config | none |
| UI | `StatsView.vue`, the `/stats` route, a fifth header link, `api.stats` |
| Docs | README: what the page shows, and exactly what each prune removes |

## Implementation steps

Seven steps, each reviewable on its own. The first three change no behaviour that
a user can see, which is what makes them cheap to read.

**1 — `internal/hostinfo`.** The package, its types, the Linux implementation and
the fallback, plus tests that parse a fixed `/proc/stat` and `/proc/meminfo`
sample from `t.TempDir()` rather than the machine's own. Reviewable question: are
the numbers right, and does the first sample refuse to guess.

**2 — `dockerx`: `ListImages`, `DiskUsage`, `PruneContainers`.** The three SDK
calls, their result types, and the `fakeDocker` in `internal/httpapi` extended so
the package still compiles. No caller yet. Reviewable question: is the container
prune filter `label!=hexagon.managed=true` and nothing wider.

**3 — `store.AllImageRefs`.** One query and its comment. Reviewable question: is
the missing `user_id` deliberate and explained.

**4 — `GET /api/stats`.** The handler, the wire types, the route, and the
decisions about badges and `sameMachine`. Tested in the `testEnv` style over the
real router. Reviewable question: does the response degrade sensibly when the
host half is unavailable and when the daemon is down.

**5 — The two prune endpoints.** The image selection against the protected set,
the container prune, the reclaimed-bytes reporting. This is the step to read
carefully. Reviewable question: can any sequence of rows and images cause a
registered image to be removed.

**6 — The page.** `StatsView.vue`, the route, the header entry, `api.stats`, the
confirmations and the notices. Reviewable question: does the page say what the
buttons will do before they do it.

**7 — README.** The Stats section, and in particular a sentence per button
saying exactly what it deletes.

## Verification

`internal/hostinfo` — against sample files in `t.TempDir()`: `MemAvailable` is
what is reported and `MemFree` is not; two `/proc/stat` samples produce the
percentage the jiffy delta implies; a single sample produces none; a malformed
file is an error naming the file rather than a zero; and the fallback build
reports unavailable without touching the filesystem.

`internal/dockerx` — the SDK calls are thin, so the tests that earn their place
are the ones about arguments: the container prune carries the `label!` filter and
nothing else, and the image listing asks for container counts, without which
every image looks unused.

`internal/httpapi`, in the `testEnv` style over the real router — `GET
/api/stats` is 401 without a session; it answers with `host.available` false when
the host half is unavailable and 503 when the daemon is; an image with a row in
`images` is marked registered and one with a container is marked in use;
`POST /api/stats/prune/images` removes an unused unregistered image, leaves an
unused **registered** one, leaves one with a container, and leaves another user's
registered image alone — that last case is the whole of the non-scoped query;
and the container prune goes through `PruneContainers` rather than enumerating
and removing, so a Hexagon container can never be passed to `RemoveContainer` by
this path.

By hand: `make dev`, open Stats, confirm the CPU figure tracks `top` within a few
points and the disk figure matches `df -h` on the data directory. Then `docker
run --name leftover alpine true` and `docker build` an unrelated image, press
both buttons, and confirm the leftover container and the unrelated image are gone
while every image on the Images page is still there and every stopped session
still starts.

## The security invariants this touches

None of them move, and two are worth naming because this page comes close.

The endpoints are in the `protected` map like everything else, so the machine's
figures and the daemon's image list are behind a session. That is not paranoia
about disk sizes: the image tags on that page name the projects somebody is
working on.

The prune buttons delete, and they delete objects that are not scoped to the
user pressing them — that is in the nature of a Docker daemon, which has no
users. What keeps that acceptable is the same thing that already keeps the
settings page acceptable: every account that can sign in is on the allowlist, and
the allowlist is the admin list. What this point adds is the protected set, so
that even an allowlisted user pressing the button in good faith cannot delete
something Hexagon depends on.
