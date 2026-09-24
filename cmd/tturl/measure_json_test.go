package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/tantosec/tturl/internal/h2test"
	"github.com/tantosec/tturl/tth2"
)

func TestMeasureJSONIntegration(t *testing.T) {
	t.Parallel()
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	}))
	out, stdout, stderr := captureOutput(t)
	err := runMeasure(t.Context(), []string{
		"--report", "json",
		"--insecure",
		"--trials", "3",
		"--warmup", "1",
		"--connections", "1",
		"--block", "https://" + addr + "/a",
		"--name", "first",
		"--block", "https://" + addr + "/b",
		"--name", "second",
	}, out)
	if err != nil {
		t.Fatalf("runMeasure: %v", err)
	}

	lines := jsonLines(t, stdout.Bytes())
	if len(lines) != 4 {
		t.Fatalf("records = %d, want run + 2 requests + summary:\n%s",
			len(lines), stdout.String())
	}
	validateStructuredRecords(t, lines)
	wantKinds := []string{"run", "request", "request", "result"}
	for i, line := range lines {
		if got := recordKind(t, line); got != wantKinds[i] {
			t.Errorf("record %d kind = %q, want %q", i, got, wantKinds[i])
		}
	}

	var run jsonMeasureRunRecord
	if err := json.Unmarshal(lines[0], &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.Schema != measureSchema.logical || run.Command != "measure" ||
		run.RequestCount != 2 || run.Plan.Work.Requested != 3 ||
		run.Plan.Work.PlannedTrials != 4 ||
		run.Plan.Warmup.TrialsPerConnection != 1 ||
		run.Plan.Arrangement != "rotate" ||
		run.Acquisition.Delivery.ConnectionLimit != 1 ||
		run.Acquisition.Delivery.Selection.Mode != "fixed" {
		t.Errorf("run = %+v", run)
	}
	if run.Plan.Work.Unit != "trials" || run.Plan.Work.Requested != 3 {
		t.Errorf("work = %+v", run.Plan.Work)
	}
	if run.Plan.Work.PlannedCycles == nil ||
		*run.Plan.Work.PlannedCycles != 2 ||
		run.Plan.PositionBalance != "exact_per_complete_cycle" {
		t.Errorf("run plan = %+v", run)
	}

	for i, wantLabel := range []string{"first", "second"} {
		var request jsonMeasureRequestRecord
		if err := json.Unmarshal(lines[i+1], &request); err != nil {
			t.Fatalf("decode request %d: %v", i, err)
		}
		if request.RequestID != i || request.Role != "measured" ||
			request.Label != wantLabel {
			t.Errorf("request %d = %+v", i, request)
		}
	}

	var summary jsonMeasureResultRecord
	if err := json.Unmarshal(lines[3], &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if summary.Execution.Trials != (jsonMeasureExecutionTrials{
		Planned: 4, Attempted: 4, RankComplete: 4, Retained: 4,
	}) {
		t.Errorf("trials = %+v", summary.Execution.Trials)
	}
	if summary.Execution.Connections != (structuredExecutionConnections{
		Planned: 1, Observed: 1, Replacements: 0,
	}) {
		t.Errorf("connections = %+v", summary.Execution.Connections)
	}
	if summary.Execution.Cycles == nil ||
		*summary.Execution.Cycles != (jsonMeasureExecutionCycles{
			Planned: 2, Attempted: 2, Retained: 2,
		}) {
		t.Errorf("cycles = %+v", summary.Execution.Cycles)
	}
	if summary.Measurement.PositionBalance != "exact" ||
		summary.Measurement.State != "available" ||
		summary.Completion.State != completionComplete {
		t.Errorf("summary state = %+v", summary)
	}
	if summary.Measurement.PositionSpread.MinMeanRank == nil ||
		summary.Measurement.PositionSpread.MaxMeanRank == nil ||
		summary.Measurement.PositionSpread.Raw == nil ||
		summary.Measurement.PositionSpread.Normalised == nil {
		t.Errorf("spread = %+v", summary.Measurement.PositionSpread)
	}
	if len(summary.Measurement.Requests) != 2 || len(summary.Measurement.Positions) != 2 ||
		len(summary.Measurement.PairwisePrecedence.Counts) != 2 ||
		len(summary.Measurement.Assignments.Counts) != 2 {
		t.Fatalf("summary widths = requests %d, positions %d, pairwise %d, assignments %d",
			len(summary.Measurement.Requests), len(summary.Measurement.Positions),
			len(summary.Measurement.PairwisePrecedence.Counts), len(summary.Measurement.Assignments.Counts))
	}
	for row, requestID := range summary.Measurement.PairwisePrecedence.BeforeRequestIDs {
		counts := summary.Measurement.PairwisePrecedence.Counts[row]
		if requestID != row || len(counts) != 2 || counts[row] != 0 ||
			counts[1-row]+summary.Measurement.PairwisePrecedence.Counts[1-row][row] != 4 {
			t.Errorf("pairwise precedence = %+v", summary.Measurement.PairwisePrecedence)
		}
	}
	if !slices.Equal(summary.Measurement.PairwisePrecedence.BeforeRequestIDs, []int{0, 1}) ||
		!slices.Equal(summary.Measurement.PairwisePrecedence.AfterRequestIDs, []int{0, 1}) ||
		!slices.Equal(summary.Measurement.Assignments.RequestIDs, []int{0, 1}) ||
		!slices.Equal(summary.Measurement.Assignments.Positions, []int{0, 1}) {
		t.Fatalf("aggregate axes = %+v/%+v", summary.Measurement.PairwisePrecedence, summary.Measurement.Assignments)
	}
	for request, counts := range summary.Measurement.Assignments.Counts {
		if !slices.Equal(counts, []int{2, 2}) {
			t.Errorf("request %d position counts = %v, want [2 2]", request, counts)
		}
	}
	for _, stats := range summary.Measurement.Requests {
		if stats.Samples != 4 || sumMeasureRankCounts(stats.RankCounts) != 4 ||
			stats.MeanRank == nil || stats.NormalisedMeanRank == nil ||
			*stats.MeanRank != *stats.NormalisedMeanRank ||
			!slices.Equal(stats.StatusCounts,
				[]structuredStatusCount{{Status: http.StatusNoContent, Count: 4}}) {
			t.Errorf("request stats = %+v", stats)
		}
	}
	for _, stats := range summary.Measurement.Positions {
		if stats.Samples != 4 || sumMeasureRankCounts(stats.RankCounts) != 4 ||
			stats.MeanRank == nil {
			t.Errorf("position stats = %+v", stats)
		}
	}
	wantRaw := *summary.Measurement.PositionSpread.MaxMeanRank -
		*summary.Measurement.PositionSpread.MinMeanRank
	if *summary.Measurement.PositionSpread.Raw != wantRaw ||
		*summary.Measurement.PositionSpread.Normalised != wantRaw {
		t.Errorf("position spread = %+v, want raw and normalised %v",
			summary.Measurement.PositionSpread, wantRaw)
	}
	for _, b := range stdout.Bytes() {
		if b >= 0x80 {
			t.Fatalf("JSON output contains non-ASCII byte 0x%02x", b)
		}
	}
	if !strings.Contains(stderr.String(), "Target:") ||
		!strings.Contains(stderr.String(), "Total: 2 requests.") ||
		strings.Contains(stdout.String(), "Target:") ||
		strings.Contains(stdout.String(), "Measurement") {
		t.Errorf("report streams are not separated:\nstdout:\n%s\nstderr:\n%s",
			stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), diagnosticPrefix(diagnosticWarning)) {
		t.Errorf("replicated balanced plan emitted a warning:\n%s", stderr)
	}
}

func TestMeasureJSONRendersPlanWarningAfterRequestEcho(t *testing.T) {
	t.Parallel()
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	}))
	out, _, stderr := captureOutput(t)
	err := runMeasure(t.Context(), []string{
		"--report", "json", "--insecure", "--trials", "1",
		"--arrange", "none",
		"--block", "https://" + addr + "/a",
		"--block", "https://" + addr + "/b",
	}, out)
	if err != nil {
		t.Fatalf("runMeasure: %v", err)
	}
	got := stderr.String()
	want := "Total: 2 requests.\n\n" + diagnosticPrefix(diagnosticWarning) +
		"fixed request positions confound request identity"
	if !strings.Contains(got, want) {
		t.Errorf("stderr does not place plan warning after request echo:\n%s", got)
	}
}

