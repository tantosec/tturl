package stats

import (
	"math"
	"testing"
)

func TestDecisionString(t *testing.T) {
	cases := []struct {
		d    Decision
		want string
	}{
		{Continue, "Continue"},
		{AcceptNull, "AcceptNull"},
		{AcceptAlternative, "AcceptAlternative"},
		{Decision(99), "Decision(99)"},
		{Decision(-1), "Decision(-1)"},
	}
	for _, c := range cases {
		if got := c.d.String(); got != c.want {
			t.Errorf("Decision(%d).String() = %q, want %q", int(c.d), got, c.want)
		}
	}
}

func TestSPRTPanics(t *testing.T) {
	valid := func() { NewSPRT(0.1, 0.9, 0.05, 0.05) }
	valid() // must not panic

	cases := []struct {
		name string
		fn   func()
	}{
		{"p0=0", func() { NewSPRT(0, 0.9, 0.05, 0.05) }},
		{"p0=1", func() { NewSPRT(1, 0.9, 0.05, 0.05) }},
		{"p0<0", func() { NewSPRT(-0.1, 0.9, 0.05, 0.05) }},
		{"p1=0", func() { NewSPRT(0.1, 0, 0.05, 0.05) }},
		{"p1=1", func() { NewSPRT(0.1, 1, 0.05, 0.05) }},
		{"p0==p1", func() { NewSPRT(0.5, 0.5, 0.05, 0.05) }},
		{"alpha=0", func() { NewSPRT(0.1, 0.9, 0, 0.05) }},
		{"alpha=1", func() { NewSPRT(0.1, 0.9, 1, 0.05) }},
		{"beta=0", func() { NewSPRT(0.1, 0.9, 0.05, 0) }},
		{"beta=1", func() { NewSPRT(0.1, 0.9, 0.05, 1) }},
		{"alpha+beta=1", func() { NewSPRT(0.1, 0.9, 0.5, 0.5) }},
		{"alpha+beta>1", func() { NewSPRT(0.1, 0.9, 0.6, 0.5) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("NewSPRT(%s) did not panic", c.name)
				}
			}()
			c.fn()
		})
	}
}

func TestSPRTInitialState(t *testing.T) {
	s := NewSPRT(0.1, 0.9, 0.05, 0.05)
	if s.Count() != 0 {
		t.Errorf("Count = %d, want 0", s.Count())
	}
	if s.LogLR() != 0 {
		t.Errorf("LogLR = %v, want 0", s.LogLR())
	}
	if s.Decision() != Continue {
		t.Errorf("Decision = %v, want Continue", s.Decision())
	}
}

func TestSPRTLogLRIncrements(t *testing.T) {
	// p0=0.1, p1=0.5:
	//   per success: log(0.5/0.1) = log(5)           ≈ 1.6094
	//   per failure: log(0.5/0.9) = log(5/9)         ≈ -0.5878
	s := NewSPRT(0.1, 0.5, 0.05, 0.05)
	wantHit := math.Log(5.0)
	wantMiss := math.Log(5.0 / 9.0)

	s.Observe(true)
	if !within(s.LogLR(), wantHit, 1e-14) {
		t.Errorf("after 1 success: LogLR = %.15g, want %.15g", s.LogLR(), wantHit)
	}
	s.Observe(false)
	if !within(s.LogLR(), wantHit+wantMiss, 1e-14) {
		t.Errorf("after success+failure: LogLR = %.15g, want %.15g", s.LogLR(), wantHit+wantMiss)
	}
	if s.Count() != 2 {
		t.Errorf("Count = %d, want 2", s.Count())
	}
}

func TestSPRTDecisions(t *testing.T) {
	cases := []struct {
		name        string
		p0, p1      float64
		observation bool
		want        Decision
	}{
		{"alternative", 0.1, 0.5, true, AcceptAlternative},
		{"null", 0.5, 0.9, false, AcceptNull},
		{"swapped hypotheses", 0.9, 0.1, true, AcceptNull},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewSPRT(c.p0, c.p1, 0.05, 0.05)
			s.Observe(c.observation)
			if s.Decision() != Continue {
				t.Errorf("after one observation: Decision = %v, want Continue",
					s.Decision())
			}
			s.Observe(c.observation)
			if s.Decision() != c.want {
				t.Errorf("after two observations: Decision = %v, want %v",
					s.Decision(), c.want)
			}
		})
	}
}

