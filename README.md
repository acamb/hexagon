<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="img/hexagon-banner-dark.png">
    <img src="img/hexagon-banner-alpha.png" alt="Hexagon — Scalable &amp; Connected Environments" width="760">
  </picture>
</p>

Hexagon runs [Claude Code](https://claude.com/claude-code) in Docker containers and puts
them in your browser. You pick a repository and a base image; it clones the repository on
the host, starts a container with that clone mounted inside, runs `tmux` in it, and renders
that terminal on a web page. Close the tab and the agent keeps working; open it again and
you are back where you left off.

It is one binary — a Go server with the frontend built into it — and it talks to the Docker
daemon on the machine it runs on.

![A session: Claude Code running in tmux inside its container, with the session's published
port beside the controls](docs/images/session.jpg)

## Features

### Sessions

A session is a repository, a container and a terminal. Choose an image, then a repository
from any account you have connected — or none at all, for a session that starts on an empty
workspace and clones something later.

The clone lives on the host, under the workspace directory, and is bind mounted at
`/workspace` in the container. The container runs as **you**, not as root, so every file the
agent writes stays yours.

Stopping a session shuts its container down and keeps the clone. Starting it again brings
the container back with a fresh `tmux`, so the previous scrollback is gone but the work is
not. Deleting removes the container, and removes the clone on disk only if you tick the box.

### The terminal

The session page attaches to the container's `tmux`. Closing the tab only detaches — Claude
Code keeps going — and reopening the page rejoins the same session. Opening a second tab
takes the terminal over from the first.

A session either opens with `claude` already running or leaves you at a shell prompt. It is
a checkbox when you create the session, on by default, and a switch on the session page
afterwards; since it is the command `tmux` was started with, flipping it on a running session
takes effect the next time that session starts. When Claude Code exits you get a shell
rather than a terminal that closes.

The **tmux keys** button in the header opens the shortcuts worth knowing, starting with how
to scroll back through the output.

### Images

![The Images page: a Dockerfile, the three kinds of source, and the box that asks Claude Code
to change it](docs/images/images.jpg)

A session runs in a container built from a base image you register. There are three kinds:

- **A Dockerfile.** Hexagon builds it and follows the build log live. The image has to
  provide `git`, `tmux` and `claude` on the `PATH` and a long-running `CMD`;
  [deploy/images/base/Dockerfile](deploy/images/base/Dockerfile) is offered as the starting
  point and is the reference for what a session needs.
- **A registry reference.** `node:22-bookworm-slim` and the like, pulled as it is.
- **A Dockerfile and a compose file.** For a project that needs a database, a cache or a
  queue running beside it.

The repository is never baked into an image: it arrives as a bind mount when the session
starts, so one image serves every project that needs the same tools.

### Images with services beside them

![The compose editor, with the rules a service has to obey and its own Ask Claude
box](docs/images/compose.jpg)

The compose file describes the services a session needs *next to* the agent — Hexagon
supplies the agent's own service, so you never describe it yourself. It is called `hexagon`,
it starts after everything you listed, and it reaches your services on the project's network
by their service name: a session can talk to a `db:` service as `db`.

The file is checked before the image is saved, so a refusal arrives while you are still
looking at the editor. A service is refused if it uses `build`, bind mounts a host path
(named volumes are fine), fixes a host port, asks for `privileged`, `cap_add`,
`security_opt` or `devices`, sets `network_mode`, `pid`, `ipc` or `uts` to `host`, or runs
as root. [deploy/images/base/compose.yaml](deploy/images/base/compose.yaml) is the starting
point. This mode needs `docker compose` on the server; without it, it is not offered.

Stopping such a session stops the whole project, and deleting it takes the project with it —
including the named volumes, if you also chose to delete the workspace.

### Editing an image with Claude Code

Under each editor there is a box: say what you want changed — "add the Go toolchain", "add a
postgres 16" — and Hexagon rewrites the file for you, showing a one-line summary and an
undo.

The call runs with every built-in tool removed, so it is a pure text transformation: no
shell, no file access, no network of its own. A compose file it writes goes through exactly
the same refusals as one you typed by hand.

It signs in the way a session does, and with the same order of preference: your default
Claude account, then the key the server was configured with, then the machine-wide login —
the file its browser login writes. Configure Claude once and both halves of Hexagon use it.

The call needs the Claude Code CLI, and where the server has no `claude` binary of its own —
a packaged install has none: the service user has no home to install one into — it runs the
CLI in a container instead, from the same image the browser login uses. That is the same
answer a little slower, plus a wait for that image the first time, and the box says which of
the two you are getting rather than leaving you to wonder why it is thinking. What it needs
either way is a Claude account, from the Accounts page.

Installing `claude` on the server is still worth it if you use this a lot: put it on the
`PATH` of the account the server runs as, or name it with `claude.binary`, and the box uses
it instead.

### Published ports

![The new-session options: what to start, which token to pass, VS Code, and the published
ports with the address they bind](docs/images/new-session.jpg)

A session can publish container ports on the host: type them as a list — `3000, 5173` — with
the address they bind beside them. Docker picks the host port, so a second session of the
same project never fails to start over a port already taken, and the session page shows each
pair as a link once the container is up.

The address defaults to `0.0.0.0`, because published ports are most useful when Hexagon runs
on another machine and a port bound to *that* machine's loopback is reachable by nobody. The
warning under the field is the whole point of it: on `0.0.0.0` those ports are open to
anyone who can reach the machine, with nothing in front of them — no password, and not
Hexagon's own sign-in. Use `127.0.0.1` to keep them local.

Ports can be changed later, but only while the session is stopped: **Ports** on its card in
the sessions list, or on the session page, edits the list and the address. A container keeps
the bindings it was created with, so saving builds a new container from the same image over
the same workspace. The clone and the session's home directory survive — they are directories
on this machine — and anything installed inside the old container by hand does not.

### VS Code in the browser

Tick the box when you create a session and its page gets a **VS Code** button that opens
`code-server` on the workspace in a new tab. The editor is not part of any image: one
release is downloaded into the data directory the first time a session asks for it, and
shared by all of them.

Unlike the ports above, this is a creation-time choice and stays one: the mount is part of
the container, and there is no equivalent of the rebuild for it.

### Accounts

![The Accounts page: the connected providers, and the list of Claude accounts sessions can run
as](docs/images/accounts.jpg)

Repositories come from the accounts you connect. GitHub is there already: it is how you
signed in.

Bitbucket is added with an Atlassian account email and an API token, created under Atlassian
account settings → Security → API tokens with `read:workspace:bitbucket` and
`read:repository:bitbucket`, plus `write:repository:bitbucket` if Claude Code should push.
Both reads are needed because Bitbucket lists workspaces first and their repositories one
workspace at a time. App passwords are not supported — Atlassian removed them in July 2026.

The GitHub box also takes a **personal access token**, and it is worth setting. The
credential that arrives with the sign-in expires after a few hours, and a container keeps
the token it was built with, so a session outlives its own ability to fetch and push. A
personal access token does not expire, and sessions created after you paste one use it for
git. Make one under GitHub Settings → Developer settings → Personal access tokens, with the
`repo` scope for a classic token or read and write access to Contents for a fine-grained
one. It is used for git only — your repositories are still listed with the account you
signed in with — so its scopes can be as narrow as you like.

Credentials are checked against a real API call before they are stored, and sealed at rest.
A token that turns out to belong to a different account is refused rather than stored: it
would produce a session pushing commits under somebody else's name.

### The provider token

A session created from a repository carries that account's credentials unless you untick the
box; a session created without one carries none unless you choose an account for it. The
clone on the host uses the credentials either way — it could not reach a private repository
otherwise — so the choice is only about what runs *inside* the container. Without the token
nothing in the session can fetch or push.

It cannot be changed afterwards: a container keeps the environment it was created with. That
is also why a GitHub session is worth giving a personal access token before it is created —
see [Accounts](#accounts). Without one it carries the token from signing in, and when that
expires the session can no longer fetch or push at all; the new-session dialog says so.

### Claude accounts

More than one Claude account can be configured on the Accounts page, without needing a shell
on the server: paste an API key from the Console, the token `claude setup-token` prints, or
add one that signs in with a subscription — press **Log in** on it to get a real terminal
running `claude auth login`. A pasted secret is checked against the CLI before it is stored.
One account is marked **default**, and a session created naming none gets it; naming another
one instead is a choice in the new-session dialog when there is more than one to choose
between.

A stopped session's account can be changed too — the **Account** button beside **Ports**, on
its card in the sessions list or on the session page. Changing it rebuilds the container from
the same image, over the same workspace, for the same reason changing its ports does: a
container keeps the credential it was created with. What it ends up authenticating as is
whatever that account holds at the moment of the rebuild, so one that has been signed out of
since fails the rebuild rather than quietly producing a container that cannot authenticate.

Each subscription account gets its own directory to sign in to, so two of them can be logged
in to at once without either overwriting the other's credential. A machine-wide login further
down the Accounts page still exists too, for a session that names no account and has no
default configured either: it writes `claude.credentials`, exactly as it always has, and a
session that resolves to it mounts that file read-only.

Either kind of login runs `claude` in a throwaway container, which means it needs an image
with Claude Code in it. On a machine where you have not built one yet it uses Hexagon's own —
the same reference image the Images page starts from, built the first time something asks for
it. The dialog says so while that build is running, and offers your own images once you have
any.

Inside a session, `claude` opens straight into the repository: each session gets its own
`$HOME/.claude.json` marked as already onboarded and already trusting `/workspace`, so it
does not greet you with the first-run flow.

### Settings

![The Settings page: the listen address, the flag that qualifies it, and a public URL an
environment variable is overriding](docs/images/settings.jpg)

The whole configuration file, edited from the browser, grouped as the file groups it, each
key labelled with the environment variable that overrides it.

Most settings are read once when the server starts, so the page shows what the process is
*running on* beside what the file now *says*, and marks the ones waiting for a restart —
including changes you made in the file by hand. Only the GitHub client id, client secret and
allowlist take effect the moment they are saved.

Nothing is written until the candidate file has been loaded successfully, so a save cannot
leave behind a configuration the server would refuse to start from. Everyone the allowlist
admits can use the page: that list is also the list of administrators, and it cannot be
saved without you on it.

## Getting started

### Requirements

Docker, and a GitHub account. The released binary is static and needs nothing else at
runtime except `git`, which the package pulls in for you. Building from source needs Go 1.26
and Node 22.

### 1. Install

**Debian and Ubuntu.** Take the `.deb` from the
[latest release](https://github.com/acamb/hexagon/releases/latest) and install it with
`apt`, which pulls its dependencies in — `dpkg -i` would leave the package half-configured:

```sh
curl -fsSLO https://github.com/acamb/hexagon/releases/download/v0.1.0/hexagon_0.1.0_amd64.deb
sudo apt install ./hexagon_0.1.0_amd64.deb
```

It installs `/usr/bin/hexagon`, a system service running as a dedicated `hexagon` user, and
`/etc/hexagon/config.json`. Under `DEBIAN_FRONTEND=noninteractive` every answer is its
default, which is the wizard below. The package registers a **systemd** service; on Devuan
or any other Debian without systemd, use the installer below — the package still carries the
OpenRC init script, at `/usr/share/doc/hexagon/examples/hexagon.openrc`.

**Anywhere else, with systemd or OpenRC.** The installer downloads the latest release,
checks it against the release's `SHA256SUMS`, and sets up a service under whichever of the
two this machine runs. Download and run it.

```sh
curl -fsSLO https://raw.githubusercontent.com/acamb/hexagon/master/install.sh
sh install.sh
```

It installs for **you** by default — `~/.local/bin`, a service running as your account, with
your `~/.claude` and your docker group — and offers a system-wide install:
 `sh install.sh --system`, `--yes` for every default, and
`--version v0.1.0` to pin a release. Piping it into `sh` works too and takes every default,
which is the unattended form.

Per-user means `systemctl --user` under systemd, and an OpenRC **user service** in
`~/.config/rc/init.d` (OpenRC 0.55 and newer) otherwise. OpenRC keeps a user's services in
that user's own session, so to have yours start at boot the installer prints the one root
command that arranges it — the counterpart of `loginctl enable-linger`. With neither init
system, the files go in and the installer says how to run the server yourself.

**From source**, which is also how you work on Hexagon:

```sh
make build && ./bin/hexagon
```

`make dev` runs the Go server on `:8080` and the Vite dev server on `:5173`, which proxies
the API to it. Open <http://localhost:5173> — **not** `127.0.0.1:5173`: the origin check
compares hostnames, and the two are different origins. [AGENTS.md](AGENTS.md) has the
conventions.

Whichever path you took, **the account the server runs as must be in the `docker` group** —
the package puts the `hexagon` user there, and the installer tells you if you are not. It is
root-equivalent access to the machine, which is what running containers requires.

Both installers ask, as their first question, whether to configure Hexagon now or leave it
to the wizard. The default is the wizard, and the rest of this section assumes it.

They ask for the **listen address** and the **public URL** either way, because the wizard is
a page in a browser and can only be reached where the server binds: the default,
`127.0.0.1:8080`, is unusable on a machine you reach over the network, and nothing in the
browser can move it — the address is read when the server starts. If the pair you give would
publish Hexagon in plaintext (an address open to the network and a public URL that is not
https) they ask once more, because whoever reaches that port controls the Docker socket and
the session cookie would travel in the clear. Saying yes writes `insecureHttp`; saying no
leaves the server refusing to start until the two agree, which is the same refusal it makes
on its own.

### 2. Register a GitHub OAuth App

You can skip this: the first-time wizard asks for these values in the browser and shows the
exact callback URL for the public URL you gave it. Doing it first, at
<https://github.com/settings/developers> → **New OAuth App**:

| Field | Value |
|---|---|
| Application name | Hexagon |
| Homepage URL | `http://127.0.0.1:8080` |
| Authorization callback URL | `http://127.0.0.1:8080/api/auth/callback` |

Then **Generate a new client secret**. The callback URL has to match exactly: GitHub
compares it verbatim against what the server sends, so it has to be built from the address
you actually open — `http://localhost:5173/api/auth/callback` when you run `make dev`.

Signing in asks GitHub for the `repo` scope, because that is what Claude Code needs to clone
and push.

### First run

Started with no GitHub client id and secret, Hexagon does not refuse to run. It logs a line
like

```
WARN first-time setup is open until somebody signs in url=http://127.0.0.1:8080/setup password=K7QX-4M2A-...
```

As a service that line is in the journal, or — OpenRC having no journal — in the log file
the init script names, which the installer prints when it finishes:

```sh
journalctl -u hexagon | grep 'first-time setup'          # systemd, the package or --system
journalctl --user -u hexagon | grep 'first-time setup'   # systemd, the installer's default
grep 'first-time setup' /var/log/hexagon.log             # OpenRC, --system
grep 'first-time setup' ~/.local/state/hexagon/hexagon.log   # OpenRC, per-user
```

The server serves a wizard at that address which asks for the password, then for the client
id, the client secret and the accounts allowed to sign in. It shows the exact callback URL
to register on GitHub, writes everything into the configuration file — creating it with mode
`600` if it is not there — and reconfigures the running server, so signing in works without
a restart.

The password lives only in that process's memory: a restart prints a new one and retires the
old. The wizard stays open until somebody signs in successfully, so a client secret with a
typo can be fixed from the same page. After the first sign-in it closes for good.

### Uninstalling

```sh
sudo apt remove hexagon    # or: sudo apt purge hexagon
sh uninstall.sh            # what install.sh put there; --system for a system install
```

`uninstall.sh` finds the layout and the init system by itself, so it takes no arguments
beyond `--system`. Both keep the session workspaces — git clones that may carry commits
nobody pushed — and print where they are. `apt purge` also removes `/etc/hexagon` and the database, and the
installer asks about both, defaulting to keeping them.

### Publishing it

Hexagon has no TLS of its own. To reach it from anywhere but the machine it runs on, leave
it bound to loopback and put a reverse proxy in front: [deploy/proxy/](deploy/proxy/) is an
nginx configuration known to work, with a compose file and the two values to change.

The server refuses to start on an address the network can reach unless its public URL is
`https`, so serving it in plaintext is something you have to say out loud rather than
something you can do by accident.

## Configuration

Every setting can come from a JSON configuration file or from the environment, and has a
default that suits a single user on a developer machine. **The environment wins over the
file, which wins over the defaults.**

| Variable | File key | Default | |
|---|---|---|---|
| `HEXAGON_CONFIG` | — | `~/.config/hexagon/config.json` | Where the configuration file is; `-config <path>` overrides it |
| `HEXAGON_ADDR` | `addr` | `127.0.0.1:8080` | The interface and port the server binds |
| `HEXAGON_PUBLIC_URL` | `publicUrl` | `http://127.0.0.1:8080` | The origin your browser uses; the OAuth callback and the origin check derive from it |
| `HEXAGON_INSECURE_HTTP` | `insecureHttp` | — | Set to anything to serve a non-loopback address without https, which the server otherwise refuses to do |
| `HEXAGON_DATA_DIR` | `dataDir` | `~/.local/share/hexagon` | Database and secret key |
| `HEXAGON_WORKSPACE_ROOT` | `workspaceRoot` | `<data dir>/workspaces` | One directory per session, holding its workspace |
| `HEXAGON_GITHUB_CLIENT_ID` | `github.clientId` | — | From the first-time wizard when it is not set |
| `HEXAGON_GITHUB_CLIENT_SECRET` | `github.clientSecret` | — | From the first-time wizard when it is not set |
| `HEXAGON_ALLOWED_USERS` | `github.allowedUsers` | — | Who may sign in: comma-separated in the environment, a JSON array in the file. A number is a GitHub account id, anything else a login — prefer ids, since a login is released when an account is renamed and can then be claimed by somebody else |
| `HEXAGON_GITHUB_API_URL` | `github.apiUrl` | `https://api.github.com` | Override for GitHub Enterprise |
| `HEXAGON_BITBUCKET_API_URL` | `bitbucket.apiUrl` | `https://api.bitbucket.org/2.0` | Override |
| `HEXAGON_SECRET_KEY` | `secretKey` | `<data dir>/secret.key` | 32 bytes, base64. Generated on first run |
| `HEXAGON_DEBUG` | `debug` | — | Set to anything for debug logging |
| `HEXAGON_CLAUDE_CREDENTIALS` | `claude.credentials` | `~/.claude/.credentials.json` | Mounted read-only into a session that resolves to no Claude account. Empty disables the mount. Also the file the machine-wide login writes |
| `ANTHROPIC_API_KEY` | `claude.anthropicApiKey` | — | Handed to a session that resolves to no Claude account |
| `HEXAGON_CLAUDE_BINARY` | `claude.binary` | `claude` on `PATH`, then `~/.local/bin/claude` | Rewrites a Dockerfile or a compose file from the Images page. Without one, that runs in a container instead |
| `HEXAGON_CLAUDE_MODEL` | `claude.model` | — | Model for that call; empty leaves the choice to the CLI |
| `HEXAGON_GIT_USER_NAME` | `git.userName` | — | Git identity for clones and for commits made inside containers |
| `HEXAGON_GIT_USER_EMAIL` | `git.userEmail` | — | |
| `HEXAGON_VSCODE_DIR` | `vscode.dir` | `<data dir>/code-server` | Where the code-server release is kept, downloaded once for the machine |
| `HEXAGON_VSCODE_VERSION` | `vscode.version` | the pinned version | Release fetched when the directory is empty |
| `DOCKER_HOST` | `docker.host` | SDK default | Docker Engine endpoint |
| `HEXAGON_DOCKER_CLI` | `docker.cli` | `docker` on `PATH` | Runs `docker compose` for images that carry a compose file |
| `HEXAGON_MAX_SESSIONS_PER_USER` | `limits.maxSessionsPerUser` | `20` | Sessions one account may have at once |
| `HEXAGON_MAX_CONCURRENT_BUILDS` | `limits.maxConcurrentBuilds` | `2` | Image builds one account may have in flight |
| `HEXAGON_PUBLIC_RATE_PER_MINUTE` | `limits.publicRatePerMinute` | `60` | Requests a minute, per client address, to the routes that answer without a session |

### The configuration file

[config.example.json](config.example.json) is a complete one. Copy it to
`~/.config/hexagon/config.json`, or keep it anywhere and point `-config` at it. The server
looks for `-config`, then `HEXAGON_CONFIG`, then the default path. A packaged install is
told about `/etc/hexagon/config.json` through `HEXAGON_CONFIG` in its unit, and the
examples land in `/usr/share/doc/hexagon/examples/`.

Five things are worth knowing:

- **A file named with `-config` or `HEXAGON_CONFIG` must exist.** Ignoring a path you asked
  for would start the server configured by accident. The default location is optional.
- **It must not be readable by other users.** It can hold the OAuth client secret, an API
  key and the key that seals stored tokens, so the server refuses a file with wider
  permissions than `600`.
- **The directory has to be writable too**, for the Settings page and the first-time wizard
  to save: the file is replaced by writing a new one beside it and renaming it, which never
  leaves a half-written configuration or a moment with wider permissions. The packages give
  `/etc/hexagon` to the `hexagon` user for exactly this. Both pages say so when they find it
  otherwise, rather than failing at the save.
- **An unknown key is an error.** A misspelled `allowedUsers` would otherwise be dropped in
  silence, and a dropped allowlist is an authentication bypass.
- **`claude.credentials` is the one key where an empty string means something**: mount
  nothing. Remove the key to go back to the default path.

Almost all of it can also be edited from the Settings page. The one exception is the secret
key: a wrong value there could not be corrected from the page that wrote it, because it
would make every stored token undecryptable.

The listen address *can* be edited there, and it is the one setting whose mistake the page
cannot undo. It is read when the process starts, so a bad value is saved, marked as waiting
for a restart, and correctable right up until that restart — after which the page is behind
a port nobody can reach, and the only way back is the file.

## Security notes

Hexagon drives the Docker socket and holds credentials for your repositories. Anyone who can
reach it and hold a session can run anything on the machine it runs on. These are the
properties it maintains, and the ones it deliberately does not.

**One instance is meant for one person, and that person owns the machine it runs on.** The
allowlist and the per-user scoping keep *unauthenticated* callers out and keep the data model
honest; they are not a sandbox between two people who have both signed in. A session drives the
Docker socket, so whoever holds one already controls the host — signing in is not a privilege
boundary between people you would not hand a shell. Put such people on their own instance. What
the container *does* wall off is the code a session runs — an untrusted repository, or the
autonomous agent — from the host, which is why it runs as you and never as root.

- **There is no way in without signing in.** Every endpoint except the health check, the
  login handshake and the first-run wizard requires a session, the terminal WebSocket
  included. Admission is re-checked against the allowlist on every request, not only at
  login, so removing somebody takes effect immediately; their existing sessions are deleted
  at the next startup.
- **It binds loopback by default and should stay there.** To publish it, put a TLS reverse
  proxy in front and leave the bind where it is. The server refuses to start on an address
  the network can reach unless `publicUrl` is https, or `insecureHttp` says the plaintext is
  deliberate.
- **Containers run as you, never as root**, and a user-supplied compose file is screened
  before anything is created from it — no `privileged`, no added capabilities, no host
  namespaces, no host bind mounts, no fixed host ports, no `build`, no service running as
  root — against Docker's own normalized view of the file. Treat the screen as a guard-rail
  against a careless compose file, not a sandbox around a hostile one: the Compose format has
  other routes to the host it does not yet model (a named volume backed by a bind driver, a
  host network defined at the top level, `device_cgroup_rules`, `env_file`, and more). Since
  the file's author is the instance's own operator — see the one-person rule above — tightening
  it further is defense in depth, not a trust boundary.
- **A published session port has nothing in front of it.** Neither has code-server on its
  own port. Hexagon's sign-in guards the API and the VS Code proxy, not a port you asked it
  to publish — which is why the address field carries a warning. code-server's own port
  always stays on loopback, whatever a session chose for its own.
- **Tokens are sealed at rest** with AES-256-GCM under the server key, and never leave the
  server: no response ever carries one. Only the SHA-256 of a session cookie is stored.
- **Session cookies are `HttpOnly` and `SameSite=Lax`.** Over https the cookie is `Secure`
  and named `__Host-hexagon_session`, a prefix the browser enforces so no sibling subdomain
  can overwrite it over plaintext.
- **A mutating API call is refused** unless it shows a same-origin signal — a matching
  `Origin`, or `Sec-Fetch-Site: same-origin` — and a JSON content type.
- **The routes that answer without a session are rate limited** per client address, and the
  health check tells an unauthenticated caller only that the server is up. Behind the proxy
  the address comes from `X-Forwarded-For`, believed only because the peer is loopback;
  exposed directly, the header is ignored.
- **The `repo` scope is broad**, and that token is handed to a container running an
  autonomous agent. That is the deliberate trade-off of the project, and the reason it can be
  declined per session. A session gets one account's credentials at most, chosen when it is
  created, and never another's.
