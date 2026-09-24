package tth2

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tantosec/tturl/internal/h2test"
)

func TestRequestInterval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		width int
		rate  float64
		want  time.Duration
	}{
		{"disabled", 2, 0, 0},
		{"one request", 1, 100, 10 * time.Millisecond},
		{"whole batch", 8, 100, 80 * time.Millisecond},
		{"rounds up", 1, 3, 333333334 * time.Nanosecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := requestInterval(tt.width, tt.rate); got != tt.want {
				t.Errorf("requestInterval(%d, %v) = %v, want %v",
					tt.width, tt.rate, got, tt.want)
			}
		})
	}
}

func TestPaceStateMixedWidths(t *testing.T) {
	t.Parallel()
	base := time.Unix(0, 0)
	tests := []struct {
		name   string
		widths []int
		wantMS []int
	}{
		{"narrow then wide", []int{2, 8, 2}, []int{0, 80, 100}},
		{"wide then narrow", []int{8, 2, 8}, []int{0, 20, 100}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			state := paceState{config: paceConfig{
				requestsPerSecond: 100,
				minBatchInterval:  20 * time.Millisecond,
			}}
			for i, width := range tt.widths {
				ready := state.readyAt(width)
				if !state.started {
					ready = base
				}
				want := base.Add(time.Duration(tt.wantMS[i]) * time.Millisecond)
				if !ready.Equal(want) {
					t.Errorf("batch %d ready = %v, want %v", i, ready, want)
				}
				state.last, state.started = ready, true
			}
		})
	}
}

// TestPaceSchedulerReleaseTrace exercises the scheduler as a whole against
// independently derived release times. It covers each ceiling alone, both
// possible winners when ceilings combine, and the actual width of every batch.
func TestPaceSchedulerReleaseTrace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		config paceConfig
		widths []int
		wantMS []int
	}{
		{
			name: "batch ceiling",
			config: paceConfig{
				minBatchInterval: 50 * time.Millisecond,
			},
			widths: []int{2, 2, 2}, wantMS: []int{0, 50, 100},
		},
		{
			name: "request ceiling",
			config: paceConfig{
				requestsPerSecond: 100,
			},
			widths: []int{2, 2, 2}, wantMS: []int{0, 20, 40},
		},
		{
			name: "batch ceiling tighter",
			config: paceConfig{
				requestsPerSecond: 100,
				minBatchInterval:  50 * time.Millisecond,
			},
			widths: []int{2, 2, 2}, wantMS: []int{0, 50, 100},
		},
		{
			name: "request ceiling tighter with changing width",
			config: paceConfig{
				requestsPerSecond: 100,
				minBatchInterval:  20 * time.Millisecond,
			},
			widths: []int{2, 8, 2}, wantMS: []int{0, 80, 100},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			clock := newManualSchedulerClock()
			s := newPaceSchedulerWithClock(test.config, clock)
			elapsed := 0
			for i, width := range test.widths {
				admission := beginAdmission(s, nil, width)
				delta := test.wantMS[i] - elapsed
				if delta > 0 {
					clock.waitForTimer(
						t, time.Duration(delta)*time.Millisecond)
					wantPending(t, admission)
					clock.Advance(time.Duration(delta) * time.Millisecond)
				}
				wantAdmission(t, admission)
				elapsed = test.wantMS[i]
			}
		})
	}
}

