# Installers — a Debian package and a setup script

## What was asked

> ora vorrei che il make file avesse un target per produrre un installer .deb.
> Oltre a questo come metodo secondario di installazione vorrei un install.sh /
> uninstall.sh che fa il setup su una macchina chiedendo all'utente le
> configurazioni proponendo dei default opinionated.

and, after the first reading:

> sia il deb che l'installer devono chiedere come prima cosa se procedere con la
> configurazione o lasciarla vuota per lanciare il first time wizard via web.

and, after the second:

> l'installer .sh deve prendere hexagon dall'ultima release su github (non c'e'
> ancora). Includi nel piano anche di aggiornare gli step di installazione per
> usare il deb o l'installer.

and, after the third:

> vorrei che l'installer .sh fosse compatibile anche con openRC

Six decisions were taken with the user while this was written, and they are
treated here as settled rather than re-argued: the package installs a system
service running as a dedicated `hexagon` user; `make deb` builds inside a Debian
container; the version comes from a `VERSION` file and nothing else;
`install.sh` asks, and defaults to installing for the invoking user; the binary
`install.sh` installs is downloaded from the latest GitHub release rather than
built or found locally; and the README's installation steps become the package
and the script, with building from source kept for development. A seventh was taken
with the OpenRC reading: a per-user install there is an OpenRC **user service**, the
package stays systemd-only, and the root command that makes a user's services start
at boot is printed rather than run.

## Where this document sits

Not in a milestone folder. This is not one of the four points of
[milestones/M2.md](milestones/M2.md), and that file is the user's to write, so
the analysis lands at `plans/installer.md` by request rather than as
`plans/M2/05-…`. If it is later folded into a milestone, it moves; nothing in it
depends on the location.

## Reading of the request

**An installer is not a build target with a different name.** `make build`
produces an artifact; an installer produces a *running service on a machine*,
which means deciding four things this repository has so far left unstated: what
user the server runs as, where its data lives, how it survives a reboot, and how
it is told about Docker. Those are the questions the package answers, and the
reason it is worth having at all.

**The two methods are one layout seen twice.** A `.deb` and a shell script that
disagree about where the binary goes are two products to support. They are
designed here as one system layout with two ways of reaching it, and `make
install` is the single definition of that layout: the package stages it, the
release tarball *is* that stage, and the script unpacks it.

**The script installs a release, not the tree it is standing in.** Reading
`./bin/hexagon`, or running `make build` when it is absent, made `install.sh` a
convenience for somebody who had already cloned the repository and installed Go
and Node — the one audience that does not need an installer. The machine this
script is written for has Docker, `curl` and nothing else, so the binary has to
come from somewhere: the latest release of `github.com/acamb/hexagon`, produced
by the same `make` invocation that produces the package, so the package and the
script put identical bytes on disk.

**The release it downloads does not exist.** There is no tag, no release and no
published artifact, so a good half of this is deciding what a release *is* — the
tag, the assets, the names `install.sh` will construct from them — and cutting
`v0.1.0` before the script can be pointed at anything. Until it exists the script
installs from an artifact named on the command line, which is also how it is
developed.

**The installation steps are part of the deliverable.** A README that still says
`make build && ./bin/hexagon` describes a project without installers, whatever
the tree contains. The steps are rewritten around the package and the script,
and the OAuth App registration moves after them — it was only first because
there was nothing to run before it.

**The machine this is written on cannot run what was written.** The first three
readings assumed systemd without saying so — `systemctl`, `systemctl --user`,
`loginctl enable-linger`, `journalctl`. The development machine is Gentoo with
OpenRC 0.63 and no `systemctl` at all, so `install.sh` there copied the files,
registered nothing, and printed instructions for a program that is not installed.
An installer that only works on the distributions it was tested on is a shell
script with a nice preamble.

**Two init systems is a mapping, not a fork.** Everything either of them is asked
is the same question — install a description of the service, enable it, start it,
say where its log is — so the script gets four small functions with one case each
and stops naming an init system anywhere else. What genuinely differs is not the
commands but two facts about OpenRC: it has no journal, so the first-time wizard's
password has to go to a file the installer names; and it runs a user's services
from that user's own session, so *staying* started across a logout is a root
command rather than something a user install can arrange.

**The first question is a real feature, not a courtesy.** Hexagon already has a
first-time wizard that configures a server from the browser
([M2/01-first-time-wizard.md](M2/01-first-time-wizard.md)). An installer that
asked for a GitHub client secret at a terminal prompt, on a machine the user is
about to open in a browser anyway, would be duplicating a page that does the job
better — it shows the exact callback URL, validates against GitHub, and can be
corrected on the spot. So the installers ask whether to configure at all, and
the default answer is *no, use the wizard*. Answering *yes* is for the
unattended case: a machine being provisioned, where a browser round-trip is the
thing to avoid.

**"Opinionated defaults" means Enter is always a correct answer.** Every prompt
in both installers has a default, and the shortest complete run of `install.sh`
is four presses of Enter.

## Current behaviour

There is no installation story at all.

- `make build` writes `bin/hexagon` with a plain `go build -o bin/hexagon
  ./cmd/hexagon` — no `-trimpath`, no stripping, no version stamping. The
  resulting 20 MB binary carries DWARF and, on the machine this was written on,
  is dynamically linked against glibc (`ldd bin/hexagon` shows `libc.so.6` and
  `libresolv.so.2`). It is a development artifact, not something to hand to
  another machine.
- There is **no version anywhere**: no `VERSION` file, no `-X` ldflags, no
  `version` constant, no `-version` flag, and `git tag` is empty.
  `web/package.json` says `0.0.0`, which is a Vite placeholder.
- There is **no release**: `git tag` is empty, GitHub has no release, and no
  artifact has ever been published anywhere. The upstream repository is not named
  in the tree either — it is `acamb/hexagon`, from `git remote`, and an installer
  that downloads has to say so somewhere.
