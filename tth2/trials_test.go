package tth2_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tantosec/tturl/internal/h2test"
	"github.com/tantosec/tturl/tth2"
)

// TestClientRunTrialsValidation verifies RunTrials input checks before dialling.
func TestClientRunTrialsValidation(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, okHandler)
	c := newClient(t, tlsCfg)

	sameHost := func(t *testing.T) []*http.Request {
		t.Helper()
		return []*http.Request{
			mustReq(t, "https://"+addr+"/"),
			mustReq(t, "https://"+addr+"/"),
		}
	}

	tests := []struct {
		name    string
		trials  int
		opts    []tth2.RunTrialsOption
		reqs    func(t *testing.T) []*http.Request
		wantErr string
	}{
		{
			name:    "no requests",
			trials:  2,
			reqs:    func(*testing.T) []*http.Request { return nil },
			wantErr: "at least 1",
		},
		{
			name:    "trials zero",
			trials:  0,
			reqs:    sameHost,
			wantErr: "trials must be positive",
		},
		{
			name:    "trials negative",
			trials:  -1,
			reqs:    sameHost,
			wantErr: "trials must be positive",
		},
		{
			name:    "trials not a multiple of len(reqs) with rotation",
			trials:  3,
			reqs:    sameHost,
			wantErr: "multiple",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := c.RunTrials(t.Context(), tc.reqs(t), tc.trials, tc.opts...)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestRunTrialsSingleRequest verifies the valid, vacuous one-request tally.
func TestRunTrialsSingleRequest(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, okHandler)
	c := newClient(t, tlsCfg)

	const trials = 3
	reqs := []*http.Request{mustReq(t, "https://"+addr+"/only")}
	summary, err := c.RunTrials(t.Context(), reqs, trials)
	if err != nil {
		t.Fatalf("RunTrials: %v", err)
	}
	if summary.Trials != trials || summary.FailedTrials != 0 {
		t.Errorf("Trials, FailedTrials = %d, %d, want %d, 0", summary.Trials, summary.FailedTrials, trials)
	}
	// One request, one rank: it arrives first in every trial, keyed either way.
	if got := summary.RankCounts; len(got) != 1 || len(got[0]) != 1 || got[0][0] != trials {
		t.Errorf("RankCounts = %v, want [[%d]]", got, trials)
	}
	if got := summary.PositionRankCounts; len(got) != 1 || got[0][0] != trials {
		t.Errorf("PositionRankCounts = %v, want [[%d]]", got, trials)
	}
}

func TestRunTrialsWarmupDiscarded(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, okHandler)
	ctx := t.Context()
	c := newClient(t, tlsCfg)

	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/a"),
		mustReq(t, "https://"+addr+"/b"),
	}
	summary, err := c.RunTrials(ctx, reqs, 4, tth2.WithWarmup(2))
	if err != nil {
		t.Fatalf("RunTrials: %v", err)
	}
	if got := summary.Trials; got != 4 {
		t.Errorf("Trials = %d, want 4 (warmup trials must not be counted)", got)
	}
	if got := summary.RankCounts[0][0] + summary.RankCounts[1][0]; got != 4 {
		t.Errorf("first-place counts sum = %d, want 4", got)
	}
}

// TestRunTrialsNonBalancingPoliciesAllowPartialCounts checks that policies
// without exact cycles accept a trial count that rotation rejects and still
// produce complete request- and position-keyed tallies.
func TestRunTrialsNonBalancingPoliciesAllowPartialCounts(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, okHandler)
	const n, trials = 3, 7
	for _, tc := range []struct {
		name   string
		policy tth2.ArrangementPolicy
	}{
		{"none", tth2.ArrangeNone},
		{"random", tth2.ArrangeRandom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newClient(t, tlsCfg)
			reqs := []*http.Request{
				mustReq(t, "https://"+addr+"/a"),
				mustReq(t, "https://"+addr+"/b"),
				mustReq(t, "https://"+addr+"/c"),
			}
			summary, err := c.RunTrials(t.Context(), reqs, trials,
				tth2.WithArrangementPolicy(tc.policy))
			if err != nil {
				t.Fatalf("RunTrials: %v", err)
			}
			if summary.Trials != trials || summary.FailedTrials != 0 {
				t.Fatalf("Trials/FailedTrials = %d/%d, want %d/0",
					summary.Trials, summary.FailedTrials, trials)
			}
			for rank := range n {
				requestTotal, positionTotal := 0, 0
				for i := range n {
					requestTotal += summary.RankCounts[i][rank]
					positionTotal += summary.PositionRankCounts[i][rank]
				}
				if requestTotal != trials || positionTotal != trials {
					t.Errorf("rank %d request/position totals = %d/%d, want %d/%d",
						rank, requestTotal, positionTotal, trials, trials)
				}
			}
		})
	}
}

