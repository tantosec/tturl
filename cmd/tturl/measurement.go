package main

import (
	"fmt"
	"math"
	"slices"

	"github.com/tantosec/tturl/tth2"
)

// measurePlan is the resolved experiment configuration shared by friendly and
// structured reporting. Planned workloads are HTTP request operations, not a
// claim about what reached the wire.
type measurePlan struct {
	requested, planned int
	budget             measureBudget
	warmup, width      int
	warmupWidth        int
	arrangement        tth2.ArrangementPolicy
	padding            []PaddingRecord
	delivery           deliveryPlan
	bodyRelease        bodyReleasePlan
	responseLimits     responseLimits
	runTimeout         runTimeout
	insecure           bool
}

type plannedWorkload struct {
	measured, initialWarmup, total int
}

func (p measurePlan) workload(conns int) (plannedWorkload, error) {
	measured, ok := checkedProduct(p.planned, p.width)
	if !ok {
		return plannedWorkload{}, fmt.Errorf(
			"planned measured request workload is too large")
	}
	warmupWidth := p.warmupWidth
	if p.warmup > 0 && warmupWidth == 0 {
		warmupWidth = p.width
	}
	warmup, ok := checkedProduct(p.warmup, conns, warmupWidth)
	if !ok {
		return plannedWorkload{}, fmt.Errorf(
			"planned warmup request workload is too large")
	}
	if measured > math.MaxInt-warmup {
		return plannedWorkload{}, fmt.Errorf("planned request workload is too large")
	}
	return plannedWorkload{
		measured: measured, initialWarmup: warmup,
		total: measured + warmup,
	}, nil
}

func checkedProduct(values ...int) (int, bool) {
	product := 1
	for _, value := range values {
		if value != 0 && product > math.MaxInt/value {
			return 0, false
		}
		product *= value
	}
	return product, true
}

// measurement retains the trial-level facts a TrialSummary deliberately folds
// away. It is the descriptive record for one bounded measure run.
type measurement struct {
	summary          *tth2.TrialSummary
	transportSummary *tth2.TrialSummary
	runErr           error
	completion       runCompletion

	requested        int
	planned          int
	attempted        int
	complete         int
	incomplete       int
	retained         int
	excluded         int
	warmups          int
	warmupComplete   int
	warmupIncomplete int
	warmupPerConn    int
	warmupWidth      int
	width            int

	connsPlanned int
	connsSeen    map[int]bool

	arrangement   tth2.ArrangementPolicy
	cycles        map[int]*measuredCycle
	cycleTally    cycleCounts
	statusCounts  []map[int]int
	assignments   [][]int
	pairwise      [][]int
	batchFailures []measurementBatchFailure
}

type measurementBatchFailure struct {
	Index      int
	Warmup     bool
	Connection int
	Err        error
}

// measuredCycle holds compact rank counts until every shift in one rotation
// cycle has arrived. A failed, duplicate, or missing shift excludes the cycle.
type measuredCycle struct {
	received       int
	completeTrials int
	incomplete     bool
	shifts         map[int]bool
	requestRanks   [][]int
	positionRanks  [][]int
	assignments    [][]int
	statusCounts   []map[int]int
	pairwise       [][]int
}

func newMeasurement(
	requested, planned, width, conns int,
	arrangement tth2.ArrangementPolicy,
) *measurement {
	return &measurement{
		requested: requested, planned: planned,
		width:        width,
		connsPlanned: conns, connsSeen: make(map[int]bool),
		arrangement: arrangement, cycles: make(map[int]*measuredCycle),
		summary: &tth2.TrialSummary{
			RankCounts:         newRankMatrix(width),
			PositionRankCounts: newRankMatrix(width),
			ResetCounts:        make([]int, width),
		},
		statusCounts: newStatusCounts(width),
		assignments:  newRankMatrix(width),
		pairwise:     newRankMatrix(width),
	}
}

