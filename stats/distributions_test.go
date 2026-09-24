package stats

import (
	"math"
	"strconv"
	"testing"
)

// within reports whether |got−want| ≤ tol, with explicit handling for NaN and
// Inf.
func within(got, want, tol float64) bool {
	if math.IsNaN(want) {
		return math.IsNaN(got)
	}
	if math.IsInf(want, 0) {
		return got == want
	}
	return math.Abs(got-want) <= tol
}

func TestNormPDF(t *testing.T) {
	// 1/sqrt(2π) ≈ 0.3989422804014327
	const invSqrt2piRef = 0.3989422804014327

	cases := []struct {
		x    float64
		want float64
	}{
		{0, invSqrt2piRef},
		{1, 0.24197072451914337},
		{-1, 0.24197072451914337}, // symmetric
		{2, 0.05399096651318806},
		{-2, 0.05399096651318806}, // symmetric
		{math.Inf(1), 0},
		{math.Inf(-1), 0},
	}
	for _, c := range cases {
		got := NormPDF(c.x)
		if !within(got, c.want, 1e-15) {
			t.Errorf("NormPDF(%v) = %v, want %v", c.x, got, c.want)
		}
	}
}

func TestNormLogPDF(t *testing.T) {
	cases := []struct {
		x    float64
		want float64
	}{
		{0, -0.9189385332046728},
		{1, -1.4189385332046727},
		{-1, -1.4189385332046727},
	}
	for _, c := range cases {
		got := NormLogPDF(c.x)
		if !within(got, c.want, 1e-15) {
			t.Errorf("NormLogPDF(%v) = %v, want %v", c.x, got, c.want)
		}
	}
	// NormLogPDF and NormPDF must be consistent.
	for _, x := range []float64{-3, -1, 0, 1, 3} {
		if !within(math.Exp(NormLogPDF(x)), NormPDF(x), 1e-15) {
			t.Errorf("exp(NormLogPDF(%v)) != NormPDF(%v)", x, x)
		}
	}
}

func TestNormCDF(t *testing.T) {
	cases := []struct {
		x    float64
		want float64
		tol  float64
	}{
		{0, 0.5, 1e-15},
		{1, 0.8413447460685429, 1e-15},
		{-1, 0.15865525393145705, 1e-15},
		{1.96, 0.9750021048517795, 1e-12},
		{-1.96, 0.0249978951482205, 1e-12},
		{3, 0.9986501019683699, 1e-12},
		{math.Inf(1), 1, 0},
		{math.Inf(-1), 0, 0},
	}
	for _, c := range cases {
		got := NormCDF(c.x)
		if !within(got, c.want, c.tol) {
			t.Errorf("NormCDF(%v) = %.15g, want %.15g", c.x, got, c.want)
		}
	}
	// CDF and its reflection must sum to 1.
	for _, x := range []float64{-2, -1, 0, 1, 2} {
		if !within(NormCDF(x)+NormCDF(-x), 1, 1e-15) {
			t.Errorf("NormCDF(%v)+NormCDF(%v) != 1", x, -x)
		}
	}
}

func TestNormQuantile(t *testing.T) {
	cases := []struct {
		p    float64
		want float64
		tol  float64
	}{
		{0, math.Inf(-1), 0},
		{1, math.Inf(1), 0},
		{-0.1, math.NaN(), 0},
		{1.1, math.NaN(), 0},
		{math.NaN(), math.NaN(), 0},
		{0.5, 0, 2e-9},
		{0.975, 1.9599639845400536, 2e-9},
		{0.025, -1.9599639845400536, 2e-9},
		{0.99, 2.3263478740408408, 2e-9},
		{0.01, -2.3263478740408408, 2e-9},
		// Far tail: error slightly exceeds the 1.15e-9 central guarantee.
		{0.001, -3.090232306167813, 5e-9},
		{0.9999, 3.719016485455709, 5e-9},
	}
	for _, c := range cases {
		got := NormQuantile(c.p)
		if !within(got, c.want, c.tol) {
			t.Errorf("NormQuantile(%v) = %.15g, want %.15g", c.p, got, c.want)
		}
	}
	// Round-trip: NormCDF(NormQuantile(p)) ≈ p.
	// Composite error: quantile error × NormPDF(quantile); use 5e-9 headroom.
	for _, p := range []float64{0.01, 0.1, 0.5, 0.9, 0.99} {
		if !within(NormCDF(NormQuantile(p)), p, 5e-9) {
			t.Errorf("round-trip failed at p=%v", p)
		}
	}
}

