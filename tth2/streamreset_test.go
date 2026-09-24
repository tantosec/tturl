package tth2_test

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"slices"
	"sort"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/tantosec/tturl/tth2"
)

// resetOneStreamServer serves exactly two batches of batchSize requests over
// one connection. For the first batch it resets the first stream *before*
// answering the rest, which is the ordering that leaves those answers on the
// wire when a client tears down on the reset. The second batch is answered
// normally.
//
// Both batches are answered in reverse stream order so that a correct client
// reports ranks that are a non-trivial permutation: an implementation that
// numbered ranks by request order rather than arrival order would still pass a
// same-order check.
func resetOneStreamServer(batchSize int) func(testing.TB, *http2.Framer, *bufio.Writer, net.Conn) {
	return func(tb testing.TB, framer *http2.Framer, bw *bufio.Writer, _ net.Conn) {
		tb.Helper()
		if err := doServerPreface(tb, framer, bw); err != nil {
			tb.Logf("resetOneStreamServer: preface: %v", err)
			return
		}

		for batch := range 2 {
			ids := make([]uint32, 0, batchSize)
			for len(ids) < batchSize {
				f, err := framer.ReadFrame()
				if err != nil {
					tb.Logf("resetOneStreamServer: batch %d read: %v", batch, err)
					return
				}
				if hf, ok := f.(*http2.MetaHeadersFrame); ok {
					ids = append(ids, hf.StreamID)
				}
			}

			answer := ids
			if batch == 0 {
				// Reset the first stream, then answer the others. A client that
				// abandons the connection here leaves those answers unread.
				if err := framer.WriteRSTStream(ids[0], http2.ErrCodeRefusedStream); err != nil {
					tb.Logf("resetOneStreamServer: WriteRSTStream: %v", err)
					return
				}
				if err := bw.Flush(); err != nil {
					tb.Logf("resetOneStreamServer: flush RST: %v", err)
					return
				}
				answer = ids[1:]
			}

			for _, sid := range slices.Backward(answer) {
				if err := writeOKResponse(framer, bw, sid); err != nil {
					tb.Logf("resetOneStreamServer: WriteHeaders: %v", err)
					return
				}
			}
		}

		// Stay alive until the client hangs up.
		for {
			if _, err := framer.ReadFrame(); err != nil {
				return
			}
		}
	}
}

// TestStreamResetLeavesConnectionReusable verifies that a partial reset drains
// fully and does not contaminate a later batch on the pooled connection.
func TestStreamResetLeavesConnectionReusable(t *testing.T) {
	t.Parallel()
	const batchSize = 3
	addr, tlsCfg := startRawH2Server(t, resetOneStreamServer(batchSize))

	c := newClient(t, tlsCfg)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	reqs := func() []*http.Request {
		out := make([]*http.Request, batchSize)
		for i := range out {
			out[i] = mustReq(t, "https://"+addr+"/")
		}
		return out
	}

	if _, err := c.SendBatch(ctx, reqs()); err == nil {
		t.Fatal("first SendBatch: got nil error, want the server's stream reset")
	} else {
		var se *tth2.StreamError
		if !errors.As(err, &se) {
			t.Fatalf("first SendBatch: got %v, want a *tth2.StreamError", err)
		}
		if se.Code != http2.ErrCodeRefusedStream {
			t.Errorf("StreamError.Code = %v, want %v", se.Code, http2.ErrCodeRefusedStream)
		}
	}

	batch, err := c.SendBatch(ctx, reqs())
	if err != nil {
		t.Fatalf("second SendBatch: %v", err)
	}
	results := batch.Results
	if len(results) != batchSize {
		t.Fatalf("second SendBatch returned %d results, want %d", len(results), batchSize)
	}

	// The ranks of a batch are a permutation of [0, batchSize): every request
	// arrives at exactly one place, and every place is taken.
	ranks := make([]int, batchSize)
	for i, r := range results {
		ranks[i] = r.ArrivalRank
	}
	sorted := append([]int(nil), ranks...)
	sort.Ints(sorted)
	for want, got := range sorted {
		if got != want {
			t.Fatalf("second batch ranks %v are not a permutation of [0,%d)", ranks, batchSize)
		}
	}
}

