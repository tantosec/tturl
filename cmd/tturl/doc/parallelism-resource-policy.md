# `tturl` automatic parallel-fit resource policy

## Contract

Repeated `race`, `measure`, `analyse`, and `detect` work explicitly selected
with `--connections-fit` shares one automatic path-fit review policy. Fitting
requires a finite pacing limit. Rates alone pace work with up to one
connection; `--connections` and `--connections-fit` conflict. A fit maximum
requires `--connections-fit` and does not enable fitting by itself:

```text
effective connections <= 512
effective active request streams <= 1,024
```

Equality is accepted. A path-fitted plan that exceeds either dimension stops
after its three PING samples and before trial connections dial. Direct
`--connections` is a deliberate fixed selection and is not subject to the
automatic review.

`--connections-fit-max N` retains PING fitting, caps the fitted ceiling at `N`, and
records consent to an effective plan beyond the review boundary. A binding cap
warns that the requested cadence may be unsustainable. `--connections-fit-max
unlimited` retains fitting without an operator ceiling. It still obeys
representability and finite-work constraints.

This fitted ceiling is distinct from `--connections`: the latter skips PING
and selects a fixed experimental condition. Reusing one observed fit as a
direct maximum would discard the path evidence and become stale as RTT changes.
An exact acknowledgement receipt would be brittle because normal PING variation
changes `ceil(RTT / interval)` between otherwise identical invocations.

## Resolution model

```text
effective interval = max(batch interval, measured width / request-rate ceiling)
raw fit             = ceil(largest PING RTT / effective interval)
headroom            = max(1, ceil(raw fit / 4))
fitted connections  = raw fit + headroom
selected ceiling    = min(fitted connections, positive fit maximum), if supplied
effective conns     = min(selected ceiling, finite work units)
active streams      = effective conns * maximum active batch width
```

Unbounded work has no finite-work narrowing. Rotation assigns whole cycles;
other arrangements distribute individual trials. `analyse` distributes cycles,
and a bounded `detect` distributes comparisons. Reports preserve the fitted
connection count separately from every narrowing step. The exact RTT and
interval evidence preserve the inputs from which the raw fit is derived.

The largest unacknowledged effective fit by supported batch width is:

| Width | Connections |
| ---: | ---: |
| 1 | 512 |
| 2 | 512 |
| 3 | 341 |
| 4 | 256 |
| 5 | 204 |
| 6 | 170 |
| 7 | 146 |
| 8 | 128 |

## Why both dimensions are bounded

Connections drive sockets, TLS state, handshakes, descriptors, and much of the
client and server memory. Streams approximate simultaneous request work. Equal
stream counts can have substantially different costs when divided across
different numbers of connections, so neither dimension replaces the other.

The thresholds are review boundaries informed by controlled resource
measurements, not portable capacity guarantees or target-safe defaults. Values
above them remain available through an explicit fitted or direct choice.

## When review becomes likely

With only a batch ceiling of `B` batches per second and sufficient work, review
begins when the observed RTT is greater than `C/B`, where `C` is the accepted
connection count for the batch width. Examples:

| Width | 10 batches/s | 100 batches/s | 1,000 batches/s |
| ---: | ---: | ---: | ---: |
| 1-2 | 40.9 s | 4.09 s | 409 ms |
| 4 | 20.4 s | 2.04 s | 204 ms |
| 8 | 10.2 s | 1.02 s | 102 ms |

The policy is therefore normally invisible at tens of batches per second. It
becomes relevant for high-rate or high-latency work, particularly wide batches.
A request-rate ceiling increases the effective interval and can only reduce the
fit.

## What the policy does and does not protect

The review prevents a large *automatic magnitude surprise*. It bounds the
connection and stream fan-out derived from RTT and a rate flag unless the
operator supplies an explicit magnitude.

It does not establish:

- a safe load for the target;
- preservation of an arrival-order signal;
- suitability for the local host's memory, descriptor, or port limits;
- a bound on response transfer time, received byte count, or hashing work;
- bounded retention when the operator selects unlimited body capture;
- a paced TLS connection ramp; or
- enough pipeline for application service time.

PING measures a path round trip and may terminate at an intermediary. It does
not invoke an HTTP handler or measure handler execution, response transfer, or
load-dependent queueing. The fitted connection count adds 25% headroom, with at
least one extra connection, to absorb modest unobserved connection occupancy.
It is not a service-time estimate or a throughput guarantee. Parallelism itself
can change the target and the signal. Batch and request ceilings bound offered
starts over time, not the initial connection-establishment burst or
instantaneous requests within one batch.

`tturl` limits each decoded response header or trailer section to 1 MiB and
retains no body prefixes by default. It still drains and hashes every received
body, so a target controls transfer duration and hashing work. A finite body
capture limit bounds only the retained prefix of each response. Selecting
`--capture-body unlimited` deliberately retains every received byte in each
response result.

Connection count also multiplies per-connection priming and replacement work.
Warmup remains paced and separately accounted, but neither a review acceptance
nor a fitted maximum claims that this extra work is safe or signal preserving.

## Enduring validation requirements

Tests should preserve:

- exact acceptance and one-step rejection for every supported batch width;
- finite-work narrowing before transport pool configuration and review;
- path-fit evidence even when finite work or a fit maximum narrows it;
- positive cap binding and non-binding behaviour, plus the `unlimited` bypass;
- flag applicability and mutual exclusion;
- pre-handler failure for every repeated-work command;
- shared friendly and structured delivery evidence; and
- exact-boundary live coverage at narrow and wide batch shapes.

Resource measurements should be repeated when transport buffering, response
collection, worker ownership, or connection establishment changes materially.
