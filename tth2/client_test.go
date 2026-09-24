package tth2_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tantosec/tturl/internal/h2test"
	"github.com/tantosec/tturl/internal/h2tls"
	"github.com/tantosec/tturl/tth2"
)

// refusedAddr returns a localhost TCP address verified to refuse connections.
// We bind 127.0.0.1:0 to grab an ephemeral port, release it, then probe with
// a short dial to confirm the kernel now responds with RST (ECONNREFUSED).
// Race-prone only in the microseconds between close and the dial under test
// if a third party rebinds the exact ephemeral port — the probe converts that
// pathological case into a clear setup-time failure rather than a confusing
// assertion later.
func refusedAddr(tb testing.TB) string {
	tb.Helper()
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("listen on 127.0.0.1:0: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		tb.Fatalf("close listener: %v", err)
	}
	conn, err := (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext(context.Background(), "tcp", addr)
	if err == nil {
		_ = conn.Close()
		tb.Fatalf("expected %s to refuse connections, but a peer accepted", addr)
	}
	return addr
}

// TestSendBatchToRefusedAddrErrors verifies that SendBatch reports a lazy dial
// failure.
func TestSendBatchToRefusedAddrErrors(t *testing.T) {
	t.Parallel()
	c := newClient(t, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) //nolint:gosec // test only
	addr := refusedAddr(t)
	_, err := c.SendBatch(t.Context(), []*http.Request{mustReq(t, "https://"+addr+"/")})
	if err == nil {
		t.Fatal("expected error sending to a refused address, got nil")
	}
}

// TestClientSendValidation verifies that invalid inputs fail before dialling.
func TestClientSendValidation(t *testing.T) {
	t.Parallel()
	c := newClient(t, nil)

	tests := []struct {
		name    string
		reqs    func(t *testing.T) []*http.Request
		wantErr string
	}{
		{
			name:    "no requests",
			reqs:    func(t *testing.T) []*http.Request { return nil },
			wantErr: "at least one",
		},
		{
			name: "mixed hosts",
			reqs: func(t *testing.T) []*http.Request {
				return []*http.Request{
					mustReq(t, "https://host-a.example.com/"),
					mustReq(t, "https://host-b.example.com/"),
				}
			},
			wantErr: "differs from",
		},
		{
			name: "one-shot body (GetBody nil)",
			reqs: func(t *testing.T) []*http.Request {
				r, err := http.NewRequestWithContext(t.Context(), "POST", "https://host/", bytes.NewReader([]byte("x")))
				if err != nil {
					t.Fatalf("NewRequest: %v", err)
				}
				r.GetBody = nil // simulate a one-shot body: Body set, no rewind
				return []*http.Request{r}
			},
			wantErr: "one-shot body",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := c.SendBatch(t.Context(), tc.reqs(t))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestSendBatchSuccessfulResultContract checks the complete public result
// shape for ordinary successful narrow and wide batches.
func TestSendBatchSuccessfulResultContract(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, okHandler)
	for _, n := range []int{2, 64} {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			t.Parallel()
			c := newClient(t, tlsCfg)
			reqs := make([]*http.Request, n)
			for i := range reqs {
				reqs[i] = mustReq(t, fmt.Sprintf("https://%s/%d", addr, i))
			}

			batch, err := c.SendBatch(t.Context(), reqs)
			if err != nil {
				t.Fatalf("SendBatch: %v", err)
			}
			if len(batch.Results) != n {
				t.Fatalf("len(Results) = %d, want %d", len(batch.Results), n)
			}
			if len(batch.ArrivalOrder) != n {
				t.Fatalf("len(ArrivalOrder) = %d, want %d",
					len(batch.ArrivalOrder), n)
			}

			seenRanks := make([]bool, n)
			seenStreams := make(map[uint32]bool, n)
			for i, result := range batch.Results {
				if !result.Arrived() {
					t.Fatalf("result %d = %+v, want a response", i, result)
				}
				if result.Response.StatusCode != http.StatusOK || result.Reset != nil {
					t.Errorf("result %d status/reset = %d/%v, want 200/nil",
						i, result.Response.StatusCode, result.Reset)
				}
				if result.StreamID == 0 || result.StreamID%2 == 0 ||
					seenStreams[result.StreamID] {
					t.Errorf("result %d StreamID = %d, want a unique non-zero odd ID",
						i, result.StreamID)
				}
				seenStreams[result.StreamID] = true
				if result.ArrivalRank < 0 || result.ArrivalRank >= n ||
					seenRanks[result.ArrivalRank] {
					t.Errorf("result %d ArrivalRank = %d, want a unique rank in [0,%d)",
						i, result.ArrivalRank, n)
					continue
				}
				seenRanks[result.ArrivalRank] = true
			}
			for rank, i := range batch.ArrivalOrder {
				if i < 0 || i >= n {
					t.Errorf("ArrivalOrder[%d] = %d, want a request index", rank, i)
					continue
				}
				if got := batch.Results[i].ArrivalRank; got != rank {
					t.Errorf("ArrivalOrder[%d] = %d with ArrivalRank %d",
						rank, i, got)
				}
			}
		})
	}
}

func TestResultSeparatesDeclaredAndReceivedBodyLength(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "body")
	}))
	c := newClient(t, tlsCfg)

	batch, err := c.SendBatch(t.Context(), []*http.Request{
		mustReq(t, "https://"+addr+"/"),
	})
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	result := batch.Results[0]
	if result.Response.ContentLength != 4 {
		t.Errorf("ContentLength = %d, want 4", result.Response.ContentLength)
	}
	if result.Response.Body.BytesReceived != 4 {
		t.Errorf("Body.BytesReceived = %d, want 4",
			result.Response.Body.BytesReceived)
	}
}

