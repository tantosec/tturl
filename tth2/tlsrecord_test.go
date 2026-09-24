package tth2_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/tantosec/tturl/internal/h2test"
	"github.com/tantosec/tturl/tth2"
)

const (
	tlsRecordHeaderBytes = 5
	tlsApplicationData   = 23
	tlsPlaintextLimit    = 1 << 14
	dataFrameHeaderBytes = 9
)

// TestFinalReleaseUsesOneTLSRecord observes the encrypted client wire rather
// than inferring record count from buffered plaintext. It covers both supported
// TLS versions and each load-bearing send shape. The exact-limit case also
// detects dynamic record sizing: on a fresh connection, Go would split a
// 16 KiB write into smaller records unless tth2 disabled that behaviour.
func TestFinalReleaseUsesOneTLSRecord(t *testing.T) {
	tests := []struct {
		name          string
		requests      int
		bodyBytes     int
		withheld      int
		single        bool
		directRecords int
	}{
		{name: "no-body-wide", requests: 128},
		{
			name: "body-partial-wide", requests: 32,
			bodyBytes: 1024, withheld: 1,
		},
		{
			name: "body-single-burst-wide", requests: 128,
			bodyBytes: 1, single: true,
		},
		{
			name: "body-final-frame-wide", requests: 128,
			bodyBytes: 1, withheld: 1,
		},
		{
			name: "body-exact-record-limit", requests: 2,
			bodyBytes: (tlsPlaintextLimit - 2*dataFrameHeaderBytes) / 2,
			withheld:  (tlsPlaintextLimit - 2*dataFrameHeaderBytes) / 2,
		},
		{name: "fitting-complete-body", requests: 2, bodyBytes: 4, directRecords: 2},
		{name: "fitting-one-byte-tail", requests: 2, bodyBytes: 4, withheld: 1, directRecords: 3},
		{name: "fitting-three-byte-tail", requests: 2, bodyBytes: 4, withheld: 3, directRecords: 3},
		{name: "fitting-whole-body-tail", requests: 2, bodyBytes: 4, withheld: 4, directRecords: 2},
		{name: "greedy-oversized-zero", requests: 2, bodyBytes: 10000, directRecords: 3},
	}
	versions := []struct {
		name    string
		version uint16
	}{
		{name: "TLS12", version: tls.VersionTLS12},
		{name: "TLS13", version: tls.VersionTLS13},
	}

	for _, version := range versions {
		for _, test := range tests {
			t.Run(version.name+"/"+test.name, func(t *testing.T) {
				t.Parallel()
				testFinalReleaseRecord(t, version.version, test.requests,
					test.bodyBytes, test.withheld, test.single, test.directRecords)
			})
		}
	}
}

