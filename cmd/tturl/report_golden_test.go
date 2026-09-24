package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tantosec/tturl/internal/ranking"
	"github.com/tantosec/tturl/tth2"
)

var updateReportGoldens = flag.Bool(
	"update-report-goldens", false,
	"rewrite the authoritative tturl text report fixtures",
)

type reportGoldenCase struct {
	command string
	state   string
	render  func(*testing.T) string
}

func TestTextReportGoldens(t *testing.T) {
	tests := []reportGoldenCase{
		{command: "race", state: "complete", render: renderGoldenRaceComplete},
		{command: "race", state: "failed", render: renderGoldenRaceFailed},
		{command: "measure", state: "available", render: renderGoldenMeasureAvailable},
		{command: "measure", state: "unavailable", render: renderGoldenMeasureUnavailable},
		{command: "analyse", state: "difference", render: renderGoldenAnalyseDifference},
		{command: "analyse", state: "no-detection", render: renderGoldenAnalyseNoDetection},
		{command: "analyse", state: "unavailable", render: renderGoldenAnalyseUnavailable},
		{command: "analyse", state: "response-review", render: renderGoldenAnalyseResponseReview},
		{command: "detect", state: "found", render: renderGoldenDetectFound},
		{command: "detect", state: "negative", render: renderGoldenDetectNegative},
		{command: "detect", state: "unavailable", render: renderGoldenDetectUnavailable},
	}

	for _, test := range tests {
		t.Run(test.command+"/"+test.state, func(t *testing.T) {
			got := test.render(t)
			assertGoldenReportPortable(t, got)
			path := filepath.Join("testdata", "reports", test.command,
				test.state+".txt")
			if *updateReportGoldens {
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			//nolint:gosec // The table constructs paths from fixed command/state names.
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (use -update-report-goldens to create it): %v", err)
			}
			if got != string(want) {
				t.Fatalf("report differs from %s%s", path,
					firstReportDifference(string(want), got))
			}
		})
	}
}

func assertGoldenReportPortable(t *testing.T, report string) {
	t.Helper()
	for line, value := range strings.Split(report, "\n") {
		if len(value) > textWidth {
			t.Errorf("line %d has %d columns, want at most %d:\n%s",
				line+1, len(value), textWidth, value)
		}
		for _, b := range []byte(value) {
			if b >= utf8RuneSelf || b == '\x1b' {
				t.Errorf("line %d contains non-ASCII terminal output", line+1)
				break
			}
		}
	}
}

func firstReportDifference(want, got string) string {
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")
	for line := 0; line < max(len(wantLines), len(gotLines)); line++ {
		var wantLine, gotLine string
		if line < len(wantLines) {
			wantLine = wantLines[line]
		}
		if line < len(gotLines) {
			gotLine = gotLines[line]
		}
		if wantLine != gotLine {
			return fmt.Sprintf("\nfirst difference at line %d:\n- %s\n+ %s\n\nfull report:\n%s",
				line+1, wantLine, gotLine, got)
		}
	}
	return ""
}

func goldenReportStart(t *testing.T, command string, labels ...string) (
	*reportWriter, runReferences, *bytes.Buffer,
) {
	t.Helper()
	args := make([]string, 0, len(labels)*4)
	for index, label := range labels {
		args = append(args,
			"--block", fmt.Sprintf("https://example.test/request/%d", index),
			"--name", label)
	}
	assembled := assembleArgs(t, args...)
	refs := fullRunReferences(assembled.labels)
	var output bytes.Buffer
	report := newReportWriter(&output)
	writeBanner(report, toolName+" "+command)
	summariseSection(report, assembled.groups, refs, false)
	return report, refs, &output
}

func goldenDelivery() deliveryPlan {
	return deliveryPlan{
		connectionLimit: 1, connectionCeiling: 1,
		connectionSelection: connectionDefault,
	}
}