// TestStreamTrialsConnectionPlan verifies caps from the transport and available
// rotation cycles.
func TestStreamTrialsConnectionPlan(t *testing.T) {
	t.Parallel()
	// n = 2, so cycles = trials/2. Conns = min(maxConns, cycles).
	for _, tc := range []struct {
		name      string
		maxConns  int
		trials    int
		wantConns int
	}{
		{name: "single conn", maxConns: 1, trials: 8, wantConns: 1},
		{name: "conns match cycles", maxConns: 4, trials: 8, wantConns: 4},
		{name: "cycles cap conns", maxConns: 8, trials: 4, wantConns: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := &tth2.Transport{MaxConnsPerHost: tc.maxConns}
			c := &tth2.Client{Transport: tr}
			reqs := []*http.Request{
				mustReq(t, "https://example.test/a"),
				mustReq(t, "https://example.test/b"),
			}
			stream := c.StreamTrials(t.Context(), reqs,
				tth2.WithMaxTrials(tc.trials))
			if err := stream.Err(); err != nil {
				t.Fatalf("StreamTrials: %v", err)
			}
			if got := stream.Conns(); got != tc.wantConns {
				t.Errorf("Conns = %d, want %d (maxConns=%d, cycles=%d)",
					got, tc.wantConns, tc.maxConns, tc.trials/2)
			}
		})
	}
}

// TestRunTrialsParallelConcurrentBatches verifies deterministic overlap across
// all planned workers.
func TestRunTrialsParallelConcurrentBatches(t *testing.T) {
	t.Parallel()
	const maxConns, n, trials = 4, 2, 8 // cycles = 4, so workers = 4
	want := maxConns * n                // streams in flight when every worker has sent

	var cur, peak atomic.Int64
	release := make(chan struct{})
	var once sync.Once
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		c := cur.Add(1)
		for {
			p := peak.Load()
			if c <= p || peak.CompareAndSwap(p, c) {
				break
			}
		}
		if int(c) == want {
			once.Do(func() { close(release) })
		}
		<-release
		cur.Add(-1)
		w.WriteHeader(http.StatusOK)
	})
	addr, tlsCfg := h2test.Serve(t, handler)
	c := newClientMaxConns(t, tlsCfg, maxConns)
	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/a"),
		mustReq(t, "https://"+addr+"/b"),
	}

	done := make(chan error, 1)
	go func() {
		_, err := c.RunTrials(t.Context(), reqs, trials)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunTrials: %v", err)
		}
	case <-time.After(10 * time.Second):
		once.Do(func() { close(release) }) // unblock so the test fails cleanly
		<-done
		t.Fatal("RunTrials did not complete within 10s")
	}
	if got := peak.Load(); got != int64(want) {
		t.Errorf("peak concurrent streams = %d, want %d (%d workers x %d streams)", got, want, maxConns, n)
	}
}

