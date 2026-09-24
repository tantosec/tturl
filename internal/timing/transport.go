package timing

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2/hpack"
)

// acquireTransport binds protocol setup to the acquisition budget. Temporary
// acquisition cancellation ownership ends before the lease is returned.
func acquireTransport(ctx context.Context, request Request, plan Plan) (transportLease, error) {
	if request.HTTP == nil || request.HTTP.URL == nil {
		return nil, errors.New("missing request URL")
	}
	url := request.HTTP.URL
	port := url.Port()
	if port == "" {
		port = "443"
		if url.Scheme == "http" {
			port = "80"
		}
	}
	if request.Protocol != HTTP11 && request.Protocol != HTTP2 {
		return nil, errors.New("unsupported HTTP protocol")
	}
	plainInvalid := url.Scheme != "http" || request.Protocol != HTTP11 || plan.SingleRecord || plan.LastByteSync
	if url.Scheme != "https" && plainInvalid {
		return nil, errors.New("plain HTTP requires HTTP/1.1 without controlled TLS delivery")
	}
	if _, err := h1ReaderSize(plan.ReceiveHeaderMax); err != nil {
		return nil, err
	}
	var headerMax uint32
	if request.Protocol == HTTP2 {
		limit := plan.ReceiveHeaderMax
		if limit < 1 || limit > int64(^uint32(0)) {
			return nil, errors.New("HTTP/2 response header limit is out of range")
		}
		headerMax = uint32(limit)
	}
	var config *tls.Config
	if url.Scheme == "https" {
		config = &tls.Config{MinVersion: tls.VersionTLS12}
		if plan.TLSConfig != nil {
			config = plan.TLSConfig.Clone()
		}
		if config.MaxVersion != 0 && config.MaxVersion < tls.VersionTLS12 {
			return nil, errors.New("controlled TLS delivery requires TLS 1.2 or later")
		}
		config.MinVersion = max(config.MinVersion, uint16(tls.VersionTLS12))
		config.NextProtos = []string{string(request.Protocol)}
	}
	var h2 *h2Conn
	conn, err := acquireSocket(ctx, net.JoinHostPort(url.Hostname(), port), config, plan.RequestTimeout,
		func(setupCtx context.Context, conn net.Conn) error {
			if request.Protocol == HTTP2 {
				secure, ok := conn.(*tls.Conn)
				if !ok {
					return errors.New("HTTP/2 requires TLS")
				}
				var err error
				h2, err = h2AcquireWithLimits(setupCtx, secure, headerMax, plan.ResponseBodyMax)
				return err
			}
			if secure, ok := conn.(*tls.Conn); ok {
				protocol := secure.ConnectionState().NegotiatedProtocol
				if protocol != "" && protocol != string(HTTP11) {
					return errors.New("HTTP/1.1 was not negotiated")
				}
			}
			return nil
		})
	if err != nil {
		if h2 != nil {
			h2.close(err)
			<-h2.readerDone
			<-h2.writerDone
		}
		return nil, err
	}
	if h2 != nil {
		return &h2Lease{conn: h2, plan: plan}, nil
	}
	return &h1Lease{conn: conn, plan: plan}, nil
}

func connectionMetadata(conn net.Conn, fallback Protocol) transportMetadata {
	protocol := string(fallback)
	if secure, ok := conn.(*tls.Conn); ok {
		protocol = secure.ConnectionState().NegotiatedProtocol
	}
	return transportMetadata{protocol, conn.LocalAddr().String(), conn.RemoteAddr().String()}
}

type h1Lease struct {
	mu               sync.Mutex
	conn             net.Conn
	plan             Plan
	active           bool
	err              error
	retirementReason string
}

func (l *h1Lease) lockState()                  { l.mu.Lock() }
func (l *h1Lease) unlockState()                { l.mu.Unlock() }
func (l *h1Lease) metadata() transportMetadata { return connectionMetadata(l.conn, HTTP11) }
func (l *h1Lease) usable() (bool, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.usableLocked()
}

