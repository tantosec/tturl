package main

import (
	"math"
	"slices"
)

type timeDurationSummary struct {
	Count uint64   `json:"count"`
	Min   *int64   `json:"min_ns"`
	P50   *float64 `json:"p50_ns"`
	P90   *float64 `json:"p90_ns"`
	P95   *float64 `json:"p95_ns"`
	P99   *float64 `json:"p99_ns"`
	Max   *int64   `json:"max_ns"`
	Mean  *float64 `json:"mean_ns"`
	SD    *float64 `json:"standard_deviation_ns"`
}

func summariseTimeDurations(samples []int64) timeDurationSummary {
	summary := timeDurationSummary{Count: uint64(len(samples))}
	if len(samples) == 0 {
		return summary
	}
	ordered := slices.Clone(samples)
	slices.Sort(ordered)
	minimum, maximum := ordered[0], ordered[len(ordered)-1]
	summary.Min, summary.Max = &minimum, &maximum
	var mean, squared float64
	for index, value := range ordered {
		centred := timeSampleDifference(value, minimum)
		delta := centred - mean
		mean += delta / float64(index+1)
		squared += delta * (centred - mean)
	}
	populationMean := float64(minimum) + mean
	deviation := math.Sqrt(max(0, squared/float64(len(ordered))))
	summary.Mean, summary.SD = &populationMean, &deviation
	summary.P50 = timeQuantile(ordered, 0.50)
	summary.P90 = timeQuantile(ordered, 0.90)
	summary.P95 = timeQuantile(ordered, 0.95)
	summary.P99 = timeQuantile(ordered, 0.99)
	return summary
}

// Sorted subtraction preserves small differences near int64 extrema.
// Crossing zero splits the subtraction to avoid signed integer overflow.
func timeSampleDifference(high, low int64) float64 {
	if high >= 0 && low < 0 {
		return float64(high) + float64(-(low + 1)) + 1
	}
	return float64(high - low)
}

func timeQuantile(ordered []int64, probability float64) *float64 {
	index := float64(len(ordered)-1) * probability
	lower := min(int(index), len(ordered)-1)
	upper := min(lower+1, len(ordered)-1)
	fraction := index - float64(lower)
	value := float64(ordered[lower]) + fraction*timeSampleDifference(ordered[upper], ordered[lower])
	return &value
}
