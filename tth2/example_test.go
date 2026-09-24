package tth2_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/tantosec/tturl/stats"
	"github.com/tantosec/tturl/tth2"
)

func ExampleClient_SendBatch_timingDifferential() {
	fastResponded := make(chan struct{})
	client, baseURL, closeServer := exampleClient(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/slow" {
				<-fastResponded
			}
			w.WriteHeader(http.StatusOK)
			if r.URL.Path == "/fast" {
				// Flushing a body byte writes the headers before releasing the
				// sibling; a header-only flush may just queue the headers.
				_, _ = w.Write([]byte("response"))
				w.(http.Flusher).Flush()
				close(fastResponded)
			}
		},
	))
	defer closeServer()

	requests := []*http.Request{
		exampleRequest(baseURL + "/slow"),
		exampleRequest(baseURL + "/fast"),
	}
	batch, err := client.SendBatch(context.Background(), requests)
	if err != nil {
		panic(err)
	}
	order, complete := batch.FullArrivalOrder()
	if !complete {
		panic("incomplete arrival order")
	}
	for _, request := range order {
		fmt.Println(requests[request].URL.Path)
	}

	// Output:
	// /fast
	// /slow
}

func ExampleClient_SendBatch_httpRace() {
	// The barrier makes handler overlap a deterministic observation: neither
	// handler can respond until both requests have reached it.
	var handlers sync.WaitGroup
	handlers.Add(2)
	client, baseURL, closeServer := exampleClient(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			handlers.Done()
			handlers.Wait()
			w.WriteHeader(http.StatusNoContent)
		},
	))
	defer closeServer()

	batch, err := client.SendBatch(context.Background(), []*http.Request{
		exampleRequest(baseURL + "/one"),
		exampleRequest(baseURL + "/two"),
	})
	if err != nil {
		panic(err)
	}
	fmt.Println("both handlers ran before either responded:",
		batch.AllResponsesArrived())

	// Output:
	// both handlers ran before either responded: true
}

func ExampleClient_StreamTrials_responseCapture() {
	client, baseURL, closeServer := exampleClient(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Evidence", "header")
			_, _ = w.Write([]byte("body evidence"))
		},
	))
	defer closeServer()

	stream := client.StreamTrials(context.Background(), []*http.Request{
		exampleRequest(baseURL + "/"),
	},
		tth2.WithMaxTrials(1),
		tth2.WithResponseHeaderCapture(),
		tth2.WithResponseBodyCaptureBytes(4),
	)
	for trial := range stream.All() {
		response := trial.Results[0].Response
		fmt.Println(response.Header.Get("X-Evidence"))
		fmt.Println(string(response.Body.Captured))
	}
	if err := stream.Err(); err != nil {
		panic(err)
	}

	// Output:
	// header
	// body
}

func ExampleClient_Acquire() {
	client, baseURL, closeServer := exampleClient(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) },
	))
	defer closeServer()
	request := exampleRequest(baseURL + "/")
	lease, err := client.Acquire(context.Background(), request.URL.Host)
	if err != nil {
		panic(err)
	}
	defer func() { _ = lease.Close() }()
	for range 2 {
		batch, err := lease.SendBatch(context.Background(), []*http.Request{request})
		if err != nil {
			panic(err)
		}
		fmt.Println(batch.Connection == lease.ID(), batch.AllResponsesArrived())
	}

	// Output:
	// true true
	// true true
}

func ExampleClient_RunTrials() {
	client, baseURL, closeServer := exampleClient(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) },
	))
	defer closeServer()
	summary, err := client.RunTrials(context.Background(), []*http.Request{
		exampleRequest(baseURL + "/"),
	}, 3, tth2.WithWarmup(1))
	if err != nil {
		panic(err)
	}
	fmt.Println(summary.Trials, summary.FailedTrials, summary.RankCounts)

	// Output:
	// 3 0 [[3]]
}

func ExampleClient_StreamTrials_rankStatistics() {
	// One connection runs one batch at a time. Each fast response flushes a
	// body byte before releasing its sibling, fixing the arrival order.
	fastResponded := make(chan struct{}, 1)
	client, baseURL, closeServer := exampleClient(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/slow" {
				<-fastResponded
			}
			w.WriteHeader(http.StatusOK)
			if r.URL.Path == "/fast" {
				// Flushing a body byte writes the headers before releasing the
				// sibling; a header-only flush may just queue the headers.
				_, _ = w.Write([]byte("response"))
				w.(http.Flusher).Flush()
				fastResponded <- struct{}{}
			}
		},
	))
	defer closeServer()
	stream := client.StreamTrials(context.Background(), []*http.Request{
		exampleRequest(baseURL + "/slow"),
		exampleRequest(baseURL + "/fast"),
	}, tth2.WithMaxTrials(4), tth2.WithMaxConns(1))
	var ranks stats.Welford
	for trial := range stream.All() {
		if trial.Err != nil {
			panic(trial.Err)
		}
		if !trial.Warmup {
			// Results remain keyed by request identity while positions rotate.
			ranks.Observe(float64(trial.Results[1].ArrivalRank))
		}
	}
	if err := stream.Err(); err != nil {
		panic(err)
	}
	fmt.Printf("n=%d mean rank=%g\n", ranks.Count(), ranks.Mean())

	// Output:
	// n=4 mean rank=0
}

func exampleClient(handler http.Handler) (*tth2.Client, string, func()) {
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()

	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport := &tth2.Transport{TLSClientConfig: &tls.Config{
		RootCAs: roots, MinVersion: tls.VersionTLS12,
	}}
	closeServer := func() {
		transport.CloseIdleConnections()
		server.Close()
	}
	return &tth2.Client{Transport: transport}, server.URL, closeServer
}

func exampleRequest(url string) *http.Request {
	request, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet, url, nil)
	if err != nil {
		panic(err)
	}
	return request
}
