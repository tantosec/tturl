package timing

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"time"
)

// acquireSocket uses one budget for dialling, TLS and connection setup. Its
// temporary cancellation watcher ends before the leased socket is returned.
// A nil TLS configuration selects plain TCP; zero timeout means unlimited.
func acquireSocket(
	parent context.Context, address string, tlsConfig *tls.Config,
	timeout time.Duration, setup func(context.Context, net.Conn) error,
) (net.Conn, error) {
	if timeout < 0 {
		return nil, errors.New("negative acquisition timeout")
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, timeout)
	} else {
		ctx, cancel = context.WithCancel(parent)
	}
	defer cancel()
	conn, err := new(net.Dialer).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		if err := tcp.SetNoDelay(true); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	raw := conn
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	fail := func(err error) (net.Conn, error) {
		stop()
		_ = conn.Close()
		if cause := context.Cause(ctx); cause != nil {
			err = cause
		}
		return nil, err
	}
	if tlsConfig != nil {
		config := tlsConfig.Clone()
		config.DynamicRecordSizingDisabled = true
		if config.ServerName == "" {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fail(err)
			}
			config.ServerName = host
		}
		secure := tls.Client(conn, config)
		conn = secure
		if err := secure.HandshakeContext(ctx); err != nil {
			return fail(err)
		}
	}
	if setup != nil {
		if err := setup(ctx, conn); err != nil {
			return fail(err)
		}
	}
	if !stop() || ctx.Err() != nil {
		return fail(context.Cause(ctx))
	}
	return conn, nil
}
