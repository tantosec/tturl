# `tturl analyse` structured output and data handling

`--report json` writes a `tturl.analyse/v1` JSON Lines stream: one `run`,
exactly `request_count` request records, every attempted measured `trial`, and
one terminal `result`. The [schema][schema] defines each record. The
[common protocol](../../structured-output-contract.md) defines framing, bytes,
failure codes, completion and shared acquisition semantics.

## Run and catalogue

The run envelope records original argv, start time, tool build identity and
resolved target. Tool version is required; revision and modified are null when
provenance is unknown. `target.dial_address` identifies the transport endpoint;
`target.origin` records the resolved origin when available.

`plan.work` records planned cycles and trials. `plan.batch_width`, rotation
arrangement, assignment unit, warmup widths, positional padding and modelled
default describe the experiment. The plan is below the modelled default exactly
when its planned cycles are fewer than that default.

The sibling `method` records ID `analyse-a-r-holm/4` and the randomisation seed
chosen before acquisition. The [method contract](analyse-method-contract.md)
defines the fixed statistic, randomisation, pair adjustment and accepted scope.

Request records have dense ordered `request_id` values. Measured requests come
first, followed by distinct warmup requests. Their roles are `measured` and
`warmup`. Labels, URLs, headers and bodies belong to the catalogue. Trial
outcomes and statistical relations join it by request ID.

## Acquisition and raw evidence

`acquisition` contains resolved delivery, nullable run timeout, release delay,
withheld body bytes, TLS verification, response limits and capture. Delivery
separates requested selection, post-cap connection ceiling and effective
whole-cycle capacity. Finite rates create pacing; explicit path fitting adds
three observed PING samples and their maximum RTT basis. Fitting uses measured
width. Wider active priming affects admission and resource review without
changing that measured interval or fitted pool.

Response limits use nullable `body_bytes` and `batch_timeout_ns`;
null means unlimited. Capture uses `body_bytes: null` for unlimited and zero
for disabled. Header capture records its positive decoded header-section limit
when enabled. The shared parser limit still applies when capture is disabled.
The default receive limits are 8 MiB per response and 30s per batch.

Each measured trial has explicit `trial_index`, cycle, shift, `connection_id`,
dispatch, assignments, nullable failure and outcomes. Assignments contain
`{request_id, position}` and preserve intended rotation even when acquisition
fails before responses arrive. Failure is null on success or a common
`{code, message}` object. Local receive limits carry no response facts or ranks
and make analysis unavailable.

Dispatch timestamps describe outbound wall-clock activity. Arrival rank remains
the timing evidence. Concurrent trial lines may arrive out of numeric order;
use their coordinates rather than line numbers.

Capture retains accepted HTTP/2 bytes without decompressing content encodings.
Response metadata contains status, declared length or null, accepted body byte
count, captured byte count and completeness, SHA-256, nullable header and trailer
maps, and optional body data. Capture completeness means the prefix covers
accepted bytes; inspect failure and reset facts for response completion.

## Terminal analysis

The result separates `completion`, `execution` and `analysis`. Completion
explains how acquisition ended. Execution reconciles planned and observed
connections, priming, measured trials, cycles and request operations.

Available analysis contains:

- `state: "available"` and `finding.status`, either `difference_detected` or
  `no_difference_detected`;
- `global_test` with alpha, statistic, components and observed calibration;
- request statistics with IDs, samples, raw and normalised mean rank, status
  counts, status variation and non-2xx observations;
- `status_distributions_differ` as a descriptive response diagnostic; and
- pair tests with explicit left and right IDs, precedence counts, mean-rank
  difference, p-values, upper-bound flags and `significant`.

Calibration records method, draws, extremes, p-value, p-value kind and completion
reason. Pair significance agrees with the Holm-adjusted p-value and global
alpha. Labels join through requests; the method seed belongs to the run.

Unavailable analysis contains `state: "unavailable"` and a reason with an
analyse-owned code and non-empty diagnostic message. The common protocol lists
the exhaustive reason registry. A complete acquisition may still have
unavailable analysis. Partial execution and raw trials remain audit evidence.

## Replay and stored artefacts

Repository replay checks validate `tturl.analyse/v1` records against their
embedded schema, then check framing, catalogue identities and bodies, delivery
arithmetic, work, padding, assignments, coordinates, ranks, cycle completeness
and execution. They reconstruct transport-independent evidence, derive
unavailability from recorded acquisition and trials, repeat the recorded
method, and compare the complete terminal projection with the calculated
result.

A prefix without a terminal result has no inferential meaning. A partial final
line is truncated evidence, even if its bytes otherwise form a JSON object.

Reports preserve user-authored requests and target responses. URLs, arguments,
headers, bodies, diagnostics, extracts and stable digests can contain sensitive
engagement data. Protect stored and shared artefacts accordingly.

[schema]: ../../schemas/tturl-analyse-v1.schema.json
