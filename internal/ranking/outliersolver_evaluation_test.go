package ranking

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"testing"
)

var (
	evaluationRunsFlag = flag.Int(
		"evaluation.runs", 0,
		"run the opt-in outlier solver evaluation with this many paired seeds",
	)
	evaluationSeedFlag = flag.Uint64(
		"evaluation.seed", 1,
		"first master seed for the opt-in outlier solver evaluation",
	)
	evaluationPanelFlag = flag.String(
		"evaluation.panel", "smoke",
		"named outlier solver evaluation panel",
	)
	evaluationScenarioFlag = flag.String(
		"evaluation.scenario", ".*",
		"regular expression selecting outlier solver evaluation scenarios",
	)
	evaluationSolverFlag = flag.String(
		"evaluation.solver", ".*",
		"regular expression selecting outlier solver variants",
	)
	evaluationOutputFlag = flag.String(
		"evaluation.output", "",
		"new JSONL path for raw outlier solver evaluation evidence",
	)
	evaluationListFlag = flag.Bool(
		"evaluation.list", false,
		"list selected outlier solver evaluation scenarios and solvers",
	)
)

type evaluationClassification string

const (
	evaluationCorrectFound     evaluationClassification = "correct-found"
	evaluationCorrectNegative  evaluationClassification = "correct-negative"
	evaluationFalseFind        evaluationClassification = "false-find"
	evaluationWrongItem        evaluationClassification = "wrong-item"
	evaluationWrongSign        evaluationClassification = "wrong-sign"
	evaluationSubedgeFound     evaluationClassification = "subedge-found"
	evaluationFalseNegative    evaluationClassification = "false-negative"
	evaluationSubedgeNegative  evaluationClassification = "subedge-negative"
	evaluationUnscoredNegative evaluationClassification = "unscored-negative"
	evaluationInconclusive     evaluationClassification = "inconclusive"
	evaluationSolverError      evaluationClassification = "solver-error"
	evaluationAccountingError  evaluationClassification = "accounting-error"
	evaluationInvalidResult    evaluationClassification = "invalid-result"
	evaluationUnsupported      evaluationClassification = "unsupported"
)

type evaluationTargetTruthRecord struct {
	Index     int     `json:"index"`
	Direction string  `json:"sign,omitempty"`
	Scale     float64 `json:"scale"`
	Scored    bool    `json:"scored"`
	Qualifies bool    `json:"qualifies"`
}

type evaluationTruthRecord struct {
	Targets []evaluationTargetTruthRecord `json:"targets"`
}

type evaluationTargetSpecRecord struct {
	Direction string  `json:"sign"`
	Scale     float64 `json:"scale"`
}

type evaluationRequestRecord struct {
	Population        int     `json:"population"`
	Direction         string  `json:"sign"`
	FalseFindRisk     float64 `json:"false_find_risk"`
	FalseNegativeRisk float64 `json:"false_negative_risk"`
}

type evaluationProfileRecord struct {
	MaxWidth         int    `json:"max_width"`
	MaxConcurrency   int    `json:"max_concurrency"`
	BaselineControls int    `json:"baseline_controls"`
	BaselineSupply   string `json:"baseline_supply"`
	PositiveControls int    `json:"positive_controls"`
	PositiveSupply   string `json:"positive_supply"`
}

type evaluationResultRecord struct {
	Outcome        string  `json:"outcome"`
	Index          int     `json:"index"`
	Direction      string  `json:"sign,omitempty"`
	ErrorRiskLimit float64 `json:"error_risk_limit,omitempty"`
	Inference      string  `json:"inference,omitempty"`
	Comparisons    int     `json:"comparisons"`
	Participations int     `json:"participations"`
}

type evaluationCostRecord struct {
	Comparisons           int `json:"comparisons"`
	Participations        int `json:"participations"`
	RestrictedComparisons int `json:"restricted_comparisons"`
}

type evaluationSamplingRecord struct {
	TargetSamples  []int `json:"target_samples"`
	SampledItems   int   `json:"sampled_items"`
	MaxItemSamples int   `json:"max_item_samples"`
	TotalSamples   int   `json:"total_samples"`
}

type evaluationRunRecord struct {
	Schema          string                   `json:"schema"`
	Solver          string                   `json:"solver"`
	SolverParams    map[string]string        `json:"solver_params"`
	Scenario        string                   `json:"scenario"`
	Run             int                      `json:"run"`
	Seed            uint64                   `json:"seed"`
	EnvironmentSeed uint64                   `json:"environment_seed"`
	SolverSeed      uint64                   `json:"solver_seed"`
	Truth           evaluationTruthRecord    `json:"truth"`
	Request         evaluationRequestRecord  `json:"request"`
	Profile         evaluationProfileRecord  `json:"profile"`
	MinimumEdge     float64                  `json:"minimum_edge"`
	MaxComparisons  int                      `json:"max_comparisons"`
	Channel         evaluationChannelSpec    `json:"channel"`
	PremiseNote     string                   `json:"premise_note,omitempty"`
	Result          evaluationResultRecord   `json:"result"`
	Classification  evaluationClassification `json:"classification"`
	Cost            evaluationCostRecord     `json:"cost"`
	Sampling        evaluationSamplingRecord `json:"sampling"`
	ObservedChannel evaluationChannelStats   `json:"observed_channel"`
	Error           string                   `json:"error,omitempty"`
}

type evaluationScenarioManifest struct {
	Name              string                       `json:"name"`
	Panels            []string                     `json:"panels"`
	Population        int                          `json:"population"`
	RequestDirection  string                       `json:"request_sign"`
	Targets           []evaluationTargetSpecRecord `json:"targets"`
	PremiseNote       string                       `json:"premise_note,omitempty"`
	FalseFindRisk     float64                      `json:"false_find_risk"`
	FalseNegativeRisk float64                      `json:"false_negative_risk"`
	MinimumEdge       float64                      `json:"minimum_edge"`
	MaxComparisons    int                          `json:"max_comparisons"`
	Profile           evaluationProfileRecord      `json:"profile"`
	Channel           evaluationChannelSpec        `json:"channel"`
}

type evaluationSolverManifest struct {
	Name      string            `json:"name"`
	Inference string            `json:"inference"`
	Params    map[string]string `json:"params"`
}

type evaluationManifest struct {
	Schema         string                       `json:"schema"`
	FixtureVersion string                       `json:"fixture_version"`
	Panel          string                       `json:"panel"`
	Runs           int                          `json:"runs"`
	SeedStart      uint64                       `json:"seed_start"`
	SeedEnd        uint64                       `json:"seed_end"`
	GoVersion      string                       `json:"go_version"`
	GOOS           string                       `json:"goos"`
	GOARCH         string                       `json:"goarch"`
	Revision       string                       `json:"revision,omitempty"`
	Dirty          bool                         `json:"dirty,omitempty"`
	SourceSHA256   string                       `json:"source_sha256"`
	Solvers        []evaluationSolverManifest   `json:"solvers"`
	Scenarios      []evaluationScenarioManifest `json:"scenarios"`
}

var errEvaluationCapExceeded = errors.New("evaluation comparison cap exceeded")

type evaluationDriver uint8

const (
	evaluationIdentifyDriver evaluationDriver = iota
	evaluationResumableDriver
)

func runEvaluationTrial(
	ctx context.Context, solverSpec evaluationSolver, scenario evaluationScenario,
	masterSeed uint64, run int,
) evaluationRunRecord {
	return runEvaluationTrialWithDriver(
		ctx, solverSpec, scenario, masterSeed, run, evaluationIdentifyDriver)
}

