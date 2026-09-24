package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tantosec/tturl/internal/ranking"
	"github.com/tantosec/tturl/tth2"
)

// These tests exercise the reporters end to end on hand-built inputs: the
// rendering is a pure function of the summary/result structs (the timing
// signal that makes those structs non-deterministic lives upstream, in the
// live send), so they assert the *properties* the layout exists to convey —
// which row carries the outlier marker, which sections appear, that labels
// cross-reference — rather than pinning exact bytes, which would break on
// every cosmetic tweak.

// summaryFromRanks builds a TrialSummary from per-trial arrival orders:
// orders[t][i] is request i's arrival rank in trial t. Without rotation a
// position equals a request identity, so the per-position matrix is keyed by
// request index too.
func summaryFromRanks(orders [][]int) *tth2.TrialSummary {
	n := 0
	if len(orders) > 0 {
		n = len(orders[0])
	}
	s := &tth2.TrialSummary{
		RankCounts:         newRankMatrix(n),
		PositionRankCounts: newRankMatrix(n),
	}
	for _, ord := range orders {
		for i, rank := range ord {
			s.RankCounts[i][rank]++
			s.PositionRankCounts[i][rank]++
		}
		s.Trials++
	}
	return s
}

func measurementFromRanks(
	orders [][]int,
	planned, width int,
	arrangement tth2.ArrangementPolicy,
) *measurement {
	measured := newMeasurement(planned, planned, width, 1, arrangement)
	for index, order := range orders {
		results := make([]tth2.Result, width)
		arrivalOrder := make([]int, width)
		layout := make([]int, width)
		for i, rank := range order {
			results[i] = tth2.Result{
				Response:    &tth2.Response{StatusCode: http.StatusOK},
				ArrivalRank: rank,
			}
			arrivalOrder[rank] = i
			layout[i] = i
		}
		trial := &tth2.Trial{
			BatchResult: tth2.BatchResult{
				Results:      results,
				ArrivalOrder: arrivalOrder,
			},
			Arrangement: layout,
			Index:       index,
			Cycle:       -1,
			Shift:       -1,
			Conn:        0,
		}
		if arrangement == tth2.ArrangeRotate {
			trial.Cycle = index / width
			trial.Shift = index % width
		}
		mustObserveMeasurement(measured, trial)
	}
	measured.finish(summaryFromRanks(orders), nil)
	return measured
}

// markedRow returns the single output line bearing a detect outlier marker
// ("<- ..."), failing if there is not exactly one.
func markedRow(t *testing.T, out string) string {
	t.Helper()
	var marked []string
	for ln := range strings.SplitSeq(out, "\n") {
		if strings.Contains(ln, "<- ") {
			marked = append(marked, ln)
		}
	}
	if len(marked) != 1 {
		t.Fatalf("want exactly one marked row, got %d:\n%s", len(marked), out)
	}
	return marked[0]
}

// boundedView is an explicitly bounded ranking-table layout for reporter
// tests. A small hand-built field still renders whole, so these tests probe the
// verdict/marker properties rather than elision.
var boundedView = rankView{endRows: detectDefaultTop}

// allRowsView is the normal unbounded result view: every ranking row is listed.
var allRowsView = rankView{endRows: detectDefaultTop, allRows: true}

func fullRunReferences(labels []string) runReferences {
	return projectRunReferences(labels, 1000)
}

