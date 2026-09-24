package main

import (
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/tantosec/tturl/internal/ranking"
	"github.com/tantosec/tturl/tth2"
)

type detectStatusCount struct {
	Status int
	Count  int
}

type detectRequestEvidence struct {
	Request        int
	Participations int
	RankSum        int
	StatusCounts   []detectStatusCount
}

type detectResponseReview struct {
	Changing          []int
	Non2xx            []int
	Different         []int
	BaselineChanging  []int
	BaselineNon2xx    []int
	BaselineDifferent []int
}

type detectExecutionEvidence struct {
	Recorded               bool
	ConnectionsObserved    int
	ConnectionReplacements int
	PrimingAttempts        []detectPrimingAttempt
	AcquisitionFailures    []detectAcquisitionFailure
	ComparisonFailures     []detectComparisonFailure
}

type detectPrimingAttempt struct {
	Worker            int
	Attempt           int
	Connection        tth2.ConnectionID
	Replacement       bool
	RequestOperations int
	Complete          bool
	Err               error
}

func (e detectExecutionEvidence) primingSummary() (
	requestOperations, complete, incomplete int,
) {
	for _, attempt := range e.PrimingAttempts {
		requestOperations += attempt.RequestOperations
		if attempt.Complete {
			complete++
		} else {
			incomplete++
		}
	}
	return requestOperations, complete, incomplete
}

type detectAcquisitionFailure struct {
	Worker      int
	Replacement bool
	Err         error
}

type detectComparisonFailure struct {
	Worker         int
	Connection     tth2.ConnectionID
	Participations int
	Items          []detectEvidenceRef
	Err            error
}

type detectRunResult struct {
	Ranking     ranking.OutlierResult
	Requests    []detectRequestEvidence
	Baselines   []detectRequestEvidence
	Responses   detectResponseReview
	Execution   detectExecutionEvidence
	Unavailable bool
	Reason      error
	Completion  runCompletion
}

func (result detectRunResult) executionAccounting(
	maximum, plannedConnections int,
) (detectExecution, error) {
	var maximumValue *int
	if maximum >= 0 {
		maximumValue = new(maximum)
	}
	primingAttempted, primingComplete, primingBatchesComplete := 0, 0, 0
	for _, attempt := range result.Execution.PrimingAttempts {
		if attempt.RequestOperations < 0 ||
			primingAttempted > math.MaxInt-attempt.RequestOperations {
			return detectExecution{}, fmt.Errorf(
				"execution detect priming work cannot be represented")
		}
		primingAttempted += attempt.RequestOperations
		if attempt.Complete {
			if primingComplete > math.MaxInt-attempt.RequestOperations {
				return detectExecution{}, fmt.Errorf(
					"execution detect rank-complete priming work cannot be represented")
			}
			primingBatchesComplete++
			primingComplete += attempt.RequestOperations
		}
	}
	failedOperations := 0
	for _, failure := range result.Execution.ComparisonFailures {
		if failure.Participations < 0 ||
			failure.Participations != len(failure.Items) {
			return detectExecution{}, fmt.Errorf(
				"execution detect failed comparison detail is inconsistent")
		}
		for _, item := range failure.Items {
			limit := len(result.Requests)
			if item.role == requestGroupBaseline {
				limit = len(result.Baselines)
			} else if item.role != requestGroupCandidate {
				return detectExecution{}, fmt.Errorf(
					"execution detect failed comparison has an unknown request role")
			}
			if item.index < 0 || item.index >= limit {
				return detectExecution{}, fmt.Errorf(
					"execution detect failed comparison request is out of range")
			}
		}
		if failedOperations > math.MaxInt-failure.Participations {
			return detectExecution{}, fmt.Errorf(
				"execution detect failed comparison work cannot be represented")
		}
		failedOperations += failure.Participations
	}
	successfulOperations, err := detectDetailedParticipations(
		result.Requests, result.Baselines)
	if err != nil {
		return detectExecution{}, err
	}
	if !result.Execution.Recorded && result.Requests == nil &&
		result.Baselines == nil {
		for _, item := range result.Ranking.Items {
			if item.Samples < 0 ||
				successfulOperations > math.MaxInt-item.Samples {
				return detectExecution{}, fmt.Errorf(
					"execution detect solver work cannot be represented")
			}
			successfulOperations += item.Samples
		}
	}
	if successfulOperations != result.Ranking.Participations {
		return detectExecution{}, fmt.Errorf(
			"execution detect request evidence has %d participations; solver reports %d",
			successfulOperations, result.Ranking.Participations)
	}
	execution := detectExecution{
		sharedExecution: sharedExecution{
			Connections: executionConnections{
				Planned:      plannedConnections,
				Observed:     result.Execution.ConnectionsObserved,
				Replacements: result.Execution.ConnectionReplacements,
			},
			Priming: executionPriming{
				Batches: executionBatches{
					Attempted:    len(result.Execution.PrimingAttempts),
					RankComplete: primingBatchesComplete,
					Incomplete: len(result.Execution.PrimingAttempts) -
						primingBatchesComplete,
				},
				RequestOperations: executionPrimingOperations{
					Attempted: primingAttempted, RankComplete: primingComplete,
				},
			},
			RequestOperations: executionRequestOperations{
				Attempted:    result.Ranking.Participations + failedOperations,
				RankComplete: result.Ranking.Participations,
				Retained:     result.Ranking.Participations,
			},
		},
		Comparisons: detectExecutionComparisons{
			Maximum: maximumValue,
			Attempted: result.Ranking.Comparisons +
				len(result.Execution.ComparisonFailures),
			RankComplete: result.Ranking.Comparisons,
			Incomplete:   len(result.Execution.ComparisonFailures),
		},
	}
	if err := validateDetectExecution(execution); err != nil {
		return detectExecution{}, err
	}
	return execution, nil
}

