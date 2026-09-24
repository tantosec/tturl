# tturl structured-output contract

This document specifies the common framing and evidence rules for arrival-order
streams and the separate request-duration stream contract. The schema index
identifies the contracts currently emitted by each command. The
request-duration section defines `tturl.time` accounting and timing; the other
operational and inferential sections describe the arrival-order commands.

## Retrieve schemas offline

Use the binary that produced the report to export its exact shipped contracts:

```sh
tturl schema race --output race.schema.json
tturl schema common -o common.schema.json
```

Replace `race` with `time`, `measure`, `analyse` or `detect` for the command.
The consumer needs that command document and common v1; those are the only
project-schema dependencies of the current family. Common is a definitions
resource, not a report schema on its own. Export preserves the documents' bytes
and identities. It does not validate reports or select historical contracts.

Register both documents with a Draft 2020-12 resolver under their original
`$id` values. Relative `$ref` values resolve against those identifiers, not
necessarily the saved filenames. Saving the files beside each other alone does
not guarantee offline resolution; configure the consumer's resolver.

The report's logical `schema` value selects an exact contract and differs from
the document's `$id` URI. Export neither requires nor promises network retrieval
of that URI. Each command schema validates individual JSONL records;
whole-stream order, catalogue joins and accounting require the
stream rules in this contract.
Run `tturl help schema` for resolver guidance in the executable.

## Vocabulary

| Term | Meaning |
| --- | --- |
| Request ID | Identity of one concrete request in a report's global catalogue. IDs are the dense zero-based range `0..request_count-1`; catalogue records appear in increasing ID order. |
| Role | A request's experimental purpose. Race, measure and analyse use `measured` for evidence-bearing requests; detect uses `candidate` or `baseline`. Every command may catalogue `warmup` requests. |
| Position | Zero-based outbound place within a batch. |
| Rank | Zero-based response arrival ordinal within a batch. A null rank is unobserved. |
| Trial | One measured batch attempt, which may be incomplete. An attempt does not prove that bytes reached the peer. |
| Cycle | One whole rotation of request-position assignments, acquired on one connection. |
| Comparison | Detect's physical measured batch attempt. Completed physical participation includes baseline requests and eliminated candidates reused as filler. |
| Batch | Requests sent as one operation, including measured and priming operations. |
| Warmup | Unmeasured preparation trials: connection-local in arrival-order commands and worker-local in tturl time. Accounting includes replacement work. |
| Priming | Preparation using warmup before measured work. Disabled priming contributes no active width. |
| Plan | Resolved experimental assignment and intended work, including requested work where it differs from planned work. |
| Acquisition | Live delivery, execution bounds, release controls and response limits or capture settings. |
| Method | Command-owned inferential configuration, independent of assignment and delivery. |
| Execution | Observed operational accounting. Attempted, rank-complete and retained counts describe different populations. |
| Completion | How acquisition ended: complete, stopped by interruption, or failed. A complete race may contain failed individual trials within its attempt budget. |
| Finding | A command-owned scientific conclusion from available analysis or detection. |
| Availability | Whether a command can provide its measurement or inference. Acquisition completion alone does not establish statistical availability. |
| Primary evidence | Concrete observations and execution accounting. Arrival order is the signal; outbound timestamps are diagnostic observations. |
| Derived aggregate | A summary calculated from observations, such as a histogram, mean rank, reset count or normalised rank. |

Detect inferential samples count comparisons in which a candidate participated
while in contention. Physical participation counts every inclusion in a
rank-complete comparison, including reuse after elimination. Failed comparison
operations are separate from completed solver cost.

## Framing and identity

A complete report is JSON Lines: one `run`, exactly `request_count` `request`
records, zero or more command evidence records, and one terminal `result`.
Only `run` carries `schema` and `command`. Select the schema from that first
record and use it for the entire stream. A report has one global request-ID
namespace. Public references use `request_id`, `left_request_id` and
`right_request_id`; resolve them through the catalogue. Connection and worker
identities use `connection_id` and `worker_id`. Evidence may arrive out of
`trial_index` order. Independent reports are independent streams.