// TestRunTrialsParallelMatchesSequential compares request and position tallies
// under deterministic response frame order.
func TestRunTrialsParallelMatchesSequential(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := startRawH2Server(t,
		respondInPathOrder("/fast", "/slow"))
	const trials = 8
	newReqs := func() []*http.Request {
		return []*http.Request{
			mustReq(t, "https://"+addr+"/fast"),
			mustReq(t, "https://"+addr+"/slow"),
		}
	}

	seq, err := newClientMaxConns(t, tlsCfg, 1).RunTrials(t.Context(), newReqs(), trials)
	if err != nil {
		t.Fatalf("sequential RunTrials: %v", err)
	}
	par, err := newClientMaxConns(t, tlsCfg, 4).RunTrials(t.Context(), newReqs(), trials)
	if err != nil {
		t.Fatalf("parallel RunTrials: %v", err)
	}

	for _, s := range []*tth2.TrialSummary{seq, par} {
		if s.FailedTrials != 0 {
			t.Fatalf("FailedTrials = %d, want 0", s.FailedTrials)
		}
		if got := s.Trials; got != trials {
			t.Fatalf("Trials = %d, want %d", got, trials)
		}
	}
	// /fast (request 0) arrives first every trial; under balanced rotation it
	// occupies each of the two positions equally often, so neither position holds
	// an advantage.
	if want := [][]int{{trials, 0}, {0, trials}}; !rankMatrixEqual(seq.RankCounts, want) {
		t.Errorf("sequential RankCounts = %v, want %v (/fast always first)", seq.RankCounts, want)
	}
	half := trials / 2
	if want := [][]int{{half, half}, {half, half}}; !rankMatrixEqual(seq.PositionRankCounts, want) {
		t.Errorf("sequential PositionRankCounts = %v, want %v (balanced rotation)", seq.PositionRankCounts, want)
	}
	if !rankMatrixEqual(seq.RankCounts, par.RankCounts) {
		t.Errorf("RankCounts differ: sequential %v, parallel %v", seq.RankCounts, par.RankCounts)
	}
	if !rankMatrixEqual(seq.PositionRankCounts, par.PositionRankCounts) {
		t.Errorf("PositionRankCounts differ: sequential %v, parallel %v",
			seq.PositionRankCounts, par.PositionRankCounts)
	}
}

// TestRunTrialsParallelWholeCyclesPerConn verifies that healthy workers receive
// whole rotation cycles while their individual workloads may differ.
//
// The handler holds every request until all maxConns connections exist, which
// forces the fan-out rather than racing the scheduler for it: no worker can
// draw a second block before every worker has drawn its first.
func TestRunTrialsParallelWholeCyclesPerConn(t *testing.T) {
	t.Parallel()
	const maxConns, n, trials = 4, 2, 16 // cycles = 8, workers = 4

	var (
		mu      sync.Mutex
		perConn = map[string]int{}
		once    sync.Once
		all     = make(chan struct{})
	)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		perConn[r.RemoteAddr]++
		conns := len(perConn)
		mu.Unlock()
		if conns == maxConns {
			once.Do(func() { close(all) })
		}
		select {
		case <-all:
		case <-time.After(10 * time.Second): // fan-out never happened; fail below
		}
		w.WriteHeader(http.StatusOK)
	})
	addr, tlsCfg := h2test.Serve(t, handler)
	c := newClientMaxConns(t, tlsCfg, maxConns)
	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/a"),
		mustReq(t, "https://"+addr+"/b"),
	}
	if _, err := c.RunTrials(t.Context(), reqs, trials); err != nil {
		t.Fatalf("RunTrials: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got := len(perConn); got != maxConns {
		t.Fatalf("used %d connections, want %d", got, maxConns)
	}
	// One cycle is n trials of n requests, and a connection that opened at all
	// carried at least one.
	const cycleRequests = n * n
	total := 0
	for addr, got := range perConn {
		if got == 0 || got%cycleRequests != 0 {
			t.Errorf("connection %s handled %d requests, want a positive multiple of %d (whole cycles)",
				addr, got, cycleRequests)
		}
		total += got
	}
	if want := trials * n; total != want {
		t.Errorf("handled %d requests in total, want %d", total, want)
	}
}

