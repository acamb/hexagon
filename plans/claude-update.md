# "Auto-update failed: no write permission to npm prefix"

*Investigation only — no code changes in this pass. The options below are for
the user to pick from before anything is implemented.*

## What was asked

A Claude Code session started through Hexagon prints, at some point after the
CLI comes up:

> Auto-update failed: no write permission to npm prefix

The question was whether this comes from how the base image is built or from
how credentials are mounted, and what could be done about it.

## Root cause

It is neither the image build nor the credential mount directly — it is the
combination of two things that are both deliberate:

1. **The reference image installs Claude Code as root, at build time,
   into npm's system-wide prefix.**
   [deploy/images/base/Dockerfile](../deploy/images/base/Dockerfile) does:

   ```dockerfile
   FROM node:22-bookworm-slim
   RUN npm install -g @anthropic-ai/claude-code
   ```

   `node:22-bookworm-slim`'s default npm prefix is `/usr/local`, so this puts
   the package under `/usr/local/lib/node_modules/@anthropic-ai/claude-code`
   and the `claude` binary under `/usr/local/bin`, both owned by root — the
   only user that exists while a `RUN` step is building the image.

2. **The container runs as the host user, never as root.** This is a stated
   security invariant, not an accident: `cmd/hexagon/main.go` sets
   `session.Config.ContainerUser` to `fmt.Sprintf("%d:%d", os.Getuid(),
   os.Getgid())` — the uid:gid of whoever is running the `hexagon` server
   process — and `session.Manager.containerSpec` passes it straight through
   as the container's `User`. `dockerx.AgentHome` (`/home/agent`) is bind
   mounted and therefore writable by that user, but `/usr/local` is still
   the image's own read-only-in-spirit layer, owned by root: the host user
   has no write access to it inside the container.