func (l *h1Lease) usableLocked() (bool, string) {
	if l.err != nil {
		return false, l.err.Error()
	}
	if l.active {
		return false, "request owns connection"
	}
	return true, ""
}

func (l *h1Lease) retirement() (bool, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.retirementReason != "", l.retirementReason
}

func (l *h1Lease) dispose(err error) {
	l.mu.Lock()
	if l.err == nil {
		if err == nil {
			err = errors.New("connection retired")
		}
		l.err = err
	}
	l.mu.Unlock()
	_ = l.conn.Close()
}

// HTTP/1.1 has no persistent pump; sender.run joins its reader before return.
func (l *h1Lease) join() {}

func (l *h1Lease) prepare(ctx context.Context, request Request) (transportSender, error) {
	if request.Protocol != HTTP11 {
		return nil, errors.New("request protocol does not match HTTP/1.1 lease")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, err := prepareH1(request.HTTP, request.Body, l.plan.SingleRecord, l.plan.LastByteSync)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return nil, l.err
	}
	if l.active {
		return nil, errors.New("request owns connection")
	}
	l.active = true
	return &h1Sender{lease: l, prepared: p, request: request}, nil
}

type h1Sender struct {
	lease    *h1Lease
	prepared *h1Prepared
	request  Request
}

func (s *h1Sender) readinessSnapshot(phase releasePhase) (bool, <-chan struct{}) {
	s.lease.lockState()
	defer s.lease.unlockState()
	return s.readyLocked(phase) == nil, nil
}

func (s *h1Sender) readyLocked(_ releasePhase) error {
	if s.lease.err != nil {
		return s.lease.err
	}
	if !s.lease.active {
		return errors.New("request does not own connection")
	}
	return nil
}

func (s *h1Sender) waitReady(ctx context.Context, phase releasePhase) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.lease.lockState()
	defer s.lease.unlockState()
	return s.readyLocked(phase)
}

func (s *h1Sender) run(ctx context.Context, gates releaseGates, hooks senderHooks) Outcome {
	l := s.lease
	collector := newBodyCollector(l.plan.Capture.Body, l.plan.Capture.BodyMax)
	observation := runH1WithCallbacks(ctx, l.conn, s.prepared, gates.initial, gates.final,
		l.plan.ReceiveHeaderMax, l.plan.ResponseBodyMax,
		func(at time.Time) error {
			l.mu.Lock()
			defer l.mu.Unlock()
			if err := s.readyLocked(initialPhase); err != nil {
				return err
			}
			if hooks.initialRelease != nil {
				return hooks.initialRelease(at)
			}
			return nil
		}, hooks.prefixComplete, nil,
		func(body []byte, _ h1Observation) error { collector.accept(body); return nil }, hooks.exchangeComplete)
	if observation.retired && observation.responseErr == nil && !observation.complete.IsZero() {
		l.mu.Lock()
		l.retirementReason = "HTTP/1.1 response requested connection close"
		if observation.eofObserved {
			l.retirementReason = "HTTP/1.1 response ended at EOF"
		}
		l.mu.Unlock()
	}
	if observation.err != nil || observation.retired {
		l.dispose(observation.err)
	}
	l.mu.Lock()
	l.active = false
	l.mu.Unlock()
	m := requestMilestones{
		observation.initial, observation.final, observation.written,
		observation.firstHeaders, observation.finalHeaders, observation.complete,
	}
	response := collectorResponse(collector, l.plan.Capture)
	response.Status = observation.status
	response.ReceivedBodyBytes = observation.receivedBytes
	response.DeclaredContentLength = OptionalInt{observation.declaredLength, observation.lengthPresent}
	response.Complete = observation.responseErr == nil && !observation.complete.IsZero()
	noBody := s.prepared.request.Method == "HEAD" || response.Status == 204 || response.Status == 304
	response.LengthMismatch = !noBody && observation.lengthPresent &&
		(observation.receivedBytes > observation.declaredLength ||
			(!observation.complete.IsZero() || errors.Is(observation.responseErr, io.ErrUnexpectedEOF)) &&
				observation.receivedBytes != observation.declaredLength)
	response.Truncated = errors.Is(observation.err, io.ErrUnexpectedEOF) ||
		l.plan.ResponseBodyMax >= 0 && observation.receivedBytes > l.plan.ResponseBodyMax
	if l.plan.Capture.Headers {
		response.Headers, response.Trailers = observation.header.Clone(), observation.trailer.Clone()
	}
	return transportOutcome(s.request, gates.anchor, m, response, observation.err)
}

