package stats

import (
	"math"
	"testing"
)

func TestNormalTailP(t *testing.T) {
	// z values that map to known p-values; NormCDF is Erfc-based, so a tight
	// tolerance is justified.
	cases := []struct {
		z    float64
		alt  Alternative
		want float64
		tol  float64
	}{
		{0, TwoSided, 1.0, 1e-12},
		{0, Greater, 0.5, 1e-12},
		{0, Less, 0.5, 1e-12},
		{1.9599639845, TwoSided, 0.05, 1e-9},
		{1.6448536270, Greater, 0.05, 1e-9},
		{-1.6448536270, Less, 0.05, 1e-9},
		{2.5758293035, TwoSided, 0.01, 1e-9},
		// Published standard-normal tails. Direct tail evaluation must preserve
		// these representable values instead of cancelling them to zero.
		{8, Greater, 6.220960574271784e-16, 1e-29},
		{-8, Less, 6.220960574271784e-16, 1e-29},
		{8, TwoSided, 1.2441921148543568e-15, 2e-29},
		{math.NaN(), TwoSided, math.NaN(), 0},
		{1.0, Alternative(99), math.NaN(), 0}, // undefined alternative
	}
	for _, c := range cases {
		got := NormalTailP(c.z, c.alt)
		if !within(got, c.want, c.tol) {
			t.Errorf("NormalTailP(%v, %v) = %v, want %v", c.z, c.alt, got, c.want)
		}
	}

	// Symmetry: P(Z ≥ -z) under Greater equals P(Z ≤ z) under Less.
	if !within(NormalTailP(-0.7, Greater), NormalTailP(0.7, Less), 1e-12) {
		t.Errorf("NormalTailP sign symmetry violated")
	}
}

func TestOneSampleZ(t *testing.T) {
	cases := []struct {
		mean, mu0, sd float64
		n             int
		want          float64
		tol           float64
	}{
		{1, 0, 2, 4, 1, 1e-12},        // (1−0)/(2/√4) = 1/1
		{5, 2, 6, 4, 1, 1e-12},        // 3/(6/2)
		{2, 0, 1, 100, 20, 1e-12},     // 2/(1/10)
		{-1, 0, 2, 4, -1, 1e-12},      // symmetric below
		{10, 10, 5, 9, 0, 1e-12},      // mean exactly at the null
		{3, 3, 0, 4, 0, 0},            // sd = 0, mean = mu0 → 0
		{5, 3, 0, 4, math.Inf(1), 0},  // sd = 0, mean > mu0 → +Inf
		{1, 3, 0, 4, math.Inf(-1), 0}, // sd = 0, mean < mu0 → −Inf
		{1, 0, 2, 0, math.NaN(), 0},   // no observations
		{1, 0, -1, 4, math.NaN(), 0},  // invalid sd
		{math.Inf(1), 0, 1, 4, math.NaN(), 0},
		{1, math.NaN(), 1, 4, math.NaN(), 0},
		{1, 0, math.Inf(1), 4, math.NaN(), 0},
	}
	for _, c := range cases {
		got := OneSampleZ(c.mean, c.mu0, c.sd, c.n)
		if !within(got, c.want, c.tol) {
			t.Errorf("OneSampleZ(%v, %v, %v, %d) = %v, want %v", c.mean, c.mu0, c.sd, c.n, got, c.want)
		}
	}

	// OneSampleZ generalises ProportionZ: with the proportion null SD
	// sqrt(p0(1−p0)) it reproduces the proportion z.
	if got, want := OneSampleZ(0.6, 0.5, math.Sqrt(0.5*0.5), 100), ProportionZ(60, 100, 0.5); !within(got, want, 1e-9) {
		t.Errorf("OneSampleZ proportion form = %v, want ProportionZ = %v", got, want)
	}
}