func TestSPRTExactBoundary(t *testing.T) {
	// Direct state placement isolates the inclusive threshold comparisons from
	// the observation increments that are covered separately.
	s := NewSPRT(0.1, 0.5, 0.05, 0.05)
	s.llr = s.logA
	if s.Decision() != AcceptAlternative {
		t.Errorf("at llr == logA: Decision = %v, want AcceptAlternative", s.Decision())
	}
	// Manually set llr to logB.
	s.llr = s.logB
	if s.Decision() != AcceptNull {
		t.Errorf("at llr == logB: Decision = %v, want AcceptNull", s.Decision())
	}
	// logA - epsilon → Continue.
	s.llr = s.logA - 1e-10
	if s.Decision() != Continue {
		t.Errorf("at llr just below logA: Decision = %v, want Continue", s.Decision())
	}
}

func TestNormalMixtureHalfWidthKnown(t *testing.T) {
	// n=100, varSum=25 (= n*rho for rho=0.25), rho=0.25, alpha=0.05.
	//
	// vr = 25 + 0.25 = 25.25
	// inner = 2*log(2/0.05) + log(25.25/0.25)
	//       = 2*log(40) + log(101)
	//       = 7.37775890822... + 4.61512066704...
	//       = 11.99287957527...
	// halfWidth = sqrt(25.25 * 11.99287957527) / 100
	//           = sqrt(302.8552....) / 100
	//           = 17.4028..... / 100
	//           = 0.174028....
	got := normalMixtureHalfWidth(100, 25, 0.25, 0.05)
	vr := 25.25
	inner := 2*math.Log(40) + math.Log(101)
	want := math.Sqrt(vr*inner) / 100
	if !within(got, want, 1e-14) {
		t.Errorf("got %.15g, want %.15g", got, want)
	}
}

func TestBernoulliCSPanics(t *testing.T) {
	for _, alpha := range []float64{0, 1, -0.1, 1.1, math.NaN()} {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("NewBernoulliCS(%v) did not panic", alpha)
				}
			}()
			NewBernoulliCS(alpha)
		}()
	}
}

func FuzzBernoulliCSMatchesBoundedMeanCS(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 1, 1, 0})
	f.Add([]byte{1})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 256 {
			t.Skip()
		}
		bernoulli := NewBernoulliCS(0.05)
		bounded := NewBoundedMeanCS(0, 1, 0.05)
		check := func(step int) {
			t.Helper()
			gotLo, gotHi := bernoulli.Interval()
			wantLo, wantHi := bounded.Interval()
			meansMatch := bernoulli.Mean() == bounded.Mean() ||
				(math.IsNaN(bernoulli.Mean()) && math.IsNaN(bounded.Mean()))
			intervalsMatch := gotLo == wantLo && gotHi == wantHi ||
				math.IsNaN(gotLo) && math.IsNaN(gotHi) &&
					math.IsNaN(wantLo) && math.IsNaN(wantHi)
			if bernoulli.Count() != bounded.Count() || !meansMatch || !intervalsMatch {
				t.Errorf("step %d: Bernoulli %d/%v/[%v,%v], bounded %d/%v/[%v,%v]",
					step, bernoulli.Count(), bernoulli.Mean(), gotLo, gotHi,
					bounded.Count(), bounded.Mean(), wantLo, wantHi)
			}
		}
		check(0)
		for i, value := range data {
			observation := value&1 == 1
			bernoulli.Observe(observation)
			bounded.Observe(float64(value & 1))
			check(i + 1)
		}
	})
}

func TestBoundedMeanCSPanics(t *testing.T) {
	for _, alpha := range []float64{0, 1, -0.1, 1.1, math.NaN()} {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("NewBoundedMeanCS(0,1,%v) did not panic", alpha)
				}
			}()
			NewBoundedMeanCS(0, 1, alpha)
		}()
	}
	for _, ab := range [][2]float64{
		{1, 0},
		{0.5, 0.5},
		{1, 1},
		{math.NaN(), 1},
		{0, math.NaN()},
		{math.Inf(-1), 1},
		{0, math.Inf(1)},
	} {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("NewBoundedMeanCS(%v,%v,0.05) did not panic", ab[0], ab[1])
				}
			}()
			NewBoundedMeanCS(ab[0], ab[1], 0.05)
		}()
	}
}

