package tth2

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Transport pools TLS HTTP/2 connections by origin. Its zero value uses secure
// TLS defaults. A Transport is safe for concurrent use and may serve multiple
// origins.
//
// Configure fields before first use and do not mutate them concurrently with
// calls. Configuration changes after an origin's first dial may not affect its
// existing pool. Do not copy a Transport after first use.
//
// Call [Transport.CloseIdleConnections] after in-flight calls finish to close
// every connection. The Transport remains usable afterwards.
type Transport struct {
	// TLSClientConfig customises TLS. nil uses secure defaults.
	//
	// The Transport clones it per dial, adds "h2" to NextProtos when absent,
	// and forces DynamicRecordSizingDisabled so a load-bearing flush is not
	// split across dynamically sized records. The config and referenced
	// certificate pools must remain immutable while calls can dial; cloning
	// does not transfer ownership of those pools.
	TLSClientConfig *tls.Config

	// MaxResponseHeaderBytes bounds each decoded response header or trailer
	// section. The HTTP/2 header-list size is the decoded name and value bytes
	// plus 32 bytes per field. The decoder enforces the limit whether or not
	// the peer honours its advertised value and whether or not header capture
	// is enabled. Zero uses 1 MiB. [UnlimitedResponseBytes] omits the
	// advertised policy limit and uses the decoder's representational ceiling.
	// Values below -1 or above math.MaxUint32 make connection setup fail.
	MaxResponseHeaderBytes int64

	// DialContext, if non-nil, establishes the underlying transport connection.
	// tth2 wraps the returned [net.Conn] with TLS and HTTP/2, so the dialer is
	// responsible only for the underlying transport. nil is equivalent to
	// (&net.Dialer{}).DialContext.
	//
	// This mirrors [http.Transport.DialContext]. tth2 must own the TLS record
	// layer, so it does not accept a caller-supplied TLS connection.
	//
	// When provided, tth2 sets TCP_NODELAY only if the returned conn is a
	// [*net.TCPConn]; for any other conn type the dialer is presumed to have
	// applied whatever transport-level tuning its conn needs.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)

	// MaxConnsPerHost bounds the total number of connections (dialling, active,
	// and idle) the Transport keeps per origin. A call that would exceed the
	// limit blocks until a connection is returned.
	//
	// Values below one mean one, unlike [http.Transport.MaxConnsPerHost], where
	// zero is unlimited.
	MaxConnsPerHost int

	// IdleConnTimeout is how long a pooled connection may sit idle before it is
	// closed and evicted. Values at or below zero mean no limit.
	//
	// The pool reuses the oldest live idle connection first so sequential work
	// rotates through available capacity instead of leaving an older spare
	// cold.
	//
	// Eviction is lazy: an expired connection is closed when an acquiring call
	// next inspects the origin's pool, or by CloseIdleConnections — not by a
	// background timer. The send path retries a stale idle connection
	// regardless of this setting.
	IdleConnTimeout time.Duration

	// MaxRequestsPerSecondPerHost caps the average rate, in requests per
	// second, at which the Transport sends to one origin across all concurrent
	// calls. Values at or below zero mean no limit. Non-finite values make
	// connection setup fail.
	//
	// The limit gates batch starts and does not space requests within a batch.
	// A run may narrow it with [WithMaxRequestsPerSecond].
	MaxRequestsPerSecondPerHost float64

	// MinBatchIntervalPerHost is the minimum interval between starts of
	// batches sent to one origin, jointly across every concurrent
	// call sharing the Transport. Values at or below zero mean no limit.
	//
	// A run may choose a longer interval with [WithMinBatchInterval] but cannot
	// weaken this one. The first batch is eligible immediately, and idle time
	// does not accumulate credit.
	//
	// This controls client-side starts, not arrival spacing at the target.
	MinBatchIntervalPerHost time.Duration

	mu     sync.Mutex
	pools  map[string]*originPool
	pingID atomic.Uint64
	connID atomic.Uint64
}

// DefaultTransport is the Transport used by a [Client] whose Transport is nil.
// It dials with secure TLS defaults. The pool and pacing state are shared
// across all such clients. Use an explicit Transport to own configuration and
// cleanup independently. Do not replace or mutate DefaultTransport while any
// client may use it.
var DefaultTransport = &Transport{}

// originPool manages one origin's connection limit, idle connections, pacing,
// and CloseIdleConnections epoch. A token is held while a connection is active
// or being dialled; idle connections hold no token.
type originPool struct {
	tokens      chan struct{}
	idleTimeout time.Duration
	paceMu      sync.Mutex
	pacer       *paceScheduler
	paceConfig  paceConfig
	mu          sync.Mutex
	idle        []idleConn
	epoch       uint64
}

