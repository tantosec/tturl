package tth2

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"sync"
)

var (
	// ErrLeaseClosed reports use of a connection lease after it was released or
	// deliberately discarded, including a nil or zero lease. Inspect with
	// errors.Is.
	ErrLeaseClosed = errors.New("tth2: connection lease is closed")
	// ErrLeaseInvalid reports use of a lease whose connection was lost during a
	// send. A lease never replaces that connection transparently. Inspect with
	// errors.Is.
	ErrLeaseInvalid = errors.New("tth2: connection lease is invalid")
)

type leaseState uint8

const (
	leaseClosed leaseState = iota
	leaseActive
	leaseInvalid
)

// ConnLease exclusively owns one physical HTTP/2 connection. Batches sent
// through it never migrate to another connection. Close returns a healthy
// connection to the pool; Discard closes it instead. Both are idempotent.
//
// ConnLease serialises its methods. A connection-fatal send, configured
// response-body limit, or configured batch timeout invalidates it, while a
// response-phase [StreamError] leaves it active. A dispatch-phase reset
// invalidates the lease because siblings may be only partly sent. A lease
// consumes one [Transport.MaxConnsPerHost] slot until it is closed, discarded,
// or invalidated. The zero value and a nil lease are closed; acquire a lease
// with [Client.Acquire]. Do not copy a lease. Methods wait for a preceding
// method to finish, so cancelling a queued SendBatch does not interrupt the
// active one.
type ConnLease struct {
	mu sync.Mutex

	owner  connOwner
	origin string
	pace   paceLease
	id     ConnectionID
	state  leaseState
}

// Acquire obtains an exclusive connection for origin, which is a host or
// host:port. A missing port defaults to 443. A stale pooled connection is
// discarded before Acquire returns, so the returned ID will not change.
//
// Acquisition observes the transport's per-origin connection limit and blocks
// until a slot is available or ctx is cancelled. ctx must be non-nil. The
// caller must eventually call [ConnLease.Close] or [ConnLease.Discard].
func (c *Client) Acquire(ctx context.Context, origin string) (*ConnLease, error) {
	if ctx == nil {
		panic("tth2: nil context")
	}
	origin, err := normaliseAddr(origin)
	if err != nil {
		return nil, fmt.Errorf("tth2: acquire origin: %w", err)
	}
	transport := c.transport()
	pool := transport.pool(origin)
	dial := func() (*http2conn, error) { return transport.dial(ctx, origin) }
	for {
		conn, reused, epoch, err := pool.acquire(ctx, dial)
		if err != nil {
			return nil, err
		}
		if reused {
			var payload [8]byte
			binary.BigEndian.PutUint64(payload[:], transport.pingID.Add(1))
			if _, err := conn.ping(ctx, payload); err != nil {
				pool.discard(conn)
				if ctx.Err() != nil {
					return nil, err
				}
				continue
			}
		}
		return &ConnLease{
			owner: connOwner{
				pool: pool, dial: dial, conn: conn, epoch: epoch,
				held: true, reused: reused,
			},
			origin: origin,
			pace:   pool.leasePace(paceConfig{}), id: conn.id, state: leaseActive,
		}, nil
	}
}

// ID returns the connection's transport-lifetime identity. It remains available
// after the lease closes or becomes invalid. A nil or zero lease returns zero.
func (l *ConnLease) ID() ConnectionID {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.id
}

// Active reports whether the lease still owns a connection that has not been
// released, discarded, or observed to fail. A nil or zero lease is inactive. It
// does not check the live connection: idle peer failure is observed by a later
// send.
func (l *ConnLease) Active() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state == leaseActive
}

// SendBatch releases reqs as one batch on the leased connection. The batch must
// target the lease origin. Its validation, options, and partial-result contract
// match [Client.SendBatch].
func (l *ConnLease) SendBatch(
	ctx context.Context,
	reqs []*http.Request,
	opts ...SendOption,
) (*BatchResult, error) {
	if ctx == nil {
		panic("tth2: nil context")
	}
	if l == nil {
		return nil, ErrLeaseClosed
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	switch l.state {
	case leaseClosed:
		return nil, ErrLeaseClosed
	case leaseInvalid:
		return nil, ErrLeaseInvalid
	}
	origin, toSend, cfg, err := prepareBatch(reqs, opts)
	if err != nil {
		return nil, err
	}
	if origin != l.origin {
		return nil, fmt.Errorf(
			"tth2: batch origin %q differs from lease origin %q", origin, l.origin)
	}
	batch, _, _, err := l.owner.send(
		ctx, toSend, cfg, l.pace.beforeFirstFlush(ctx, len(toSend)))
	if l.owner.held {
		return batch, err
	}
	l.state = leaseInvalid
	l.pace.close()
	return batch, err
}

// Close releases a healthy connection to the pool. It is safe to call more than
// once and after invalidation or Discard.
func (l *ConnLease) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state != leaseActive {
		return nil
	}
	l.owner.release()
	l.state = leaseClosed
	l.pace.close()
	return nil
}

// Discard closes the owned connection instead of returning it to the pool. Use
// it when connection priming or remote state makes reuse undesirable. It is
// safe to call more than once and after invalidation or Close.
func (l *ConnLease) Discard() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state != leaseActive {
		return
	}
	l.owner.discard()
	l.state = leaseClosed
	l.pace.close()
}