- There is no `install` target, no `DESTDIR`, no service unit, no packaging
  metadata, and no CI. The README's "Getting started" is `make build &&
  ./bin/hexagon`.
- The `docker` group requirement is **stated nowhere in the repository**, even
  though the process is useless without socket access.
- Nothing here knows about **any init system but systemd**, and the machine this
  is developed on runs OpenRC 0.63 with no `systemctl` installed.

Two properties of the running server constrain everything below.

- **Containers run as the server's own uid**: `ContainerUser:
  fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())` in `cmd/hexagon/main.go`. The
  service must therefore run as a real account that owns the workspace tree and
  can reach the Docker socket — not as `nobody`, and not under `DynamicUser`.
- **The configuration file must be `0600`**: `config.go` refuses a file with any
  group or other bits set, because it can hold the OAuth client secret and the
  key that seals stored tokens. Anything that writes that file, package included,
  is held to it.

## Design

### The version

A `VERSION` file at the root, one line, `0.1.0`, is the single source of truth.
No git suffix: a package built from a dirty tree and one built from a tag say
the same thing, and the discipline that keeps that honest is bumping the file.

`cmd/hexagon/main.go` gains `var version = "dev"` and a `-version` flag printing
`hexagon <version>`; the Makefile stamps it with `-ldflags "-s -w -X
main.version=$(VERSION)"`. `-s -w` is not decoration — it takes roughly a
quarter off a binary that is shipped, and nothing reads Hexagon's DWARF in
production.

The drift this invites — the file bumped and the package not, or the reverse —
is caught by a test in `cmd/hexagon/main_test.go` that reads `../../VERSION` and
compares it to the stamped value's default. It is the kind of failure that is
invisible until a release, which is exactly what a test is for.

### What a release contains

`git tag` is empty, so the contract `install.sh` depends on is invented here
rather than observed. A release is a tag `v<VERSION>` — the file's contents with
a `v` in front, and nothing else — carrying five assets:

```
hexagon_<version>_linux_amd64.tar.gz
hexagon_<version>_linux_arm64.tar.gz
hexagon_<version>_amd64.deb
hexagon_<version>_arm64.deb
SHA256SUMS
```

The names are interface, not decoration: `install.sh` builds the tarball's name
from the tag and `uname -m`, so renaming an asset breaks every future install of
every past version. The `.deb` names are `dpkg-deb`'s own convention and are
already what `make deb` writes.

**The tarball is the `make install` stage, rolled up.** It unpacks into a single
directory holding the prefixed layout and nothing else:

```
hexagon_0.1.0_linux_amd64/
  usr/bin/hexagon
  lib/systemd/system/hexagon.service
  usr/share/doc/hexagon/{copyright,README.md.gz,examples/...}
```

which is what keeps the claim above — one layout, several ways of reaching it —
true and checkable: `dpkg -L hexagon` and `find` over an unpacked tarball list
the same paths. A bare binary in a `.tar.gz` would have been smaller and would
have forced `install.sh` to carry the unit file in a heredoc, which is the second
source of truth this whole section exists to avoid.

### One definition of the layout

```
/usr/bin/hexagon
/lib/systemd/system/hexagon.service
/usr/share/doc/hexagon/copyright
/usr/share/doc/hexagon/changelog.Debian.gz
/usr/share/doc/hexagon/README.md.gz
/usr/share/doc/hexagon/examples/config.example.json
/usr/share/doc/hexagon/examples/proxy/{compose.yaml,nginx.conf,README.md}
```

created at install time, not shipped:

```
/etc/hexagon/            hexagon:hexagon 0750
/etc/hexagon/config.json hexagon:hexagon 0600
/var/lib/hexagon/        hexagon:hexagon 0700    (dataDir, and $HOME)
```

The directory belongs to the service user and not to root, which is a correction
made after the first installs: `config.Update` replaces the file by writing a
candidate beside it and renaming it over the old one — never a half-written
configuration, never a moment at wider permissions — and both halves of that need
write permission on the *directory*. Owned by root at `0750` the file was
writable and the directory was not, so the settings page and the wizard failed at
the save with `permission denied` on a temporary file. `config.Writable`, which
is what those two pages ask before offering a form at all, was wrong the same
way: it probed the file. It probes the directory now.

A `make install` target with `PREFIX` and `DESTDIR` lays out the first block and
nothing else. The package's build script calls it with `DESTDIR=<stage>`, and so
does the target that rolls the release tarball, which is what `install.sh`
unpacks — so the places that could disagree about where the binary goes all read
from the same rule.

The reference proxy and `config.example.json` ship as documentation because they
are the two things an operator looks for immediately after the service starts —
how to publish it, and what else can be configured. The reference session
Dockerfile does not: it is already compiled into the binary (`embed.go`) and
served to the Images page, so a copy on disk would be a second source of truth.

### `/etc/hexagon/config.json` is generated, not a conffile

Declaring it a dpkg conffile would be wrong twice over. The settings page and
the wizard rewrite that file at runtime (`internal/config/update.go`), so dpkg
would prompt about "local modifications" on every upgrade of a file the
application owns; and a conffile ships in the package, where a client secret
cannot go.

So `postinst` writes it, only when it is absent, and an upgrade never touches
it. It has to exist at all because the unit sets `HEXAGON_CONFIG`, and a path
named explicitly must exist or `config.Load` refuses to start — deliberately, so
that a server is never configured by accident.

### The first question

Both installers ask this before anything else:

> Hexagon can be configured now, or left empty so that the first-time wizard
> configures it from the browser. The wizard prints a one-time password to the
> service log and asks for the same values there.
>
>   1. Use the web wizard  (default)
>   2. Configure now

Left to the wizard, the generated file is the minimum that names the daemon's
paths — no `github` block at all, which is precisely what holds the wizard open:

```json
{
  "addr": "127.0.0.1:8080",
  "publicUrl": "http://127.0.0.1:8080",
  "dataDir": "/var/lib/hexagon"
}
```

*Configure now* unfolds the rest, each with a default on Enter:

| | default | file key |
|---|---|---|
| Listen address | `127.0.0.1:8080` | `addr` |
| Public URL | `http://127.0.0.1:8080` | `publicUrl` |
| GitHub OAuth client id | empty | `github.clientId` |
| GitHub OAuth client secret | empty | `github.clientSecret` |
| Allowed GitHub logins | empty (package) / `$USER` (script) | `github.allowedUsers` |
| Git user name / email | from `git config --global` | `git.userName`, `git.userEmail` |