func TestProportionZ(t *testing.T) {
	cases := []struct {
		successes, n int
		p0           float64
		want         float64
		tol          float64
	}{
		{60, 100, 0.5, 2.0, 1e-12},  // (0.6-0.5)/sqrt(0.25/100) = 0.1/0.05
		{50, 100, 0.5, 0.0, 1e-12},  // exactly at the null
		{40, 100, 0.5, -2.0, 1e-12}, // symmetric below
		{0, 100, 0.5, -10.0, 1e-12}, // extreme: zero successes is a valid observation
		{-1, 100, 0.5, math.NaN(), 0},
		{101, 100, 0.5, math.NaN(), 0},
		{50, 0, 0.5, math.NaN(), 0}, // no trials
		{50, 100, 0, math.NaN(), 0}, // p0 at closed endpoint
		{50, 100, 1, math.NaN(), 0},
	}
	for _, c := range cases {
		got := ProportionZ(c.successes, c.n, c.p0)
		if !within(got, c.want, c.tol) {
			t.Errorf("ProportionZ(%d, %d, %v) = %v, want %v", c.successes, c.n, c.p0, got, c.want)
		}
	}

	// Equivalence to the count form (wins − n·p0)/sqrt(n·p0(1−p0)).
	successes, n, p0 := 73, 200, 1.0/3.0
	countForm := (float64(successes) - float64(n)*p0) / math.Sqrt(float64(n)*p0*(1-p0))
	if !within(ProportionZ(successes, n, p0), countForm, 1e-9) {
		t.Errorf("ProportionZ disagrees with the equivalent count form")
	}
}

func TestWilsonInterval(t *testing.T) {
	// Published 95% Wilson interval for 50/100 is ≈ [0.4038, 0.5962].
	lo, hi := WilsonInterval(50, 100, 1.96)
	if !within(lo, 0.403831, 1e-5) || !within(hi, 0.596169, 1e-5) {
		t.Errorf("WilsonInterval(50,100,1.96) = [%v, %v], want ~= [0.4038, 0.5962]", lo, hi)
	}

	// Within [0,1] even at an extreme rate.
	lo, hi = WilsonInterval(100, 100, 1.96)
	if lo < 0 || hi > 1 || lo > hi {
		t.Errorf("WilsonInterval(100,100,1.96) = [%v, %v], expected a valid sub-interval of [0,1]", lo, hi)
	}

	// No observations → (NaN, NaN).
	lo, hi = WilsonInterval(0, 0, 1.96)
	if !math.IsNaN(lo) || !math.IsNaN(hi) {
		t.Errorf("WilsonInterval with n=0 = [%v, %v], want (NaN, NaN)", lo, hi)
	}
	for _, successes := range []int{-1, 101} {
		lo, hi = WilsonInterval(successes, 100, 1.96)
		if !math.IsNaN(lo) || !math.IsNaN(hi) {
			t.Errorf("WilsonInterval(%d,100,1.96) = [%v, %v], want (NaN, NaN)",
				successes, lo, hi)
		}
	}
	for _, z := range []float64{-1, math.NaN(), math.Inf(1)} {
		lo, hi = WilsonInterval(50, 100, z)
		if !math.IsNaN(lo) || !math.IsNaN(hi) {
			t.Errorf("WilsonInterval(50,100,%v) = [%v, %v], want (NaN, NaN)",
				z, lo, hi)
		}
	}
}