func renderGoldenRaceComplete(t *testing.T) string {
	report, refs, output := goldenReportStart(t, "race", "early", "late")
	reporter := newTextRaceReporter(report)
	if err := reporter.Run(RunRecord{
		Trials: 1, TrialsRequested: 1, TrialBatchWidth: 2, Connections: 1,
		Delivery: goldenDelivery(), ArrangementPolicy: arrangeNone,
		ResponseLimits: defaultResponseLimits(),
	}); err != nil {
		t.Fatal(err)
	}
	goldenRaceRequests(t, reporter, refs)
	results := []tth2.Result{
		response(http.StatusNoContent, 0, 0),
		response(http.StatusNoContent, 0, 0),
	}
	results[0].ArrivalRank, results[1].ArrivalRank = 0, 1
	if err := reporter.Trial(TrialRecord{Trial: &tth2.Trial{
		BatchResult: tth2.BatchResult{
			Results: results, ArrivalOrder: []int{0, 1},
		},
		Arrangement: []int{0, 1}, Index: 0, Cycle: -1, Shift: -1,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := reporter.Summary(SummaryRecord{
		Execution: mustProjectRaceExecution(t, 1, 1, 2, 0,
			map[int]bool{0: true}, executionBatches{}, executionBatches{
				Attempted: 1, RankComplete: 1,
			}),
		ResetCount: make([]int, 2),
	}); err != nil {
		t.Fatal(err)
	}
	return output.String()
}

func renderGoldenRaceFailed(t *testing.T) string {
	report, refs, output := goldenReportStart(t, "race", "control", "candidate")
	reporter := newTextRaceReporter(report)
	if err := reporter.Run(RunRecord{
		Trials: 1, TrialsRequested: 1, TrialBatchWidth: 2, Connections: 1,
		Delivery: goldenDelivery(), ArrangementPolicy: arrangeNone,
		ResponseLimits: defaultResponseLimits(),
	}); err != nil {
		t.Fatal(err)
	}
	goldenRaceRequests(t, reporter, refs)
	wantErr := errors.New("connection reset during response collection")
	if err := reporter.Trial(TrialRecord{Trial: &tth2.Trial{
		Arrangement: []int{0, 1}, Index: 0, Cycle: -1, Shift: -1,
		Err: wantErr,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := reporter.Summary(SummaryRecord{
		Execution: mustProjectRaceExecution(t, 1, 1, 2, 0,
			map[int]bool{0: true}, executionBatches{}, executionBatches{
				Attempted: 1, Incomplete: 1,
			}),
		ResetCount: make([]int, 2),
		Completion: failedRun(wantErr, completionAcquisitionFailed),
	}); err != nil {
		t.Fatal(err)
	}
	return output.String()
}

func goldenRaceRequests(t *testing.T, reporter RaceReporter, refs runReferences) {
	t.Helper()
	for _, ref := range refs.refs {
		if err := reporter.Request(RequestRecord{Reference: ref}); err != nil {
			t.Fatal(err)
		}
	}
}

func renderGoldenMeasureAvailable(t *testing.T) string {
	report, refs, output := goldenReportStart(t, "measure", "early", "late")
	plan := measurePlan{
		requested: 4, planned: 4, width: 2,
		arrangement: tth2.ArrangeRotate, delivery: goldenDelivery(),
		responseLimits: defaultResponseLimits(),
	}
	reporter := newTextMeasurementReporter(report, allRowsView, refs, plan)
	if err := reporter.Run(1); err != nil {
		t.Fatal(err)
	}
	measured := measurementFromRanks(
		[][]int{{0, 1}, {0, 1}, {0, 1}, {0, 1}},
		4, 2, tth2.ArrangeRotate)
	if err := reporter.Summary(measured); err != nil {
		t.Fatal(err)
	}
	return output.String()
}

func renderGoldenMeasureUnavailable(t *testing.T) string {
	report, refs, output := goldenReportStart(t, "measure", "control", "candidate")
	plan := measurePlan{
		requested: 2, planned: 2, width: 2,
		arrangement: tth2.ArrangeRotate, delivery: goldenDelivery(),
		responseLimits: defaultResponseLimits(),
	}
	reporter := newTextMeasurementReporter(report, allRowsView, refs, plan)
	if err := reporter.Run(1); err != nil {
		t.Fatal(err)
	}
	measured := newMeasurement(2, 2, 2, 1, tth2.ArrangeRotate)
	wantErr := errors.New("connection closed before a complete trial")
	measured.finish(&tth2.TrialSummary{ResetCounts: make([]int, 2)}, wantErr)
	measured.completion = failedRun(wantErr, completionAcquisitionFailed)
	if err := reporter.Summary(measured); err != nil {
		t.Fatal(err)
	}
	return output.String()
}

func renderGoldenAnalyseDifference(t *testing.T) string {
	return renderGoldenAnalyse(t,
		analyseOffline(
			analyseOrderedEvidence(2, 6, func(int) []int { return []int{0, 1} }),
			analyseConfig{RandomisationSeed: 7}),
		[]string{"early", "late"})
}

func renderGoldenAnalyseNoDetection(t *testing.T) string {
	return renderGoldenAnalyse(t,
		analyseOffline(analyseOrderedEvidence(2, 6, func(cycle int) []int {
			if cycle%2 == 0 {
				return []int{0, 1}
			}
			return []int{1, 0}
		}), analyseConfig{}),
		[]string{"control", "candidate"})
}

func renderGoldenAnalyseUnavailable(t *testing.T) string {
	return renderGoldenAnalyse(t,
		analyseOffline(
			analyseOrderedEvidence(3, 6, func(int) []int { return []int{0, 1, 2} }),
			analyseConfig{}),
		[]string{"A", "B", "C"})
}

func renderGoldenAnalyseResponseReview(t *testing.T) string {
	evidence := analyseOrderedEvidence(2, 6, func(int) []int { return []int{0, 1} })
	for cycle := range evidence.Cycles {
		for trial := range evidence.Cycles[cycle].Trials {
			if (cycle+trial)%2 == 0 {
				evidence.Cycles[cycle].Trials[trial].Statuses[1] = http.StatusInternalServerError
			}
		}
	}
	return renderGoldenAnalyse(t, analyseOffline(evidence, analyseConfig{}),
		[]string{"control", "candidate"})
}

func renderGoldenAnalyse(t *testing.T, result analyseRunResult, labels []string) string {
	t.Helper()
	report, refs, output := goldenReportStart(t, "analyse", labels...)
	emitln(report)
	if err := reportAnalyseDocumentWithContext(report, result, refs.inlineLabels(),
		false, analyseReportContext{
			Delivery: goldenDelivery(), ResponseLimits: defaultResponseLimits(),
		}); err != nil {
		t.Fatal(err)
	}
	return output.String()
}

func renderGoldenDetectFound(t *testing.T) string {
	result := detectRunResult{
		Ranking: ranking.OutlierResult{
			Outcome: ranking.OutlierFound, Index: 2,
			OutlierGuarantee: ranking.OutlierGuarantee{
				ErrorDirection: ranking.OutlierErrorFalseFind,
				ErrorRiskLimit: defaultFalsePositiveRisk,
				Direction:      ranking.Late,
				Premise:        ranking.OutlierPremiseScheduledReferenceMean,
				Regime:         ranking.OutlierInferenceAnytimeValid,
			},
			Cost: ranking.Cost{Comparisons: 12, Participations: 36},
			Items: []ranking.ItemStat{
				{Samples: 8, MeanRank: 0.75},
				{Samples: 11, MeanRank: 1.25},
				{Samples: 17, MeanRank: 2},
			},
		},
		Requests: []detectRequestEvidence{
			{Request: 0, Participations: 8, StatusCounts: []detectStatusCount{{Status: 204, Count: 8}}},
			{Request: 1, Participations: 11, StatusCounts: []detectStatusCount{{Status: 204, Count: 11}}},
			{Request: 2, Participations: 17, StatusCounts: []detectStatusCount{{Status: 204, Count: 17}}},
		},
		Execution:  detectExecutionEvidence{Recorded: true, ConnectionsObserved: 1},
		Completion: completedRun(),
	}
	return renderGoldenDetect(t, result, []string{"reference-a", "reference-b", "candidate"})
}

func renderGoldenDetectNegative(t *testing.T) string {
	result := detectRunResult{
		Ranking: ranking.OutlierResult{
			Outcome: ranking.OutlierNegative, Index: -1,
			OutlierGuarantee: ranking.OutlierGuarantee{
				ErrorDirection: ranking.OutlierErrorFalseNegative,
				ErrorRiskLimit: defaultFalseNegativeRisk,
				MinimumEdge:    ranking.OutlierDefaultMinimumEdge, Direction: ranking.Late,
				Premise: ranking.OutlierPremiseScheduledReferenceMean,
				Regime:  ranking.OutlierInferenceAnytimeValid,
			},
			Cost: ranking.Cost{Comparisons: 20, Participations: 60},
			Items: []ranking.ItemStat{
				{Samples: 20, MeanRank: 0.95},
				{Samples: 20, MeanRank: 1},
				{Samples: 20, MeanRank: 1.05},
			},
		},
		Requests: []detectRequestEvidence{
			{Request: 0, Participations: 20, StatusCounts: []detectStatusCount{{Status: 204, Count: 20}}},
			{Request: 1, Participations: 20, StatusCounts: []detectStatusCount{{Status: 204, Count: 20}}},
			{Request: 2, Participations: 20, StatusCounts: []detectStatusCount{{Status: 204, Count: 20}}},
		},
		Execution:  detectExecutionEvidence{Recorded: true, ConnectionsObserved: 1},
		Completion: completedRun(),
	}
	return renderGoldenDetect(t, result, []string{"A", "B", "C"})
}

func renderGoldenDetectUnavailable(t *testing.T) string {
	wantErr := errors.New("connection closed during adaptive comparison")
	result := detectRunResult{
		Ranking: ranking.OutlierResult{
			Outcome: ranking.OutlierInconclusive, Index: -1,
			Cost:  ranking.Cost{Comparisons: 1, Participations: 2},
			Items: []ranking.ItemStat{{Samples: 1, MeanRank: 0}, {Samples: 1, MeanRank: 1}},
		},
		Requests: []detectRequestEvidence{
			{Request: 0, Participations: 1, StatusCounts: []detectStatusCount{{Status: 204, Count: 1}}},
			{Request: 1, Participations: 1, StatusCounts: []detectStatusCount{{Status: 204, Count: 1}}},
		},
		Execution:   detectExecutionEvidence{Recorded: true, ConnectionsObserved: 1},
		Unavailable: true, Reason: wantErr,
		Completion: failedRun(wantErr, completionAcquisitionFailed),
	}
	return renderGoldenDetect(t, result, []string{"control", "candidate"})
}

func renderGoldenDetect(t *testing.T, result detectRunResult, labels []string) string {
	t.Helper()
	report, refs, output := goldenReportStart(t, "detect", labels...)
	plan := detectExperimentPlan{
		Candidates: len(labels), WidthRequested: defaultWidth,
		StrategyRequested: detectStrategyAuto,
		StrategyResolved:  detectStrategyPeerFirst,
		SolverMethod:      (&ranking.PeerFirstOutlierSolver{}).MethodID(),
		WidthEffective:    len(labels),
		FalsePositiveRisk: defaultFalsePositiveRisk,
		FalseNegativeRisk: defaultFalseNegativeRisk,
		Direction:         ranking.Late, ComparisonsMax: 100,
		ConnectionLimit:         1,
		ParticipationLimit:      100 * len(labels),
		MinimumEdge:             ranking.OutlierDefaultMinimumEdge,
		InFlightComparisonLimit: 1,
		EvidenceUpdatePolicy:    "complete-atomic-wave",
		DependencePremise:       "arbitrary-within-atomic-wave",
		ResponseLimits:          defaultResponseLimits(),
		Delivery:                goldenDelivery(),
	}
	reportDetectExperiment(report, plan)
	reportDetectResult(report, result, detectReportContext{
		ComparisonsMax:         plan.ComparisonsMax,
		ConnectionLimit:        plan.ConnectionLimit,
		ConfiguredNegativeEdge: plan.MinimumEdge,
	}, plan.WidthEffective, allRowsView, refs)
	if err := report.Err(); err != nil {
		t.Fatal(err)
	}
	return output.String()
}
