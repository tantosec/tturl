package timing

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

type h2CipherRecord struct {
	Kind   byte
	Length int
}

// h2WireTap parses encrypted TLS records, independent of TCP write boundaries.
// It retains record metadata only, with no plaintext or session secrets.
type h2WireTap struct {
	net.Conn
	mu        sync.Mutex
	pending   []byte
	records   []h2CipherRecord
	block     <-chan struct{}
	began     chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
}

func (t *h2WireTap) Write(p []byte) (int, error) {
	t.mu.Lock()
	block, began := t.block, t.began
	t.mu.Unlock()
	if block != nil {
		select {
		case began <- struct{}{}:
		default:
		}
		select {
		case <-block:
		case <-t.closed:
			return 0, net.ErrClosed
		}
	}
	n, err := t.Conn.Write(p)
	t.mu.Lock()
	t.pending = append(t.pending, p[:n]...)
	for len(t.pending) >= 5 {
		length := int(binary.BigEndian.Uint16(t.pending[3:5]))
		if len(t.pending) < 5+length {
			break
		}
		t.records = append(t.records, h2CipherRecord{t.pending[0], length})
		t.pending = t.pending[5+length:]
	}
	t.mu.Unlock()
	return n, err
}

func (t *h2WireTap) Close() error {
	t.closeOnce.Do(func() { close(t.closed) })
	return t.Conn.Close()
}

func (t *h2WireTap) snapshot() []h2CipherRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]h2CipherRecord(nil), t.records...)
}

type h2Peer struct {
	Frame  http2.FrameHeader
	Fields []hpack.HeaderField
	Body   []byte
	Code   http2.ErrCode
}

type h2Fixture struct {
	client   *h2Conn
	tap      *h2WireTap
	frames   chan h2Peer
	commands chan func(*http2.Framer) error
	peerDone chan struct{}
	ctx      context.Context
	version  uint16
	peer     net.Conn
}

func h2Certificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"fixture.test"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func h2NewFixture(t *testing.T, version uint16) *h2Fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	x := &h2Fixture{
		frames: make(chan h2Peer, 128), commands: make(chan func(*http2.Framer) error, 128),
		peerDone: make(chan struct{}), ctx: ctx, version: version,
	}
	cert := h2Certificate(t)
	roots := x509.NewCertPool()
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots.AddCert(parsed)
	acknowledged := make(chan struct{})
	peerConn := make(chan net.Conn, 1)
	go func() {
		defer close(x.peerDone)
		raw, err := ln.Accept()
		if err != nil {
			return
		}
		peerConn <- raw
		defer func() { _ = raw.Close() }()
		peer := tls.Server(raw, &tls.Config{
			Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2"},
			MinVersion: version, MaxVersion: version, CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		})
		if err := peer.HandshakeContext(ctx); err != nil {
			return
		}
		preface := make([]byte, len(http2.ClientPreface))
		if _, err := io.ReadFull(peer, preface); err != nil || string(preface) != http2.ClientPreface {
			return
		}
		reader := http2.NewFramer(nil, peer)
		writer := http2.NewFramer(peer, nil)
		if _, err := reader.ReadFrame(); err != nil {
			return
		}
		if err := writer.WriteSettings(); err != nil {
			return
		}
		frame, err := reader.ReadFrame()
		if err != nil {
			return
		}
		settings, ok := frame.(*http2.SettingsFrame)
		if !ok || !settings.IsAck() {
			return
		}
		close(acknowledged)
		var writerWG sync.WaitGroup
		writerCtx, stopWriter := context.WithCancel(ctx)
		writerWG.Go(func() {
			for {
				select {
				case <-writerCtx.Done():
					return
				case command := <-x.commands:
					if command(writer) != nil {
						return
					}
				}
			}
		})
		// Peer reader teardown stops its writer without cancelling the caller's
		// command-result budget before an in-flight command reports completion.
		defer func() { stopWriter(); writerWG.Wait() }()
		decoder := hpack.NewDecoder(4096, nil)
		var block, body []byte
		for {
			frame, err := reader.ReadFrame()
			if err != nil {
				return
			}
			obs := h2Peer{Frame: frame.Header()}
			switch f := frame.(type) {
			case *http2.HeadersFrame:
				block = append(block[:0], f.HeaderBlockFragment()...)
				body = body[:0]
				if f.HeadersEnded() {
					obs.Fields, err = decoder.DecodeFull(block)
				}
			case *http2.ContinuationFrame:
				block = append(block, f.HeaderBlockFragment()...)
				if f.HeadersEnded() {
					obs.Fields, err = decoder.DecodeFull(block)
				}
			case *http2.DataFrame:
				body = append(body, f.Data()...)
				obs.Body = bytes.Clone(body)
			case *http2.RSTStreamFrame:
				obs.Code = f.ErrCode
			}
			if err != nil {
				return
			}
			select {
			case x.frames <- obs:
			case <-ctx.Done():
				return
			}
		}
	}()
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	x.tap = &h2WireTap{Conn: raw, closed: make(chan struct{})}
	conn := tls.Client(x.tap, &tls.Config{
		RootCAs: roots, ServerName: "fixture.test", NextProtos: []string{"h2"},
		MinVersion: version, MaxVersion: version, DynamicRecordSizingDisabled: true,
		CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
	})
	if err := conn.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	x.client, err = h2Acquire(ctx, conn, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-acknowledged:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Peer receipt can precede the tap's metadata append after raw Write.
	// Wire ownership proves the setup ACK and its instrumentation have finished.
	_ = x.syncedRecords()
	serverRaw := <-peerConn
	x.peer = serverRaw
	t.Cleanup(func() {
		x.client.close(nil)
		cancel()
		_ = serverRaw.Close()
		_ = ln.Close()
		select {
		case <-x.peerDone:
		case <-time.After(time.Second):
			t.Error("peer did not terminate")
		}
		select {
		case <-x.client.readerDone:
		case <-time.After(time.Second):
			t.Error("reader did not terminate")
		}
		select {
		case <-x.client.writerDone:
		case <-time.After(time.Second):
			t.Error("writer did not terminate")
		}
	})
	return x
}

// syncedRecords observes metadata only after the current wire owner finishes.
func (x *h2Fixture) syncedRecords() []h2CipherRecord {
	x.client.wire.Lock()
	defer x.client.wire.Unlock()
	return x.tap.snapshot()
}

func (x *h2Fixture) next(t *testing.T) h2Peer {
	t.Helper()
	select {
	case frame := <-x.frames:
		return frame
	case <-x.ctx.Done():
		t.Fatal(x.ctx.Err())
	}
	return h2Peer{}
}

func (x *h2Fixture) send(t *testing.T, command func(*http2.Framer) error) {
	t.Helper()
	done := make(chan error, 1)
	x.commands <- func(f *http2.Framer) error { err := command(f); done <- err; return err }
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-x.ctx.Done():
		t.Fatal(x.ctx.Err())
	}
}

func (x *h2Fixture) respond(t *testing.T, stream uint32, fields []hpack.HeaderField, end bool) {
	t.Helper()
	var b bytes.Buffer
	encoder := hpack.NewEncoder(&b)
	for _, field := range fields {
		if err := encoder.WriteField(field); err != nil {
			t.Fatal(err)
		}
	}
	x.send(t, func(f *http2.Framer) error {
		return f.WriteHeaders(http2.HeadersFrameParam{
			StreamID:      stream,
			BlockFragment: b.Bytes(), EndHeaders: true, EndStream: end,
		})
	})
}

func h2Fields() []hpack.HeaderField {
	return []hpack.HeaderField{
		{Name: ":method", Value: "POST"},
		{Name: ":scheme", Value: "https"},
		{Name: ":authority", Value: "fixture.test"},
		{Name: ":path", Value: "/resource"},
		{Name: "x-shared", Value: "compression history"},
	}
}

func h2SendRecords(t *testing.T, x *h2Fixture, p *h2Prepared, records []h2Record, ending bool) {
	t.Helper()
	for i, record := range records {
		if err := x.client.h2WaitReady(x.ctx, p, record); err != nil {
			t.Fatal(err)
		}
		if err := x.client.h2Write(x.ctx, p, record, ending && i == len(records)-1); err != nil {
			t.Fatal(err)
		}
	}
}

