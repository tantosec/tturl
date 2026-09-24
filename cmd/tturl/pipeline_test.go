package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tantosec/tturl/internal/h2test"
	"github.com/tantosec/tturl/internal/h2tls"
	"github.com/tantosec/tturl/internal/netdelay"
	"github.com/tantosec/tturl/tth2"
)

func TestFittedConnectionCount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		rtt, interval time.Duration
		want          int
	}{
		{"no observation", 0, 50 * time.Millisecond, 1},
		{"one interval adds one", 50 * time.Millisecond, 50 * time.Millisecond, 2},
		{"four intervals add one", 200 * time.Millisecond, 50 * time.Millisecond, 5},
		{
			"partial interval and fractional headroom round up",
			201 * time.Millisecond, 50 * time.Millisecond, 7,
		},
		{"largest width-two reviewed fit", 409 * time.Millisecond, time.Millisecond, 512},
		{"first width-two fit beyond review", 410 * time.Millisecond, time.Millisecond, 513},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := fittedConnectionCount(test.rtt, test.interval)
			if err != nil {
				t.Fatalf("fittedConnectionCount: %v", err)
			}
			if got != test.want {
				t.Errorf("fit = %d, want %d", got, test.want)
			}
		})
	}
}

func TestFittedConnectionCountRejectsHeadroomOverflow(t *testing.T) {
	t.Parallel()
	_, err := fittedConnectionCount(time.Duration(math.MaxInt), time.Nanosecond)
	if err == nil || !strings.Contains(err.Error(), "cannot be represented") {
		t.Fatalf("overflow error = %v", err)
	}
}

func TestFitPipelineThroughNetdelay(t *testing.T) {
	t.Parallel()
	const oneWay = 60 * time.Millisecond
	wrapper, err := netdelay.NewListenerWrapper(netdelay.Config{
		Delay: oneWay,
		Seed:  1,
	})
	if err != nil {
		t.Fatalf("NewListenerWrapper: %v", err)
	}
	addr, tlsConfig := h2test.Serve(
		t,
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("pipeline PING reached the HTTP handler")
		}),
		h2tls.WithListenerWrapper(wrapper),
	)
	request, err := http.NewRequestWithContext(
		t.Context(), "GET", "https://"+addr+"/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	transport := &tth2.Transport{TLSClientConfig: tlsConfig}
	pre := &prepared{
		materialised: &materialised{reqs: []*http.Request{request}},
		client:       &tth2.Client{Transport: transport},
	}
	tuning := transportTuningConfig{
		maxConns:       1,
		fitSelected:    true,
		requestRateMax: 10,
	}
	got, err := fitPipeline(t.Context(), pre, tuning, 1, 1, 0, true)
	if err != nil {
		t.Fatalf("fitPipeline: %v", err)
	}
	if got.connectionFit != connectionPathPing || got.pathRTT < 2*oneWay {
		t.Errorf("fit source = %q, RTT = %v; want path PING and >= %v",
			got.connectionFit, got.pathRTT, 2*oneWay)
	}
	if len(got.pathRTTSamples) != pathPingSamples {
		t.Errorf("PING samples = %d, want %d",
			len(got.pathRTTSamples), pathPingSamples)
	}
	basisObserved := false
	for i, sample := range got.pathRTTSamples {
		if sample < 2*oneWay || sample > got.pathRTT {
			t.Errorf("PING sample %d = %v, basis = %v; want [%v, basis]",
				i, sample, got.pathRTT, 2*oneWay)
		}
		basisObserved = basisObserved || sample == got.pathRTT
	}
	if !basisObserved {
		t.Errorf("PING basis %v is not one of the recorded samples %v",
			got.pathRTT, got.pathRTTSamples)
	}
	interval := effectiveBatchInterval(tuning, 1)
	wantFit, err := fittedConnectionCount(got.pathRTT, interval)
	if err != nil {
		t.Fatalf("fittedConnectionCount: %v", err)
	}
	if wantFit < 2 {
		t.Fatalf("fit = %d from RTT %v and interval %v, want at least 2",
			wantFit, got.pathRTT, interval)
	}
	if got.maxConns != wantFit || transport.MaxConnsPerHost != wantFit {
		t.Errorf("fitted connections = %d, transport = %d; want %d",
			got.maxConns, transport.MaxConnsPerHost, wantFit)
	}
	delivery, err := makeStructuredDelivery(resolveDeliveryPlan(
		got, 1, got.maxConns, false))
	if err != nil {
		t.Fatal(err)
	}
	if delivery.RateLimits.BatchMaxPerSecond != nil ||
		delivery.RateLimits.RequestMaxPerSecond == nil ||
		*delivery.RateLimits.RequestMaxPerSecond != 10 ||
		delivery.RateLimits == nil ||
		delivery.RateLimits.EffectiveBatchIntervalNS != int64(100*time.Millisecond) ||
		delivery.ConnectionCeiling != wantFit ||
		delivery.ConnectionLimit != wantFit ||
		delivery.Selection.Mode != "path_fit" ||
		len(delivery.PathFit.RTTSamplesNS) != pathPingSamples ||
		delivery.PathFit == nil ||
		delivery.PathFit.RTTBasisNS != got.pathRTT.Nanoseconds() ||
		delivery.PathFit.FittedConnections != wantFit {
		t.Errorf("delivery = %+v", delivery)
	}
	if delivery.PeerStreamLimitIgnored {
		t.Fatal("default delivery reports peer stream limit ignored")
	}
	ignored, err := makeStructuredDelivery(resolveDeliveryPlan(
		got, 1, got.maxConns, true))
	if err != nil {
		t.Fatal(err)
	}
	if !ignored.PeerStreamLimitIgnored {
		t.Fatal("opt-out delivery does not report peer stream limit ignored")
	}
}