Every successful record write produces a complete JSON object and newline.
A failed write may leave a truncated final record. The valid prefix consists
only of complete newline-terminated records before that fragment. Consumers
may discard the fragment; a complete report still requires terminal `result`.
Failure before a run envelope is available may produce no stream.

## Reconstruction and strings

Required `argv` preserves the original ordered argument strings, starting with
the subcommand and excluding the executable path. It preserves aliases,
argument boundaries and empty arguments. Resolved `plan`, `acquisition` and
`method` are authoritative; argv supplies reconstruction context. Referenced
files and external state are not captured.

Byte-bearing scalar strings and header keys or values use the wire-string
encoding: valid UTF-8 remains a string, except a literal canonical `$B64[...]`
marker is itself encoded. Invalid UTF-8 uses `$B64[BASE64]` with canonical
standard base64. Decode a canonical marker once to recover the original bytes.
Explicit `{encoding, data}` objects instead use `utf-8` or `base64` directly.
JSON escapes keep the serialised output ASCII without changing decoded text.
Argv, requests and captured responses can contain sensitive data.

## Values and accounting

Durations are integer nanoseconds and have `_ns` suffixes. A finite bound is
positive; `null` means unlimited only for applicable bounds. Counts are
non-negative. Disabled body capture is zero; unlimited capture is null. Unknown
response content length is null. Omission means the concept does not apply.
Unobserved ranks, timestamps and means are null, never unlimited.

Failure is `{code, message}`, with a stable code and non-empty diagnostic
message. Completion is a discriminated state: `complete` has null failure,
`stopped` has an `interrupted` failure, and `failed` has a non-null diagnostic failure.
An attempt's nullable failure is independent of terminal completion.

For each batch population, attempted equals rank-complete plus incomplete.
For retained evidence, rank-complete equals retained plus excluded where those
counts apply. Bounded planned work equals attempted plus unattempted.
Request operations count physical inclusions, rather than inferred samples.
Raw mean rank uses zero-based ranks; normalised mean rank divides by batch
width minus one, with a zero value for a single-request batch.

Measure histograms omit zero entries and order records by rank. Pairwise
precedence uses rows from `before_request_ids` and columns from
`after_request_ids`. Assignment counts use rows from `request_ids` and columns
from `positions`. Axes are unique and increasing; matrix dimensions match
their axes. Explicit axes provide identity, including when every count is zero.

## Record order and references

| Command | Evidence between catalogue and result | Catalogue role order |
| --- | --- | --- |
| `tturl race` | Zero or more warmups, then measured trials as each connection progresses; warmups on replacement connections may interleave with trials. | measured, warmup |
| `tturl measure` | None; the result contains descriptive aggregates. | measured, warmup |
| `tturl analyse` | Every attempted measured trial, including incomplete attempts. | measured, warmup |
| `tturl detect` | None; the result contains adaptive aggregates and failures. | candidate, baseline, warmup |

Each role forms a contiguous catalogue group; optional groups may be empty.
Every catalogue entry describes a concrete request, including expanded baseline
and distinct warmup requests. Labels are unique complete printable ASCII
identities. Baselines alone carry `supply_limit`: positive finite supply or null
for unlimited. When a race reuses measured requests for priming, warmup outcomes
reference those measured IDs instead of duplicating catalogue entries.

Every reference must resolve to a catalogue entry of the appropriate role.
Measured trial assignments and outcomes reference measured requests; detect
physical evidence references candidates or baselines, inference and selected
findings reference candidates. Warmup evidence references warmup requests, or
reused measured requests when there is no distinct warmup catalogue.

Request aggregates are in increasing request-ID order. Status, reset and rank
count lists contain positive counts with unique increasing keys; omitted keys
have zero count. Pair tests use increasing `(left_request_id, right_request_id)`
pairs with left less than right. Assignments are ordered by outbound position. Outcomes increase by global
request ID and have unique positions and request IDs within a batch.
Observed ranks are unique within a batch. Complete ranks cover `0..width-1`.
Trial indices are unique and lie within planned work when work is bounded.
Cycle/shift coordinates describe the plan's rotation and must agree with the
trial index. A complete analyse cycle has every shift, on one connection.

