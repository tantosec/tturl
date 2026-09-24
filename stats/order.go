package stats

import (
	"math"
	"sort"
)

// QuantileSorted returns the p-quantile (0 <= p <= 1) of the ascending-sorted
// sample using Hyndman and Fan's type-7 linear interpolation (1996).
//
// Returns NaN if the sample is empty or p is outside [0, 1] (or NaN). p = 0 and
// p = 1 return the sample minimum and maximum respectively.
func QuantileSorted(sorted []float64, p float64) float64 {
	// The `!(...)` form catches NaN: any comparison with NaN is false, so the
	// negation is true and we return NaN.
	if len(sorted) == 0 || !(p >= 0 && p <= 1) {
		return math.NaN()
	}
	h := p * float64(len(sorted)-1)
	lo := int(h)
	if lo >= len(sorted)-1 {
		return sorted[len(sorted)-1]
	}
	fraction := h - float64(lo)
	a, b := sorted[lo], sorted[lo+1]
	if fraction == 0 {
		return a
	}
	if (a < 0) != (b < 0) {
		return (1-fraction)*a + fraction*b
	}
	return a + fraction*(b-a)
}

// Quantile returns [QuantileSorted] applied to a sorted copy of xs. It does not
// modify xs and takes O(n log n) time.
func Quantile(xs []float64, p float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return QuantileSorted(s, p)
}

// MedianSorted returns the type-7 median of the ascending-sorted sample. It
// returns NaN for an empty sample.
func MedianSorted(sorted []float64) float64 { return QuantileSorted(sorted, 0.5) }

// Median returns [MedianSorted] applied to a sorted copy of xs. It does not
// modify xs and takes O(n log n) time.
func Median(xs []float64) float64 { return Quantile(xs, 0.5) }

// IQRSorted returns the interquartile range of the ascending-sorted sample:
// the difference between its type-7 0.75 and 0.25 quantiles. It returns NaN for
// an empty sample and zero for a single-element sample.
func IQRSorted(sorted []float64) float64 {
	return QuantileSorted(sorted, 0.75) - QuantileSorted(sorted, 0.25)
}

// IQR returns [IQRSorted] applied to a sorted copy of xs. It does not modify xs
// and takes O(n log n) time.
func IQR(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return IQRSorted(s)
}

// MADSorted returns the raw median absolute deviation of the ascending-sorted
// sample. It is not scaled to estimate standard deviation. It returns NaN for
// an empty sample and zero for a single-element or constant sample.
func MADSorted(sorted []float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	m := MedianSorted(sorted)
	dev := make([]float64, len(sorted))
	for i, v := range sorted {
		dev[i] = math.Abs(v - m)
	}
	sort.Float64s(dev)
	return MedianSorted(dev)
}

// MAD returns [MADSorted] applied to a sorted copy of xs. It does not modify xs
// and takes O(n log n) time.
func MAD(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return MADSorted(s)
}

// ECDF returns the empirical cumulative distribution function of the sample xs
// at x: the fraction of observations no greater than x. It takes O(n) time and
// returns NaN for an empty sample or a NaN query. The sample need not be
// sorted; its observations must be finite.
func ECDF(xs []float64, x float64) float64 {
	if len(xs) == 0 || math.IsNaN(x) {
		return math.NaN()
	}
	count := 0
	for _, v := range xs {
		if v <= x {
			count++
		}
	}
	return float64(count) / float64(len(xs))
}

// ECDFSorted is ECDF for a sample already sorted ascending, evaluated by binary
// search in O(log n). Returns NaN for an empty sample or a NaN query.
func ECDFSorted(sorted []float64, x float64) float64 {
	if len(sorted) == 0 || math.IsNaN(x) {
		return math.NaN()
	}
	// The count of elements <= x is the index of the first element > x.
	i := sort.Search(len(sorted), func(i int) bool { return sorted[i] > x })
	return float64(i) / float64(len(sorted))
}
