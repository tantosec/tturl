package ranking

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"reflect"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	qualificationPlanID           = "tturl/ranking/outlier-qualification/v3"
	qualificationMetaSchema       = "tturl/ranking/qualification-meta/v4"
	qualificationLegacyMetaSchema = "tturl/ranking/qualification-meta/v3"
	qualificationDriverID         = "resumable-progress-or-" +
		"dispatch-indexed-identify-v2"
	qualificationIdentifyDriverID  = "one-shot-dispatch-indexed-v2"
	rollingAdmissionPlanID         = "tturl/ranking/rolling-outlier-admission/v2"
	rollingSiblingsAdmissionPlanID = "tturl/ranking/rolling-siblings-admission/v1"
	qualificationRuns              = 3_000
	qualificationSeedStart         = uint64(1)
	qualificationFalseAccept       = 0.001
	qualificationFalseReject       = 0.05
	qualificationGuaranteeMargin   = 0.04
)

var (
	qualificationShardFlag = flag.String(
		"qualification.shard", "",
		"exact solver ID to qualify, or all for the complete fleet",
	)
	qualificationOutputFlag = flag.String(
		"qualification.output", "",
		"new JSONL path for raw qualification evidence",
	)
	qualificationListFlag = flag.Bool(
		"qualification.list", false,
		"list the qualification plan without running trials",
	)
	qualificationPlanFlag = flag.String(
		"qualification.plan", "release",
		"qualification plan: release, rolling-admission, or rolling-siblings-admission",
	)
	qualificationCampaignSeedFlag = flag.String(
		"qualification-campaign.seed", "",
		"required first master seed for an independent qualification campaign",
	)
	qualificationCampaignRunIDFlag = flag.String(
		"qualification-campaign.run-id", "",
		"GitHub workflow run ID from which to allocate a campaign seed window",
	)
	qualificationCampaignShardFlag = flag.String(
		"qualification-campaign.shard", "",
		"exact solver ID for an independent campaign, or all",
	)
	qualificationCampaignOutputFlag = flag.String(
		"qualification-campaign.output", "",
		"required new JSONL path for independent campaign evidence",
	)
	qualificationCampaignListFlag = flag.Bool(
		"qualification-campaign.list", false,
		"list an independent campaign plan without running trials",
	)
)

type qualificationCampaignKind string

const (
	qualificationFixedGate   qualificationCampaignKind = "fixed-gate"
	qualificationIndependent qualificationCampaignKind = "independent"
)

type qualificationCheckKind string

const (
	qualificationFalseFind     qualificationCheckKind = "false-find"
	qualificationFalseNegative qualificationCheckKind = "false-negative"
	qualificationUtility       qualificationCheckKind = "unacceptable-decision"
	qualificationExhaustion    qualificationCheckKind = "budget-exhaustion"
)

type qualificationCheck struct {
	Kind   qualificationCheckKind `json:"kind"`
	Budget float64                `json:"budget,omitempty"`
	Margin float64                `json:"margin,omitempty"`
}

type qualificationScenario struct {
	Name   string               `json:"name"`
	Checks []qualificationCheck `json:"checks"`
}

type qualificationPlan struct {
	ID        string                  `json:"id"`
	Driver    string                  `json:"driver"`
	Runs      int                     `json:"runs"`
	SeedStart uint64                  `json:"seed_start"`
	Fleet     []string                `json:"fleet"`
	Scenarios []qualificationScenario `json:"scenarios"`
}

func currentQualificationPlan() qualificationPlan {
	find := qualificationCheck{
		Kind: qualificationFalseFind, Margin: qualificationGuaranteeMargin,
	}
	negative := qualificationCheck{
		Kind: qualificationFalseNegative, Margin: qualificationGuaranteeMargin,
	}
	utility := qualificationCheck{
		Kind: qualificationUtility, Budget: 0.25, Margin: 0.10,
	}
	return qualificationPlan{
		ID: qualificationPlanID, Driver: qualificationDriverID,
		Runs: qualificationRuns, SeedStart: qualificationSeedStart,
		Fleet: []string{
			"baseline-reserved",
			"rolling-baseline-reserved",
			"peer-first",
			"rolling-peer-first",
			"baseline-confirmed",
			"rolling-baseline-confirmed",
			"edge-directed",
		},
		Scenarios: []qualificationScenario{
			{Name: "endpoint/m4/late/null/w4/p1", Checks: []qualificationCheck{find}},
			{Name: "endpoint/m4/late/boundary/w4/p1", Checks: []qualificationCheck{find, negative}},
			{Name: "endpoint/m4/either/null/w4/p1", Checks: []qualificationCheck{find}},
			{Name: "endpoint/m4/either/boundary/w4/p1", Checks: []qualificationCheck{find, negative}},
			{Name: "gaussian/m8/late/null/stepped/w4/p1", Checks: []qualificationCheck{find}},
			{Name: "gaussian/m8/early/null/heteroskedastic-reversed/w4/p1", Checks: []qualificationCheck{find}},
			{Name: "gaussian/m8/either/null/one-sided-linear-control1/w4/p1", Checks: []qualificationCheck{find}},
			{Name: "gaussian/m16/late/strong/stepped/w4/p1", Checks: []qualificationCheck{utility}},
			{Name: "gaussian/m16/either/strong/one-sided-reversed-control1/w4/p1", Checks: []qualificationCheck{utility}},
			{Name: "endpoint/m4/late/null/w4/p4", Checks: []qualificationCheck{find}},
			{Name: "endpoint/m4/late/boundary/w4/p4", Checks: []qualificationCheck{find, negative}},
			{Name: "endpoint/m4/late/null/exhausted/w4/p4", Checks: []qualificationCheck{{Kind: qualificationExhaustion}}},
			{Name: "gaussian/m8/either/multi2-late-boundary/baseline/w4/p1", Checks: []qualificationCheck{find, negative}},
			{Name: "gaussian/m8/either/multi2-opposed-boundary/baseline/w4/p1", Checks: []qualificationCheck{find, negative}},
		},
	}
}

func rollingAdmissionPlan() qualificationPlan {
	plan := currentQualificationPlan()
	plan.ID = rollingAdmissionPlanID
	plan.Driver = qualificationIdentifyDriverID
	plan.SeedStart = qualificationSeedStart + uint64(qualificationRuns)
	plan.Fleet = []string{"rolling-baseline-reserved"}
	return plan
}