func TestReportMeasureExperiment(t *testing.T) {
	t.Run("rounded rotation connects design and workload", func(t *testing.T) {
		var out bytes.Buffer
		err := reportMeasureExperiment(&out, measurePlan{
			requested: 3, planned: 4, warmup: 1, width: 2,
			arrangement: tth2.ArrangeRotate,
			delivery:    deliveryPlan{requestRateMax: 10},
			bodyRelease: bodyReleasePlan{
				releaseDelay: 5 * time.Millisecond, hasBodyBytes: true,
			},
		}, 2)
		if err != nil {
			t.Fatalf("reportMeasureExperiment: %v", err)
		}
		flat := strings.Join(strings.Fields(out.String()), " ")
		for _, want := range []string{
			"Experiment",
			"Design: 3 trials requested; 4 trials planned as 2 balanced rotation cycles (8 measured request operations).",
			"Arrangement: rotate; each request occupies every position once per cycle.",
			"Priming: 1 priming attempt per connection (4 request operations), excluded from ranks.",
			"Pacing: 2 requests/batch; batch-rate ceiling unlimited; " +
				"request-rate ceiling 10/s;",
			"Connections: up to 2 active (default).",
			"Body release: no fixed tail or delay; non-empty bodies share a final record.",
		} {
			if !strings.Contains(flat, want) {
				t.Errorf("plan lacks %q:\n%s", want, out.String())
			}
		}
		if strings.Contains(out.String(), "Caution:") {
			t.Errorf("replicated warmed plan has a caution:\n%s", out.String())
		}
	})

	t.Run("cycle budget states expression and quadratic load", func(t *testing.T) {
		plan := measurePlan{
			requested: 40, planned: 40,
			budget:      measureBudget{unit: "cycles", requested: 5},
			warmup:      0,
			width:       8,
			arrangement: tth2.ArrangeRotate,
		}
		var b bytes.Buffer
		if err := reportMeasureExperiment(&b, plan, 1); err != nil {
			t.Fatalf("reportMeasureExperiment: %v", err)
		}
		out := strings.Join(strings.Fields(b.String()), " ")
		for _, want := range []string{
			"Design: 5 balanced rotation cycles (40 trials, 320 measured request operations).",
			"Arrangement: rotate; each request occupies every position once per cycle.",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("cycle plan lacks %q:\n%s", want, b.String())
			}
		}
	})

	t.Run("one cold rotation cycle states structural limits", func(t *testing.T) {
		var out bytes.Buffer
		err := reportMeasureExperiment(&out, measurePlan{
			requested: 2, planned: 2, width: 2,
			arrangement: tth2.ArrangeRotate,
		}, 1)
		if err != nil {
			t.Fatalf("reportMeasureExperiment: %v", err)
		}
		flat := strings.Join(strings.Fields(out.String()), " ")
		for _, want := range []string{
			"Design: 1 balanced rotation cycle (2 trials, 4 measured request operations).",
			"Priming: none; early trials may include cold-state effects.",
			"only 1 complete rotation cycle is planned",
			"no replication across rotation blocks",
		} {
			if !strings.Contains(flat, want) {
				t.Errorf("plan lacks %q:\n%s", want, out.String())
			}
		}
	})

	for _, test := range []struct {
		name        string
		arrangement tth2.ArrangementPolicy
		want        []string
		forbid      string
	}{
		{
			name: "random", arrangement: tth2.ArrangeRandom,
			want: []string{
				"Design: 2 trials (4 measured request operations).",
				"Arrangement: random; shuffled per trial and balanced only in expectation.",
			},
			forbid: "rotation cycle",
		},
		{
			name: "none", arrangement: tth2.ArrangeNone,
			want: []string{
				"Design: 2 trials (4 measured request operations).",
				"Arrangement: none; request identity remains confounded with outbound position.",
			},
			forbid: "rotation cycle",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			err := reportMeasureExperiment(&out, measurePlan{
				requested: 2, planned: 2, warmup: 1, width: 2,
				arrangement: test.arrangement,
			}, 1)
			if err != nil {
				t.Fatalf("reportMeasureExperiment: %v", err)
			}
			flat := strings.Join(strings.Fields(out.String()), " ")
			for _, want := range test.want {
				if !strings.Contains(flat, want) {
					t.Errorf("plan lacks %q:\n%s", want, out.String())
				}
			}
			if strings.Contains(out.String(), test.forbid) {
				t.Errorf("plan contains %q:\n%s", test.forbid, out.String())
			}
		})
	}
}

func TestReportDeliveryPlan(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	plan := deliveryPlan{
		batchRateMax:           200,
		requestRateMax:         300,
		effectiveBatchInterval: 5 * time.Millisecond,
		connectionCeiling:      41,
		connectionLimit:        12,
		connectionSelection:    connectionPathPing,
		pathRTTBasis:           201438946 * time.Nanosecond,
	}
	if err := reportDeliveryPlan(&out, plan, 2); err != nil {
		t.Fatalf("reportDeliveryPlan: %v", err)
	}
	want := "Pacing: 2 requests/batch; batch-rate ceiling 200/s; " +
		"request-rate ceiling 300/s; " +
		"minimum batch-start interval 5ms. Connections: up to 12 active " +
		"for this run; path-fitted ceiling 41 from 201ms HTTP/2 PING RTT " +
		"with 25% (at least one connection) " +
		"headroom; network RTT only; service time excluded. " +
		"Streams: honour the peer's advertised concurrent-stream limit."
	if got := strings.Join(strings.Fields(out.String()), " "); got != want {
		t.Errorf("delivery plan = %q, want %q", got, want)
	}
	for i, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if len(line) > textWidth {
			t.Errorf("line %d is %d columns, want <= %d: %q",
				i+1, len(line), textWidth, line)
		}
	}
}

func TestReportDeliveryPlanCappedFit(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	plan := deliveryPlan{
		batchRateMax:           1000,
		effectiveBatchInterval: time.Millisecond,
		connectionCeiling:      512,
		connectionLimit:        512,
		connectionSelection:    connectionPathPingCapped,
		parallelFitMax:         512,
		parallelFitSet:         true,
		pathRTTBasis:           600 * time.Millisecond,
		pathFittedConnections:  600,
		pathFitCapped:          true,
	}
	if err := reportDeliveryPlan(&out, plan, 2); err != nil {
		t.Fatalf("reportDeliveryPlan: %v", err)
	}
	flat := strings.Join(strings.Fields(out.String()), " ")
	for _, want := range []string{
		"capped at 512 by --connections-fit-max",
		"Caution: path fit requested 600 connections",
		"requested batch cadence unsustainable",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("delivery plan lacks %q:\n%s", want, out.String())
		}
	}
}

