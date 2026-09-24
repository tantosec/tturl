package main

import (
	"testing"

	"github.com/tantosec/tturl/internal/ranking"
)

func TestSharedExecutionValidationRejectsInvalidAccounting(t *testing.T) {
	valid := sharedExecution{
		Connections: executionConnections{Planned: 2, Observed: 3, Replacements: 1},
		Priming: executionPriming{
			Batches: executionBatches{Attempted: 2, RankComplete: 1, Incomplete: 1},
			RequestOperations: executionPrimingOperations{
				Attempted: 4, RankComplete: 2,
			},
		},
		RequestOperations: executionRequestOperations{
			Attempted: 8, RankComplete: 6, Retained: 4,
		},
	}
	if err := validateSharedExecution(valid); err != nil {
		t.Fatalf("valid execution rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*sharedExecution)
	}{
		{"negative", func(e *sharedExecution) { e.Connections.Planned = -1 }},
		{"replacement", func(e *sharedExecution) { e.Connections.Replacements = 4 }},
		{"batch arithmetic", func(e *sharedExecution) { e.Priming.Batches.Incomplete = 0 }},
		{"priming operations", func(e *sharedExecution) {
			e.Priming.RequestOperations.RankComplete = 5
		}},
		{"measured rank complete", func(e *sharedExecution) {
			e.RequestOperations.RankComplete = 9
		}},
		{"measured retained", func(e *sharedExecution) {
			e.RequestOperations.Retained = 7
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := valid
			test.mutate(&got)
			if err := validateSharedExecution(got); err == nil {
				t.Fatalf("invalid execution accepted: %+v", got)
			}
		})
	}
}

func TestRaceExecutionValidationFiniteAndUnlimited(t *testing.T) {
	maximum, unattempted := 5, 3
	valid := raceExecution{
		sharedExecution: sharedExecution{
			Connections: executionConnections{Planned: 1, Observed: 1},
			Priming: executionPriming{
				Batches:           executionBatches{},
				RequestOperations: executionPrimingOperations{},
			},
			RequestOperations: executionRequestOperations{
				Attempted: 4, RankComplete: 2, Retained: 2,
			},
		},
		Trials: raceExecutionTrials{
			Maximum: &maximum, Attempted: 2, RankComplete: 1,
			Incomplete: 1, Unattempted: &unattempted,
		},
	}
	if err := validateRaceExecution(valid, 2, 0); err != nil {
		t.Fatalf("valid finite execution rejected: %v", err)
	}
	valid.Trials.Maximum = nil
	valid.Trials.Unattempted = nil
	if err := validateRaceExecution(valid, 2, 0); err != nil {
		t.Fatalf("valid unlimited execution rejected: %v", err)
	}
	zero := 0
	valid.Trials.Unattempted = &zero
	if err := validateRaceExecution(valid, 2, 0); err == nil {
		t.Fatal("mixed finite and unlimited execution accepted")
	}
}

func TestMeasurementExecutionProjectsFixedWidths(t *testing.T) {
	measured := newMeasurement(2, 2, 2, 1, arrangementFromName("none"))
	measured.warmupWidth = 1
	measured.warmups = 2
	measured.warmupComplete = 1
	measured.warmupIncomplete = 1
	measured.attempted = 2
	measured.complete = 1
	measured.incomplete = 1
	measured.retained = 1
	execution, err := measured.execution()
	if err != nil {
		t.Fatal(err)
	}
	if execution.Priming.RequestOperations.Attempted != 2 ||
		execution.Priming.RequestOperations.RankComplete != 1 ||
		execution.RequestOperations.Attempted != 4 ||
		execution.RequestOperations.RankComplete != 2 ||
		execution.RequestOperations.Retained != 2 {
		t.Fatalf("execution = %+v", execution)
	}
}

func TestMeasureExecutionRejectsCycleTrialContradictions(t *testing.T) {
	measured := newMeasurement(4, 4, 2, 1, arrangementFromName("rotate"))
	measured.attempted = 4
	measured.complete = 4
	measured.retained = 4
	measured.cycleTally = cycleCounts{attempted: 2, retained: 2}
	execution, err := measured.execution()
	if err != nil {
		t.Fatal(err)
	}
	execution.Cycles.Retained--
	execution.Cycles.Incomplete++
	if err := validateMeasureExecution(execution, 2, 0); err == nil {
		t.Fatal("measure execution accepted cycles contradicting retained trials")
	}
}

func TestAnalyseExecutionExcludesCompleteUnavailableEvidence(t *testing.T) {
	result := analyseOffline(
		analyseOrderedEvidence(2, 1, func(int) []int { return []int{0, 1} }),
		analyseConfig{ConnectionLimit: 1},
	)
	if result.Validity.Available {
		t.Fatal("one cycle unexpectedly produced an available analysis")
	}
	execution, err := result.executionAccounting(0)
	if err != nil {
		t.Fatal(err)
	}
	if execution.Trials.RankComplete != 2 || execution.Trials.Retained != 0 ||
		execution.Trials.Excluded != 2 || execution.Cycles.Complete != 1 ||
		execution.Cycles.Retained != 0 {
		t.Fatalf("execution = %+v", execution)
	}

	execution.Trials.Unattempted++
	if err := validateAnalyseExecution(execution, 2, 0); err == nil {
		t.Fatal("analyse execution with invalid trial arithmetic was accepted")
	}
	execution.Trials.Unattempted--
	execution.Cycles.Retained = 1
	if err := validateAnalyseExecution(execution, 2, 0); err == nil {
		t.Fatal("analyse execution accepted cycles contradicting retained trials")
	}
}

func TestDetectExecutionProjectsComparisonAttemptsAndParticipations(t *testing.T) {
	result := detectRunResult{
		Ranking: ranking.OutlierResult{Cost: ranking.Cost{
			Comparisons: 1, Participations: 3,
		}},
		Requests: []detectRequestEvidence{{
			Participations: 3,
			StatusCounts:   []detectStatusCount{{Status: 204, Count: 3}},
		}},
		Execution: detectExecutionEvidence{
			ConnectionsObserved: 1,
			ComparisonFailures: []detectComparisonFailure{{
				Participations: 2,
				Items: []detectEvidenceRef{
					{role: requestGroupCandidate},
					{role: requestGroupCandidate},
				},
			}},
		},
	}
	execution, err := result.executionAccounting(3, 1)
	if err != nil {
		t.Fatal(err)
	}
	if execution.Comparisons.Attempted != 2 ||
		execution.Comparisons.RankComplete != 1 ||
		execution.Comparisons.Incomplete != 1 ||
		execution.RequestOperations.Attempted != 5 ||
		execution.RequestOperations.RankComplete != 3 ||
		execution.RequestOperations.Retained != 3 {
		t.Fatalf("execution = %+v", execution)
	}
}

func TestDetectExecutionDoesNotCountPreComparisonFailures(t *testing.T) {
	result := detectRunResult{Execution: detectExecutionEvidence{
		ConnectionsObserved: 1,
		PrimingAttempts: []detectPrimingAttempt{{
			RequestOperations: 3,
		}},
		AcquisitionFailures: []detectAcquisitionFailure{{Replacement: true}},
	}}
	execution, err := result.executionAccounting(-1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if execution.Comparisons.Maximum != nil ||
		execution.Comparisons.Attempted != 0 ||
		execution.RequestOperations.Attempted != 0 ||
		execution.Priming.Batches.Attempted != 1 ||
		execution.Priming.RequestOperations.Attempted != 3 {
		t.Fatalf("execution = %+v", execution)
	}
}

func TestDetectExecutionRejectsDetailedEvidenceContradictions(t *testing.T) {
	result := detectRunResult{
		Ranking: ranking.OutlierResult{Cost: ranking.Cost{
			Comparisons: 1, Participations: 2,
		}},
		Requests: []detectRequestEvidence{{
			Participations: 1,
			StatusCounts:   []detectStatusCount{{Status: 204, Count: 1}},
		}},
	}
	if _, err := result.executionAccounting(1, 1); err == nil {
		t.Fatal("detect execution accepted request evidence contradicting solver cost")
	}

	result.Ranking.Participations = 1
	result.Execution.ComparisonFailures = []detectComparisonFailure{{
		Participations: 2,
		Items:          []detectEvidenceRef{{role: requestGroupCandidate}},
	}}
	if _, err := result.executionAccounting(2, 1); err == nil {
		t.Fatal("detect execution accepted inconsistent failed comparison detail")
	}
}
