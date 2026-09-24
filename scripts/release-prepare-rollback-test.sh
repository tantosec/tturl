#!/bin/sh

# This is an explicit release integration check, not part of ordinary Go tests
# or `make check`. It creates disposable Git repositories and launches shell,
# Git, and fully local fake commands to exercise rollback after each release
# preparation phase. It performs no network access or real release build.

set -eu

cd "$(dirname "$0")/.."

release_prepare="$PWD/scripts/release-prepare.sh"
temporary_directory=$(mktemp -d "${TMPDIR:-/tmp}/tturl-release-rollback.XXXXXX")
cleanup() {
	find "$temporary_directory" -depth -delete
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

for stage in before-version after-version after-citation validation rehearsal; do
	fixture="$temporary_directory/$stage/repository"
	bin="$temporary_directory/$stage/bin"
	mkdir -p "$fixture/scripts" "$fixture/release" "$bin"
	cp "$release_prepare" "$fixture/scripts/release-prepare.sh"

	printf '1.2.3\n' >"$fixture/release/version.txt"
	cat >"$fixture/CITATION.cff" <<'EOF'
cff-version: 1.2.0
version: 1.2.3
message: >-
  Exact bytes.
EOF
	cp "$fixture/release/version.txt" "$fixture/original-version.txt"
	cp "$fixture/CITATION.cff" "$fixture/original-CITATION.cff"

	cat >"$fixture/scripts/release-rehearse.sh" <<'EOF'
#!/bin/sh
[ "$RELEASE_PREPARE_TEST_STAGE" = rehearsal ] && exit 1
exit 0
EOF
	cat >"$bin/go" <<'EOF'
#!/bin/sh
case "$3" in
prepare)
	[ "$RELEASE_PREPARE_TEST_STAGE" = before-version ] && exit 1
	;;
set)
	printf '1.2.4\n' >release/version.txt
	[ "$RELEASE_PREPARE_TEST_STAGE" = after-version ] && exit 1
	printf 'cff-version: 1.2.0\nversion: 1.2.4\nchanged: true\n' >CITATION.cff
	[ "$RELEASE_PREPARE_TEST_STAGE" = after-citation ] && exit 1
	;;
check) ;;
*) exit 2 ;;
esac
EOF
	cat >"$bin/make" <<'EOF'
#!/bin/sh
[ "$RELEASE_PREPARE_TEST_STAGE" = validation ] && exit 1
exit 0
EOF
	chmod +x "$fixture/scripts/release-rehearse.sh" "$bin/go" "$bin/make"

	git -C "$fixture" init --quiet
	git -C "$fixture" config user.name 'Release rollback test'
	git -C "$fixture" config user.email 'release-rollback-test.invalid'
	git -C "$fixture" add .
	git -C "$fixture" commit --quiet -m fixture

	if (
		cd "$fixture"
		PATH="$bin:$PATH" RELEASE_PREPARE_TEST_STAGE="$stage" \
			./scripts/release-prepare.sh 1.2.4 \
			>"$temporary_directory/$stage/output.txt" 2>&1
	); then
		echo "release rollback test: $stage unexpectedly succeeded" >&2
		exit 1
	fi
	if ! cmp -s "$fixture/original-version.txt" "$fixture/release/version.txt" ||
		! cmp -s "$fixture/original-CITATION.cff" "$fixture/CITATION.cff"; then
		echo "release rollback test: $stage did not restore exact metadata" >&2
		exit 1
	fi
done

echo 'release rollback test: all failure phases restored exact metadata'
