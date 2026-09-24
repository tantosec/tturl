package timing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"hash"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

func transportTestPlan(server *httptest.Server) Plan {
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	return Plan{
		SingleRecord: true, LastByteSync: true, RequestTimeout: time.Second,
		ReceiveHeaderMax: 4096, ResponseBodyMax: -1,
		Capture:   Capture{Headers: true, Body: true, BodyMax: 9},
		TLSConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}
}

func transportTestRequest(t *testing.T, address string, protocol Protocol) Request {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, address, nil)
	if err != nil {
		t.Fatal(err)
	}
	return Request{ID: 1, HTTP: request, Protocol: protocol}
}

func TestTransportLeaseDrainsBeyondCaptureAndReuses(t *testing.T) {
	body := bytes.Repeat([]byte("opaque body "), 8000)
	sum := sha256.Sum256(body)
	for _, protocol := range []Protocol{HTTP11, HTTP2} {
		t.Run(string(protocol), func(t *testing.T) {
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Trailer", "X-End")
				_, _ = w.Write(body)
				w.Header().Set("X-End", "drained")
			}))
			server.EnableHTTP2 = true
			server.StartTLS()
			defer server.Close()
			plan := transportTestPlan(server)
			request := transportTestRequest(t, server.URL, protocol)
			acquireCtx, cancel := context.WithCancel(t.Context())
			lease, err := acquireTransport(acquireCtx, request, plan)
			if err != nil {
				t.Fatal(err)
			}
			cancel()
			defer func() { lease.dispose(nil); lease.join() }()
			negotiated := lease.metadata().negotiatedProtocol
			if negotiated != string(protocol) && (protocol != HTTP11 || negotiated != "") {
				t.Fatal("protocol selection changed")
			}
			for range 2 {
				sender, err := lease.prepare(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
				if err := sender.waitReady(t.Context(), initialPhase); err != nil {
					t.Fatal(err)
				}
				lease.lockState()
				err = sender.readyLocked(initialPhase)
				lease.unlockState()
				if err != nil {
					t.Fatal(err)
				}
				initial, final := make(chan struct{}), make(chan struct{})
				close(initial)
				close(final)
				var completed atomic.Int32
				outcome := sender.run(t.Context(), releaseGates{initial, final, time.Now()}, senderHooks{
					exchangeComplete: func(time.Time) error { completed.Add(1); return nil },
				})
				response := outcome.Response
				if outcome.Failure != nil || !response.Complete || !outcome.Timing.ResponseComplete.Present {
					t.Fatalf("drain failed: %+v", outcome)
				}
				if completed.Load() != 1 || response.AcceptedBodyBytes != int64(len(body)) ||
					response.ReceivedBodyBytes != int64(len(body)) || response.Digest != base64.StdEncoding.EncodeToString(sum[:]) {
					t.Fatal("full accepted-body evidence or completion hook lost")
				}
				if !bytes.Equal(response.Body, body[:9]) || !response.CaptureTruncated ||
					response.Trailers.Get("X-End") != "drained" {
					t.Fatal("capture policy changed drain or lost trailers")
				}
				if ok, reason := lease.usable(); !ok {
					t.Fatalf("full drain did not release lease: %s", reason)
				}
			}
		})
	}
}

func TestTransportReceiveLimitRetainsAcceptedPrefix(t *testing.T) {
	for _, protocol := range []Protocol{HTTP11, HTTP2} {
		t.Run(string(protocol), func(t *testing.T) {
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "abcdef")
			}))
			server.EnableHTTP2 = true
			server.StartTLS()
			defer server.Close()
			plan := transportTestPlan(server)
			plan.ResponseBodyMax = 3
			plan.Capture.BodyMax = -1
			request := transportTestRequest(t, server.URL, protocol)
			lease, err := acquireTransport(t.Context(), request, plan)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { lease.dispose(nil); lease.join() }()
			sender, err := lease.prepare(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			gate := make(chan struct{})
			close(gate)
			outcome := sender.run(t.Context(), releaseGates{gate, gate, time.Now()}, senderHooks{})
			sum := sha256.Sum256([]byte("abc"))
			response := outcome.Response
			if outcome.Failure == nil || response.Complete || !response.Truncated ||
				response.AcceptedBodyBytes != 3 || response.ReceivedBodyBytes <= 3 ||
				response.Digest != base64.StdEncoding.EncodeToString(sum[:]) || string(response.Body) != "abc" {
				t.Fatalf("receive cap lost partial evidence: %+v", outcome)
			}
		})
	}
}

