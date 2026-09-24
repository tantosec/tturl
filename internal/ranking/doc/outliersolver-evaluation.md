# Outlier solver evaluation

The outlier solver evaluation harness runs interchangeable `OutlierSolver`
variants against reviewed synthetic comparison channels. It supports
exploratory comparison, robustness investigation, and the shared execution
mechanics used by the separate
[qualification gate](outliersolver-qualification.md).

The harness is neutral infrastructure. It does not decide which solvers are
release-supported and an evaluation panel does not enforce sampled error or
utility thresholds.

## Structure and terminology

- The **solver catalogue** contains every solver variant available to the
  harness. A variant has a stable ID, inference regime, stable parameters,
  capability predicate, and fresh-instance factory.
- The **scenario catalogue** contains stable synthetic experiments. A scenario
  defines truth, request, profile, comparison limit, and channel law.
- A **panel** is a named exploratory selection from the scenario catalogue.
- A **trial** is one solver, one scenario, and one master seed.
- A **campaign** is a recorded set of trials over an explicit half-open seed
  interval.
- A **bake-off** is a comparative campaign. It is a use of the evaluation
  harness, not the name of the harness or an acceptance gate.

Scenarios may belong to several panels. Qualification-only scenarios need not
belong to an exploratory panel; the qualification plan references their stable
IDs directly.

## Commands

Run a small smoke comparison with the root Makefile:

```sh
make ranking-evaluate
```

Select a panel, seed interval, solver variants, and scenarios with Make
variables. Solver and scenario selectors are regular expressions:

```sh
make ranking-evaluate \
  RANKING_PANEL=robustness \
  RANKING_RUNS=50 \
  RANKING_SEED_START=10001 \
  RANKING_SOLVER='baseline-reserved|peer-first' \
  RANKING_SCENARIO='either|peer-only'
```

Write canonical JSONL to a new path with `RANKING_OUTPUT`. Existing files are
never overwritten:

```sh
make ranking-evaluate \
  RANKING_PANEL=core-comparison \
  RANKING_RUNS=200 \
  RANKING_OUTPUT=/new/path/ranking-evaluation.jsonl
```

List selected inputs without running trials:

```sh
go test ./internal/ranking -run '^TestOutlierSolverEvaluation$' -count=1 -v \
  -args -evaluation.list -evaluation.panel=robustness
```

The direct interface is `TestOutlierSolverEvaluation` with these flags:

| Flag | Meaning |
|---|---|
| `-evaluation.panel` | Named exploratory panel |
| `-evaluation.runs` | Number of consecutive master seeds |
| `-evaluation.seed` | First master seed |
| `-evaluation.solver` | Solver-ID regular expression |
| `-evaluation.scenario` | Scenario-ID regular expression |
| `-evaluation.output` | New JSONL output path |
| `-evaluation.list` | List selected inputs and exit |

These flags intentionally cannot enable qualification.

## Panels

| Panel | Purpose |
|---|---|
| `smoke` | Quick execution and gross-behaviour comparison |
| `core-comparison` | Broad quality and efficiency comparison |
| `robustness` | Direction, scale, width, controls, bias, noise, and premise stress |
| `request-policy` | Minimum-edge and requested-risk sensitivity |
| `concurrency` | Atomic-wave and bounded-concurrency behaviour |

Panels report evidence. Their test fails only for inability to execute the
experiment faithfully: solver errors, invalid results, or accounting errors.
Outcome rates and comparative summaries require interpretation in light of the
scenario premise.

## Fairness and reproducibility

For each master seed, the harness derives separate environment and solver
seeds. All selected solvers receive the same environment seed and common
solver-side seed for a scenario. Environment draws are keyed by stable
comparison input and occurrence count, so callback arrival order does not
assign randomness.

Each trial receives fresh solver, comparator, and channel state. Solver order,
scenario order, filtering, and concurrent callback scheduling therefore do not
change another trial's random stream.

The ordinary exploratory driver invokes `OutlierSolver.Identify` once. The
qualification plan owns its different resumable driver explicitly; selecting a
panel cannot change the driver.

## Outcomes and comparisons

The harness classifies decisions as correct, false-find, wrong-item,
wrong-sign, false-negative, subedge, unscored, inconclusive, unsupported, or a
structural error. Unscored outcomes identify scenarios whose synthetic model
does not establish the relevant guarantee premise.

Paired reports compare a candidate with the first selected solver using matched
trial inputs. They report quality wins/ties/losses, same-decision cost
wins/ties/losses, acceptable-rate differences, and paired cost intervals.
There is deliberately no global score: the importance of safety, work, signs,
controls, population size, and operating regime belongs to the decision being
made.

The benchmark front end provides compact Go benchmark output. Fix `b.N`; Go's
adaptive benchmark duration is not a statistical replicate policy:

```sh
go test ./internal/ranking -run '^$' -bench '^BenchmarkOutlierSolverEvaluation$' \
  -benchtime=20x -count=1 \
  -args -evaluation.panel=core-comparison
```

`ns/op` is suppressed because simulator wall time is not channel cost.

## Evidence format

An evaluation JSONL file begins with an `evaluation-meta/v2` manifest, followed
by `evaluation-run/v1` trial records and one `evidence-complete/v1` record. The
manifest records the evaluation fixture version, selected panel, source
fingerprint, Go environment, seed interval, solver parameters, and expanded
scenarios. Each trial records both derived seeds, truth, request, profile,
channel, result, classification, sampling, and abstract cost. The completion
record binds the manifest and ordered trial stream with SHA-256 digests and an
exact record count. A file without a valid completion record is partial, not
campaign evidence.

Changing seed derivation, channel semantics, scenario definitions, solver
metadata, classifications, record fields, or manifest fields requires the
corresponding fixture or schema version to change.

## Enrolling a solver for evaluation

Add a stable entry to `evaluationSolverCatalogue` in
`outliersolver_evaluation_fixture_test.go`:

1. Choose an ID for the enduring solver variant, not a temporary experiment or
   performance claim.
2. Record stable parameters that distinguish the variant.
3. Return a fresh solver for every trial and inject its supplied deterministic
   seed into all solver-side randomness.
4. Declare unsupported scenarios through `Supports`; never modify a scenario
   silently to make the solver run.
5. Add catalogue, factory, result-contract, and representative execution tests.
6. Confirm that the harness driver and result validator support the solver's
   inference regime. Add a regime-correct driver before enrolling a solver that
   cannot be called through `Identify` at its valid decision boundary.

Evaluation enrolment does not admit the solver to the qualification fleet.
Follow the separate qualification admission process before treating its
guarantees as release-supported.

## Material comparative decisions

Choose the panel, solvers, scenarios, metrics, seed interval, and decision rule
before inspecting outcomes. Preserve the raw JSONL and record exceptions. Use
`core-comparison` for the main quality/cost comparison and add the narrower
panels relevant to the claim being considered.

Selection of a preferred solver and qualification admission are independent. A
qualified solver need not be the default, and an experimental solver may be
compared without changing the release gate.
