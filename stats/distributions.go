package stats

import "math"

const (
	sqrt2pi    = 2.5066282746310002 // math.Sqrt(2 * math.Pi)
	invSqrt2   = 0.7071067811865476 // 1.0 / math.Sqrt(2)
	logSqrt2pi = 0.9189385332046728 // 0.5 * logFloat(2*math.Pi)
)

// NormPDF returns the probability density at x for X ~ N(0, 1).
func NormPDF(x float64) float64 {
	return math.Exp(-0.5*x*x) / sqrt2pi
}

// NormLogPDF returns the log probability density at x for X ~ N(0, 1).
func NormLogPDF(x float64) float64 {
	return -0.5*x*x - logSqrt2pi
}

// NormCDF returns P(X ≤ x) for X ~ N(0, 1).
func NormCDF(x float64) float64 {
	return 0.5 * math.Erfc(-x*invSqrt2)
}

// Rational approximation coefficients from Acklam (2010).
var (
	acklamA = [6]float64{
		-3.969683028665376e+01, 2.209460984245205e+02, -2.759285104469687e+02,
		1.383577518672690e+02, -3.066479806614716e+01, 2.506628277459239e+00,
	}
	acklamB = [5]float64{
		-5.447609879822406e+01, 1.615858368580409e+02, -1.556989798598866e+02,
		6.680131188771972e+01, -1.328068155288572e+01,
	}
	acklamC = [6]float64{
		-7.784894002430293e-03, -3.223964580411365e-01, -2.400758277161838e+00,
		-2.549732539343734e+00, 4.374664141464968e+00, 2.938163982698783e+00,
	}
	acklamD = [4]float64{
		7.784695709041462e-03, 3.224671290700398e-01, 2.445134137142996e+00,
		3.754408661907416e+00,
	}
)

