# Outlier solver qualification

The outlier solver qualification gate is the enforcing sampled acceptance gate
for the release-supported solver fleet. It uses the shared
[evaluation harness](outliersolver-evaluation.md), but a versioned
qualification plan independently owns the release decision.

The gate combines three forms of evidence:

- **error-control calibration** checks false-find and false-negative rates;
- **utility regression** checks operational detection outcomes without making
  theorem claims; and
- **exact conformance** checks behaviour and accounting with zero tolerance.

This distinction is why the complete gate is called qualification rather than
calibration or a stress test.

## Commands

Run the complete fleet locally:

```sh
make ranking-qualify
```

Run one exact solver shard:

```sh
make ranking-qualify \
  RANKING_QUALIFICATION_SHARD=baseline-reserved
```

List the authoritative plan, shard IDs, scenarios, and checks without running
trials:

```sh
make ranking-qualification-plan
```

The direct interface is `TestOutlierSolverQualification`:

```sh
go test ./internal/ranking -run '^TestOutlierSolverQualification$' -count=1 -v \
  -args -qualification.shard=baseline-reserved \
  -qualification.output=/new/path/ranking-qualification.jsonl
```

`-qualification.shard` accepts one exact admitted solver ID or `all`. It is not
a regular expression. The plan supplies every other enforcing input; callers
cannot filter scenarios, change the run count, change seeds, or switch drivers.

An experimental one-shot rolling admission plan is separately selectable. It
does not alter the release fleet:

```sh
go test ./internal/ranking -run '^TestOutlierSolverQualificationCampaign$' -count=1 -v \
  -args -qualification.plan=rolling-admission \
  -qualification-campaign.run-id=1 \
  -qualification-campaign.shard=rolling-baseline-reserved \
  -qualification-campaign.output=/new/path/rolling-admission.jsonl
```

The rolling plan uses `Identify` as its declared decision boundary. Its fixed
seed interval is `[3001, 6001)`; campaign run ID 1 receives `[6001, 9001)`.
Passing this plan supplies admission evidence but does not itself modify the
release plan.

Rolling evaluation binds simulated channel draws to the solver's logical
dispatch indices. This keeps concurrent one-shot runs exactly replayable even
when worker calls enter the channel in a different physical order.

Rolling Peer First and rolling Baseline Confirmed use a second prospective
one-shot admission plan:

```sh
go test ./internal/ranking -run '^TestOutlierSolverQualificationCampaign$' -count=1 -v \
  -args -qualification.plan=rolling-siblings-admission \
  -qualification-campaign.run-id=1 \
  -qualification-campaign.shard=all \
  -qualification-campaign.output=/new/path/rolling-siblings-admission.jsonl
```

Plan `tturl/ranking/rolling-siblings-admission/v1` has fixed seed
interval `[9001, 12001)`; campaign run ID 1 receives `[12001, 15001)`. It uses
the same 14 reviewed scenarios and 18 checks per solver as the release plan,
but qualifying it does not itself add either sibling to the release fleet.

### Independent campaigns

An independent campaign applies the same plan to a prospectively selected,
disjoint seed window without changing the fixed gate:

```sh
make ranking-qualification-campaign-plan \
  RANKING_CAMPAIGN_SEED_START=1000001

make ranking-qualification-campaign \
  RANKING_CAMPAIGN_SEED_START=1000001 \
  RANKING_QUALIFICATION_SHARD=baseline-reserved \
  RANKING_OUTPUT=/new/path/baseline-reserved.jsonl
```

The plan command accepts `RANKING_CAMPAIGN_RUN_ID` instead of an explicit seed
start. Run ID `1` receives `[3001, 6001)`, run ID `2` receives `[6001, 9001)`,
and so on. The checked allocation is deterministic and rejects zero or
overflow. Supplying both forms, overlapping the fixed gate, selecting an
unknown shard, or omitting the new output path fails before execution.

Verify a directory containing every solver shard:

```sh
make ranking-qualification-verify \
  RANKING_EVIDENCE_PATH=/path/to/shards \
  RANKING_SUMMARY_OUTPUT=/new/path/qualification-summary.json \
  RANKING_DISPOSITION_OUTPUT=/new/path/qualification-disposition.md
```

