package tth2_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/tantosec/tturl/internal/h2test"
	"github.com/tantosec/tturl/tth2"
)

func TestMaxResponseBodyBytesBoundaries(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		w.Header().Set("Trailer", "X-End")
		for remaining := n; remaining > 0; {
			chunk := min(2, remaining)
			_, _ = w.Write(bytes.Repeat([]byte{'x'}, chunk))
			remaining -= chunk
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		w.Header().Set("X-End", "complete")
	}))

	for _, test := range []struct {
		name      string
		bodyBytes int
		wantLimit bool
	}{
		{name: "below", bodyBytes: 3},
		{name: "exact", bodyBytes: 4},
		{name: "crossing", bodyBytes: 5, wantLimit: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := newClient(t, tlsCfg)
			batch, err := client.SendBatch(t.Context(), []*http.Request{
				mustReq(t, "https://"+addr+"/?n="+strconv.Itoa(test.bodyBytes)),
			}, tth2.WithMaxResponseBodyBytes(4),
				tth2.WithResponseBodyCaptureBytes(2),
				tth2.WithResponseHeaderCapture())
			if test.wantLimit {
				if batch != nil {
					t.Fatalf("BatchResult = %+v, want nil", batch)
				}
				var limit *tth2.ResponseBodyLimitError
				if !errors.As(err, &limit) || limit.Limit != 4 ||
					limit.BytesAccepted != 4 || limit.RequestIndex != 0 ||
					limit.StreamID == 0 {
					t.Fatalf("error = %#v, want body limit details", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("SendBatch: %v", err)
			}
			body := batch.Results[0].Response.Body
			wantDigest := sha256.Sum256(bytes.Repeat(
				[]byte{'x'}, test.bodyBytes))
			if body.BytesReceived != int64(test.bodyBytes) ||
				len(body.Captured) != 2 || body.SHA256 != wantDigest {
				t.Errorf("body = %+v, want %d accepted bytes and 2 captured",
					body, test.bodyBytes)
			}
			if got := batch.Results[0].Response.Trailer.Get("X-End"); got != "complete" {
				t.Errorf("trailer = %q, want complete", got)
			}
		})
	}
}

func TestResponseBodyLimitFailsSiblingsAndDiscardsConnection(t *testing.T) {
	t.Parallel()
	var slowStarted sync.Once
	var overflows atomic.Int32
	started := make(chan struct{})
	addr, tlsCfg := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		switch r.URL.Path {
		case "/slow":
			slowStarted.Do(func() { close(started) })
			<-r.Context().Done()
		case "/overflow":
			overflows.Add(1)
			_, _ = w.Write([]byte("12345"))
		default:
			_, _ = w.Write([]byte("1234"))
		}
	}))
	client := newClient(t, tlsCfg)
	healthy, err := client.SendBatch(t.Context(), []*http.Request{
		mustReq(t, "https://"+addr+"/healthy"),
	}, tth2.WithMaxResponseBodyBytes(4))
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct {
		batch *tth2.BatchResult
		err   error
	}, 1)
	go func() {
		batch, err := client.SendBatch(t.Context(), []*http.Request{
			mustReq(t, "https://"+addr+"/slow"),
			mustReq(t, "https://"+addr+"/overflow"),
		}, tth2.WithMaxResponseBodyBytes(4))
		done <- struct {
			batch *tth2.BatchResult
			err   error
		}{batch, err}
	}()
	awaitSignals(t, started, 1, "stalled sibling")
	var failed struct {
		batch *tth2.BatchResult
		err   error
	}
	select {
	case failed = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("body limit waited for a stalled sibling")
	}
	var limit *tth2.ResponseBodyLimitError
	if failed.batch != nil || !errors.As(failed.err, &limit) ||
		limit.RequestIndex != 1 || limit.BytesAccepted != 4 {
		t.Fatalf("failed batch/error = %+v/%#v", failed.batch, failed.err)
	}
	if got := overflows.Load(); got != 1 {
		t.Fatalf("overflow attempts = %d, want 1", got)
	}

	next, err := client.SendBatch(t.Context(), []*http.Request{
		mustReq(t, "https://"+addr+"/healthy"),
	}, tth2.WithMaxResponseBodyBytes(4))
	if err != nil {
		t.Fatal(err)
	}
	if next.Connection == healthy.Connection {
		t.Fatalf("locally terminated connection %d was reused", next.Connection)
	}
}