func TestReportDeliveryPlanUnlimitedFit(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := reportDeliveryPlan(&out, deliveryPlan{
		connectionCeiling:   8,
		connectionLimit:     8,
		connectionSelection: connectionPathPing,
		parallelFitMax:      -1,
		parallelFitSet:      true,
		pathRTTBasis:        10 * time.Millisecond,
	}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(strings.Fields(out.String()), " "),
		"--connections-fit-max unlimited") {
		t.Errorf("delivery plan does not report the uncapped fit:\n%s", &out)
	}
}

func TestReportersUseProjectedRunReferences(t *testing.T) {
	labels := []string{"extraordinarily-long-request-A", "B"}
	refs := projectRunReferences(labels, 12)
	inline := refs.inlineLabels()
	if !refs.compact {
		t.Fatal("test requires compact references")
	}

	var b bytes.Buffer
	reportMeasurement(&b, measurementFromRanks(
		[][]int{{0, 1}, {1, 0}}, 2, 2, tth2.ArrangeRotate),
		allRowsView, refs)
	reportDetect(&b, ranking.OutlierResult{
		Outcome: ranking.OutlierFound, Index: 0,
		OutlierGuarantee: ranking.OutlierGuarantee{
			ErrorDirection: ranking.OutlierErrorFalseFind, ErrorRiskLimit: 0.05,
			Direction: ranking.Early, Premise: ranking.OutlierPremiseBaselineMean,
			Regime: ranking.OutlierInferenceAnytimeValid,
		},
		Cost: ranking.Cost{Comparisons: 2, Participations: 4},
		Items: []ranking.ItemStat{
			{Samples: 2, MeanRank: 0},
			{Samples: 2, MeanRank: 1},
		},
	}, 2, boundedView, refs)
	out := b.String()

	for _, ref := range inline {
		if !strings.Contains(out, ref) {
			t.Errorf("projected reference %q is absent:\n%s", ref, out)
		}
	}
	if strings.Contains(out, labels[0]) {
		t.Errorf("reporter bypassed projected reference with complete label:\n%s", out)
	}
}

func TestReportPairwisePrecedenceKeepsWideMatrixComplete(t *testing.T) {
	const width = 17
	labels := make([]string, width)
	counts := make([][]int, width)
	for row := range width {
		labels[row] = fmt.Sprintf("R%02d", row)
		counts[row] = make([]int, width)
		for column := range width {
			if row != column {
				counts[row][column] = 1
			}
		}
	}

	var output bytes.Buffer
	reportPairwisePrecedence(&output, counts, labels, 1)
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if got, want := len(lines), width+2; got != want {
		t.Fatalf("matrix has %d lines, want %d:\n%s", got, want, &output)
	}
	if fields := strings.Fields(lines[1]); len(fields) != width+1 || fields[0] != "request" || fields[width] != "R16" {
		t.Fatalf("matrix header is incomplete: %q", lines[1])
	}
	overWidth := false
	for row, line := range lines[2:] {
		fields := strings.Fields(line)
		if len(fields) != width+1 || fields[0] != labels[row] {
			t.Errorf("matrix row %d is incomplete: %q", row, line)
		}
		overWidth = overWidth || len(line) > textWidth
	}
	if !overWidth {
		t.Fatal("wide evidence matrix unexpectedly fit the ordinary text width")
	}
}

