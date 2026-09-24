package tth2_test

import (
	"context"
	"errors"
	"net"
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

// TestStreamTrialsDialFailureEndsRun verifies that acquisition failure yields
// no trial, cancels sibling acquisitions, and releases their pool slots.
func TestStreamTrialsDialFailureEndsRun(t *testing.T) {
	t.Parallel()
	const maxConns = 4
	errDial := errors.New("dial unavailable")
	addr, tlsCfg := h2test.Serve(t, okHandler)
	allStarted := make(chan struct{})
	var (
		dialCalls atomic.Int64
		cancelled atomic.Int64
		recover   atomic.Bool
		started   sync.Once
	)
	tr := &tth2.Transport{
		TLSClientConfig: tlsCfg,
		MaxConnsPerHost: maxConns,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if recover.Load() {
				return (&net.Dialer{}).DialContext(ctx, network, address)
			}
			call := dialCalls.Add(1)
			if call == maxConns {
				started.Do(func() { close(allStarted) })
			}
			if call != 1 {
				// Cancellation can win either select below; both paths are a
				// cancelled sibling dial.
				defer cancelled.Add(1)
			}
			select {
			case <-allStarted:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if call == 1 {
				return nil, errDial
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	c := &tth2.Client{Transport: tr}
	reqs := []*http.Request{mustReq(t, "https://"+addr+"/")}

	stream := c.StreamTrials(
		t.Context(), reqs, tth2.WithResponseHeaderCapture())
	seen := 0
	for range stream.All() {
		seen++
	}
	if seen != 0 {
		t.Errorf("yielded %d trials, want 0", seen)
	}
	if err := stream.Err(); !errors.Is(err, errDial) {
		t.Errorf("Err = %v, want %v", err, errDial)
	}
	if got := stream.Summary(); got.Trials != 0 || got.FailedTrials != 0 {
		t.Errorf("summary trials/failed = %d/%d, want 0/0",
			got.Trials, got.FailedTrials)
	}
	if got := stream.Progress(); got.Trials != 0 || got.Requests != 0 {
		t.Errorf("progress trials/requests = %d/%d, want 0/0",
			got.Trials, got.Requests)
	}
	if got := cancelled.Load(); got != maxConns-1 {
		t.Errorf("cancelled sibling dials = %d, want %d", got, maxConns-1)
	}

	recover.Store(true)
	summary, err := c.RunTrials(t.Context(), reqs, maxConns)
	if err != nil {
		t.Fatalf("run after failed acquisitions: %v", err)
	}
	if summary.Trials != maxConns {
		t.Errorf("later run trials = %d, want %d", summary.Trials, maxConns)
	}
}

func TestStreamTrialsCancellationWinsOverDialError(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, okHandler)
	errDial := errors.New("dial failed after cancellation")
	dialling := make(chan struct{})
	var once sync.Once
	tr := &tth2.Transport{
		TLSClientConfig: tlsCfg,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			once.Do(func() { close(dialling) })
			<-ctx.Done()
			return nil, errDial
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	ctx, cancel := context.WithCancel(t.Context())
	stream := (&tth2.Client{Transport: tr}).StreamTrials(ctx,
		[]*http.Request{mustReq(t, "https://"+addr+"/")})
	done := make(chan struct{})
	go func() {
		for range stream.All() {
		}
		close(done)
	}()
	<-dialling
	cancel()
	<-done
	if err := stream.Err(); !errors.Is(err, context.Canceled) {
		t.Errorf("Err = %v, want context cancellation", err)
	}
	if got := stream.Progress().Trials; got != 0 {
		t.Errorf("progress trials = %d, want 0", got)
	}
}

func TestRunTrialsDialFailureDoesNotSpendBound(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, okHandler)
	errDial := errors.New("dial unavailable")
	var dialCalls atomic.Int64
	tr := &tth2.Transport{
		TLSClientConfig: tlsCfg,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			dialCalls.Add(1)
			return nil, errDial
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	summary, err := (&tth2.Client{Transport: tr}).RunTrials(t.Context(),
		[]*http.Request{mustReq(t, "https://"+addr+"/")}, 50)
	if !errors.Is(err, errDial) {
		t.Errorf("RunTrials error = %v, want %v", err, errDial)
	}
	if summary.Trials != 0 || summary.FailedTrials != 0 {
		t.Errorf("summary trials/failed = %d/%d, want 0/0",
			summary.Trials, summary.FailedTrials)
	}
	if got := dialCalls.Load(); got != 1 {
		t.Errorf("dial calls = %d, want 1 for a 50-trial bound", got)
	}
}

func TestStreamTrialsDistinctWarmupBatch(t *testing.T) {
	t.Parallel()
	var (
		mu               sync.Mutex
		warmupRequests   int
		measuredRequests int
		paddedMeasured   int
		paddedPaths      = map[string]int{}
	)
	addr, tlsCfg := h2test.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.URL.Path == "/warm" {
			warmupRequests++
			if r.URL.Query().Has("_pad1") {
				t.Error("distinct warmup request was padded")
			}
		} else {
			measuredRequests++
			if r.URL.Query().Has("_pad1") {
				paddedMeasured++
				paddedPaths[r.URL.Path]++
			}
		}
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	c := newClientMaxConns(t, tlsCfg, 1)
	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/a"),
		mustReq(t, "https://"+addr+"/b"),
	}
	warmupReq := mustReq(t, "https://"+addr+"/warm")
	stream := c.StreamTrials(t.Context(), reqs,
		tth2.WithWarmup(2),
		tth2.WithWarmupRequests(warmupReq),
		tth2.WithPadding(tth2.RequestPadding{URLParams: 1}),
		tth2.WithMaxTrials(2),
	)
	var warmups, counted int
	for trial := range stream.All() {
		if trial.Err != nil {
			t.Fatalf("trial error: %v", trial.Err)
		}
		if trial.Warmup {
			warmups++
			if !slices.Equal(trial.Arrangement, []int{0}) ||
				len(trial.Results) != 1 {
				t.Errorf("warmup arrangement/results = %v/%d, want [0]/1",
					trial.Arrangement, len(trial.Results))
			}
		} else {
			counted++
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("StreamTrials: %v", err)
	}
	if warmups != 2 || counted != 2 {
		t.Errorf("warmup/counted trials = %d/%d, want 2/2", warmups, counted)
	}
	mu.Lock()
	if warmupRequests != 2 || measuredRequests != 4 || paddedMeasured != 2 {
		t.Errorf("warmup/measured/padded requests = %d/%d/%d, want 2/4/2",
			warmupRequests, measuredRequests, paddedMeasured)
	}
	if paddedPaths["/a"] != 1 || paddedPaths["/b"] != 1 {
		t.Errorf("padded measured paths = %v, want /a and /b once each",
			paddedPaths)
	}
	mu.Unlock()
	if got := stream.Progress().Requests; got != 6 {
		t.Errorf("progress requests = %d, want 6", got)
	}
}

// TestStreamTrialsBreakReleasesConnections verifies that stopping joins workers
// and lets a later run use the full connection pool.
func TestStreamTrialsBreakReleasesConnections(t *testing.T) {
	t.Parallel()
	const maxConns = 2
	var (
		verify     atomic.Bool
		verifyMu   sync.Mutex
		verifySeen = map[string]bool{}
	)
	allConns := make(chan struct{})
	release := make(chan struct{})
	var allConnsOnce sync.Once
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if verify.Load() {
			verifyMu.Lock()
			verifySeen[r.RemoteAddr] = true
			if len(verifySeen) == maxConns {
				allConnsOnce.Do(func() { close(allConns) })
			}
			verifyMu.Unlock()
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	})
	addr, tlsCfg := h2test.Serve(t, handler)
	c := newClientMaxConns(t, tlsCfg, maxConns)
	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/a"),
		mustReq(t, "https://"+addr+"/b"),
	}

	stream := c.StreamTrials(
		t.Context(), reqs, tth2.WithResponseHeaderCapture())
	seen := 0
	for range stream.All() {
		seen++
		break // stop after one trial, mid-cycle and mid-run
	}
	if seen != 1 {
		t.Fatalf("saw %d trials, want 1", seen)
	}
	if err := stream.Err(); err != nil {
		t.Errorf("Err after a consumer stop = %v, want nil: stopping is not a failure", err)
	}

	// Require two distinct connections to enter the handler together. One worker
	// cannot finish its block and impersonate a full-width run while blocked here.
	verify.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	type runResult struct {
		summary *tth2.TrialSummary
		err     error
	}
	done := make(chan runResult, 1)
	go func() {
		summary, err := c.RunTrials(ctx, reqs, 4)
		done <- runResult{summary: summary, err: err}
	}()
	select {
	case <-allConns:
		close(release)
	case <-ctx.Done():
		close(release)
		<-done
		t.Fatal("later run did not use every released connection concurrently")
	}
	result := <-done
	summary, err := result.summary, result.err
	if err != nil {
		t.Fatalf("run after a stopped stream: %v", err)
	}
	if summary.Trials != 4 {
		t.Errorf("Trials = %d, want 4", summary.Trials)
	}
}

// TestStreamTrialsSummaryTracksYields checks the tally is the consumer's own
// view: it counts the trials handed over, so a summary read after k trials
// describes exactly those k.
func TestStreamTrialsSummaryTracksYields(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, okHandler)
	c := newClient(t, tlsCfg) // one connection: trials arrive in order
	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/a"),
		mustReq(t, "https://"+addr+"/b"),
	}

	stream := c.StreamTrials(t.Context(), reqs)
	seen := 0
	for range stream.All() {
		seen++
		if got := stream.Summary().Trials; got != seen {
			t.Fatalf("after %d yields Summary().Trials = %d", seen, got)
		}
		if seen == 5 {
			break
		}
	}
	if got := stream.Summary().Trials; got != 5 {
		t.Errorf("final Summary().Trials = %d, want 5: a trial in flight is not one the consumer saw", got)
	}
}