func TestBoundedMeanCSEmpty(t *testing.T) {
	c := NewBoundedMeanCS(0, 1, 0.05)
	if c.Count() != 0 {
		t.Errorf("Count = %d, want 0", c.Count())
	}
	if !math.IsNaN(c.Mean()) {
		t.Errorf("Mean = %v, want NaN", c.Mean())
	}
	lo, hi := c.Interval()
	if !math.IsNaN(lo) || !math.IsNaN(hi) {
		t.Errorf("Interval = (%v, %v), want (NaN, NaN)", lo, hi)
	}
}

func TestBoundedMeanCSPanicsOnInvalidObservation(t *testing.T) {
	for _, observation := range []float64{-0.1, 1.1, math.NaN(), math.Inf(1)} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Observe(%v) did not panic", observation)
				}
			}()
			NewBoundedMeanCS(0, 1, 0.05).Observe(observation)
		}()
	}
}

func TestBoundedMeanCSKnownHalfWidth(t *testing.T) {
	// n=100, all observations = 0.5, a=0, b=1, alpha=0.05.
	//
	// rho = (1-0)^2/4 = 0.25
	// V_n = 100 * 0.25 = 25
	// vr = 25.25
	// inner = 2*log(2/0.05) + log(25.25/0.25) = 2*log(40) + log(101)
	// halfWidth = sqrt(25.25 * inner) / 100
	c := NewBoundedMeanCS(0, 1, 0.05)
	for range 100 {
		c.Observe(0.5)
	}
	const rho = 0.25
	n := 100.0
	varSum := n * rho
	wantH := normalMixtureHalfWidth(n, varSum, rho, 0.05)

	lo, hi := c.Interval()
	if !within(lo, 0.5-wantH, 1e-14) {
		t.Errorf("lo = %.15g, want %.15g", lo, 0.5-wantH)
	}
	if !within(hi, 0.5+wantH, 1e-14) {
		t.Errorf("hi = %.15g, want %.15g", hi, 0.5+wantH)
	}
	// Verify numeric value (half-width ≈ 0.174028).
	if !within(wantH, 0.17402, 1e-5) {
		t.Errorf("expected half-width ~= 0.174, got %.6f", wantH)
	}
}

func TestBoundedMeanCSIntervalStaysInBounds(t *testing.T) {
	c := NewBoundedMeanCS(-3, 7, 0.05)
	for range 5 {
		c.Observe(5.0) // near upper end — would clip without clamping
	}
	lo, hi := c.Interval()
	if lo < -3-1e-14 {
		t.Errorf("lo = %.15g below a=-3", lo)
	}
	if hi > 7+1e-14 {
		t.Errorf("hi = %.15g above b=7", hi)
	}
}

func TestBoundedMeanCSWidthShrinksWithN(t *testing.T) {
	c := NewBoundedMeanCS(0, 10, 0.05)
	prevWidth := math.Inf(1)
	for range 300 {
		c.Observe(5.0)
		lo, hi := c.Interval()
		w := hi - lo
		if w > prevWidth+1e-10 {
			t.Errorf("width grew at n=%d: %.6f > %.6f", c.Count(), w, prevWidth)
		}
		prevWidth = w
	}
}

func TestBoundedMeanCSNonUnitInterval(t *testing.T) {
	// The unit-interval construction scales back to [2, 8].
	c := NewBoundedMeanCS(2, 8, 0.1)
	for range 50 {
		c.Observe(5.0)
	}
	if c.Count() != 50 {
		t.Errorf("Count = %d, want 50", c.Count())
	}
	lo, hi := c.Interval()
	if lo < 2-1e-14 || hi > 8+1e-14 {
		t.Errorf("interval [%.6f, %.6f] outside [2, 8]", lo, hi)
	}
	if !within(c.Mean(), 5, 1e-12) {
		t.Errorf("Mean = %.15g, want 5", c.Mean())
	}
}