type h2Lease struct {
	conn *h2Conn
	plan Plan
}

func (l *h2Lease) lockState()                  { l.conn.mu.Lock() }
func (l *h2Lease) unlockState()                { l.conn.mu.Unlock() }
func (l *h2Lease) metadata() transportMetadata { return connectionMetadata(l.conn.conn, HTTP2) }
func (l *h2Lease) dispose(err error)           { l.conn.close(err) }
func (l *h2Lease) join()                       { <-l.conn.readerDone; <-l.conn.writerDone }
func (l *h2Lease) usable() (bool, string) {
	l.lockState()
	defer l.unlockState()
	return l.usableLocked()
}

func (l *h2Lease) usableLocked() (bool, string) {
	if l.conn.err != nil {
		return false, l.conn.err.Error()
	}
	if l.conn.retiring {
		return false, "HTTP/2 connection retired"
	}
	if l.conn.active != nil {
		return false, "request owns connection"
	}
	return true, ""
}

func (l *h2Lease) retirement() (bool, string) {
	l.lockState()
	defer l.unlockState()
	return l.conn.retiring, l.conn.retirementReason
}

func (l *h2Lease) prepare(ctx context.Context, request Request) (transportSender, error) {
	if request.Protocol != HTTP2 {
		return nil, errors.New("request protocol does not match HTTP/2 lease")
	}
	if request.HTTP != nil && len(request.HTTP.Trailer) > 0 {
		return nil, errors.New("HTTP/2 request trailers are unsupported")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fields, err := h2RequestFields(request)
	if err != nil {
		return nil, err
	}
	p, err := l.conn.h2Prepare(fields, request.Body, l.plan.SingleRecord, l.plan.LastByteSync)
	if err != nil {
		return nil, err
	}
	l.lockState()
	p.collector = newBodyCollector(l.plan.Capture.Body, l.plan.Capture.BodyMax)
	l.unlockState()
	return &h2Sender{l, p, request}, nil
}

type h2Sender struct {
	lease    *h2Lease
	prepared *h2Prepared
	request  Request
}

func (s *h2Sender) record(phase releasePhase) h2Record {
	records := s.prepared.Prefix
	if phase == finalPhase && len(s.prepared.Final) > 0 {
		records = s.prepared.Final
	}
	if len(records) == 0 {
		return h2Record{}
	}
	record := records[0]
	if !s.prepared.Strict && record.Data > 0 {
		record.Data = 1
	}
	return record
}

func (s *h2Sender) readinessSnapshot(phase releasePhase) (bool, <-chan struct{}) {
	s.lease.lockState()
	defer s.lease.unlockState()
	return s.readyLocked(phase) == nil, s.lease.conn.changed
}

func (s *h2Sender) readyLocked(phase releasePhase) error {
	return s.lease.conn.readyLocked(s.prepared, s.record(phase))
}

func (s *h2Sender) waitReady(ctx context.Context, phase releasePhase) error {
	return s.lease.conn.h2WaitReady(ctx, s.prepared, s.record(phase))
}

func (s *h2Sender) run(ctx context.Context, gates releaseGates, hooks senderHooks) Outcome {
	c, p := s.lease.conn, s.prepared
	c.mu.Lock()
	p.completion = hooks.exchangeComplete
	c.mu.Unlock()
	wait := func(gate <-chan struct{}) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			c.mu.Lock()
			err := c.err
			c.mu.Unlock()
			return err
		case <-gate:
			return ctx.Err()
		}
	}
	err := wait(gates.initial)
	if err == nil {
		err = c.h2TransferWithRelease(ctx, p, p.Prefix, len(p.Final) == 0, hooks.initialRelease)
	}
	if err == nil && hooks.prefixComplete != nil {
		err = hooks.prefixComplete()
	}
	if err == nil && len(p.Final) > 0 {
		err = wait(gates.final)
		if err == nil {
			err = c.h2TransferWithRelease(ctx, p, p.Final, true, hooks.initialRelease)
		}
	}
	if err != nil {
		c.close(err)
	}
	observation := c.h2WaitResponse(ctx, p)
	if err == nil {
		err = observation.Err
	}
	c.mu.Lock()
	if !p.completionDone {
		p.completionDone = true
		if hooks.exchangeComplete != nil {
			if hookErr := hooks.exchangeComplete(time.Now()); err == nil {
				err = hookErr
			}
		}
	}
	response := collectorResponse(p.collector, s.lease.plan.Capture)
	response.Status, response.ReceivedBodyBytes = observation.Status, p.Received
	response.DeclaredContentLength = OptionalInt{p.Length, p.Length >= 0}
	response.LengthMismatch = !p.NoBody && p.Length >= 0 &&
		(p.Received > p.Length || !observation.Times.Complete.IsZero() && p.Received != p.Length)
	response.Complete = p.responseValid
	response.Reset = p.Reset
	response.Truncated = s.lease.plan.ResponseBodyMax >= 0 && p.Received > s.lease.plan.ResponseBodyMax
	if s.lease.plan.Capture.Headers {
		headers, trailers := observation.Headers, observation.Trailers
		if observation.diagnosticTrailers {
			if trailers == nil {
				trailers = observation.DiagnosticHeaders
			}
		} else if headers == nil {
			headers = observation.DiagnosticHeaders
		}
		response.Headers, response.Trailers = h2HTTPHeaders(headers), h2HTTPHeaders(trailers)
	}
	c.mu.Unlock()
	if releaseErr := c.h2Release(p); releaseErr != nil {
		c.close(releaseErr)
	}
	m := requestMilestones{
		observation.Times.Initial, observation.Times.Final, observation.Times.Written,
		observation.Times.FirstHeaders, observation.Times.Headers, observation.Times.Complete,
	}
	return transportOutcome(s.request, gates.anchor, m, response, err)
}

