package main

import "fmt"

type jsonDetectPlan struct {
	Width           jsonDetectWidth       `json:"width"`
	ComparisonLimit *int                  `json:"comparison_limit"`
	Padding         []structuredPadding   `json:"padding"`
	Warmup          structuredWarmup      `json:"warmup"`
	Priming         jsonDetectPrimingPlan `json:"priming"`
}

type jsonDetectWidth struct {
	Requested int `json:"requested"`
	Effective int `json:"effective"`
}

type jsonDetectPrimingPlan struct {
	InitialRequestOperations int `json:"initial_request_operations"`
	MaximumRequestOperations int `json:"maximum_request_operations"`
}

type jsonDetectMethod struct {
	ID                string             `json:"id"`
	Strategy          jsonDetectStrategy `json:"strategy"`
	Direction         string             `json:"direction"`
	FalsePositiveRisk float64            `json:"false_positive_risk"`
	FalseNegativeRisk float64            `json:"false_negative_risk"`
	Edge              float64            `json:"edge"`
}

type jsonDetectStrategy struct {
	Requested string `json:"requested"`
	Resolved  string `json:"resolved"`
}

func makeJSONDetectPlan(run DetectRunRecord) jsonDetectPlan {
	var limit *int
	if run.ComparisonsMax > 0 {
		limit = new(run.ComparisonsMax)
	}
	return jsonDetectPlan{
		Width:           jsonDetectWidth{Requested: run.WidthRequested, Effective: run.WidthEffective},
		ComparisonLimit: limit, Padding: makeStructuredPadding(run.Padding),
		Warmup: structuredWarmup{TrialsPerConnection: run.WarmupTrialsPerConnection, BatchWidth: run.WarmupBatchWidth},
		Priming: jsonDetectPrimingPlan{
			InitialRequestOperations: run.PlannedInitialPrimingRequestOperations,
			MaximumRequestOperations: run.PlannedMaximumPrimingRequestOperations,
		},
	}
}

func validateDetectJSONPlan(
	run DetectRunRecord,
	requests []detectCapturedRequest,
) error {
	if len(requests) != run.RequestCount {
		return fmt.Errorf("detect has %d request records, want %d",
			len(requests), run.RequestCount)
	}
	if run.CandidateCount < 1 || run.BaselineTemplateCount < 0 ||
		run.CandidateCount+run.BaselineTemplateCount > run.RequestCount {
		return fmt.Errorf("detect request role counts are inconsistent")
	}
	selection, err := resolveDetectStrategy(
		string(run.RequestedStrategy), run.Direction, run.BaselineTemplateCount > 0)
	if err != nil {
		return fmt.Errorf("detect reported strategy: %w", err)
	}
	if selection.resolved != run.ResolvedStrategy {
		return fmt.Errorf("detect resolved strategy is %q, want %q",
			run.ResolvedStrategy, selection.resolved)
	}
	if selection.method != run.SolverMethod {
		return fmt.Errorf("detect solver method is %q, want %q",
			run.SolverMethod, selection.method)
	}
	if run.InFlightComparisonLimit < 1 {
		return fmt.Errorf("detect in-flight comparison limit must be positive")
	}
	if run.InFlightComparisonLimit > run.ConnectionLimit {
		return fmt.Errorf(
			"detect in-flight comparison limit exceeds connection limit")
	}
	for id, request := range requests {
		if request.Reference.ID != id {
			return fmt.Errorf("detect request record %d has ID %d", id,
				request.Reference.ID)
		}
		expectedRole := "warmup"
		if id < run.CandidateCount {
			expectedRole = "candidate"
		} else if id < run.CandidateCount+run.BaselineTemplateCount {
			expectedRole = "baseline"
		}
		if request.Role != expectedRole {
			return fmt.Errorf("detect request %d has role %q, want %q",
				id, request.Role, expectedRole)
		}
		switch request.Role {
		case "candidate":
			if request.BaselineSupply != (baselineSupply{}) {
				return fmt.Errorf("candidate request has baseline supply")
			}
		case "baseline":
			if !request.BaselineSupply.unlimited && request.BaselineSupply.count < 1 {
				return fmt.Errorf("baseline request has invalid supply")
			}
		case "warmup":
			if request.BaselineSupply != (baselineSupply{}) {
				return fmt.Errorf("warmup request has baseline supply")
			}
		default:
			return fmt.Errorf("detect request has invalid role %q", request.Role)
		}
	}
	return nil
}