func TestH2GOAWAYBoundsCannotIncrease(t *testing.T) {
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		for _, increase := range []bool{false, true} {
			t.Run(fmt.Sprintf("TLS%x/increase=%t", version, increase), func(t *testing.T) {
				x := h2NewFixture(t, version)
				p, err := x.client.h2Prepare(h2Fields(), nil, true, false)
				if err != nil {
					t.Fatal(err)
				}
				h2SendRecords(t, x, p, p.Prefix, true)
				for x.next(t).Frame.Type != http2.FrameHeaders {
				}
				bounds := []uint32{0x7fffffff, 0x7fffffff, p.Stream}
				if increase {
					bounds = []uint32{p.Stream, p.Stream + 2}
				}
				for _, bound := range bounds {
					x.send(t, func(f *http2.Framer) error { return f.WriteGoAway(bound, http2.ErrCodeNo, nil) })
				}
				if !increase {
					x.respond(t, p.Stream, []hpack.HeaderField{{Name: ":status", Value: "200"}}, true)
				}
				observation := x.client.h2WaitResponse(x.ctx, p)
				if increase {
					if !errors.Is(observation.Err, http2.ConnectionError(http2.ErrCodeProtocol)) {
						t.Fatalf("increasing GOAWAY bound did not dispose connection: %v", observation.Err)
					}
					<-x.client.readerDone
					<-x.client.writerDone
				} else if observation.Err != nil || observation.Times.Complete.IsZero() || observation.Times.Written.IsZero() {
					t.Fatalf("legal equal/decreasing GOAWAY interrupted accepted stream: %+v", observation)
				}
				x.client.mu.Lock()
				last := x.client.goAwayLast
				x.client.mu.Unlock()
				if last != p.Stream {
					t.Fatalf("last valid GOAWAY bound = %d, want %d", last, p.Stream)
				}
			})
		}
	}
}

func h2AssertCipherRecords(t *testing.T, x *h2Fixture, before int, records []h2Record) {
	t.Helper()
	actual := x.tap.snapshot()[before:]
	if len(actual) != len(records) {
		t.Fatalf("encrypted records = %v; want %d controlled records", actual, len(records))
	}
	overhead := 17 // TLS 1.3: authenticated inner content type and GCM tag
	if x.version == tls.VersionTLS12 {
		overhead = 24
	} // explicit nonce and tag
	for i, record := range records {
		if actual[i].Kind != 23 || actual[i].Length != len(record.Bytes)+overhead {
			t.Fatalf("encrypted record %d = %+v; prepared plaintext = %d", i, actual[i], len(record.Bytes))
		}
	}
}

func TestH2ControlledRecords(t *testing.T) {
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		for _, last := range []bool{false, true} {
			for _, body := range []string{"", "x", "ordinary payload"} {
				t.Run(fmt.Sprintf("TLS%x/last%t/body%d", version, last, len(body)), func(t *testing.T) {
					x := h2NewFixture(t, version)
					fields := append(h2Fields(), hpack.HeaderField{Name: "content-length", Value: "999"})
					p, err := x.client.h2Prepare(fields, []byte(body), true, last)
					if err != nil {
						t.Fatal(err)
					}
					before := len(x.tap.snapshot())
					h2SendRecords(t, x, p, p.Prefix, !last)
					if last {
						h2SendRecords(t, x, p, p.Final, true)
					}
					h2AssertCipherRecords(t, x, before, append(append([]h2Record(nil), p.Prefix...), p.Final...))
					var decoded []hpack.HeaderField
					var received string
					for {
						obs := x.next(t)
						if obs.Fields != nil {
							decoded = obs.Fields
						}
						if obs.Frame.Type == http2.FrameData {
							received = string(obs.Body)
							if obs.Frame.Flags.Has(http2.FlagDataEndStream) {
								break
							}
						}
						if body == "" {
							if obs.Frame.Type == http2.FrameHeaders && obs.Frame.Flags.Has(http2.FlagHeadersEndHeaders) {
								break
							}
							if obs.Frame.Type == http2.FrameContinuation && obs.Frame.Flags.Has(http2.FlagContinuationEndHeaders) {
								break
							}
						}
					}
					if len(decoded) != len(fields) || received != body {
						t.Fatalf("decoded=%v body=%q", decoded, received)
					}
					x.respond(t, p.Stream, []hpack.HeaderField{{Name: ":status", Value: "200"}}, true)
					obs := x.client.h2WaitResponse(x.ctx, p)
					if obs.Err != nil || obs.Status != 200 || obs.Times.Headers.IsZero() || obs.Times.Complete.IsZero() {
						t.Fatalf("observation=%+v", obs)
					}
					if !last && !obs.Times.Initial.Equal(obs.Times.Final) {
						t.Fatal("single-record release points differ")
					}
					if err := x.client.h2Release(p); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}

func TestH2DeferredControlsAndEarlyResponse(t *testing.T) {
	x := h2NewFixture(t, tls.VersionTLS13)
	p, err := x.client.h2Prepare(h2Fields(), nil, true, true)
	if err != nil {
		t.Fatal(err)
	}
	h2SendRecords(t, x, p, p.Prefix, false)
	first := x.next(t)
	if first.Frame.Type != http2.FrameHeaders || first.Frame.Flags.Has(http2.FlagHeadersEndHeaders) ||
		!first.Frame.Flags.Has(http2.FlagHeadersEndStream) {
		t.Fatalf("prefix = %T %+v", first.Frame, first.Frame)
	}
	x.send(t, func(f *http2.Framer) error { return f.WritePing(false, [8]byte{1}) })
	x.send(t, func(f *http2.Framer) error {
		return f.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 0})
	})
	x.respond(t, p.Stream, []hpack.HeaderField{{Name: ":status", Value: "200"}}, true)
	obs := x.client.h2WaitResponse(x.ctx, p)
	if obs.Err != nil || obs.Times.Headers.IsZero() || !obs.Times.Final.IsZero() {
		t.Fatalf("early observation=%+v", obs)
	}
	before := len(x.tap.snapshot())
	h2SendRecords(t, x, p, p.Final, true)
	frame := x.next(t)
	if frame.Frame.Type != http2.FrameContinuation ||
		!frame.Frame.Flags.Has(http2.FlagContinuationEndHeaders) || frame.Frame.Length != 1 {
		t.Fatalf("final frame = %T", frame.Frame)
	}
	for range 2 {
		frame = x.next(t)
		if frame.Frame.Type != http2.FramePing && frame.Frame.Type != http2.FrameSettings {
			t.Fatalf("deferred frame = %v", frame.Frame)
		}
		if !frame.Frame.Flags.Has(http2.FlagSettingsAck) {
			t.Fatal("control lacks ACK")
		}
	}
	records := x.syncedRecords()[before:]
	if len(records) != 3 || records[0].Length != 10+17 {
		t.Fatalf("final and separate control records = %v", records)
	}
	if err := x.client.h2Release(p); err != nil {
		t.Fatal(err)
	}
}

func TestH2CreditPumpAndReadinessRevalidation(t *testing.T) {
	x := h2NewFixture(t, tls.VersionTLS13)
	x.send(t, func(f *http2.Framer) error {
		return f.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 0})
	})
	_ = x.next(t) // SETTINGS ACK proves pump applied the zero stream window.
	p, err := x.client.h2Prepare(h2Fields(), []byte("payload"), true, false)
	if err != nil {
		t.Fatal(err)
	}
	x.client.mu.Lock()
	x.client.window = 0
	x.client.mu.Unlock()
	if err := x.client.h2Write(x.ctx, p, p.Prefix[0], true); !errors.Is(err, errH2NotReady) {
		t.Fatalf("zero credit result=%v", err)
	}
	if !p.Obs.Times.Initial.IsZero() {
		t.Fatal("insufficient credit dispatched request")
	}
	ready := make(chan error, 1)
	go func() { ready <- x.client.h2WaitReady(x.ctx, p, p.Prefix[0]) }()
	x.send(t, func(f *http2.Framer) error { return f.WritePing(false, [8]byte{2}) })
	x.send(t, func(f *http2.Framer) error {
		return f.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 7})
	})
	x.send(t, func(f *http2.Framer) error { return f.WriteWindowUpdate(0, 7) })
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-x.ctx.Done():
		t.Fatal(x.ctx.Err())
	}
	h2SendRecords(t, x, p, p.Prefix, true)
	for {
		frame := x.next(t)
		if frame.Frame.Type == http2.FrameData && frame.Frame.Flags.Has(http2.FlagDataEndStream) {
			break
		}
	}
	x.respond(t, p.Stream, []hpack.HeaderField{{Name: ":status", Value: "200"}}, true)
	if obs := x.client.h2WaitResponse(x.ctx, p); obs.Err != nil {
		t.Fatal(obs.Err)
	}
}