func TestTwoPointLLRIncrementsPanics(t *testing.T) {
	cases := []struct {
		name   string
		p0, p1 float64
	}{
		{"p0=0", 0, 0.5},
		{"p0=1", 1, 0.5},
		{"p0<0", -0.1, 0.5},
		{"p0>1", 1.1, 0.5},
		{"p0=NaN", math.NaN(), 0.5},
		{"p1=0", 0.5, 0},
		{"p1=1", 0.5, 1},
		{"p0==p1", 0.4, 0.4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("TwoPointLLRIncrements(%v, %v) did not panic", c.p0, c.p1)
				}
			}()
			TwoPointLLRIncrements(c.p0, c.p1)
		})
	}
}

func TestTwoPointLLRIncrementsExact(t *testing.T) {
	// p0=0.1, p1=0.5:
	//   perSuccess = log(0.5/0.1) = log(5)
	//   perFailure = log(0.5/0.9)
	s, f := TwoPointLLRIncrements(0.1, 0.5)
	if !within(s, math.Log(5), 1e-15) {
		t.Errorf("perSuccess = %.15g, want log(5) = %.15g", s, math.Log(5))
	}
	if !within(f, math.Log(0.5/0.9), 1e-15) {
		t.Errorf("perFailure = %.15g, want log(0.5/0.9) = %.15g", f, math.Log(0.5/0.9))
	}
}

func TestTwoPointLLRIncrementsSignSwap(t *testing.T) {
	// Swapping p0 ↔ p1 negates both increments.
	s1, f1 := TwoPointLLRIncrements(0.2, 0.7)
	s2, f2 := TwoPointLLRIncrements(0.7, 0.2)
	if !within(s2, -s1, 1e-15) {
		t.Errorf("swap: perSuccess = %.15g, want %.15g", s2, -s1)
	}
	if !within(f2, -f1, 1e-15) {
		t.Errorf("swap: perFailure = %.15g, want %.15g", f2, -f1)
	}
}

func TestTwoPointLLRIncrementsH0Martingale(t *testing.T) {
	// Under H0: E_{p0}[exp(Δ)] = p0·exp(perSuccess) + (1-p0)·exp(perFailure)
	//         = p0·(p1/p0) + (1-p0)·((1-p1)/(1-p0)) = p1 + (1-p1) = 1.
	// This is the martingale property that authorises Ville-style thresholds.
	for _, c := range []struct{ p0, p1 float64 }{
		{0.1, 0.5},
		{0.3, 0.8},
		{0.5, 0.51},
		{0.9, 0.1},
	} {
		s, f := TwoPointLLRIncrements(c.p0, c.p1)
		got := c.p0*math.Exp(s) + (1-c.p0)*math.Exp(f)
		if !within(got, 1, 1e-14) {
			t.Errorf("p0=%v p1=%v: E[exp(delta)] under H0 = %.15g, want 1", c.p0, c.p1, got)
		}
	}
}

func TestWeightedLLRInitialState(t *testing.T) {
	s := NewWeightedLLR(0.1, 0.5)
	if s.Count() != 0 {
		t.Errorf("Count = %d, want 0", s.Count())
	}
	if s.LogLR() != 0 {
		t.Errorf("LogLR = %v, want 0", s.LogLR())
	}
}

func TestWeightedLLRPanicsOnInvalidProbs(t *testing.T) {
	// Same domain rules as TwoPointLLRIncrements.
	cases := []struct {
		name   string
		p0, p1 float64
	}{
		{"p0=0", 0, 0.5},
		{"p1=1", 0.5, 1},
		{"p0==p1", 0.4, 0.4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("NewWeightedLLR(%v, %v) did not panic", c.p0, c.p1)
				}
			}()
			NewWeightedLLR(c.p0, c.p1)
		})
	}
}

func TestWeightedLLRUnitWeightMatchesSPRT(t *testing.T) {
	// With weight=1 booleans, WeightedLLR must produce the same LLR as SPRT
	// observation-by-observation.
	const p0, p1 = 0.2, 0.7
	sprt := NewSPRT(p0, p1, 0.05, 0.05)
	wl := NewWeightedLLR(p0, p1)

	pattern := []bool{true, false, true, true, false, true, false, false, true}
	for _, b := range pattern {
		sprt.Observe(b)
		wl.ObserveWeighted(b, 1)
		if !within(wl.LogLR(), sprt.LogLR(), 1e-14) {
			t.Fatalf("LLR diverged: SPRT=%.15g, WeightedLLR=%.15g", sprt.LogLR(), wl.LogLR())
		}
	}
	if wl.Count() != int64(len(pattern)) {
		t.Errorf("Count = %d, want %d", wl.Count(), len(pattern))
	}
}

