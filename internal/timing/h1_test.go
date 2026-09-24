package timing

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type h1Wire struct {
	net.Conn
	mu      sync.Mutex
	records []byte
	enabled bool
}

func (wire *h1Wire) Write(data []byte) (int, error) {
	n, err := wire.Conn.Write(data)
	wire.mu.Lock()
	defer wire.mu.Unlock()
	if wire.enabled {
		wire.records = append(wire.records, data[:n]...)
	}
	return n, err
}

func h1TLS(t *testing.T, version uint16) (*tls.Conn, *tls.Conn, *h1Wire) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	raw, err := new(net.Dialer).DialContext(context.Background(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	wire := &h1Wire{Conn: raw}
	roots := x509.NewCertPool()
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots.AddCert(parsed)
	config := &tls.Config{
		MinVersion: version, MaxVersion: version, DynamicRecordSizingDisabled: true,
		CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
	}
	serverConfig := config.Clone()
	serverConfig.Certificates = []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}
	config.RootCAs = roots
	config.ServerName = "localhost"
	client, server := tls.Client(wire, config), tls.Server(accepted, serverConfig)
	deadline := time.Now().Add(5 * time.Second)
	_ = client.SetDeadline(deadline)
	_ = server.SetDeadline(deadline)
	done := make(chan error, 1)
	go func() { done <- server.HandshakeContext(context.Background()) }()
	if err := client.HandshakeContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	wire.enabled = true
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return client, server, wire
}

func h1Request(t *testing.T, body []byte, chunked bool) *http.Request {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://localhost/item", nil)
	if err != nil {
		t.Fatal(err)
	}
	if chunked {
		request.Header.Set("Transfer-Encoding", "chunked")
	}
	return request
}

func TestH1EncryptedRecordGrouping(t *testing.T) {
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		for _, chunked := range []bool{false, true} {
			for _, body := range []string{"", "x", "payload", strings.Repeat("x", 40000)} {
				for _, last := range []bool{false, true} {
					name := fmt.Sprintf("TLS-%x/body-%d/chunked-%t/held-%t", version, len(body), chunked, last)
					t.Run(name, func(t *testing.T) {
						client, server, wire := h1TLS(t, version)
						request := h1Request(t, []byte(body), chunked)
						if !chunked {
							request.Header.Set("Content-Length", fmt.Sprint(len(body)))
						}
						if chunked {
							request.Trailer = http.Header{"X-End": []string{"done"}}
						}
						prepared, err := prepareH1(request, []byte(body), len(body) < 20000, last)
						if err != nil {
							t.Fatal(err)
						}
						peer := make(chan error, 1)
						go func() {
							request, err := http.ReadRequest(bufio.NewReader(server))
							if err == nil {
								if chunked && !reflect.DeepEqual(request.TransferEncoding, []string{"chunked"}) ||
									!chunked && (len(request.TransferEncoding) != 0 || request.ContentLength != int64(len(body)) ||
										request.Header.Get("Content-Length") != fmt.Sprint(len(body))) {
									err = fmt.Errorf("unexpected peer framing: length=%d coding=%v header=%v",
										request.ContentLength, request.TransferEncoding, request.Header)
								}
								var received []byte
								if err == nil {
									received, err = io.ReadAll(request.Body)
								}
								if err == nil && string(received) != body {
									err = io.ErrUnexpectedEOF
								}
							}
							if err == nil {
								_, err = io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
							}
							peer <- err
						}()
						gate := make(chan struct{})
						close(gate)
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						result := runH1(ctx, client, prepared, gate, gate, 8192, 1024)
						if result.err != nil {
							t.Fatal(result.err)
						}
						if err := <-peer; err != nil {
							t.Fatal(err)
						}
						if result.finalHeaders.IsZero() || result.complete.IsZero() || result.written.IsZero() {
							t.Fatal("missing milestones")
						}
						if len(prepared.records) == 1 && !last && !result.initial.Equal(result.final) {
							t.Fatal("single record has unequal release milestones")
						}
						wire.mu.Lock()
						encrypted := bytes.Clone(wire.records)
						wire.mu.Unlock()
						lengths := []int{}
						for len(encrypted) > 0 {
							if len(encrypted) < 5 {
								t.Fatal("truncated record")
							}
							size := int(binary.BigEndian.Uint16(encrypted[3:5]))
							if size+5 > len(encrypted) {
								t.Fatal("truncated encrypted payload")
							}
							if encrypted[0] != 23 {
								t.Fatalf("unexpected control record %d", encrypted[0])
							}
							overhead := 17
							if version == tls.VersionTLS12 {
								overhead = 24
							}
							lengths = append(lengths, size-overhead)
							encrypted = encrypted[5+size:]
						}
						expected := append([][]byte{}, prepared.records...)
						if last {
							expected = append(expected, prepared.final)
						}
						if len(lengths) != len(expected) {
							t.Fatalf("encrypted records %v, wanted %d", lengths, len(expected))
						}
						for i, length := range lengths {
							if length != len(expected[i]) {
								t.Fatalf("record %d plaintext length %d, wanted %d", i, length, len(expected[i]))
							}
						}
					})
				}
			}
		}
	}
}