func TestReportMeasurement(t *testing.T) {
	// Request B (index 1) always finishes last; A and C alternate the two earlier
	// positions.
	var lateB [][]int
	for k := range 30 {
		if k%2 == 0 {
			lateB = append(lateB, []int{0, 2, 1})
		} else {
			lateB = append(lateB, []int{1, 2, 0})
		}
	}
	names := []string{"A", "B", "C"}

	t.Run("descriptive ranking", func(t *testing.T) {
		var b bytes.Buffer
		reportMeasurement(&b,
			measurementFromRanks(lateB, 30, 3, tth2.ArrangeRotate),
			boundedView, fullRunReferences(names))
		out := b.String()

		if !strings.Contains(out, "B        [......@@]") ||
			!strings.Contains(out, " 2.00") {
			t.Errorf("ranking does not show B at mean rank 2:\n%s", out)
		}
		if !strings.Contains(out,
			"Descriptive result only; no statistical finding is inferred.") {
			t.Errorf("missing descriptive-result qualification:\n%s", out)
		}
		if !strings.Contains(out, "Position check") {
			t.Errorf("rotation on: want a Position check section:\n%s", out)
		}
		for _, want := range []string{
			"Pairwise precedence (row-before-column fraction):",
			"A           -  1.00  0.50",
			"B        0.00     -  0.00",
			"C        0.50  1.00     -",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("pairwise matrix lacks %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "Caution:") {
			t.Errorf("position-spread magnitude produced a caution:\n%s", out)
		}
		for _, want := range []string{
			"30/30 trials were rank-complete and retained in 10/10 balanced cycles",
			"One planned connection was observed; no resets or replacements",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("missing execution evidence %q:\n%s", want, out)
			}
		}
	})

	t.Run("arrange none explains position aliasing", func(t *testing.T) {
		var b bytes.Buffer
		reportMeasurement(&b,
			measurementFromRanks(lateB, 30, 3, tth2.ArrangeNone),
			boundedView, fullRunReferences(names))
		out := b.String()
		flat := strings.Join(strings.Fields(out), " ")
		for _, want := range []string{
			"Position check",
			"No separate position estimate is available",
		} {
			if !strings.Contains(flat, want) {
				t.Errorf("fixed-position report lacks %q:\n%s", want, out)
			}
		}
	})

	t.Run("random reports expectation rather than cycles", func(t *testing.T) {
		var b bytes.Buffer
		reportMeasurement(&b,
			measurementFromRanks(lateB, 30, 3, tth2.ArrangeRandom),
			boundedView, fullRunReferences(names))
		out := b.String()
		if !strings.Contains(out,
			"30/30 trials were rank-complete and retained") {
			t.Errorf("random policy changed trial accounting:\n%s", out)
		}
		if strings.Contains(out, "Cycles:") {
			t.Errorf("random policy has rotation cycles:\n%s", out)
		}
		if flat := strings.Join(strings.Fields(out), " "); !strings.Contains(flat,
			"Random assignment: request-position counts ranged 0-30 (expected 10.00)") {
			t.Errorf("random policy omits empirical assignment imbalance:\n%s", out)
		}
	})

	t.Run("unattempted work uses the same accounting", func(t *testing.T) {
		var b bytes.Buffer
		reportMeasurement(&b, measurementFromRanks(
			lateB[:2], 5, 3, tth2.ArrangeRandom),
			boundedView, fullRunReferences(names))
		out := b.String()
		for _, want := range []string{
			"2 attempted; 2 rank-complete; 0 incomplete; 2 retained",
			"3 unattempted",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("partial execution lacks %q:\n%s", want, out)
			}
		}
	})

	t.Run("position table is complete", func(t *testing.T) {
		var b bytes.Buffer
		reportMeasurement(&b,
			measurementFromRanks(lateB, 30, 3, tth2.ArrangeRotate),
			allRowsView, fullRunReferences(names))
		out := b.String()
		for _, want := range []string{
			"position  observations  mean rank  normalised mean rank",
			"       0            30", "spread: 1.50 (normalised 0.75)",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("position check missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("one retained cycle states observation limit", func(t *testing.T) {
		var b bytes.Buffer
		reportMeasurement(&b, measurementFromRanks(
			[][]int{{0, 1}, {1, 0}}, 2, 2, tth2.ArrangeRotate),
			boundedView, fullRunReferences([]string{"A", "B"}))
		if out := b.String(); !strings.Contains(out,
			"only 1 complete rotation cycle was retained") ||
			!strings.Contains(out,
				"cycle-to-cycle variation cannot be observed") {
			t.Errorf("one-cycle report lacks structural caution:\n%s", out)
		}
	})

	t.Run("balanced remains descriptive", func(t *testing.T) {
		var balanced [][]int
		for k := range 20 {
			if k%2 == 0 {
				balanced = append(balanced, []int{0, 1})
			} else {
				balanced = append(balanced, []int{1, 0})
			}
		}
		var b bytes.Buffer
		reportMeasurement(&b,
			measurementFromRanks(balanced, 20, 2, tth2.ArrangeRotate),
			boundedView,
			fullRunReferences([]string{"A", "B"}))
		out := b.String()
		if !strings.Contains(out, "Descriptive result only") {
			t.Errorf("balanced field should remain descriptive:\n%s", out)
		}
	})

	t.Run("equal means expose different distributions", func(t *testing.T) {
		orders := [][]int{{0, 1, 2}, {2, 1, 0}}
		var b bytes.Buffer
		reportMeasurement(&b,
			measurementFromRanks(orders, 3, 3, tth2.ArrangeRandom),
			boundedView, fullRunReferences(names))
		out := b.String()
		for _, want := range []string{
			"A        [+++...++]",
			"A        [ 1  0  1 ]",
			"B        [ 0  2  0 ]",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("missing distribution detail %q:\n%s", want, out)
			}
		}
	})

	t.Run("no complete trials attributes resets", func(t *testing.T) {
		measured := newMeasurement(5, 6, 3, 1, tth2.ArrangeRotate)
		for shift := range 3 {
			mustObserveMeasurement(measured, measurementTrial(
				0, shift, 0, errors.New("reset")))
		}
		measured.finish(&tth2.TrialSummary{
			RankCounts:         newRankMatrix(3),
			PositionRankCounts: newRankMatrix(3),
			FailedTrials:       3,
			ResetCounts:        []int{0, 3, 0},
		}, nil)
		var b bytes.Buffer
		reportMeasurement(&b, measured, boundedView, fullRunReferences(names))
		out := b.String()
		for _, want := range []string{
			"Measurement unavailable",
			"Reset trials by request:",
			"B  3",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("want %q:\n%s", want, out)
			}
		}
	})

	t.Run("failed trials are noted", func(t *testing.T) {
		measured := measurementFromRanks(lateB, 33, 3, tth2.ArrangeRotate)
		measured.summary.FailedTrials = 3
		measured.attempted += 3
		measured.incomplete += 3
		measured.cycleTally.attempted++
		measured.cycleTally.incomplete++
		var b bytes.Buffer
		reportMeasurement(&b, measured, boundedView, fullRunReferences(names))
		out := b.String()
		for _, want := range []string{
			"33 attempted; 30 rank-complete; 3 incomplete; 30 retained",
			"Cycles: 10/11 retained; 1 incomplete",
			"3 incomplete trials did not contribute ranks",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("execution section should report %q:\n%s", want, out)
			}
		}
	})
}