func TestRunTimeoutCoversPipelineFitting(t *testing.T) {
	t.Parallel()
	request, err := http.NewRequestWithContext(
		t.Context(), http.MethodGet, "https://example.test/", nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &tth2.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	pre := &prepared{
		materialised: &materialised{reqs: []*http.Request{request}},
		client:       &tth2.Client{Transport: transport},
	}
	live := startLiveRunPhase(t.Context(), runTimeout(20*time.Millisecond))
	_, fitErr := fitPipeline(live.ctx, pre, transportTuningConfig{
		maxConns: 1, fitSelected: true, batchRate: 10, minBatchInterval: 100 * time.Millisecond,
	}, 1, 1, 1, true)
	cause := live.finish(transport.CloseIdleConnections)
	if err := liveRunError(fitErr, cause); !errors.Is(err, errRunTimeout) {
		t.Fatalf("fit error = %v, cause = %v; want run timeout", fitErr, cause)
	}
}

func TestFitPipelineDoesNotProbeWithoutAuthority(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		tuning transportTuningConfig
		steady bool
		want   connectionFit
	}{
		{
			"no batch pace",
			transportTuningConfig{maxConns: 1},
			true, connectionDefault,
		},
		{
			"pacing without fitting",
			transportTuningConfig{maxConns: 1, batchRate: 10, minBatchInterval: 100 * time.Millisecond},
			true, connectionDefault,
		},
		{
			"one shot",
			transportTuningConfig{maxConns: 1, fitSelected: true, batchRate: 10},
			false, connectionDefault,
		},
		{
			"explicit connections", transportTuningConfig{
				maxConns: 7, parallelSet: true, batchRate: 10,
			}, true, connectionExplicit,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := fitPipeline(
				t.Context(), nil, test.tuning, 2, 2, 0, test.steady)
			if err != nil {
				t.Fatalf("fitPipeline: %v", err)
			}
			if got.connectionFit != test.want {
				t.Errorf("fit source = %q, want %q",
					got.connectionFit, test.want)
			}
		})
	}
}

func TestFinishPipelinePlanReviewBoundary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		width    int
		maxConns int
	}{
		{1, 512},
		{2, 512},
		{3, 341},
		{4, 256},
		{5, 204},
		{6, 170},
		{7, 146},
		{8, 128},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("width_%d", test.width), func(t *testing.T) {
			t.Parallel()
			got, err := finishPipelinePlan(nil, transportTuningConfig{
				maxConns: test.maxConns, connectionFit: connectionPathPing,
				pathRTT: time.Second, minBatchInterval: time.Millisecond,
				pathFittedConns: test.maxConns,
			}, test.width, test.width, 0)
			if err != nil {
				t.Fatalf("accepted boundary: %v", err)
			}
			if got.connectionLimit != test.maxConns {
				t.Errorf("connection limit = %d, want %d",
					got.connectionLimit, test.maxConns)
			}
			_, err = finishPipelinePlan(nil, transportTuningConfig{
				maxConns:      test.maxConns + 1,
				connectionFit: connectionPathPing,
				pathRTT:       time.Second, minBatchInterval: time.Millisecond,
				pathFittedConns: test.maxConns + 1,
			}, test.width, test.width, 0)
			if err == nil || !strings.Contains(
				err.Error(), "--connections-fit-max",
			) {
				t.Fatalf("above-boundary error = %v", err)
			}
		})
	}
}

