package tth2_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/tantosec/tturl/internal/h2test"
	"github.com/tantosec/tturl/tth2"
)

// TestSendLargeResponseBodyIntegrity exercises collector backpressure. The
// unbuffered handoff makes the dispatcher wait for each 16 KB DATA frame to be
// accepted, so an 8 MB response repeatedly rides the collector's drain. A
// SHA-256 confirms every byte survives the round-trip.
func TestSendLargeResponseBodyIntegrity(t *testing.T) {
	t.Parallel()
	const bodySize = 8 << 20 // 8 MB
	payload := make([]byte, bodySize)
	for i := range payload {
		payload[i] = byte(i * 31) //nolint:gosec // test payload byte; wrap is intentional
	}
	wantHash := sha256.Sum256(payload)

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "")
		_, _ = w.Write(payload)
	})
	addr, tlsCfg := h2test.Serve(t, handler)

	client := newClient(t, tlsCfg)

	// Two concurrent streams: the dispatcher juggles backpressure across both
	// collectors, so a deadlock-on-blocked-send bug would freeze the trial rather
	// than just one stream.
	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/"),
		mustReq(t, "https://"+addr+"/"),
	}
	batch, err := client.SendBatch(t.Context(), reqs,
		tth2.WithResponseBodyCaptureBytes(tth2.UnlimitedResponseBytes))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	results := batch.Results
	if len(results) != len(reqs) {
		t.Fatalf("len(results) = %d, want %d", len(results), len(reqs))
	}
	for i, r := range results {
		if r.Response.StatusCode != http.StatusOK {
			t.Errorf("result %d: status = %d", i, r.Response.StatusCode)
			continue
		}
		got := r.Response.Body.Captured
		if len(got) != bodySize {
			t.Errorf("result %d: body size %d, want %d", i, len(got), bodySize)
			continue
		}
		if h := sha256.Sum256(got); h != wantHash {
			t.Errorf("result %d: body sha256 = %s, want %s",
				i, hex.EncodeToString(h[:]), hex.EncodeToString(wantHash[:]))
		}
	}
}

// TestSendMixedEmptyAndNonEmptyBodies checks that an empty body ends with its
// HEADERS while non-empty neighbours continue through both body phases.
func TestSendMixedEmptyAndNonEmptyBodies(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(b)
	})
	addr, tlsCfg := h2test.Serve(t, handler)

	client := newClient(t, tlsCfg)

	bodies := []string{"hello", "", "world", "", "!"}
	reqs := make([]*http.Request, len(bodies))
	for i, b := range bodies {
		req, err := http.NewRequestWithContext(t.Context(), "POST", "https://"+addr+"/", strings.NewReader(b))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		reqs[i] = req
	}

	batch, err := client.SendBatch(t.Context(), reqs,
		tth2.WithResponseBodyCaptureBytes(tth2.UnlimitedResponseBytes))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	results := batch.Results
	if len(results) != len(bodies) {
		t.Fatalf("len(results) = %d, want %d", len(results), len(bodies))
	}
	for i, r := range results {
		if r.Response.StatusCode != http.StatusOK {
			t.Errorf("body[%d]=%q: status = %d, want 200", i, bodies[i], r.Response.StatusCode)
			continue
		}
		got := r.Response.Body.Captured
		if string(got) != bodies[i] {
			t.Errorf("body[%d]: echoed body = %q, want %q", i, got, bodies[i])
		}
	}
}

// TestSendBodyUnwithheld checks WithBodyBytesWithheld(0). A body larger than
// the initial flow-control window forces multiple DATA frames and a credit
// drain.
func TestSendBodyUnwithheld(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, sha256Handler)

	client := newClient(t, tlsCfg)

	body := make([]byte, 200*1024)
	for i := range body {
		body[i] = byte(i*101 ^ 0x5A) //nolint:gosec // test payload byte; wrap is intentional
	}
	wantHash := sha256.Sum256(body)

	req, err := http.NewRequestWithContext(t.Context(), "POST", "https://"+addr+"/", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	batch, err := client.SendBatch(t.Context(), []*http.Request{req},
		tth2.WithBodyBytesWithheld(0), tth2.WithResponseHeaderCapture())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	results := batch.Results
	if len(results) != 1 || results[0].Response.StatusCode != http.StatusOK {
		t.Fatalf("unexpected results: %+v", results)
	}
	if got := results[0].Response.Header.Get(bodyHashHeader); got != hex.EncodeToString(wantHash[:]) {
		t.Errorf("body hash mismatch: got %s, want %x", got, wantHash)
	}
}

// TestGreedyCompletionWithSmallStreamWindows exercises a peer that grants
// more credit only after receiving intermediate DATA. A fitting byte budget alone
// must not make the sender wait for an impossible all-body reservation.
func TestGreedyCompletionWithSmallStreamWindows(t *testing.T) {
	t.Parallel()
	for _, withheld := range []int{0, 1, 3} {
		t.Run(fmt.Sprintf("withheld-%d", withheld), func(t *testing.T) {
			t.Parallel()
			testGreedyCompletionWithSmallStreamWindows(t, withheld)
		})
	}
}

