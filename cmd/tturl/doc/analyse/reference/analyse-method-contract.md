# `tturl analyse` method contract

**Status: supported contract for method version `analyse-a-r-holm/4`.**

This document defines the calculations represented by
`tturl.analyse/v1`. It is sufficient to reproduce the global decision
and every pair adjustment from a structured report. It does not broaden the
accepted statistical scope.

## Accepted evidence

The method accepts two through eight requests in complete balanced rotation
cycles. Every trial in one cycle uses the same connection; different cycles
may use different connections and may overlap. If the width is `k` and the
retained cycle count is `C`, each cycle contains each of the `k` cyclic shifts
exactly once. There are `T=C*k` trials and `C*k*k` measured request operations.

Arrival ranks are zero-based permutations `0..k-1`. Every accepted trial has
one request at each outbound position, one arrival rank for each request, and
one final HTTP status for each request. The base arrangement and all cyclic
shifts must agree. Connection replacement within a cycle is invalid.

Inference is unavailable when any of these conditions fails, when measured
work is incomplete or interrupted, or when fewer than the width-specific pair
resolution floor is retained:

| Requests | Minimum cycles |
| ---: | ---: |
| 2 | 6 |
| 3 | 7 |
| 4 | 8 |
| 5 | 9 |
| 6 | 10 |
| 7 | 10 |
| 8 | 11 |

HTTP status differences do not select trials or change ranks. They are
reported for application-level review.

## Global statistic A

For request `i`, let `S_i` be its arrival-rank sum over the `T` retained
trials, and let `M=T*(k-1)/2`. The two normalised components are:

```text
L2   = sqrt(12 * sum_i((S_i-M)^2) / (T^2*k*(k^2-1)))
Linf = max_i(abs(S_i-M)/T) / ((k-1)/2)
A    = max(L2, Linf/sqrt(2))
```

`A` has one decision and one p-value. `L2` and `Linf` are components of that
statistic, not separately tested methods.

The sharp global null is `no_request_identity_effect`: changing request
identities according to the actual cycle-assignment mechanism would not
change the joint arrival ranks. For every randomisation assignment, ranks,
cycle order, shifts, positions, and each cycle's connection are held fixed.
One base request permutation is independently assigned to each cycle, and its
cyclic shifts reconstruct the trials in that cycle.

This is conditional randomisation inference under the sharp joint null. It
does not assume that outcomes from concurrent cycles are statistically
independent. Target-wide load or interference that changes arrival order by
request identity is part of the concurrent experiment, not noise removed by
the method. A parallel result therefore does not establish single-connection
behaviour.

Floating-point comparisons count a randomised statistic as at least as
extreme when it is greater than or equal to `A_observed-1e-12`.

### Exact calibration

When `(k!)^C <= 9,999`, all assignments are enumerated. The exact p-value is
the number whose statistic is at least as extreme as observed divided by the
complete orbit size. The outcome is `difference_detected` when `p <= 0.05`
and `no_difference_detected` otherwise.

### Fixed Monte Carlo calibration

Larger orbits use the seed chosen before measured responses. The randomiser
is SplitMix64. Each cycle receives an independent Fisher-Yates shuffle; a swap
index for a remaining prefix of size `b` uses rejection sampling. It rejects
outputs below `(-b) mod b`, evaluated with unsigned 64-bit arithmetic, then
reduces the first accepted output modulo `b`. The accepted range has a size
divisible by `b`, so every swap index is equally likely.

The implementation always evaluates 9,999 sampled assignments. If `E` sampled
statistics are at least as extreme as observed, the conservative Monte Carlo
p-value is:

```text
p = (E + 1) / 10,000
```

The added one represents the observed assignment and prevents a zero p-value.
The observed assignment and sampled assignments are exchangeable under the
sharp null, so rejecting when this p-value is at most 0.05 is a finite-sample
Monte Carlo randomisation test. The sampler never stops early. The report
records the seed, 9,999 draws, sampled extremes, p-value kind, and completion
reason. A sampled p-value is never reported as exact.

## Guarded pair method R

Pair localisation is `unavailable` with invalid evidence and `not_tested`
unless the global outcome is `difference_detected`. A detected global result
tests every unordered request pair.

For pair `(i,j)`, the cycle contribution is the sum over that cycle's trials
of `rank_i-rank_j`. The raw two-sided p-value is the probability, over all
independent sign flips of the cycle contributions, that the absolute signed
sum is at least the observed absolute sum. The implementation computes this
exactly by dynamic programming.

The sign flips represent independent cycle assignment choices, not an
assumption that observed cycle outcomes are independent samples. The pair null
is sharp for the joint concurrent experiment: swapping the pair identities in
any set of cycles would not change the joint arrival ranks.

The descriptive mean-rank difference is the total contribution divided by
`T`. A negative value means the left request arrived earlier on average. The
report also records the exact number of retained trials in which each request
arrived before the other.

For `m=k*(k-1)/2` pairs, Holm adjustment sorts raw p-values ascending, breaking
ties by left request ID and then right request ID. For zero-based sorted index
`r`, the candidate adjusted value is `(m-r)*p_r`. The reported value is the
maximum candidate through `r`, capped at one. A pair is selected when its
adjusted p-value is at most `0.05`.

The dynamic program uses binary64 probabilities. When its raw tail falls below
the `1e-300` reporting floor, the reported value is the larger of that floor
and a two-sided Hoeffding upper bound, with a binary64 evaluation guard. Text
marks the value with `<=`; JSON marks it as an upper bound. Holm propagates
that status through its multiplication and running maximum. This treatment is
conservative: an imprecise extreme tail can prevent selection but cannot cause
selection.

## Outcome semantics

- `difference_detected`: the valid global procedure crossed its fixed
  rejection boundary; guarded pair tests were performed.
- `no_difference_detected`: the valid global procedure did not reject; pair tests were
  not performed. This is not equivalence.
- `unavailable`: evidence or execution did not support inference; no global or
  pair value exists.

The method concerns relative response arrival order in this fixed batch and
run. It does not establish elapsed delay, cause, exploitability, severity, or
a vulnerability.
