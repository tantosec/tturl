package main

import (
	"encoding/json"
	"math"
	"testing"
)

func TestTimeSummaryPopulationAndR7Quantiles(t *testing.T) {
	empty := summariseTimeDurations(nil)
	if empty.Count != 0 || empty.Min != nil || empty.Mean != nil || empty.P50 != nil || empty.SD != nil {
		t.Fatal("empty population supplied statistics")
	}
	single := summariseTimeDurations([]int64{17})
	if single.Count != 1 || *single.Min != 17 || *single.Max != 17 || *single.P99 != 17 || *single.SD != 0 {
		t.Fatal("single observation has an invented spread")
	}
	samples := []int64{4, 1, 3, 2}
	summary := summariseTimeDurations(samples)
	for _, pair := range [][2]float64{
		{*summary.Mean, 2.5},
		{*summary.P50, 2.5},
		{*summary.P90, 3.7},
		{*summary.P95, 3.85},
		{*summary.P99, 3.97},
		{*summary.SD, math.Sqrt(1.25)},
	} {
		if math.Abs(pair[0]-pair[1]) > 1e-12 {
			t.Fatalf("population statistic %g, expected %g", pair[0], pair[1])
		}
	}
	if samples[0] != 4 {
		t.Fatal("summary reordered retained samples")
	}
}

func TestTimeSummaryPreservesExtremaAndSmallSpread(t *testing.T) {
	summary := summariseTimeDurations([]int64{math.MaxInt64 - 2, math.MaxInt64, math.MaxInt64 - 1})
	if *summary.Min != math.MaxInt64-2 || *summary.Max != math.MaxInt64 ||
		math.Abs(*summary.SD-math.Sqrt(2.0/3)) > 1e-12 {
		t.Fatal("large observations lost exact extrema or centred spread")
	}
	extreme := summariseTimeDurations([]int64{math.MinInt64, math.MaxInt64})
	if math.IsInf(*extreme.SD, 0) || math.IsNaN(*extreme.SD) || *extreme.SD <= 0 {
		t.Fatal("extreme observations overflowed population arithmetic")
	}
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if string(decoded["max_ns"]) != "9223372036854775807" {
		t.Fatal("JSON rounded an integer maximum")
	}
}
