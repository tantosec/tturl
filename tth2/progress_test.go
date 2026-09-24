package tth2_test

import (
	"context"
	"math"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/tantosec/tturl/internal/h2test"
	"github.com/tantosec/tturl/tth2"
)

// TestProgressCountsEveryRequestARunSpends verifies measured, warmup, and
// request counts for a bounded single-connection run.
func TestProgressCountsEveryRequestARunSpends(t *testing.T) {
	t.Parallel()
	const (
		n      = 2
		trials = 4
		warmup = 3
	)
	addr, tlsCfg := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		// Keep successive completions distinct even on platforms whose monotonic
		// clock resolution is coarser than the requested progress interval.
		time.Sleep(20 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	c := newClientMaxConns(t, tlsCfg, 1)
	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/a"),
		mustReq(t, "https://"+addr+"/b"),
	}

	var last tth2.TrialProgress
	reports := 0
	_, err := c.RunTrials(t.Context(), reqs, trials,
		tth2.WithWarmup(warmup),
		// A floor below any real trial, so a report lands on every completion.
		tth2.WithProgress(time.Nanosecond, func(p tth2.TrialProgress) {
			reports++
			last = p
		}))
	if err != nil {
		t.Fatalf("RunTrials: %v", err)
	}

	if last.Trials != trials {
		t.Errorf("Trials = %d, want %d", last.Trials, trials)
	}
	if last.Warmup != warmup {
		t.Errorf("Warmup = %d, want %d: one connection primes once", last.Warmup, warmup)
	}
	if want := (trials + warmup) * n; last.Requests != want {
		t.Errorf("Requests = %d, want %d: every trial costs the batch's width", last.Requests, want)
	}
	// Every completion is a report at this cadence, plus the final one.
	if want := trials + warmup + 1; reports != want {
		t.Errorf("delivered %d reports, want %d (one per completion, then the final)", reports, want)
	}
	if math.IsNaN(last.RecentTrialsPerSecond) || math.IsNaN(last.RecentRequestsPerSecond) {
		t.Errorf("rates = (%v, %v), want a measurable rate by the end of the run",
			last.RecentTrialsPerSecond, last.RecentRequestsPerSecond)
	}
}

// TestProgressBeforeARunStarts verifies zero counts and NaN rates before work.
func TestProgressBeforeARunStarts(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, okHandler)
	c := newClient(t, tlsCfg)
	s := c.StreamTrials(t.Context(), []*http.Request{mustReq(t, "https://"+addr+"/")},
		tth2.WithMaxTrials(1))

	p := s.Progress()
	if p.Trials != 0 || p.Warmup != 0 || p.Requests != 0 {
		t.Errorf("Progress() = %+v, want zero counts before the run starts", p)
	}
	if !math.IsNaN(p.RecentTrialsPerSecond) || !math.IsNaN(p.RecentRequestsPerSecond) {
		t.Errorf("rates = (%v, %v), want NaN: no wall time has been covered",
			p.RecentTrialsPerSecond, p.RecentRequestsPerSecond)
	}
}

// TestProgressPolledWhileRanging verifies monotone concurrent snapshots and
// that dispatched request counts lead completed trials.
func TestProgressPolledWhileRanging(t *testing.T) {
	t.Parallel()
	const (
		n      = 2
		trials = 12
	)
	addr, tlsCfg := h2test.Serve(t, okHandler)
	c := newClientMaxConns(t, tlsCfg, 2)
	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/a"),
		mustReq(t, "https://"+addr+"/b"),
	}
	s := c.StreamTrials(t.Context(), reqs, tth2.WithWarmup(1), tth2.WithMaxTrials(trials))

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		prev := s.Progress()
		for {
			select {
			case <-done:
				return
			default:
			}
			p := s.Progress()
			if p.Trials < prev.Trials || p.Warmup < prev.Warmup || p.Requests < prev.Requests {
				t.Errorf("progress went backwards: %+v then %+v", prev, p)
				return
			}
			if want := (p.Trials + p.Warmup) * n; p.Requests < want {
				t.Errorf("Requests = %d, want at least %d for %d completed trials",
					p.Requests, want, p.Trials+p.Warmup)
				return
			}
			prev = p
		}
	})

	for range s.All() { //nolint:revive // ranged for its progress, not its trials
	}
	close(done)
	wg.Wait()

	if err := s.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	p := s.Progress()
	if p.Trials != trials {
		t.Errorf("Trials = %d, want %d", p.Trials, trials)
	}
	// Priming runs once per connection, and the run's fan-out is its own to
	// choose, so the arithmetic holds against Conns rather than a fixed number.
	if p.Warmup < 1 || p.Warmup > s.Conns() {
		t.Errorf("Warmup = %d, want between 1 and Conns() = %d", p.Warmup, s.Conns())
	}
	if want := (p.Trials + p.Warmup) * n; p.Requests != want {
		t.Errorf("Requests = %d, want %d", p.Requests, want)
	}
}

