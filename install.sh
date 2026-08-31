#!/bin/sh
# Hexagon installer: downloads the latest release and sets up a service, under
# systemd or OpenRC, whichever this machine runs.
#
#   sh install.sh                 ask, install for the invoking user
#   sh install.sh --system        install the system service, as the deb does
#   sh install.sh --yes           take every default, ask nothing
#   sh install.sh --tarball F     install a local artifact instead of downloading
#   sh install.sh --version v1.2.3   install that release rather than the latest
#   sh install.sh --init openrc   force an init system instead of detecting one
#
# Every question has a default, and the shortest complete run is four presses of
# Enter. The default answers are: leave the configuration to the first-time
# wizard, install for this user, bind loopback — so a run with no terminal at
# all is a correct unattended install rather than a degraded one.
set -eu

REPO=${HEXAGON_REPO:-acamb/hexagon}
BASE="https://github.com/$REPO"
SOURCE_HINT="build from source: git clone https://github.com/$REPO && cd hexagon && make build"

ASSUME_YES=0
MODE=
CONFIGURE=
TARBALL=
TAG=
INIT=
WORK=
STAGE=

usage() {
	# The header of this file is the help text, when there is a file to read:
	# piped into sh, $0 is the shell and there is nothing to quote.
	if [ -r "$0" ]; then
		sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'
	else
		echo "install.sh [--system|--user] [--yes] [--tarball FILE] [--version TAG]"
	fi
}

die() {
	printf 'install.sh: %s\n' "$*" >&2
	exit 1
}

have() {
	command -v "$1" >/dev/null 2>&1
}

cleanup() {
	if [ -n "$WORK" ] && [ -d "$WORK" ]; then rm -rf "$WORK"; fi
}
trap cleanup EXIT

# ---------------------------------------------------------------- asking

# `curl ... | sh` puts the script itself on stdin, so a read here would swallow
# the script's own text and every question would answer itself, badly. Ask on
# the terminal when there is one; when there is none — an image build, a
# provisioning run — take the defaults, exactly as --yes does.
TTY=
if [ -t 0 ]; then
	TTY=/dev/tty
	if [ ! -r "$TTY" ]; then TTY=; fi
elif [ -r /dev/tty ]; then
	TTY=/dev/tty
fi

ask() { # ask "prompt" "default" -> the answer on stdout
	_prompt=$1
	_default=$2
	if [ "$ASSUME_YES" = 1 ] || [ -z "$TTY" ]; then
		printf '%s [%s]\n' "$_prompt" "$_default" >&2
		printf '%s' "$_default"
		return 0
	fi
	printf '%s [%s]: ' "$_prompt" "$_default" >&2
	if ! IFS= read -r _answer <"$TTY"; then _answer=; fi
	if [ -z "$_answer" ]; then _answer=$_default; fi
	printf '%s' "$_answer"
}

confirm() { # confirm "question" Y|N -> exit status
	_question=$1
	_default=$2
	case "$_default" in
	Y | y) _hint="[Y/n]" ;;
	*) _hint="[y/N]" ;;
	esac
	if [ "$ASSUME_YES" = 1 ] || [ -z "$TTY" ]; then
		printf '%s %s %s\n' "$_question" "$_hint" "$_default" >&2
		_answer=$_default
	else
		printf '%s %s: ' "$_question" "$_hint" >&2
		if ! IFS= read -r _answer <"$TTY"; then _answer=; fi
		if [ -z "$_answer" ]; then _answer=$_default; fi
	fi
	case "$_answer" in
	[yY]*) return 0 ;;
	*) return 1 ;;
	esac
}

secret() { # secret "prompt" -> the answer on stdout, never echoed
	_prompt=$1
	if [ "$ASSUME_YES" = 1 ] || [ -z "$TTY" ]; then
		printf ''
		return 0
	fi
	printf '%s: ' "$_prompt" >&2
	_saved=$(stty -g <"$TTY" 2>/dev/null || true)
	if [ -n "$_saved" ]; then
		# Restored through a trap as well, so Ctrl-C cannot leave the terminal
		# mute for whatever the user does next.
		trap 'stty "$_saved" <"$TTY" 2>/dev/null || true; echo >&2; exit 130' INT
		stty -echo <"$TTY" 2>/dev/null || true
	fi
	if ! IFS= read -r _value <"$TTY"; then _value=; fi
	if [ -n "$_saved" ]; then
		stty "$_saved" <"$TTY" 2>/dev/null || true
		trap - INT
	fi
	echo >&2
	printf '%s' "$_value"
}

