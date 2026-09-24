package main

import (
	"fmt"
	"math"
	"slices"

	"github.com/tantosec/tturl/internal/ranking"
)

type jsonDetectResultRecord struct {
	Kind                string                         `json:"kind"`
	Completion          structuredCompletion           `json:"completion"`
	Execution           jsonDetectExecution            `json:"execution"`
	Detection           jsonDetectDetection            `json:"detection"`
	PrimingAttempts     []jsonDetectPrimingAttempt     `json:"priming_attempts"`
	AcquisitionFailures []jsonDetectAcquisitionFailure `json:"acquisition_failures"`
	ComparisonFailures  []jsonDetectComparisonFailure  `json:"comparison_failures"`
}

type jsonDetectDetection struct {
	State                      string                   `json:"state"`
	Finding                    *jsonDetectFinding       `json:"finding,omitempty"`
	CommitThresholdLogEvidence *float64                 `json:"commit_threshold_log_evidence,omitempty"`
	Requests                   []jsonDetectRequestStats `json:"requests,omitempty"`
	StatusDistributionsDiffer  *bool                    `json:"status_distributions_differ,omitempty"`
}

type jsonDetectFinding struct {
	Status       string               `json:"status"`
	RequestID    *int                 `json:"request_id,omitempty"`
	Direction    *string              `json:"direction,omitempty"`
	Guarantee    *jsonDetectGuarantee `json:"guarantee,omitempty"`
	ExcludedEdge *float64             `json:"excluded_edge,omitempty"`
}

type jsonDetectGuarantee struct {
	ErrorDirection string  `json:"error_direction"`
	ErrorRiskLimit float64 `json:"error_risk_limit"`
}

type jsonDetectRequestStats struct {
	RequestID      int                     `json:"request_id"`
	Physical       jsonDetectPhysical      `json:"physical"`
	Inference      *jsonDetectInference    `json:"inference,omitempty"`
	StatusCounts   []structuredStatusCount `json:"status_counts"`
	StatusVaried   bool                    `json:"status_varied"`
	Non2xxObserved bool                    `json:"non_2xx_observed"`
}

type jsonDetectPhysical struct {
	Participations     int      `json:"participations"`
	MeanRank           *float64 `json:"mean_rank"`
	NormalisedMeanRank *float64 `json:"normalised_mean_rank"`
}

type jsonDetectInference struct {
	Samples                int      `json:"samples"`
	MeanRank               *float64 `json:"mean_rank"`
	NormalisedMeanRank     *float64 `json:"normalised_mean_rank"`
	PositiveLogEvidence    float64  `json:"positive_log_evidence"`
	EliminationLogEvidence float64  `json:"elimination_log_evidence"`
	Eliminated             bool     `json:"eliminated"`
}

type jsonDetectPrimingAttempt struct {
	WorkerID          int                `json:"worker_id"`
	ConnectionID      uint64             `json:"connection_id"`
	Attempt           int                `json:"attempt"`
	Replacement       bool               `json:"replacement"`
	RequestOperations int                `json:"request_operations"`
	Complete          bool               `json:"complete"`
	Failure           *structuredFailure `json:"failure"`
}

type jsonDetectAcquisitionFailure struct {
	WorkerID    int                `json:"worker_id"`
	Replacement bool               `json:"replacement"`
	Failure     *structuredFailure `json:"failure"`
}

type jsonDetectComparisonFailure struct {
	WorkerID              int                    `json:"worker_id"`
	ConnectionID          uint64                 `json:"connection_id"`
	RequestParticipations int                    `json:"request_participations"`
	Assignments           []jsonDetectAssignment `json:"assignments"`
	Failure               *structuredFailure     `json:"failure"`
}

type jsonDetectAssignment struct {
	RequestID int `json:"request_id"`
	Position  int `json:"position"`
}

