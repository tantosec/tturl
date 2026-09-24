package tth2_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/net/http2"

	"github.com/tantosec/tturl/tth2"
)

// TestPrimingRecoveryBoundsConnectionAttempts verifies that a worker preserves
// its priming observations but does not consume measured design positions when
// it cannot establish a connection eligible for measured work.
func TestPrimingRecoveryBoundsConnectionAttempts(t *testing.T) {
	t.Parallel()
	var connections atomic.Int64
	addr, tlsCfg := startRawH2Server(t, func(
		tb testing.TB,
		framer *http2.Framer,
		bw *bufio.Writer,
		conn net.Conn,
	) {
		tb.Helper()
		connections.Add(1)
		if err := doServerPreface(tb, framer, bw); err != nil {
			return
		}
		for {
			frame, err := framer.ReadFrame()
			if err != nil {
				return
			}
			if _, ok := frame.(*http2.MetaHeadersFrame); ok {
				_ = conn.Close()
				return
			}
		}
	})

	c := newClientMaxConns(t, tlsCfg, 1)
	req := mustReq(t, "https://"+addr+"/measured")
	warm := mustReq(t, "https://"+addr+"/warm")
	stream := c.StreamTrials(t.Context(), []*http.Request{req},
		tth2.WithWarmup(3),
		tth2.WithWarmupRequests(warm),
	)
	var observed []*tth2.Trial
	for trial := range stream.All() {
		observed = append(observed, trial)
	}
	if stream.Err() == nil {
		t.Fatal("StreamTrials error = nil, want terminal priming error")
	}
	if got := connections.Load(); got != 2 {
		t.Errorf("connections = %d, want original and one replacement", got)
	}
	if len(observed) != 2 {
		t.Fatalf("yielded %d observations, want 2 priming attempts", len(observed))
	}
	for i, trial := range observed {
		if !trial.Warmup || trial.Err == nil {
			t.Errorf("observation %d warmup/error = %t/%v, want true/non-nil",
				i, trial.Warmup, trial.Err)
		}
		if trial.Conn != i {
			t.Errorf("observation %d connection = %d, want %d", i, trial.Conn, i)
		}
	}
	if got := stream.Summary(); got.Trials != 0 || got.FailedTrials != 0 {
		t.Errorf("summary trials/failed = %d/%d, want 0/0",
			got.Trials, got.FailedTrials)
	}
	if got := stream.Progress(); got.Warmup != 2 || got.Trials != 0 || got.Requests != 2 {
		t.Errorf("progress warmup/trials/requests = %d/%d/%d, want 2/0/2",
			got.Warmup, got.Trials, got.Requests)
	}
}