func TestSendOrderingFollowsResponseHeaders(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := startRawH2Server(t,
		respondInPathOrder("/fast", "/slow"))
	ctx := t.Context()
	c := newClient(t, tlsCfg)

	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/slow"),
		mustReq(t, "https://"+addr+"/fast"),
	}
	batch, err := c.SendBatch(ctx, reqs, tth2.WithResponseHeaderCapture())
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	results := batch.Results
	if got := results[0].ArrivalRank; got != 1 {
		t.Errorf("/slow ArrivalRank = %d, want 1", got)
	}
	if got := results[1].ArrivalRank; got != 0 {
		t.Errorf("/fast ArrivalRank = %d, want 0", got)
	}
}

// TestMaxConnsPerHostLimitsConcurrency verifies the per-origin connection cap
// with deterministic handler overlap.
func TestMaxConnsPerHostLimitsConcurrency(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ cap, k int }{
		{cap: 1, k: 3},
		{cap: 2, k: 4},
		{cap: 4, k: 4},
	} {
		t.Run(fmt.Sprintf("cap=%d/k=%d", tc.cap, tc.k), func(t *testing.T) {
			t.Parallel()
			want := min(tc.k, tc.cap)

			var cur, peak atomic.Int64
			release := make(chan struct{})
			var once sync.Once
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				n := cur.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				if int(n) == want {
					once.Do(func() { close(release) }) // enough overlap reached
				}
				<-release
				cur.Add(-1)
				w.WriteHeader(http.StatusOK)
			})
			addr, tlsCfg := h2test.Serve(t, handler)
			tr := &tth2.Transport{TLSClientConfig: tlsCfg, MaxConnsPerHost: tc.cap}
			t.Cleanup(tr.CloseIdleConnections)
			c := &tth2.Client{Transport: tr}
			ctx := t.Context()
			url := "https://" + addr + "/"

			var wg sync.WaitGroup
			errs := make([]error, tc.k)
			for i := range tc.k {
				req := mustReq(t, url) // built on the test goroutine, one per call
				wg.Go(func() {
					_, errs[i] = c.SendBatch(ctx, []*http.Request{req})
				})
			}

			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				once.Do(func() { close(release) }) // unblock so the test fails cleanly
				<-done
				t.Fatal("SendBatch calls did not complete within 10s")
			}

			for i, err := range errs {
				if err != nil {
					t.Errorf("SendBatch %d: %v", i, err)
				}
			}
			if got := peak.Load(); got != int64(want) {
				t.Errorf("peak concurrency = %d, want %d (cap=%d, k=%d)", got, want, tc.cap, tc.k)
			}
		})
	}
}