func runEvaluationTrialWithDriver(
	ctx context.Context, solverSpec evaluationSolver, scenario evaluationScenario,
	masterSeed uint64, run int, driver evaluationDriver,
) evaluationRunRecord {
	environmentSeed, solverSeed := evaluationSeeds(masterSeed, run)
	truth := scenario.truth(environmentSeed)
	if solverSpec.Supports != nil {
		if reason := solverSpec.Supports(scenario); reason != "" {
			record := newEvaluationRunRecord(
				solverSpec, scenario, truth, evaluationRunSeed(masterSeed, run),
				environmentSeed, solverSeed, run,
				OutlierResult{Index: -1}, evaluationChannelStats{},
			)
			record.Classification = evaluationUnsupported
			record.Cost.RestrictedComparisons = scenario.MaxComparisons
			record.Error = reason
			return record
		}
	}
	comparator, channel := newEvaluationComparator(scenario, truth, environmentSeed)
	solver := solverSpec.New(evaluationSolverConfig{
		Seed:           solverSeed,
		MaxComparisons: scenario.MaxComparisons,
		MaxConcurrency: scenario.MaxConcurrency,
	})
	if regime := solver.Inference(); regime != solverSpec.Inference {
		record := newEvaluationRunRecord(
			solverSpec, scenario, truth, evaluationRunSeed(masterSeed, run),
			environmentSeed, solverSeed, run,
			OutlierResult{Index: -1}, evaluationChannelStats{},
		)
		record.Classification = evaluationUnsupported
		record.Cost.RestrictedComparisons = scenario.MaxComparisons
		record.Error = "solver inference changed from " +
			solverSpec.Inference.String() + " to " + regime.String()
		return record
	}
	request := OutlierRequest{
		M: scenario.Population, Direction: scenario.RequestDirection,
		MinimumEdge:       scenario.MinimumEdge,
		FalseFindRisk:     scenario.FalseFindRisk,
		FalseNegativeRisk: scenario.FalseNegativeRisk,
	}
	result, err := runEvaluationSolver(ctx, solver, request, comparator,
		scenario.MaxComparisons, scenario.MaxConcurrency,
		evaluationRunSeed(masterSeed, run), driver)
	stats := channel.snapshot()
	record := newEvaluationRunRecord(
		solverSpec, scenario, truth, evaluationRunSeed(masterSeed, run),
		environmentSeed, solverSeed, run,
		result, stats,
	)
	if err != nil {
		record.Classification = evaluationSolverError
		record.Error = err.Error()
		record.Cost.RestrictedComparisons = scenario.MaxComparisons
		return record
	}
	if stats.Comparisons > scenario.MaxComparisons ||
		result.Comparisons != stats.Comparisons ||
		result.Participations != stats.Participations {
		record.Classification = evaluationAccountingError
		record.Error = fmt.Sprintf(
			"result cost %d/%d, observed %d/%d, cap %d",
			result.Comparisons, result.Participations,
			stats.Comparisons, stats.Participations, scenario.MaxComparisons,
		)
		record.Cost.RestrictedComparisons = scenario.MaxComparisons
		return record
	}
	if err := validateEvaluationResult(request, scenario.profile(), result); err != nil {
		record.Classification = evaluationInvalidResult
		record.Error = err.Error()
		record.Cost.RestrictedComparisons = scenario.MaxComparisons
		return record
	}
	record.Classification = classifyEvaluationResult(truth, result)
	if evaluationQuality(record.Classification) < 2 {
		record.Cost.RestrictedComparisons = scenario.MaxComparisons
	}
	return record
}

func runEvaluationSolver(
	ctx context.Context, solver OutlierSolver, request OutlierRequest,
	comparator *Comparator, maxComparisons, concurrency int, seed uint64,
	driver evaluationDriver,
) (OutlierResult, error) {
	if driver == evaluationIdentifyDriver {
		return solver.Identify(ctx, request, comparator)
	}
	resumable, ok := solver.(ResumableOutlierSolver)
	if !ok {
		return OutlierResult{Index: -1}, errors.New(
			"resumable evaluation requires the resumable anytime-valid capability")
	}
	run, err := resumable.Start(request, comparator)
	if err != nil {
		return OutlierResult{Index: -1}, err
	}
	defer run.Close()
	remaining := maxComparisons
	var result OutlierResult
	for remaining > 0 {
		waves := 32 + int(seed%97)
		if result.Comparisons > 0 && len(result.Items) > 0 {
			waves = 32 + result.Items[0].Samples%97
		} else if concurrency == 1 && seed%16 == 0 {
			waves = 1
		}
		tranche := min(remaining, max(concurrency, 1)*waves)
		result, err = run.Advance(ctx, Budget{Comparisons: tranche})
		if err != nil || result.Outcome != OutlierPaused {
			return result, err
		}
		remaining = maxComparisons - result.Comparisons
	}
	result.Outcome = OutlierInconclusive
	return result, nil
}

func newEvaluationRunRecord(
	solver evaluationSolver, scenario evaluationScenario, truth evaluationTruth,
	seed, environmentSeed, solverSeed uint64, run int, result OutlierResult,
	stats evaluationChannelStats,
) evaluationRunRecord {
	targetSamples := make([]int, len(truth.Targets))
	sampledItems := 0
	maxItemSamples := 0
	totalSamples := 0
	for item, itemStats := range result.Items {
		if itemStats.Samples > 0 {
			sampledItems++
		}
		maxItemSamples = max(maxItemSamples, itemStats.Samples)
		totalSamples += itemStats.Samples
		for targetIndex, target := range truth.Targets {
			if item == target.Index {
				targetSamples[targetIndex] = itemStats.Samples
			}
		}
	}
	truthRecords := make([]evaluationTargetTruthRecord, len(truth.Targets))
	for i, target := range truth.Targets {
		truthRecords[i] = evaluationTargetTruthRecord{
			Index: target.Index, Direction: target.Direction.String(), Scale: target.Scale,
			Scored: target.Scored, Qualifies: target.Qualifies,
		}
	}
	resultSign := ""
	if result.Outcome == OutlierFound {
		resultSign = result.Direction.String()
	}
	resultInference := ""
	if result.ErrorDirection != OutlierErrorUncontrolled {
		resultInference = result.Regime.String()
	}
	return evaluationRunRecord{
		Schema: evaluationRunSchema,
		Solver: solver.Name, SolverParams: solver.Params,
		Scenario: scenario.Name, Run: run, Seed: seed,
		EnvironmentSeed: environmentSeed, SolverSeed: solverSeed,
		Truth: evaluationTruthRecord{Targets: truthRecords},
		Request: evaluationRequestRecord{
			Population: scenario.Population, Direction: scenario.RequestDirection.String(),
			FalseFindRisk:     scenario.FalseFindRisk,
			FalseNegativeRisk: scenario.FalseNegativeRisk,
		},
		Profile:        evaluationProfile(scenario),
		MinimumEdge:    scenario.MinimumEdge,
		MaxComparisons: scenario.MaxComparisons,
		Channel:        scenario.Channel,
		PremiseNote:    scenario.PremiseNote,
		Result: evaluationResultRecord{
			Outcome: result.Outcome.String(), Index: result.Index,
			Direction: resultSign, ErrorRiskLimit: result.ErrorRiskLimit,
			Inference:   resultInference,
			Comparisons: result.Comparisons, Participations: result.Participations,
		},
		Cost: evaluationCostRecord{
			Comparisons:           stats.Comparisons,
			Participations:        stats.Participations,
			RestrictedComparisons: stats.Comparisons,
		},
		Sampling: evaluationSamplingRecord{
			TargetSamples: targetSamples, SampledItems: sampledItems,
			MaxItemSamples: maxItemSamples, TotalSamples: totalSamples,
		},
		ObservedChannel: stats,
	}
}

func validateEvaluationResult(
	request OutlierRequest, profile Profile, result OutlierResult,
) error {
	if result.Comparisons < 0 || result.Participations < 0 {
		return fmt.Errorf("negative reported work")
	}
	if len(result.Items) != request.M {
		return fmt.Errorf("items length %d, want %d", len(result.Items), request.M)
	}
	samples := 0
	for index, item := range result.Items {
		if item.Samples < 0 || item.Samples > result.Comparisons {
			return fmt.Errorf("item %d samples %d outside [0, %d]",
				index, item.Samples, result.Comparisons)
		}
		if samples > result.Participations ||
			item.Samples > result.Participations-samples {
			return fmt.Errorf("item samples exceed participations %d",
				result.Participations)
		}
		samples += item.Samples
		switch {
		case item.Samples == 0 && !math.IsNaN(item.MeanRank):
			return fmt.Errorf("item %d unsampled mean rank is not NaN", index)
		case item.Samples > 0 && (math.IsNaN(item.MeanRank) ||
			math.IsInf(item.MeanRank, 0) || item.MeanRank < 0 ||
			item.MeanRank >= float64(profile.MaxWidth)):
			return fmt.Errorf("item %d mean rank %v outside [0, %d)",
				index, item.MeanRank, profile.MaxWidth)
		}
	}
	switch result.Outcome {
	case OutlierFound:
		if result.Index < 0 || result.Index >= request.M {
			return fmt.Errorf("found index %d outside [0, %d)", result.Index, request.M)
		}
		if result.Direction != Late && result.Direction != Early {
			return fmt.Errorf("found sign %v is invalid", result.Direction)
		}
		if request.Direction != Either && result.Direction != request.Direction {
			return fmt.Errorf("found sign %v differs from request %v",
				result.Direction, request.Direction)
		}
		if result.ErrorDirection != OutlierErrorFalseFind || result.Premise == OutlierPremiseUnspecified ||
			result.Regime != OutlierInferenceAnytimeValid ||
			result.MinimumEdge < 0 ||
			result.MinimumEdge >= 0.5 {
			return fmt.Errorf("found guarantee is invalid: %+v", result.OutlierGuarantee)
		}
		if result.ErrorRiskLimit != request.FalseFindRisk {
			return fmt.Errorf("found error-risk limit %v, want %v",
				result.ErrorRiskLimit, request.FalseFindRisk)
		}
	case OutlierNegative:
		if result.Index != -1 {
			return fmt.Errorf("negative index %d, want -1", result.Index)
		}
		if result.ErrorDirection != OutlierErrorFalseNegative ||
			result.Premise == OutlierPremiseUnspecified ||
			result.Regime != OutlierInferenceAnytimeValid || result.Direction != request.Direction ||
			!(result.MinimumEdge > 0 && result.MinimumEdge < 0.5) {
			return fmt.Errorf("negative guarantee is invalid: %+v", result.OutlierGuarantee)
		}
		if result.ErrorRiskLimit != request.FalseNegativeRisk {
			return fmt.Errorf("negative error-risk limit %v, want %v",
				result.ErrorRiskLimit, request.FalseNegativeRisk)
		}
	case OutlierInconclusive:
		if result.Index != -1 {
			return fmt.Errorf("inconclusive index %d, want -1", result.Index)
		}
		if result.OutlierGuarantee != (OutlierGuarantee{}) {
			return fmt.Errorf("inconclusive guarantee %+v, want zero", result.OutlierGuarantee)
		}
	default:
		return fmt.Errorf("unknown outcome %v", result.Outcome)
	}
	return nil
}