// TestRunTrialsBatchIntervalIncludesWarmupTransition verifies that priming and
// measured work use one pacing sequence under the manual clock.
func TestRunTrialsBatchIntervalIncludesWarmupTransition(t *testing.T) {
	t.Parallel()
	const interval = 30 * time.Millisecond
	started := make(chan struct{}, 3)
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		w.WriteHeader(http.StatusOK)
	})
	addr, tlsConfig := h2test.Serve(t, handler)
	transport := &Transport{TLSClientConfig: tlsConfig}
	t.Cleanup(transport.CloseIdleConnections)

	clock := newManualSchedulerClock()
	endpoint := "https://" + addr + "/"
	pool := transport.pool(addr)
	pool.paceMu.Lock()
	pool.pacer = newPaceSchedulerWithClock(pool.paceConfig, clock)
	pool.paceMu.Unlock()

	type runResult struct {
		summary *TrialSummary
		err     error
	}
	done := make(chan runResult, 1)
	go func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
			endpoint, nil)
		if err != nil {
			done <- runResult{err: err}
			return
		}
		summary, err := (&Client{Transport: transport}).RunTrials(
			t.Context(), []*http.Request{req}, 2,
			WithWarmup(1), WithMinBatchInterval(interval))
		done <- runResult{summary: summary, err: err}
	}()

	wantBatchStart(t, started)
	for transition := 1; transition <= 2; transition++ {
		clock.waitForTimer(t, interval)
		wantNoBatchStart(t, started, transition)
		clock.Advance(interval)
		wantBatchStart(t, started)
	}

	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("RunTrials: %v", result.err)
		}
		if result.summary.Trials != 2 || result.summary.FailedTrials != 0 {
			t.Errorf("summary trials/failed = %d/%d, want 2/0",
				result.summary.Trials, result.summary.FailedTrials)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunTrials did not complete")
	}
}

// TestProgressRequestsAdvanceAtPacingRelease distinguishes queued work from
// offered work. Requests advances after admission and before HEADERS reach the
// handler.
func TestProgressRequestsAdvanceAtPacingRelease(t *testing.T) {
	t.Parallel()
	const interval = 200 * time.Millisecond
	started := make(chan struct{}, 2)
	releaseSecond := make(chan struct{})
	var releaseSecondOnce sync.Once
	allowSecond := func() { releaseSecondOnce.Do(func() { close(releaseSecond) }) }
	t.Cleanup(allowSecond)
	var handled atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		if handled.Add(1) == 2 {
			<-releaseSecond
		}
		w.WriteHeader(http.StatusNoContent)
	})
	addr, tlsConfig := h2test.Serve(t, handler)
	transport := &Transport{TLSClientConfig: tlsConfig}
	t.Cleanup(transport.CloseIdleConnections)

	clock := newManualSchedulerClock()
	pool := transport.pool(addr)
	pool.paceMu.Lock()
	pool.pacer = newPaceSchedulerWithClock(pool.paceConfig, clock)
	pool.paceMu.Unlock()

	stream := (&Client{Transport: transport}).StreamTrials(t.Context(),
		[]*http.Request{mustReq(t, "https://"+addr+"/")},
		WithMaxTrials(2), WithMinBatchInterval(interval))
	trials := make(chan *Trial)
	go func() {
		defer close(trials)
		for trial := range stream.All() {
			trials <- trial
		}
	}()

	wantBatchStart(t, started)
	first := <-trials
	if first.Err != nil {
		t.Fatalf("first trial: %v", first.Err)
	}
	clock.waitForTimer(t, interval)
	if got := stream.Progress().Requests; got != 1 {
		t.Fatalf("requests while second batch queued = %d, want 1", got)
	}
	wantNoBatchStart(t, started, 1)

	clock.Advance(interval)
	wantBatchStart(t, started)
	if got := stream.Progress().Requests; got != 2 {
		t.Errorf("requests at second handler start = %d, want 2", got)
	}
	allowSecond()
	second := <-trials
	if second.Err != nil {
		t.Errorf("second trial: %v", second.Err)
	}
	for range trials { //nolint:revive // drain verifies clean stream shutdown
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("StreamTrials: %v", err)
	}
}