func TestMeasureCyclesJSONIntegration(t *testing.T) {
	t.Parallel()
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	}))
	out, stdout, _ := captureOutput(t)
	err := runMeasure(t.Context(), []string{
		"--report", "json", "--insecure", "--cycles", "2",
		"--block", "https://" + addr + "/a",
		"--block", "https://" + addr + "/b",
	}, out)
	if err != nil {
		t.Fatalf("runMeasure: %v", err)
	}
	lines := jsonLines(t, stdout.Bytes())
	var run jsonMeasureRunRecord
	if err := json.Unmarshal(lines[0], &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.Plan.Work.Unit != "cycles" || run.Plan.Work.Requested != 2 || run.Plan.Work.PlannedTrials != 4 ||
		run.Plan.Work.PlannedCycles == nil ||
		*run.Plan.Work.PlannedCycles != 2 {
		t.Errorf("cycle run = %+v", run)
	}
	var summary jsonMeasureResultRecord
	if err := json.Unmarshal(lines[len(lines)-1], &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if summary.Execution.Trials.Planned != 4 ||
		summary.Execution.Trials.Retained != 4 ||
		summary.Execution.Cycles == nil ||
		summary.Execution.Cycles.Planned != 2 ||
		summary.Execution.Cycles.Retained != 2 {
		t.Errorf("cycle summary = %+v", summary)
	}
}

func TestMeasureJSONPreservesExactRandomPositionCounts(t *testing.T) {
	measured := measurementFromRanks(
		[][]int{{0, 1}, {1, 0}, {0, 1}}, 3, 2, tth2.ArrangeRandom)
	var output bytes.Buffer
	if err := startMeasureJSONCatalogue(t, &output, measured).Summary(measured); err != nil {
		t.Fatal(err)
	}
	var summary jsonMeasureResultRecord
	if err := json.Unmarshal(lastJSONRecord(t, output.Bytes()), &summary); err != nil {
		t.Fatal(err)
	}
	want := [][]int{{3, 0}, {0, 3}}
	if !slices.EqualFunc(summary.Measurement.Assignments.Counts, want, slices.Equal) {
		t.Fatalf("request-position counts = %v, want %v",
			summary.Measurement.Assignments.Counts, want)
	}
}

func TestMeasureDefaultBudgetJSONIntegration(t *testing.T) {
	t.Parallel()
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	}))
	out, stdout, _ := captureOutput(t)
	err := runMeasure(t.Context(), []string{
		"--report", "json", "--insecure",
		"--block", "https://" + addr + "/a",
		"--block", "https://" + addr + "/b",
	}, out)
	if err != nil {
		t.Fatalf("runMeasure: %v", err)
	}
	lines := jsonLines(t, stdout.Bytes())
	var run jsonMeasureRunRecord
	if err := json.Unmarshal(lines[0], &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.Plan.Work.Unit != "trials" ||
		run.Plan.Work.Requested != 20 || run.Plan.Work.PlannedTrials != 20 {
		t.Errorf("default run budget = %+v", run)
	}
}

