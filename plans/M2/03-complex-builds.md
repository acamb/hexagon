# M2.3 — Support for complex builds

## What was asked

> Deve essere possibile specificare un compose ed un docker file (modalita'
> "avanzata"), oltre al modo attuale in cui si specifica solo un docker
> file/immagine (modalita' "semplice").
> Nella modalita' semplice deve essere possibili indicare le porte da pubblicare
> (facendo vedere all'utente container port / container host)

## Reading of the request

Two changes that share a page, and it is worth keeping them apart.

**A second shape of image.** "Advanced" is a compose file *and* a Dockerfile,
not a compose file instead of one. The Dockerfile goes on describing the
container Claude Code runs in, exactly as it does today; the compose file
describes the services that have to be there beside it. That reading was
confirmed with the user, and it is the one that costs least: the build path does
not change at all — an advanced image is `ready` when its Dockerfile has built —
and the compose file is a session-time concern.

**Published ports.** Today a session container publishes one port and only when
it was created with the VS Code integration. A project that runs a web server on
3000 has no way to be looked at. The statement puts this under simple mode; the
mechanism is identical in both, because Hexagon owns the agent service either
way, so it is offered in both. That is a deliberate widening rather than an
accident of implementation, and it is recorded here as one.

Two divergences from the statement, decided with the user:

- **The ports belong to the session, not to the image.** A port binding is a
  property of a container, fixed when it is created — the same reasoning
  `vscode` and `propagateToken` already carry in `store.Session`, and the same
  consequence: there is no setter, and changing it means a new session.
- **The host side is Docker's to choose.** The statement asks to show the user
  "container port / host port", and showing is what this does: Hexagon publishes
  on loopback with the host port left to the daemon and reports the pair. A host
  port typed by the user would read better in a URL and would make the second
  session from the same image fail to start.

And one addition, asked for while this was being written: **"Ask Claude" belongs
to each editor**. Advanced mode has two files, so it has two of those buttons.

## Current behaviour

`internal/store/images.go` — `source_type` is `dockerfile` or `registry`, with a
`CHECK` constraint written in `001_init.sql`. `Image.Dockerfile` holds the
source; `ImageRef` is what a session runs from once it is ready.

`internal/session/manager.go` — `containerSpec` is described in its own comment
as "the whole contract between Hexagon and a session container": the image,
`sleep infinity` as the command, the environment, the host uid, the binds, the
labels, `AutoRestart`, and `Ports`, which today carries `dockerx.VSCodePort` and
nothing else.

`internal/dockerx/containers.go` — `ContainerSpec.Ports []int` publishes each
port on `127.0.0.1` with the host side left empty for Docker to fill;
`ContainerState.Ports map[int]int` reports the mapping back, and its comment
already gives the rule this point depends on: "Docker picks a new host port
every time the container starts, so this is never stored — only looked up".
Both exist, and both are exactly what session ports need.

`internal/session/lifecycle.go` — `VSCodeEndpoint` is the pattern for reporting
a published port: looked up on every read, never persisted. `Start`, `Stop` and
`Delete` act on one container id. `Reconcile` matches every managed container
against a session row and logs a warning for the rest.

`internal/dockerx/containers.go` — `LabelRole` exists for precisely the problem
compose is about to create: "a container with no row would be reported as an
orphan at every startup". Only the browser-login container uses it so far.

`internal/claudex` — the shape a CLI wrapper takes in this repository:
`New(configured)` falling back to `exec.LookPath`, an `ErrUnavailable` the
caller turns into a feature the UI is told about rather than a button that
always fails. `EditDockerfile` runs `claude -p` with every tool removed and a
JSON schema, and `POST /api/images/dockerfile` is the one route that calls it.

`internal/gitops` — the precedent for shelling out to a command at all.

## Design

### The image keeps its table and its page

A third `source_type`, `compose`, and one new column holding the compose file.
Not a new table and not a new page: it has the same lifecycle, the same build,
the same name uniqueness, the same "pick one when creating a session" and the
same refusal to be deleted while a session still uses it. Splitting it would be
two of everything in order to say one thing.

One schema detail worth recording, because it makes migration `007` bigger than
it looks: SQLite cannot alter a `CHECK` constraint, so admitting a third
`source_type` means rebuilding `images` — create the new table, copy, drop,
rename — rather than adding a column to it. `sessions` only gains columns, so
those stay plain `ALTER`s.

### Hexagon generates the agent service

The user's compose file describes the dependencies. Hexagon renders its own
service into a second file, and the project is
`docker compose -f user.yaml -f hexagon.yaml`, both written into
`<workspace>/compose/`, under the project name `hexagon-<session id>`.

The alternative — attaching to a service the user wrote, found by name or named
on the image — was rejected, and the reason is not convenience. Every invariant a
session container carries would move into a YAML file that Hexagon then has to
police: the workspace bind mount, the host uid, the labels the reconciler
matches on, `never as root`. Generating the service leaves all of them where
they already are, and leaves the user with the thing they actually wanted, which
is a database on the network.

**The generated service is rendered from `ContainerSpec` rather than written
beside it.** `containerSpec` stays the single description of a session
container, and gains a rendering into a compose service. Two hand-written lists
of binds would drift inside a milestone; one list rendered two ways cannot.

`container_name` is set to the `hexagon-<session id>` the plain path already
uses, so the naming convention holds and the agent container can be found with
the `InspectContainer` that is there — Docker accepts a name where it accepts an
id, so nothing new is needed to learn the container id after `up`. `depends_on`
names every service in the user's file, so the agent starts last.

### internal/composex, wrapping the compose CLI

A new package in the shape of `gitops` and `claudex`: no business logic, an
interface defined by its consumer (`session.Compose`), and a small surface —
`Available`, `Validate`, `Up`, `Start`, `Stop`, `Down`.

Compose is a client-side format. Networks, `depends_on`, healthchecks, profiles,
`extends` and interpolation are all things the CLI does before the daemon sees
anything, and the Engine API has no compose in it at all. Reimplementing that is
a project rather than a feature, and `dockerx` is explicitly a wrapper with no
business logic in it, so it is not where such a thing would go either. Shelling
out to a command has precedent here twice over.

Availability is decided once at startup — `docker compose version` — and the
Images page is told, the way it is already told `canAsk`. A server without the
plugin does not offer advanced mode at all, rather than offering one that fails
at the first session. `DOCKER_HOST` is passed through from `cfg.DockerHost`, so
the CLI talks to the daemon `dockerx` talks to and not to whichever one the
server process happens to have inherited.

### Validation over the normalized document, with no new dependency

`docker compose config --format json` resolves aliases, `extends`, merge keys,
interpolation and the short and long form of every key into one document.
Validating *that*, decoded with `encoding/json`, means a refusal cannot be dodged
by writing the same request another way — and it costs no YAML parser, which
would otherwise be a dependency to justify in this change.

Refused, each named in the error:

| Key | Why |
|---|---|
| `privileged`, `cap_add`, `security_opt`, `devices` | the container boundary is the whole of the protection |
| `network_mode`, `pid`, `ipc`, `uts` set to `host` | the same boundary, left by another door |
| `user` resolving to uid 0 | containers run as the host user, and never as root |
| a host path in `volumes` (named volumes are fine) | a bind mount of `/`, or of the Docker socket, is a root shell on the machine |
| `build` | there is no build context at session time; the Dockerfile is the one thing that gets built |
| a fixed host port in `ports` | two sessions of one project would fight over it, and the form without a host side works |

The honest part, which belongs in the analysis rather than in a comment: this
widens what an allowlisted user can ask for. A Dockerfile can already run
anything they like *inside* a container; a compose file can ask to leave one.
The list above is what keeps that answer no, and it is a denylist over a
normalized document rather than over the source text precisely because a
denylist over source text would not hold.

Validation runs when the image is registered, so the error arrives while the
operator is still looking at the editor rather than at the first session that
fails to start.

### Session ports

`sessions` gains `ports TEXT NOT NULL DEFAULT ''`, the container ports separated
by commas, and `compose INTEGER NOT NULL DEFAULT 0`. Both are set at creation
and never edited, for the reason `vscode` already carries: a container keeps the
port bindings it was created with.

Nothing new is needed under them. `ContainerSpec.Ports` already publishes on
loopback with a host port Docker chooses, and `ContainerState.Ports` already
reports the mapping. The session response reports the pairs looked up live and
never stored, the way `VSCodeEndpoint` does.

Checked when the session is created: each port in 1–65535, no duplicates, a
small maximum, and not `dockerx.VSCodePort` when the session also asks for VS
Code — otherwise the editor's own binding is quietly taken by something else and
the button stops working for a reason nobody could find.

The UI has to repeat the honesty the README already carries for code-server: a
published port answers on the host's loopback interface with nothing in front of
it, so any other process on the machine can reach it while the session is up.

### "Ask Claude" edits either file

Advanced mode needs the button simple mode already has, twice. So
`claudex.EditDockerfile` becomes `Edit(ctx, cred, kind, content, instruction)`,
where the kind chooses a prompt and a JSON schema, returning
`Edit{Content, Summary}`.

Everything that makes the existing call safe is already independent of which
file it is: `--safe-mode` and `--strict-mcp-config`, so the developer's own
Claude Code setup cannot change what a Hexagon request does; `--tools ""`, so
there is nothing to run, read or fetch; and `--json-schema`, which is what stops
the answer arriving wrapped in a Markdown fence. Only the prompt and the field
name are per kind.

The compose prompt carries the rules the validator enforces — Hexagon supplies
the agent service, so the file describes only the services beside it; no
`build`, no host bind mounts, no fixed host ports, nothing privileged. **That is
a convenience and not the check.** What comes back goes through exactly the same
validation as a file typed by hand, and a refusal lands back in the editor for
the user to ask again. A prompt is not a security boundary, and this is written
out here because a reader who finds the rules stated in two places will
otherwise have to guess which copy is load-bearing.

`POST /api/images/dockerfile` becomes `POST /api/images/source`, taking
`{kind, content, instruction}` and answering `{content, summary}`. Renaming an
endpoint is churn, and it is worth paying once: the alternative is a second
handler, a second wire type and a second `claudex` method differing in a prompt
string. The route stays outside `/api/images/{id}` for the reason its comment
already gives — the image does not exist yet, and may never.

`canAsk` goes on governing every one of those buttons.

### Lifecycle and reconciliation

`Start`, `Stop` and `Delete` branch on the session's `compose` flag and act on
the project through `composex` instead of on the one container. `container_id`
still holds the **agent** container, so the terminal, the bootstrap, `RunExec`
and the VS Code proxy are untouched — which is the point of setting
`container_name` in the generated service.

`down -v` takes the project's named volumes with it, and that belongs to the
`purge` flag `Delete` already has: purge already means "the workspace too", and
a project's volumes are the same kind of thing — the work, rather than the
machinery.

The dependency containers carry `hexagon.managed=true`, `hexagon.session.id` and
`hexagon.role=service`, and `ManagedContainer` gains `Role` so `Reconcile`
attributes them to their session instead of logging "container with no session
left" for every database at every startup. That is the problem `LabelRole` was
introduced for, finally met.

### Alternatives rejected

- **Compose over the Engine API.** Above: a project, not a feature.
- **A separate environments table and page.** Above: two of everything.
- **Attaching to a user-written agent service.** Above: it moves every invariant
  into a file Hexagon would have to police.
- **Building the project with `docker compose build`.** The agent image is built
  once, at registration, and cached; `ready` goes on meaning what it means. The
  user's services come from registries, which is what refusing `build` says out
  loud.
- **Fixed host ports.** Prettier URLs, and the second session from the same
  image fails to start with a Docker error nobody can act on.
- **A YAML parser.** `docker compose config --format json` is already there,
  already normalizes, and adds nothing to `go.mod`.

## API

- `createImageRequest` gains `compose`, and `sourceType` accepts `"compose"`.
  `imageResponse` gains `compose` beside `dockerfile`.
- `POST /api/images/source` replaces `POST /api/images/dockerfile`:
  `{kind, content, instruction}` in, `{content, summary}` out.
  `httpapi.DockerfileEditor` becomes `SourceEditor`, with `Edit` and the
  unchanged `Check`.
- `GET /api/images/template` gains `canCompose` beside `canAsk`, and a starting
  compose file, embedded from `deploy/images/base/` the way the reference
  Dockerfile already is — an empty editor with a list of rules attached is a
  worse place to start than two services commented out.
- `NewSession` gains `ports: number[]`. `sessionResponse` gains
  `ports: [{"container": 3000, "host": 49154}]`, with `host` absent while the
  session is not running, because that is the truth: there is no binding to
  report until the container is up.
- A new `session.ErrComposeUnavailable`, mapped to 503 in the shape of
  `ErrVSCodeUnavailable`.
- Bodies stay under `maxImageRequestBody`; a compose file is shorter than a
  Dockerfile's worth of `RUN` lines.

## Configuration

One new setting: `docker.cli` / `HEXAGON_DOCKER_CLI`, defaulting to `docker` on
the `PATH`, in the shape of `claude.binary`. Since M2.2 a setting touches more
places than it used to, and the list is part of the work: `config.Config`,
`config.file`, `Load`, `config.Patch`, the settings response and request, the
settings view, the README table and `config.example.json`.

## UI

- `ImagesView.vue`: a Simple / Advanced switch on the create form. Simple is
  what is there now. Advanced shows the Dockerfile editor beside a compose
  editor, with a line saying what the compose file is for — the services next to
  the agent, which Hexagon supplies — and what it will not accept. Advanced is
  left out entirely when `canCompose` is false.
- Each editor carries its own **Ask Claude** control, which means the ask box,
  its pending state and the summary line stop being inline in the view and
  become one small component used three times: the simple Dockerfile, the
  advanced Dockerfile, the advanced compose file.
- `NewSessionDialog.vue`: a ports field beside the VS Code toggle, container
  ports only, saying that Hexagon picks the host side.
- `SessionView.vue`: the published ports as links, `3000 → 127.0.0.1:49154`,
  with the loopback caveat beside them.
- `api.ts` mirrors the Go structs field for field, as always.

## Impact

| Area | Change |
|---|---|
| Schema | `007`: rebuild `images` for the `source_type` CHECK and its `compose` column; add `ports` and `compose` to `sessions` |
| Store | `ImageSourceCompose`, `Image.Compose`, `Session.Ports`, `Session.Compose` |
| dockerx | `ManagedContainer.Role`, and nothing else: the port machinery is already there |
| composex | new package: `Available`, `Validate`, `Up`, `Start`, `Stop`, `Down` |
| claudex | `EditDockerfile` becomes `Edit(kind, …)`, with a compose prompt and schema beside the Dockerfile ones |
| session | `containerSpec` renders to a compose service; `Create`, `Start`, `Stop`, `Delete` and `Reconcile` branch on the compose flag; `ErrComposeUnavailable` |
| Config | `docker.cli`, and the places a setting now touches |
| API | `compose` on images, `canCompose` and a starting file on the template, `ports` on sessions, `POST /api/images/source` in place of `/dockerfile` |
| UI | the mode switch, the ask control extracted into a component, the ports field, the ports list |
| Docs | README: advanced images, published ports, and the compose keys that are refused |

## Verification

`internal/composex` — against a fake `docker` on the `PATH`, in the shape of
`internal/claudex/claudex_test.go`: the arguments carry both files and the
project name, `DOCKER_HOST` is passed through, a non-zero exit becomes an error
carrying what the CLI said, and `Available` is false when there is no binary.

Validation — a table over normalized documents: every refused key is refused and
named; the same request written another way is refused too (the long and short
form of `volumes`, `user: root` against `user: "0:0"`); a named volume is
accepted; a port with no host side is accepted; and a plain two-service file
passes.

`internal/session` — the rendered compose service carries the same image, binds,
user, labels and environment as the `ContainerSpec` for the same session, which
is the test that keeps the two from drifting; a compose session's `Stop` and
`Delete` go through the compose fake rather than through `RemoveContainer`; and
`Reconcile` does not report a dependency container as an orphan.

`internal/claudex` — the existing tests extended: the compose kind sends the
compose prompt and asks for the compose schema, an answer that is not the
requested kind is an error, and the Dockerfile path behaves exactly as it does
now, which is what says the generalisation cost nothing.

`internal/httpapi`, in the `testEnv` style over the real router — an advanced
image is refused when compose is unavailable and accepted when it is; a compose
file with a refused key answers 400 naming the key; a compose file Claude
answered with goes through the same validation, so a refused key from the model
is a 400 and not a saved image; `POST /api/images/source` refuses an unknown
kind and answers 503 with no Claude binary; a session created with ports reports
the mapping once it is running and no `host` before that; and a port colliding
with the VS Code port is refused.

By hand: an image whose compose file is a postgres, a session created from it,
`psql -h db` from the session's terminal, and a server on 3000 inside the
session reached from the browser through its published port.

## The security invariant this touches

AGENTS.md says containers run as the host user and never as root. A compose file
is the first thing a user of Hexagon could write that asks otherwise, so the
refusals above are named there as what upholds that invariant rather than as
validation, and the file gains a line saying that a user-supplied compose file is
checked against that list before anything is created from it.

Nothing else moves. The routes this point adds are in the `protected` map like
every other endpoint, published ports keep binding loopback, and the agent
container goes on running as the host user in both modes — which is the whole
reason Hexagon writes that service rather than reading it.
