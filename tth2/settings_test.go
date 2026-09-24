package tth2_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/tantosec/tturl/tth2"
)

// TestPrefaceRejectsInvalidInitialWindowSize checks the RFC 9113 section 6.5.2
// limit. Accepting a larger value would overflow the signed send-window
// arithmetic.
func TestPrefaceRejectsInvalidInitialWindowSize(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := startRawH2Server(t, func(tb testing.TB, framer *http2.Framer, bw *bufio.Writer, _ net.Conn) {
		// 1<<31 = 2^31, exactly one above the legal cap.
		if err := framer.WriteSettings(http2.Setting{
			ID: http2.SettingInitialWindowSize, Val: 1 << 31,
		}); err != nil {
			tb.Logf("server WriteSettings: %v", err)
			return
		}
		_ = bw.Flush()
		// Stay alive until the client hangs up.
		for {
			if _, err := framer.ReadFrame(); err != nil {
				return
			}
		}
	})

	c := newClient(t, tlsCfg)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := c.SendBatch(ctx, []*http.Request{mustReq(t, "https://"+addr+"/")})
	if err == nil {
		t.Fatal("SendBatch: expected error from invalid INITIAL_WINDOW_SIZE")
	}
	// Accept either error wording: tth2's applySettings mentions
	// cspell:ignore emption
	// INITIAL_WINDOW_SIZE; the framer pre-emption mentions FLOW_CONTROL_ERROR.
	if !strings.Contains(err.Error(), "INITIAL_WINDOW_SIZE") &&
		!strings.Contains(err.Error(), "FLOW_CONTROL_ERROR") {
		t.Errorf("error %q does not mention INITIAL_WINDOW_SIZE or FLOW_CONTROL_ERROR", err)
	}
}

// TestPrefaceRejectsInvalidMaxFrameSize checks the RFC 9113 section 6.5.2
// ceiling. Accepting a larger value would exceed the signed domain used by
// flow-control arithmetic.
func TestPrefaceRejectsInvalidMaxFrameSize(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := startRawH2Server(t, func(tb testing.TB, framer *http2.Framer, bw *bufio.Writer, _ net.Conn) {
		// 1<<24 = 2^24, exactly one above the legal ceiling of 2^24-1.
		if err := framer.WriteSettings(http2.Setting{
			ID: http2.SettingMaxFrameSize, Val: 1 << 24,
		}); err != nil {
			tb.Logf("server WriteSettings: %v", err)
			return
		}
		_ = bw.Flush()
		// Stay alive until the client hangs up.
		for {
			if _, err := framer.ReadFrame(); err != nil {
				return
			}
		}
	})

	c := newClient(t, tlsCfg)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := c.SendBatch(ctx, []*http.Request{mustReq(t, "https://"+addr+"/")})
	if err == nil {
		t.Fatal("SendBatch: expected error from invalid MAX_FRAME_SIZE")
	}
	if !strings.Contains(err.Error(), "MAX_FRAME_SIZE") {
		t.Errorf("error %q does not mention MAX_FRAME_SIZE", err)
	}
}

