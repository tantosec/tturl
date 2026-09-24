package stats_test

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/tantosec/tturl/stats"
)

func TestNumericalBoundaries(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	if got := stats.BinomialLogPMF(0, maxInt, 0.5); math.IsNaN(got) || math.IsInf(got, 0) {
		t.Fatalf("maximum trial count yielded %v", got)
	}
	if got := stats.DiscreteUniformVariance(maxInt-1, maxInt); got != 0.25 {
		t.Fatalf("adjacent integer variance = %v, want 0.25", got)
	}
	if !math.IsNaN(stats.DiscreteUniformMean(2, 1)) ||
		!math.IsNaN(stats.DiscreteUniformVariance(2, 1)) {
		t.Fatal("invalid distribution bounds must yield NaN")
	}
	if got := stats.Median([]float64{-math.MaxFloat64, math.MaxFloat64}); got != 0 {
		t.Fatalf("symmetric median = %v, want 0", got)
	}
	for _, cdf := range []func([]float64, float64) float64{stats.ECDF, stats.ECDFSorted} {
		if !math.IsNaN(cdf([]float64{1, 2}, math.NaN())) {
			t.Error("NaN query must yield NaN")
		}
	}
	lo, hi := stats.WilsonInterval(1, 2, math.MaxFloat64)
	if lo != 0 || hi != 1 {
		t.Fatalf("extreme critical value interval = [%v, %v]", lo, hi)
	}
	alpha := math.SmallestNonzeroFloat64
	if got := stats.RankLogLik(2, 0, alpha); math.Abs(got+1074*math.Ln2) > 1e-12 {
		t.Fatalf("subnormal rank log-likelihood = %v", got)
	}
	posterior := stats.NewRankPosterior(2, alpha)
	posterior.ObservePairwise(0, 1)
	if got := posterior.Probabilities()[0]; got != alpha {
		t.Fatalf("subnormal posterior probability = %v, want %v", got, alpha)
	}
	if quantile := stats.NormQuantile(alpha); !(quantile < -38 && quantile > -39) {
		t.Fatalf("subnormal lower-tail normal quantile = %v", quantile)
	}
	if quantile := stats.BetaQuantile(0.5, math.MaxFloat64, math.MaxFloat64); !math.IsNaN(quantile) {
		t.Fatalf("undefined incomplete-beta evaluation yielded %v", quantile)
	}
	if got := stats.VilleUnionThreshold(2, alpha); math.Abs(got-1075*math.Ln2) > 1e-12 {
		t.Fatalf("finite log threshold = %v", got)
	}
	s, f := stats.TwoPointLLRIncrements(alpha, 0.5)
	if math.IsInf(s, 0) || math.IsNaN(s) || math.IsInf(f, 0) || math.IsNaN(f) {
		t.Fatalf("finite log increments = %v, %v", s, f)
	}
	tiny := stats.NewWeightedLLR(alpha, 2*alpha)
	tiny.ObserveWeighted(false, 1)
	if tiny.LogLR() >= 0 {
		t.Fatal("tiny distinct hypotheses lost their negative increment")
	}
	nearby := stats.NewWeightedLLR(0.7, math.Nextafter(0.7, 1))
	nearby.ObserveWeighted(true, 1)
	if nearby.LogLR() <= 0 {
		t.Fatal("distinct nearby hypotheses lost their positive increment")
	}
	if got := stats.GammaCDF(math.MaxFloat64, 1, 2); got != 1 {
		t.Fatalf("overflowing scaled gamma argument = %v, want 1", got)
	}
	rng := rand.New(rand.NewPCG(3, 4)) //nolint:gosec // synthetic statistical samples
	if got := stats.BetaRV(rng, math.MaxFloat64, math.MaxFloat64); got != 0.5 {
		t.Fatalf("large balanced beta sample = %v, want 0.5", got)
	}
	weights := []float64{math.MaxFloat64, math.MaxFloat64}
	got := stats.LogWeightedExpSum([]float64{0, 0}, weights)
	want := math.Log(math.MaxFloat64) + math.Ln2
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("log weighted sum = %v, want %v", got, want)
	}
}