func detectDetailedParticipations(groups ...[]detectRequestEvidence) (int, error) {
	total := 0
	for _, requests := range groups {
		for _, request := range requests {
			counted := 0
			for _, status := range request.StatusCounts {
				if status.Count < 0 || counted > math.MaxInt-status.Count {
					return 0, fmt.Errorf(
						"execution detect response counts cannot be represented")
				}
				counted += status.Count
			}
			if request.Participations < 0 || counted != request.Participations {
				return 0, fmt.Errorf(
					"execution detect request participation detail is inconsistent")
			}
			if total > math.MaxInt-request.Participations {
				return 0, fmt.Errorf(
					"execution detect request participations cannot be represented")
			}
			total += request.Participations
		}
	}
	return total, nil
}

// detectEvidenceCollector retains response facts from successful comparisons.
// Compare calls may overlap, so every mutation is protected by mu.
type detectEvidenceCollector struct {
	mu                     sync.Mutex
	statuses               []map[int]int
	rankSums               []int
	observations           []int
	baselineStatuses       []map[int]int
	baselineRankSums       []int
	baselineObservations   []int
	connections            map[tth2.ConnectionID]struct{}
	replacementConnections map[tth2.ConnectionID]struct{}
	comparisons            int
	participations         int
	priming                []detectPrimingAttempt
	acquisitions           []detectAcquisitionFailure
	failures               []detectComparisonFailure
}

type detectEvidenceRef struct {
	role  requestGroupRole
	index int
}

func newDetectEvidenceCollector(
	requests int,
	baselineCounts ...int,
) *detectEvidenceCollector {
	baselines := 0
	if len(baselineCounts) > 0 {
		baselines = baselineCounts[0]
	}
	statuses := make([]map[int]int, requests)
	for request := range statuses {
		statuses[request] = make(map[int]int)
	}
	baselineStatuses := make([]map[int]int, baselines)
	for baseline := range baselineStatuses {
		baselineStatuses[baseline] = make(map[int]int)
	}
	return &detectEvidenceCollector{
		statuses:               statuses,
		rankSums:               make([]int, requests),
		observations:           make([]int, requests),
		baselineStatuses:       baselineStatuses,
		baselineRankSums:       make([]int, baselines),
		baselineObservations:   make([]int, baselines),
		connections:            make(map[tth2.ConnectionID]struct{}),
		replacementConnections: make(map[tth2.ConnectionID]struct{}),
	}
}

func (c *detectEvidenceCollector) connectionAcquired(
	id tth2.ConnectionID, replacement bool,
) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id != 0 && replacement {
		c.replacementConnections[id] = struct{}{}
	}
}

