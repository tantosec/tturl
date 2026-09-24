package timing

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/net/http2/hpack"

	"github.com/tantosec/tturl/internal/h2test"
	"github.com/tantosec/tturl/internal/h2tls"
)

func TestAcquisitionHandshakeStallUsesFiniteBudget(t *testing.T) {
	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		peer, err := listener.Accept()
		if err == nil {
			defer func() { _ = peer.Close() }()
			// Read the ClientHello without replying; no TLS setup can finish.
			_, _ = bufio.NewReader(peer).ReadBytes(0)
			var byteBuffer [1]byte
			for {
				if _, err := peer.Read(byteBuffer[:]); err != nil {
					return
				}
			}
		}
	}()
	setupCalled := false
	conn, err := acquireSocket(t.Context(), listener.Addr().String(),
		&tls.Config{MinVersion: tls.VersionTLS12, ServerName: "localhost"},
		100*time.Millisecond, func(context.Context, net.Conn) error {
			setupCalled = true
			return nil
		})
	if conn != nil || !errors.Is(err, context.DeadlineExceeded) || setupCalled {
		t.Fatalf("stalled TLS acquisition: conn=%v err=%v setup=%t", conn, err, setupCalled)
	}
	<-peerDone
}

func TestAcquisitionSetupRemainsCancellableWhenUnlimited(t *testing.T) {
	address, config := h2test.Serve(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		conn, err := acquireSocket(ctx, address, config, 0, func(ctx context.Context, _ net.Conn) error {
			close(entered)
			<-ctx.Done()
			return context.Cause(ctx)
		})
		if conn != nil {
			_ = conn.Close()
		}
		result <- err
	}()
	select {
	case <-entered:
	case err := <-result:
		t.Fatalf("acquisition failed before setup: %v", err)
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("unlimited setup cancellation: %v", err)
	}
}

func TestAcquisitionBudgetEndsBeforeLeasedExchange(t *testing.T) {
	address, config := h2test.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), h2tls.WithProtocols("h2", "http/1.1"))
	config.NextProtos = []string{"http/1.1"}
	var acquisitionDone <-chan struct{}
	conn, err := acquireSocket(t.Context(), address, config, time.Minute,
		func(ctx context.Context, conn net.Conn) error {
			acquisitionDone = ctx.Done()
			if conn.(*tls.Conn).ConnectionState().NegotiatedProtocol != "http/1.1" {
				return errors.New("HTTP/1.1 was not negotiated")
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	select {
	case <-acquisitionDone:
	default:
		t.Fatal("temporary acquisition context outlived acquisition")
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+address+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := request.Write(conn); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("leased exchange status = %d", response.StatusCode)
	}
}

func TestAcquisitionHandshakeAndSetupShareDeadline(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, ClientAuth: tls.RequestClientCert}
	server.StartTLS()
	defer server.Close()
	config := server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	var handshakeDeadline time.Time
	config.GetClientCertificate = func(info *tls.CertificateRequestInfo) (*tls.Certificate, error) {
		var ok bool
		handshakeDeadline, ok = info.Context().Deadline()
		if !ok {
			return nil, errors.New("handshake lacks acquisition deadline")
		}
		return &tls.Certificate{}, nil
	}
	conn, err := acquireSocket(t.Context(), server.Listener.Addr().String(), config, time.Minute,
		func(ctx context.Context, _ net.Conn) error {
			deadline, ok := ctx.Deadline()
			if !ok || handshakeDeadline.IsZero() || !deadline.Equal(handshakeDeadline) {
				return errors.New("protocol setup renewed the acquisition budget")
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
}

func TestAcquisitionHTTP2SetupWatcherEndsBeforeLease(t *testing.T) {
	address, config := h2test.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	config.NextProtos = []string{"h2"}
	var client *h2Conn
	var acquisitionDone <-chan struct{}
	conn, err := acquireSocket(t.Context(), address, config, time.Minute,
		func(ctx context.Context, conn net.Conn) error {
			acquisitionDone = ctx.Done()
			var err error
			client, err = h2Acquire(ctx, conn.(*tls.Conn), 1024)
			return err
		})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		client.close(context.Canceled)
		<-client.readerDone
		<-client.writerDone
	}()
	if conn != client.conn {
		t.Fatal("protocol setup changed the leased socket")
	}
	select {
	case <-acquisitionDone:
	default:
		t.Fatal("temporary acquisition context outlived setup")
	}
	fields := []hpack.HeaderField{
		{Name: ":method", Value: "GET"},
		{Name: ":scheme", Value: "https"},
		{Name: ":authority", Value: address},
		{Name: ":path", Value: "/"},
	}
	prepared, err := client.h2Prepare(fields, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := client.h2Transfer(ctx, prepared, prepared.Prefix, true); err != nil {
		t.Fatal(err)
	}
	observed := client.h2WaitResponse(ctx, prepared)
	if observed.Err != nil || observed.Status != http.StatusNoContent || observed.Times.Complete.IsZero() {
		t.Fatalf("leased HTTP/2 exchange: %+v", observed)
	}
	if err := client.h2Release(prepared); err != nil {
		t.Fatal(err)
	}
}
