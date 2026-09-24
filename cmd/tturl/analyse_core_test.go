package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
)

func TestAnalyseOfflineExactWidthTwo(t *testing.T) {
	evidence := analyseOrderedEvidence(2, 6, func(int) []int {
		return []int{0, 1}
	})
	result := analyseOffline(evidence, analyseConfig{RandomisationSeed: 7})

	if !result.Validity.Available || result.Validity.Code != analyseValid {
		t.Fatalf("validity = %+v", result.Validity)
	}
	if result.Global.Outcome != analyseDifference ||
		result.Global.Calibration.Method != "exact_randomisation" ||
		result.Global.Calibration.Draws != 64 ||
		result.Global.Calibration.Extremes != 2 ||
		result.Global.Calibration.PValue != 0.03125 {
		t.Fatalf("global = %+v", result.Global)
	}
	if math.Abs(result.Global.Statistic-1) > 1e-15 ||
		math.Abs(result.Global.Components.L2-1) > 1e-15 ||
		math.Abs(result.Global.Components.LInfinity-1) > 1e-15 {
		t.Fatalf("global statistic/components = %+v", result.Global)
	}
	if len(result.Requests) != 2 || result.Requests[0].MeanRank != 0 ||
		result.Requests[0].NormalisedMeanRank != 0 || result.Requests[1].MeanRank != 1 ||
		result.Requests[1].NormalisedMeanRank != 1 ||
		!reflect.DeepEqual(result.Requests[0].RankCounts, []int{12, 0}) ||
		!reflect.DeepEqual(result.Requests[1].RankCounts, []int{0, 12}) {
		t.Fatalf("request effects = %+v", result.Requests)
	}
	if len(result.Positions) != 2 || result.Positions[0].Samples != 12 ||
		result.Positions[0].MeanRank != 0.5 ||
		result.Positions[1].MeanRank != 0.5 {
		t.Fatalf("position effects = %+v", result.Positions)
	}
	if result.Pairs.State != analysePairsPerformed || len(result.Pairs.Pairs) != 1 {
		t.Fatalf("pairs = %+v", result.Pairs)
	}
	pair := result.Pairs.Pairs[0]
	if pair.Left != 0 || pair.Right != 1 || pair.Trials != 12 ||
		pair.LeftBeforeRight != 12 || pair.RightBeforeLeft != 0 ||
		pair.MeanRankDifference != -1 ||
		pair.RawP != 0.03125 || pair.AdjustedP != 0.03125 || !pair.Selected {
		t.Fatalf("pair = %+v", pair)
	}
}

func TestAnalyseOfflineNoDetectionDoesNotLocalise(t *testing.T) {
	evidence := analyseOrderedEvidence(2, 6, func(cycle int) []int {
		if cycle%2 == 0 {
			return []int{0, 1}
		}
		return []int{1, 0}
	})
	result := analyseOffline(evidence, analyseConfig{RandomisationSeed: 9})

	if result.Global.Outcome != analyseNoDetection ||
		result.Global.Calibration.PValue != 1 {
		t.Fatalf("global = %+v", result.Global)
	}
	if result.Pairs.State != analysePairsNotTested || result.Pairs.Pairs != nil {
		t.Fatalf("pairs = %+v", result.Pairs)
	}
}

func TestAnalyseOfflineDescribesOutboundPositionRanks(t *testing.T) {
	evidence := analyseOrderedEvidence(2, 6, func(int) []int {
		return []int{0, 1}
	})
	for cycle := range evidence.Cycles {
		for trial := range evidence.Cycles[cycle].Trials {
			entry := &evidence.Cycles[cycle].Trials[trial]
			for position, request := range entry.Arrangement {
				entry.Ranks[request] = position
			}
		}
	}
	result := analyseOffline(evidence, analyseConfig{})
	if len(result.Positions) != 2 || result.Positions[0].MeanRank != 0 ||
		result.Positions[1].MeanRank != 1 {
		t.Fatalf("position effects = %+v", result.Positions)
	}
}