func TestH1StrictBoundaryAndValidation(t *testing.T) {
	request := h1Request(t, nil, false)
	baseline, err := prepareH1(request, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	// Header padding avoids a Content-Length digit-width dependency.
	request.Header.Set("X-Padding", strings.Repeat("x", h1RecordLimit-len(baseline.records[0])-13))
	exact, err := prepareH1(request, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(exact.records[0]) != h1RecordLimit {
		t.Fatalf("size %d", len(exact.records[0]))
	}
	request.Header.Set("X-Padding", request.Header.Get("X-Padding")+"x")
	if _, err := prepareH1(request, nil, true, false); err == nil {
		t.Fatal("oversize accepted")
	}
	for _, headers := range []http.Header{
		{"Content-Length": []string{"2"}},
		{"Content-Length": []string{"-1"}},
		{"Content-Length": []string{"0", "1"}},
		{"Content-Length": []string{"0"}, "Transfer-Encoding": []string{"chunked"}},
		{"Transfer-Encoding": []string{"gzip, chunked"}},
		{"Upgrade": []string{"websocket"}},
	} {
		request := h1Request(t, nil, false)
		request.Header = headers
		if _, err := prepareH1(request, nil, true, false); err == nil {
			t.Fatalf("accepted %v", headers)
		}
	}
}

func TestH1ResponseValidationAndPreservation(t *testing.T) {
	cases := []struct {
		name, response, method   string
		valid, complete, retired bool
	}{
		{"informational", "HTTP/1.1 100 Continue\r\n\r\n" +
			"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n", "GET", true, true, false},
		{"conflict", "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n" +
			"Transfer-Encoding: chunked\r\n\r\n0\r\n\r\n", "GET", false, false, false},
		{"negative", "HTTP/1.1 200 OK\r\nContent-Length: -1\r\n\r\n", "GET", false, false, false},
		{"short body", "HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\nx", "GET", true, false, false},
		{"HEAD", "HTTP/1.1 200 OK\r\nContent-Length: 99\r\n\r\n", "HEAD", true, true, false},
		{"retirement", "HTTP/1.1 200 OK\r\nConnection: close\r\n\r\nx", "GET", true, true, true},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer func() { _ = client.Close() }()
			go func() { defer func() { _ = server.Close() }(); _, _ = io.WriteString(server, item.response) }()
			request := h1Request(t, nil, false)
			request.Method = item.method
			result := readH1(client, request, 8192, 1024)
			if !result.finalHeaders.IsZero() != item.valid ||
				!result.complete.IsZero() != item.complete || result.retired != item.retired {
				t.Fatalf("unexpected observation %+v", result)
			}
		})
	}
}

func TestH1CancellationBeforeRelease(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = server.Close() }()
	prepared, err := prepareH1(h1Request(t, nil, false), nil, true, true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gate := make(chan struct{})
	result := runH1(ctx, client, prepared, gate, gate, 1024, 1024)
	if result.err == nil || !result.initial.IsZero() || !result.final.IsZero() {
		t.Fatalf("unexpected observation %+v", result)
	}
}