func TestTransportPreparedHPACKAbandonmentRetiresLease(t *testing.T) {
	x := h2NewFixture(t, tls.VersionTLS13)
	lease := &h2Lease{conn: x.client, plan: Plan{ReceiveHeaderMax: 4096, ResponseBodyMax: -1}}
	request := transportTestRequest(t, "https://fixture.test/resource", HTTP2)
	if _, err := lease.prepare(x.ctx, request); err != nil {
		t.Fatal(err)
	}
	lease.dispose(errors.New("prepared request abandoned"))
	lease.join()
	if ok, _ := lease.usable(); ok {
		t.Fatal("abandoned normal HPACK state remained reusable")
	}
}

func TestTransportPeerCannotExpandLocalHPACKBudget(t *testing.T) {
	x := h2NewFixture(t, tls.VersionTLS13)
	x.send(t, func(f *http2.Framer) error {
		return f.WriteSettings(http2.Setting{ID: http2.SettingHeaderTableSize, Val: ^uint32(0)})
	})
	for {
		frame := x.next(t)
		if frame.Frame.Type == http2.FrameSettings && frame.Frame.Flags.Has(http2.FlagSettingsAck) {
			break
		}
	}
	x.client.mu.Lock()
	size := x.client.encoder.MaxDynamicTableSize()
	x.client.mu.Unlock()
	if size != h2TableLimit {
		t.Fatalf("peer expanded local HPACK budget: %d", size)
	}
}

func TestTransportRejectsPlainControlledDeliveryBeforeDial(t *testing.T) {
	request := transportTestRequest(t, "http://127.0.0.1:1/resource", HTTP11)
	_, err := acquireTransport(t.Context(), request, Plan{SingleRecord: true, ReceiveHeaderMax: 4096})
	if err == nil {
		t.Fatal("plain controlled delivery was not rejected locally")
	}
}

type transportBlockedDigest struct {
	hash.Hash
	entered chan struct{}
	release <-chan struct{}
}

func (h *transportBlockedDigest) Write(body []byte) (int, error) {
	close(h.entered)
	<-h.release
	return h.Hash.Write(body)
}

func TestTransportCompletionPrecedesTerminalBodyDigest(t *testing.T) {
	x := h2NewFixture(t, tls.VersionTLS13)
	lease := &h2Lease{conn: x.client, plan: Plan{ResponseBodyMax: -1}}
	request := transportTestRequest(t, "https://fixture.test/resource", HTTP2)
	sender, err := lease.prepare(x.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	prepared := sender.(*h2Sender).prepared
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	prepared.collector.digest = &transportBlockedDigest{sha256.New(), entered, release}
	completed := make(chan struct{})
	gate := make(chan struct{})
	close(gate)
	result := make(chan Outcome, 1)
	go func() {
		result <- sender.run(x.ctx, releaseGates{gate, gate, time.Now()}, senderHooks{
			exchangeComplete: func(time.Time) error { close(completed); return nil },
		})
	}()
	for {
		frame := x.next(t)
		if frame.Frame.Type == http2.FrameHeaders {
			break
		}
	}
	// Wire ownership ends after Write returns and Written is stored, so the
	// controlled response arrives after complete outbound delivery.
	x.client.wire.Lock()
	x.client.mu.Lock()
	written := !prepared.Obs.Times.Written.IsZero()
	x.client.mu.Unlock()
	x.client.wire.Unlock()
	if !written {
		t.Fatal("outbound write did not finish before controlled response")
	}
	x.respond(t, prepared.Stream, []hpack.HeaderField{{Name: ":status", Value: "200"}}, false)
	x.send(t, func(f *http2.Framer) error { return f.WriteData(prepared.Stream, true, []byte("body")) })
	select {
	case <-entered:
	case <-x.ctx.Done():
		t.Fatal(x.ctx.Err())
	}
	select {
	case <-completed:
	default:
		t.Fatal("terminal digest delayed execution completion")
	}
	unblock()
	outcome := <-result
	if outcome.Failure != nil || !outcome.Response.Complete || !outcome.Timing.ResponseComplete.Present {
		t.Fatalf("digest processing changed terminal evidence: %+v", outcome)
	}
}

func TestH1TransportCompletionPrecedesTerminalConsumption(t *testing.T) {
	client, peer, _ := h1TLS(t, tls.VersionTLS13)
	request := transportTestRequest(t, "https://localhost/resource", HTTP11)
	prepared, err := prepareH1(request.HTTP, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	close(gate)
	entered, release, completed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	result := make(chan h1Observation, 1)
	go func() {
		result <- runH1WithCallbacks(t.Context(), client, prepared, gate, gate, 4096, -1,
			nil, nil, nil, func(body []byte, observation h1Observation) error {
				if observation.complete.IsZero() {
					return nil
				}
				close(entered)
				<-release
				return nil
			}, func(time.Time) error { close(completed); return nil })
	}()
	peerDone := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(peer, make([]byte, len(prepared.records[0])))
		if err == nil {
			_, err = io.WriteString(peer, "HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\nbody")
		}
		peerDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("terminal consumer did not start")
	}
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("terminal consumer delayed completion hook")
	}
	unblock()
	observation := <-result
	if observation.err != nil || observation.complete.IsZero() {
		t.Fatal("terminal observation lost")
	}
	if err := <-peerDone; err != nil {
		t.Fatal(err)
	}
}