// idleConn is a pooled connection tagged with the time it was returned to the
// pool. Because putIdle stamps with a monotonically advancing clock, idle is
// ordered oldest-first, so [originPool.takeIdle] evicts an expired leading
// prefix.
type idleConn struct {
	conn  *http2conn
	since time.Time
}

// maxConns resolves MaxConnsPerHost, mapping the 0 default to 1.
func (t *Transport) maxConns() int {
	if t.MaxConnsPerHost > 0 {
		return t.MaxConnsPerHost
	}
	return 1
}

// pool returns the origin's connection pool, creating it on first use. The
// token semaphore is sized to the resolved MaxConnsPerHost and pre-filled.
func (t *Transport) pool(origin string) *originPool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pools == nil {
		t.pools = make(map[string]*originPool)
	}
	p := t.pools[origin]
	if p == nil {
		n := t.maxConns()
		p = &originPool{
			tokens:      make(chan struct{}, n),
			idleTimeout: t.IdleConnTimeout,
			paceConfig: paceConfig{
				requestsPerSecond: t.MaxRequestsPerSecondPerHost,
				minBatchInterval:  t.MinBatchIntervalPerHost,
			},
		}
		for range n {
			p.tokens <- struct{}{}
		}
		t.pools[origin] = p
	}
	return p
}

// acquire takes a connection slot, blocking until one is free when all are in
// use — then reuses an idle connection or dials a fresh one via dial. reused
// reports whether the connection came from the pool (and so might be stale). On
// any error the slot is released and no connection is returned.
func (p *originPool) acquire(
	ctx context.Context,
	dial func() (*http2conn, error),
) (conn *http2conn, reused bool, epoch uint64, err error) {
	if err := ctx.Err(); err != nil {
		return nil, false, 0, err // fast-fail before taking a slot
	}
	select {
	case <-p.tokens:
	case <-ctx.Done():
		return nil, false, 0, ctx.Err()
	}

	expired, conn, epoch := p.takeIdle()
	for _, c := range expired {
		c.close() // idle connections hold no token, so eviction is pure cleanup
	}
	if conn != nil {
		return conn, true, epoch, nil
	}

	conn, err = dial()
	if err != nil {
		p.tokens <- struct{}{} // release the slot; no connection was created
		return nil, false, 0, err
	}
	return conn, false, epoch, nil
}

// takeIdle removes and returns the oldest live idle connection to reuse (nil if
// none), plus any connections idle longer than idleTimeout for the caller to
// close outside the lock. idle is ordered oldest-first, so the expired ones are
// a leading prefix and the next connection preserves FIFO reuse.
func (p *originPool) takeIdle() (
	expired []*http2conn,
	reuse *http2conn,
	epoch uint64,
) {
	p.mu.Lock()
	defer p.mu.Unlock()
	epoch = p.epoch
	if p.idleTimeout > 0 && len(p.idle) > 0 {
		cutoff := time.Now().Add(-p.idleTimeout)
		k := 0
		for k < len(p.idle) && p.idle[k].since.Before(cutoff) {
			k++
		}
		if k > 0 {
			expired = make([]*http2conn, k)
			for i := range k {
				expired[i] = p.idle[i].conn
			}
			p.idle = slices.Delete(p.idle, 0, k)
		}
	}
	if len(p.idle) > 0 {
		reuse = p.idle[0].conn
		p.idle[0] = idleConn{}
		p.idle = slices.Delete(p.idle, 0, 1)
	}
	return expired, reuse, epoch
}

// putIdle returns a healthy connection to the pool for reuse.
//
// The append MUST precede the token release: a goroutine woken by the release
// then finds this connection in the idle set and reuses it rather than dialling
// a new one. Releasing first would let the woken goroutine see an empty idle
// set and dial, transiently creating a connection beyond MaxConnsPerHost. At a
// limit of 1, that would create a second connection and break the bound.
func (p *originPool) putIdle(conn *http2conn, epoch uint64) {
	ic := idleConn{conn: conn, since: time.Now()}
	p.mu.Lock()
	if epoch != p.epoch {
		p.mu.Unlock()
		conn.close()
		p.tokens <- struct{}{}
		return
	}
	p.idle = append(p.idle, ic)
	p.mu.Unlock()
	p.tokens <- struct{}{}
}

