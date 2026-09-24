package stats

import (
	"math"
	"strconv"
)

// Decision is the outcome of a sequential stopping rule at the current step.
type Decision int

const (
	// Continue means the evidence does not yet cross a stopping boundary.
	Continue Decision = iota
	// AcceptNull means the evidence supports stopping for the null hypothesis.
	AcceptNull
	// AcceptAlternative means the evidence supports stopping for the
	// alternative.
	AcceptAlternative
)

// String returns "Continue", "AcceptNull", or "AcceptAlternative". It formats
// an unknown value as "Decision(n)".
func (d Decision) String() string {
	switch d {
	case Continue:
		return "Continue"
	case AcceptNull:
		return "AcceptNull"
	case AcceptAlternative:
		return "AcceptAlternative"
	default:
		return "Decision(" + strconv.Itoa(int(d)) + ")"
	}
}

// SPRT runs Wald's sequential probability ratio test for a stream of Bernoulli
// observations under H0: p=p0 and H1: p=p1.
//
// The test uses Wald's (1945) approximate boundaries log((1-beta)/alpha) and
// log(beta/(1-alpha)). Ignoring boundary overshoot, Wald's inequalities bound
// the actual type-I error by alpha/(1-beta) and type-II error by
// beta/(1-alpha).
//
// Construct an SPRT with [NewSPRT]. Its zero value is invalid, and its methods
// panic when called on one. An SPRT may be copied when no method call is in
// progress; the copy is independent.
type SPRT struct {
	logA       float64 // upper boundary: log((1-beta)/alpha) → AcceptAlternative
	logB       float64 // lower boundary: log(beta/(1-alpha)) → AcceptNull
	llrPerHit  float64 // per-success increment: log(p1/p0)
	llrPerMiss float64 // per-failure increment: log((1-p1)/(1-p0))
	llr        float64
	n          int64
}

// NewSPRT returns a test for H0: p=p0 against H1: p=p1 with nominal errors
// alpha and beta. It panics unless p0 and p1 are distinct values in (0, 1),
// alpha and beta are in (0, 1), and alpha+beta is less than one.
func NewSPRT(p0, p1, alpha, beta float64) *SPRT {
	if !(p0 > 0 && p0 < 1) {
		panic("stats: NewSPRT: p0 must be in (0, 1)")
	}
	if !(p1 > 0 && p1 < 1) {
		panic("stats: NewSPRT: p1 must be in (0, 1)")
	}
	if p0 == p1 {
		panic("stats: NewSPRT: p0 and p1 must differ")
	}
	if !(alpha > 0 && alpha < 1) {
		panic("stats: NewSPRT: alpha must be in (0, 1)")
	}
	if !(beta > 0 && beta < 1) {
		panic("stats: NewSPRT: beta must be in (0, 1)")
	}
	if alpha+beta >= 1 {
		panic("stats: NewSPRT: alpha+beta must be less than 1")
	}
	success, failure := TwoPointLLRIncrements(p0, p1)
	return &SPRT{
		logA:       math.Log1p(-beta) - logFloat(alpha),
		logB:       logFloat(beta) - math.Log1p(-alpha),
		llrPerHit:  success,
		llrPerMiss: failure,
	}
}

// Observe incorporates one Bernoulli trial.
func (s *SPRT) Observe(success bool) {
	s.checkInitialised()
	s.n = incrementCount(s.n, "SPRT.Observe")
	if success {
		s.llr += s.llrPerHit
	} else {
		s.llr += s.llrPerMiss
	}
}

// Decision returns the current stopping decision. Continuing to observe after a
// terminal decision invalidates the test's error guarantees.
func (s *SPRT) Decision() Decision {
	s.checkInitialised()
	return s.decision()
}

func (s *SPRT) decision() Decision {
	switch {
	case s.llr >= s.logA:
		return AcceptAlternative
	case s.llr <= s.logB:
		return AcceptNull
	default:
		return Continue
	}
}

// LogLR returns the current log-likelihood ratio, or zero before any
// observations.
func (s *SPRT) LogLR() float64 {
	s.checkInitialised()
	return s.llr
}

// Count returns the number of observations incorporated so far.
func (s *SPRT) Count() int64 {
	s.checkInitialised()
	return s.n
}

