package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tantosec/tturl/internal/h2test"
	"github.com/tantosec/tturl/tth2"
)

func TestDriveAnalyseLiveEvidence(t *testing.T) {
	addr, tlsConfig := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	}))
	transport := &tth2.Transport{TLSClientConfig: tlsConfig}
	t.Cleanup(transport.CloseIdleConnections)
	client := &tth2.Client{Transport: transport}
	requests := []*http.Request{
		analyseRequestFixture(t, addr, "/a"),
		analyseRequestFixture(t, addr, "/b"),
	}

	result := driveAnalyse(
		t.Context(), client, requests, 6, 1,
		analyseConfig{RandomisationSeed: 0x414e414c595345},
		tth2.WithWarmup(1),
	)
	if !result.Validity.Available || result.Width != 2 || result.Cycles != 6 ||
		result.Trials != 12 || result.Operations != 24 || len(result.Requests) != 2 {
		t.Fatalf("result = %+v", result)
	}
	for _, request := range result.Requests {
		if request.Samples != 12 || len(request.StatusCounts) != 1 ||
			request.StatusCounts[0] != (analyseStatusCount{
				Status: http.StatusNoContent, Count: 12,
			}) {
			t.Fatalf("request = %+v", request)
		}
	}
	if result.Global.Outcome == analyseUnavailable {
		t.Fatalf("global = %+v", result.Global)
	}
	if result.Evidence.Observations == nil ||
		len(result.Evidence.Observations) != 0 {
		t.Fatalf("live rich observations retained in memory: %d",
			len(result.Evidence.Observations))
	}
}

func TestAnalyseControlledNullOfflineLiveEquivalence(t *testing.T) {
	addr, tlsConfig := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	}))
	client := analyseTestClient(t, tlsConfig)
	requests := []*http.Request{
		analyseRequestFixture(t, addr, "/a"),
		analyseRequestFixture(t, addr, "/b"),
	}
	workload, err := resolveAnalyseWorkload(len(requests), 0)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	evidence, conns := collectAnalyseEvidence(
		t.Context(), client, requests, workload, 1,
	)
	elapsed := time.Since(started)
	config := analyseConfig{RandomisationSeed: 0x4e554c4c}
	offline := analyseOffline(evidence, config)
	live := finishAnalyseLive(evidence, conns, config)
	if !equalAnalyseResults(t, live, offline) {
		t.Fatalf("live result differs from offline replay\nlive: %+v\noffline: %+v",
			live, offline)
	}
	if !live.Validity.Available || live.Plan.Cycles != 150 ||
		live.Execution.AttemptedTrials != 300 {
		t.Fatalf("result = %+v", live)
	}
	t.Logf("null default: %s in %s", live.Global.Outcome, elapsed)
}

func TestAnalyseControlledSignal(t *testing.T) {
	addr, tlsConfig := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		if r.URL.Path == "/late" {
			time.Sleep(2 * time.Millisecond)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	client := analyseTestClientMaxConns(t, tlsConfig, 2)
	started := time.Now()
	result := driveAnalyse(t.Context(), client, []*http.Request{
		analyseRequestFixture(t, addr, "/early"),
		analyseRequestFixture(t, addr, "/late"),
	}, 0, 2, analyseConfig{RandomisationSeed: 0x5349474e414c})
	if result.Global.Outcome != analyseDifference ||
		result.Execution.Connections != 2 || result.ConnectionLimit != 2 ||
		result.Pairs.State != analysePairsPerformed ||
		len(result.Pairs.Pairs) != 1 || !result.Pairs.Pairs[0].Selected ||
		result.Pairs.Pairs[0].LeftBeforeRight < 290 {
		t.Fatalf("signal result = %+v", result)
	}
	t.Logf("signal default: %s in %s; early count %d/%d",
		result.Global.Outcome, time.Since(started),
		result.Pairs.Pairs[0].LeftBeforeRight, result.Pairs.Pairs[0].Trials)
}

func TestAnalyseControlledParallelCyclesOverlap(t *testing.T) {
	var active, peak atomic.Int32
	gate := make(chan struct{})
	var release sync.Once
	addr, tlsConfig := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				break
			}
		}
		if current >= 4 {
			release.Do(func() { close(gate) })
		}
		select {
		case <-gate:
		case <-time.After(2 * time.Second):
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	client := analyseTestClientMaxConns(t, tlsConfig, 2)
	result := driveAnalyse(t.Context(), client, []*http.Request{
		analyseRequestFixture(t, addr, "/a"),
		analyseRequestFixture(t, addr, "/b"),
	}, 6, 2, analyseConfig{RandomisationSeed: 0x504152414c4c454c})
	if !result.Validity.Available || result.Execution.Connections != 2 ||
		result.Execution.CompleteCycles != 6 {
		t.Fatalf("parallel result = %+v", result)
	}
	if peak.Load() < 4 {
		t.Fatalf("peak active requests = %d, want at least 4", peak.Load())
	}
}

func TestAnalyseControlledResponseMismatch(t *testing.T) {
	addr, tlsConfig := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		status := http.StatusNoContent
		if r.URL.Path == "/accepted" {
			status = http.StatusAccepted
		}
		w.WriteHeader(status)
	}))
	client := analyseTestClient(t, tlsConfig)
	result := driveAnalyse(t.Context(), client, []*http.Request{
		analyseRequestFixture(t, addr, "/no-content"),
		analyseRequestFixture(t, addr, "/accepted"),
	}, 6, 1, analyseConfig{RandomisationSeed: 0x535441545553})
	if !result.Validity.Available || len(result.Responses.Different) != 2 ||
		len(result.Responses.Changing) != 0 || len(result.Responses.Non2xx) != 0 {
		t.Fatalf("response review = %+v", result.Responses)
	}
}