# Two of the answers are free text that lands in JSON: a git user name with an
# apostrophe would otherwise produce a file the server rejects at parse time.
json_str() {
	printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'
}

# "alice, bob" -> "alice", "bob"
json_list() {
	printf '%s' "$1" | tr ',' '\n' |
		sed 's/^[[:space:]]*//; s/[[:space:]]*$//; s/["\\]//g; /^$/d; s/.*/"&"/' |
		paste -sd, -
}

# ---------------------------------------------------------------- arguments

while [ $# -gt 0 ]; do
	case "$1" in
	--yes | -y) ASSUME_YES=1 ;;
	--system) MODE=system ;;
	--user) MODE=user ;;
	--configure-now) CONFIGURE=now ;;
	--use-wizard) CONFIGURE=wizard ;;
	--tarball)
		TARBALL=${2:?--tarball needs a path}
		shift
		;;
	--version)
		TAG=${2:?--version needs a tag}
		shift
		;;
	--init)
		INIT=${2:?--init needs systemd, openrc or none}
		shift
		;;
	--help | -h)
		usage
		exit 0
		;;
	*) die "unknown option $1 (try --help)" ;;
	esac
	shift
done

# ---------------------------------------------------------------- preflight

if [ "$(uname -s)" != Linux ]; then
	die "Hexagon runs on Linux: the service is a systemd unit or an OpenRC init script, and its containers run as the invoking user against a local Docker socket"
fi

case "$(uname -m)" in
x86_64 | amd64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
*) die "no release is built for $(uname -m); only amd64 and arm64 are. To run it here, $SOURCE_HINT" ;;
esac

DOWNLOADER=
if have curl; then
	DOWNLOADER=curl
elif have wget; then
	DOWNLOADER=wget
fi
if [ -z "$DOWNLOADER" ] && [ -z "$TARBALL" ]; then
	die "neither curl nor wget is installed, so there is no way to fetch a release"
fi
have tar || die "tar is not installed"
have sha256sum || die "sha256sum is not installed: a download that cannot be verified is not installed"

echo "Checking this machine:"
if have docker; then
	if docker version >/dev/null 2>&1; then
		echo "  docker: reachable"
	else
		echo "  docker: installed, but the daemon does not answer — sessions will not start until it does"
	fi
else
	echo "  docker: not installed — Hexagon runs, but no session can start without it"
fi
if have git; then
	echo "  git: present"
else
	echo "  git: missing — repositories are cloned on the host, so install it"
fi
if id -nG 2>/dev/null | tr ' ' '\n' | grep -qx docker; then
	echo "  docker group: you are in it"
else
	echo "  docker group: you are not in it (only matters for a per-user install)"
fi

# Which init system will be asked to run the service. The order matters: a
# machine with systemd installed but booted under OpenRC has no
# /run/systemd/system, and belongs in the second branch — the runtime directory
# answers "what is running", which is the question, while `have systemctl` only
# answers "what is installed". This is the same guard the package's postinst
# uses.
if [ -z "$INIT" ]; then
	if [ -d /run/systemd/system ]; then
		INIT=systemd
	elif have rc-update && have rc-service; then
		INIT=openrc
	else
		INIT=none
	fi
fi
case "$INIT" in
systemd) echo "  init: systemd" ;;
openrc) echo "  init: OpenRC" ;;
none) echo "  init: neither systemd nor OpenRC — the files go in, the service does not" ;;
*) die "unknown init system $INIT: expected systemd, openrc or none" ;;
esac

# User services arrived in OpenRC 0.55 and stopped being experimental in 0.62.
# Where they are missing there is no per-user supervisor at all, so a user
# install installs the files and says how to run them.
OPENRC_USER=no
if [ "$INIT" = openrc ] && rc-update --help 2>&1 | grep -q -- '--user'; then
	OPENRC_USER=yes
fi
echo ""

# ---------------------------------------------------------------- the release

download() { # download URL DEST
	case "$DOWNLOADER" in
	curl) curl -fsSL "$1" -o "$2" ;;
	wget) wget -q -O "$2" "$1" ;;
	esac
}