func (s *SPRT) checkInitialised() {
	if !(s.logA > 0 && s.logB < 0) {
		panic("stats: SPRT: zero or invalid value")
	}
}

// TwoPointLLRIncrements returns the per-success and per-failure
// log-likelihood-ratio increments for the two-point Bernoulli hypotheses
// H0: p = p0 versus H1: p = p1.
//
//	perSuccess = log(p1 / p0)
//	perFailure = log((1 - p1) / (1 - p0))
//
// p0 and p1 must lie in (0, 1) and must differ; panics otherwise.
//
// Under H0 these increments make exp(LLR) a martingale. Multiplying one
// increment by a weight in [0, 1] preserves the supermartingale property by
// Jensen's inequality; see [WeightedLLR].
func TwoPointLLRIncrements(p0, p1 float64) (perSuccess, perFailure float64) {
	if !(p0 > 0 && p0 < 1) {
		panic("stats: TwoPointLLRIncrements: p0 must be in (0, 1)")
	}
	if !(p1 > 0 && p1 < 1) {
		panic("stats: TwoPointLLRIncrements: p1 must be in (0, 1)")
	}
	if p0 == p1 {
		panic("stats: TwoPointLLRIncrements: p0 and p1 must differ")
	}
	failure := logRatio(1-p1, 1-p0)
	if p0 < 0.5 && p1 < 0.5 {
		failure = math.Log1p(-p1) - math.Log1p(-p0)
	}
	return logRatio(p1, p0), failure
}

// logRatio avoids both ratio overflow and cancellation for nearby values.
func logRatio(numerator, denominator float64) float64 {
	delta := (numerator - denominator) / denominator
	if math.Abs(delta) < 0.5 {
		return math.Log1p(delta)
	}
	return logFloat(numerator) - logFloat(denominator)
}

// WeightedLLR accumulates a two-point Bernoulli log-likelihood ratio from
// weighted observations or bulk fractional counts.
//
// Hypotheses: H0: p = p0 versus H1: p = p1, with p0, p1 in (0, 1), p0 ≠ p1.
//
// Anytime-valid use with a Ville threshold requires every step delta to satisfy
// E_H0[exp(delta)] <= 1. A single observation with weight in [0, 1] satisfies
// this by Jensen's inequality. For aggregated steps passed to
// [WeightedLLR.Add], [LogWeightedExpSum] can check the condition.
//
// Construct a WeightedLLR with [NewWeightedLLR]. Its zero value is invalid,
// and its methods panic when called on one. A WeightedLLR may be copied when no
// method call is in progress; the copy is independent.
type WeightedLLR struct {
	perSuccess, perFailure float64
	llr                    float64
	n                      int64
}

// NewWeightedLLR returns an accumulator for H0: p=p0 against H1: p=p1. It
// panics unless p0 and p1 are distinct values in (0, 1).
func NewWeightedLLR(p0, p1 float64) *WeightedLLR {
	s, f := TwoPointLLRIncrements(p0, p1)
	return &WeightedLLR{perSuccess: s, perFailure: f}
}

// ObserveWeighted incorporates one observation and increments Count. It panics
// unless weight is finite and non-negative. Anytime-valid use requires weight
// in [0, 1]. A unit weight gives the unweighted SPRT increment.
func (s *WeightedLLR) ObserveWeighted(success bool, weight float64) {
	s.checkInitialised()
	if !finite(weight) || weight < 0 {
		panic("stats: WeightedLLR.ObserveWeighted: weight must be finite and non-negative")
	}
	s.n = incrementCount(s.n, "WeightedLLR.ObserveWeighted")
	if success {
		s.llr += weight * s.perSuccess
	} else {
		s.llr += weight * s.perFailure
	}
}

// Add applies finite, non-negative effective success and failure counts as one
// update step. It panics if either count is invalid.
func (s *WeightedLLR) Add(effSuccesses, effFailures float64) {
	s.checkInitialised()
	if !finite(effSuccesses) || effSuccesses < 0 ||
		!finite(effFailures) || effFailures < 0 {
		panic("stats: WeightedLLR.Add: effective counts must be finite and non-negative")
	}
	s.n = incrementCount(s.n, "WeightedLLR.Add")
	s.llr += effSuccesses*s.perSuccess + effFailures*s.perFailure
}