func TestGammaPDF(t *testing.T) {
	cases := []struct {
		x, alpha, beta float64
		want           float64
		tol            float64
	}{
		// Gamma(1,1) = Exponential(1): f(x) = exp(-x)
		{1, 1, 1, math.Exp(-1), 1e-15},
		{2, 1, 1, math.Exp(-2), 1e-15},
		// Gamma(2,1): f(x) = x*exp(-x)
		{1, 2, 1, 1 * math.Exp(-1), 1e-15},
		{2, 2, 1, 2 * math.Exp(-2), 1e-15},
		// Gamma(2,3): f(x) = 9x*exp(-3x)
		{1, 2, 3, 9 * math.Exp(-3), 1e-14},
		// Boundary / invalid
		{0, 0.5, 1, math.Inf(1), 0},
		{0, 1, 3, 3, 0},
		{0, 2, 1, 0, 0},
		{-1, 1, 1, 0, 0},
		{math.Inf(1), 2, 1, 0, 0},
	}
	for _, c := range cases {
		got := GammaPDF(c.x, c.alpha, c.beta)
		if !within(got, c.want, c.tol) {
			t.Errorf("GammaPDF(%v,%v,%v) = %v, want %v", c.x, c.alpha, c.beta, got, c.want)
		}
	}
	// Invalid parameters return NaN.
	for _, bad := range [][2]float64{{0, 1}, {1, 0}, {-1, 1}, {1, -1}} {
		if got := GammaPDF(1, bad[0], bad[1]); !math.IsNaN(got) {
			t.Errorf("GammaPDF(1,%v,%v) = %v, want NaN", bad[0], bad[1], got)
		}
	}
}

func TestGammaLogPDF(t *testing.T) {
	// LogPDF must be consistent with PDF.
	cases := [][3]float64{
		{1, 1, 1},
		{2, 2, 1},
		{0.5, 3, 2},
	}
	for _, c := range cases {
		x, alpha, beta := c[0], c[1], c[2]
		got := math.Exp(GammaLogPDF(x, alpha, beta))
		want := GammaPDF(x, alpha, beta)
		if !within(got, want, 1e-13) {
			t.Errorf("exp(GammaLogPDF(%v,%v,%v)) = %v, want %v", x, alpha, beta, got, want)
		}
	}
	// The density at zero depends on alpha.
	if !math.IsInf(GammaLogPDF(0, 0.5, 1), 1) {
		t.Error("GammaLogPDF(0,0.5,1) should be +Inf")
	}
	if got := GammaLogPDF(0, 1, 3); got != math.Log(3) {
		t.Errorf("GammaLogPDF(0,1,3) = %v, want log(3)", got)
	}
	if !math.IsInf(GammaLogPDF(0, 2, 1), -1) {
		t.Error("GammaLogPDF(0,2,1) should be -Inf")
	}
	if !math.IsInf(GammaLogPDF(-1, 1, 1), -1) {
		t.Error("GammaLogPDF(-1,1,1) should be -Inf")
	}
	if !math.IsInf(GammaLogPDF(math.Inf(1), 2, 1), -1) {
		t.Error("GammaLogPDF(+Inf,2,1) should be -Inf")
	}
}

func TestGammaCDF(t *testing.T) {
	const tol = 1e-7
	cases := []struct {
		x, alpha, beta float64
		want           float64
	}{
		// Gamma(1,1) = Exp(1): CDF = 1 − e^{−x}
		{1, 1, 1, 1 - math.Exp(-1)},
		{2, 1, 1, 1 - math.Exp(-2)},
		// Gamma(2,1): CDF(1) = 1 − 2e^{−1}
		{1, 2, 1, 1 - 2*math.Exp(-1)},
		// Gamma(2,3): x maps to β*x=3, CDF(1,2,3) = P(2,3)
		// P(2,3) = 1 − e^{-3}(1+3) = 1 − 4e^{-3}
		{1, 2, 3, 1 - 4*math.Exp(-3)},
		// Boundaries
		{0, 1, 1, 0},
		{-5, 1, 1, 0},
		{math.Inf(1), 2, 1, 1},
	}
	for _, c := range cases {
		got := GammaCDF(c.x, c.alpha, c.beta)
		if !within(got, c.want, tol) {
			t.Errorf("GammaCDF(%v,%v,%v) = %.10g, want %.10g", c.x, c.alpha, c.beta, got, c.want)
		}
	}
	// Invalid parameters.
	if got := GammaCDF(1, 0, 1); !math.IsNaN(got) {
		t.Errorf("GammaCDF(1,0,1) = %v, want NaN", got)
	}
	// CDF is monotonically non-decreasing.
	for x0, x1 := 0.5, 1.0; x0 < 5; x0, x1 = x1, x1+0.5 {
		if GammaCDF(x0, 2, 1) > GammaCDF(x1, 2, 1) {
			t.Errorf("GammaCDF not monotone at %v->%v", x0, x1)
		}
	}
}