An id and secret left empty here are **not an error**: the wizard stays open, and
the allowlist and git identity that were given are still written. The two paths
meet rather than fork, which is what makes the choice safe to get wrong.

The client id prompt carries the callback URL to register on GitHub, derived
from the public URL just answered — `<publicUrl>/api/auth/callback`. It is the
value GitHub compares verbatim, and the one nobody reconstructs correctly from
memory.

#### The address is asked either way

*Revised after the installers shipped, at the user's request.* The two address
questions were originally part of *configure now*, and the wizard path took the
defaults. That was wrong, and the reason is the wizard itself: it is a page in a
browser, so it can only be reached at the address the server binds. A Hexagon
installed on a remote machine and left on `127.0.0.1:8080` has a first-time
wizard nobody can open — and nothing in the browser can move it, because the
listen address is read when the process starts and the pages that could change
it are behind it. The one setting the wizard cannot recover from is therefore the
one the installer must not assume.

So the listen address and the public URL are asked before the branch, and only
the GitHub and git answers are behind it. The generated file is unchanged in
shape; it simply carries the values that were given.

Asking them of every install brings the pair's own refusal forward with it.
`config.checkTransport` rejects an address open to the network with a public URL
that is not `https`, which until now was a corner of *configure now* and is now
the ordinary answer for a machine on a LAN. Both installers therefore ask one
more question when the pair meets that description — plaintext, deliberately? —
and write `insecureHttp` only when it is answered yes. Answering no fails: the
script stops with what to do instead, and the package writes the file, says the
server will refuse to start and names the two ways out. Neither installer sets
the key on its own, and the debconf default is `false`, so an unattended install
that nobody answered fails closed rather than publishing a Docker socket in the
clear.

### In the package: debconf

`packaging/deb/templates` and `packaging/deb/config`, with `debconf` added to
`Depends`.

- `templates` declares `hexagon/configure-now` as a `select` with the two
  choices, and one template per follow-up question. The client secret is
  `Type: password`, which is what keeps it out of `config.dat` and in
  `passwords.dat` (0600, root) instead.
- `config` is the script debconf runs *before* unpacking: it sources
  `/usr/share/debconf/confmodule`, asks the first question, then the two
  addresses, and asks the rest only when the answer was *configure now*. It
  evaluates the address pair itself — the same `case` the server's
  `checkTransport` makes — to decide whether `hexagon/insecure-http` is worth
  asking at all. Asking from `config` rather than from `postinst` is what lets
  `apt` collect every question up front instead of stopping halfway through an
  install.
- `postinst` reads the answers with `db_get`, writes the configuration file, and
  then clears the secret with `db_set … ""`. The secret's home is a `0600` file
  owned by `hexagon`; a second copy sitting in a database that outlives the
  package is the kind of thing this repository does not leave behind.
- `postrm purge` calls `db_purge`.

This also settles the unattended case for free: under
`DEBIAN_FRONTEND=noninteractive` every answer is its default, and the default is
the wizard. Reading stdin from `postinst` instead would have broken `dpkg -i` in
a script, in an image build, and under any apt frontend — which is why debconf is
the only mechanism considered here, not the most convenient of several.

### `make deb`

```make
DEB_ARCH  ?= amd64
DEB_IMAGE ?= golang:1.26-bookworm

deb: build-web
	docker run --rm -u $$(id -u):$$(id -g) \
	  -v $(CURDIR):/src -w /src \
	  -v $(HOME)/go/pkg/mod:/go/pkg/mod \
	  -e HOME=/tmp -e GOCACHE=/tmp/gocache -e GOFLAGS=-buildvcs=false \
	  $(DEB_IMAGE) packaging/deb/build.sh $(VERSION) $(DEB_ARCH)
```

The container is not a convenience. `dpkg-deb` is not installed on the machine
this was written on, and the host's Go toolchain links against the host's libc;
building in `golang:1.26-bookworm` — which is Debian, so it has `dpkg-deb` —
removes both problems at once. The frontend is built on the host first, because
`web/node_modules` is already there and the Go image has no Node; `web/dist` is
then embedded by the `go build` that runs inside.

`packaging/deb/build.sh`, running in the container:

1. `go build` with `CGO_ENABLED=0` and the version ldflags. Static, so the
   artifact does not depend on the container's glibc either — and so
   `DEB_ARCH=arm64` cross-builds for free, through a two-row `GOARCH` mapping.
2. `make install DESTDIR=$stage PREFIX=/usr`, then gzip the two files that need
   it.
3. Render `control.in` with version, architecture and `Installed-Size`
   (`du -ks`).
4. Copy `config`, `postinst`, `prerm`, `postrm` in at 0755 and `templates` at
   0644; write `md5sums`.
5. Roll the stage into `dist/hexagon_<version>_linux_<arch>.tar.gz` — the release
   tarball is this same directory, before the packaging metadata is copied into
   it — and skip this step when only a package was asked for.
6. `dpkg-deb --build --root-owner-group $stage dist/hexagon_<version>_<arch>.deb`.
   `--root-owner-group` is what lets the container run as the invoking user and
   still produce root-owned paths in the archive, so `make deb` never leaves
   root-owned files in the working tree.

`control.in`:

```
Package: hexagon
Version: @VERSION@
Architecture: @ARCH@
Installed-Size: @SIZE@
Maintainer: Andrea Cambieri <andrea.cambieri@gmail.com>
Section: devel
Priority: optional
Homepage: https://github.com/acamb/hexagon
Depends: git, adduser, ca-certificates, debconf (>= 0.5) | debconf-2.0
Recommends: docker.io | docker-ce, docker-compose-plugin
Suggests: nginx
Description: web UI for running Claude Code sessions in Docker containers
 Hexagon clones a repository on the host, starts a container with that clone
 mounted inside, runs tmux in it, and renders that terminal on a web page.
 It is one binary: a Go server with the frontend built into it.
```

`git` is a hard dependency: clones run on the host (`internal/gitops/clone.go`).
`tar` is Essential, so it is not listed. Docker is a `Recommends` rather than a
`Depends` because it can come from Docker's own repository under a name Debian
does not know, or be remote via `DOCKER_HOST` — and a daemon that is down at
startup is already only a warning, not a failure to boot.

### `make release`

