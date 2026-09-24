package stats

import (
	"math"
	"slices"
	"sort"
	"testing"
)

// A ten-point ramp 10,20,...,100. Under Hyndman & Fan type-7, quantiles are
// analytically derivable. At p, the interpolation index is h = p·(n−1) = 9p,
// so the quantile is 10 + 90p. Cases below derive the corresponding
// interpolation directly.
var ramp10 = []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}

func TestQuantileSorted(t *testing.T) {
	cases := []struct {
		p    float64
		want float64
	}{
		{0, 10},      // minimum
		{1, 100},     // maximum
		{0.5, 55},    // h=4.5 -> (s[4]+s[5])/2 = (50+60)/2
		{0.25, 32.5}, // h=2.25 -> 30 + 0.25*10
		{0.95, 95.5}, // h=8.55 -> 90 + 0.55*10
		{0.99, 99.1}, // h=8.91 -> 90 + 0.91*10
		{-0.1, math.NaN()},
		{1.1, math.NaN()},
		{math.NaN(), math.NaN()},
	}
	for _, c := range cases {
		if got := QuantileSorted(ramp10, c.p); !within(got, c.want, 1e-12) {
			t.Errorf("QuantileSorted(ramp10, %v) = %v, want %v", c.p, got, c.want)
		}
	}
	if got := QuantileSorted(nil, 0.5); !math.IsNaN(got) {
		t.Errorf("QuantileSorted(empty) = %v, want NaN", got)
	}
	// A single-element sample has every quantile equal to that element.
	for _, p := range []float64{0, 0.5, 1} {
		if got := QuantileSorted([]float64{42}, p); got != 42 {
			t.Errorf("QuantileSorted([42], %v) = %v, want 42", p, got)
		}
	}
}

func TestMedian(t *testing.T) {
	cases := []struct {
		xs   []float64
		want float64
	}{
		{[]float64{3, 1, 2}, 2},      // odd n: middle value
		{[]float64{4, 1, 3, 2}, 2.5}, // even n: mean of two central
		{[]float64{7}, 7},            // single element
		{[]float64{5, 5, 5}, 5},      // constant
		{ramp10, 55},                 // matches Quantile(., 0.5)
		{[]float64{}, math.NaN()},    // empty
	}
	for _, c := range cases {
		if got := Median(c.xs); !within(got, c.want, 1e-12) {
			t.Errorf("Median(%v) = %v, want %v", c.xs, got, c.want)
		}
	}
	// MedianSorted must agree with Median on sorted input.
	if MedianSorted(ramp10) != Median(ramp10) {
		t.Errorf("MedianSorted(ramp10) = %v, Median(ramp10) = %v; must agree", MedianSorted(ramp10), Median(ramp10))
	}
}

func TestIQR(t *testing.T) {
	cases := []struct {
		xs   []float64
		want float64
	}{
		{ramp10, 45},               // Q3=77.5, Q1=32.5
		{[]float64{42}, 0},         // single element
		{[]float64{5, 5, 5, 5}, 0}, // constant
		{[]float64{}, math.NaN()},  // empty
	}
	for _, c := range cases {
		if got := IQR(c.xs); !within(got, c.want, 1e-12) {
			t.Errorf("IQR(%v) = %v, want %v", c.xs, got, c.want)
		}
	}
	if IQRSorted(ramp10) != IQR(ramp10) {
		t.Errorf("IQRSorted(ramp10) = %v, IQR(ramp10) = %v; must agree", IQRSorted(ramp10), IQR(ramp10))
	}
}

func TestMAD(t *testing.T) {
	cases := []struct {
		xs   []float64
		want float64
	}{
		// median 3; abs devs sorted [0,1,1,2,2]; their median is 1.
		{[]float64{1, 2, 3, 4, 5}, 1},
		// median 55; abs devs sorted [5,5,15,15,25,25,35,35,45,45]; median 25.
		{ramp10, 25},
		{[]float64{42}, 0},         // single element
		{[]float64{5, 5, 5, 5}, 0}, // constant
		{[]float64{}, math.NaN()},  // empty
	}
	for _, c := range cases {
		if got := MAD(c.xs); !within(got, c.want, 1e-12) {
			t.Errorf("MAD(%v) = %v, want %v", c.xs, got, c.want)
		}
	}
	// MADSorted must agree with MAD and must not mutate its input.
	sorted := append([]float64(nil), ramp10...)
	if got := MADSorted(sorted); got != MAD(ramp10) {
		t.Errorf("MADSorted(ramp10) = %v, MAD(ramp10) = %v; must agree", got, MAD(ramp10))
	}
	for i := range ramp10 {
		if sorted[i] != ramp10[i] {
			t.Fatalf("MADSorted mutated its argument at index %d: got %v, want %v", i, sorted[i], ramp10[i])
		}
	}
}

