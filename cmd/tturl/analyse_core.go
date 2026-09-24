package main

import (
	"fmt"
	"math"
	"slices"
	"sort"

	"github.com/tantosec/tturl/tth2"
)

const (
	analyseAlpha           = 0.05
	analyseMonteCarloDraws = 9_999
)

type analyseOutcome string

const (
	analyseDifference  analyseOutcome = "difference_detected"
	analyseNoDetection analyseOutcome = "no_detection"
	analyseUnavailable analyseOutcome = "unavailable"
)

type analysePairState string

const (
	analysePairsPerformed   analysePairState = "performed"
	analysePairsNotTested   analysePairState = "not_tested"
	analysePairsUnavailable analysePairState = "unavailable"
)

type analyseValidityCode string

const (
	analyseValid              analyseValidityCode = "valid"
	analyseInvalidWidth       analyseValidityCode = "unsupported_width"
	analyseTooFewCycles       analyseValidityCode = "insufficient_cycles"
	analyseIncompleteCycle    analyseValidityCode = "incomplete_cycle"
	analyseInvalidShift       analyseValidityCode = "invalid_shift"
	analyseInvalidArrangement analyseValidityCode = "invalid_arrangement"
	analyseInvalidRanks       analyseValidityCode = "invalid_ranks"
	analyseInvalidStatus      analyseValidityCode = "invalid_response_status"
	analyseMixedConnections   analyseValidityCode = "mixed_connections"
	analyseEvidenceTooLarge   analyseValidityCode = "evidence_too_large"
	analyseInvalidWorkload    analyseValidityCode = "invalid_workload"
	analyseInterrupted        analyseValidityCode = "interrupted"
)

// analyseEvidence is the transport-independent evidence for one fixed
// balanced experiment. Ranks are keyed by request; arrangements are keyed by
// outbound position.
type analyseEvidence struct {
	Width        int
	Cycles       []analyseCycleEvidence
	Observations []analyseTrialObservation
	Capture      raceCaptureConfig
	Execution    analyseExecutionEvidence
	Issue        *analyseEvidenceIssue
	Completion   runCompletion
}

type analyseExecutionEvidence struct {
	Recorded                bool
	PlannedCycles           int
	WarmupTrials            int
	WarmupRankComplete      int
	WarmupIncomplete        int
	WarmupRequestOperations int
	AttemptedTrials         int
	CompletedTrials         int
	AttemptedCycles         int
	Connections             int
	Replacements            int
}

type analyseEvidenceIssue struct {
	Code   analyseValidityCode
	Detail string
}

type analyseCycleEvidence struct {
	Trials []analyseTrialEvidence
}

type analyseTrialEvidence struct {
	Connection  int
	Shift       int
	Arrangement []int
	Ranks       []int
	Statuses    []int
}

// analyseTrialObservation retains every measured trial for audit and replay,
// including trials that cannot contribute to inference.
type analyseTrialObservation struct {
	Index       int
	Cycle       int
	Shift       int
	Connection  int
	Dispatch    tth2.DispatchTimes
	Arrangement []int
	Error       string
	cause       error
	Failure     *batchFailure
	Outcomes    []OutcomeRecord
}

// analyseConfig contains choices fixed before measured responses arrive. The
// seed makes approximate randomisation reproducible without deriving its
// random stream from the observed outcomes.
type analyseConfig struct {
	RandomisationSeed  uint64
	ConnectionLimit    int
	Capture            raceCaptureConfig
	WarmupWidth        int
	observationSink    func(analyseTrialObservation) error
	retainObservations bool
}

type analyseValidity struct {
	Available bool
	Code      analyseValidityCode
	Detail    string
}

type analyseComponents struct {
	L2        float64
	LInfinity float64
}

type analyseCalibration struct {
	Method            string
	Draws             int
	Extremes          int
	PValue            float64
	PValueKind        string
	SeedUsed          bool
	RandomisationSeed uint64
	CompletionReason  string
}