# The redirect from /releases/latest lands on the newest release's page, and the
# tag is the last component of where it lands. One request, no token, and
# nothing to parse but a path — the REST endpoint would need a JSON parser this
# script does not have, and is rate limited per address.
resolve_tag() {
	case "$DOWNLOADER" in
	curl)
		_url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$BASE/releases/latest" 2>/dev/null || true)
		;;
	wget)
		_url=$(wget -q -S --max-redirect=0 -O /dev/null "$BASE/releases/latest" 2>&1 |
			sed -n 's/^[[:space:]]*[Ll]ocation:[[:space:]]*//p' | tail -n 1)
		;;
	esac
	case "$_url" in
	*/releases/tag/*) printf '%s' "${_url##*/}" ;;
	*) return 1 ;;
	esac
}

verify() { # verify FILE SUMS
	_file=$1
	_sums=$2
	_base=$(basename "$_file")
	_want=$(sed -n "s/^\\([0-9a-f]*\\)  *$_base\$/\\1/p" "$_sums" | head -n 1)
	if [ -z "$_want" ]; then
		die "$_base is not listed in $_sums"
	fi
	_got=$(sha256sum "$_file" | cut -d' ' -f1)
	if [ "$_want" != "$_got" ]; then
		printf 'install.sh: checksum mismatch for %s\n  want %s\n  got  %s\nnothing was installed.\n' \
			"$_base" "$_want" "$_got" >&2
		exit 1
	fi
	echo "  verified $_base"
}

WORK=$(mktemp -d)

if [ -n "$TARBALL" ]; then
	[ -f "$TARBALL" ] || die "no such file: $TARBALL"
	ARCHIVE=$TARBALL
	SUMS=$(dirname "$TARBALL")/SHA256SUMS
	if [ -f "$SUMS" ]; then
		verify "$ARCHIVE" "$SUMS"
	else
		# Said out loud rather than passed over: a local path is not held to a
		# lower standard than a download, it simply has nothing to check against.
		echo "  no SHA256SUMS beside $TARBALL: installing an unverified local artifact" >&2
	fi
else
	if [ -z "$TAG" ]; then
		echo "Looking for the latest release of $REPO..."
		TAG=$(resolve_tag || true)
		if [ -z "$TAG" ]; then
			die "$REPO has no releases yet, so there is nothing to download. In the meantime, $SOURCE_HINT"
		fi
	fi
	VERSION=${TAG#v}
	NAME="hexagon_${VERSION}_linux_${ARCH}.tar.gz"
	echo "Downloading $NAME ($TAG)..."
	download "$BASE/releases/download/$TAG/$NAME" "$WORK/$NAME" ||
		die "cannot download $NAME from release $TAG: no such asset, or no network"
	download "$BASE/releases/download/$TAG/SHA256SUMS" "$WORK/SHA256SUMS" ||
		die "release $TAG has no SHA256SUMS: refusing to install what cannot be verified"
	ARCHIVE=$WORK/$NAME
	verify "$ARCHIVE" "$WORK/SHA256SUMS"
fi

# Unpacked here and copied into place from there, so a truncated archive cannot
# half-replace an installation that was working a moment ago.
mkdir -p "$WORK/unpacked"
tar -xzf "$ARCHIVE" -C "$WORK/unpacked"
STAGE=$(find "$WORK/unpacked" -mindepth 1 -maxdepth 1 -type d | head -n 1)
[ -n "$STAGE" ] && [ -x "$STAGE/usr/bin/hexagon" ] ||
	die "$ARCHIVE does not look like a Hexagon release: no usr/bin/hexagon inside it"
echo "  $("$STAGE/usr/bin/hexagon" -version 2>/dev/null || echo hexagon)"
echo ""

# ---------------------------------------------------------------- question 1

if [ -z "$CONFIGURE" ]; then
	cat >&2 <<-'TXT'
		Hexagon can be configured now, or left empty so that the first-time wizard
		configures it from the browser. The wizard prints a one-time password to the
		service log and asks for the same values there.

		  1. Use the web wizard  (default)
		  2. Configure now
	TXT
	answer=$(ask "Which one" "1")
	case "$answer" in
	2*) CONFIGURE=now ;;
	*) CONFIGURE=wizard ;;
	esac
	echo "" >&2
fi

# ---------------------------------------------------------------- question 2