// TestRunTrialsParallelCancellation verifies prompt cancellation of blocked
// parallel workers.
func TestRunTrialsParallelCancellation(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	t.Cleanup(func() { close(block) }) // release any hung handlers at teardown
	started := make(chan struct{}, 64)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-block:
		case <-r.Context().Done():
		}
	})
	addr, tlsCfg := h2test.Serve(t, handler)
	c := newClientMaxConns(t, tlsCfg, 4)
	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/a"),
		mustReq(t, "https://"+addr+"/b"),
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := c.RunTrials(ctx, reqs, 8)
		done <- err
	}()

	<-started // a trial is in flight
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("RunTrials error = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunTrials did not return after cancellation")
	}
}

// TestStreamTrialsMaxConns verifies that WithMaxConns caps a run below
// MaxConnsPerHost. A value at or above MaxConnsPerHost, or a non-positive one,
// imposes no bound.
func TestStreamTrialsMaxConns(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		poolLimit int
		cap       int
		trials    int
		wantConns int
	}{
		{name: "caps below the pool limit", poolLimit: 8, cap: 3, trials: 16, wantConns: 3},
		{name: "at or above the pool limit is a no-op", poolLimit: 4, cap: 8, trials: 16, wantConns: 4},
		{name: "zero imposes no bound", poolLimit: 4, cap: 0, trials: 16, wantConns: 4},
		{name: "negative imposes no bound", poolLimit: 4, cap: -1, trials: 16, wantConns: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := &tth2.Transport{MaxConnsPerHost: tc.poolLimit}
			c := &tth2.Client{Transport: tr}
			reqs := []*http.Request{
				mustReq(t, "https://example.test/a"),
				mustReq(t, "https://example.test/b"),
			}
			stream := c.StreamTrials(t.Context(), reqs,
				tth2.WithMaxTrials(tc.trials), tth2.WithMaxConns(tc.cap))
			if err := stream.Err(); err != nil {
				t.Fatalf("StreamTrials: %v", err)
			}
			if got := stream.Conns(); got != tc.wantConns {
				t.Errorf("Conns = %d, want %d", got, tc.wantConns)
			}
		})
	}
}

// TestRunTrialsRequestRateLimited verifies both request-rate controls against a
// conservative duration floor.
func TestRunTrialsRequestRateLimited(t *testing.T) {
	t.Parallel()
	const rps, trials = 50.0, 6 // n=2: floor (12-2)/50 = 200ms
	lower := 150 * time.Millisecond

	newReqs := func(addr string) []*http.Request {
		return []*http.Request{
			mustReq(t, "https://"+addr+"/a"),
			mustReq(t, "https://"+addr+"/b"),
		}
	}

	t.Run("per-call option", func(t *testing.T) {
		t.Parallel()
		addr, tlsCfg := h2test.Serve(t, okHandler)
		c := newClient(t, tlsCfg) // one connection: trials run sequentially
		start := time.Now()
		s, err := c.RunTrials(t.Context(), newReqs(addr), trials, tth2.WithMaxRequestsPerSecond(rps))
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("RunTrials: %v", err)
		}
		if s.FailedTrials != 0 {
			t.Errorf("FailedTrials = %d, want 0", s.FailedTrials)
		}
		if elapsed < lower {
			t.Errorf("elapsed %v, want >= %v (rate %g req/s)", elapsed, lower, rps)
		}
	})

	t.Run("transport per-host", func(t *testing.T) {
		t.Parallel()
		addr, tlsCfg := h2test.Serve(t, okHandler)
		tr := &tth2.Transport{TLSClientConfig: tlsCfg, MaxRequestsPerSecondPerHost: rps}
		t.Cleanup(tr.CloseIdleConnections)
		c := &tth2.Client{Transport: tr}
		start := time.Now()
		s, err := c.RunTrials(t.Context(), newReqs(addr), trials)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("RunTrials: %v", err)
		}
		if s.FailedTrials != 0 {
			t.Errorf("FailedTrials = %d, want 0", s.FailedTrials)
		}
		if elapsed < lower {
			t.Errorf("elapsed %v, want >= %v (rate %g req/s)", elapsed, lower, rps)
		}
	})
}

// rankMatrixEqual compares two rank-count matrices row by row.
func rankMatrixEqual(a, b [][]int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !slices.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}
