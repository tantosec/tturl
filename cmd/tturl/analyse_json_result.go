package main

import (
	"fmt"
	"slices"
)

type jsonAnalyseResultRecord struct {
	Kind       string               `json:"kind"`
	Completion structuredCompletion `json:"completion"`
	Execution  jsonAnalyseExecution `json:"execution"`
	Analysis   jsonAnalyseAnalysis  `json:"analysis"`
}

type jsonAnalyseAnalysis struct {
	State                     string                 `json:"state"`
	Reason                    *jsonAnalyseReason     `json:"reason,omitempty"`
	Finding                   *jsonAnalyseFinding    `json:"finding,omitempty"`
	GlobalTest                *jsonAnalyseGlobalTest `json:"global_test,omitempty"`
	Requests                  []jsonAnalyseRequest   `json:"requests,omitempty"`
	PairTests                 *[]jsonAnalysePair     `json:"pair_tests,omitempty"`
	StatusDistributionsDiffer *bool                  `json:"status_distributions_differ,omitempty"`
}

type jsonAnalyseReason struct {
	Code    analyseValidityCode `json:"code"`
	Message wireString          `json:"message"`
}

type jsonAnalyseFinding struct {
	Status string `json:"status"`
}

type jsonAnalyseRequest struct {
	RequestID          int                     `json:"request_id"`
	Samples            int                     `json:"samples"`
	MeanRank           *float64                `json:"mean_rank"`
	NormalisedMeanRank *float64                `json:"normalised_mean_rank"`
	StatusCounts       []structuredStatusCount `json:"status_counts"`
	StatusVaried       bool                    `json:"status_varied"`
	Non2xxObserved     bool                    `json:"non_2xx_observed"`
}

type jsonAnalyseGlobalTest struct {
	Alpha       float64                `json:"alpha"`
	Statistic   float64                `json:"statistic"`
	Components  jsonAnalyseComponents  `json:"components"`
	Calibration jsonAnalyseCalibration `json:"calibration"`
}

type jsonAnalyseComponents struct {
	L2        float64 `json:"l2"`
	LInfinity float64 `json:"l_infinity"`
}

type jsonAnalyseCalibration struct {
	Method           string  `json:"method"`
	Draws            int     `json:"draws"`
	Extremes         int     `json:"extremes"`
	PValue           float64 `json:"p_value"`
	PValueKind       string  `json:"p_value_kind"`
	CompletionReason string  `json:"completion_reason"`
}

type jsonAnalysePair struct {
	LeftRequestID       int     `json:"left_request_id"`
	RightRequestID      int     `json:"right_request_id"`
	Trials              int     `json:"trials"`
	LeftBeforeRight     int     `json:"left_before_right"`
	RightBeforeLeft     int     `json:"right_before_left"`
	MeanRankDifference  float64 `json:"mean_rank_difference"`
	RawP                float64 `json:"raw_p"`
	RawPUpperBound      bool    `json:"raw_p_upper_bound"`
	AdjustedP           float64 `json:"adjusted_p"`
	AdjustedPUpperBound bool    `json:"adjusted_p_upper_bound"`
	Significant         bool    `json:"significant"`
}

func (s *analyseJSONStream) Finish(result analyseRunResult, context analyseJSONContext) error {
	if _, err := result.executionAccounting(context.WarmupBatchWidth); err != nil {
		return err
	}
	if err := s.stream.Finish(makeJSONAnalyseResultRecord(result, context)); err != nil {
		return fmt.Errorf("write analyse JSON result: %w", err)
	}
	return nil
}