func TestH2HPACKHistoryAndInvalidation(t *testing.T) {
	x := h2NewFixture(t, tls.VersionTLS13)
	var sizes []int
	for range 2 {
		p, err := x.client.h2Prepare(h2Fields(), nil, true, false)
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, len(p.Prefix[0].Bytes))
		h2SendRecords(t, x, p, p.Prefix, true)
		if fields := x.next(t).Fields; len(fields) != len(h2Fields()) {
			t.Fatalf("peer decoding failed=%v", fields)
		}
		x.respond(t, p.Stream, []hpack.HeaderField{{Name: ":status", Value: "200"}}, true)
		if obs := x.client.h2WaitResponse(x.ctx, p); obs.Err != nil {
			t.Fatal(obs.Err)
		}
		if err := x.client.h2Release(p); err != nil {
			t.Fatal(err)
		}
	}
	if sizes[1] >= sizes[0] {
		t.Fatalf("normal HPACK history did not compress repeated fields: %v", sizes)
	}
	p, err := x.client.h2Prepare(h2Fields(), nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	x.send(t, func(f *http2.Framer) error {
		return f.WriteSettings(http2.Setting{ID: http2.SettingHeaderTableSize, Val: 0})
	})
	_ = x.next(t)
	if err := x.client.h2WaitReady(x.ctx, p, p.Prefix[0]); err == nil {
		t.Fatal("stale HPACK preparation accepted")
	}
	if !p.Obs.Times.Initial.IsZero() {
		t.Fatal("invalidated preparation dispatched")
	}
	x.client.close(errors.New("abandoned preparation"))
	if _, err := x.client.h2Prepare(h2Fields(), nil, true, false); err == nil {
		t.Fatal("abandoned HPACK connection reused")
	}
}

func TestH2StrictEncodedSizeAndLargeBody(t *testing.T) {
	x := h2NewFixture(t, tls.VersionTLS13)
	p, err := x.client.h2Prepare(h2Fields(), bytes.Repeat([]byte{'a'}, h2RecordLimit), true, false)
	if err == nil || p != nil {
		t.Fatal("oversized strict request accepted")
	}
	if len(x.tap.snapshot()) == 0 {
		t.Fatal("TLS fixture missing records")
	}
	x.client.close(errors.New("strict preparation abandoned"))
	x = h2NewFixture(t, tls.VersionTLS13)
	body := bytes.Repeat([]byte{'b'}, 80000)
	p, err = x.client.h2Prepare(h2Fields(), body, false, true)
	if err != nil {
		t.Fatal(err)
	}
	peerFinished := make(chan struct{})
	go func() {
		defer close(peerFinished)
		for {
			select {
			case <-x.ctx.Done():
				return
			case obs := <-x.frames:
				if obs.Frame.Type == http2.FrameData {
					n := obs.Frame.Length
					x.commands <- func(w *http2.Framer) error {
						if err := w.WriteWindowUpdate(0, n); err != nil {
							return err
						}
						return w.WriteWindowUpdate(p.Stream, n)
					}
					if obs.Frame.Flags.Has(http2.FlagDataEndStream) {
						return
					}
				}
			}
		}
	}()
	before := len(x.tap.snapshot())
	h2SendRecords(t, x, p, p.Prefix, false)
	h2SendRecords(t, x, p, p.Final, true)
	h2AssertCipherRecords(t, x, before, append(append([]h2Record(nil), p.Prefix...), p.Final...))
	select {
	case <-peerFinished:
	case <-x.ctx.Done():
		t.Fatal(x.ctx.Err())
	}
	x.respond(t, p.Stream, []hpack.HeaderField{{Name: ":status", Value: "200"}}, true)
	obs := x.client.h2WaitResponse(x.ctx, p)
	if obs.Err != nil || obs.Times.Initial.IsZero() || obs.Times.Final.IsZero() ||
		obs.Times.Final.Before(obs.Times.Initial) {
		t.Fatalf("large observation=%+v", obs)
	}
}

func TestH2MalformedHeadersAndLaterBodyFailure(t *testing.T) {
	for _, malformed := range []bool{true, false} {
		t.Run(fmt.Sprint(malformed), func(t *testing.T) {
			x := h2NewFixture(t, tls.VersionTLS13)
			p, err := x.client.h2Prepare(h2Fields(), nil, true, false)
			if err != nil {
				t.Fatal(err)
			}
			h2SendRecords(t, x, p, p.Prefix, true)
			_ = x.next(t)
			fields := []hpack.HeaderField{{Name: ":status", Value: "200"}, {Name: "content-length", Value: "3"}}
			if malformed {
				fields = append(fields, hpack.HeaderField{Name: "connection", Value: "close"})
			}
			x.respond(t, p.Stream, fields, false)
			if !malformed {
				x.send(t, func(f *http2.Framer) error { return f.WriteData(p.Stream, true, []byte("x")) })
			}
			obs := x.client.h2WaitResponse(x.ctx, p)
			if obs.Err == nil || obs.Times.Headers.IsZero() != malformed || obs.Times.Complete.IsZero() != malformed {
				t.Fatalf("observation=%+v", obs)
			}
		})
	}
}

func TestH2CancellationAndControlQueueBound(t *testing.T) {
	x := h2NewFixture(t, tls.VersionTLS13)
	p, err := x.client.h2Prepare(h2Fields(), nil, true, true)
	if err != nil {
		t.Fatal(err)
	}
	h2SendRecords(t, x, p, p.Prefix, false)
	_ = x.next(t)
	for range h2ControlLimit + 2 {
		x.send(t, func(f *http2.Framer) error { return f.WritePing(false, [8]byte{3}) })
	}
	obs := x.client.h2WaitResponse(x.ctx, p)
	if obs.Err == nil || !strings.Contains(obs.Err.Error(), "queue exhausted") {
		t.Fatalf("queue failure=%v", obs.Err)
	}
	if !obs.Times.Final.IsZero() {
		t.Fatal("exhausted queue released final fragment")
	}
	x = h2NewFixture(t, tls.VersionTLS13)
	p, err = x.client.h2Prepare(h2Fields(), []byte("x"), true, false)
	if err != nil {
		t.Fatal(err)
	}
	x.client.mu.Lock()
	x.client.streamWindow = 0
	x.client.mu.Unlock()
	ctx, cancel := context.WithCancel(x.ctx)
	cancel()
	if err := x.client.h2WaitReady(ctx, p, p.Prefix[0]); !errors.Is(err, context.Canceled) {
		t.Fatalf("credit cancellation=%v", err)
	}
	x.client.close(context.Canceled)
	if obs := x.client.h2WaitResponse(x.ctx, p); !errors.Is(obs.Err, context.Canceled) {
		t.Fatalf("partial cancellation=%+v", obs)
	}
}

func TestH2IncrementalCredit(t *testing.T) {
	x := h2NewFixture(t, tls.VersionTLS13)
	x.send(t, func(f *http2.Framer) error {
		return f.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 4})
	})
	_ = x.next(t)
	p, err := x.client.h2Prepare(h2Fields(), []byte("twelve bytes"), false, false)
	if err != nil {
		t.Fatal(err)
	}
	sent := make(chan error, 1)
	go func() { sent <- x.client.h2Transfer(x.ctx, p, p.Prefix, true) }()
	var received []byte
	for {
		frame := x.next(t)
		if frame.Frame.Type != http2.FrameData {
			continue
		}
		if frame.Frame.Length > 4 {
			t.Fatalf("DATA exceeded available credit: %v", frame.Frame)
		}
		received = frame.Body
		if frame.Frame.Flags.Has(http2.FlagDataEndStream) {
			break
		}
		n := frame.Frame.Length
		x.send(t, func(f *http2.Framer) error { return f.WriteWindowUpdate(p.Stream, n) })
	}
	if string(received) != "twelve bytes" {
		t.Fatalf("body=%q", received)
	}
	select {
	case err := <-sent:
		if err != nil {
			t.Fatal(err)
		}
	case <-x.ctx.Done():
		t.Fatal(x.ctx.Err())
	}
	x.respond(t, p.Stream, []hpack.HeaderField{{Name: ":status", Value: "200"}}, true)
	if obs := x.client.h2WaitResponse(x.ctx, p); obs.Err != nil {
		t.Fatal(obs.Err)
	}
}