func h2HTTPHeaders(fields []hpack.HeaderField) http.Header {
	headers := make(http.Header)
	for _, field := range fields {
		if !strings.HasPrefix(field.Name, ":") {
			headers.Add(field.Name, field.Value)
		}
	}
	return headers
}

func collectorResponse(collector *bodyCollector, capture Capture) Response {
	sum := collector.sum()
	body := collector.capturedBody()
	return Response{
		AcceptedBodyBytes: collector.accepted, Digest: base64.StdEncoding.EncodeToString(sum[:]), Body: body,
		CaptureTruncated: capture.Body && int64(len(body)) < collector.accepted,
	}
}

func transportOutcome(request Request, anchor time.Time, m requestMilestones, response Response, err error) Outcome {
	evidence := m.evidence(anchor, "not observed")
	offset := func(value eventOffset) Offset { return Offset{value.ns, value.present, value.reason} }
	outcome := Outcome{
		RequestID: request.ID, Protocol: request.Protocol, Attempted: !m.initial.IsZero(), Response: response,
		Timing: Timing{
			offset(evidence.initial), offset(evidence.final), offset(evidence.written),
			offset(evidence.firstHeaders), offset(evidence.headers), offset(evidence.complete), offset(evidence.duration),
		},
	}
	outcome.EarlyResponse = !m.headers.IsZero() && (m.final.IsZero() || m.headers.Before(m.final))
	if err != nil {
		phase := PhaseReadiness
		if outcome.Attempted {
			phase = PhaseExecution
		}
		code := "transport"
		if errors.Is(err, context.DeadlineExceeded) {
			code = "timeout"
		} else if errors.Is(err, context.Canceled) {
			code = "cancelled"
		}
		outcome.Failure = &Failure{Phase: phase, Code: code, Message: fmt.Sprintf("%s: %s", phase, err), Cause: err}
	}
	return outcome
}
