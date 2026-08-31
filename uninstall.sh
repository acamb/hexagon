#!/bin/sh
# Removes an installation made by install.sh. It detects which layout is there
# rather than asking again, and the two questions it does ask both default to
# keeping what they name.
#
#   sh uninstall.sh            remove the user installation
#   sh uninstall.sh --system   remove the system installation (needs root)
#   sh uninstall.sh --init openrc   force an init system instead of detecting one
set -eu

MODE=
INIT=
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
	--init)
		INIT=${2:?--init needs systemd, openrc or none}
		shift
		;;
	--help | -h)
		sed -n '2,8p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*) die "unknown option $1 (try --help)" ;;
	esac
	shift
done

USER_RC=${XDG_CONFIG_HOME:-$HOME/.config}/rc/init.d/hexagon

if [ -z "$MODE" ]; then
	if [ -e "$HOME/.local/bin/hexagon" ] || [ -e "$HOME/.config/systemd/user/hexagon.service" ] ||
		[ -e "$USER_RC" ]; then
		MODE=user
	elif [ -e /usr/bin/hexagon ] || [ -e /lib/systemd/system/hexagon.service ] ||
		[ -e /etc/init.d/hexagon ]; then
		MODE=system
	else
		die "no installation found in $HOME/.local or in /usr"
	fi
fi

# What removes the service. Detected the same way install.sh detects it, and
# then corrected by what is actually on disk: a machine can have been rebooted
# into another init system since the install.
if [ -z "$INIT" ]; then
	if [ -d /run/systemd/system ]; then
		INIT=systemd
	elif have rc-update && have rc-service; then
		INIT=openrc
	else
		INIT=none
	fi
fi
if [ "$MODE" = system ] && [ -e /etc/init.d/hexagon ] && have rc-service; then
	INIT=openrc
elif [ "$MODE" = user ] && [ -e "$USER_RC" ] && have rc-service; then
	INIT=openrc
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
	RC_PATH=/etc/init.d/hexagon
	RC_CONF=/etc/conf.d/hexagon
	LOG_PATH=/var/log/hexagon.log
	DOC_DIR=/usr/share/doc/hexagon
	DATA_DEFAULT=/var/lib/hexagon
	SYSTEMCTL="systemctl"
	RC_UPDATE="rc-update"
	RC_SERVICE="rc-service"
else
	BIN_PATH=$HOME/.local/bin/hexagon
	CONFIG_DIR=$HOME/.config/hexagon
	UNIT_PATH=$HOME/.config/systemd/user/hexagon.service
	RC_PATH=$USER_RC
	RC_CONF=${XDG_CONFIG_HOME:-$HOME/.config}/rc/conf.d/hexagon
	LOG_PATH=${XDG_STATE_HOME:-$HOME/.local/state}/hexagon/hexagon.log
	DOC_DIR=
	DATA_DEFAULT=$HOME/.local/share/hexagon
	SYSTEMCTL="systemctl --user"
	RC_UPDATE="rc-update --user"
	RC_SERVICE="rc-service --user"
fi

# The configuration file knows where the data actually is, which may not be the
# default for this layout.
data_dir=$DATA_DEFAULT
if [ -r "$CONFIG_DIR/config.json" ]; then
	found=$(sed -n 's/.*"dataDir"[[:space:]]*:[[:space:]]*"\(.*\)".*/\1/p' "$CONFIG_DIR/config.json" | head -n 1)
	if [ -n "$found" ]; then data_dir=$found; fi
fi

case "$INIT" in
systemd)
	$SYSTEMCTL stop hexagon.service 2>/dev/null || true
	$SYSTEMCTL disable hexagon.service 2>/dev/null || true
	$SYSTEMCTL daemon-reload 2>/dev/null || true
	;;
openrc)
	$RC_SERVICE hexagon stop 2>/dev/null || true
	$RC_UPDATE del hexagon default 2>/dev/null || true
	rm -f "$RC_PATH" "$RC_CONF"
	# The log is not user data, and it holds every one-time password the wizard
	# ever printed, so it goes with the service rather than being left behind.
	rm -f "$LOG_PATH"
	;;
esac
# Outside the case on purpose: a unit left behind by an install made under
# another init system should go too.
rm -f "$UNIT_PATH"
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