type analyseGlobalResult struct {
	Outcome     analyseOutcome
	Alpha       float64
	Statistic   float64
	Components  analyseComponents
	Calibration analyseCalibration
}

type analysePairResult struct {
	Left                int
	Right               int
	Trials              int
	LeftBeforeRight     int
	RightBeforeLeft     int
	MeanRankDifference  float64
	RawP                float64
	AdjustedP           float64
	RawPUpperBound      bool
	AdjustedPUpperBound bool
	Selected            bool
}

type analysePairResults struct {
	State analysePairState
	Pairs []analysePairResult
}

type analyseRequestEffect struct {
	Request            int
	Samples            int
	MeanRank           float64
	NormalisedMeanRank float64
	RankCounts         []int
	StatusCounts       []analyseStatusCount
}

type analyseStatusCount struct {
	Status int
	Count  int
}

type analysePositionEffect struct {
	Position int
	Samples  int
	MeanRank float64
}

type analyseResponseReview struct {
	Changing  []int
	Non2xx    []int
	Different []int
}

type analyseRunResult struct {
	Width           int
	ConnectionLimit int
	Plan            analysePlanResult
	Execution       analyseExecutionResult
	Cycles          int
	Trials          int
	Operations      int
	Validity        analyseValidity
	Requests        []analyseRequestEffect
	Positions       []analysePositionEffect
	Responses       analyseResponseReview
	Global          analyseGlobalResult
	Pairs           analysePairResults
	Evidence        analyseEvidence
	Completion      runCompletion
}

type analysePlanResult struct {
	Cycles           int
	Trials           int
	Operations       int
	DefaultCycles    int
	LowerSensitivity bool
}

type analyseExecutionResult struct {
	WarmupTrials            int
	WarmupRankComplete      int
	WarmupIncomplete        int
	WarmupRequestOperations int
	AttemptedTrials         int
	CompletedTrials         int
	CompleteCycles          int
	AttemptedCycles         int
	RetainedTrials          int
	Connections             int
	Replacements            int
}

type analyseRankEvidence struct {
	width      int
	cycles     int
	ranks      []int
	base       []int
	rankCounts [][]int
	statuses   []map[int]int
}

// analyseOffline applies the fixed inferential policy without depending on
// HTTP or report rendering.
func analyseOffline(input analyseEvidence, config analyseConfig) analyseRunResult {
	if input.Observations == nil {
		input.Observations = analyseObservationsFromCycles(input.Cycles)
	}
	connectionLimit := max(1, config.ConnectionLimit)
	result := analyseRunResult{
		Width: input.Width, ConnectionLimit: connectionLimit,
		Cycles: len(input.Cycles), Evidence: input,
		Completion: normaliseCompletion(input.Completion),
	}
	if trials, ok := checkedProduct(len(input.Cycles), input.Width); ok {
		result.Trials = trials
		if operations, valid := checkedProduct(trials, input.Width); valid {
			result.Operations = operations
		}
	}
	result.Plan = analysePlanFromEvidence(input)
	result.Execution = analyseExecutionFromEvidence(input, result)
	evidence, validity := validateAnalyseEvidence(input)
	result.Validity = validity
	if !validity.Available {
		result.Global.Outcome = analyseUnavailable
		result.Pairs.State = analysePairsUnavailable
		return result
	}

	result.Requests = analyseObservedEffects(evidence)
	result.Positions = analyseObservedPositionEffects(evidence)
	result.Responses = analyseReviewResponses(result.Requests)
	result.Global = analyseGlobalDecision(evidence, config.RandomisationSeed)
	if result.Global.Outcome != analyseDifference {
		result.Pairs.State = analysePairsNotTested
		return result
	}
	result.Pairs = analysePairwiseDecision(evidence, analyseAlpha)
	return result
}