// TestStreamResetWithHungSiblingsRespectsContext verifies that cancellation
// bounds the drain and supersedes a survivable reset error.
func TestStreamResetWithHungSiblingsRespectsContext(t *testing.T) {
	t.Parallel()
	const batchSize = 3
	addr, tlsCfg := startRawH2Server(t,
		func(tb testing.TB, framer *http2.Framer, bw *bufio.Writer, _ net.Conn) {
			tb.Helper()
			if err := doServerPreface(tb, framer, bw); err != nil {
				tb.Logf("preface: %v", err)
				return
			}
			seen := 0
			for seen < batchSize {
				f, err := framer.ReadFrame()
				if err != nil {
					tb.Logf("read: %v", err)
					return
				}
				if hf, ok := f.(*http2.MetaHeadersFrame); ok {
					seen++
					if seen == 1 {
						if err := framer.WriteRSTStream(hf.StreamID,
							http2.ErrCodeRefusedStream); err != nil {
							tb.Logf("WriteRSTStream: %v", err)
							return
						}
						_ = bw.Flush()
					}
				}
			}
			// Answer nothing further; wait for the client to give up.
			for {
				if _, err := framer.ReadFrame(); err != nil {
					return
				}
			}
		})

	c := newClient(t, tlsCfg)
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()

	reqs := make([]*http.Request, batchSize)
	for i := range reqs {
		reqs[i] = mustReq(t, "https://"+addr+"/")
	}

	done := make(chan error, 1)
	go func() {
		_, err := c.SendBatch(ctx, reqs)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("SendBatch: got nil error, want the context deadline")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("SendBatch: got %v, want context.DeadlineExceeded", err)
		}
		if _, ok := errors.AsType[*tth2.StreamError](err); ok {
			t.Errorf("SendBatch reported a reusable stream reset (%v) for a "+
				"connection abandoned mid-read", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SendBatch did not return; the reset batch waits on siblings " +
			"that never arrive, so only the context can end it")
	}
}

// TestStreamResetYieldsPartialBatch verifies the reset result and surviving
// siblings' ranks.
//
// resetOneStreamServer refuses the first request and answers the rest in
// reverse, so every value here is derivable: request 0 never arrived, request 2
// arrived first and request 1 second.
func TestStreamResetYieldsPartialBatch(t *testing.T) {
	t.Parallel()
	const batchSize = 3
	addr, tlsCfg := startRawH2Server(t, resetOneStreamServer(batchSize))

	c := newClient(t, tlsCfg)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	reqs := make([]*http.Request, batchSize)
	for i := range reqs {
		reqs[i] = mustReq(t, "https://"+addr+"/")
	}

	batch, err := c.SendBatch(ctx, reqs)
	if _, ok := errors.AsType[*tth2.StreamError](err); !ok {
		t.Fatalf("SendBatch: got %v, want a *tth2.StreamError", err)
	}
	if batch == nil {
		t.Fatal("SendBatch returned no batch alongside the reset; the siblings arrived")
	}
	if batch.AllResponsesArrived() {
		t.Error("AllResponsesArrived = true with a pre-header reset")
	}
	if order, ok := batch.FullArrivalOrder(); ok || order != nil {
		t.Errorf("FullArrivalOrder = %v, %v; want nil, false", order, ok)
	}
	if len(batch.Results) != batchSize {
		t.Fatalf("len(Results) = %d, want %d: one result per request, refused or not",
			len(batch.Results), batchSize)
	}

	// The refused request: no response, no rank, and the reset that says so.
	refused := batch.Results[0]
	if refused.Reset == nil {
		t.Errorf("Results[0].Reset is nil, want the server's reset")
	} else if refused.Reset.Code != http2.ErrCodeRefusedStream {
		t.Errorf("Results[0].Reset.Code = %v, want %v", refused.Reset.Code, http2.ErrCodeRefusedStream)
	}
	if refused.Response != nil {
		t.Errorf("Results[0].Response = %v, want nil: the reset preceded the headers",
			refused.Response.Status)
	}
	if refused.ArrivalRank != -1 {
		t.Errorf("Results[0].ArrivalRank = %d, want -1: it never arrived", refused.ArrivalRank)
	}

	// The survivors: answered in reverse, so they take the two ranks there are.
	for i, want := range map[int]int{1: 1, 2: 0} {
		r := batch.Results[i]
		if r.Reset != nil {
			t.Errorf("Results[%d].Reset = %v, want nil: only the first stream was reset", i, r.Reset)
		}
		if r.Response == nil {
			t.Errorf("Results[%d].Response is nil, want the server's answer", i)
		}
		if r.ArrivalRank != want {
			t.Errorf("Results[%d].ArrivalRank = %d, want %d", i, r.ArrivalRank, want)
		}
	}

	// The arrival order covers what arrived, and nothing else.
	if got := batch.ArrivalOrder; !slices.Equal(got, []int{2, 1}) {
		t.Errorf("ArrivalOrder = %v, want [2 1]: the arrivals in order, the refused request absent", got)
	}
}

