package stats_test

import (
	"testing"

	"github.com/tantosec/tturl/stats"
)

func TestInvalidZeroValuesPanic(t *testing.T) {
	tests := []struct {
		name string
		call func()
	}{
		{"EWMA", func() { new(stats.EWMA).Count() }},
		{"PairwiseRecord", func() { new(stats.PairwiseRecord).PopulationCount() }},
		{"RankPosterior", func() { new(stats.RankPosterior).PopulationCount() }},
		{"SPRT", func() { new(stats.SPRT).Decision() }},
		{"WeightedLLR", func() { new(stats.WeightedLLR).LogLR() }},
		{"BernoulliCS", func() { new(stats.BernoulliCS).Mean() }},
		{"BoundedMeanCS", func() { new(stats.BoundedMeanCS).Mean() }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("method on invalid zero value did not panic")
				}
			}()
			test.call()
		})
	}
}

func TestCopyableEstimatorValuesAreIndependent(t *testing.T) {
	var original stats.Welford
	original.Observe(1)
	clone := original

	original.Observe(3)
	clone.Observe(5)
	if original.Mean() != 2 || clone.Mean() != 3 {
		t.Fatalf("copies were not independent: original=%v clone=%v",
			original.Mean(), clone.Mean())
	}
}
