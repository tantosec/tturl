package h2tls_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/tantosec/tturl/internal/h2tls"
)

func TestSelfSignedServerCert(t *testing.T) {
	t.Parallel()
	started := time.Now()
	cert, err := h2tls.SelfSignedServerCert()
	finished := time.Now()
	if err != nil {
		t.Fatalf("SelfSignedServerCert: %v", err)
	}
	if cert.Leaf == nil {
		t.Fatal("Leaf not populated")
	}
	if len(cert.Certificate) != 1 {
		t.Fatalf("certificate chain length = %d, want 1", len(cert.Certificate))
	}

	leaf := cert.Leaf
	if !bytes.Equal(leaf.Raw, cert.Certificate[0]) {
		t.Error("Leaf does not represent the returned certificate DER")
	}
	privateKey, ok := cert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatalf("PrivateKey type = %T, want *ecdsa.PrivateKey", cert.PrivateKey)
	}
	publicKey, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("leaf public key type = %T, want *ecdsa.PublicKey", leaf.PublicKey)
	}
	if privateKey.Curve != publicKey.Curve ||
		privateKey.Curve.Params().Name != "P-256" {
		t.Errorf("key curve = %q, want P-256", privateKey.Curve.Params().Name)
	}
	if !privateKey.PublicKey.Equal(publicKey) {
		t.Error("private key does not match leaf public key")
	}
	if err := leaf.CheckSignature(
		leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature); err != nil {
		t.Errorf("self-signature: %v", err)
	}

	if leaf.Subject.CommonName != "h2tls" {
		t.Errorf("CommonName = %q, want h2tls", leaf.Subject.CommonName)
	}
	if leaf.Version != 3 {
		t.Errorf("Version = %d, want 3", leaf.Version)
	}
	if !leaf.BasicConstraintsValid {
		t.Error("basic constraints are not valid")
	}
	if leaf.IsCA {
		t.Error("server certificate is a CA")
	}
	if leaf.KeyUsage != x509.KeyUsageDigitalSignature {
		t.Errorf("KeyUsage = %v, want digital signature only", leaf.KeyUsage)
	}
	if want := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}; !slices.Equal(leaf.ExtKeyUsage, want) {
		t.Errorf("ExtKeyUsage = %v, want %v", leaf.ExtKeyUsage, want)
	}
	if leaf.SerialNumber.Sign() <= 0 || leaf.SerialNumber.BitLen() > 128 {
		t.Errorf("serial number = %v, want a positive 128-bit value", leaf.SerialNumber)
	}
	// X.509 timestamps have one-second precision, so the lower bounds allow
	// one second for the fractional part discarded during encoding.
	notBeforeMin := started.Add(-time.Hour - time.Second)
	notBeforeMax := finished.Add(-time.Hour)
	if leaf.NotBefore.Before(notBeforeMin) || leaf.NotBefore.After(notBeforeMax) {
		t.Errorf("NotBefore = %v, outside certificate generation bounds", leaf.NotBefore)
	}
	validFor := 10 * 365 * 24 * time.Hour
	notAfterMin := started.Add(validFor - time.Second)
	notAfterMax := finished.Add(validFor)
	if leaf.NotAfter.Before(notAfterMin) || leaf.NotAfter.After(notAfterMax) {
		t.Errorf("NotAfter = %v, outside ten-year generation bounds", leaf.NotAfter)
	}

	if want := []string{"localhost"}; !slices.Equal(leaf.DNSNames, want) {
		t.Errorf("DNSNames = %v, want %v", leaf.DNSNames, want)
	}
	wantIPs := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	if len(leaf.IPAddresses) != len(wantIPs) {
		t.Errorf("IPAddresses = %v, want %v", leaf.IPAddresses, wantIPs)
	} else {
		for i, want := range wantIPs {
			if !leaf.IPAddresses[i].Equal(want) {
				t.Errorf("IPAddresses[%d] = %v, want %v", i, leaf.IPAddresses[i], want)
			}
		}
	}
	for _, host := range []string{"127.0.0.1", "::1", "localhost"} {
		if err := leaf.VerifyHostname(host); err != nil {
			t.Errorf("VerifyHostname(%q): %v", host, err)
		}
	}
}