func TestWeightedLLRZeroWeightNoLLRChange(t *testing.T) {
	// weight=0 must not move LLR but still counts as a step.
	wl := NewWeightedLLR(0.2, 0.7)
	wl.ObserveWeighted(true, 0)
	wl.ObserveWeighted(false, 0)
	if wl.LogLR() != 0 {
		t.Errorf("LogLR = %.15g after zero-weighted updates, want 0", wl.LogLR())
	}
	if wl.Count() != 2 {
		t.Errorf("Count = %d, want 2", wl.Count())
	}
}

func TestWeightedLLRPanicsOnInvalidUpdates(t *testing.T) {
	for _, update := range []struct {
		name string
		fn   func(*WeightedLLR)
	}{
		{"negative weight", func(s *WeightedLLR) { s.ObserveWeighted(true, -0.1) }},
		{"NaN weight", func(s *WeightedLLR) { s.ObserveWeighted(true, math.NaN()) }},
		{"infinite weight", func(s *WeightedLLR) { s.ObserveWeighted(true, math.Inf(1)) }},
		{"negative successes", func(s *WeightedLLR) { s.Add(-0.1, 0) }},
		{"negative failures", func(s *WeightedLLR) { s.Add(0, -0.1) }},
		{"NaN successes", func(s *WeightedLLR) { s.Add(math.NaN(), 0) }},
		{"infinite failures", func(s *WeightedLLR) { s.Add(0, math.Inf(1)) }},
	} {
		t.Run(update.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("invalid update did not panic")
				}
			}()
			update.fn(NewWeightedLLR(0.2, 0.7))
		})
	}
}

func TestWeightedLLRLinearityInWeight(t *testing.T) {
	// Two successes at weight 0.5 give the same LLR contribution as one
	// success at weight 1.
	a := NewWeightedLLR(0.2, 0.7)
	a.ObserveWeighted(true, 1)
	b := NewWeightedLLR(0.2, 0.7)
	b.ObserveWeighted(true, 0.5)
	b.ObserveWeighted(true, 0.5)
	if !within(a.LogLR(), b.LogLR(), 1e-14) {
		t.Errorf("linearity: a=%.15g, b=%.15g", a.LogLR(), b.LogLR())
	}
}

func TestWeightedLLRAddMatchesIncrementalSum(t *testing.T) {
	// Add(effS, effF) must equal effS·perSuccess + effF·perFailure.
	const p0, p1 = 0.3, 0.6
	pS, pF := TwoPointLLRIncrements(p0, p1)

	cases := []struct{ effS, effF float64 }{
		{0, 0},
		{1.5, 0},
		{0, 2.25},
		{1.5, 2.25},
		{0.1, 0.9},
	}
	for _, c := range cases {
		wl := NewWeightedLLR(p0, p1)
		wl.Add(c.effS, c.effF)
		want := c.effS*pS + c.effF*pF
		if !within(wl.LogLR(), want, 1e-14) {
			t.Errorf("Add(%v,%v): LogLR=%.15g, want %.15g", c.effS, c.effF, wl.LogLR(), want)
		}
		if wl.Count() != 1 {
			t.Errorf("Add(%v,%v): Count=%d, want 1", c.effS, c.effF, wl.Count())
		}
	}
}

func TestWaldBoundariesPanics(t *testing.T) {
	cases := []struct {
		name        string
		alpha, beta float64
	}{
		{"alpha=0", 0, 0.05},
		{"alpha=1", 1, 0.05},
		{"beta=0", 0.05, 0},
		{"beta=1", 0.05, 1},
		{"alpha+beta=1", 0.5, 0.5},
		{"alpha+beta>1", 0.6, 0.5},
		{"alpha=NaN", math.NaN(), 0.05},
	}
	for _, c := range cases {
		t.Run(c.name+"/upper", func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("WaldUpperBoundary(%v, %v) did not panic", c.alpha, c.beta)
				}
			}()
			WaldUpperBoundary(c.alpha, c.beta)
		})
		t.Run(c.name+"/lower", func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("WaldLowerBoundary(%v, %v) did not panic", c.alpha, c.beta)
				}
			}()
			WaldLowerBoundary(c.alpha, c.beta)
		})
	}
}

