package tth2

import (
	"context"
	"math"
	"sync"
	"time"
)

// paceConfig describes independent ceilings on request and batch starts. A
// zero field adds no constraint.
type paceConfig struct {
	requestsPerSecond float64
	minBatchInterval  time.Duration
}

func (c paceConfig) active() bool {
	return c.requestsPerSecond > 0 || c.minBatchInterval > 0
}

// paceState is one clock in the unified scheduler. The origin owns one and a
// trial run may own another. last is the actual scheduler release, not a
// planned deadline, so a late wake cannot create catch-up admissions.
type paceState struct {
	config  paceConfig
	last    time.Time
	started bool
}

func (s *paceState) readyAt(width int) time.Time {
	if !s.started {
		return time.Time{}
	}
	ready := s.last.Add(s.config.minBatchInterval)
	if d := requestInterval(width, s.config.requestsPerSecond); d > 0 {
		ready = laterTime(ready, s.last.Add(d))
	}
	return ready
}

func requestInterval(width int, perSecond float64) time.Duration {
	if width <= 0 || perSecond <= 0 || math.IsInf(perSecond, 1) {
		return 0
	}
	nanos := math.Ceil(float64(width) * float64(time.Second) / perSecond)
	if nanos >= float64(math.MaxInt64) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(nanos)
}

func laterTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return b
	}
	return a
}

// paceRun is the private clock shared by every worker in one trial run. A nil
// run means the admission has origin constraints only.
type paceRun struct {
	state  paceState
	queue  []*paceAdmission
	closed bool
}

type paceAdmission struct {
	ctx       context.Context
	width     int
	seq       uint64
	granted   chan struct{}
	cancelled bool
}

// paceScheduler serialises batch starts for one origin. Its loop exists while
// work is queued or a trial run holds a lease. Direct SendBatch calls leave no
// background goroutine behind once their admission is granted.
type paceScheduler struct {
	clock schedulerClock
	wake  chan struct{}

	mu          sync.Mutex
	running     bool
	origin      paceState
	direct      paceRun
	runs        map[*paceRun]struct{}
	nextSeq     uint64
	selected    *paceAdmission
	selectedRun *paceRun
}

func newPaceScheduler(config paceConfig) *paceScheduler {
	return newPaceSchedulerWithClock(config, realSchedulerClock{})
}

func newPaceSchedulerWithClock(
	config paceConfig,
	clock schedulerClock,
) *paceScheduler {
	s := &paceScheduler{
		clock:  clock,
		wake:   make(chan struct{}, 1),
		origin: paceState{config: config},
		runs:   make(map[*paceRun]struct{}),
	}
	s.runs[&s.direct] = struct{}{}
	return s
}

func (s *paceScheduler) newRun(config paceConfig) *paceRun {
	r := &paceRun{state: paceState{config: config}}
	s.mu.Lock()
	s.runs[r] = struct{}{}
	s.mu.Unlock()
	return r
}

func (s *paceScheduler) wait(
	ctx context.Context,
	run *paceRun,
	width int,
) error {
	if run == nil {
		run = &s.direct
	}
	a := &paceAdmission{
		ctx:     ctx,
		width:   width,
		granted: make(chan struct{}),
	}
	s.mu.Lock()
	a.seq = s.nextSeq
	s.nextSeq++
	run.queue = append(run.queue, a)
	s.startLocked()
	s.mu.Unlock()
	s.notify()

	select {
	case <-a.granted:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		select {
		case <-a.granted:
			s.mu.Unlock()
			return nil
		default:
			a.cancelled = true
		}
		s.mu.Unlock()
		s.notify()
		return ctx.Err()
	}
}

func (s *paceScheduler) startLocked() {
	if s.running {
		return
	}
	s.running = true
	go s.loop()
}

func (s *paceScheduler) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *paceScheduler) loop() {
	for {
		wait, idle := s.step()
		if idle {
			return
		}
		if wait <= 0 {
			continue
		}
		timer := s.clock.NewTimer(wait)
		select {
		case <-timer.C():
		case <-s.wake:
			if !timer.Stop() {
				select {
				case <-timer.C():
				default:
				}
			}
		}
	}
}