func analyseObservationsFromCycles(
	cycles []analyseCycleEvidence,
) []analyseTrialObservation {
	observations := make([]analyseTrialObservation, 0)
	for cycleID, cycle := range cycles {
		for _, trial := range cycle.Trials {
			outcomes := make([]OutcomeRecord, len(trial.Ranks))
			positions := make([]int, len(trial.Ranks))
			for request := range positions {
				positions[request] = -1
			}
			for position, request := range trial.Arrangement {
				if request >= 0 && request < len(positions) {
					positions[request] = position
				}
			}
			for request, rank := range trial.Ranks {
				status := 0
				if request < len(trial.Statuses) {
					status = trial.Statuses[request]
				}
				outcomes[request] = OutcomeRecord{
					Request: request, Position: positions[request],
					Rank: new(rank),
					Response: &ResponseRecord{
						Status: status, BodyBytes: 0, BodyCapturedComplete: true,
						SHA256: digestBase64(nil), Extracts: []ExtractRecord{},
					},
				}
			}
			observations = append(observations, analyseTrialObservation{
				Index: cycleID*len(cycle.Trials) + trial.Shift,
				Cycle: cycleID, Shift: trial.Shift,
				Connection:  trial.Connection,
				Arrangement: append([]int(nil), trial.Arrangement...),
				Outcomes:    outcomes,
			})
		}
	}
	return observations
}

func analysePlanFromEvidence(input analyseEvidence) analysePlanResult {
	cycles := len(input.Cycles)
	if input.Execution.Recorded {
		cycles = input.Execution.PlannedCycles
	}
	if cycles < 0 {
		cycles = 0
	}
	defaultCycles := analyseDefaultCycles(input.Width)
	result := analysePlanResult{
		Cycles: cycles, DefaultCycles: defaultCycles,
		LowerSensitivity: cycles < defaultCycles,
	}
	if trials, ok := checkedProduct(cycles, input.Width); ok {
		result.Trials = trials
		if operations, valid := checkedProduct(trials, input.Width); valid {
			result.Operations = operations
		}
	}
	return result
}

func analyseExecutionFromEvidence(
	input analyseEvidence, retained analyseRunResult,
) analyseExecutionResult {
	if !input.Execution.Recorded {
		connections := analyseEvidenceConnectionCount(input)
		return analyseExecutionResult{
			AttemptedTrials: retained.Trials, CompletedTrials: retained.Trials,
			AttemptedCycles: retained.Cycles, CompleteCycles: retained.Cycles,
			RetainedTrials: retained.Trials,
			Connections:    connections,
		}
	}
	warmupRankComplete := input.Execution.WarmupRankComplete
	if input.Execution.WarmupTrials > 0 && warmupRankComplete == 0 &&
		input.Execution.WarmupIncomplete == 0 {
		warmupRankComplete = input.Execution.WarmupTrials
	}
	return analyseExecutionResult{
		WarmupTrials:            input.Execution.WarmupTrials,
		WarmupRankComplete:      warmupRankComplete,
		WarmupIncomplete:        input.Execution.WarmupIncomplete,
		WarmupRequestOperations: input.Execution.WarmupRequestOperations,
		AttemptedTrials:         input.Execution.AttemptedTrials,
		CompletedTrials:         input.Execution.CompletedTrials,
		AttemptedCycles:         input.Execution.AttemptedCycles,
		CompleteCycles:          len(input.Cycles), RetainedTrials: retained.Trials,
		Connections:  input.Execution.Connections,
		Replacements: input.Execution.Replacements,
	}
}