func TestH2FirstHeadersBeforeContinuationAndInformational(t *testing.T) {
	x := h2NewFixture(t, tls.VersionTLS13)
	p, err := x.client.h2Prepare(h2Fields(), nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	h2SendRecords(t, x, p, p.Prefix, true)
	_ = x.next(t)
	var b bytes.Buffer
	if err := hpack.NewEncoder(&b).WriteField(hpack.HeaderField{Name: ":status", Value: "103"}); err != nil {
		t.Fatal(err)
	}
	x.send(t, func(f *http2.Framer) error {
		return f.WriteHeaders(http2.HeadersFrameParam{StreamID: p.Stream, BlockFragment: b.Bytes()[:len(b.Bytes())-1]})
	})
	// The blocked decoder has not seen END_HEADERS. Observe the reader hook.
	var firstHeaders time.Time
	for {
		x.client.mu.Lock()
		first, final, changed := p.Obs.Times.FirstHeaders, p.Obs.Times.Headers, x.client.changed
		x.client.mu.Unlock()
		if !first.IsZero() {
			firstHeaders = first
			if !final.IsZero() {
				t.Fatal("incomplete informational headers produced primary endpoint")
			}
			break
		}
		select {
		case <-changed:
		case <-x.ctx.Done():
			t.Fatal("first HEADERS not observed before CONTINUATION")
		}
	}
	x.send(t, func(f *http2.Framer) error { return f.WriteContinuation(p.Stream, true, b.Bytes()[len(b.Bytes())-1:]) })
	// A subsequent PING ACK proves the reader processed the complete 103.
	x.send(t, func(f *http2.Framer) error { return f.WritePing(false, [8]byte{5}) })
	if ack := x.next(t); ack.Frame.Type != http2.FramePing || !ack.Frame.Flags.Has(http2.FlagPingAck) {
		t.Fatalf("informational processing barrier received %+v", ack)
	}
	x.client.mu.Lock()
	informational, done := p.Obs, p.Done
	x.client.mu.Unlock()
	if informational.Err != nil || done || informational.Status != 0 ||
		!informational.Times.Headers.IsZero() || !informational.Times.Complete.IsZero() ||
		informational.Times.FirstHeaders != firstHeaders {
		t.Fatalf("informational response supplied final evidence: %+v done=%t", informational, done)
	}
	x.respond(t, p.Stream, []hpack.HeaderField{{Name: ":status", Value: "200"}}, true)
	obs := x.client.h2WaitResponse(x.ctx, p)
	// The pre-CONTINUATION check establishes order; clock readings may tie.
	if obs.Err != nil || obs.Status != 200 || obs.Times.FirstHeaders != firstHeaders ||
		obs.Times.Headers.IsZero() || obs.Times.Complete.IsZero() ||
		obs.Times.Headers.Before(obs.Times.FirstHeaders) {
		t.Fatalf("milestones=%+v error=%v", obs.Times, obs.Err)
	}
}

func TestH2StreamErrorsRetainConnection(t *testing.T) {
	x := h2NewFixture(t, tls.VersionTLS13)
	p, err := x.client.h2Prepare(h2Fields(), nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	h2SendRecords(t, x, p, p.Prefix, true)
	_ = x.next(t)
	fields := []hpack.HeaderField{{Name: ":status", Value: "200"}, {Name: "content-length", Value: "+1"}}
	x.respond(t, p.Stream, fields, true)
	obs := x.client.h2WaitResponse(x.ctx, p)
	if obs.Err == nil || !obs.Times.Headers.IsZero() {
		t.Fatalf("malformed endpoint=%+v", obs)
	}
	reset := x.next(t)
	if reset.Frame.Type != http2.FrameRSTStream {
		t.Fatalf("stream failure response=%v", reset.Frame)
	}
	if err := x.client.h2Release(p); err != nil {
		t.Fatal(err)
	}
	p, err = x.client.h2Prepare(h2Fields(), nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	h2SendRecords(t, x, p, p.Prefix, true)
	if fields := x.next(t).Fields; len(fields) != len(h2Fields()) {
		t.Fatalf("HPACK after stream failure=%v", fields)
	}
	x.respond(t, p.Stream, []hpack.HeaderField{{Name: ":status", Value: "200"}}, true)
	if obs := x.client.h2WaitResponse(x.ctx, p); obs.Err != nil {
		t.Fatal(obs.Err)
	}
}

func TestH2RetirementAndFinalDirection(t *testing.T) {
	x := h2NewFixture(t, tls.VersionTLS13)
	p, err := x.client.h2Prepare(h2Fields(), []byte("x"), true, true)
	if err != nil {
		t.Fatal(err)
	}
	h2SendRecords(t, x, p, p.Prefix, false)
	_ = x.next(t)
	x.send(t, func(f *http2.Framer) error {
		return f.WriteSettings(http2.Setting{ID: http2.SettingMaxConcurrentStreams, Val: 0})
	})
	_ = x.next(t)
	x.send(t, func(f *http2.Framer) error { return f.WriteGoAway(p.Stream, http2.ErrCodeNo, nil) })
	x.respond(t, p.Stream, []hpack.HeaderField{{Name: ":status", Value: "200"}}, true)
	if obs := x.client.h2WaitResponse(x.ctx, p); obs.Err != nil {
		t.Fatal(obs.Err)
	}
	h2SendRecords(t, x, p, p.Final, true)
	frame := x.next(t)
	if frame.Frame.Type != http2.FrameData || !frame.Frame.Flags.Has(http2.FlagDataEndStream) {
		t.Fatalf("accepted direction finalisation=%v", frame.Frame)
	}
	if err := x.client.h2Release(p); err != nil {
		t.Fatal(err)
	}
	if _, err := x.client.h2Prepare(h2Fields(), nil, true, false); err == nil {
		t.Fatal("retired connection accepted new work")
	}
}

func TestH2BlockedControlWriterCancellation(t *testing.T) {
	x := h2NewFixture(t, tls.VersionTLS13)
	p, err := x.client.h2Prepare(h2Fields(), nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	x.tap.mu.Lock()
	x.tap.block = make(chan struct{})
	x.tap.began = make(chan struct{}, 1)
	began := x.tap.began
	x.tap.mu.Unlock()
	x.send(t, func(f *http2.Framer) error { return f.WritePing(false, [8]byte{4}) })
	select {
	case <-began:
	case <-x.ctx.Done():
		t.Fatal(x.ctx.Err())
	}
	ctx, cancel := context.WithCancel(x.ctx)
	finished := make(chan error, 1)
	go func() { finished <- x.client.h2Write(ctx, p, p.Prefix[0], true) }()
	cancel()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("blocked request sender succeeded after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("wire waiter stranded")
	}
	select {
	case <-x.client.writerDone:
	case <-time.After(time.Second):
		t.Fatal("control writer stranded")
	}
	obs := x.client.h2WaitResponse(x.ctx, p)
	if !errors.Is(obs.Err, context.Canceled) || !obs.Times.Initial.IsZero() {
		t.Fatalf("unreleased cancellation=%+v", obs)
	}
}

func TestH2ExactRecordBudgetWithHistory(t *testing.T) {
	for _, last := range []bool{false, true} {
		for _, excess := range []int{0, 1} {
			t.Run(fmt.Sprintf("last%t/excess%d", last, excess), func(t *testing.T) {
				x := h2NewFixture(t, tls.VersionTLS13)
				p, err := x.client.h2Prepare(h2Fields(), nil, true, false)
				if err != nil {
					t.Fatal(err)
				}
				h2SendRecords(t, x, p, p.Prefix, true)
				_ = x.next(t)
				x.respond(t, p.Stream, []hpack.HeaderField{{Name: ":status", Value: "200"}}, true)
				if obs := x.client.h2WaitResponse(x.ctx, p); obs.Err != nil {
					t.Fatal(obs.Err)
				}
				if err := x.client.h2Release(p); err != nil {
					t.Fatal(err)
				}
				// Every repeated field is now one HPACK indexed byte on this connection.
				bodySize := h2RecordLimit - (9 + len(h2Fields())) - 9 + excess
				if last {
					bodySize++
				}
				before := len(x.tap.snapshot())
				p, err = x.client.h2Prepare(h2Fields(), bytes.Repeat([]byte{'a'}, bodySize), true, last)
				if excess != 0 {
					if err == nil || len(x.tap.snapshot()) != before {
						t.Fatal("over-budget request dispatched")
					}
					x.client.close(errors.New("over-budget preparation"))
					return
				}
				if err != nil || len(p.Prefix[0].Bytes) != h2RecordLimit {
					t.Fatalf("exact encoded boundary=%v err=%v", p, err)
				}
				h2SendRecords(t, x, p, p.Prefix, !last)
				if last {
					h2SendRecords(t, x, p, p.Final, true)
				}
				h2AssertCipherRecords(t, x, before, append(append([]h2Record(nil), p.Prefix...), p.Final...))
			})
		}
	}
}

func TestH2ResetPreventsFinalisationPreservingEndpoint(t *testing.T) {
	x := h2NewFixture(t, tls.VersionTLS13)
	p, err := x.client.h2Prepare(h2Fields(), []byte("x"), true, true)
	if err != nil {
		t.Fatal(err)
	}
	h2SendRecords(t, x, p, p.Prefix, false)
	_ = x.next(t)
	x.respond(t, p.Stream, []hpack.HeaderField{{Name: ":status", Value: "200"}}, true)
	if obs := x.client.h2WaitResponse(x.ctx, p); obs.Err != nil {
		t.Fatal(obs.Err)
	}
	x.send(t, func(f *http2.Framer) error { return f.WriteRSTStream(p.Stream, http2.ErrCodeCancel) })
	for {
		x.client.mu.Lock()
		reset, changed := p.Reset, x.client.changed
		x.client.mu.Unlock()
		if reset {
			break
		}
		select {
		case <-changed:
		case <-x.ctx.Done():
			t.Fatal(x.ctx.Err())
		}
	}
	if err := x.client.h2Write(x.ctx, p, p.Final[0], true); err == nil {
		t.Fatal("reset request direction finalised")
	}
	obs := x.client.h2WaitResponse(x.ctx, p)
	if obs.Err == nil || obs.Times.Headers.IsZero() || !obs.Times.Final.IsZero() {
		t.Fatalf("reset evidence=%+v", obs)
	}
}

func TestH2HeadBodylessAndTrailers(t *testing.T) {
	for _, head := range []bool{false, true} {
		t.Run(fmt.Sprint(head), func(t *testing.T) {
			x := h2NewFixture(t, tls.VersionTLS13)
			fields := h2Fields()
			if head {
				fields[0].Value = "HEAD"
			}
			p, err := x.client.h2Prepare(fields, nil, true, false)
			if err != nil {
				t.Fatal(err)
			}
			h2SendRecords(t, x, p, p.Prefix, true)
			_ = x.next(t)
			if head {
				response := []hpack.HeaderField{{Name: ":status", Value: "200"}, {Name: "content-length", Value: "99"}}
				x.respond(t, p.Stream, response, true)
			} else {
				response := []hpack.HeaderField{{Name: ":status", Value: "200"}, {Name: "content-length", Value: "1"}}
				x.respond(t, p.Stream, response, false)
				x.send(t, func(f *http2.Framer) error { return f.WriteData(p.Stream, false, []byte("x")) })
				// The initial response contains no dynamic entries; a fresh encoder here
				// uses literals, which the connection-bound decoder can safely accept.
				x.respond(t, p.Stream, []hpack.HeaderField{{Name: "x-trailer", Value: "complete"}}, true)
			}
			obs := x.client.h2WaitResponse(x.ctx, p)
			if obs.Err != nil || obs.Times.Complete.IsZero() || (!head && len(obs.Trailers) != 1) {
				t.Fatalf("bodyless/trailers=%+v", obs)
			}
		})
	}
}

func TestH2IdleStreamControlsAndRetirement(t *testing.T) {
	for _, control := range []string{"window", "reset", "goaway"} {
		t.Run(control, func(t *testing.T) {
			x := h2NewFixture(t, tls.VersionTLS13)
			p, err := x.client.h2Prepare(h2Fields(), nil, true, false)
			if err != nil {
				t.Fatal(err)
			}
			x.send(t, func(f *http2.Framer) error {
				switch control {
				case "window":
					return f.WriteWindowUpdate(p.Stream, 1)
				case "reset":
					return f.WriteRSTStream(p.Stream, http2.ErrCodeCancel)
				default:
					return f.WriteGoAway(p.Stream, http2.ErrCodeNo, nil)
				}
			})
			for {
				x.client.mu.Lock()
				observed, changed := x.client.err != nil || x.client.retiring, x.client.changed
				x.client.mu.Unlock()
				if observed {
					break
				}
				select {
				case <-changed:
				case <-x.ctx.Done():
					t.Fatal(x.ctx.Err())
				}
			}
			if err := x.client.h2Write(x.ctx, p, p.Prefix[0], true); err == nil {
				t.Fatal("idle stream control allowed initial request release")
			}
			x.client.mu.Lock()
			initial := p.Obs.Times.Initial
			x.client.mu.Unlock()
			if !initial.IsZero() {
				t.Fatal("unreleased stream has initial milestone")
			}
		})
	}
}

func TestH2ReleaseHookExactlyOnce(t *testing.T) {
	x := h2NewFixture(t, tls.VersionTLS13)
	p, err := x.client.h2Prepare(h2Fields(), []byte("x"), true, true)
	if err != nil {
		t.Fatal(err)
	}
	var called int
	var release time.Time
	hook := func(at time.Time) error { called++; release = at; return nil }
	if err := x.client.h2TransferWithRelease(x.ctx, p, p.Prefix, false, hook); err != nil {
		t.Fatal(err)
	}
	if err := x.client.h2TransferWithRelease(x.ctx, p, p.Final, true, hook); err != nil {
		t.Fatal(err)
	}
	if called != 1 || !release.Equal(p.Obs.Times.Initial) {
		t.Fatalf("release callback count=%d at=%v initial=%v", called, release, p.Obs.Times.Initial)
	}
}

func h2PreparationConn() *h2Conn {
	c := &h2Conn{nextStream: 1, initialWindow: 65535, maxFrame: 16384, maxStreams: ^uint32(0)}
	c.encoder = hpack.NewEncoder(&c.encoded)
	return c
}

func TestH2RequestPreflightAndPreparationIntegrity(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]hpack.HeaderField) []hpack.HeaderField
	}{
		{"missing-method", func(f []hpack.HeaderField) []hpack.HeaderField { return f[1:] }},
		{"missing-scheme", func(f []hpack.HeaderField) []hpack.HeaderField { return append(f[:1], f[2:]...) }},
		{"missing-authority", func(f []hpack.HeaderField) []hpack.HeaderField { return append(f[:2], f[3:]...) }},
		{"missing-path", func(f []hpack.HeaderField) []hpack.HeaderField { return append(f[:3], f[4:]...) }},
		{"duplicate-pseudo", func(f []hpack.HeaderField) []hpack.HeaderField { return append(f[:1], f...) }},
		{"late-pseudo", func(f []hpack.HeaderField) []hpack.HeaderField { return append(f, f[0]) }},
		{"unknown-pseudo", func(f []hpack.HeaderField) []hpack.HeaderField { f[0].Name = ":protocol"; return f }},
		{"empty-pseudo", func(f []hpack.HeaderField) []hpack.HeaderField { f[0].Value = ""; return f }},
		{"invalid-method", func(f []hpack.HeaderField) []hpack.HeaderField { f[0].Value = "GET POST"; return f }},
		{"connect", func(f []hpack.HeaderField) []hpack.HeaderField { f[0].Value = "CONNECT"; return f }},
		{"scheme", func(f []hpack.HeaderField) []hpack.HeaderField { f[1].Value = "http"; return f }},
		{"authority-user", func(f []hpack.HeaderField) []hpack.HeaderField { f[2].Value = "user@fixture.test"; return f }},
		{"authority-port", func(f []hpack.HeaderField) []hpack.HeaderField { f[2].Value = "fixture.test:invalid"; return f }},
		{"authority-host", func(f []hpack.HeaderField) []hpack.HeaderField { f[2].Value = ":8443"; return f }},
		{"relative-path", func(f []hpack.HeaderField) []hpack.HeaderField { f[3].Value = "relative"; return f }},
		{"fragment-path", func(f []hpack.HeaderField) []hpack.HeaderField { f[3].Value = "/path#fragment"; return f }},
		{"asterisk-method", func(f []hpack.HeaderField) []hpack.HeaderField { f[3].Value = "*"; return f }},
		{"uppercase-field", func(f []hpack.HeaderField) []hpack.HeaderField { f[4].Name = "X-Field"; return f }},
		{"invalid-field", func(f []hpack.HeaderField) []hpack.HeaderField { f[4].Name = "invalid field"; return f }},
		{"invalid-value", func(f []hpack.HeaderField) []hpack.HeaderField { f[4].Value = "value\r\n"; return f }},
		{"whitespace-value", func(f []hpack.HeaderField) []hpack.HeaderField { f[4].Value = " value"; return f }},
	}
	for _, field := range []hpack.HeaderField{
		{Name: "content-length", Value: ""},
		{Name: "content-length", Value: "-1"},
		{Name: "content-length", Value: "+1"},
		{Name: "content-length", Value: "1x"},
		{Name: "content-length", Value: "9999999999999999999999999"},
		{Name: "transfer-encoding", Value: "chunked"},
		{Name: "te", Value: "gzip"},
		{Name: "upgrade", Value: "websocket"},
		{Name: "connection", Value: "upgrade"},
		{Name: "proxy-connection", Value: "keep-alive"},
		{Name: "keep-alive", Value: "timeout=1"},
	} {
		tests = append(tests, struct {
			name   string
			mutate func([]hpack.HeaderField) []hpack.HeaderField
		}{
			field.Name + "/" + field.Value, func(f []hpack.HeaderField) []hpack.HeaderField { return append(f, field) },
		})
	}
	tests = append(tests, struct {
		name   string
		mutate func([]hpack.HeaderField) []hpack.HeaderField
	}{
		"conflicting-length", func(f []hpack.HeaderField) []hpack.HeaderField {
			return append(f,
				hpack.HeaderField{Name: "content-length", Value: "1"}, hpack.HeaderField{Name: "content-length", Value: "2"})
		},
	})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fields := test.mutate(h2Fields())
			if _, err := h2ValidateRequest(fields); err == nil {
				t.Fatal("invalid materialised request passed pure preflight")
			}
			// This connection has no socket. Rejection must neither use transport nor
			// allocate a stream or advance the compression context.
			c := h2PreparationConn()
			c.encoded.WriteString("untouched")
			if _, err := c.h2Prepare(fields, nil, true, false); err == nil {
				t.Fatal("invalid preparation accepted")
			}
			if c.active != nil || c.nextStream != 1 || c.encoded.String() != "untouched" {
				t.Fatal("rejection mutated connection state")
			}
			actual, err := c.h2Prepare(h2Fields(), nil, true, false)
			if err != nil {
				t.Fatal(err)
			}
			expected, err := h2PreparationConn().h2Prepare(h2Fields(), nil, true, false)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(actual.Prefix[0].Bytes, expected.Prefix[0].Bytes) {
				t.Fatal("rejected preparation changed HPACK output")
			}
		})
	}
}

