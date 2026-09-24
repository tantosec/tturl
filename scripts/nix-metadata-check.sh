#!/bin/sh

set -eu

cd "$(dirname "$0")/.."

nix_bin=${NIX:-nix}
command -v "$nix_bin" >/dev/null 2>&1 || {
	echo "nix-metadata-check: Nix executable not found: $nix_bin" >&2
	exit 1
}

temporary_directory=$(mktemp -d "${TMPDIR:-/tmp}/tturl-nix-metadata.XXXXXX")
cleanup() {
	find "$temporary_directory" -depth -delete
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

run_nix() {
	"$nix_bin" --extra-experimental-features 'nix-command flakes' "$@"
}

system=$(run_nix eval --impure --raw --expr builtins.currentSystem)
attribute="packages.$system.tturl"
fixture="$temporary_directory/repository"

# The disposable repository isolates source-reference behaviour from the
# contributor's current dirty state. Overlay the files that define this check
# so the fixture always exercises the implementation being reviewed.
git clone --quiet --no-hardlinks . "$fixture"
cp flake.nix flake.lock govendor.toml "$fixture"
cp release/version.txt "$fixture/release/version.txt"
cp scripts/nix-metadata-check.sh "$fixture/scripts/nix-metadata-check.sh"
git -C "$fixture" config user.name 'Nix metadata check'
git -C "$fixture" config user.email 'nix-metadata-check.invalid'
git -C "$fixture" checkout --quiet -b nix-metadata-check
git -C "$fixture" add flake.nix flake.lock govendor.toml release/version.txt \
	scripts/nix-metadata-check.sh
git -C "$fixture" commit --quiet --allow-empty -m 'Nix metadata fixture'
revision=$(git -C "$fixture" rev-parse HEAD)
git -C "$fixture" tag nix-metadata-check

fixture_url="git+file://$fixture"
branch_ref="$fixture_url?ref=nix-metadata-check"
revision_ref="$fixture_url?rev=$revision"
tag_ref="$fixture_url?ref=refs/tags/nix-metadata-check"

identity() {
	run_nix eval --no-write-lock-file --raw "$1#$attribute.$2"
}

expected_diagnostic=$(identity "$branch_ref" diagnosticVersion)
expected_package_version=$(identity "$branch_ref" packageVersion)
expected_snapshot_date=$(identity "$branch_ref" snapshotDate)
for reference in "$fixture" "$branch_ref" "$revision_ref" "$tag_ref"; do
	actual_revision=$(identity "$reference" sourceRevision)
	if [ "$actual_revision" != "$revision" ]; then
		echo "nix-metadata-check: $reference resolved $actual_revision, want $revision" >&2
		exit 1
	fi
	if [ "$(identity "$reference" sourceTreeState)" != clean ]; then
		echo "nix-metadata-check: clean reference was not classified clean: $reference" >&2
		exit 1
	fi
	if [ "$(identity "$reference" diagnosticVersion)" != "$expected_diagnostic" ] ||
		[ "$(identity "$reference" packageVersion)" != "$expected_package_version" ] ||
		[ "$(identity "$reference" snapshotDate)" != "$expected_snapshot_date" ]; then
		echo "nix-metadata-check: reference spelling changed identity: $reference" >&2
		exit 1
	fi
done

clean_derivation=$(identity "$fixture" drvPath)
untracked="$fixture/untracked-metadata-check"
touch "$untracked"
if [ "$(identity "$fixture" drvPath)" != "$clean_derivation" ]; then
	echo 'nix-metadata-check: an untracked file changed the derivation' >&2
	exit 1
fi
if [ "$(identity "$fixture" sourceTreeState)" != clean ]; then
	echo 'nix-metadata-check: an excluded untracked file made the source dirty' >&2
	exit 1
fi
rm -f "$untracked"

printf '\n' >>"$fixture/cmd/tturl/main.go"
if [ "$(identity "$fixture" sourceRevision)" != "$revision" ]; then
	echo 'nix-metadata-check: dirty source lost its base revision' >&2
	exit 1
fi
if [ "$(identity "$fixture" sourceTreeState)" != dirty ]; then
	echo 'nix-metadata-check: a tracked source edit was not classified dirty' >&2
	exit 1
fi

echo 'nix-metadata-check: clean, dirty, untracked, branch, revision, and tag cases passed'
