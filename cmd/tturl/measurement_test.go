package main

import (
	"errors"
	"math"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/tantosec/tturl/tth2"
)

func TestMeasurementTracksDesignIntegrity(t *testing.T) {
	measured := newMeasurement(5, 6, 3, 2, tth2.ArrangeRotate)
	mustObserveMeasurement(measured, &tth2.Trial{Warmup: true, Conn: 0})
	mustObserveMeasurement(measured, &tth2.Trial{Warmup: true, Conn: 1})

	for shift := range 3 {
		mustObserveMeasurement(measured, measurementTrial(0, shift, 0, nil))
	}
	mustObserveMeasurement(measured, measurementTrial(1, 0, 1, nil))
	mustObserveMeasurement(measured,
		measurementTrial(1, 1, 1, errors.New("reset")))
	mustObserveMeasurement(measured, measurementTrial(1, 2, 2, nil))
	measured.finish(&tth2.TrialSummary{Trials: 5, FailedTrials: 1}, nil)

	if measured.requested != 5 || measured.planned != 6 ||
		measured.attempted != 6 || measured.warmups != 2 {
		t.Errorf("trial counts = requested %d, planned %d, attempted %d, warmup %d",
			measured.requested, measured.planned,
			measured.attempted, measured.warmups)
	}
	if measured.connsObserved() != 3 || measured.replacements() != 1 {
		t.Errorf("connections = %d observed, %d replacements; want 3 and 1",
			measured.connsObserved(), measured.replacements())
	}
	cycles := measured.cycleCounts(3)
	if cycles.planned != 2 || cycles.attempted != 2 ||
		cycles.retained != 1 || cycles.incomplete != 1 ||
		cycles.unattempted != 0 {
		t.Errorf("cycle counts = %+v, want 2 intended/attempted, 1 complete/incomplete",
			cycles)
	}
}

func TestMeasurementRetainsCompleteRotationCyclesAtomically(t *testing.T) {
	measured := newMeasurement(4, 4, 2, 1, tth2.ArrangeRotate)
	trials := []*tth2.Trial{
		completeMeasurementTrial(2, 1, 1, 0),
		completeMeasurementTrial(2, 0, 0, 0),
		completeMeasurementTrial(2, 1, 0, 0),
		{
			Arrangement: []int{1, 0}, Index: 1, Cycle: 0, Shift: 1,
			Conn: 0, Err: errors.New("stream reset"),
		},
	}
	for _, trial := range trials {
		mustObserveMeasurement(measured, trial)
	}
	measured.finish(&tth2.TrialSummary{
		Trials: 3, FailedTrials: 1,
		RankCounts:         [][]int{{2, 1}, {1, 2}},
		PositionRankCounts: [][]int{{3, 0}, {0, 3}},
		ResetCounts:        []int{0, 1},
	}, nil)

	if measured.complete != 3 || measured.incomplete != 1 ||
		measured.retained != 2 || measured.excluded != 1 {
		t.Errorf("trial retention = complete %d, incomplete %d, retained %d, excluded %d",
			measured.complete, measured.incomplete,
			measured.retained, measured.excluded)
	}
	if measured.summary.Trials != 2 ||
		!reflect.DeepEqual(measured.summary.RankCounts,
			[][]int{{1, 1}, {1, 1}}) ||
		!reflect.DeepEqual(measured.summary.PositionRankCounts,
			[][]int{{2, 0}, {0, 2}}) {
		t.Errorf("retained summary = %+v", measured.summary)
	}
	if !slices.Equal(measured.summary.ResetCounts, []int{0, 1}) {
		t.Errorf("reset counts = %v, want [0 1]", measured.summary.ResetCounts)
	}
	if !reflect.DeepEqual(measured.assignments, [][]int{{1, 1}, {1, 1}}) {
		t.Errorf("retained assignments = %v, want exact balanced cycle",
			measured.assignments)
	}
	if !reflect.DeepEqual(measured.pairwise, [][]int{{0, 1}, {1, 0}}) {
		t.Errorf("retained pairwise counts = %v, want retained cycle only",
			measured.pairwise)
	}
	for request, counts := range measured.statusCounts {
		if counts[http.StatusOK] != 2 || len(counts) != 1 {
			t.Errorf("request %d status counts = %v, want retained trials only",
				request, counts)
		}
	}
	cycles := measured.cycleCounts(2)
	if cycles.planned != 2 || cycles.attempted != 2 ||
		cycles.retained != 1 || cycles.incomplete != 1 {
		t.Errorf("cycles = %+v", cycles)
	}
	if len(measured.cycles) != 0 {
		t.Errorf("finalised cycle state retained: %v", measured.cycles)
	}
}