func TestLogWeightedSumValidationPrecedesUndefinedResult(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("invalid later weight must panic even with an earlier NaN")
		}
	}()
	stats.LogWeightedExpSum([]float64{math.NaN(), 0}, []float64{1, -1})
}

func TestBoundedSequenceScaling(t *testing.T) {
	for _, bounds := range [][2]float64{
		{-math.MaxFloat64, math.MaxFloat64},
		{0, math.SmallestNonzeroFloat64},
		{math.MaxFloat64 / 2, math.MaxFloat64},
	} {
		c := stats.NewBoundedMeanCS(bounds[0], bounds[1], 0.05)
		unit := stats.NewBoundedMeanCS(0, 1, 0.05)
		for i := range 1000 {
			upper := i%2 == 0
			x, u := bounds[0], 0.0
			if upper {
				x, u = bounds[1], 1
			}
			c.Observe(x)
			unit.Observe(u)
		}
		lo, hi := c.Interval()
		if math.IsNaN(lo) || math.IsNaN(hi) || lo < bounds[0] || hi > bounds[1] || lo > hi {
			t.Fatalf("bounds %v produced [%v, %v]", bounds, lo, hi)
		}
		if bounds[0] < 0 {
			uLo, uHi := unit.Interval()
			if math.Abs(lo/math.MaxFloat64-(2*uLo-1)) > 1e-14 ||
				math.Abs(hi/math.MaxFloat64-(2*uHi-1)) > 1e-14 || math.Abs(c.Mean()/math.MaxFloat64) > 1e-14 {
				t.Fatalf("normalised construction did not scale: [%v, %v], mean %v", lo, hi, c.Mean())
			}
		}
	}
}

func TestPrimitiveCompositionAndOwnership(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // synthetic statistical samples
	var estimate stats.Welford
	sample := make([]float64, 100)
	for i := range sample {
		sample[i] = stats.NormalRV(rng, 3, 2)
		estimate.Observe(sample[i])
	}
	original := slices.Clone(sample)
	median := stats.Median(sample)
	if !slices.Equal(sample, original) || math.IsNaN(median) ||
		math.IsNaN(stats.NormalTailP(stats.OneSampleZ(estimate.Mean(), 3, 2, 100), stats.TwoSided)) {
		t.Fatal("sample preparation or normal inference failed")
	}
	alpha, beta := stats.BetaBinomialUpdate(1, 1, 3, 5)
	if draw := stats.BetaRV(rng, alpha, beta); !(draw >= 0 && draw <= 1) {
		t.Fatalf("posterior sample = %v", draw)
	}
	posterior := stats.NewRankPosterior(3, 0.7)
	ranking := []int{2, 0, 1}
	posterior.ObserveRanking(ranking)
	ranking[0] = 1
	probabilities := posterior.Probabilities()
	probabilities[2] = 0
	if posterior.MaxPopulation() != 2 || posterior.Probabilities()[2] == 0 {
		t.Fatal("posterior retained or exposed mutable slice storage")
	}
	record := stats.NewPairwiseRecord(3)
	record.Observe(2, 0)
	if record.Wins(2, 0) != 1 || record.Total(0, 2) != 1 {
		t.Fatal("pairwise observation failed")
	}
	sequence := stats.NewBernoulliCS(0.05)
	llr := stats.NewWeightedLLR(0.5, 0.7)
	sprt := stats.NewSPRT(0.5, 0.7, 0.05, 0.1)
	ewma := stats.NewEWMA(0.2)
	var meter stats.BernoulliMeter
	for _, success := range []bool{true, false, true} {
		sequence.Observe(success)
		llr.ObserveWeighted(success, stats.RankComparisonWeight(2))
		sprt.Observe(success)
		meter.Observe(success)
		ewma.Observe(meter.Rate())
	}
	lo, hi := sequence.Interval()
	if !(lo <= meter.Rate() && meter.Rate() <= hi) || llr.Count() != 3 ||
		sprt.Count() != 3 || math.IsNaN(ewma.Variance()) {
		t.Fatal("streaming composition failed")
	}
}
