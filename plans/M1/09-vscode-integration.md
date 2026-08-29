# M1.9 — VS Code integration

This document is the reasoning. The step by step that implements it is
[09-vscode-integration-implementation.md](09-vscode-integration-implementation.md),
and it is the plan to follow: read this one first for why the design is what it
is, then work through that one in order.

## What was asked

> Alla creazione della sessione deve essere possibile scegliere se avere
> l'integrazione con vscode.
> Con l'integrazione attiva nella schermata della sessione deve esserci un
> bottone per lanciare un istanza web di vscode che punta al repository nel
> container in modo da poter fare review, push, ecc.

A choice made when the session is created, and, for the sessions that made it, a
button on the session page that opens VS Code in the browser on the workspace
inside the container.

## Reading of the request

The integration is two things that behave differently, and the request names
both without separating them.

One is **a place for VS Code to run**: the container has to have the editor's
files and a way for the browser to reach the port it listens on. Both are
properties of the container, decided when Docker creates it and unchangeable
afterwards. That is why the point says *alla creazione* and why the answer is a
flag on the session, not a control.

The other is **reaching it**, which is the button. Hexagon already knows how to
put a browser in touch with something inside a container — the terminal does it
over a WebSocket — and the same rules apply here: the caller is authenticated by
the session cookie, and the session is looked up scoped to its owner.

"Punta al repository nel container" settles what the editor opens: `/workspace`,
the bind mount, whether it holds a clone or the empty directory a session
without a repository gets. "Review, push, ecc." settles that the editor is
useful only in a session that also has the token, but that is point 7's switch
and not this one's business: an editor on a workspace with no credentials is
still an editor.

## Current behaviour

There is nothing to build on. `containerSpec` produces a `ContainerSpec` whose
host configuration is a list of binds and a restart policy, and `dockerx` turns
it into:

```go
config := &container.Config{Image, Cmd, Env, WorkingDir, User, Labels, Tty: false}
hostConfig := &container.HostConfig{Binds: spec.Binds}
```

No port is published or exposed anywhere in the repository, `ContainerState`
carries `{ID, Running, Status}` and nothing about the network, and the only
route that is not a JSON endpoint or the SPA is the terminal WebSocket. Three
facts about the surrounding code decide the design.

**A container keeps the mounts and the port bindings it was created with.** The
same fact that made point 7's switch creation-time makes this one creation-time.

**Docker picks the host port, and picks a new one at every start.** A binding
requested as `127.0.0.1::8443` is resolved when the container starts, so the
port is not something to store; it is something to look up.

**The bootstrap runs at provisioning and at every start.** It is where the tmux
session is created and the credential helper installed, and it is where
code-server gets started, for the same reason: it re-reads the session row every
time, so nothing has to be remembered between starts.

## Design

### code-server lives on the host, not in the image

The editor is a 350 MB release that is identical for every image. Putting it in
the reference image would add that weight to an image whose whole contract is
"git, tmux and claude on the PATH", and would mean every image already built is
one that cannot be used with the integration until it is rebuilt — the cost
point 7 went out of its way to avoid.

So Hexagon keeps one extracted release under its data directory and bind mounts
it read-only at `/opt/code-server`. The image is untouched, every existing image
gains the feature, and the release is downloaded once for the machine rather
than once per image.

`internal/codeserver` owns that directory. It has one rule worth stating: **if
`<dir>/bin/code-server` exists, it is used and never touched**. A directory that
already holds an install is never deleted, overwritten or version-checked, which
is what makes a hand-placed release work on a machine with no route to GitHub,
and what makes upgrading an explicit act — delete the directory. Only an empty
directory triggers a download, of the pinned version, over HTTPS from the
project's releases.

Rejected: **installing code-server inside the container** at first start, which
needs no host state at all but pays a hundred-megabyte download per session and
requires the container to have network.

Rejected: **extracting the tarball in Go**. The release carries symlinks and
executable bits that `archive/tar` code gets wrong on the first attempt;
`internal/gitops` already shells out to `git`, and `tar --strip-components=1`
is ten lines that are right.