// TestProgressChargesOnlyAttemptsThatReachTheDispatchGate verifies that a
// stale connection rejected before dispatch is not charged as a request.
func TestProgressChargesOnlyAttemptsThatReachTheDispatchGate(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, okHandler)
	var (
		mu      sync.Mutex
		dialled []net.Conn
	)
	dialer := func(
		ctx context.Context,
		network string,
		address string,
	) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err == nil {
			mu.Lock()
			dialled = append(dialled, conn)
			mu.Unlock()
		}
		return conn, err
	}
	transport := &tth2.Transport{
		TLSClientConfig: tlsCfg,
		DialContext:     dialer,
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := &tth2.Client{Transport: transport}
	request := mustReq(t, "https://"+addr+"/")
	if _, err := client.SendBatch(
		t.Context(), []*http.Request{request},
	); err != nil {
		t.Fatalf("prime pooled connection: %v", err)
	}
	mu.Lock()
	first := dialled[0]
	mu.Unlock()
	if err := first.Close(); err != nil {
		t.Fatalf("close pooled connection: %v", err)
	}

	stream := client.StreamTrials(t.Context(), []*http.Request{request},
		tth2.WithMaxTrials(1),
		tth2.WithMinBatchInterval(20*time.Millisecond))
	for trial := range stream.All() {
		if trial.Err != nil {
			t.Errorf("trial: %v", trial.Err)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("StreamTrials: %v", err)
	}
	// The read pump usually observes the close before the stale attempt reaches
	// its dispatch gate. If the two race, the attempt can legitimately reach the
	// gate first; either way, only reached gates count as progress.
	if got := stream.Progress().Requests; got < 1 || got > 2 {
		t.Errorf("progress requests = %d, want 1 or 2 reached dispatch gates", got)
	}
	mu.Lock()
	dials := len(dialled)
	mu.Unlock()
	if dials != 2 {
		t.Errorf("dialled connections = %d, want stale connection plus retry", dials)
	}
}

// TestProgressRunsAheadOfTheSummary verifies completion before consumer tally.
func TestProgressRunsAheadOfTheSummary(t *testing.T) {
	t.Parallel()
	const n = 2
	addr, tlsCfg := h2test.Serve(t, okHandler)
	c := newClientMaxConns(t, tlsCfg, 1)
	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/a"),
		mustReq(t, "https://"+addr+"/b"),
	}
	s := c.StreamTrials(t.Context(), reqs, tth2.WithWarmup(2), tth2.WithMaxTrials(4))

	// Stop on the first counted trial. The priming ones are yielded too, flagged,
	// and the tally skips them.
	for tr := range s.All() {
		if !tr.Warmup {
			break
		}
	}
	if err := s.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	p, summary := s.Progress(), s.Summary()
	if summary.Trials != 1 {
		t.Fatalf("Summary().Trials = %d, want 1: one trial was received", summary.Trials)
	}
	// The priming trials are progress the summary never sees at all, so this holds
	// however many counted trials the late stop let through.
	if p.Trials+p.Warmup <= summary.Trials {
		t.Errorf("progress counted %d trials + %d warmup, want more than the %d tallied",
			p.Trials, p.Warmup, summary.Trials)
	}
	if want := (p.Trials + p.Warmup) * n; p.Requests != want {
		t.Errorf("Requests = %d, want %d", p.Requests, want)
	}
}

// TestProgressFinalReportLandsOnAStoppedRun verifies the final callback and
// that no report occurs after return.
func TestProgressFinalReportLandsOnAStoppedRun(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, okHandler)
	c := newClientMaxConns(t, tlsCfg, 1)
	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/a"),
		mustReq(t, "https://"+addr+"/b"),
	}

	var mu sync.Mutex
	var last tth2.TrialProgress
	reports := 0
	s := c.StreamTrials(t.Context(), reqs,
		// An interval no test run reaches, so the only report is the final one.
		tth2.WithProgress(time.Hour, func(p tth2.TrialProgress) {
			mu.Lock()
			defer mu.Unlock()
			reports++
			last = p
		}))

	for range s.All() {
		break
	}
	mu.Lock()
	got, n := last, reports
	mu.Unlock()
	if n != 1 {
		t.Fatalf("delivered %d reports, want 1: the final one, with the interval never due", n)
	}
	if want := s.Progress(); !sameProgress(got, want) {
		t.Errorf("final report %+v, want the run's final state %+v", got, want)
	}
	if got.Trials == 0 {
		t.Error("final report counted no trials, but one was yielded before the stop")
	}
}

func sameProgress(a, b tth2.TrialProgress) bool {
	sameRate := func(x, y float64) bool {
		return x == y || math.IsNaN(x) && math.IsNaN(y)
	}
	return a.Trials == b.Trials &&
		a.Warmup == b.Warmup &&
		a.Requests == b.Requests &&
		sameRate(a.RecentTrialsPerSecond, b.RecentTrialsPerSecond) &&
		sameRate(a.RecentRequestsPerSecond, b.RecentRequestsPerSecond)
}

// TestWithProgressRejectsANonReport verifies the option's argument domain.
func TestWithProgressRejectsANonReport(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		d      time.Duration
		report func(tth2.TrialProgress)
	}{
		{"zero interval", 0, func(tth2.TrialProgress) {}},
		{"negative interval", -time.Second, func(tth2.TrialProgress) {}},
		{"nil report", time.Second, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Error("WithProgress did not panic")
				}
			}()
			tth2.WithProgress(tc.d, tc.report)
		})
	}
}
