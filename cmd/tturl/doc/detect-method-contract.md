# Detect method contract

`tturl.detect/v1` records a ranking-owned method ID and its requested and
resolved strategy beside the assignment plan and acquisition bounds. The ID
owns allocation, adaptive scheduling, uniform position randomisation, evidence
updates and stopping boundaries. Terminal aggregates preserve the
reported decision but do not provide a replay transcript.

Peer-first and baseline strategies differ in their reference mean and control
allocation. A baseline-relative score is conditional on the controls used in
that comparison. A measure mean rank alone does not establish that premise.
Candidate means from different adaptive phases need not be comparable.

Atomic strategies fold complete waves and allow arbitrary dependence within
an atomic wave. Rolling strategies fold the maximal contiguous dispatch
prefix and require the conditional mean premise across dispatch indices.
Changing collection order or replacing this folding policy would change the
method contract.

`edge` is a positive magnitude strictly between zero and 0.5. A negative
finding excludes the configured directional advantage: a late target scores
at least 0.5 plus edge; an early target scores at most 0.5 minus edge. It does
not exclude weaker effects or establish equality. Edge configures elimination
and does not enter positive evidence.

A found finding preserves the method's actual false-positive risk guarantee
and identifies the observed direction. A negative finding preserves the
false-negative risk guarantee and records the excluded edge. The two-direction
policy remains owned by the ranking method; readers must use the reported
risk rather than independently recalibrating the evidence. Inconclusive and
unavailable detection establish no exclusion claim.

Physical participations count successful request appearances, including
baseline instances and reused filler. Inference samples count active solver
observations for candidates. The counts and means can differ after adaptive
elimination or while comparisons are pending. Baselines have physical
participations without invented candidate inference; warmup operations are
excluded from both facets.

The [structured-output contract](structured-output-contract.md) defines
framing, catalogue joins, delivery and failures. See `tturl help detect` for
strategy selection and operational interpretation.
