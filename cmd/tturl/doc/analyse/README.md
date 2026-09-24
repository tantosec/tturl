# `tturl analyse`

`tturl analyse` tests whether request identity systematically changes relative
response arrival order in a fixed HTTP/2 batch. It is designed for a focused
hypothesis containing two through eight requests.

## When to use it

Use `analyse` when you need one inferential decision across a small, fixed set
of requests. `analyse` uses arrival order only. It does not measure elapsed
delay or, by itself, identify a cause, establish exploitability, or establish a
vulnerability.

## Quick start

This local exercise requires Linux or macOS because `tturl demo-server` is
unsupported on Windows.

Start the controlled local target in one terminal:

```console
go run ./cmd/tturl demo-server
```

Then compare two controlled delays in another terminal:

```console
go run ./cmd/tturl analyse -k \
  --block 'https://127.0.0.1:8443/sleep?duration=100us' --name short \
  --block 'https://127.0.0.1:8443/sleep?duration=5000us' --name long
```

`-k` accepts the demo server's local certificate. Do not use it when server
authentication matters.

The text report records the resolved number of cycles, trials, request
operations, connections and concurrent request streams before measured
collection. Automatic connection fitting may already have opened TLS
connections and sent HTTP/2 PINGs before that report is printed.

Its default workload depends on the number of requests and ranges from 600 to
3,200 measured request operations.

`--warmup-only` assigns expanded blocks to a distinct connection-local priming
batch. `--padding` and `--padding-at` add synthetic fields by measured outbound
position. The report separates both interventions from canonical measured
request identity and records their resolved work and schedule.

## How the experiment is balanced

A **trial** releases the complete request batch once and ranks responses by
arrival order. A **cycle** contains one trial for every cyclic shift of the
batch, so every request occupies every outbound position exactly once.

Each cycle stays on one HTTP/2 connection. `--connections` may overlap whole
cycles on several connections. Automatic path fitting stops for review above
512 effective connections or 1,024 active request streams unless
`--connections-fit-max` supplies an explicit fitted ceiling or `unlimited` accepts
any fit. The product-wide policy and resource evidence are documented in the
[parallelism resource policy](../parallelism-resource-policy.md).
Parallel load can preserve activity on a high-latency target and reduce wall
time, but it can also alter or destroy the signal. The result describes the
selected concurrent setting. Use `--batch-rate-max` to cap trial starts and
`--request-rate-max` when an independent request-operation ceiling is needed.

The measured cycle count is fixed before the command connects. There is no
result-dependent acquisition stopping. `--cycles` can select another accepted
fixed workload; values below the modelled default receive a sensitivity
warning, and measured work cannot exceed 50,000 request operations.

## Reading the result

The report leads with one of three findings:

- `Evidence of a request-identity effect on relative arrival order`: the
  overall randomisation test rejected its fixed null at alpha 0.05. Pair rows,
  if any, identify contrasts supported after Holm adjustment across every
  request pair.
- `No request-dependent arrival-order effect detected`: the overall test did
  not reject. This is not evidence of equivalence, safety, or absence of a
  timing side channel.
- `Finding unavailable`: execution or evidence failed a validity check. No
  overall or pair inference is reported.

An available report distinguishes two kinds of p-value:

1. **Global p-value.** Method `A` asks whether request identity affects the
   batch anywhere. Small assignment spaces are enumerated exactly. Larger
   spaces use 9,999 reproducible Monte Carlo assignments.
2. **Pair p-values.** After a global detection, method `R` tests every request
   pair. The localisation summary displays Holm-adjusted values; the complete
   method section explicitly labels both raw and adjusted values.

An assignment is a post-acquisition relabelling of the retained rank table. It
does not send a request or add an observation. The 9,999 assignments calibrate
the randomisation test from the collected cycles; they do not stretch,
resample, or replace the HTTP evidence. The Monte Carlo p-value uses
`(extreme assignments + 1) / 10,000`, remains positive, and is reproducible
from the seed in text or JSON.

Every available text report shows the retained request-by-rank evidence twice:
an ASCII intensity profile for quickly reading distribution shape, followed by
the exact bracketed count vector for each request. Mean and normalised mean
rank are descriptive summaries in the profile, not additional tests.

The HTTP status outcomes, Position check, and Execution sections make the
report's validity evidence concrete. They show the exact HTTP statuses that
supplied the ranks, mean-rank spread across outbound positions, and
rank-complete trial, balanced-cycle, and connection accounting. Uniform status
evidence stays on one line; changes or differences expand into one row per
request with a review note.

Pair direction and size are descriptive. For example:

```text
short before long in 290/300 trials; mean rank gap 0.93; adjusted p=0.0001
```

This says `short` arrived before `long` in 290 retained trials, their average
rank differed by 0.93 places, and the pair remained supported after controlling
the complete pair family. It does not explain why.

Response-status warnings identify a possible change in server behaviour.
Response digests remain available in JSON evidence. Neither status nor digest
facts filter trials or invalidate arrival ranks. Run a separate `race`
experiment when full response inspection is needed.

## Reports and sensitive data

Text is the default and includes method components, reproduction details, and
every pair test. `--verbose` expands only the complete request headers and
bodies. `--report json` emits a `tturl.analyse/v1` JSONL stream: one run
record, measured then distinct priming request records, every measured trial as
it completes, and one terminal result. Only the result carries an inference; a
prefix without it is partial acquisition evidence.

By default, response evidence records status, lengths, and a digest over every
received body byte without retaining body bytes or header maps.
`--capture-headers` retains complete accepted headers and trailers.
`--capture-body SIZE` retains that many bytes from each response;
`--capture-body unlimited` explicitly permits unbounded per-response capture.
The run record also records the default 8 MiB accepted-body limit, default 30s
batch timeout, default unlimited run timeout, per-response body capture limit,
and the decoded header-section limit when header capture is enabled. The
shared 1 MiB parser limit applies in both capture modes. `--response-body-max` and
`--batch-timeout` apply to priming and measured work. `--run-timeout` bounds
fitting through live-resource disposal while leaving offline inference and
report writing outside its deadline. Crossing any finite limit before live
completion makes inference unavailable. Body capture is independent of these
receive and time limits.

Inputs and reports used during penetration tests can contain credentials,
request bodies, target data, and stable response fingerprints. Protect command
history, request files, terminal output, reports, and copied artefacts
accordingly. `tturl`
does not redact or encrypt them.

## Reference map

Most users need only this page and `tturl help analyse`. Deeper material is
collected in the [reference index](reference/README.md):

- the [statistical method](reference/analyse-method-contract.md);
- the [structured-output and data-handling reference](reference/analyse-structured-output.md);
- the [JSON Schema](../schemas/tturl-analyse-v1.schema.json); and
- the [parallelism resource policy](parallelism-resource-policy.md).