Only this complete-bundle verification can report a fleet result. It requires
one common plan, source, environment, seed interval, and scenario/check design;
exactly one copy of every admitted solver; and the complete Cartesian trial
set. It recomputes seed derivation, truth, scenario inputs, classification,
accounting, statistical counts, critical values, and exact checks.

Run verification and exact replay from the recorded source revision with the
recorded Go version, operating system, and architecture. These inputs are part
of the manifest identity, so a different checkout or Go environment is rejected
rather than treated as equivalent evidence.

For a rejection, the summary groups deterministic failures, sampled
rejections, and exact-check rejections. Each group identifies every stored run
and master seed that contributed to it. The optional disposition document
prefills immutable campaign identity and failure references while leaving the
investigation, release impact, corrective action, and review decision to the
maintainer. A permitted sampled error below its prospective critical count is
reported in its cell but is not labelled as a failure.

Replay one stored trial against the matching source:

```sh
make ranking-qualification-replay \
  RANKING_EVIDENCE_PATH=/path/to/baseline-reserved.jsonl \
  RANKING_REPLAY_SCENARIO='endpoint/m4/late/null/w4/p1' \
  RANKING_REPLAY_SOLVER=baseline-reserved \
  RANKING_REPLAY_SEED=1000001
```

Replay compares the complete regenerated record with the stored record. A
mismatch is a source/environment discrepancy or fixture nondeterminism.

## Qualification plan v3

The authoritative plan is `currentQualificationPlan` in
`outliersolver_qualification_test.go`.

| Property | Value |
|---|---|
| Plan ID | `tturl/ranking/outlier-qualification/v3` |
| Driver | `resumable-progress-or-dispatch-indexed-identify-v2` |
| Master seed interval | `[1, 3001)` |
| Runs per solver/scenario | 3,000 |
| Fleet | `baseline-reserved`, `rolling-baseline-reserved`, `peer-first`, `rolling-peer-first`, `baseline-confirmed`, `rolling-baseline-confirmed`, `edge-directed` |
| Scenarios | 14 |
| Sampled checks per solver | 18 |
| Statistical cells | 126 |
| Exact checks per solver | 1 |
| Family-wide false-accept budget | 0.001 |
| Maximum boundary-valid false-reject probability per cell | 0.05 |

The plan contains 11 false-find checks, five false-negative checks, two utility
checks, and one exact exhaustion check per solver. Its scenarios cover serial
and concurrent endpoint cells, biased and contaminated nulls, utility targets,
and same-sign and opposing-sign multiple-target premises.

List the plan to obtain the exact current inventory. The source definition,
not a copied documentation table, is authoritative for scenario IDs and check
membership.

## Driver and experimental protocol

Plan v3 uses deterministic, progress-dependent comparison tranches for
resumable solvers and dispatch-indexed `Identify` for identified one-shot
solvers. Resumable runs are observed only after complete advances, exercising
arbitrary valid decision looks. One-shot runs are observed at their declared
implementation-selected boundary.

The driver is part of the plan identity. A fixed-design or scheduled-look
solver requires a plan and driver that invoke exactly its valid decision
schedule. An uncontrolled solver cannot be qualified as providing calibrated
error guarantees.

Plan validation rejects a missing catalogue solver, duplicate fleet or
scenario entry, unsupported scenario, incompatible inference regime, a solver
that is neither resumable nor an identified one-shot implementation, an invalid
check, an empty family, or seed-interval overflow.

## Acceptance design

For an error-control check, nominal error `q` comes from the scenario request.
For a utility check, the plan supplies an operational unacceptable-decision
budget of `q = 0.25`.

Guarantee checks use a regression margin of 0.04 and utility checks use 0.10.
Each of the 126 sampled cells receives an equal share of the family-wide 0.001
false-accept budget. Before outcomes are examined, the gate chooses the largest
critical error count `k` satisfying:

```text
P[Binomial(N, q + margin) <= k] <= cell false-accept budget
```

The cell passes when its observed error count is at most `k`. The plan also
computes the probability that a boundary-valid rate `q` exceeds `k` and rejects
a design whose false-reject probability is greater than 0.05.

The exact exhaustion check requires an inconclusive result at the comparison
cap with exact comparison and participation accounting on every run. It has no
sampled tolerance.