func rollingSiblingsAdmissionPlan() qualificationPlan {
	plan := currentQualificationPlan()
	plan.ID = rollingSiblingsAdmissionPlanID
	plan.Driver = qualificationIdentifyDriverID
	plan.SeedStart = qualificationSeedStart + 3*uint64(qualificationRuns)
	plan.Fleet = []string{
		"rolling-peer-first",
		"rolling-baseline-confirmed",
	}
	return plan
}

func selectedQualificationPlan() (qualificationPlan, error) {
	switch *qualificationPlanFlag {
	case "release":
		return currentQualificationPlan(), nil
	case "rolling-admission":
		return rollingAdmissionPlan(), nil
	case "rolling-siblings-admission":
		return rollingSiblingsAdmissionPlan(), nil
	default:
		return qualificationPlan{}, fmt.Errorf(
			"qualification plan must be release, rolling-admission, or "+
				"rolling-siblings-admission; got %q",
			*qualificationPlanFlag)
	}
}

type calibrationCellPlan struct {
	Runs              int
	NominalBudget     float64
	RegressionMargin  float64
	FalseAcceptBudget float64
	CriticalErrors    int
	FalseRejectRate   float64
}

type qualificationManifest struct {
	Schema                 string                       `json:"schema"`
	FixtureVersion         string                       `json:"fixture_version"`
	PlanID                 string                       `json:"plan_id"`
	Campaign               qualificationCampaignKind    `json:"campaign"`
	Driver                 string                       `json:"driver"`
	Runs                   int                          `json:"runs"`
	SeedStart              uint64                       `json:"seed_start"`
	SeedEnd                uint64                       `json:"seed_end"`
	FalseAcceptBudget      float64                      `json:"false_accept_budget"`
	CellFalseAcceptBudget  float64                      `json:"cell_false_accept_budget"`
	FalseRejectBudget      float64                      `json:"false_reject_budget"`
	StatisticalCells       int                          `json:"statistical_cells"`
	Fleet                  []string                     `json:"fleet"`
	FleetSolvers           []evaluationSolverManifest   `json:"fleet_solvers"`
	Shard                  []string                     `json:"shard"`
	QualificationScenarios []qualificationScenario      `json:"qualification_scenarios"`
	GoVersion              string                       `json:"go_version"`
	GOOS                   string                       `json:"goos"`
	GOARCH                 string                       `json:"goarch"`
	Revision               string                       `json:"revision,omitempty"`
	Dirty                  bool                         `json:"dirty,omitempty"`
	SourceSHA256           string                       `json:"source_sha256"`
	Solvers                []evaluationSolverManifest   `json:"solvers"`
	Scenarios              []evaluationScenarioManifest `json:"scenarios"`
}

func (p qualificationPlan) statisticalCells() int {
	checks := 0
	for _, scenario := range p.Scenarios {
		for _, check := range scenario.Checks {
			if check.Kind != qualificationExhaustion {
				checks++
			}
		}
	}
	return checks * len(p.Fleet)
}

func (p qualificationPlan) validate(
	catalogue []evaluationScenario, solvers []evaluationSolver,
) error {
	if p.ID == "" ||
		(p.Driver != qualificationDriverID &&
			p.Driver != qualificationIdentifyDriverID) || p.Runs < 1 ||
		len(p.Fleet) == 0 || len(p.Scenarios) == 0 {
		return fmt.Errorf("qualification plan has incomplete identity or design")
	}
	if _, err := evaluationSeedEnd(p.SeedStart, p.Runs); err != nil {
		return fmt.Errorf("qualification seed interval: %w", err)
	}
	scenarioByName := make(map[string]evaluationScenario, len(catalogue))
	for _, scenario := range catalogue {
		scenarioByName[scenario.Name] = scenario
	}
	solverByName := make(map[string]evaluationSolver, len(solvers))
	for _, solver := range solvers {
		solverByName[solver.Name] = solver
	}
	seenFleet := make(map[string]bool, len(p.Fleet))
	for _, name := range p.Fleet {
		solver, ok := solverByName[name]
		if !ok {
			return fmt.Errorf("qualification fleet solver %q is not in the catalogue", name)
		}
		if seenFleet[name] {
			return fmt.Errorf("qualification fleet repeats solver %q", name)
		}
		seenFleet[name] = true
		if solver.Inference != OutlierInferenceAnytimeValid {
			return fmt.Errorf("qualification solver %q is not anytime-valid", name)
		}
		instance := solver.New(evaluationSolverConfig{
			Seed: 1, MaxComparisons: 1, MaxConcurrency: 1,
		})
		if _, resumable := instance.(ResumableOutlierSolver); !resumable {
			if _, identified := instance.(IdentifiedOutlierSolver); !identified {
				return fmt.Errorf(
					"qualification solver %q supports neither resumable nor identified one-shot driving",
					name)
			}
		}
	}
	seenScenarios := make(map[string]bool, len(p.Scenarios))
	for _, planned := range p.Scenarios {
		scenario, ok := scenarioByName[planned.Name]
		if !ok {
			return fmt.Errorf("qualification scenario %q is not in the catalogue", planned.Name)
		}
		if seenScenarios[planned.Name] {
			return fmt.Errorf("qualification plan repeats scenario %q", planned.Name)
		}
		seenScenarios[planned.Name] = true
		if len(planned.Checks) == 0 {
			return fmt.Errorf("qualification scenario %q has no checks", planned.Name)
		}
		seenChecks := make(map[qualificationCheckKind]bool, len(planned.Checks))
		for _, check := range planned.Checks {
			if err := validateQualificationCheck(scenario, check); err != nil {
				return err
			}
			if seenChecks[check.Kind] {
				return fmt.Errorf("qualification scenario %q repeats check %q",
					planned.Name, check.Kind)
			}
			seenChecks[check.Kind] = true
		}
		for _, name := range p.Fleet {
			solver := solverByName[name]
			if solver.Supports != nil {
				if reason := solver.Supports(scenario); reason != "" {
					return fmt.Errorf("qualification solver %q does not support scenario %q: %s",
						name, scenario.Name, reason)
				}
			}
		}
	}
	if p.statisticalCells() == 0 {
		return fmt.Errorf("qualification plan has no statistical cells")
	}
	return nil
}

