# Structured-output examples

These complete, deterministic synthetic JSON Lines streams teach the common
structured-output protocol. They are checked against the current command
schemas and whole-stream relationships. The parent schema index identifies
live output.

The filenames identify completion or scientific state. Additional race streams
teach rate-only pacing, fixed capacity narrowed from eight connections to two
work units, all three fitted-maximum modes, and the single-trial skipped fit.
The width-five fitted examples use three 1s PINGs and a 200ms measured interval:
five base connections plus two headroom connections gives seven before capping.
The bounded maximum reduces this to four; finite work may narrow it further.

Analyse's available examples use complete rotation-cycle evidence. The
difference example uses the same observations and exact calibration as the
current internal replay fixture. The no-difference example balances request
ranks across positions. Detect retains input alias `fast` in argv while its
resolved method direction is `early`.

## Consumer joins

Run these commands from this directory with jq. Each expression uses explicit
identity; axes describe matrix orientation.

Read requested work beside resolved acquisition:

```sh
jq -s '.[0] | {argv, plan, acquisition, method}' race-fitted-bounded.jsonl
```

Join raw observations to their concrete request labels:

```sh
jq -s 'map(select(.kind == "request")) | INDEX(.request_id)' race-complete.jsonl
jq -s '(map(select(.kind == "request")) | INDEX(.request_id)) as $requests |
  .[] | select(.kind == "trial" or .kind == "warmup") | .outcomes[] |
  {label: $requests[.request_id | tostring].label, position, rank, response}' \
  race-complete.jsonl
```

Require terminal closure and inspect the failure that ended acquisition:

```sh
jq -se 'last | select(.kind == "result") | .completion' race-interrupted.jsonl
```

Join descriptive request aggregates and inspect the assignment axes:

```sh
jq -s '(map(select(.kind == "request")) | INDEX(.request_id)) as $requests |
  last.measurement.requests[] |
  {label: $requests[.request_id | tostring].label, samples, rank_counts}' \
  measure-available.jsonl
jq -s 'last.measurement.assignments' measure-available.jsonl
```

Read inferential evidence beside physical participation:

```sh
jq -s '(map(select(.kind == "request")) | INDEX(.request_id)) as $requests |
  last.detection.requests[] |
  {label: $requests[.request_id | tostring].label, physical, inference}' \
  detect-found.jsonl
```

Read a scientific finding independently of acquisition completion:

```sh
jq -s 'last | {completion, analysis}' analyse-difference.jsonl
jq -s 'last | {completion, detection}' detect-negative.jsonl
```