func TestGammaMoments(t *testing.T) {
	if !within(GammaMean(4, 2), 2, 1e-15) {
		t.Errorf("GammaMean(4,2) = %v, want 2", GammaMean(4, 2))
	}
	if !within(GammaVariance(4, 2), 1, 1e-15) {
		t.Errorf("GammaVariance(4,2) = %v, want 1", GammaVariance(4, 2))
	}
	if !within(GammaMean(3, 6), 0.5, 1e-15) {
		t.Errorf("GammaMean(3,6) = %v, want 0.5", GammaMean(3, 6))
	}
	if !within(GammaVariance(3, 6), 3.0/36, 1e-15) {
		t.Errorf("GammaVariance(3,6) = %v, want %v", GammaVariance(3, 6), 3.0/36)
	}
	for _, parameters := range [][2]float64{
		{0, 1},
		{-1, 1},
		{1, 0},
		{1, -1},
		{math.NaN(), 1},
		{1, math.NaN()},
		{math.Inf(1), 1},
		{1, math.Inf(1)},
	} {
		if got := GammaMean(parameters[0], parameters[1]); !math.IsNaN(got) {
			t.Errorf("GammaMean(%v,%v) = %v, want NaN", parameters[0], parameters[1], got)
		}
		if got := GammaVariance(parameters[0], parameters[1]); !math.IsNaN(got) {
			t.Errorf("GammaVariance(%v,%v) = %v, want NaN", parameters[0], parameters[1], got)
		}
	}
}

func TestBetaPDF(t *testing.T) {
	cases := []struct {
		x, alpha, beta float64
		want           float64
		tol            float64
	}{
		// Beta(1,1) = Uniform(0,1): density = 1
		{0.5, 1, 1, 1, 1e-15},
		{0.1, 1, 1, 1, 1e-15},
		// Beta(2,2): density = 6x(1-x), normalised by B(2,2)=1/6
		{0.5, 2, 2, 1.5, 1e-14}, // 6 * 0.5 * 0.5
		{0.3, 2, 2, 6 * 0.3 * 0.7, 1e-14},
		// Beta(2,5): B(2,5)=1/30, density=30·x·(1-x)^4
		{0.3, 2, 5, 30 * 0.3 * math.Pow(0.7, 4), 1e-14},
		// Boundaries
		{0, 2, 2, 0, 0},
		{1, 2, 2, 0, 0},
		{-0.1, 2, 2, 0, 0},
		{1.1, 2, 2, 0, 0},
		{0, 0.5, 2, math.Inf(1), 0},
		{0, 1, 2, 2, 0},
		{1, 2, 0.5, math.Inf(1), 0},
		{1, 2, 1, 2, 0},
	}
	for _, c := range cases {
		got := BetaPDF(c.x, c.alpha, c.beta)
		if !within(got, c.want, c.tol) {
			t.Errorf("BetaPDF(%v,%v,%v) = %v, want %v", c.x, c.alpha, c.beta, got, c.want)
		}
	}
	// Invalid parameters.
	if got := BetaPDF(0.5, 0, 1); !math.IsNaN(got) {
		t.Errorf("BetaPDF(0.5,0,1) = %v, want NaN", got)
	}
}