A per-record schema cannot establish these references, dimensions, orderings
or arithmetic relationships. Consumers enforce them across the selected
stream. Unknown schema identifiers must not be interpreted as a known family.
Schema and binary versions are independent. The coordinated current family is
listed in [the schema index](schemas/README.md).

## Response and capture interpretation

Header objects map byte-preserving names to ordered arrays of byte-preserving
values. Apply wire-string decoding independently to each key and value.
Request body and extracted data objects state their own encoding. Captured
response bodies use base64. SHA-256 digests use canonical standard base64 of
32 digest bytes, including the digest of an empty accepted body.

Accepted body bytes count DATA accepted before completion or reset, without
content decompression. Captured bytes count the retained prefix.
`body_captured_complete` means that prefix covers all accepted bytes. It does
not establish a clean stream end or agreement with declared content length.
Disabled capture omits body data; uncaptured headers and trailers are null.
Unknown response metadata is null. Local receive-limit failures provide no
response facts from that batch. Reset facts describe protocol outcomes
separately from command completion.

`headers_sent_at` and `body_finalised_at` are UTC wall-clock timestamps sampled
immediately before the corresponding TLS dispatch. They are null when dispatch
did not complete; body finalisation is also null for a bodyless batch. They
are outbound diagnostics, never response elapsed-time measurements.

## Failure and availability registries

Common failure codes are `response_body_limit`, `batch_timeout`, `run_timeout`,
`interrupted`, `deadline_exceeded`, `connection_error`, `transport_error`,
`comparison_failed`, `acquisition_failed` and `internal_error`. Codes classify
local receive limits, execution context, connection establishment, transport,
or command-owned fallback failures. Messages preserve diagnostic detail;
consumers branch on codes rather than parsing messages.

Measurement availability describes whether retained rank observations can be
summarised. Analyse availability additionally requires evidence accepted by its
fixed method. Detect availability and its found, negative or inconclusive
finding describe the adaptive method, separately from physical participation.
A partial acquisition can retain descriptive observations while inference is
unavailable. A complete acquisition can also have unavailable analysis.

Analyse unavailable reason codes are `unsupported_width`,
`insufficient_cycles`, `incomplete_cycle`, `invalid_shift`,
`invalid_arrangement`, `invalid_ranks`, `invalid_response_status`,
`mixed_connections`, `evidence_too_large`, `invalid_workload` and `interrupted`.
Their evidence and replay boundaries belong to the
[analyse structured-output reference](analyse/reference/analyse-structured-output.md)
and [method contract](analyse/reference/analyse-method-contract.md).
The [detect method contract](detect-method-contract.md) defines inference
samples, logarithmic evidence, directional edge and error guarantees.
Descriptive status diagnostics do not modify either inferential method.

Raw trials are primary evidence in race and analyse. Measure and detect omit
raw measured transcripts intentionally: their labelled aggregates retain the
calculations needed to interpret descriptive or adaptive evidence. Request
operation accounting counts physical inclusions, including priming and failed
attempts in their respective populations, rather than inference samples.

A complete newline-terminated prefix can be structurally valid without being
a complete report. An output failure or consumer-closed pipe can prevent the
terminal result. Exit status alone does not certify completeness. A native
analyse prefix without a terminal result has no inferential meaning.

Protect stored argv, URLs, headers, bodies, diagnostics, extracts and digests
as sensitive engagement data. Encoding preserves bytes; it provides no
redaction or confidentiality.

## Resolved delivery relationships

`selection.mode` distinguishes default capacity, fixed requested connections
and explicit path fitting. A finite rate alone adds pacing to default capacity.
Path fitting carries a maximum policy: `automatic_review` when omitted,
`unlimited` when explicitly acknowledged without a cap, or `bounded` with
positive connections. Only bounded mode carries that numerical maximum.
`skipped_single_trial` applies only to race with one planned measured trial,
including when priming is active. Rotation can turn one requested trial into
several planned trials and make fitting eligible.

