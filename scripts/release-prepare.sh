#!/bin/sh

set -eu

cd "$(dirname "$0")/.."

version=${1:-}
if [ -z "$version" ]; then
	echo 'usage: make release-prepare VERSION=X.Y.Z' >&2
	exit 2
fi

if [ -n "$(git status --porcelain)" ]; then
	echo 'release-prepare: working tree must be clean' >&2
	exit 1
fi

temporary_directory=$(mktemp -d "${TMPDIR:-/tmp}/tturl-release.XXXXXX")
keep_update=false
cleanup() {
	if [ "$keep_update" = false ]; then
		restored=true
		cp "$temporary_directory/version.txt" release/version.txt || restored=false
		cp "$temporary_directory/CITATION.cff" CITATION.cff || restored=false
		if [ "$restored" = true ]; then
			echo 'release-prepare: restored release metadata after preparation failed' >&2
		else
			echo 'release-prepare: could not restore release intent' >&2
		fi
	fi
	rm -f "$temporary_directory/version.txt" "$temporary_directory/CITATION.cff" \
		"$temporary_directory/tags.txt"
	rmdir "$temporary_directory"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

cp release/version.txt "$temporary_directory"
cp CITATION.cff "$temporary_directory"
git tag --list 'v*' >"$temporary_directory/tags.txt"

go run ./tools/releasemaint prepare "$version" <"$temporary_directory/tags.txt"
go run ./tools/releasemaint set "$version"
make nix-deps-check
go run ./tools/releasemaint check "$version" <"$temporary_directory/tags.txt"
./scripts/release-rehearse.sh "$version"

keep_update=true
echo "release-prepare: prepared version $version; review and commit both metadata files"