func TestH1EarlyHeadersRetainFinalisation(t *testing.T) {
	client, server, _ := h1TLS(t, tls.VersionTLS13)
	prepared, err := prepareH1(h1Request(t, []byte("x"), false), []byte("x"), true, true)
	if err != nil {
		t.Fatal(err)
	}
	early := make(chan struct{})
	peer := make(chan error, 1)
	go func() {
		prefix := make([]byte, len(prepared.records[0]))
		_, err := io.ReadFull(server, prefix)
		if err == nil {
			_, err = io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
		}
		close(early)
		if err == nil {
			final := make([]byte, 1)
			_, err = io.ReadFull(server, final)
			if err == nil && final[0] != 'x' {
				err = io.ErrUnexpectedEOF
			}
		}
		peer <- err
	}()
	start, finish := make(chan struct{}), make(chan struct{})
	close(start)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan h1Observation, 1)
	go func() { result <- runH1(ctx, client, prepared, start, finish, 8192, 1024) }()
	<-early
	close(finish)
	observed := <-result
	if observed.err != nil {
		t.Fatal(observed.err)
	}
	if err := <-peer; err != nil {
		t.Fatal(err)
	}
	if observed.final.IsZero() || observed.finalHeaders.IsZero() {
		t.Fatal("early response withdrew finalisation")
	}
}

func TestH1ReceiveBounds(t *testing.T) {
	for _, item := range []struct {
		response           string
		headerMax, bodyMax int64
		valid              bool
	}{
		{"HTTP/1.1 200 OK\r\nX-Large: " + strings.Repeat("x", 20000) + "\r\n\r\n", 1024, 1024, false},
		{"HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\nabc", 1024, 2, true},
	} {
		client, server := net.Pipe()
		go func() { defer func() { _ = server.Close() }(); _, _ = io.WriteString(server, item.response) }()
		observed := readH1(client, h1Request(t, nil, false), item.headerMax, item.bodyMax)
		_ = client.Close()
		if observed.err == nil || !observed.finalHeaders.IsZero() != item.valid ||
			!observed.complete.IsZero() && !item.valid {
			t.Fatalf("unexpected bounds observation %+v", observed)
		}
		if item.valid && (observed.bodyBytes != item.bodyMax || observed.complete.IsZero()) {
			t.Fatal("accepted byte cap or actual boundary-crossing EOF evidence was lost")
		}
	}
}

func TestH1ExplicitEmptyLengthPreserved(t *testing.T) {
	request := h1Request(t, nil, false)
	request.Method = http.MethodGet
	request.Header["Content-Length"] = []string{"0", "0"}
	prepared, err := prepareH1(request, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(prepared.records[0], []byte("\r\nContent-Length: 0\r\n")) {
		t.Fatal("explicit zero length was omitted")
	}
}

func TestH1CancellationDuringFinalGateWriteAndDrain(t *testing.T) {
	for _, phase := range []string{"final gate", "write", "drain"} {
		t.Run(phase, func(t *testing.T) {
			client, server := net.Pipe()
			defer func() { _ = server.Close() }()
			last := phase == "final gate"
			prepared, err := prepareH1(h1Request(t, []byte("x"), false), []byte("x"), true, last)
			if err != nil {
				t.Fatal(err)
			}
			start, finish := make(chan struct{}), make(chan struct{})
			close(start)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			released := make(chan struct{})
			result := make(chan h1Observation, 1)
			go func() {
				result <- runH1WithRelease(ctx, client, prepared, start, finish, 8192, 1024,
					func(time.Time) error { close(released); return nil })
			}()
			<-released
			if phase != "write" {
				requestBytes := make([]byte, len(prepared.records[0]))
				if _, err := io.ReadFull(server, requestBytes); err != nil {
					t.Fatal(err)
				}
				if phase == "drain" {
					if _, err := io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nx"); err != nil {
						t.Fatal(err)
					}
				}
			}
			cancel()
			observed := <-result
			if observed.err == nil || observed.initial.IsZero() {
				t.Fatalf("unexpected cancellation %+v", observed)
			}
			if last && !observed.final.IsZero() {
				t.Fatal("final gate cancellation attempted final release")
			}
			if phase == "drain" && observed.finalHeaders.IsZero() {
				t.Fatal("drain cancellation lost header duration")
			}
		})
	}
}

func TestH1BodyFailureAbortsFinalGate(t *testing.T) {
	client, server := net.Pipe()
	prepared, err := prepareH1(h1Request(t, []byte("x"), false), []byte("x"), true, true)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer func() { _ = server.Close() }()
		prefix := make([]byte, len(prepared.records[0]))
		_, _ = io.ReadFull(server, prefix)
		_, _ = io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nx")
	}()
	start, finish := make(chan struct{}), make(chan struct{})
	close(start)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	observed := runH1(ctx, client, prepared, start, finish, 8192, 1024)
	if observed.err == nil || observed.finalHeaders.IsZero() || !observed.final.IsZero() {
		t.Fatalf("failure did not preserve headers and abort gate: %+v", observed)
	}
}