func TestReportMeasureHTTPStatusOutcomes(t *testing.T) {
	t.Run("uniform 2xx compacts", func(t *testing.T) {
		measured := measurementFromStatuses([][]int{{200, 200}, {200, 200}})
		var b bytes.Buffer
		reportMeasureHTTPStatusOutcomes(&b, measured, []string{"A", "B"})
		out := b.String()
		if !strings.Contains(out,
			"Ranked responses: HTTP 200 in all 2 ranked trials for every request.") ||
			strings.Contains(out, "Review:") || strings.Contains(out, "A  200") {
			t.Errorf("uniform outcomes =\n%s", out)
		}
	})

	t.Run("distinct 2xx expands", func(t *testing.T) {
		measured := measurementFromStatuses([][]int{{201, 200, 204}})
		var b bytes.Buffer
		reportMeasureHTTPStatusOutcomes(&b, measured, []string{"create", "check", "delete"})
		out := b.String()
		for _, want := range []string{
			"Complete ranked trials only; warmup excluded.",
			"create  201 x 1", "check   200 x 1", "delete  204 x 1",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("distinct outcomes lack %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "Review:") {
			t.Errorf("distinct 2xx outcomes received a review:\n%s", out)
		}
	})

	t.Run("changing non-2xx reviews", func(t *testing.T) {
		measured := measurementFromStatuses([][]int{
			{200, 500}, {200, 429}, {200, 200},
		})
		measured.incomplete = 1
		var b bytes.Buffer
		reportMeasureHTTPStatusOutcomes(&b, measured, []string{"control", "target"})
		out := b.String()
		flat := strings.Join(strings.Fields(out), " ")
		for _, want := range []string{
			"target   200 x 1, 429 x 1, 500 x 1",
			"HTTP status changed across ranked trials for 1 request: target.",
			"Its aggregate rank combines different response outcomes.",
			"non-2xx responses contributed to the ranking for 1 request: target.",
			"HTTP responses receive ranks regardless of status.",
			"1 incomplete trial excluded from these counts and the ranking.",
			"Partial responses from that trial are not summarised here",
		} {
			if !strings.Contains(flat, strings.Join(strings.Fields(want), " ")) {
				t.Errorf("review outcomes lack %q:\n%s", want, out)
			}
		}
	})

	t.Run("uniform non-2xx stays prominent", func(t *testing.T) {
		measured := measurementFromStatuses([][]int{{401, 401}})
		var b bytes.Buffer
		reportMeasureHTTPStatusOutcomes(&b, measured, []string{"A", "B"})
		out := b.String()
		if !strings.Contains(out, "Ranked responses: HTTP 401") ||
			!strings.Contains(out,
				"Review: all ranked responses had a non-2xx HTTP status.") ||
			!strings.Contains(out,
				"HTTP responses receive ranks regardless of status.") {
			t.Errorf("uniform non-2xx outcomes =\n%s", out)
		}
	})

	t.Run("wide batch does not elide an anomaly", func(t *testing.T) {
		statuses := make([]int, 12)
		labels := make([]string, 12)
		for i := range statuses {
			statuses[i] = 200
			labels[i] = fmt.Sprintf("request-%02d", i)
		}
		statuses[11] = 404
		measured := measurementFromStatuses([][]int{statuses})
		var b bytes.Buffer
		reportMeasureHTTPStatusOutcomes(&b, measured, labels)
		out := b.String()
		for _, label := range labels {
			if !strings.Contains(out, label) {
				t.Errorf("wide outcomes elided %q:\n%s", label, out)
			}
		}
		if !strings.Contains(out, "request-11  404 x 1") {
			t.Errorf("wide outcomes hide anomaly:\n%s", out)
		}
	})
}

func measurementFromStatuses(statuses [][]int) *measurement {
	width := len(statuses[0])
	measured := newMeasurement(
		len(statuses), len(statuses), width, 1, tth2.ArrangeRandom)
	orders := make([][]int, len(statuses))
	for trialIndex, trialStatuses := range statuses {
		trial := completeMeasurementTrial(width, -1, -1, 0)
		trial.Index = trialIndex
		orders[trialIndex] = make([]int, width)
		for request, status := range trialStatuses {
			trial.Results[request].Response.StatusCode = status
			orders[trialIndex][request] = trial.Results[request].ArrivalRank
		}
		if err := measured.observe(trial); err != nil {
			panic(err)
		}
	}
	measured.finish(summaryFromRanks(orders), nil)
	if err := measured.reconcile(); err != nil {
		panic(err)
	}
	return measured
}

func TestReportDetect(t *testing.T) {
	names := []string{"A", "B", "C"}
	items := []ranking.ItemStat{
		{Samples: 10, MeanRank: 0.5},
		{Samples: 40, MeanRank: 1.8},
		{Samples: 10, MeanRank: 0.7},
	}

	t.Run("late outlier found", func(t *testing.T) {
		res := ranking.OutlierResult{
			Outcome: ranking.OutlierFound, Index: 1,
			OutlierGuarantee: ranking.OutlierGuarantee{
				ErrorDirection: ranking.OutlierErrorFalseFind, ErrorRiskLimit: 0.01,
				Direction: ranking.Late, Premise: ranking.OutlierPremiseBaselineMean,
				Regime: ranking.OutlierInferenceAnytimeValid,
			},
			Cost: ranking.Cost{Comparisons: 1234, Participations: 60}, Items: items,
		}
		var b bytes.Buffer
		reportDetect(&b, res, 3, boundedView, fullRunReferences(names))
		out := b.String()
		assertTextLinesAtMost(t, out, textWidth)
		if !strings.Contains(out,
			"Finding: Evidence identifies request B as the late arrival-order outlier") {
			t.Errorf("missing late-outlier headline:\n%s", out)
		}
		if !strings.Contains(out, "false-positive risk at most 1.0%") {
			t.Errorf("missing false-positive risk:\n%s", out)
		}
		if !strings.Contains(out, "Inference: Anytime-valid;") {
			t.Errorf("missing inference regime:\n%s", out)
		}
		if !strings.Contains(out,
			"Premise: Each non-outlier's conditional mean normalised rank against") {
			t.Errorf("missing baseline-control premise:\n%s", out)
		}
		if row := markedRow(t, out); strings.Fields(row)[0] != "B" {
			t.Errorf("selected marker not on B's row: %q", row)
		}
		if !strings.Contains(out,
			"Comparisons: 1234 attempted; 1234 rank-complete; 0 incomplete;") ||
			!strings.Contains(out,
				"Measured request operations: 60 attempted; 60 rank-complete; 60 retained.") {
			t.Errorf("missing decision total:\n%s", out)
		}
	})

	t.Run("early outlier found", func(t *testing.T) {
		res := ranking.OutlierResult{
			Outcome: ranking.OutlierFound, Index: 0,
			OutlierGuarantee: ranking.OutlierGuarantee{
				ErrorDirection: ranking.OutlierErrorFalseFind, ErrorRiskLimit: 0.05,
				Direction: ranking.Early, Premise: ranking.OutlierPremiseBaselineMean,
				Regime: ranking.OutlierInferenceAnytimeValid,
			},
			Cost: ranking.Cost{Comparisons: 42, Participations: 60}, Items: items,
		}
		var b bytes.Buffer
		reportDetect(&b, res, 3, boundedView, fullRunReferences(names))
		out := b.String()
		if !strings.Contains(out,
			"Finding: Evidence identifies request A as the early arrival-order outlier") {
			t.Errorf("missing early-outlier headline:\n%s", out)
		}
		if !strings.Contains(out, "halfway toward the early extreme") {
			t.Errorf("premise does not follow the searched direction:\n%s", out)
		}
		if row := markedRow(t, out); strings.Fields(row)[0] != "A" {
			t.Errorf("selected marker not on A's row: %q", row)
		}
	})

	t.Run("negative decision", func(t *testing.T) {
		res := ranking.OutlierResult{
			Outcome: ranking.OutlierNegative,
			OutlierGuarantee: ranking.OutlierGuarantee{
				ErrorDirection: ranking.OutlierErrorFalseNegative, ErrorRiskLimit: 0.05,
				MinimumEdge: 0.1, Direction: ranking.Late,
				Premise: ranking.OutlierPremiseBaselineMean,
				Regime:  ranking.OutlierInferenceAnytimeValid,
			},
			Cost: ranking.Cost{Comparisons: 500, Participations: 60}, Items: items,
		}
		var b bytes.Buffer
		reportDetect(&b, res, 3, boundedView, fullRunReferences(names))
		out := b.String()
		assertTextLinesAtMost(t, out, textWidth)
		if !strings.Contains(out,
			"Finding: No qualifying late arrival-order outlier detected") ||
			!strings.Contains(out, "Negative scope: normalised-rank edge at least 0.1.") ||
			!strings.Contains(out, "excluded negative edge 0.1.") ||
			!strings.Contains(out, "premise code \"Baseline conditional mean\"") {
			t.Errorf("missing negative headline:\n%s", out)
		}
		if !strings.Contains(out,
			"Target premise: Against those controls a qualifying outlier's") ||
			!strings.Contains(out, "exceeds halfway by at least 0.1.") {
			t.Errorf("missing negative target premise:\n%s", out)
		}
		if strings.Contains(out, "<- ") {
			t.Errorf("a negative decision marks no outlier:\n%s", out)
		}
		if !strings.Contains(out,
			"Comparisons: 500 attempted; 500 rank-complete; 0 incomplete;") {
			t.Errorf("a controlled negative is a decision, not a stop:\n%s", out)
		}
	})

	inconclusive := ranking.OutlierResult{
		Outcome: ranking.OutlierInconclusive,
		Cost:    ranking.Cost{Comparisons: 300, Participations: 60}, Items: items,
	}

	t.Run("inconclusive", func(t *testing.T) {
		var b bytes.Buffer
		reportDetect(&b, inconclusive, 3, boundedView, fullRunReferences(names))
		out := b.String()
		assertTextLinesAtMost(t, out, textWidth)
		if !strings.Contains(out,
			"Finding: Inconclusive; no decision before the 300-comparison limit") {
			t.Errorf("missing concise inconclusive headline:\n%s", out)
		}
		if !strings.Contains(out,
			"Comparisons: 300 attempted; 300 rank-complete; 0 incomplete;") {
			t.Errorf("an inconclusive run stops, it does not decide:\n%s", out)
		}
	})
}

func TestReportDetectExperiment(t *testing.T) {
	t.Parallel()
	base := detectExperimentPlan{
		Candidates: 2, WidthRequested: defaultWidth, WidthEffective: 2,
		StrategyRequested: detectStrategyAuto,
		StrategyResolved:  detectStrategyPeerFirst,
		SolverMethod:      (&ranking.PeerFirstOutlierSolver{}).MethodID(),
		FalsePositiveRisk: defaultFalsePositiveRisk,
		FalseNegativeRisk: defaultFalseNegativeRisk,
		Direction:         ranking.Late, ComparisonsMax: 100,
		ParticipationLimit:      200,
		MinimumEdge:             ranking.OutlierDefaultMinimumEdge,
		InFlightComparisonLimit: 1,
		EvidenceUpdatePolicy:    "complete-atomic-wave",
		DependencePremise:       "arbitrary-within-atomic-wave",
		ResponseLimits:          defaultResponseLimits(),
		Delivery: deliveryPlan{
			connectionLimit: 1, connectionCeiling: 1,
			connectionSelection: connectionDefault,
		},
		BodyRelease: bodyReleasePlan{
			bodyBytesWithheld: 1, hasBodyBytes: true,
		},
	}
	var output bytes.Buffer
	reportDetectExperiment(&output, base)
	defaultReport := output.String()
	for _, want := range []string{
		"Search: late arrival-order outlier among 2 candidates; effective width 2.",
		"Strategy: requested auto; resolved peer-first; solver method",
		"Scheduling: up to 1 in-flight comparison.",
		"Dependence: arbitrary within an atomic wave.",
		"Baselines: none.",
		"Risk bounds: false-positive 1.0%; false-negative 5.0%.",
		"Negative boundary: minimum normalised-rank edge 0.02.",
		"Budget: up to 100 adaptive comparisons (200 measured request participations).",
		"Priming: none; early comparisons per connection may include cold-state effects.",
		"Pacing: batch-rate ceiling unlimited; request-rate ceiling unlimited;",
		"Streams: honour the peer's advertised concurrent-stream limit.",
		"Body release: withhold up to 1 trailing byte per body; no release delay.",
	} {
		if !strings.Contains(defaultReport, want) {
			t.Errorf("default experiment lacks %q:\n%s", want, defaultReport)
		}
	}
	if strings.Contains(defaultReport, "--width") ||
		strings.Contains(defaultReport, "TLS:") {
		t.Errorf("default experiment exposes inert settings:\n%s", defaultReport)
	}

	constrained := base
	constrained.Candidates = 20
	constrained.WidthRequested = 4
	constrained.WidthEffective = 4
	constrained.WidthExplicit = true
	constrained.ComparisonsMax = -1
	constrained.Insecure = true
	constrained.Delivery.peerStreamLimitIgnored = true
	constrained.BaselineTemplates = 2
	constrained.BaselineSupply = finiteBaselineSupply(9)
	constrained.BaselineEffectiveCap = 3
	output.Reset()
	reportDetectExperiment(&output, constrained)
	report := output.String()
	flat := strings.Join(strings.Fields(report), " ")
	for _, want := range []string{
		"effective width 4; requested '--width' 4.",
		"2 templates; configured aggregate supply 9; effective cap 3 at width 4.",
		"Budget: unbounded adaptive comparisons.",
		"Streams: intentionally ignore the peer's advertised concurrent-stream",
		"TLS: certificate verification disabled (--insecure).",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("non-default experiment lacks %q:\n%s", want, report)
		}
	}
	assertTextLinesAtMost(t, report, textWidth)

	rolling := constrained
	rolling.StrategyRequested = detectStrategyRollingBaselineReserved
	rolling.StrategyResolved = detectStrategyRollingBaselineReserved
	rolling.SolverMethod = (&ranking.RollingBaselineReservedOutlierSolver{}).MethodID()
	rolling.InFlightComparisonLimit = 64
	rolling.EvidenceUpdatePolicy = "maximal-contiguous-dispatch-prefix"
	rolling.DependencePremise = "conditional-across-dispatch-indices"
	output.Reset()
	reportDetectExperiment(&output, rolling)
	flat = strings.Join(strings.Fields(output.String()), " ")
	for _, want := range []string{
		"requested rolling-baseline-reserved; resolved rolling-baseline-reserved",
		"up to 64 in-flight comparisons",
		"Evidence updates: maximal contiguous dispatch prefix",
		"Dependence: conditional across dispatch indices.",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("rolling experiment lacks %q:\n%s", want, &output)
		}
	}
	assertTextLinesAtMost(t, output.String(), textWidth)
}