func TestBetaLogPDF(t *testing.T) {
	// Must be consistent with BetaPDF.
	cases := [][3]float64{
		{0.3, 2, 5},
		{0.5, 2, 2},
		{0.1, 3, 7},
	}
	for _, c := range cases {
		x, alpha, beta := c[0], c[1], c[2]
		got := math.Exp(BetaLogPDF(x, alpha, beta))
		want := BetaPDF(x, alpha, beta)
		if !within(got, want, 1e-13) {
			t.Errorf("exp(BetaLogPDF(%v,%v,%v)) = %v, want %v", x, alpha, beta, got, want)
		}
	}
	// x outside [0,1] → -Inf.
	for _, x := range []float64{-0.1, 1.1} {
		if !math.IsInf(BetaLogPDF(x, 2, 2), -1) {
			t.Errorf("BetaLogPDF(%v,2,2) should be -Inf", x)
		}
	}
	for _, boundary := range [][3]float64{
		{0, 0.5, math.Inf(1)},
		{0, 1, math.Log(2)},
		{0, 2, math.Inf(-1)},
		{1, 0.5, math.Inf(1)},
		{1, 1, math.Log(2)},
		{1, 2, math.Inf(-1)},
	} {
		x, adjacentShape, want := boundary[0], boundary[1], boundary[2]
		var got float64
		if x == 0 {
			got = BetaLogPDF(x, adjacentShape, 2)
		} else {
			got = BetaLogPDF(x, 2, adjacentShape)
		}
		if !within(got, want, 0) {
			t.Errorf("BetaLogPDF boundary (%v,%v) = %v, want %v", x, adjacentShape, got, want)
		}
	}
}

func TestBetaCDF(t *testing.T) {
	const tol = 1e-7
	cases := []struct {
		x, alpha, beta float64
		want           float64
	}{
		// Beta(1,1) = Uniform: CDF(x) = x
		{0.3, 1, 1, 0.3},
		{0.7, 1, 1, 0.7},
		// Beta(2,5): CDF(0.3) = 0.579825 (analytically derived)
		{0.3, 2, 5, 0.579825},
		// Beta(a,a) is symmetric: CDF(0.5) = 0.5
		{0.5, 3, 3, 0.5},
		{0.5, 5, 5, 0.5},
		// Beta(3,1): CDF(x) = x^3
		{0.5, 3, 1, 0.125},
		{0.8, 3, 1, 0.512},
		// Boundaries
		{0, 2, 5, 0},
		{1, 2, 5, 1},
		{-0.1, 2, 5, 0},
		{1.1, 2, 5, 1},
	}
	for _, c := range cases {
		got := BetaCDF(c.x, c.alpha, c.beta)
		if !within(got, c.want, tol) {
			t.Errorf("BetaCDF(%v,%v,%v) = %.10g, want %.10g", c.x, c.alpha, c.beta, got, c.want)
		}
	}
	// Symmetry: I_x(a,b) + I_{1-x}(b,a) = 1
	for _, x := range []float64{0.2, 0.4, 0.6, 0.8} {
		if !within(BetaCDF(x, 2, 5)+BetaCDF(1-x, 5, 2), 1, tol) {
			t.Errorf("BetaCDF symmetry failed at x=%v", x)
		}
	}
	// Invalid parameters.
	if got := BetaCDF(0.5, -1, 1); !math.IsNaN(got) {
		t.Errorf("BetaCDF(0.5,-1,1) = %v, want NaN", got)
	}
}