func evaluationProfile(scenario evaluationScenario) evaluationProfileRecord {
	baselineSupply := "none"
	baselineControls := scenario.BaselineControls
	if scenario.UnboundedControls {
		baselineSupply = "unbounded"
		baselineControls = -1
	} else if baselineControls > 0 {
		baselineSupply = "bounded"
	}
	positiveSupply := "none"
	positiveControls := scenario.PositiveControls
	if scenario.UnboundedPositive {
		positiveSupply = "unbounded"
		positiveControls = -1
	} else if positiveControls > 0 {
		positiveSupply = "bounded"
	}
	return evaluationProfileRecord{
		MaxWidth: scenario.Width, MaxConcurrency: scenario.MaxConcurrency,
		BaselineControls: baselineControls, BaselineSupply: baselineSupply,
		PositiveControls: positiveControls, PositiveSupply: positiveSupply,
	}
}

func classifyEvaluationResult(
	truth evaluationTruth, result OutlierResult,
) evaluationClassification {
	switch result.Outcome {
	case OutlierFound:
		if len(truth.Targets) == 0 {
			return evaluationFalseFind
		}
		target, ok := truth.target(result.Index)
		if !ok {
			return evaluationWrongItem
		}
		if result.Direction != target.Direction {
			return evaluationWrongSign
		}
		if target.Scored && !target.Qualifies {
			return evaluationSubedgeFound
		}
		return evaluationCorrectFound
	case OutlierNegative:
		if len(truth.Targets) == 0 {
			return evaluationCorrectNegative
		}
		scored := false
		for _, target := range truth.Targets {
			if target.Scored {
				scored = true
			}
			if target.Qualifies {
				return evaluationFalseNegative
			}
		}
		if scored {
			return evaluationSubedgeNegative
		}
		return evaluationUnscoredNegative
	default:
		return evaluationInconclusive
	}
}

type evaluationSummary struct {
	Classifications map[evaluationClassification]int
	ReturnedTargets map[string]int
	Comparisons     []int
	Participations  []int
	Restricted      []int
}

type evaluationPairSummary struct {
	QualityWins, QualityTies, QualityLosses int
	CostWins, CostTies, CostLosses          int
	AcceptableRateDelta                     float64
	AcceptableRateLower95                   float64
	MeanRestrictedComparisonDelta           float64
	RestrictedComparisonDeltaLow            float64
	RestrictedComparisonDeltaHigh           float64
	MeanParticipationDelta                  float64
	ParticipationDeltaLow                   float64
	ParticipationDeltaHigh                  float64
}

func summariseEvaluation(records []evaluationRunRecord) evaluationSummary {
	summary := evaluationSummary{
		Classifications: make(map[evaluationClassification]int),
		ReturnedTargets: make(map[string]int),
		Comparisons:     make([]int, 0, len(records)),
		Participations:  make([]int, 0, len(records)),
		Restricted:      make([]int, 0, len(records)),
	}
	for _, record := range records {
		summary.Classifications[record.Classification]++
		if record.Result.Outcome == OutlierFound.String() {
			for _, target := range record.Truth.Targets {
				if target.Index == record.Result.Index {
					label := fmt.Sprintf("%s@%g", target.Direction, target.Scale)
					summary.ReturnedTargets[label]++
				}
			}
		}
		summary.Comparisons = append(summary.Comparisons, record.Cost.Comparisons)
		summary.Participations = append(
			summary.Participations, record.Cost.Participations)
		summary.Restricted = append(
			summary.Restricted, record.Cost.RestrictedComparisons)
	}
	return summary
}

func evaluationMean(values []int) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	var total int64
	for _, value := range values {
		total += int64(value)
	}
	return float64(total) / float64(len(values))
}

func evaluationQuantile(values []int, probability float64) int {
	if len(values) == 0 {
		return 0
	}
	ordered := slices.Clone(values)
	slices.Sort(ordered)
	index := int(math.Ceil(probability*float64(len(ordered)))) - 1
	index = max(0, min(index, len(ordered)-1))
	return ordered[index]
}

func compareEvaluationPairs(
	reference, candidate []evaluationRunRecord,
) (evaluationPairSummary, error) {
	if len(reference) != len(candidate) {
		return evaluationPairSummary{}, fmt.Errorf(
			"paired record lengths differ: %d and %d", len(reference), len(candidate))
	}
	var summary evaluationPairSummary
	acceptableWins := 0
	acceptableLosses := 0
	restrictedDeltas := make([]int, 0, len(reference))
	participationDeltas := make([]int, 0, len(reference))
	for i := range reference {
		left, right := reference[i], candidate[i]
		if left.Scenario != right.Scenario || left.Run != right.Run ||
			left.Seed != right.Seed ||
			left.EnvironmentSeed != right.EnvironmentSeed ||
			left.SolverSeed != right.SolverSeed ||
			!slices.Equal(left.Truth.Targets, right.Truth.Targets) ||
			left.Request != right.Request ||
			left.Profile != right.Profile || left.MinimumEdge != right.MinimumEdge ||
			left.MaxComparisons != right.MaxComparisons || left.Channel != right.Channel ||
			left.PremiseNote != right.PremiseNote {
			return evaluationPairSummary{}, fmt.Errorf(
				"pair %d has different experimental inputs: %s/%d and %s/%d",
				i, left.Scenario, left.Seed, right.Scenario, right.Seed)
		}
		leftQuality := evaluationQuality(left.Classification)
		rightQuality := evaluationQuality(right.Classification)
		switch {
		case rightQuality > leftQuality:
			summary.QualityWins++
		case rightQuality < leftQuality:
			summary.QualityLosses++
		default:
			summary.QualityTies++
		}
		acceptableDelta := evaluationAcceptable(right.Classification) -
			evaluationAcceptable(left.Classification)
		if acceptableDelta > 0 {
			acceptableWins++
		} else if acceptableDelta < 0 {
			acceptableLosses++
		}
		if left.Classification == right.Classification && leftQuality == 2 {
			switch {
			case right.Cost.Comparisons < left.Cost.Comparisons:
				summary.CostWins++
			case right.Cost.Comparisons > left.Cost.Comparisons:
				summary.CostLosses++
			default:
				summary.CostTies++
			}
		}
		restrictedDeltas = append(restrictedDeltas,
			right.Cost.RestrictedComparisons-left.Cost.RestrictedComparisons)
		participationDeltas = append(participationDeltas,
			right.Cost.Participations-left.Cost.Participations)
	}
	if len(reference) > 0 {
		summary.AcceptableRateDelta = float64(acceptableWins-acceptableLosses) / float64(len(reference))
	} else {
		summary.AcceptableRateDelta = math.NaN()
	}
	summary.AcceptableRateLower95 = evaluationPairedQualityLower(
		len(reference), acceptableWins, acceptableLosses, 0.05)
	summary.MeanRestrictedComparisonDelta,
		summary.RestrictedComparisonDeltaLow,
		summary.RestrictedComparisonDeltaHigh = evaluationPairedMeanInterval(
		restrictedDeltas, 0x434f4d5041524953)
	summary.MeanParticipationDelta,
		summary.ParticipationDeltaLow,
		summary.ParticipationDeltaHigh = evaluationPairedMeanInterval(
		participationDeltas, 0x5041525449434950)
	return summary, nil
}

// evaluationPairedQualityLower returns a simultaneous one-sided lower confidence
// bound for p(win)-p(loss). Each marginal bound is exact Clopper-Pearson at
// alpha/2; Bonferroni therefore gives at least 1-alpha joint coverage without
// assumptions about the tie probability.
func evaluationPairedQualityLower(
	runs, wins, losses int, alpha float64,
) float64 {
	if runs == 0 {
		return math.NaN()
	}
	lowerWins := evaluationBinomialProportionLower(runs, wins, alpha/2)
	upperLosses := evaluationBinomialProportionUpper(runs, losses, alpha/2)
	return max(-1, lowerWins-upperLosses)
}

