#!/bin/sh

set -eu

cd "$(dirname "$0")/.."

nix_bin=${NIX:-nix}
command -v "$nix_bin" >/dev/null 2>&1 || {
	echo "nix-check: Nix executable not found: $nix_bin" >&2
	exit 1
}

before=$(cksum flake.nix flake.lock govendor.toml)
assert_unchanged() {
	after=$(cksum flake.nix flake.lock govendor.toml)
	if [ "$before" != "$after" ]; then
		echo "nix-check: validation modified committed Nix state" >&2
		exit 1
	fi
}
trap assert_unchanged EXIT

run_nix() {
	"$nix_bin" --extra-experimental-features 'nix-command flakes' "$@"
}

# Evaluate every advertised platform, then build and exercise checks that are
# compatible with the current runner.
NIX="$nix_bin" ./scripts/nix-metadata-check.sh
run_nix flake check --all-systems --no-build --no-update-lock-file
run_nix flake check --no-update-lock-file --print-build-logs
run_nix build --no-link --no-update-lock-file .#tturl
run_nix run --no-write-lock-file .#tturl -- --version
