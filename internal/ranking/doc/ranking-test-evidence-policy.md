# Ranking test evidence policy

Ranking combines deterministic contracts with probabilistic behaviour. Its test
layers therefore have different purposes and different authority. Evidence is
interpreted according to the layer that produced it.

The [evaluation manual](outliersolver-evaluation.md) describes exploratory
execution. The [qualification specification](outliersolver-qualification.md)
defines the enforcing release gate.

## Evidence layers

### Ordinary tests

`go test ./internal/ranking` owns deterministic API contracts, invariants, boundaries,
regressions, and fixed-seed path coverage. A decision assertion must control
solver and channel randomness. These tests do not estimate an error rate or
detection power.

Run `go test -race ./internal/ranking` after changes to concurrency or reachable shared
state.

### Exploratory evaluation

`make ranking-evaluate` runs a named panel over an explicit seed interval. It
supports comparison, diagnosis, robustness investigation, and prospective
solver-selection studies. Panel outcomes are reports, not automatic quality
gates.

Material decisions declare the panel, solvers, scenarios, metrics, seed
interval, and decision rule before outcomes are inspected. Preserve canonical
JSONL for the decision record.

### Fixed qualification gate

`make ranking-qualify` runs the versioned qualification plan. The plan fixes its
fleet, scenarios, checks, driver, run count, seed interval, margins, and
family-wide statistical spending. This is the only sampled release-acceptance
gate supplied by the package.

The same fixed window is intentionally reused across source revisions to detect
implementation regressions. A sampled rejection is evidence to investigate,
not an invitation to choose a different seed or rerun until green.

### Independent qualification campaigns

An independent campaign exercises a frozen qualification proposal over a seed
window selected before outcomes are observed. Its window is disjoint from the
fixed gate and from other campaigns for the same plan. Such campaigns support
solver admission, material plan changes, and independent confirmation; they do
not silently replace the fixed gate.

Independent campaigns use `make ranking-qualification-campaign`. The caller
declares an explicit seed start or an immutable allocation ID before execution;
the command preserves every other plan input.

The weekly `ranking qualification weekly` workflow derives its interval from
the immutable GitHub workflow run ID. Different scheduled runs receive
non-overlapping 3,000-seed windows above the fixed gate; rerunning one workflow
run reuses its original interval. A manual dispatch requires an explicit seed
start and is an exact-window replay, not replacement evidence. Weekly JSONL and
its integrated summary are retained for 90 days.

Scheduled qualification is a monitoring and discovery control, not a required
pull-request status. Repository maintainers own failure triage through native
failed-workflow notifications. A complete rejection remains visible and does
not trigger an automatic retry. It does not retroactively invalidate a release,
but its disposition can block the next release or require corrective action.

## Lifecycle

| Event | Required evidence | Decision authority |
|---|---|---|
| Ordinary ranking change | `go test ./internal/ranking`; race test when applicable | Deterministic correctness |
| Ranking behaviour or evidence change | Ordinary tests and `make ranking-qualify` | Merge acceptance |
| Pull request affecting qualification | Repository CI, including all qualification shards | Required CI status |
| Other pull request | Repository CI with a reviewable qualification skip | Required CI status |
| Tagged release | Reused CI on the immutable tag commit | Publication dependency |
| Weekly schedule | Independent rotating qualification campaign | Monitoring and discovery |
| Solver evaluation enrolment | Catalogue and representative execution tests | Eligibility for exploratory use |
| Solver qualification admission | Analytic review, frozen proposed plan, and independent campaign | Release-supported fleet membership |
| Preferred-solver change | Prospective comparative campaign plus qualification | Default selection |

CI runs qualification for pull requests whose complete diff can affect ranking
code, its statistical dependencies, the qualification harness, toolchain or
workflow ownership, or release intent. The repository-owned classifier fails
closed on malformed or empty input. Other pull requests report an explicit
successful skip and continue to run the ordinary repository gates. Every tag
release runs qualification unconditionally.

When required, CI derives the shard matrix from the qualification plan, runs
the gate once on the consumer-floor Go toolchain, verifies the complete fleet
bundle, and retains canonical JSONL for 14 days on pull requests and 90 days on
releases.

## When qualification is required locally

Run `make ranking-qualify` after changing:

- evidence calculation or decision thresholds;
- stopping, resumption, elimination, or sign handling;
- randomised positioning or solver randomness;
- controls, profiles, concurrency, or atomic-wave execution;
- a qualification scenario or channel fixture;
- result classification, cost accounting, or evidence records;
- qualification checks, calibration mathematics, or the plan; or
- a qualified solver's dependencies.

Documentation-only changes need ordinary repository validation unless they
change the stated contract or reveal that implementation and policy disagree.

## Seed rules

Ordinary tests use explicit deterministic seeds when outcomes depend on
randomness. Exploratory evaluations always record an explicit seed start and
run count. Their seeds may be selected for diagnosis, but a reported material
decision must identify any prior use of the same window.

The fixed qualification seed interval belongs to the plan and cannot be
changed from its command line. Independent campaign windows are declared
before execution and remain associated with all results, including rejections.

## Failure handling

Classify a failure before repeating it:

- A deterministic contract, solver, invalid-result, or accounting failure is a
  correctness failure. Reproduce the exact trial and fix the defect.
- A sampled qualification cell above its critical count is a gate rejection.
  Preserve the source, plan, shard, and complete output; inspect individual
  trials and aggregate behaviour without changing seeds or thresholds.
- An infrastructure interruption is a run that produced no complete decision
  evidence. It may be repeated with identical source, plan, and seeds.
- An exploratory outcome is not a CI flake merely because a solver performed
  poorly. Interpret it under the campaign's declared purpose.

Do not add retries, widen margins, lower run counts, remove scenarios, or rotate
seeds in response to an unexplained qualification rejection. A justified
contract or plan change is prospective, versioned, analytically reviewed, and
validated as a new design.

`make ranking-qualification-verify` distinguishes invalid evidence,
deterministic failures, sampled rejections, and exact-check rejections. Its
machine-readable summary identifies the rejecting cells and contributing
seeds. Use `RANKING_DISPOSITION_OUTPUT` to create a review template, and use
`make ranking-qualification-replay` with the original shard, scenario, solver,
and seed to reproduce an exact trial. A replay mismatch indicates different
source or environment inputs, or nondeterminism in the fixture; it is not a
replacement result.

## Evidence records

Canonical evaluation and qualification evidence is JSONL containing a manifest,
complete trial records, and a cryptographic completion record. The manifest
identifies the source, fixture or plan version, environment, solver selection,
scenarios, and seed interval. The completion record binds the manifest and
ordered trial stream. Missing, truncated, duplicated, mixed, or noncanonical
evidence does not constitute a completed campaign.

Console summaries are diagnostic views, not substitutes for raw evidence.
Material local campaigns must choose and preserve an output path explicitly;
CI owns the short-term retention periods stated above.

Long-lived design decisions belong in versioned documentation and source:
statistical premises, qualification plans, material waivers, solver admission,
and preferred-solver rationale. Raw trial evidence supports those decisions
but does not replace them.
