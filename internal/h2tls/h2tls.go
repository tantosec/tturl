// Package h2tls provides TLS endpoints intended to carry HTTP/2. It creates
// ephemeral self-signed certificates, listeners that advertise h2 by default,
// matching client configurations, and optional TLS key logging. It does not
// configure or run an HTTP/2 server.
package h2tls

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"
)

const defaultCommonName = "h2tls"

type certificateConfig struct {
	commonName string
	hosts      []string
}

// CertificateOption configures a certificate created by
// [SelfSignedServerCert].
type CertificateOption func(*certificateConfig)

// WithCommonName sets the certificate subject's common name. Hostname
// verification uses the certificate's SANs, configured separately by
// [WithHosts].
func WithCommonName(name string) CertificateOption {
	return func(c *certificateConfig) { c.commonName = name }
}

// WithHosts replaces the DNS names and IP addresses the certificate verifies
// for. IP literals become IP SANs; all other values become DNS SANs. The
// default is 127.0.0.1, ::1, and localhost.
func WithHosts(hosts ...string) CertificateOption {
	names := append([]string(nil), hosts...)
	return func(c *certificateConfig) {
		c.hosts = append(c.hosts[:0], names...)
	}
}

// SelfSignedServerCert generates an ephemeral self-signed P-256 server
// certificate valid for ten years. It defaults to the common name "h2tls" and
// the loopback identities 127.0.0.1, ::1, and localhost. The returned
// certificate has its Leaf field populated.
func SelfSignedServerCert(opts ...CertificateOption) (tls.Certificate, error) {
	cfg := certificateConfig{
		commonName: defaultCommonName,
		hosts:      []string{"127.0.0.1", "::1", "localhost"},
	}
	for _, o := range opts {
		o(&cfg)
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("h2tls: generate key: %w", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	var serial *big.Int
	for serial == nil || serial.Sign() == 0 {
		serial, err = rand.Int(rand.Reader, serialLimit)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("h2tls: serial: %w", err)
		}
	}

	var dnsNames []string
	var ipAddresses []net.IP
	for _, host := range cfg.hosts {
		if host == "" {
			return tls.Certificate{}, fmt.Errorf("h2tls: certificate host is empty")
		}
		if ip := net.ParseIP(host); ip != nil {
			ipAddresses = append(ipAddresses, ip)
			continue
		}
		dnsNames = append(dnsNames, host)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cfg.commonName},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           ipAddresses,
		DNSNames:              dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("h2tls: create certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("h2tls: parse certificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: leaf}, nil
}

// Endpoint is a TLS listener preconfigured for HTTP/2 ALPN, together with the
// material a client needs to reach it. It says nothing about what serves on the
// connection: an http.Server, a raw framer loop, or a bare accept-and-close all
// build on the same Endpoint.
//
// The caller owns the Listener's lifetime. Closing it (directly, or via an
// http.Server that Serves on it) shuts the Endpoint down.
type Endpoint struct {
	// Listener is the TLS listener with ALPN configured.
	Listener net.Listener
	// Addr is Listener.Addr().String().
	Addr        string
	trustAnchor *x509.Certificate
}

type config struct {
	addr            string
	protocols       []string
	listenerWrapper func(net.Listener) net.Listener
	wrapperSet      bool
}

// Option configures Listen.
type Option func(*config)

// WithAddr sets the bind address, a "host:port" string as taken by [net.Listen]
// (default "127.0.0.1:0", a free loopback port). Callers assembling it from a
// separate host and port should use [net.JoinHostPort], not fmt.Sprintf, so an
// IPv6 host is bracketed correctly.
func WithAddr(addr string) Option {
	return func(c *config) { c.addr = addr }
}

// WithProtocols sets the ALPN protocols the listener offers, replacing the
// default of {"h2"}. This controls advertisement, not application-layer
// protocol enforcement.
func WithProtocols(protos ...string) Option {
	protocols := append([]string(nil), protos...)
	return func(c *config) {
		c.protocols = append(c.protocols[:0], protocols...)
	}
}

// WithListenerWrapper applies wrap between the listener that configures
// accepted TCP connections and the TLS listener. It supports transport
// instrumentation such as fault or latency injection that must also affect the
// TLS handshake. wrap must return a listener with the same externally
// reachable address.
func WithListenerWrapper(
	wrap func(net.Listener) net.Listener,
) Option {
	return func(c *config) {
		c.listenerWrapper = wrap
		c.wrapperSet = true
	}
}

// Listen opens a TLS listener bearing cert that negotiates HTTP/2 by default
// and returns its Endpoint. The first certificate in cert becomes the trust
// anchor returned through [Endpoint.ClientConfig]. The caller owns the
// listener and must close it. Listen returns an error for invalid certificate
// material, ALPN protocols, listener wrappers, or bind addresses.
func Listen(cert tls.Certificate, opts ...Option) (*Endpoint, error) {
	cfg := config{
		addr:      "127.0.0.1:0",
		protocols: []string{"h2"},
	}
	for _, o := range opts {
		o(&cfg)
	}
	for _, protocol := range cfg.protocols {
		if len(protocol) == 0 || len(protocol) > 255 {
			return nil, fmt.Errorf(
				"h2tls: invalid ALPN protocol length %d", len(protocol))
		}
	}
	if cfg.wrapperSet && cfg.listenerWrapper == nil {
		return nil, fmt.Errorf("h2tls: listener wrapper is nil")
	}
	if !cfg.wrapperSet {
		cfg.listenerWrapper = func(ln net.Listener) net.Listener { return ln }
	}

	leaf, err := validatedLeaf(cert)
	if err != nil {
		return nil, err
	}
	cert.Leaf = leaf

	raw, err := (&net.ListenConfig{}).Listen(
		context.Background(), "tcp", cfg.addr)
	if err != nil {
		return nil, fmt.Errorf("h2tls: listen on %q: %w", cfg.addr, err)
	}
	// Go already defaults accepted TCP connections to TCP_NODELAY. Configure it
	// explicitly to make this property clear at the raw transport boundary.
	tcpListener := &noDelayListener{Listener: raw}
	wrapped := cfg.listenerWrapper(tcpListener)
	if wrapped == nil {
		_ = raw.Close()
		return nil, fmt.Errorf("h2tls: listener wrapper returned nil")
	}
	ln := tls.NewListener(wrapped, &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   cfg.protocols,
		MinVersion:   tls.VersionTLS12,
	})

	return &Endpoint{
		Listener:    ln,
		Addr:        ln.Addr().String(),
		trustAnchor: leaf,
	}, nil
}

