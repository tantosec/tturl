package stats

import "math"

// Welford computes a running mean and sample variance in constant space using
// the Welford-West algorithm. Its zero value is an empty estimator. A Welford
// may be copied when no method call is in progress; the copy is independent.
type Welford struct {
	n    int64
	mean float64
	m2   float64
}

// Observe incorporates the finite observation x. It panics if x is not
// finite.
func (w *Welford) Observe(x float64) {
	if !finite(x) {
		panic("stats: Welford.Observe: x must be finite")
	}
	w.n = incrementCount(w.n, "Welford.Observe")
	delta := x - w.mean
	w.mean += delta / float64(w.n)
	w.m2 += delta * (x - w.mean)
}

// Count returns the number of observations made so far.
func (w *Welford) Count() int64 { return w.n }

// Mean returns the arithmetic mean, or NaN before the first observation.
func (w *Welford) Mean() float64 {
	if w.n == 0 {
		return math.NaN()
	}
	return w.mean
}

// Variance returns the Bessel-corrected sample variance, or NaN for fewer than
// two observations.
func (w *Welford) Variance() float64 {
	if w.n < 2 {
		return math.NaN()
	}
	return w.m2 / float64(w.n-1)
}

// StdDev returns the sample standard deviation, or NaN for fewer than two
// observations.
func (w *Welford) StdDev() float64 { return math.Sqrt(w.Variance()) }

// BernoulliMeter records a stream of Bernoulli trials. Its zero value is an
// empty meter. A BernoulliMeter may be copied when no method call is in
// progress; the copy is independent.
type BernoulliMeter struct {
	n         int64
	successes int64
}

// Observe records one Bernoulli trial.
func (m *BernoulliMeter) Observe(success bool) {
	m.n = incrementCount(m.n, "BernoulliMeter.Observe")
	if success {
		m.successes++
	}
}

// Count returns the total number of trials observed.
func (m *BernoulliMeter) Count() int64 { return m.n }

// Successes returns the number of successful trials.
func (m *BernoulliMeter) Successes() int64 { return m.successes }

// Rate returns the empirical success rate, or NaN before the first trial.
func (m *BernoulliMeter) Rate() float64 {
	if m.n == 0 {
		return math.NaN()
	}
	return float64(m.successes) / float64(m.n)
}

// EWMA maintains an exponentially weighted moving average and variance of a
// stream of observations. Each new observation x updates the mean μ and
// variance σ² by:
//
//	diff = x − μ
//	μ    = μ + α·diff
//	σ²   = (1−α)·(σ² + α·diff²)
//
// This is West's exponentially weighted form (West, 1979). The variance is
// biased for short streams and has no Bessel correction.
//
// The smoothing factor alpha in (0, 1] controls memory length; larger values
// give more weight to recent observations. Construct an EWMA with [NewEWMA].
// An EWMA may be copied when no method call is in progress; the copy is
// independent. Its zero value is invalid, and its methods panic when called on
// one.
type EWMA struct {
	alpha float64
	mean  float64
	vari  float64
	n     int64
}

// NewEWMA returns an estimator with smoothing factor alpha in (0, 1]. It panics
// when alpha is outside that range.
func NewEWMA(alpha float64) *EWMA {
	if !(alpha > 0 && alpha <= 1) {
		panic("stats: NewEWMA: alpha must be in (0, 1]")
	}
	return &EWMA{alpha: alpha}
}

// Observe incorporates the finite observation x into the running EWMA mean
// and variance. It panics if x is not finite.
func (e *EWMA) Observe(x float64) {
	e.checkInitialised()
	if !finite(x) {
		panic("stats: EWMA.Observe: x must be finite")
	}
	e.n = incrementCount(e.n, "EWMA.Observe")
	if e.n == 1 {
		e.mean = x
		return
	}
	diff := x - e.mean
	e.mean += e.alpha * diff
	e.vari = (1 - e.alpha) * (e.vari + e.alpha*diff*diff)
}

// Count returns the number of observations made so far.
func (e *EWMA) Count() int64 {
	e.checkInitialised()
	return e.n
}

// Mean returns the exponentially weighted moving average, or NaN before the
// first observation.
func (e *EWMA) Mean() float64 {
	e.checkInitialised()
	if e.n == 0 {
		return math.NaN()
	}
	return e.mean
}

// Variance returns the exponentially weighted moving variance, or NaN for
// fewer than two observations.
func (e *EWMA) Variance() float64 {
	e.checkInitialised()
	if e.n < 2 {
		return math.NaN()
	}
	return e.vari
}

// StdDev returns the square root of the exponentially weighted moving variance.
// Returns NaN for fewer than two observations.
func (e *EWMA) StdDev() float64 { return math.Sqrt(e.Variance()) }

func (e *EWMA) checkInitialised() {
	if !(e.alpha > 0 && e.alpha <= 1) {
		panic("stats: EWMA: zero or invalid value")
	}
}

func finite(x float64) bool {
	return !math.IsNaN(x) && !math.IsInf(x, 0)
}

func incrementCount(count int64, operation string) int64 {
	if count == int64(^uint64(0)>>1) {
		panic("stats: " + operation + ": observation count overflow")
	}
	return count + 1
}

// logFloat normalises positive subnormal values before taking their logarithm.
// This avoids losing exponent bits in architecture-specific math.Log paths.
func logFloat(x float64) float64 {
	if x > 0 && x < 0x1p-1022 {
		fraction, exponent := math.Frexp(x)
		return math.Log(fraction) + float64(exponent)*math.Ln2
	}
	return math.Log(x)
}