// TestMultipleStreamResetsYieldOneResultPerRequest verifies that each
// pre-header reset remains visible even though SendBatch returns only one
// stream-scoped error for the batch.
func TestMultipleStreamResetsYieldOneResultPerRequest(t *testing.T) {
	t.Parallel()
	const batchSize = 4
	addr, tlsCfg := startRawH2Server(t,
		func(tb testing.TB, framer *http2.Framer, bw *bufio.Writer, _ net.Conn) {
			tb.Helper()
			if err := doServerPreface(tb, framer, bw); err != nil {
				tb.Logf("preface: %v", err)
				return
			}
			ids := make([]uint32, 0, batchSize)
			for len(ids) < batchSize {
				frame, err := framer.ReadFrame()
				if err != nil {
					tb.Logf("read: %v", err)
					return
				}
				if headers, ok := frame.(*http2.MetaHeadersFrame); ok {
					ids = append(ids, headers.StreamID)
				}
			}
			for _, id := range ids[:2] {
				if err := framer.WriteRSTStream(id, http2.ErrCodeRefusedStream); err != nil {
					tb.Logf("WriteRSTStream: %v", err)
					return
				}
			}
			if err := bw.Flush(); err != nil {
				tb.Logf("flush resets: %v", err)
				return
			}
			for _, id := range slices.Backward(ids[2:]) {
				if err := writeOKResponse(framer, bw, id); err != nil {
					tb.Logf("WriteHeaders: %v", err)
					return
				}
			}
			for {
				if _, err := framer.ReadFrame(); err != nil {
					return
				}
			}
		})

	client := newClient(t, tlsCfg)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	requests := make([]*http.Request, batchSize)
	for i := range requests {
		requests[i] = mustReq(t, "https://"+addr+"/")
	}

	batch, err := client.SendBatch(ctx, requests)
	if _, ok := errors.AsType[*tth2.StreamError](err); !ok {
		t.Fatalf("SendBatch: got %v, want a *tth2.StreamError", err)
	}
	if batch == nil || len(batch.Results) != batchSize {
		t.Fatalf("batch results = %v, want %d results", batch, batchSize)
	}
	for i := range 2 {
		result := batch.Results[i]
		if result.Arrived() || result.Reset == nil || result.ArrivalRank != -1 {
			t.Errorf("result %d = %+v, want a pre-header reset", i, result)
		}
	}
	if got := batch.ArrivalOrder; !slices.Equal(got, []int{3, 2}) {
		t.Errorf("ArrivalOrder = %v, want [3 2]", got)
	}
}

// TestStreamResetAfterHeadersKeepsItsRank verifies that a post-header reset
// preserves the response and arrival rank.
func TestStreamResetAfterHeadersKeepsItsRank(t *testing.T) {
	t.Parallel()
	const batchSize = 2
	addr, tlsCfg := startRawH2Server(t,
		func(tb testing.TB, framer *http2.Framer, bw *bufio.Writer, _ net.Conn) {
			tb.Helper()
			if err := doServerPreface(tb, framer, bw); err != nil {
				tb.Logf("preface: %v", err)
				return
			}
			ids := make([]uint32, 0, batchSize)
			for len(ids) < batchSize {
				f, err := framer.ReadFrame()
				if err != nil {
					tb.Logf("read: %v", err)
					return
				}
				if hf, ok := f.(*http2.MetaHeadersFrame); ok {
					ids = append(ids, hf.StreamID)
				}
			}
			// The first stream is answered first and then abandoned mid-body; the
			// second completes normally.
			if err := writeHeadersNoEnd(framer, bw, ids[0]); err != nil {
				tb.Logf("WriteHeaders: %v", err)
				return
			}
			if err := framer.WriteRSTStream(ids[0], http2.ErrCodeInternal); err != nil {
				tb.Logf("WriteRSTStream: %v", err)
				return
			}
			if err := bw.Flush(); err != nil {
				tb.Logf("flush RST: %v", err)
				return
			}
			if err := writeOKResponse(framer, bw, ids[1]); err != nil {
				tb.Logf("WriteHeaders: %v", err)
				return
			}
			for {
				if _, err := framer.ReadFrame(); err != nil {
					return
				}
			}
		})

	c := newClient(t, tlsCfg)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	reqs := make([]*http.Request, batchSize)
	for i := range reqs {
		reqs[i] = mustReq(t, "https://"+addr+"/")
	}

	batch, err := c.SendBatch(ctx, reqs)
	if _, ok := errors.AsType[*tth2.StreamError](err); !ok {
		t.Fatalf("SendBatch: got %v, want a *tth2.StreamError", err)
	}
	if batch == nil {
		t.Fatal("SendBatch returned no batch alongside the reset")
	}
	if !batch.AllResponsesArrived() {
		t.Error("AllResponsesArrived = false for a post-header reset")
	}
	if order, ok := batch.FullArrivalOrder(); !ok ||
		!slices.Equal(order, []int{0, 1}) {
		t.Errorf("FullArrivalOrder = %v, %v; want [0 1], true", order, ok)
	}
	reset := batch.Results[0]
	if reset.Reset == nil {
		t.Fatalf("Results[0].Reset is nil, want the server's reset")
	}
	if reset.Response == nil {
		t.Fatalf("Results[0].Response is nil: the headers arrived before the reset")
	}
	if reset.Response.StatusCode != 200 {
		t.Errorf("Results[0].Response.StatusCode = %d, want 200", reset.Response.StatusCode)
	}
	// It arrived first, and both requests arrived, so the ranks are a full
	// permutation despite the reset.
	if reset.ArrivalRank != 0 {
		t.Errorf("Results[0].ArrivalRank = %d, want 0: it answered first", reset.ArrivalRank)
	}
	if got := batch.Results[1].ArrivalRank; got != 1 {
		t.Errorf("Results[1].ArrivalRank = %d, want 1", got)
	}
	if got := batch.ArrivalOrder; !slices.Equal(got, []int{0, 1}) {
		t.Errorf("ArrivalOrder = %v, want [0 1]: a reset after the answer still arrived", got)
	}
}