// LogLR returns the accumulated log-likelihood ratio.
func (s *WeightedLLR) LogLR() float64 {
	s.checkInitialised()
	return s.llr
}

// Count returns the number of update steps. Each call to [WeightedLLR.Add] or
// [WeightedLLR.ObserveWeighted] counts once, regardless of magnitude.
func (s *WeightedLLR) Count() int64 {
	s.checkInitialised()
	return s.n
}

func (s *WeightedLLR) checkInitialised() {
	if s.perSuccess == 0 || s.perFailure == 0 {
		panic("stats: WeightedLLR: zero or invalid value")
	}
}

// WaldUpperBoundary returns Wald's upper SPRT boundary log((1-beta)/alpha), at
// which an SPRT accepts the alternative under approximate type-I rate alpha
// and type-II rate beta.
//
// Ignoring boundary overshoot, Wald's (1945) inequalities bound the actual
// type-I error by alpha/(1-beta) and type-II error by beta/(1-alpha). Use
// [VilleThreshold] for exact anytime-valid type-I control.
//
// Panics unless alpha and beta lie in (0, 1) with alpha + beta < 1.
func WaldUpperBoundary(alpha, beta float64) float64 {
	checkWaldErrors(alpha, beta, "WaldUpperBoundary")
	return math.Log1p(-beta) - logFloat(alpha)
}

// WaldLowerBoundary returns Wald's lower SPRT boundary log(beta/(1-alpha)), at
// which an SPRT accepts the null. See [WaldUpperBoundary] for its error bounds.
//
// Panics unless alpha and beta lie in (0, 1) with alpha + beta < 1.
func WaldLowerBoundary(alpha, beta float64) float64 {
	checkWaldErrors(alpha, beta, "WaldLowerBoundary")
	return logFloat(beta) - math.Log1p(-alpha)
}

func checkWaldErrors(alpha, beta float64, name string) {
	if !(alpha > 0 && alpha < 1) {
		panic("stats: " + name + ": alpha must be in (0, 1)")
	}
	if !(beta > 0 && beta < 1) {
		panic("stats: " + name + ": beta must be in (0, 1)")
	}
	if alpha+beta >= 1 {
		panic("stats: " + name + ": alpha+beta must be less than 1")
	}
}

// VilleThreshold returns log(1/alpha), the log-evidence threshold for an
// e-process with initial value one and anytime-valid type-I error alpha.
//
// By Ville's inequality (1939), P(sup_t M_t >= 1/alpha) <= alpha for any
// non-negative supermartingale M with M_0=1, including under data-dependent
// stopping.
//
// Panics unless alpha is in (0, 1).
func VilleThreshold(alpha float64) float64 {
	if !(alpha > 0 && alpha < 1) {
		panic("stats: VilleThreshold: alpha must be in (0, 1)")
	}
	return -logFloat(alpha)
}

// VilleUnionThreshold returns log(n/alpha), the per-stream log-evidence
// threshold giving family-wise anytime-valid type-I error alpha across n
// e-processes initially equal to one.
//
// The guarantee permits arbitrary dependence among streams: their sum is a
// non-negative supermartingale initially equal to n, so any stream crossing
// n/alpha is contained in the sum's Ville event.
//
// Panics unless n >= 1 and alpha is in (0, 1).
func VilleUnionThreshold(n int, alpha float64) float64 {
	if n < 1 {
		panic("stats: VilleUnionThreshold: n must be at least 1")
	}
	if !(alpha > 0 && alpha < 1) {
		panic("stats: VilleUnionThreshold: alpha must be in (0, 1)")
	}
	return logFloat(float64(n)) - logFloat(alpha)
}

