package stats

import (
	"math"
	"math/rand"
	"slices"
	"sort"
	"testing"
)

type sequenceRNG struct {
	values []float64
	next   int
}

func (r *sequenceRNG) Float64() float64 {
	v := r.values[r.next]
	r.next++
	return v
}

// sampleVarianceSE returns the standard error of the unbiased sample variance
// from its population variance and excess kurtosis. For n independent draws,
// Var(S²) = (mu4 - ((n-3)/(n-1))*sigma^4) / n.
func sampleVarianceSE(n int, variance, excessKurtosis float64) float64 {
	nf := float64(n)
	mu4 := (excessKurtosis + 3) * variance * variance
	return math.Sqrt((mu4 - (nf-3)/(nf-1)*variance*variance) / nf)
}

func betaExcessKurtosis(alpha, beta float64) float64 {
	s := alpha + beta
	numerator := 6 * ((alpha-beta)*(alpha-beta)*(s+1) - alpha*beta*(s+2))
	denominator := alpha * beta * (s + 2) * (s + 3)
	return numerator / denominator
}

func TestNormalRVPanic(t *testing.T) {
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // math/rand in tests, not security-sensitive
	for _, c := range []struct {
		name     string
		mean, sd float64
	}{
		{"negative sd", 0, -1},
		{"NaN mean", math.NaN(), 1},
		{"infinite mean", math.Inf(1), 1},
		{"NaN sd", 0, math.NaN()},
		{"infinite sd", 0, math.Inf(1)},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("NormalRV(%v, %v) did not panic", c.mean, c.sd)
				}
			}()
			NormalRV(rng, c.mean, c.sd)
		})
	}
}

func TestNormalRVRejectsZeroUniformEndpoint(t *testing.T) {
	rng := &sequenceRNG{values: []float64{0, 0, 0.5, 0.25}}
	if got := NormalRV(rng, 0, 1); !within(got, 0, 1e-15) {
		t.Errorf("NormalRV with zero endpoint retries = %v, want 0", got)
	}
	if rng.next != len(rng.values) {
		t.Errorf("RNG draws = %d, want %d", rng.next, len(rng.values))
	}
}

func TestNormalRVConstant(t *testing.T) {
	// sd = 0: degenerate distribution — must return mean exactly regardless of
	// the RNG state (the sd factor zeroes out the random component).
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // math/rand in tests, not security-sensitive
	for _, mean := range []float64{0, 1, -3.14, 1e10} {
		got := NormalRV(rng, mean, 0)
		if got != mean {
			t.Errorf("NormalRV(rng, %v, 0) = %v, want %v", mean, got, mean)
		}
	}
}

func TestNormalRVMoments(t *testing.T) {
	// Draw n samples from N(mean, sd²) and verify the sample mean, variance, and
	// probability of exceeding the mean within 4 standard errors.
	//
	// The normal law has zero excess kurtosis, which determines the exact
	// finite-sample standard error of the unbiased sample variance.
	const n = 50_000
	rng := rand.New(rand.NewSource(42)) //nolint:gosec // math/rand in tests, not security-sensitive
	mean, sd := 3.0, 2.0
	var w Welford
	aboveMean := 0
	for range n {
		v := NormalRV(rng, mean, sd)
		w.Observe(v)
		if v > mean {
			aboveMean++
		}
	}
	seMean := sd / math.Sqrt(n)
	if !within(w.Mean(), mean, 4*seMean) {
		t.Errorf("NormalRV mean = %.6f, want %.6f +/- %.6f", w.Mean(), mean, 4*seMean)
	}
	seVar := sampleVarianceSE(n, sd*sd, 0)
	if !within(w.Variance(), sd*sd, 4*seVar) {
		t.Errorf("NormalRV variance = %.6f, want %.6f +/- %.6f", w.Variance(), sd*sd, 4*seVar)
	}
	if frac := float64(aboveMean) / n; !within(frac, 0.5, 4*math.Sqrt(0.25/n)) {
		t.Errorf("NormalRV P(X>mean) = %.4f, want 0.5 within 4 SE", frac)
	}
}

func TestGammaRVPanic(t *testing.T) {
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // math/rand in tests, not security-sensitive
	for _, alpha := range []float64{
		0, -1, -0.001, math.Inf(-1), math.Inf(1), math.NaN(),
	} {
		func(a float64) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("GammaRV with alpha=%v did not panic", a)
				}
			}()
			GammaRV(rng, a)
		}(alpha)
	}
}