func TestBetaQuantile(t *testing.T) {
	cases := []struct {
		p, alpha, beta float64
		want           float64
		tol            float64
	}{
		// Beta(1,1) = Uniform: quantile(p) = p.
		{0.3, 1, 1, 0.3, 1e-8},
		{0.7, 1, 1, 0.7, 1e-8},
		// Beta(3,1): CDF(x) = x^3, so quantile(p) = p^(1/3).
		{0.125, 3, 1, 0.5, 1e-7}, // 0.5^3 = 0.125
		{0.512, 3, 1, 0.8, 1e-7}, // 0.8^3 = 0.512
		// Beta(1,3): CDF(x) = 1-(1-x)^3, so quantile(p) = 1-(1-p)^(1/3).
		{0.875, 1, 3, 0.5, 1e-7}, // 1-(1-0.5)^3 = 0.875
		// Beta(a,a) is symmetric: median is 0.5.
		{0.5, 2, 2, 0.5, 1e-8},
		{0.5, 5, 5, 0.5, 1e-8},
		// Beta(1,100): quantile(0.975) = 1 - 0.025^(1/100). This is the two-sided
		// 95% Clopper–Pearson upper bound for 0 successes in 100 trials.
		{0.975, 1, 100, 1 - math.Pow(0.025, 0.01), 1e-8},
		// Beta(1/2,1/2) = arcsine: density unbounded at BOTH endpoints. CDF(x) =
		// (2/π)·arcsin(√x), so quantile(p) = sin²(pπ/2). This exercises the
		// unbounded-density case a bare Newton step mishandles.
		{0.3, 0.5, 0.5, math.Pow(math.Sin(0.3*math.Pi/2), 2), 1e-7},
		{0.8, 0.5, 0.5, math.Pow(math.Sin(0.8*math.Pi/2), 2), 1e-7},
		// Boundaries (exact).
		{0, 2, 5, 0, 0},
		{1, 2, 5, 1, 0},
	}
	for _, c := range cases {
		got := BetaQuantile(c.p, c.alpha, c.beta)
		if !within(got, c.want, c.tol) {
			t.Errorf("BetaQuantile(%v,%v,%v) = %.12g, want %.12g", c.p, c.alpha, c.beta, got, c.want)
		}
	}

	// Invalid p → NaN.
	for _, p := range []float64{-0.1, 1.1, math.NaN()} {
		if got := BetaQuantile(p, 2, 5); !math.IsNaN(got) {
			t.Errorf("BetaQuantile(%v,2,5) = %v, want NaN", p, got)
		}
	}
	// Invalid parameters → NaN.
	for _, bad := range [][2]float64{{0, 1}, {1, 0}, {-1, 1}, {1, -1}} {
		if got := BetaQuantile(0.5, bad[0], bad[1]); !math.IsNaN(got) {
			t.Errorf("BetaQuantile(0.5,%v,%v) = %v, want NaN", bad[0], bad[1], got)
		}
	}

	// Round-trip against BetaCDF (the defining property), over a spread of
	// parameters and probabilities. Tolerance is set by BetaCDF's own accuracy.
	params := [][2]float64{{1, 1}, {2, 5}, {5, 2}, {0.5, 0.5}, {3, 3}, {2, 100}}
	probs := []float64{0.01, 0.1, 0.25, 0.5, 0.75, 0.9, 0.99}
	for _, ab := range params {
		a, b := ab[0], ab[1]
		for _, p := range probs {
			x := BetaQuantile(p, a, b)
			if x < 0 || x > 1 {
				t.Errorf("BetaQuantile(%v,%v,%v) = %v, out of [0,1]", p, a, b, x)
			}
			if !within(BetaCDF(x, a, b), p, 1e-7) {
				t.Errorf("round-trip BetaCDF(BetaQuantile(%v,%v,%v)) = %.10g, want %.10g",
					p, a, b, BetaCDF(x, a, b), p)
			}
		}
	}

	// Reflection symmetry: quantile(p; a, b) = 1 − quantile(1−p; b, a).
	for _, p := range []float64{0.1, 0.3, 0.6, 0.9} {
		lhs := BetaQuantile(p, 2, 5)
		rhs := 1 - BetaQuantile(1-p, 5, 2)
		if !within(lhs, rhs, 1e-8) {
			t.Errorf("BetaQuantile symmetry failed at p=%v: %v vs %v", p, lhs, rhs)
		}
	}

	// Monotonicity in p (the quantile function is increasing).
	prev := BetaQuantile(0.01, 2, 5)
	for _, p := range []float64{0.05, 0.2, 0.5, 0.8, 0.95, 0.99} {
		cur := BetaQuantile(p, 2, 5)
		if cur <= prev {
			t.Errorf("BetaQuantile not increasing: at p=%v got %v, previous %v", p, cur, prev)
		}
		prev = cur
	}
}

