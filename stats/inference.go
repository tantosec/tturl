package stats

import "math"

// Alternative selects the sidedness of a hypothesis test.
type Alternative int

const (
	// TwoSided rejects for large |z| (H1: the effect is non-zero).
	TwoSided Alternative = iota
	// Greater rejects for large positive z (H1: the effect is positive).
	Greater
	// Less rejects for large negative z (H1: the effect is negative).
	Less
)

// NormalTailP returns the p-value for a statistic z that is standard normal
// under the null hypothesis, for the given Alternative:
//
//	Greater  → P(Z ≥ z)     = 1 − Φ(z)
//	Less     → P(Z ≤ z)     = Φ(z)
//	TwoSided → P(|Z| ≥ |z|) = 2(1 − Φ(|z|))
//
// It returns NaN if z is NaN or alt is unknown.
func NormalTailP(z float64, alt Alternative) float64 {
	switch alt {
	case Greater:
		return 0.5 * math.Erfc(z*invSqrt2)
	case Less:
		return 0.5 * math.Erfc(-z*invSqrt2)
	case TwoSided:
		return math.Erfc(math.Abs(z) * invSqrt2)
	default:
		return math.NaN()
	}
}

// ProportionZ returns the one-sample z statistic for an observed count of
// successes in n independent trials against the null proportion p0:
//
//	z = (p̂ − p0) / sqrt(p0(1−p0)/n),   p̂ = successes/n
//
// The standard error is evaluated at p0. Positive z means that the observed
// proportion exceeds p0.
//
// Returns NaN if n ≤ 0, successes is outside [0, n], or p0 is outside
// the open interval (0, 1) — the observation or null standard deviation is
// invalid.
func ProportionZ(successes, n int, p0 float64) float64 {
	if n <= 0 || successes < 0 || successes > n || !(p0 > 0 && p0 < 1) {
		return math.NaN()
	}
	phat := float64(successes) / float64(n)
	return (phat - p0) / math.Sqrt(p0*(1-p0)/float64(n))
}

// OneSampleZ returns the one-sample location test statistic
//
//	(mean − mu0) / (sd / √n),
//
// where sd is the known or estimated standard deviation. Positive results mean
// that mean exceeds mu0. The normal reference is exact for a known-variance
// z-test and asymptotic when sd is a sample estimate.
//
// OneSampleZ returns NaN when n is less than one or any numeric input is not
// finite or sd is negative. When sd is zero, it returns zero if mean equals mu0
// and otherwise returns a signed infinity.
func OneSampleZ(mean, mu0, sd float64, n int) float64 {
	if n < 1 || !finite(mean) || !finite(mu0) || !finite(sd) || sd < 0 {
		return math.NaN()
	}
	diff := mean - mu0
	if sd == 0 {
		if diff == 0 {
			return 0
		}
		return math.Inf(int(math.Copysign(1, diff)))
	}
	return diff / (sd / math.Sqrt(float64(n)))
}

// WilsonInterval returns the Wilson score interval for successes out of n
// Bernoulli trials at standard-normal critical value z. The interval lies in
// [0, 1]. It returns a NaN interval when n is non-positive, successes is
// outside [0, n], or z is negative or non-finite.
func WilsonInterval(successes, n int, z float64) (lo, hi float64) {
	if n <= 0 || successes < 0 || successes > n || !finite(z) || z < 0 {
		return math.NaN(), math.NaN()
	}
	k := float64(successes)
	nf := float64(n)
	z2 := z * z
	if math.IsInf(z2, 1) {
		return 0, 1
	}
	denom := nf + z2
	centre := k + z2/2
	spread := z * math.Sqrt(k*(nf-k)/nf+z2/4)
	return (centre - spread) / denom, (centre + spread) / denom
}

// ClopperPearson returns the exact Clopper-Pearson interval for successes out
// of n Bernoulli trials at confidence level. Its coverage is at least level for
// every true proportion, so it can be conservative (Clopper and Pearson,
// 1934).
//
// The interval is equal-tailed: the total error α = 1−level is split evenly,
// α/2 in each tail, giving the bounds as Beta quantiles (see [BetaQuantile])
//
//	lo = BetaQuantile(α/2,   k,   n−k+1)     (0 when k = 0)
//	hi = BetaQuantile(1−α/2, k+1, n−k)       (1 when k = n)
//
// where k is successes. The lower bound is zero when k is zero; the upper bound
// is one when k equals n. The numerical bounds inherit [BetaQuantile]'s
// approximation limits; exact coverage describes the mathematical construction.
//
// Returns (NaN, NaN) if n ≤ 0, successes is outside [0, n], or level is
// outside the open interval (0, 1).
func ClopperPearson(successes, n int, level float64) (lo, hi float64) {
	if n <= 0 || successes < 0 || successes > n || !(level > 0 && level < 1) {
		return math.NaN(), math.NaN()
	}
	alpha := 1 - level
	k := float64(successes)
	nf := float64(n)

	lo = 0
	if successes > 0 {
		lo = BetaQuantile(alpha/2, k, nf-k+1)
	}
	hi = 1
	if successes < n {
		hi = BetaQuantile(1-alpha/2, k+1, nf-k)
	}
	return lo, hi
}

// CohenH returns Cohen's h for two proportions using the variance-stabilising
// arcsine transform:
//
//	h = 2·arcsin(√p1) − 2·arcsin(√p2)
//
// Its sign follows p1-p2. It returns NaN if either proportion is outside
// [0, 1]. See Cohen (1988).
func CohenH(p1, p2 float64) float64 {
	if !(p1 >= 0 && p1 <= 1) || !(p2 >= 0 && p2 <= 1) {
		return math.NaN()
	}
	return 2*math.Asin(math.Sqrt(p1)) - 2*math.Asin(math.Sqrt(p2))
}