### A published loopback port, with Hexagon in front of it

The container publishes `8443` on `127.0.0.1`, Docker chooses the host port, and
Hexagon reverse-proxies `/api/sessions/{id}/vscode/` to it. The path is under
`/api`, so it is in the `protected` map and behind `requireAuth` like everything
else: the editor is reachable exactly by whoever could already open the
terminal, from the same origin, with the same cookie, and it works from another
machine because the only address the browser ever contacts is Hexagon's.

code-server itself runs with `--auth none`. Hexagon's session is the
authentication; a second password would be one code-server generates, that
nobody knows, and that the proxy would have to replay.

Rejected: **linking straight to the published port**. It is the smallest change
by far, and it is wrong in two ways at once: the editor would answer without
Hexagon's cookie to anything that can reach the port, and it would not work at
all for a browser that is not on the Docker host, which is the case
`HEXAGON_PUBLIC_URL` exists for.

Rejected: **proxying to the container's IP** and publishing nothing. It exposes
less — nothing on the host at all — and it would even let the flag be flipped
after creation, since then nothing about the container is decided in advance.
It was turned down because it only works where the Docker bridge is routable
from the host, which is true on Linux and false on Docker Desktop.

What the published port costs is worth stating rather than engineering around:
while a session runs, any process on the machine that finds the port gets an
editor, with the workspace and whatever the environment holds. That is the same
boundary the README already draws around the listen address — whoever reaches
the port controls the Docker socket — and it is drawn for the same reason.

### A creation-time flag, defaulted off

`sessions.vscode`, set at creation, read by `containerSpec` and by the
bootstrap. There is no `PATCH`: the bind mount and the port binding are the
container, and changing them means building another one. The session page shows
the integration by having the button, or by not having it.

Absent from the request body it is **off**, which is the opposite of the two
flags before it and deliberately so. `autoClaude` and `propagateToken` default
on because a session without them is a diminished session; this one defaults off
because a session that never opens the editor should not be carrying a mount and
a published port for it. Existing rows default to 0 for the stronger version of
the same reason: their containers have neither, and any other value would be a
lie about what is inside them.

### What the container actually runs

The bootstrap starts code-server detached, when the session asks for it:

```sh
if ! kill -0 "$(cat /tmp/hexagon-code-server.pid 2>/dev/null)" 2>/dev/null; then
  nohup /opt/code-server/bin/code-server --bind-addr 0.0.0.0:8443 --auth none \
    --disable-telemetry --disable-update-check --disable-workspace-trust \
    /workspace >/tmp/hexagon-code-server.log 2>&1 &
  echo $! >/tmp/hexagon-code-server.pid
fi
```

Three details in it are not decoration.

**The output goes to a file.** `RunExec` returns when the exec's output stream
closes, so a background process that inherited the pipe would hold provisioning
open for as long as the editor ran.

**It binds `0.0.0.0`, not loopback.** A published port is forwarded to the
container's own interface; a server on the container's loopback would never see
a packet, and the failure looks exactly like a server that did not start.

**The guard is a pid file, not `pgrep`.** The bootstrap runs as
`sh -c "<script>"`, so its own command line contains the code-server path and
`pgrep -f` would match it every time and never start anything.

The port is a fixed 8443 rather than code-server's own 8080, which is the port a
project under `/workspace` is likeliest to want for itself. The release is
mounted read-only: the container gets to run the editor, not to modify it.
Everything code-server writes goes under `$HOME`, which is already the session's
own directory on the host, so settings and installed extensions survive a
restart and belong to one session.

### The prefix is Hexagon's, not code-server's

code-server is served at the root of its port and emits relative URLs, so the
proxy strips `/api/sessions/{id}/vscode` before forwarding — the same shape as
the `proxy_pass` recipe in code-server's own documentation. Two consequences
follow.

A request without the trailing slash is redirected to one; relative URLs
resolved against `.../vscode` instead of `.../vscode/` would all land one
segment too high.

