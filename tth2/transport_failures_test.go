package tth2_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/tantosec/tturl/tth2"
)

// TestRSTStreamMidBodyTransfer asserts that a server RST_STREAM arriving while
// the client is transferring a body (specifically, while sendBytesUpTo has
// parked in drainForSendCredit waiting for flow-control credit) surfaces as a
// *tth2.StreamError with the correct stream ID and code. A 200 KB body forces
// cycles because the server never emits WINDOW_UPDATE, so the client exhausts
// the default 65535-byte connection window quickly.
func TestRSTStreamMidBodyTransfer(t *testing.T) {
	t.Parallel()
	var serverStreamID uint32
	rstSent := make(chan struct{})
	addr, tlsCfg := startRawH2Server(t, func(tb testing.TB, framer *http2.Framer, bw *bufio.Writer, _ net.Conn) {
		if err := doServerPreface(tb, framer, bw); err != nil {
			tb.Logf("doServerPreface: %v", err)
			return
		}
		for {
			f, err := framer.ReadFrame()
			if err != nil {
				return
			}
			switch fr := f.(type) {
			case *http2.MetaHeadersFrame:
				serverStreamID = fr.StreamID
			case *http2.DataFrame:
				if err := framer.WriteRSTStream(fr.StreamID, http2.ErrCodeRefusedStream); err != nil {
					tb.Logf("WriteRSTStream: %v", err)
					return
				}
				if err := bw.Flush(); err != nil {
					tb.Logf("flush RST: %v", err)
					return
				}
				close(rstSent)
				// Stay alive briefly so the client has time to read the RST before our end
				// of the conn dies.
				for {
					if _, err := framer.ReadFrame(); err != nil {
						return
					}
				}
			}
		}
	})

	client := newClient(t, tlsCfg)

	body := make([]byte, 200*1024)
	req, err := http.NewRequestWithContext(t.Context(), "POST", "https://"+addr+"/", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err = client.SendBatch(ctx, []*http.Request{req})

	select {
	case <-rstSent:
	case <-time.After(5 * time.Second):
		t.Fatal("server never sent RST_STREAM; test setup is broken")
	}

	if err == nil {
		t.Fatal("Send: expected RST_STREAM error, got nil")
	}
	var se *tth2.StreamError
	if !errors.As(err, &se) {
		t.Fatalf("Send: error %v (%T) is not a *tth2.StreamError", err, err)
	}
	if serverStreamID == 0 {
		t.Fatal("server sent RST_STREAM without recording a stream ID")
	}
	if se.Code != http2.ErrCodeRefusedStream {
		t.Errorf("StreamError.Code = %v, want %v", se.Code, http2.ErrCodeRefusedStream)
	}
	if se.StreamID != serverStreamID {
		t.Errorf("StreamError.StreamID = %d, want %d", se.StreamID, serverStreamID)
	}
}

// TestGoAwayDuringTrial asserts that a server GOAWAY mid-trial fails the
// in-flight Send with an error that mentions GOAWAY.
func TestGoAwayDuringTrial(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := startRawH2Server(t, func(tb testing.TB, framer *http2.Framer, bw *bufio.Writer, _ net.Conn) {
		if err := doServerPreface(tb, framer, bw); err != nil {
			tb.Logf("doServerPreface: %v", err)
			return
		}
		for {
			f, err := framer.ReadFrame()
			if err != nil {
				return
			}
			if _, ok := f.(*http2.MetaHeadersFrame); ok {
				_ = framer.WriteGoAway(0, http2.ErrCodeEnhanceYourCalm, nil)
				_ = bw.Flush()
				return
			}
		}
	})

	client := newClient(t, tlsCfg)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := client.SendBatch(ctx, []*http.Request{mustReq(t, "https://"+addr+"/")})
	if err == nil {
		t.Fatal("Send: expected GOAWAY error, got nil")
	}
	if !strings.Contains(err.Error(), "GOAWAY") {
		t.Errorf("error %v does not mention GOAWAY", err)
	}
}

// TestServerHangUpUnwrapsCloseSignals asserts that when the peer closes the
// connection mid-trial, the error returned from Send is one of the standard
// "conn gone" sentinels (io.EOF / io.ErrUnexpectedEOF / net.ErrClosed)
// reachable via errors.Is; closeAll preserves the sentinels when it reports a
// read failure.
func TestServerHangUpUnwrapsCloseSignals(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := startRawH2Server(t, func(tb testing.TB, framer *http2.Framer, bw *bufio.Writer, conn net.Conn) {
		if err := doServerPreface(tb, framer, bw); err != nil {
			tb.Logf("doServerPreface: %v", err)
			return
		}
		// Wait for the client's HEADERS, then drop the connection.
		for {
			f, err := framer.ReadFrame()
			if err != nil {
				return
			}
			if _, ok := f.(*http2.MetaHeadersFrame); ok {
				_ = conn.Close()
				return
			}
		}
	})

	client := newClient(t, tlsCfg)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := client.SendBatch(ctx, []*http.Request{mustReq(t, "https://"+addr+"/")})
	if err == nil {
		t.Fatal("Send: expected error from peer hang-up, got nil")
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return // expected
	}
	t.Errorf("error %v: expected io.EOF / io.ErrUnexpectedEOF / net.ErrClosed reachable via errors.Is", err)
}
