package timing

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// The connection-bound HTTP/2 transport owns framing and normal HPACK state.
// A prepared request mutates that state; abandoning it retires the connection.
// There is one sender, a persistent reader and a bounded control writer.
const (
	h2RecordLimit  = 16384
	h2ControlLimit = 64
	h2TableLimit   = 4096
)

var (
	errH2NotReady     = errors.New("HTTP/2 readiness changed")
	errH2ControlQueue = errors.New("HTTP/2 pending control queue exhausted")
)

type h2Milestones struct {
	Initial, Final, Written, FirstHeaders, Headers, Complete time.Time
}

type h2Observation struct {
	Times              h2Milestones
	Status             int
	Headers, Trailers  []hpack.HeaderField
	DiagnosticHeaders  []hpack.HeaderField
	diagnosticTrailers bool
	Body               []byte
	Err                error
}

type h2Record struct {
	Bytes                       []byte
	Data                        int64
	OpensHeaders, ClosesHeaders bool
}

type h2Prepared struct {
	Stream         uint32
	Epoch          uint64
	Prefix, Final  []h2Record
	Head           bool
	Sent           int
	Obs            h2Observation
	Length         int64
	Received       int64
	NoBody         bool
	Done           bool
	Reset          bool
	Strict         bool
	ReceiveWindow  int64
	DeclaredLength int64
	collector      *bodyCollector
	terminal       bool
	completion     func(time.Time) error
	completionDone bool
	responseValid  bool
}

type h2Conn struct {
	conn                   *tls.Conn
	mu                     sync.Mutex
	wire                   sync.Mutex
	changed                chan struct{}
	done                   chan struct{}
	readerDone, writerDone chan struct{}
	err                    error
	controls               chan []byte
	headerOpen             bool // guarded by mu; changed while holding wire
	ready                  bool
	retiring               bool
	retirementReason       string
	goAwayLast             uint32
	window                 int64
	receiveWindow          int64
	initialWindow          int64
	streamWindow           int64
	maxFrame               uint32
	maxStreams             uint32
	tableEpoch             uint64
	nextStream             uint32
	lastSentStream         uint32
	active                 *h2Prepared
	encoded                bytes.Buffer
	encoder                *hpack.Encoder
	bodyMax                int64
	headerMax              uint32
}

// h2Acquire uses an already connected TLS socket. The caller owns TCP/TLS
// setup and must disable dynamic record sizing before the handshake.
func h2Acquire(ctx context.Context, conn *tls.Conn, bodyMax int64) (*h2Conn, error) {
	return h2AcquireWithLimits(ctx, conn, 1<<20, bodyMax)
}