func TestSelfSignedServerCertOptions(t *testing.T) {
	t.Parallel()
	hosts := []string{"demo.test", "192.0.2.1"}
	hostOption := h2tls.WithHosts(hosts...)
	hosts[0] = "mutated.test"

	cert, err := h2tls.SelfSignedServerCert(
		h2tls.WithCommonName("client.test"), hostOption)
	if err != nil {
		t.Fatalf("SelfSignedServerCert: %v", err)
	}
	if got := cert.Leaf.Subject.CommonName; got != "client.test" {
		t.Errorf("CommonName = %q, want client.test", got)
	}
	if want := []string{"demo.test"}; !slices.Equal(cert.Leaf.DNSNames, want) {
		t.Errorf("DNSNames = %v, want %v", cert.Leaf.DNSNames, want)
	}
	wantIP := net.ParseIP("192.0.2.1")
	if len(cert.Leaf.IPAddresses) != 1 ||
		!cert.Leaf.IPAddresses[0].Equal(wantIP) {
		t.Errorf("IPAddresses = %v, want [%v]", cert.Leaf.IPAddresses, wantIP)
	}
}

func TestSelfSignedServerCertRejectsEmptyHost(t *testing.T) {
	t.Parallel()
	if _, err := h2tls.SelfSignedServerCert(h2tls.WithHosts("")); err == nil {
		t.Fatal("SelfSignedServerCert accepted an empty host")
	}
}

// mustCert returns a throwaway self-signed certificate or fails the test.
func mustCert(t *testing.T) tls.Certificate {
	t.Helper()
	cert, err := h2tls.SelfSignedServerCert()
	if err != nil {
		t.Fatalf("SelfSignedServerCert: %v", err)
	}
	return cert
}

// serve starts srv in the background. Cleanup closes it and verifies that its
// serving goroutine terminates with the documented shutdown result.
func serve(t *testing.T, ep *h2tls.Endpoint, srv *http.Server) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ep.Listener) }()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("close HTTP server: %v", err)
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve returned %v, want http.ErrServerClosed", err)
		}
	})
}

// serveH2 configures HTTP/2 on an http.Server and starts it with serve.
func serveH2(t *testing.T, ep *h2tls.Endpoint, h http.Handler) {
	t.Helper()
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	if err := http2.ConfigureServer(srv, &http2.Server{}); err != nil {
		t.Fatalf("ConfigureServer: %v", err)
	}
	serve(t, ep, srv)
}

// get issues a GET to the endpoint using tr and returns the status, negotiated
// major version, and body.
func get(t *testing.T, ep *h2tls.Endpoint, tr http.RoundTripper) (status, protoMajor int, body string) {
	t.Helper()
	client := &http.Client{Transport: tr}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+ep.Addr+"/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return resp.StatusCode, resp.ProtoMajor, string(b)
}

// TestListenServesHTTP2 verifies certificate trust, h2 negotiation, and a full
// request round trip.
func TestListenServesHTTP2(t *testing.T) {
	t.Parallel()
	ep, err := h2tls.Listen(mustCert(t))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ep.Listener.Close() })

	serveH2(t, ep, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "proto=%d", r.ProtoMajor) //nolint:gosec // test handler output
	}))

	tr := &http2.Transport{TLSClientConfig: ep.ClientConfig()}
	defer tr.CloseIdleConnections()

	status, proto, body := get(t, ep, tr)
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if proto != 2 {
		t.Errorf("ProtoMajor = %d, want 2", proto)
	}
	if body != "proto=2" {
		t.Errorf("body = %q, want %q", body, "proto=2")
	}
}

func TestListenAddress(t *testing.T) {
	t.Parallel()

	t.Run("default is IPv4 loopback", func(t *testing.T) {
		ep, err := h2tls.Listen(mustCert(t))
		if err != nil {
			t.Fatalf("Listen: %v", err)
		}
		t.Cleanup(func() { _ = ep.Listener.Close() })

		host, _, err := net.SplitHostPort(ep.Addr)
		if err != nil {
			t.Fatalf("SplitHostPort(%q): %v", ep.Addr, err)
		}
		if host != "127.0.0.1" {
			t.Errorf("host = %q, want 127.0.0.1", host)
		}
	})

	t.Run("custom bind error is wrapped", func(t *testing.T) {
		occupied, err := (&net.ListenConfig{}).Listen(
			t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("occupy address: %v", err)
		}
		t.Cleanup(func() { _ = occupied.Close() })

		ep, err := h2tls.Listen(
			mustCert(t), h2tls.WithAddr(occupied.Addr().String()))
		if err == nil {
			_ = ep.Listener.Close()
			t.Fatal("Listen succeeded on an occupied address")
		}
		var opErr *net.OpError
		if !errors.As(err, &opErr) {
			t.Fatalf("error type = %T, want wrapped *net.OpError", err)
		}
		if opErr.Op != "listen" {
			t.Errorf("network operation = %q, want listen", opErr.Op)
		}
	})
}