func TestAnalyseControlledDamagedTarget(t *testing.T) {
	addr, tlsConfig := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		if r.URL.Path == "/broken" {
			panic(http.ErrAbortHandler)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	client := analyseTestClient(t, tlsConfig)
	result := driveAnalyse(t.Context(), client, []*http.Request{
		analyseRequestFixture(t, addr, "/healthy"),
		analyseRequestFixture(t, addr, "/broken"),
	}, 6, 1, analyseConfig{RandomisationSeed: 0x44414d414745})
	if result.Validity.Available ||
		result.Validity.Code != analyseIncompleteCycle ||
		result.Global.Outcome != analyseUnavailable ||
		result.Execution.AttemptedTrials == 0 ||
		result.Execution.CompletedTrials != 0 {
		t.Fatalf("damaged result = %+v", result)
	}
}

func TestAnalyseControlledDefaultWorkloadsComplete(t *testing.T) {
	addr, tlsConfig := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for width := 2; width <= 8; width++ {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			client := analyseTestClient(t, tlsConfig)
			requests := make([]*http.Request, width)
			for request := range width {
				requests[request] = analyseRequestFixture(t, addr, "/control")
			}
			started := time.Now()
			result := driveAnalyse(
				t.Context(), client, requests, 0, 1,
				analyseConfig{RandomisationSeed: 0x434f4e54524f4c},
			)
			if !result.Validity.Available ||
				result.Plan.Cycles != analyseDefaultCycles(width) ||
				result.Execution.AttemptedTrials != result.Plan.Trials ||
				result.Execution.CompletedTrials != result.Plan.Trials ||
				result.Execution.CompleteCycles != result.Plan.Cycles {
				t.Fatalf("result = %+v", result)
			}
			t.Logf("%d operations in %s; outcome %s", result.Plan.Operations,
				time.Since(started), result.Global.Outcome)
		})
	}
}

func TestDriveAnalyseCancellationIsUnavailable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	transport := &tth2.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	result := driveAnalyse(ctx, &tth2.Client{Transport: transport}, []*http.Request{
		analyseRequestFixture(t, "127.0.0.1:1", "/a"),
		analyseRequestFixture(t, "127.0.0.1:1", "/b"),
	}, 6, 1, analyseConfig{})
	if result.Validity.Available || result.Validity.Code != analyseInterrupted ||
		result.Global.Outcome != analyseUnavailable {
		t.Fatalf("result = %+v", result)
	}
}

func BenchmarkAnalyseLiveHighRTT(b *testing.B) {
	addr, tlsConfig := h2test.Serve(b, http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		time.Sleep(20 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, connections := range []int{1, 2, 4} {
		b.Run(fmt.Sprintf("connections_%d", connections), func(b *testing.B) {
			client := analyseTestClientMaxConns(b, tlsConfig, connections)
			requests := []*http.Request{
				analyseRequestFixture(b, addr, "/a"),
				analyseRequestFixture(b, addr, "/b"),
			}
			b.ResetTimer()
			for b.Loop() {
				result := driveAnalyse(
					b.Context(), client, requests, 20, connections,
					analyseConfig{RandomisationSeed: 0x48494748525454},
				)
				if !result.Validity.Available {
					b.Fatalf("result = %+v", result)
				}
			}
		})
	}
}

func BenchmarkAnalyseLiveMaximumParallelism(b *testing.B) {
	for _, width := range []int{2, 8} {
		connections := automaticFitReviewMaximumStreams / width
		b.Run(fmt.Sprintf("width_%d/connections_%d", width, connections),
			func(b *testing.B) {
				benchmarkAnalyseMaximumParallelism(b, width, connections)
			})
	}
}

func benchmarkAnalyseMaximumParallelism(
	b *testing.B, width, connections int,
) {
	var active, peak atomic.Int32
	gate := make(chan struct{})
	var release sync.Once
	addr, tlsConfig := h2test.Serve(b, http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				break
			}
		}
		if current >= automaticFitReviewMaximumStreams {
			release.Do(func() { close(gate) })
		}
		select {
		case <-gate:
		case <-time.After(5 * time.Second):
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	client := analyseTestClientMaxConns(b, tlsConfig, connections)
	requests := make([]*http.Request, width)
	for index := range requests {
		requests[index] = analyseRequestFixture(b, addr, "/control")
	}
	b.ResetTimer()
	for b.Loop() {
		result := driveAnalyse(
			b.Context(), client, requests, connections, connections,
			analyseConfig{RandomisationSeed: 0x4d4158434f4e4e53},
		)
		if !result.Validity.Available ||
			result.Execution.Connections != connections {
			b.Fatalf("result = %+v", result)
		}
	}
	if got := peak.Load(); got != automaticFitReviewMaximumStreams {
		b.Fatalf("peak active requests = %d, want %d",
			got, automaticFitReviewMaximumStreams)
	}
}

func analyseRequestFixture(t testing.TB, address, path string) *http.Request {
	t.Helper()
	request, err := http.NewRequestWithContext(
		t.Context(), http.MethodGet, "https://"+address+path, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func analyseTestClient(t testing.TB, tlsConfig *tls.Config) *tth2.Client {
	return analyseTestClientMaxConns(t, tlsConfig, 1)
}

func analyseTestClientMaxConns(
	t testing.TB, tlsConfig *tls.Config, maxConns int,
) *tth2.Client {
	t.Helper()
	transport := &tth2.Transport{
		TLSClientConfig: tlsConfig, MaxConnsPerHost: maxConns,
	}
	t.Cleanup(transport.CloseIdleConnections)
	return &tth2.Client{Transport: transport}
}