if [ -z "$MODE" ]; then
	case "$INIT" in
	systemd) user_service="a systemd --user service running as you" ;;
	openrc)
		if [ "$OPENRC_USER" = yes ]; then
			user_service="an OpenRC user service running as you"
		else
			user_service="no service: this OpenRC is too old for user services"
		fi
		;;
	*) user_service="no service: nothing here to run one" ;;
	esac
	cat >&2 <<-TXT
		Where should it be installed?

		  1. For you       ~/.local/bin, $user_service  (default)
		  2. System-wide   /usr/bin, a system service running as a dedicated hexagon user
	TXT
	answer=$(ask "Which one" "1")
	case "$answer" in
	2*) MODE=system ;;
	*) MODE=user ;;
	esac
	echo "" >&2
fi

# A system install repeats what the package's postinst does, and needs the same
# privileges. Re-exec rather than fail at the first mkdir, and hand the child
# the archive that was already downloaded and verified.
if [ "$MODE" = system ] && [ "$(id -u)" != 0 ]; then
	case "$0" in
	*install.sh) script=$0 ;;
	*) script= ;;
	esac
	if [ -z "$script" ] || [ ! -r "$script" ]; then
		die "a system install needs root, and this script was piped rather than saved. Download it and run: sudo sh install.sh --system"
	fi
	have sudo || die "a system install needs root: re-run this as root"
	echo "Re-running the system install under sudo..."
	set -- --system --init "$INIT" "--$( [ "$CONFIGURE" = now ] && echo configure-now || echo use-wizard )" --tarball "$ARCHIVE"
	if [ "$ASSUME_YES" = 1 ]; then set -- "$@" --yes; fi
	sudo -- sh "$script" "$@"
	exit $?
fi

# ---------------------------------------------------------------- paths

# Where the installation lives is decided by the mode; how the service is
# described is decided by the init system. Everything below reads these, so a
# third init system would be another case here and three cases in the service_*
# functions, not a rewrite.
if [ "$MODE" = system ]; then
	BIN_PATH=/usr/bin/hexagon
	CONFIG_DIR=/etc/hexagon
	DATA_DEFAULT=/var/lib/hexagon
	SERVICE_USER=hexagon
	SERVICE_OWNER=hexagon:hexagon
	UNIT_PATH=/lib/systemd/system/hexagon.service
	RC_PATH=/etc/init.d/hexagon
	RC_CONF=/etc/conf.d/hexagon
	LOG_PATH=/var/log/hexagon.log
	PID_PATH=/run/hexagon.pid
	SERVICE_HOME=$DATA_DEFAULT
	SYSTEMCTL="systemctl"
	RC_UPDATE="rc-update"
	RC_SERVICE="rc-service"
else
	BIN_PATH=$HOME/.local/bin/hexagon
	CONFIG_DIR=$HOME/.config/hexagon
	DATA_DEFAULT=$HOME/.local/share/hexagon
	SERVICE_USER=$(id -un)
	# A user service already runs as whoever starts it, and OpenRC's guide says
	# command_user must stay unset there — so no owner is written either.
	SERVICE_OWNER=
	UNIT_PATH=$HOME/.config/systemd/user/hexagon.service
	RC_PATH=${XDG_CONFIG_HOME:-$HOME/.config}/rc/init.d/hexagon
	RC_CONF=${XDG_CONFIG_HOME:-$HOME/.config}/rc/conf.d/hexagon
	LOG_PATH=${XDG_STATE_HOME:-$HOME/.local/state}/hexagon/hexagon.log
	# Runtime data belongs in XDG_RUNTIME_DIR for a user service; /tmp is the
	# fallback for a session that has none, where it is at least writable.
	PID_PATH=${XDG_RUNTIME_DIR:-/tmp}/hexagon.pid
	SERVICE_HOME=$HOME
	SYSTEMCTL="systemctl --user"
	RC_UPDATE="rc-update --user"
	RC_SERVICE="rc-service --user"
fi
CONFIG_FILE=$CONFIG_DIR/config.json

# ---------------------------------------------------------------- the service

# Four functions with one case each, so nothing below this point names an init
# system: adding a third one is a case in each, not a rewrite.