// LogWeightedExpSum returns log(sum_i weights[i]*exp(values[i])) using a stable
// log-sum-exp calculation.
//
// When weights is a probability distribution over increments, a result no
// greater than zero certifies E[exp(delta)] <= 1 for that step.
//
// LogWeightedExpSum panics unless values and weights have equal, non-empty
// lengths and the weights are finite, non-negative, and not all zero. It
// returns -Inf if every positive-weight value is -Inf and +Inf if any
// positive-weight value is +Inf. A positive-weight NaN yields NaN after all
// weights have been validated; zero-weight values are ignored. Neither slice
// is modified or retained.
func LogWeightedExpSum(values, weights []float64) float64 {
	if len(values) != len(weights) {
		panic("stats: LogWeightedExpSum: values and weights must have equal length")
	}
	if len(values) == 0 {
		panic("stats: LogWeightedExpSum: input must be non-empty")
	}
	maxV := math.Inf(-1)
	positiveWeight := false
	undefined := false
	for i, weight := range weights {
		if !finite(weight) || weight < 0 {
			panic("stats: LogWeightedExpSum: weights must be finite and non-negative")
		}
		if weight > 0 {
			positiveWeight = true
			undefined = undefined || math.IsNaN(values[i])
			maxV = math.Max(maxV, values[i]+logFloat(weight))
		}
	}
	if !positiveWeight {
		panic("stats: LogWeightedExpSum: at least one weight must be positive")
	}
	if undefined {
		return math.NaN()
	}
	if math.IsInf(maxV, 0) {
		return maxV
	}
	var sum float64
	for i, v := range values {
		if weights[i] > 0 {
			sum += math.Exp(v + logFloat(weights[i]) - maxV)
		}
	}
	return maxV + logFloat(sum)
}

// normalMixtureHalfWidth returns the half-width of the normal-mixture
// confidence sequence at sample size n, cumulative variance sum varSum, mixing
// constant rho, and significance level alpha.
//
// Formula: sqrt((varSum+rho) · (2·log(2/α) + log((varSum+rho)/rho))) / n
//
// This gives (1−α) two-sided coverage valid simultaneously at every n ≥ 1.
// See Howard et al. (2021), "Time-uniform, nonparametric, nonasymptotic
// confidence sequences", JRSS-B, Theorem 1. The factor 2/α accounts for the
// bound.
func normalMixtureHalfWidth(n, varSum, rho, alpha float64) float64 {
	vr := varSum + rho
	return math.Sqrt(vr*(2*(math.Ln2-logFloat(alpha))+logFloat(vr/rho))) / n
}

// BernoulliCS maintains an anytime-valid confidence sequence for the mean of a
// Bernoulli distribution. The interval returned by Interval is guaranteed to
// contain the true mean with probability at least 1−alpha simultaneously for
// all n ≥ 1.
//
// Uses the sub-Gaussian normal-mixture CS (Howard et al., 2021). A Bernoulli
// observation is 1/4-sub-Gaussian, so the cumulative variance proxy is n/4.
//
// Construct one with [NewBernoulliCS]. Its zero value is invalid, and its
// methods panic when called on one. A BernoulliCS may be copied when no method
// call is in progress; the copy is independent.
type BernoulliCS struct {
	alpha float64
	n     int64
	sum   float64 // cumulative successes
}

// NewBernoulliCS returns a sequence with significance level alpha. It panics
// unless alpha is in (0, 1).
func NewBernoulliCS(alpha float64) *BernoulliCS {
	if !(alpha > 0 && alpha < 1) {
		panic("stats: NewBernoulliCS: alpha must be in (0, 1)")
	}
	return &BernoulliCS{alpha: alpha}
}

// Observe incorporates one Bernoulli trial.
func (c *BernoulliCS) Observe(success bool) {
	c.checkInitialised()
	c.n = incrementCount(c.n, "BernoulliCS.Observe")
	if success {
		c.sum++
	}
}

// Count returns the number of observations incorporated so far.
func (c *BernoulliCS) Count() int64 {
	c.checkInitialised()
	return c.n
}

// Mean returns the empirical success rate, or NaN before the first observation.
func (c *BernoulliCS) Mean() float64 {
	c.checkInitialised()
	if c.n == 0 {
		return math.NaN()
	}
	return c.sum / float64(c.n)
}

// Interval returns the current anytime-valid confidence interval [lo, hi],
// clipped to [0, 1]. Returns (NaN, NaN) if no observations have been made.
func (c *BernoulliCS) Interval() (lo, hi float64) {
	c.checkInitialised()
	if c.n == 0 {
		return math.NaN(), math.NaN()
	}
	const rho = 0.25
	mean := c.sum / float64(c.n)
	n := float64(c.n)
	h := normalMixtureHalfWidth(n, n*rho, rho, c.alpha)
	return math.Max(0, mean-h), math.Min(1, mean+h)
}

