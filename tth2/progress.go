package tth2

import (
	"math"
	"time"
)

// TrialProgress is a snapshot of a run's completed work and recent throughput.
// Statistical outcomes belong to [TrialSummary].
type TrialProgress struct {
	// Trials and Warmup count completed measured and priming trials. Failed
	// trials count here, and completion may precede delivery to the stream
	// consumer, so Trials can exceed [TrialSummary.Trials].
	Trials, Warmup int

	// Requests counts dispatched requests, including warmups, failed trials,
	// and stale-connection retries. Dispatch precedes trial completion.
	Requests int

	// RecentTrialsPerSecond and RecentRequestsPerSecond are exponentially
	// smoothed rates over recent completed work, including warmups. Each is NaN
	// until a time window has been observed.
	RecentTrialsPerSecond, RecentRequestsPerSecond float64
}

// WithProgress calls report after at least d has elapsed at a trial completion,
// and once more when a started run ends. A configuration error produces no
// report.
//
// The callback runs on the goroutine draining the run and must return promptly,
// since it delays consumption of the next trial. It does not run after the run
// returns. Reusing the option across runs can invoke report concurrently; the
// report function owns any synchronisation it needs. WithProgress panics if d
// is not positive or report is nil.
func WithProgress(d time.Duration, report func(TrialProgress)) progressOption {
	if d <= 0 {
		panic("tth2: WithProgress requires d > 0")
	}
	if report == nil {
		panic("tth2: WithProgress requires a report function")
	}
	return progressOption{d: d, report: report}
}

type progressOption struct {
	d      time.Duration
	report func(TrialProgress)
}

func (o progressOption) applyTrials(c *trialsConfig) {
	c.progressEvery, c.progressReport = o.d, o.report
}

func (progressOption) runTrialsOption() {}

// Progress returns an independent snapshot of the run's cost and throughput. It
// is safe to call concurrently with [TrialStream.All].
func (s *TrialStream) Progress() TrialProgress {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	p := s.progress
	p.RecentTrialsPerSecond, p.RecentRequestsPerSecond = s.meter.rate()
	return p
}

// dispatched records that the scheduler authorised a batch's first flush. It is
// called once per attempted send, so a re-send onto a fresh connection counts
// again. An ambiguous write failure is charged conservatively even when the
// client cannot know how much reached the target.
func (s *TrialStream) dispatched(requests int) {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	s.progress.Requests += requests
}

// completed records a finished trial and meters the run's throughput at that
// moment. Every trial counts, priming and failed alike: each one cost the
// target the work it did.
func (s *TrialStream) completed(warmup bool) {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	if warmup {
		s.progress.Warmup++
	} else {
		s.progress.Trials++
	}
	total := s.progress.Trials + s.progress.Warmup
	s.meter.observe(total, s.progress.Requests, time.Now())
}

// reportIfDue delivers a progress report if one is owed, and reportFinal
// delivers the terminal one unconditionally. Both are inert without
// [WithProgress], so the ranging loop calls them with no guard, and both run on
// the consumer's goroutine -- the cadence state below is therefore unguarded.
func (s *TrialStream) reportIfDue() {
	if s.cfg.progressReport == nil {
		return
	}
	if now := time.Now(); now.Sub(s.lastReport) >= s.cfg.progressEvery {
		s.lastReport = now
		s.cfg.progressReport(s.Progress())
	}
}

func (s *TrialStream) reportFinal() {
	if s.cfg.progressReport == nil {
		return
	}
	s.lastReport = time.Now()
	s.cfg.progressReport(s.Progress())
}

// rateMemory is the time constant for recent throughput.
const rateMemory = 2 * time.Second

// rateMeter exponentially smooths trial and request throughput. It decays cost
// and elapsed time together, making the result independent of observation
// cadence. The stream's progress mutex guards it.
type rateMeter struct {
	lastTrials, lastRequests int
	lastTime                 time.Time

	// Decayed cost, and the decayed wall time it was accrued over.
	trials, requests, window float64
}

// newRateMeter returns a meter measuring from start (the run's start), when
// cost is zero.
func newRateMeter(start time.Time) rateMeter {
	return rateMeter{lastTime: start}
}

// observe folds the running cost at now into the decayed totals. A gap of zero
// or less contributes nothing: it carries no time to attribute cost to.
func (m *rateMeter) observe(trials, requests int, now time.Time) {
	d := now.Sub(m.lastTime).Seconds()
	if d <= 0 {
		return
	}
	w := math.Exp(-d / rateMemory.Seconds())
	m.trials = w*m.trials + float64(trials-m.lastTrials)
	m.requests = w*m.requests + float64(requests-m.lastRequests)
	m.window = w*m.window + d
	m.lastTrials, m.lastRequests, m.lastTime = trials, requests, now
}

// rate returns the current smoothed trials- and requests-per-second, each NaN
// until an observation has covered some wall time.
func (m *rateMeter) rate() (trialsPerSecond, requestsPerSecond float64) {
	if m.window == 0 {
		return math.NaN(), math.NaN()
	}
	return m.trials / m.window, m.requests / m.window
}
