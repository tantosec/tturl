#!/bin/sh

set -eu

# Release output shape and linker behaviour must not drift between rehearsal
# and publication. CI supplies the checksum-verified prebuilt executable from
# goreleaser-action; maintainers retain a pinned go-run path with no installed
# tool prerequisite.
version=v2.17.0
if [ -n "${GORELEASER_BIN:-}" ]; then
	actual_version=$(
		"$GORELEASER_BIN" --version |
			sed -n 's/^GitVersion:[[:space:]]*//p'
	)
	if [ "${actual_version#v}" != "${version#v}" ]; then
		echo "GoReleaser is $actual_version; want $version" >&2
		exit 1
	fi
	exec "$GORELEASER_BIN" "$@"
fi

exec go run github.com/goreleaser/goreleaser/v2@"$version" "$@"
