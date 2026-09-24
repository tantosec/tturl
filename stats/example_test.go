package stats_test

import (
	"fmt"
	"math"

	"github.com/tantosec/tturl/stats"
)

func ExampleWelford() {
	var estimate stats.Welford
	fmt.Println(math.IsNaN(estimate.Mean()))

	for _, observation := range []float64{4, 7, 13} {
		estimate.Observe(observation)
	}
	fmt.Printf("n=%d mean=%.0f variance=%.0f\n",
		estimate.Count(), estimate.Mean(), estimate.Variance())

	// Output:
	// true
	// n=3 mean=8 variance=21
}

func ExampleQuantile() {
	sample := []float64{9, 1, 5}
	fmt.Println(stats.Quantile(sample, 0.5))
	fmt.Println(sample)

	// Output:
	// 5
	// [9 1 5]
}

func ExampleBernoulliCS() {
	sequence := stats.NewBernoulliCS(0.05)
	lo, hi := sequence.Interval()
	fmt.Println(math.IsNaN(lo), math.IsNaN(hi))
	for i := range 100 {
		sequence.Observe(i%2 == 0)
	}
	lo, hi = sequence.Interval()
	fmt.Printf("n=%d mean=%.1f interval=[%.2f, %.2f]\n",
		sequence.Count(), sequence.Mean(), lo, hi)

	// Output:
	// true true
	// n=100 mean=0.5 interval=[0.33, 0.67]
}