func TestSendBodyIntegrityAcrossSizes(t *testing.T) {
	t.Parallel()
	// Three bodies sent in parallel over one HTTP/2 connection. 256 B fits in a
	// single DATA frame; 64 KiB spans several frames; 4 MiB exceeds the
	// x/net/http2 server's 1 MiB default per-stream receive window, so multiple
	// WINDOW_UPDATE round-trips are needed to drain it, and the 4 MiB + 64 KiB +
	// 256 B aggregate likewise drains the per-connection window several times.
	// Each byte depends on both its position and the stream index, so
	// within-stream reordering, truncation, and cross-stream contamination all
	// break the hash.
	addr, tlsCfg := h2test.Serve(t, sha256Handler)
	ctx := t.Context()
	c := newClient(t, tlsCfg)

	sizes := []int{256, 64 << 10, 4 << 20}
	wantHash := make([]string, len(sizes))
	reqs := make([]*http.Request, len(sizes))
	for i, sz := range sizes {
		b := make([]byte, sz)
		for j := range b {
			b[j] = byte(j ^ (i + 1)) //nolint:gosec // test payload byte; wrap is intentional
		}
		sum := sha256.Sum256(b)
		wantHash[i] = hex.EncodeToString(sum[:])
		req, err := http.NewRequestWithContext(ctx, "POST",
			"https://"+addr+"/", bytes.NewReader(b))
		if err != nil {
			t.Fatalf("size=%d: NewRequest: %v", sz, err)
		}
		reqs[i] = req
	}

	batch, err := c.SendBatch(ctx, reqs, tth2.WithResponseHeaderCapture())
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	results := batch.Results
	if got := len(results); got != len(reqs) {
		t.Fatalf("len(results) = %d, want %d", got, len(reqs))
	}
	for i, r := range results {
		if r.Response.StatusCode != http.StatusOK {
			t.Errorf("size=%d: status = %d, want 200",
				sizes[i], r.Response.StatusCode)
			continue
		}
		if got := r.Response.Header.Get(bodyHashHeader); got != wantHash[i] {
			t.Errorf("size=%d: server sha256 = %s, want %s",
				sizes[i], got, wantHash[i])
		}
	}
}

// TestSendBatchFailsAgainstNonH2Server verifies that a negotiated non-h2 ALPN
// protocol fails the first send without downgrading. The caller and server
// advertise http/1.1 so the failure reaches the post-handshake protocol check.
func TestSendBatchFailsAgainstNonH2Server(t *testing.T) {
	t.Parallel()
	ep, err := h2tls.Listen(h2test.Cert(t), h2tls.WithProtocols("http/1.1"))
	if err != nil {
		t.Fatalf("h2tls.Listen: %v", err)
	}
	t.Cleanup(func() { _ = ep.Listener.Close() })

	go func() {
		for {
			conn, err := ep.Listener.Accept()
			if err != nil {
				return
			}
			if tc, ok := conn.(*tls.Conn); ok {
				_ = tc.HandshakeContext(context.Background())
			}
			_ = conn.Close()
		}
	}()

	// tth2 prepends "h2", so this client offers ["h2", "http/1.1"]; the server
	// offers only "http/1.1", ALPN settles there, and tth2's h2 check fires.
	tlsCfg := ep.ClientConfig()
	tlsCfg.NextProtos = []string{"http/1.1"}
	c := newClient(t, tlsCfg)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err = c.SendBatch(ctx, []*http.Request{mustReq(t, "https://"+ep.Addr+"/")})
	if err == nil {
		t.Fatal("SendBatch: expected error against an http/1.1-only server")
	}
	if !strings.Contains(err.Error(), "expected h2") {
		t.Errorf("error %q does not mention 'expected h2'", err)
	}
}