func TestECDF(t *testing.T) {
	// Fraction of ramp10 that is <= x. 10 points spaced by 10.
	cases := []struct {
		x    float64
		want float64
	}{
		{5, 0.0},   // below all
		{10, 0.1},  // exactly the minimum: <= includes it
		{25, 0.2},  // 10,20 are <= 25
		{30, 0.3},  // 10,20,30 (boundary included)
		{100, 1.0}, // exactly the maximum
		{150, 1.0}, // above all
	}
	for _, c := range cases {
		if got := ECDF(ramp10, c.x); !within(got, c.want, 1e-12) {
			t.Errorf("ECDF(ramp10, %v) = %v, want %v", c.x, got, c.want)
		}
	}
	if got := ECDF(nil, 0); !math.IsNaN(got) {
		t.Errorf("ECDF(empty) = %v, want NaN", got)
	}
}

func TestECDFSortedMatchesECDF(t *testing.T) {
	// The binary-search form must agree with the linear form at every point,
	// including exact ties and points between observations.
	for _, x := range []float64{5, 10, 15, 20, 55, 90, 100, 150} {
		lin := ECDF(ramp10, x)
		bin := ECDFSorted(ramp10, x)
		if lin != bin {
			t.Errorf("ECDFSorted(ramp10, %v) = %v, ECDF = %v; must agree", x, bin, lin)
		}
	}
	if got := ECDFSorted(nil, 0); !math.IsNaN(got) {
		t.Errorf("ECDFSorted(empty) = %v, want NaN", got)
	}
	withDuplicates := []float64{1, 2, 2, 2, 3}
	if got := ECDFSorted(withDuplicates, 2); got != 0.8 {
		t.Errorf("ECDFSorted(%v, 2) = %v, want 0.8", withDuplicates, got)
	}
}

func FuzzOrderStatisticsSortedEquivalence(f *testing.F) {
	f.Add([]byte{3, 1, 4, 1, 5})
	f.Add([]byte{0, 0, 0, 0})
	f.Add([]byte{255, 0, 127, 128})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 || len(data) > 64 {
			t.Skip()
		}
		xs := make([]float64, len(data))
		for i, value := range data {
			xs[i] = float64(int(value) - 128)
		}
		orig := slices.Clone(xs)
		sorted := slices.Clone(xs)
		sort.Float64s(sorted)
		p := float64(data[0]) / 255

		checks := []struct {
			name  string
			plain func([]float64) float64
			fast  func([]float64) float64
		}{
			{
				"quantile", func(v []float64) float64 { return Quantile(v, p) },
				func(v []float64) float64 { return QuantileSorted(v, p) },
			},
			{"median", Median, MedianSorted},
			{"IQR", IQR, IQRSorted},
			{"MAD", MAD, MADSorted},
			{
				"ECDF", func(v []float64) float64 { return ECDF(v, xs[0]) },
				func(v []float64) float64 { return ECDFSorted(v, xs[0]) },
			},
		}
		for _, check := range checks {
			plainInput := slices.Clone(xs)
			sortedInput := slices.Clone(sorted)
			plain, fast := check.plain(plainInput), check.fast(sortedInput)
			if plain != fast {
				t.Errorf("%s: plain = %v, sorted = %v", check.name, plain, fast)
			}
			if !slices.Equal(plainInput, xs) {
				t.Errorf("%s plain form mutated input: got %v, want %v",
					check.name, plainInput, xs)
			}
			if !slices.Equal(sortedInput, sorted) {
				t.Errorf("%s sorted form mutated input: got %v, want %v",
					check.name, sortedInput, sorted)
			}
		}
		if !slices.Equal(xs, orig) {
			t.Errorf("fuzz fixture mutated: got %v, want %v", xs, orig)
		}
	})
}