func TestAnalyseOfflineMonteCarloIsReproducible(t *testing.T) {
	evidence := analyseOrderedEvidence(3, 7, func(int) []int {
		return []int{0, 1, 2}
	})
	config := analyseConfig{RandomisationSeed: 0x414e414c595345}
	first := analyseOffline(evidence, config)
	second := analyseOffline(evidence, config)

	if !equalAnalyseResults(t, first, second) {
		t.Fatalf("same evidence and precommitted seed gave different results")
	}
	if first.Global.Calibration.Method != "monte_carlo_randomisation" ||
		first.Global.Outcome != analyseDifference ||
		first.Global.Calibration.PValueKind != "monte_carlo" ||
		first.Global.Calibration.Draws != analyseMonteCarloDraws ||
		first.Global.Calibration.PValue != 0.0001 ||
		first.Global.Calibration.CompletionReason !=
			"fixed_draw_budget_complete" {
		t.Fatalf("global = %+v", first.Global)
	}
}

func TestAnalyseUniformBelowRejectsIncompleteRange(t *testing.T) {
	source := &analyseScriptedUint64Source{values: []uint64{0, 4}}
	if got := analyseUniformBelow(source, 3); got != 1 {
		t.Fatalf("uniform value = %d, want 1", got)
	}
	if source.index != 2 {
		t.Fatalf("source reads = %d, want 2", source.index)
	}
}

func TestAnalyseUniformBelowBoundaries(t *testing.T) {
	tests := []struct {
		name   string
		values []uint64
		bound  uint64
		want   uint64
	}{
		{name: "unit", values: []uint64{math.MaxUint64}, bound: 1, want: 0},
		{
			name: "power of two", values: []uint64{math.MaxUint64},
			bound: 1 << 63, want: 1<<63 - 1,
		},
		{
			name: "maximum rejects zero", values: []uint64{0, math.MaxUint64 - 1},
			bound: math.MaxUint64, want: math.MaxUint64 - 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &analyseScriptedUint64Source{values: test.values}
			if got := analyseUniformBelow(source, test.bound); got != test.want {
				t.Fatalf("uniform value = %d, want %d", got, test.want)
			}
			if source.index != len(test.values) {
				t.Fatalf("source reads = %d, want %d", source.index, len(test.values))
			}
		})
	}
}

func TestAnalyseRandomiserShufflePreservesPermutation(t *testing.T) {
	values := []int{0, 1, 2, 3, 4, 5, 6, 7}
	rng := analyseRandomiser{state: 0x5045524d555445}
	rng.shuffle(values)

	seen := make([]bool, len(values))
	for _, value := range values {
		if value < 0 || value >= len(values) || seen[value] {
			t.Fatalf("shuffled values are not a permutation: %v", values)
		}
		seen[value] = true
	}
}

type analyseScriptedUint64Source struct {
	values []uint64
	index  int
}

func (source *analyseScriptedUint64Source) next() uint64 {
	value := source.values[source.index]
	source.index++
	return value
}

func TestAnalyseMonteCarloPUsesObservedAssignment(t *testing.T) {
	tests := []struct {
		extremes int
		want     float64
	}{
		{extremes: 0, want: 0.0001},
		{extremes: 499, want: 0.05},
		{extremes: 500, want: 0.0501},
		{extremes: 9_999, want: 1},
	}
	for _, test := range tests {
		if got := analyseMonteCarloP(test.extremes, 9_999); got != test.want {
			t.Errorf("%d extremes: p = %g, want %g",
				test.extremes, got, test.want)
		}
	}
}

func TestAnalyseOfflineUnavailable(t *testing.T) {
	evidence := analyseOrderedEvidence(3, 6, func(int) []int {
		return []int{0, 1, 2}
	})
	result := analyseOffline(evidence, analyseConfig{})

	if result.Validity.Available || result.Validity.Code != analyseTooFewCycles ||
		result.Global.Outcome != analyseUnavailable ||
		result.Pairs.State != analysePairsUnavailable {
		t.Fatalf("result = %+v", result)
	}
}