type noDelayListener struct {
	net.Listener
}

func (l *noDelayListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		return nil, closeAcceptedConn(conn, fmt.Errorf(
			"h2tls: accepted connection has type %T, want *net.TCPConn", conn))
	}
	if err := tcpConn.SetNoDelay(true); err != nil {
		return nil, closeAcceptedConn(conn, fmt.Errorf(
			"h2tls: enable TCP_NODELAY: %w", err))
	}
	return tcpConn, nil
}

func closeAcceptedConn(conn net.Conn, acceptErr error) error {
	if err := conn.Close(); err != nil {
		return errors.Join(acceptErr, fmt.Errorf(
			"h2tls: close rejected connection: %w", err))
	}
	return acceptErr
}

// ClientConfig returns a fresh *tls.Config that trusts this endpoint's server
// certificate as its sole root, with TLS 1.2 as its minimum. It sets no
// NextProtos: ALPN is the client's to choose. A new value is returned on each
// call, so callers may mutate it freely.
func (e *Endpoint) ClientConfig() *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(e.trustAnchor)
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

// validatedLeaf verifies the certificate material needed both by the listener
// and by Endpoint.ClientConfig. It derives the trust anchor from the DER the
// server will present rather than trusting a possibly stale cached Leaf.
func validatedLeaf(cert tls.Certificate) (*x509.Certificate, error) {
	if len(cert.Certificate) == 0 {
		return nil, fmt.Errorf("h2tls: certificate chain is empty")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("h2tls: parse server certificate: %w", err)
	}
	if cert.Leaf != nil && !bytes.Equal(cert.Leaf.Raw, leaf.Raw) {
		return nil, fmt.Errorf(
			"h2tls: cached certificate leaf does not match certificate chain")
	}
	signer, ok := cert.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("h2tls: certificate private key is missing or unsupported")
	}
	publicDER, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil {
		return nil, fmt.Errorf("h2tls: marshal private key public key: %w", err)
	}
	leafPublicDER, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("h2tls: marshal certificate public key: %w", err)
	}
	if !bytes.Equal(publicDER, leafPublicDER) {
		return nil, fmt.Errorf(
			"h2tls: certificate private key does not match certificate public key")
	}
	return leaf, nil
}