func TestResponseBodyLimitIgnoresHeadersAndWinsOrderedReset(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		headerFirst   bool
		contentLength string
		reset         bool
	}{
		{name: "before final headers"},
		{name: "absent declared length", headerFirst: true},
		{
			name:        "dishonest declared length before peer reset",
			headerFirst: true, contentLength: "1", reset: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			addr, tlsCfg := startRawH2Server(t, func(
				tb testing.TB,
				framer *http2.Framer,
				bw *bufio.Writer,
				_ net.Conn,
			) {
				if err := doServerPreface(tb, framer, bw); err != nil {
					tb.Logf("preface: %v", err)
					return
				}
				var streamID uint32
				for streamID == 0 {
					frame, err := framer.ReadFrame()
					if err != nil {
						return
					}
					if headers, ok := frame.(*http2.MetaHeadersFrame); ok {
						streamID = headers.StreamID
					}
				}
				if test.headerFirst {
					var block bytes.Buffer
					encoder := hpack.NewEncoder(&block)
					_ = encoder.WriteField(hpack.HeaderField{
						Name: ":status", Value: "200",
					})
					if test.contentLength != "" {
						_ = encoder.WriteField(hpack.HeaderField{
							Name: "content-length", Value: test.contentLength,
						})
					}
					if err := framer.WriteHeaders(http2.HeadersFrameParam{
						StreamID: streamID, BlockFragment: block.Bytes(),
						EndHeaders: true,
					}); err != nil {
						return
					}
				} else {
					var block bytes.Buffer
					_ = hpack.NewEncoder(&block).WriteField(hpack.HeaderField{
						Name: ":status", Value: "103",
					})
					if err := framer.WriteHeaders(http2.HeadersFrameParam{
						StreamID: streamID, BlockFragment: block.Bytes(),
						EndHeaders: true,
					}); err != nil {
						return
					}
				}
				if err := framer.WriteData(
					streamID, false, []byte("12345")); err != nil {
					return
				}
				if test.reset {
					_ = framer.WriteRSTStream(streamID, http2.ErrCodeCancel)
				}
				_ = bw.Flush()
				for {
					if _, err := framer.ReadFrame(); err != nil {
						return
					}
				}
			})
			client := newClient(t, tlsCfg)
			batch, err := client.SendBatch(t.Context(), []*http.Request{
				mustReq(t, "https://"+addr+"/"),
			}, tth2.WithMaxResponseBodyBytes(4))
			var limit *tth2.ResponseBodyLimitError
			if batch != nil || !errors.As(err, &limit) {
				t.Fatalf("batch/error = %+v/%#v, want body limit", batch, err)
			}
			if _, ok := errors.AsType[*tth2.StreamError](err); ok {
				t.Fatalf("body limit was reported as peer reset: %v", err)
			}
		})
	}
}

