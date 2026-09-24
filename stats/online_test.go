package stats

import (
	"math"
	"testing"
)

func TestObservationCountOverflowPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("incrementCount(MaxInt64) did not panic")
		}
	}()
	incrementCount(int64(^uint64(0)>>1), "test")
}

func TestWelfordEmpty(t *testing.T) {
	var w Welford
	if w.Count() != 0 {
		t.Errorf("Count = %d, want 0", w.Count())
	}
	if !math.IsNaN(w.Mean()) {
		t.Errorf("Mean = %v, want NaN", w.Mean())
	}
	if !math.IsNaN(w.Variance()) {
		t.Errorf("Variance = %v, want NaN", w.Variance())
	}
	if !math.IsNaN(w.StdDev()) {
		t.Errorf("StdDev = %v, want NaN", w.StdDev())
	}
}

func TestWelfordPanicsOnNonFiniteObservation(t *testing.T) {
	for _, observation := range []float64{math.NaN(), math.Inf(-1), math.Inf(1)} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Observe(%v) did not panic", observation)
				}
			}()
			new(Welford).Observe(observation)
		}()
	}
}

func TestWelfordSingle(t *testing.T) {
	var w Welford
	w.Observe(7)
	if w.Count() != 1 {
		t.Errorf("Count = %d, want 1", w.Count())
	}
	if !within(w.Mean(), 7, 0) {
		t.Errorf("Mean = %v, want 7", w.Mean())
	}
	if !math.IsNaN(w.Variance()) {
		t.Errorf("Variance = %v, want NaN (n=1)", w.Variance())
	}
	if !math.IsNaN(w.StdDev()) {
		t.Errorf("StdDev = %v, want NaN (n=1)", w.StdDev())
	}
}

func TestWelfordKnown(t *testing.T) {
	cases := []struct {
		name           string
		values         []float64
		mean, variance float64
	}{
		{"constant", []float64{42, 42}, 42, 0},
		{"ramp", []float64{1, 2, 3, 4, 5}, 3, 2.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var w Welford
			for _, x := range c.values {
				w.Observe(x)
			}
			if w.Count() != int64(len(c.values)) {
				t.Errorf("Count = %d, want %d", w.Count(), len(c.values))
			}
			if !within(w.Mean(), c.mean, 1e-14) {
				t.Errorf("Mean = %.15g, want %.15g", w.Mean(), c.mean)
			}
			if !within(w.Variance(), c.variance, 1e-14) {
				t.Errorf("Variance = %.15g, want %.15g", w.Variance(), c.variance)
			}
			if !within(w.StdDev(), math.Sqrt(c.variance), 1e-14) {
				t.Errorf("StdDev = %.15g, want %.15g",
					w.StdDev(), math.Sqrt(c.variance))
			}
		})
	}
}

func TestWelfordMatchesBrute(t *testing.T) {
	// Two-pass reference vs. Welford single-pass.
	data := []float64{3.1, 1.4, 1.5, 9.2, 6.5, 3.5, 8.9, 7.9, 3.2, 3.8}

	var w Welford
	for _, x := range data {
		w.Observe(x)
	}

	var sum float64
	for _, x := range data {
		sum += x
	}
	mean := sum / float64(len(data))
	var ss float64
	for _, x := range data {
		d := x - mean
		ss += d * d
	}
	variance := ss / float64(len(data)-1)

	if !within(w.Mean(), mean, 1e-13) {
		t.Errorf("Mean = %.15g, want %.15g", w.Mean(), mean)
	}
	if !within(w.Variance(), variance, 1e-13) {
		t.Errorf("Variance = %.15g, want %.15g", w.Variance(), variance)
	}
}

func TestWelfordLargeOffset(t *testing.T) {
	// Adding a large constant to all observations must not inflate variance.
	// Welford is numerically stable because it accumulates deviations from
	// a running mean, not squared raw values.
	const offset = 1e14
	var w Welford
	for _, x := range []float64{1, 2, 3, 4, 5} {
		w.Observe(offset + x)
	}
	if !within(w.Mean(), offset+3, 1e-6) {
		t.Errorf("Mean = %.15g, want %.15g", w.Mean(), offset+3)
	}
	// Variance is unaffected by the offset; only the deviations matter.
	if !within(w.Variance(), 2.5, 1e-6) {
		t.Errorf("Variance = %.15g, want 2.5", w.Variance())
	}
}

func TestBernoulliMeterEmpty(t *testing.T) {
	var m BernoulliMeter
	if m.Count() != 0 {
		t.Errorf("Count = %d, want 0", m.Count())
	}
	if m.Successes() != 0 {
		t.Errorf("Successes = %d, want 0", m.Successes())
	}
	if !math.IsNaN(m.Rate()) {
		t.Errorf("Rate = %v, want NaN", m.Rate())
	}
	lo, hi := WilsonInterval(int(m.Successes()), int(m.Count()), 1.96)
	if !math.IsNaN(lo) || !math.IsNaN(hi) {
		t.Errorf("WilsonInterval = (%v, %v), want (NaN, NaN)", lo, hi)
	}
}