// observe retains execution evidence and builds the opinionated measurement.
// Rotation counts are committed only when a whole cycle is complete.
func (m *measurement) observe(trial *tth2.Trial) error {
	if trial.Conn >= 0 {
		m.connsSeen[trial.Conn] = true
	}
	if trial.Err != nil {
		m.batchFailures = append(m.batchFailures, measurementBatchFailure{
			Index: trial.Index, Warmup: trial.Warmup,
			Connection: trial.Conn, Err: trial.Err,
		})
	}
	if trial.Warmup {
		m.warmups++
		if trial.Err == nil {
			m.warmupComplete++
		} else {
			m.warmupIncomplete++
		}
		return nil
	}
	m.attempted++
	complete := trial.Err == nil
	if complete {
		if err := m.validateCompleteTrial(trial); err != nil {
			return err
		}
		m.complete++
	} else {
		m.incomplete++
	}

	if m.arrangement != tth2.ArrangeRotate {
		if complete {
			m.tallyTrial(
				m.summary, m.statusCounts, m.assignments, m.pairwise, trial)
			m.retained++
		}
		return nil
	}
	cycle := m.cycles[trial.Cycle]
	if cycle == nil {
		cycle = &measuredCycle{
			shifts:        make(map[int]bool),
			requestRanks:  newRankMatrix(m.width),
			positionRanks: newRankMatrix(m.width),
			assignments:   newRankMatrix(m.width),
			statusCounts:  newStatusCounts(m.width),
			pairwise:      newRankMatrix(m.width),
		}
		m.cycles[trial.Cycle] = cycle
		m.cycleTally.attempted++
	}
	cycle.received++
	if trial.Shift < 0 || trial.Shift >= m.width || cycle.shifts[trial.Shift] {
		cycle.incomplete = true
	} else {
		cycle.shifts[trial.Shift] = true
	}
	if complete {
		cycle.completeTrials++
		m.tallyTrial(&tth2.TrialSummary{
			RankCounts:         cycle.requestRanks,
			PositionRankCounts: cycle.positionRanks,
		}, cycle.statusCounts, cycle.assignments, cycle.pairwise, trial)
	} else {
		cycle.incomplete = true
	}
	if cycle.received == m.width {
		m.finaliseCycle(trial.Cycle, cycle)
	}
	return nil
}

func (m *measurement) finish(rawSummary *tth2.TrialSummary, err error) {
	m.transportSummary = rawSummary
	for id, cycle := range m.cycles {
		m.finaliseCycle(id, cycle)
	}
	// The incrementally tallied summary contains only trials that passed this
	// collector's rank validation. The transport summary may include a trial
	// that exposed the invariant failure which stopped collection, so retain
	// only its independent reset accounting.
	m.summary.FailedTrials = m.incomplete
	copy(m.summary.ResetCounts, rawSummary.ResetCounts)
	m.runErr = err
}

func (m *measurement) validateCompleteTrial(trial *tth2.Trial) error {
	if len(trial.Results) != m.width || len(trial.Arrangement) != m.width {
		return fmt.Errorf(
			"internal consistency error: rank-complete trial %d has width %d/%d, want %d",
			trial.Index, len(trial.Results), len(trial.Arrangement), m.width)
	}
	if _, ok := trial.FullArrivalOrder(); !ok {
		return fmt.Errorf(
			"internal consistency error: rank-complete trial %d has invalid arrival order",
			trial.Index)
	}
	seenRequests := make([]bool, m.width)
	for request, result := range trial.Results {
		if result.Response.StatusCode < 100 || result.Response.StatusCode > 999 {
			return fmt.Errorf(
				"internal consistency error: rank-complete trial %d request %d has invalid HTTP status %d",
				trial.Index, request, result.Response.StatusCode)
		}
	}
	for _, request := range trial.Arrangement {
		if request < 0 || request >= m.width || seenRequests[request] {
			return fmt.Errorf(
				"internal consistency error: rank-complete trial %d has invalid arrangement",
				trial.Index)
		}
		seenRequests[request] = true
	}
	return nil
}

func (m *measurement) tallyTrial(
	summary *tth2.TrialSummary,
	statuses []map[int]int,
	assignments [][]int,
	pairwise [][]int,
	trial *tth2.Trial,
) {
	summary.Trials++
	for request, result := range trial.Results {
		summary.RankCounts[request][result.ArrivalRank]++
		statuses[request][result.Response.StatusCode]++
	}
	for position, request := range trial.Arrangement {
		rank := trial.Results[request].ArrivalRank
		summary.PositionRankCounts[position][rank]++
		assignments[request][position]++
	}
	for left, leftResult := range trial.Results {
		for right, rightResult := range trial.Results {
			if leftResult.ArrivalRank < rightResult.ArrivalRank {
				pairwise[left][right]++
			}
		}
	}
}