service_install() {
	case "$INIT" in
	systemd)
		if [ "$MODE" = system ]; then
			install -D -m 0644 "$STAGE/lib/systemd/system/hexagon.service" "$UNIT_PATH"
		else
			# A user unit genuinely differs: no User=, no supplementary group,
			# and paths under $HOME.
			mkdir -p "$(dirname "$UNIT_PATH")"
			cat >"$UNIT_PATH" <<-UNIT
				[Unit]
				Description=Hexagon: Claude Code sessions in Docker containers
				Documentation=https://github.com/$REPO
				After=network-online.target
				Wants=network-online.target

				[Service]
				Environment=HEXAGON_CONFIG=$CONFIG_FILE
				ExecStart=$BIN_PATH
				Restart=on-failure
				RestartSec=5

				[Install]
				WantedBy=default.target
			UNIT
		fi
		echo "  $UNIT_PATH"
		;;
	openrc)
		# Copied from the release rather than written here, and the same file in
		# both modes: everything that differs is in the conf.d beside it, which
		# OpenRC sources on its own.
		_src=$STAGE/usr/share/doc/hexagon/examples/hexagon.openrc
		[ -f "$_src" ] ||
			die "this release carries no OpenRC init script, so it predates OpenRC support: install a newer release, or --init none to place the files only"
		install -D -m 0755 "$_src" "$RC_PATH"
		mkdir -p "$(dirname "$RC_CONF")"
		# A user's runlevels are created when their OpenRC session first runs,
		# and rc-update refuses a runlevel that is not there yet. Creating the
		# three a session would have makes enabling work on an account that has
		# never started one.
		if [ "$MODE" = user ]; then
			mkdir -p "${XDG_CONFIG_HOME:-$HOME/.config}"/rc/runlevels/boot \
				"${XDG_CONFIG_HOME:-$HOME/.config}"/rc/runlevels/default \
				"${XDG_CONFIG_HOME:-$HOME/.config}"/rc/runlevels/shutdown
			# checkpath in the init script creates one directory, not a path:
			# ~/.local/state may not exist at all on a fresh account.
			mkdir -p "$(dirname "$LOG_PATH")"
		fi
		{
			echo "# Written by install.sh: the paths $RC_PATH reads."
			echo "HEXAGON_BIN=\"$BIN_PATH\""
			echo "HEXAGON_HOME=\"$SERVICE_HOME\""
			echo "HEXAGON_CONFIG=\"$CONFIG_FILE\""
			echo "HEXAGON_LOG=\"$LOG_PATH\""
			echo "HEXAGON_PIDFILE=\"$PID_PATH\""
			# Left out of a user service on purpose: it already runs as whoever
			# starts it, and OpenRC refuses to be told otherwise.
			if [ -n "$SERVICE_OWNER" ]; then
				echo "HEXAGON_USER=\"$SERVICE_OWNER\""
				echo "HEXAGON_OWNER=\"$SERVICE_OWNER\""
			fi
		} >"$RC_CONF"
		chmod 0644 "$RC_CONF"
		echo "  $RC_PATH"
		echo "  $RC_CONF"
		;;
	none)
		echo "  no service registered: no init system to register it with"
		;;
	esac
}

# Enables the service and starts it. Non-zero when that did not work, so the
# caller can fall back to telling the user how to run the server by hand.
service_enable_start() {
	case "$INIT" in
	systemd)
		$SYSTEMCTL daemon-reload 2>/dev/null &&
			$SYSTEMCTL enable --now hexagon.service 2>/dev/null &&
			echo "Service started: $SYSTEMCTL status hexagon"
		;;
	openrc)
		$RC_UPDATE add hexagon default >/dev/null 2>&1 &&
			$RC_SERVICE hexagon start &&
			echo "Service started: $RC_SERVICE hexagon status"
		;;
	*) return 1 ;;
	esac
}

service_enable_hint() {
	case "$INIT" in
	systemd) echo "  $SYSTEMCTL enable --now hexagon" ;;
	openrc) echo "  $RC_UPDATE add hexagon default && $RC_SERVICE hexagon start" ;;
	*) echo "  HEXAGON_CONFIG=$CONFIG_FILE $BIN_PATH" ;;
	esac
}

# Where the first-time wizard's one-time password can be read. OpenRC has no
# journal, so on OpenRC this is a file the init script points the server at.
service_log_hint() {
	case "$INIT" in
	systemd)
		if [ "$MODE" = system ]; then
			echo "  journalctl -u hexagon | grep 'first-time setup'"
		else
			echo "  journalctl --user -u hexagon | grep 'first-time setup'"
		fi
		;;
	openrc) echo "  grep 'first-time setup' $LOG_PATH" ;;
	*) echo "  on the terminal you start the server from" ;;
	esac
}