func TestWithListenerWrapper(t *testing.T) {
	t.Parallel()
	var raw net.Listener
	ep, err := h2tls.Listen(mustCert(t), h2tls.WithListenerWrapper(
		func(listener net.Listener) net.Listener {
			raw = listener
			return listener
		},
	))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ep.Listener.Close() })
	if raw == nil {
		t.Fatal("listener wrapper was not called")
	}
	if ep.Addr != raw.Addr().String() {
		t.Errorf("endpoint address = %q, raw listener = %q",
			ep.Addr, raw.Addr())
	}

	if _, err := h2tls.Listen(
		mustCert(t), h2tls.WithListenerWrapper(nil)); err == nil {
		t.Fatal("Listen accepted a nil listener wrapper")
	}
	if _, err := h2tls.Listen(mustCert(t), h2tls.WithListenerWrapper(
		func(net.Listener) net.Listener { return nil },
	)); err == nil {
		t.Fatal("Listen accepted a wrapper returning nil")
	}
}

// TestWithProtocolsHTTP1 confirms ALPN is controllable: offering only http/1.1
// makes even an h2-capable client fall back to HTTP/1.1.
func TestWithProtocolsHTTP1(t *testing.T) {
	t.Parallel()
	ep, err := h2tls.Listen(mustCert(t), h2tls.WithProtocols("http/1.1"))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ep.Listener.Close() })

	// No ConfigureServer: a plain HTTP/1.1 server over the TLS listener.
	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(w, "proto=%d", r.ProtoMajor) //nolint:gosec // test handler output
		}),
	}
	serve(t, ep, srv)

	tr := &http.Transport{TLSClientConfig: ep.ClientConfig(), ForceAttemptHTTP2: true}
	defer tr.CloseIdleConnections()

	status, proto, body := get(t, ep, tr)
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if proto != 1 {
		t.Errorf("ProtoMajor = %d, want 1", proto)
	}
	if body != "proto=1" {
		t.Errorf("body = %q, want %q", body, "proto=1")
	}
}

// TestH2ListenerServesHTTP1Client checks that a listener
// advertising only "h2" still serves a client that offers only "http/1.1",
// because Go's crypto/tls special-cases the h2->http/1.1 downgrade. Offering
// "h2" alone does not refuse HTTP/1.1.
func TestH2ListenerServesHTTP1Client(t *testing.T) {
	t.Parallel()
	ep, err := h2tls.Listen(mustCert(t)) // default ALPN {"h2"}
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ep.Listener.Close() })

	serveH2(t, ep, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "proto=%d", r.ProtoMajor) //nolint:gosec // test handler output
	}))

	cfg := ep.ClientConfig()
	cfg.NextProtos = []string{"http/1.1"} // client that cannot speak h2
	tr := &http.Transport{TLSClientConfig: cfg}
	defer tr.CloseIdleConnections()

	status, proto, body := get(t, ep, tr)
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if proto != 1 {
		t.Errorf("ProtoMajor = %d, want 1 (h2 listener still serves http/1.1)", proto)
	}
	if body != "proto=1" {
		t.Errorf("body = %q, want %q", body, "proto=1")
	}
}

func TestListenRequiresTLS12(t *testing.T) {
	t.Parallel()
	ep, err := h2tls.Listen(mustCert(t))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ep.Listener.Close() })

	raw, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", ep.Addr)
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	serverDone := make(chan error, 1)
	go func() {
		conn, err := ep.Listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer func() { _ = conn.Close() }()
		serverDone <- conn.(*tls.Conn).HandshakeContext(t.Context())
	}()

	clientConfig := ep.ClientConfig()
	clientConfig.MinVersion = tls.VersionTLS10
	clientConfig.MaxVersion = tls.VersionTLS11
	client := tls.Client(raw, clientConfig)
	clientErr := client.HandshakeContext(t.Context())
	_ = client.Close()
	serverErr := <-serverDone
	if clientErr == nil {
		t.Fatal("TLS 1.1 client completed a handshake")
	}
	if serverErr == nil {
		t.Error("server accepted a TLS 1.1 handshake")
	}
}