// discard closes a dead connection and frees its slot.
func (p *originPool) discard(conn *http2conn) {
	conn.close()
	p.tokens <- struct{}{}
}

// dialer returns the transport dialer, defaulting to a net.Dialer.
func (t *Transport) dialer() func(ctx context.Context, network, addr string) (net.Conn, error) {
	if t.DialContext != nil {
		return t.DialContext
	}
	return (&net.Dialer{}).DialContext
}

// amendedTLS returns a TLS config ready for an HTTP/2 dial: cloned from
// TLSClientConfig (or a fresh zero value if nil), with "h2" prepended to
// NextProtos if absent and DynamicRecordSizingDisabled forced on. See
// [Transport.TLSClientConfig] for the record-sizing rationale.
func (t *Transport) amendedTLS() *tls.Config {
	var cfg *tls.Config
	if t.TLSClientConfig != nil {
		cfg = t.TLSClientConfig.Clone()
	} else {
		cfg = &tls.Config{}
	}
	if !slices.Contains(cfg.NextProtos, "h2") {
		cfg.NextProtos = append([]string{"h2"}, cfg.NextProtos...)
	}
	cfg.DynamicRecordSizingDisabled = true
	return cfg
}

// dial establishes a fresh connection to origin.
func (t *Transport) dial(ctx context.Context, origin string) (*http2conn, error) {
	if math.IsNaN(t.MaxRequestsPerSecondPerHost) || math.IsInf(t.MaxRequestsPerSecondPerHost, 0) {
		return nil, errors.New("tth2: MaxRequestsPerSecondPerHost must be finite")
	}
	headerBytes, advertise, err := t.responseHeaderBytes()
	if err != nil {
		return nil, err
	}
	inner, err := dialHTTP2(
		ctx, origin, t.amendedTLS(), t.dialer(), headerBytes, advertise)
	if err != nil {
		return nil, &ConnectionError{Err: fmt.Errorf("tth2: %w", err)}
	}
	inner.id = ConnectionID(t.connID.Add(1))
	return inner, nil
}

func (t *Transport) responseHeaderBytes() (uint32, bool, error) {
	switch {
	case t.MaxResponseHeaderBytes == UnlimitedResponseBytes:
		return math.MaxUint32, false, nil
	case t.MaxResponseHeaderBytes < UnlimitedResponseBytes:
		return 0, false, fmt.Errorf(
			"tth2: MaxResponseHeaderBytes must be >= -1")
	case t.MaxResponseHeaderBytes == 0:
		return 1 << 20, true, nil
	case t.MaxResponseHeaderBytes > math.MaxUint32:
		return 0, false, fmt.Errorf(
			"tth2: MaxResponseHeaderBytes exceeds %d", uint64(math.MaxUint32))
	default:
		return uint32(t.MaxResponseHeaderBytes), true, nil
	}
}

// CloseIdleConnections closes every idle connection and marks active
// connections to close when returned. It does not interrupt in-flight calls and
// leaves the Transport usable. Repeated calls are safe.
func (t *Transport) CloseIdleConnections() {
	t.mu.Lock()
	pools := make([]*originPool, 0, len(t.pools))
	for _, p := range t.pools {
		pools = append(pools, p)
	}
	t.mu.Unlock()
	for _, p := range pools {
		p.mu.Lock()
		idle := p.idle
		p.idle = nil
		p.epoch++
		p.mu.Unlock()
		for _, ic := range idle {
			ic.conn.close()
		}
	}
}