// TestBatchPacingFollowsConnectionAcquisition reproduces a saturated pool.
// The manual clock verifies that work blocked on the pool receives a fresh
// pacing slot after a connection becomes available.
func TestBatchPacingFollowsConnectionAcquisition(t *testing.T) {
	t.Parallel()
	const (
		conns    = 1
		calls    = 3
		interval = 25 * time.Millisecond
	)

	started := make(chan struct{}, calls)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHandlers := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHandlers)
	var handled atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := handled.Add(1)
		started <- struct{}{}
		if n <= conns {
			<-release
		}
		w.WriteHeader(http.StatusOK)
	})
	addr, tlsConfig := h2test.Serve(t, handler)
	transport := &Transport{
		TLSClientConfig:         tlsConfig,
		MaxConnsPerHost:         conns,
		MinBatchIntervalPerHost: interval,
	}
	t.Cleanup(transport.CloseIdleConnections)

	clock := newManualSchedulerClock()
	pool := transport.pool(addr)
	pool.paceMu.Lock()
	pool.pacer = newPaceSchedulerWithClock(pool.paceConfig, clock)
	scheduler := pool.pacer
	pool.paceMu.Unlock()

	errCh := make(chan error, calls)
	acquiring := make(chan struct{}, calls)
	requests := make([]*http.Request, calls)
	for i := range calls {
		requests[i] = mustReq(t, "https://"+addr+"/")
	}
	for i := range calls {
		go func() {
			ctx := &observedContext{
				Context:  t.Context(),
				observed: acquiring,
			}
			_, err := (&Client{Transport: transport}).SendBatch(ctx,
				[]*http.Request{requests[i]})
			errCh <- err
		}()
	}
	for range calls {
		<-acquiring
	}

	// Fill the pool one exact pacing slot at a time.
	wantBatchStart(t, started)
	for range conns - 1 {
		clock.waitForTimer(t, interval)
		clock.Advance(interval)
		wantBatchStart(t, started)
	}

	// If work were admitted before acquiring a connection, it would have timers
	// queued while every connection is blocked. Drain any such timers before
	// releasing the pool; the correct implementation is already idle here.
	clock.Advance(time.Duration(calls-conns+1) * interval)
	drainSchedulerTimers(t, scheduler, clock)
	releaseHandlers()

	// The first newly acquired call is eligible immediately after the artificial
	// stall. Every sibling must then wait for a distinct scheduler release. The
	// assertion observes the pacing clock itself: handler wake-up latency is not
	// part of the pacing contract and can be heavily distorted by the race
	// detector.
	wantBatchStart(t, started)
	wantSchedulerReleaseAt(t, scheduler, clock.Now())
	for range calls - conns - 1 {
		clock.waitForTimer(t, interval)
		before := clock.Now()
		wantSchedulerReleaseAt(t, scheduler, before)
		clock.Advance(interval)
		wantSchedulerReleaseAt(t, scheduler, before.Add(interval))
	}
	for range calls - conns - 1 {
		wantBatchStart(t, started)
	}
	for range calls {
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("SendBatch: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("SendBatch did not complete")
		}
	}
}

// TestPaceSchedulerNoCatchUp uses a controlled monotonic clock to verify that a
// late timer wake shifts the next deadline instead of releasing stored work in
// a burst.
func TestPaceSchedulerNoCatchUp(t *testing.T) {
	t.Parallel()
	clock := newManualSchedulerClock()
	s := newPaceSchedulerWithClock(paceConfig{
		minBatchInterval: 50 * time.Millisecond,
	}, clock)

	first := beginAdmission(s, nil, 1)
	wantAdmission(t, first)
	second := beginAdmission(s, nil, 1)
	clock.waitForTimer(t, 50*time.Millisecond)
	wantPending(t, second)

	clock.Advance(200 * time.Millisecond)
	wantAdmission(t, second)
	third := beginAdmission(s, nil, 1)
	clock.waitForTimer(t, 50*time.Millisecond)
	wantPending(t, third)
	clock.Advance(49 * time.Millisecond)
	wantPending(t, third)
	clock.Advance(time.Millisecond)
	wantAdmission(t, third)
}

// TestPaceSchedulerCancellationDoesNotAdvanceClock verifies that a cancelled
// selected admission spends no interval and the next queued batch can leave at
// the original deadline.
func TestPaceSchedulerCancellationDoesNotAdvanceClock(t *testing.T) {
	t.Parallel()
	clock := newManualSchedulerClock()
	s := newPaceSchedulerWithClock(paceConfig{
		minBatchInterval: 50 * time.Millisecond,
	}, clock)
	wantAdmission(t, beginAdmission(s, nil, 1))

	ctx, cancel := context.WithCancel(t.Context())
	cancelled := beginAdmissionContext(s, ctx, nil, 1)
	clock.waitForTimer(t, 50*time.Millisecond)
	previousTimers := clock.timerCount()
	cancel()
	if err := <-cancelled; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled admission = %v, want context.Canceled", err)
	}

	next := beginAdmission(s, nil, 1)
	// Wait for the scheduler to consume the cancellation wake and install the
	// successor's timer. Advancing before that handoff can move a manual clock
	// past the instant from which the timer is subsequently calculated.
	clock.waitForTimerAfter(t, 50*time.Millisecond, previousTimers)
	clock.Advance(50 * time.Millisecond)
	wantAdmission(t, next)
}