func TestH1EmptyChunkWithoutTrailers(t *testing.T) {
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		client, server, wire := h1TLS(t, version)
		prepared, err := prepareH1(h1Request(t, nil, true), nil, true, true)
		if err != nil {
			t.Fatal(err)
		}
		peer := make(chan error, 1)
		go func() {
			request, err := http.ReadRequest(bufio.NewReader(server))
			if err == nil {
				_, err = io.Copy(io.Discard, request.Body)
			}
			if err == nil {
				_, err = io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
			}
			peer <- err
		}()
		gate := make(chan struct{})
		close(gate)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		observed := runH1(ctx, client, prepared, gate, gate, 8192, 1024)
		cancel()
		if observed.err != nil {
			t.Fatal(observed.err)
		}
		if err := <-peer; err != nil {
			t.Fatal(err)
		}
		if !bytes.HasSuffix(prepared.records[0], []byte("0\r\n\r")) || string(prepared.final) != "\n" {
			t.Fatal("empty chunk did not hold final framing LF")
		}
		wire.mu.Lock()
		encrypted := bytes.Clone(wire.records)
		wire.mu.Unlock()
		firstSize := int(binary.BigEndian.Uint16(encrypted[3:5]))
		second := encrypted[firstSize+5:]
		overhead := 17
		if version == tls.VersionTLS12 {
			overhead = 24
		}
		if len(second) != 5+overhead+1 || int(binary.BigEndian.Uint16(second[3:5])) != overhead+1 {
			t.Fatal("held framing byte was not an independent encrypted record")
		}
	}
}

func TestH1DecodedHeaderAndTrailerBounds(t *testing.T) {
	for _, trailers := range []bool{false, true} {
		for _, accepted := range []bool{false, true} {
			value := strings.Repeat("x", 8192)
			limit := int64(len("X-Field") + len(value) + 32)
			if !accepted {
				limit--
			}
			field := "X-Field: " + value + "\r\n"
			response := "HTTP/1.1 200 OK\r\n" + field + "\r\n"
			if trailers {
				response = "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n" + field + "\r\n"
			}
			client, server := net.Pipe()
			go func() { defer func() { _ = server.Close() }(); _, _ = io.WriteString(server, response) }()
			observed := readH1(client, h1Request(t, nil, false), limit, 1024)
			_ = client.Close()
			if (observed.err == nil) != accepted {
				t.Fatalf("trailers=%t accepted=%t: %+v", trailers, accepted, observed)
			}
			if trailers && (observed.finalHeaders.IsZero() || observed.complete.IsZero()) {
				t.Fatal("trailer validation lost valid headers or observed protocol end")
			}
			if !trailers && !accepted && !observed.finalHeaders.IsZero() {
				t.Fatal("oversized decoded headers supplied endpoint")
			}
		}
	}
}

func TestH1MalformedFieldSyntaxRejected(t *testing.T) {
	for _, field := range []string{"X-Field : value", " folded", "X-Field: value\r\n continuation"} {
		client, server := net.Pipe()
		go func() {
			defer func() { _ = server.Close() }()
			_, _ = io.WriteString(server, "HTTP/1.1 200 OK\r\n"+field+"\r\n\r\n")
		}()
		observed := readH1(client, h1Request(t, nil, false), 1024, 1024)
		_ = client.Close()
		if observed.err == nil || !observed.finalHeaders.IsZero() {
			t.Fatalf("malformed field accepted: %+v", observed)
		}
	}
}

func TestH1EarlyFullClosureFailsFinalWrite(t *testing.T) {
	client, server := net.Pipe()
	prepared, err := prepareH1(h1Request(t, []byte("x"), false), []byte("x"), true, true)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer func() { _ = server.Close() }()
		prefix := make([]byte, len(prepared.records[0]))
		_, _ = io.ReadFull(server, prefix)
		_, _ = io.WriteString(server, "HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n")
	}()
	start, finish := make(chan struct{}), make(chan struct{})
	close(start)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	observed, completed := make(chan h1Observation, 1), make(chan h1Observation, 1)
	go func() {
		completed <- runH1WithObserver(ctx, client, prepared, start, finish, 8192, 1024,
			nil, nil, func(snapshot h1Observation) { observed <- snapshot })
	}()
	snapshot := <-observed
	if snapshot.err != nil || !snapshot.eofObserved {
		t.Fatal("clean response EOF was misclassified")
	}
	close(finish)
	result := <-completed
	if result.err == nil || result.finalHeaders.IsZero() || result.complete.IsZero() ||
		!result.eofObserved || result.final.IsZero() || !result.written.IsZero() {
		t.Fatalf("full closure did not preserve response and failed-write evidence: %+v", result)
	}
}

