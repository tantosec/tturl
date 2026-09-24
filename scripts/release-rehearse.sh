#!/bin/sh

set -eu

cd "$(dirname "$0")/.."

version=${1:-}
if [ -z "$version" ]; then
	echo 'usage: scripts/release-rehearse.sh VERSION' >&2
	exit 2
fi

goreleaser_bin="$PWD/scripts/goreleaser.sh"
git tag --list 'v*' | go run ./tools/releasemaint check "$version"

temporary_directory=$(mktemp -d "${TMPDIR:-/tmp}/tturl-release-rehearse.XXXXXX")
cleanup() {
	find "$temporary_directory" -depth -delete
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

fixture="$temporary_directory/repository"
git clone --quiet --no-hardlinks . "$fixture"
cp release/version.txt "$fixture/release/version.txt"
cp CITATION.cff "$fixture/CITATION.cff"
git -C "$fixture" config user.name 'Release rehearsal'
git -C "$fixture" config user.email 'release-rehearsal.invalid'
if remote=$(git remote get-url origin 2>/dev/null); then
	git -C "$fixture" remote set-url origin "$remote"
fi
git -C "$fixture" add release/version.txt CITATION.cff
git -C "$fixture" commit --quiet --allow-empty -m "Rehearse v$version"
revision=$(git -C "$fixture" rev-parse HEAD)
git -C "$fixture" tag -d "v$version" >/dev/null 2>&1 || true
git -C "$fixture" tag "v$version"
git -C "$fixture" checkout --quiet --detach "v$version"

(
	cd "$fixture"
	go test ./...
	"$goreleaser_bin" check
	"$goreleaser_bin" release --clean --skip=publish
)

go run ./tools/releasemaint verify-dist "$version" "$revision" "$fixture/dist"
echo "release-rehearse: v$version release artefacts and identities passed"