func h2AcquireWithLimits(ctx context.Context, conn *tls.Conn, headerMax uint32, bodyMax int64) (*h2Conn, error) {
	if conn.ConnectionState().NegotiatedProtocol != "h2" {
		return nil, errors.New("HTTP/2 was not negotiated")
	}
	c := &h2Conn{
		conn: conn, changed: make(chan struct{}), done: make(chan struct{}),
		readerDone: make(chan struct{}), writerDone: make(chan struct{}),
		controls: make(chan []byte, h2ControlLimit), window: 65535, receiveWindow: 65535,
		initialWindow: 65535, maxFrame: 16384, maxStreams: ^uint32(0),
		nextStream: 1, bodyMax: bodyMax, headerMax: headerMax,
	}
	c.encoder = hpack.NewEncoder(&c.encoded)
	pumpsStarted, acquired := false, false
	defer func() {
		if !acquired {
			c.close(errors.New("HTTP/2 acquisition failed"))
			if pumpsStarted {
				<-c.readerDone
				<-c.writerDone
			}
		}
	}()
	stop := context.AfterFunc(ctx, func() { c.close(ctx.Err()) })
	defer stop()
	var setup bytes.Buffer
	setup.WriteString(http2.ClientPreface)
	f := http2.NewFramer(&setup, nil)
	if err := f.WriteSettings(http2.Setting{ID: http2.SettingEnablePush, Val: 0}); err != nil {
		return nil, err
	}
	if _, err := conn.Write(setup.Bytes()); err != nil {
		c.close(err)
		return nil, err
	}
	go c.readPump()
	go c.controlPump()
	pumpsStarted = true
	for {
		c.mu.Lock()
		ready, err, changed := c.ready, c.err, c.changed
		c.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if ready {
			acquired = true
			return c, nil
		}
		select {
		case <-ctx.Done():
			c.close(ctx.Err())
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (c *h2Conn) signalLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// Completion hooks run at the terminal transport boundary before terminal
// body retention. They never perform I/O or re-enter connection state.
func (p *h2Prepared) completeLocked() {
	if !p.terminal || p.Obs.Times.Written.IsZero() || p.completionDone {
		return
	}
	p.completionDone = true
	if p.completion != nil {
		if err := p.completion(time.Now()); err != nil && p.Obs.Err == nil {
			p.Obs.Err = err
		}
	}
}

func (p *h2Prepared) acceptBody(data []byte) {
	if p.collector != nil {
		p.collector.accept(data)
	} else {
		p.Obs.Body = append(p.Obs.Body, data...)
	}
}

func (p *h2Prepared) acceptedBytes() int64 {
	if p.collector != nil {
		return p.collector.accepted
	}
	return int64(len(p.Obs.Body))
}

func (c *h2Conn) close(err error) {
	c.mu.Lock()
	if c.err == nil {
		if err == nil {
			err = io.EOF
		}
		c.err = err
		if c.active != nil && (!c.active.Done || c.active.Obs.Times.Written.IsZero()) {
			if c.active.Obs.Err == nil {
				c.active.Obs.Err = err
			}
			c.active.Done = true
			c.active.terminal = true
		}
		close(c.done)
		c.signalLocked()
	}
	c.mu.Unlock()
	_ = c.conn.Close()
}

// h2Prepare encodes on the assigned connection before authorising release.
// Peer table changes invalidate unsent bytes; they are never silently sent.
func (c *h2Conn) h2Prepare(fields []hpack.HeaderField, body []byte, strict, last bool) (*h2Prepared, error) {
	declared, err := h2ValidateRequest(fields)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	if c.retiring {
		return nil, errors.New("HTTP/2 connection is retiring")
	}
	if c.active != nil {
		return nil, errors.New("HTTP/2 connection already has an active request")
	}
	if c.nextStream > 0x7fffffff {
		return nil, errors.New("HTTP/2 stream IDs exhausted")
	}
	p := &h2Prepared{Stream: c.nextStream, Epoch: c.tableEpoch, Length: -1, Strict: strict, ReceiveWindow: 65535}
	p.DeclaredLength = declared
	c.nextStream += 2
	c.active = p
	c.streamWindow = c.initialWindow
	c.encoded.Reset()
	for _, field := range fields {
		if field.Name == ":method" {
			p.Head = field.Value == "HEAD"
		}
		if err := c.encoder.WriteField(field); err != nil {
			return nil, err
		}
	}
	block := bytes.Clone(c.encoded.Bytes())
	if len(block) == 0 {
		return nil, errors.New("HTTP/2 request header block is empty")
	}
	var finalHeader []byte
	if last && len(body) == 0 {
		finalHeader = block[len(block)-1:]
		block = block[:len(block)-1]
	}
	maxPayload := min(int(c.maxFrame), h2RecordLimit-9)
	first := true
	for first || len(block) > 0 {
		n := min(len(block), maxPayload)
		end := n == len(block) && finalHeader == nil
		var b bytes.Buffer
		f := http2.NewFramer(&b, nil)
		var err error
		if first {
			err = f.WriteHeaders(http2.HeadersFrameParam{
				StreamID:      p.Stream,
				BlockFragment: block[:n], EndHeaders: end, EndStream: len(body) == 0,
			})
		} else {
			err = f.WriteContinuation(p.Stream, end, block[:n])
		}
		if err != nil {
			return nil, err
		}
		p.Prefix = append(p.Prefix, h2Record{
			Bytes:        bytes.Clone(b.Bytes()),
			OpensHeaders: first && !end, ClosesHeaders: end,
		})
		block = block[n:]
		first = false
	}
	if finalHeader != nil {
		var b bytes.Buffer
		if err := http2.NewFramer(&b, nil).WriteContinuation(p.Stream, true, finalHeader); err != nil {
			return nil, err
		}
		p.Final = []h2Record{{Bytes: bytes.Clone(b.Bytes()), ClosesHeaders: true}}
	}
	data := body
	if last && len(body) > 0 {
		data = body[:len(body)-1]
	}
	for len(data) > 0 {
		n := min(len(data), maxPayload)
		var b bytes.Buffer
		if err := http2.NewFramer(&b, nil).WriteData(p.Stream, !last && n == len(data), data[:n]); err != nil {
			return nil, err
		}
		p.Prefix = append(p.Prefix, h2Record{Bytes: bytes.Clone(b.Bytes()), Data: int64(n)})
		data = data[n:]
	}
	if last && len(body) > 0 {
		var b bytes.Buffer
		if err := http2.NewFramer(&b, nil).WriteData(p.Stream, true, body[len(body)-1:]); err != nil {
			return nil, err
		}
		p.Final = []h2Record{{Bytes: bytes.Clone(b.Bytes()), Data: 1}}
	}
	if strict {
		var record h2Record
		for _, part := range p.Prefix {
			record.Bytes = append(record.Bytes, part.Bytes...)
			record.Data += part.Data
			record.OpensHeaders = record.OpensHeaders || part.OpensHeaders
			record.ClosesHeaders = record.ClosesHeaders || part.ClosesHeaders
		}
		if len(record.Bytes) > h2RecordLimit {
			return nil, fmt.Errorf("HTTP/2 encoded request exceeds %d-byte record limit", h2RecordLimit)
		}
		p.Prefix = []h2Record{record}
	}
	return p, nil
}

// h2ValidateRequest is pure preflight for a materialised HTTPS request. An
// absent declared Content-Length is -1. A valid declaration is preserved even
// when it differs from actual body bytes; framing uses those actual bytes.
func h2ValidateRequest(fields []hpack.HeaderField) (int64, error) {
	length := int64(-1)
	pseudo := make(map[string]string, 4)
	regular := false
	for _, field := range fields {
		if !httpguts.ValidHeaderFieldValue(field.Value) || strings.TrimSpace(field.Value) != field.Value {
			return -1, errors.New("HTTP/2 invalid request field value")
		}
		if strings.HasPrefix(field.Name, ":") {
			if regular {
				return -1, errors.New("HTTP/2 request pseudo-header follows ordinary field")
			}
			if _, present := pseudo[field.Name]; present {
				return -1, errors.New("HTTP/2 repeated request pseudo-header")
			}
			switch field.Name {
			case ":method", ":scheme", ":authority", ":path":
				if field.Value == "" {
					return -1, errors.New("HTTP/2 empty request pseudo-header")
				}
			default:
				return -1, errors.New("HTTP/2 unsupported request pseudo-header")
			}
			pseudo[field.Name] = field.Value
			continue
		}
		regular = true
		if strings.ToLower(field.Name) != field.Name || !httpguts.ValidHeaderFieldName(field.Name) {
			return -1, errors.New("HTTP/2 invalid request field name")
		}
		switch field.Name {
		case "transfer-encoding":
			return -1, errors.New("HTTP/2 request Transfer-Encoding is unsupported")
		case "connection", "proxy-connection", "keep-alive", "upgrade":
			return -1, errors.New("HTTP/2 connection-specific request field is unsupported")
		case "te":
			if !strings.EqualFold(field.Value, "trailers") {
				return -1, errors.New("HTTP/2 request TE must be trailers")
			}
		case "content-length":
			if field.Value == "" || strings.IndexFunc(field.Value, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
				return -1, errors.New("HTTP/2 invalid request Content-Length")
			}
			n, err := strconv.ParseInt(field.Value, 10, 64)
			if err != nil || length >= 0 && length != n {
				return -1, errors.New("HTTP/2 conflicting request Content-Length")
			}
			length = n
		}
	}
	for _, name := range []string{":method", ":scheme", ":authority", ":path"} {
		if _, present := pseudo[name]; !present {
			return -1, fmt.Errorf("HTTP/2 missing request %s", name)
		}
	}
	method := pseudo[":method"]
	if !httpguts.ValidHeaderFieldName(method) {
		return -1, errors.New("HTTP/2 invalid request method")
	}
	if method == "CONNECT" {
		return -1, errors.New("HTTP/2 CONNECT tunnels are unsupported")
	}
	if pseudo[":scheme"] != "https" {
		return -1, errors.New("HTTP/2 requires HTTPS")
	}
	authority, err := url.Parse("//" + pseudo[":authority"])
	if !httpguts.ValidHostHeader(pseudo[":authority"]) || err != nil || authority.Hostname() == "" ||
		authority.User != nil || authority.Path != "" || authority.RawQuery != "" || authority.Fragment != "" {
		return -1, errors.New("HTTP/2 invalid request authority")
	}
	path := pseudo[":path"]
	if path != "*" && !strings.HasPrefix(path, "/") || strings.ContainsAny(path, " \t#") ||
		path == "*" && method != "OPTIONS" {
		return -1, errors.New("HTTP/2 invalid request path")
	}
	return length, nil
}

func (c *h2Conn) readyLocked(p *h2Prepared, record h2Record) error {
	if c.err != nil {
		return c.err
	}
	if c.active != p {
		return errors.New("HTTP/2 request does not own connection")
	}
	if c.retiring && p.Sent == 0 {
		return errors.New("HTTP/2 connection retired before request release")
	}
	if p.Reset {
		return errors.New("HTTP/2 stream was reset")
	}
	if p.Epoch != c.tableEpoch && p.Sent == 0 {
		return errors.New("HTTP/2 prepared HPACK state was invalidated")
	}
	if (c.maxStreams == 0 && p.Sent == 0) ||
		record.Data > 0 && (c.window < record.Data || c.streamWindow < record.Data) {
		return errH2NotReady
	}
	return nil
}

func (c *h2Conn) h2WaitReady(ctx context.Context, p *h2Prepared, record h2Record) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		err, changed := c.readyLocked(p, record), c.changed
		c.mu.Unlock()
		if !errors.Is(err, errH2NotReady) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

// h2Transfer permits incremental DATA progress in multi-record mode. Each
// actual write remains a separately prepared, bounded TLS application record.
// A coordinated sender uses h2Write for its indivisible controlled record.
func (c *h2Conn) h2Transfer(ctx context.Context, p *h2Prepared, records []h2Record, ending bool) error {
	return c.h2TransferWithRelease(ctx, p, records, ending, nil)
}

func (c *h2Conn) h2TransferWithRelease(ctx context.Context, p *h2Prepared, records []h2Record,
	ending bool, beforeInitial func(time.Time) error,
) error {
	for index, record := range records {
		last := ending && index == len(records)-1
		if p.Strict || record.Data == 0 {
			if err := c.h2WaitReady(ctx, p, record); err != nil {
				return err
			}
			if err := c.h2WriteWithRelease(ctx, p, record, last, beforeInitial); err != nil {
				return err
			}
			continue
		}
		data := record.Bytes[9:]
		endStream := record.Bytes[4]&byte(http2.FlagDataEndStream) != 0
		for len(data) > 0 {
			if err := c.h2WaitReady(ctx, p, h2Record{Data: 1}); err != nil {
				return err
			}
			c.mu.Lock()
			n := min(int64(len(data)), c.window, c.streamWindow, int64(c.maxFrame), int64(h2RecordLimit-9))
			c.mu.Unlock()
			if n <= 0 {
				continue
			}
			var b bytes.Buffer
			if err := http2.NewFramer(&b, nil).WriteData(p.Stream, endStream && int(n) == len(data), data[:n]); err != nil {
				return err
			}
			part := h2Record{Bytes: bytes.Clone(b.Bytes()), Data: n}
			err := c.h2WriteWithRelease(ctx, p, part, last && int(n) == len(data), beforeInitial)
			if errors.Is(err, errH2NotReady) {
				continue
			}
			if err != nil {
				return err
			}
			data = data[n:]
		}
	}
	return nil
}

// h2Write revalidates credit under exclusive wire ownership. Readiness changes
// return to the coordinator before release; no controlled write is split.
func (c *h2Conn) h2Write(ctx context.Context, p *h2Prepared, record h2Record, final bool) error {
	return c.h2WriteWithRelease(ctx, p, record, final, nil)
}

// The callback transfers the remaining readiness budget to execution. It runs
// once under acquired wire ownership and must perform no I/O or engine calls.
func (c *h2Conn) h2WriteWithRelease(ctx context.Context, p *h2Prepared, record h2Record,
	final bool, beforeInitial func(time.Time) error,
) error {
	stop := context.AfterFunc(ctx, func() { c.close(ctx.Err()) })
	defer stop()
	c.wire.Lock()
	defer c.wire.Unlock()
	c.mu.Lock()
	if err := c.readyLocked(p, record); err != nil {
		c.mu.Unlock()
		return err
	}
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return err
	}
	now := time.Now()
	if p.Sent == 0 {
		if beforeInitial != nil {
			if err := beforeInitial(now); err != nil {
				c.mu.Unlock()
				return err
			}
		}
		p.Obs.Times.Initial = now
		c.lastSentStream = p.Stream
	}
	c.window -= record.Data
	c.streamWindow -= record.Data
	if record.OpensHeaders {
		c.headerOpen = true
	}
	if final {
		p.Obs.Times.Final = now
	}
	p.Sent++
	c.mu.Unlock()
	n, err := c.conn.Write(record.Bytes)
	if err == nil && n != len(record.Bytes) {
		err = io.ErrShortWrite
	}
	if err != nil {
		c.close(err)
		return err
	}
	c.mu.Lock()
	if record.ClosesHeaders {
		c.headerOpen = false
		c.signalLocked()
	}
	if final {
		p.Obs.Times.Written = time.Now()
		p.completeLocked()
	}
	c.mu.Unlock()
	return nil
}

func (c *h2Conn) h2WaitResponse(ctx context.Context, p *h2Prepared) h2Observation {
	for {
		c.mu.Lock()
		obs, done, changed := p.Obs, p.Done, c.changed
		c.mu.Unlock()
		if done {
			return obs
		}
		select {
		case <-ctx.Done():
			c.close(ctx.Err())
		case <-changed:
		}
	}
}

func (c *h2Conn) h2Release(p *h2Prepared) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != p || !p.Done || p.Obs.Times.Written.IsZero() {
		return errors.New("HTTP/2 incomplete exchange cannot be reused")
	}
	c.active = nil
	return nil
}

func (c *h2Conn) queueControl(build func(*http2.Framer) error) error {
	var b bytes.Buffer
	if err := build(http2.NewFramer(&b, nil)); err != nil {
		return err
	}
	select {
	case c.controls <- bytes.Clone(b.Bytes()):
		return nil
	case <-c.done:
		return io.EOF
	default:
		return errH2ControlQueue
	}
}

func (c *h2Conn) controlPump() {
	defer close(c.writerDone)
	for {
		var data []byte
		select {
		case <-c.done:
			return
		case data = <-c.controls:
		}
		for {
			c.wire.Lock()
			c.mu.Lock()
			open, changed, err := c.headerOpen, c.changed, c.err
			c.mu.Unlock()
			if err != nil {
				c.wire.Unlock()
				return
			}
			if !open {
				if len(data) == 13 && data[3] == byte(http2.FrameWindowUpdate) {
					stream := uint32(data[5])<<24 | uint32(data[6])<<16 | uint32(data[7])<<8 | uint32(data[8])
					credit := int64(uint32(data[9])<<24 | uint32(data[10])<<16 | uint32(data[11])<<8 | uint32(data[12]))
					c.mu.Lock()
					if stream == 0 {
						c.receiveWindow += credit
					} else if c.active != nil && c.active.Stream == stream {
						c.active.ReceiveWindow += credit
					}
					c.mu.Unlock()
				}
				_, err = c.conn.Write(data)
				c.wire.Unlock()
				if err != nil {
					c.close(err)
					return
				}
				break
			}
			c.wire.Unlock()
			select {
			case <-c.done:
				return
			case <-changed:
			}
		}
	}
}

func (c *h2Conn) readPump() {
	defer close(c.readerDone)
	f := http2.NewFramer(nil, c.conn)
	f.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	f.MaxHeaderListSize = c.headerMax
	f.SetMaxReadFrameSize(16384)
	for {
		header, err := f.ReadFrameHeader()
		if err == nil && header.Type == http2.FrameHeaders {
			c.mu.Lock()
			if p := c.active; p != nil && p.Sent > 0 && p.Stream == header.StreamID && p.Obs.Times.FirstHeaders.IsZero() {
				p.Obs.Times.FirstHeaders = time.Now()
				c.signalLocked()
			}
			c.mu.Unlock()
		}
		var frame http2.Frame
		if err == nil {
			frame, err = f.ReadFrameForHeader(header)
		}
		if err == nil {
			err = c.readFrame(frame)
		}
		if err != nil {
			if streamErr, ok := errors.AsType[http2.StreamError](err); ok {
				if err = c.streamFailure(streamErr.StreamID, streamErr); err == nil {
					continue
				}
			}
			c.close(err)
			return
		}
	}
}

func (c *h2Conn) readFrame(frame http2.Frame) error {
	c.mu.Lock()
	ready := c.ready
	c.mu.Unlock()
	if !ready {
		f, ok := frame.(*http2.SettingsFrame)
		if !ok || f.IsAck() {
			return errors.New("HTTP/2 peer did not begin with SETTINGS")
		}
	}
	switch f := frame.(type) {
	case *http2.SettingsFrame:
		if f.IsAck() {
			return nil
		}
		c.mu.Lock()
		err := f.ForeachSetting(func(s http2.Setting) error {
			if err := s.Valid(); err != nil {
				return err
			}
			switch s.ID {
			case http2.SettingHeaderTableSize:
				budget := min(s.Val, uint32(h2TableLimit))
				c.encoder.SetMaxDynamicTableSizeLimit(budget)
				c.encoder.SetMaxDynamicTableSize(budget)
				c.tableEpoch++
			case http2.SettingInitialWindowSize:
				c.streamWindow += int64(s.Val) - c.initialWindow
				c.initialWindow = int64(s.Val)
				if c.streamWindow > 0x7fffffff {
					return http2.ConnectionError(http2.ErrCodeFlowControl)
				}
			case http2.SettingMaxFrameSize:
				c.maxFrame = s.Val
			case http2.SettingMaxConcurrentStreams:
				c.maxStreams = s.Val
			case http2.SettingEnablePush:
				return errors.New("HTTP/2 server sent ENABLE_PUSH")
			}
			return nil
		})
		c.ready = true
		c.signalLocked()
		c.mu.Unlock()
		if err != nil {
			return err
		}
		return c.queueControl(func(w *http2.Framer) error { return w.WriteSettingsAck() })
	case *http2.PingFrame:
		if !f.IsAck() {
			return c.queueControl(func(w *http2.Framer) error { return w.WritePing(true, f.Data) })
		}
	case *http2.WindowUpdateFrame:
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.closedStreamLocked(f.StreamID) {
			return nil
		}
		var window *int64
		if f.StreamID == 0 {
			window = &c.window
		} else if c.active != nil && f.StreamID == c.active.Stream {
			if c.active.Sent == 0 {
				return http2.ConnectionError(http2.ErrCodeProtocol)
			}
			window = &c.streamWindow
		} else if !c.closedStreamLocked(f.StreamID) {
			return http2.ConnectionError(http2.ErrCodeProtocol)
		}
		if window != nil {
			*window += int64(f.Increment)
			if *window > 0x7fffffff {
				if f.StreamID == 0 {
					return http2.ConnectionError(http2.ErrCodeFlowControl)
				}
				return http2.StreamError{
					StreamID: f.StreamID, Code: http2.ErrCodeFlowControl,
					Cause: errors.New("HTTP/2 stream flow-control window overflow"),
				}
			}
			c.signalLocked()
		}
	case *http2.RSTStreamFrame:
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.closedStreamLocked(f.StreamID) {
			return nil
		}
		if c.active != nil && f.StreamID == c.active.Stream {
			if c.active.Sent == 0 {
				return http2.ConnectionError(http2.ErrCodeProtocol)
			}
			c.active.Reset, c.active.Done = true, true
			c.active.Obs.Err = fmt.Errorf("HTTP/2 stream reset: %s", f.ErrCode)
			c.active.terminal = true
			c.active.completeLocked()
			c.signalLocked()
		} else if !c.closedStreamLocked(f.StreamID) {
			return http2.ConnectionError(http2.ErrCodeProtocol)
		}
	case *http2.GoAwayFrame:
		c.mu.Lock()
		if c.retiring && f.LastStreamID > c.goAwayLast {
			c.mu.Unlock()
			return http2.ConnectionError(http2.ErrCodeProtocol)
		}
		c.retiring = true
		c.goAwayLast = f.LastStreamID
		c.retirementReason = fmt.Sprintf("HTTP/2 GOAWAY: %s, last stream %d", f.ErrCode, f.LastStreamID)
		p := c.active
		c.signalLocked()
		c.mu.Unlock()
		if p != nil && p.Stream > f.LastStreamID {
			return c.streamFailure(p.Stream, errors.New("HTTP/2 stream not accepted before GOAWAY"))
		}
	case *http2.MetaHeadersFrame:
		if err := c.responseHeaders(f); err != nil {
			return c.streamFailure(f.StreamID, err)
		}
	case *http2.DataFrame:
		if err := c.responseData(f); err != nil {
			if errors.Is(err, errH2ControlQueue) {
				return err
			}
			if _, ok := errors.AsType[http2.ConnectionError](err); ok {
				return err
			}
			return c.streamFailure(f.StreamID, err)
		}
	case *http2.PushPromiseFrame:
		return errors.New("HTTP/2 unsolicited push promise")
	}
	return nil
}

// Sequential stream IDs require only a high-water mark to distinguish a
// previously sent closed stream from an idle or server-initiated stream.
func (c *h2Conn) closedStreamLocked(stream uint32) bool {
	return stream != 0 && stream&1 == 1 && stream <= c.lastSentStream &&
		(c.active == nil || c.active.Stream != stream || c.active.Reset ||
			c.active.Done && !c.active.Obs.Times.Written.IsZero())
}

func (c *h2Conn) streamFailure(stream uint32, err error) error {
	c.mu.Lock()
	if c.closedStreamLocked(stream) {
		c.mu.Unlock()
		return nil
	}
	if c.active == nil || c.active.Stream != stream || c.active.Sent == 0 {
		c.mu.Unlock()
		return http2.ConnectionError(http2.ErrCodeProtocol)
	}
	if p := c.active; p != nil && p.Stream == stream {
		p.Obs.Err = err
		p.Reset, p.Done = true, true
		p.terminal = true
		p.completeLocked()
		c.signalLocked()
	}
	c.mu.Unlock()
	code := http2.ErrCodeProtocol
	if streamErr, ok := errors.AsType[http2.StreamError](err); ok {
		code = streamErr.Code
	}
	return c.queueControl(func(f *http2.Framer) error { return f.WriteRSTStream(stream, code) })
}

func h2ValidFields(fields []hpack.HeaderField, trailers bool) (int, int64, error) {
	status, length := 0, int64(-1)
	regular := false
	for _, field := range fields {
		if field.Name == ":status" && !trailers {
			if regular || status != 0 || len(field.Value) != 3 {
				return 0, -1, errors.New("HTTP/2 invalid response status")
			}
			n, err := strconv.Atoi(field.Value)
			if err != nil || n < 100 || n > 599 || n == 101 {
				return 0, -1, errors.New("HTTP/2 invalid response status")
			}
			status = n
			continue
		}
		regular = true
		if strings.HasPrefix(field.Name, ":") || strings.ToLower(field.Name) != field.Name ||
			!httpguts.ValidHeaderFieldName(field.Name) || !httpguts.ValidHeaderFieldValue(field.Value) ||
			strings.TrimSpace(field.Value) != field.Value {
			return 0, -1, errors.New("HTTP/2 invalid response field")
		}
		if trailers && !httpguts.ValidTrailerHeader(field.Name) {
			return 0, -1, errors.New("HTTP/2 prohibited trailer field")
		}
		switch field.Name {
		case "connection", "proxy-connection", "keep-alive", "transfer-encoding", "upgrade", "te":
			return 0, -1, errors.New("HTTP/2 prohibited response field")
		case "content-length":
			if trailers {
				return 0, -1, errors.New("HTTP/2 content-length in trailers")
			}
			if field.Value == "" || strings.IndexFunc(field.Value, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
				return 0, -1, errors.New("HTTP/2 invalid response content-length")
			}
			n, err := strconv.ParseInt(field.Value, 10, 64)
			if err != nil || n < 0 || (length >= 0 && length != n) {
				return 0, -1, errors.New("HTTP/2 invalid response content-length")
			}
			length = n
		}
	}
	if !trailers && status == 0 {
		return 0, -1, errors.New("HTTP/2 missing response status")
	}
	return status, length, nil
}

func (c *h2Conn) responseHeaders(f *http2.MetaHeadersFrame) (err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.active
	if c.closedStreamLocked(f.StreamID) {
		return nil
	}
	if p == nil || p.Sent == 0 || f.StreamID != p.Stream || p.Done {
		return errors.New("HTTP/2 unexpected response headers")
	}
	if p.Obs.Times.FirstHeaders.IsZero() {
		p.Obs.Times.FirstHeaders = time.Now()
	}
	trailers := !p.Obs.Times.Headers.IsZero()
	defer func() {
		if err != nil {
			p.terminal = true
			p.completeLocked()
		}
		p.Obs.DiagnosticHeaders = append([]hpack.HeaderField(nil), f.Fields...)
		p.Obs.diagnosticTrailers = trailers
	}()
	status, length, err := h2ValidFields(f.Fields, trailers)
	if err != nil {
		if f.StreamEnded() {
			p.Obs.Times.Complete = time.Now()
		}
		return err
	}
	if f.Truncated {
		if f.StreamEnded() {
			p.Obs.Times.Complete = time.Now()
		}
		return errors.New("HTTP/2 response headers exceed limit")
	}
	if trailers {
		if !f.StreamEnded() {
			return errors.New("HTTP/2 trailers do not end stream")
		}
		p.Obs.Times.Complete = time.Now()
		defer func() { p.Obs.Trailers = append([]hpack.HeaderField(nil), f.Fields...) }()
	} else if status < 200 {
		if f.StreamEnded() {
			p.Obs.Times.Complete = time.Now()
			return errors.New("HTTP/2 informational response ends stream")
		}
		return nil
	} else {
		p.NoBody = p.Head || status == 204 || status == 304
		p.Length = length
		p.Obs.Times.Headers = time.Now()
		if f.StreamEnded() {
			p.Obs.Times.Complete = time.Now()
		}
		p.Obs.Status = status
		defer func() { p.Obs.Headers = append([]hpack.HeaderField(nil), f.Fields...) }()
	}
	if f.StreamEnded() {
		return c.responseEndLocked(p)
	}
	c.signalLocked()
	return nil
}

func (c *h2Conn) responseEndLocked(p *h2Prepared) error {
	if p.Obs.Times.Complete.IsZero() {
		p.Obs.Times.Complete = time.Now()
	}
	if !p.NoBody && p.Length >= 0 && p.Received != p.Length {
		p.terminal = true
		p.completeLocked()
		return errors.New("HTTP/2 response content-length mismatch")
	}
	p.Done = true
	p.responseValid = true
	p.terminal = true
	p.completeLocked()
	c.signalLocked()
	return nil
}

func (c *h2Conn) responseData(f *http2.DataFrame) (err error) {
	c.mu.Lock()
	p := c.active
	closed := c.closedStreamLocked(f.StreamID)
	if !closed && (p == nil || p.Sent == 0 || f.StreamID != p.Stream) {
		c.mu.Unlock()
		return http2.ConnectionError(http2.ErrCodeProtocol)
	}
	if !closed {
		p.Received += int64(len(f.Data()))
		if f.StreamEnded() {
			p.Obs.Times.Complete = time.Now()
		}
	}
	c.receiveWindow -= int64(f.Length)
	if c.receiveWindow < 0 {
		c.mu.Unlock()
		return http2.ConnectionError(http2.ErrCodeFlowControl)
	}
	if closed {
		c.mu.Unlock()
		if f.Length == 0 {
			return nil
		}
		return c.queueControl(func(w *http2.Framer) error { return w.WriteWindowUpdate(0, f.Length) })
	}
	// Stream failure still consumes connection credit. Reclaim it before the
	// caller queues RST_STREAM, so repeated failures cannot starve later work.
	restore := true
	defer func() {
		if restore && err != nil && f.Length > 0 {
			if updateErr := c.queueControl(func(w *http2.Framer) error {
				return w.WriteWindowUpdate(0, f.Length)
			}); updateErr != nil {
				err = updateErr
			}
		}
	}()
	if p.Done || p.Obs.Times.Headers.IsZero() {
		c.mu.Unlock()
		return errors.New("HTTP/2 unexpected response DATA")
	}
	data := f.Data()
	p.ReceiveWindow -= int64(f.Length)
	if p.ReceiveWindow < 0 {
		c.mu.Unlock()
		return http2.StreamError{
			StreamID: p.Stream, Code: http2.ErrCodeFlowControl,
			Cause: errors.New("HTTP/2 response exceeds stream flow-control window"),
		}
	}
	if p.NoBody && len(data) > 0 {
		c.mu.Unlock()
		return errors.New("HTTP/2 bodyless response has DATA")
	}
	if c.bodyMax >= 0 && p.Received > c.bodyMax {
		p.terminal = true
		p.completeLocked()
		remaining := c.bodyMax - p.acceptedBytes()
		if remaining > 0 {
			p.acceptBody(data[:min(remaining, int64(len(data)))])
		}
		c.mu.Unlock()
		return errors.New("HTTP/2 response body exceeds limit")
	}
	if f.StreamEnded() {
		if err := c.responseEndLocked(p); err != nil {
			p.acceptBody(data)
			c.mu.Unlock()
			return err
		}
	}
	// Completion is observed before retention and downstream consumers.
	p.acceptBody(data)
	c.mu.Unlock()
	restore = false
	if f.Length > 0 {
		if err := c.queueControl(func(w *http2.Framer) error { return w.WriteWindowUpdate(0, f.Length) }); err != nil {
			return err
		}
		return c.queueControl(func(w *http2.Framer) error { return w.WriteWindowUpdate(f.StreamID, f.Length) })
	}
	return nil
}
