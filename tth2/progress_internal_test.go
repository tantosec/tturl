package tth2

import (
	"math"
	"testing"
	"time"
)

// TestRateMeterCadenceIndependence feeds the same steady rate at two
// observation cadences and requires equal results.
func TestRateMeterCadenceIndependence(t *testing.T) {
	t.Parallel()
	const (
		want  = 640.0
		width = 8 // requests per trial, so request rates track trial rates x8
		eps   = 1e-9
	)
	base := time.Unix(3_000_000, 0)
	fine, coarse := newRateMeter(base), newRateMeter(base)
	for i := 1; i <= 100; i++ { // every 100ms for 10s
		n := 64 * i
		fine.observe(n, n*width, base.Add(time.Duration(i)*100*time.Millisecond))
	}
	for i := 1; i <= 10; i++ { // every 1s for the same 10s
		n := 640 * i
		coarse.observe(n, n*width, base.Add(time.Duration(i)*time.Second))
	}
	f, fr := fine.rate()
	c, cr := coarse.rate()
	if math.Abs(f-want) > eps || math.Abs(c-want) > eps {
		t.Errorf("trial rates = (%v fine, %v coarse), want %v for both", f, c, want)
	}
	if math.Abs(fr-want*width) > eps || math.Abs(cr-want*width) > eps {
		t.Errorf("request rates = (%v fine, %v coarse), want %v for both", fr, cr, want*width)
	}
}

// TestRateMeterFollowsASlowdown compares a changing rate with the closed-form
// decayed-cost-over-decayed-window result.
func TestRateMeterFollowsASlowdown(t *testing.T) {
	t.Parallel()
	const eps = 1e-9
	w := math.Exp(-time.Second.Seconds() / rateMemory.Seconds()) // one second of decay
	base := time.Unix(1_000_000, 0)
	m := newRateMeter(base)

	m.observe(640, 640, base.Add(1*time.Second)) // 640 over 1s from the start
	if got, _ := m.rate(); math.Abs(got-640) > eps {
		t.Fatalf("seed: %v, want 640", got)
	}
	// +320 over the next second, against a decayed history of 640-per-1s.
	m.observe(960, 960, base.Add(2*time.Second))
	want := (w*640 + 320) / (w*1 + 1)
	if got, _ := m.rate(); math.Abs(got-want) > eps {
		t.Errorf("slowdown: %v, want %v (between 320 and 640)", got, want)
	}
	if want <= 320 || want >= 640 {
		t.Fatalf("the expectation itself is wrong: %v is not between the old and new rates", want)
	}
}

// TestRateMeterReportsNoRateWithoutAWindow verifies that an unfed meter reports
// NaN rather than a fabricated rate.
func TestRateMeterReportsNoRateWithoutAWindow(t *testing.T) {
	t.Parallel()
	base := time.Unix(2_000_000, 0)
	var zero rateMeter
	if tr, rr := zero.rate(); !math.IsNaN(tr) || !math.IsNaN(rr) {
		t.Errorf("zero meter = (%v, %v), want NaN rates", tr, rr)
	}
	m := newRateMeter(base)
	if tr, rr := m.rate(); !math.IsNaN(tr) || !math.IsNaN(rr) {
		t.Errorf("unfed meter = (%v, %v), want NaN rates", tr, rr)
	}
	m.observe(10, 80, base) // no elapsed time: forms no sample
	if tr, rr := m.rate(); !math.IsNaN(tr) || !math.IsNaN(rr) {
		t.Errorf("zero-gap observe = (%v, %v), want NaN rates", tr, rr)
	}
	m.observe(60, 480, base.Add(1*time.Second)) // 60 over 1s from the start
	if tr, rr := m.rate(); math.Abs(tr-60) > 1e-9 || math.Abs(rr-480) > 1e-9 {
		t.Errorf("first sample = (%v, %v), want (60, 480)", tr, rr)
	}
}