// TestSendBatchAfterCloseIdleReconnects checks that CloseIdleConnections is
// idempotent before and after use, and that closing a pooled connection leaves
// the Transport usable through a transparent re-dial.
func TestSendBatchAfterCloseIdleReconnects(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, okHandler)
	tr := &tth2.Transport{TLSClientConfig: tlsCfg}
	t.Cleanup(tr.CloseIdleConnections)
	c := &tth2.Client{Transport: tr}

	tr.CloseIdleConnections()
	tr.CloseIdleConnections()

	reqs := []*http.Request{mustReq(t, "https://"+addr+"/")}
	if _, err := c.SendBatch(t.Context(), reqs); err != nil {
		t.Fatalf("first SendBatch: %v", err)
	}

	tr.CloseIdleConnections()
	tr.CloseIdleConnections()

	batch, err := c.SendBatch(t.Context(), reqs)
	if err != nil {
		t.Fatalf("SendBatch after CloseIdleConnections: %v (expected silent re-dial)", err)
	}
	results := batch.Results
	if len(results) != 1 || results[0].Response.StatusCode != http.StatusOK {
		t.Errorf("SendBatch after CloseIdleConnections: unexpected results %+v", results)
	}
}

// TestSendMixedMethodBatch verifies mixed body-bearing and bodyless streams in
// the withheld-tail send path.
func TestSendMixedMethodBatch(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := sha256.New()
		_, _ = io.Copy(h, r.Body)
		w.Header().Set("X-Method", r.Method)
		w.Header().Set(bodyHashHeader, hex.EncodeToString(h.Sum(nil)))
	})
	addr, tlsCfg := h2test.Serve(t, handler)

	c := newClient(t, tlsCfg)

	postBody := make([]byte, 4096)
	for i := range postBody {
		postBody[i] = byte(i * 13)
	}
	postReq, err := http.NewRequestWithContext(t.Context(), "POST", "https://"+addr+"/p", bytes.NewReader(postBody))
	if err != nil {
		t.Fatalf("POST NewRequest: %v", err)
	}
	getReq := mustReq(t, "https://"+addr+"/g")

	batch, err := c.SendBatch(t.Context(), []*http.Request{postReq, getReq},
		tth2.WithResponseHeaderCapture())
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	results := batch.Results
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	for i, want := range []string{"POST", "GET"} {
		if got := results[i].Response.Header.Get("X-Method"); got != want {
			t.Errorf("result %d: X-Method = %q, want %q", i, got, want)
		}
		if results[i].Response.StatusCode != http.StatusOK {
			t.Errorf("result %d: status = %d, want 200", i, results[i].Response.StatusCode)
		}
	}
	wantPost := sha256.Sum256(postBody)
	if got := results[0].Response.Header.Get(bodyHashHeader); got != hex.EncodeToString(wantPost[:]) {
		t.Errorf("POST body hash mismatch: got %s, want %x", got, wantPost)
	}
	wantEmpty := sha256.Sum256(nil)
	if got := results[1].Response.Header.Get(bodyHashHeader); got != hex.EncodeToString(wantEmpty[:]) {
		t.Errorf("GET body hash = %s, want sha256 of empty = %x", got, wantEmpty)
	}
}