func (r *jsonDetectReporter) Result(result detectRunResult) error {
	execution, err := result.executionAccounting(r.comparisonsMaximum, r.plannedConnections)
	if err != nil {
		return err
	}
	ranked := result.Ranking
	if len(ranked.Items) != len(result.Requests) {
		return fmt.Errorf("detect result has %d solver items, want %d candidates", len(ranked.Items), len(result.Requests))
	}
	if ranked.Outcome == ranking.OutlierFound && (ranked.Index < 0 || ranked.Index >= len(r.candidates)) {
		return fmt.Errorf("detect selected candidate %d is out of range", ranked.Index)
	}
	if len(result.Requests) != len(r.candidates) || len(result.Baselines) != len(r.baselines) {
		return fmt.Errorf("detect result and request catalogue role counts disagree")
	}
	detail, hasInference := ranked.Detail.(ranking.OutlierDiagnostics)
	if (!result.Unavailable || hasInference) && (!hasInference || len(detail.Items) != len(ranked.Items)) {
		return fmt.Errorf("detect result omitted outlier evidence")
	}
	if hasInference {
		if err := validateDetectInference(detail); err != nil {
			return err
		}
	}
	detection := jsonDetectDetection{State: "unavailable"}
	if result.Unavailable && normaliseCompletion(result.Completion).State == completionComplete {
		return fmt.Errorf("unavailable detection requires stopped or failed completion")
	}
	if !result.Unavailable {
		detection.State = "available"
		finding := &jsonDetectFinding{Status: detectOutcome(ranked.Outcome)}
		if ranked.Outcome == ranking.OutlierFound {
			finding.RequestID = new(r.candidates[ranked.Index])
			finding.Direction = new(detectDirection(ranked.Direction))
		}
		if ranked.Outcome != ranking.OutlierInconclusive {
			direction, ok := detectErrorDirection(ranked.ErrorDirection)
			if !ok {
				return fmt.Errorf("detect result has no controlled error direction")
			}
			finding.Guarantee = &jsonDetectGuarantee{ErrorDirection: direction, ErrorRiskLimit: ranked.ErrorRiskLimit}
			if ranked.Outcome == ranking.OutlierNegative {
				finding.ExcludedEdge = new(ranked.MinimumEdge)
			}
		}
		detection.Finding = finding
		detection.CommitThresholdLogEvidence = new(detail.Threshold)
	}
	review := result.Responses
	requests := make([]jsonDetectRequestStats, 0, len(result.Requests)+len(result.Baselines))
	for index, evidence := range result.Requests {
		if evidence.Request != index {
			return fmt.Errorf("detect candidate evidence has inconsistent identity")
		}
		request := makeJSONDetectRequestStats(evidence, r.candidates[index], r.width,
			slices.Contains(review.Changing, index), slices.Contains(review.Non2xx, index))
		if hasInference {
			item := ranked.Items[index]
			diagnostic := detail.Items[index]
			mean := finiteFloat(item.MeanRank)
			request.Inference = &jsonDetectInference{
				Samples: item.Samples, MeanRank: mean, NormalisedMeanRank: normalisedDetectMean(mean, r.width),
				PositiveLogEvidence: diagnostic.LogEvidence, EliminationLogEvidence: diagnostic.EliminationLogE,
				Eliminated: diagnostic.Eliminated,
			}
		}
		requests = append(requests, request)
	}
	for index, evidence := range result.Baselines {
		if evidence.Request != index {
			return fmt.Errorf("detect baseline evidence has inconsistent identity")
		}
		requests = append(requests, makeJSONDetectRequestStats(evidence, r.baselines[index], r.width,
			slices.Contains(review.BaselineChanging, index), slices.Contains(review.BaselineNon2xx, index)))
	}
	if !result.Unavailable || execution.RequestOperations.Retained > 0 {
		detection.Requests = requests
		detection.StatusDistributionsDiffer = new(len(review.Different)+len(review.BaselineDifferent) > 0)
	}
	priming := make([]jsonDetectPrimingAttempt, len(result.Execution.PrimingAttempts))
	for i, attempt := range result.Execution.PrimingAttempts {
		priming[i] = jsonDetectPrimingAttempt{
			WorkerID: attempt.Worker, ConnectionID: uint64(attempt.Connection), Attempt: attempt.Attempt,
			Replacement: attempt.Replacement, RequestOperations: attempt.RequestOperations,
			Complete: attempt.Complete, Failure: makeStructuredFailure(attempt.Err, "acquisition_failed"),
		}
	}
	acquisitions := make([]jsonDetectAcquisitionFailure, len(result.Execution.AcquisitionFailures))
	for i, failure := range result.Execution.AcquisitionFailures {
		if failure.Err == nil {
			return fmt.Errorf("detect acquisition failure has no cause")
		}
		acquisitions[i] = jsonDetectAcquisitionFailure{
			WorkerID: failure.Worker, Replacement: failure.Replacement,
			Failure: makeStructuredFailure(failure.Err, "acquisition_failed"),
		}
	}
	comparisons := make([]jsonDetectComparisonFailure, len(result.Execution.ComparisonFailures))
	for i, failure := range result.Execution.ComparisonFailures {
		if failure.Err == nil {
			return fmt.Errorf("detect comparison failure has no cause")
		}
		assignments := make([]jsonDetectAssignment, len(failure.Items))
		for position, item := range failure.Items {
			catalogue := r.candidates
			if item.role == requestGroupBaseline {
				catalogue = r.baselines
			}
			assignments[position] = jsonDetectAssignment{RequestID: catalogue[item.index], Position: position}
		}
		comparisons[i] = jsonDetectComparisonFailure{
			WorkerID: failure.Worker, ConnectionID: uint64(failure.Connection),
			RequestParticipations: failure.Participations, Assignments: assignments,
			Failure: makeStructuredFailure(failure.Err, "comparison_failed"),
		}
	}
	return r.stream.Finish(jsonDetectResultRecord{
		Kind: "result", Completion: makeStructuredCompletion(result.Completion),
		Execution: makeJSONDetectExecution(execution),
		Detection: detection, PrimingAttempts: priming, AcquisitionFailures: acquisitions, ComparisonFailures: comparisons,
	})
}