func TestMeasurementRotationCompletionOrderDoesNotChangeAggregate(t *testing.T) {
	ordered := []*tth2.Trial{
		completeMeasurementTrial(2, 0, 0, 0),
		completeMeasurementTrial(2, 0, 1, 0),
		completeMeasurementTrial(2, 1, 0, 1),
		completeMeasurementTrial(2, 1, 1, 1),
	}
	outOfOrder := []*tth2.Trial{
		ordered[3], ordered[0], ordered[2], ordered[1],
	}
	aggregate := func(trials []*tth2.Trial) *measurement {
		measured := newMeasurement(4, 4, 2, 2, tth2.ArrangeRotate)
		for _, trial := range trials {
			mustObserveMeasurement(measured, trial)
		}
		measured.finish(&tth2.TrialSummary{
			Trials:             4,
			RankCounts:         newRankMatrix(2),
			PositionRankCounts: newRankMatrix(2),
			ResetCounts:        []int{0, 0},
		}, nil)
		return measured
	}
	want, got := aggregate(ordered), aggregate(outOfOrder)
	if !reflect.DeepEqual(got.summary, want.summary) ||
		!reflect.DeepEqual(got.assignments, want.assignments) ||
		got.cycleTally != want.cycleTally || got.retained != want.retained {
		t.Errorf("out-of-order aggregate = %+v/%+v, want %+v/%+v",
			got.summary, got.cycleTally, want.summary, want.cycleTally)
	}
}

func TestMeasurementFinalisesPartialRotationCycle(t *testing.T) {
	measured := newMeasurement(4, 4, 2, 1, tth2.ArrangeRotate)
	mustObserveMeasurement(measured, completeMeasurementTrial(2, 0, 0, 0))
	measured.finish(&tth2.TrialSummary{
		Trials:             1,
		RankCounts:         [][]int{{1, 0}, {0, 1}},
		PositionRankCounts: [][]int{{1, 0}, {0, 1}},
		ResetCounts:        []int{0, 0},
	}, errors.New("fatal stream error"))

	if measured.retained != 0 || measured.excluded != 1 ||
		measured.summary.Trials != 0 {
		t.Errorf("partial cycle retention = retained %d, excluded %d, trials %d",
			measured.retained, measured.excluded, measured.summary.Trials)
	}
	cycles := measured.cycleCounts(2)
	if cycles.attempted != 1 || cycles.incomplete != 1 ||
		cycles.unattempted != 1 {
		t.Errorf("partial cycle counts = %+v", cycles)
	}
}