func (c *detectEvidenceCollector) primingAttempt(attempt detectPrimingAttempt) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.priming = append(c.priming, attempt)
	if attempt.Connection != 0 {
		c.connections[attempt.Connection] = struct{}{}
	}
}

func (c *detectEvidenceCollector) acquisitionFailed(
	worker int, replacement bool, err error,
) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.acquisitions = append(c.acquisitions, detectAcquisitionFailure{
		Worker: worker, Replacement: replacement, Err: err,
	})
}

func (c *detectEvidenceCollector) comparisonFailed(
	worker int,
	connection tth2.ConnectionID,
	items []detectEvidenceRef,
	err error,
) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures = append(c.failures, detectComparisonFailure{
		Worker: worker, Connection: connection,
		Participations: len(items),
		Items:          append([]detectEvidenceRef(nil), items...),
		Err:            err,
	})
	if connection != 0 {
		c.connections[connection] = struct{}{}
	}
}

func (c *detectEvidenceCollector) observe(
	byPosition []ranking.ItemID, batch *tth2.BatchResult,
) {
	refs := make([]detectEvidenceRef, len(byPosition))
	for position, id := range byPosition {
		if !id.IsCandidate() {
			panic("detect candidate evidence received a control")
		}
		refs[position] = detectEvidenceRef{
			role: requestGroupCandidate, index: int(id),
		}
	}
	c.observeResolved(refs, batch)
}

func (c *detectEvidenceCollector) observeResolved(
	byPosition []detectEvidenceRef,
	batch *tth2.BatchResult,
) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.comparisons++
	c.participations += len(byPosition)
	if batch.Connection != 0 {
		c.connections[batch.Connection] = struct{}{}
	}
	for position, ref := range byPosition {
		result := batch.Results[position]
		if ref.role == requestGroupBaseline {
			c.baselineStatuses[ref.index][result.Response.StatusCode]++
			c.baselineRankSums[ref.index] += result.ArrivalRank
			c.baselineObservations[ref.index]++
			continue
		}
		c.statuses[ref.index][result.Response.StatusCode]++
		c.rankSums[ref.index] += result.ArrivalRank
		c.observations[ref.index]++
	}
}

func (c *detectEvidenceCollector) result(
	result ranking.OutlierResult,
) (detectRunResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.comparisons != result.Comparisons ||
		c.participations != result.Participations {
		return detectRunResult{}, fmt.Errorf(
			"internal consistency error: detect collected %d comparisons and "+
				"%d request participations; solver reported %d and %d",
			c.comparisons, c.participations,
			result.Comparisons, result.Participations)
	}
	if err := validateDetectSolverResult(result, len(c.statuses)); err != nil {
		return detectRunResult{}, err
	}
	return c.resultLocked(result, false, nil, completedRun()), nil
}

func (c *detectEvidenceCollector) unavailable(
	reason error, completion runCompletion,
) detectRunResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	items := make([]ranking.ItemStat, len(c.statuses))
	for request, observations := range c.observations {
		mean := math.NaN()
		if observations > 0 {
			mean = float64(c.rankSums[request]) / float64(observations)
		}
		items[request] = ranking.ItemStat{
			Samples: observations, MeanRank: mean,
		}
	}
	result := ranking.OutlierResult{
		Index: -1, Items: items,
		Cost: ranking.Cost{
			Comparisons: c.comparisons, Participations: c.participations,
		},
	}
	return c.resultLocked(result, true, reason, completion)
}

func (c *detectEvidenceCollector) resultLocked(
	result ranking.OutlierResult,
	unavailable bool,
	reason error,
	completion runCompletion,
) detectRunResult {
	requests := make([]detectRequestEvidence, len(c.statuses))
	fillDetectRequestEvidence(requests, c.statuses, c.rankSums)
	baselines := make([]detectRequestEvidence, len(c.baselineStatuses))
	fillDetectRequestEvidence(baselines, c.baselineStatuses, c.baselineRankSums)
	execution := detectExecutionEvidence{
		Recorded: true, ConnectionsObserved: len(c.connections),
		PrimingAttempts: append([]detectPrimingAttempt(nil), c.priming...),
		AcquisitionFailures: append(
			[]detectAcquisitionFailure(nil), c.acquisitions...),
		ComparisonFailures: append(
			[]detectComparisonFailure(nil), c.failures...),
	}
	for connection := range c.connections {
		if _, ok := c.replacementConnections[connection]; ok {
			execution.ConnectionReplacements++
		}
	}
	return detectRunResult{
		Ranking:     result,
		Requests:    requests,
		Baselines:   baselines,
		Responses:   reviewDetectResponses(requests, baselines),
		Execution:   execution,
		Unavailable: unavailable, Reason: reason, Completion: completion,
	}
}