func TestPeerConcurrentStreamLimit(t *testing.T) {
	t.Run("honoured by default", func(t *testing.T) {
		t.Parallel()
		addr, tlsCfg := startRawH2Server(t, func(
			tb testing.TB, framer *http2.Framer, bw *bufio.Writer, _ net.Conn,
		) {
			if err := doServerPreface(tb, framer, bw, http2.Setting{
				ID: http2.SettingMaxConcurrentStreams, Val: 1,
			}); err != nil {
				tb.Logf("doServerPreface: %v", err)
				return
			}
			for {
				if _, err := framer.ReadFrame(); err != nil {
					return
				}
			}
		})
		client := newClient(t, tlsCfg)
		requests := []*http.Request{
			mustReq(t, "https://"+addr+"/a"),
			mustReq(t, "https://"+addr+"/b"),
		}
		_, err := client.SendBatch(t.Context(), requests)
		if err == nil || !strings.Contains(err.Error(),
			"peer concurrent-stream limit is 1; batch requires 2 streams") {
			t.Fatalf("SendBatch error = %v, want peer stream-limit refusal", err)
		}
	})

	t.Run("explicitly ignored", func(t *testing.T) {
		t.Parallel()
		addr, tlsCfg := startRawH2Server(t, func(
			tb testing.TB, framer *http2.Framer, bw *bufio.Writer, _ net.Conn,
		) {
			if err := doServerPreface(tb, framer, bw, http2.Setting{
				ID: http2.SettingMaxConcurrentStreams, Val: 1,
			}); err != nil {
				tb.Logf("doServerPreface: %v", err)
				return
			}
			streams := make([]uint32, 0, 2)
			for len(streams) < 2 {
				frame, err := framer.ReadFrame()
				if err != nil {
					return
				}
				if headers, ok := frame.(*http2.MetaHeadersFrame); ok {
					streams = append(streams, headers.StreamID)
				}
			}
			for _, streamID := range streams {
				if err := writeOKResponse(framer, bw, streamID); err != nil {
					tb.Logf("writeOKResponse: %v", err)
					return
				}
			}
		})
		client := newClient(t, tlsCfg)
		requests := []*http.Request{
			mustReq(t, "https://"+addr+"/a"),
			mustReq(t, "https://"+addr+"/b"),
		}
		batch, err := client.SendBatch(
			t.Context(), requests, tth2.WithPeerStreamLimitIgnored())
		if err != nil {
			t.Fatalf("SendBatch: %v", err)
		}
		if len(batch.Results) != 2 {
			t.Fatalf("results = %d, want 2", len(batch.Results))
		}
	})
}

func TestIdleConnectionProcessesControlFramesBeforeReuse(t *testing.T) {
	t.Parallel()
	releaseControl := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseControl) })
	t.Cleanup(release)
	controlHandled := make(chan struct{})
	markHandled := sync.OnceFunc(func() { close(controlHandled) })
	addr, tlsCfg := startRawH2Server(t, func(
		tb testing.TB, framer *http2.Framer, bw *bufio.Writer, _ net.Conn,
	) {
		if err := doServerPreface(tb, framer, bw, http2.Setting{
			ID: http2.SettingMaxConcurrentStreams, Val: 1,
		}); err != nil {
			tb.Logf("doServerPreface: %v", err)
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
				tb.Logf("first response: %v", err)
				return
			}
			break
		}

		<-releaseControl
		if err := framer.WriteSettings(http2.Setting{
			ID: http2.SettingMaxConcurrentStreams, Val: 2,
		}); err != nil {
			tb.Logf("first idle SETTINGS: %v", err)
			return
		}
		if err := framer.WriteSettings(http2.Setting{
			ID: http2.SettingMaxConcurrentStreams, Val: 3,
		}); err != nil {
			tb.Logf("second idle SETTINGS: %v", err)
			return
		}
		if err := bw.Flush(); err != nil {
			tb.Logf("flush idle SETTINGS: %v", err)
			return
		}
		acks := 0
		for acks < 2 {
			frame, err := framer.ReadFrame()
			if err != nil {
				return
			}
			if settings, ok := frame.(*http2.SettingsFrame); ok &&
				settings.IsAck() {
				acks++
			}
		}
		pingData := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
		if err := framer.WritePing(false, pingData); err != nil {
			tb.Logf("idle PING: %v", err)
			return
		}
		if err := bw.Flush(); err != nil {
			tb.Logf("flush idle PING: %v", err)
			return
		}
		for {
			frame, err := framer.ReadFrame()
			if err != nil {
				return
			}
			if ping, ok := frame.(*http2.PingFrame); ok &&
				ping.IsAck() && ping.Data == pingData {
				break
			}
		}
		markHandled()

		streams := make([]uint32, 0, 3)
		for len(streams) < 3 {
			frame, err := framer.ReadFrame()
			if err != nil {
				return
			}
			if headers, ok := frame.(*http2.MetaHeadersFrame); ok {
				streams = append(streams, headers.StreamID)
			}
		}
		for _, streamID := range streams {
			if err := writeOKResponse(framer, bw, streamID); err != nil {
				tb.Logf("second response: %v", err)
				return
			}
		}
	})

	client := newClient(t, tlsCfg)
	if _, err := client.SendBatch(t.Context(), []*http.Request{
		mustReq(t, "https://"+addr+"/first"),
	}); err != nil {
		t.Fatalf("first SendBatch: %v", err)
	}
	release()
	select {
	case <-controlHandled:
	case <-time.After(3 * time.Second):
		t.Fatal("idle connection did not acknowledge SETTINGS and PING")
	}

	requests := make([]*http.Request, 3)
	for i := range requests {
		requests[i] = mustReq(t, fmt.Sprintf("https://%s/second/%d", addr, i))
	}
	batch, err := client.SendBatch(t.Context(), requests)
	if err != nil {
		t.Fatalf("second SendBatch: %v", err)
	}
	if len(batch.Results) != len(requests) {
		t.Fatalf("second results = %d, want %d", len(batch.Results), len(requests))
	}
}