// TestStreamTrialsCoordinates verifies dense indices, rotation coordinates,
// positional coverage, and the sentinel coordinates used by warmups.
func TestStreamTrialsCoordinates(t *testing.T) {
	t.Parallel()
	const (
		n      = 3
		warmup = 1
		cycles = 4
		wanted = n * cycles
	)
	addr, tlsCfg := h2test.Serve(t, okHandler)
	c := newClient(t, tlsCfg)
	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/a"),
		mustReq(t, "https://"+addr+"/b"),
		mustReq(t, "https://"+addr+"/c"),
	}

	stream := c.StreamTrials(t.Context(), reqs, tth2.WithWarmup(warmup))
	var (
		counted   int
		seenIndex = map[int]bool{}
		byCycle   = map[int][][]int{}
		primings  int
	)
	for tr := range stream.All() {
		if tr.Warmup {
			if tr.Index != -1 || tr.Cycle != -1 || tr.Shift != -1 {
				t.Errorf("warmup trial coordinates = (%d, %d, %d), want all -1",
					tr.Index, tr.Cycle, tr.Shift)
			}
			if len(tr.Arrangement) != n {
				t.Errorf("warmup Arrangement = %v, want a real layout of %d", tr.Arrangement, n)
			}
			primings++
			continue
		}
		if tr.Err != nil {
			t.Fatalf("trial %d failed: %v", tr.Index, tr.Err)
		}
		if want := tr.Cycle*n + tr.Shift; tr.Index != want {
			t.Errorf("Index = %d, want Cycle*n+Shift = %d", tr.Index, want)
		}
		if seenIndex[tr.Index] {
			t.Errorf("Index %d yielded twice", tr.Index)
		}
		seenIndex[tr.Index] = true
		byCycle[tr.Cycle] = append(byCycle[tr.Cycle], tr.Arrangement)
		if counted++; counted == wanted {
			break
		}
	}

	if primings != warmup {
		t.Errorf("ran %d priming trials on one connection, want %d", primings, warmup)
	}
	for i := range wanted {
		if !seenIndex[i] {
			t.Errorf("index %d never yielded; indices are dense over a run's trials", i)
		}
	}
	// Within a complete cycle every request visits every position once, which is
	// what the cycle is for.
	for cycle, arrangements := range byCycle {
		if len(arrangements) != n {
			continue // a cycle cut short by the stop
		}
		for position := range n {
			seen := map[int]bool{}
			for _, a := range arrangements {
				seen[a[position]] = true
			}
			if len(seen) != n {
				t.Errorf("cycle %d position %d saw %d distinct requests, want %d",
					cycle, position, len(seen), n)
			}
		}
	}
}