func TestMeasurementRetainsHealthyCycleAfterConnectionReplacement(t *testing.T) {
	measured := newMeasurement(6, 6, 2, 1, tth2.ArrangeRotate)
	measured.warmupPerConn = 1
	mustObserveMeasurement(measured, &tth2.Trial{Warmup: true, Conn: 0})
	mustObserveMeasurement(measured, completeMeasurementTrial(2, 0, 0, 0))
	mustObserveMeasurement(measured, &tth2.Trial{
		Arrangement: []int{1, 0}, Index: 1, Cycle: 0, Shift: 1,
		Conn: 0, Err: errors.New("connection died"),
	})
	mustObserveMeasurement(measured, &tth2.Trial{Warmup: true, Conn: 1})
	mustObserveMeasurement(measured, completeMeasurementTrial(2, 1, 1, 1))
	mustObserveMeasurement(measured, completeMeasurementTrial(2, 1, 0, 1))
	measured.finish(&tth2.TrialSummary{
		Trials: 3, FailedTrials: 1,
		RankCounts:         newRankMatrix(2),
		PositionRankCounts: newRankMatrix(2),
		ResetCounts:        []int{0, 0},
	}, nil)

	cycles := measured.cycleCounts(2)
	if measured.retained != 2 || measured.excluded != 1 ||
		cycles.retained != 1 || cycles.incomplete != 1 ||
		cycles.unattempted != 1 || measured.replacements() != 1 ||
		measured.warmups != 2 {
		t.Errorf("replacement accounting: retained %d excluded %d cycles %+v replacements %d warmups %d",
			measured.retained, measured.excluded, cycles,
			measured.replacements(), measured.warmups)
	}
}

func TestMeasurementNonRotationRetainsEveryCompleteTrial(t *testing.T) {
	for _, arrangement := range []tth2.ArrangementPolicy{
		tth2.ArrangeRandom, tth2.ArrangeNone,
	} {
		measured := newMeasurement(2, 2, 2, 1, arrangement)
		warmup := completeMeasurementTrial(2, -1, -1, 0)
		warmup.Warmup = true
		mustObserveMeasurement(measured, warmup)
		mustObserveMeasurement(measured,
			completeMeasurementTrial(2, -1, -1, 0))
		mustObserveMeasurement(measured,
			completeMeasurementTrial(2, -1, -1, 1))
		raw := summaryFromRanks([][]int{{0, 1}, {1, 0}})
		measured.finish(raw, nil)
		if measured.complete != 2 || measured.retained != 2 ||
			measured.excluded != 0 ||
			measured.statusCounts[0][http.StatusOK] != 2 {
			t.Errorf("arrangement %d retention = complete %d, retained %d, excluded %d",
				arrangement, measured.complete,
				measured.retained, measured.excluded)
		}
	}
}

func TestMeasurementRejectsIncompleteCompleteTrialEvidence(t *testing.T) {
	measured := newMeasurement(1, 1, 2, 1, tth2.ArrangeRandom)
	trial := completeMeasurementTrial(2, -1, -1, 0)
	trial.Results[1].Response = nil
	if err := measured.observe(trial); err == nil ||
		!strings.Contains(err.Error(), "internal consistency error") {
		t.Errorf("observe error = %v, want internal consistency error", err)
	}

	measured = newMeasurement(1, 1, 2, 1, tth2.ArrangeRandom)
	if err := measured.observe(completeMeasurementTrial(2, -1, -1, 0)); err != nil {
		t.Fatalf("observe: %v", err)
	}
	measured.finish(summaryFromRanks([][]int{{0, 1}, {1, 0}}), nil)
	if err := measured.reconcile(); err == nil ||
		!strings.Contains(err.Error(), "HTTP statuses for 2 rank samples") {
		t.Errorf("reconcile error = %v, want rank/status mismatch", err)
	}
}

func TestMeasurePlanWorkload(t *testing.T) {
	plan := measurePlan{planned: 20, warmup: 2, width: 4}
	got, err := plan.workload(3)
	if err != nil {
		t.Fatalf("workload: %v", err)
	}
	if got != (plannedWorkload{measured: 80, initialWarmup: 24, total: 104}) {
		t.Errorf("workload = %+v", got)
	}
	for _, plan := range []measurePlan{
		{planned: math.MaxInt, width: 2},
		{planned: 1, warmup: math.MaxInt, width: 2},
	} {
		if _, err := plan.workload(2); err == nil {
			t.Errorf("workload(%+v) accepted overflow", plan)
		}
	}
}