func validateQualificationCheck(
	scenario evaluationScenario, check qualificationCheck,
) error {
	switch check.Kind {
	case qualificationFalseFind:
		if check.Budget != 0 {
			return fmt.Errorf("scenario %q false-find budget comes from its request",
				scenario.Name)
		}
	case qualificationFalseNegative:
		if len(scenario.Targets) == 0 || !scenario.hasQualifyingTarget() {
			return fmt.Errorf("scenario %q has no qualifying false-negative premise",
				scenario.Name)
		}
		if check.Budget != 0 {
			return fmt.Errorf("scenario %q false-negative budget comes from its request",
				scenario.Name)
		}
	case qualificationUtility:
		if len(scenario.Targets) == 0 || check.Budget <= 0 || check.Budget >= 1 {
			return fmt.Errorf("scenario %q has invalid utility check", scenario.Name)
		}
	case qualificationExhaustion:
		if check.Budget != 0 || check.Margin != 0 {
			return fmt.Errorf("scenario %q exhaustion check has statistical parameters",
				scenario.Name)
		}
		return nil
	default:
		return fmt.Errorf("scenario %q has unknown qualification check %q",
			scenario.Name, check.Kind)
	}
	nominal := qualificationNominalBudget(scenario, check)
	if check.Margin <= 0 || nominal+check.Margin >= 1 {
		return fmt.Errorf("scenario %q has invalid qualification margin %v",
			scenario.Name, check.Margin)
	}
	return nil
}

func qualificationNominalBudget(
	scenario evaluationScenario, check qualificationCheck,
) float64 {
	switch check.Kind {
	case qualificationFalseNegative:
		return scenario.FalseNegativeRisk
	case qualificationUtility:
		return check.Budget
	default:
		return scenario.FalseFindRisk
	}
}

func qualificationInputs(
	plan qualificationPlan, shard string,
) ([]evaluationScenario, []evaluationSolver, error) {
	catalogue := evaluationScenarioCatalogue()
	solverCatalogue := evaluationSolverCatalogue()
	if err := validateEvaluationScenarioCatalogue(catalogue); err != nil {
		return nil, nil, err
	}
	if err := validateEvaluationSolvers(solverCatalogue); err != nil {
		return nil, nil, err
	}
	if err := plan.validate(catalogue, solverCatalogue); err != nil {
		return nil, nil, err
	}
	scenarioByName := make(map[string]evaluationScenario, len(catalogue))
	for _, scenario := range catalogue {
		scenarioByName[scenario.Name] = scenario
	}
	solverByName := make(map[string]evaluationSolver, len(solverCatalogue))
	for _, solver := range solverCatalogue {
		solverByName[solver.Name] = solver
	}
	scenarios := make([]evaluationScenario, 0, len(plan.Scenarios))
	for _, planned := range plan.Scenarios {
		scenarios = append(scenarios, scenarioByName[planned.Name])
	}
	if shard == "all" {
		solvers := make([]evaluationSolver, 0, len(plan.Fleet))
		for _, name := range plan.Fleet {
			solvers = append(solvers, solverByName[name])
		}
		return scenarios, solvers, nil
	}
	if !slices.Contains(plan.Fleet, shard) {
		return nil, nil, fmt.Errorf("qualification shard %q is not in fleet: %v",
			shard, plan.Fleet)
	}
	return scenarios, []evaluationSolver{solverByName[shard]}, nil
}

type qualificationCellKey struct {
	Scenario string
	Solver   string
}

type qualificationCellResult struct {
	Records          int
	StructuralErrors int
	FirstStructural  string
	CheckErrors      map[qualificationCheckKind]int
	StructuralRuns   []qualificationFailureRecord
	CheckErrorRuns   map[qualificationCheckKind][]qualificationFailureRecord
}

type qualificationFailureRecord struct {
	Run            int                      `json:"run"`
	Seed           uint64                   `json:"seed"`
	Classification evaluationClassification `json:"classification"`
	Error          string                   `json:"error,omitempty"`
}

type qualificationResults map[qualificationCellKey]*qualificationCellResult

func newQualificationResults(
	plan qualificationPlan, scenarios []evaluationScenario,
	solvers []evaluationSolver, captureFailureRuns bool,
) qualificationResults {
	results := make(qualificationResults, len(scenarios)*len(solvers))
	for _, scenario := range scenarios {
		for _, solver := range solvers {
			checks := make(map[qualificationCheckKind]int)
			var checkRuns map[qualificationCheckKind][]qualificationFailureRecord
			if captureFailureRuns {
				checkRuns = make(map[qualificationCheckKind][]qualificationFailureRecord)
			}
			for _, planned := range plan.Scenarios {
				if planned.Name == scenario.Name {
					for _, check := range planned.Checks {
						checks[check.Kind] = 0
						if captureFailureRuns {
							checkRuns[check.Kind] = nil
						}
					}
				}
			}
			results[qualificationCellKey{scenario.Name, solver.Name}] = &qualificationCellResult{
				CheckErrors: checks, CheckErrorRuns: checkRuns,
			}
		}
	}
	return results
}

func (r qualificationResults) add(record evaluationRunRecord) {
	cell := r[qualificationCellKey{record.Scenario, record.Solver}]
	cell.Records++
	failure := qualificationFailureRecord{
		Run: record.Run, Seed: record.Seed,
		Classification: record.Classification, Error: record.Error,
	}
	if record.Classification == evaluationSolverError ||
		record.Classification == evaluationAccountingError ||
		record.Classification == evaluationInvalidResult ||
		record.Classification == evaluationUnsupported {
		cell.StructuralErrors++
		if cell.FirstStructural == "" {
			cell.FirstStructural = fmt.Sprintf("run=%d %s: %s", record.Run,
				record.Classification, record.Error)
		}
		if cell.CheckErrorRuns != nil {
			cell.StructuralRuns = append(cell.StructuralRuns, failure)
		}
	}
	for kind := range cell.CheckErrors {
		if qualificationRecordIsError(kind, record) {
			cell.CheckErrors[kind]++
			if cell.CheckErrorRuns != nil {
				cell.CheckErrorRuns[kind] = append(cell.CheckErrorRuns[kind], failure)
			}
		}
	}
}