func testFinalReleaseRecord(
	t *testing.T,
	tlsVersion uint16,
	requestCount int,
	bodyBytes int,
	withheld int,
	singleBurst bool,
	directRecords int,
) {
	t.Helper()
	ready := make(chan error, requestCount)
	negotiated := make(chan uint16, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/warm" {
			negotiated <- r.TLS.Version
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if bodyBytes == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		pivot := int64(bodyBytes - withheld)
		_, err := io.CopyN(io.Discard, r.Body, pivot)
		ready <- err
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	addr, tlsConfig := h2test.Serve(t, handler)
	tlsConfig.MinVersion = tlsVersion
	tlsConfig.MaxVersion = tlsVersion

	connected := make(chan *wireRecordingConn, 1)
	var dialOnce sync.Once
	dialer := func(
		ctx context.Context,
		network string,
		address string,
	) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		recorded := &wireRecordingConn{Conn: conn}
		dialOnce.Do(func() { connected <- recorded })
		return recorded, nil
	}
	transport := &tth2.Transport{
		TLSClientConfig: tlsConfig,
		DialContext:     dialer,
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := &tth2.Client{Transport: transport}

	warm, err := http.NewRequestWithContext(
		t.Context(), http.MethodGet, "https://"+addr+"/warm", nil)
	if err != nil {
		t.Fatalf("warm request: %v", err)
	}
	if _, err := client.SendBatch(t.Context(), []*http.Request{warm}); err != nil {
		t.Fatalf("warm SendBatch: %v", err)
	}
	if got := <-negotiated; got != tlsVersion {
		t.Fatalf("negotiated TLS version = %#x, want %#x", got, tlsVersion)
	}
	var wire *wireRecordingConn
	select {
	case wire = <-connected:
	case <-time.After(time.Second):
		t.Fatal("dialer did not expose the recorded connection")
	}

	requests := make([]*http.Request, requestCount)
	for i := range requests {
		var body io.Reader
		method := http.MethodGet
		if bodyBytes > 0 {
			method = http.MethodPost
			body = bytes.NewReader(bytes.Repeat([]byte{byte(i)}, bodyBytes))
		}
		request, err := http.NewRequestWithContext(
			t.Context(), method, "https://"+addr+"/test", body)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		requests[i] = request
	}

	if bodyBytes == 0 {
		start := wire.mark()
		batch, err := client.SendBatch(t.Context(), requests)
		if err != nil {
			t.Fatalf("SendBatch: %v", err)
		}
		assertStatuses(t, batch)
		assertOneApplicationRecord(t, wire.bytesBetween(start, wire.mark()))
		return
	}
	if singleBurst {
		start := wire.mark()
		batch, err := client.SendBatch(
			t.Context(), requests, tth2.WithBodyBytesWithheld(0))
		if err != nil {
			t.Fatalf("SendBatch: %v", err)
		}
		assertStatuses(t, batch)
		assertApplicationRecords(t, wire.bytesBetween(start, wire.mark()), 2)
		return
	}
	if directRecords > 0 {
		start := wire.mark()
		batch, err := client.SendBatch(
			t.Context(), requests, tth2.WithBodyBytesWithheld(withheld))
		if err != nil {
			t.Fatalf("SendBatch: %v", err)
		}
		assertStatuses(t, batch)
		assertApplicationRecords(t, wire.bytesBetween(start, wire.mark()), directRecords)
		return
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan sendOutcome, 1)
	go func() {
		batch, err := client.SendBatch(
			ctx,
			requests,
			tth2.WithBodyBytesWithheld(withheld),
			tth2.WithReleaseDelay(2*time.Second),
		)
		done <- sendOutcome{batch: batch, err: err}
	}()

	for i := range requestCount {
		select {
		case err := <-ready:
			if err != nil {
				t.Fatalf("handler %d could not read body prefix: %v", i, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("handler %d did not reach the final-release gate", i)
		}
	}
	start := wire.mark()
	outcome := <-done
	if outcome.err != nil {
		t.Fatalf("SendBatch: %v", outcome.err)
	}
	assertStatuses(t, outcome.batch)
	assertOneApplicationRecord(t, wire.bytesBetween(start, wire.mark()))
}

type sendOutcome struct {
	batch *tth2.BatchResult
	err   error
}

func assertStatuses(t *testing.T, batch *tth2.BatchResult) {
	t.Helper()
	for i, result := range batch.Results {
		if !result.Arrived() {
			t.Fatalf("result %d has no response", i)
		}
		if got := result.Response.StatusCode; got != http.StatusNoContent {
			t.Fatalf("result %d status = %d, want %d",
				i, got, http.StatusNoContent)
		}
	}
}

func assertOneApplicationRecord(t *testing.T, wire []byte) {
	t.Helper()
	assertApplicationRecords(t, wire, 1)
}

func assertApplicationRecords(t *testing.T, wire []byte, want int) {
	t.Helper()
	records, err := tlsRecordTypes(wire)
	if err != nil {
		t.Fatalf("parse client TLS records: %v", err)
	}
	if len(records) != want {
		t.Fatalf("TLS record types = %v, want %d application records",
			records, want)
	}
	for i, recordType := range records {
		if recordType != tlsApplicationData {
			t.Fatalf("TLS record %d type = %d, want %d; all types = %v",
				i, recordType, tlsApplicationData, records)
		}
	}
}

func tlsRecordTypes(wire []byte) ([]byte, error) {
	var records []byte
	for len(wire) > 0 {
		if len(wire) < tlsRecordHeaderBytes {
			return nil, fmt.Errorf("trailing %d-byte partial header", len(wire))
		}
		length := int(binary.BigEndian.Uint16(wire[3:5]))
		recordBytes := tlsRecordHeaderBytes + length
		if len(wire) < recordBytes {
			return nil, fmt.Errorf(
				"record type %d needs %d bytes, have %d",
				wire[0], recordBytes, len(wire))
		}
		records = append(records, wire[0])
		wire = wire[recordBytes:]
	}
	return records, nil
}

func TestTLSRecordTypes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		wire    []byte
		want    []byte
		wantErr bool
	}{
		{
			name: "multiple complete records",
			wire: []byte{
				tlsApplicationData, 3, 3, 0, 2, 'a', 'b',
				22, 3, 3, 0, 1, 'c',
			},
			want: []byte{tlsApplicationData, 22},
		},
		{name: "partial header", wire: []byte{23, 3}, wantErr: true},
		{
			name:    "partial payload",
			wire:    []byte{tlsApplicationData, 3, 3, 0, 2, 'a'},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := tlsRecordTypes(test.wire)
			if test.wantErr {
				if err == nil {
					t.Fatalf("tlsRecordTypes error = nil, want non-nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("tlsRecordTypes error = %v", err)
			}
			if !slices.Equal(got, test.want) {
				t.Errorf("tlsRecordTypes = %v, want %v", got, test.want)
			}
		})
	}
}

type wireRecordingConn struct {
	net.Conn
	mu   sync.Mutex
	wire []byte
}

func (c *wireRecordingConn) Write(p []byte) (int, error) {
	// Cover the real write so a mark cannot place already-transmitted bytes on
	// the wrong side of its snapshot before they are appended to wire.
	c.mu.Lock()
	defer c.mu.Unlock()
	n, err := c.Conn.Write(p)
	c.wire = append(c.wire, p[:n]...)
	return n, err
}

func TestWireRecordingConnMarkWaitsForWrite(t *testing.T) {
	t.Parallel()
	wrote := make(chan []byte, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	conn := &wireRecordingConn{Conn: &gatedWriteConn{
		wrote:   wrote,
		release: release,
	}}
	payload := []byte("record")
	writeDone := make(chan error, 1)
	go func() {
		n, err := conn.Write(payload)
		if err == nil && n != len(payload) {
			err = fmt.Errorf("Write length = %d, want %d", n, len(payload))
		}
		writeDone <- err
	}()

	if got := <-wrote; !bytes.Equal(got, payload) {
		t.Fatalf("underlying Write payload = %q, want %q", got, payload)
	}
	markStarted := make(chan struct{})
	markDone := make(chan int, 1)
	go func() {
		close(markStarted)
		markDone <- conn.mark()
	}()
	<-markStarted
	select {
	case got := <-markDone:
		t.Fatalf("mark returned %d while the underlying Write was in progress", got)
	case <-time.After(100 * time.Millisecond):
	}

	releaseOnce.Do(func() { close(release) })
	if err := <-writeDone; err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := <-markDone; got != len(payload) {
		t.Fatalf("mark after Write = %d, want %d", got, len(payload))
	}
}

type gatedWriteConn struct {
	net.Conn
	wrote   chan<- []byte
	release <-chan struct{}
}

func (c *gatedWriteConn) Write(p []byte) (int, error) {
	c.wrote <- append([]byte(nil), p...)
	<-c.release
	return len(p), nil
}

func (c *wireRecordingConn) mark() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.wire)
}

func (c *wireRecordingConn) bytesBetween(start, end int) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.wire[start:end]...)
}