func TestBernoulliMeterObservations(t *testing.T) {
	cases := []struct {
		name                string
		successes, failures int
		wantRate            float64
	}{
		{"all successes", 5, 0, 1},
		{"all failures", 0, 5, 0},
		{"mixed", 3, 7, 0.3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var m BernoulliMeter
			for range c.successes {
				m.Observe(true)
			}
			for range c.failures {
				m.Observe(false)
			}
			if got := m.Count(); got != int64(c.successes+c.failures) {
				t.Errorf("Count = %d, want %d", got, c.successes+c.failures)
			}
			if got := m.Successes(); got != int64(c.successes) {
				t.Errorf("Successes = %d, want %d", got, c.successes)
			}
			if got := m.Rate(); !within(got, c.wantRate, 1e-15) {
				t.Errorf("Rate = %.15g, want %.15g", got, c.wantRate)
			}
		})
	}
}

func TestEWMAInvalidAlpha(t *testing.T) {
	for _, alpha := range []float64{0, -0.1, 1.1, math.Inf(1), math.NaN()} {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("NewEWMA(%v) did not panic", alpha)
				}
			}()
			NewEWMA(alpha)
		}()
	}
	// alpha = 1 is valid (no memory: each observation replaces the last).
	if e := NewEWMA(1); e == nil {
		t.Error("NewEWMA(1) returned nil")
	}
}

func TestEWMAEmpty(t *testing.T) {
	e := NewEWMA(0.2)
	if e.Count() != 0 {
		t.Errorf("Count = %d, want 0", e.Count())
	}
	if !math.IsNaN(e.Mean()) {
		t.Errorf("Mean = %v, want NaN", e.Mean())
	}
	if !math.IsNaN(e.Variance()) {
		t.Errorf("Variance = %v, want NaN", e.Variance())
	}
	if !math.IsNaN(e.StdDev()) {
		t.Errorf("StdDev = %v, want NaN", e.StdDev())
	}
}

func TestEWMAPanicsOnNonFiniteObservation(t *testing.T) {
	for _, observation := range []float64{math.NaN(), math.Inf(-1), math.Inf(1)} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Observe(%v) did not panic", observation)
				}
			}()
			NewEWMA(0.5).Observe(observation)
		}()
	}
}

func TestEWMAKnown(t *testing.T) {
	// α=0.5, sequence [2, 4, 6].
	//
	// n=1: mean=2, vari=0 (undefined, returned as NaN)
	// n=2: diff=4-2=2, mean=2+0.5*2=3, vari=0.5*(0+0.5*4)=1
	// n=3: diff=6-3=3, mean=3+0.5*3=4.5, vari=0.5*(1+0.5*9)=2.75
	e := NewEWMA(0.5)
	e.Observe(2)
	if e.Count() != 1 || e.Mean() != 2 || !math.IsNaN(e.Variance()) {
		t.Errorf("after [2]: count/mean/variance = %d/%v/%v, want 1/2/NaN",
			e.Count(), e.Mean(), e.Variance())
	}
	e.Observe(4)
	if !within(e.Mean(), 3, 1e-15) {
		t.Errorf("after [2,4]: Mean = %.15g, want 3", e.Mean())
	}
	if !within(e.Variance(), 1, 1e-15) {
		t.Errorf("after [2,4]: Variance = %.15g, want 1", e.Variance())
	}
	e.Observe(6)
	if e.Count() != 3 {
		t.Errorf("Count = %d, want 3", e.Count())
	}
	if !within(e.Mean(), 4.5, 1e-14) {
		t.Errorf("after [2,4,6]: Mean = %.15g, want 4.5", e.Mean())
	}
	if !within(e.Variance(), 2.75, 1e-14) {
		t.Errorf("after [2,4,6]: Variance = %.15g, want 2.75", e.Variance())
	}
	if !within(e.StdDev(), math.Sqrt(2.75), 1e-14) {
		t.Errorf("after [2,4,6]: StdDev = %.15g, want %.15g", e.StdDev(), math.Sqrt(2.75))
	}
}

func TestEWMAConstantSequence(t *testing.T) {
	// A constant stream must produce mean = constant and variance = 0
	// regardless of alpha, since every diff is exactly 0.
	for _, alpha := range []float64{0.1, 0.5, 0.9, 1} {
		e := NewEWMA(alpha)
		for range 20 {
			e.Observe(7)
		}
		if !within(e.Mean(), 7, 1e-15) {
			t.Errorf("alpha=%.1f: Mean = %.15g, want 7", alpha, e.Mean())
		}
		if !within(e.Variance(), 0, 1e-15) {
			t.Errorf("alpha=%.1f: Variance = %.15g, want 0", alpha, e.Variance())
		}
	}
}

func TestEWMAAlphaOne(t *testing.T) {
	// α=1 gives zero memory: mean is always the latest observation.
	e := NewEWMA(1)
	e.Observe(10)
	e.Observe(20)
	e.Observe(30)
	if !within(e.Mean(), 30, 0) {
		t.Errorf("alpha=1: Mean = %v, want 30", e.Mean())
	}
	// Variance = (1−1)*(…) = 0 for all n≥2.
	if !within(e.Variance(), 0, 0) {
		t.Errorf("alpha=1: Variance = %v, want 0", e.Variance())
	}
}