func (m *measurement) finaliseCycle(id int, cycle *measuredCycle) {
	complete := cycle.received == m.width &&
		cycle.completeTrials == m.width &&
		len(cycle.shifts) == m.width && !cycle.incomplete
	if complete {
		addRankMatrix(m.summary.RankCounts, cycle.requestRanks)
		addRankMatrix(m.summary.PositionRankCounts, cycle.positionRanks)
		addRankMatrix(m.assignments, cycle.assignments)
		addRankMatrix(m.pairwise, cycle.pairwise)
		addStatusCounts(m.statusCounts, cycle.statusCounts)
		m.summary.Trials += m.width
		m.retained += m.width
		m.cycleTally.retained++
	} else {
		m.excluded += cycle.completeTrials
		m.cycleTally.incomplete++
	}
	delete(m.cycles, id)
}

func addRankMatrix(destination, source [][]int) {
	for row := range destination {
		for rank := range destination[row] {
			destination[row][rank] += source[row][rank]
		}
	}
}

func newRankMatrix(width int) [][]int {
	matrix := make([][]int, width)
	for row := range matrix {
		matrix[row] = make([]int, width)
	}
	return matrix
}

func newStatusCounts(width int) []map[int]int {
	counts := make([]map[int]int, width)
	for request := range counts {
		counts[request] = make(map[int]int)
	}
	return counts
}

func addStatusCounts(destination, source []map[int]int) {
	for request, counts := range source {
		for status, count := range counts {
			destination[request][status] += count
		}
	}
}

func (m *measurement) reconcile() error {
	stats := requestStats(m.summary)
	transportStats := stats
	if m.arrangement != tth2.ArrangeRotate && m.transportSummary != nil {
		transportStats = requestStats(m.transportSummary)
	}
	for request, counts := range m.statusCounts {
		total := 0
		for _, count := range counts {
			total += count
		}
		if total != transportStats[request].N {
			return fmt.Errorf(
				"internal consistency error: request %d has %d HTTP statuses for %d rank samples",
				request, total, transportStats[request].N)
		}
		if !slices.Equal(stats[request].RankCounts,
			transportStats[request].RankCounts) {
			return fmt.Errorf(
				"internal consistency error: request %d rank counts disagree with transport summary",
				request)
		}
	}
	for left, row := range m.pairwise {
		for right, count := range row {
			if left == right {
				if count != 0 {
					return fmt.Errorf(
						"internal consistency error: request %d precedes itself %d times",
						left, count)
				}
				continue
			}
			if count+m.pairwise[right][left] != m.retained {
				return fmt.Errorf(
					"internal consistency error: requests %d and %d have %d pairwise samples for %d retained trials",
					left, right, count+m.pairwise[right][left], m.retained)
			}
		}
	}
	return nil
}

func (m *measurement) connsObserved() int { return len(m.connsSeen) }

func (m *measurement) runCompletion() runCompletion {
	if m.completion.State == "" && m.runErr != nil {
		return failedRun(m.runErr, completionAcquisitionFailed)
	}
	return normaliseCompletion(m.completion)
}

func (m *measurement) replacements() int {
	replacements := 0
	for id := range m.connsSeen {
		if id >= m.connsPlanned {
			replacements++
		}
	}
	return replacements
}