// TestPrimingRecoveryAllowanceRenewsAfterMeasuredEligibility verifies that
// each independent loss of a usable worker connection receives the same
// replacement allowance and that prior measured outcomes retain their places.
func TestPrimingRecoveryAllowanceRenewsAfterMeasuredEligibility(t *testing.T) {
	t.Parallel()
	var connections atomic.Int64
	addr, tlsCfg := startRawH2Server(t, func(
		tb testing.TB,
		framer *http2.Framer,
		bw *bufio.Writer,
		conn net.Conn,
	) {
		tb.Helper()
		connection := int(connections.Add(1) - 1)
		if err := doServerPreface(tb, framer, bw); err != nil {
			return
		}
		measured := 0
		for {
			frame, err := framer.ReadFrame()
			if err != nil {
				return
			}
			headers, ok := frame.(*http2.MetaHeadersFrame)
			if !ok {
				continue
			}
			path := headers.PseudoValue("path")
			switch {
			case connection == 0, connection == 2:
				_ = conn.Close()
				return
			case connection == 1 && path == "/measured":
				measured++
				if measured == 2 {
					_ = conn.Close()
					return
				}
			}
			if err := writeOKResponse(framer, bw, headers.StreamID); err != nil {
				return
			}
		}
	})

	c := newClientMaxConns(t, tlsCfg, 1)
	req := mustReq(t, "https://"+addr+"/measured")
	warm := mustReq(t, "https://"+addr+"/warm")
	stream := c.StreamTrials(t.Context(), []*http.Request{req},
		tth2.WithWarmup(1),
		tth2.WithWarmupRequests(warm),
		tth2.WithMaxTrials(3),
	)
	warmups := make(map[int]*tth2.Trial)
	var measured []*tth2.Trial
	for trial := range stream.All() {
		if trial.Warmup {
			warmups[trial.Conn] = trial
		} else {
			measured = append(measured, trial)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("StreamTrials: %v", err)
	}
	if got := connections.Load(); got != 4 {
		t.Fatalf("connections = %d, want 4", got)
	}
	if len(warmups) != 4 {
		t.Fatalf("primed connections = %d, want 4", len(warmups))
	}
	for _, conn := range []int{0, 2} {
		if warmups[conn] == nil || warmups[conn].Err == nil {
			t.Errorf("connection %d priming error = %v, want non-nil",
				conn, warmups[conn])
		}
	}
	for _, conn := range []int{1, 3} {
		if warmups[conn] == nil || warmups[conn].Err != nil {
			t.Errorf("connection %d priming observation = %+v, want success",
				conn, warmups[conn])
		}
	}
	if len(measured) != 3 {
		t.Fatalf("measured trials = %d, want 3", len(measured))
	}
	for i, trial := range measured {
		if trial.Index != i {
			t.Errorf("measured trial %d index = %d, want %d", i, trial.Index, i)
		}
	}
	if measured[0].Err != nil || measured[1].Err == nil || measured[2].Err != nil {
		t.Errorf("measured errors = [%v, %v, %v], want [nil, non-nil, nil]",
			measured[0].Err, measured[1].Err, measured[2].Err)
	}
	if got := stream.Summary(); got.Trials != 2 || got.FailedTrials != 1 {
		t.Errorf("summary trials/failed = %d/%d, want 2/1",
			got.Trials, got.FailedTrials)
	}
}

// TestPrimingStreamFailureKeepsEligibleConnection verifies that a priming
// attempt is observational rather than a health requirement when its failure
// is confined to one HTTP/2 stream.
func TestPrimingStreamFailureKeepsEligibleConnection(t *testing.T) {
	t.Parallel()
	var connections atomic.Int64
	addr, tlsCfg := startRawH2Server(t, func(
		tb testing.TB,
		framer *http2.Framer,
		bw *bufio.Writer,
		_ net.Conn,
	) {
		tb.Helper()
		connections.Add(1)
		if err := doServerPreface(tb, framer, bw); err != nil {
			return
		}
		attempt := 0
		for {
			frame, err := framer.ReadFrame()
			if err != nil {
				return
			}
			headers, ok := frame.(*http2.MetaHeadersFrame)
			if !ok {
				continue
			}
			attempt++
			if attempt == 1 {
				if err := framer.WriteRSTStream(headers.StreamID, http2.ErrCodeRefusedStream); err != nil {
					return
				}
				if err := bw.Flush(); err != nil {
					return
				}
				continue
			}
			if err := writeOKResponse(framer, bw, headers.StreamID); err != nil {
				return
			}
		}
	})

	c := newClientMaxConns(t, tlsCfg, 1)
	req := mustReq(t, "https://"+addr+"/measured")
	warm := mustReq(t, "https://"+addr+"/warm")
	stream := c.StreamTrials(t.Context(), []*http.Request{req},
		tth2.WithWarmup(2),
		tth2.WithWarmupRequests(warm),
		tth2.WithMaxTrials(1),
	)
	var warmups, measured []*tth2.Trial
	for trial := range stream.All() {
		if trial.Warmup {
			warmups = append(warmups, trial)
		} else {
			measured = append(measured, trial)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("StreamTrials: %v", err)
	}
	if got := connections.Load(); got != 1 {
		t.Errorf("connections = %d, want 1", got)
	}
	if len(warmups) != 2 || warmups[0].Err == nil || warmups[1].Err != nil {
		t.Fatalf("priming observations = %+v, want one failed then one successful",
			warmups)
	}
	if len(measured) != 1 || measured[0].Err != nil || measured[0].Conn != 0 {
		t.Errorf("measured observations = %+v, want one success on connection 0",
			measured)
	}
}

// TestPrimingReplacementAcquisitionFailureEndsRun verifies that acquisition is
// terminal and does not manufacture a measured attempt when a priming
// replacement cannot be obtained.
func TestPrimingReplacementAcquisitionFailureEndsRun(t *testing.T) {
	t.Parallel()
	errDial := errors.New("replacement unavailable")
	addr, tlsCfg := startRawH2Server(t, func(
		tb testing.TB,
		framer *http2.Framer,
		bw *bufio.Writer,
		conn net.Conn,
	) {
		tb.Helper()
		if err := doServerPreface(tb, framer, bw); err != nil {
			return
		}
		for {
			frame, err := framer.ReadFrame()
			if err != nil {
				return
			}
			if _, ok := frame.(*http2.MetaHeadersFrame); ok {
				_ = conn.Close()
				return
			}
		}
	})
	var dials atomic.Int64
	tr := &tth2.Transport{
		TLSClientConfig: tlsCfg,
		MaxConnsPerHost: 1,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if dials.Add(1) > 1 {
				return nil, errDial
			}
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	req := mustReq(t, "https://"+addr+"/measured")
	stream := (&tth2.Client{Transport: tr}).StreamTrials(t.Context(),
		[]*http.Request{req}, tth2.WithWarmup(1), tth2.WithMaxTrials(10))
	var observed []*tth2.Trial
	for trial := range stream.All() {
		observed = append(observed, trial)
	}
	if err := stream.Err(); !errors.Is(err, errDial) {
		t.Errorf("Err = %v, want %v", err, errDial)
	}
	if len(observed) != 1 || !observed[0].Warmup || observed[0].Err == nil {
		t.Errorf("observations = %+v, want one failed priming attempt", observed)
	}
	if got := stream.Progress(); got.Trials != 0 || got.Warmup != 1 {
		t.Errorf("progress trials/warmup = %d/%d, want 0/1",
			got.Trials, got.Warmup)
	}
}

// TestCompletedPrimingRetirementPrecedesMeasuredDispatch verifies that normal
// retirement after a complete priming response moves pending measured work to
// a primed replacement without assigning the retirement a measured index.
func TestCompletedPrimingRetirementPrecedesMeasuredDispatch(t *testing.T) {
	t.Parallel()
	var (
		connections atomic.Int64
		retire      = make(chan struct{})
		retired     = make(chan struct{})
	)
	addr, tlsCfg := startRawH2Server(t, func(
		tb testing.TB,
		framer *http2.Framer,
		bw *bufio.Writer,
		conn net.Conn,
	) {
		tb.Helper()
		connection := int(connections.Add(1) - 1)
		if err := doServerPreface(tb, framer, bw); err != nil {
			return
		}
		for {
			frame, err := framer.ReadFrame()
			if err != nil {
				return
			}
			headers, ok := frame.(*http2.MetaHeadersFrame)
			if !ok {
				continue
			}
			if err := writeOKResponse(framer, bw, headers.StreamID); err != nil {
				return
			}
			if connection == 0 {
				<-retire
				if err := framer.WriteGoAway(
					headers.StreamID, http2.ErrCodeNo, nil,
				); err != nil {
					return
				}
				if err := bw.Flush(); err != nil {
					return
				}
				// EOF proves the client read pump observed GOAWAY and retired
				// the connection before measured request construction resumes.
				for {
					if _, err := framer.ReadFrame(); err != nil {
						close(retired)
						return
					}
				}
			}
		}
	})

	c := newClientMaxConns(t, tlsCfg, 1)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		"https://"+addr+"/measured", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.GetBody = func() (io.ReadCloser, error) {
		<-retired
		return io.NopCloser(strings.NewReader("x")), nil
	}
	warm := mustReq(t, "https://"+addr+"/warm")
	stream := c.StreamTrials(t.Context(), []*http.Request{req},
		tth2.WithWarmup(1),
		tth2.WithWarmupRequests(warm),
		tth2.WithMaxTrials(1),
	)
	var warmups, measured []*tth2.Trial
	for trial := range stream.All() {
		if trial.Warmup {
			warmups = append(warmups, trial)
			if trial.Conn == 0 {
				close(retire)
			}
		} else {
			measured = append(measured, trial)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("StreamTrials: %v", err)
	}
	if got := connections.Load(); got != 2 {
		t.Fatalf("connections = %d, want retired connection and replacement", got)
	}
	if len(warmups) != 2 || warmups[0].Err != nil || warmups[1].Err != nil {
		t.Fatalf("priming observations = %+v, want two complete attempts", warmups)
	}
	if len(measured) != 1 || measured[0].Index != 0 || measured[0].Conn != 1 ||
		measured[0].Err != nil {
		t.Errorf("measured observations = %+v, want index 0 success on replacement",
			measured)
	}
	if got := stream.Progress(); got.Trials != 1 || got.Warmup != 2 {
		t.Errorf("progress trials/warmup = %d/%d, want 1/2",
			got.Trials, got.Warmup)
	}
}

// TestCancellationDuringPrimingEndsRecovery verifies that caller cancellation
// is terminal and does not spend the replacement allowance or a measured
// design position.
func TestCancellationDuringPrimingEndsRecovery(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	var connections atomic.Int64
	addr, tlsCfg := startRawH2Server(t, func(
		tb testing.TB,
		framer *http2.Framer,
		bw *bufio.Writer,
		_ net.Conn,
	) {
		tb.Helper()
		connections.Add(1)
		if err := doServerPreface(tb, framer, bw); err != nil {
			return
		}
		for {
			frame, err := framer.ReadFrame()
			if err != nil {
				return
			}
			if _, ok := frame.(*http2.MetaHeadersFrame); ok {
				cancel()
				return
			}
		}
	})

	c := newClientMaxConns(t, tlsCfg, 1)
	req := mustReq(t, "https://"+addr+"/")
	stream := c.StreamTrials(ctx, []*http.Request{req},
		tth2.WithWarmup(1), tth2.WithMaxTrials(10))
	var observed []*tth2.Trial
	for trial := range stream.All() {
		observed = append(observed, trial)
	}
	if err := stream.Err(); !errors.Is(err, context.Canceled) {
		t.Errorf("Err = %v, want context.Canceled", err)
	}
	if got := connections.Load(); got != 1 {
		t.Errorf("connections = %d, want 1", got)
	}
	if len(observed) != 1 || !observed[0].Warmup || observed[0].Err == nil {
		t.Errorf("observations = %+v, want one failed priming attempt", observed)
	}
	if got := stream.Progress(); got.Trials != 0 || got.Warmup != 1 {
		t.Errorf("progress trials/warmup = %d/%d, want 0/1",
			got.Trials, got.Warmup)
	}
}

// TestReplacementDialFailurePreservesCompletedTrials verifies that losing a
// connection still yields the batch that was attempted, while inability to
// acquire its replacement ends the run without manufacturing another trial.
func TestReplacementDialFailurePreservesCompletedTrials(t *testing.T) {
	t.Parallel()
	errDial := errors.New("replacement unavailable")
	addr, tlsCfg := startRawH2Server(t, func(
		tb testing.TB,
		framer *http2.Framer,
		bw *bufio.Writer,
		conn net.Conn,
	) {
		tb.Helper()
		if err := doServerPreface(tb, framer, bw); err != nil {
			return
		}
		requests := 0
		for {
			frame, err := framer.ReadFrame()
			if err != nil {
				return
			}
			headers, ok := frame.(*http2.MetaHeadersFrame)
			if !ok {
				continue
			}
			requests++
			if requests == 2 {
				_ = conn.Close()
				return
			}
			if err := writeOKResponse(framer, bw, headers.StreamID); err != nil {
				return
			}
		}
	})
	dialCalls := 0
	tr := &tth2.Transport{
		TLSClientConfig: tlsCfg,
		MaxConnsPerHost: 1,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialCalls++
			if dialCalls > 1 {
				return nil, errDial
			}
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	req := mustReq(t, "https://"+addr+"/")
	stream := (&tth2.Client{Transport: tr}).StreamTrials(t.Context(),
		[]*http.Request{req}, tth2.WithMaxTrials(10))
	var trials []*tth2.Trial
	for trial := range stream.All() {
		trials = append(trials, trial)
	}
	if err := stream.Err(); !errors.Is(err, errDial) {
		t.Errorf("Err = %v, want %v", err, errDial)
	}
	if len(trials) != 2 {
		t.Fatalf("yielded %d trials, want 2", len(trials))
	}
	if trials[0].Err != nil {
		t.Errorf("first trial error = %v, want nil", trials[0].Err)
	}
	if trials[1].Err == nil {
		t.Error("connection-fatal trial has nil error")
	}
	if got := stream.Summary(); got.Trials != 1 || got.FailedTrials != 1 {
		t.Errorf("summary trials/failed = %d/%d, want 1/1",
			got.Trials, got.FailedTrials)
	}
	if got := stream.Progress(); got.Trials != 2 || got.Requests != 2 {
		t.Errorf("progress trials/requests = %d/%d, want 2/2",
			got.Trials, got.Requests)
	}
	if dialCalls != 2 {
		t.Errorf("dial calls = %d, want 2", dialCalls)
	}
}

// TestReplacementConnectionContinuesDamagedCycle kills a connection during a
// rotating cycle. The failed trial keeps its design coordinates and connection
// id; after priming, a replacement carries the cycle's remaining shift.
//
// Priming is deliberately expensive relative to what is left to run -- four
// priming batches against the one trial that outlives the first connection --
// so the two are distinguishable by request count alone: an unprimed
// replacement could not reach the priming batches' worth of requests from the
// trials left to it.
func TestReplacementConnectionContinuesDamagedCycle(t *testing.T) {
	t.Parallel()
	const (
		warmup = 4
		n      = 2
		trials = 4 // two cycles, so trials outlive the first connection
		// Requests each connection must serve before it can be said to have been
		// primed. The trials left after the kill cannot reach this on their own.
		primed = warmup
		// The first connection is killed once it has served its priming and the
		// first cycle, so one trial is left for its replacement.
		killAfter = primed + trials/2*n
	)

	var (
		mu      sync.Mutex
		perConn []int // requests served, one entry per connection, in arrival order
	)
	addr, tlsCfg := startRawH2Server(t, func(tb testing.TB, framer *http2.Framer, bw *bufio.Writer, conn net.Conn) {
		tb.Helper()
		if err := doServerPreface(tb, framer, bw); err != nil {
			tb.Logf("preface: %v", err)
			return
		}
		mu.Lock()
		perConn = append(perConn, 0)
		self := len(perConn) - 1
		mu.Unlock()

		for {
			f, err := framer.ReadFrame()
			if err != nil {
				return // the client closed, or the deadline struck
			}
			hf, ok := f.(*http2.MetaHeadersFrame)
			if !ok {
				continue
			}
			mu.Lock()
			perConn[self]++
			served := perConn[self]
			mu.Unlock()

			// The first connection dies mid-run, after proving itself live: the
			// client must replace it rather than fail the run. Killing it before
			// responding costs the trial in flight, which the summary counts.
			if self == 0 && served > killAfter {
				_ = conn.Close()
				return
			}
			if err := writeOKResponse(framer, bw, hf.StreamID); err != nil {
				tb.Logf("respond: %v", err)
				return
			}
		}
	})

	c := newClientMaxConns(t, tlsCfg, 1) // one connection at a time, so a second means a replacement
	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/a"),
		mustReq(t, "https://"+addr+"/b"),
	}
	warmupReq := mustReq(t, "https://"+addr+"/warm")
	stream := c.StreamTrials(t.Context(), reqs,
		tth2.WithWarmup(warmup),
		tth2.WithWarmupRequests(warmupReq),
		tth2.WithMaxTrials(trials),
	)
	var (
		counted       []*tth2.Trial
		warmupsByConn = map[int]int{}
	)
	for trial := range stream.All() {
		if trial.Warmup {
			warmupsByConn[trial.Conn]++
			continue
		}
		if got := warmupsByConn[trial.Conn]; got != warmup {
			t.Errorf("connection %d had %d warmups before a counted trial, want %d",
				trial.Conn, got, warmup)
		}
		counted = append(counted, trial)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("StreamTrials: %v", err)
	}
	summary := stream.Summary()

	mu.Lock()
	defer mu.Unlock()
	if len(perConn) < 2 {
		t.Fatalf("used %d connections, want the killed one replaced", len(perConn))
	}
	for i, served := range perConn {
		if served < primed {
			t.Errorf("connection %d served %d requests, want at least %d: a replacement is primed too",
				i, served, primed)
		}
	}
	if len(counted) != trials {
		t.Fatalf("yielded %d counted trials, want %d", len(counted), trials)
	}
	var failed *tth2.Trial
	for _, trial := range counted {
		if trial.Err == nil {
			continue
		}
		if failed != nil {
			t.Fatalf("multiple failed trials: indexes %d and %d",
				failed.Index, trial.Index)
		}
		failed = trial
	}
	if failed == nil {
		t.Fatal("no failed trial observed")
	}
	if failed.Cycle < 0 || failed.Shift < 0 {
		t.Fatalf("failed trial coordinates = cycle %d shift %d",
			failed.Cycle, failed.Shift)
	}
	shifts := map[int]*tth2.Trial{}
	for _, trial := range counted {
		if trial.Cycle == failed.Cycle {
			shifts[trial.Shift] = trial
		}
	}
	if len(shifts) != n {
		t.Fatalf("damaged cycle yielded %d shifts, want %d", len(shifts), n)
	}
	laterReplacement := false
	for shift := range n {
		trial, ok := shifts[shift]
		if !ok {
			t.Errorf("damaged cycle did not yield shift %d", shift)
			continue
		}
		if shift > failed.Shift && trial.Err == nil && trial.Conn != failed.Conn {
			laterReplacement = true
		}
	}
	if !laterReplacement {
		t.Error("no later successful shift used the replacement connection")
	}

	// Derive the complete-trial tallies independently. This verifies the failed
	// shift contributes no ranks while its successful siblings still do.
	wantRequest := make([][]int, n)
	wantPosition := make([][]int, n)
	for i := range n {
		wantRequest[i] = make([]int, n)
		wantPosition[i] = make([]int, n)
	}
	for _, trial := range counted {
		if trial.Err != nil {
			continue
		}
		for request, result := range trial.Results {
			wantRequest[request][result.ArrivalRank]++
		}
		for position, request := range trial.Arrangement {
			wantPosition[position][trial.Results[request].ArrivalRank]++
		}
	}
	if !reflect.DeepEqual(summary.RankCounts, wantRequest) {
		t.Errorf("RankCounts = %v, want complete-trial tally %v",
			summary.RankCounts, wantRequest)
	}
	if !reflect.DeepEqual(summary.PositionRankCounts, wantPosition) {
		t.Errorf("PositionRankCounts = %v, want complete-trial tally %v",
			summary.PositionRankCounts, wantPosition)
	}
	if summary.Trials != trials-1 || summary.FailedTrials != 1 {
		t.Errorf("Trials/FailedTrials = %d/%d, want %d/1",
			summary.Trials, summary.FailedTrials, trials-1)
	}
	if got := summary.Trials + summary.FailedTrials; got != trials {
		t.Errorf("Trials+FailedTrials = %d, want %d", got, trials)
	}
	// Every connection that reaches measured work spends its own priming batches
	// through the same scheduler callback. Measured attempts spend their full
	// width, including the failed batch that forced replacement.
	wantRequests := len(warmupsByConn)*warmup + trials*n
	if got := stream.Progress().Requests; got != wantRequests {
		t.Errorf("progress requests = %d, want %d: replacement priming and "+
			"measured attempts must all be admitted", got, wantRequests)
	}
}
