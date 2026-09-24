# `tturl` output tour

This developer fixture captures representative help pages and command reports
from the current source tree. It covers routine, pathological, structured,
redirected, and partial-failure cases across `race`, `measure`, `analyse`,
`detect`, and `time`.

Start the demo server at `https://127.0.0.1:8443`, then run:

```sh
scripts/tturl-tour/generate.sh
scripts/tturl-tour/generate.sh --slug rank-profile-review
scripts/tturl-tour/generate.sh --url https://demo.example:9443
```

Each run writes `captures/<UTC-datetime>[-SLUG]/usage/` and
`captures/<UTC-datetime>[-SLUG]/commands/`. These generated directories are
ignored by Git. Captures show the rendered command followed by interleaved
stdout and stderr; every stderr line has a `STDERR: ` prefix.

Use `--url` when the demo server is remote or listening on another port. The
optional `--slug` must consist of letters and numbers separated by `.`, `_`, or
`-`. Generation fails rather than overwriting a capture created in the same
second with the same slug. Run `scripts/tturl-tour/generate.sh --help` for the
complete interface.

Set `TTURL_TOUR_BASE_URL` to use a demo server at another origin. Set
`TTURL_TOUR_VERBOSITY` to `normal` or `verbose` to force that request-detail
mode across every command capture; its default, `as-authored`, preserves each
case as written. For example:

```sh
TTURL_TOUR_VERBOSITY=normal \
  scripts/tturl-tour/generate.sh --slug normal
TTURL_TOUR_VERBOSITY=verbose \
  scripts/tturl-tour/generate.sh --slug verbose
```

The generator requires Bash, Go, Python 3, curl, and standard POSIX utilities.
It builds `tturl` into a temporary directory, retains partial captures when a
case fails, reports every unexpected result at the end, and exits nonzero.

The `time` cases show traditional duration summaries with compact and verbose
requests, a mixed-protocol dry-run, synchronised finalising release, bounded
pacing, JSON capture and extraction, mixed HTTP statuses, and acquisition
failure. They add 22 local target request operations, including two priming
operations. Their distributions describe observed durations; they do not infer
significance or identify a vulnerability. The dry-run adds no target traffic.