`make deb` builds one package for one architecture. That is the right unit of
work and it is not a release, so a second target runs the same container once per
architecture and adds the tarballs and the checksum file:

```make
RELEASE_ARCHES ?= amd64 arm64

release: build-web
	rm -rf dist
	for a in $(RELEASE_ARCHES); do $(MAKE) artifacts ARCH=$$a; done
	cd dist && sha256sum hexagon_* > SHA256SUMS
```

`artifacts` is the container run that `make deb` already is, with the script
inside doing one more step: after `make install DESTDIR=<stage> PREFIX=/usr` it
rolls the stage into `hexagon_<version>_linux_<arch>.tar.gz` and *then* hands the
same stage to `dpkg-deb`. One `go build` per architecture — `CGO_ENABLED=0`, the
version ldflags, `GOARCH` from `ARCH` — so the binary in the tarball and the
binary in the `.deb` are the same file rather than two builds that ought to
agree. `make deb` stays as the shortcut for one package on one architecture, and
calls the same script.

`SHA256SUMS` is written last and covers everything beside it. It is what
`install.sh` verifies against, and what somebody downloading a `.deb` by hand can
check for themselves.

### Cutting the first release

Not automated, and recorded in `AGENTS.md` beside `make deb` because it is a
maintainer command rather than a user one:

```sh
make test && make vet && make release
git tag -a v0.1.0 -m 'hexagon 0.1.0' && git push --tags
gh release create v0.1.0 --title 'hexagon 0.1.0' --notes '...' dist/*
```

A `.github/workflows/release.yml` doing this on a pushed tag is the obvious next
step and is deliberately not in this change: the container build already removes
the toolchain drift a CI job would remove, there is no CI in this repository to
extend, and what `install.sh` depends on is the *names* of the assets, not the
machine that produced them. Adding the workflow later changes nothing written
here.

**Nothing downstream can be verified before this step happens**, which is why it
sits in the middle of the order below rather than at the end, where releases
usually go.

### The unit

```ini
[Service]
User=hexagon
Group=hexagon
SupplementaryGroups=docker
Environment=HOME=/var/lib/hexagon
Environment=HEXAGON_CONFIG=/etc/hexagon/config.json
ExecStart=/usr/bin/hexagon
Restart=on-failure
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/var/lib/hexagon /etc/hexagon
PrivateTmp=yes
```

Two lines want a comment in the file itself.

`HOME` is set explicitly rather than left to systemd's account lookup, because
`config.load` opens with `os.UserHomeDir()` and fails outright without it.

The hardening stops where it does deliberately. `ReadWritePaths` has to include
`/etc/hexagon`, because the settings page writes the configuration file back.
Nothing stronger than this is used: the process drives the Docker socket, which
is root-equivalent by construction, so sandboxing it further buys appearance
rather than safety. `PrivateTmp` is safe only because workspaces live under
`/var/lib/hexagon` and never in `/tmp`.

### The second init system: OpenRC

Detection is by what is *running*, not by what is installed: `[ -d /run/systemd/system ]`
first — the same guard the package's `postinst` uses — then `rc-update` and `rc-service`
on the path, then neither. A Gentoo with systemd installed but booted under OpenRC has no
`/run/systemd/system` and lands in the second branch, which `have systemctl` would have got
wrong. `--init systemd|openrc|none` overrides the answer.

The mapping, four combinations of two modes and two init systems:

| | systemd | OpenRC |
|---|---|---|
| system service | `/lib/systemd/system/hexagon.service` | `/etc/init.d/hexagon` + `/etc/conf.d/hexagon` |
| user service | `~/.config/systemd/user/hexagon.service` | `~/.config/rc/init.d/hexagon` + `~/.config/rc/conf.d/hexagon` |
| enable and start | `systemctl [--user] enable --now` | `rc-update [--user] add hexagon default`, `rc-service [--user] start` |
| restart on failure | `Restart=on-failure` | `supervisor=supervise-daemon`, `respawn_delay=5` |
| the wizard's password | `journalctl [--user] -u hexagon` | `grep` in the log file the conf.d names |
| survives a logout | `loginctl enable-linger` | the `user.<login>` service, one root command, printed |

**One init script for both modes.** `packaging/hexagon.openrc` ships in the stage as
`usr/share/doc/hexagon/examples/hexagon.openrc` and is copied — never generated — into
`/etc/init.d` or `~/.config/rc/init.d`. It can be one file because OpenRC sources
`conf.d/<name>` on its own, so every difference between a system install and a per-user one
is data: `HEXAGON_BIN`, `HEXAGON_HOME`, `HEXAGON_CONFIG`, `HEXAGON_LOG`, `HEXAGON_PIDFILE`,
and only for a system install `HEXAGON_USER` / `HEXAGON_OWNER`. That is the opposite of the
systemd case, where the user unit has to be written by hand because it cannot carry `User=`
— and it is why the OpenRC script is not a heredoc in `install.sh`.

`command_user` is set **only** for a system install: OpenRC's own guide says a user service
must not have one, since it already runs as whoever starts it, and runtime files belong in
`XDG_RUNTIME_DIR`.

**OpenRC has no journal**, which is not a detail here: the one-time password that opens the
first-time wizard is printed to the log and nowhere else. `output_log` and `error_log` in
the init script point at `/var/log/hexagon.log`, or at
`${XDG_STATE_HOME:-~/.local/state}/hexagon/hexagon.log` for a user install, and the
installer's last lines name that file instead of a `journalctl` invocation. `start_pre`
creates the file with `checkpath`, and creates its *directory* only when it is missing —
for a system install that directory is `/var/log`, whose ownership must not be rewritten.

**Two things a user install has to arrange itself**, both found by running it: a user's
runlevels are created when their OpenRC session first runs, and `rc-update --user add`
refuses a runlevel that is not there, so `install.sh` creates
`~/.config/rc/runlevels/{boot,default,shutdown}`; and `checkpath` creates one directory
rather than a path, so the log's parent is created by the installer.

**Boot persistence is printed, not done.** OpenRC starts a user's services from a
`user.<login>` service that only root can add, so the installer ends with the two commands
rather than reaching for `sudo` on its own — unlike `loginctl enable-linger`, which the user
can run themselves and the script therefore offers to run.