func TestBetaMoments(t *testing.T) {
	// Beta(2,5): mean = 2/7, variance = 2*5/(7^2*8)
	if !within(BetaMean(2, 5), 2.0/7, 1e-15) {
		t.Errorf("BetaMean(2,5) = %v, want %v", BetaMean(2, 5), 2.0/7)
	}
	want := 2.0 * 5 / (49 * 8)
	if !within(BetaVariance(2, 5), want, 1e-15) {
		t.Errorf("BetaVariance(2,5) = %v, want %v", BetaVariance(2, 5), want)
	}
	// Beta(1,1) = Uniform: mean = 0.5, variance = 1/12
	if !within(BetaMean(1, 1), 0.5, 1e-15) {
		t.Errorf("BetaMean(1,1) = %v, want 0.5", BetaMean(1, 1))
	}
	if !within(BetaVariance(1, 1), 1.0/12, 1e-15) {
		t.Errorf("BetaVariance(1,1) = %v, want %v", BetaVariance(1, 1), 1.0/12)
	}
	for _, parameters := range [][2]float64{
		{0, 1},
		{-1, 1},
		{1, 0},
		{1, -1},
		{math.NaN(), 1},
		{1, math.NaN()},
		{math.Inf(1), 1},
		{1, math.Inf(1)},
	} {
		if got := BetaMean(parameters[0], parameters[1]); !math.IsNaN(got) {
			t.Errorf("BetaMean(%v,%v) = %v, want NaN", parameters[0], parameters[1], got)
		}
		if got := BetaVariance(parameters[0], parameters[1]); !math.IsNaN(got) {
			t.Errorf("BetaVariance(%v,%v) = %v, want NaN", parameters[0], parameters[1], got)
		}
	}
	if got := BetaMean(math.MaxFloat64, math.MaxFloat64); got != 0.5 {
		t.Errorf("BetaMean(MaxFloat64,MaxFloat64) = %v, want 0.5", got)
	}
}

func TestBernoulliPMF(t *testing.T) {
	cases := []struct {
		k    int
		p    float64
		want float64
	}{
		{0, 0.3, 0.7},
		{1, 0.3, 0.3},
		{0, 0.0, 1.0},
		{1, 0.0, 0.0},
		{0, 1.0, 0.0},
		{1, 1.0, 1.0},
		{2, 0.5, 0},
		{-1, 0.5, 0},
	}
	for _, c := range cases {
		got := BernoulliPMF(c.k, c.p)
		if !within(got, c.want, 1e-15) {
			t.Errorf("BernoulliPMF(%v,%v) = %v, want %v", c.k, c.p, got, c.want)
		}
	}
	// Invalid p returns NaN regardless of k.
	for _, p := range []float64{-0.1, 1.1, math.NaN()} {
		for _, k := range []int{0, 1, 2} {
			if got := BernoulliPMF(k, p); !math.IsNaN(got) {
				t.Errorf("BernoulliPMF(%v,%v) = %v, want NaN", k, p, got)
			}
		}
	}
}

func TestBernoulliLogPMF(t *testing.T) {
	if !within(BernoulliLogPMF(1, 0.3), math.Log(0.3), 1e-15) {
		t.Error("BernoulliLogPMF(1,0.3)")
	}
	if !within(BernoulliLogPMF(0, 0.3), math.Log(0.7), 1e-15) {
		t.Error("BernoulliLogPMF(0,0.3)")
	}
	if !math.IsInf(BernoulliLogPMF(2, 0.5), -1) {
		t.Error("BernoulliLogPMF(2,0.5) should be -Inf")
	}
	// Consistency with PMF.
	for _, p := range []float64{0.1, 0.5, 0.9} {
		for _, k := range []int{0, 1} {
			if !within(math.Exp(BernoulliLogPMF(k, p)), BernoulliPMF(k, p), 1e-15) {
				t.Errorf("exp(BernoulliLogPMF(%v,%v)) != BernoulliPMF", k, p)
			}
		}
	}
	// Invalid p returns NaN regardless of k.
	for _, p := range []float64{-0.1, 1.1, math.NaN()} {
		if got := BernoulliLogPMF(0, p); !math.IsNaN(got) {
			t.Errorf("BernoulliLogPMF(0,%v) = %v, want NaN", p, got)
		}
	}
}

func TestBernoulliMoments(t *testing.T) {
	if !within(BernoulliMean(0.3), 0.3, 1e-15) {
		t.Error("BernoulliMean")
	}
	if !within(BernoulliVariance(0.3), 0.21, 1e-15) {
		t.Error("BernoulliVariance")
	}
	if !within(BernoulliVariance(0.5), 0.25, 1e-15) {
		t.Error("BernoulliVariance(0.5)")
	}
	if !within(BernoulliVariance(0.0), 0, 1e-15) {
		t.Error("BernoulliVariance(0)")
	}
	for _, p := range []float64{-0.1, 1.1, math.Inf(-1), math.Inf(1), math.NaN()} {
		if got := BernoulliMean(p); !math.IsNaN(got) {
			t.Errorf("BernoulliMean(%v) = %v, want NaN", p, got)
		}
		if got := BernoulliVariance(p); !math.IsNaN(got) {
			t.Errorf("BernoulliVariance(%v) = %v, want NaN", p, got)
		}
	}
}