// TestStreamTrialsHealthyCycleUsesOneConn verifies whole-cycle assignment and
// per-connection priming. It does not require every planned worker to acquire
// work.
func TestStreamTrialsHealthyCycleUsesOneConn(t *testing.T) {
	t.Parallel()
	const (
		n      = 2
		conns  = 2
		warmup = 1
		wanted = 16
	)
	addr, tlsCfg := h2test.Serve(t, okHandler)
	c := newClientMaxConns(t, tlsCfg, conns)
	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/a"),
		mustReq(t, "https://"+addr+"/b"),
	}

	// Drain a bounded run so every connection that acquired an available block is
	// observable, regardless of which worker completes first.
	stream := c.StreamTrials(t.Context(), reqs,
		tth2.WithWarmup(warmup), tth2.WithMaxTrials(wanted))
	var (
		counted     int
		cycleConn   = map[int]int{}
		warmupConns = map[int]int{}
	)
	for tr := range stream.All() {
		if tr.Warmup {
			warmupConns[tr.Conn]++
			continue
		}
		if got, ok := cycleConn[tr.Cycle]; ok && got != tr.Conn {
			t.Errorf("cycle %d ran on connections %d and %d", tr.Cycle, got, tr.Conn)
		}
		cycleConn[tr.Cycle] = tr.Conn
		counted++
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("StreamTrials: %v", err)
	}
	if counted != wanted {
		t.Errorf("ran %d counted trials, want %d", counted, wanted)
	}

	if len(warmupConns) == 0 || len(warmupConns) > conns {
		t.Errorf("priming ran on %d connections, want between 1 and %d", len(warmupConns), conns)
	}
	for conn, got := range warmupConns {
		if got != warmup {
			t.Errorf("connection %d ran %d priming trials, want %d", conn, got, warmup)
		}
	}
	// Every connection that carried a counted trial was primed first: priming is
	// what a fresh connection earns before it counts for anything.
	for cycle, conn := range cycleConn {
		if _, ok := warmupConns[conn]; !ok {
			t.Errorf("cycle %d ran on connection %d, which was never primed", cycle, conn)
		}
	}
	// Ids are dense from 0, so whatever connections a run held carry the first ids
	// -- there is no gap unless one was lost, which a healthy local server does
	// not cause.
	for id := range len(warmupConns) {
		if _, ok := warmupConns[id]; !ok {
			t.Errorf("no connection carried id %d; ids are dense from 0", id)
		}
	}
}