func TestMeasureJSONPresentationFlagsRetainEveryOutcome(t *testing.T) {
	t.Parallel()
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		if r.URL.Path == "/review" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	out, stdout, _ := captureOutput(t)
	err := runMeasure(t.Context(), []string{
		"--report", "json", "--insecure", "--trials", "3",
		"--rank-rows", "0", "--verbose",
		"--block", "https://" + addr + "/a",
		"--block", "https://" + addr + "/review",
		"--block", "https://" + addr + "/c",
	}, out)
	if err != nil {
		t.Fatalf("runMeasure: %v", err)
	}
	lines := jsonLines(t, stdout.Bytes())
	var summary jsonMeasureResultRecord
	if err := json.Unmarshal(lines[len(lines)-1], &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if len(summary.Measurement.Requests) != 3 ||
		len(summary.Measurement.Assignments.Counts) != 3 ||
		!slices.Equal(summary.Measurement.Requests[0].StatusCounts,
			[]structuredStatusCount{{Status: http.StatusNoContent, Count: 3}}) ||
		!slices.Equal(summary.Measurement.Requests[1].StatusCounts,
			[]structuredStatusCount{{Status: http.StatusNotFound, Count: 3}}) ||
		!slices.Equal(summary.Measurement.Requests[2].StatusCounts,
			[]structuredStatusCount{{Status: http.StatusNoContent, Count: 3}}) ||
		!summary.Measurement.Requests[1].Non2xxObserved {
		t.Errorf("presentation flags changed structured outcomes: %+v", summary)
	}
}

func TestMeasureJSONNoCompleteTrialsUsesNullMeans(t *testing.T) {
	t.Parallel()
	measured := newMeasurement(2, 2, 2, 1, tth2.ArrangeRotate)
	for shift := range 2 {
		mustObserveMeasurement(measured, &tth2.Trial{
			Index: shift, Cycle: 0, Shift: shift, Conn: 0,
			Err: errors.New("reset"),
		})
	}
	runErr := errors.New("connection failed")
	measured.finish(&tth2.TrialSummary{
		RankCounts:         newRankMatrix(2),
		PositionRankCounts: newRankMatrix(2),
		FailedTrials:       2,
		ResetCounts:        []int{1, 0},
	}, runErr)

	var out bytes.Buffer
	reporter := startMeasureJSONCatalogue(t, &out, measured)
	if err := reporter.Summary(measured); err != nil {
		t.Fatalf("Summary: %v", err)
	}
	lines := jsonLines(t, out.Bytes())
	var summary jsonMeasureResultRecord
	if err := json.Unmarshal(lines[len(lines)-1], &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	for _, stats := range summary.Measurement.Requests {
		if stats.MeanRank != nil || stats.NormalisedMeanRank != nil {
			t.Errorf("request means = %v/%v, want null",
				stats.MeanRank, stats.NormalisedMeanRank)
		}
		if stats.StatusCounts == nil || len(stats.StatusCounts) != 0 {
			t.Errorf("status counts = %v, want []", stats.StatusCounts)
		}
	}
	for _, stats := range summary.Measurement.Positions {
		if stats.MeanRank != nil {
			t.Errorf("position mean = %v, want null", stats.MeanRank)
		}
	}
	if summary.Measurement.PositionSpread.MinMeanRank != nil ||
		summary.Measurement.PositionSpread.MaxMeanRank != nil ||
		summary.Measurement.PositionSpread.Raw != nil ||
		summary.Measurement.PositionSpread.Normalised != nil {
		t.Errorf("position spread = %+v, want all null", summary.Measurement.PositionSpread)
	}
	if summary.Execution.Cycles == nil ||
		summary.Execution.Cycles.Incomplete != 1 ||
		summary.Measurement.PositionBalance != "unobserved" ||
		summary.Measurement.State != "unavailable" ||
		summary.Measurement.Reason != "no_retained_trials" ||
		summary.Completion.Failure == nil || string(summary.Completion.Failure.Message) != runErr.Error() {
		t.Errorf("failure design = %+v", summary)
	}
	if summary.Execution.Trials.RankComplete != 0 ||
		summary.Execution.Trials.Incomplete != 2 ||
		summary.Execution.Trials.Retained != 0 ||
		summary.Execution.Trials.Excluded != 0 {
		t.Errorf("failure trials = %+v", summary.Execution.Trials)
	}
}

func TestMeasureJSONRequestResponseFacts(t *testing.T) {
	t.Parallel()
	measured := measurementFromStatuses([][]int{
		{201, 200}, {201, 429}, {201, 500},
	})
	var out bytes.Buffer
	reporter := startMeasureJSONCatalogue(t, &out, measured)
	if err := reporter.Summary(measured); err != nil {
		t.Fatalf("Summary: %v", err)
	}
	var summary jsonMeasureResultRecord
	if err := json.Unmarshal(lastJSONRecord(t, out.Bytes()), &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if !slices.Equal(summary.Measurement.Requests[0].StatusCounts,
		[]structuredStatusCount{{Status: 201, Count: 3}}) ||
		!slices.Equal(summary.Measurement.Requests[1].StatusCounts,
			[]structuredStatusCount{
				{Status: 200, Count: 1},
				{Status: 429, Count: 1},
				{Status: 500, Count: 1},
			}) {
		t.Errorf("status distributions = %+v", summary.Measurement.Requests)
	}
	if summary.Measurement.Requests[0].StatusVaried || summary.Measurement.Requests[0].Non2xxObserved ||
		!summary.Measurement.Requests[1].StatusVaried || !summary.Measurement.Requests[1].Non2xxObserved {
		t.Errorf("response facts = %+v", summary.Measurement.Requests)
	}
}

func TestMeasureJSONReportsRetainedAndExcludedRotationTrials(t *testing.T) {
	t.Parallel()
	measured := newMeasurement(4, 4, 2, 1, tth2.ArrangeRotate)
	for _, trial := range []*tth2.Trial{
		completeMeasurementTrial(2, 0, 0, 0),
		completeMeasurementTrial(2, 0, 1, 0),
		completeMeasurementTrial(2, 1, 0, 0),
		{
			Index: 3, Cycle: 1, Shift: 1, Conn: 0,
			Err: errors.New("reset"),
		},
	} {
		mustObserveMeasurement(measured, trial)
	}
	measured.finish(&tth2.TrialSummary{
		Trials: 3, FailedTrials: 1,
		RankCounts:         newRankMatrix(2),
		PositionRankCounts: newRankMatrix(2),
		ResetCounts:        []int{0, 1},
	}, nil)

	var out bytes.Buffer
	reporter := startMeasureJSONCatalogue(t, &out, measured)
	if err := reporter.Summary(measured); err != nil {
		t.Fatalf("Summary: %v", err)
	}
	var summary jsonMeasureResultRecord
	if err := json.Unmarshal(lastJSONRecord(t, out.Bytes()), &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	wantTrials := jsonMeasureExecutionTrials{
		Planned: 4, Attempted: 4, RankComplete: 3,
		Incomplete: 1, Retained: 2, Excluded: 1,
	}
	if summary.Execution.Trials != wantTrials {
		t.Errorf("trials = %+v, want %+v", summary.Execution.Trials, wantTrials)
	}
	wantCycles := jsonMeasureExecutionCycles{
		Planned: 2, Attempted: 2, Retained: 1, Incomplete: 1,
	}
	if summary.Execution.Cycles == nil ||
		*summary.Execution.Cycles != wantCycles {
		t.Errorf("cycles = %+v, want %+v", summary.Execution.Cycles, wantCycles)
	}
	if summary.Measurement.PositionBalance != "exact" || summary.Measurement.Requests[1].ResetCount != 1 {
		t.Errorf("measurement integrity = %+v", summary.Measurement)
	}
	for _, request := range summary.Measurement.Requests {
		if request.Samples != 2 || sumMeasureRankCounts(request.RankCounts) != 2 {
			t.Errorf("request ranks include excluded trials: %+v", request)
		}
	}
	for _, position := range summary.Measurement.Positions {
		if position.Samples != 2 || sumMeasureRankCounts(position.RankCounts) != 2 {
			t.Errorf("position ranks include excluded trials: %+v", position)
		}
	}
}

func TestMeasureJSONBalanceMatchesArrangement(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		arrangement tth2.ArrangementPolicy
		observe     []*tth2.Trial
		want        string
	}{
		{
			name: "rotate exact", arrangement: tth2.ArrangeRotate,
			observe: []*tth2.Trial{
				measurementTrial(0, 0, 0, nil),
				measurementTrial(0, 1, 0, nil),
				measurementTrial(0, 2, 0, nil),
			},
			want: "exact",
		},
		{
			name: "rotate incomplete", arrangement: tth2.ArrangeRotate,
			observe: []*tth2.Trial{
				measurementTrial(0, 0, 0, nil),
			},
			want: "unobserved",
		},
		{
			name: "rotate unobserved", arrangement: tth2.ArrangeRotate,
			want: "unobserved",
		},
		{name: "random", arrangement: tth2.ArrangeRandom, want: "in_expectation"},
		{name: "none", arrangement: tth2.ArrangeNone, want: "none"},
	} {
		t.Run(test.name, func(t *testing.T) {
			measured := newMeasurement(3, 3, 3, 1, test.arrangement)
			for _, trial := range test.observe {
				mustObserveMeasurement(measured, trial)
			}
			if got := measurementPositionBalance(measured); got != test.want {
				t.Errorf("balance = %q, want %q", got, test.want)
			}
		})
	}
}

func TestMeasureJSONReporterReturnsWriteFailure(t *testing.T) {
	t.Parallel()
	want := errors.New("write failed")
	reporter := newJSONMeasureReporter(
		failingWriter{err: want}, MeasureRunRecord{Delivery: deliveryPlan{connectionCeiling: 1, connectionLimit: 1}}, nil)
	if err := reporter.Run(1); !errors.Is(err, want) {
		t.Errorf("Run error = %v, want %v", err, want)
	}
}

func sumMeasureRankCounts(counts []jsonMeasureRankCount) int {
	result := 0
	for _, count := range counts {
		result += count.Count
	}
	return result
}

func denseMeasureRankCounts(counts []jsonMeasureRankCount, width int) []int {
	result := make([]int, width)
	for _, count := range counts {
		result[count.Rank] = count.Count
	}
	return result
}