func TestResponseBodyLimitOutranksSiblingReset(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := startRawH2Server(t, func(
		tb testing.TB,
		framer *http2.Framer,
		bw *bufio.Writer,
		_ net.Conn,
	) {
		if err := doServerPreface(tb, framer, bw); err != nil {
			tb.Logf("preface: %v", err)
			return
		}
		var streamIDs []uint32
		for len(streamIDs) < 2 {
			frame, err := framer.ReadFrame()
			if err != nil {
				return
			}
			if headers, ok := frame.(*http2.MetaHeadersFrame); ok {
				streamIDs = append(streamIDs, headers.StreamID)
			}
		}
		if err := framer.WriteRSTStream(
			streamIDs[0], http2.ErrCodeCancel); err != nil {
			return
		}
		var block bytes.Buffer
		_ = hpack.NewEncoder(&block).WriteField(hpack.HeaderField{
			Name: ":status", Value: "200",
		})
		if err := framer.WriteHeaders(http2.HeadersFrameParam{
			StreamID: streamIDs[1], BlockFragment: block.Bytes(),
			EndHeaders: true,
		}); err != nil {
			return
		}
		_ = framer.WriteData(streamIDs[1], false, []byte("12345"))
		_ = bw.Flush()
		for {
			if _, err := framer.ReadFrame(); err != nil {
				return
			}
		}
	})
	client := newClient(t, tlsCfg)
	batch, err := client.SendBatch(t.Context(), []*http.Request{
		mustReq(t, "https://"+addr+"/reset"),
		mustReq(t, "https://"+addr+"/overflow"),
	}, tth2.WithMaxResponseBodyBytes(4))
	var limit *tth2.ResponseBodyLimitError
	if batch != nil || !errors.As(err, &limit) || limit.RequestIndex != 1 {
		t.Fatalf("batch/error = %+v/%#v, want second-request body limit",
			batch, err)
	}
}

func TestResponseBodyLimitAcrossTrialAndLeasePaths(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		_, _ = w.Write([]byte("12345"))
	}))
	req := mustReq(t, "https://"+addr+"/")

	streamClient := newClient(t, tlsCfg)
	stream := streamClient.StreamTrials(t.Context(), []*http.Request{req},
		tth2.WithMaxTrials(1), tth2.WithArrangementPolicy(tth2.ArrangeNone),
		tth2.WithMaxResponseBodyBytes(4))
	var streamed []*tth2.Trial
	for trial := range stream.All() {
		streamed = append(streamed, trial)
	}
	var streamLimit *tth2.ResponseBodyLimitError
	if len(streamed) != 1 || streamed[0].Err == nil ||
		!errors.As(streamed[0].Err, &streamLimit) ||
		len(streamed[0].Results) != 0 || stream.Err() != nil ||
		stream.Summary().FailedTrials != 1 {
		t.Fatalf("streamed trials/summary/error = %+v/%+v/%v",
			streamed, stream.Summary(), stream.Err())
	}

	runClient := newClient(t, tlsCfg)
	summary, err := runClient.RunTrials(t.Context(), []*http.Request{req}, 1,
		tth2.WithArrangementPolicy(tth2.ArrangeNone),
		tth2.WithMaxResponseBodyBytes(4))
	if err != nil || summary.FailedTrials != 1 || summary.Trials != 0 {
		t.Fatalf("RunTrials summary/error = %+v/%v", summary, err)
	}

	leaseClient := newClient(t, tlsCfg)
	lease, err := leaseClient.Acquire(t.Context(), addr)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := lease.SendBatch(t.Context(), []*http.Request{req},
		tth2.WithMaxResponseBodyBytes(4))
	var leaseLimit *tth2.ResponseBodyLimitError
	if batch != nil || !errors.As(err, &leaseLimit) || lease.Active() {
		t.Fatalf("lease batch/error/active = %+v/%#v/%t",
			batch, err, lease.Active())
	}
	if _, err := lease.SendBatch(t.Context(), []*http.Request{req}); !errors.Is(err, tth2.ErrLeaseInvalid) {
		t.Errorf("send after local termination = %v, want ErrLeaseInvalid", err)
	}
}