func makeJSONAnalyseResultRecord(result analyseRunResult, context analyseJSONContext) jsonAnalyseResultRecord {
	execution, _ := result.executionAccounting(context.WarmupBatchWidth)
	analysis := jsonAnalyseAnalysis{State: "unavailable"}
	if !result.Validity.Available {
		message := result.Validity.Detail
		if message == "" {
			message = "Recorded analysis evidence is unavailable."
		}
		analysis.Reason = &jsonAnalyseReason{Code: result.Validity.Code, Message: wireString(message)}
	} else {
		analysis.State = "available"
		status := string(result.Global.Outcome)
		if result.Global.Outcome == analyseNoDetection {
			status = "no_difference_detected"
		}
		analysis.Finding = &jsonAnalyseFinding{Status: status}
		calibration := result.Global.Calibration
		analysis.GlobalTest = &jsonAnalyseGlobalTest{
			Alpha: result.Global.Alpha, Statistic: result.Global.Statistic,
			Components: jsonAnalyseComponents{L2: result.Global.Components.L2, LInfinity: result.Global.Components.LInfinity},
			Calibration: jsonAnalyseCalibration{
				Method:   calibration.Method,
				Draws:    calibration.Draws,
				Extremes: calibration.Extremes,

				PValue: calibration.PValue, PValueKind: calibration.PValueKind, CompletionReason: calibration.CompletionReason,
			},
		}
		analysis.Requests = make([]jsonAnalyseRequest, len(result.Requests))
		for i, request := range result.Requests {
			statuses := make([]structuredStatusCount, len(request.StatusCounts))
			for j, count := range request.StatusCounts {
				statuses[j] = structuredStatusCount(count)
			}
			analysis.Requests[i] = jsonAnalyseRequest{
				RequestID: request.Request, Samples: request.Samples,
				MeanRank: new(request.MeanRank), NormalisedMeanRank: new(request.NormalisedMeanRank),
				StatusCounts: statuses, StatusVaried: slices.Contains(result.Responses.Changing, request.Request),
				Non2xxObserved: slices.Contains(result.Responses.Non2xx, request.Request),
			}
		}
		pairs := make([]jsonAnalysePair, len(result.Pairs.Pairs))
		for i, pair := range result.Pairs.Pairs {
			pairs[i] = jsonAnalysePair{
				LeftRequestID: pair.Left, RightRequestID: pair.Right, Trials: pair.Trials,
				LeftBeforeRight: pair.LeftBeforeRight, RightBeforeLeft: pair.RightBeforeLeft,
				MeanRankDifference: pair.MeanRankDifference, RawP: pair.RawP, RawPUpperBound: pair.RawPUpperBound,
				AdjustedP: pair.AdjustedP, AdjustedPUpperBound: pair.AdjustedPUpperBound, Significant: pair.Selected,
			}
		}
		analysis.PairTests = &pairs
		differ := len(result.Responses.Different) > 0
		analysis.StatusDistributionsDiffer = &differ
	}
	return jsonAnalyseResultRecord{
		Kind:       "result",
		Completion: makeStructuredCompletion(result.Completion),
		Execution:  makeJSONAnalyseExecution(execution),
		Analysis:   analysis,
	}
}

type jsonAnalyseExecutionTrials struct {
	Planned      int `json:"planned"`
	Attempted    int `json:"attempted"`
	RankComplete int `json:"rank_complete"`
	Incomplete   int `json:"incomplete"`
	Unattempted  int `json:"unattempted"`
	Retained     int `json:"retained"`
	Excluded     int `json:"excluded"`
}
type jsonAnalyseExecutionCycles struct {
	Planned     int `json:"planned"`
	Attempted   int `json:"attempted"`
	Complete    int `json:"complete"`
	Incomplete  int `json:"incomplete"`
	Unattempted int `json:"unattempted"`
	Retained    int `json:"retained"`
}
type jsonAnalyseExecution struct {
	structuredExecution
	Trials jsonAnalyseExecutionTrials `json:"trials"`
	Cycles jsonAnalyseExecutionCycles `json:"cycles"`
}

func makeJSONAnalyseExecution(value analyseExecution) jsonAnalyseExecution {
	result := jsonAnalyseExecution{structuredExecution: makeStructuredExecution(value.sharedExecution)}
	result.Trials = jsonAnalyseExecutionTrials(value.Trials)
	result.Cycles = jsonAnalyseExecutionCycles(value.Cycles)
	return result
}

func (value jsonAnalyseExecution) accounting() analyseExecution {
	return analyseExecution{
		sharedExecution: value.structuredExecution.accounting(),
		Trials:          analyseExecutionTrials(value.Trials), Cycles: analyseExecutionCycles(value.Cycles),
	}
}