func TestFinishPipelinePlanFitMaximum(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		fitMax        int
		wantLimit     int
		wantSelection connectionFit
		wantCapped    bool
	}{
		{"binding maximum", 512, 512, connectionPathPingCapped, true},
		{"maximum above fit", 700, 600, connectionPathPing, false},
		{"unlimited acknowledgement", -1, 600, connectionPathPing, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := finishPipelinePlan(nil, transportTuningConfig{
				maxConns: 600, connectionFit: connectionPathPing,
				parallelFitSet: true, parallelFitMax: test.fitMax,
				pathFittedConns: 600,
			}, 2, 2, 0)
			if err != nil {
				t.Fatalf("finishPipelinePlan: %v", err)
			}
			if got.connectionLimit != test.wantLimit ||
				got.connectionFit != test.wantSelection ||
				got.pathFitCapped != test.wantCapped {
				t.Errorf("plan = %+v, want limit %d, selection %q, capped %t",
					got, test.wantLimit, test.wantSelection, test.wantCapped)
			}
		})
	}
}

func TestFinishPipelinePlanFiniteWorkNarrowsRawFit(t *testing.T) {
	t.Parallel()
	transport := &tth2.Transport{MaxConnsPerHost: 200_000}
	pre := &prepared{
		materialised: &materialised{},
		client:       &tth2.Client{Transport: transport},
	}
	got, err := finishPipelinePlan(pre, transportTuningConfig{
		maxConns: 200_000, connectionFit: connectionPathPing,
		pathFittedConns: 200_000,
	}, 8, 8, 2)
	if err != nil {
		t.Fatalf("finishPipelinePlan: %v", err)
	}
	if got.maxConns != 200_000 || got.connectionLimit != 2 ||
		transport.MaxConnsPerHost != 2 {
		t.Errorf("plan = %+v, transport limit = %d",
			got, transport.MaxConnsPerHost)
	}
}

func TestTrialConnectionWorkLimit(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		trials      int
		width       int
		arrangement tth2.ArrangementPolicy
		want        int
	}{
		{"unbounded", -1, 8, tth2.ArrangeRandom, 0},
		{"individual trials", 9, 8, tth2.ArrangeRandom, 9},
		{"rotation cycles", 24, 8, tth2.ArrangeRotate, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := trialConnectionWorkLimit(
				test.trials, test.width, test.arrangement,
			); got != test.want {
				t.Errorf("work limit = %d, want %d", got, test.want)
			}
		})
	}
}

func TestEffectiveBatchInterval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		tuning transportTuningConfig
		width  int
		want   time.Duration
	}{
		{
			"batch ceiling", transportTuningConfig{
				minBatchInterval: 50 * time.Millisecond,
			}, 8, 50 * time.Millisecond,
		},
		{
			"request ceiling", transportTuningConfig{
				minBatchInterval: 50 * time.Millisecond,
				requestRateMax:   100,
			}, 8, 80 * time.Millisecond,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := effectiveBatchInterval(test.tuning, test.width); got != test.want {
				t.Errorf("interval = %v, want %v", got, test.want)
			}
		})
	}
}

func TestCompactDuration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   time.Duration
		want string
	}{
		{201438946 * time.Nanosecond, "201ms"},
		{20153421 * time.Nanosecond, "20.2ms"},
		{1234567 * time.Nanosecond, "1.23ms"},
		{178999 * time.Nanosecond, "179us"},
		{12345 * time.Nanosecond, "12.3us"},
		{5 * time.Millisecond, "5ms"},
		{2014 * time.Millisecond, "2.01s"},
	}
	for _, test := range tests {
		if got := compactDuration(test.in); got != test.want {
			t.Errorf("compactDuration(%v) = %q, want %q",
				test.in, got, test.want)
		}
	}
}