func TestAnalyseOfflineRejectsMalformedEvidenceWithoutPanic(t *testing.T) {
	tests := []struct {
		name string
		edit func(*analyseTrialEvidence)
		code analyseValidityCode
	}{
		{
			name: "short arrangement",
			edit: func(trial *analyseTrialEvidence) {
				trial.Arrangement = trial.Arrangement[:1]
			},
			code: analyseInvalidArrangement,
		},
		{
			name: "extra rank",
			edit: func(trial *analyseTrialEvidence) {
				trial.Ranks = append(trial.Ranks, 0)
			},
			code: analyseInvalidRanks,
		},
		{
			name: "short statuses",
			edit: func(trial *analyseTrialEvidence) {
				trial.Statuses = trial.Statuses[:1]
			},
			code: analyseInvalidStatus,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := analyseOrderedEvidence(
				2, 6, func(int) []int { return []int{0, 1} },
			)
			test.edit(&evidence.Cycles[0].Trials[0])
			result := analyseOffline(evidence, analyseConfig{})
			if result.Validity.Available || result.Validity.Code != test.code {
				t.Fatalf("validity = %+v, want %s", result.Validity, test.code)
			}
		})
	}
}

func TestAnalyseEvidenceAllowsWholeCyclesAcrossConnections(t *testing.T) {
	evidence := analyseOrderedEvidence(
		2, 6, func(int) []int { return []int{0, 1} },
	)
	want := analyseOffline(evidence, analyseConfig{RandomisationSeed: 7})
	for cycle := range evidence.Cycles {
		for trial := range evidence.Cycles[cycle].Trials {
			evidence.Cycles[cycle].Trials[trial].Connection = cycle % 2
		}
	}
	got := analyseOffline(evidence, analyseConfig{
		RandomisationSeed: 7, ConnectionLimit: 2,
	})
	if !got.Validity.Available || got.Execution.Connections != 2 ||
		got.ConnectionLimit != 2 {
		t.Fatalf("parallel evidence = %+v", got)
	}
	if got.Global != want.Global || !reflect.DeepEqual(got.Pairs, want.Pairs) ||
		!reflect.DeepEqual(got.Requests, want.Requests) {
		t.Fatalf("connection allocation changed inference\ngot: %+v\nwant: %+v",
			got, want)
	}
}

func TestValidateAnalyseEvidenceRejectsDamagedDesign(t *testing.T) {
	tests := []struct {
		name string
		edit func(*analyseEvidence)
		code analyseValidityCode
	}{
		{
			name: "incomplete cycle",
			edit: func(evidence *analyseEvidence) {
				evidence.Cycles[0].Trials = evidence.Cycles[0].Trials[:1]
			},
			code: analyseIncompleteCycle,
		},
		{
			name: "duplicate shift",
			edit: func(evidence *analyseEvidence) {
				evidence.Cycles[0].Trials[1].Shift = 0
			},
			code: analyseInvalidShift,
		},
		{
			name: "non-rotation arrangement",
			edit: func(evidence *analyseEvidence) {
				evidence.Cycles[0].Trials[1].Arrangement = []int{0, 1}
			},
			code: analyseInvalidArrangement,
		},
		{
			name: "duplicate rank",
			edit: func(evidence *analyseEvidence) {
				evidence.Cycles[0].Trials[0].Ranks = []int{0, 0}
			},
			code: analyseInvalidRanks,
		},
		{
			name: "invalid response status",
			edit: func(evidence *analyseEvidence) {
				evidence.Cycles[0].Trials[0].Statuses[0] = 0
			},
			code: analyseInvalidStatus,
		},
		{
			name: "connection changes within cycle",
			edit: func(evidence *analyseEvidence) {
				evidence.Cycles[1].Trials[0].Connection = 1
			},
			code: analyseMixedConnections,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := analyseOrderedEvidence(2, 6, func(int) []int {
				return []int{0, 1}
			})
			test.edit(&evidence)
			result := analyseOffline(evidence, analyseConfig{})
			if result.Validity.Available || result.Validity.Code != test.code {
				t.Fatalf("validity = %+v, want %s", result.Validity, test.code)
			}
		})
	}
}