// TestStreamTrialsPrimingIsConnectionLocal verifies that workers may finish
// priming independently but never measure on an unprimed connection.
func TestStreamTrialsPrimingIsConnectionLocal(t *testing.T) {
	t.Parallel()
	var (
		warmups            atomic.Int64
		measuredBefore     atomic.Bool
		measuredOnUnprimed atomic.Bool
		measuredOnce       sync.Once
		primedMu           sync.Mutex
		primedByRemoteAddr = make(map[string]bool)
	)
	measuredStarted := make(chan struct{})
	firstWarmDone := make(chan struct{})
	handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/warm" {
			isFirst := warmups.Add(1) == 1
			if isFirst {
				select {
				case <-measuredStarted:
				case <-r.Context().Done():
					return
				}
			}
			primedMu.Lock()
			primedByRemoteAddr[r.RemoteAddr] = true
			primedMu.Unlock()
			if isFirst {
				close(firstWarmDone)
			}
			return
		}
		primedMu.Lock()
		primed := primedByRemoteAddr[r.RemoteAddr]
		primedMu.Unlock()
		if !primed {
			measuredOnUnprimed.Store(true)
		}
		select {
		case <-firstWarmDone:
		default:
			measuredBefore.Store(true)
		}
		measuredOnce.Do(func() { close(measuredStarted) })
	})

	addr, tlsCfg := h2test.Serve(t, handler)
	c := newClientMaxConns(t, tlsCfg, 2)
	req := mustReq(t, "https://"+addr+"/measured")
	warm := mustReq(t, "https://"+addr+"/warm")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stream := c.StreamTrials(ctx, []*http.Request{req},
		tth2.WithWarmup(1), tth2.WithWarmupRequests(warm),
		tth2.WithMaxTrials(2))
	if got := stream.Conns(); got != 2 {
		t.Fatalf("planned connections = %d, want 2", got)
	}
	counted := 0
	for trial := range stream.All() {
		if trial.Err != nil {
			t.Errorf("trial: %v", trial.Err)
		}
		if !trial.Warmup {
			counted++
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("StreamTrials: %v", err)
	}
	if counted != 2 {
		t.Errorf("counted trials = %d, want 2", counted)
	}
	if !measuredBefore.Load() {
		t.Fatal("counted work did not overlap another connection's priming")
	}
	if measuredOnUnprimed.Load() {
		t.Fatal("counted work used a connection before its own priming completed")
	}
}