// step grants at most one admission and reports how long the loop should wait.
// idle is true only when no trial lease or queued direct call needs the loop.
func (s *paceScheduler) step() (wait time.Duration, idle bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()

	s.pruneCancelled()
	if s.selected == nil {
		earliest := s.selectAdmission(now)
		if s.selected == nil && !earliest.IsZero() {
			return earliest.Sub(now), false
		}
	}
	if s.selected == nil {
		if len(s.runs) == 1 {
			s.running = false
			return 0, true
		}
		return 24 * time.Hour, false
	}
	if s.selected.cancelled || s.selected.ctx.Err() != nil {
		s.removeSelected()
		return 0, false
	}

	ready := laterTime(
		s.origin.readyAt(s.selected.width),
		s.selectedRun.state.readyAt(s.selected.width),
	)
	if ready.After(now) {
		return ready.Sub(now), false
	}

	granted := s.selected
	s.origin.last, s.origin.started = now, true
	if s.selectedRun.state.config.active() {
		s.selectedRun.state.last = now
		s.selectedRun.state.started = true
	}
	s.removeSelected()
	if len(s.runs) == 1 && !s.hasQueuedAdmissions() {
		s.running = false
		idle = true
	}
	close(granted.granted)
	return 0, idle
}

func (s *paceScheduler) pruneCancelled() {
	for run := range s.runs {
		for len(run.queue) > 0 &&
			(run.queue[0].cancelled || run.queue[0].ctx.Err() != nil) {
			if run.queue[0] == s.selected {
				s.selected, s.selectedRun = nil, nil
			}
			run.queue = run.queue[1:]
		}
		if run != &s.direct && run.closed && len(run.queue) == 0 {
			delete(s.runs, run)
		}
	}
}

func (s *paceScheduler) hasQueuedAdmissions() bool {
	for run := range s.runs {
		if len(run.queue) > 0 {
			return true
		}
	}
	return false
}

func (s *paceScheduler) selectAdmission(now time.Time) time.Time {
	var earliest time.Time
	for run := range s.runs {
		if len(run.queue) == 0 {
			continue
		}
		a := run.queue[0]
		ready := run.state.readyAt(a.width)
		if ready.After(now) {
			if earliest.IsZero() || ready.Before(earliest) {
				earliest = ready
			}
			continue
		}
		if s.selected == nil || a.seq < s.selected.seq {
			s.selected, s.selectedRun = a, run
		}
	}
	return earliest
}

func (s *paceScheduler) removeSelected() {
	run := s.selectedRun
	if run != nil && len(run.queue) > 0 && run.queue[0] == s.selected {
		run.queue = run.queue[1:]
	}
	s.selected, s.selectedRun = nil, nil
	if run != nil && run != &s.direct && run.closed && len(run.queue) == 0 {
		delete(s.runs, run)
	}
}

type schedulerTimer interface {
	C() <-chan time.Time
	Stop() bool
}

type schedulerClock interface {
	Now() time.Time
	NewTimer(time.Duration) schedulerTimer
}

type realSchedulerClock struct{}

func (realSchedulerClock) Now() time.Time { return time.Now() }

func (realSchedulerClock) NewTimer(d time.Duration) schedulerTimer {
	return realSchedulerTimer{Timer: time.NewTimer(d)}
}

type realSchedulerTimer struct{ *time.Timer }

func (t realSchedulerTimer) C() <-chan time.Time { return t.Timer.C }

// paceLease is the scheduler state for one send path. Its zero value is the
// allocation-free, callback-free unlimited path.
type paceLease struct {
	scheduler *paceScheduler
	run       *paceRun
}

func (p *originPool) leasePace(private paceConfig) paceLease {
	p.paceMu.Lock()
	defer p.paceMu.Unlock()
	if p.pacer == nil {
		if !p.paceConfig.active() && !private.active() {
			return paceLease{}
		}
		p.pacer = newPaceScheduler(p.paceConfig)
	}
	lease := paceLease{scheduler: p.pacer}
	if private.active() {
		lease.run = p.pacer.newRun(private)
	}
	return lease
}

func (l paceLease) beforeFirstFlush(
	ctx context.Context,
	width int,
) func() error {
	if l.scheduler == nil {
		return nil
	}
	return func() error { return l.scheduler.wait(ctx, l.run, width) }
}

func (l paceLease) close() {
	if l.scheduler == nil || l.run == nil {
		return
	}
	l.scheduler.mu.Lock()
	l.run.closed = true
	if len(l.run.queue) == 0 {
		delete(l.scheduler.runs, l.run)
	}
	l.scheduler.mu.Unlock()
	l.scheduler.notify()
}