The account creation in system mode grew a third branch for the same reason: Debian's
`adduser` and busybox's are different programs under one name, so the order is Debian
`adduser`, then `useradd` (Gentoo, Fedora, Arch), then busybox `adduser` + `addgroup`
(Alpine).

### Maintainer scripts

`postinst configure`:

1. `adduser --system --group --home /var/lib/hexagon --shell /usr/sbin/nologin hexagon`
2. `chown hexagon:hexagon /var/lib/hexagon`, `chmod 0700`
3. If `getent group docker` succeeds, `adduser hexagon docker`; otherwise print
   a warning naming the exact command to run once Docker is installed. Installing
   in the wrong order should not fail the install.
4. Create `/etc/hexagon`, and `config.json` from the debconf answers when it is
   absent, `0600` and owned by `hexagon`. Then clear the secret out of debconf.
5. Guarded by `[ -d /run/systemd/system ]`: `daemon-reload`, then
   `enable --now`, or `restart` when the unit is already enabled.
6. Print what to do next, which depends on the first answer: either open
   `<publicUrl>/setup` and read the one-time password with
   `journalctl -u hexagon | grep 'first-time setup'`, or open `<publicUrl>` and
   sign in — with the callback URL to register in both cases.

Upgrades are handled by being idempotent rather than by branching on dpkg's
version argument: `adduser` on an existing account is a no-op, the configuration
file is only written when absent, and the last step restarts instead of enabling.

`prerm remove` stops the unit; `postrm remove` reloads systemd.

`postrm purge` calls `db_purge`, removes `/etc/hexagon` and
`/var/lib/hexagon/{hexagon.db*,secret.key,claude-login,code-server}`, and then
`deluser hexagon` — but **keeps `/var/lib/hexagon/workspaces`**, printing the
path and the command to remove it.

That is a deliberate deviation from Debian policy, which wants purge to remove
all state, and the reason belongs in a comment in `postrm` and not only here:
those directories hold git clones of the user's repositories, which may carry
commits that were never pushed. A `dpkg --purge` that deletes them silently is a
worse failure than a directory left behind, and the second is recoverable.

### `install.sh`

One POSIX `sh` script, `set -eu`, nothing beyond coreutils, `tar`, and one of
`curl` or `wget` — the only dependency the download adds, and the one thing the
preflight refuses to continue without, since there is no way to fetch anything if
both are missing. `--yes` accepts every default for a scripted run.

It opens with a preflight that reports rather than refuses — is `docker` there
and is the socket reachable, is `git` there, is the invoking user in the `docker`
group — and then gets a binary, which it downloads **before it asks anything**:
a machine that cannot get one should fail in five seconds, not after six
answers.

#### Where the binary comes from

`https://github.com/acamb/hexagon/releases/latest` redirects to the newest
release's page, and the tag is the last component of where it lands:

```sh
url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$base/releases/latest")
tag=${url##*/}
```

One request, no token, and nothing to parse but a path. The REST endpoint that
answers the same question in JSON would need a parser this script does not have —
`jq` is not on a fresh Debian, and a `sed` that pulls a field out of JSON is the
kind of code that works until the day it does not — and it is rate limited to 60
requests an hour per address, which one office behind one NAT can reach.

With the tag known, the rest are ordinary URLs under
`$base/releases/download/$tag/`: the tarball for this architecture, and
`SHA256SUMS`. The checksum is checked with `sha256sum -c` on the single line for
the downloaded file, **before anything is unpacked**, and a mismatch stops the
install with both digests printed rather than falling back to installing the file
anyway. The archive is then unpacked into a temporary directory and copied into
place from there, so a truncated download cannot half-replace an installation
that was working a moment ago.

Three overrides, consulted in this order:

| | |
|---|---|
| `--tarball <path>` | install this artifact, download nothing |
| `--version <tag>` | install that release rather than the latest |
| `HEXAGON_REPO` | repository to download from, default `acamb/hexagon` |

`--tarball` is what makes the script testable before any release exists, and what
a developer uses against `dist/` after `make release`. It verifies too, when a
`SHA256SUMS` sits beside the file it was given — which is exactly the shape of
`dist/` — and says out loud that it is installing an unverified local artifact
when there is none, rather than quietly holding a local path to a lower standard
than a download. `HEXAGON_REPO` costs one variable and makes a fork installable.

The architecture comes from `uname -m`, mapped `x86_64` → `amd64` and
`aarch64`/`arm64` → `arm64`; anything else stops, naming the two that exist and
pointing at the source build. `uname -s` other than `Linux` stops too: the
service is a systemd unit, and a server whose containers run as the invoking uid
against a local Docker socket is not something this repository has ever claimed
to do on macOS.

If the redirect 404s — the state of the world until `v0.1.0` is cut — the message
says the repository has no releases yet and prints the source alternative,
instead of leaving a `curl` exit status for the reader to interpret.

#### The prompts survive `curl ... | sh`

The one-liner people paste puts the *script* on stdin, so a `read` inside `ask()`
swallows the script's own text or hits EOF, and an installer that asks six
questions becomes one that asks none, badly. So `ask()` reads from `/dev/tty`
whenever stdin is not a terminal; where there is no `/dev/tty` either — an image
build, a provisioning run — it takes every default, exactly as `--yes` does.
Given that the default answers are the wizard, a user-mode install and a loopback
bind, a piped run is a correct unattended install rather than a degraded one. The
README documents downloading the script and running it, and mentions the pipe as
the unattended form, which is what it is.

The service itself is never touched inline: `service_install`, `service_enable_start`,
`service_enable_hint` and `service_log_hint` carry one case per init system, and nothing
after the paths block names systemd or OpenRC. The paths block is where a mode and an init
system meet, and it is the only place that has to grow when a third one arrives.

**Question 1** is the configure-or-wizard question above. It comes before the
layout question even though the layout decides the paths, because it decides how
many questions there are at all, and answering it the default way should not mean
first sitting through a screenful of others.

**Question 2** is the layout, defaulting to `user`:

| | user (default) | system |
|---|---|---|
| binary | `~/.local/bin/hexagon` | `/usr/bin/hexagon`, and the rest of the stage |
| config | `~/.config/hexagon/config.json` | `/etc/hexagon/config.json` |
| data | `~/.local/share/hexagon` | `/var/lib/hexagon` |
| unit | `~/.config/systemd/user/hexagon.service` | `/lib/systemd/system/hexagon.service` |
| runs as | you: your `~/.claude`, your docker group | the `hexagon` user |

System mode re-execs under `sudo` and then repeats what `postinst` does — the
`hexagon` account, the group, the directories, the unit — copying the unpacked
stage into place instead of calling `make install`, which a script that never saw
the repository does not have. User mode takes `usr/bin/hexagon` out of the same
stage and writes its own unit, because a user unit genuinely differs: no `User=`,
no `SupplementaryGroups=`, and paths under `$HOME`. Everything else the two
methods do, they do identically.

**Question 3 onwards.** The first block is asked only after *configure now*:
listen address, public URL, client id (with the callback URL printed), client
secret read with the terminal echo off, allowed logins, git identity. Always
asked: the data directory, whether to enable and start the service now, and — in
user mode — whether to `loginctl enable-linger` so it survives logout.

Four helpers carry the whole script, so there is one place to change how a
question is asked:

```sh
ask()     { # ask "prompt" "default"; honours --yes by echoing the default
          }
confirm() { # confirm "question" "Y"|"N" -> exit status
          }
secret()  { # ask with stty -echo, restored through a trap so Ctrl-C cannot
            # leave the terminal mute
          }
json_str() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }
```

`json_str` earns its place: two of the answers are free text that lands in JSON,
and a git user name with an apostrophe or a Windows-style path would otherwise
produce a file the server rejects at parse time.

Three rules govern what is written.

- **Only keys that got a value are emitted.** An unknown key is a hard error
  (`DisallowUnknownFields`), and `claude.credentials` means *mount nothing* when
  present and empty — so it is omitted, never written as `""`.
- **`umask 077` around the write**, and an existing configuration file is never
  overwritten without an explicit `y`, defaulting to no.
- **The transport check is mirrored before writing**, not after: a non-loopback
  address with a non-https public URL is refused at the prompt, with the advice
  the server itself gives. Writing a file that makes the service fail to start,
  and only then discovering it, is the failure mode this avoids.

### `uninstall.sh`

Detects which layout **and which init system** is present rather than asking again — and
corrects the second by what is on disk, because a machine can have been rebooted into
another init system since the install. On OpenRC it also removes the log file: it is not
user data, and it holds every one-time password the wizard has ever printed. Stops and disables the
unit, removes the unit file and the binary, and then asks two questions that both
default to **keep**: remove the configuration file, and remove the data directory
— naming it, and saying that it contains the session workspaces. Same reasoning
as `postrm purge`.

### The installation steps in the README

"Getting started" today asks the reader to register a GitHub OAuth App, then to
install Go and Node and run `make build`. Both halves are wrong once this lands:
the second because there is a package and a script, the first because the wizard
exists precisely so that nobody has to register an OAuth App before they have
something running to point it at. The section is reordered around what exists.

**Requirements** becomes Docker and a GitHub account, and stops there. The
released binary is static and needs nothing at runtime but `git`, which the
package depends on; Go 1.26 and Node 22 move down into the source path, which is
where they are actually needed.

**1. Install** replaces "Run it", with the paths in the order most readers want
them:

```sh
# Debian, Ubuntu
curl -fsSLO https://github.com/acamb/hexagon/releases/latest/download/hexagon_0.1.0_amd64.deb
sudo apt install ./hexagon_0.1.0_amd64.deb

# anywhere else with systemd
curl -fsSLO https://raw.githubusercontent.com/acamb/hexagon/master/install.sh
sh install.sh
```

`apt install ./file.deb` rather than `dpkg -i`, because it pulls `Depends` in
instead of leaving a half-configured package and a reader looking up `apt-get -f
install`. The `.deb` line carries a version because
`/releases/latest/download/` can only serve a filename you can already spell —
the script has the redirect for that, a human has the releases page.

Each path gets the few sentences that matter and no more: both ask the
configure-or-wizard question and both default to the wizard; the service is
`hexagon`, system-wide from the package and `--user` from the script's default
mode; the account it runs as must be in the `docker` group, which the package
arranges and the script checks and tells you about. **From source** keeps `make
build && ./bin/hexagon`, absorbs the `make dev` paragraph, and is framed as what
it now is — the development path, not the way to install this.

**2. Register a GitHub OAuth App** moves after installing and becomes explicitly
optional, with the wizard as the recommended route. Its table stays as it is,
with a note that the wizard shows the exact callback URL for the public URL you
gave it; the table is for people who would rather do it first.

**First run** keeps the wizard explanation and gains the packaged form of its one
command — `journalctl -u hexagon` for the system service, `journalctl --user -u
hexagon` for the user one — because the line it tells you to look for is not on a
terminal any more.

**Publishing it** and **Configuration** are unchanged, except that the
configuration file's path gains `/etc/hexagon/config.json` beside
`~/.config/hexagon/config.json`, since a packaged install is told about its file
through `HEXAGON_CONFIG` in the unit. **Uninstall** is a short closing section:
`sudo apt remove hexagon`, or `purge` with the sentence about workspaces being
kept on purpose, or `./uninstall.sh`.

### Alternatives rejected

**`dpkg-deb` on the host, or a hand-rolled `ar` archive.** A `.deb` is an `ar`
archive of three members and can be assembled with `ar` and `tar` alone, which
would have worked on this machine where `dpkg` is absent. Rejected once the build
moved into a container: with Debian available, `dpkg-deb` is there anyway, and it
computes what a hand-rolled script would have to get right by hand.

**`nfpm`.** A Go packager needing no system dependency, and a path to `.rpm`
later from the same file. Rejected as a build-time download of a large tool for
a job `dpkg-deb` already does inside a container we are running regardless.

**A systemd *user* service as the package's default.** It matches Hexagon's
design more closely — your `~/.config`, your `~/.claude`, your docker group — but
a `.deb` cannot enable a unit for a user it does not know, and the result would
be a package that installs a service nobody started. It survives as the default
of `install.sh` instead, which is where a per-user install belongs.

**Asking from `postinst` over stdin.** Simpler than debconf and wrong in every
non-interactive install.