func evaluationBinomialProportionLower(n, successes int, alpha float64) float64 {
	if successes == 0 {
		return 0
	}
	target := 1 - alpha
	low, high := 0.0, 1.0
	for range 80 {
		mid := (low + high) / 2
		if binomialCDF(n, successes-1, mid) > target {
			low = mid
		} else {
			high = mid
		}
	}
	return (low + high) / 2
}

func evaluationBinomialProportionUpper(n, successes int, alpha float64) float64 {
	if successes == n {
		return 1
	}
	low, high := 0.0, 1.0
	for range 80 {
		mid := (low + high) / 2
		if binomialCDF(n, successes, mid) > alpha {
			low = mid
		} else {
			high = mid
		}
	}
	return (low + high) / 2
}

func evaluationAcceptable(classification evaluationClassification) int {
	if evaluationQuality(classification) == 2 {
		return 1
	}
	return 0
}

// evaluationPairedMeanInterval returns the mean and a deterministic 95% percentile
// bootstrap interval over paired deltas. It describes Monte Carlo uncertainty;
// it is not a calibration or theorem certificate.
func evaluationPairedMeanInterval(values []int, salt uint64) (float64, float64, float64) {
	if len(values) == 0 {
		return math.NaN(), math.NaN(), math.NaN()
	}
	mean := evaluationMean(values)
	if len(values) == 1 {
		return mean, mean, mean
	}
	const resamples = 2_000
	means := make([]float64, resamples)
	for resample := range resamples {
		var total int64
		for draw := range len(values) {
			key := salt ^ uint64(resample)*0x9e3779b97f4a7c15 ^ uint64(draw)
			// Values is nonempty, so the modulus fits in an int.
			width := uint64(len(values))
			index := int(evaluationMix(key) % width) //nolint:gosec // Modulus fits.
			total += int64(values[index])
		}
		means[resample] = float64(total) / float64(len(values))
	}
	slices.Sort(means)
	return mean, means[49], means[1949]
}

func evaluationQuality(classification evaluationClassification) int {
	switch classification {
	case evaluationCorrectFound, evaluationSubedgeFound, evaluationCorrectNegative,
		evaluationSubedgeNegative:
		return 2
	case evaluationInconclusive, evaluationUnscoredNegative:
		return 1
	default:
		return 0
	}
}

func selectEvaluationInputs(
	panel string, scenarioPattern, solverPattern string,
) ([]evaluationScenario, []evaluationSolver, error) {
	scenarioRE, err := regexp.Compile(scenarioPattern)
	if err != nil {
		return nil, nil, fmt.Errorf("scenario expression: %w", err)
	}
	solverRE, err := regexp.Compile(solverPattern)
	if err != nil {
		return nil, nil, fmt.Errorf("solver expression: %w", err)
	}
	allScenarios := evaluationScenarioCatalogue()
	if err := validateEvaluationScenarioCatalogue(allScenarios); err != nil {
		return nil, nil, err
	}
	var scenarios []evaluationScenario
	for _, scenario := range allScenarios {
		if scenario.inPanel(panel) && scenarioRE.MatchString(scenario.Name) {
			scenarios = append(scenarios, scenario)
		}
	}
	var solvers []evaluationSolver
	catalogue := evaluationSolverCatalogue()
	if err := validateEvaluationSolvers(catalogue); err != nil {
		return nil, nil, err
	}
	for _, solver := range catalogue {
		if solverRE.MatchString(solver.Name) {
			solvers = append(solvers, solver)
		}
	}
	if len(scenarios) == 0 {
		if !slices.Contains(evaluationPanelNames, panel) {
			return nil, nil, fmt.Errorf("unknown panel %q; available: %s",
				panel, strings.Join(evaluationPanelNames, ", "))
		}
		var available []string
		for _, scenario := range allScenarios {
			if scenario.inPanel(panel) {
				available = append(available, scenario.Name)
			}
		}
		return nil, nil, fmt.Errorf(
			"no scenarios selected for panel %q; available: %s",
			panel, strings.Join(available, ", "))
	}
	if len(solvers) == 0 {
		return nil, nil, fmt.Errorf("no solvers selected")
	}
	return scenarios, solvers, nil
}

func validateEvaluationSolvers(solvers []evaluationSolver) error {
	if len(solvers) == 0 {
		return fmt.Errorf("evaluation solver catalogue is empty")
	}
	seen := make(map[string]bool, len(solvers))
	for _, solver := range solvers {
		if solver.Name == "" {
			return fmt.Errorf("evaluation solver name is empty")
		}
		if solver.New == nil {
			return fmt.Errorf("evaluation solver %q has no factory", solver.Name)
		}
		if solver.Params == nil {
			return fmt.Errorf("evaluation solver %q has nil parameters", solver.Name)
		}
		if solver.Inference != OutlierInferenceAnytimeValid {
			return fmt.Errorf("evaluation solver %q uses %s inference; want anytime-valid",
				solver.Name, solver.Inference)
		}
		for key, value := range solver.Params {
			if key == "" || value == "" {
				return fmt.Errorf(
					"evaluation solver %q has an empty parameter", solver.Name)
			}
		}
		if seen[solver.Name] {
			return fmt.Errorf("duplicate evaluation solver name %q", solver.Name)
		}
		seen[solver.Name] = true
	}
	return nil
}

func runEvaluation(
	ctx context.Context, scenarios []evaluationScenario,
	solvers []evaluationSolver, seedStart uint64, runs int,
) []evaluationRunRecord {
	records := make([]evaluationRunRecord, 0, len(scenarios)*len(solvers)*runs)
	for _, scenario := range scenarios {
		for run := range runs {
			for _, solver := range solvers {
				records = append(records, runEvaluationTrial(
					ctx, solver, scenario, seedStart, run))
			}
		}
	}
	return records
}

func evaluationRecordsFor(
	records []evaluationRunRecord, scenario, solver string,
) []evaluationRunRecord {
	selected := make([]evaluationRunRecord, 0)
	for _, record := range records {
		if record.Scenario == scenario && record.Solver == solver {
			selected = append(selected, record)
		}
	}
	return selected
}

func buildEvaluationManifest(
	panel string, scenarios []evaluationScenario,
	solvers []evaluationSolver, seedStart uint64, runs int,
) (evaluationManifest, error) {
	seedEnd, err := evaluationSeedEnd(seedStart, runs)
	if err != nil {
		return evaluationManifest{}, err
	}
	manifest := evaluationManifest{
		Schema: evaluationMetaSchema, FixtureVersion: evaluationFixtureVersion,
		Panel: panel, Runs: runs, SeedStart: seedStart,
		SeedEnd:   seedEnd,
		GoVersion: runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
	}
	fingerprint, err := evaluationSourceFingerprint()
	if err != nil {
		return evaluationManifest{}, err
	}
	manifest.SourceSHA256 = fingerprint
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
	for _, solver := range solvers {
		manifest.Solvers = append(manifest.Solvers, evaluationSolverManifest{
			Name: solver.Name, Inference: solver.Inference.String(),
			Params: solver.Params,
		})
	}
	for _, scenario := range scenarios {
		targets := make([]evaluationTargetSpecRecord, len(scenario.Targets))
		for i, target := range scenario.Targets {
			targets[i] = evaluationTargetSpecRecord{
				Direction: target.Direction.String(), Scale: target.Scale,
			}
		}
		manifest.Scenarios = append(manifest.Scenarios, evaluationScenarioManifest{
			Name: scenario.Name, Panels: scenario.Panels,
			Population:        scenario.Population,
			RequestDirection:  scenario.RequestDirection.String(),
			Targets:           targets,
			PremiseNote:       scenario.PremiseNote,
			FalseFindRisk:     scenario.FalseFindRisk,
			FalseNegativeRisk: scenario.FalseNegativeRisk,
			MinimumEdge:       scenario.MinimumEdge,
			MaxComparisons:    scenario.MaxComparisons,
			Profile:           evaluationProfile(scenario), Channel: scenario.Channel,
		})
	}
	return manifest, nil
}

func evaluationSourceFingerprint() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("source fingerprint working directory: %w", err)
	}
	root := directory
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			return "", fmt.Errorf("source fingerprint: go.mod not found")
		}
		root = parent
	}
	return evaluationSourceFingerprintAt(root)
}

