package h2tls

import (
	"net"
	"strings"
	"testing"
)

type singleConnListener struct {
	conn net.Conn
}

func (l *singleConnListener) Accept() (net.Conn, error) { return l.conn, nil }
func (l *singleConnListener) Close() error              { return nil }
func (l *singleConnListener) Addr() net.Addr            { return testAddr("listener") }

type testAddr string

func (a testAddr) Network() string { return string(a) }
func (a testAddr) String() string  { return string(a) }

type closeTrackingConn struct {
	net.Conn
	closed bool
}

func (c *closeTrackingConn) Close() error {
	c.closed = true
	return c.Conn.Close()
}

func TestNoDelayListenerRejectsNonTCPConnection(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = client.Close() }()
	tracked := &closeTrackingConn{Conn: server}
	listener := &noDelayListener{Listener: &singleConnListener{conn: tracked}}

	conn, err := listener.Accept()
	if conn != nil {
		_ = conn.Close()
		t.Fatalf("Accept connection = %T, want nil", conn)
	}
	if err == nil || !strings.Contains(err.Error(), "want *net.TCPConn") {
		t.Fatalf("Accept error = %v, want unexpected connection type", err)
	}
	if !tracked.closed {
		t.Error("rejected connection was not closed")
	}
}

func TestNoDelayListenerReportsConfigurationFailure(t *testing.T) {
	server, client := connectedTCPPair(t)
	defer func() { _ = client.Close() }()
	if err := server.Close(); err != nil {
		t.Fatalf("close server connection: %v", err)
	}
	listener := &noDelayListener{Listener: &singleConnListener{conn: server}}

	conn, err := listener.Accept()
	if conn != nil {
		_ = conn.Close()
		t.Fatalf("Accept connection = %T, want nil", conn)
	}
	if err == nil || !strings.Contains(err.Error(), "enable TCP_NODELAY") {
		t.Fatalf("Accept error = %v, want TCP_NODELAY configuration failure", err)
	}
}

func connectedTCPPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenTCP: %v", err)
	}
	defer func() { _ = listener.Close() }()

	clientResult := make(chan struct {
		conn *net.TCPConn
		err  error
	}, 1)
	go func() {
		conn, err := net.DialTCP("tcp4", nil, listener.Addr().(*net.TCPAddr))
		clientResult <- struct {
			conn *net.TCPConn
			err  error
		}{conn: conn, err: err}
	}()
	server, err := listener.AcceptTCP()
	if err != nil {
		t.Fatalf("AcceptTCP: %v", err)
	}
	client := <-clientResult
	if client.err != nil {
		_ = server.Close()
		t.Fatalf("DialTCP: %v", client.err)
	}
	return server, client.conn
}