And `guardStateChanges` cannot apply its JSON content-type rule here. That rule
exists so a mutating call has to look like it came from this frontend, and it
works because everything under `/api` is this frontend. The proxy carries a
whole other application's traffic — form posts, blobs, an upgrade to a
WebSocket — none of which is JSON or can be made to be. The exemption is
narrow: the content type check is lifted for this path only, and the same-origin
check and `requireAuth` are not.

That WebSocket is a deviation worth naming. AGENTS.md says there is no WebSocket
other than the terminal; from here on there is a second one, which Hexagon does
not speak but forwards.

### The button

An `<a target="_blank">` in the session header, next to *tmux keys*, shown only
when the session was created with the integration and is running. A session
without it shows nothing, on the same principle as the Dockerfile editor's
`canAsk`: leave the control out rather than offer one that fails.

A new tab rather than an iframe in the page. VS Code wants the whole window and
claims most of the keyboard, and the session page is already the terminal's.

## Impact

| Area | Change |
|---|---|
| Schema | `005_session_vscode.sql`: `sessions.vscode INTEGER NOT NULL DEFAULT 0 CHECK (vscode IN (0, 1))` |
| Store | `Session.VSCode`, in `sessionColumns`, `CreateSession` and `scanSession`. No setter: the flag is not editable |
| Docker | `ContainerSpec.Ports`, published on `127.0.0.1` with a host port Docker chooses; `ContainerState.Ports` so the binding can be read back; `VSCodeMount` and `VSCodePort` constants |
| Session | New `internal/codeserver` and the `VSCodeSource` interface it satisfies, nil when the server cannot provide a release; `CreateRequest.VSCode`; the mount and the port in `containerSpec`; the launch in `bootstrapScript`; `VSCodeEndpoint`, which inspects rather than remembers, and the sentinels `ErrVSCodeUnavailable`, `ErrVSCodeDisabled`, `ErrVSCodeNotReady` |
| API | `vscode` on `createSessionRequest` as a `*bool` defaulting to false, and on `sessionResponse`. `/api/sessions/{id}/vscode/{path...}` reverse-proxied, in the protected map; `guardStateChanges` lifts the content type rule for it alone |
| Config | `HEXAGON_VSCODE_DIR` / `vscode.dir`, default `<DataDir>/code-server`; `HEXAGON_VSCODE_VERSION` / `vscode.version`, the release fetched when that directory is empty |
| UI | A third checkbox in the new-session dialog, off by default; a **VS Code** link in the session header for the sessions that have it |

## Verification

In `internal/httpapi/sessions_test.go`, which already reads back the spec the
fake Docker was asked for:

- `vscode: true` creates a container with `8443` in its published ports and the
  release bind mounted read-only at `/opt/code-server`, and a bootstrap that
  starts code-server;
- the default creates a container with none of the three — the test that proves
  the integration did not change the ordinary session;
- the flag survives a round trip through `GET /api/sessions/{id}`.

A new `internal/httpapi/vscode_test.go`, with the fake reporting the port of an
`httptest` backend: a request to `/api/sessions/{id}/vscode/foo?x=1` reaches the
backend as `/foo?x=1` and its body comes back; without a cookie it is 401;
another user's session is 404; a session created without the integration is 409.
And, asserted together because they are the two halves of one decision: a `POST`
to the proxy path with a non-JSON content type is forwarded, while the same
content type on `POST /api/sessions` is still refused.

In `internal/codeserver`, `Ensure` against an `httptest` server serving a small
tarball extracts it, returns the directory, and makes no second request when
called again — the rule that an existing install is never touched.

In `internal/store`, the migration test gains a row proving an existing session
comes out with the integration off, matching the container it already has.

By hand, which is the only thing that proves the proxy really carries VS Code: a
session created with the box ticked, whose first provisioning downloads the
release; the button opens an editor showing the clone; a terminal inside that
editor works, which is the WebSocket; the source control panel commits and
pushes, which is what the point was for. Then stop and start the session and
open it again — the host port will have changed, which is exactly why nothing
stores it.