func TestH2RequestLengthEvidence(t *testing.T) {
	for _, declared := range []int64{-1, 0, 1, 999} {
		for _, body := range [][]byte{nil, {}, []byte("x"), []byte("payload")} {
			fields := h2Fields()
			if declared >= 0 {
				fields = append(fields, hpack.HeaderField{Name: "content-length", Value: fmt.Sprint(declared)},
					hpack.HeaderField{Name: "content-length", Value: fmt.Sprintf("0%d", declared)})
			}
			fields = append(fields, hpack.HeaderField{Name: "te", Value: "Trailers"})
			length, err := h2ValidateRequest(fields)
			if err != nil || length != declared {
				t.Fatalf("declared=%d body=%d evidence=%d err=%v", declared, len(body), length, err)
			}
			p, err := h2PreparationConn().h2Prepare(fields, body, true, true)
			if err != nil || p.DeclaredLength != declared {
				t.Fatalf("preparation declared=%d err=%v", declared, err)
			}
			if len(body) > 0 && p.Final[0].Data != 1 || len(body) == 0 && !p.Final[0].ClosesHeaders {
				t.Fatal("declared length changed actual finalisation shape")
			}
		}
	}
	fields := h2Fields()
	fields[0].Value = "OPTIONS"
	fields[3].Value = "*"
	fields[2].Value = "[::1]:8443"
	if _, err := h2ValidateRequest(fields); err != nil {
		t.Fatal(err)
	}
}