func testGreedyCompletionWithSmallStreamWindows(t *testing.T, withheld int) {
	t.Helper()
	addr, tlsCfg := startRawH2Server(t, func(
		tb testing.TB, framer *http2.Framer, bw *bufio.Writer, _ net.Conn,
	) {
		if err := doServerPreface(tb, framer, bw, http2.Setting{
			ID: http2.SettingInitialWindowSize, Val: 1024,
		}); err != nil {
			tb.Logf("doServerPreface: %v", err)
			return
		}
		bodies := make(map[uint32][]byte)
		ended := 0
		for ended < 2 {
			frame, err := framer.ReadFrame()
			if err != nil {
				tb.Logf("read frame: %v", err)
				return
			}
			data, ok := frame.(*http2.DataFrame)
			if !ok {
				continue
			}
			bodies[data.StreamID] = append(bodies[data.StreamID], data.Data()...)
			if data.StreamEnded() {
				ended++
				if len(data.Data()) == 0 {
					tb.Errorf("stream %d ended with empty DATA", data.StreamID)
				}
				if err := writeOKResponse(framer, bw, data.StreamID); err != nil {
					tb.Logf("write response: %v", err)
					return
				}
				continue
			}
			credit := uint32(len(data.Data())) //nolint:gosec // DATA is bounded by peer window
			if err := framer.WriteWindowUpdate(data.StreamID, credit); err != nil {
				tb.Logf("write stream credit: %v", err)
				return
			}
			if err := bw.Flush(); err != nil {
				tb.Logf("flush stream credit: %v", err)
				return
			}
		}
		if len(bodies) != 2 || len(bodies[1]) != 4096 || len(bodies[3]) != 4096 {
			tb.Errorf("received body lengths: stream 1=%d, stream 3=%d", len(bodies[1]), len(bodies[3]))
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reqs := make([]*http.Request, 2)
	for i := range reqs {
		var err error
		reqs[i], err = http.NewRequestWithContext(ctx, http.MethodPost,
			"https://"+addr+"/", bytes.NewReader(bytes.Repeat([]byte{byte('a' + i)}, 4096)))
		if err != nil {
			t.Fatal(err)
		}
	}
	batch, err := (&tth2.Client{Transport: &tth2.Transport{
		TLSClientConfig: tlsCfg,
	}}).SendBatch(ctx, reqs, tth2.WithBodyBytesWithheld(withheld))
	if err != nil {
		t.Fatal(err)
	}
	for i, result := range batch.Results {
		if !result.Arrived() || result.Response.StatusCode != http.StatusOK {
			t.Errorf("result %d = %+v", i, result)
		}
	}
}

// TestGreedyResponseSurvivesSendCreditDrain covers a server that completes its
// response after request headers, before the client finishes a large request
// body. The response arrives while the send path is reading WINDOW_UPDATE and
// must remain available to the ordinary response result.
func TestGreedyResponseSurvivesSendCreditDrain(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := startRawH2Server(t, func(
		tb testing.TB, framer *http2.Framer, bw *bufio.Writer, _ net.Conn,
	) {
		if err := doServerPreface(tb, framer, bw, http2.Setting{
			ID: http2.SettingInitialWindowSize, Val: 1024,
		}); err != nil {
			tb.Logf("doServerPreface: %v", err)
			return
		}
		var streamID uint32
		for {
			frame, err := framer.ReadFrame()
			if err != nil {
				return
			}
			switch frame := frame.(type) {
			case *http2.MetaHeadersFrame:
				streamID = frame.StreamID
				if err := writeOKResponse(framer, bw, streamID); err != nil {
					tb.Logf("write early response: %v", err)
					return
				}
				if err := framer.WriteWindowUpdate(0, 1<<20); err != nil {
					tb.Logf("write connection credit: %v", err)
					return
				}
				if err := framer.WriteWindowUpdate(streamID, 1<<20); err != nil {
					tb.Logf("write stream credit: %v", err)
					return
				}
				if err := bw.Flush(); err != nil {
					tb.Logf("flush early response and credit: %v", err)
					return
				}
			case *http2.DataFrame:
				if frame.StreamEnded() {
					return
				}
			}
		}
	})

	body := bytes.Repeat([]byte{'x'}, 200*1024)
	request, err := http.NewRequestWithContext(
		t.Context(), http.MethodPost, "https://"+addr+"/", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	batch, err := (&tth2.Client{Transport: &tth2.Transport{
		TLSClientConfig: tlsCfg,
	}}).SendBatch(ctx, []*http.Request{request})
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if len(batch.Results) != 1 || batch.Results[0].Response == nil ||
		batch.Results[0].Response.StatusCode != http.StatusOK {
		t.Fatalf("result = %+v, want one early 200 response", batch.Results)
	}
}

func TestWithBodyBytesWithheldRejectsNegative(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("WithBodyBytesWithheld(-1): no panic")
		}
	}()
	_ = tth2.WithBodyBytesWithheld(-1)
}

func TestBodyBytesWithheldMayExceedBody(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, sha256Handler)
	client := newClient(t, tlsCfg)
	body := []byte("abc")
	req, err := http.NewRequestWithContext(
		t.Context(), "POST", "https://"+addr+"/", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	batch, err := client.SendBatch(t.Context(), []*http.Request{req},
		tth2.WithBodyBytesWithheld(len(body)+10),
		tth2.WithResponseHeaderCapture())
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	want := sha256.Sum256(body)
	if got := batch.Results[0].Response.Header.Get(bodyHashHeader); got != hex.EncodeToString(want[:]) {
		t.Errorf("body hash = %s, want %x", got, want)
	}
}