The ceiling is one for default or skipped fitting, the requested count for
fixed selection, or the fitted count after a numerical cap. The effective
connection limit is positive and no greater than that ceiling after narrowing
to usable work. None/random arrangement distributes trials; rotation distributes
whole cycles. Detect uses its resolved adaptive concurrency. Opened and
replacement connections are execution observations.

`rate_limits` appears exactly when at least one rate is finite. Each rate is
positive or null for unlimited; both null requires omission of the object.
The measured interval in integer nanoseconds is the maximum of
`ceil(1e9 / batch_rate)` and `ceil(width * 1e9 / request_rate)` for the finite
rates. Detect width includes scheduled baselines and filler. Wider active
priming uses its own admission width without changing this measured interval.
The interval is a start-eligibility bound, rather than observed cadence.

`path_fit` appears exactly for fitted selection. Its basis is the maximum of
three positive observed PING RTTs. Let `base = ceil(basis / measured_interval)`;
the pre-cap fitted count is `base + max(1, ceil(base / 4))`. A bounded maximum
sets `ceiling = min(fitted, maximum)` and `capped = (ceiling < fitted)`.
Finite-work narrowing alone never sets capped. The fit is fixed before
acquisition and estimates path delay. The operational resource-review and
pacing rules are owned by [delivery help](../helptext/delivery.txt).

## Request-duration streams

`tturl.time/v1` uses the framing, catalogue identity, argv and lossless
string rules above. Its command-owned envelope references common v1 byte and
build definitions. Export `tturl schema time` and `tturl schema common` for
individual-record validation. The resolved policy is `run.experiment`; the
extractor catalogue is `run.extractors`.

The request catalogue contains measured requests followed by any distinct
priming requests, in dense request-ID order. `request_set` is `measured` or
`priming`. The experiment lists both sets explicitly. With measured-set priming,
priming IDs refer to measured catalogue entries. Requests retain protocol,
complete label, prepared body bytes, emitted Content-Length when present and
length-mismatch classification. HTTP/2 pseudo-fields are separate from ordinary
fields; HTTP/1.1 has an empty pseudo-field map.

Evidence records are `phase`, `warmup` or `trial`. Phase records mark campaign
initial acquisition (`initial_acquisition`), each worker's completed warmup and
usable readiness (`worker_warmup`), and campaign initial warmup
(`initial_warmup`). Campaign phase records have worker ID -1 and phase index
zero. Initial acquisition precedes every warmup release; campaign initial
warmup completion precedes every measured release. Worker completion is emitted
only after all required trials and usable-readiness checks succeed. Trial indices identify planned work
and may arrive out of index order. Each trial retains actual position order and
one outcome per planned request, including unreleased members. Physical
connection IDs start at one; null means no assigned connection. Inline
`connections` entries on evidence and result records retain acquisition,
retirement, disposal and replacement history, pool and worker membership,
negotiated protocol and local and remote addresses. A replacement names its
predecessor and never replays a measured request.

Every measured or warmup trial has a UTC `started_at` calendar anchor. Its
monotonic milestone offsets are signed integer nanoseconds from the shared
anchor: `initial_release_ns`, `final_release_ns`, `write_complete_ns`,
`first_response_headers_ns`, `final_response_headers_ns` and
`response_complete_ns`. Missing observations are null with nonempty entries in
`timing.missing_reasons`. The only stored derived request duration is
`duration_ns = final_response_headers_ns - initial_release_ns`. Calendar
strings are provenance and do not calculate elapsed measurements.

Warmup records identify a worker, `phase` (`initial` or `replacement`),
`phase_index` and `trial_index`. Phase indices are zero-based per worker: initial
warmup is zero, and every replacement restart advances it, including interrupted
phases. Trial indices restart at zero in each phase. Every outcome identifies
its own physical connection.

Common gates exist for synchronised measured and warmup trials. Sequential
trials retain absent gate offsets and explicit reasons. Warmup executes the
resolved request set once per trial with the selected arrangement and delivery
settings. Its rotation starts at zero, runs exactly N trials and does not
advance measured rotation. Gate offsets may be negative, and initial
release need not be zero. Preserve early responses and successful validated
response draining even when later request delivery fails. `response.complete`
describes validated draining; successful request-operation accounting also
requires no exchange failure.