func (result analyseRunResult) executionAccounting(
	primingWidth int,
) (analyseExecution, error) {
	if primingWidth == 0 && result.Execution.WarmupTrials > 0 &&
		result.Execution.WarmupRequestOperations%
			result.Execution.WarmupTrials == 0 {
		primingWidth = result.Execution.WarmupRequestOperations /
			result.Execution.WarmupTrials
	}
	retainedCycles, retainedTrials := 0, 0
	if result.Validity.Available {
		retainedCycles = result.Execution.CompleteCycles
		retainedTrials = result.Execution.RetainedTrials
	}
	primingComplete, ok := checkedProduct(
		result.Execution.WarmupRankComplete, primingWidth)
	if !ok {
		return analyseExecution{}, fmt.Errorf(
			"analyse rank-complete priming work cannot be represented")
	}
	measuredAttempted, ok := checkedProduct(
		result.Execution.AttemptedTrials, result.Width)
	if !ok {
		return analyseExecution{}, fmt.Errorf(
			"analyse attempted request work cannot be represented")
	}
	measuredComplete, ok := checkedProduct(
		result.Execution.CompletedTrials, result.Width)
	if !ok {
		return analyseExecution{}, fmt.Errorf(
			"analyse rank-complete request work cannot be represented")
	}
	measuredRetained, ok := checkedProduct(retainedTrials, result.Width)
	if !ok {
		return analyseExecution{}, fmt.Errorf(
			"analyse retained request work cannot be represented")
	}
	execution := analyseExecution{
		sharedExecution: sharedExecution{
			Connections: executionConnections{
				Planned:      result.ConnectionLimit,
				Observed:     result.Execution.Connections,
				Replacements: result.Execution.Replacements,
			},
			Priming: executionPriming{
				Batches: executionBatches{
					Attempted:    result.Execution.WarmupTrials,
					RankComplete: result.Execution.WarmupRankComplete,
					Incomplete:   result.Execution.WarmupIncomplete,
				},
				RequestOperations: executionPrimingOperations{
					Attempted:    result.Execution.WarmupRequestOperations,
					RankComplete: primingComplete,
				},
			},
			RequestOperations: executionRequestOperations{
				Attempted: measuredAttempted, RankComplete: measuredComplete,
				Retained: measuredRetained,
			},
		},
		Trials: analyseExecutionTrials{
			Planned:      result.Plan.Trials,
			Attempted:    result.Execution.AttemptedTrials,
			RankComplete: result.Execution.CompletedTrials,
			Incomplete: result.Execution.AttemptedTrials -
				result.Execution.CompletedTrials,
			Unattempted: result.Plan.Trials - result.Execution.AttemptedTrials,
			Retained:    retainedTrials,
			Excluded:    result.Execution.CompletedTrials - retainedTrials,
		},
		Cycles: analyseExecutionCycles{
			Planned:   result.Plan.Cycles,
			Attempted: result.Execution.AttemptedCycles,
			Complete:  result.Execution.CompleteCycles,
			Incomplete: result.Execution.AttemptedCycles -
				result.Execution.CompleteCycles,
			Unattempted: result.Plan.Cycles - result.Execution.AttemptedCycles,
			Retained:    retainedCycles,
		},
	}
	if err := validateAnalyseExecution(
		execution, result.Width, primingWidth); err != nil {
		return analyseExecution{}, err
	}
	return execution, nil
}

func analyseEvidenceConnectionCount(input analyseEvidence) int {
	connections := make(map[int]bool)
	for _, cycle := range input.Cycles {
		for _, trial := range cycle.Trials {
			if trial.Connection >= 0 {
				connections[trial.Connection] = true
			}
		}
	}
	return len(connections)
}