func runQualification(
	ctx context.Context, scenarios []evaluationScenario, solvers []evaluationSolver,
	plan qualificationPlan, seedStart uint64, writer *evidenceWriter,
) (qualificationResults, error) {
	results := newQualificationResults(plan, scenarios, solvers, false)
	for _, scenario := range scenarios {
		for run := range plan.Runs {
			for _, solver := range solvers {
				driver := qualificationEvaluationDriver(plan, solver)
				record := runEvaluationTrialWithDriver(
					ctx, solver, scenario, seedStart, run, driver)
				results.add(record)
				if writer != nil {
					if err := writer.Write(record); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	return results, nil
}

func qualificationEvaluationDriver(
	plan qualificationPlan, solver evaluationSolver,
) evaluationDriver {
	if plan.Driver == qualificationIdentifyDriverID {
		return evaluationIdentifyDriver
	}
	instance := solver.New(evaluationSolverConfig{
		Seed: 1, MaxComparisons: 1, MaxConcurrency: 1,
	})
	if _, ok := instance.(ResumableOutlierSolver); !ok {
		return evaluationIdentifyDriver
	}
	return evaluationResumableDriver
}

func buildQualificationManifest(
	plan qualificationPlan, scenarios []evaluationScenario,
	solvers []evaluationSolver, campaign qualificationCampaignKind,
	seedStart uint64,
) (qualificationManifest, error) {
	evaluation, err := buildEvaluationManifest(
		"", scenarios, solvers, seedStart, plan.Runs)
	if err != nil {
		return qualificationManifest{}, err
	}
	shard := make([]string, len(solvers))
	for i, solver := range solvers {
		shard[i] = solver.Name
	}
	solverByName := make(map[string]evaluationSolver, len(evaluationSolverCatalogue()))
	for _, solver := range evaluationSolverCatalogue() {
		solverByName[solver.Name] = solver
	}
	fleetSolvers := make([]evaluationSolverManifest, 0, len(plan.Fleet))
	for _, name := range plan.Fleet {
		solver := solverByName[name]
		fleetSolvers = append(fleetSolvers, evaluationSolverManifest{
			Name: solver.Name, Inference: solver.Inference.String(), Params: solver.Params,
		})
	}
	manifest := qualificationManifest{
		Schema: qualificationMetaSchema, FixtureVersion: evaluationFixtureVersion,
		PlanID: plan.ID, Campaign: campaign, Driver: plan.Driver,
		Runs: plan.Runs, SeedStart: seedStart, SeedEnd: evaluation.SeedEnd,
		FalseAcceptBudget: qualificationFalseAccept,
		CellFalseAcceptBudget: qualificationFalseAccept /
			float64(plan.statisticalCells()),
		FalseRejectBudget: qualificationFalseReject,
		StatisticalCells:  plan.statisticalCells(), Fleet: slices.Clone(plan.Fleet),
		FleetSolvers: fleetSolvers, Shard: shard,
		QualificationScenarios: slices.Clone(plan.Scenarios),
		GoVersion:              runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		SourceSHA256: evaluation.SourceSHA256,
		Solvers:      evaluation.Solvers, Scenarios: evaluation.Scenarios,
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				manifest.Revision = setting.Value
			case "vcs.modified":
				manifest.Dirty = setting.Value == "true"
			}
		}
	}
	return manifest, nil
}

func qualificationErrors(
	kind qualificationCheckKind, records []evaluationRunRecord,
) int {
	errors := 0
	for _, record := range records {
		if qualificationRecordIsError(kind, record) {
			errors++
		}
	}
	return errors
}

func qualificationRecordIsError(
	kind qualificationCheckKind, record evaluationRunRecord,
) bool {
	switch kind {
	case qualificationFalseFind:
		return record.Classification == evaluationFalseFind ||
			record.Classification == evaluationWrongItem ||
			record.Classification == evaluationWrongSign
	case qualificationFalseNegative:
		return record.Classification == evaluationFalseNegative
	case qualificationUtility:
		return record.Classification != evaluationCorrectFound
	case qualificationExhaustion:
		return record.Classification != evaluationInconclusive ||
			record.Result.Comparisons != record.MaxComparisons ||
			record.Result.Participations !=
				record.MaxComparisons*record.Profile.MaxWidth
	default:
		return true
	}
}

func checkQualification(
	t *testing.T, plan qualificationPlan, scenarios []evaluationScenario,
	solvers []evaluationSolver, results qualificationResults,
) {
	t.Helper()
	falseAccept := qualificationFalseAccept / float64(plan.statisticalCells())
	checksByScenario := make(map[string][]qualificationCheck, len(plan.Scenarios))
	for _, scenario := range plan.Scenarios {
		checksByScenario[scenario.Name] = scenario.Checks
	}
	for _, scenario := range scenarios {
		for _, solver := range solvers {
			result := results[qualificationCellKey{scenario.Name, solver.Name}]
			if result.StructuralErrors != 0 {
				t.Errorf("qualification scenario=%s solver=%s structural-errors=%d first=%s",
					scenario.Name, solver.Name, result.StructuralErrors,
					result.FirstStructural)
			}
		}
	}
	for _, scenario := range scenarios {
		for _, check := range checksByScenario[scenario.Name] {
			if check.Kind == qualificationExhaustion {
				for _, solver := range solvers {
					result := results[qualificationCellKey{scenario.Name, solver.Name}]
					errors := result.CheckErrors[check.Kind]
					t.Logf("qualification scenario=%s check=%s solver=%s "+
						"errors=%d/%d required=0",
						scenario.Name, check.Kind, solver.Name, errors, result.Records)
					if errors != 0 {
						t.Errorf("scenario=%s check=%s solver=%s errors=%d, want 0",
							scenario.Name, check.Kind, solver.Name, errors)
					}
				}
				continue
			}
			nominal := qualificationNominalBudget(scenario, check)
			cell, err := newCalibrationCellPlan(
				plan.Runs, nominal, check.Margin, falseAccept)
			if err != nil {
				t.Fatalf("scenario=%s calibration cell: %v", scenario.Name, err)
			}
			if cell.FalseRejectRate > qualificationFalseReject {
				t.Errorf("scenario=%s check=%s false-reject probability %.4f "+
					"exceeds %.4f", scenario.Name, check.Kind,
					cell.FalseRejectRate, qualificationFalseReject)
				continue
			}
			for _, solver := range solvers {
				result := results[qualificationCellKey{scenario.Name, solver.Name}]
				errors := result.CheckErrors[check.Kind]
				t.Logf("qualification scenario=%s check=%s solver=%s "+
					"errors=%d/%d critical=%d nominal=%.4f regression=%.4f "+
					"false-accept<=%.6f false-reject=%.4f",
					scenario.Name, check.Kind, solver.Name, errors, result.Records,
					cell.CriticalErrors, nominal, nominal+check.Margin,
					falseAccept, cell.FalseRejectRate)
				if errors > cell.CriticalErrors {
					t.Errorf("scenario=%s check=%s solver=%s errors=%d "+
						"exceed critical=%d", scenario.Name, check.Kind,
						solver.Name, errors, cell.CriticalErrors)
				}
			}
		}
	}
}

func TestOutlierSolverQualification(t *testing.T) {
	if *qualificationShardFlag == "" && !*qualificationListFlag {
		t.Skip("set -qualification.shard to run the release qualification gate")
	}
	plan, err := selectedQualificationPlan()
	if err != nil {
		t.Fatal(err)
	}
	if *qualificationListFlag {
		if err := plan.validate(evaluationScenarioCatalogue(), evaluationSolverCatalogue()); err != nil {
			t.Fatal(err)
		}
		shards, err := json.Marshal(plan.Fleet)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("qualification-shards=%s\n", shards)
		fmt.Printf("qualification-artefact-id=%s\n", qualificationArtefactID(plan))
		t.Logf("qualification-plan=%s", plan.ID)
		for _, scenario := range plan.Scenarios {
			t.Logf("scenario=%s checks=%v", scenario.Name, scenario.Checks)
		}
		return
	}
	scenarios, solvers, err := qualificationInputs(plan, *qualificationShardFlag)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan=%s driver=%s runs=%d seed-start=%d shard=%s fleet=%v",
		plan.ID, plan.Driver, plan.Runs, plan.SeedStart,
		*qualificationShardFlag, plan.Fleet)
	runQualificationGate(t, plan, scenarios, solvers, qualificationFixedGate,
		plan.SeedStart, *qualificationOutputFlag)
}

func runQualificationGate(
	t *testing.T, plan qualificationPlan, scenarios []evaluationScenario,
	solvers []evaluationSolver, campaign qualificationCampaignKind,
	seedStart uint64, output string,
) {
	t.Helper()
	var writer *evidenceWriter
	if output != "" {
		manifest, err := buildQualificationManifest(
			plan, scenarios, solvers, campaign, seedStart)
		if err != nil {
			t.Fatalf("build qualification manifest: %v", err)
		}
		writer, err = newEvidenceWriter(
			output, manifest, len(scenarios)*len(solvers)*plan.Runs)
		if err != nil {
			t.Fatalf("open qualification output: %v", err)
		}
		defer writer.Abort()
	}
	results, err := runQualification(
		t.Context(), scenarios, solvers, plan, seedStart, writer)
	if err != nil {
		t.Fatalf("write qualification output: %v", err)
	}
	if writer != nil {
		if err := writer.Close(); err != nil {
			t.Fatalf("complete qualification output: %v", err)
		}
	}
	checkQualification(t, plan, scenarios, solvers, results)
}

func qualificationArtefactID(plan qualificationPlan) string {
	id := strings.TrimPrefix(plan.ID, "tturl/ranking/")
	return strings.ReplaceAll(id, "/", "-")
}

func qualificationCampaignSeed(plan qualificationPlan) (uint64, error) {
	return qualificationCampaignSeedFor(
		plan, *qualificationCampaignSeedFlag, *qualificationCampaignRunIDFlag)
}

func qualificationCampaignSeedFor(
	plan qualificationPlan, seedText, runIDText string,
) (uint64, error) {
	if seedText != "" && runIDText != "" {
		return 0, fmt.Errorf("campaign seed and run ID are mutually exclusive")
	}
	var seed uint64
	var err error
	switch {
	case seedText != "":
		seed, err = strconv.ParseUint(seedText, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("campaign seed: %w", err)
		}
	case runIDText != "":
		runID, parseErr := strconv.ParseUint(runIDText, 10, 64)
		if parseErr != nil || runID == 0 {
			return 0, fmt.Errorf("campaign run ID must be a positive uint64")
		}
		fixedEnd, endErr := evaluationSeedEnd(plan.SeedStart, plan.Runs)
		if endErr != nil {
			return 0, endErr
		}
		offset := runID - 1
		// The qualification plan has already established a positive run count.
		runs := uint64(plan.Runs) //nolint:gosec // Positive validated plan count.
		if offset > (^uint64(0)-fixedEnd)/runs {
			return 0, fmt.Errorf("campaign run ID seed allocation overflows uint64")
		}
		seed = fixedEnd + offset*runs
	default:
		return 0, fmt.Errorf("campaign seed or run ID is required")
	}
	end, err := evaluationSeedEnd(seed, plan.Runs)
	if err != nil {
		return 0, fmt.Errorf("campaign seed interval: %w", err)
	}
	fixedEnd, err := evaluationSeedEnd(plan.SeedStart, plan.Runs)
	if err != nil {
		return 0, err
	}
	if seed < fixedEnd && end > plan.SeedStart {
		return 0, fmt.Errorf("campaign seed interval [%d, %d) overlaps fixed gate [%d, %d)",
			seed, end, plan.SeedStart, fixedEnd)
	}
	return seed, nil
}

func TestOutlierSolverQualificationCampaign(t *testing.T) {
	if *qualificationCampaignShardFlag == "" && !*qualificationCampaignListFlag {
		t.Skip("set -qualification-campaign.shard to run an independent campaign")
	}
	plan, err := selectedQualificationPlan()
	if err != nil {
		t.Fatal(err)
	}
	seedStart, err := qualificationCampaignSeed(plan)
	if err != nil {
		t.Fatal(err)
	}
	seedEnd, err := evaluationSeedEnd(seedStart, plan.Runs)
	if err != nil {
		t.Fatal(err)
	}
	if *qualificationCampaignListFlag {
		if err := plan.validate(
			evaluationScenarioCatalogue(), evaluationSolverCatalogue()); err != nil {
			t.Fatal(err)
		}
		shards, err := json.Marshal(plan.Fleet)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("qualification-shards=%s\n", shards)
		fmt.Printf("qualification-artefact-id=%s\n", qualificationArtefactID(plan))
		fmt.Printf("qualification-campaign-seed-start=%d\n", seedStart)
		fmt.Printf("qualification-campaign-seed-end=%d\n", seedEnd)
		return
	}
	if *qualificationCampaignOutputFlag == "" {
		t.Fatal("-qualification-campaign.output is required")
	}
	scenarios, solvers, err := qualificationInputs(
		plan, *qualificationCampaignShardFlag)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan=%s campaign=%s driver=%s runs=%d seed-window=[%d,%d) shard=%s fleet=%v",
		plan.ID, qualificationIndependent, plan.Driver, plan.Runs, seedStart,
		seedEnd, *qualificationCampaignShardFlag, plan.Fleet)
	runQualificationGate(t, plan, scenarios, solvers, qualificationIndependent,
		seedStart, *qualificationCampaignOutputFlag)
}