func TestPaceSchedulerCombinesOriginAndRunIntervals(t *testing.T) {
	t.Parallel()
	clock := newManualSchedulerClock()
	s := newPaceSchedulerWithClock(paceConfig{
		minBatchInterval: 20 * time.Millisecond,
	}, clock)
	run := s.newRun(paceConfig{minBatchInterval: 50 * time.Millisecond})
	wantAdmission(t, beginAdmission(s, run, 1))

	direct := beginAdmission(s, nil, 1)
	clock.waitForTimer(t, 20*time.Millisecond)
	clock.Advance(20 * time.Millisecond)
	wantAdmission(t, direct)

	runAgain := beginAdmission(s, run, 1)
	clock.waitForTimer(t, 30*time.Millisecond)
	clock.Advance(29 * time.Millisecond)
	wantPending(t, runAgain)
	clock.Advance(time.Millisecond)
	wantAdmission(t, runAgain)
}

// TestPaceSchedulerSelectedWideBatchOwnsSlot verifies the fairness
// rule. Once an older wide batch is selected, a younger narrow batch cannot use
// an earlier request-rate slot and repeatedly postpone the wide one.
func TestPaceSchedulerSelectedWideBatchOwnsSlot(t *testing.T) {
	t.Parallel()
	clock := newManualSchedulerClock()
	s := newPaceSchedulerWithClock(paceConfig{requestsPerSecond: 100}, clock)
	wantAdmission(t, beginAdmission(s, nil, 1))

	wideRun := s.newRun(paceConfig{})
	narrowRun := s.newRun(paceConfig{})
	wide := beginAdmission(s, wideRun, 8)
	clock.waitForTimer(t, 80*time.Millisecond)
	previousTimers := clock.timerCount()
	narrow := beginAdmission(s, narrowRun, 2)
	// Enqueuing the narrow batch wakes the scheduler. Observe the replacement
	// timer before advancing so the manual clock cannot move between the
	// scheduler calculating the wide batch's deadline and installing its timer.
	clock.waitForTimerAfter(t, 80*time.Millisecond, previousTimers)
	clock.Advance(20 * time.Millisecond)
	wantPending(t, wide)
	wantPending(t, narrow)
	clock.waitForTimer(t, 60*time.Millisecond)
	clock.Advance(60 * time.Millisecond)
	wantAdmission(t, wide)

	clock.waitForTimer(t, 20*time.Millisecond)
	clock.Advance(20 * time.Millisecond)
	wantAdmission(t, narrow)
}

// TestPaceSchedulerIdleRestart verifies that an origin-only scheduler releases
// its goroutine after direct work while retaining the pacing clock needed by a
// later call. The manual clock makes both lifecycle and spacing exact.
func TestPaceSchedulerIdleRestart(t *testing.T) {
	t.Parallel()
	clock := newManualSchedulerClock()
	s := newPaceSchedulerWithClock(paceConfig{
		minBatchInterval: 50 * time.Millisecond,
	}, clock)
	wantAdmission(t, beginAdmission(s, nil, 1))

	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if running {
		t.Fatal("scheduler loop remained active with no work or trial lease")
	}

	second := beginAdmission(s, nil, 1)
	clock.waitForTimer(t, 50*time.Millisecond)
	wantPending(t, second)
	clock.Advance(50 * time.Millisecond)
	wantAdmission(t, second)
}