// TestCloseIdleConnectionsRetiresInflightConn verifies that a connection
// checked out when CloseIdleConnections runs is discarded when its call ends.
func TestCloseIdleConnectionsRetiresInflightConn(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHandler)
	var first atomic.Bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !first.Swap(true) {
			close(started)
			<-release
		}
		w.WriteHeader(http.StatusOK)
	})
	addr, tlsCfg := h2test.Serve(t, handler)

	var dials atomic.Int32
	dialer := func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	tr := &tth2.Transport{TLSClientConfig: tlsCfg, DialContext: dialer}
	t.Cleanup(tr.CloseIdleConnections)
	client := &tth2.Client{Transport: tr}
	reqs := []*http.Request{mustReq(t, "https://"+addr+"/")}

	done := make(chan error, 1)
	go func() {
		_, err := client.SendBatch(t.Context(), reqs)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}
	tr.CloseIdleConnections()
	releaseHandler()
	if err := <-done; err != nil {
		t.Fatalf("in-flight SendBatch: %v", err)
	}
	if _, err := client.SendBatch(t.Context(), reqs); err != nil {
		t.Fatalf("SendBatch after CloseIdleConnections: %v", err)
	}
	if got := dials.Load(); got != 2 {
		t.Errorf("dial count = %d, want 2", got)
	}
}

// TestTransportServesMultipleOrigins verifies independent per-origin pools.
func TestTransportServesMultipleOrigins(t *testing.T) {
	t.Parallel()
	addrA, _ := h2test.Serve(t, okHandler)
	addrB, _ := h2test.Serve(t, okHandler)

	tr := &tth2.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // test only
		MinVersion:         tls.VersionTLS12,
	}}
	t.Cleanup(tr.CloseIdleConnections)
	c := &tth2.Client{Transport: tr}

	for _, addr := range []string{addrA, addrB} {
		batch, err := c.SendBatch(t.Context(), []*http.Request{mustReq(t, "https://"+addr+"/")})
		if err != nil {
			t.Fatalf("SendBatch to %s: %v", addr, err)
		}
		results := batch.Results
		if len(results) != 1 || results[0].Response.StatusCode != http.StatusOK {
			t.Fatalf("SendBatch to %s: unexpected results %+v", addr, results)
		}
	}
}

// TestTransportReusesPooledConn asserts sequential batches to one origin reuse
// the pooled connection rather than dialling each time.
func TestTransportReusesPooledConn(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, okHandler)

	var mu sync.Mutex
	dials := 0
	dialer := func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err == nil {
			mu.Lock()
			dials++
			mu.Unlock()
		}
		return conn, err
	}
	tr := &tth2.Transport{TLSClientConfig: tlsCfg, DialContext: dialer}
	t.Cleanup(tr.CloseIdleConnections)
	c := &tth2.Client{Transport: tr}

	reqs := []*http.Request{mustReq(t, "https://"+addr+"/")}
	for i := range 5 {
		if _, err := c.SendBatch(t.Context(), reqs); err != nil {
			t.Fatalf("SendBatch %d: %v", i, err)
		}
	}
	mu.Lock()
	got := dials
	mu.Unlock()
	if got != 1 {
		t.Errorf("5 sequential SendBatch dialled %d connections, want 1 (pooled reuse)", got)
	}
}

// TestSendBatchRetriesStalePooledConn verifies one fresh dial after reuse of a
// dead pooled connection.
func TestSendBatchRetriesStalePooledConn(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, okHandler)

	var mu sync.Mutex
	var dialled []net.Conn
	dialer := func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err == nil {
			mu.Lock()
			dialled = append(dialled, conn)
			mu.Unlock()
		}
		return conn, err
	}
	tr := &tth2.Transport{TLSClientConfig: tlsCfg, DialContext: dialer}
	t.Cleanup(tr.CloseIdleConnections)
	c := &tth2.Client{Transport: tr}
	reqs := []*http.Request{mustReq(t, "https://"+addr+"/")}

	// First send dials one connection and pools it.
	if _, err := c.SendBatch(t.Context(), reqs); err != nil {
		t.Fatalf("first SendBatch: %v", err)
	}
	mu.Lock()
	n, first := len(dialled), dialled[0]
	mu.Unlock()
	if n != 1 {
		t.Fatalf("first SendBatch dialled %d connections, want 1", n)
	}

	// Kill the pooled connection out from under the pool.
	_ = first.Close()

	// The next send reuses the dead connection, discards it, and retries fresh.
	batch, err := c.SendBatch(t.Context(), reqs)
	if err != nil {
		t.Fatalf("SendBatch after stale pooled conn: %v", err)
	}
	results := batch.Results
	if len(results) != 1 || results[0].Response.StatusCode != http.StatusOK {
		t.Fatalf("unexpected results after retry: %+v", results)
	}
	mu.Lock()
	n = len(dialled)
	mu.Unlock()
	if n != 2 {
		t.Errorf("after stale-conn retry, dialled %d connections, want 2", n)
	}
}

