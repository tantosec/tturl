package tth2_test

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/tantosec/tturl/internal/h2test"
	"github.com/tantosec/tturl/tth2"
)

func TestResponseCaptureContract(t *testing.T) {
	t.Parallel()
	body := []byte("response body")
	digest := sha256.Sum256(body)
	addr, tlsCfg := h2test.Serve(t, http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Evidence", "head")
			w.Header().Set("Trailer", "X-Checksum")
			_, _ = w.Write(body)
			w.Header().Set("X-Checksum", "tail")
		}))

	tests := []struct {
		name        string
		opts        []tth2.SendOption
		wantBody    []byte
		wantHeaders bool
	}{
		{name: "metadata"},
		{
			name: "bounded body",
			opts: []tth2.SendOption{
				tth2.WithResponseBodyCaptureBytes(4),
			},
			wantBody: []byte("resp"),
		},
		{
			name: "headers only",
			opts: []tth2.SendOption{
				tth2.WithResponseHeaderCapture(),
			},
			wantHeaders: true,
		},
		{
			name: "unlimited body and headers",
			opts: []tth2.SendOption{
				tth2.WithResponseBodyCaptureBytes(
					tth2.UnlimitedResponseBytes),
				tth2.WithResponseHeaderCapture(),
			},
			wantBody:    body,
			wantHeaders: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, tlsCfg)
			batch, err := c.SendBatch(t.Context(), []*http.Request{
				mustReq(t, "https://"+addr+"/"),
			}, tc.opts...)
			if err != nil {
				t.Fatalf("SendBatch: %v", err)
			}
			resp := batch.Results[0].Response
			if resp.Body.BytesReceived != int64(len(body)) {
				t.Errorf("BytesReceived = %d, want %d",
					resp.Body.BytesReceived, len(body))
			}
			if resp.Body.SHA256 != digest {
				t.Errorf("SHA256 = %x, want %x", resp.Body.SHA256, digest)
			}
			if !slices.Equal(resp.Body.Captured, tc.wantBody) {
				t.Errorf("Captured = %q, want %q",
					resp.Body.Captured, tc.wantBody)
			}
			if got := resp.Body.CapturedComplete(); got !=
				(len(tc.wantBody) == len(body)) {
				t.Errorf("CapturedComplete = %t", got)
			}
			if tc.wantHeaders {
				if resp.Header.Get("X-Evidence") != "head" ||
					resp.Trailer.Get("X-Checksum") != "tail" {
					t.Errorf("Header/Trailer = %v/%v", resp.Header, resp.Trailer)
				}
			} else if resp.Header != nil || resp.Trailer != nil {
				t.Errorf("Header/Trailer = %v/%v, want nil/nil",
					resp.Header, resp.Trailer)
			}
		})
	}
}

func TestConnLeaseResponseCapture(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Evidence", "header")
			_, _ = w.Write([]byte("body evidence"))
		}))
	client := newClient(t, tlsCfg)
	lease, err := client.Acquire(t.Context(), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLease(t, lease)

	batch, err := lease.SendBatch(t.Context(), []*http.Request{
		mustReq(t, "https://"+addr+"/"),
	}, tth2.WithResponseHeaderCapture(),
		tth2.WithResponseBodyCaptureBytes(4))
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	response := batch.Results[0].Response
	if got := response.Header.Get("X-Evidence"); got != "header" {
		t.Errorf("X-Evidence = %q, want header", got)
	}
	if got := string(response.Body.Captured); got != "body" {
		t.Errorf("Captured = %q, want body", got)
	}
}