// TestPaceLeaseCloseDrainsQueuedAdmission verifies that closing a run lease
// cannot unregister work already waiting in the scheduler.
func TestPaceLeaseCloseDrainsQueuedAdmission(t *testing.T) {
	t.Parallel()
	clock := newManualSchedulerClock()
	s := newPaceSchedulerWithClock(paceConfig{
		minBatchInterval: 50 * time.Millisecond,
	}, clock)
	run := s.newRun(paceConfig{})
	lease := paceLease{scheduler: s, run: run}
	wantAdmission(t, beginAdmission(s, run, 1))

	queued := beginAdmission(s, run, 1)
	clock.waitForTimer(t, 50*time.Millisecond)
	previousTimers := clock.timerCount()
	lease.close()
	wantPending(t, queued)
	// Closing the lease wakes the scheduler but must leave its queued admission
	// registered. Observe the replacement timer before advancing the manual
	// clock; its presence also verifies that the closed run is still draining.
	clock.waitForTimerAfter(t, 50*time.Millisecond, previousTimers)
	clock.Advance(50 * time.Millisecond)
	wantAdmission(t, queued)

	s.mu.Lock()
	_, registered := s.runs[run]
	running := s.running
	s.mu.Unlock()
	if registered || running {
		t.Errorf("closed run registered/running = %t/%t, want false/false",
			registered, running)
	}
}

func TestPaceLeaseConfiguration(t *testing.T) {
	t.Parallel()
	limited := (&Transport{
		MaxRequestsPerSecondPerHost: 41,
		MinBatchIntervalPerHost:     23 * time.Millisecond,
	}).pool("limited")
	lease := limited.leasePace(paceConfig{
		requestsPerSecond: 37,
		minBatchInterval:  29 * time.Millisecond,
	})
	if lease.scheduler == nil || lease.run == nil {
		t.Fatal("limited lease has no origin scheduler or private run")
	}
	if got := lease.scheduler.origin.config.requestsPerSecond; got != 41 {
		t.Errorf("origin request rate = %v, want 41", got)
	}
	if got := lease.scheduler.origin.config.minBatchInterval; got != 23*time.Millisecond {
		t.Errorf("origin batch interval = %v, want 23ms", got)
	}
	if got := lease.run.state.config.requestsPerSecond; got != 37 {
		t.Errorf("run request rate = %v, want 37", got)
	}
	if got := lease.run.state.config.minBatchInterval; got != 29*time.Millisecond {
		t.Errorf("run batch interval = %v, want 29ms", got)
	}
	if unlimited := new(originPool).leasePace(paceConfig{}); unlimited.scheduler != nil {
		t.Errorf("unlimited lease scheduler = %p, want nil", unlimited.scheduler)
	}
}

func BenchmarkPaceScheduler(b *testing.B) {
	b.Run("disabled fast path", func(b *testing.B) {
		lease := paceLease{}
		ctx := context.Background()
		b.ReportAllocs()
		for b.Loop() {
			if wait := lease.beforeFirstFlush(ctx, 2); wait != nil {
				b.Fatal("disabled pacing returned a callback")
			}
		}
	})

	b.Run("one active run", func(b *testing.B) {
		s := newPaceScheduler(paceConfig{})
		run := s.newRun(paceConfig{minBatchInterval: time.Nanosecond})
		b.Cleanup((paceLease{scheduler: s, run: run}).close)
		ctx := context.Background()
		b.ReportAllocs()
		for b.Loop() {
			if err := s.wait(ctx, run, 2); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("many workers", func(b *testing.B) {
		s := newPaceScheduler(paceConfig{
			minBatchInterval: time.Nanosecond,
		})
		ctx := context.Background()
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				if err := s.wait(ctx, nil, 2); err != nil {
					b.Error(err)
					return
				}
			}
		})
	})
}

func beginAdmission(
	s *paceScheduler,
	run *paceRun,
	width int,
) <-chan error {
	return beginAdmissionContext(s, context.Background(), run, width)
}

func beginAdmissionContext(
	s *paceScheduler,
	ctx context.Context,
	run *paceRun,
	width int,
) <-chan error {
	done := make(chan error, 1)
	go func() { done <- s.wait(ctx, run, width) }()
	return done
}

func wantAdmission(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("admission failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("admission did not complete")
	}
}

func wantBatchStart(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("batch did not start")
	}
}

func wantNoBatchStart(t *testing.T, started <-chan struct{}, transition int) {
	t.Helper()
	select {
	case <-started:
		t.Fatalf("batch crossed pacing transition %d before admission", transition)
	default:
	}
}