func fillDetectRequestEvidence(
	requests []detectRequestEvidence,
	allCounts []map[int]int,
	rankSums []int,
) {
	for request, counts := range allCounts {
		statuses := make([]int, 0, len(counts))
		for status := range counts {
			statuses = append(statuses, status)
		}
		sort.Ints(statuses)
		requests[request].Request = request
		requests[request].RankSum = rankSums[request]
		for _, status := range statuses {
			count := counts[status]
			requests[request].Participations += count
			requests[request].StatusCounts = append(
				requests[request].StatusCounts,
				detectStatusCount{Status: status, Count: count})
		}
	}
}

func validateDetectSolverResult(result ranking.OutlierResult, requests int) error {
	if len(result.Items) != requests {
		return fmt.Errorf(
			"internal consistency error: detect result has %d items for %d requests",
			len(result.Items), requests)
	}
	switch result.Outcome {
	case ranking.OutlierInconclusive, ranking.OutlierNegative:
		if result.Index != -1 {
			return fmt.Errorf(
				"internal consistency error: detect result has unexpected request %d",
				result.Index)
		}
	case ranking.OutlierFound:
		if result.Index < 0 || result.Index >= requests {
			return fmt.Errorf(
				"internal consistency error: detect selected request %d outside 0..%d",
				result.Index, max(0, requests-1))
		}
	default:
		return fmt.Errorf("internal consistency error: unknown detect outcome %q",
			result.Outcome)
	}
	return nil
}

func reviewDetectResponses(
	requests []detectRequestEvidence,
	baselines ...[]detectRequestEvidence,
) detectResponseReview {
	var review detectResponseReview
	var reference []detectStatusCount
	different := false
	baselineRequests := []detectRequestEvidence(nil)
	if len(baselines) > 0 {
		baselineRequests = baselines[0]
	}
	for roleIndex, roleRequests := range [][]detectRequestEvidence{
		requests, baselineRequests,
	} {
		for _, request := range roleRequests {
			if len(request.StatusCounts) == 0 {
				continue
			}
			non2xx := false
			for _, count := range request.StatusCounts {
				if count.Status < 200 || count.Status >= 300 {
					non2xx = true
				}
			}
			if non2xx {
				if roleIndex == 0 {
					review.Non2xx = append(review.Non2xx, request.Request)
				} else {
					review.BaselineNon2xx = append(
						review.BaselineNon2xx, request.Request)
				}
			}
			if len(request.StatusCounts) > 1 {
				if roleIndex == 0 {
					review.Changing = append(review.Changing, request.Request)
				} else {
					review.BaselineChanging = append(
						review.BaselineChanging, request.Request)
				}
			}
			if reference == nil {
				reference = request.StatusCounts
			} else if !equalStatusDistributions(reference, request.StatusCounts) {
				different = true
			}
		}
	}
	if different {
		for _, request := range requests {
			if request.Participations > 0 {
				review.Different = append(review.Different, request.Request)
			}
		}
		for _, request := range baselineRequests {
			if request.Participations > 0 {
				review.BaselineDifferent = append(
					review.BaselineDifferent, request.Request)
			}
		}
	}
	return review
}

// equalStatusDistributions compares exact proportions without multiplication
// overflow. Dividing every count by the vector's greatest common divisor gives
// a canonical distribution even when adaptive participation counts differ.
func equalStatusDistributions(left, right []detectStatusCount) bool {
	if len(left) != len(right) {
		return false
	}
	divisor := func(counts []detectStatusCount) int {
		value := 0
		for _, count := range counts {
			other := count.Count
			for other != 0 {
				value, other = other, value%other
			}
		}
		return value
	}
	ld, rd := divisor(left), divisor(right)
	for i, count := range left {
		if count.Status != right[i].Status || count.Count/ld != right[i].Count/rd {
			return false
		}
	}
	return true
}
