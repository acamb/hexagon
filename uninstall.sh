#!/bin/sh
# Removes an installation made by install.sh. It detects which layout is there
# rather than asking again, and the two questions it does ask both default to
# keeping what they name.
#
#   sh uninstall.sh            remove the user installation
#   sh uninstall.sh --system   remove the system installation (needs root)
set -eu

MODE=
ASSUME_YES=0

die() {
	printf 'uninstall.sh: %s\n' "$*" >&2
	exit 1
}

have() {
	command -v "$1" >/dev/null 2>&1
}

TTY=
if [ -t 0 ] && [ -r /dev/tty ]; then
	TTY=/dev/tty
elif [ -r /dev/tty ]; then
	TTY=/dev/tty
fi

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

while [ $# -gt 0 ]; do
	case "$1" in
	--system) MODE=system ;;
	--user) MODE=user ;;
	--yes | -y) ASSUME_YES=1 ;;
	--help | -h)
		sed -n '2,7p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*) die "unknown option $1 (try --help)" ;;
	esac
	shift
done

if [ -z "$MODE" ]; then
	if [ -e "$HOME/.local/bin/hexagon" ] || [ -e "$HOME/.config/systemd/user/hexagon.service" ]; then
		MODE=user
	elif [ -e /usr/bin/hexagon ] || [ -e /lib/systemd/system/hexagon.service ]; then
		MODE=system
	else
		die "no installation found in $HOME/.local or in /usr"
	fi
fi

# A packaged install is dpkg's to undo: removing its files behind its back
# leaves the package half-installed and the next upgrade confused.
if [ "$MODE" = system ] && have dpkg-query && dpkg-query -S /usr/bin/hexagon >/dev/null 2>&1; then
	die "/usr/bin/hexagon belongs to the hexagon package. Remove it with: sudo apt remove hexagon (or apt purge)"
fi

if [ "$MODE" = system ]; then
	[ "$(id -u)" = 0 ] || die "a system uninstall needs root: sudo sh uninstall.sh --system"
	BIN_PATH=/usr/bin/hexagon
	CONFIG_DIR=/etc/hexagon
	UNIT_PATH=/lib/systemd/system/hexagon.service
	DOC_DIR=/usr/share/doc/hexagon
	DATA_DEFAULT=/var/lib/hexagon
	SYSTEMCTL="systemctl"
else
	BIN_PATH=$HOME/.local/bin/hexagon
	CONFIG_DIR=$HOME/.config/hexagon
	UNIT_PATH=$HOME/.config/systemd/user/hexagon.service
	DOC_DIR=
	DATA_DEFAULT=$HOME/.local/share/hexagon
	SYSTEMCTL="systemctl --user"
fi

# The configuration file knows where the data actually is, which may not be the
# default for this layout.
data_dir=$DATA_DEFAULT
if [ -r "$CONFIG_DIR/config.json" ]; then
	found=$(sed -n 's/.*"dataDir"[[:space:]]*:[[:space:]]*"\(.*\)".*/\1/p' "$CONFIG_DIR/config.json" | head -n 1)
	if [ -n "$found" ]; then data_dir=$found; fi
fi

$SYSTEMCTL stop hexagon.service 2>/dev/null || true
$SYSTEMCTL disable hexagon.service 2>/dev/null || true
rm -f "$UNIT_PATH"
$SYSTEMCTL daemon-reload 2>/dev/null || true
rm -f "$BIN_PATH"
if [ -n "$DOC_DIR" ]; then rm -rf "$DOC_DIR"; fi
echo "Removed the service and $BIN_PATH."

if [ -e "$CONFIG_DIR/config.json" ]; then
	if confirm "Remove $CONFIG_DIR/config.json (it holds the OAuth settings)?" "N"; then
		rm -rf "$CONFIG_DIR"
		echo "Removed $CONFIG_DIR."
	else
		echo "Kept $CONFIG_DIR."
	fi
fi

if [ -d "$data_dir" ]; then
	echo "$data_dir holds the database and the session workspaces — git clones of"
	echo "your repositories, which may carry commits that were never pushed."
	if confirm "Remove $data_dir?" "N"; then
		rm -rf "$data_dir"
		echo "Removed $data_dir."
	else
		echo "Kept $data_dir."
	fi
fi

if [ "$MODE" = system ] && getent passwd hexagon >/dev/null 2>&1; then
	if confirm "Remove the hexagon user?" "N"; then
		if have deluser; then deluser --quiet --system hexagon || true; else userdel hexagon || true; fi
		echo "Removed the hexagon user."
	fi
fi
