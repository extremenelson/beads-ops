#!/bin/sh
# Build the bops release archives (bops-ss6.14).
#
#   scripts/release-build.sh <version> [outdir]
#
# <version> is the tag's version part, e.g. v0.2.0 for tag bops/v0.2.0. It is
# stamped into the binary with -ldflags -X main.version, so `bops version`
# prints it. Writes bops_<version>_<os>_<arch>.tar.gz (.zip for windows) and
# checksums.txt (sha256) to outdir, default dist/. The release workflow runs
# this on a bops/v* tag; CI runs it on every change with a snapshot version.
#
# Plain go build, not goreleaser: goreleaser's free edition cannot read a
# monorepo tag like bops/v0.2.0 (tag_prefix is a Pro feature), and the
# targets need nothing beyond CGO_ENABLED=0.
set -eu

version=${1:?usage: release-build.sh <version> [outdir]}
case $version in
v[0-9]*) ;;
*) echo "release-build: version must start with v and a digit: $version" >&2; exit 2 ;;
esac
root=$(cd "$(dirname "$0")/.." && pwd)
out=${2:-$root/dist}
mkdir -p "$out"
out=$(cd "$out" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

targets="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64 android/arm64"

for t in $targets; do
	os=${t%/*} arch=${t#*/}
	name=bops_${version}_${os}_${arch}
	exe=bops
	[ "$os" = windows ] && exe=bops.exe
	mkdir -p "$work/$name"
	(cd "$root/bops" && GOOS=$os GOARCH=$arch CGO_ENABLED=0 \
		go build -trimpath -ldflags "-s -w -X main.version=$version" \
		-o "$work/$name/$exe" ./cmd/bops)
	cp "$root/LICENSE" "$root/README.md" "$root/CHANGELOG.md" "$work/$name/"
	if [ "$os" = windows ]; then
		(cd "$work" && zip -qr "$out/$name.zip" "$name")
	else
		tar -C "$work" -czf "$out/$name.tar.gz" "$name"
	fi
	echo "built $name"
done

cd "$out"
if command -v sha256sum >/dev/null 2>&1; then
	sha256sum bops_"$version"_* >checksums.txt
else
	shasum -a 256 bops_"$version"_* >checksums.txt
fi
echo "wrote $out/checksums.txt"