func validateAnalyseEvidence(input analyseEvidence) (
	analyseRankEvidence, analyseValidity,
) {
	invalid := func(code analyseValidityCode, detail string) (
		analyseRankEvidence, analyseValidity,
	) {
		return analyseRankEvidence{}, analyseValidity{Code: code, Detail: detail}
	}
	if input.Issue != nil {
		return invalid(input.Issue.Code, input.Issue.Detail)
	}
	if input.Width < 2 || input.Width > 8 {
		return invalid(analyseInvalidWidth,
			fmt.Sprintf("request count %d is outside the supported range 2..8",
				input.Width))
	}
	minimum := analysePairResolutionFloor(input.Width)
	if len(input.Cycles) < minimum {
		return invalid(analyseTooFewCycles,
			fmt.Sprintf("%d complete cycles are required for %d requests; got %d",
				minimum, input.Width, len(input.Cycles)))
	}
	rankCount, ok := checkedProduct(len(input.Cycles), input.Width, input.Width)
	if !ok || rankCount > analyseMaximumOperations {
		return invalid(analyseEvidenceTooLarge,
			"complete-cycle evidence is too large to analyse safely")
	}
	baseCount, ok := checkedProduct(len(input.Cycles), input.Width)
	if !ok {
		return invalid(analyseEvidenceTooLarge,
			"complete-cycle evidence is too large to analyse safely")
	}

	evidence := analyseRankEvidence{
		width: input.Width, cycles: len(input.Cycles),
		ranks: make([]int, rankCount), base: make([]int, baseCount),
		rankCounts: newRankMatrix(input.Width),
		statuses:   make([]map[int]int, input.Width),
	}
	for request := range evidence.statuses {
		evidence.statuses[request] = make(map[int]int)
	}
	for cycleIndex, cycle := range input.Cycles {
		if len(cycle.Trials) != input.Width {
			return invalid(analyseIncompleteCycle,
				fmt.Sprintf("cycle %d has %d trials; want %d", cycleIndex,
					len(cycle.Trials), input.Width))
		}
		byShift := make([]*analyseTrialEvidence, input.Width)
		connection := -1
		for trialIndex := range cycle.Trials {
			trial := &cycle.Trials[trialIndex]
			if trial.Shift < 0 || trial.Shift >= input.Width ||
				byShift[trial.Shift] != nil {
				return invalid(analyseInvalidShift,
					fmt.Sprintf("cycle %d does not contain each shift exactly once",
						cycleIndex))
			}
			if connection < 0 {
				connection = trial.Connection
			}
			if trial.Connection < 0 || trial.Connection != connection {
				return invalid(analyseMixedConnections,
					fmt.Sprintf("cycle %d trials do not share one connection",
						cycleIndex))
			}
			if !analysePermutation(trial.Arrangement, input.Width) {
				return invalid(analyseInvalidArrangement,
					fmt.Sprintf("cycle %d shift %d has an invalid arrangement",
						cycleIndex, trial.Shift))
			}
			if !analysePermutation(trial.Ranks, input.Width) {
				return invalid(analyseInvalidRanks,
					fmt.Sprintf("cycle %d shift %d has invalid arrival ranks",
						cycleIndex, trial.Shift))
			}
			if len(trial.Statuses) != input.Width {
				return invalid(analyseInvalidStatus,
					fmt.Sprintf("cycle %d shift %d has incomplete response statuses",
						cycleIndex, trial.Shift))
			}
			for _, status := range trial.Statuses {
				if status < 100 || status > 999 {
					return invalid(analyseInvalidStatus,
						fmt.Sprintf("cycle %d shift %d has an invalid response status",
							cycleIndex, trial.Shift))
				}
			}
			byShift[trial.Shift] = trial
		}
		base := byShift[0].Arrangement
		for position, request := range base {
			evidence.base[cycleIndex*input.Width+position] = request
		}
		for shift, trial := range byShift {
			for position := range input.Width {
				request := base[(position+shift)%input.Width]
				if trial.Arrangement[position] != request {
					return invalid(analyseInvalidArrangement,
						fmt.Sprintf("cycle %d shift %d is not the registered rotation",
							cycleIndex, shift))
				}
				offset := (cycleIndex*input.Width+shift)*input.Width + position
				rank := trial.Ranks[request]
				evidence.ranks[offset] = rank
				evidence.rankCounts[request][rank]++
				evidence.statuses[request][trial.Statuses[request]]++
			}
		}
	}
	return evidence, analyseValidity{Available: true, Code: analyseValid}
}

