package tth2_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/tantosec/tturl/internal/h2test"
)

// TestSendCancelMidTrialUnblocks asserts that cancelling the context passed to
// Send returns promptly even when the server is silent. readResponses closes
// the TLS connection on a non-StreamError, which unblocks the dispatcher's
// ReadFrame and lets the call finish.
func TestSendCancelMidTrialUnblocks(t *testing.T) {
	t.Parallel()
	const requests = 2
	started := make(chan struct{}, requests)
	// Handler hangs on r.Context() — when the client cancels and the connection
	// drops, the handler's context fires too and the goroutine exits cleanly, so
	// the test cleanup doesn't leak.
	handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
	})
	addr, tlsCfg := h2test.Serve(t, handler)

	client := newClient(t, tlsCfg)

	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/"),
		mustReq(t, "https://"+addr+"/"),
	}

	sendCtx, cancelSend := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := client.SendBatch(sendCtx, reqs)
		done <- err
	}()

	awaitSignals(t, started, requests, "request handlers")
	cancelSend()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("SendBatch error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Send did not return within 2s of cancellation; dispatchLoop is wedged")
	}
}

// TestSendCancelMidDrainUnblocks asserts that cancelling the context while the
// send is parked in drainForSendCredit (waiting for flow-control credit) and
// returns promptly with the cancellation error, rather than hanging until the
// internal drain watchdog fires. The raw server completes the preface, then
// reads and discards client frames without ever sending WINDOW_UPDATE. The
// client exhausts the 65535-byte connection window and blocks draining.
func TestSendCancelMidDrainUnblocks(t *testing.T) {
	t.Parallel()
	const initialConnectionWindow = 65_535
	windowExhausted := make(chan struct{})
	var signalWindow sync.Once
	addr, tlsCfg := startRawH2Server(t, func(tb testing.TB, framer *http2.Framer, bw *bufio.Writer, _ net.Conn) {
		if err := doServerPreface(tb, framer, bw); err != nil {
			tb.Logf("doServerPreface: %v", err)
			return
		}
		dataBytes := 0
		for {
			frame, err := framer.ReadFrame()
			if err != nil {
				return
			}
			if data, ok := frame.(*http2.DataFrame); ok {
				dataBytes += len(data.Data())
				if dataBytes >= initialConnectionWindow {
					signalWindow.Do(func() { close(windowExhausted) })
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

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := client.SendBatch(ctx, []*http.Request{req})
		done <- err
	}()

	awaitSignals(t, windowExhausted, 1, "connection-window exhaustion")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("SendBatch error = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SendBatch did not return within 10s of cancellation mid-drain")
	}
}

// TestSendCancelDuringPrefaceReadUnblocks asserts that cancellation interrupts
// a connection waiting for the peer's initial SETTINGS frame.
func TestSendCancelDuringPrefaceReadUnblocks(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := startRawH2Server(t, func(
		_ testing.TB,
		framer *http2.Framer,
		_ *bufio.Writer,
		_ net.Conn,
	) {
		for {
			if _, err := framer.ReadFrame(); err != nil {
				return
			}
		}
	})

	client := newClient(t, tlsCfg)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := client.SendBatch(ctx, []*http.Request{
		mustReq(t, "https://"+addr+"/"),
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("SendBatch error = %v, want context.DeadlineExceeded", err)
	}
}

// TestSendCancelThenReuseReconnects asserts that after a cancellation kills the
// connection, a subsequent Send re-dials and succeeds. This covers the
// discard-and-redial path in Transport.sendBatch: a context cancellation is not
// a recoverable retry, but it must not leave the Client in an unusable state.
func TestSendCancelThenReuseReconnects(t *testing.T) {
	t.Parallel()
	const requests = 2
	started := make(chan struct{}, requests)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			started <- struct{}{}
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	addr, tlsCfg := h2test.Serve(t, handler)

	client := newClient(t, tlsCfg)

	// First Send: cancel mid-trial.
	sendCtx, cancelSend := context.WithCancel(t.Context())
	first := make(chan error, 1)
	go func() {
		_, err := client.SendBatch(sendCtx, []*http.Request{
			mustReq(t, "https://"+addr+"/slow"),
			mustReq(t, "https://"+addr+"/slow"),
		})
		first <- err
	}()
	awaitSignals(t, started, requests, "slow request handlers")
	cancelSend()
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("first SendBatch error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first Send did not return promptly")
	}

	// Second Send: should re-dial. /fast does not block.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	batch, err := client.SendBatch(ctx, []*http.Request{
		mustReq(t, "https://"+addr+"/fast"),
		mustReq(t, "https://"+addr+"/fast"),
	})
	if err != nil {
		t.Fatalf("second Send: %v", err)
	}
	results := batch.Results
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	for i, r := range results {
		if r.Response.StatusCode != http.StatusOK {
			t.Errorf("result %d: status = %d, want 200", i, r.Response.StatusCode)
		}
	}
}

// TestSendWithPreCancelledCtxReturnsFast asserts that a Send called with an
// already-cancelled context returns quickly with the cancellation error. The
// send phase writes HEADERS synchronously (the framer doesn't check ctx), so
// the wire may briefly carry data before the ctx is observed — but the
// collector goroutines see ctx.Done() immediately and readResponses cancels
// innerCtx and tears the conn down without retries.
func TestSendWithPreCancelledCtxReturnsFast(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, okHandler)

	client := newClient(t, tlsCfg)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	done := make(chan error, 1)
	go func() {
		_, err := client.SendBatch(ctx, []*http.Request{mustReq(t, "https://"+addr+"/")})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("SendBatch error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SendBatch did not return for a pre-cancelled context")
	}
}

func awaitSignals(tb testing.TB, signals <-chan struct{}, n int, state string) {
	tb.Helper()
	for range n {
		select {
		case <-signals:
		case <-time.After(10 * time.Second):
			tb.Fatalf("timed out waiting for %s", state)
		}
	}
}

// TestSendDeadlineReturnsDeadlineExceeded asserts the same dispatcher unblock
// works for a context.DeadlineExceeded — the prompt cancellation flow is the
// same, but the error surface is the one consumers tend to branch on. errors.Is
// must resolve to context.DeadlineExceeded.
func TestSendDeadlineReturnsDeadlineExceeded(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	addr, tlsCfg := h2test.Serve(t, handler)

	client := newClient(t, tlsCfg)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := client.SendBatch(ctx, []*http.Request{mustReq(t, "https://"+addr+"/")})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Send: expected DeadlineExceeded, got nil")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Send: error %v is not context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Send did not honour the 100ms deadline within 2s")
	}
}