func evaluationSourceFingerprintAt(root string) (string, error) {
	paths := []string{filepath.Join(root, "go.mod")}
	for _, optional := range []string{"go.sum", "go.work", "go.work.sum"} {
		candidate := filepath.Join(root, optional)
		if _, err := os.Stat(candidate); err == nil {
			paths = append(paths, candidate)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("source fingerprint stat %s: %w", optional, err)
		}
	}
	for _, directory := range []string{"internal/ranking", "stats"} {
		err := filepath.WalkDir(filepath.Join(root, directory),
			func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") {
					paths = append(paths, path)
				}
				return nil
			})
		if err != nil {
			return "", fmt.Errorf("source fingerprint walk %s: %w", directory, err)
		}
	}
	sort.Strings(paths)
	hash := sha256.New()
	for _, path := range paths {
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return "", fmt.Errorf("source fingerprint path: %w", err)
		}
		// Paths come from walking the discovered module root.
		data, err := os.ReadFile(path) //nolint:gosec // Module-owned source path.
		if err != nil {
			return "", fmt.Errorf("source fingerprint read %s: %w", relative, err)
		}
		_, _ = fmt.Fprintf(hash, "%s\x00%d\x00", filepath.ToSlash(relative), len(data))
		_, _ = hash.Write(data)
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func TestEvaluationSourceFingerprintTracksQualificationDependencies(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "internal"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"internal/ranking", "stats", "unrelated"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, contents string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)),
			[]byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.test/fingerprint\n")
	write("internal/ranking/solver.go", "package ranking\n")
	write("stats/mean.go", "package stats\n")
	write("unrelated/sample.go", "package unrelated\n")

	baseline, err := evaluationSourceFingerprintAt(root)
	if err != nil {
		t.Fatal(err)
	}
	write("unrelated/sample.go", "package unrelated\n// Unrelated change.\n")
	unrelated, err := evaluationSourceFingerprintAt(root)
	if err != nil {
		t.Fatal(err)
	}
	if unrelated != baseline {
		t.Fatal("unrelated package changed the qualification source fingerprint")
	}
	write("stats/mean.go", "package stats\n// Dependency change.\n")
	dependency, err := evaluationSourceFingerprintAt(root)
	if err != nil {
		t.Fatal(err)
	}
	if dependency == baseline {
		t.Fatal("ranking dependency did not change the source fingerprint")
	}
}

func TestOutlierSolverEvaluation(t *testing.T) {
	if *evaluationRunsFlag == 0 && !*evaluationListFlag {
		t.Skip("set -evaluation.runs to run the opt-in comparative panel")
	}
	if *evaluationRunsFlag < 0 {
		t.Fatalf("-evaluation.runs = %d, want >= 0", *evaluationRunsFlag)
	}
	if *evaluationRunsFlag > 0 {
		if _, err := evaluationSeedEnd(*evaluationSeedFlag, *evaluationRunsFlag); err != nil {
			t.Fatal(err)
		}
	}
	scenarios, solvers, err := selectEvaluationInputs(
		*evaluationPanelFlag, *evaluationScenarioFlag, *evaluationSolverFlag,
	)
	if err != nil {
		t.Fatal(err)
	}
	if *evaluationListFlag {
		for _, scenario := range scenarios {
			t.Logf("scenario=%s", scenario.Name)
		}
		for _, solver := range solvers {
			t.Logf("solver=%s", solver.Name)
		}
		return
	}
	solverNames := make([]string, len(solvers))
	for i, solver := range solvers {
		solverNames[i] = solver.Name
	}
	t.Logf("panel=%s runs=%d seed-start=%d solvers=%s reference=%s",
		*evaluationPanelFlag, *evaluationRunsFlag, *evaluationSeedFlag,
		strings.Join(solverNames, ","), solvers[0].Name)
	records := runEvaluation(
		t.Context(), scenarios, solvers, *evaluationSeedFlag, *evaluationRunsFlag)
	for _, record := range records {
		if record.Classification == evaluationSolverError ||
			record.Classification == evaluationAccountingError ||
			record.Classification == evaluationInvalidResult {
			t.Errorf("scenario=%s solver=%s run=%d: %s: %s",
				record.Scenario, record.Solver, record.Run,
				record.Classification, record.Error)
		}
	}
	for _, scenario := range scenarios {
		for _, solver := range solvers {
			var selected []evaluationRunRecord
			for _, record := range records {
				if record.Scenario == scenario.Name && record.Solver == solver.Name {
					selected = append(selected, record)
				}
			}
			summary := summariseEvaluation(selected)
			t.Logf("scenario=%s solver=%s outcomes=%v returned-targets=%v "+
				"comparisons mean=%.1f p50=%d p90=%d "+
				"restricted mean=%.1f p90=%d "+
				"participations mean=%.1f",
				scenario.Name, solver.Name, summary.Classifications,
				summary.ReturnedTargets,
				evaluationMean(summary.Comparisons),
				evaluationQuantile(summary.Comparisons, 0.50),
				evaluationQuantile(summary.Comparisons, 0.90),
				evaluationMean(summary.Restricted),
				evaluationQuantile(summary.Restricted, 0.90),
				evaluationMean(summary.Participations))
		}
		if len(solvers) > 1 {
			reference := evaluationRecordsFor(records, scenario.Name, solvers[0].Name)
			for _, candidate := range solvers[1:] {
				paired, err := compareEvaluationPairs(
					reference, evaluationRecordsFor(records, scenario.Name, candidate.Name))
				if err != nil {
					t.Errorf("scenario=%s candidate=%s: %v",
						scenario.Name, candidate.Name, err)
					continue
				}
				t.Logf("scenario=%s candidate=%s reference=%s "+
					"quality W/T/L=%d/%d/%d same-decision cost W/T/L=%d/%d/%d "+
					"acceptable-rate delta=%+.3f lower95=%+.3f "+
					"restricted-comparison delta=%+.1f [%+.1f,%+.1f] "+
					"participation delta=%+.1f [%+.1f,%+.1f]",
					scenario.Name, candidate.Name, solvers[0].Name,
					paired.QualityWins, paired.QualityTies, paired.QualityLosses,
					paired.CostWins, paired.CostTies, paired.CostLosses,
					paired.AcceptableRateDelta,
					paired.AcceptableRateLower95,
					paired.MeanRestrictedComparisonDelta,
					paired.RestrictedComparisonDeltaLow,
					paired.RestrictedComparisonDeltaHigh,
					paired.MeanParticipationDelta,
					paired.ParticipationDeltaLow,
					paired.ParticipationDeltaHigh)
			}
		}
	}
	if *evaluationOutputFlag != "" {
		manifest, err := buildEvaluationManifest(
			*evaluationPanelFlag, scenarios, solvers,
			*evaluationSeedFlag, *evaluationRunsFlag,
		)
		if err != nil {
			t.Fatalf("build evaluation manifest: %v", err)
		}
		if err := writeEvaluationJSONL(*evaluationOutputFlag, manifest, records); err != nil {
			t.Fatalf("write evaluation output: %v", err)
		}
	}
}

func TestEvaluationResultClassification(t *testing.T) {
	target := func(index int, sign OutlierDirection, scored, qualifies bool) evaluationTruth {
		return evaluationTruth{Targets: []evaluationTargetTruth{{
			Index: index, Direction: sign, Scale: 1,
			Scored: scored, Qualifies: qualifies,
		}}}
	}
	tests := []struct {
		name   string
		truth  evaluationTruth
		result OutlierResult
		want   evaluationClassification
	}{
		{
			"null found",
			evaluationTruth{},
			OutlierResult{
				Outcome: OutlierFound, Index: 2,
				OutlierGuarantee: OutlierGuarantee{Direction: Late},
			},
			evaluationFalseFind,
		},
		{
			"null negative",
			evaluationTruth{},
			OutlierResult{Outcome: OutlierNegative, Index: -1},
			evaluationCorrectNegative,
		},
		{
			"wrong item",
			target(3, Late, true, true),
			OutlierResult{
				Outcome: OutlierFound, Index: 2,
				OutlierGuarantee: OutlierGuarantee{Direction: Late},
			},
			evaluationWrongItem,
		},
		{
			"wrong sign",
			target(3, Early, true, true),
			OutlierResult{
				Outcome: OutlierFound, Index: 3,
				OutlierGuarantee: OutlierGuarantee{Direction: Late},
			},
			evaluationWrongSign,
		},
		{
			"correct found",
			target(3, Early, true, true),
			OutlierResult{
				Outcome: OutlierFound, Index: 3,
				OutlierGuarantee: OutlierGuarantee{Direction: Early},
			},
			evaluationCorrectFound,
		},
		{
			"either qualifying target",
			evaluationTruth{Targets: []evaluationTargetTruth{
				{Index: 1, Direction: Early, Scored: true, Qualifies: true},
				{Index: 3, Direction: Late, Scored: true, Qualifies: true},
			}},
			OutlierResult{
				Outcome: OutlierFound, Index: 3,
				OutlierGuarantee: OutlierGuarantee{Direction: Late},
			},
			evaluationCorrectFound,
		},
		{
			"subedge found",
			target(3, Late, true, false),
			OutlierResult{
				Outcome: OutlierFound, Index: 3,
				OutlierGuarantee: OutlierGuarantee{Direction: Late},
			},
			evaluationSubedgeFound,
		},
		{
			"qualifying negative",
			target(3, Late, true, true),
			OutlierResult{Outcome: OutlierNegative, Index: -1},
			evaluationFalseNegative,
		},
		{
			"one qualifying target makes the negative false",
			evaluationTruth{Targets: []evaluationTargetTruth{
				{Index: 1, Direction: Late, Scored: true},
				{Index: 3, Direction: Late, Scored: true, Qualifies: true},
			}},
			OutlierResult{Outcome: OutlierNegative, Index: -1},
			evaluationFalseNegative,
		},
		{
			"subedge negative",
			target(3, Late, true, false),
			OutlierResult{Outcome: OutlierNegative, Index: -1},
			evaluationSubedgeNegative,
		},
		{
			"unscored negative",
			target(3, Late, false, false),
			OutlierResult{Outcome: OutlierNegative, Index: -1},
			evaluationUnscoredNegative,
		},
		{
			"inconclusive",
			target(3, Late, true, true),
			OutlierResult{Outcome: OutlierInconclusive, Index: -1},
			evaluationInconclusive,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyEvaluationResult(test.truth, test.result); got != test.want {
				t.Errorf("classification = %q, want %q", got, test.want)
			}
		})
	}
}