A passing gate establishes conformance only for the plan's synthetic premises
and regression alternatives. Independent mathematical review continues to own
the solver guarantees and their dependence assumptions.

## Fleet and sharding

The qualification fleet is explicit and separate from
`evaluationSolverCatalogue`.
Adding a solver to the evaluation catalogue therefore cannot change the
statistical family, critical values, or CI release fleet.

CI obtains its matrix from `make ranking-qualification-plan`; solver IDs are
not repeated in workflow YAML. Each shard retains the full plan's 126-cell error
spending. A shard proves only that its solver passed its allocated cells. The
complete CI matrix is the release gate.

Qualification JSONL is streamed in deterministic order rather than retained in
memory. Its manifest records:

- qualification plan and evaluation fixture versions;
- driver, fixed seed interval, run count, and family-wide budgets;
- complete fleet IDs and stable solver parameters;
- selected shard;
- complete planned scenario/check membership;
- expanded scenario definitions;
- a source fingerprint over `ranking`, `stats`, and module or workspace
  dependency files;
- revision state and Go environment.

The terminal `evidence-complete/v1` record binds the manifest and ordered record
stream with SHA-256 digests and the expected record count. Output paths are
never overwritten. Writer failure removes the path; abrupt interruption can
leave only a file without a valid completion record, which verification
rejects.

Pull requests that can affect qualification retain each fixed-gate shard and
their integrated summary and disposition template for 14 days. Other pull
requests report a successful, reviewable skip. Tag-triggered release CI always
runs the complete fixed gate and retains its evidence for 90 days. Evidence
artefact identities include the qualification plan identity, source SHA,
workflow run and attempt, and exact solver ID. Evidence is uploaded even after
a completed statistical rejection; missing output remains an infrastructure
failure.

The weekly workflow runs the same plan over an independently allocated window,
retains every shard, integrated summary, and disposition template for 90 days,
and reports the tightest sampled cell as observed errors versus its critical
count. It is a monitoring control rather than a pull-request or release gate.
Manual dispatch requires the exact seed start to reproduce a prior campaign
window.

### Pull-request impact classification

The repository-owned `tools/ciimpact` classifier runs qualification when a
pull request changes `internal/ranking`, `stats`, module or workspace
dependency files, the root Makefile, the qualification classifier itself, CI
or release workflow ownership, or reviewed release intent. It compares the
complete base/head diff with rename detection disabled, so both sides of a
rename remain visible.
Malformed or empty input requires qualification. Non-pull-request callers,
including the tag-triggered release workflow, always require qualification.

The current local dependency check is:

```sh
go list -test -deps ./internal/ranking
```

It contains only `ranking` and `stats`. Expand the classifier before
qualification begins reading another fixture or depending on another local
package. Its source is the authoritative path policy and its tests document
included and excluded examples.

## Admitting or changing a solver

Evaluation enrolment is necessary but not sufficient. Before adding a solver
variant to the qualification fleet:

1. Review its guarantee, premises, inference regime, valid decision looks, and
   dependence assumptions independently of simulation.
2. Ensure the evaluation catalogue has a faithful deterministic factory,
   capability predicate, result validation, and regime-correct driver.
3. Select the scenarios and classify every proposed check as error-control,
   utility, or exact conformance.
4. Set budgets and meaningful regression margins before inspecting admission
   outcomes.
5. Recalculate family size, cell spending, critical counts, and false-reject
   probabilities; increase the run count if necessary.
6. Advance the qualification plan version.
7. Evaluate the frozen proposal on a prospectively reserved seed window that
   was not used to tune the solver or plan.
8. Admit the solver only after analytic review and the complete proposed fleet
   pass.

Choosing a default solver is a separate comparative decision made with
evaluation campaigns.

## Versioning the plan

Advance the qualification plan when changing its fleet, scenarios, checks,
driver, run count, fixed seed interval, margins, budgets, classifications,
channel semantics, or statistical mathematics. Advance the evaluation fixture
or evidence schema version when their respective recorded meanings change.

Run the complete gate after changes to evidence, stopping, elimination, sign
handling, randomised positioning, controls, concurrency, channel fixtures,
qualification checks, or calibration mathematics.