func TestPositionSpreadUsesUnroundedMeans(t *testing.T) {
	summary := summaryFromRanks([][]int{
		{0, 1, 2},
		{1, 0, 2},
		{2, 0, 1},
	})
	got := positionSpread(summary)
	if got.min == nil || got.max == nil || got.raw == nil ||
		got.normalised == nil {
		t.Fatalf("spread has null values: %+v", got)
	}
	// Position means are 1, 1/3, and 5/3. The raw spread is 4/3 and
	// normalisation divides it by the full rank range, 2.
	// Each aggregate uses three exact integer observations. A tolerance of
	// 1e-12 is far below the two-decimal presentation precision while allowing
	// normal subtraction rounding in the derived spread.
	close := func(got, want float64) bool { return math.Abs(got-want) < 1e-12 }
	if !close(*got.min, 1.0/3.0) || !close(*got.max, 5.0/3.0) ||
		!close(*got.raw, 4.0/3.0) || !close(*got.normalised, 2.0/3.0) {
		t.Errorf("spread = min %v max %v raw %v normalised %v",
			*got.min, *got.max, *got.raw, *got.normalised)
	}
	empty := positionSpread(&tth2.TrialSummary{
		PositionRankCounts: newRankMatrix(3),
	})
	if empty.min != nil || empty.max != nil || empty.raw != nil ||
		empty.normalised != nil {
		t.Errorf("empty spread = %+v, want null values", empty)
	}
}

func TestMeasureWarningCodes(t *testing.T) {
	plan := measurePlan{
		planned: 2, width: 2, arrangement: tth2.ArrangeRotate,
	}
	if got, want := measurePlanWarnings(plan),
		[]string{"no_warmup", "one_rotation_cycle_planned"}; !slices.Equal(got, want) {
		t.Errorf("plan warnings = %v, want %v", got, want)
	}
	measured := newMeasurement(2, 2, 2, 1, tth2.ArrangeRotate)
	measured.finish(&tth2.TrialSummary{
		RankCounts:         newRankMatrix(2),
		PositionRankCounts: newRankMatrix(2),
	}, nil)
	if got, want := measurementWarnings(measured),
		[]string{"unattempted_trials", "no_rotation_cycles_retained"}; !slices.Equal(got, want) {
		t.Errorf("measurement warnings = %v, want %v", got, want)
	}
}

func measurementTrial(
	cycle, shift, conn int,
	err error,
) *tth2.Trial {
	results := make([]tth2.Result, 3)
	arrangement := make([]int, 3)
	arrivalOrder := make([]int, 3)
	for i := range 3 {
		results[i] = tth2.Result{
			Response: &tth2.Response{StatusCode: http.StatusOK}, ArrivalRank: i,
		}
		arrangement[i] = i
		arrivalOrder[i] = i
	}
	return &tth2.Trial{
		BatchResult: tth2.BatchResult{
			Results: results, ArrivalOrder: arrivalOrder,
		},
		Arrangement: arrangement,
		Index:       cycle*3 + shift,
		Cycle:       cycle,
		Shift:       shift,
		Conn:        conn,
		Err:         err,
	}
}

func completeMeasurementTrial(
	width, cycle, shift, conn int,
) *tth2.Trial {
	results := make([]tth2.Result, width)
	arrangement := make([]int, width)
	arrivalOrder := make([]int, width)
	for position := range width {
		request := position
		if shift >= 0 {
			request = (position + shift) % width
		}
		arrangement[position] = request
		results[request] = tth2.Result{
			Response:    &tth2.Response{StatusCode: http.StatusOK},
			ArrivalRank: position,
		}
		arrivalOrder[position] = request
	}
	index := -1
	if cycle >= 0 {
		index = cycle*width + shift
	}
	return &tth2.Trial{
		BatchResult: tth2.BatchResult{
			Results: results, ArrivalOrder: arrivalOrder,
		},
		Arrangement: arrangement,
		Index:       index,
		Cycle:       cycle,
		Shift:       shift,
		Conn:        conn,
	}
}

func mustObserveMeasurement(measured *measurement, trial *tth2.Trial) {
	if err := measured.observe(trial); err != nil {
		panic(err)
	}
}