func TestBinomialPMF(t *testing.T) {
	cases := []struct {
		k, n int
		p    float64
		want float64
		tol  float64
	}{
		// Binomial(5, 0.5)
		{0, 5, 0.5, 0.03125, 1e-15},
		{5, 5, 0.5, 0.03125, 1e-15},
		{2, 5, 0.5, 0.3125, 1e-15},
		// Binomial(10, 0.3): PMF(3) = C(10,3)*0.3^3*0.7^7
		//nolint:staticcheck // math.Pow mirrors the p^k q^(n-k) binomial formula
		{3, 10, 0.3, 120 * math.Pow(0.3, 3) * math.Pow(0.7, 7), 1e-12},
		// Degenerate p=0 and p=1
		{0, 10, 0, 1, 1e-15},
		{1, 10, 0, 0, 1e-15},
		{10, 10, 1, 1, 1e-15},
		{9, 10, 1, 0, 1e-15},
		// Out-of-range k
		{-1, 5, 0.5, 0, 0},
		{6, 5, 0.5, 0, 0},
		// Large n: Binomial(100, 0.5) at mean — just checks no panic / NaN.
		{50, 100, 0.5, 0.07958923738717867, 1e-8},
	}
	for _, c := range cases {
		got := BinomialPMF(c.k, c.n, c.p)
		if !within(got, c.want, c.tol) {
			t.Errorf("BinomialPMF(%v,%v,%v) = %.12g, want %.12g", c.k, c.n, c.p, got, c.want)
		}
	}
	if got := BinomialPMF(0, -1, 0.5); !math.IsNaN(got) {
		t.Errorf("BinomialPMF(0,-1,0.5) = %v, want NaN", got)
	}
}

func TestBinomialLogPMF(t *testing.T) {
	// Consistency with BinomialPMF for valid inputs.
	cases := [][3]float64{
		{2, 5, 0.5},
		{3, 10, 0.3},
		{0, 10, 0.2},
		{10, 10, 0.8},
	}
	for _, c := range cases {
		k, n, p := int(c[0]), int(c[1]), c[2]
		got := math.Exp(BinomialLogPMF(k, n, p))
		want := BinomialPMF(k, n, p)
		if !within(got, want, 1e-12) {
			t.Errorf("exp(BinomialLogPMF(%v,%v,%v)) = %v, want %v", k, n, p, got, want)
		}
	}
	// Out of range → -Inf.
	if !math.IsInf(BinomialLogPMF(-1, 5, 0.5), -1) {
		t.Error("BinomialLogPMF(-1,5,0.5) should be -Inf")
	}
	if got := BinomialLogPMF(0, -1, 0.5); !math.IsNaN(got) {
		t.Errorf("BinomialLogPMF(0,-1,0.5) = %v, want NaN", got)
	}
	// p=0, k=0 → 0 (log(1) = 0).
	if !within(BinomialLogPMF(0, 5, 0), 0, 1e-15) {
		t.Error("BinomialLogPMF(0,5,0) should be 0")
	}
	// p=0, k>0 → -Inf.
	if !math.IsInf(BinomialLogPMF(1, 5, 0), -1) {
		t.Error("BinomialLogPMF(1,5,0) should be -Inf")
	}
	// Invalid p → NaN.
	for _, p := range []float64{-0.1, 1.1, math.NaN()} {
		if got := BinomialLogPMF(2, 5, p); !math.IsNaN(got) {
			t.Errorf("BinomialLogPMF(2,5,%v) = %v, want NaN", p, got)
		}
		if got := BinomialPMF(2, 5, p); !math.IsNaN(got) {
			t.Errorf("BinomialPMF(2,5,%v) = %v, want NaN", p, got)
		}
	}
}