func TestAnalyseHolmAdjustment(t *testing.T) {
	pairs := analyseHolmPairs([]analysePairResult{
		{Left: 0, Right: 1, RawP: 0.01},
		{Left: 0, Right: 2, RawP: 0.03},
		{Left: 1, Right: 2, RawP: 0.02},
	})
	want := []float64{0.03, 0.04, 0.04}
	for index, pair := range pairs {
		if math.Abs(pair.AdjustedP-want[index]) > 1e-15 {
			t.Fatalf("pair %d adjusted p = %v, want %v", index,
				pair.AdjustedP, want[index])
		}
	}
}

func TestAnalyseHolmAdjustmentPreservesUpperBounds(t *testing.T) {
	pairs := []analysePairResult{
		{Left: 0, Right: 1, RawP: analysePairPReportingFloor, RawPUpperBound: true},
		{Left: 0, Right: 2, RawP: 0.01},
	}
	adjusted := analyseHolmPairs(pairs)
	if adjusted[0].AdjustedP != 2*analysePairPReportingFloor ||
		!adjusted[0].AdjustedPUpperBound {
		t.Fatalf("bounded adjustment = %+v", adjusted[0])
	}
	if adjusted[1].AdjustedP != 0.01 || adjusted[1].AdjustedPUpperBound {
		t.Fatalf("ordinary adjustment = %+v", adjusted[1])
	}
}

func TestAnalyseExactCycleSignP(t *testing.T) {
	if got, bounded := analyseExactCycleSignP([]int{1, 1, 1}); got != 0.25 || bounded {
		t.Fatalf("three aligned signs p = %v, want 0.25", got)
	}
	if got, bounded := analyseExactCycleSignP([]int{1, 1, -1}); got != 1 || bounded {
		t.Fatalf("one opposing sign p = %v, want 1", got)
	}
}

func TestAnalyseExactCycleSignPAtCycleLimit(t *testing.T) {
	cycles := analyseMaximumOperations / 4
	contributions := make([]int, cycles)
	for index := range contributions {
		contributions[index] = 1
	}
	got, bounded := analyseExactCycleSignP(contributions)
	if got != analysePairPReportingFloor || !bounded {
		t.Fatalf("%d aligned signs p = %g, bounded %t; want <= %g",
			cycles, got, bounded, analysePairPReportingFloor)
	}
}

func TestAnalyseCycleSignUpperBoundCanExceedReportingFloor(t *testing.T) {
	contributions := []int{1000, 1, 1, 1, 1, 1, 1, 1, 1, 1}
	bound := analyseCycleSignUpperBound(contributions, 1009)
	if bound <= analysePairPReportingFloor || bound > 1 {
		t.Fatalf("Hoeffding bound = %g", bound)
	}
}

func TestAnalyseExactCycleSignPMatchesAllocatingRecurrence(t *testing.T) {
	var check func([]int, int)
	check = func(contributions []int, remaining int) {
		if remaining == 0 {
			got, bounded := analyseExactCycleSignP(contributions)
			want := analyseAllocatingCycleSignP(contributions)
			if bounded || got != want {
				t.Fatalf("contributions %v: p = %g, bounded %t; want %g",
					contributions, got, bounded, want)
			}
			return
		}
		for _, contribution := range []int{-2, -1, 0, 1, 2} {
			check(append(contributions, contribution), remaining-1)
		}
	}
	for length := 1; length <= 6; length++ {
		check(nil, length)
	}
}