// TestStreamTrialsStopLeavesAtMostConnsPartialCycles verifies the bound on
// incomplete cycles after an early consumer stop.
func TestStreamTrialsStopLeavesAtMostConnsPartialCycles(t *testing.T) {
	t.Parallel()
	const (
		n     = 2
		conns = 3
		// Enough trials to have several complete cycles behind the stop, so the
		// bound is doing work rather than being trivially satisfied.
		wanted = 30
	)
	addr, tlsCfg := h2test.Serve(t, okHandler)
	c := newClientMaxConns(t, tlsCfg, conns)
	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/a"),
		mustReq(t, "https://"+addr+"/b"),
	}

	stream := c.StreamTrials(t.Context(), reqs)
	perCycle := map[int]int{}
	counted := 0
	for tr := range stream.All() {
		if tr.Err != nil {
			t.Fatalf("trial %d failed: %v", tr.Index, tr.Err)
		}
		perCycle[tr.Cycle]++
		if counted++; counted == wanted {
			break // a late stop: each connection is mid-cycle or between cycles
		}
	}

	partial := 0
	for cycle, got := range perCycle {
		switch {
		case got > n:
			t.Errorf("cycle %d yielded %d trials, want at most %d", cycle, got, n)
		case got < n:
			partial++
		}
	}
	if partial > stream.Conns() {
		t.Errorf("%d cycles left incomplete by the stop, want at most Conns() = %d",
			partial, stream.Conns())
	}
	if len(perCycle) <= partial {
		t.Errorf("every one of %d cycles was incomplete; the bound is only meaningful "+
			"against a run that completed some", len(perCycle))
	}
}