func TestGammaRVRejectsZeroUniformEndpoint(t *testing.T) {
	// The first three draws make Gamma(1.5) accept its first proposal. The zero
	// is the U in the Gamma(0.5) reduction and must be retried.
	rng := &sequenceRNG{values: []float64{0.5, 0.5, 0.5, 0, 0.5}}
	if got := GammaRV(rng, 0.5); !(got > 0) {
		t.Errorf("GammaRV(0.5) = %v, want > 0", got)
	}
	if rng.next != len(rng.values) {
		t.Errorf("RNG draws = %d, want %d", rng.next, len(rng.values))
	}
}

func TestGammaRVPositive(t *testing.T) {
	// All Gamma samples must be strictly positive for all valid alpha.
	const n = 5_000
	rng := rand.New(rand.NewSource(3)) //nolint:gosec // math/rand in tests, not security-sensitive
	for _, alpha := range []float64{0.1, 0.5, 1, 5, 20} {
		for range n {
			if v := GammaRV(rng, alpha); v <= 0 {
				t.Errorf("GammaRV(alpha=%.1f) = %v, want > 0", alpha, v)
			}
		}
	}
}

func TestGammaRVMoments(t *testing.T) {
	// For Gamma(alpha, 1): E[X] = alpha, Var[X] = alpha.
	//
	// Gamma(alpha, 1) has excess kurtosis 6/alpha, which determines the exact
	// finite-sample standard error of the unbiased sample variance.
	const n = 50_000
	rng := rand.New(rand.NewSource(42)) //nolint:gosec // math/rand in tests, not security-sensitive
	for _, alpha := range []float64{0.5, 1, 2, 5, 10} {
		var w Welford
		aboveOne := 0
		for range n {
			v := GammaRV(rng, alpha)
			w.Observe(v)
			if alpha == 1 && v > 1 {
				aboveOne++
			}
		}
		seMean := math.Sqrt(alpha / n)
		if !within(w.Mean(), alpha, 4*seMean) {
			t.Errorf("GammaRV(alpha=%.1f) mean = %.6f, want %.6f +/- %.6f",
				alpha, w.Mean(), alpha, 4*seMean)
		}
		seVar := sampleVarianceSE(n, alpha, 6/alpha)
		if !within(w.Variance(), alpha, 4*seVar) {
			t.Errorf("GammaRV(alpha=%.1f) variance = %.6f, want %.6f +/- %.6f",
				alpha, w.Variance(), alpha, 4*seVar)
		}
		if alpha == 1 {
			want := math.Exp(-1)
			se := math.Sqrt(want * (1 - want) / n)
			if frac := float64(aboveOne) / n; !within(frac, want, 4*se) {
				t.Errorf("GammaRV(1) P(X>1) = %.4f, want %.4f within 4 SE",
					frac, want)
			}
		}
	}
}

func TestBetaRVPanic(t *testing.T) {
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // math/rand in tests, not security-sensitive
	for _, c := range [][2]float64{
		{0, 1},
		{1, 0},
		{-1, 1},
		{1, -1},
		{0, 0},
		{math.NaN(), 1},
		{1, math.NaN()},
		{math.Inf(1), 1},
		{1, math.Inf(1)},
	} {
		func(alpha, beta float64) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("BetaRV(alpha=%v, beta=%v) did not panic", alpha, beta)
				}
			}()
			BetaRV(rng, alpha, beta)
		}(c[0], c[1])
	}
}

func TestBetaRVRange(t *testing.T) {
	// All Beta samples must lie strictly in (0, 1).
	const n = 5_000
	rng := rand.New(rand.NewSource(5)) //nolint:gosec // math/rand in tests, not security-sensitive
	for _, c := range [][2]float64{{1, 1}, {2, 5}, {0.5, 0.5}, {10, 1}} {
		alpha, beta := c[0], c[1]
		for range n {
			v := BetaRV(rng, alpha, beta)
			if v <= 0 || v >= 1 {
				t.Errorf("BetaRV(%.1f, %.1f) = %v, want value in (0, 1)", alpha, beta, v)
			}
		}
	}
}

func TestBetaRVMoments(t *testing.T) {
	// E[Beta(a,b)] = a/(a+b), Var[Beta(a,b)] = ab / ((a+b)²·(a+b+1)).
	// The exact finite-sample SE of the unbiased sample variance follows from
	// the Beta excess-kurtosis identity.
	const n = 50_000
	rng := rand.New(rand.NewSource(42)) //nolint:gosec // math/rand in tests, not security-sensitive
	for _, c := range [][2]float64{{1, 1}, {2, 5}, {3, 7}, {10, 10}} {
		a, b := c[0], c[1]
		s := a + b
		wantMean := a / s
		wantVar := a * b / (s * s * (s + 1))
		var w Welford
		for range n {
			w.Observe(BetaRV(rng, a, b))
		}
		seMean := math.Sqrt(wantVar / n)
		if !within(w.Mean(), wantMean, 4*seMean) {
			t.Errorf("BetaRV(%.0f,%.0f) mean = %.6f, want %.6f +/- %.6f",
				a, b, w.Mean(), wantMean, 4*seMean)
		}
		seVar := sampleVarianceSE(n, wantVar, betaExcessKurtosis(a, b))
		if !within(w.Variance(), wantVar, 4*seVar) {
			t.Errorf("BetaRV(%.0f,%.0f) variance = %.6f, want %.6f +/- %.6f",
				a, b, w.Variance(), wantVar, 4*seVar)
		}
	}
}