// newCalibrationCellPlan designs a one-sided Monte Carlo guard before outcomes
// are inspected. At an actual error rate of nominal+margin, the probability of
// passing is at most falseAccept.
func newCalibrationCellPlan(
	runs int, nominal, margin, falseAccept float64,
) (calibrationCellPlan, error) {
	plan := calibrationCellPlan{
		Runs: runs, NominalBudget: nominal,
		RegressionMargin: margin, FalseAcceptBudget: falseAccept,
	}
	if runs < 1 {
		return plan, fmt.Errorf("calibration runs must be >= 1")
	}
	if nominal <= 0 || nominal >= 1 {
		return plan, fmt.Errorf("nominal budget must be in (0, 1)")
	}
	if margin <= 0 || nominal+margin >= 1 {
		return plan, fmt.Errorf("regression margin must be positive and keep rate below 1")
	}
	if falseAccept <= 0 || falseAccept >= 1 {
		return plan, fmt.Errorf("false-accept budget must be in (0, 1)")
	}
	plan.CriticalErrors = binomialLowerCritical(
		runs, nominal+margin, falseAccept)
	if plan.CriticalErrors < 0 {
		plan.FalseRejectRate = 1
		return plan, nil
	}
	plan.FalseRejectRate = 1 - binomialCDF(runs, plan.CriticalErrors, nominal)
	return plan, nil
}

