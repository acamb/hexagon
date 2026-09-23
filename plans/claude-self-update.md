# Claude Code updating itself inside a container

## What was asked

Claude Code inside a session cannot update itself. The request is to give the
install a fixed group in the image and add that group to every container Hexagon
creates, so the container's user can update it, without making the install
writable by everyone.

## Current behaviour

The reference image installs Claude Code with `npm install -g`, as root, into
`/usr/local/lib/node_modules/@anthropic-ai/claude-code`, with a symlink in
`/usr/local/bin`. Every container Hexagon starts runs as the host user
(`ContainerUser`, `uid:gid`), never as root. That user owns nothing under
`/usr/local`, so `claude update` fails with `EACCES` and the session keeps the
version the image was built with.

The uid is only known at runtime: an image is built once and used by whoever
runs Hexagon. So the image cannot hand the install to its user.

## Design

A fixed gid, `dockerx.AgentGroup` (2000), bridges the two times:

- **Build time.** The reference Dockerfile gives the install to gid 2000 and makes
  it group-writable: `@anthropic-ai` recursively, and the two parent directories
  (`/usr/local/lib/node_modules`, `/usr/local/bin`) only for themselves. npm renames
  the old package inside `@anthropic-ai` and swaps the bin symlink, which only needs
  write access to the directory. `node`, `npm` and everything else in those
  directories stay root's. `/usr/local` gets the same treatment, again only for
  itself: it is npm's global prefix, and Claude Code enables auto-updates only when
  `access(prefix, W_OK)` succeeds, although npm never writes there during an
  update. Without it `claude update` works by hand, and `/status` reports "npm
  global folder isn't writable".
- **Runtime.** `dockerx.ContainerSpec` has a `GroupAdd` field, passed as
  `HostConfig.GroupAdd`, and set to `AgentGroup` by all three places that create a
  container Claude Code runs in: the session (`session.Manager.containerSpec`), the
  Claude login container, and the image editor's container (`claudex`). A compose
  session gets it through `group_add` on the overlay service. Docker applies the
  container's added groups to an exec that names a user, so the terminal, which
  execs as `ContainerUser`, has the group too.

The gid is numeric so an image needs no `/etc/group` entry for it. The Dockerfile
and the constant must agree; the constant's comment says so.

The update lives in the container's writable layer. A recreated session starts
from the image again, which is what an image is for.

### Rejected

- **World-writable install.** What the request rules out: any process under any
  uid could replace the `claude` binary.
- **Building the image for the host uid** (a build arg). Ties an image to one
  machine's user, and the images are backed up and restored across machines.
- **A `chown` when the container starts.** Needs root at startup, and nothing
  Hexagon starts runs as root.
- **Installing into the user's home.** The home is `dockerx.AgentHome`, not a
  persistent per-user place the image controls, and moving the install changes the
  PATH contract every custom image relies on.

## Impact

- No schema, API or configuration change.
- Images already built from the old template keep working, and keep not updating,
  until they are rebuilt. The editor's own image is tagged after its Dockerfile's
  content, so it is rebuilt on its own.
- The image editor's prompt asks the model to keep the group grant when it edits a
  Dockerfile that has it. That is best effort: an image without it still runs.

## Verification

- `TestSessionContainerSpec`, `TestComposeServiceMatchesTheContainerSpec`,
  `TestStartClaudeLoginBindsTheCredentialsDirectoryReadWrite` and
  `TestContainerPassesThePromptThroughTheEnvironment` check the group on each of
  the four ways a container is created.
- Manually, on the built reference image: as `1000:1000` without the group,
  `npm install -g @anthropic-ai/claude-code@<older>` fails with `EACCES`. With
  `--group-add 2000`, the same downgrade succeeds and `claude update` brings it back
  to the latest version. `claude doctor` reports auto-updates enabled with no
  warnings. `/usr/local/bin/node` stays `root:root`, `0755`. A
  `docker exec -u 1000:1000` into such a container reports group 2000.