func evaluationTestScenario(t testing.TB, name string) evaluationScenario {
	t.Helper()
	for _, scenario := range evaluationScenarioCatalogue() {
		if scenario.Name == name {
			return scenario
		}
	}
	t.Fatalf("scenario %q not found", name)
	return evaluationScenario{}
}

func TestEvaluationResultContract(t *testing.T) {
	request := OutlierRequest{
		M: 2, Direction: Late, MinimumEdge: 0.1,
		FalseFindRisk: 0.10, FalseNegativeRisk: 0.20,
	}
	profile := Profile{MaxWidth: 4}
	valid := OutlierResult{
		Outcome: OutlierFound, Index: 1,
		OutlierGuarantee: OutlierGuarantee{
			ErrorDirection: OutlierErrorFalseFind, ErrorRiskLimit: 0.10,
			Direction: Late, Premise: OutlierPremiseBaselineMean,
			Regime: OutlierInferenceAnytimeValid,
		},
		Cost: Cost{Comparisons: 1, Participations: 2},
		Items: []ItemStat{
			{Samples: 1, MeanRank: 0},
			{Samples: 1, MeanRank: 1},
		},
	}
	if err := validateEvaluationResult(request, profile, valid); err != nil {
		t.Fatalf("valid result: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*OutlierResult)
	}{
		{"wrong error risk", func(r *OutlierResult) { r.ErrorRiskLimit = 0.11 }},
		{"missing inference regime", func(r *OutlierResult) {
			r.Regime = OutlierInferenceUncontrolled
		}},
		{"unknown outcome", func(r *OutlierResult) { r.Outcome = OutlierOutcome(99) }},
		{"bad index", func(r *OutlierResult) { r.Index = 2 }},
		{"bad sign", func(r *OutlierResult) { r.Direction = Early }},
		{"missing items", func(r *OutlierResult) { r.Items = nil }},
		{"negative samples", func(r *OutlierResult) { r.Items[0].Samples = -1 }},
		{"unsampled finite mean", func(r *OutlierResult) {
			r.Items[0] = ItemStat{}
		}},
		{"sampled NaN mean", func(r *OutlierResult) {
			r.Items[0].MeanRank = math.NaN()
		}},
		{"samples exceed work", func(r *OutlierResult) {
			r.Items[0].Samples = 2
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := valid
			result.Items = slices.Clone(valid.Items)
			test.mutate(&result)
			if err := validateEvaluationResult(request, profile, result); err == nil {
				t.Fatal("validation succeeded")
			}
		})
	}

	negative := valid
	negative.Outcome = OutlierNegative
	negative.Index = -1
	negative.OutlierGuarantee = OutlierGuarantee{
		ErrorDirection: OutlierErrorFalseNegative, ErrorRiskLimit: request.FalseNegativeRisk,
		MinimumEdge: 0.1, Direction: Late, Premise: OutlierPremiseBaselineMean,
		Regime: OutlierInferenceAnytimeValid,
	}
	if err := validateEvaluationResult(request, profile, negative); err != nil {
		t.Fatalf("valid negative: %v", err)
	}
	inconclusive := negative
	inconclusive.Outcome = OutlierInconclusive
	inconclusive.OutlierGuarantee = OutlierGuarantee{}
	if err := validateEvaluationResult(request, profile, inconclusive); err != nil {
		t.Fatalf("valid inconclusive: %v", err)
	}
}

func TestEvaluationUnacceptableDecisionChargedCap(t *testing.T) {
	scenario := evaluationTestScenario(t, "gaussian/m16/late/strong/flat/w8/p1")
	solver := evaluationSolver{
		Name: "premature-negative", Inference: OutlierInferenceAnytimeValid,
		Params: map[string]string{},
		New: func(evaluationSolverConfig) OutlierSolver {
			return evaluationOutlierSolverFunc(func(
				ctx context.Context, request OutlierRequest, comparator *Comparator,
			) (OutlierResult, error) {
				width := comparator.Profile().MaxWidth
				items := make([]ItemID, width)
				for i := range items {
					items[i] = ItemID(i)
				}
				if _, err := comparator.Compare(ctx, items); err != nil {
					return OutlierResult{}, err
				}
				itemStats := evaluationUnsampledItems(request.M)
				for i := range width {
					itemStats[i] = ItemStat{Samples: 1, MeanRank: float64(i)}
				}
				return OutlierResult{
					Outcome: OutlierNegative, Index: -1,
					OutlierGuarantee: OutlierGuarantee{
						ErrorDirection: OutlierErrorFalseNegative,
						ErrorRiskLimit: request.FalseNegativeRisk,
						MinimumEdge:    scenario.MinimumEdge, Direction: request.Direction,
						Premise: OutlierPremiseScheduledReferenceMean,
						Regime:  OutlierInferenceAnytimeValid,
					},
					Cost:  Cost{Comparisons: 1, Participations: width},
					Items: itemStats,
				}, nil
			})
		},
	}
	record := runEvaluationTrial(t.Context(), solver, scenario, 1, 0)
	if record.Classification != evaluationFalseNegative {
		t.Fatalf("classification = %q, want %q",
			record.Classification, evaluationFalseNegative)
	}
	if record.Cost.Comparisons != 1 || record.Cost.Participations != scenario.Width {
		t.Errorf("physical cost = %d/%d, want 1/%d",
			record.Cost.Comparisons, record.Cost.Participations, scenario.Width)
	}
	if record.Cost.RestrictedComparisons != scenario.MaxComparisons {
		t.Errorf("restricted comparisons = %d, want %d",
			record.Cost.RestrictedComparisons, scenario.MaxComparisons)
	}
}

func TestEvaluationPairedComparison(t *testing.T) {
	reference := []evaluationRunRecord{
		{
			Scenario: "s", Seed: 1, Classification: evaluationCorrectFound,
			Cost: evaluationCostRecord{
				Comparisons: 20, Participations: 80,
				RestrictedComparisons: 20,
			},
		},
		{
			Scenario: "s", Seed: 2, Classification: evaluationInconclusive,
			Cost: evaluationCostRecord{
				Comparisons: 10, Participations: 40,
				RestrictedComparisons: 100,
			},
		},
	}
	candidate := []evaluationRunRecord{
		{
			Scenario: "s", Seed: 1, Classification: evaluationCorrectFound,
			Cost: evaluationCostRecord{
				Comparisons: 15, Participations: 60,
				RestrictedComparisons: 15,
			},
		},
		{
			Scenario: "s", Seed: 2, Classification: evaluationCorrectNegative,
			Cost: evaluationCostRecord{
				Comparisons: 80, Participations: 320,
				RestrictedComparisons: 80,
			},
		},
	}
	got, err := compareEvaluationPairs(reference, candidate)
	if err != nil {
		t.Fatal(err)
	}
	want := evaluationPairSummary{
		QualityWins: 1, QualityTies: 1,
		CostWins:                      1,
		AcceptableRateDelta:           0.5,
		AcceptableRateLower95:         evaluationPairedQualityLower(2, 1, 0, 0.05),
		MeanRestrictedComparisonDelta: -12.5,
		RestrictedComparisonDeltaLow:  -20,
		RestrictedComparisonDeltaHigh: -5,
		MeanParticipationDelta:        130,
		ParticipationDeltaLow:         -20,
		ParticipationDeltaHigh:        280,
	}
	if got != want {
		t.Errorf("paired summary = %+v, want %+v", got, want)
	}
}

func TestEvaluationPairedQualityLowerAllTies(t *testing.T) {
	const runs = 200
	want := -(1 - math.Pow(0.025, 1.0/runs))
	got := evaluationPairedQualityLower(runs, 0, 0, 0.05)
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("all-tie lower bound = %.17g, want %.17g", got, want)
	}
	if got == 0 {
		t.Fatal("finite all-tie panel claimed zero regression uncertainty")
	}
}