func TestThompsonSamplePanic(t *testing.T) {
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // math/rand in tests, not security-sensitive
	// Empty slices.
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("ThompsonSample with empty slices did not panic")
			}
		}()
		ThompsonSample(rng, nil, nil)
	}()
	// Mismatched lengths.
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("ThompsonSample with mismatched slices did not panic")
			}
		}()
		ThompsonSample(rng, []float64{1, 1}, []float64{1})
	}()
}

func TestThompsonSampleSingle(t *testing.T) {
	// n=1: must always return 0 regardless of draws.
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // math/rand in tests, not security-sensitive
	for range 100 {
		if got := ThompsonSample(rng, []float64{2}, []float64{3}); got != 0 {
			t.Errorf("ThompsonSample single population: got %d, want 0", got)
		}
	}
}

func TestThompsonSampleClearWinner(t *testing.T) {
	// Beta(100, 1) vs Beta(1, 100): population 0 is overwhelmingly superior.
	// The probability that a draw from Beta(100,1) loses to Beta(1,100) is
	// astronomically small, so we expect population 0 to win every trial.
	const n = 1000
	rng := rand.New(rand.NewSource(42)) //nolint:gosec // math/rand in tests, not security-sensitive
	alphas := []float64{100, 1}
	betas := []float64{1, 100}
	wins := 0
	for range n {
		if ThompsonSample(rng, alphas, betas) == 0 {
			wins++
		}
	}
	if wins < n-3 {
		t.Errorf("clear winner (pop 0) won %d/%d, want at least %d", wins, n, n-3)
	}
}

func TestThompsonSampleUniform(t *testing.T) {
	// Three equal Beta(2, 2) posteriors: each population should win roughly
	// 1/3 of the time. 4 SE ≈ 4·√((1/3)·(2/3)/n) ≈ 0.011.
	const n = 30_000
	rng := rand.New(rand.NewSource(42)) //nolint:gosec // math/rand in tests, not security-sensitive
	alphas := []float64{2, 2, 2}
	betas := []float64{2, 2, 2}
	var counts [3]int
	for range n {
		counts[ThompsonSample(rng, alphas, betas)]++
	}
	for i, c := range counts {
		if frac := float64(c) / n; !within(frac, 1.0/3, 0.015) {
			t.Errorf("equal posteriors: pop %d won %.4f, want 1/3 +/- 0.015", i, frac)
		}
	}
}

func TestThompsonTopKSamplePanic(t *testing.T) {
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // math/rand in tests, not security-sensitive
	cases := []struct {
		name   string
		alphas []float64
		betas  []float64
		k      int
	}{
		{"k=0", []float64{1, 1}, []float64{1, 1}, 0},
		{"k>n", []float64{1, 1}, []float64{1, 1}, 3},
		{"empty", nil, nil, 1},
		{"mismatched", []float64{1, 1}, []float64{1}, 1},
	}
	for _, c := range cases {
		func(name string, alphas, betas []float64, k int) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("ThompsonTopKSample (%s) did not panic", name)
				}
			}()
			ThompsonTopKSample(rng, alphas, betas, k)
		}(c.name, c.alphas, c.betas, c.k)
	}
}

func TestThompsonTopKSampleDescendingOrder(t *testing.T) {
	const seed = 55
	alphas := []float64{2, 5, 1, 8, 3}
	betas := []float64{7, 2, 4, 1, 3}

	referenceRNG := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic test fixture
	samples := make([]float64, len(alphas))
	want := make([]int, len(alphas))
	for i := range alphas {
		samples[i] = BetaRV(referenceRNG, alphas[i], betas[i])
		want[i] = i
	}
	sort.Slice(want, func(i, j int) bool {
		return samples[want[i]] > samples[want[j]]
	})

	for k := 1; k <= len(alphas); k++ {
		rng := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic test fixture
		got := ThompsonTopKSample(rng, alphas, betas, k)
		if !slices.Equal(got, want[:k]) {
			t.Errorf("k=%d: indices = %v, want descending prefix %v", k, got, want[:k])
		}
	}
}