Claude Code's own updater shells out to `npm install -g
@anthropic-ai/claude-code@latest` against that same prefix at runtime, in the
background, whenever it finds a newer version. This was verified directly
against the CLI binary installed on this machine
(`npm root -g`, version 2.1.260): it catches `EACCES`/`EPERM` from that
install, reports the outcome as `status: "no_permissions"`, and that status
renders as exactly the message above, with "Run `claude doctor`" appended.
There is no ambiguity here — the string and the code path that produces it
both exist verbatim in the shipped binary.

**Ruling out the credentials mount.** The guess that this is related to how
credentials are mounted is reasonable — `internal/session` does bind-mount a
few things read-only into the container — but it doesn't hold up:
`.credentials.json` is mounted at `$HOME/.claude/.credentials.json`, nowhere
near `/usr/local`, and a read-only *credentials* file produces a different
class of message when it's the problem — "Not logged in · Please run
/login" or "Failed to authenticate: OAuth session expired", both documented
in [plans/claude-without-a-host-cli.md](claude-without-a-host-cli.md). The
npm-prefix message is unrelated to authentication; it is purely "the process
this container runs as cannot write to the directory the package it wants to
overwrite lives in."

**A subtlety worth keeping in mind when weighing fixes:** even a container
that *could* write to `/usr/local` would only be patching its own writable
container layer. Session containers are long-lived for the life of one
session (`sleep infinity`, kept and restarted, not recreated per command), so
an in-place update would stick around for that one session — but the next
*new* session still starts a fresh container from the same image, with
whatever version was baked in at the last build. The actual, durable update
path in Hexagon's model is rebuilding the image — which the product already
supports front and center (the README's "Editing an image with Claude Code"
section, and the Images page rebuilding a Dockerfile-based image live) — and
the Dockerfile's `npm install -g @anthropic-ai/claude-code` is unpinned, so
every rebuild already picks up whatever is current on npm. In-container
self-update is, at best, a way to skip an image rebuild for the rest of one
session's life; it was never going to be how anyone stays current release to
release.

## Where this shows up

Three places create a container that runs `claude`, and any fix that isn't
purely image-level has to be applied consistently to all three — this is the
same shape of bug
[plans/claude-without-a-host-cli.md](claude-without-a-host-cli.md) describes
for credential resolution: fixed in one place and left to drift in another
looks, from the outside, like a bug in the one that was missed.

- `session.Manager.containerSpec` in `internal/session/manager.go` (session
  containers) — builds the `Env` slice and sets `User` on the
  `dockerx.ContainerSpec` it returns.
- `session.Manager.startClaudeLoginContainer` in
  `internal/session/claude_login.go` (the browser login, shared by the
  machine-wide login and per-account logins) — same two fields, on its own
  `dockerx.ContainerSpec`.
- `claudex.Container.run` in `internal/claudex/container.go` (the "Ask
  Claude" image editor, used when the server has no `claude` binary on the
  host) — builds its own `env` slice before creating the container.

Out of scope: `internal/claudex/claudex.go`, the host-runner "Ask Claude"
path. It runs as the server's own process user against the server's own
`$HOME`, entirely outside any container Hexagon builds — if that installation
hits the same message, it is a property of however Claude Code was installed
on the host, not of anything Hexagon does.

## Options

Grouped by what they can actually reach. `ContainerUser` is only known at
runtime (it's the uid:gid of whoever starts the `hexagon` process, set in
`cmd/hexagon/main.go`), and a session's base image is any of three kinds
a user can register — Hexagon's own Dockerfile, a bare registry reference
like `node:22-bookworm-slim`, or a Dockerfile with a compose file (README,
"Images"). That split is what separates the two groups below: one only ever
touches the one Dockerfile Hexagon ships as a starting point; the other
touches every session, on every image, because it comes from the container
spec Hexagon itself builds.

### A — fix the reference image only

Changes confined to
[deploy/images/base/Dockerfile](../deploy/images/base/Dockerfile). Only
covers sessions built from that exact file (or a fork of it that keeps the
fix) — a registry reference or a from-scratch custom Dockerfile is untouched.

- **A1. Loosen permissions on the npm prefix after install.** For example:

  ```dockerfile
  RUN npm install -g @anthropic-ai/claude-code \
      && chmod -R o+rwX /usr/local/lib/node_modules/@anthropic-ai/claude-code /usr/local/bin/claude
  ```

  Whatever uid the container ends up running as (unknown at build time),
  "other" permissions cover it. This doesn't cross any of the security
  invariants in AGENTS.md — those are about the host boundary: root, host
  bind mounts, ownership of files written into the workspace — but it is a
  real, worth-a-comment trade-off in its own right: the `claude` binary and
  package become writable by *anything* that runs inside the container,
  including code checked out from the cloned repository the agent is working
  in, not just by the intended host user. Given the container's threat model
  already treats "the process running inside it" as trusted (it's a single
  developer's own session, with shell access), this is a narrow, deliberate
  exception rather than a new hole — but it should be spelled out as one if
  chosen, not left implicit.

- **A2. Move the npm global prefix off `/usr/local` entirely**, into a
  directory built and opened up for this purpose alone, e.g.:

  ```dockerfile
  ENV NPM_CONFIG_PREFIX=/opt/npm-global
  ENV PATH=/opt/npm-global/bin:$PATH
  RUN mkdir -p /opt/npm-global && chmod o+rwX /opt/npm-global \
      && npm install -g @anthropic-ai/claude-code
  ```

  Same permission trade as A1, but confined to one directory built for
  exactly this instead of widening `/usr/local` as a whole — the cleaner
  member of this family, at the cost of one more line to explain.

### B — fix it where Hexagon controls every session, regardless of image

Changes to the container spec / seeded config Hexagon itself builds at the
three call sites above. Covers every session on every image, including ones
Hexagon never built, because the fix travels with the session rather than
with the image.

- **B1. Set `DISABLE_AUTOUPDATER` in the container's environment**, at all
  three sites. Verified against the installed CLI binary: this is checked
  before anything else —

  ```
  if(xe(process.env.DISABLE_AUTOUPDATER))return{type:"env",envVar:"DISABLE_AUTOUPDATER"};
  ```

  — and short-circuits the updater outright. The message disappears because
  the update attempt never runs, not because it now succeeds. The value
  that satisfies the check is also verified, not guessed — the parser is
  `["1","true","yes","on"].includes(String(v).toLowerCase().trim())`, so
  `DISABLE_AUTOUPDATER=1` is enough. This is three one-line additions to
  `Env` slices that already exist at each of the three sites above, and it
  needs no change to any Dockerfile — it works with the reference image, a
  bare registry reference, or any custom one, exactly because it's set by
  the process that starts the container rather than baked into it.

- **B2. Disable it through Claude Code's own config instead of an env var** —
  `"autoUpdates": false` in the JSON Hexagon already seeds as
  `$HOME/.claude.json`. Also verified against the binary: the config route
  is read too, gated on `installMethod !== "native"` (true for an
  npm-installed CLI, which is what the reference image produces). But it is
  weaker than B1 for a reason specific to this codebase: `seedClaudeConfig`
  in `internal/session/manager.go` explicitly skips writing when
  `$HOME/.claude.json` already exists — "A session being provisioned into
  an existing workspace keeps whatever state it already had" — so this
  would only take effect for session homes created *after* the change.
  Every session provisioned before it, and anyone who has hand-edited that
  file since, keeps seeing the message. `claudex/container.go` also writes
  its own separate, smaller JSON inline rather than calling
  `seedClaudeConfig`, so this route would need to touch two independent
  pieces of code for the same effect B1 gets from one uniform env var.

### C — leave the mechanism alone, explain it instead

- **C1. Document, in the README, that the message is expected and why**:
  auto-update can't write inside a Hexagon container by design (host-user
  containers, root-owned image layer), and the way to get a newer
  `claude-code` is to rebuild the image — which already happens on every
  rebuild, since the `npm install -g` line is unpinned. This doesn't touch
  any code, and can be paired with B1 (silence the message) or left standing
  alone (leave the message, but make it a known, explained one rather than
  an alarming unexplained one).

## Where this leaves it

B1 is the option that actually stops the message everywhere, with the least
code and no image rebuild required for existing installs; A1/A2 only help
people who both use the reference image and rely on letting a running
session self-update in place, which — per the durability point above — was
never a complete answer anyway. C1 is complementary to any of them: whatever
is chosen (or if nothing is), the README should probably say what the
rebuild-is-the-update-path story actually is, since right now nothing states
it.

One tangential note, not a proposal of its own: the Dockerfile's `npm
install -g @anthropic-ai/claude-code` has no version pin, so two rebuilds a
week apart can silently produce different `claude-code` versions. That cuts
both ways for this investigation — it's *why* C1's "rebuild to update"
framing is true today — but it also means a rebuild is not reproducible,
which is a separate question from the one asked here.

An in-place update is even less durable than "lasts for the rest of one
session" suggests: `rebuildSpec` (`internal/session/manager.go`, used
whenever a stopped or restarted session's container is recreated) builds the
container fresh from the image every time, discarding whatever the previous
container's writable layer held. So a self-update only survives between two
points in the *same* running container's life, not across a stop/start of
the same session.

If B1 is the direction taken, it belongs in the container spec Hexagon
builds, not in `config.Config`: it is not a per-installation choice like the
settings AGENTS.md's Configuration section describes (no sensible default
varies by user or environment, there's nothing to correct from a running
page, and there's no reason to ever want the in-container updater on) — it
is a fixed property of how a session container is put together, the same
way `HOME=` and `TERM=xterm-256color` already are at each of the three call
sites.

## How this would be verified, whichever option is chosen

- **By hand:** start a session on the reference image before the change and
  confirm the message appears; apply the change, rebuild, start a new
  session, confirm it doesn't.
- **In tests:** `internal/session/manager_test.go` already asserts on the
  container spec a session produces (e.g. checking `agent.User` against
  `spec.User`); a `DISABLE_AUTOUPDATER` entry in `Env` fits the same pattern,
  asserted at all three call sites rather than only the one under most
  direct test coverage.
