package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/tantosec/tturl/tth2"
)

func TestAnalyseCollectorFeedsOfflineCore(t *testing.T) {
	collector := newAnalyseCollector(2, 6)
	for cycle := 5; cycle >= 0; cycle-- {
		collector.observe(analyseTrialFixture(cycle, 1, 0, []int{0, 1}))
		collector.observe(analyseTrialFixture(cycle, 0, 0, []int{0, 1}))
	}
	evidence := collector.finish(nil)
	result := analyseOffline(evidence, analyseConfig{RandomisationSeed: 7})

	if result.Global.Outcome != analyseDifference ||
		result.Global.Calibration.PValue != 0.03125 ||
		result.Pairs.State != analysePairsPerformed {
		t.Fatalf("result = %+v", result)
	}
	if result.Plan.Cycles != 6 || result.Execution.AttemptedTrials != 12 ||
		result.Execution.CompletedTrials != 12 ||
		result.Execution.CompleteCycles != 6 ||
		result.Execution.Connections != 1 {
		t.Fatalf("plan/execution = %+v / %+v", result.Plan, result.Execution)
	}
}

func TestAnalyseCollectorIgnoresWarmup(t *testing.T) {
	collector := newAnalyseCollector(2, 6)
	warmup := analyseTrialFixture(0, 0, 0, []int{0, 1})
	warmup.Warmup = true
	collector.observe(warmup)
	for cycle := range 6 {
		for shift := range 2 {
			collector.observe(analyseTrialFixture(cycle, shift, 0, []int{0, 1}))
		}
	}
	result := analyseOffline(collector.finish(nil), analyseConfig{})
	if !result.Validity.Available || result.Cycles != 6 {
		t.Fatalf("result = %+v", result)
	}
}

func TestAnalyseCollectorRejectsFailedTrial(t *testing.T) {
	collector := newAnalyseCollector(2, 6)
	trial := analyseTrialFixture(0, 0, 0, []int{0, 1})
	trial.Err = errors.New("stream reset")
	collector.observe(trial)
	result := analyseOffline(collector.finish(nil), analyseConfig{})
	if result.Validity.Available ||
		result.Validity.Code != analyseIncompleteCycle {
		t.Fatalf("result = %+v", result)
	}
	if result.Plan.Cycles != 6 || result.Execution.AttemptedTrials != 1 ||
		result.Execution.CompletedTrials != 0 {
		t.Fatalf("plan/execution = %+v / %+v", result.Plan, result.Execution)
	}
}

func TestAnalyseRankCompleteCountRequiresValidatedRankFacts(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*tth2.Trial)
	}{
		{"width", func(trial *tth2.Trial) {
			trial.Results = trial.Results[:1]
		}},
		{"arrangement", func(trial *tth2.Trial) {
			trial.Arrangement[1] = trial.Arrangement[0]
		}},
		{"arrival order", func(trial *tth2.Trial) {
			trial.ArrivalOrder = trial.ArrivalOrder[:1]
		}},
		{"HTTP status", func(trial *tth2.Trial) {
			trial.Results[0].Response.StatusCode = 0
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			collector := newAnalyseCollector(2, 6)
			trial := analyseTrialFixture(0, 0, 0, []int{0, 1})
			test.edit(trial)
			collector.observe(trial)
			result := analyseOffline(collector.finish(nil), analyseConfig{})
			if result.Execution.AttemptedTrials != 1 ||
				result.Execution.CompletedTrials != 0 ||
				result.Validity.Available {
				t.Fatalf("malformed trial execution = %+v; validity = %+v",
					result.Execution, result.Validity)
			}
		})
	}
}

func TestAnalyseCollectorFailsClosed(t *testing.T) {
	tests := []struct {
		name      string
		edit      func(*analyseCollector)
		finishErr error
		code      analyseValidityCode
	}{
		{
			name: "failed trial",
			edit: func(collector *analyseCollector) {
				collector.cycles[0].trials = collector.cycles[0].trials[:1]
			},
			code: analyseIncompleteCycle,
		},
		{
			name: "replacement connection",
			edit: func(collector *analyseCollector) {
				collector.cycles[1].trials[0].Connection = 1
			},
			code: analyseMixedConnections,
		},
		{
			name:      "interrupted",
			edit:      func(*analyseCollector) {},
			finishErr: errors.New("cancelled"),
			code:      analyseInterrupted,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			collector := newAnalyseCollector(2, 6)
			for cycle := range 6 {
				for shift := range 2 {
					collector.observe(analyseTrialFixture(
						cycle, shift, 0, []int{0, 1},
					))
				}
			}
			test.edit(collector)
			result := analyseOffline(collector.finish(test.finishErr), analyseConfig{})
			if result.Validity.Available || result.Validity.Code != test.code ||
				result.Global.Outcome != analyseUnavailable {
				t.Fatalf("result = %+v, want unavailable %s", result, test.code)
			}
		})
	}
}

func TestAnalyseObservationFailureSurvivesDriveCancellation(t *testing.T) {
	want := errors.New("write trial evidence")
	collector := newLiveAnalyseCollector(
		2, 1, 2, 1, raceCaptureConfig{},
		func(analyseTrialObservation) error { return want }, true)
	collector.observe(analyseTrialFixture(0, 0, 0, []int{0, 1}))
	evidence := collector.finish(context.Canceled)
	if evidence.Completion.State != completionFailed ||
		evidence.Completion.Code != completionInternalError ||
		!strings.Contains(evidence.Completion.Detail, want.Error()) {
		t.Fatalf("completion = %+v, want originating internal error",
			evidence.Completion)
	}
}

func analyseTrialFixture(
	cycle, shift, connection int, ranks []int,
) *tth2.Trial {
	width := len(ranks)
	arrangement := make([]int, width)
	results := make([]tth2.Result, width)
	arrivalOrder := make([]int, width)
	for position := range width {
		request := (position + cycle + shift) % width
		arrangement[position] = request
	}
	for request, rank := range ranks {
		results[request] = tth2.Result{
			ArrivalRank: rank,
			Response:    &tth2.Response{StatusCode: http.StatusOK},
		}
		arrivalOrder[rank] = request
	}
	return &tth2.Trial{
		BatchResult: tth2.BatchResult{
			Results: results, ArrivalOrder: arrivalOrder,
		},
		Index: cycle*width + shift, Cycle: cycle, Shift: shift,
		Conn: connection, Arrangement: arrangement,
	}
}
