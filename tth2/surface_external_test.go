package tth2_test

import (
	"context"
	"errors"
	"math"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tantosec/tturl/tth2"
)

func TestZeroLeaseLifecycle(t *testing.T) {
	for _, lease := range []*tth2.ConnLease{nil, new(tth2.ConnLease)} {
		if lease.Active() || lease.ID() != 0 {
			t.Fatal("lease without a connection reports a live connection")
		}
		batch, err := lease.SendBatch(t.Context(), nil)
		if batch != nil || !errors.Is(err, tth2.ErrLeaseClosed) {
			t.Fatalf("lease without a connection send = %v, %v", batch, err)
		}
		lease.Discard()
		if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNilMethodContextsPanic(t *testing.T) {
	var client tth2.Client
	var transport tth2.Transport
	for _, call := range []func(context.Context){
		func(ctx context.Context) { _, _ = client.SendBatch(ctx, nil) },
		func(ctx context.Context) { _ = client.StreamTrials(ctx, nil) },
		func(ctx context.Context) { _, _ = client.RunTrials(ctx, nil, 0) },
		func(ctx context.Context) { _, _ = client.Acquire(ctx, "") },
		func(ctx context.Context) { _, _ = transport.Ping(ctx, "") },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("nil context did not panic")
				}
			}()
			call(nil)
		}()
	}
}

func TestPaddingRequestWithoutHeaders(t *testing.T) {
	client, baseURL, closeServer := exampleClient(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			//nolint:gosec // local text/plain echo verifies request padding
			_, _ = w.Write([]byte(r.Header.Get("X-Pad-1")))
		},
	))
	defer closeServer()
	request := exampleRequest(baseURL + "/")
	request.Header = nil
	batch, err := client.SendBatch(t.Context(), []*http.Request{request},
		tth2.WithPadding(tth2.RequestPadding{Headers: 1}),
		tth2.WithResponseBodyCaptureBytes(1))
	if err != nil {
		t.Fatal(err)
	}
	if request.Header != nil || string(batch.Results[0].Response.Body.Captured) != "x" {
		t.Fatal("header padding failed to preserve an uninitialised header map")
	}
	request.Header = http.Header{"Content-Type": {"application/json"}}
	batch, err = client.SendBatch(t.Context(), []*http.Request{request},
		tth2.WithPadding(tth2.RequestPadding{BodyParams: 1}))
	if batch != nil || err == nil || !strings.Contains(err.Error(), "GetBody") {
		t.Fatalf("body padding without a factory = %v, %v", batch, err)
	}
}

func TestMaximumTrialBoundPreparation(t *testing.T) {
	var client tth2.Client
	stream := client.StreamTrials(t.Context(), []*http.Request{
		exampleRequest("https://example.com/one"),
		exampleRequest("https://example.com/two"),
	}, tth2.WithMaxTrials(int(^uint(0)>>1)))
	if stream.Err() != nil || stream.Conns() != 1 {
		t.Fatalf("maximum bound preparation = %v, connections %d", stream.Err(), stream.Conns())
	}
}

func TestPaddingOptionAndStreamOwnInputSlices(t *testing.T) {
	client, baseURL, closeServer := exampleClient(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			//nolint:gosec // local text/plain echo verifies request padding
			_, _ = w.Write([]byte(r.URL.RawQuery))
		},
	))
	defer closeServer()
	requests := []*http.Request{exampleRequest(baseURL + "/?value=1")}
	pads := []tth2.RequestPadding{{URLParams: 1}}
	var option tth2.SendOption = tth2.WithPadding(pads...)
	stream := client.StreamTrials(t.Context(), requests, option,
		tth2.WithMaxTrials(1), tth2.WithResponseBodyCaptureBytes(100))
	pads[0].URLParams = -1
	requests[0] = nil
	for trial := range stream.All() {
		if trial.Err != nil || string(trial.Results[0].Response.Body.Captured) != "_pad1=x&value=1" {
			t.Fatalf("owned configuration produced %+v", trial)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	snapshot := stream.Summary()
	snapshot.RankCounts[0][0] = 0
	if stream.Summary().RankCounts[0][0] != 1 {
		t.Fatal("summary snapshot shares live count storage")
	}
}

func TestStreamConsumerPanicReleasesWorkers(t *testing.T) {
	client, baseURL, closeServer := exampleClient(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) },
	))
	defer closeServer()
	request := exampleRequest(baseURL + "/")
	stream := client.StreamTrials(t.Context(), []*http.Request{request},
		tth2.WithBatchTimeout(time.Second))
	func() {
		defer func() {
			if got := recover(); got != "consumer stopped" {
				t.Fatalf("consumer panic = %v", got)
			}
		}()
		for range stream.All() {
			panic("consumer stopped")
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := client.SendBatch(ctx, []*http.Request{request}); err != nil {
		t.Fatalf("worker did not release the connection slot: %v", err)
	}
}

func TestNonFiniteRateConfiguration(t *testing.T) {
	for _, rate := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("non-finite run rate did not panic")
				}
			}()
			tth2.WithMaxRequestsPerSecond(rate)
		}()
		transport := &tth2.Transport{
			MaxRequestsPerSecondPerHost: rate,
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				t.Fatal("invalid transport configuration attempted a dial")
				return nil, errors.New("unexpected dial")
			},
		}
		client := tth2.Client{Transport: transport}
		batch, err := client.SendBatch(t.Context(), []*http.Request{exampleRequest("https://example.com/")})
		if batch != nil || err == nil || !strings.Contains(err.Error(), "finite") {
			t.Fatalf("invalid transport rate = %v, %v", batch, err)
		}
	}
}