// TestStreamTrialsResultsAreRequestKeyed checks the re-keying every consumer
// depends on: a trial reports by request identity whatever arrangement it was
// sent in, and its arrival order names requests rather than positions.
func TestStreamTrialsResultsAreRequestKeyed(t *testing.T) {
	t.Parallel()
	// Each handler names itself, so a result can be traced back to the request it
	// answers regardless of the position it was sent at.
	mux := http.NewServeMux()
	for _, p := range []string{"/a", "/b", "/c"} {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Path", r.URL.Path)
			w.WriteHeader(http.StatusOK)
		})
	}
	addr, tlsCfg := h2test.Serve(t, mux)
	c := newClient(t, tlsCfg)
	paths := []string{"/a", "/b", "/c"}
	reqs := make([]*http.Request, len(paths))
	for i, p := range paths {
		reqs[i] = mustReq(t, "https://"+addr+p)
	}

	stream := c.StreamTrials(
		t.Context(), reqs, tth2.WithResponseHeaderCapture())
	seen := 0
	for tr := range stream.All() {
		if tr.Err != nil {
			t.Fatalf("trial %d: %v", tr.Index, tr.Err)
		}
		for i, r := range tr.Results {
			if got := r.Response.Header.Get("X-Path"); got != paths[i] {
				t.Errorf("Results[%d] answers %q, want %q (results are keyed by request)", i, got, paths[i])
			}
		}
		// ArrivalOrder inverts the per-request ranks, in request indices.
		for rank, i := range tr.ArrivalOrder {
			if tr.Results[i].ArrivalRank != rank {
				t.Errorf("ArrivalOrder[%d] = %d, whose ArrivalRank is %d",
					rank, i, tr.Results[i].ArrivalRank)
			}
		}
		if seen++; seen == 4 {
			break
		}
	}
}

// TestStreamTrialsCancellationSurfaces distinguishes a fatal context error from
// a yielded per-trial error.
func TestStreamTrialsCancellationSurfaces(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, okHandler)
	c := newClient(t, tlsCfg)
	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/a"),
		mustReq(t, "https://"+addr+"/b"),
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel() // a run that ends some other way must not leak the context
	stream := c.StreamTrials(ctx, reqs)
	for range stream.All() {
		cancel() // the run ends on its own from here
	}
	if err := stream.Err(); !errors.Is(err, context.Canceled) {
		t.Errorf("Err = %v, want context.Canceled", err)
	}
}

// TestStreamTrialsConfigurationFault checks a run that cannot be honoured says
// so without opening a connection: a batch spanning two origins yields nothing
// and reports the fault.
func TestStreamTrialsConfigurationFault(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, okHandler)
	c := newClient(t, tlsCfg)
	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/a"),
		mustReq(t, "https://other.invalid/b"),
	}

	stream := c.StreamTrials(t.Context(), reqs)
	for range stream.All() {
		t.Fatal("a stream that cannot run yielded a trial")
	}
	if stream.Err() == nil {
		t.Error("Err = nil, want the origin mismatch")
	}
	if got := stream.Summary().Trials; got != 0 {
		t.Errorf("Trials = %d, want 0", got)
	}
}

// TestStreamTrialsSummaryPolledConcurrently verifies monotone snapshots while
// another goroutine ranges the stream.
func TestStreamTrialsSummaryPolledConcurrently(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, okHandler)
	c := newClientMaxConns(t, tlsCfg, 2)
	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/a"),
		mustReq(t, "https://"+addr+"/b"),
	}

	stream := c.StreamTrials(t.Context(), reqs)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		last := 0
		for {
			select {
			case <-done:
				return
			default:
			}
			got := stream.Summary().Trials
			if got < last {
				t.Errorf("Summary().Trials went backwards: %d after %d", got, last)
				return
			}
			last = got
		}
	})

	seen := 0
	for range stream.All() {
		if seen++; seen == 20 {
			break
		}
	}
	close(done)
	wg.Wait()
	if got := stream.Summary().Trials; got != 20 {
		t.Errorf("Trials = %d, want 20", got)
	}
}