service_restart_command() {
	case "$INIT" in
	openrc) echo "rc-service hexagon restart" ;;
	*) echo "systemctl restart hexagon" ;;
	esac
}

# A user install on an OpenRC too old for user services has nowhere to put one.
if [ "$INIT" = openrc ] && [ "$MODE" = user ] && [ "$OPENRC_USER" != yes ]; then
	echo "This OpenRC has no user services (they need 0.55 or newer), so nothing will" >&2
	echo "supervise Hexagon for you. Installing the files only; --system installs a service." >&2
	echo "" >&2
	INIT=none
fi

# ---------------------------------------------------------------- question 3+

addr=127.0.0.1:8080
public_url=http://127.0.0.1:8080
client_id=
client_secret=
allowed=
git_name=
git_email=

if [ "$CONFIGURE" = now ]; then
	addr=$(ask "Listen address" "$addr")
	public_url=$(ask "Public URL" "$public_url")
	echo "  Callback URL to register on GitHub: $public_url/api/auth/callback" >&2
	client_id=$(ask "GitHub OAuth client id (empty leaves the wizard open)" "")
	client_secret=$(secret "GitHub OAuth client secret")
	if [ "$MODE" = system ]; then
		allowed=$(ask "GitHub logins allowed to sign in (comma separated)" "")
	else
		allowed=$(ask "GitHub logins allowed to sign in (comma separated)" "$(id -un)")
	fi
	git_name=$(ask "Git user name for sessions" "$(git config --global user.name 2>/dev/null || true)")
	git_email=$(ask "Git email for sessions" "$(git config --global user.email 2>/dev/null || true)")
fi

data_dir=$(ask "Data directory (database, workspaces, downloads)" "$DATA_DEFAULT")

# The same refusal the server makes at startup, made here instead: writing a
# file that stops the service from ever coming up, and finding out afterwards,
# is the failure this avoids.
case "$addr" in
127.* | localhost:* | "[::1]"* | "::1"*) ;;
*)
	case "$public_url" in
	https://*) ;;
	*)
		die "addr $addr accepts traffic from the network but publicUrl $public_url is not https: put a TLS reverse proxy in front and bind loopback, or set insecureHttp in $CONFIG_FILE deliberately"
		;;
	esac
	;;
esac

start_now=no
if [ "$INIT" != none ]; then
	if confirm "Enable and start the service now?" "Y"; then start_now=yes; fi
fi
# Lingering is systemd's; OpenRC keeps a user's services in that user's own
# session, and what makes them survive a logout is a root command printed at the
# end rather than anything this script can do.
linger=no
if [ "$INIT" = systemd ] && [ "$MODE" = user ] && [ "$start_now" = yes ]; then
	if confirm "Keep it running when you log out (loginctl enable-linger)?" "Y"; then linger=yes; fi
fi
echo ""

# ---------------------------------------------------------------- install

echo "Installing:"
if [ "$MODE" = system ]; then
	if ! getent passwd hexagon >/dev/null 2>&1; then
		# Three ways to say the same thing. Debian's adduser is checked first
		# because it is a different program from busybox's under the same name,
		# and the flags are not interchangeable.
		if have adduser && [ -f /etc/debian_version ]; then
			adduser --system --group --home "$DATA_DEFAULT" --shell /usr/sbin/nologin \
				--quiet --disabled-login hexagon
		elif have useradd; then
			useradd --system --home-dir "$DATA_DEFAULT" --shell /sbin/nologin \
				--user-group hexagon
		elif have adduser && have addgroup; then
			addgroup -S hexagon 2>/dev/null || true
			adduser -S -D -H -h "$DATA_DEFAULT" -s /sbin/nologin -G hexagon hexagon
		else
			die "cannot create the hexagon account: no adduser and no useradd here"
		fi
		echo "  created the hexagon user"
	fi
	if getent group docker >/dev/null 2>&1; then
		if have usermod; then
			usermod -aG docker hexagon
		elif have gpasswd; then
			gpasswd -a hexagon docker >/dev/null
		else
			addgroup hexagon docker
		fi
		echo "  added hexagon to the docker group"
	else
		echo "  no docker group yet. Once Docker is installed, run:"
		echo "    sudo usermod -aG docker hexagon && sudo $(service_restart_command)"
	fi
fi

install -D -m 0755 "$STAGE/usr/bin/hexagon" "$BIN_PATH"
echo "  $BIN_PATH"

