package ranking

import (
	"math"
	"testing"
	"time"
)

// TestProgressCadenceConstructors verifies that cadence modes are exclusive.
func TestProgressCadenceConstructors(t *testing.T) {
	if c := EveryComparisons(50); c.comparisons != 50 || c.interval != 0 {
		t.Errorf("EveryComparisons(50) = %+v, want {comparisons:50}", c)
	}
	if c := EveryInterval(time.Second); c.interval != time.Second || c.comparisons != 0 {
		t.Errorf("EveryInterval(1s) = %+v, want {interval:1s}", c)
	}
}

// TestProgressCadenceDueCount verifies comparison-count boundaries.
func TestProgressCadenceDueCount(t *testing.T) {
	r := &CadenceRun{comparisons: 50}
	for _, s := range []struct {
		comparisons int
		want        bool
	}{
		{49, false}, {50, true}, {99, false}, {100, true}, {150, true}, {151, false},
	} {
		if got := r.due(s.comparisons, time.Time{}); got != s.want {
			t.Errorf("due(%d) = %v, want %v", s.comparisons, got, s.want)
		}
	}
}

// TestProgressCadenceDueInterval verifies interval boundaries from the start of
// a run and from each report.
func TestProgressCadenceDueInterval(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	// Start seeds lastTime at the solve start; set it directly to drive due.
	r := &CadenceRun{interval: time.Second, lastTime: base}
	for i, s := range []struct {
		dt   time.Duration
		want bool
	}{
		{0, false},                       // no time elapsed yet
		{999 * time.Millisecond, false},  // not yet a full interval
		{time.Second, true},              // a full interval from the start
		{1500 * time.Millisecond, false}, // < 1s since the last fire (at 1s)
		{2 * time.Second, true},          // another full interval
	} {
		if got := r.due(i, base.Add(s.dt)); got != s.want {
			t.Errorf("due at +%v = %v, want %v", s.dt, got, s.want)
		}
	}
}

func TestProgressCadenceDisabledNeverDue(t *testing.T) {
	for _, r := range []*CadenceRun{
		{},
		{comparisons: -1},
		{interval: -time.Second},
	} {
		if r.due(1_000_000, time.Unix(1, 0)) {
			t.Errorf("disabled run %+v should never be due", r)
		}
	}
}

// TestRateMeter verifies rates through steady, slower, and faster observations.
//
// Expected values use decay w = exp(-d/rateMemory) and divide decayed cost by
// the decayed window. Participations remain eight times the comparisons.
func TestRateMeter(t *testing.T) {
	const (
		eps   = 1e-9
		width = 8
	)
	// One second of decay, the cadence every step below uses.
	w := math.Exp(-time.Second.Seconds() / rateMemory.Seconds())
	base := time.Unix(1_000_000, 0)
	m := newRateMeter(base)
	obs := func(step string, cmp int, dt time.Duration, wantCmp float64) {
		m.observe(cmp, cmp*width, base.Add(dt))
		gotCmp, gotPart := m.rate()
		if math.Abs(gotCmp-wantCmp) > eps {
			t.Errorf("%s: comparisons/s = %v, want %v", step, gotCmp, wantCmp)
		}
		if math.Abs(gotPart-wantCmp*width) > eps {
			t.Errorf("%s: participations/s = %v, want %v", step, gotPart, wantCmp*width)
		}
	}
	obs("seed", 640, 1*time.Second, 640)    // 640 over 1s
	obs("steady", 1280, 2*time.Second, 640) // +640/1s: a steady stream reports itself
	// +320 over 1s against a decayed 640-per-1s history, twice over.
	slow := (w*(w*640+640) + 320) / (w*(w*1+1) + 1)
	obs("slowdown", 1600, 3*time.Second, slow)
	// A zero-length gap carries no time to attribute cost to, so it is ignored --
	// which is what keeps a final report landing on top of a periodic one from
	// dividing by nothing.
	obs("zero gap", 1600, 3*time.Second, slow)
	backCmp := w*(w*(w*640+640)+320) + 640
	backWin := w*(w*(w*1+1)+1) + 1
	obs("re-accelerated", 2240, 4*time.Second, backCmp/backWin)
}

// TestRateMeterCadenceIndependence verifies that observation frequency does not
// change the rate for an identical steady stream.
func TestRateMeterCadenceIndependence(t *testing.T) {
	const (
		want  = 640.0
		width = 8
		eps   = 1e-9
	)
	base := time.Unix(3_000_000, 0)
	fine, coarse := newRateMeter(base), newRateMeter(base)
	for i := 1; i <= 100; i++ { // every 100ms for 10s
		c := 64 * i
		fine.observe(c, c*width, base.Add(time.Duration(i)*100*time.Millisecond))
	}
	for i := 1; i <= 10; i++ { // every 1s for the same 10s
		c := 640 * i
		coarse.observe(c, c*width, base.Add(time.Duration(i)*time.Second))
	}
	f, _ := fine.rate()
	c, _ := coarse.rate()
	if math.Abs(f-want) > eps || math.Abs(c-want) > eps {
		t.Errorf("rates = (%v fine, %v coarse), want %v for both", f, c, want)
	}
}

// TestRateMeterNaNUntilFirstSample verifies that rates remain undefined until
// an observation spans positive wall time.
func TestRateMeterNaNUntilFirstSample(t *testing.T) {
	base := time.Unix(2_000_000, 0)
	m := newRateMeter(base)
	if c, p := m.rate(); !math.IsNaN(c) || !math.IsNaN(p) {
		t.Errorf("unfed meter = (%v, %v), want NaN rates", c, p)
	}
	m.observe(10, 80, base) // no elapsed time: forms no sample
	if c, p := m.rate(); !math.IsNaN(c) || !math.IsNaN(p) {
		t.Errorf("zero-gap observe = (%v, %v), want NaN rates", c, p)
	}
	m.observe(60, 480, base.Add(1*time.Second)) // 60 over 1s from the start
	if c, p := m.rate(); math.Abs(c-60) > 1e-9 || math.Abs(p-480) > 1e-9 {
		t.Errorf("first sample = (%v, %v), want (60, 480)", c, p)
	}
}