A trial is complete when every planned request was attempted and completed its
exchange successfully. A bounded run completes when all planned trials were
attempted and terminal reporting succeeds; incomplete trials remain visible.
An incomplete warmup exchange, fatal recovery, run timeout, output failure
or unfinished bounded work fails. Complete non-2xx warmup responses are acceptable; limited
body capture does not change exchange completion. Normal retirement after a
complete exchange may interrupt a sequential warmup trial; bounded recovery
restarts its worker phase and retains completed and unattempted outcomes.
A fulfilled unlimited user stop is `stopped`. Report-channel failure can leave
a complete-line prefix without a terminal result. Use the terminal completion,
failure and accounting together when assessing execution.

The terminal result uses `completion: {state, failure}`. Complete runs have a
null failure. Fulfilled unlimited stops carry a run-phase `interrupted`
disposition; this is a successful stop, not an engine error. Failed runs retain
one non-null phased failure inside completion, including bounded cancellation.
Failure codes are the timing engine's nonempty taxonomy; messages are nonempty.
Unlimited trial counts, planned operations, timeouts, rates and accepted-body
limits are null. Disabled body capture has a zero bound; unlimited capture is
null. A zero release delay and derived connection allowance remain factual zero.

Response `body_bytes` counts accepted bytes, while `received_body_bytes`
retains bytes received before acceptance limits or validation. Unavailable status,
`content_length` and digest are null. A parsed response status is diagnostic
evidence and may include an invalid code; its presence does not establish valid
final response headers or a duration. Disabled or unavailable captures have null
headers, trailers and body. A captured body uses the shared base64 encoded-data
object. `body_captured_bytes` describes its retained prefix;
`body_captured_complete` means it covers all accepted bytes, independently of
clean response completion. The response digest covers accepted bytes, including
bytes outside the captured prefix.

The result reconciles measured trials, rotation cycles, request operations,
warmup trials and operations, commitments and offered operation cost,
connections and acquisition failures. Attempted request operations begin at
actual initial release. Commitments charge the whole upcoming batch once,
including unreleased members after a later failure. Priming uses separate
populations and costs. Initial and replacement warmup trials are counted
separately. A complete operation has a fully drained response and no exchange
failure; a complete trial has every operation complete. Unreleased participants
are unattempted, distinct from failed attempted operations.
`warmup_operations_unattempted` counts unreleased participants of observed
warmup trials; it excludes warmup trials that never entered execution. Planned
initial costs describe the initial budget separately. Rotation counts whole
worker-owned measured cycles.

`warmup_width`, `planned_initial_warmup_trials` and
`planned_initial_warmup_operations` describe resolved initial costs: K*N trials
and K*N*Wp operations for K admitted workers and width Wp. Both planned costs
are zero when warmup is disabled; warmup width still describes the resolved
request set. Replacement costs are observed rather than planned. Cost
arithmetic is checked for overflow during resolution.

Each measured request's duration population includes every valid final-header
duration, including non-2xx responses, incomplete trials and later body failures.
It excludes priming and absent final-header durations. Count, minimum and maximum
are exact integers; R7 percentiles, mean and population standard deviation are
finite floating-point nanoseconds. Empty populations have null statistics;
a single observation has zero standard deviation. Per-request quality counts
retain statuses, failures, incomplete responses, early responses and resets.
Attempted and unattempted operation counts reconcile terminal engine accounting,
including partial operations produced during cancellation. Duration, status,
failure and response-quality populations aggregate delivered observations.

Optional capture preserves accepted body prefixes, headers and trailers without
changing the timer endpoint. Extraction uses the run catalogue, shared byte
encoding, byte offsets and named-group participation. JSON retains selected
matches. Text reports aggregate capture coverage, matched responses, match
counts and named-group participation. Exact summary quantiles retain integer
samples; streamed evidence applies bounded backpressure and does not retain
trial bodies for summaries. The live run budget ends after all target I/O and
disposal are joined; terminal report draining has no live execution deadline.