func TestClopperPearson(t *testing.T) {
	// The k = 0 and k = n bounds are closed-form, so assert them exactly.
	// For 0/n the upper bound solves 1 − (1−hi)^n = 1 − α/2, i.e.
	// hi = 1 − (α/2)^(1/n); the lower bound is exactly 0.
	lo, hi := ClopperPearson(0, 100, 0.95)
	if lo != 0 {
		t.Errorf("ClopperPearson(0,100,0.95) lo = %v, want 0", lo)
	}
	wantHi := 1 - math.Pow(0.025, 1.0/100.0) // ≈ 0.03621669264517646
	if !within(hi, wantHi, 1e-9) {
		t.Errorf("ClopperPearson(0,100,0.95) hi = %.12f, want %.12f", hi, wantHi)
	}

	// n/n is the mirror image: hi = 1 exactly, lo = (α/2)^(1/n) = 1 − hi(0,n).
	lo, hi = ClopperPearson(100, 100, 0.95)
	if hi != 1 {
		t.Errorf("ClopperPearson(100,100,0.95) hi = %v, want 1", hi)
	}
	wantLo := math.Pow(0.025, 1.0/100.0) // ≈ 0.96378330735482354
	if !within(lo, wantLo, 1e-9) {
		t.Errorf("ClopperPearson(100,100,0.95) lo = %.12f, want %.12f", lo, wantLo)
	}

	// Under k ↔ n−k symmetry, the interval for k successes is the mirror
	// about 0.5 of the interval for n−k successes. This holds for interior k.
	loA, hiA := ClopperPearson(30, 100, 0.95)
	loB, hiB := ClopperPearson(70, 100, 0.95)
	if !within(loA, 1-hiB, 1e-9) || !within(hiA, 1-loB, 1e-9) {
		t.Errorf("ClopperPearson not mirror-symmetric: [%.6f,%.6f] vs 1-[%.6f,%.6f]", loA, hiA, loB, hiB)
	}

	// A central case brackets the point estimate and is ~symmetric about it.
	lo, hi = ClopperPearson(50, 100, 0.95)
	if !(lo < 0.5 && hi > 0.5) {
		t.Errorf("ClopperPearson(50,100,0.95) = [%v,%v], should bracket 0.5", lo, hi)
	}
	if !within((lo+hi)/2, 0.5, 1e-6) {
		t.Errorf("ClopperPearson(50,100,0.95) = [%v,%v], should be ~symmetric about 0.5", lo, hi)
	}

	// Conservatism: Clopper–Pearson contains the Wilson interval at the same
	// level (the exact interval is never narrower than the score approximation).
	wLo, wHi := WilsonInterval(50, 100, 1.96)
	if !(lo <= wLo && hi >= wHi) {
		t.Errorf("ClopperPearson [%v,%v] should contain Wilson [%v,%v]", lo, hi, wLo, wHi)
	}

	// Higher confidence widens the interval.
	lo95, hi95 := ClopperPearson(8, 20, 0.95)
	lo99, hi99 := ClopperPearson(8, 20, 0.99)
	if !(lo99 < lo95 && hi99 > hi95) {
		t.Errorf("99%% interval [%v,%v] should strictly contain 95%% interval [%v,%v]", lo99, hi99, lo95, hi95)
	}

	// Invalid inputs → (NaN, NaN).
	for _, c := range []struct {
		k, n  int
		level float64
	}{
		{0, 0, 0.95}, {-1, 10, 0.95}, {11, 10, 0.95}, {5, 10, 0}, {5, 10, 1}, {5, 10, -0.1},
	} {
		lo, hi := ClopperPearson(c.k, c.n, c.level)
		if !math.IsNaN(lo) || !math.IsNaN(hi) {
			t.Errorf("ClopperPearson(%d,%d,%v) = (%v,%v), want (NaN,NaN)", c.k, c.n, c.level, lo, hi)
		}
	}
}

func TestCohenH(t *testing.T) {
	cases := []struct {
		p1, p2 float64
		want   float64
		tol    float64
	}{
		{0.5, 0.25, math.Pi / 6, 1e-12}, // 2·(π/4) − 2·(π/6) = π/6 ≈ 0.5236
		{0.3, 0.3, 0.0, 1e-12},          // no difference
		{1, 0, math.Pi, 1e-12},          // 2·(π/2) − 0
		{1.1, 0.5, math.NaN(), 0},       // p1 out of range
		{0.5, -0.1, math.NaN(), 0},      // p2 out of range
	}
	for _, c := range cases {
		got := CohenH(c.p1, c.p2)
		if !within(got, c.want, c.tol) {
			t.Errorf("CohenH(%v, %v) = %v, want %v", c.p1, c.p2, got, c.want)
		}
	}

	// Antisymmetry.
	if !within(CohenH(0.7, 0.2), -CohenH(0.2, 0.7), 1e-12) {
		t.Errorf("CohenH antisymmetry violated")
	}
}