func TestH2ResponseEndAndPartialBodyEvidence(t *testing.T) {
	for _, length := range []string{"2", "9"} {
		t.Run(length, func(t *testing.T) {
			x := h2NewFixture(t, tls.VersionTLS13)
			x.client.bodyMax = 3
			p, err := x.client.h2Prepare(h2Fields(), nil, true, false)
			if err != nil {
				t.Fatal(err)
			}
			h2SendRecords(t, x, p, p.Prefix, true)
			_ = x.next(t)
			fields := []hpack.HeaderField{{Name: ":status", Value: "200"}, {Name: "content-length", Value: length}}
			x.respond(t, p.Stream, fields, false)
			x.send(t, func(f *http2.Framer) error { return f.WriteData(p.Stream, false, []byte("a")) })
			x.send(t, func(f *http2.Framer) error { return f.WriteData(p.Stream, true, []byte("text")) })
			obs := x.client.h2WaitResponse(x.ctx, p)
			if obs.Err == nil || obs.Times.Headers.IsZero() || obs.Times.Complete.IsZero() || string(obs.Body) != "ate" {
				t.Fatalf("partial evidence=%+v", obs)
			}
			if p.Received != 5 {
				t.Fatalf("received bytes=%d", p.Received)
			}
		})
	}
}

