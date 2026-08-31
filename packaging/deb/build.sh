#!/bin/sh
# Builds the release artifacts for one architecture, inside the Debian container
# `make artifacts` starts. Nothing here runs on the host: dpkg-deb is not
# necessarily installed there, and the host's Go toolchain links against the
# host's libc.
#
#   build.sh <version> <debian-arch> [all|deb]
#
# The binary is static (CGO_ENABLED=0), so it depends on the container's glibc
# no more than on the host's, and another architecture is a cross-build away.
set -eu

version=${1:?version}
arch=${2:?debian architecture}
artifacts=${3:-all}

case "$arch" in
amd64) goarch=amd64 ;;
arm64) goarch=arm64 ;;
*)
	echo "build.sh: unknown architecture $arch, expected amd64 or arm64" >&2
	exit 1
	;;
esac

root=$(pwd)
out=$root/dist
mkdir -p "$out"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

bin=$work/hexagon
CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" \
	go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$bin" ./cmd/hexagon

# One `make install` for both artifacts: the tarball is this staging directory
# rolled up, and the package is the same directory with DEBIAN/ added to it, so
# the two cannot disagree about where anything goes.
name=hexagon_${version}_linux_${arch}
stage=$work/$name
make --no-print-directory install BIN="$bin" DESTDIR="$stage" PREFIX=/usr
gzip -9n "$stage/usr/share/doc/hexagon/README.md" "$stage/usr/share/doc/hexagon/changelog.Debian"

# Before DEBIAN/ exists, so the control files are not counted as installed size.
size=$(du -ks "$stage" | cut -f1)

if [ "$artifacts" = all ]; then
	# --owner/--group so unpacking as root reproduces the package's ownership
	# rather than that of whoever built the release.
	tar -czf "$out/$name.tar.gz" -C "$work" --owner=0 --group=0 "$name"
fi

mkdir -p "$stage/DEBIAN"
sed -e "s/@VERSION@/$version/" -e "s/@ARCH@/$arch/" -e "s/@SIZE@/$size/" \
	packaging/deb/control.in >"$stage/DEBIAN/control"
for script in config postinst prerm postrm; do
	install -m 0755 "packaging/deb/$script" "$stage/DEBIAN/$script"
done
install -m 0644 packaging/deb/templates "$stage/DEBIAN/templates"
(cd "$stage" && find usr lib -type f -print0 | sort -z | xargs -0 md5sum >DEBIAN/md5sums)

# --root-owner-group is what lets the container run as the invoking user and
# still produce root-owned paths in the archive, so a release build never leaves
# root-owned files in the working tree.
dpkg-deb --build --root-owner-group "$stage" "$out/hexagon_${version}_${arch}.deb" >/dev/null
echo "built $out/hexagon_${version}_${arch}.deb"