func (c *BernoulliCS) checkInitialised() {
	if !(c.alpha > 0 && c.alpha < 1) {
		panic("stats: BernoulliCS: zero or invalid value")
	}
}

// BoundedMeanCS maintains an anytime-valid confidence sequence for the mean of
// a distribution supported on [a, b]. Observe rejects values outside [a, b].
//
// Uses the sub-Gaussian normal-mixture CS (Howard et al., 2021): every
// observation is treated as (b−a)²/4-sub-Gaussian, giving a conservative but
// universally valid bound for any distribution on [a, b].
//
// Construct one with [NewBoundedMeanCS]. Its zero value is invalid, and its
// methods panic when called on one. A BoundedMeanCS may be copied when no
// method call is in progress; the copy is independent. Observations are
// normalised to [0, 1] internally to avoid overflow and underflow in the
// variance proxy and cumulative sum.
type BoundedMeanCS struct {
	alpha float64
	a, b  float64
	n     int64
	mean  float64 // running mean after normalisation to [0, 1]
}

// NewBoundedMeanCS returns a BoundedMeanCS for observations in [a, b] with
// significance level alpha. Panics if alpha is not in (0, 1), if a >= b, or if
// a or b is not finite.
func NewBoundedMeanCS(a, b, alpha float64) *BoundedMeanCS {
	if !(alpha > 0 && alpha < 1) {
		panic("stats: NewBoundedMeanCS: alpha must be in (0, 1)")
	}
	if math.IsInf(a, 0) || math.IsInf(b, 0) || !(a < b) {
		panic("stats: NewBoundedMeanCS: bounds must be finite with a < b")
	}
	return &BoundedMeanCS{alpha: alpha, a: a, b: b}
}

// Observe incorporates x. It panics unless x is finite and lies in [a, b].
func (c *BoundedMeanCS) Observe(x float64) {
	c.checkInitialised()
	if !finite(x) || x < c.a || x > c.b {
		panic("stats: BoundedMeanCS.Observe: x must be finite and in [a, b]")
	}
	c.n = incrementCount(c.n, "BoundedMeanCS.Observe")
	u := (x - c.a) / (c.b - c.a)
	if math.IsInf(c.b-c.a, 1) {
		u = (x/2 - c.a/2) / (c.b/2 - c.a/2)
	}
	c.mean += (u - c.mean) / float64(c.n)
}

// Count returns the number of observations incorporated so far.
func (c *BoundedMeanCS) Count() int64 {
	c.checkInitialised()
	return c.n
}

// Mean returns the sample mean. Returns NaN if no observations have been made.
func (c *BoundedMeanCS) Mean() float64 {
	c.checkInitialised()
	if c.n == 0 {
		return math.NaN()
	}
	return c.fromUnit(c.mean)
}

// Interval returns the current anytime-valid confidence interval [lo, hi],
// clipped to [a, b]. Returns (NaN, NaN) if no observations have been made.
func (c *BoundedMeanCS) Interval() (lo, hi float64) {
	c.checkInitialised()
	if c.n == 0 {
		return math.NaN(), math.NaN()
	}
	const rho = 0.25
	n := float64(c.n)
	h := normalMixtureHalfWidth(n, n*rho, rho, c.alpha)
	return c.fromUnit(math.Max(0, c.mean-h)), c.fromUnit(math.Min(1, c.mean+h))
}

// fromUnit maps a convex combination back without overflowing the range.
func (c *BoundedMeanCS) fromUnit(u float64) float64 {
	if u == 0 {
		return c.a
	}
	if u == 1 {
		return c.b
	}
	if (c.a < 0) == (c.b < 0) {
		return c.a + u*(c.b-c.a)
	}
	return (1-u)*c.a + u*c.b
}

func (c *BoundedMeanCS) checkInitialised() {
	if !(c.a < c.b) || !(c.alpha > 0 && c.alpha < 1) {
		panic("stats: BoundedMeanCS: zero or invalid value")
	}
}