func TestDetectPrimingReportDistinguishesAttemptsFromCompletion(t *testing.T) {
	t.Parallel()
	plan := detectExperimentPlan{
		Candidates: 2, WidthRequested: 2, WidthEffective: 2,
		FalsePositiveRisk: 0.05, FalseNegativeRisk: 0.05,
		Direction: ranking.Late, ComparisonsMax: 10, MinimumEdge: 0.02,
		ParticipationLimit:        20,
		ResponseLimits:            defaultResponseLimits(),
		WarmupTrialsPerConnection: 2, WarmupBatchWidth: 1,
		ConnectionLimit:         1,
		InFlightComparisonLimit: 1,
		EvidenceUpdatePolicy:    "complete-atomic-wave",
		DependencePremise:       "arbitrary-within-atomic-wave",
		Delivery: deliveryPlan{
			connectionLimit: 1, connectionCeiling: 1,
			connectionSelection: connectionDefault,
		},
	}
	var output bytes.Buffer
	reportDetectExperiment(&output, plan)
	if !strings.Contains(output.String(),
		"2 distinct-request attempts per connection") {
		t.Fatalf("planned priming does not promise attempts:\n%s", &output)
	}

	output.Reset()
	reportDetectExecution(&output, detectRunResult{
		Execution: detectExecutionEvidence{
			Recorded: true, ConnectionsObserved: 1,
			PrimingAttempts: []detectPrimingAttempt{
				{
					Worker: 0, Attempt: 1, Connection: 1,
					RequestOperations: 1, Complete: true,
				},
				{
					Worker: 0, Attempt: 2, Connection: 1,
					RequestOperations: 1, Err: errors.New("stream reset"),
				},
			},
		},
	})
	flat := strings.Join(strings.Fields(output.String()), " ")
	for _, want := range []string{
		"Priming batches: 2 attempted; 1 rank-complete; 1 incomplete.",
		"Priming request operations: 2 attempted; 1 rank-complete; excluded from inference.",
		"incomplete priming may leave cold-state effects in adaptive comparisons",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("priming execution lacks %q:\n%s", want, &output)
		}
	}
	assertTextLinesAtMost(t, output.String(), textWidth)
}