func TestH1PrefixCompletionHookPrecedesFinalGate(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = server.Close() }()
	prepared, err := prepareH1(h1Request(t, []byte("x"), false), []byte("x"), true, true)
	if err != nil {
		t.Fatal(err)
	}
	go func() { prefix := make([]byte, len(prepared.records[0])); _, _ = io.ReadFull(server, prefix) }()
	start, finish := make(chan struct{}), make(chan struct{})
	close(start)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	completed := make(chan struct{})
	result := make(chan h1Observation, 1)
	go func() {
		result <- runH1WithHooks(ctx, client, prepared, start, finish, 8192, 1024, nil,
			func() error { close(completed); return nil })
	}()
	<-completed
	cancel()
	observed := <-result
	if observed.initial.IsZero() || !observed.final.IsZero() || observed.err == nil {
		t.Fatalf("prefix hook did not preserve held gate: %+v", observed)
	}
}

func TestH1StatusValidationAndDiagnosticEvidence(t *testing.T) {
	cases := []struct {
		status, fields   string
		valid, supported bool
	}{
		{"+20 Invalid", "X-Evidence: retained\r\nContent-Length: 0\r\n", false, false},
		{"000 Invalid", "X-Evidence: retained\r\nContent-Length: 0\r\n", false, false},
		{"600 Invalid", "X-Evidence: retained\r\nContent-Length: 0\r\n", false, false},
		{"501 Unsupported", "X-Evidence: retained\r\nContent-Length: 0\r\n", true, true},
		{"101 Switching Protocols", "X-Evidence: retained\r\n", true, false},
		{"200 OK", "X-Evidence: retained\r\nContent-Length: -1\r\n", false, false},
		{"200 OK", "X-Evidence: retained\r\nContent-Length: 0\r\nTransfer-Encoding: chunked\r\n", false, false},
	}
	for _, item := range cases {
		t.Run(item.status+item.fields, func(t *testing.T) {
			client, server := net.Pipe()
			go func() {
				defer func() { _ = server.Close() }()
				_, _ = io.WriteString(server,
					"HTTP/1.1 "+item.status+"\r\n"+item.fields+"\r\n")
			}()
			observed := readH1(client, h1Request(t, nil, false), 8192, 1024)
			_ = client.Close()
			if !observed.finalHeaders.IsZero() != item.valid || (observed.err == nil) != item.supported {
				t.Fatalf("unexpected acceptance: %+v", observed)
			}
			if observed.header.Get("X-Evidence") != "retained" || observed.firstHeaders.IsZero() {
				t.Fatal("diagnostic response evidence was lost")
			}
		})
	}
}

func TestH1ReceiveMilestonesPrecedeConsumer(t *testing.T) {
	cases := []struct{ name, method, response, body string }{
		{"fixed one byte", "GET", "HTTP/1.1 200 OK\r\nContent-Length: 1\r\n\r\nx", "x"},
		{"chunked", "GET", "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n1\r\nx\r\n0\r\n\r\n", "x"},
		{"HEAD", "HEAD", "HTTP/1.1 200 OK\r\nContent-Length: 99\r\n\r\n", ""},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer func() { _ = client.Close() }()
			go func() { defer func() { _ = server.Close() }(); _, _ = io.WriteString(server, item.response) }()
			request := h1Request(t, nil, false)
			request.Method = item.method
			entered := make(chan h1Observation)
			resume := make(chan struct{})
			completed := make(chan h1Observation, 1)
			digest := sha256.New()
			var captured []byte
			var extracted bool
			go func() {
				completed <- readH1WithConsume(client, request, 8192, 1024, func(payload []byte, snapshot h1Observation) error {
					if !snapshot.complete.IsZero() {
						entered <- snapshot
						<-resume
					}
					_, _ = digest.Write(payload)
					captured = append(captured, payload...)
					extracted = strings.Contains(string(captured), "x")
					return nil
				})
			}()
			snapshot := <-entered
			if snapshot.finalHeaders.IsZero() || snapshot.complete.IsZero() || snapshot.complete.Before(snapshot.finalHeaders) {
				t.Fatal("consumer ran without reader-side receive milestones")
			}
			select {
			case <-completed:
				t.Fatal("consumer did not retain controlled downstream wait")
			default:
			}
			close(resume)
			observed := <-completed
			if observed.err != nil || !observed.finalHeaders.Equal(snapshot.finalHeaders) ||
				!observed.complete.Equal(snapshot.complete) {
				t.Fatalf("downstream processing changed receive evidence: %+v", observed)
			}
			expectedDigest := sha256.Sum256([]byte(item.body))
			if string(captured) != item.body || !bytes.Equal(digest.Sum(nil), expectedDigest[:]) ||
				extracted != strings.Contains(item.body, "x") {
				t.Fatal("consumer lost accepted body evidence")
			}
		})
	}
}