**Building the Go binary on the host and only packaging in a container.** Would
have worked with `CGO_ENABLED=0`, and was the first design. Rejected because it
leaves the artifact's toolchain dependent on whatever the developer's machine
has, which is the drift a release process exists to remove.

**The GitHub REST API for "latest".** `api.github.com/repos/.../releases/latest`
is the documented way and hands back the asset URLs outright. Rejected for the
parser and the rate limit, above; the redirect answers the same question with
`curl` alone.

**Installing the `.deb` from `install.sh` when `dpkg` is present.** It sounds
like a kindness and it recreates exactly the problem two methods were designed to
avoid: the script would be a thin wrapper on Debian and a real installer
everywhere else, two behaviours to keep in step — and its default, a per-user
install, has no package form at all. The README sends Debian users to the package
directly, which is an instruction rather than an inference.

**A bare binary as the release tarball.** Smaller, and it would make the unit
file something `install.sh` carries in a heredoc: a second copy of a file the
package also installs, free to drift. The tarball ships the staging directory
instead.

**Falling back to a source build when the download fails.** Rejected because it
turns a clear failure — no release, no network, an architecture that was never
built — into several minutes of silence followed by a different failure, on a
machine that by assumption has neither Go nor Node.

**A system init script running as you, for the per-user install on OpenRC.**
`command_user="$USER"` in `/etc/init.d/hexagon` would have kept the identity that
matters and started at boot with no extra step — at the price of needing root for
the install whose whole point is not needing it, and against OpenRC's own guidance
that a user service must not set `command_user`. Rejected in favour of user
services, with the boot command printed for whoever wants it.

**Teaching the `.deb` about OpenRC.** Debian and Ubuntu are systemd, the `postinst`
is already behind `[ -d /run/systemd/system ]`, and Devuan is one `install.sh` away.
Rejected as a third branch through three maintainer scripts that are dense already;
the package does ship the init script as an example, so nothing is lost.

**Writing the OpenRC init script from a heredoc in `install.sh`.** It is what the
systemd *user* unit does, so it would have been consistent — and it would have put a
second copy of the service definition in a second file, free to drift from the one
the package installs. Rejected: the script ships in the release, and conf.d carries
what differs.

**Pre-building the reference session image at install time.** Tempting — a fresh
install has no images, and the first session needs a build. Rejected: it would
put a multi-minute network operation inside `postinst`, and the Images page is
where that build belongs, with a progress display in front of it.

## Configuration

**No new setting.** Nothing here adds a field to `config.Config`, to
`config.file`, to `config.Patch`, or a row to the README table. That is the
point: the package configures Hexagon entirely through the file and the
environment that already exist, and the wizard it defers to writes the same file
through the same code path. An installer that needed a new setting to work would
be evidence that the layout was wrong.

The only new knobs are Makefile variables — `PREFIX`, `DESTDIR`, `DEB_ARCH`,
`DEB_IMAGE` — and they configure the build, not the server.

## Impact

| File | Change |
|---|---|
| `VERSION` | new |
| `Makefile` | `VERSION`, ldflags, `install` (unit, OpenRC script, docs), `artifacts`, `deb`, `release`, `dist` in `clean` |
| `cmd/hexagon/main.go` | `version` variable and `-version` flag |
| `cmd/hexagon/main_test.go` | version-drift test |
| `packaging/hexagon.service` | new |
| `packaging/hexagon.openrc` | new: one init script for both modes, driven by conf.d |
| `packaging/deb/{build.sh,control.in,templates,config,postinst,prerm,postrm,copyright,changelog.Debian}` | new; `build.sh` also rolls the release tarball |
| `install.sh`, `uninstall.sh` | new; `install.sh` downloads the latest release, and both detect systemd or OpenRC |
| `.gitignore` | `dist/` |
| `README.md` | "Getting started" rewritten: install the package or run the script, source build demoted, OAuth App made optional and moved after, the `docker` group, `DEBIAN_FRONTEND=noninteractive`, an uninstall section |
| `AGENTS.md` | `make deb`, `make install` and `make release` in Commands, `packaging/` in Layout, the release sequence |
| GitHub | the `v0.1.0` tag and release, carrying the five assets |

No migration, no API change, no frontend change.

Suggested order, each step leaving the tree working: the analysis; then the
version plumbing; then the unit and `make install`, verifiable with `make install
DESTDIR=/tmp/stage` and a `find`; then the package; then `make release`, and the
tag and release published from it — the point at which there is something to
download; then the scripts, written against `--tarball dist/...` and finished
against the real release; then the documentation, which cannot be written
honestly until the URLs it prints resolve.

## Verification

```sh
make test && make vet
make build && ./bin/hexagon -version
make deb                       # -> dist/hexagon_0.1.0_amd64.deb
make release                   # -> two tarballs, two packages, SHA256SUMS
cd dist && sha256sum -c SHA256SUMS
tar -tzf hexagon_0.1.0_linux_amd64.tar.gz   # the stage, under one directory
```

The maintainer scripts are then exercised in a container, which is the only
honest test of them on a machine with no dpkg. There is no systemd as PID 1
there, so `postinst` takes its `[ -d /run/systemd/system ]` guard and skips the
enable — which is exactly the path worth checking, and why the binary is started
by hand afterwards.

```sh
docker run --rm -it -v "$PWD/dist:/pkg" debian:bookworm bash
  apt-get update && apt-get install -y systemd git ca-certificates

  # 1. nobody there to ask: every default, so the wizard path
  DEBIAN_FRONTEND=noninteractive dpkg -i /pkg/hexagon_0.1.0_amd64.deb
  cat /etc/hexagon/config.json          # three keys, no github block
  dpkg -P hexagon

  # 2. asked, and answered "configure now"
  dpkg -i /pkg/hexagon_0.1.0_amd64.deb  # warns: no docker group
  cat /etc/hexagon/config.json          # github.clientId, allowedUsers
  debconf-show hexagon | grep -i secret # cleared, not retained

  dpkg -L hexagon                       # the layout above
  ls -l /etc/hexagon/config.json        # hexagon:hexagon 0600
  id hexagon                            # exists, nologin
  runuser -u hexagon -- env HEXAGON_CONFIG=/etc/hexagon/config.json \
    /usr/bin/hexagon                    # starts, logs the setup password
  dpkg -r hexagon && dpkg -P hexagon    # clean; workspaces kept, with a notice
```