// TestOutputIsASCII enforces the portability rule on the module's biggest
// terminal-output producer: every rendered report must be pure ASCII.
func TestOutputIsASCII(t *testing.T) {
	names := []string{"A", "B", "C"}
	var b bytes.Buffer
	writeRaceForTest(t, &b)
	reportMeasurement(&b, measurementFromRanks(
		[][]int{{0, 2, 1}, {1, 2, 0}}, 3, 3, tth2.ArrangeRotate),
		allRowsView, fullRunReferences(names))
	reportDetect(&b, ranking.OutlierResult{
		Outcome: ranking.OutlierFound, Index: 1,
		OutlierGuarantee: ranking.OutlierGuarantee{
			ErrorDirection: ranking.OutlierErrorFalseFind, ErrorRiskLimit: 0.01,
			Direction: ranking.Late, Premise: ranking.OutlierPremiseBaselineMean,
			Regime: ranking.OutlierInferenceAnytimeValid,
		},
		Cost:  ranking.Cost{Comparisons: 10, Participations: 15},
		Items: []ranking.ItemStat{{Samples: 5, MeanRank: 0.5}, {Samples: 5, MeanRank: 1.8}, {Samples: 5, MeanRank: 0.7}},
	}, 3, boundedView, fullRunReferences(names))

	// The help text is held to the same bar as the reports: it is the first thing
	// most users see, and a non-ASCII byte in it is mojibake on a console whose
	// codepage is not UTF-8.
	b.WriteString(topLevelUsage)
	for _, command := range []string{"race", "measure", "analyse", "detect"} {
		b.WriteString(helpText(t, command))
	}

	for i, r := range b.Bytes() {
		if r > 0x7F {
			t.Fatalf("non-ASCII byte 0x%02x at offset %d in rendered output", r, i)
		}
	}
}