func analyseObservedEffects(evidence analyseRankEvidence) []analyseRequestEffect {
	workspace := newAnalyseRankWorkspace(evidence.width)
	_, _ = workspace.statistic(evidence, evidence.base)
	trials := evidence.cycles * evidence.width
	result := make([]analyseRequestEffect, evidence.width)
	for request, sum := range workspace.rankSums {
		mean := float64(sum) / float64(trials)
		result[request] = analyseRequestEffect{
			Request: request, Samples: trials, MeanRank: mean,
			NormalisedMeanRank: mean / float64(evidence.width-1),
			RankCounts:         slices.Clone(evidence.rankCounts[request]),
		}
		statuses := make([]int, 0, len(evidence.statuses[request]))
		for status := range evidence.statuses[request] {
			statuses = append(statuses, status)
		}
		sort.Ints(statuses)
		for _, status := range statuses {
			result[request].StatusCounts = append(
				result[request].StatusCounts,
				analyseStatusCount{
					Status: status, Count: evidence.statuses[request][status],
				},
			)
		}
	}
	return result
}

func analyseObservedPositionEffects(
	evidence analyseRankEvidence,
) []analysePositionEffect {
	samples := evidence.cycles * evidence.width
	sums := make([]int, evidence.width)
	for cycle := range evidence.cycles {
		for shift := range evidence.width {
			for position := range evidence.width {
				offset := (cycle*evidence.width+shift)*evidence.width + position
				sums[position] += evidence.ranks[offset]
			}
		}
	}
	result := make([]analysePositionEffect, evidence.width)
	for position, sum := range sums {
		result[position] = analysePositionEffect{
			Position: position,
			Samples:  samples,
			MeanRank: float64(sum) / float64(samples),
		}
	}
	return result
}

func analyseReviewResponses(requests []analyseRequestEffect) analyseResponseReview {
	var result analyseResponseReview
	for _, request := range requests {
		if len(request.StatusCounts) > 1 {
			result.Changing = append(result.Changing, request.Request)
		}
		for _, count := range request.StatusCounts {
			if count.Status < 200 || count.Status >= 300 {
				result.Non2xx = append(result.Non2xx, request.Request)
				break
			}
		}
	}
	if len(requests) > 1 {
		reference := requests[0].StatusCounts
		different := false
		for _, request := range requests[1:] {
			if !slices.Equal(reference, request.StatusCounts) {
				different = true
				break
			}
		}
		if different {
			for _, request := range requests {
				result.Different = append(result.Different, request.Request)
			}
		}
	}
	return result
}