func TestWaldBoundariesExact(t *testing.T) {
	// alpha=0.05, beta=0.1:
	//   upper = log(0.9/0.05) = log(18)
	//   lower = log(0.1/0.95) = log(2/19)
	u := WaldUpperBoundary(0.05, 0.1)
	l := WaldLowerBoundary(0.05, 0.1)
	if !within(u, math.Log(18), 1e-15) {
		t.Errorf("upper = %.15g, want log(18) = %.15g", u, math.Log(18))
	}
	if !within(l, math.Log(2.0/19), 1e-15) {
		t.Errorf("lower = %.15g, want log(2/19) = %.15g", l, math.Log(2.0/19))
	}
}

func TestWaldBoundariesMatchSPRT(t *testing.T) {
	// The exposed boundaries must match the values SPRT uses internally.
	const alpha, beta = 0.05, 0.05
	s := NewSPRT(0.1, 0.9, alpha, beta)
	if !within(WaldUpperBoundary(alpha, beta), s.logA, 1e-15) {
		t.Errorf("WaldUpperBoundary = %.15g, SPRT.logA = %.15g", WaldUpperBoundary(alpha, beta), s.logA)
	}
	if !within(WaldLowerBoundary(alpha, beta), s.logB, 1e-15) {
		t.Errorf("WaldLowerBoundary = %.15g, SPRT.logB = %.15g", WaldLowerBoundary(alpha, beta), s.logB)
	}
}

func TestWaldBoundariesSymmetricWhenAlphaEqBeta(t *testing.T) {
	// For alpha = beta, upper = -lower.
	for _, a := range []float64{0.01, 0.05, 0.1, 0.25} {
		u := WaldUpperBoundary(a, a)
		l := WaldLowerBoundary(a, a)
		if !within(u, -l, 1e-15) {
			t.Errorf("alpha=beta=%v: upper=%.15g, -lower=%.15g", a, u, -l)
		}
	}
}

func TestVilleThresholdPanics(t *testing.T) {
	for _, a := range []float64{0, 1, -0.1, 1.1, math.NaN()} {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("VilleThreshold(%v) did not panic", a)
				}
			}()
			VilleThreshold(a)
		}()
	}
}

func TestVilleThresholdExact(t *testing.T) {
	// VilleThreshold(alpha) = -log(alpha).
	for _, a := range []float64{0.001, 0.01, 0.05, 0.1, 0.5, 0.99} {
		got := VilleThreshold(a)
		want := -math.Log(a)
		if !within(got, want, 1e-15) {
			t.Errorf("VilleThreshold(%v) = %.15g, want %.15g", a, got, want)
		}
	}
}

func TestVilleUnionThresholdPanics(t *testing.T) {
	// Invalid n.
	for _, n := range []int{-1, 0} {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("VilleUnionThreshold(%d, 0.05) did not panic", n)
				}
			}()
			VilleUnionThreshold(n, 0.05)
		}()
	}
	// Invalid alpha.
	for _, a := range []float64{0, 1, -0.1, math.NaN()} {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("VilleUnionThreshold(5, %v) did not panic", a)
				}
			}()
			VilleUnionThreshold(5, a)
		}()
	}
}

func TestVilleUnionThresholdExact(t *testing.T) {
	// VilleUnionThreshold(n, alpha) = log(n) - log(alpha).
	for _, n := range []int{1, 2, 10, 1000} {
		for _, a := range []float64{0.01, 0.05, 0.1} {
			got := VilleUnionThreshold(n, a)
			want := math.Log(float64(n)) - math.Log(a)
			if !within(got, want, 1e-14) {
				t.Errorf("VilleUnionThreshold(%d, %v) = %.15g, want %.15g", n, a, got, want)
			}
		}
	}
}

func TestVilleUnionThresholdReducesToSingle(t *testing.T) {
	// n=1 must equal the single-stream threshold.
	for _, a := range []float64{0.01, 0.05, 0.1, 0.5} {
		if !within(VilleUnionThreshold(1, a), VilleThreshold(a), 1e-15) {
			t.Errorf("n=1, alpha=%v: union %.15g != single %.15g",
				a, VilleUnionThreshold(1, a), VilleThreshold(a))
		}
	}
}