// NormQuantile returns x such that P(X <= x) = p for X ~ N(0, 1). It returns
// -Inf at zero, +Inf at one, and NaN outside [0, 1]. It uses Acklam's rational
// approximation.
func NormQuantile(p float64) float64 {
	// The `!(...)` form catches NaN: any comparison with NaN is false, so the
	// negation is true and we return NaN.
	if !(p >= 0 && p <= 1) {
		return math.NaN()
	}
	if p == 0 {
		return math.Inf(-1)
	}
	if p == 1 {
		return math.Inf(1)
	}

	const pLow = 0.02425
	a, b, c, d := &acklamA, &acklamB, &acklamC, &acklamD

	switch {
	case p < pLow:
		q := math.Sqrt(-2 * logFloat(p))
		return (((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	case p <= 1-pLow:
		q := p - 0.5
		r := q * q
		return (((((a[0]*r+a[1])*r+a[2])*r+a[3])*r+a[4])*r + a[5]) * q /
			(((((b[0]*r+b[1])*r+b[2])*r+b[3])*r+b[4])*r + 1)
	default:
		q := math.Sqrt(-2 * logFloat(1-p))
		return -(((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	}
}

// GammaPDF returns the Gamma(alpha, beta) density at x. At x = 0 it returns
// +Inf for alpha < 1, beta for alpha = 1, and zero for alpha > 1. It returns
// zero outside the support and NaN unless alpha and beta are finite and
// positive.
func GammaPDF(x, alpha, beta float64) float64 {
	if !positiveFinite(alpha) || !positiveFinite(beta) {
		return math.NaN()
	}
	if x < 0 {
		return 0
	}
	if x == 0 {
		switch {
		case alpha < 1:
			return math.Inf(1)
		case alpha == 1:
			return beta
		default:
			return 0
		}
	}
	if math.IsInf(x, 1) {
		return 0
	}
	return math.Exp(gammaLogPDFUnchecked(x, alpha, beta))
}

// GammaLogPDF returns the log Gamma(alpha, beta) density at x. Its behaviour at
// zero is the logarithm of GammaPDF's boundary value. It returns -Inf outside
// the support and NaN unless alpha and beta are finite and positive.
func GammaLogPDF(x, alpha, beta float64) float64 {
	if !positiveFinite(alpha) || !positiveFinite(beta) {
		return math.NaN()
	}
	if x < 0 {
		return math.Inf(-1)
	}
	if x == 0 {
		switch {
		case alpha < 1:
			return math.Inf(1)
		case alpha == 1:
			return logFloat(beta)
		default:
			return math.Inf(-1)
		}
	}
	if math.IsInf(x, 1) {
		return math.Inf(-1)
	}
	return gammaLogPDFUnchecked(x, alpha, beta)
}

func gammaLogPDFUnchecked(x, alpha, beta float64) float64 {
	lg, _ := math.Lgamma(alpha)
	return alpha*logFloat(beta) - lg + (alpha-1)*logFloat(x) - beta*x
}

// GammaCDF returns P(X <= x) for X ~ Gamma(alpha, beta). It returns zero for
// non-positive x and NaN unless alpha and beta are finite and positive.
// Its series and continued fraction stop at a relative term tolerance of
// 3e-10 or 300 iterations; extreme shapes can exhaust that limit, and
// overflowing intermediate arithmetic can yield NaN.
func GammaCDF(x, alpha, beta float64) float64 {
	if !positiveFinite(alpha) || !positiveFinite(beta) {
		return math.NaN()
	}
	if x <= 0 {
		return 0
	}
	if math.IsInf(x, 1) {
		return 1
	}
	scaled := beta * x
	if math.IsInf(scaled, 1) {
		return 1
	}
	return gammaIncReg(alpha, scaled)
}

// GammaMean returns the mean alpha/beta of Gamma(alpha, beta). It returns NaN
// unless alpha and beta are finite and positive.
func GammaMean(alpha, beta float64) float64 {
	if !positiveFinite(alpha) || !positiveFinite(beta) {
		return math.NaN()
	}
	return alpha / beta
}

// GammaVariance returns the variance alpha/(beta*beta) of Gamma(alpha, beta).
// It returns NaN unless alpha and beta are finite and positive.
func GammaVariance(alpha, beta float64) float64 {
	if !positiveFinite(alpha) || !positiveFinite(beta) {
		return math.NaN()
	}
	return alpha / (beta * beta)
}

// BetaPDF returns the Beta(alpha, beta) density at x. At either endpoint it
// returns the distribution's limiting density: +Inf when the adjacent shape
// is less than one, the opposite shape when it equals one, and zero when it is
// greater than one. It returns zero outside [0, 1] and NaN unless alpha and
// beta are finite and positive.
func BetaPDF(x, alpha, beta float64) float64 {
	if !positiveFinite(alpha) || !positiveFinite(beta) {
		return math.NaN()
	}
	if x < 0 || x > 1 {
		return 0
	}
	if x == 0 {
		return betaBoundaryDensity(alpha, beta)
	}
	if x == 1 {
		return betaBoundaryDensity(beta, alpha)
	}
	return math.Exp(betaLogPDFUnchecked(x, alpha, beta))
}

// BetaLogPDF returns the log Beta(alpha, beta) density at x. Its behaviour at
// each endpoint is the logarithm of BetaPDF's limiting density. It returns
// -Inf outside [0, 1] and NaN unless alpha and beta are finite and positive.
func BetaLogPDF(x, alpha, beta float64) float64 {
	if !positiveFinite(alpha) || !positiveFinite(beta) {
		return math.NaN()
	}
	if x < 0 || x > 1 {
		return math.Inf(-1)
	}
	if x == 0 {
		return betaBoundaryLogDensity(alpha, beta)
	}
	if x == 1 {
		return betaBoundaryLogDensity(beta, alpha)
	}
	return betaLogPDFUnchecked(x, alpha, beta)
}

func betaBoundaryDensity(adjacentShape, oppositeShape float64) float64 {
	switch {
	case adjacentShape < 1:
		return math.Inf(1)
	case adjacentShape == 1:
		return oppositeShape
	default:
		return 0
	}
}

func betaBoundaryLogDensity(adjacentShape, oppositeShape float64) float64 {
	switch {
	case adjacentShape < 1:
		return math.Inf(1)
	case adjacentShape == 1:
		return logFloat(oppositeShape)
	default:
		return math.Inf(-1)
	}
}

func betaLogPDFUnchecked(x, alpha, beta float64) float64 {
	lgA, _ := math.Lgamma(alpha)
	lgB, _ := math.Lgamma(beta)
	lgAB, _ := math.Lgamma(alpha + beta)
	return (lgAB - lgA - lgB) + (alpha-1)*logFloat(x) + (beta-1)*logFloat(1-x)
}

// BetaCDF returns P(X <= x) for X ~ Beta(alpha, beta). It returns zero when x
// is at most zero, one when x is at least one, and NaN unless alpha and beta
// are finite and positive. Its continued fraction stops at a relative term
// tolerance of 3e-10 or 300 iterations. Extreme shapes can exhaust that limit
// or overflow intermediate arithmetic; no uniform error bound is guaranteed.
func BetaCDF(x, alpha, beta float64) float64 {
	if !positiveFinite(alpha) || !positiveFinite(beta) {
		return math.NaN()
	}
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	return betaIncReg(x, alpha, beta)
}

// BetaQuantile returns x in [0, 1] such that BetaCDF(x, alpha, beta) = p. It
// returns the endpoints for p equal to zero or one, and NaN for p outside
// [0, 1] or non-positive or non-finite shape parameters.
//
// The implementation bisects the regularised incomplete beta function (DLMF
// 8.17). It inherits [BetaCDF]'s finite iteration limits and extreme-tail
// saturation. It does not guarantee a uniform error bound over all finite
// shape parameters and returns NaN if incomplete-beta evaluation yields NaN.
func BetaQuantile(p, alpha, beta float64) float64 {
	if !positiveFinite(alpha) || !positiveFinite(beta) {
		return math.NaN()
	}
	// The `!(...)` form catches NaN: any comparison with NaN is false, so the
	// negation is true and we return NaN.
	if !(p >= 0 && p <= 1) {
		return math.NaN()
	}
	if p == 0 {
		return 0
	}
	if p == 1 {
		return 1
	}

	// Invariant: I_lo(α,β) ≤ p ≤ I_hi(α,β), with lo < hi. It holds initially
	// because BetaCDF is increasing with BetaCDF(0) = 0 < p < 1 = BetaCDF(1),
	// and bisection preserves it. maxIter bounds the work even when a tail
	// needs more iterations to reach adjacent float64 values. betaIncReg
	// (not BetaCDF) is called directly because mid is always strictly inside
	// (0, 1), so the bound checks are moot.
	lo, hi := 0.0, 1.0
	const maxIter = 200
	for range maxIter {
		mid := 0.5 * (lo + hi)
		if mid <= lo || mid >= hi {
			return mid // bracket down to adjacent floats; cannot narrow further
		}
		cdf := betaIncReg(mid, alpha, beta)
		if math.IsNaN(cdf) {
			return math.NaN()
		}
		if cdf < p {
			lo = mid
		} else {
			hi = mid
		}
	}
	return 0.5 * (lo + hi)
}

// BetaMean returns alpha/(alpha+beta), the mean of Beta(alpha, beta). It
// returns NaN unless alpha and beta are finite and positive.
func BetaMean(alpha, beta float64) float64 {
	if !positiveFinite(alpha) || !positiveFinite(beta) {
		return math.NaN()
	}
	if alpha >= beta {
		return 1 / (1 + beta/alpha)
	}
	ratio := alpha / beta
	return ratio / (1 + ratio)
}

// BetaVariance returns the variance of Beta(alpha, beta). It returns NaN unless
// alpha and beta are finite and positive.
func BetaVariance(alpha, beta float64) float64 {
	if !positiveFinite(alpha) || !positiveFinite(beta) {
		return math.NaN()
	}
	mean := BetaMean(alpha, beta)
	return mean * (1 - mean) / (alpha + beta + 1)
}

// BernoulliPMF returns P(X = k) for X ~ Bernoulli(p). It returns zero when k is
// neither zero nor one, and NaN when p is outside [0, 1].
func BernoulliPMF(k int, p float64) float64 {
	if !(p >= 0 && p <= 1) {
		return math.NaN()
	}
	switch k {
	case 0:
		return 1 - p
	case 1:
		return p
	default:
		return 0
	}
}

// BernoulliLogPMF returns log P(X = k) for X ~ Bernoulli(p). It returns -Inf
// when k is neither zero nor one, and NaN when p is outside [0, 1].
func BernoulliLogPMF(k int, p float64) float64 {
	if !(p >= 0 && p <= 1) {
		return math.NaN()
	}
	switch k {
	case 0:
		return logFloat(1 - p)
	case 1:
		return logFloat(p)
	default:
		return math.Inf(-1)
	}
}

// BernoulliMean returns p, the mean of Bernoulli(p). It returns NaN when p is
// outside [0, 1].
func BernoulliMean(p float64) float64 {
	if !(p >= 0 && p <= 1) {
		return math.NaN()
	}
	return p
}

// BernoulliVariance returns the variance of Bernoulli(p). It returns NaN when
// p is outside [0, 1].
func BernoulliVariance(p float64) float64 {
	if !(p >= 0 && p <= 1) {
		return math.NaN()
	}
	return p * (1 - p)
}

// BinomialPMF returns P(X = k) for X ~ Binomial(n, p). It returns zero when k
// is outside [0, n], and NaN when n is negative or p is outside [0, 1].
func BinomialPMF(k, n int, p float64) float64 {
	if n < 0 || !(p >= 0 && p <= 1) {
		return math.NaN()
	}
	if k < 0 || k > n {
		return 0
	}
	return math.Exp(BinomialLogPMF(k, n, p))
}

// BinomialLogPMF returns log P(X = k) for X ~ Binomial(n, p). It returns -Inf
// when k is outside [0, n], and NaN when n is negative or p is outside [0, 1].
func BinomialLogPMF(k, n int, p float64) float64 {
	if n < 0 || !(p >= 0 && p <= 1) {
		return math.NaN()
	}
	if k < 0 || k > n {
		return math.Inf(-1)
	}
	if p == 0 {
		if k == 0 {
			return 0
		}
		return math.Inf(-1)
	}
	if p == 1 {
		if k == n {
			return 0
		}
		return math.Inf(-1)
	}
	return logBinomCoeff(n, k) + float64(k)*logFloat(p) + float64(n-k)*logFloat(1-p)
}

// BinomialMean returns n*p, the mean of Binomial(n, p). It returns NaN when n
// is negative or p is outside [0, 1].
func BinomialMean(n int, p float64) float64 {
	if n < 0 || !(p >= 0 && p <= 1) {
		return math.NaN()
	}
	return float64(n) * p
}

// BinomialVariance returns the variance of Binomial(n, p). It returns NaN when
// n is negative or p is outside [0, 1].
func BinomialVariance(n int, p float64) float64 {
	if n < 0 || !(p >= 0 && p <= 1) {
		return math.NaN()
	}
	return float64(n) * p * (1 - p)
}

// logBinomCoeff returns log C(n, k) using the log-gamma identity to avoid
// overflow for large n.
func logBinomCoeff(n, k int) float64 {
	lgN, _ := math.Lgamma(float64(n) + 1)
	lgK, _ := math.Lgamma(float64(k) + 1)
	lgNK, _ := math.Lgamma(float64(n-k) + 1)
	return lgN - lgK - lgNK
}

// BetaBinomialUpdate applies the conjugate Beta-Binomial update for independent
// Bernoulli observations. It panics unless alpha and beta are finite and
// positive and 0 <= successes <= trials.
func BetaBinomialUpdate(alpha, beta float64, successes, trials int) (postAlpha, postBeta float64) {
	if !positiveFinite(alpha) || !positiveFinite(beta) {
		panic("stats: BetaBinomialUpdate: alpha and beta must be finite and positive")
	}
	if successes < 0 || successes > trials {
		panic("stats: BetaBinomialUpdate: successes must be in [0, trials]")
	}
	return alpha + float64(successes), beta + float64(trials-successes)
}

// DiscreteUniformMean returns the mean (a+b)/2 of the discrete uniform
// distribution on the integers from a through b. It returns NaN if a > b.
func DiscreteUniformMean(a, b int) float64 {
	if a > b {
		return math.NaN()
	}
	// Same-sign endpoints use their non-negative distance; opposite-sign
	// endpoints have an overflow-safe sum. Carry the odd half separately so
	// exact cancellation remains visible before conversion to float64.
	if (a < 0) == (b < 0) {
		d := b - a
		return float64(a+d/2) + float64(d%2)/2
	}
	s := a + b
	return float64(s/2) + float64(s%2)/2
}

// DiscreteUniformVariance returns the variance (n²−1)/12 of the discrete
// uniform distribution on the integers from a through b, where n = b-a+1. It
// returns NaN if a > b.
func DiscreteUniformVariance(a, b int) float64 {
	if a > b {
		return math.NaN()
	}
	// Unsigned subtraction preserves the distance even across zero, before
	// float64 rounding can erase differences between large adjacent bounds.
	n := float64(uint(b)-uint(a)) + 1
	return (n*n - 1) / 12
}

func positiveFinite(x float64) bool {
	return x > 0 && !math.IsInf(x, 0)
}

// gammaIncReg returns the regularised lower incomplete gamma P(a, x).
func gammaIncReg(a, x float64) float64 {
	if x == 0 {
		return 0
	}
	if x < a+1 {
		return gammaIncSeries(a, x)
	}
	return 1 - gammaIncCF(a, x)
}

// gammaIncSeries computes P(a, x) via the series expansion.
func gammaIncSeries(a, x float64) float64 {
	const (
		maxIter = 300
		eps     = 3e-10
	)
	ap := a
	sum := 1.0 / a
	del := sum
	for range maxIter {
		ap++
		del *= x / ap
		sum += del
		if math.Abs(del) < math.Abs(sum)*eps {
			break
		}
	}
	lg, _ := math.Lgamma(a)
	return sum * math.Exp(-x+a*logFloat(x)-lg)
}

// gammaIncCF computes Q(a, x) = 1-P(a, x) using Lentz's continued fraction.
func gammaIncCF(a, x float64) float64 {
	const (
		maxIter = 300
		eps     = 3e-10
		fpmin   = 1e-300
	)
	b := x + 1 - a
	c := 1.0 / fpmin
	d := 1.0 / b
	h := d
	for i := 1; i <= maxIter; i++ {
		fi := float64(i)
		an := -fi * (fi - a)
		b += 2
		d = an*d + b
		if math.Abs(d) < fpmin {
			d = fpmin
		}
		c = b + an/c
		if math.Abs(c) < fpmin {
			c = fpmin
		}
		d = 1 / d
		del := d * c
		h *= del
		if math.Abs(del-1) < eps {
			break
		}
	}
	lg, _ := math.Lgamma(a)
	return math.Exp(-x+a*logFloat(x)-lg) * h
}

// betaIncReg returns the regularised incomplete beta I_x(a, b) using Lentz's
// continued fraction and a symmetry swap.
func betaIncReg(x, a, b float64) float64 {
	lgA, _ := math.Lgamma(a)
	lgB, _ := math.Lgamma(b)
	lgAB, _ := math.Lgamma(a + b)
	bt := math.Exp(a*logFloat(x) + b*logFloat(1-x) - (lgA + lgB - lgAB))
	if x < (a+1)/(a+b+2) {
		return bt * betaCF(a, b, x) / a
	}
	return 1 - bt*betaCF(b, a, 1-x)/b
}

// betaCF evaluates the continued fraction for the regularised incomplete beta
// via Lentz's method.
func betaCF(a, b, x float64) float64 {
	const (
		maxIter = 300
		eps     = 3e-10
		fpmin   = 1e-300
	)
	qab := a + b
	qap := a + 1
	qam := a - 1
	c := 1.0
	d := 1 - qab*x/qap
	if math.Abs(d) < fpmin {
		d = fpmin
	}
	d = 1 / d
	h := d
	for m := 1; m <= maxIter; m++ {
		m2 := float64(2 * m)
		mf := float64(m)
		// Even step.
		aa := mf * (b - mf) * x / ((qam + m2) * (a + m2))
		d = 1 + aa*d
		if math.Abs(d) < fpmin {
			d = fpmin
		}
		c = 1 + aa/c
		if math.Abs(c) < fpmin {
			c = fpmin
		}
		d = 1 / d
		h *= d * c
		// Odd step.
		aa = -(a + mf) * (qab + mf) * x / ((a + m2) * (qap + m2))
		d = 1 + aa*d
		if math.Abs(d) < fpmin {
			d = fpmin
		}
		c = 1 + aa/c
		if math.Abs(c) < fpmin {
			c = fpmin
		}
		d = 1 / d
		delta := d * c
		h *= delta
		if math.Abs(delta-1) < eps {
			break
		}
	}
	return h
}