func analysePermutation(values []int, width int) bool {
	if len(values) != width {
		return false
	}
	seen := make([]bool, width)
	for _, value := range values {
		if value < 0 || value >= width || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func analysePairResolutionFloor(width int) int {
	switch width {
	case 2:
		return 6
	case 3:
		return 7
	case 4:
		return 8
	case 5:
		return 9
	case 6, 7:
		return 10
	case 8:
		return 11
	default:
		return 0
	}
}

func analysePairwiseDecision(
	evidence analyseRankEvidence, alpha float64,
) analysePairResults {
	pairEvidence := analysePairEvidenceFromRanks(evidence)
	pairs := make([]analysePairResult, 0, len(pairEvidence))
	pairIndex := 0
	trials := float64(evidence.width * evidence.cycles)
	for left := 0; left < evidence.width; left++ {
		for right := left + 1; right < evidence.width; right++ {
			observed := pairEvidence[pairIndex]
			total := 0
			for _, contribution := range observed.contributions {
				total += contribution
			}
			rawP, upperBound := analyseExactCycleSignP(observed.contributions)
			pairs = append(pairs, analysePairResult{
				Left: left, Right: right,
				Trials:             int(trials),
				LeftBeforeRight:    observed.leftBeforeRight,
				RightBeforeLeft:    observed.rightBeforeLeft,
				MeanRankDifference: float64(total) / trials,
				RawP:               rawP, RawPUpperBound: upperBound,
			})
			pairIndex++
		}
	}
	adjusted := analyseHolmPairs(pairs)
	for index := range adjusted {
		adjusted[index].Selected = adjusted[index].AdjustedP <= alpha
	}
	return analysePairResults{State: analysePairsPerformed, Pairs: adjusted}
}

type analysePairEvidence struct {
	contributions   []int
	leftBeforeRight int
	rightBeforeLeft int
}

func analysePairEvidenceFromRanks(
	evidence analyseRankEvidence,
) []analysePairEvidence {
	pairCount := evidence.width * (evidence.width - 1) / 2
	result := make([]analysePairEvidence, pairCount)
	trialRanks := make([]int, evidence.width)
	for cycle := range evidence.cycles {
		cycleContributions := make([]int, pairCount)
		for shift := range evidence.width {
			for position := range evidence.width {
				request := int(evidence.base[cycle*evidence.width+
					(position+shift)%evidence.width])
				trialRanks[request] = int(evidence.ranks[(cycle*evidence.width+shift)*evidence.width+position])
			}
			pair := 0
			for left := 0; left < evidence.width; left++ {
				for right := left + 1; right < evidence.width; right++ {
					cycleContributions[pair] += trialRanks[left] - trialRanks[right]
					if trialRanks[left] < trialRanks[right] {
						result[pair].leftBeforeRight++
					} else {
						result[pair].rightBeforeLeft++
					}
					pair++
				}
			}
		}
		for pair, contribution := range cycleContributions {
			result[pair].contributions = append(
				result[pair].contributions, contribution,
			)
		}
	}
	return result
}

func analyseExactCycleSignP(contributions []int) (float64, bool) {
	rangeLimit := 0
	observed := 0
	for _, contribution := range contributions {
		rangeLimit += analyseAbs(contribution)
		observed += contribution
	}
	offset := rangeLimit
	probabilities := make([]float64, 2*rangeLimit+1)
	next := make([]float64, len(probabilities))
	probabilities[offset] = 1
	currentRange := 0
	for _, contribution := range contributions {
		nextRange := currentRange + analyseAbs(contribution)
		clear(next[offset-nextRange : offset+nextRange+1])
		for value := -currentRange; value <= currentRange; value++ {
			probability := probabilities[offset+value]
			if probability == 0 {
				continue
			}
			next[offset+value+contribution] += probability / 2
			next[offset+value-contribution] += probability / 2
		}
		probabilities, next = next, probabilities
		currentRange = nextRange
	}
	extreme := analyseAbs(observed)
	p := 0.0
	for value := -rangeLimit; value <= rangeLimit; value++ {
		if analyseAbs(value) >= extreme {
			p += probabilities[offset+value]
		}
	}
	p = math.Min(1, p)
	if p < analysePairPReportingFloor {
		return analyseCycleSignUpperBound(contributions, observed), true
	}
	return p, false
}

func analyseCycleSignUpperBound(contributions []int, observed int) float64 {
	sumSquares := 0.0
	for _, contribution := range contributions {
		value := float64(contribution)
		sumSquares += value * value
	}
	if sumSquares == 0 {
		return 1
	}
	exponent := -float64(observed*observed) / (2 * sumSquares)
	// The factor comfortably covers binary64 evaluation error while retaining
	// the conservative two-sided Hoeffding bound.
	bound := 2 * math.Exp(exponent) * (1 + 1e-12)
	return math.Min(1, math.Max(analysePairPReportingFloor, bound))
}

func analyseAbs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func analyseHolmPairs(pairs []analysePairResult) []analysePairResult {
	result := append([]analysePairResult(nil), pairs...)
	sort.Slice(result, func(left, right int) bool {
		if result[left].RawP != result[right].RawP {
			return result[left].RawP < result[right].RawP
		}
		if result[left].Left != result[right].Left {
			return result[left].Left < result[right].Left
		}
		return result[left].Right < result[right].Right
	})
	previous := 0.0
	previousUpperBound := false
	for index := range result {
		adjusted := float64(len(result)-index) * result[index].RawP
		upperBound := result[index].RawPUpperBound
		if previous > adjusted {
			adjusted = previous
			upperBound = previousUpperBound
		} else if previous == adjusted {
			upperBound = upperBound || previousUpperBound
		}
		adjusted = math.Min(1, adjusted)
		result[index].AdjustedP = adjusted
		result[index].AdjustedPUpperBound = upperBound
		previous = adjusted
		previousUpperBound = upperBound
	}
	return result
}
