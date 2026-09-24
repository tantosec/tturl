package tth2

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"
)

// Ping measures one HTTP/2 PING round trip to origin. It may open a TLS
// connection, but it sends no HTTP request and does not consume request-rate or
// batch-interval allowance. A healthy connection is returned to the pool.
//
// The duration includes path latency and peer processing observed by this
// process. It excludes request handling and response service time. An
// intermediary can terminate the PING, so it need not reach the origin server.
//
// origin is a host or host:port. A missing port defaults to 443, as it does for
// request origins. Failed pooled connections are discarded and retried under
// the same rules as [Client.SendBatch]; a failed fresh connection is not
// retried. ctx must be non-nil.
func (t *Transport) Ping(
	ctx context.Context,
	origin string,
) (time.Duration, error) {
	if ctx == nil {
		panic("tth2: nil context")
	}
	origin, err := normaliseAddr(origin)
	if err != nil {
		return 0, fmt.Errorf("tth2: ping origin: %w", err)
	}
	p := t.pool(origin)
	dial := func() (*http2conn, error) { return t.dial(ctx, origin) }
	for {
		conn, reused, epoch, err := p.acquire(ctx, dial)
		if err != nil {
			return 0, err
		}
		var payload [8]byte
		binary.BigEndian.PutUint64(payload[:], t.pingID.Add(1))
		rtt, err := conn.ping(ctx, payload)
		if err == nil {
			p.putIdle(conn, epoch)
			return rtt, nil
		}
		p.discard(conn)
		if reused && ctx.Err() == nil {
			continue
		}
		return 0, err
	}
}

func (c *http2conn) ping(
	ctx context.Context,
	payload [8]byte,
) (time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	waiter, err := c.readPump.registerPing(payload)
	if err != nil {
		return 0, fmt.Errorf("tth2: ping: %w", err)
	}
	defer c.readPump.cancelPing(payload)
	start := time.Now()
	if err := c.writeControl("PING", func() error {
		return c.framer.WritePing(false, payload)
	}); err != nil {
		return 0, err
	}
	select {
	case result := <-waiter:
		if result.err != nil {
			return 0, fmt.Errorf("tth2: ping: %w", result.err)
		}
		return result.at.Sub(start), nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