func TestPushPromiseAfterPushDisabledFailsConnection(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := startRawH2Server(t, func(
		tb testing.TB, framer *http2.Framer, bw *bufio.Writer, _ net.Conn,
	) {
		if err := doServerPreface(tb, framer, bw); err != nil {
			tb.Logf("doServerPreface: %v", err)
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
			if err := framer.WritePushPromise(http2.PushPromiseParam{
				StreamID: headers.StreamID, PromiseID: 2, EndHeaders: true,
			}); err != nil {
				tb.Logf("WritePushPromise: %v", err)
				return
			}
			if err := bw.Flush(); err != nil {
				tb.Logf("flush PUSH_PROMISE: %v", err)
			}
			return
		}
	})

	client := newClient(t, tlsCfg)
	_, err := client.SendBatch(t.Context(), []*http.Request{
		mustReq(t, "https://"+addr+"/"),
	})
	if err == nil || !strings.Contains(err.Error(), "PUSH_PROMISE") {
		t.Fatalf("SendBatch error = %v, want PUSH_PROMISE protocol failure", err)
	}
}

// TestSettingsInitialWindowShrinkMidSend verifies that a negative SETTINGS
// delta reaches active stream windows. The server shrinks its initial window
// after the first body record, then supplies fresh credit.
func TestSettingsInitialWindowShrinkMidSend(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := startRawH2Server(t, func(tb testing.TB, framer *http2.Framer, bw *bufio.Writer, _ net.Conn) {
		// Generous initial window: 256 KB.
		if err := doServerPreface(tb, framer, bw,
			http2.Setting{ID: http2.SettingInitialWindowSize, Val: 256 * 1024},
		); err != nil {
			tb.Logf("doServerPreface: %v", err)
			return
		}

		var streamID uint32
		bytesRead := 0
		shrunk := false
		for {
			f, err := framer.ReadFrame()
			if err != nil {
				return
			}
			switch fr := f.(type) {
			case *http2.MetaHeadersFrame:
				streamID = fr.StreamID
			case *http2.DataFrame:
				bytesRead += len(fr.Data())
				if !shrunk && bytesRead >= 32*1024 {
					shrunk = true
					// Shrink INITIAL_WINDOW_SIZE to 8 KB, then top up connection and
					// per-stream credit so the client can finish despite the smaller
					// per-stream allowance.
					if err := framer.WriteSettings(http2.Setting{
						ID: http2.SettingInitialWindowSize, Val: 8 * 1024,
					}); err != nil {
						tb.Logf("WriteSettings shrink: %v", err)
						return
					}
					if err := framer.WriteWindowUpdate(0, 4*1024*1024); err != nil {
						tb.Logf("WriteWindowUpdate(0): %v", err)
						return
					}
					if streamID != 0 {
						if err := framer.WriteWindowUpdate(streamID, 4*1024*1024); err != nil {
							tb.Logf("WriteWindowUpdate(stream): %v", err)
							return
						}
					}
					if err := bw.Flush(); err != nil {
						tb.Logf("flush: %v", err)
						return
					}
				}
				if fr.StreamEnded() {
					if err := writeOKResponse(framer, bw, fr.StreamID); err != nil {
						tb.Logf("writeOKResponse: %v", err)
					}
					return
				}
			}
		}
	})

	client := newClient(t, tlsCfg)

	body := make([]byte, 128*1024)
	for i := range body {
		body[i] = byte(i)
	}
	req, err := http.NewRequestWithContext(t.Context(), "POST", "https://"+addr+"/", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	batch, err := client.SendBatch(ctx, []*http.Request{req})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	results := batch.Results
	if len(results) != 1 || results[0].Response.StatusCode != http.StatusOK {
		t.Errorf("unexpected results: %+v", results)
	}
}