func binomialLowerCritical(n int, probability, lowerTail float64) int {
	logProbability := float64(n) * math.Log1p(-probability)
	logCDF := math.Inf(-1)
	critical := -1
	for errors := 0; errors <= n; errors++ {
		logCDF = logAdd(logCDF, logProbability)
		if math.Exp(logCDF) <= lowerTail {
			critical = errors
		} else {
			break
		}
		if errors < n {
			logProbability += math.Log(float64(n-errors)) -
				math.Log(float64(errors+1)) + math.Log(probability) -
				math.Log1p(-probability)
		}
	}
	return critical
}

func binomialCDF(n, atMost int, probability float64) float64 {
	if atMost < 0 {
		return 0
	}
	if atMost >= n {
		return 1
	}
	logProbability := float64(n) * math.Log1p(-probability)
	logCDF := math.Inf(-1)
	for errors := 0; errors <= atMost; errors++ {
		logCDF = logAdd(logCDF, logProbability)
		if errors < atMost {
			logProbability += math.Log(float64(n-errors)) -
				math.Log(float64(errors+1)) + math.Log(probability) -
				math.Log1p(-probability)
		}
	}
	return min(1, math.Exp(logCDF))
}

func logAdd(left, right float64) float64 {
	if math.IsInf(left, -1) {
		return right
	}
	if right > left {
		left, right = right, left
	}
	return left + math.Log1p(math.Exp(right-left))
}

func TestQualificationPlanReleaseDesign(t *testing.T) {
	plan := currentQualificationPlan()
	if err := plan.validate(evaluationScenarioCatalogue(), evaluationSolverCatalogue()); err != nil {
		t.Fatal(err)
	}
	if len(plan.Fleet) != 7 || len(plan.Scenarios) != 14 {
		t.Fatalf("qualification design has %d solvers and %d scenarios, want 7 and 14",
			len(plan.Fleet), len(plan.Scenarios))
	}
	if cells := plan.statisticalCells(); cells != 126 {
		t.Fatalf("statistical cells = %d, want 126", cells)
	}
	_, solvers, err := qualificationInputs(plan, "all")
	if err != nil {
		t.Fatal(err)
	}
	for _, solver := range solvers {
		want := evaluationResumableDriver
		if solver.Name == "rolling-baseline-reserved" ||
			solver.Name == "rolling-peer-first" ||
			solver.Name == "rolling-baseline-confirmed" {
			want = evaluationIdentifyDriver
		}
		if got := qualificationEvaluationDriver(plan, solver); got != want {
			t.Errorf("driver for %s = %v, want %v", solver.Name, got, want)
		}
	}
	counts := map[qualificationCheckKind]int{}
	for _, scenario := range plan.Scenarios {
		for _, check := range scenario.Checks {
			counts[check.Kind]++
		}
	}
	if counts[qualificationFalseFind] != 11 ||
		counts[qualificationFalseNegative] != 5 ||
		counts[qualificationUtility] != 2 ||
		counts[qualificationExhaustion] != 1 {
		t.Fatalf("qualification check counts = %v", counts)
	}
	cell, err := newCalibrationCellPlan(
		plan.Runs, 0.10, qualificationGuaranteeMargin,
		qualificationFalseAccept/float64(plan.statisticalCells()))
	if err != nil {
		t.Fatal(err)
	}
	if cell.FalseRejectRate > qualificationFalseReject {
		t.Errorf("%d-run false-reject rate %.4f exceeds %.4f",
			plan.Runs, cell.FalseRejectRate, qualificationFalseReject)
	}
}

func TestRollingAdmissionPlanDesign(t *testing.T) {
	plan := rollingAdmissionPlan()
	if err := plan.validate(
		evaluationScenarioCatalogue(), evaluationSolverCatalogue()); err != nil {
		t.Fatal(err)
	}
	if plan.ID != rollingAdmissionPlanID ||
		plan.Driver != qualificationIdentifyDriverID ||
		plan.SeedStart != 3_001 || plan.Runs != 3_000 ||
		!slices.Equal(plan.Fleet, []string{"rolling-baseline-reserved"}) ||
		len(plan.Scenarios) != 14 || plan.statisticalCells() != 18 {
		t.Fatalf("rolling admission design = %+v", plan)
	}
}

