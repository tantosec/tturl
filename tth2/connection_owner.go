package tth2

import (
	"context"
	"net/http"
)

// connOwner is the shared checked-out-connection lifecycle used by trial
// workers and public leases. It is confined to its caller's synchronisation.
type connOwner struct {
	pool *originPool
	dial func() (*http2conn, error)

	conn   *http2conn
	epoch  uint64
	held   bool
	reused bool
}

// acquire takes a connection unless the owner already holds one. acquired
// reports a transition from unheld to held, irrespective of pool reuse.
func (o *connOwner) acquire(ctx context.Context) (acquired bool, err error) {
	if o.held {
		return false, nil
	}
	conn, reused, epoch, err := o.pool.acquire(ctx, o.dial)
	if err != nil {
		return false, err
	}
	o.conn, o.reused, o.held = conn, reused, true
	o.epoch = epoch
	return true, nil
}

// send runs a batch without replacing the held connection. stale reports only
// a pooled connection that failed before proving itself live. Local termination
// is never stale: the trial driver may use a replacement for later work, but it
// must not repeat the failed batch.
func (o *connOwner) send(
	ctx context.Context,
	reqs []*http.Request,
	cfg sendConfig,
	beforeFirstFlush func() error,
) (batch *BatchResult, dispatch DispatchTimes, stale bool, err error) {
	batch, dispatch, keepConn, err := o.conn.attempt(
		ctx, reqs, cfg, beforeFirstFlush,
	)
	if err == nil {
		o.reused = false
		return batch, dispatch, false, nil
	}
	if keepConn {
		o.reused = false
		return batch, dispatch, false, err
	}
	untried := o.reused && !isLocalBatchTermination(err)
	o.pool.discard(o.conn)
	o.conn, o.held, o.reused = nil, false, false
	if ctx.Err() != nil {
		return nil, dispatch, false, err
	}
	return nil, dispatch, untried, err
}

// release returns a live held connection to the pool.
func (o *connOwner) release() {
	if o.held {
		o.pool.putIdle(o.conn, o.epoch)
		o.conn, o.held, o.reused = nil, false, false
	}
}

// discard closes a held connection and frees its pool slot.
func (o *connOwner) discard() {
	if o.held {
		o.pool.discard(o.conn)
		o.conn, o.held, o.reused = nil, false, false
	}
}