func TestFittingUsesMeasuredWidthIndependentlyOfPriming(t *testing.T) {
	const measuredWidth = 5
	for _, warmupWidth := range []int{0, 2, 5, 10} {
		t.Run(fmt.Sprintf("priming_%d", warmupWidth), func(t *testing.T) {
			activeWidth := max(measuredWidth, warmupWidth)
			tuning := transportTuningConfig{
				maxConns: 1, fitSelected: true, batchRate: 100,
				minBatchInterval: 10 * time.Millisecond, requestRateMax: 100,
			}
			preview, _, err := previewDeliveryPlan(tuning, measuredWidth, activeWidth, 100, true, false)
			if err != nil {
				t.Fatal(err)
			}
			if preview.effectiveBatchInterval != 50*time.Millisecond || preview.activeBatchWidth != activeWidth {
				t.Errorf("preview = %+v", preview)
			}
			fit, err := fittedConnectionCount(time.Second, preview.effectiveBatchInterval)
			if err != nil {
				t.Fatal(err)
			}
			tuning.maxConns, tuning.pathFittedConns = fit, fit
			tuning.pathRTT, tuning.connectionFit = time.Second, connectionPathPing
			resolved, err := finishPipelinePlan(nil, tuning, measuredWidth, activeWidth, 100)
			if err != nil {
				t.Fatal(err)
			}
			livePlan := resolveDeliveryPlan(resolved, measuredWidth, resolved.connectionLimit, false)
			if resolved.connectionLimit != 25 || livePlan.effectiveBatchInterval != 50*time.Millisecond {
				t.Errorf("resolved = %+v, delivery = %+v", resolved, livePlan)
			}
			if warmupWidth == 10 && livePlan.intervalForWidth(warmupWidth) != 100*time.Millisecond {
				t.Errorf("priming interval = %v", livePlan.intervalForWidth(warmupWidth))
			}
		})
	}
}

func TestDetectPrimingStreamReview(t *testing.T) {
	for _, tc := range []struct {
		name                                        string
		warmup, warmupWidth, connections, workLimit int
		cap                                         int
		wantWidth, wantConnections                  int
		stop                                        bool
	}{
		{"narrower", 1, 1, 200, 0, 0, 2, 200, false},
		{"equal", 1, 2, 200, 0, 0, 2, 200, false},
		{"wider", 1, 8, 200, 0, 0, 8, 0, true},
		{"disabled", 0, 8, 200, 0, 0, 2, 200, false},
		{"finite work", 1, 8, 200, 100, 0, 8, 100, false},
		{"stream boundary", 1, 8, 128, 0, 0, 8, 128, false},
		{"beyond boundary", 1, 8, 129, 0, 0, 8, 0, true},
		{"positive acknowledgement", 1, 8, 200, 0, 200, 8, 200, false},
		{"unlimited acknowledgement", 1, 8, 200, 0, -1, 8, 200, false},
		{"acknowledged arithmetic overflow", 1, 8, math.MaxInt, 0, -1, 8, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := &materialised{
				reqs:       make([]*http.Request, 20),
				warmupReqs: make([]*http.Request, tc.warmupWidth),
			}
			activeWidth := requests.activeBatchWidth(2, tc.warmup)
			if activeWidth != tc.wantWidth {
				t.Fatalf("active width = %d", activeWidth)
			}
			tuning := transportTuningConfig{
				maxConns: tc.connections, fitSelected: true,
				connectionFit: connectionPathPing, pathFittedConns: tc.connections,
				parallelFitSet: tc.cap != 0, parallelFitMax: tc.cap,
				requestRateMax: 100, minBatchInterval: 10 * time.Millisecond,
			}
			preview, _, err := previewDeliveryPlan(tuning, 2, activeWidth, tc.workLimit, true, false)
			if err != nil {
				t.Fatal(err)
			}
			if preview.activeBatchWidth != tc.wantWidth || preview.effectiveBatchInterval != 20*time.Millisecond {
				t.Errorf("preview = %+v", preview)
			}
			resolved, err := finishPipelinePlan(nil, tuning, 2, activeWidth, tc.workLimit)
			if (err != nil) != tc.stop {
				t.Fatalf("review error = %v, stop = %t", err, tc.stop)
			}
			if !tc.stop && resolved.connectionLimit != tc.wantConnections {
				t.Errorf("connections = %d", resolved.connectionLimit)
			}
		})
	}
}
