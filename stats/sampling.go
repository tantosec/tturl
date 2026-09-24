package stats

import "math"

// RNG supplies values uniformly distributed in [0, 1). Sampling functions
// require a non-nil source and advance it synchronously without retaining it.
// A source must eventually produce a positive value; rejection sampling may
// make an unbounded number of draws. Concurrent sampling requires a source
// safe for concurrent use. The draw sequence is not a compatibility contract.
type RNG interface {
	Float64() float64
}

// NormalRV returns one sample from N(mean, sd*sd). It panics unless mean is
// finite and sd is finite and non-negative.
func NormalRV(rng RNG, mean, sd float64) float64 {
	if math.IsNaN(mean) || math.IsInf(mean, 0) {
		panic("stats: NormalRV: mean must be finite")
	}
	if math.IsNaN(sd) || math.IsInf(sd, 0) || sd < 0 {
		panic("stats: NormalRV: sd must be finite and non-negative")
	}
	return mean + sd*normalStd(rng)
}

// normalStd samples N(0, 1) with the Box-Muller transform.
func normalStd(rng RNG) float64 {
	u1 := uniformOpen01(rng)
	return math.Sqrt(-2*logFloat(u1)) * math.Cos(2*math.Pi*rng.Float64())
}

// uniformOpen01 rejects the zero endpoint permitted by [RNG].
func uniformOpen01(rng RNG) float64 {
	for {
		if u := rng.Float64(); u > 0 {
			return u
		}
	}
}

// GammaRV returns one sample from Gamma(alpha, 1) using the Marsaglia-Tsang
// method (2000).
//
// Divide the result by beta to sample Gamma(alpha, beta). GammaRV panics unless
// alpha is finite and positive.
func GammaRV(rng RNG, alpha float64) float64 {
	if math.IsNaN(alpha) || math.IsInf(alpha, 0) || alpha <= 0 {
		panic("stats: GammaRV: alpha must be finite and positive")
	}
	if alpha < 1 {
		// Reduction: Gamma(alpha) ≡ Gamma(alpha+1) × U^(1/alpha), where
		// U ~ Uniform(0,1).
		return GammaRV(rng, alpha+1) * math.Pow(uniformOpen01(rng), 1/alpha)
	}
	d := alpha - 1.0/3.0
	c := 1.0 / math.Sqrt(9.0*d)
	for {
		x := normalStd(rng)
		v := 1.0 + c*x
		if v <= 0 {
			continue
		}
		v = v * v * v
		u := rng.Float64()
		// Cheap squeeze test avoids the log in the common case.
		if u < 1.0-0.0331*(x*x)*(x*x) {
			return d * v
		}
		if logFloat(u) < 0.5*x*x+d*(1.0-v+logFloat(v)) {
			return d * v
		}
	}
}

// BetaRV returns one sample from Beta(alpha, beta) where alpha, beta > 0.
// Uses the ratio-of-gammas identity:
//
//	X = Gamma(alpha) / (Gamma(alpha) + Gamma(beta))
//
// BetaRV panics unless alpha and beta are finite and positive. Extreme shapes
// can underflow both gamma samples to zero, yielding NaN. This sampler is
// intended for shapes representable at float64 precision.
func BetaRV(rng RNG, alpha, beta float64) float64 {
	if math.IsNaN(alpha) || math.IsInf(alpha, 0) || alpha <= 0 ||
		math.IsNaN(beta) || math.IsInf(beta, 0) || beta <= 0 {
		panic("stats: BetaRV: alpha and beta must be finite and positive")
	}
	x := GammaRV(rng, alpha)
	y := GammaRV(rng, beta)
	if x >= y {
		return 1 / (1 + y/x)
	}
	ratio := x / y
	return ratio / (1 + ratio)
}

// ThompsonSample draws one Beta variate per population and returns the index of
// the largest, breaking ties by the smallest index.
//
// Alphas and betas contain each population's posterior shape parameters.
// ThompsonSample does not modify or retain them. It panics if they are empty,
// have different lengths, or contain non-positive or non-finite shapes.
func ThompsonSample(rng RNG, alphas, betas []float64) int {
	n := len(alphas)
	if n == 0 || len(betas) != n {
		panic("stats: ThompsonSample: alphas and betas must be non-empty slices of equal length")
	}
	bestIdx := 0
	bestVal := BetaRV(rng, alphas[0], betas[0])
	for i := 1; i < n; i++ {
		v := BetaRV(rng, alphas[i], betas[i]) //nolint:gosec // i < n = len(alphas) = len(betas), checked above
		if v > bestVal {
			bestVal = v
			bestIdx = i
		}
	}
	return bestIdx
}

// ThompsonTopKSample draws one Beta variate per population and returns the
// indices of the k largest in descending sample order without replacement.
//
// Panics if the slices are empty or have different lengths, or if k is not in
// [1, n], or a shape is non-positive or non-finite. Input slices are neither
// modified nor retained; the returned slice is independently owned. Equal
// samples retain a deterministic selection order, without a promised tie rule.
func ThompsonTopKSample(rng RNG, alphas, betas []float64, k int) []int {
	n := len(alphas)
	if n == 0 || len(betas) != n {
		panic("stats: ThompsonTopKSample: alphas and betas must be non-empty slices of equal length")
	}
	if k < 1 || k > n {
		panic("stats: ThompsonTopKSample: k must be in [1, n]")
	}
	samples := make([]float64, n)
	indices := make([]int, n)
	for i := range n {
		samples[i] = BetaRV(rng, alphas[i], betas[i])
		indices[i] = i
	}
	partialSortDesc(indices, samples, k)
	return indices[:k]
}

// partialSortDesc places the indices of the k largest values first in
// descending value order. It uses O(n*k) comparisons and leaves the tail
// unordered.
func partialSortDesc(indices []int, vals []float64, k int) {
	n := len(indices)
	for j := range k {
		maxAt := j
		for i := j + 1; i < n; i++ {
			if vals[indices[i]] > vals[indices[maxAt]] {
				maxAt = i
			}
		}
		if maxAt != j {
			indices[j], indices[maxAt] = indices[maxAt], indices[j]
		}
	}
}