func TestResponseHashCoversReceivedResetPrefix(t *testing.T) {
	t.Parallel()
	const prefix = "received before reset"
	addr, tlsCfg := startRawH2Server(t, resetAfterBody(prefix))
	c := newClient(t, tlsCfg)
	batch, err := c.SendBatch(t.Context(), []*http.Request{
		mustReq(t, "https://"+addr+"/"),
	}, tth2.WithResponseBodyCaptureBytes(4))
	if _, ok := errors.AsType[*tth2.StreamError](err); !ok {
		t.Fatalf("SendBatch error = %v, want *tth2.StreamError", err)
	}
	if batch == nil {
		t.Fatalf("SendBatch returned no batch after reset: %v", err)
	}
	if len(batch.Results) != 1 {
		t.Fatalf("len(Results) = %d, want 1", len(batch.Results))
	}
	result := batch.Results[0]
	if result.Reset == nil {
		t.Fatal("Result.Reset is nil, want the server's reset")
	}
	resp := result.Response
	if resp == nil {
		t.Fatalf("Response is nil after HEADERS and DATA before reset: result=%+v; error=%v",
			result, err)
	}
	if resp.Body.BytesReceived != int64(len(prefix)) {
		t.Fatalf("BytesReceived = %d, want %d",
			resp.Body.BytesReceived, len(prefix))
	}
	if got, want := resp.Body.SHA256, sha256.Sum256([]byte(prefix)); got != want {
		t.Errorf("SHA256 = %x, want %x", got, want)
	}
	if string(resp.Body.Captured) != prefix[:4] {
		t.Errorf("Captured = %q, want %q", resp.Body.Captured, prefix[:4])
	}
}

func resetAfterBody(
	body string,
) func(testing.TB, *http2.Framer, *bufio.Writer, net.Conn) {
	return func(
		tb testing.TB,
		framer *http2.Framer,
		bw *bufio.Writer,
		_ net.Conn,
	) {
		tb.Helper()
		if err := doServerPreface(tb, framer, bw); err != nil {
			tb.Logf("preface: %v", err)
			return
		}
		var streamID uint32
		for streamID == 0 {
			frame, err := framer.ReadFrame()
			if err != nil {
				tb.Logf("read request: %v", err)
				return
			}
			if headers, ok := frame.(*http2.MetaHeadersFrame); ok {
				streamID = headers.StreamID
			}
		}
		var block bytes.Buffer
		if err := hpack.NewEncoder(&block).WriteField(
			hpack.HeaderField{Name: ":status", Value: "200"}); err != nil {
			tb.Logf("encode response: %v", err)
			return
		}
		if err := framer.WriteHeaders(http2.HeadersFrameParam{
			StreamID: streamID, BlockFragment: block.Bytes(), EndHeaders: true,
		}); err != nil {
			tb.Logf("write response: %v", err)
			return
		}
		if err := framer.WriteData(streamID, false, []byte(body)); err != nil {
			tb.Logf("write body: %v", err)
			return
		}
		if err := framer.WriteRSTStream(
			streamID, http2.ErrCodeInternal); err != nil {
			tb.Logf("write reset: %v", err)
			return
		}
		if err := bw.Flush(); err != nil {
			tb.Logf("flush reset response: %v", err)
			return
		}
		// Keep the connection viable so the stream reset remains the batch's only
		// failure. Returning here would close the connection and let EOF race the
		// reset result, correctly replacing its partial batch as a fatal error.
		for {
			if _, err := framer.ReadFrame(); err != nil {
				return
			}
		}
	}
}

func TestWithResponseBodyCaptureBytesRejectsInvalidLimit(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("WithResponseBodyCaptureBytes(-2) did not panic")
		}
	}()
	_ = tth2.WithResponseBodyCaptureBytes(-2)
}

func TestMaxResponseHeaderBytesRejectsOversizedSection(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Oversized", strings.Repeat("x", 1024))
			w.WriteHeader(http.StatusOK)
		}))
	tr := &tth2.Transport{
		TLSClientConfig:        tlsCfg,
		MaxResponseHeaderBytes: 64,
	}
	t.Cleanup(tr.CloseIdleConnections)
	c := &tth2.Client{Transport: tr}
	_, err := c.SendBatch(t.Context(), []*http.Request{
		mustReq(t, "https://"+addr+"/"),
	})
	if err == nil {
		t.Fatal("SendBatch accepted an oversized response header section")
	}
}