func TestEvaluationPairRejectsDifferentSolverSeed(t *testing.T) {
	left := evaluationRunRecord{Scenario: "s", Seed: 1, SolverSeed: 2}
	right := left
	right.SolverSeed++
	if _, err := compareEvaluationPairs(
		[]evaluationRunRecord{left}, []evaluationRunRecord{right},
	); err == nil {
		t.Fatal("pair with different solver seed accepted")
	}
}

func TestEvaluationScenarioCatalogueValid(t *testing.T) {
	if err := validateEvaluationScenarioCatalogue(evaluationScenarioCatalogue()); err != nil {
		t.Fatal(err)
	}
}

func TestEvaluationScenarioValidationRejectsContradictions(t *testing.T) {
	valid := evaluationTestScenario(t, "gaussian/m16/late/strong/flat/w8/p1")
	for _, test := range []struct {
		name   string
		mutate func(*evaluationScenario)
	}{
		{"Either without controls", func(s *evaluationScenario) {
			s.RequestDirection = Either
			s.Targets = evaluationTargets(Either)
			s.UnboundedControls = false
		}},
		{"two control supplies", func(s *evaluationScenario) {
			s.BaselineControls = 1
		}},
		{"too many targets", func(s *evaluationScenario) {
			s.Targets = make([]evaluationTargetSpec, s.Population+1)
		}},
		{"target direction mismatch", func(s *evaluationScenario) {
			s.Targets = evaluationTargets(Early)
		}},
		{"negative target scale", func(s *evaluationScenario) {
			s.Targets[0].Scale = -1
		}},
		{"multiple endpoint targets", func(s *evaluationScenario) {
			s.Channel.Kind = evaluationEndpoint
			s.Channel.EndpointEdge = 0.1
			s.Targets = evaluationTargets(Late, Late)
		}},
		{"unknown noise", func(s *evaluationScenario) {
			s.Channel.Noise = evaluationNoiseKind("unknown")
		}},
		{"unknown bias", func(s *evaluationScenario) {
			s.Channel.Bias = evaluationBiasKind("unknown")
		}},
		{"invalid endpoint edge", func(s *evaluationScenario) {
			s.Channel.Kind = evaluationEndpoint
			s.Channel.EndpointEdge = 0.5
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			scenario := valid
			test.mutate(&scenario)
			if err := validateEvaluationScenarioCatalogue([]evaluationScenario{scenario}); err == nil {
				t.Fatal("validation succeeded")
			}
		})
	}
	if err := validateEvaluationScenarioCatalogue([]evaluationScenario{valid, valid}); err == nil {
		t.Fatal("duplicate scenario validation succeeded")
	}
	unknownPanel := valid
	unknownPanel.Panels = []string{"unknown"}
	if err := validateEvaluationScenarioCatalogue([]evaluationScenario{unknownPanel}); err == nil {
		t.Fatal("unknown panel validation succeeded")
	}
}

func TestEvaluationSolverRegistryValid(t *testing.T) {
	if err := validateEvaluationSolvers(evaluationSolverCatalogue()); err != nil {
		t.Fatal(err)
	}
	valid := evaluationSolver{
		Name: "valid", Inference: OutlierInferenceAnytimeValid,
		Params: map[string]string{},
		New: func(evaluationSolverConfig) OutlierSolver {
			return oneComparisonEvaluationOutlierSolver()
		},
	}
	fixed := valid
	fixed.Name = "fixed"
	fixed.Inference = OutlierInferenceFixedDesign
	fixed.New = func(evaluationSolverConfig) OutlierSolver {
		return fixedEvaluationOutlierSolver{oneComparisonEvaluationOutlierSolver()}
	}
	for _, test := range []struct {
		name    string
		solvers []evaluationSolver
	}{
		{"empty", nil},
		{"empty name", []evaluationSolver{{Params: map[string]string{}, New: valid.New}}},
		{"nil factory", []evaluationSolver{{Name: "nil", Params: map[string]string{}}}},
		{"nil parameters", []evaluationSolver{{Name: "nil", New: valid.New}}},
		{"empty parameter", []evaluationSolver{{
			Name: "empty", Params: map[string]string{"evidence": ""}, New: valid.New,
		}}},
		{"fixed inference", []evaluationSolver{fixed}},
		{"duplicate", []evaluationSolver{valid, valid}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateEvaluationSolvers(test.solvers); err == nil {
				t.Fatal("validation succeeded")
			}
		})
	}
}

func TestEvaluationRegisteredSolversRun(t *testing.T) {
	scenarios := evaluationScenarioCatalogue()
	for _, solver := range evaluationSolverCatalogue() {
		t.Run(solver.Name, func(t *testing.T) {
			var selected *evaluationScenario
			for i := range scenarios {
				if solver.Supports == nil || solver.Supports(scenarios[i]) == "" {
					selected = &scenarios[i]
					break
				}
			}
			if selected == nil {
				t.Fatal("solver supports no reviewed scenario")
			}
			record := runEvaluationTrial(t.Context(), solver, *selected, 1, 0)
			if record.Classification == evaluationSolverError ||
				record.Classification == evaluationAccountingError ||
				record.Classification == evaluationInvalidResult ||
				record.Classification == evaluationUnsupported {
				t.Fatalf("scenario=%s classification=%s error=%s",
					record.Scenario, record.Classification, record.Error)
			}
		})
	}
}

func TestEvaluationSeedSeparation(t *testing.T) {
	environmentA, solverA := evaluationSeeds(11, 7)
	environmentB, solverB := evaluationSeeds(11, 7)
	if environmentA != environmentB || solverA != solverB {
		t.Errorf("paired seeds differ: environment %d/%d solver %d/%d",
			environmentA, environmentB, solverA, solverB)
	}
	if environmentA == solverA {
		t.Errorf("environment and solver seeds collide: %d", environmentA)
	}
	if next, _ := evaluationSeeds(11, 8); next == environmentA {
		t.Errorf("environment seed did not change across runs: %d", next)
	}
}

func TestEvaluationSeedInterval(t *testing.T) {
	if end, err := evaluationSeedEnd(7, 3); err != nil || end != 10 {
		t.Fatalf("seed end = %d, %v; want 10, nil", end, err)
	}
	for _, test := range []struct {
		start uint64
		runs  int
	}{
		{0, 0},
		{0, -1},
		{^uint64(0), 1},
		{^uint64(0) - 1, 2},
	} {
		if _, err := evaluationSeedEnd(test.start, test.runs); err == nil {
			t.Errorf("seed interval start=%d runs=%d accepted", test.start, test.runs)
		}
	}
}

func TestEvaluationChannelKeyedReproduction(t *testing.T) {
	scenario := evaluationTestScenario(t, "gaussian/m16/late/strong/flat/w8/p1")
	truth := scenario.truth(17)
	first, _ := newEvaluationComparator(scenario, truth, 17)
	second, _ := newEvaluationComparator(scenario, truth, 17)
	layoutA := []ItemID{
		0, 1, 2, 3, Control(Baseline, 0),
		Control(Baseline, 1), Control(Baseline, 2), Control(Baseline, 3),
	}
	layoutB := slices.Clone(layoutA)
	slices.Reverse(layoutB)

	firstA1, err := first.Compare(t.Context(), layoutA)
	if err != nil {
		t.Fatal(err)
	}
	firstB1, err := first.Compare(t.Context(), layoutB)
	if err != nil {
		t.Fatal(err)
	}
	firstA2, err := first.Compare(t.Context(), layoutA)
	if err != nil {
		t.Fatal(err)
	}
	secondB1, err := second.Compare(t.Context(), layoutB)
	if err != nil {
		t.Fatal(err)
	}
	secondA1, err := second.Compare(t.Context(), layoutA)
	if err != nil {
		t.Fatal(err)
	}
	secondA2, err := second.Compare(t.Context(), layoutA)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(firstA1, secondA1) ||
		!slices.Equal(firstA2, secondA2) ||
		!slices.Equal(firstB1, secondB1) {
		t.Errorf("keyed outcomes depend on interleaving: "+
			"first A1/B1/A2=%v/%v/%v second=%v/%v/%v",
			firstA1, firstB1, firstA2, secondA1, secondB1, secondA2)
	}
}

