package tth2_test

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/tantosec/tturl/internal/h2test"
	"github.com/tantosec/tturl/tth2"
)

func closeLease(t *testing.T, lease *tth2.ConnLease) {
	t.Helper()
	if err := lease.Close(); err != nil {
		t.Errorf("Close lease: %v", err)
	}
}

func TestConnLeaseRetainsAndReleasesConnection(t *testing.T) {
	t.Parallel()
	var (
		remoteMu sync.Mutex
		remotes  = make(map[string]int)
	)
	addr, tlsConfig := h2test.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteMu.Lock()
		remotes[r.RemoteAddr]++
		remoteMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	client := newClientMaxConns(t, tlsConfig, 1)
	req := mustReq(t, "https://"+addr+"/")

	lease, err := client.Acquire(t.Context(), addr)
	if err != nil {
		t.Fatal(err)
	}
	firstID := lease.ID()
	if firstID == 0 || !lease.Active() {
		t.Fatalf("new lease ID/active = %d/%t", firstID, lease.Active())
	}
	for range 2 {
		batch, err := lease.SendBatch(t.Context(), []*http.Request{req})
		if err != nil {
			t.Fatal(err)
		}
		if batch.Connection != firstID {
			t.Errorf("batch connection = %d, want lease ID %d",
				batch.Connection, firstID)
		}
	}
	remoteMu.Lock()
	if len(remotes) != 1 {
		t.Errorf("leased batches used %d server-side connections, want 1", len(remotes))
	}
	remoteMu.Unlock()
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if lease.Active() {
		t.Fatal("closed lease remains active")
	}
	if _, err := lease.SendBatch(t.Context(), []*http.Request{req}); !errors.Is(err, tth2.ErrLeaseClosed) {
		t.Errorf("send after Close = %v, want ErrLeaseClosed", err)
	}

	reused, err := client.Acquire(t.Context(), addr)
	if err != nil {
		t.Fatal(err)
	}
	if reused.ID() != firstID {
		t.Errorf("released connection ID = %d, want reused ID %d", reused.ID(), firstID)
	}
	reused.Discard()
	reused.Discard()

	replacement, err := client.Acquire(t.Context(), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLease(t, replacement)
	if replacement.ID() == firstID {
		t.Errorf("discarded connection reused ID %d", firstID)
	}
}

func TestConnLeaseConcurrentAcquisitionIsExclusive(t *testing.T) {
	t.Parallel()
	addr, tlsConfig := h2test.Serve(t, okHandler)
	client := newClientMaxConns(t, tlsConfig, 2)
	first, err := client.Acquire(t.Context(), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLease(t, first)
	second, err := client.Acquire(t.Context(), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLease(t, second)
	if first.ID() == second.ID() {
		t.Fatalf("concurrent leases share connection %d", first.ID())
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		lease, err := client.Acquire(ctx, addr)
		if lease != nil {
			_ = lease.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("third acquisition completed while both slots held: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled acquisition = %v, want context.Canceled", err)
	}
}

func TestConnLeaseReusesIdleConnectionsFIFO(t *testing.T) {
	t.Parallel()
	addr, tlsConfig := h2test.Serve(t, okHandler)
	client := newClientMaxConns(t, tlsConfig, 2)

	first, err := client.Acquire(t.Context(), addr)
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.Acquire(t.Context(), addr)
	if err != nil {
		first.Discard()
		t.Fatal(err)
	}
	firstID, secondID := first.ID(), second.ID()
	closeLease(t, first)
	closeLease(t, second)

	reusedFirst, err := client.Acquire(t.Context(), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLease(t, reusedFirst)
	reusedSecond, err := client.Acquire(t.Context(), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLease(t, reusedSecond)
	if reusedFirst.ID() != firstID || reusedSecond.ID() != secondID {
		t.Errorf("reused connection IDs = (%d, %d), want FIFO order (%d, %d)",
			reusedFirst.ID(), reusedSecond.ID(), firstID, secondID)
	}
}

func TestConnLeaseStreamResetKeepsConnection(t *testing.T) {
	t.Parallel()
	const batchSize = 3
	addr, tlsConfig := startRawH2Server(t, resetOneStreamServer(batchSize))
	client := newClient(t, tlsConfig)
	lease, err := client.Acquire(t.Context(), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLease(t, lease)
	reqs := make([]*http.Request, batchSize)
	for i := range reqs {
		reqs[i] = mustReq(t, "https://"+addr+"/")
	}
	first, err := lease.SendBatch(t.Context(), reqs)
	if _, ok := errors.AsType[*tth2.StreamError](err); !ok || first == nil {
		t.Fatalf("first batch/error = %v/%v, want partial StreamError", first, err)
	}
	if !lease.Active() || first.Connection != lease.ID() {
		t.Fatalf("lease after reset active/connection = %t/%d, want true/%d",
			lease.Active(), first.Connection, lease.ID())
	}
	second, err := lease.SendBatch(t.Context(), reqs)
	if err != nil {
		t.Fatal(err)
	}
	if second.Connection != lease.ID() {
		t.Errorf("second connection = %d, want %d", second.Connection, lease.ID())
	}
}

func TestConnLeaseConnectionFailureInvalidates(t *testing.T) {
	t.Parallel()
	addr, tlsConfig := startRawH2Server(t, func(
		tb testing.TB,
		framer *http2.Framer,
		writer *bufio.Writer,
		conn net.Conn,
	) {
		if err := doServerPreface(tb, framer, writer); err != nil {
			return
		}
		for {
			frame, err := framer.ReadFrame()
			if err != nil {
				return
			}
			if _, ok := frame.(*http2.MetaHeadersFrame); ok {
				_ = conn.Close()
				return
			}
		}
	})
	client := newClient(t, tlsConfig)
	lease, err := client.Acquire(t.Context(), addr)
	if err != nil {
		t.Fatal(err)
	}
	req := mustReq(t, "https://"+addr+"/")
	if _, err := lease.SendBatch(t.Context(), []*http.Request{req}); err == nil {
		t.Fatal("connection-fatal send succeeded")
	}
	if lease.Active() {
		t.Fatal("failed connection left lease active")
	}
	if _, err := lease.SendBatch(t.Context(), []*http.Request{req}); !errors.Is(err, tth2.ErrLeaseInvalid) {
		t.Errorf("send after connection failure = %v, want ErrLeaseInvalid", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("Close invalid lease: %v", err)
	}
}

func TestConnLeaseGoAwayInvalidates(t *testing.T) {
	t.Parallel()
	addr, tlsConfig := startRawH2Server(t, func(
		tb testing.TB,
		framer *http2.Framer,
		writer *bufio.Writer,
		_ net.Conn,
	) {
		if err := doServerPreface(tb, framer, writer); err != nil {
			return
		}
		for {
			frame, err := framer.ReadFrame()
			if err != nil {
				return
			}
			if _, ok := frame.(*http2.MetaHeadersFrame); !ok {
				continue
			}
			if err := framer.WriteGoAway(0, http2.ErrCodeNo, nil); err != nil {
				return
			}
			_ = writer.Flush()
			return
		}
	})
	client := newClient(t, tlsConfig)
	lease, err := client.Acquire(t.Context(), addr)
	if err != nil {
		t.Fatal(err)
	}
	request := mustReq(t, "https://"+addr+"/")
	if _, err := lease.SendBatch(t.Context(), []*http.Request{request}); err == nil {
		t.Fatal("GOAWAY send succeeded")
	}
	if lease.Active() {
		t.Fatal("GOAWAY left lease active")
	}
}

func TestConnLeaseRejectsDifferentOrigin(t *testing.T) {
	t.Parallel()
	addr, tlsConfig := h2test.Serve(t, okHandler)
	client := newClient(t, tlsConfig)
	lease, err := client.Acquire(t.Context(), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLease(t, lease)
	request, err := http.NewRequestWithContext(
		t.Context(), http.MethodGet, "https://example.invalid/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lease.SendBatch(t.Context(), []*http.Request{request}); err == nil {
		t.Fatal("different-origin send succeeded")
	}
	if !lease.Active() {
		t.Fatal("validation error invalidated lease")
	}
}

func TestConnLeaseCancelledSendInvalidates(t *testing.T) {
	t.Parallel()
	addr, tlsConfig := h2test.Serve(t, okHandler)
	client := newClient(t, tlsConfig)
	lease, err := client.Acquire(t.Context(), addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	request := mustReq(t, "https://"+addr+"/")
	if _, err := lease.SendBatch(ctx, []*http.Request{request}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled send = %v, want context.Canceled", err)
	}
	if lease.Active() {
		t.Fatal("cancelled send left lease active")
	}
	replacement, err := client.Acquire(t.Context(), addr)
	if err != nil {
		t.Fatalf("replacement acquisition after cancellation: %v", err)
	}
	defer closeLease(t, replacement)
}

func TestSendBatchReportsConnectionIdentity(t *testing.T) {
	t.Parallel()
	addr, tlsConfig := h2test.Serve(t, okHandler)
	client := newClient(t, tlsConfig)
	request := mustReq(t, "https://"+addr+"/")
	first, err := client.SendBatch(t.Context(), []*http.Request{request})
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.SendBatch(t.Context(), []*http.Request{request})
	if err != nil {
		t.Fatal(err)
	}
	if first.Connection == 0 || second.Connection != first.Connection {
		t.Fatalf("pooled connection IDs = %d, %d; want one non-zero ID",
			first.Connection, second.Connection)
	}
	client.Transport.CloseIdleConnections()
	third, err := client.SendBatch(t.Context(), []*http.Request{request})
	if err != nil {
		t.Fatal(err)
	}
	if third.Connection == first.Connection {
		t.Errorf("replacement retained connection ID %d", third.Connection)
	}
}

func TestConnLeaseReleaseUnblocksPooledSend(t *testing.T) {
	t.Parallel()
	addr, tlsConfig := h2test.Serve(t, okHandler)
	client := newClientMaxConns(t, tlsConfig, 1)
	lease, err := client.Acquire(t.Context(), addr)
	if err != nil {
		t.Fatal(err)
	}
	request := mustReq(t, "https://"+addr+"/")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := client.SendBatch(ctx, []*http.Request{request})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("pooled send completed while lease held the only slot: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("pooled send after release: %v", err)
	}
}

func TestConnLeaseCloseIdleRetiresAfterRelease(t *testing.T) {
	t.Parallel()
	addr, tlsConfig := h2test.Serve(t, okHandler)
	client := newClientMaxConns(t, tlsConfig, 1)
	lease, err := client.Acquire(t.Context(), addr)
	if err != nil {
		t.Fatal(err)
	}
	firstID := lease.ID()
	client.Transport.CloseIdleConnections()

	request := mustReq(t, "https://"+addr+"/")
	batch, err := lease.SendBatch(t.Context(), []*http.Request{request})
	if err != nil {
		t.Fatalf("leased send after CloseIdleConnections: %v", err)
	}
	if batch.Connection != firstID || !lease.Active() {
		t.Fatalf("batch connection/lease active = %d/%t, want %d/true",
			batch.Connection, lease.Active(), firstID)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}

	replacement, err := client.Acquire(t.Context(), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLease(t, replacement)
	if replacement.ID() == firstID {
		t.Fatalf("retired leased connection reused ID %d", firstID)
	}
}