func TestH1FramingHeaderCasingAndCallerOwnership(t *testing.T) {
	cases := []struct {
		name     string
		header   http.Header
		accepted bool
	}{
		{"matching lowercase length", http.Header{"content-length": {"1"}}, true},
		{"consistent mixed case length", http.Header{"content-length": {"1"}, "Content-Length": {"01"}}, true},
		{"lowercase mismatch", http.Header{"content-length": {"2"}}, false},
		{"mixed case conflicting length", http.Header{"content-length": {"2"}, "Content-Length": {"1"}}, false},
		{"mixed case length and coding", http.Header{"content-length": {"1"}, "Transfer-Encoding": {"chunked"}}, false},
		{"lowercase coding and length", http.Header{"Content-Length": {"1"}, "transfer-encoding": {"chunked"}}, false},
		{"lowercase unsupported coding", http.Header{"transfer-encoding": {"gzip"}}, false},
		{"lowercase upgrade", http.Header{"upgrade": {"websocket"}}, false},
		{"lowercase connection upgrade", http.Header{"connection": {"upgrade"}}, false},
		{"invalid field name", http.Header{"content-length ": {"1"}}, false},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			request := h1Request(t, []byte("x"), false)
			request.Header = item.header.Clone()
			before := request.Header.Clone()
			prepared, err := prepareH1(request, []byte("x"), true, false)
			if (err == nil) != item.accepted {
				t.Fatalf("accepted=%t error=%v", item.accepted, err)
			}
			if !reflect.DeepEqual(request.Header, before) || request.ContentLength != 0 || request.Body != nil {
				t.Fatal("preparation mutated caller request")
			}
			if !item.accepted {
				return
			}
			decoded, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(prepared.records[0])))
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(decoded.Body)
			if err != nil || string(body) != "x" || decoded.ContentLength != 1 {
				t.Fatalf("matching lowercase framing changed: body=%q length=%d error=%v", body, decoded.ContentLength, err)
			}
		})
	}
}

func TestH1MethodAndAuthorityPreflight(t *testing.T) {
	for _, method := range []string{"GET /", "GET\r\nInjected:", "bad(method)"} {
		request := h1Request(t, nil, false)
		request.Method = method
		if _, err := prepareH1(request, nil, true, false); err == nil {
			t.Fatalf("accepted malformed method %q", method)
		}
	}
	for _, authority := range []string{"bad host", "bad/host", "bad\r\nInjected:"} {
		request := h1Request(t, nil, false)
		request.Host = authority
		if _, err := prepareH1(request, nil, true, false); err == nil {
			t.Fatalf("accepted malformed authority %q", authority)
		}
		if request.Host != authority {
			t.Fatal("preparation changed caller authority")
		}
	}
	missing := h1Request(t, nil, false)
	missing.Host, missing.URL.Host = "", ""
	if _, err := prepareH1(missing, nil, true, false); err == nil {
		t.Fatal("accepted missing authority")
	}
	request := h1Request(t, nil, false)
	request.Method = ""
	request.Host = "authority.test:9443"
	destination := request.URL.Host
	prepared, err := prepareH1(request, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(prepared.records[0])))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Method != "GET" || decoded.Host != request.Host || prepared.request.URL.Host != destination ||
		request.URL.Host != destination || request.Method != "" {
		t.Fatal("valid authority override or default method changed destination or caller state")
	}
}

