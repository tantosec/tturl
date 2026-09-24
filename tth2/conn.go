package tth2

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"math/bits"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// maxDataFramePayload is the HTTP/2 default SETTINGS_MAX_FRAME_SIZE. A peer
// may negotiate a larger value, which we then track in peerMaxFrameSize.
const maxDataFramePayload = 1 << 14

// tlsRecordPlaintextBudget is the TLS 1.2/1.3 plaintext-record cap.
// flushOneRecord enforces "≤ one record buffered" before every load-bearing
// flush so each becomes exactly one TLS application data record.
const tlsRecordPlaintextBudget = 1 << 14

// http2FrameHeaderSize is the fixed HTTP/2 frame header length.
const http2FrameHeaderSize = 9

// maxBodyCarryingStreams is the hard upper bound on streams whose body bytes
// can share one final body release. It stays below the point where per-stream
// DATA frame overhead consumes an entire TLS record.
const maxBodyCarryingStreams = 1024

// defaultDrainTimeout bounds how long sendFlowControlledBody waits for a
// server flow-control beacon (WINDOW_UPDATE/SETTINGS/PING) before failing
// the trial. The caller's context bounds all other waiting.
const defaultDrainTimeout = 5 * time.Second

// Below this planned record size, favour useful DATA frame quanta over a
// one-byte contribution from every stream under scarce flow credit.
const greedyCreditRecordFloor = 8 << 10

// collectorChannelBuffer leaves the per-stream event handoff unbuffered. A
// collector accepts each copied DATA frame before the dispatcher reads another,
// bounding queued payload to one in-flight frame without changing response
// semantics.
const collectorChannelBuffer = 0

// maxInitialWindowSize is the RFC 9113 §6.5.2 cap on
// SETTINGS_INITIAL_WINDOW_SIZE: values above 2^31-1 are a connection error
// of type FLOW_CONTROL_ERROR.
const maxInitialWindowSize = (1 << 31) - 1

// maxMaxFrameSize is the RFC 9113 §6.5.2 upper bound on
// SETTINGS_MAX_FRAME_SIZE (2^24-1); the lower bound is the default
// maxDataFramePayload (2^14). A value outside that range is a connection error
// of type PROTOCOL_ERROR. Enforcing the ceiling keeps peerMaxFrameSize within
// the int32 domain the send-side flow-control arithmetic narrows it into.
const maxMaxFrameSize = (1 << 24) - 1

// HTTP/2 initially places no bound on peer-created streams. Every advertised
// SETTINGS_MAX_CONCURRENT_STREAMS value fits below this sentinel.
const unlimitedConcurrentStreams = uint64(1) << 32

// maxStreamID is the RFC 9113 §5.1.1 cap on stream IDs (2^31-1). When the
// client's next ID would exceed this, the connection must be replaced:
// allocateStreamIDs returns an error, and the send path discards the connection
// and re-dials.
const maxStreamID = (1 << 31) - 1

// frameEvent carries a single decoded frame from the dispatcher to a per-stream
// collector goroutine. Byte slices are copies — safe to hold after the next
// ReadFrame call.
//
// RST_STREAM is not represented here: the dispatcher signals a stream reset on
// the collector itself (streamCollector.rst), which needs no buffered channel
// slot and so cannot block behind a collector that is not reading.
type frameEvent struct {
	// Projected response HEADERS. Full fields survive dispatch only when capture
	// is enabled, in the owned Header or trailer map.
	headerKind responseHeaderKind
	response   *Response
	trailer    http.Header
	arrivalSeq int

	// DATA payload.
	data []byte

	endStream bool
}

type responseHeaderKind uint8

const (
	responseHeaderInformational responseHeaderKind = iota + 1
	responseHeaderFinal
	responseHeaderTrailer
)

// streamCollector is the per-stream event sink fed by the connection read pump.
//
// collectStream returns on the END_STREAM event itself. Only resetStream closes
// the channel, after storing rst; closeAll instead cancels the context so a
// connection-wide failure cannot be confused with clean stream completion.
type streamCollector struct {
	ch  chan frameEvent
	rst atomic.Pointer[StreamError]
}

// flowControl tracks HTTP/2 send credit for one batch. The atomic
// connection-level window is shared with response dispatch; only the sending
// goroutine accesses the per-stream windows.
//
// DATA writes use one of two paths:
//
//   - take debits available credit for streamed body data;
//   - reserve checks and debits all credit for one record atomically with
//     respect to SETTINGS updates, avoiding a mid-record drain and flush.
type flowControl struct {
	c         *http2conn
	mu        sync.Mutex
	stream    []int32  // per-stream credit; guarded by mu
	streamIDs []uint32 // wire IDs parallel to stream
	changed   chan struct{}
}

// newFlowControl initialises per-stream credit from the peer's current
// SETTINGS_INITIAL_WINDOW_SIZE. A SETTINGS update mid-send is applied through
// applyInitDelta by the connection read pump.
func newFlowControl(c *http2conn, streamIDs []uint32) *flowControl {
	init := int32(c.peerInitialStreamWindow()) //nolint:gosec // validated
	stream := make([]int32, len(streamIDs))
	for i := range stream {
		stream[i] = init
	}
	return &flowControl{
		c: c, stream: stream, streamIDs: streamIDs,
		changed: make(chan struct{}, 1),
	}
}

// take debits up to want bytes of credit for stream position idx, capped by
// SETTINGS_MAX_FRAME_SIZE. Returns the debited amount; zero indicates a window
// is empty and the caller should drain.
func (f *flowControl) take(idx int, want int) int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Cap to the per-frame ceiling first, in int64, so want -- a
	// body-length-derived count that may exceed the int32 domain -- narrows
	// safely. peerMaxFrameSize is validated to the RFC 9113 §6.5.2 range in
	// applySettings, so the capped value is a valid int32.
	frameCap := int64(f.c.peerMaximumFrameSize())
	avail := int32(min(int64(want), frameCap)) //nolint:gosec // <= frameCap <= 2^24-1
	if cw := f.c.peerConnSendWindow.Load(); cw < avail {
		avail = cw
	}
	if f.stream[idx] < avail {
		avail = f.stream[idx]
	}
	if avail <= 0 {
		return 0
	}
	f.c.peerConnSendWindow.Add(-avail)
	f.stream[idx] -= avail
	return avail
}

// reserve checks and debits the connection and per-stream windows in one
// critical section. A concurrent SETTINGS shrink must not invalidate credit
// after the sender commits to a shared record. needs[i] = 0 means stream i
// contributes no DATA to the record.
func (f *flowControl) reserve(needs []int32) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	var total int32
	for i, n := range needs {
		if n > 0 {
			if f.stream[i] < n {
				return false
			}
			total += n // one-record budget keeps the sum below MaxInt32
		}
	}
	if f.c.peerConnSendWindow.Load() < total {
		return false
	}
	for i, n := range needs {
		f.stream[i] -= n
	}
	f.c.peerConnSendWindow.Add(-total)
	return true
}

// available snapshots current send credit for planning one DATA record.
// reserve checks the chosen debit again before the record is written.
func (f *flowControl) available() (int32, []int32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.c.peerConnSendWindow.Load(), append([]int32(nil), f.stream...)
}

// addStream applies a WINDOW_UPDATE on a non-zero stream. A stream ID not in
// this batch is silently dropped — by the time a stale WINDOW_UPDATE arrives,
// its stream may have closed.
func (f *flowControl) addStream(sid uint32, n int32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, s := range f.streamIDs {
		if s == sid {
			updated := int64(f.stream[i]) + int64(n)
			if updated > maxInitialWindowSize {
				return fmt.Errorf("stream %d send window exceeds %d", sid,
					maxInitialWindowSize)
			}
			f.stream[i] = int32(updated) //nolint:gosec // checked above
			f.notify()
			return nil
		}
	}
	return nil
}