func TestLogWeightedExpSumPanics(t *testing.T) {
	cases := []struct {
		name string
		v, p []float64
	}{
		{"empty", []float64{}, []float64{}},
		{"nil", nil, nil},
		{"length mismatch", []float64{0, 1}, []float64{0.5}},
		{"all zero weights", []float64{0, 1}, []float64{0, 0}},
		{"negative weight", []float64{0, 1}, []float64{1, -0.1}},
		{"non-finite weight", []float64{0, 1}, []float64{1, math.Inf(1)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("LogWeightedExpSum(%v, %v) did not panic", c.v, c.p)
				}
			}()
			LogWeightedExpSum(c.v, c.p)
		})
	}
}

func TestLogWeightedExpSumSingleton(t *testing.T) {
	// log( 1 · exp(v) ) = v.
	for _, v := range []float64{-5, -1, 0, 1, 100} {
		got := LogWeightedExpSum([]float64{v}, []float64{1})
		if !within(got, v, 1e-14) {
			t.Errorf("LogWeightedExpSum([%v],[1]) = %.15g, want %v", v, got, v)
		}
	}
}

func TestLogWeightedExpSumStability(t *testing.T) {
	// Values that would overflow a naive sum_i p_i exp(v_i) must still produce
	// the correct stable result.
	values := []float64{1000, 1000}
	probs := []float64{0.5, 0.5}
	got := LogWeightedExpSum(values, probs)
	if !within(got, 1000, 1e-12) {
		t.Errorf("got %.15g, want ~1000", got)
	}
}

func TestLogWeightedExpSumInfiniteValues(t *testing.T) {
	got := LogWeightedExpSum([]float64{math.Inf(-1), math.Inf(-1)}, []float64{0.5, 0.5})
	if !math.IsInf(got, -1) {
		t.Errorf("got %v, want -Inf", got)
	}
	got = LogWeightedExpSum([]float64{0, math.Inf(1)}, []float64{0, 1})
	if !math.IsInf(got, 1) {
		t.Errorf("got %v, want +Inf", got)
	}
}

func TestLogWeightedExpSumTwoPointBernoulliCertificate(t *testing.T) {
	// For genuine two-point LR increments under H0, E_{H0}[exp(Δ)] = 1
	// exactly, so LogWeightedExpSum returns 0.
	for _, c := range []struct{ p0, p1 float64 }{
		{0.1, 0.5},
		{0.3, 0.8},
		{0.5, 0.51},
		{0.9, 0.05},
	} {
		s, f := TwoPointLLRIncrements(c.p0, c.p1)
		got := LogWeightedExpSum([]float64{s, f}, []float64{c.p0, 1 - c.p0})
		if !within(got, 0, 1e-14) {
			t.Errorf("p0=%v p1=%v: LogWeightedExpSum = %.15g, want 0", c.p0, c.p1, got)
		}
	}
}

func TestLogWeightedExpSumDownScalingLemma(t *testing.T) {
	// If E[exp(Δ)] = 1 (so LogWeightedExpSum(Δ) = 0), then scaling Δ by
	// any s ∈ (0, 1] yields LogWeightedExpSum ≤ 0 (Jensen on x ↦ x^s).
	pS, pF := TwoPointLLRIncrements(0.3, 0.8)
	const p0 = 0.3
	for _, scale := range []float64{1e-4, 0.1, 0.5, 0.75, 1.0} {
		got := LogWeightedExpSum([]float64{scale * pS, scale * pF}, []float64{p0, 1 - p0})
		if got > 1e-13 {
			t.Errorf("scale=%v: LogWeightedExpSum = %.15g, want <= 0 (down-scaling lemma)", scale, got)
		}
	}
	// Over-weighting (scale > 1) is permitted to break the certificate.
	got := LogWeightedExpSum([]float64{1.5 * pS, 1.5 * pF}, []float64{p0, 1 - p0})
	if got <= 0 {
		t.Errorf("scale=1.5: LogWeightedExpSum = %.15g; an over-weighted increment must "+
			"produce a positive log-moment for at least some delta distributions, "+
			"signalling the supermartingale property is no longer certified", got)
	}
}
