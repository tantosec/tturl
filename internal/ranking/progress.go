package ranking

import (
	"math"
	"time"
)

// ProgressCore contains the cost and throughput shared by progress snapshots.
type ProgressCore struct {
	_ struct{} // keep literals keyed

	Cost
	// ComparisonsPerSecond and ParticipationsPerSecond are exponentially smoothed
	// recent rates. Each is NaN until an observation spans a positive duration.
	ComparisonsPerSecond    float64
	ParticipationsPerSecond float64
}

// ProgressCadence controls periodic progress delivery. Construct one with
// [EveryComparisons] or [EveryInterval]. The zero value disables periodic
// reports but does not suppress a solver's final report. A cadence is immutable
// and reusable.
type ProgressCadence struct {
	comparisons int           // count mode when > 0: fire every N comparisons
	interval    time.Duration // interval mode when > 0: fire once this much wall time has passed
}

// EveryComparisons returns a cadence that reports after each n comparisons.
// Non-positive values disable periodic reports.
func EveryComparisons(n int) ProgressCadence { return ProgressCadence{comparisons: n} }

// EveryInterval returns a cadence that reports at the first comparison boundary
// at least d after the previous report. Non-positive values disable periodic
// reports.
func EveryInterval(d time.Duration) ProgressCadence { return ProgressCadence{interval: d} }

// CadenceRun meters and schedules progress for one solve. It is not safe for
// concurrent use.
type CadenceRun struct {
	comparisons int           // count-mode policy, copied from the cadence
	interval    time.Duration // interval-mode policy, copied from the cadence

	// Advanced as reports fire; the clock and meter are seeded at the solve
	// start by [ProgressCadence.Start], so no per-report start check is needed.
	lastComparisons int
	lastTime        time.Time
	meter           *rateMeter
}

// Start returns a new run whose timing begins at the current time.
func (c ProgressCadence) Start() *CadenceRun {
	now := time.Now()
	return &CadenceRun{
		comparisons: c.comparisons,
		interval:    c.interval,
		lastTime:    now,
		meter:       newRateMeter(now),
	}
}

// ReportIfDue records cumulative costs and calls deliver when the cadence is
// due. Call it after each atomic evidence update. With a non-nil deliver it
// meters every call, even when no report is due; a nil deliver makes it inert.
func (r *CadenceRun) ReportIfDue(comparisons, participations int, deliver func(ProgressCore)) {
	if deliver == nil {
		return
	}
	now := time.Now()
	r.meter.observe(comparisons, participations, now)
	if r.due(comparisons, now) {
		deliver(r.core(comparisons, participations))
	}
}

// ReportFinal records cumulative costs and calls deliver unconditionally. A nil
// deliver makes it inert.
func (r *CadenceRun) ReportFinal(comparisons, participations int, deliver func(ProgressCore)) {
	if deliver == nil {
		return
	}
	r.meter.observe(comparisons, participations, time.Now())
	deliver(r.core(comparisons, participations))
}

// reportBoundary records cumulative costs and delivers a resumable-run pause.
// It advances both cadence anchors so the next tranche does not immediately
// repeat the boundary report.
func (r *CadenceRun) reportBoundary(
	comparisons, participations int, deliver func(ProgressCore),
) {
	if deliver == nil {
		return
	}
	now := time.Now()
	r.meter.observe(comparisons, participations, now)
	r.lastComparisons = comparisons
	r.lastTime = now
	deliver(r.core(comparisons, participations))
}

// core returns the running counts with the throughput metered so far.
func (r *CadenceRun) core(comparisons, participations int) ProgressCore {
	cps, pps := r.meter.rate()
	return ProgressCore{
		Cost:                 Cost{Comparisons: comparisons, Participations: participations},
		ComparisonsPerSecond: cps, ParticipationsPerSecond: pps,
	}
}

// due reports whether a snapshot is due and advances the cadence when it is.
func (r *CadenceRun) due(comparisons int, now time.Time) bool {
	switch {
	case r.comparisons > 0:
		if comparisons-r.lastComparisons >= r.comparisons {
			r.lastComparisons = comparisons
			return true
		}
	case r.interval > 0:
		if now.Sub(r.lastTime) >= r.interval {
			r.lastTime = now
			return true
		}
	}
	return false
}

// rateMemory is the exponential meter's time constant.
const rateMemory = 2 * time.Second

// rateMeter tracks exponentially decayed costs and elapsed time. Decaying both
// makes its rates independent of observation frequency.
type rateMeter struct {
	lastCmp, lastPart int
	lastTime          time.Time

	// Decayed cost, and the decayed wall time it was accrued over.
	cmp, part, window float64
}

// newRateMeter returns a zero-cost meter beginning at start.
func newRateMeter(start time.Time) *rateMeter {
	return &rateMeter{lastTime: start}
}

// observe folds cumulative costs at now into the meter. Non-positive elapsed
// time contributes nothing.
func (m *rateMeter) observe(comparisons, participations int, now time.Time) {
	d := now.Sub(m.lastTime).Seconds()
	if d <= 0 {
		return
	}
	w := math.Exp(-d / rateMemory.Seconds())
	m.cmp = w*m.cmp + float64(comparisons-m.lastCmp)
	m.part = w*m.part + float64(participations-m.lastPart)
	m.window = w*m.window + d
	m.lastCmp, m.lastPart, m.lastTime = comparisons, participations, now
}

// rate returns the current smoothed comparisons- and participations-per-second,
// each NaN until an observation has covered some wall time.
func (m *rateMeter) rate() (comparisonsPerSecond, participationsPerSecond float64) {
	if m.window == 0 {
		return math.NaN(), math.NaN()
	}
	return m.cmp / m.window, m.part / m.window
}