// attempt runs one batch over conn and classifies the outcome for
// the pool. keepConn reports whether the connection survived and may be reused.
// The rule is the same during sending and response collection: a StreamError is
// stream-scoped, so the connection survives; anything else — a context
// cancellation, configured local termination, or any other failure — is
// connection-fatal. Dispatch contains any successfully flushed diagnostic
// milestones, including on a later error.
//
//   - success: the batch result, keepConn true, nil error.
//   - StreamError in the read phase: keepConn true, the error returned, and
//     with it the partial batch — every stream's result, the reset ones
//     carrying [Result.Reset].
//   - StreamError while sending: keepConn false, error returned, no results.
//     The reset is stream-scoped, but abandoning its partially sent siblings
//     leaves the connection unsuitable for another batch.
//   - context cancellation, local termination, or any other failure:
//     keepConn false. A cancelled send leaves half-transmitted streams on the
//     connection, and a cancelled read closes the connection to unblock the
//     dispatcher — neither is reusable.
//
// Send-phase errors are wrapped "tth2: %w" except for typed local termination;
// read-phase errors (StreamError or connection-fatal) are returned as-is,
// already carrying package context.
func (c *http2conn) attempt(
	ctx context.Context,
	reqs []*http.Request,
	cfg sendConfig,
	beforeFirstFlush func() error,
) (batch *BatchResult, dispatch DispatchTimes, keepConn bool, err error) {
	if err := c.readPump.readError(); err != nil {
		return nil, DispatchTimes{}, false, err
	}
	if err := c.checkPeerStreamLimit(
		len(reqs), cfg.peerStreamLimitIgnored,
	); err != nil {
		return nil, DispatchTimes{}, true, err
	}
	streamIDs, err := c.allocateStreamIDs(len(reqs))
	if err != nil {
		return nil, DispatchTimes{}, false, fmt.Errorf("tth2: %w", err)
	}
	attemptCtx := ctx
	cancelAttempt := func(error) {}
	var timeout *time.Timer
	if cfg.batchTimeout > 0 {
		attemptCtx, cancelAttempt = context.WithCancelCause(ctx)
	}
	defer func() {
		if timeout != nil {
			timeout.Stop()
		}
		cancelAttempt(nil)
	}()
	startTransport := func() error {
		if beforeFirstFlush != nil {
			if err := beforeFirstFlush(); err != nil {
				return err
			}
		}
		if cfg.batchTimeout > 0 {
			timeout = time.AfterFunc(cfg.batchTimeout, func() {
				cancelAttempt(&BatchTimeoutError{Limit: cfg.batchTimeout})
			})
		}
		if attemptCtx.Err() != nil {
			return batchContextError(attemptCtx)
		}
		return nil
	}
	responses, err := c.newResponseRead(attemptCtx, streamIDs, cfg)
	if err != nil {
		return nil, DispatchTimes{}, false, err
	}
	defer c.readPump.retire(responses)
	dispatch, err = c.sendRequests(
		attemptCtx, reqs, streamIDs, cfg.releaseDelay, cfg.bodyBytesWithheld,
		startTransport, responses,
	)
	if err != nil {
		responses.abort(err)
		if isLocalBatchTermination(err) {
			return nil, dispatch, false, err
		}
		return nil, dispatch, false, fmt.Errorf("tth2: %w", err)
	}
	results, err := c.readResponses(responses)
	if err != nil {
		if _, ok := errors.AsType[*StreamError](err); ok {
			// The reset took one stream, so the batch is partial rather than
			// absent: its siblings raced to completion and their ranks stand.
			batch = newBatchResult(results)
			batch.Connection = c.id
			return batch, dispatch, true, err
		}
		return nil, dispatch, false, err
	}
	batch = newBatchResult(results)
	batch.Connection = c.id
	return batch, dispatch, true, nil
}

// sendBatch releases reqs as one batch over a connection to origin, returning
// the [BatchResult], with one [Result] per request in input order and the batch
// facts. It is the fault-tolerant engine.
//
// StreamErrors and context errors propagate immediately — StreamErrors are
// stream-scoped (the connection survives them and is returned to the pool) and
// context errors must not be retried. A configured body limit or batch timeout
// is also never retried. Any connection-fatal failure discards the connection;
// the attempt is retried on a fresh connection only when the failed one was
// reused from the pool and may simply have gone stale while idle. A freshly
// dialled connection that fails is not retried, matching net/http's isReused
// rule. Because only healthy connections are ever pooled, the reused
// connections drain within a bounded number of iterations, so the loop needs no
// attempt counter.
func (t *Transport) sendBatch(
	ctx context.Context,
	origin string,
	reqs []*http.Request,
	cfg sendConfig,
) (*BatchResult, error) {
	p := t.pool(origin)
	lease := p.leasePace(paceConfig{})
	dial := func() (*http2conn, error) { return t.dial(ctx, origin) }
	for {
		conn, reused, epoch, err := p.acquire(ctx, dial)
		if err != nil {
			return nil, err // context cancellation or dial failure — not retried
		}
		batch, _, keepConn, err := conn.attempt(
			ctx, reqs, cfg,
			lease.beforeFirstFlush(ctx, len(reqs)),
		)
		if keepConn {
			p.putIdle(conn, epoch)
		} else {
			p.discard(conn)
		}
		if err == nil {
			return batch, nil
		}
		if !keepConn && reused && ctx.Err() == nil &&
			!isLocalBatchTermination(err) {
			continue // a stale pooled connection; try again on a fresh one
		}
		// A read-phase stream reset carries the arrivals that raced beside it;
		// every other failure has no batch to report.
		return batch, err
	}
}