func TestRollingSiblingsAdmissionPlanDesign(t *testing.T) {
	plan := rollingSiblingsAdmissionPlan()
	if err := plan.validate(
		evaluationScenarioCatalogue(), evaluationSolverCatalogue()); err != nil {
		t.Fatal(err)
	}
	if plan.ID != rollingSiblingsAdmissionPlanID ||
		plan.Driver != qualificationIdentifyDriverID ||
		plan.SeedStart != 9_001 || plan.Runs != 3_000 ||
		!slices.Equal(plan.Fleet, []string{
			"rolling-peer-first", "rolling-baseline-confirmed",
		}) || len(plan.Scenarios) != 14 || plan.statisticalCells() != 36 {
		t.Fatalf("rolling siblings admission design = %+v", plan)
	}
}

func TestRollingAdmissionTrialIsExactlyReplayable(t *testing.T) {
	plan := rollingAdmissionPlan()
	scenario := evaluationScenarioByName("endpoint/m4/late/boundary/w4/p4")
	_, solvers, err := qualificationInputs(plan, "rolling-baseline-reserved")
	if err != nil {
		t.Fatal(err)
	}
	want := runEvaluationTrialWithDriver(
		t.Context(), solvers[0], scenario, 6_001, 2, evaluationIdentifyDriver)
	for replay := range 20 {
		got := runEvaluationTrialWithDriver(
			t.Context(), solvers[0], scenario, 6_001, 2,
			evaluationIdentifyDriver)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("replay %d differs\nfirst: %+v\nreplay: %+v", replay, want, got)
		}
	}
}

func TestQualificationConcurrentExhaustionHonoursFixtureCap(t *testing.T) {
	plan := currentQualificationPlan()
	scenario := evaluationScenarioByName(
		"endpoint/m4/late/null/exhausted/w4/p4")
	_, solvers, err := qualificationInputs(plan, "all")
	if err != nil {
		t.Fatal(err)
	}
	for _, solver := range solvers {
		t.Run(solver.Name, func(t *testing.T) {
			for run := range 100 {
				record := runEvaluationTrialWithDriver(
					t.Context(), solver, scenario, plan.SeedStart, run,
					qualificationEvaluationDriver(plan, solver))
				if qualificationRecordIsError(qualificationExhaustion, record) {
					t.Fatalf("run %d failed exact exhaustion: %+v", run, record)
				}
			}
		})
	}
}

