# `tturl` request identity and reference contract

This contract defines how parsed request input becomes stable identities in
`tturl` reports. It spans two owners:

- `internal/curlblocks` constructs complete labels and group-selection keys;
- `cmd/tturl` assigns run-local IDs and projects labels into bounded text
  references.

The public declarations and tests remain authoritative for implementation
details. This document owns the invariants that must hold across parsing,
request roles, text presentation, structured output, and diagnostics.

## Identities

Four values have distinct jobs:

| Value | Meaning | Stability |
|---|---|---|
| name | Optional `--name` input selecting one source block. | User-authored. |
| label | Complete, unique, printable-ASCII identity of one concrete request. | Stable for one resolved plan. |
| request ID | Zero-based index assigned by `tturl` after request roles are known. | Stable for one run. |
| reference | Repeated inline text representation of one request. | Stable for one report layout. |

A name is a group key and may select several requests. A label identifies one
request after URL, fan, and repeat expansion. An ID joins structured evidence
and compact text back to that label. A reference is presentation only.

Labels and IDs are output identities, not a syntax for reconstructing requests.

## Complete labels

`curlblocks.Plan.Expand` resolves labels across the complete plan before
`tturl` builds requests. The label forms are:

| Input shape | Label |
|---|---|
| unnamed plain request | `A` |
| named plain request | `group` |
| unnamed fan | `TOKEN=value` |
| named fan | `group:TOKEN=value` |
| unnamed multi-token fan | `A=1,B=x` |
| named multi-token fan | `group:A=1,B=x` |

Fan token/value pairs retain their declared order. A name always remains
visible in a named fan's labels.

Repeats and duplicate fan variants within one source receive a `~N` suffix.
The ordinal is one-based and padded to the decimal width of the multiplicity:

```text
group~1
group~2

group:TOKEN=value~01
group:TOKEN=value~02
...
group:TOKEN=value~12
```

Separately authored unnamed requests receive distinct spreadsheet-style
labels: `A` through `Z`, then `AA`, `AB`, and so on. Allocation skips
explicit, fan-derived, and multiplicity labels already claimed by the plan.

Labels are complete. Presentation width never changes their construction and
a fan value is never replaced by an index.

## Uniqueness and group selection

Name and label resolution uses structured provenance: source block, optional
name, ordered fan bindings, and multiplicity. It rejects:

- reuse of one explicit name by distinct source blocks;
- distinct inputs that produce the same rendered label; and
- collisions between automatic labels and already claimed labels or
  multiplicity forms.

Silent deconfliction is forbidden. It would allow the meaning of a persisted
label to change when unrelated input is added.

Every label also carries zero or more group keys:

- a named plain block carries its name;
- a fan carries each token, in declaration order;
- a named fan also carries its name.

`--pin` accepts either one complete label or one group key. It applies the
same display conversion as label construction, so raw input and the
`$HEX[...]` spelling copied from output select the same group.

## Printable text

`curlblocks` converts every label component and group key to printable ASCII
before collision detection. A high byte, control byte, or edge whitespace is
rendered with hashcat-style `$HEX[<hex>]`.

The representation is deliberately inspectable rather than injective.
Collision detection prevents two distinct inputs that render identically from
silently sharing a label.

`tturl` does not escape labels again. The complete label is the common identity
from which request summaries, text reports, progress, diagnostics, pin
selection, and structured output are derived.

## Request IDs and roles

`tturl` assigns IDs after classifying requests as primary or distinct warmup
requests:

1. primary requests receive IDs in their expansion order;
2. distinct warmup requests follow in their expansion order.

Adding a distinct warmup set therefore does not renumber primary requests.
Labels are still resolved across the whole input plan; only the ID ordering is
role-aware.

Ordinary warmup trials use primary request IDs. A distinct priming batch uses
warmup request IDs. Non-warmup trials use only primary request IDs.

Every structured request record includes its ID, complete label, and request
set. All four request commands emit these records in their JSONL streams.
Race names its primary set `trial`; the aggregating commands name theirs
`measured`. Every command names a distinct priming set `warmup`.

## Bounded text references

Each reporter computes the maximum width available to an inline request
identity and creates one projection for its report:

- if every complete label fits, every reference is the label;
- if any label is too wide, every reference is
  `#<id> <middle-elided-label>`.

The ID prefix counts against the width. Middle elision preserves both ends of
the label. A reporter may choose a different width because its fixed columns
differ, but it must not choose different identity forms for different rows.

When compact references are active, the request summary prints the complete
ID-to-label mapping once. Complete labels may wrap there but are never
truncated.

Structured output always carries the complete label and never adopts the
textual elision.

## Encoding in structured output

Labels are printable ASCII and therefore need no byte fallback. Other
byte-bearing scalar strings use the shared structured-output rule:

- valid UTF-8 remains text and is JSON-escaped to ASCII on the wire;
- invalid UTF-8 uses padded standard base64 in `$B64[...]`;
- a valid literal that is itself a canonical `$B64[...]` marker is encoded
  too, keeping the representation injective.

Bodies and extracted matches use their explicit UTF-8 or base64 data objects
instead of the scalar marker convention.

## Ownership and validation

The implementation homes are:

| Concern | Home |
|---|---|
| Label construction, provenance, collisions, and pins | [`internal/curlblocks/naming.go`](../../../internal/curlblocks/naming.go) |
| Printable display conversion | [`internal/curlblocks/varyencode.go`](../../../internal/curlblocks/varyencode.go) |
| Measured-before-warmup ID ordering | [`batch.go`](../batch.go) |
| Bounded report references | [`run_identity.go`](../run_identity.go) |
| Structured request identities and byte encoding | [`race_capture.go`](../race_capture.go), [`json_wire.go`](../json_wire.go), and command-specific reporters |

Tests must preserve:

- every canonical label form and multiplicity width;
- collision rejection after display conversion;
- automatic-label allocation around claimed labels;
- group pins by name and fan token;
- measured IDs preceding distinct warmup IDs;
- the report-wide switch to ID-prefixed references;
- width accounting for multi-digit IDs and middle elision;
- complete structured labels independent of text width; and
- injective structured encoding for invalid UTF-8 and reserved-marker
  collisions.