if [ "$MODE" = system ]; then
	(cd "$STAGE" && find usr/share/doc -type f -exec install -D -m 0644 "{}" "/{}" \;)
fi
service_install

mkdir -p "$CONFIG_DIR" "$data_dir"
chmod 0700 "$data_dir"
if [ "$MODE" = system ]; then
	chown root:hexagon "$CONFIG_DIR"
	chmod 0750 "$CONFIG_DIR"
	chown hexagon:hexagon "$data_dir"
else
	chmod 0700 "$CONFIG_DIR"
fi

write_config=yes
if [ -e "$CONFIG_FILE" ]; then
	write_config=no
	if confirm "$CONFIG_FILE exists. Overwrite it?" "N"; then write_config=yes; fi
fi

if [ "$write_config" = yes ]; then
	# umask, not a chmod afterwards: the file can hold the OAuth client secret,
	# and it should never exist with wider permissions even briefly.
	(
		umask 077
		{
			printf '{\n'
			printf '  "addr": "%s",\n' "$(json_str "$addr")"
			printf '  "publicUrl": "%s",\n' "$(json_str "$public_url")"
			printf '  "dataDir": "%s"' "$(json_str "$data_dir")"
			if [ -n "$client_id$client_secret$allowed" ]; then
				printf ',\n  "github": {\n'
				# Only keys that got a value are emitted: an unknown key is a
				# hard error for the server, and an empty value is not the same
				# thing as an absent one.
				{
					if [ -n "$client_id" ]; then printf '    "clientId": "%s"\n' "$(json_str "$client_id")"; fi
					if [ -n "$client_secret" ]; then printf '    "clientSecret": "%s"\n' "$(json_str "$client_secret")"; fi
					if [ -n "$allowed" ]; then printf '    "allowedUsers": [%s]\n' "$(json_list "$allowed")"; fi
				} | sed '$ ! s/$/,/'
				printf '  }'
			fi
			if [ -n "$git_name$git_email" ]; then
				printf ',\n  "git": {\n'
				{
					if [ -n "$git_name" ]; then printf '    "userName": "%s"\n' "$(json_str "$git_name")"; fi
					if [ -n "$git_email" ]; then printf '    "userEmail": "%s"\n' "$(json_str "$git_email")"; fi
				} | sed '$ ! s/$/,/'
				printf '  }'
			fi
			printf '\n}\n'
		} >"$CONFIG_FILE"
	)
	if [ "$MODE" = system ]; then chown hexagon:hexagon "$CONFIG_FILE"; fi
	echo "  $CONFIG_FILE"
else
	echo "  $CONFIG_FILE kept as it is"
fi
echo ""

# ---------------------------------------------------------------- start it

if [ "$start_now" = yes ]; then
	if ! service_enable_start; then
		echo "Could not start the service. Run the server by hand with:" >&2
		echo "  HEXAGON_CONFIG=$CONFIG_FILE $BIN_PATH" >&2
	fi
	if [ "$linger" = yes ]; then
		loginctl enable-linger "$(id -un)" 2>/dev/null ||
			echo "  could not enable lingering: the service stops when you log out" >&2
	fi
elif [ "$INIT" = none ]; then
	echo "No service was registered. Run the server with:"
	service_enable_hint
else
	echo "Not started. When you want it:"
	service_enable_hint
fi

echo ""
if [ -n "$client_id" ]; then
	echo "Open $public_url and sign in."
else
	echo "Open $public_url/setup and complete the first-time wizard."
	echo "Its one-time password is printed once, when the server starts:"
	service_log_hint
fi
echo "Callback URL to register on GitHub: $public_url/api/auth/callback"
# OpenRC runs a user's services from that user's own session, which is started
# by a service only root can add. This is the counterpart of enable-linger, and
# it is printed rather than run because it is a change to the system.
if [ "$INIT" = openrc ] && [ "$MODE" = user ]; then
	echo ""
	echo "To start it at boot and keep it running when you log out, once, as root:"
	echo "  sudo ln -s user /etc/init.d/user.$(id -un)"
	echo "  sudo rc-update add user.$(id -un) default"
fi
if [ "$MODE" = user ] && ! id -nG 2>/dev/null | tr ' ' '\n' | grep -qx docker; then
	echo ""
	echo "You are not in the docker group, so no session will start. Fix it with:"
	echo "  sudo usermod -aG docker $SERVICE_USER   # then log out and back in"
fi