func TestListenRejectsInvalidCertificate(t *testing.T) {
	t.Parallel()
	valid, err := h2tls.SelfSignedServerCert()
	if err != nil {
		t.Fatalf("SelfSignedServerCert: %v", err)
	}
	other, err := h2tls.SelfSignedServerCert()
	if err != nil {
		t.Fatalf("other SelfSignedServerCert: %v", err)
	}

	malformed := valid
	malformed.Certificate = [][]byte{{1, 2, 3}}
	missingKey := valid
	missingKey.PrivateKey = nil
	staleLeaf := valid
	staleLeaf.Leaf = other.Leaf
	mismatchedKey := valid
	mismatchedKey.PrivateKey = other.PrivateKey

	for _, tc := range []struct {
		name string
		cert tls.Certificate
	}{
		{name: "empty chain", cert: tls.Certificate{}},
		{name: "malformed leaf", cert: malformed},
		{name: "missing private key", cert: missingKey},
		{name: "stale cached leaf", cert: staleLeaf},
		{name: "mismatched private key", cert: mismatchedKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if ep, err := h2tls.Listen(tc.cert); err == nil {
				_ = ep.Listener.Close()
				t.Error("Listen accepted invalid certificate")
			}
		})
	}
}

func TestWithProtocolsCopiesAndValidates(t *testing.T) {
	t.Parallel()
	cert := mustCert(t)

	t.Run("copies caller input", func(t *testing.T) {
		protocols := []string{"h2"}
		option := h2tls.WithProtocols(protocols...)
		protocols[0] = ""
		ep, err := h2tls.Listen(cert, option)
		if err != nil {
			t.Fatalf("Listen with copied protocol: %v", err)
		}
		_ = ep.Listener.Close()
	})

	t.Run("accepts 255 byte protocol", func(t *testing.T) {
		protocol := strings.Repeat("a", 255)
		ep, err := h2tls.Listen(cert, h2tls.WithProtocols(protocol))
		if err != nil {
			t.Fatalf("Listen: %v", err)
		}
		_ = ep.Listener.Close()
	})

	for _, tc := range []struct {
		name     string
		protocol string
	}{
		{name: "rejects empty protocol", protocol: ""},
		{name: "rejects 256 byte protocol", protocol: strings.Repeat("a", 256)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if ep, err := h2tls.Listen(
				cert, h2tls.WithProtocols(tc.protocol)); err == nil {
				_ = ep.Listener.Close()
				t.Errorf("Listen accepted ALPN protocol of length %d", len(tc.protocol))
			}
		})
	}
}

// TestClientConfigFreshCopies confirms each ClientConfig is independent.
func TestClientConfigFreshCopies(t *testing.T) {
	t.Parallel()
	cert := mustCert(t)
	ep, err := h2tls.Listen(cert)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ep.Listener.Close() })

	a := ep.ClientConfig()
	b := ep.ClientConfig()
	if a == b {
		t.Fatal("ClientConfig returned the same pointer twice")
	}
	if a.MinVersion != tls.VersionTLS12 || b.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = (%d, %d), want TLS 1.2", a.MinVersion, b.MinVersion)
	}
	if len(a.NextProtos) != 0 || len(b.NextProtos) != 0 {
		t.Errorf("NextProtos = (%v, %v), want empty", a.NextProtos, b.NextProtos)
	}
	if a.RootCAs == nil || b.RootCAs == nil {
		t.Fatal("ClientConfig has no RootCAs to trust the endpoint")
	}
	if a.RootCAs == b.RootCAs {
		t.Error("ClientConfig returned a shared RootCAs pool")
	}
	for i, cfg := range []*tls.Config{a, b} {
		opts := x509.VerifyOptions{
			DNSName: "127.0.0.1",
			Roots:   cfg.RootCAs,
		}
		if _, err := cert.Leaf.Verify(opts); err != nil {
			t.Errorf("config %d does not trust endpoint certificate: %v", i, err)
		}
	}

	a.NextProtos = []string{"http/1.1"}
	if len(b.NextProtos) != 0 {
		t.Errorf("mutating one config affected another: b.NextProtos = %v", b.NextProtos)
	}
}