func TestH1FinalExchangeRetainsValidatedRetirement(t *testing.T) {
	for _, test := range []struct {
		name, response, reason string
		valid                  bool
	}{
		{"close", "HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: 4\r\n\r\nbody", "connection close", true},
		{"EOF", "HTTP/1.1 200 OK\r\nConnection: close\r\n\r\nbody", "EOF", true},
		{"short-body", "HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: 5\r\n\r\nabc", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, peer, _ := h1TLS(t, tls.VersionTLS13)
			lease := &h1Lease{conn: client, plan: Plan{SingleRecord: true, ReceiveHeaderMax: 4096, ResponseBodyMax: -1}}
			request := transportTestRequest(t, "https://localhost/resource", HTTP11)
			sender, err := lease.prepare(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			prepared := sender.(*h1Sender).prepared
			peerDone := make(chan error, 1)
			go func() {
				_, err := io.ReadFull(peer, make([]byte, len(prepared.records[0])))
				if err == nil {
					_, err = io.WriteString(peer, test.response)
				}
				_ = peer.Close()
				peerDone <- err
			}()
			gate := make(chan struct{})
			close(gate)
			outcome := sender.run(t.Context(), releaseGates{gate, gate, time.Now()}, senderHooks{})
			retired, reason := lease.retirement()
			if retired != test.valid || test.valid && !strings.Contains(reason, test.reason) {
				t.Fatalf("protocol retirement evidence lost or fabricated: %t %q", retired, reason)
			}
			if test.valid {
				if outcome.Failure != nil || !outcome.Response.Complete ||
					!outcome.Timing.ResponseComplete.Present || !outcome.Timing.Duration.Present {
					t.Fatalf("retirement changed valid final timing: %+v", outcome)
				}
			} else if outcome.Failure == nil || outcome.Response.Complete || !outcome.Response.LengthMismatch {
				t.Fatal("broken final response became normal retirement")
			}
			if ok, _ := lease.usable(); ok {
				t.Fatal("retired or broken HTTP/1.1 lease remained usable")
			}
			if err := <-peerDone; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestH2FinalExchangeRetainsGOAWAYAfterPeerEOF(t *testing.T) {
	for _, code := range []http2.ErrCode{http2.ErrCodeNo, http2.ErrCodeInternal} {
		t.Run(code.String(), func(t *testing.T) {
			x := h2NewFixture(t, tls.VersionTLS13)
			lease := &h2Lease{conn: x.client, plan: Plan{ResponseBodyMax: -1}}
			request := transportTestRequest(t, "https://fixture.test/resource", HTTP2)
			sender, err := lease.prepare(x.ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			p := sender.(*h2Sender).prepared
			gate := make(chan struct{})
			close(gate)
			result := make(chan Outcome, 1)
			go func() { result <- sender.run(x.ctx, releaseGates{gate, gate, time.Now()}, senderHooks{}) }()
			for x.next(t).Frame.Type != http2.FrameHeaders {
			}
			x.send(t, func(f *http2.Framer) error { return f.WriteGoAway(p.Stream, code, []byte("opaque debug content")) })
			x.respond(t, p.Stream, []hpack.HeaderField{{Name: ":status", Value: "200"}}, true)
			outcome := <-result
			_ = x.peer.Close()
			lease.join()
			retired, reason := lease.retirement()
			if !retired || !strings.Contains(reason, code.String()) ||
				!strings.Contains(reason, "last stream 1") || strings.Contains(reason, "opaque") {
				t.Fatalf("GOAWAY evidence did not survive peer EOF: %t %q", retired, reason)
			}
			if outcome.Failure != nil || !outcome.Response.Complete ||
				!outcome.Timing.ResponseComplete.Present || !outcome.Timing.Duration.Present {
				t.Fatalf("accepted retiring stream lost valid timing: %+v", outcome)
			}
			if ok, _ := lease.usable(); ok {
				t.Fatal("GOAWAY lease remained usable")
			}
		})
	}
}

func TestH2BrokenExchangeDoesNotFabricateRetirement(t *testing.T) {
	x := h2NewFixture(t, tls.VersionTLS13)
	lease := &h2Lease{conn: x.client, plan: Plan{ResponseBodyMax: -1}}
	request := transportTestRequest(t, "https://fixture.test/resource", HTTP2)
	sender, err := lease.prepare(x.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	p := sender.(*h2Sender).prepared
	gate := make(chan struct{})
	close(gate)
	result := make(chan Outcome, 1)
	go func() { result <- sender.run(x.ctx, releaseGates{gate, gate, time.Now()}, senderHooks{}) }()
	for x.next(t).Frame.Type != http2.FrameHeaders {
	}
	x.respond(t, p.Stream, []hpack.HeaderField{
		{Name: ":status", Value: "200"}, {Name: "content-length", Value: "5"},
	}, false)
	x.send(t, func(f *http2.Framer) error { return f.WriteData(p.Stream, true, []byte("abc")) })
	outcome := <-result
	if retired, reason := lease.retirement(); retired || reason != "" {
		t.Fatal("broken stream fabricated GOAWAY")
	}
	if outcome.Failure == nil || outcome.Response.Complete ||
		!outcome.Response.LengthMismatch || !outcome.Timing.ResponseComplete.Present {
		t.Fatalf("broken stream lost independent end evidence: %+v", outcome)
	}
}

func TestH2MalformedHeaderCaptureRetainsOnlyObservedEndpoints(t *testing.T) {
	for _, trailers := range []bool{false, true} {
		name := "initial"
		if trailers {
			name = "trailers"
		}
		t.Run(name, func(t *testing.T) {
			x := h2NewFixture(t, tls.VersionTLS13)
			lease := &h2Lease{conn: x.client, plan: Plan{ResponseBodyMax: -1, Capture: Capture{Headers: true}}}
			request := transportTestRequest(t, "https://fixture.test/resource", HTTP2)
			sender, err := lease.prepare(x.ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			p := sender.(*h2Sender).prepared
			gate := make(chan struct{})
			close(gate)
			result := make(chan Outcome, 1)
			go func() { result <- sender.run(x.ctx, releaseGates{gate, gate, time.Now()}, senderHooks{}) }()
			for x.next(t).Frame.Type != http2.FrameHeaders {
			}
			fields := []hpack.HeaderField{{Name: "x-observed", Value: "diagnostic"}, {Name: "content-length", Value: "+1"}}
			if trailers {
				x.respond(t, p.Stream, []hpack.HeaderField{
					{Name: ":status", Value: "200"}, {Name: "x-valid", Value: "final"},
				}, false)
			} else {
				fields = append([]hpack.HeaderField{{Name: ":status", Value: "200"}}, fields...)
			}
			x.respond(t, p.Stream, fields, true)
			outcome := <-result
			captured := outcome.Response.Headers
			if trailers {
				captured = outcome.Response.Trailers
				if outcome.Response.Headers.Get("X-Valid") != "final" {
					t.Fatal("malformed trailer replaced validated headers")
				}
			}
			if captured.Get("X-Observed") != "diagnostic" || captured.Get("Content-Length") != "+1" {
				t.Fatal("malformed header section lost captured diagnostic fields")
			}
			if outcome.Failure == nil || outcome.Response.Complete || !outcome.Timing.FirstResponseHeaders.Present ||
				!outcome.Timing.ResponseComplete.Present || outcome.Timing.FinalResponseHeaders.Present != trailers {
				t.Fatalf("capture fabricated or erased a reader endpoint: %+v", outcome)
			}
		})
	}
}