func TestH1EarlyRetirementIntentRetainsFinalisation(t *testing.T) {
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		t.Run(fmt.Sprintf("TLS-%x", version), func(t *testing.T) {
			client, server, _ := h1TLS(t, version)
			prepared, err := prepareH1(h1Request(t, []byte("x"), false), []byte("x"), true, true)
			if err != nil {
				t.Fatal(err)
			}
			peer := make(chan error, 1)
			go func() {
				defer func() { _ = server.Close() }()
				prefix := make([]byte, len(prepared.records[0]))
				_, err := io.ReadFull(server, prefix)
				if err == nil {
					_, err = io.WriteString(server,
						"HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
				}
				final := make([]byte, 1)
				if err == nil {
					_, err = io.ReadFull(server, final)
				}
				if err == nil && final[0] != 'x' {
					err = io.ErrUnexpectedEOF
				}
				peer <- err
			}()
			start, finish := make(chan struct{}), make(chan struct{})
			close(start)
			observed := make(chan h1Observation, 1)
			completed := make(chan h1Observation, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			go func() {
				completed <- runH1WithObserver(ctx, client, prepared, start, finish, 8192, 1024,
					nil, nil, func(snapshot h1Observation) { observed <- snapshot })
			}()
			snapshot := <-observed
			if snapshot.err != nil || !snapshot.retired || snapshot.eofObserved || snapshot.complete.IsZero() {
				t.Fatalf("retirement intent became delivery failure: %+v", snapshot)
			}
			// Observation delivery precedes opening the final gate.
			close(finish)
			result := <-completed
			if err := <-peer; err != nil {
				t.Fatal(err)
			}
			if result.err != nil || result.final.IsZero() || result.written.IsZero() || !result.retired ||
				result.complete.IsZero() || result.finalHeaders.IsZero() ||
				result.final.Before(result.complete) || result.final.Before(result.finalHeaders) {
				t.Fatalf("early retiring response lost legal finalisation: %+v", result)
			}
		})
	}
}

func TestH1EarlyDirectionalEOFPermitsFinalisation(t *testing.T) {
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		t.Run(fmt.Sprintf("TLS-%x", version), func(t *testing.T) {
			client, server, _ := h1TLS(t, version)
			prepared, err := prepareH1(h1Request(t, []byte("x"), false), []byte("x"), true, true)
			if err != nil {
				t.Fatal(err)
			}
			peer := make(chan error, 1)
			go func() {
				defer func() { _ = server.Close() }()
				prefix := make([]byte, len(prepared.records[0]))
				_, err := io.ReadFull(server, prefix)
				if err == nil {
					_, err = io.WriteString(server, "HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n")
				}
				if err == nil {
					err = server.CloseWrite()
				}
				final := make([]byte, 1)
				if err == nil {
					_, err = io.ReadFull(server, final)
				}
				if err == nil && final[0] != 'x' {
					err = io.ErrUnexpectedEOF
				}
				peer <- err
			}()
			start, finish := make(chan struct{}), make(chan struct{})
			close(start)
			observed, completed := make(chan h1Observation, 1), make(chan h1Observation, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			go func() {
				completed <- runH1WithObserver(ctx, client, prepared, start, finish, 8192, 1024,
					nil, nil, func(snapshot h1Observation) { observed <- snapshot })
			}()
			snapshot := <-observed
			if snapshot.err != nil || !snapshot.eofObserved || !snapshot.retired || snapshot.complete.IsZero() {
				t.Fatalf("directional EOF was classified as failed delivery: %+v", snapshot)
			}
			// Observation delivery precedes opening the final gate.
			close(finish)
			result := <-completed
			if err := <-peer; err != nil {
				t.Fatal(err)
			}
			if result.err != nil || result.final.IsZero() || result.written.IsZero() || !result.retired ||
				result.complete.IsZero() || result.finalHeaders.IsZero() ||
				result.final.Before(result.complete) || result.final.Before(result.finalHeaders) {
				t.Fatalf("directional EOF prevented legal current finalisation: %+v", result)
			}
		})
	}
}

func TestH1ManyInformationalSectionsPreserveFinalResponse(t *testing.T) {
	for _, surplus := range []bool{false, true} {
		t.Run(fmt.Sprintf("surplus-%t", surplus), func(t *testing.T) {
			client, server := net.Pipe()
			response := strings.Repeat("HTTP/1.1 103 Early Hints\r\nX-Hint: neutral\r\n\r\n", 256) +
				"HTTP/1.1 200 OK\r\nContent-Length: 1\r\nX-Final: retained\r\n\r\nx"
			if surplus {
				response += "unexpected"
			}
			go func() { defer func() { _ = server.Close() }(); _, _ = io.WriteString(server, response) }()
			var captured []byte
			result := readH1WithConsume(client, h1Request(t, nil, false), 128, 1024,
				func(payload []byte, _ h1Observation) error { captured = append(captured, payload...); return nil })
			_ = client.Close()
			if (result.err != nil) != surplus || result.status != 200 || result.header.Get("X-Final") != "retained" ||
				result.firstHeaders.IsZero() || result.finalHeaders.IsZero() ||
				result.complete.IsZero() || string(captured) != "x" {
				t.Fatalf("informational sections lost bounded final parsing: %+v captured=%q", result, captured)
			}
			if result.finalHeaders.Before(result.firstHeaders) {
				t.Fatal("final response headers preceded the first header observation")
			}
		})
	}
}

func TestH1FramingWhitespaceUsesASCIIOnly(t *testing.T) {
	cases := []struct {
		name     string
		header   http.Header
		accepted bool
	}{
		{"length ASCII OWS", http.Header{"Content-Length": {" \t1\t "}}, true},
		{"coding ASCII OWS", http.Header{"Transfer-Encoding": {" \tChUnKeD\t "}}, true},
		{"length non-breaking space", http.Header{"Content-Length": {"\u00a01\u00a0"}}, false},
		{"length em space", http.Header{"Content-Length": {"\u20031"}}, false},
		{"length next line", http.Header{"Content-Length": {"1\u0085"}}, false},
		{"coding non-breaking space", http.Header{"Transfer-Encoding": {"\u00a0chunked\u00a0"}}, false},
		{"coding em space", http.Header{"Transfer-Encoding": {"\u2003chunked"}}, false},
		{"coding next line", http.Header{"Transfer-Encoding": {"chunked\u0085"}}, false},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			request := h1Request(t, []byte("x"), false)
			request.Header = item.header
			prepared, err := prepareH1(request, []byte("x"), true, false)
			if (err == nil) != item.accepted {
				t.Fatalf("accepted=%t error=%v", item.accepted, err)
			}
			if !item.accepted {
				return
			}
			decoded, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(prepared.records[0])))
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(decoded.Body)
			if err != nil || string(body) != "x" {
				t.Fatalf("ASCII OWS altered body framing: %q error=%v", body, err)
			}
		})
	}
}

