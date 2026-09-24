// Package h2test provides in-process HTTP/2 servers for tests. Each server uses
// a throwaway loopback certificate and shuts down during test cleanup.
// Listener options pass through to [h2tls.Listen].
package h2test

import (
	"crypto/tls"
	"errors"
	"net/http"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/tantosec/tturl/internal/h2tls"
)

// Cert returns a fresh throwaway self-signed loopback certificate, or fails tb.
func Cert(tb testing.TB) tls.Certificate {
	tb.Helper()
	cert, err := h2tls.SelfSignedServerCert()
	if err != nil {
		tb.Fatalf("h2test: self-signed cert: %v", err)
	}
	return cert
}

// Serve starts an HTTPS server handled by h on a random loopback port. It
// returns the dial address and a fresh [tls.Config] that trusts the server's
// certificate. The server uses the default [http2.Server] settings and shuts
// down during tb.Cleanup. Options configure the listener before TLS is applied.
func Serve(
	tb testing.TB,
	h http.Handler,
	options ...h2tls.Option,
) (addr string, tlsCfg *tls.Config) {
	tb.Helper()
	ep, err := h2tls.Listen(Cert(tb), options...)
	if err != nil {
		tb.Fatalf("h2test: listen: %v", err)
	}

	// ReadHeaderTimeout is set because a server without one is a slowloris
	// target (gosec G112); no test should come near it.
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	if err := http2.ConfigureServer(srv, &http2.Server{}); err != nil {
		_ = ep.Listener.Close()
		tb.Fatalf("h2test: configure server: %v", err)
	}

	var serveErr error
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		serveErr = srv.Serve(ep.Listener)
	}()

	tb.Cleanup(func() {
		// Close forcibly tears down the listener and any active conns, so tests
		// with hanging handlers (e.g. cancellation tests holding r.Context())
		// don't leak their handler goroutines.
		_ = srv.Close()
		<-serveDone
		// A server that died on its own leaves every dial failing for a reason
		// that looks nothing like the cause; say so once, here, rather than let
		// the test report a connection error.
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			tb.Errorf("h2test: server stopped early: %v", serveErr)
		}
	})

	return ep.Addr, ep.ClientConfig()
}