func TestQualificationPlanRejectsInvalidDesign(t *testing.T) {
	base := currentQualificationPlan()
	tests := []struct {
		name   string
		mutate func(*qualificationPlan, *[]evaluationSolver)
	}{
		{"unknown driver", func(p *qualificationPlan, _ *[]evaluationSolver) {
			p.Driver = "unknown"
		}},
		{"seed overflow", func(p *qualificationPlan, _ *[]evaluationSolver) {
			p.SeedStart = ^uint64(0)
		}},
		{"empty fleet", func(p *qualificationPlan, _ *[]evaluationSolver) {
			p.Fleet = nil
		}},
		{"unknown fleet solver", func(p *qualificationPlan, _ *[]evaluationSolver) {
			p.Fleet[0] = "unknown"
		}},
		{"duplicate fleet solver", func(p *qualificationPlan, _ *[]evaluationSolver) {
			p.Fleet[1] = p.Fleet[0]
		}},
		{"non-resumable fleet solver", func(_ *qualificationPlan, solvers *[]evaluationSolver) {
			(*solvers)[0].New = func(evaluationSolverConfig) OutlierSolver {
				return oneComparisonEvaluationOutlierSolver()
			}
		}},
		{"incompatible inference regime", func(_ *qualificationPlan, solvers *[]evaluationSolver) {
			(*solvers)[0].Inference = OutlierInferenceFixedDesign
		}},
		{"unsupported scenario", func(_ *qualificationPlan, solvers *[]evaluationSolver) {
			(*solvers)[0].Supports = func(evaluationScenario) string {
				return "unsupported for test"
			}
		}},
		{"empty scenarios", func(p *qualificationPlan, _ *[]evaluationSolver) {
			p.Scenarios = nil
		}},
		{"unknown scenario", func(p *qualificationPlan, _ *[]evaluationSolver) {
			p.Scenarios[0].Name = "unknown"
		}},
		{"duplicate scenario", func(p *qualificationPlan, _ *[]evaluationSolver) {
			p.Scenarios[1].Name = p.Scenarios[0].Name
		}},
		{"empty checks", func(p *qualificationPlan, _ *[]evaluationSolver) {
			p.Scenarios[0].Checks = nil
		}},
		{"duplicate check", func(p *qualificationPlan, _ *[]evaluationSolver) {
			p.Scenarios[0].Checks = append(
				p.Scenarios[0].Checks, p.Scenarios[0].Checks[0])
		}},
		{"false-negative on null", func(p *qualificationPlan, _ *[]evaluationSolver) {
			p.Scenarios[0].Checks = []qualificationCheck{{
				Kind:   qualificationFalseNegative,
				Margin: qualificationGuaranteeMargin,
			}}
		}},
		{"utility without budget", func(p *qualificationPlan, _ *[]evaluationSolver) {
			p.Scenarios[7].Checks = []qualificationCheck{{
				Kind: qualificationUtility, Margin: 0.10,
			}}
		}},
		{"unknown check", func(p *qualificationPlan, _ *[]evaluationSolver) {
			p.Scenarios[0].Checks = []qualificationCheck{{
				Kind: "unknown", Margin: qualificationGuaranteeMargin,
			}}
		}},
		{"parameterised exact check", func(p *qualificationPlan, _ *[]evaluationSolver) {
			p.Scenarios[11].Checks[0].Margin = 0.10
		}},
		{"no statistical cells", func(p *qualificationPlan, _ *[]evaluationSolver) {
			p.Scenarios = []qualificationScenario{{
				Name:   "endpoint/m4/late/null/exhausted/w4/p4",
				Checks: []qualificationCheck{{Kind: qualificationExhaustion}},
			}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := base
			plan.Fleet = slices.Clone(base.Fleet)
			plan.Scenarios = make([]qualificationScenario, len(base.Scenarios))
			for i, scenario := range base.Scenarios {
				plan.Scenarios[i] = qualificationScenario{
					Name: scenario.Name, Checks: slices.Clone(scenario.Checks),
				}
			}
			solvers := slices.Clone(evaluationSolverCatalogue())
			test.mutate(&plan, &solvers)
			if err := plan.validate(evaluationScenarioCatalogue(), solvers); err == nil {
				t.Fatal("validation succeeded")
			}
		})
	}
}

func TestQualificationErrors(t *testing.T) {
	records := []evaluationRunRecord{
		{Classification: evaluationCorrectFound},
		{Classification: evaluationCorrectNegative},
		{Classification: evaluationFalseFind},
		{Classification: evaluationWrongItem},
		{Classification: evaluationWrongSign},
		{Classification: evaluationFalseNegative},
		{
			Classification: evaluationInconclusive, MaxComparisons: 4,
			Profile: evaluationProfileRecord{MaxWidth: 4},
			Result:  evaluationResultRecord{Comparisons: 4, Participations: 16},
		},
		{
			Classification: evaluationInconclusive, MaxComparisons: 4,
			Profile: evaluationProfileRecord{MaxWidth: 4},
			Result:  evaluationResultRecord{Comparisons: 3, Participations: 12},
		},
	}
	for _, test := range []struct {
		kind qualificationCheckKind
		want int
	}{
		{qualificationFalseFind, 3},
		{qualificationFalseNegative, 1},
		{qualificationUtility, 7},
		{qualificationExhaustion, 7},
	} {
		if got := qualificationErrors(test.kind, records); got != test.want {
			t.Errorf("kind %q errors = %d, want %d", test.kind, got, test.want)
		}
	}
}

func TestQualificationFleetIsIndependentOfEvaluationCatalogue(t *testing.T) {
	plan := currentQualificationPlan()
	solvers := evaluationSolverCatalogue()
	catalogue := append(slices.Clone(solvers), evaluationSolver{
		Name: "experimental", Inference: OutlierInferenceAnytimeValid,
		Params: map[string]string{"status": "experimental"}, New: solvers[0].New,
	})
	if err := plan.validate(evaluationScenarioCatalogue(), catalogue); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(plan.Fleet, "experimental") {
		t.Fatal("evaluation enrolment changed the qualification fleet")
	}
}

func TestEvaluationScenariosHaveEvidenceOwners(t *testing.T) {
	planScenarios := make(map[string]bool)
	for _, scenario := range currentQualificationPlan().Scenarios {
		planScenarios[scenario.Name] = true
	}
	panelScenarios := make(map[string]int)
	for _, scenario := range evaluationScenarioCatalogue() {
		if len(scenario.Panels) == 0 && !planScenarios[scenario.Name] {
			t.Errorf("scenario %q belongs to no panel or qualification plan",
				scenario.Name)
		}
		for _, panel := range scenario.Panels {
			panelScenarios[panel]++
		}
	}
	for _, panel := range evaluationPanelNames {
		if panelScenarios[panel] == 0 {
			t.Errorf("evaluation panel %q has no scenarios", panel)
		}
	}
}

func TestQualificationShardSelectionIsExact(t *testing.T) {
	plan := currentQualificationPlan()
	_, solvers, err := qualificationInputs(plan, "baseline-reserved")
	if err != nil {
		t.Fatal(err)
	}
	if len(solvers) != 1 || solvers[0].Name != "baseline-reserved" {
		t.Fatalf("selected solvers = %v", solvers)
	}
	if _, _, err := qualificationInputs(plan, "baseline-.*"); err == nil {
		t.Fatal("regular expression accepted as a qualification shard")
	}
}

func TestQualificationCampaignSeedAllocation(t *testing.T) {
	plan := currentQualificationPlan()
	tests := []struct {
		name, seed, runID string
		want              uint64
		wantError         bool
	}{
		{name: "explicit", seed: "10000", want: 10_000},
		{name: "first run", runID: "1", want: 3_001},
		{name: "second run", runID: "2", want: 6_001},
		{name: "missing", wantError: true},
		{name: "both", seed: "10000", runID: "1", wantError: true},
		{name: "invalid seed", seed: "-1", wantError: true},
		{name: "zero run", runID: "0", wantError: true},
		{name: "overlap", seed: "3000", wantError: true},
		{name: "seed overflow", seed: "18446744073709551615", wantError: true},
		{name: "run overflow", runID: "18446744073709551615", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := qualificationCampaignSeedFor(plan, test.seed, test.runID)
			if (err != nil) != test.wantError {
				t.Fatalf("seed = %d, error = %v", got, err)
			}
			if err == nil && got != test.want {
				t.Fatalf("seed = %d, want %d", got, test.want)
			}
		})
	}
}

func TestQualificationManifestCarriesFullPlanForShard(t *testing.T) {
	plan := currentQualificationPlan()
	scenarios, solvers, err := qualificationInputs(plan, "baseline-reserved")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := buildQualificationManifest(
		plan, scenarios, solvers, qualificationFixedGate, plan.SeedStart)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Fleet) != 7 || len(manifest.FleetSolvers) != 7 ||
		len(manifest.Shard) != 1 || len(manifest.QualificationScenarios) != 14 ||
		manifest.StatisticalCells != 126 || manifest.PlanID != qualificationPlanID ||
		manifest.Schema != qualificationMetaSchema ||
		manifest.FixtureVersion != evaluationFixtureVersion ||
		manifest.Campaign != qualificationFixedGate ||
		manifest.SeedEnd != qualificationSeedStart+qualificationRuns {
		t.Fatalf("incomplete qualification manifest: %+v", manifest)
	}
}

func TestCalibrationCellPlanExact(t *testing.T) {
	plan, err := newCalibrationCellPlan(3, 0.25, 0.25, 0.20)
	if err != nil {
		t.Fatal(err)
	}
	if plan.CriticalErrors != 0 {
		t.Errorf("critical errors = %d, want 0", plan.CriticalErrors)
	}
	wantFalseReject := 1 - 0.75*0.75*0.75
	if math.Abs(plan.FalseRejectRate-wantFalseReject) > 1e-15 {
		t.Errorf("false-reject rate = %.17g, want %.17g",
			plan.FalseRejectRate, wantFalseReject)
	}
}

func TestBinomialCDF(t *testing.T) {
	want := 0.75*0.75*0.75*0.75 + 4*0.25*0.75*0.75*0.75
	if got := binomialCDF(4, 1, 0.25); math.Abs(got-want) > 1e-15 {
		t.Errorf("CDF = %.17g, want %.17g", got, want)
	}
}