// TestStreamTrialsAllTwicePanics verifies the single-consumer contract.
func TestStreamTrialsAllTwicePanics(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, okHandler)
	c := newClient(t, tlsCfg)
	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/a"),
		mustReq(t, "https://"+addr+"/b"),
	}

	stream := c.StreamTrials(t.Context(), reqs)
	for range stream.All() {
		break
	}
	defer func() {
		if recover() == nil {
			t.Error("ranging a second time did not panic")
		}
	}()
	for range stream.All() {
		break
	}
}

// TestStreamTrialsMaxTrialsSendsNothingExtra verifies the pre-dispatch bound
// under concurrent workers.
func TestStreamTrialsMaxTrialsSendsNothingExtra(t *testing.T) {
	t.Parallel()
	const (
		n      = 2
		bound  = 8
		conns  = 2
		wanted = bound * n
	)
	var served atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	addr, tlsCfg := h2test.Serve(t, handler)
	c := newClientMaxConns(t, tlsCfg, conns)
	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/a"),
		mustReq(t, "https://"+addr+"/b"),
	}

	stream := c.StreamTrials(t.Context(), reqs, tth2.WithMaxTrials(bound))
	counted := 0
	for tr := range stream.All() {
		if tr.Err != nil {
			t.Fatalf("trial %d: %v", tr.Index, tr.Err)
		}
		if tr.Index >= bound {
			t.Errorf("yielded Index %d, beyond the bound of %d", tr.Index, bound)
		}
		counted++
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("Err = %v, want nil: the run ended by running out", err)
	}
	if counted != bound {
		t.Errorf("yielded %d trials, want %d", counted, bound)
	}
	if got := served.Load(); got != wanted {
		t.Errorf("server served %d requests, want exactly %d", got, wanted)
	}
}

// TestStreamTrialsMaxTrialsMidCycle verifies that a stream preserves a bound
// that truncates its last rotation cycle.
func TestStreamTrialsMaxTrialsMidCycle(t *testing.T) {
	t.Parallel()
	const (
		n     = 2
		bound = 5 // two whole cycles and one trial
	)
	addr, tlsCfg := h2test.Serve(t, okHandler)
	c := newClient(t, tlsCfg)
	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/a"),
		mustReq(t, "https://"+addr+"/b"),
	}

	stream := c.StreamTrials(t.Context(), reqs, tth2.WithMaxTrials(bound))
	perCycle := map[int]int{}
	last := -1
	for tr := range stream.All() {
		perCycle[tr.Cycle]++
		last = tr.Index
	}
	if last != bound-1 {
		t.Errorf("last Index = %d, want %d", last, bound-1)
	}
	// Two complete cycles, then one trial of a third.
	for cycle, want := range map[int]int{0: n, 1: n, 2: 1} {
		if got := perCycle[cycle]; got != want {
			t.Errorf("cycle %d ran %d trials, want %d", cycle, got, want)
		}
	}
	if len(perCycle) != 3 {
		t.Errorf("ran %d cycles, want 3", len(perCycle))
	}
}

// TestWithMaxTrialsPanicsBelowOne verifies the option's positive domain.
func TestWithMaxTrialsPanicsBelowOne(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, -1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("WithMaxTrials(%d): no panic", n)
				}
			}()
			_ = tth2.WithMaxTrials(n)
		}()
	}
}

func TestWithWarmupRequestsPanicsWhenEmpty(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("WithWarmupRequests(): no panic")
		}
	}()
	_ = tth2.WithWarmupRequests()
}

func TestStreamTrialsRejectsDifferentWarmupOrigin(t *testing.T) {
	reqs := []*http.Request{mustReq(t, "https://one.example/a")}
	warmup := mustReq(t, "https://two.example/warm")
	stream := new(tth2.Client).StreamTrials(t.Context(), reqs,
		tth2.WithWarmup(1),
		tth2.WithWarmupRequests(warmup),
	)
	for range stream.All() {
		t.Fatal("invalid warmup origin yielded a trial")
	}
	if err := stream.Err(); err == nil ||
		!strings.Contains(err.Error(), "warmup origin") {
		t.Errorf("Err = %v, want a warmup-origin error", err)
	}
}