func wantPending(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("admission completed early: %v", err)
	default:
	}
}

func wantSchedulerReleaseAt(
	t *testing.T,
	scheduler *paceScheduler,
	want time.Time,
) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		scheduler.mu.Lock()
		last, started := scheduler.origin.last, scheduler.origin.started
		scheduler.mu.Unlock()
		if started && last.Equal(want) {
			return
		}
		if started && last.After(want) {
			t.Fatalf("scheduler released at %v, passed expected time %v", last, want)
		}
		time.Sleep(time.Millisecond)
	}
	scheduler.mu.Lock()
	last, started := scheduler.origin.last, scheduler.origin.started
	scheduler.mu.Unlock()
	t.Fatalf("scheduler last release = %v (started %t), want %v",
		last, started, want)
}

// observedContext reports the first cancellation check made for a SendBatch
// call. SendBatch performs that check when originPool.acquire begins, before it
// waits for a connection token. Waiting for every report therefore verifies all
// calls have reached the acquisition boundary before the test examines the
// pacing scheduler.
type observedContext struct {
	context.Context
	once     sync.Once
	observed chan<- struct{}
}

func (c *observedContext) Done() <-chan struct{} {
	c.observe()
	return c.Context.Done()
}

func (c *observedContext) Err() error {
	c.observe()
	return c.Context.Err()
}

func (c *observedContext) observe() {
	c.once.Do(func() { c.observed <- struct{}{} })
}

// drainSchedulerTimers advances every timer installed while callers are
// otherwise blocked. Every caller has already reported reaching the acquisition
// boundary, so an idle scheduler is final rather than a timing assumption.
func drainSchedulerTimers(
	t *testing.T,
	scheduler *paceScheduler,
	clock *manualSchedulerClock,
) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		scheduler.mu.Lock()
		running := scheduler.running
		scheduler.mu.Unlock()

		clock.mu.Lock()
		var (
			next    time.Duration
			pending bool
		)
		for _, timer := range clock.timers {
			if timer.stopped || timer.fired {
				continue
			}
			wait := timer.at.Sub(clock.now)
			if !pending || wait < next {
				next, pending = wait, true
			}
		}
		clock.mu.Unlock()

		if pending {
			if next < 0 {
				next = 0
			}
			clock.Advance(next)
			time.Sleep(time.Millisecond)
			continue
		}
		if !running {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("pacing scheduler did not become idle")
}

type manualSchedulerClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*manualSchedulerTimer
}

func newManualSchedulerClock() *manualSchedulerClock {
	return &manualSchedulerClock{now: time.Unix(0, 0)}
}

func (c *manualSchedulerClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualSchedulerClock) NewTimer(d time.Duration) schedulerTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &manualSchedulerTimer{
		clock: c,
		at:    c.now.Add(d),
		ch:    make(chan time.Time, 1),
	}
	c.timers = append(c.timers, t)
	return t
}

func (c *manualSchedulerClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	for _, timer := range c.timers {
		if !timer.stopped && !timer.fired && !timer.at.After(c.now) {
			timer.fired = true
			timer.ch <- c.now
		}
	}
	c.mu.Unlock()
}

func (c *manualSchedulerClock) waitForTimer(
	t *testing.T,
	d time.Duration,
) {
	c.waitForTimerAfter(t, d, 0)
}

func (c *manualSchedulerClock) timerCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

func (c *manualSchedulerClock) waitForTimerAfter(
	t *testing.T,
	d time.Duration,
	after int,
) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		for _, timer := range c.timers[after:] {
			if !timer.stopped && !timer.fired &&
				timer.at.Sub(c.now) == d {
				c.mu.Unlock()
				return
			}
		}
		c.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("scheduler did not install a %v timer", d)
}

type manualSchedulerTimer struct {
	clock   *manualSchedulerClock
	at      time.Time
	ch      chan time.Time
	stopped bool
	fired   bool
}

func (t *manualSchedulerTimer) C() <-chan time.Time { return t.ch }

func (t *manualSchedulerTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	wasActive := !t.stopped && !t.fired
	t.stopped = true
	return wasActive
}