// trackedConn wraps a net.Conn to record whether it has been closed, letting a
// test observe that an evicted idle connection is actually torn down.
type trackedConn struct {
	net.Conn
	closed atomic.Bool
}

func (c *trackedConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

// TestSendBatchSharesReusableRequestConcurrently verifies shared request
// immutability and body integrity across concurrent calls.
func TestSendBatchSharesReusableRequestConcurrently(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, sha256Handler)
	tr := &tth2.Transport{TLSClientConfig: tlsCfg, MaxConnsPerHost: 4}
	t.Cleanup(tr.CloseIdleConnections)
	c := &tth2.Client{Transport: tr}

	body := bytes.Repeat([]byte("x"), 4096)
	sum := sha256.Sum256(body)
	wantHash := hex.EncodeToString(sum[:])
	req, err := http.NewRequestWithContext(t.Context(), "POST", "https://"+addr+"/", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	const k = 8
	var wg sync.WaitGroup
	errs := make([]error, k)
	hashes := make([]string, k)
	for i := range k {
		wg.Go(func() {
			batch, err := c.SendBatch(t.Context(), []*http.Request{req},
				tth2.WithResponseHeaderCapture()) // same request in every goroutine
			if err != nil {
				errs[i] = err
				return
			}
			results := batch.Results
			hashes[i] = results[0].Response.Header.Get(bodyHashHeader)
		})
	}
	wg.Wait()

	for i := range k {
		if errs[i] != nil {
			t.Errorf("call %d: %v", i, errs[i])
			continue
		}
		if hashes[i] != wantHash {
			t.Errorf("call %d: body hash %q, want %q (concurrent sharing corrupted the body)", i, hashes[i], wantHash)
		}
	}
}

// TestIdleConnTimeoutEvictsExpired asserts that a connection idle longer than
// IdleConnTimeout is closed and not reused: the next send dials afresh.
func TestIdleConnTimeoutEvictsExpired(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, okHandler)

	var mu sync.Mutex
	var conns []*trackedConn
	dialer := func(ctx context.Context, network, address string) (net.Conn, error) {
		raw, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		tc := &trackedConn{Conn: raw}
		mu.Lock()
		conns = append(conns, tc)
		mu.Unlock()
		return tc, nil
	}
	tr := &tth2.Transport{TLSClientConfig: tlsCfg, DialContext: dialer, IdleConnTimeout: 50 * time.Millisecond}
	t.Cleanup(tr.CloseIdleConnections)
	c := &tth2.Client{Transport: tr}
	reqs := []*http.Request{mustReq(t, "https://"+addr+"/")}

	if _, err := c.SendBatch(t.Context(), reqs); err != nil {
		t.Fatalf("first SendBatch: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // let the pooled connection age past the timeout
	if _, err := c.SendBatch(t.Context(), reqs); err != nil {
		t.Fatalf("second SendBatch: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(conns) != 2 {
		t.Fatalf("dialled %d connections, want 2 (expired idle conn evicted, fresh dial)", len(conns))
	}
	if !conns[0].closed.Load() {
		t.Error("expired idle connection was not closed on eviction")
	}
	if conns[1].closed.Load() {
		t.Error("freshly-dialled connection should still be open after being pooled")
	}
}

// TestIdleConnTimeoutReusesWithinWindow asserts that a connection reused before
// IdleConnTimeout elapses is not evicted: no second dial.
func TestIdleConnTimeoutReusesWithinWindow(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, okHandler)

	var mu sync.Mutex
	dials := 0
	dialer := func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err == nil {
			mu.Lock()
			dials++
			mu.Unlock()
		}
		return conn, err
	}
	tr := &tth2.Transport{TLSClientConfig: tlsCfg, DialContext: dialer, IdleConnTimeout: 2 * time.Second}
	t.Cleanup(tr.CloseIdleConnections)
	c := &tth2.Client{Transport: tr}
	reqs := []*http.Request{mustReq(t, "https://"+addr+"/")}

	for i := range 3 {
		if _, err := c.SendBatch(t.Context(), reqs); err != nil {
			t.Fatalf("SendBatch %d: %v", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if dials != 1 {
		t.Errorf("dialled %d connections, want 1 (reuse within the idle window)", dials)
	}
}

// TestIdleConnTimeoutZeroNeverEvicts asserts that the zero value disables idle
// eviction: a connection idle well past any short interval is still reused.
func TestIdleConnTimeoutZeroNeverEvicts(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, okHandler)

	var mu sync.Mutex
	dials := 0
	dialer := func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err == nil {
			mu.Lock()
			dials++
			mu.Unlock()
		}
		return conn, err
	}
	tr := &tth2.Transport{TLSClientConfig: tlsCfg, DialContext: dialer} // IdleConnTimeout: 0 (disabled)
	t.Cleanup(tr.CloseIdleConnections)
	c := &tth2.Client{Transport: tr}
	reqs := []*http.Request{mustReq(t, "https://"+addr+"/")}

	if _, err := c.SendBatch(t.Context(), reqs); err != nil {
		t.Fatalf("first SendBatch: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := c.SendBatch(t.Context(), reqs); err != nil {
		t.Fatalf("second SendBatch: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if dials != 1 {
		t.Errorf("dialled %d connections, want 1 (IdleConnTimeout 0 keeps idle conns)", dials)
	}
}

// TestBatchOrigin checks the pre-flight a caller can run before opening a
// connection: it returns the single origin a batch shares, normalised the way a
// dial address is — punycoded, defaulting to tth2's :443 — and rejects the
// three inputs SendBatch would reject.
func TestBatchOrigin(t *testing.T) {
	t.Parallel()

	t.Run("normalises the shared origin", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct{ url, want string }{
			{"https://h/x", "h:443"},       // TLS-only, so :443 is the default port
			{"https://h:8443/x", "h:8443"}, // an explicit port is kept
			{"https://[::1]:8443/x", "[::1]:8443"},
			{"https://bücher.example/x", "xn--bcher-kva.example:443"}, // IDN -> A-label
		} {
			got, err := tth2.BatchOrigin([]*http.Request{mustReq(t, tc.url)})
			if err != nil || got != tc.want {
				t.Errorf("BatchOrigin(%q) = (%q, %v), want (%q, nil)", tc.url, got, err, tc.want)
			}
		}
	})

	t.Run("accepts a batch sharing one origin", func(t *testing.T) {
		t.Parallel()
		reqs := []*http.Request{mustReq(t, "https://h:8443/a"), mustReq(t, "https://h:8443/b")}
		if got, err := tth2.BatchOrigin(reqs); err != nil || got != "h:8443" {
			t.Errorf("BatchOrigin = (%q, %v), want (\"h:8443\", nil)", got, err)
		}
	})

	t.Run("rejects what SendBatch rejects", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name    string
			reqs    []*http.Request
			wantSub string
		}{
			{"empty", nil, "at least one"},
			{"nil request", []*http.Request{nil}, "no valid host"},
			{"nil URL", []*http.Request{{}}, "no valid host"},
			{"nil request after first", []*http.Request{
				mustReq(t, "https://h/x"), nil,
			}, "no valid host"},
			{"hostless", []*http.Request{mustReq(t, "https:///x")}, "no valid host"},
			{"mixed origins", []*http.Request{
				mustReq(t, "https://a.example/x"), mustReq(t, "https://b.example/x"),
			}, "differs from"},
		} {
			_, err := tth2.BatchOrigin(tc.reqs)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("%s: error = %v, want one containing %q", tc.name, err, tc.wantSub)
			}
		}
	})
}