func TestBatchTimeoutFailureAndConnectionDisposal(t *testing.T) {
	t.Parallel()
	var slowAttempts atomic.Int32
	addr, tlsCfg := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		if r.URL.Path == "/slow" {
			slowAttempts.Add(1)
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	client := newClient(t, tlsCfg)
	first, err := client.SendBatch(t.Context(), []*http.Request{
		mustReq(t, "https://"+addr+"/healthy"),
	}, tth2.WithBatchTimeout(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	batch, err := client.SendBatch(t.Context(), []*http.Request{
		mustReq(t, "https://"+addr+"/slow"),
	}, tth2.WithBatchTimeout(50*time.Millisecond))
	var timeout *tth2.BatchTimeoutError
	if batch != nil || !errors.As(err, &timeout) ||
		timeout.Limit != 50*time.Millisecond {
		t.Fatalf("batch/error = %+v/%#v", batch, err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("batch timeout returned after %v", elapsed)
	}
	if got := slowAttempts.Load(); got != 1 {
		t.Fatalf("timed-out batch attempts = %d, want 1", got)
	}
	next, err := client.SendBatch(t.Context(), []*http.Request{
		mustReq(t, "https://"+addr+"/healthy"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if next.Connection == first.Connection {
		t.Fatalf("timed-out connection %d was reused", next.Connection)
	}
}

func TestBatchTimeoutStopsContinuousResponse(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		flusher := w.(http.Flusher)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			if _, err := w.Write([]byte("x")); err != nil {
				return
			}
			flusher.Flush()
			select {
			case <-ticker.C:
			case <-r.Context().Done():
				return
			}
		}
	}))
	client := newClient(t, tlsCfg)
	batch, err := client.SendBatch(t.Context(), []*http.Request{
		mustReq(t, "https://"+addr+"/"),
	}, tth2.WithBatchTimeout(50*time.Millisecond))
	var timeout *tth2.BatchTimeoutError
	if batch != nil || !errors.As(err, &timeout) {
		t.Fatalf("batch/error = %+v/%#v, want batch timeout", batch, err)
	}
}

func TestBatchTimeoutIncludesReleaseDelay(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	client := newClient(t, tlsCfg)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		"https://"+addr+"/", bytes.NewBufferString("request"))
	if err != nil {
		t.Fatal(err)
	}
	batch, err := client.SendBatch(t.Context(), []*http.Request{req},
		tth2.WithReleaseDelay(time.Second),
		tth2.WithBatchTimeout(50*time.Millisecond))
	var timeout *tth2.BatchTimeoutError
	if batch != nil || !errors.As(err, &timeout) {
		t.Fatalf("batch/error = %+v/%#v, want batch timeout", batch, err)
	}
}

func TestBatchTimeoutFirstCause(t *testing.T) {
	t.Parallel()
	started := make(chan struct{}, 2)
	addr, tlsCfg := h2test.Serve(t, http.HandlerFunc(func(
		_ http.ResponseWriter,
		r *http.Request,
	) {
		started <- struct{}{}
		<-r.Context().Done()
	}))

	t.Run("parent cancellation first", func(t *testing.T) {
		client := newClient(t, tlsCfg)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := client.SendBatch(ctx, []*http.Request{
				mustReq(t, "https://"+addr+"/"),
			}, tth2.WithBatchTimeout(time.Second))
			done <- err
		}()
		awaitSignals(t, started, 1, "request handler")
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	})

	t.Run("local timeout first", func(t *testing.T) {
		client := newClient(t, tlsCfg)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := client.SendBatch(ctx, []*http.Request{
				mustReq(t, "https://"+addr+"/"),
			}, tth2.WithBatchTimeout(50*time.Millisecond))
			done <- err
		}()
		awaitSignals(t, started, 1, "request handler")
		err := <-done
		cancel()
		if _, ok := errors.AsType[*tth2.BatchTimeoutError](err); !ok {
			t.Fatalf("error = %v, want BatchTimeoutError", err)
		}
	})
}

func TestResponseLimitOptionsRejectNegativeValues(t *testing.T) {
	for _, test := range []struct {
		name string
		call func()
	}{
		{name: "body", call: func() { tth2.WithMaxResponseBodyBytes(-1) }},
		{name: "timeout", call: func() { tth2.WithBatchTimeout(-1) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("negative option did not panic")
				}
			}()
			test.call()
		})
	}
}

func TestResponseLimitErrorsAreASCII(t *testing.T) {
	t.Parallel()
	errs := []error{
		&tth2.ResponseBodyLimitError{
			RequestIndex: 1, StreamID: 3, Limit: 4, BytesAccepted: 4,
		},
		&tth2.BatchTimeoutError{Limit: time.Microsecond},
	}
	for _, err := range errs {
		for _, b := range []byte(err.Error()) {
			if b >= 0x80 {
				t.Errorf("%T error contains non-ASCII byte 0x%02x: %q",
					err, b, err)
			}
		}
	}
}