// applyInitDelta applies the per-RFC-9113-§6.9.2 delta when the peer changes
// SETTINGS_INITIAL_WINDOW_SIZE mid-send: every existing stream's window
// receives the delta.
func (f *flowControl) applyInitDelta(delta int32) error {
	if delta == 0 {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.stream {
		updated := int64(f.stream[i]) + int64(delta)
		if updated > maxInitialWindowSize || updated < math.MinInt32 {
			return fmt.Errorf("stream %d send window adjustment is outside the int32 range",
				f.streamIDs[i])
		}
	}
	for i := range f.stream {
		f.stream[i] += delta
	}
	f.notify()
	return nil
}

func (c *http2conn) addConnectionSendCredit(increment int32) error {
	for {
		current := c.peerConnSendWindow.Load()
		updated := int64(current) + int64(increment)
		if updated > maxInitialWindowSize {
			return fmt.Errorf("connection send window exceeds %d",
				maxInitialWindowSize)
		}
		// The cap above keeps updated in the non-negative int32 range.
		narrowed := int32(updated) //nolint:gosec // checked above
		if c.peerConnSendWindow.CompareAndSwap(current, narrowed) {
			return nil
		}
	}
}

func (f *flowControl) notify() {
	select {
	case f.changed <- struct{}{}:
	default:
	}
}

type http2conn struct {
	id                     ConnectionID
	tlsConn                *tls.Conn
	bw                     *bufio.Writer // 128 KB buffer wrapping tlsConn; all frame writes go here
	framer                 *http2.Framer // writes → bw; reads → tlsConn directly
	maxResponseHeaderBytes uint32
	advertiseHeaderLimit   bool

	hpackBuf bytes.Buffer
	hpackEnc *hpack.Encoder // bound to hpackBuf; one instance per connection

	nextStreamID uint32 // next client-initiated stream ID; starts at 1, advances by 2*n each trial

	mu         sync.Mutex
	collectors map[uint32]*streamCollector

	// Send-side flow control, tracked in atomics: the send goroutine reads these
	// for budgeting and writes decrements, while the dispatcher writes increments
	// (WINDOW_UPDATE) and updates SETTINGS values.
	peerConnSendWindow   atomic.Int32  // connection-level window; 65535 + preface WINDOW_UPDATE
	peerInitStreamWindow atomic.Uint32 // per-stream initial from SETTINGS_INITIAL_WINDOW_SIZE
	peerMaxFrameSize     atomic.Uint32 // from SETTINGS_MAX_FRAME_SIZE; caps DATA writes
	peerMaxStreams       atomic.Uint64 // SETTINGS_MAX_CONCURRENT_STREAMS, or the unlimited sentinel
	peerSettingsMu       sync.RWMutex  // guards SETTINGS visibility

	wireMu      sync.Mutex
	writeBroken atomic.Bool
	closeOnce   sync.Once
	readPump    *connectionReadPump

	// now is injectable only for deterministic package tests. A nil function
	// uses time.Now.
	now func() time.Time
}

// dialHTTP2 establishes a TLS+HTTP/2 connection to addr and performs the full
// HTTP/2 connection-preface exchange. dialContext is the transport dialer; the
// returned [net.Conn] is wrapped with TLS and HTTP/2 here. The caller must call
// close() when done.
func dialHTTP2(
	ctx context.Context,
	addr string,
	tlsCfg *tls.Config,
	dialContext func(ctx context.Context, network, addr string) (net.Conn, error),
	maxResponseHeaderBytes uint32,
	advertiseHeaderLimit bool,
) (*http2conn, error) {
	rawConn, err := dialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	// TCP_NODELAY opportunistically: Nagle would coalesce a small-body batch's
	// small final write with Linux's ~40 ms delayed-ACK timer end to end. If the
	// dialer returned a non-TCP conn (Unix socket, in-memory pipe, fault-injection
	// wrapper, proxy tunnel) we trust the dialer to have applied whatever
	// transport tuning its conn needs.
	if tcpConn, ok := rawConn.(*net.TCPConn); ok {
		if err := tcpConn.SetNoDelay(true); err != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("SetNoDelay: %w", err)
		}
	}

	// tls.Client doesn't derive ServerName from the dial address the way tls.Dial
	// does; set it so SNI is sent correctly.
	if tlsCfg.ServerName == "" {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		tlsCfg = tlsCfg.Clone()
		tlsCfg.ServerName = host
	}

	tlsConn := tls.Client(rawConn, tlsCfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = tlsConn.Close()
		return nil, fmt.Errorf("TLS handshake: %w", err)
	}
	if proto := tlsConn.ConnectionState().NegotiatedProtocol; proto != "h2" {
		_ = tlsConn.Close()
		return nil, fmt.Errorf("server negotiated %q; expected h2 (ensure HTTP/2 is enabled)", proto)
	}

	// Write path: frames → bw (128 KB buffer) → tlsConn.
	// Read path:  tlsConn → framer (bypasses bw).
	// bw.Flush() therefore controls exactly when data reaches the TLS layer.
	bw := bufio.NewWriterSize(tlsConn, 128*1024)
	framer := http2.NewFramer(bw, tlsConn)
	framer.SetMaxReadFrameSize(maxDataFramePayload)
	// ReadMetaHeaders causes ReadFrame to handle CONTINUATION frames automatically
	// and return a *MetaHeadersFrame with decoded fields.
	framer.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	framer.MaxHeaderListSize = maxResponseHeaderBytes

	c := &http2conn{
		tlsConn:                tlsConn,
		bw:                     bw,
		framer:                 framer,
		maxResponseHeaderBytes: maxResponseHeaderBytes,
		advertiseHeaderLimit:   advertiseHeaderLimit,
		nextStreamID:           1,
		collectors:             make(map[uint32]*streamCollector),
	}
	c.hpackEnc = hpack.NewEncoder(&c.hpackBuf)
	c.peerConnSendWindow.Store(65535)
	c.peerInitStreamWindow.Store(65535)
	c.peerMaxFrameSize.Store(maxDataFramePayload)
	c.peerMaxStreams.Store(unlimitedConcurrentStreams)

	if err := c.doPreface(ctx); err != nil {
		_ = tlsConn.Close()
		return nil, err
	}
	c.startReadPump()
	return c, nil
}

// flushOneRecord flushes c.bw with a defensive check that no more than one TLS
// plaintext record's worth of data is buffered. The phase-boundary flushes rely
// on "one bw.Flush() → one TLS record" as a shared authentication gate. If
// the budget is ever exceeded, the release spans multiple records and becomes
// ordinary sequential delivery. label is incorporated into the error so
// the offending call site is named in test failures and logs.
func (c *http2conn) flushOneRecord(label string) error {
	c.wireMu.Lock()
	defer c.wireMu.Unlock()
	return c.flushOneRecordUnlocked(label)
}

func (c *http2conn) flushOneRecordUnlocked(label string) error {
	if n := c.bw.Buffered(); n > tlsRecordPlaintextBudget {
		c.writeBroken.Store(true)
		return fmt.Errorf("tth2: %s: %d buffered bytes exceeds one TLS plaintext record (%d); record would split",
			label, n, tlsRecordPlaintextBudget)
	}
	if err := c.bw.Flush(); err != nil {
		c.writeBroken.Store(true)
		return fmt.Errorf("tth2: %s: flush: %w", label, err)
	}
	return nil
}

// flushOneRecordAt samples the wall clock immediately before dispatch and
// publishes the sample only after a successful flush.
func (c *http2conn) flushOneRecordAt(label string) (time.Time, error) {
	w := c.newRecordWriter()
	defer w.close()
	return w.flushAt(label)
}

// doPreface sends the HTTP/2 client connection preface and waits until the
// server's initial SETTINGS frame has been received and ACKed.
func (c *http2conn) doPreface(ctx context.Context) error {
	stopInterrupt := c.interruptReadOnCancel(ctx)
	defer stopInterrupt()

	if _, err := io.WriteString(c.bw, http2.ClientPreface); err != nil {
		return fmt.Errorf("write preface: %w", err)
	}
	settings := []http2.Setting{
		{
			ID:  http2.SettingInitialWindowSize,
			Val: 1<<24 - 1, // 16 MB per-stream receive window
		},
		{
			// tth2 has no server-push API. Advertising this prevents pushed
			// responses from entering a batch's arrival evidence.
			ID: http2.SettingEnablePush, Val: 0,
		},
	}
	if c.advertiseHeaderLimit {
		settings = append(settings, http2.Setting{
			ID:  http2.SettingMaxHeaderListSize,
			Val: c.maxResponseHeaderBytes,
		})
	}
	if err := c.framer.WriteSettings(settings...); err != nil {
		return fmt.Errorf("write SETTINGS: %w", err)
	}
	// Open the connection-level receive window to ~1 GB (default is 65535).
	if err := c.framer.WriteWindowUpdate(0, (1<<30)-(1<<16-1)); err != nil {
		return fmt.Errorf("write WINDOW_UPDATE: %w", err)
	}
	if err := c.bw.Flush(); err != nil {
		return fmt.Errorf("flush preface: %w", err)
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		f, err := c.framer.ReadFrame()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("preface read: %w", err)
		}
		switch fr := f.(type) {
		case *http2.SettingsFrame:
			if fr.IsAck() {
				continue
			}
			if _, err := c.applySettings(fr); err != nil {
				return fmt.Errorf("preface: %w", err)
			}
			return nil
		case *http2.WindowUpdateFrame:
			if fr.StreamID == 0 {
				if err := c.addConnectionSendCredit(
					int32(fr.Increment), //nolint:gosec // framer masks reserved bit
				); err != nil {
					return fmt.Errorf("preface WINDOW_UPDATE: %w", err)
				}
			}
		case *http2.PingFrame:
			if !fr.IsAck() {
				if err := c.framer.WritePing(true, fr.Data); err != nil {
					return fmt.Errorf("preface PING ACK: %w", err)
				}
				if err := c.bw.Flush(); err != nil {
					return fmt.Errorf("preface PING flush: %w", err)
				}
			}
		case *http2.GoAwayFrame:
			return fmt.Errorf("server GOAWAY during setup: code=%v", fr.ErrCode)
		}
	}
}