func TestH2MalformedFinalHeaderDiagnostics(t *testing.T) {
	x := h2NewFixture(t, tls.VersionTLS13)
	p, err := x.client.h2Prepare(h2Fields(), nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	h2SendRecords(t, x, p, p.Prefix, true)
	_ = x.next(t)
	fields := []hpack.HeaderField{{Name: ":status", Value: "200"}, {Name: "connection", Value: "close"}}
	x.respond(t, p.Stream, fields, true)
	obs := x.client.h2WaitResponse(x.ctx, p)
	if obs.Err == nil || !obs.Times.Headers.IsZero() || obs.Times.Complete.IsZero() || len(obs.DiagnosticHeaders) != 2 {
		t.Fatalf("malformed final diagnostics=%+v", obs)
	}
}

func TestH2NegativeWindowHeaderFinalisation(t *testing.T) {
	x := h2NewFixture(t, tls.VersionTLS13)
	p, err := x.client.h2Prepare(h2Fields(), []byte("x"), true, true)
	if err != nil {
		t.Fatal(err)
	}
	h2SendRecords(t, x, p, p.Prefix, false)
	_ = x.next(t)
	data := p.Final[0]
	data.Bytes = bytes.Clone(data.Bytes)
	data.Bytes[4] = 0 // Send real body bytes while leaving request direction open.
	if err := x.client.h2Write(x.ctx, p, data, false); err != nil {
		t.Fatal(err)
	}
	_ = x.next(t)
	// Trailers supply a legal header block after consuming stream DATA credit.
	// This exercises the primitive; last-byte policy itself rejects trailers.
	x.client.mu.Lock()
	x.client.encoded.Reset()
	err = x.client.encoder.WriteField(hpack.HeaderField{Name: "x-trailer", Value: "complete"})
	block := bytes.Clone(x.client.encoded.Bytes())
	x.client.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var prefix, final bytes.Buffer
	if err := http2.NewFramer(&prefix, nil).WriteHeaders(http2.HeadersFrameParam{
		StreamID: p.Stream, EndStream: true, BlockFragment: block[:len(block)-1],
	}); err != nil {
		t.Fatal(err)
	}
	if err := http2.NewFramer(&final, nil).WriteContinuation(p.Stream, true, block[len(block)-1:]); err != nil {
		t.Fatal(err)
	}
	if err := x.client.h2Write(x.ctx, p, h2Record{Bytes: prefix.Bytes(), OpensHeaders: true}, false); err != nil {
		t.Fatal(err)
	}
	_ = x.next(t)
	x.send(t, func(f *http2.Framer) error {
		return f.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 0})
	})
	for {
		x.client.mu.Lock()
		negative, changed := x.client.streamWindow < 0, x.client.changed
		x.client.mu.Unlock()
		if negative {
			break
		}
		select {
		case <-changed:
		case <-x.ctx.Done():
			t.Fatal("SETTINGS did not produce negative stream window")
		}
	}
	before := len(x.tap.snapshot())
	fragment := h2Record{Bytes: final.Bytes(), ClosesHeaders: true}
	if err := x.client.h2WaitReady(x.ctx, p, fragment); err != nil {
		t.Fatal(err)
	}
	if err := x.client.h2Write(x.ctx, p, fragment, true); err != nil {
		t.Fatal(err)
	}
	continuation := x.next(t)
	if continuation.Frame.Type != http2.FrameContinuation ||
		!continuation.Frame.Flags.Has(http2.FlagContinuationEndHeaders) {
		t.Fatalf("negative-window finalisation=%v", continuation.Frame)
	}
	_ = x.next(t) // Deferred SETTINGS ACK follows the final CONTINUATION.
	records := x.syncedRecords()[before:]
	if len(records) != 2 || records[0].Length != 10+17 {
		t.Fatalf("finalisation record=%v", records)
	}
	x.respond(t, p.Stream, []hpack.HeaderField{{Name: ":status", Value: "200"}}, true)
	if obs := x.client.h2WaitResponse(x.ctx, p); obs.Err != nil {
		t.Fatal(obs.Err)
	}
	// A future stream may also have zero DATA credit; headers need none.
	c := h2PreparationConn()
	prepared, err := c.h2Prepare(h2Fields(), nil, true, true)
	if err != nil {
		t.Fatal(err)
	}
	c.streamWindow = -1
	if err := c.readyLocked(prepared, prepared.Prefix[0]); err != nil {
		t.Fatalf("negative-window headers readiness=%v", err)
	}
}

func TestH2WindowOverflowErrorScope(t *testing.T) {
	for _, connection := range []bool{false, true} {
		t.Run(fmt.Sprint(connection), func(t *testing.T) {
			x := h2NewFixture(t, tls.VersionTLS13)
			p, err := x.client.h2Prepare(h2Fields(), nil, true, false)
			if err != nil {
				t.Fatal(err)
			}
			h2SendRecords(t, x, p, p.Prefix, true)
			_ = x.next(t)
			stream := p.Stream
			if connection {
				stream = 0
			}
			x.send(t, func(f *http2.Framer) error { return f.WriteWindowUpdate(stream, 0x7fffffff) })
			obs := x.client.h2WaitResponse(x.ctx, p)
			if obs.Err == nil || !obs.Times.Headers.IsZero() {
				t.Fatalf("overflow endpoint=%+v", obs)
			}
			if connection {
				failure, ok := errors.AsType[http2.ConnectionError](obs.Err)
				if !ok || failure != http2.ConnectionError(http2.ErrCodeFlowControl) {
					t.Fatalf("connection overflow=%v", obs.Err)
				}
				if _, err := x.client.h2Prepare(h2Fields(), nil, true, false); err == nil {
					t.Fatal("overflowed connection reused")
				}
				return
			}
			reset := x.next(t)
			if reset.Frame.Type != http2.FrameRSTStream || reset.Code != http2.ErrCodeFlowControl {
				t.Fatalf("stream overflow reset=%+v", reset)
			}
			if err := x.client.h2Release(p); err != nil {
				t.Fatal(err)
			}
			p, err = x.client.h2Prepare(h2Fields(), nil, true, false)
			if err != nil {
				t.Fatal(err)
			}
			h2SendRecords(t, x, p, p.Prefix, true)
			if fields := x.next(t).Fields; len(fields) != len(h2Fields()) {
				t.Fatalf("post-overflow HPACK=%v", fields)
			}
			x.respond(t, p.Stream, []hpack.HeaderField{{Name: ":status", Value: "200"}}, true)
			if obs := x.client.h2WaitResponse(x.ctx, p); obs.Err != nil {
				t.Fatal(obs.Err)
			}
		})
	}
}

func TestH2ResponseTEAndTrailerValidation(t *testing.T) {
	for _, name := range []string{"te", "host", "content-length", "authorization"} {
		t.Run(name, func(t *testing.T) {
			x := h2NewFixture(t, tls.VersionTLS13)
			p, err := x.client.h2Prepare(h2Fields(), nil, true, false)
			if err != nil {
				t.Fatal(err)
			}
			h2SendRecords(t, x, p, p.Prefix, true)
			_ = x.next(t)
			if name == "te" {
				x.respond(t, p.Stream, []hpack.HeaderField{{Name: ":status", Value: "200"}, {Name: "te", Value: "trailers"}}, true)
			} else {
				x.respond(t, p.Stream, []hpack.HeaderField{{Name: ":status", Value: "200"}}, false)
				x.respond(t, p.Stream, []hpack.HeaderField{{Name: name, Value: "1"}}, true)
			}
			obs := x.client.h2WaitResponse(x.ctx, p)
			if obs.Err == nil || obs.Times.Headers.IsZero() != (name == "te") || obs.Times.Complete.IsZero() {
				t.Fatalf("invalid field=%+v", obs)
			}
			if reset := x.next(t); reset.Frame.Type != http2.FrameRSTStream || reset.Code != http2.ErrCodeProtocol {
				t.Fatalf("invalid-field reset=%+v", reset)
			}
		})
	}
}

func TestH2CompressionAndFramingErrorsDisposeConnection(t *testing.T) {
	for _, kind := range []string{"compression", "continuity", "frame-size", "ping-length"} {
		t.Run(kind, func(t *testing.T) {
			x := h2NewFixture(t, tls.VersionTLS13)
			p, err := x.client.h2Prepare(h2Fields(), nil, true, false)
			if err != nil {
				t.Fatal(err)
			}
			h2SendRecords(t, x, p, p.Prefix, true)
			_ = x.next(t)
			x.commands <- func(f *http2.Framer) error {
				switch kind {
				case "compression":
					return f.WriteHeaders(http2.HeadersFrameParam{
						StreamID: p.Stream, EndHeaders: true, EndStream: true, BlockFragment: []byte{0x80},
					})
				case "continuity":
					if err := f.WriteHeaders(http2.HeadersFrameParam{StreamID: p.Stream, BlockFragment: []byte{0x88}}); err != nil {
						return err
					}
					return f.WritePing(false, [8]byte{8})
				case "frame-size":
					return f.WriteRawFrame(http2.FrameData, 0, p.Stream, make([]byte, 16385))
				default:
					return f.WriteRawFrame(http2.FramePing, 0, 0, []byte{1})
				}
			}
			obs := x.client.h2WaitResponse(x.ctx, p)
			if obs.Err == nil || !obs.Times.Headers.IsZero() {
				t.Fatalf("malformed frame=%+v", obs)
			}
			if kind == "frame-size" {
				if !errors.Is(obs.Err, http2.ErrFrameTooLarge) {
					t.Fatalf("oversized frame error=%v", obs.Err)
				}
			} else {
				want := http2.ErrCodeProtocol
				if kind == "compression" {
					want = http2.ErrCodeCompression
				}
				if kind == "ping-length" {
					want = http2.ErrCodeFrameSize
				}
				code, ok := errors.AsType[http2.ConnectionError](obs.Err)
				if !ok || code != http2.ConnectionError(want) {
					t.Fatalf("connection framing error=%v; want %s", obs.Err, want)
				}
			}
			select {
			case <-x.client.done:
			default:
				t.Fatal("connection protocol failure stayed live")
			}
			if _, err := x.client.h2Prepare(h2Fields(), nil, true, false); err == nil {
				t.Fatal("malformed connection reused")
			}
		})
	}
}