func TestBinomialMoments(t *testing.T) {
	if !within(BinomialMean(10, 0.3), 3.0, 1e-15) {
		t.Error("BinomialMean(10,0.3)")
	}
	if !within(BinomialVariance(10, 0.3), 2.1, 1e-15) {
		t.Error("BinomialVariance(10,0.3)")
	}
	if !within(BinomialMean(0, 0.5), 0, 1e-15) {
		t.Error("BinomialMean(0,0.5)")
	}
	for _, input := range []struct {
		n int
		p float64
	}{{-1, 0.5}, {1, -0.1}, {1, 1.1}, {1, math.NaN()}} {
		if got := BinomialMean(input.n, input.p); !math.IsNaN(got) {
			t.Errorf("BinomialMean(%d,%v) = %v, want NaN", input.n, input.p, got)
		}
		if got := BinomialVariance(input.n, input.p); !math.IsNaN(got) {
			t.Errorf("BinomialVariance(%d,%v) = %v, want NaN", input.n, input.p, got)
		}
	}
}

func TestBetaBinomialUpdate(t *testing.T) {
	cases := []struct {
		alpha, beta         float64
		successes, trials   int
		wantAlpha, wantBeta float64
	}{
		// Uniform prior updated with 3 successes in 5 trials → Beta(4,3)
		{1, 1, 3, 5, 4, 3},
		// Informative prior
		{2, 3, 0, 10, 2, 13},
		// Zero trials: parameters unchanged
		{5, 5, 0, 0, 5, 5},
		// All successes
		{1, 1, 7, 7, 8, 1},
	}
	for _, c := range cases {
		gotA, gotB := BetaBinomialUpdate(c.alpha, c.beta, c.successes, c.trials)
		if !within(gotA, c.wantAlpha, 1e-15) || !within(gotB, c.wantBeta, 1e-15) {
			t.Errorf("BetaBinomialUpdate(%v,%v,%v,%v) = (%v,%v), want (%v,%v)",
				c.alpha, c.beta, c.successes, c.trials, gotA, gotB, c.wantAlpha, c.wantBeta)
		}
	}
}

func TestBetaBinomialUpdatePanicsOnInvalidInput(t *testing.T) {
	cases := []struct {
		name              string
		alpha, beta       float64
		successes, trials int
	}{
		{"non-positive alpha", 0, 1, 0, 1},
		{"infinite beta", 1, math.Inf(1), 0, 1},
		{"negative successes", 1, 1, -1, 1},
		{"successes exceed trials", 1, 1, 2, 1},
		{"negative trials", 1, 1, 0, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("BetaBinomialUpdate did not panic")
				}
			}()
			BetaBinomialUpdate(c.alpha, c.beta, c.successes, c.trials)
		})
	}
}

func TestDiscreteUniformMoments(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	minInt := -maxInt - 1
	cases := []struct {
		a, b              int
		wantMean, wantVar float64
	}{
		{0, 1, 0.5, 0.25},      // two points {0,1}
		{0, 5, 2.5, 35.0 / 12}, // six points {0..5}
		{1, 6, 3.5, 35.0 / 12}, // a fair die {1..6}: mean 3.5, variance 35/12
		{2, 2, 2, 0},           // single point: no spread
		{-3, 3, 0, 4},          // seven points {-3..3}: (7²−1)/12
		{0, 9, 4.5, 99.0 / 12}, // ten points {0..9}
		{maxInt, maxInt, float64(maxInt), 0},
		{minInt, minInt, float64(minInt), 0},
		{minInt, maxInt, -0.5, discreteUniformFullRangeVariance()},
	}
	for _, c := range cases {
		if got := DiscreteUniformMean(c.a, c.b); !within(got, c.wantMean, 1e-15) {
			t.Errorf("DiscreteUniformMean(%d, %d) = %v, want %v", c.a, c.b, got, c.wantMean)
		}
		if got := DiscreteUniformVariance(c.a, c.b); !within(got, c.wantVar, 1e-15) {
			t.Errorf("DiscreteUniformVariance(%d, %d) = %v, want %v", c.a, c.b, got, c.wantVar)
		}
	}

	// The variance depends only on the number of points, not the offset.
	if !within(DiscreteUniformVariance(0, 5), DiscreteUniformVariance(100, 105), 1e-15) {
		t.Error("DiscreteUniformVariance is not shift-invariant")
	}
}

func discreteUniformFullRangeVariance() float64 {
	// A signed int spans exactly 2^wordSize consecutive values.
	n := math.Ldexp(1, strconv.IntSize)
	return (n*n - 1) / 12
}