func (m *measurement) execution() (measureExecution, error) {
	primingAttempted, ok := checkedProduct(m.warmups, m.warmupWidth)
	if !ok {
		return measureExecution{}, fmt.Errorf(
			"execution priming request-operation count cannot be represented")
	}
	primingComplete, ok := checkedProduct(m.warmupComplete, m.warmupWidth)
	if !ok {
		return measureExecution{}, fmt.Errorf(
			"execution rank-complete priming request-operation count cannot be represented")
	}
	measuredAttempted, ok := checkedProduct(m.attempted, m.width)
	if !ok {
		return measureExecution{}, fmt.Errorf(
			"execution attempted request-operation count cannot be represented")
	}
	measuredComplete, ok := checkedProduct(m.complete, m.width)
	if !ok {
		return measureExecution{}, fmt.Errorf(
			"execution rank-complete request-operation count cannot be represented")
	}
	measuredRetained, ok := checkedProduct(m.retained, m.width)
	if !ok {
		return measureExecution{}, fmt.Errorf(
			"execution retained request-operation count cannot be represented")
	}
	execution := measureExecution{
		sharedExecution: sharedExecution{
			Connections: executionConnections{
				Planned: m.connsPlanned, Observed: m.connsObserved(),
				Replacements: m.replacements(),
			},
			Priming: executionPriming{
				Batches: executionBatches{
					Attempted: m.warmups, RankComplete: m.warmupComplete,
					Incomplete: m.warmupIncomplete,
				},
				RequestOperations: executionPrimingOperations{
					Attempted: primingAttempted, RankComplete: primingComplete,
				},
			},
			RequestOperations: executionRequestOperations{
				Attempted: measuredAttempted, RankComplete: measuredComplete,
				Retained: measuredRetained,
			},
		},
		Trials: measureExecutionTrials{
			Planned: m.planned, Attempted: m.attempted,
			RankComplete: m.complete, Incomplete: m.incomplete,
			Unattempted: m.planned - m.attempted, Retained: m.retained,
			Excluded: m.excluded,
		},
	}
	if m.arrangement == tth2.ArrangeRotate {
		counts := m.cycleCounts(m.width)
		execution.Cycles = &measureExecutionCycles{
			Planned: counts.planned, Attempted: counts.attempted,
			Retained: counts.retained, Incomplete: counts.incomplete,
			Unattempted: counts.unattempted,
		}
	}
	if err := validateMeasureExecution(execution, m.width, m.warmupWidth); err != nil {
		return measureExecution{}, err
	}
	return execution, nil
}

type cycleCounts struct {
	planned, attempted, retained, incomplete, unattempted int
}

func (m *measurement) cycleCounts(width int) cycleCounts {
	if m.arrangement != tth2.ArrangeRotate || width == 0 {
		return cycleCounts{}
	}
	counts := m.cycleTally
	counts.planned = m.planned / width
	counts.unattempted = max(0, counts.planned-counts.attempted)
	return counts
}

type positionSpreadValues struct {
	min, max, raw, normalised *float64
}

func positionSpread(summary *tth2.TrialSummary) positionSpreadValues {
	stats := positionStats(summary)
	if len(stats) < 2 || stats[0].N == 0 {
		return positionSpreadValues{}
	}
	minimum, maximum := stats[0].MeanRank, stats[0].MeanRank
	for _, position := range stats[1:] {
		minimum = min(minimum, position.MeanRank)
		maximum = max(maximum, position.MeanRank)
	}
	raw := maximum - minimum
	normalised := raw / float64(len(stats)-1)
	return positionSpreadValues{
		min: &minimum, max: &maximum, raw: &raw, normalised: &normalised,
	}
}

func measurePlanWarnings(plan measurePlan) []string {
	warnings := make([]string, 0, 2)
	if plan.warmup == 0 {
		warnings = append(warnings, "no_warmup")
	}
	switch plan.arrangement {
	case tth2.ArrangeRotate:
		if plan.width > 0 && plan.planned/plan.width == 1 {
			warnings = append(warnings, "one_rotation_cycle_planned")
		}
	case tth2.ArrangeRandom:
		warnings = append(warnings, "random_balance_in_expectation")
	case tth2.ArrangeNone:
		warnings = append(warnings, "fixed_positions")
	}
	return warnings
}

func measurementWarnings(measured *measurement) []string {
	warnings := make([]string, 0, 5)
	if measured.summary.FailedTrials > 0 {
		warnings = append(warnings, "incomplete_trials")
	}
	if measured.planned > measured.attempted {
		warnings = append(warnings, "unattempted_trials")
	}
	if measured.replacements() > 0 {
		warnings = append(warnings, "connection_replacement")
	}
	if measured.arrangement == tth2.ArrangeRotate {
		cycles := measured.cycleCounts(measured.width)
		if cycles.incomplete > 0 {
			warnings = append(warnings,
				"incomplete_rotation_cycles_excluded")
		}
		switch cycles.retained {
		case 0:
			warnings = append(warnings, "no_rotation_cycles_retained")
		case 1:
			warnings = append(warnings, "one_rotation_cycle_retained")
		}
	}
	return warnings
}

func responseOutcomeReview(counts []map[int]int) (changed, non2xx []int) {
	changed = make([]int, 0)
	non2xx = make([]int, 0)
	for request, distribution := range counts {
		if len(distribution) > 1 {
			changed = append(changed, request)
		}
		for status := range distribution {
			if status < 200 || status > 299 {
				non2xx = append(non2xx, request)
				break
			}
		}
	}
	return changed, non2xx
}
