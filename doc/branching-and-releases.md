# Branching and releases

This defines the release policy and the maintainer procedure for publishing a
release.

## Release policy

- `main` is the integration branch. Changes reach it through pull requests and
  must pass the required CI checks.
- Merging a pull request does not publish a release.
- A release is an immutable Git tag on a commit already merged into `main`.
  The tag is the reviewed semantic version prefixed with `v`; a release
  candidate ends with `-rc.` and a positive counter.
- `release/version.txt` and `CITATION.cff` record the same reviewed release
  version. They are updated together in a pull request before the corresponding
  tag is created.
- All packages and commands in this Go module share the same release tags.
- Published tags and exact-version artefacts are never moved or replaced.
  Correct mistakes with a new version.

## Repository controls

This procedure relies on GitHub rules outside the repository. Those rules must:

- require pull requests and the `Go minimum`, `Go current`, `Nix`, and
  `Release snapshot` checks before changes reach `main`; and
- restrict creation of `v*` tags to authorised release maintainers and prevent
  published tags from being updated or deleted.

Review these controls when repository settings change. They do not need to be
rechecked for each release.

The required CI aggregates include the fixed ranking qualification gate. Its
place in pull-request and release evidence, and the handling of statistical
rejections, are defined by the
[ranking test evidence policy](../internal/ranking/doc/ranking-test-evidence-policy.md).

## Prerequisites

Run release preparation from Linux or macOS with Git, Go, and Make on `PATH`.
Preparation does not require GoReleaser, Nix, or Docker; the Make targets
acquire GoReleaser, and CI owns the Nix and Docker checks.

```sh
command -v git go make
```

This command must find all three executables.

Start from a clean topic branch that contains current `origin/main`, and fetch
all release tags before choosing a version:

```sh
git fetch origin main:refs/remotes/origin/main --tags
git status --short
git merge-base --is-ancestor origin/main HEAD
```

`git status --short` must print nothing, and `git merge-base` must succeed. A
candidate must be newer than the version shared by `release/version.txt` and
`CITATION.cff`, and every existing release tag under semantic-version ordering.

Enter versions without the leading `v`. A stable version has three
dot-separated numeric components. A release candidate appends `-rc.` and a
positive counter. Subsequent candidates increment that counter; removing the
suffix produces the corresponding stable version.

## 1. Prepare the release intent

Pass the intended version directly to the preparation target. For example:

```sh
make release-prepare VERSION=1.2.3-rc.1
```

The target validates the version against the reviewed version and existing
tags, updates `release/version.txt` and `CITATION.cff` as one transaction, and
rehearses the release without publishing it. A failure restores both files; a
success leaves one reviewable two-file version change.

Review and commit the result:

```sh
git diff -- release/version.txt CITATION.cff
git status --short
git add release/version.txt CITATION.cff
git commit -m "Prepare release intent"
git push -u origin HEAD
```

Open or update the pull request. Before merging, confirm that the version diff
is intentional and all required checks pass on a merge candidate current with
`main`.

## 2. Merge and create the tag

After the pull request merges, update local `main` and confirm it is exactly the
commit you intend to release:

```sh
git switch main
git pull --ff-only origin main
git status --short
test "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)"
printf 'Expected release version (without v): '
IFS= read -r expected_version
version="$(tr -d '\n' <release/version.txt)"
test "$version" = "$expected_version"
tag="v$version"
```

The working tree must be clean and both `test` commands must succeed. Create
and push only the intended tag:

```sh
git tag "$tag"
git push origin "$tag"
```

Do not use `git push --tags`. The pushed tag is the publication event.

## 3. Complete publication

Open the tag-triggered `release` workflow and wait for the complete run to
succeed. The GitHub Release and container are independent outputs, so one may
complete while the other needs recovery.

The workflow verifies the published artefacts. Confirm separately that the
release and container are accessible through the paths documented for their
intended consumers.

Release notes start after the most recent published, non-draft GitHub Release.
If a tag fails before its GitHub Release is published, its changes are included
in the next successful publication.

For a stable release after a candidate, repeat the complete preparation process
with the stable version.

## Recovery

For a transient or partial publication failure, promptly use **Re-run failed
jobs** on the same workflow run. The publication steps resume matching state
and reject mismatches. If the run's temporary artefacts have expired, use
**Re-run all jobs** instead.

Do not delete a correct artefact, overwrite a release asset, or move the tag.
If the tagged source or workflow itself is wrong, fix it through a new pull
request and prepare a newer version.

## Build identity

A release binary identifies the exact release tag and source revision. Builds
from untagged source, including Nix flake builds, identify as `devel` instead of
borrowing a release version.

| Source state | `tturl --version` | Default User-Agent |
| --- | --- | --- |
| Clean release | `tturl <release-tag> (revision)` | `tturl/<release-tag>` |
| Clean development source | `tturl devel (revision)` | `tturl/devel` |
| Dirty development source | `tturl devel (revision-dirty)` | `tturl/devel` |
| No revision metadata | `tturl devel` | `tturl/devel` |

The diagnostic revision is shortened to 12 characters. Structured reports
retain the full available revision and modification state; text reports do not
include build identity. The User-Agent never includes a revision and may be
overridden or suppressed by the operator.

`release/version.txt` is release intent and the Nix package-ordering baseline;
`CITATION.cff` carries the same reviewed version for consumers. Neither is
runtime build identity. Nix package metadata uses an unstable date version,
while the resulting command continues to identify as a development build.