func analyseAllocatingCycleSignP(contributions []int) float64 {
	rangeLimit := 0
	observed := 0
	for _, contribution := range contributions {
		rangeLimit += analyseAbs(contribution)
		observed += contribution
	}
	offset := rangeLimit
	probabilities := make([]float64, 2*rangeLimit+1)
	probabilities[offset] = 1
	currentRange := 0
	for _, contribution := range contributions {
		next := make([]float64, len(probabilities))
		for value := -currentRange; value <= currentRange; value++ {
			probability := probabilities[offset+value]
			next[offset+value+contribution] += probability / 2
			next[offset+value-contribution] += probability / 2
		}
		probabilities = next
		currentRange += analyseAbs(contribution)
	}
	p := 0.0
	for value := -rangeLimit; value <= rangeLimit; value++ {
		if analyseAbs(value) >= analyseAbs(observed) {
			p += probabilities[offset+value]
		}
	}
	return math.Min(1, p)
}

func analyseOrderedEvidence(
	width, cycles int, ranks func(cycle int) []int,
) analyseEvidence {
	evidence := analyseEvidence{Width: width, Cycles: make([]analyseCycleEvidence, cycles)}
	for cycle := range cycles {
		base := make([]int, width)
		for position := range width {
			base[position] = (position + cycle) % width
		}
		for shift := range width {
			arrangement := make([]int, width)
			for position := range width {
				arrangement[position] = base[(position+shift)%width]
			}
			statuses := make([]int, width)
			for request := range statuses {
				statuses[request] = 200
			}
			evidence.Cycles[cycle].Trials = append(
				evidence.Cycles[cycle].Trials,
				analyseTrialEvidence{
					Connection: 0, Shift: shift, Arrangement: arrangement,
					Ranks: append([]int(nil), ranks(cycle)...), Statuses: statuses,
				},
			)
		}
	}
	return evidence
}

func BenchmarkAnalyseOfflineMaximumWorkload(b *testing.B) {
	for width := 2; width <= 8; width++ {
		b.Run(fmt.Sprintf("width_%d", width), func(b *testing.B) {
			b.ReportAllocs()
			cycles := analyseMaximumOperations / (width * width)
			evidence := analyseOrderedEvidence(width, cycles, func(int) []int {
				ranks := make([]int, width)
				for index := range ranks {
					ranks[index] = index
				}
				return ranks
			})
			b.ResetTimer()
			for b.Loop() {
				result := analyseOffline(
					evidence, analyseConfig{RandomisationSeed: 0x424f554e44},
				)
				if result.Global.Outcome != analyseDifference {
					b.Fatalf("global = %+v", result.Global)
				}
			}
		})
	}
}

func BenchmarkAnalyseGlobalFixedDrawWork(b *testing.B) {
	const width = 8
	cycles := analyseMaximumOperations / (width * width)
	evidence := analyseOrderedEvidence(width, cycles, func(cycle int) []int {
		ranks := make([]int, width)
		for index := range ranks {
			ranks[index] = (index + cycle) % width
		}
		return ranks
	})
	validated, validity := validateAnalyseEvidence(evidence)
	if !validity.Available {
		b.Fatal(validity.Detail)
	}
	workspace := newAnalyseRankWorkspace(width)
	observed, components := workspace.statistic(validated, validated.base)
	b.ResetTimer()
	for b.Loop() {
		result := analyseSampledGlobal(
			validated, observed, components, 0x4d41584452415753,
		)
		if result.Calibration.Draws != analyseMonteCarloDraws {
			b.Fatalf("calibration = %+v", result.Calibration)
		}
	}
}

// equalAnalyseResults compares replayable facts and the optional live cause.
func equalAnalyseResults(t *testing.T, left, right analyseRunResult) bool {
	t.Helper()
	leftJSON, err := json.Marshal(left)
	if err != nil {
		t.Fatal(err)
	}
	rightJSON, err := json.Marshal(right)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(leftJSON, rightJSON) &&
		errors.Is(left.Completion.cause, right.Completion.cause) &&
		errors.Is(left.Evidence.Completion.cause, right.Evidence.Completion.cause)
}
