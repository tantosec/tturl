package tth2_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/tantosec/tturl/tth2"
)

// TestTrailersDoNotDisplaceTheResponse verifies that trailers preserve response
// fields and do not change the rank assigned when final headers arrive.
func TestTrailersDoNotDisplaceTheResponse(t *testing.T) {
	t.Parallel()
	const body, checksum = "trailed body", "abc123"

	addr, tlsCfg := startRawH2Server(t, func(tb testing.TB, framer *http2.Framer, bw *bufio.Writer, _ net.Conn) {
		tb.Helper()
		if err := doServerPreface(tb, framer, bw); err != nil {
			tb.Logf("preface: %v", err)
			return
		}
		var ids []uint32
		for len(ids) < 2 {
			f, err := framer.ReadFrame()
			if err != nil {
				tb.Logf("read: %v", err)
				return
			}
			if hf, ok := f.(*http2.MetaHeadersFrame); ok {
				ids = append(ids, hf.StreamID)
			}
		}
		// Stream 1's response head, then its body; stream 0's head; then stream 1's
		// trailers. So stream 1 answered first but finished last.
		var hbuf bytes.Buffer
		enc := hpack.NewEncoder(&hbuf)
		for _, hf := range []hpack.HeaderField{
			{Name: ":status", Value: "200"},
			{Name: "content-type", Value: "text/plain"},
		} {
			if err := enc.WriteField(hf); err != nil {
				tb.Logf("encode: %v", err)
				return
			}
		}
		if err := framer.WriteHeaders(http2.HeadersFrameParam{
			StreamID: ids[1], BlockFragment: hbuf.Bytes(), EndHeaders: true,
		}); err != nil {
			tb.Logf("head: %v", err)
			return
		}
		if err := framer.WriteData(ids[1], false, []byte(body)); err != nil {
			tb.Logf("data: %v", err)
			return
		}
		if err := bw.Flush(); err != nil {
			tb.Logf("flush: %v", err)
			return
		}
		if err := writeOKResponse(framer, bw, ids[0]); err != nil {
			tb.Logf("second head: %v", err)
			return
		}
		var tbuf bytes.Buffer
		tenc := hpack.NewEncoder(&tbuf)
		if err := tenc.WriteField(hpack.HeaderField{Name: "x-checksum", Value: checksum}); err != nil {
			tb.Logf("encode trailer: %v", err)
			return
		}
		if err := framer.WriteHeaders(http2.HeadersFrameParam{
			StreamID: ids[1], BlockFragment: tbuf.Bytes(), EndStream: true, EndHeaders: true,
		}); err != nil {
			tb.Logf("trailers: %v", err)
			return
		}
		if err := bw.Flush(); err != nil {
			tb.Logf("flush trailers: %v", err)
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
	batch, err := c.SendBatch(ctx, []*http.Request{
		mustReq(t, "https://"+addr+"/plain"),
		mustReq(t, "https://"+addr+"/trailed"),
	}, tth2.WithResponseHeaderCapture(),
		tth2.WithResponseBodyCaptureBytes(tth2.UnlimitedResponseBytes))
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if order, ok := batch.FullArrivalOrder(); !ok ||
		len(order) != 2 || order[0] != 1 || order[1] != 0 {
		t.Fatalf("FullArrivalOrder = %v, %v; want [1 0], true", order, ok)
	}

	trailed := batch.Results[1]
	if trailed.Response.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (the response head, not the trailers)", trailed.Response.StatusCode)
	}
	if got := trailed.Response.Header.Get("Content-Type"); got != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain (the header must survive the trailers)", got)
	}
	got := trailed.Response.Body.Captured
	if string(got) != body {
		t.Errorf("body = %q, want %q", got, body)
	}
	if v := trailed.Response.Trailer.Get("X-Checksum"); v != checksum {
		t.Errorf("Trailer[X-Checksum] = %q, want %q", v, checksum)
	}
	if trailed.Response.Header.Get("X-Checksum") != "" {
		t.Error("the trailer field leaked into Header")
	}

	// The untrailed stream has no trailers at all, rather than an empty map.
	if batch.Results[0].Response.Trailer != nil {
		t.Errorf("untrailed response carries Trailer = %v, want nil", batch.Results[0].Response.Trailer)
	}

	// Arrival follows the response heads: stream 1 answered first even though its
	// trailers landed after stream 0's answer.
	if got := batch.ArrivalOrder; len(got) != 2 || got[0] != 1 || got[1] != 0 {
		t.Errorf("ArrivalOrder = %v, want [1 0]: ranks follow the response heads, not the last frame", got)
	}
}

// TestInformationalResponseDoesNotFixArrival sends a 103 before the final 200
// on one stream. The informational head must neither become the response nor
// fix the arrival — the server has no answer yet when it sends one.
func TestInformationalResponseDoesNotFixArrival(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := startRawH2Server(t, func(tb testing.TB, framer *http2.Framer, bw *bufio.Writer, _ net.Conn) {
		tb.Helper()
		if err := doServerPreface(tb, framer, bw); err != nil {
			tb.Logf("preface: %v", err)
			return
		}
		var ids []uint32
		for len(ids) < 2 {
			f, err := framer.ReadFrame()
			if err != nil {
				tb.Logf("read: %v", err)
				return
			}
			if hf, ok := f.(*http2.MetaHeadersFrame); ok {
				ids = append(ids, hf.StreamID)
			}
		}
		// Stream 0 hints early, then stream 1 answers, then stream 0 answers. If the
		// hint fixed stream 0's arrival it would rank first; by its answer it is
		// last.
		var ibuf bytes.Buffer
		ienc := hpack.NewEncoder(&ibuf)
		if err := ienc.WriteField(hpack.HeaderField{Name: ":status", Value: "103"}); err != nil {
			tb.Logf("encode: %v", err)
			return
		}
		if err := framer.WriteHeaders(http2.HeadersFrameParam{
			StreamID: ids[0], BlockFragment: ibuf.Bytes(), EndHeaders: true,
		}); err != nil {
			tb.Logf("103: %v", err)
			return
		}
		if err := bw.Flush(); err != nil {
			tb.Logf("flush: %v", err)
			return
		}
		if err := writeOKResponse(framer, bw, ids[1]); err != nil {
			tb.Logf("stream 1: %v", err)
			return
		}
		if err := writeOKResponse(framer, bw, ids[0]); err != nil {
			tb.Logf("stream 0: %v", err)
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
	batch, err := c.SendBatch(ctx, []*http.Request{
		mustReq(t, "https://"+addr+"/hinted"),
		mustReq(t, "https://"+addr+"/plain"),
	})
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if order, ok := batch.FullArrivalOrder(); !ok ||
		len(order) != 2 || order[0] != 1 || order[1] != 0 {
		t.Fatalf("FullArrivalOrder = %v, %v; want [1 0], true", order, ok)
	}

	for i, r := range batch.Results {
		if r.Response.StatusCode != http.StatusOK {
			t.Errorf("result %d: status = %d, want the final 200", i, r.Response.StatusCode)
		}
	}
	if got := batch.ArrivalOrder; len(got) != 2 || got[0] != 1 || got[1] != 0 {
		t.Errorf("ArrivalOrder = %v, want [1 0]: an informational head is not an answer", got)
	}
	// And the ranks stay dense despite the extra head consuming a read slot.
	if a, b := batch.Results[0].ArrivalRank, batch.Results[1].ArrivalRank; a != 1 || b != 0 {
		t.Errorf("ranks = (%d, %d), want (1, 0)", a, b)
	}
}

// TestResetAfterInformationalHeadersHasNotArrived verifies that a provisional
// response does not turn a later pre-final-header reset into an arrival.
func TestResetAfterInformationalHeadersHasNotArrived(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := startRawH2Server(t,
		func(tb testing.TB, framer *http2.Framer, bw *bufio.Writer, _ net.Conn) {
			tb.Helper()
			if err := doServerPreface(tb, framer, bw); err != nil {
				tb.Logf("preface: %v", err)
				return
			}
			var ids []uint32
			for len(ids) < 2 {
				frame, err := framer.ReadFrame()
				if err != nil {
					tb.Logf("read: %v", err)
					return
				}
				if headers, ok := frame.(*http2.MetaHeadersFrame); ok {
					ids = append(ids, headers.StreamID)
				}
			}

			var encoded bytes.Buffer
			encoder := hpack.NewEncoder(&encoded)
			if err := encoder.WriteField(hpack.HeaderField{
				Name: ":status", Value: "103",
			}); err != nil {
				tb.Logf("encode 103: %v", err)
				return
			}
			if err := framer.WriteHeaders(http2.HeadersFrameParam{
				StreamID: ids[0], BlockFragment: encoded.Bytes(), EndHeaders: true,
			}); err != nil {
				tb.Logf("write 103: %v", err)
				return
			}
			if err := bw.Flush(); err != nil {
				tb.Logf("flush 103: %v", err)
				return
			}
			if err := writeOKResponse(framer, bw, ids[1]); err != nil {
				tb.Logf("write final response: %v", err)
				return
			}
			if err := framer.WriteRSTStream(
				ids[0], http2.ErrCodeRefusedStream,
			); err != nil {
				tb.Logf("write reset: %v", err)
				return
			}
			if err := bw.Flush(); err != nil {
				tb.Logf("flush reset: %v", err)
				return
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
	batch, err := client.SendBatch(ctx, []*http.Request{
		mustReq(t, "https://"+addr+"/hinted-reset"),
		mustReq(t, "https://"+addr+"/plain"),
	})
	if _, ok := errors.AsType[*tth2.StreamError](err); !ok {
		t.Fatalf("SendBatch: got %v, want a *tth2.StreamError", err)
	}
	if batch == nil {
		t.Fatal("SendBatch returned no partial batch")
	}
	reset := batch.Results[0]
	if reset.Arrived() || reset.Response != nil || reset.Reset == nil ||
		reset.ArrivalRank != -1 {
		t.Errorf("hinted reset result = %+v, want a pre-final-header reset", reset)
	}
	if batch.AllResponsesArrived() {
		t.Error("AllResponsesArrived = true after a reset before final headers")
	}
	if order, ok := batch.FullArrivalOrder(); ok || order != nil {
		t.Errorf("FullArrivalOrder = %v, %v; want nil, false", order, ok)
	}
	if got := batch.ArrivalOrder; len(got) != 1 || got[0] != 1 {
		t.Errorf("ArrivalOrder = %v, want [1]", got)
	}
}