The download path is exercised first against the artifacts `make release` just
wrote, which is the only way to test it before the release exists, and then
against the release itself once it is published:

```sh
./install.sh --tarball dist/hexagon_0.1.0_linux_amd64.tar.gz
curl -fsSLI -o /dev/null -w '%{url_effective}\n' \
  https://github.com/acamb/hexagon/releases/latest     # -> .../tag/v0.1.0
./install.sh                                           # resolves it, verifies, installs
./install.sh --version v0.1.0                          # pinned, same result

HEXAGON_REPO=acamb/nope ./install.sh   # "no releases yet", not a curl error
truncate -s -1k dist/hexagon_0.1.0_linux_amd64.tar.gz          # dist/SHA256SUMS still there
./install.sh --tarball dist/hexagon_0.1.0_linux_amd64.tar.gz   # refused, nothing installed
```

The truncation check is the one worth writing down: the failure it guards against
is a half-downloaded archive unpacked over a working installation, and the test
is that `/usr/bin/hexagon` — or `~/.local/bin/hexagon` — is exactly as it was.

On OpenRC the per-user path is exercised without root and without touching the real
runlevels, by pointing `HOME` and the XDG variables at a temporary directory:

```sh
HOME=$t XDG_CONFIG_HOME=$t/.config XDG_STATE_HOME=$t/.local/state \
  sh install.sh --tarball dist/hexagon_0.1.0_linux_amd64.tar.gz --user --use-wizard --yes
rc-service --user hexagon status              # started
grep 'first-time setup' $t/.local/state/hexagon/hexagon.log
pkill -x -f $t/.local/bin/hexagon             # supervise-daemon brings it back in 5s
sh uninstall.sh --user --yes                  # stopped, deleted from the runlevel, log gone
```

The kill is the part worth keeping: `respawn_delay=5` with `respawn_max=0` is the claim
that this matches `Restart=on-failure`, and a new pid and a fresh "listening" line in the
log are what settle it.

The system path goes in a container, where `rc-update add` works but `rc-service start`
cannot — OpenRC is not PID 1 there, and the failure is the expected one, with `install.sh`
falling back to printing the command to run the server by hand:

```sh
docker run --rm -v "$PWD/dist:/pkg:ro" -v "$PWD/install.sh:/install.sh:ro" alpine sh -c '
  apk add --no-cache openrc git tar coreutils
  sh /install.sh --tarball /pkg/hexagon_0.1.0_linux_amd64.tar.gz --system --use-wizard --yes
  cat /etc/conf.d/hexagon; id hexagon; ls -l /etc/runlevels/default/'
```

Alpine is the deliberate choice of container: it is the one distribution where the account
is created by busybox's `adduser` rather than by `useradd`, so it tests the branch that a
Gentoo or a Debian never reaches.

Then the interactive behaviour, including the form people will actually paste:

```sh
curl -fsSL https://raw.githubusercontent.com/acamb/hexagon/master/install.sh | sh
sh install.sh < /dev/null      # no terminal at all: every default, no hang
```

The first has to ask its questions on the terminal despite stdin being the
script; the second has to take the defaults and finish, rather than block on a
`read` that will never return.

And the script end to end, on a real machine:

```sh
./install.sh                   # every default -> the wizard path
systemctl --user status hexagon
journalctl --user -u hexagon | grep 'first-time setup'
# open the printed /setup URL, complete the wizard, create a session
./uninstall.sh                 # keep config and data at both prompts

./install.sh                   # again, answering "configure now"
# with a client id and secret: signing in works with no wizard at all
```

The second run is what proves the two paths meet: the file it writes has to be
one the wizard would also have produced, and the server has to come up into a
working sign-in rather than into `/setup`.

The one thing neither test covers automatically is that the packaged service can
actually reach the Docker socket, since the test container has no daemon. It is
checked by hand once, on a machine with Docker: `systemctl status hexagon` with
no "docker is unreachable" warning, and a session created through the UI.

## Security

The invariants in [AGENTS.md](../AGENTS.md) are what this has to preserve while
adding a way to install the thing that holds them.

- **The configuration file stays `0600`.** Written by `postinst` under a tight
  umask, written by `install.sh` under `umask 077`, and refused by the server
  otherwise. The package never *ships* it, so a secret cannot end up inside an
  archive that is copied around.
- **The OAuth client secret does not accumulate copies.** Declared as a debconf
  password template, so it never enters `config.dat`, and cleared from the
  debconf database as soon as `postinst` has written the file that owns it.
- **The download is verified before it is used.** `https` to `github.com`,
  `SHA256SUMS` from the same release, `sha256sum -c` before the archive is
  unpacked, and a hard stop on a mismatch. What this buys is exactly one thing —
  a corrupted, truncated or partially served download cannot be installed — and
  not one thing more: the checksum file shares an origin with the artifact, so it
  is no defence against a compromised release. Signing the artifacts
  (`minisign`, `cosign`) is the answer to that and needs a key and a place to
  publish it, which is a decision for whoever cuts releases and not something to
  invent here. The README says which of the two it is.
- **The listen address stays loopback**, in the generated file and as the
  default of every prompt. The transport check that refuses a public plaintext
  bind is mirrored in `install.sh` so the refusal happens at the prompt rather
  than at the next boot.
- **The service does not run as root**, and cannot become root: `User=hexagon`,
  `NoNewPrivileges=yes`. It is in the `docker` group, which is root-equivalent —
  that is inherent to what Hexagon does, and the README says so rather than
  pretending the hardening in the unit changes it.
- **`/var/lib/hexagon` is `0700`**, matching what `config.load` and `store.Open`
  already enforce for the directories and files they create.

## Open item: there is no LICENSE

A Debian package must carry `/usr/share/doc/hexagon/copyright`, and this
repository has no licence file at all. Choosing one is not a packaging decision,
so `packaging/deb/copyright` ships in the machine-readable format saying what is
true today: `Copyright: 2026 Andrea Cambieri`, all rights reserved. Adding a
`LICENSE` at the root later turns that file into a two-line reference to it.