func makeJSONDetectRequestStats(evidence detectRequestEvidence, requestID, width int,
	varied, non2xx bool,
) jsonDetectRequestStats {
	var mean *float64
	if evidence.Participations > 0 {
		mean = new(float64(evidence.RankSum) / float64(evidence.Participations))
	}
	statuses := make([]structuredStatusCount, len(evidence.StatusCounts))
	for i, count := range evidence.StatusCounts {
		statuses[i] = structuredStatusCount(count)
	}
	return jsonDetectRequestStats{
		RequestID: requestID,
		Physical: jsonDetectPhysical{
			Participations: evidence.Participations, MeanRank: mean,
			NormalisedMeanRank: normalisedDetectMean(mean, width),
		},
		StatusCounts: statuses, StatusVaried: varied, Non2xxObserved: non2xx,
	}
}

func normalisedDetectMean(mean *float64, width int) *float64 {
	if mean == nil || width < 2 {
		return nil
	}
	return new(*mean / float64(width-1))
}

func finiteFloat(value float64) *float64 {
	if nonFiniteFloat(value) {
		return nil
	}
	return &value
}

func validateDetectInference(detail ranking.OutlierDiagnostics) error {
	if nonFiniteFloat(detail.Threshold) {
		return fmt.Errorf(
			"detect result commit threshold is non-finite (%v)", detail.Threshold)
	}
	for candidate, item := range detail.Items {
		if nonFiniteFloat(item.LogEvidence) {
			return fmt.Errorf(
				"detect result candidate %d positive log evidence is non-finite (%v)",
				candidate, item.LogEvidence)
		}
		if nonFiniteFloat(item.EliminationLogE) {
			return fmt.Errorf(
				"detect result candidate %d elimination log evidence is non-finite (%v)",
				candidate, item.EliminationLogE)
		}
	}
	return nil
}

func nonFiniteFloat(value float64) bool {
	return math.IsNaN(value) || math.IsInf(value, 0)
}

func detectErrorDirection(direction ranking.OutlierErrorDirection) (string, bool) {
	switch direction {
	case ranking.OutlierErrorFalseFind:
		return "false_positive", true
	case ranking.OutlierErrorFalseNegative:
		return "false_negative", true
	default:
		return "", false
	}
}

func detectOutcome(outcome ranking.OutlierOutcome) string {
	switch outcome {
	case ranking.OutlierFound:
		return "found"
	case ranking.OutlierNegative:
		return "negative"
	default:
		return "inconclusive"
	}
}

func detectDirection(direction ranking.OutlierDirection) string {
	switch direction {
	case ranking.Early:
		return directionEarly
	case ranking.Either:
		return directionEither
	default:
		return directionLate
	}
}

type jsonDetectExecutionComparisons struct {
	Maximum      *int `json:"maximum"`
	Attempted    int  `json:"attempted"`
	RankComplete int  `json:"rank_complete"`
	Incomplete   int  `json:"incomplete"`
}
type jsonDetectExecution struct {
	structuredExecution
	Comparisons jsonDetectExecutionComparisons `json:"comparisons"`
}

func makeJSONDetectExecution(value detectExecution) jsonDetectExecution {
	result := jsonDetectExecution{structuredExecution: makeStructuredExecution(value.sharedExecution)}
	result.Comparisons = jsonDetectExecutionComparisons(value.Comparisons)
	return result
}