// interruptReadOnCancel makes a blocked connection read observe ctx. The
// returned cleanup waits until the watcher can no longer touch the connection
// and clears its deadline for later reads.
func (c *http2conn) interruptReadOnCancel(ctx context.Context) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			_ = c.tlsConn.SetReadDeadline(time.Now())
		case <-stop:
		}
	}()
	return func() {
		close(stop)
		<-done
		_ = c.tlsConn.SetReadDeadline(time.Time{})
	}
}

// applySettingsValues validates and atomically publishes a non-ACK SETTINGS
// frame. It returns the initial-window delta for the read pump to apply to
// existing streams as required by RFC 9113, section 6.9.2.
func (c *http2conn) applySettingsValues(
	fr *http2.SettingsFrame,
) (delta int32, err error) {
	c.peerSettingsMu.Lock()
	defer c.peerSettingsMu.Unlock()
	initialWindow := c.peerInitStreamWindow.Load()
	maxFrameSize := c.peerMaxFrameSize.Load()
	maxStreams := c.peerMaxStreams.Load()
	err = fr.ForeachSetting(func(s http2.Setting) error {
		switch s.ID {
		case http2.SettingInitialWindowSize:
			if s.Val > maxInitialWindowSize {
				return fmt.Errorf("peer SETTINGS_INITIAL_WINDOW_SIZE %d exceeds RFC 9113 section 6.5.2 cap of %d",
					s.Val, uint32(maxInitialWindowSize))
			}
			initialWindow = s.Val
		case http2.SettingMaxFrameSize:
			if s.Val < maxDataFramePayload || s.Val > maxMaxFrameSize {
				return fmt.Errorf("peer SETTINGS_MAX_FRAME_SIZE %d outside RFC 9113 section 6.5.2 range [%d, %d]",
					s.Val, maxDataFramePayload, maxMaxFrameSize)
			}
			maxFrameSize = s.Val
		case http2.SettingMaxConcurrentStreams:
			maxStreams = uint64(s.Val)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	previousWindow := c.peerInitStreamWindow.Load()
	delta = int32(initialWindow) - int32(previousWindow) //nolint:gosec // validated
	c.peerInitStreamWindow.Store(initialWindow)
	c.peerMaxFrameSize.Store(maxFrameSize)
	c.peerMaxStreams.Store(maxStreams)
	return delta, nil
}

func (c *http2conn) peerInitialStreamWindow() uint32 {
	c.peerSettingsMu.RLock()
	defer c.peerSettingsMu.RUnlock()
	return c.peerInitStreamWindow.Load()
}

func (c *http2conn) peerMaximumFrameSize() uint32 {
	c.peerSettingsMu.RLock()
	defer c.peerSettingsMu.RUnlock()
	return c.peerMaxFrameSize.Load()
}

func (c *http2conn) peerConcurrentStreamLimit() uint64 {
	c.peerSettingsMu.RLock()
	defer c.peerSettingsMu.RUnlock()
	return c.peerMaxStreams.Load()
}

func (c *http2conn) applySettings(
	fr *http2.SettingsFrame,
) (delta int32, err error) {
	delta, err = c.applySettingsValues(fr)
	if err != nil {
		return 0, err
	}
	if err := c.framer.WriteSettingsAck(); err != nil {
		return 0, fmt.Errorf("write SETTINGS ACK: %w", err)
	}
	if err := c.bw.Flush(); err != nil {
		return 0, fmt.Errorf("flush SETTINGS ACK: %w", err)
	}
	return delta, nil
}

var errPeerStreamLimit = errors.New("tth2: peer concurrent-stream limit")

func (c *http2conn) checkPeerStreamLimit(width int, ignored bool) error {
	// width comes from len(reqs), so it is non-negative.
	required := uint64(width) //nolint:gosec // A slice length cannot be negative.
	limit := c.peerConcurrentStreamLimit()
	if ignored || required <= limit {
		return nil
	}
	return fmt.Errorf(
		"%w is %d; batch requires %d streams (use "+
			"WithPeerStreamLimitIgnored for an intentional protocol probe)",
		errPeerStreamLimit, limit, width)
}

// allocateStreamIDs reserves n consecutive client-initiated stream IDs and
// advances nextStreamID. Client streams use odd IDs in [1, 2^31-1]; exhaustion
// returns a connection-fatal error so the send path replaces the connection.
func (c *http2conn) allocateStreamIDs(n int) ([]uint32, error) {
	if n <= 0 {
		return nil, nil
	}
	last := uint64(c.nextStreamID) + uint64(2*(n-1)) //nolint:gosec // n >= 1 here, so 2*(n-1) >= 0
	if last > maxStreamID {
		return nil, fmt.Errorf("tth2: client stream ID space exhausted (next=%d, batch=%d, max=%d); reconnect required",
			c.nextStreamID, n, uint32(maxStreamID))
	}
	streamIDs := make([]uint32, n)
	for i := range n {
		streamIDs[i] = c.nextStreamID + uint32(2*i)
	}
	c.nextStreamID += uint32(2 * n) //nolint:gosec // 2*n bounded by the maxStreamID check above
	return streamIDs, nil
}

// writeHeaderBlock writes hblock as a HEADERS frame followed by zero or more
// CONTINUATION frames, splitting at peerMaxFrameSize. EndStream applies to
// the leading HEADERS frame; EndHeaders is set on the final frame of the
// sequence.
//
// The complete frame sequence is checked against the TLS-record budget before
// it is written. This prevents bufio.Writer from automatically flushing a
// partial record when a header block is too large.
func (c *http2conn) writeHeaderBlock(streamID uint32, hblock []byte, endStream bool) error {
	max := int(c.peerMaximumFrameSize())
	if max <= 0 {
		return fmt.Errorf("tth2: peer SETTINGS_MAX_FRAME_SIZE %d is invalid", max)
	}
	frames := 1
	if len(hblock) > 0 {
		frames = (len(hblock)-1)/max + 1
	}
	recordBytes := int64(c.bw.Buffered()) + int64(len(hblock)) +
		int64(frames)*http2FrameHeaderSize
	if recordBytes > tlsRecordPlaintextBudget {
		return fmt.Errorf("tth2: HEADERS stream %d needs %d buffered bytes, exceeding one TLS plaintext record (%d)",
			streamID, recordBytes, tlsRecordPlaintextBudget)
	}
	first := hblock
	if len(first) > max {
		first = first[:max]
	}
	if err := c.framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      streamID,
		BlockFragment: first,
		EndStream:     endStream,
		EndHeaders:    len(first) == len(hblock),
	}); err != nil {
		return fmt.Errorf("write HEADERS stream %d: %w", streamID, err)
	}
	hblock = hblock[len(first):]
	for len(hblock) > 0 {
		chunk := hblock
		if len(chunk) > max {
			chunk = chunk[:max]
		}
		end := len(chunk) == len(hblock)
		if err := c.framer.WriteContinuation(streamID, end, chunk); err != nil {
			return fmt.Errorf("write CONTINUATION stream %d: %w", streamID, err)
		}
		hblock = hblock[len(chunk):]
	}
	return nil
}

// sendRequests selects the bodyless or body-carrying send protocol.
//
// Note: if a handler returns without reading the body at all (e.g. a GET
// handler that ignores a supplied body), the server may RST the stream before
// the final body release. Use no-body requests in that case.
func (c *http2conn) sendRequests(
	ctx context.Context,
	reqs []*http.Request,
	streamIDs []uint32,
	releaseDelay time.Duration,
	bodyBytesWithheld int,
	beforeFirstFlush func() error,
	responses *responseRead,
) (DispatchTimes, error) {
	for _, req := range reqs {
		if req.Body != nil && req.Body != http.NoBody {
			return c.sendWithBody(
				ctx, reqs, streamIDs, releaseDelay, bodyBytesWithheld,
				beforeFirstFlush, responses,
			)
		}
	}
	return c.sendNoBody(ctx, reqs, streamIDs, beforeFirstFlush)
}

// sendNoBody buffers HEADERS+END_STREAM for every stream and flushes once,
// producing a single TLS record. See the package doc for the batch-release
// invariant this relies on.
func (c *http2conn) sendNoBody(
	ctx context.Context,
	reqs []*http.Request,
	streamIDs []uint32,
	beforeFirstFlush func() error,
) (DispatchTimes, error) {
	blocks, err := c.encodeAllHeaders(reqs, nil, streamIDs)
	if err != nil {
		return DispatchTimes{}, err
	}
	if beforeFirstFlush != nil {
		if err := beforeFirstFlush(); err != nil {
			return DispatchTimes{}, err
		}
	}
	if cause := context.Cause(ctx); isLocalBatchTermination(cause) {
		return DispatchTimes{}, cause
	}
	headersAt, err := c.writeHeaderRecord(blocks, "no-body HEADERS")
	if err != nil {
		return DispatchTimes{}, err
	}
	return DispatchTimes{HeadersSentAt: headersAt}, nil
}

// sendWithBody reads each body into memory once (so trial reuse is safe), then
// selects direct body transfer or a separate withheld-tail release.
func (c *http2conn) sendWithBody(
	ctx context.Context,
	reqs []*http.Request,
	streamIDs []uint32,
	releaseDelay time.Duration,
	bodyBytesWithheld int,
	beforeFirstFlush func() error,
	responses *responseRead,
) (DispatchTimes, error) {
	bodies, err := readAllBodies(reqs)
	if err != nil {
		return DispatchTimes{}, err
	}
	carrying := 0
	for _, body := range bodies {
		if len(body) > 0 {
			carrying++
		}
	}
	if carrying > maxBodyCarryingStreams {
		return DispatchTimes{}, fmt.Errorf("tth2: %d streams carrying body bytes exceeds the cap of %d",
			carrying, maxBodyCarryingStreams)
	}

	if bodyBytesWithheld == 0 || !hasBodyBytes(bodies) {
		return c.sendBodyUnwithheld(
			ctx, reqs, bodies, streamIDs, beforeFirstFlush, responses,
		)
	}

	pivots := make([]int, len(bodies))
	for i := range bodies {
		if l := len(bodies[i]); l > 0 {
			pivots[i] = max(0, l-bodyBytesWithheld)
		}
	}
	tailNeeds, err := tailReleaseNeeds(bodies, pivots)
	if err != nil {
		return DispatchTimes{}, err
	}
	return c.sendBodyWithTailRelease(
		ctx, reqs, bodies, pivots, tailNeeds, streamIDs, releaseDelay,
		beforeFirstFlush, responses,
	)
}

func hasBodyBytes(bodies [][]byte) bool {
	for _, body := range bodies {
		if len(body) > 0 {
			return true
		}
	}
	return false
}

// sendBodyUnwithheld releases HEADERS for every stream, then transfers each
// complete body through flow-controlled DATA frames. Fitting bodies carry
// END_STREAM in one record. Larger or credit-limited bodies advance without
// completing any stream until their remaining positive suffixes can share a
// final DATA+END_STREAM record.
func (c *http2conn) sendBodyUnwithheld(
	ctx context.Context,
	reqs []*http.Request,
	bodies [][]byte,
	streamIDs []uint32,
	beforeFirstFlush func() error,
	responses *responseRead,
) (DispatchTimes, error) {
	flow, clearFlow, err := c.beginSendFlow(responses, streamIDs)
	if err != nil {
		return DispatchTimes{}, err
	}
	defer clearFlow()
	headersAt, err := c.flushBodyHeaders(
		ctx, reqs, bodies, streamIDs, beforeFirstFlush,
	)
	if err != nil {
		return DispatchTimes{}, err
	}
	bodyAt, fitting, err := c.sendFittingBodyRecord(
		flow, bodies, true, "body", responses,
	)
	if err == nil && !fitting {
		bodyAt, err = c.sendFlowControlledBody(
			ctx, flow, bodies, true, "body", responses,
		)
	}
	if err != nil {
		return DispatchTimes{HeadersSentAt: headersAt}, err
	}
	return DispatchTimes{
		HeadersSentAt: headersAt, BodyFinalisedAt: bodyAt,
	}, nil
}

// sendBodyWithTailRelease releases HEADERS, transfers each body prefix through
// flow-controlled DATA records, waits for the configured delay, then releases
// every withheld tail and END_STREAM in one record.
//
// One flowControl spans the prefix and tail: the final release needs the
// post-prefix credit state to refuse to write a stream whose receive window the
// server has not yet replenished.
func (c *http2conn) sendBodyWithTailRelease(
	ctx context.Context,
	reqs []*http.Request,
	bodies [][]byte,
	pivots []int,
	tailNeeds []int32,
	streamIDs []uint32,
	releaseDelay time.Duration,
	beforeFirstFlush func() error,
	responses *responseRead,
) (DispatchTimes, error) {
	flow, clearFlow, err := c.beginSendFlow(responses, streamIDs)
	if err != nil {
		return DispatchTimes{}, err
	}
	defer clearFlow()
	headersAt, err := c.flushBodyHeaders(
		ctx, reqs, bodies, streamIDs, beforeFirstFlush,
	)
	if err != nil {
		return DispatchTimes{}, err
	}

	prefixes := make([][]byte, len(bodies))
	for i := range bodies {
		prefixes[i] = bodies[i][:pivots[i]]
	}
	_, fitting, err := c.sendFittingBodyRecord(
		flow, prefixes, false, "body prefix", responses,
	)
	if err == nil && !fitting {
		_, err = c.sendFlowControlledBody(
			ctx, flow, prefixes, false, "body prefix", responses,
		)
	}
	if err != nil {
		return DispatchTimes{HeadersSentAt: headersAt}, err
	}

	if err := waitReleaseDelay(ctx, releaseDelay); err != nil {
		return DispatchTimes{HeadersSentAt: headersAt}, err
	}

	record := c.newRecordWriter()
	defer record.close()
	if err := c.writeTailReleaseFrames(
		ctx, flow, bodies, pivots, tailNeeds, responses, record,
	); err != nil {
		return DispatchTimes{HeadersSentAt: headersAt}, err
	}
	bodyAt, err := record.flushAtForSend("final body release", responses)
	if err != nil {
		return DispatchTimes{HeadersSentAt: headersAt}, err
	}

	return DispatchTimes{
		HeadersSentAt: headersAt, BodyFinalisedAt: bodyAt,
	}, nil
}

func (c *http2conn) beginSendFlow(
	responses *responseRead, streamIDs []uint32,
) (*flowControl, func(), error) {
	if c.readPump == nil {
		return newFlowControl(c, streamIDs), func() {}, nil
	}
	flow, err := c.readPump.setFlow(responses, streamIDs)
	if err != nil {
		return nil, nil, err
	}
	return flow, func() { c.readPump.clearFlow(flow) }, nil
}

func (c *http2conn) flushBodyHeaders(
	ctx context.Context,
	reqs []*http.Request,
	bodies [][]byte,
	streamIDs []uint32,
	beforeFirstFlush func() error,
) (time.Time, error) {
	blocks, err := c.encodeAllHeaders(reqs, bodies, streamIDs)
	if err != nil {
		return time.Time{}, err
	}
	if beforeFirstFlush != nil {
		if err := beforeFirstFlush(); err != nil {
			return time.Time{}, err
		}
	}
	if cause := context.Cause(ctx); isLocalBatchTermination(cause) {
		return time.Time{}, cause
	}
	return c.writeHeaderRecord(blocks, "body request HEADERS")
}

// readAllBodies reads each body through GetBody. Empty or nil bodies produce
// zero-length entries. Public entry points ensure that non-empty bodies are
// reusable.
func readAllBodies(reqs []*http.Request) ([][]byte, error) {
	bodies := make([][]byte, len(reqs))
	for i, req := range reqs {
		if req.Body == nil || req.Body == http.NoBody {
			continue
		}
		rc, err := req.GetBody()
		if err != nil {
			return nil, fmt.Errorf("request %d GetBody: %w", i, err)
		}
		if rc == nil {
			return nil, fmt.Errorf("request %d GetBody: returned nil body", i)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return nil, fmt.Errorf("request %d body read: %w", i, err)
		}
		bodies[i] = b
	}
	return bodies, nil
}

type encodedHeaderBlock struct {
	streamID  uint32
	block     []byte
	endStream bool
}

// encodeAllHeaders updates the connection's HPACK state before the pacing
// gate. Keeping the write buffer empty while that gate waits lets the read pump
// acknowledge control frames without disturbing the eventual header record.
func (c *http2conn) encodeAllHeaders(
	reqs []*http.Request, bodies [][]byte, streamIDs []uint32,
) ([]encodedHeaderBlock, error) {
	blocks := make([]encodedHeaderBlock, len(reqs))
	for i, req := range reqs {
		bodyLen := int64(-1)
		endStream := true
		if bodies != nil {
			bodyLen = requestBodyLength(req, bodies[i])
			endStream = len(bodies[i]) == 0
		}
		hblock, err := encodeHeaders(c.hpackEnc, &c.hpackBuf, req, bodyLen)
		if err != nil {
			return nil, fmt.Errorf("request %d headers: %w", i, err)
		}
		blocks[i] = encodedHeaderBlock{
			streamID: streamIDs[i], block: hblock,
			endStream: endStream,
		}
	}
	return blocks, nil
}

func (c *http2conn) writeHeaderRecord(
	blocks []encodedHeaderBlock, label string,
) (time.Time, error) {
	record := c.newRecordWriter()
	defer record.close()
	if err := record.begin(); err != nil {
		return time.Time{}, err
	}
	for _, block := range blocks {
		if err := c.writeHeaderBlock(
			block.streamID, block.block, block.endStream,
		); err != nil {
			return time.Time{}, err
		}
	}
	return record.flushAt(label)
}

func requestBodyLength(req *http.Request, body []byte) int64 {
	if req.Body == nil || req.Body == http.NoBody {
		return -1
	}
	return int64(len(body))
}

// sendFittingBodyRecord writes a complete direct body or withheld-tail prefix
// in one TLS record when its frames fit and the peer has enough credit now.
// Its false result leaves the flow state untouched for the streaming scheduler.
func (c *http2conn) sendFittingBodyRecord(
	flow *flowControl,
	data [][]byte,
	endStream bool,
	label string,
	responses *responseRead,
) (time.Time, bool, error) {
	needs := make([]int32, len(data))
	recordBytes := 0
	carrying := 0
	for i, body := range data {
		if len(body) == 0 {
			continue
		}
		carrying++
		if carrying > maxBodyCarryingStreams {
			return time.Time{}, true, fmt.Errorf("tth2: %d streams carrying body bytes exceeds the cap of %d",
				carrying, maxBodyCarryingStreams)
		}
		if len(body) > int(c.peerMaximumFrameSize()) ||
			len(body) > tlsRecordPlaintextBudget-recordBytes-http2FrameHeaderSize {
			return time.Time{}, false, nil
		}
		recordBytes += http2FrameHeaderSize + len(body)
		needs[i] = int32(len(body)) //nolint:gosec // bounded by TLS record budget
	}
	if recordBytes == 0 {
		return time.Time{}, true, nil
	}
	if !flow.reserve(needs) {
		return time.Time{}, false, nil
	}
	if err := responses.sendFailure(); err != nil {
		return time.Time{}, true, err
	}
	record := c.newRecordWriter()
	defer record.close()
	for i, body := range data {
		if len(body) == 0 {
			continue
		}
		if err := record.begin(); err != nil {
			return time.Time{}, true, err
		}
		if err := c.framer.WriteData(flow.streamIDs[i], endStream, body); err != nil {
			return time.Time{}, true, fmt.Errorf("write %s stream %d: %w", label, flow.streamIDs[i], err)
		}
	}
	if endStream {
		at, err := record.flushAtForSend(label+" fitting record", responses)
		return at, true, err
	}
	err := record.flushForSend(label+" fitting record", responses)
	return time.Time{}, true, err
}

// sendFlowControlledBody transfers non-fitting body data and explicit-tail
// prefixes through the same greedy record planner.
//
//  1. After return, every stream has sent exactly len(data[i]) bytes.
//  2. With endStreamOnFinal, the final flush contains a nonempty END_STREAM
//     frame for every body-carrying stream.
//  3. No flush exceeds tlsRecordPlaintextBudget.
//  4. Intermediate records advance streams towards common progress.
//
// endStreamOnFinal sets END_STREAM on each final DATA frame. label identifies
// this transfer in errors.
func (c *http2conn) sendFlowControlledBody(
	ctx context.Context,
	flow *flowControl,
	data [][]byte,
	endStreamOnFinal bool,
	label string,
	responses *responseRead,
) (time.Time, error) {
	record := c.newRecordWriter()
	defer record.close()
	if err := responses.sendFailure(); err != nil {
		return time.Time{}, err
	}
	carrying := make([]int, 0, len(data))
	for i, b := range data {
		if len(b) > 0 {
			carrying = append(carrying, i)
		}
	}
	if len(carrying) == 0 {
		return time.Time{}, nil
	}
	if len(carrying) > maxBodyCarryingStreams {
		return time.Time{}, fmt.Errorf("tth2: %d streams carrying body bytes exceeds the cap of %d",
			len(carrying), maxBodyCarryingStreams)
	}
	return c.sendGreedyBodyPhase(ctx, flow, data, carrying,
		endStreamOnFinal, label, responses, record)
}

// sendGreedyBodyPhase fills nonterminal DATA records towards common progress.
// An unwithheld body retains positive suffixes until they fit one shared
// DATA+END_STREAM record. A positive-withhold prefix has no such extra gate:
// its streams may finish their prefixes individually before the explicit tail.
func (c *http2conn) sendGreedyBodyPhase(
	ctx context.Context,
	flow *flowControl,
	data [][]byte,
	carrying []int,
	endStream bool,
	label string,
	responses *responseRead,
	record *recordWriter,
) (time.Time, error) {
	sent := make([]int, len(data))
	firstProgress := make([]uint64, len(data))
	for {
		if err := responses.sendFailure(); err != nil {
			return time.Time{}, err
		}
		if ctx.Err() != nil {
			return time.Time{}, batchContextError(ctx)
		}
		if !endStream {
			complete := true
			for _, i := range carrying {
				if sent[i] < len(data[i]) {
					complete = false
					break
				}
			}
			if complete {
				return time.Time{}, nil
			}
		} else {
			remaining := make([]int32, len(data))
			finalBytes := 0
			for _, i := range carrying {
				left := len(data[i]) - sent[i]
				finalBytes += http2FrameHeaderSize + left
				if finalBytes <= tlsRecordPlaintextBudget {
					remaining[i] = int32(left) //nolint:gosec // bounded by record budget
				}
			}
			if finalBytes <= tlsRecordPlaintextBudget && flow.reserve(remaining) {
				for _, i := range carrying {
					if err := responses.sendFailure(); err != nil {
						return time.Time{}, err
					}
					if err := record.begin(); err != nil {
						return time.Time{}, err
					}
					if err := c.framer.WriteData(flow.streamIDs[i], true, data[i][sent[i]:]); err != nil {
						return time.Time{}, fmt.Errorf("write final body stream %d: %w", flow.streamIDs[i], err)
					}
				}
				return record.flushAtForSend(label+" final record", responses)
			}
		}

		connCredit, streamCredit := flow.available()
		caps := make([]int32, len(data))
		maxFrame := min(int(c.peerMaximumFrameSize()), tlsRecordPlaintextBudget-http2FrameHeaderSize)
		for _, i := range carrying {
			left := len(data[i]) - sent[i]
			if endStream {
				left-- // keep a positive byte for the shared final DATA+END record
			}
			cap := min(left, maxFrame)
			if streamCredit[i] < int32(cap) { //nolint:gosec // cap is below the TLS record budget
				cap = int(streamCredit[i])
			}
			if cap > 0 {
				caps[i] = int32(cap)
			}
		}
		plan := planGreedyIntermediateRecordWithScratch(data, sent, carrying, caps, connCredit, firstProgress)
		if plan == nil {
			if err := c.drainWithTimeout(ctx, flow, responses); err != nil {
				return time.Time{}, err
			}
			continue
		}
		plannedBytes := 0
		plannedPayload := 0
		for _, i := range carrying {
			if plan[i] > 0 {
				plannedBytes += http2FrameHeaderSize + int(plan[i])
				plannedPayload += int(plan[i])
			}
		}
		if plannedBytes < greedyCreditRecordFloor {
			if scarce := planScarceCreditRecord(data, sent, carrying, caps, connCredit); scarce != nil {
				scarceBytes := 0
				scarcePayload := 0
				for _, i := range carrying {
					if scarce[i] > 0 {
						scarceBytes += http2FrameHeaderSize + int(scarce[i])
						scarcePayload += int(scarce[i])
					}
				}
				// The useful-frame policy must never leave body bytes unsent
				// when the ordinary greedy planner could send them now.
				if scarcePayload > plannedPayload ||
					(scarcePayload == plannedPayload && scarceBytes <= plannedBytes) {
					plan = scarce
				}
			}
		}
		if !flow.reserve(plan) {
			continue // a SETTINGS update changed credit while planning
		}
		for _, i := range carrying {
			if plan[i] == 0 {
				continue
			}
			if err := responses.sendFailure(); err != nil {
				return time.Time{}, err
			}
			if err := record.begin(); err != nil {
				return time.Time{}, err
			}
			end := sent[i] + int(plan[i])
			if err := c.framer.WriteData(flow.streamIDs[i], false, data[i][sent[i]:end]); err != nil {
				return time.Time{}, fmt.Errorf("write body stream %d: %w", flow.streamIDs[i], err)
			}
			sent[i] = end
		}
		if err := record.flushForSend(label+" intermediate record", responses); err != nil {
			return time.Time{}, err
		}
	}
}

// When credit is scarce, sending one byte on every stream feeds peers that
// return credit per DATA frame a large number of microscopic frames. Spend the
// available credit in useful chunks, rotating towards least progress and
// limiting any stream's lead to roughly one thirty-second of its body.
func planScarceCreditRecord(
	data [][]byte, sent []int, carrying []int, caps []int32, connCredit int32,
) []int32 {
	plan := make([]int32, len(data))
	maxCap := int32(0)
	for _, i := range carrying {
		maxCap = max(maxCap, caps[i])
	}
	minUsefulCap := min(int32(64), maxCap)
	maxProgress := 0.0
	for _, i := range carrying {
		maxProgress = max(maxProgress, float64(sent[i])/float64(len(data[i])))
	}
	candidates := make([]int, 0, len(carrying))
	for _, i := range carrying {
		granularity := max(1/float64(len(data[i])), 1.0/32)
		lagging := maxProgress-float64(sent[i])/float64(len(data[i])) >= granularity
		if caps[i] > 0 && (caps[i] >= minUsefulCap || lagging) {
			candidates = append(candidates, i)
		}
	}
	credit := int(connCredit)
	recordBytes := 0
	for credit > 0 {
		best := -1
		for _, i := range candidates {
			if caps[i] <= plan[i] ||
				int(plan[i]) >= max(1, (len(data[i])+31)/32) {
				continue
			}
			if recordBytes+http2FrameHeaderSize+1 > tlsRecordPlaintextBudget && plan[i] == 0 {
				continue
			}
			if best < 0 || lessBodyProgress(data, sent, plan, i, best) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		//nolint:gosec // best is checked nonnegative above
		leadBudget := max(1, (len(data[best])+31)/32)
		//nolint:gosec // best is checked nonnegative above
		amount := min(credit, int(caps[best]-plan[best]), leadBudget-int(plan[best]))
		if plan[best] == 0 {
			amount = min(amount, tlsRecordPlaintextBudget-recordBytes-http2FrameHeaderSize)
		} else {
			amount = min(amount, tlsRecordPlaintextBudget-recordBytes)
		}
		if amount <= 0 {
			break
		}
		if plan[best] == 0 {
			recordBytes += http2FrameHeaderSize
		}
		plan[best] += int32(amount)
		recordBytes += amount
		credit -= amount
	}
	if recordBytes == 0 {
		return nil
	}
	return plan
}

// planGreedyIntermediateRecord advances streams towards a common fraction of
// their original body lengths. It charges HTTP/2 frame headers only for streams
// that actually contribute bytes, then uses any rounding slack to advance the
// least-progressed eligible stream. A nil plan means no DATA can be sent until
// the peer grants credit.
func planGreedyIntermediateRecord(
	data [][]byte, sent []int, carrying []int, caps []int32, connCredit int32,
) []int32 {
	return planGreedyIntermediateRecordWithScratch(data, sent, carrying, caps, connCredit,
		make([]uint64, len(data)))
}

func planGreedyIntermediateRecordWithScratch(
	data [][]byte, sent []int, carrying []int, caps []int32, connCredit int32,
	firstProgress []uint64,
) []int32 {
	maxBodyLen := 0
	for _, i := range carrying {
		maxBodyLen = max(maxBodyLen, len(data[i]))
	}
	// There is no need to search fractions finer than one byte of the
	// largest body. The residual-byte pass spends any resulting slack.
	progressScale := uint64(1) << min(bits.Len(uint(maxBodyLen)), 48)
	plan := make([]int32, len(data))
	// A stream cannot contribute until common progress reaches its next byte.
	// This avoids division for numerous tiny streams during searches governed
	// by a much larger stream.
	for _, i := range carrying {
		if caps[i] == 0 {
			continue
		}
		hi, lo := bits.Mul64(uint64(sent[i]+1), progressScale) //nolint:gosec // sent counts body bytes and is nonnegative
		q, rem := bits.Div64(hi, lo, uint64(len(data[i])))
		firstProgress[i] = q
		if rem != 0 {
			firstProgress[i]++
		}
	}
	measure := func(progress uint64, write bool) (payload, recordBytes int) {
		for _, i := range carrying {
			if caps[i] == 0 || progress < firstProgress[i] {
				continue
			}
			hi, lo := bits.Mul64(progress, uint64(len(data[i])))
			target, _ := bits.Div64(hi, lo, progressScale)
			targetBytes := int(target) //nolint:gosec // target cannot exceed len(data[i])
			wanted := max(0, targetBytes-sent[i])
			amount := min(int(caps[i]), wanted)
			if amount == 0 {
				continue
			}
			payload += amount
			recordBytes += http2FrameHeaderSize + amount
			if write {
				plan[i] = int32(amount) //nolint:gosec // capped by one TLS record
			}
		}
		return payload, recordBytes
	}
	var low uint64
	high := progressScale
	for low < high {
		mid := low + (high-low+1)/2
		payload, recordBytes := measure(mid, false)
		if payload <= int(connCredit) && recordBytes <= tlsRecordPlaintextBudget {
			low = mid
		} else {
			high = mid - 1
		}
	}
	payload, recordBytes := measure(low, true)
	for payload < int(connCredit) {
		best := -1
		for _, i := range carrying {
			if plan[i] >= caps[i] {
				continue
			}
			cost := 1
			if plan[i] == 0 {
				cost += http2FrameHeaderSize
			}
			if recordBytes+cost > tlsRecordPlaintextBudget {
				continue
			}
			if best < 0 || lessBodyProgress(data, sent, plan, i, best) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		if plan[best] == 0 {
			recordBytes += http2FrameHeaderSize
		}
		plan[best]++
		payload++
		recordBytes++
	}
	if payload == 0 {
		return nil
	}
	return plan
}

// lessBodyProgress compares the normalised progress after a proposed record
// without floating-point rounding or overflowing a 64-bit product.
func lessBodyProgress(data [][]byte, sent []int, plan []int32, a, b int) bool {
	aHi, aLo := bits.Mul64(uint64(sent[a]+int(plan[a])), uint64(len(data[b]))) //nolint:gosec // counts are nonnegative
	bHi, bLo := bits.Mul64(uint64(sent[b]+int(plan[b])), uint64(len(data[a]))) //nolint:gosec // counts are nonnegative
	return aHi < bHi || (aHi == bHi && aLo < bLo)
}

// sendBytesUpTo writes DATA frames to advance sent[i] up to target, draining
// for flow-control credit as needed via flow.take. END_STREAM is set only when
// endStream is true and the frame completes the stream's data.
func (c *http2conn) sendBytesUpTo(
	ctx context.Context,
	flow *flowControl,
	data [][]byte,
	sent []int,
	i int,
	target int,
	endStream bool,
	responses *responseRead,
	record *recordWriter,
) error {
	for sent[i] < target {
		if err := responses.sendFailure(); err != nil {
			return err
		}
		avail := flow.take(i, target-sent[i])
		if avail == 0 {
			if err := record.flushForSend(
				"body credit drain prelude", responses,
			); err != nil {
				return err
			}
			if err := c.drainWithTimeout(ctx, flow, responses); err != nil {
				return err
			}
			continue
		}
		end := endStream && sent[i]+int(avail) == len(data[i])
		if err := record.begin(); err != nil {
			return err
		}
		// Flow-control credit can be replenished between successive calls to
		// take. A single scheduled stream contribution can therefore become
		// multiple DATA frames without ever observing zero credit. Account for
		// the actual frame header before every write instead of relying only on
		// the scheduler's one-header-per-stream allowance.
		frameBytes := http2FrameHeaderSize + int(avail)
		if c.bw.Buffered()+frameBytes > tlsRecordPlaintextBudget {
			if err := record.flushForSend(
				"body DATA frame record boundary", responses,
			); err != nil {
				return err
			}
			if err := record.begin(); err != nil {
				return err
			}
		}
		if err := c.framer.WriteData(flow.streamIDs[i], end, data[i][sent[i]:sent[i]+int(avail)]); err != nil {
			return fmt.Errorf("write body stream %d: %w", flow.streamIDs[i], err)
		}
		sent[i] += int(avail)
	}
	return nil
}

// drainWithTimeout invokes drainForSendCredit under a context bounded by
// defaultDrainTimeout, so a wedged peer fails rather than hanging the call.
func (c *http2conn) drainWithTimeout(
	ctx context.Context, flow *flowControl, responses *responseRead,
) error {
	drainCtx, cancel := context.WithTimeout(ctx, defaultDrainTimeout)
	defer cancel()
	err := c.drainForSendCredit(drainCtx, flow, responses)
	if err == nil {
		return nil
	}
	// drainForSendCredit surfaces an interrupted read as drainCtx.Err(). When
	// that is this call's own timeout firing (the parent ctx is still live),
	// the peer stopped sending flow-control beacons — report it as a wedge.
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return fmt.Errorf(
			"tth2: drainForSendCredit timed out after %v waiting for server "+
				"flow-control beacon (WINDOW_UPDATE / SETTINGS / PING); the "+
				"connection appears wedged",
			defaultDrainTimeout,
		)
	}
	return err
}

// writeTailReleaseFrames writes a DATA+END_STREAM frame for each stream that
// carries withheld trailing bytes. Streams with empty bodies are skipped
// because their HEADERS already carried END_STREAM.
//
// The final release atomically reserves enough connection and per-stream
// credit for every frame before writing the burst as one TLS record. A
// mid-burst drain would force a flush and split the record; over-sending would
// reset the offending stream with FLOW_CONTROL_ERROR per RFC 9113 section
// 6.9.1.
func tailReleaseNeeds(bodies [][]byte, pivots []int) ([]int32, error) {
	if len(pivots) != len(bodies) {
		return nil, fmt.Errorf("tth2: final body release has %d pivots for %d bodies",
			len(pivots), len(bodies))
	}
	needs := make([]int32, len(bodies))
	var recordBytes int64
	for i := range bodies {
		if pivots[i] < 0 || pivots[i] > len(bodies[i]) {
			return nil, fmt.Errorf("tth2: final body release stream %d has invalid pivot %d for length %d",
				i, pivots[i], len(bodies[i]))
		}
		need := len(bodies[i]) - pivots[i]
		if need == 0 {
			continue
		}
		if need > tlsRecordPlaintextBudget-http2FrameHeaderSize {
			return nil, fmt.Errorf("tth2: final body release stream %d needs %d bytes, exceeding one TLS plaintext record (%d)",
				i, need, tlsRecordPlaintextBudget)
		}
		recordBytes += http2FrameHeaderSize + int64(need)
		needs[i] = int32(need) //nolint:gosec // record budget is below MaxInt32
	}
	if recordBytes > tlsRecordPlaintextBudget {
		return nil, fmt.Errorf("tth2: final body release needs %d bytes, exceeding one TLS plaintext record (%d)",
			recordBytes, tlsRecordPlaintextBudget)
	}
	return needs, nil
}

func (c *http2conn) writeTailReleaseFrames(
	ctx context.Context,
	flow *flowControl,
	bodies [][]byte,
	pivots []int,
	needs []int32,
	responses *responseRead,
	record *recordWriter,
) error {
	if err := responses.sendFailure(); err != nil {
		return err
	}
	for !flow.reserve(needs) {
		if err := c.drainWithTimeout(ctx, flow, responses); err != nil {
			return err
		}
	}
	for i, sid := range flow.streamIDs {
		if needs[i] == 0 {
			continue
		}
		if err := responses.sendFailure(); err != nil {
			return err
		}
		tail := bodies[i][pivots[i]:]
		if err := record.begin(); err != nil {
			return err
		}
		if err := c.framer.WriteData(sid, true, tail); err != nil {
			return fmt.Errorf("write END_STREAM stream %d: %w", sid, err)
		}
	}
	return nil
}

// waitReleaseDelay blocks for d, returning early on ctx cancellation. d <= 0
// is a no-op — the common case, since a pause long enough to align handlers
// can also park the server's worker threads and reintroduce ordering jitter
// when they wake. See [WithReleaseDelay] for the purpose and the compromise.
func waitReleaseDelay(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return batchContextError(ctx)
	}
}

// drainForSendCredit waits for the lifetime read pump to report changed send
// credit. It also observes a stream reset, a connection failure, or caller
// cancellation without competing with that pump for frames.
func (c *http2conn) drainForSendCredit(
	ctx context.Context, flow *flowControl, responses *responseRead,
) error {
	select {
	case <-flow.changed:
		return nil
	case err := <-responses.sendErr:
		return err
	case <-responses.ctx.Done():
		return context.Cause(responses.ctx)
	case <-ctx.Done():
		return batchContextError(ctx)
	}
}

type indexedResult struct {
	i   int
	r   Result
	err error
}

// responseRead owns response collection from before request headers reach the
// wire until every response stream ends. The lifetime read pump routes early
// responses here even while the send goroutine waits for flow-control credit.
type responseRead struct {
	c              *http2conn
	ctx            context.Context
	cancel         context.CancelCauseFunc
	n              int
	resultsCh      chan indexedResult
	sendErr        chan error
	arrivals       int
	captureHeaders bool
	closeOnce      sync.Once
}

// sendFrame blocks until the stream collector accepts one copied response
// event. This preserves response bytes without allowing an unbounded queue.
// Its boolean reports whether the stream still had a collector; cancellation
// is returned as an error so a frame that was not delivered cannot be mistaken
// for a completed response.
func (c *http2conn) sendFrame(
	ctx context.Context, sid uint32, event frameEvent,
) (bool, error) {
	c.mu.Lock()
	coll, ok := c.collectors[sid]
	c.mu.Unlock()
	if !ok {
		return false, nil
	}
	select {
	case coll.ch <- event:
		return true, nil
	case <-ctx.Done():
		return false, context.Cause(ctx)
	}
}

func (c *http2conn) newResponseRead(
	ctx context.Context, streamIDs []uint32, cfg sendConfig,
) (*responseRead, error) {
	innerCtx, cancel := context.WithCancelCause(ctx)
	r := &responseRead{
		c: c, ctx: innerCtx, cancel: cancel, n: len(streamIDs),
		resultsCh:      make(chan indexedResult, len(streamIDs)),
		sendErr:        make(chan error, 1),
		captureHeaders: cfg.responseHeaderCapture,
	}
	c.mu.Lock()
	for i, sid := range streamIDs {
		coll := &streamCollector{ch: make(chan frameEvent, collectorChannelBuffer)}
		c.collectors[sid] = coll
		go func(idx int, sid uint32, coll *streamCollector) {
			result, err := c.collectStream(r, idx, sid, coll, cfg)
			r.resultsCh <- indexedResult{idx, result, err}
		}(i, sid, coll)
	}
	c.mu.Unlock()
	if err := c.readPump.activate(r); err != nil {
		r.abort(err)
		return nil, err
	}
	return r, nil
}

func (r *responseRead) failSend(err error) {
	select {
	case r.sendErr <- err:
	default:
	}
}

func (r *responseRead) sendFailure() error {
	if r == nil {
		return nil
	}
	select {
	case err := <-r.sendErr:
		return err
	default:
		return nil
	}
}

func (r *responseRead) dispatchFrame(frame http2.Frame) error {
	switch fr := frame.(type) {
	case *http2.MetaHeadersFrame:
		if fr.Truncated {
			return fmt.Errorf("tth2: response header section exceeds %d bytes",
				r.c.maxResponseHeaderBytes)
		}
		kind, response, trailer := projectResponseHeaderBlock(
			fr.Fields, r.captureHeaders)
		// The Framer owns this slice until the next ReadFrame call. That call may
		// block on a stalled body, so release the decoded fields immediately after
		// projecting the owned state above.
		fr.Fields = nil
		delivered, err := r.c.sendFrame(r.ctx, fr.StreamID, frameEvent{
			headerKind: kind,
			response:   response,
			trailer:    trailer,
			arrivalSeq: r.arrivals,
			endStream:  fr.StreamEnded(),
		})
		if err != nil {
			return err
		}
		r.arrivals++
		if fr.StreamEnded() && delivered {
			r.c.retireStream(fr.StreamID)
		}
	case *http2.DataFrame:
		data := make([]byte, len(fr.Data()))
		copy(data, fr.Data())
		delivered, err := r.c.sendFrame(r.ctx, fr.StreamID, frameEvent{
			data: data, endStream: fr.StreamEnded(),
		})
		if err != nil {
			return err
		}
		if n := uint32(len(data)); n > 0 { //nolint:gosec // DATA payload is bounded by the frame size
			if err := r.c.writeControl("response WINDOW_UPDATE", func() error {
				if !fr.StreamEnded() {
					if err := r.c.framer.WriteWindowUpdate(fr.StreamID, n); err != nil {
						return err
					}
				}
				return r.c.framer.WriteWindowUpdate(0, n)
			}); err != nil {
				return err
			}
		}
		if fr.StreamEnded() && delivered {
			r.c.retireStream(fr.StreamID)
		}
	}
	return nil
}

func (r *responseRead) dispatchReset(fr *http2.RSTStreamFrame) bool {
	return r.c.resetStream(fr.StreamID, fr.ErrCode)
}

func (r *responseRead) closeAll(cause error) {
	r.closeOnce.Do(func() {
		r.cancel(cause)
		r.c.mu.Lock()
		// Do not close collector channels here. The read pump may already have
		// selected one for delivery. Cancellation releases both sides without a
		// send-on-closed-channel race.
		r.c.collectors = make(map[uint32]*streamCollector)
		r.c.mu.Unlock()
	})
}

func (r *responseRead) abort(cause error) {
	r.closeAll(cause)
	for range r.n {
		<-r.resultsCh
	}
}

// readResponses continues a response read whose collectors were established
// before request dispatch. A connection-fatal error closes the TLS connection
// to unblock dispatch and returns nil results.
func (c *http2conn) readResponses(r *responseRead) ([]Result, error) {
	defer r.cancel(nil)

	// Two failures are tracked separately because they demand opposite handling
	// and either can arrive first. A stream-scoped error is confined to its own
	// stream: the siblings are still arriving and the dispatcher is still reading,
	// so leave both alone and let the trial finish. Anything else (ctx
	// cancellation, GOAWAY, IO failure) means the conn is dead — cancel the
	// collectors and close it so the lifetime read pump exits. Every error is
	// examined, not just the first:
	// a stream reset followed by a cancellation still has to close the connection.
	results := make([]Result, r.n)
	var streamErr, fatalErr error
	for range r.n {
		got := <-r.resultsCh
		if got.err != nil {
			var se *StreamError
			switch {
			case errors.As(got.err, &se):
				if streamErr == nil {
					streamErr = got.err
				}
			case fatalErr == nil:
				r.closeAll(got.err)
				fatalErr = context.Cause(r.ctx)
				if fatalErr == nil {
					fatalErr = got.err
				}
				_ = c.tlsConn.Close()
			}
		}
		results[got.i] = got.r
	}
	// A fatal error outranks a stream reset: the caller reads the returned error
	// to decide whether the connection may be reused, and a closed connection
	// must not be reported as a survivable reset.
	if fatalErr != nil {
		return nil, fatalErr
	}
	return results, streamErr
}

// collectStream drains one collector and builds its Result.
func (c *http2conn) collectStream(
	r *responseRead,
	requestIndex int,
	sid uint32,
	coll *streamCollector,
	cfg sendConfig,
) (Result, error) {
	ctx := r.ctx
	arrivalSeq := -1
	var response *Response
	var trailer http.Header
	body := responseBodyState{digest: sha256.New()}

	result := func() Result {
		response.Trailer = trailer
		response.Body.Captured = body.captured
		response.Body.BytesReceived = body.bytesAccepted
		copy(response.Body.SHA256[:], body.digest.Sum(nil))
		return Result{
			Response:   response,
			arrivalSeq: arrivalSeq,
			StreamID:   sid,
		}
	}
	complete := func() (Result, error) {
		if response == nil {
			return Result{}, fmt.Errorf(
				"tth2: stream %d ended before final response headers", sid)
		}
		return result(), nil
	}

	// Preserve an arrived response on reset; a pre-header reset has no response.
	reset := func(rst *StreamError) Result {
		if response == nil {
			return Result{Reset: rst, StreamID: sid}
		}
		r := result()
		r.Reset = rst
		return r
	}

	for {
		select {
		case ev, ok := <-coll.ch:
			if !ok {
				// Only a reset closes an individual collector channel. Connection-wide
				// failure cancels ctx, while clean completion is carried by END_STREAM.
				if rst := coll.rst.Load(); rst != nil {
					return reset(rst), rst
				}
				if cause := context.Cause(ctx); cause != nil {
					return Result{}, cause
				}
				return Result{}, errors.New(
					"tth2: collector channel closed without reset or cause")
			}
			switch ev.headerKind {
			case responseHeaderFinal:
				arrivalSeq = ev.arrivalSeq
				response = ev.response
			case responseHeaderTrailer:
				trailer = ev.trailer
			}
			if len(ev.data) > 0 {
				overflow, err := body.accept(
					ev.data, cfg.maxResponseBodyBytes,
					cfg.responseBodyCaptureBytes)
				if err != nil {
					return Result{}, err
				}
				if overflow {
					r.closeAll(&ResponseBodyLimitError{
						RequestIndex:  requestIndex,
						StreamID:      sid,
						Limit:         cfg.maxResponseBodyBytes,
						BytesAccepted: body.bytesAccepted,
					})
					return Result{}, context.Cause(ctx)
				}
			}
			if ev.endStream {
				return complete()
			}
		case <-ctx.Done():
			if cause := context.Cause(ctx); cause != nil {
				return Result{}, cause
			}
			return Result{}, ctx.Err()
		}
	}
}

type responseBodyState struct {
	digest        hash.Hash
	captured      []byte
	bytesAccepted int64
}

func (s *responseBodyState) accept(
	data []byte,
	maxResponseBytes int64,
	captureBytes int64,
) (overflow bool, err error) {
	accepted := data
	if maxResponseBytes > 0 {
		remaining := maxResponseBytes - s.bytesAccepted
		if int64(len(accepted)) > remaining {
			accepted = accepted[:remaining]
			overflow = true
		}
	}
	_, _ = s.digest.Write(accepted)
	if s.bytesAccepted > math.MaxInt64-int64(len(accepted)) {
		return false, errors.New(
			"tth2: response body byte count exceeds int64")
	}
	s.bytesAccepted += int64(len(accepted))
	remaining := captureBytes - int64(len(s.captured))
	if captureBytes == UnlimitedResponseBytes {
		remaining = int64(len(accepted))
	}
	if remaining <= 0 {
		return overflow, nil
	}
	n := min(int64(len(accepted)), remaining)
	maxInt := int64(^uint(0) >> 1)
	if int64(len(s.captured)) > maxInt-n {
		return false, errors.New(
			"tth2: captured response body exceeds addressable memory")
	}
	s.captured = append(s.captured, accepted[:n]...)
	return overflow, nil
}

// headerStatus returns a header block's :status value and parsed code. The
// boolean distinguishes response heads from trailer blocks. Conversion failure
// leaves code zero so a malformed response head is not mistaken for a trailer.
func headerStatus(fields []hpack.HeaderField) (value string, code int, ok bool) {
	for _, hf := range fields {
		if hf.Name == ":status" {
			code, _ = strconv.Atoi(hf.Value)
			return hf.Value, code, true
		}
	}
	return "", 0, false
}

// projectResponseHeaderBlock classifies an accepted decoded header block and
// immediately reduces it to the state a stream may retain. Informational
// headers retain nothing. Final responses retain status and declared length,
// plus an owned header map when capture is enabled. Trailers retain an owned
// map only when capture is enabled.
func projectResponseHeaderBlock(
	fields []hpack.HeaderField,
	captureHeaders bool,
) (responseHeaderKind, *Response, http.Header) {
	status, code, hasStatus := headerStatus(fields)
	switch {
	case !hasStatus:
		if !captureHeaders || len(fields) == 0 {
			return responseHeaderTrailer, nil, nil
		}
		return responseHeaderTrailer, nil, capturedHeader(fields)
	case code >= 100 && code < 200:
		return responseHeaderInformational, nil, nil
	}

	resp := &Response{
		Status:        status + " " + http.StatusText(code),
		StatusCode:    code,
		ContentLength: declaredContentLength(fields),
	}
	if captureHeaders {
		resp.Header = capturedHeader(fields)
	}
	return responseHeaderFinal, resp, nil
}

func capturedHeader(fields []hpack.HeaderField) http.Header {
	header := make(http.Header)
	for _, hf := range fields {
		if !strings.HasPrefix(hf.Name, ":") {
			// Header.Add canonicalises the key itself (via
			// textproto.CanonicalMIMEHeaderKey), so HTTP/2's lowercase wire names land
			// as Content-Type etc. without an explicit CanonicalHeaderKey call here.
			header.Add(hf.Name, strings.Clone(hf.Value))
		}
	}
	return header
}

// declaredContentLength returns the unambiguous non-negative Content-Length
// declared by the peer, or -1 when it is absent, malformed, or conflicting.
func declaredContentLength(fields []hpack.HeaderField) int64 {
	var length int64 = -1
	for _, field := range fields {
		if !strings.EqualFold(field.Name, "content-length") {
			continue
		}
		value := field.Value
		for part := range strings.SplitSeq(value, ",") {
			part = strings.TrimSpace(part)
			n, err := strconv.ParseInt(part, 10, 64)
			if err != nil || n < 0 {
				return -1
			}
			if length >= 0 && n != length {
				return -1
			}
			length = n
		}
	}
	return length
}

// retireStream removes a stream after its collector has accepted END_STREAM.
// The terminal event itself makes the collector return; channel closure is
// reserved for an out-of-band reset and cannot manufacture clean completion.
func (c *http2conn) retireStream(sid uint32) {
	c.mu.Lock()
	delete(c.collectors, sid)
	c.mu.Unlock()
}

// resetStream records that the server reset sid and closes its channel, leaving
// its siblings untouched. Reports whether the stream was one of this batch's; a
// false return means the RST names a stream this batch does not own.
//
// The reset travels on the collector rather than in a frameEvent so that it
// cannot block: a collector whose channel buffer is full would leave the
// dispatcher waiting to announce the end of a stream that is already over.
func (c *http2conn) resetStream(sid uint32, code http2.ErrCode) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	coll, ok := c.collectors[sid]
	if !ok {
		return false
	}
	coll.rst.Store(&StreamError{StreamID: sid, Code: code})
	close(coll.ch)
	delete(c.collectors, sid)
	return true
}

func (c *http2conn) close() {
	c.closeOnce.Do(func() {
		// Closing tlsConn unblocks the lifetime read pump. Waiting for it makes
		// connection disposal also dispose every goroutine that owns its state.
		_ = c.tlsConn.Close()
		if c.readPump != nil {
			<-c.readPump.done
		}
	})
}