func TestEvaluationEffectiveEdge(t *testing.T) {
	scenario := evaluationTestScenario(t, "gaussian/m99/late/boundary/flat/w8/p1")
	want := 0.5*(1+math.Erf(
		scenario.Channel.Effect/(2*scenario.Channel.NoiseScale))) - 0.5
	if got := scenario.effectiveEdge(evaluationTargetSpec{Direction: Late, Scale: 1}); math.Abs(got-want) > 1e-15 {
		t.Errorf("Gaussian effective edge = %.17g, want %.17g", got, want)
	}
	endpoint := evaluationTestScenario(t, "endpoint/m1/late/boundary/w4/p1")
	got := endpoint.effectiveEdge(evaluationTargetSpec{Direction: Late, Scale: 1})
	if got != endpoint.Channel.EndpointEdge {
		t.Errorf("endpoint effective edge = %v, want %v",
			got, endpoint.Channel.EndpointEdge)
	}
}

type evaluationOutlierSolverFunc func(
	context.Context, OutlierRequest, *Comparator,
) (OutlierResult, error)

type fixedEvaluationOutlierSolver struct {
	OutlierSolver
}

func (fixedEvaluationOutlierSolver) Inference() OutlierInferenceRegime {
	return OutlierInferenceFixedDesign
}

func (evaluationOutlierSolverFunc) Inference() OutlierInferenceRegime {
	return OutlierInferenceAnytimeValid
}

func (f evaluationOutlierSolverFunc) Identify(
	ctx context.Context, request OutlierRequest, comparator *Comparator,
) (OutlierResult, error) {
	return f(ctx, request, comparator)
}

func oneComparisonEvaluationOutlierSolver() OutlierSolver {
	return evaluationOutlierSolverFunc(func(
		ctx context.Context, request OutlierRequest, comparator *Comparator,
	) (OutlierResult, error) {
		width := comparator.Profile().MaxWidth
		items := make([]ItemID, width)
		items[0] = 0
		for i := 1; i < width; i++ {
			items[i] = Control(Baseline, i-1)
		}
		if _, err := comparator.Compare(ctx, items); err != nil {
			return OutlierResult{Index: -1}, err
		}
		return OutlierResult{
			Outcome: OutlierInconclusive, Index: -1,
			Cost:  Cost{Comparisons: 1, Participations: width},
			Items: evaluationUnsampledItems(request.M),
		}, nil
	})
}

func evaluationUnsampledItems(population int) []ItemStat {
	items := make([]ItemStat, population)
	for i := range items {
		items[i].MeanRank = math.NaN()
	}
	return items
}

func TestEvaluationSiblingFactoryAdoption(t *testing.T) {
	scenario := evaluationTestScenario(t, "endpoint/m1/late/null/w4/p1")
	solvers := []evaluationSolver{
		evaluationSolverCatalogue()[0],
		{
			Name: "one-comparison-example", Inference: OutlierInferenceAnytimeValid,
			Params: map[string]string{},
			New: func(evaluationSolverConfig) OutlierSolver {
				return oneComparisonEvaluationOutlierSolver()
			},
		},
	}
	const runs = 2
	records := runEvaluation(
		t.Context(), []evaluationScenario{scenario}, solvers, 1, runs)
	if len(records) != runs*len(solvers) {
		t.Fatalf("records = %d, want %d", len(records), runs*len(solvers))
	}
	if records[0].EnvironmentSeed != records[1].EnvironmentSeed {
		t.Errorf("paired environment seeds differ: %d vs %d",
			records[0].EnvironmentSeed, records[1].EnvironmentSeed)
	}
	if records[0].SolverSeed != records[1].SolverSeed {
		t.Errorf("paired solver seeds differ: %d vs %d",
			records[0].SolverSeed, records[1].SolverSeed)
	}
	if records[1].Classification != evaluationInconclusive {
		t.Errorf("example classification = %q, want %q",
			records[1].Classification, evaluationInconclusive)
	}
	if records[1].Cost.RestrictedComparisons != scenario.MaxComparisons {
		t.Errorf("restricted comparisons = %d, want cap %d",
			records[1].Cost.RestrictedComparisons, scenario.MaxComparisons)
	}
	reference := evaluationRecordsFor(records, scenario.Name, solvers[0].Name)
	candidate := evaluationRecordsFor(records, scenario.Name, solvers[1].Name)
	paired, err := compareEvaluationPairs(reference, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if paired.QualityLosses != runs {
		t.Errorf("candidate quality losses = %d, want %d",
			paired.QualityLosses, runs)
	}
}

func TestEvaluationUnsupportedCapability(t *testing.T) {
	scenario := evaluationTestScenario(t, "endpoint/m1/late/null/w4/p1")
	solver := evaluationSolver{
		Name: "limited-example", Inference: OutlierInferenceAnytimeValid,
		Params: map[string]string{},
		Supports: func(evaluationScenario) string {
			return "endpoint law unsupported"
		},
		New: func(evaluationSolverConfig) OutlierSolver {
			t.Fatal("unsupported solver factory called")
			return nil
		},
	}
	record := runEvaluationTrial(t.Context(), solver, scenario, 1, 0)
	if record.Classification != evaluationUnsupported {
		t.Errorf("classification = %q, want %q",
			record.Classification, evaluationUnsupported)
	}
	if record.Error != "endpoint law unsupported" {
		t.Errorf("reason = %q", record.Error)
	}
	if record.ObservedChannel.Comparisons != 0 {
		t.Errorf("observed comparisons = %d, want 0",
			record.ObservedChannel.Comparisons)
	}
}

func TestEvaluationJSONLNoOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	manifest := evaluationManifest{Schema: evaluationMetaSchema}
	records := []evaluationRunRecord{{Schema: evaluationRunSchema}}
	if err := writeEvaluationJSONL(path, manifest, records); err != nil {
		t.Fatal(err)
	}
	if err := writeEvaluationJSONL(path, manifest, records); err == nil {
		t.Fatal("second write succeeded, want no-overwrite error")
	}
	data, err := os.ReadFile(path) //nolint:gosec // Test-owned temporary path.
	if err != nil {
		t.Fatal(err)
	}
	var lines int
	for _, b := range data {
		if b == '\n' {
			lines++
		}
	}
	if lines != 3 {
		t.Errorf("JSONL lines = %d, want 3", lines)
	}
}

func TestEvaluationJSONLDeterministic(t *testing.T) {
	scenarios := []evaluationScenario{evaluationTestScenario(
		t, "gaussian/m8/either/multi2-opposed-boundary/baseline/w4/p1")}
	solvers := evaluationSolverCatalogue()
	manifest, err := buildEvaluationManifest("smoke", scenarios, solvers, 7, 2)
	if err != nil {
		t.Fatal(err)
	}
	firstRecords := runEvaluation(t.Context(), scenarios, solvers, 7, 2)
	secondRecords := runEvaluation(t.Context(), scenarios, solvers, 7, 2)
	directory := t.TempDir()
	firstPath := filepath.Join(directory, "first.jsonl")
	secondPath := filepath.Join(directory, "second.jsonl")
	if err := writeEvaluationJSONL(firstPath, manifest, firstRecords); err != nil {
		t.Fatal(err)
	}
	if err := writeEvaluationJSONL(secondPath, manifest, secondRecords); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(firstPath) //nolint:gosec // Test-owned temporary path.
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(secondPath) //nolint:gosec // Test-owned temporary path.
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(first, second) {
		t.Error("identical evaluation inputs produced different JSONL")
	}
	scanner := bufio.NewScanner(bytes.NewReader(first))
	if !scanner.Scan() {
		t.Fatal("JSONL has no manifest line")
	}
	var decodedManifest evaluationManifest
	if err := json.Unmarshal(scanner.Bytes(), &decodedManifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if decodedManifest.Schema != evaluationMetaSchema ||
		decodedManifest.FixtureVersion != evaluationFixtureVersion ||
		len(decodedManifest.Scenarios) != 1 ||
		len(decodedManifest.Solvers) != len(solvers) ||
		len(decodedManifest.SourceSHA256) != sha256.Size*2 {
		t.Errorf("decoded manifest = %+v", decodedManifest)
	}
	if !scanner.Scan() {
		t.Fatal("JSONL has no run line")
	}
	var decodedRun evaluationRunRecord
	if err := json.Unmarshal(scanner.Bytes(), &decodedRun); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if decodedRun.Schema != evaluationRunSchema || decodedRun.Seed != 7 ||
		decodedRun.Scenario != scenarios[0].Name ||
		decodedRun.Solver != solvers[0].Name {
		t.Errorf("decoded run = %+v", decodedRun)
	}
	for range len(firstRecords) - 1 {
		if !scanner.Scan() {
			t.Fatal("JSONL ended before all run records")
		}
	}
	if !scanner.Scan() {
		t.Fatal("JSONL has no completion line")
	}
	var completion evidenceCompletion
	if err := json.Unmarshal(scanner.Bytes(), &completion); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	if completion.Schema != evidenceCompletionSchema ||
		completion.Records != len(firstRecords) {
		t.Errorf("completion = %+v", completion)
	}
	if scanner.Scan() {
		t.Fatal("JSONL has data after completion")
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}