func TestH1EmptyRequestFraming(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, explicit := range []bool{false, true} {
			for _, chunked := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/explicit-%t/chunked-%t", method, explicit, chunked), func(t *testing.T) {
					request := h1Request(t, nil, chunked)
					request.Method = method
					if explicit && !chunked {
						request.Header.Set("Content-Length", "0")
					}
					prepared, err := prepareH1(request, nil, true, true)
					if err != nil {
						t.Fatal(err)
					}
					wire := append(bytes.Clone(prepared.records[0]), prepared.final...)
					end := bytes.Index(wire, []byte("\r\n\r\n"))
					if end < 0 {
						t.Fatal("missing header section")
					}
					fields := string(wire[:end])
					hasLength := strings.Contains(fields, "\r\nContent-Length: 0")
					hasCoding := strings.Contains(fields, "\r\nTransfer-Encoding: chunked")
					if hasCoding != chunked || chunked && hasLength || explicit && !chunked && !hasLength {
						t.Fatalf("unexpected raw framing: %q", fields)
					}
					if !chunked && (len(wire) != end+4 || !bytes.HasSuffix(prepared.records[0], []byte("\r\n\r"))) {
						t.Fatalf("empty fixed request held byte is not the header terminator: %q", wire)
					}
					if chunked && string(wire[end+4:]) != "0\r\n\r\n" {
						t.Fatalf("empty chunked framing: %q", wire)
					}
					decoded, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(wire)))
					if err != nil {
						t.Fatal(err)
					}
					body, err := io.ReadAll(decoded.Body)
					if err != nil || len(body) != 0 {
						t.Fatalf("empty body: %q error=%v", body, err)
					}
				})
			}
		}
	}
}