func TestH2FailedResponseRestoresConnectionCredit(t *testing.T) {
	for _, failure := range []string{"length", "limit", "head"} {
		t.Run(failure, func(t *testing.T) {
			x := h2NewFixture(t, tls.VersionTLS13)
			fields := h2Fields()
			if failure == "head" {
				fields[0].Value = "HEAD"
			}
			if failure == "limit" {
				x.client.bodyMax = 3
			}
			p, err := x.client.h2Prepare(fields, nil, true, false)
			if err != nil {
				t.Fatal(err)
			}
			h2SendRecords(t, x, p, p.Prefix, true)
			_ = x.next(t)
			length := "16384"
			if failure == "length" {
				length = "0"
			}
			response := []hpack.HeaderField{{Name: ":status", Value: "200"}, {Name: "content-length", Value: length}}
			x.respond(t, p.Stream, response, false)
			x.send(t, func(f *http2.Framer) error { return f.WriteData(p.Stream, true, make([]byte, 16384)) })
			obs := x.client.h2WaitResponse(x.ctx, p)
			if obs.Err == nil || obs.Times.Headers.IsZero() {
				t.Fatalf("failed response=%+v", obs)
			}
			credit := x.next(t)
			if credit.Frame.Type != http2.FrameWindowUpdate || credit.Frame.StreamID != 0 {
				t.Fatalf("failed-stream connection credit=%+v", credit)
			}
			reset := x.next(t)
			if reset.Frame.Type != http2.FrameRSTStream {
				t.Fatalf("failed-stream reset=%+v", reset)
			}
			x.client.mu.Lock()
			restored := x.client.receiveWindow
			x.client.bodyMax = 1 << 20
			x.client.mu.Unlock()
			if restored != 65535 {
				t.Fatalf("reusable connection receive credit=%d", restored)
			}
			if err := x.client.h2Release(p); err != nil {
				t.Fatal(err)
			}
			p, err = x.client.h2Prepare(h2Fields(), nil, true, false)
			if err != nil {
				t.Fatal(err)
			}
			h2SendRecords(t, x, p, p.Prefix, true)
			_ = x.next(t)
			body := bytes.Repeat([]byte{'x'}, 80000)
			response = []hpack.HeaderField{{Name: ":status", Value: "200"}, {Name: "content-length", Value: "80000"}}
			x.respond(t, p.Stream, response, false)
			for remaining := body; len(remaining) > 0; {
				n := min(16384, len(remaining))
				data := remaining[:n]
				remaining = remaining[n:]
				end := len(remaining) == 0
				x.send(t, func(f *http2.Framer) error { return f.WriteData(p.Stream, end, data) })
				// The peer replenishes each DATA chunk from actual client updates, keeping
				// both receive windows live throughout the larger subsequent response.
				for range 2 {
					update := x.next(t)
					if update.Frame.Type != http2.FrameWindowUpdate {
						t.Fatalf("healthy receive control=%v", update.Frame)
					}
				}
			}
			obs = x.client.h2WaitResponse(x.ctx, p)
			if obs.Err != nil || len(obs.Body) != 80000 || obs.Times.Complete.IsZero() {
				t.Fatalf("subsequent response bytes=%d err=%v", len(obs.Body), obs.Err)
			}
		})
	}
}

func TestH2ClosedStreamFramesAndIdleErrors(t *testing.T) {
	for _, idle := range []bool{false, true} {
		for _, even := range []bool{false, true} {
			t.Run(fmt.Sprintf("idle%t/even%t", idle, even), func(t *testing.T) {
				x := h2NewFixture(t, tls.VersionTLS13)
				first, err := x.client.h2Prepare(h2Fields(), nil, true, false)
				if err != nil {
					t.Fatal(err)
				}
				h2SendRecords(t, x, first, first.Prefix, true)
				_ = x.next(t)
				response := []hpack.HeaderField{{Name: ":status", Value: "200"}, {Name: "te", Value: "trailers"}}
				x.respond(t, first.Stream, response, true)
				if obs := x.client.h2WaitResponse(x.ctx, first); obs.Err == nil {
					t.Fatal("malformed stream did not reset")
				}
				_ = x.next(t)
				if err := x.client.h2Release(first); err != nil {
					t.Fatal(err)
				}
				p, err := x.client.h2Prepare(h2Fields(), nil, true, false)
				if err != nil {
					t.Fatal(err)
				}
				h2SendRecords(t, x, p, p.Prefix, true)
				_ = x.next(t)
				stream := first.Stream
				if idle {
					stream = p.Stream + 2
				}
				if even {
					stream = 2
				}
				if idle || even {
					x.send(t, func(f *http2.Framer) error { return f.WriteData(stream, true, []byte("late")) })
					if obs := x.client.h2WaitResponse(x.ctx, p); obs.Err == nil {
						t.Fatal("idle/even stream DATA accepted")
					}
					return
				}
				// Late frames on the known reset stream still advance peer HPACK state
				// and consume connection credit. Neither belongs to the new observation.
				var responseBlock bytes.Buffer
				encoder := hpack.NewEncoder(&responseBlock)
				respond := func(target uint32) {
					responseBlock.Reset()
					for _, field := range []hpack.HeaderField{
						{Name: ":status", Value: "200"},
						{Name: "x-late", Value: "compression history"},
					} {
						if err := encoder.WriteField(field); err != nil {
							t.Fatal(err)
						}
					}
					x.send(t, func(f *http2.Framer) error {
						return f.WriteHeaders(http2.HeadersFrameParam{
							StreamID: target, EndHeaders: true, EndStream: true, BlockFragment: responseBlock.Bytes(),
						})
					})
				}
				respond(stream)
				x.send(t, func(f *http2.Framer) error { return f.WriteData(stream, true, []byte("late")) })
				update := x.next(t)
				if update.Frame.Type != http2.FrameWindowUpdate || update.Frame.StreamID != 0 {
					t.Fatalf("late DATA credit=%v", update.Frame)
				}
				respond(p.Stream)
				obs := x.client.h2WaitResponse(x.ctx, p)
				if obs.Err != nil || len(obs.Body) != 0 || len(obs.Headers) != 2 {
					t.Fatalf("late stream contaminated next response=%+v", obs)
				}
			})
		}
	}
}

func TestH2DecodedHeaderAndTrailerLimits(t *testing.T) {
	for _, trailers := range []bool{false, true} {
		t.Run(fmt.Sprint(trailers), func(t *testing.T) {
			x := h2NewFixture(t, tls.VersionTLS13)
			p, err := x.client.h2Prepare(h2Fields(), nil, true, false)
			if err != nil {
				t.Fatal(err)
			}
			h2SendRecords(t, x, p, p.Prefix, true)
			_ = x.next(t)
			if trailers {
				x.respond(t, p.Stream, []hpack.HeaderField{{Name: ":status", Value: "200"}}, false)
			}
			var b bytes.Buffer
			encoder := hpack.NewEncoder(&b)
			if !trailers {
				if err := encoder.WriteField(hpack.HeaderField{Name: ":status", Value: "200"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := encoder.WriteField(hpack.HeaderField{Name: "x-large", Value: strings.Repeat("a", 1<<20)}); err != nil {
				t.Fatal(err)
			}
			x.send(t, func(f *http2.Framer) error {
				block := b.Bytes()
				first := true
				for len(block) > 0 {
					n := min(16384, len(block))
					end := n == len(block)
					if first {
						if err := f.WriteHeaders(http2.HeadersFrameParam{
							StreamID: p.Stream, EndStream: true, EndHeaders: end, BlockFragment: block[:n],
						}); err != nil {
							return err
						}
					} else {
						if err := f.WriteContinuation(p.Stream, end, block[:n]); err != nil {
							return err
						}
					}
					first = false
					block = block[n:]
				}
				return nil
			})
			obs := x.client.h2WaitResponse(x.ctx, p)
			if obs.Err == nil || obs.Times.Headers.IsZero() == trailers || obs.Times.Complete.IsZero() {
				t.Fatalf("decoded header bound=%+v", obs)
			}
			if reset := x.next(t); reset.Frame.Type != http2.FrameRSTStream {
				t.Fatalf("decoded header bound reset=%v", reset.Frame)
			}
		})
	}
}
